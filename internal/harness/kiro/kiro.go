// Package kiro parses Kiro (AWS's agentic IDE and CLI, kiro.dev) usage.
//
// Kiro keeps its logs in several shapes; MyToken reads all of them:
//
//	CLI      <KIRO_HOME>/sessions/cli/<name>.jsonl plus a <name>.json companion
//	         (session_id, cwd, created_at, session_state with the model id and
//	         per-turn metering credits)
//	IDE v2   <KIRO_HOME>/sessions/<hash>/sess_<id>/messages.jsonl plus a
//	         session.json (id, modelId, workspacePaths)
//	IDE v1   <agentDir>/<32-char workspace hash>/<name>.chat and extensionless
//	         execution documents, plus <agentDir>/workspace-sessions/<base64
//	         workspace>/*.json session files
//
// KIRO_HOME defaults to ~/.kiro; the v1 agent directory is platform specific
// (macOS ~/Library/Application Support/Kiro/User/globalStorage/kiro.kiroagent,
// Windows %USERPROFILE%/AppData/Roaming/..., Linux ~/.kiro-server/data/... or
// ~/.config/Kiro/...).
//
// Kiro reports no token counts in its CLI logs, so input, output and reasoning
// tokens are estimated from transcript characters at four characters per token;
// the v2 IDE store reports reasoning text separately from assistant text, and
// the two stay disjoint in Tokens. Kiro has no cache-token signal at all, so
// cache read and write are always zero. Credits are Kiro's native billing unit
// and are reported as Bill{Unit: "credit"} rather than converted to USD (see
// model.Bill); CostUSD is left nil so MyToken's price table can estimate it.
// A CLI "Clear" entry marks a context clear and is applied to the next request
// as model.BoundaryClear.
//
// Ported from codeburn (MIT, Copyright (c) 2026 AgentSeal),
// src/providers/kiro.ts; see THIRD_PARTY_NOTICES. Deviations from that reader:
// codeburn converts credits to dollars at the public overage rate and prices
// every call through its own model catalog, while MyToken leaves CostUSD to the
// price table and keeps credits in Bill; codeburn's tool-name mapping is not
// ported because model.UsageEvent stores no tool names — only tool-result text
// still feeds the input estimate, and tool presence still marks a turn as
// active.
//
// Not verified against real Kiro data: this machine has none. The conformance
// fixture was written from codeburn's readers and Kiro's documented layouts.
package kiro

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

const (
	// kindJSONL is the source kind of a JSONL session log.
	kindJSONL = "jsonl"
	// kindJSON is the source kind of a whole-file JSON document.
	kindJSON = "json"
	// providerName is Kiro's provider id in the logs.
	providerName = "kiro"
	// creditsUnit is Kiro's native billing unit.
	creditsUnit = "credit"

	// autoModelID is what Kiro writes when the model was not pinned.
	autoModelID = "kiro-auto"
	// defaultIDEProject labels sessions whose workspace cannot be resolved.
	defaultIDEProject = "kiro-ide"
	// defaultCLIProject labels CLI sessions without a cwd.
	defaultCLIProject = "kiro-cli"
	// charsPerToken is codeburn's transcript-to-token estimate.
	charsPerToken = 4
	// fingerprintBytes is how much of a file the change fingerprint covers.
	fingerprintBytes = 4096
	// sessionsDirName holds the CLI store and the v2 IDE store.
	sessionsDirName = "sessions"
	// cliDirName is the CLI store inside the sessions directory.
	cliDirName = "cli"
	// agentDirName is the v1 IDE agent directory inside globalStorage.
	agentDirName = "kiro.kiroagent"
	// workspaceSessionsDirName holds v1 workspace-session documents.
	workspaceSessionsDirName = "workspace-sessions"
	// workspaceStorageDirName holds the v1 workspace metadata.
	workspaceStorageDirName = "workspaceStorage"
)

// minReasonableMS is the timestamp floor codeburn applies (1e12 ms).
var minReasonableMS = float64(1e12)

// maxSafeInteger clamps hostile token counts.
var maxSafeInteger = float64(1<<53 - 1)

var (
	// modelVersionRE turns dotted model versions into dashed ones.
	modelVersionRE = regexp.MustCompile(`(\d+)\.(\d+)`)
	// toolUseRE detects Kiro's inline tool-use markers in execution text.
	toolUseRE = regexp.MustCompile(`<tool_use>\s*<name>([^<]+)</name>`)
	// hash32RE matches a Kiro IDE workspace hash directory name.
	hash32RE = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
)

// modernConversationKeys are the field names v1 execution documents use for
// their transcript arrays.
var modernConversationKeys = []string{"messages", "conversation", "chat", "transcript", "entries", "events"}

// Parser implements harness.Parser for Kiro.
type Parser struct {
	roots []string
	// prov recomputes this parser's default-root provenance from the current
	// environment. It is set only on the registry singleton; a parser built for
	// explicit roots reports those roots instead.
	prov func() ([]harness.Root, error)
}

var _ harness.Parser = (*Parser)(nil)

func init() { harness.Register(New()) }

// New returns a parser over the default roots.
func New() *Parser {
	p := NewWithRoots(defaultRoots()...)
	p.prov = rootProvenance
	return p
}

// NewWithRoot returns a parser over a single root.
func NewWithRoot(root string) *Parser { return NewWithRoots(root) }

// NewWithRoots returns a parser over the given roots. A root may be the Kiro
// home (holding sessions/) or a v1 agent directory.
func NewWithRoots(roots ...string) *Parser {
	p := &Parser{}
	seen := make(map[string]bool, len(roots))
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		root = filepath.Clean(root)
		if seen[root] {
			continue
		}
		seen[root] = true
		p.roots = append(p.roots, root)
	}
	return p
}

// rootProvenance reports Kiro's default roots and where each came from.
// MYTOKEN_KIRO_DIRS replaces the whole set, KIRO_HOME is an explicit
// harness-specific base, and the platform agent directories are defaults.
func rootProvenance() ([]harness.Root, error) {
	if v, ok := harness.Getenv("MYTOKEN_KIRO_DIRS"); ok {
		dirs, err := harness.SplitEnvList("MYTOKEN_KIRO_DIRS", v)
		if err != nil {
			return nil, err
		}
		return harness.EnvRoots("MYTOKEN_KIRO_DIRS", dirs...), nil
	}
	var out []harness.Root
	if home, ok := harness.Getenv("KIRO_HOME"); ok {
		out = append(out, harness.Root{Path: home, Origin: harness.OriginEnv, Env: "KIRO_HOME"})
	} else {
		out = append(out, harness.Root{
			Path:   filepath.Join(harness.Home(), ".kiro"),
			Origin: harness.OriginBuiltin,
		})
	}
	return append(out, harness.BuiltinRoots(defaultAgentDirs()...)...), nil
}

// defaultRoots returns the paths of rootProvenance. A malformed explicit
// override yields no roots: it is never replaced by the built-in default.
func defaultRoots() []string {
	roots, err := rootProvenance()
	if err != nil {
		return nil
	}
	return harness.RootPaths(roots)
}

// RootProvenance implements harness.Provenance.
func (p *Parser) RootProvenance() ([]harness.Root, error) {
	if p.prov == nil {
		return harness.ExplicitRoots(p.roots...), nil
	}
	return p.prov()
}

// defaultAgentDirs returns the v1 IDE agent directories for this platform.
func defaultAgentDirs() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{filepath.Join(harness.Home(), "Library", "Application Support", "Kiro", "User", "globalStorage", agentDirName)}
	case "windows":
		return []string{filepath.Join(harness.Home(), "AppData", "Roaming", "Kiro", "User", "globalStorage", agentDirName)}
	default:
		server := filepath.Join(harness.Home(), ".kiro-server", "data", "User", "globalStorage", agentDirName)
		config := filepath.Join(harness.Home(), ".config", "Kiro", "User", "globalStorage", agentDirName)
		if st, err := os.Stat(server); err == nil && st.IsDir() {
			return []string{server, config}
		}
		return []string{config}
	}
}

// Harness reports the harness this parser reads.
func (p *Parser) Harness() model.Harness { return model.Kiro }

// WithRoots implements harness.Rooted. It returns a fresh parser over exactly
// the given roots; an explicit empty list stays empty instead of falling back
// to this harness's default roots.
func (p *Parser) WithRoots(roots ...string) harness.Parser {
	for _, r := range roots {
		if strings.TrimSpace(r) != "" {
			return NewWithRoots(roots...)
		}
	}
	return &Parser{}
}

// Roots reports the configured roots.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

// Discover finds every Kiro log under the configured roots: the CLI store, the
// v2 IDE store, the v1 workspace/execution documents and the v1
// workspace-session documents.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	byPath := make(map[string]harness.Source)
	for _, root := range p.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.discoverSessions(ctx, root, byPath)
		p.discoverAgentDir(ctx, root, byPath)
	}
	out := make([]harness.Source, 0, len(byPath))
	for _, src := range byPath {
		out = append(out, src)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// discoverSessions finds the CLI store (<root>/sessions/cli/*.jsonl) and the v2
// IDE store (<root>/sessions/<hash>/sess_*/messages.jsonl).
func (p *Parser) discoverSessions(ctx context.Context, root string, byPath map[string]harness.Source) {
	sessionsDir := filepath.Join(root, sessionsDirName)
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return
		}
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if entry.Name() == cliDirName {
			files, err := os.ReadDir(filepath.Join(sessionsDir, cliDirName))
			if err != nil {
				continue
			}
			for _, file := range files {
				if file.IsDir() || !strings.HasSuffix(file.Name(), ".jsonl") {
					continue
				}
				addSource(byPath, filepath.Join(sessionsDir, cliDirName, file.Name()), kindJSONL)
			}
			continue
		}
		hashDir := filepath.Join(sessionsDir, entry.Name())
		subs, err := os.ReadDir(hashDir)
		if err != nil {
			continue
		}
		for _, sub := range subs {
			if !sub.IsDir() || !strings.HasPrefix(sub.Name(), "sess_") {
				continue
			}
			messages := filepath.Join(hashDir, sub.Name(), "messages.jsonl")
			if st, err := os.Stat(messages); err == nil && !st.IsDir() {
				addSource(byPath, messages, kindJSONL)
			}
		}
	}
}

// discoverAgentDir finds the v1 IDE documents under an agent directory.
func (p *Parser) discoverAgentDir(ctx context.Context, root string, byPath map[string]harness.Source) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return
		}
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if entry.Name() == workspaceSessionsDirName {
			p.discoverWorkspaceSessions(filepath.Join(root, entry.Name()), byPath)
			continue
		}
		if !hash32RE.MatchString(entry.Name()) {
			continue
		}
		wsDir := filepath.Join(root, entry.Name())
		children, err := os.ReadDir(wsDir)
		if err != nil {
			continue
		}
		for _, child := range children {
			if strings.HasPrefix(child.Name(), ".") {
				continue
			}
			childPath := filepath.Join(wsDir, child.Name())
			if !child.IsDir() {
				if strings.HasSuffix(child.Name(), ".chat") || filepath.Ext(child.Name()) == "" {
					addSource(byPath, childPath, kindJSON)
				}
				continue
			}
			// Extensionless execution documents sit one level deeper.
			grand, err := os.ReadDir(childPath)
			if err != nil {
				continue
			}
			for _, file := range grand {
				if file.IsDir() || strings.HasPrefix(file.Name(), ".") || filepath.Ext(file.Name()) != "" {
					continue
				}
				addSource(byPath, filepath.Join(childPath, file.Name()), kindJSON)
			}
		}
	}
}

// discoverWorkspaceSessions finds <agentDir>/workspace-sessions/<base64>/*.json.
func (p *Parser) discoverWorkspaceSessions(dir string, byPath map[string]harness.Source) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") || file.Name() == "sessions.json" {
				continue
			}
			addSource(byPath, filepath.Join(dir, entry.Name(), file.Name()), kindJSON)
		}
	}
}

func addSource(byPath map[string]harness.Source, path, kind string) {
	if _, ok := byPath[path]; !ok {
		byPath[path] = harness.Source{Path: path, Kind: kind}
	}
}

// Parse reads one source. Every Kiro format is either a whole-file JSON
// document or a JSONL log whose turns can only be grouped once the file is
// read end to end, so a changed file is reparsed in full; an unchanged file is
// a no-op and repeated records are collapsed by DedupKey.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if err := ctx.Err(); err != nil {
		return harness.Batch{}, err
	}
	st, err := os.Stat(src.Path)
	if err != nil || st.IsDir() {
		return harness.Batch{Next: cur}, nil
	}
	fp, err := fingerprint(src.Path)
	if err != nil {
		return harness.Batch{}, err
	}
	if cur.Fingerprint != "" && cur.Fingerprint == fp && cur.Size == st.Size() && cur.ModTime.Equal(st.ModTime()) {
		return harness.Batch{Next: cur}, nil
	}
	next := harness.Cursor{Offset: st.Size(), Size: st.Size(), ModTime: st.ModTime(), Fingerprint: fp}
	raw, err := os.ReadFile(src.Path)
	if err != nil {
		return harness.Batch{Next: next}, nil
	}
	events, sessions := parseSource(src.Path, raw)
	sort.Slice(events, func(i, j int) bool { return events[i].DedupKey < events[j].DedupKey })
	return harness.Batch{Events: events, Sessions: sessions, Next: next}, nil
}

// parseSource dispatches on the document shape, like codeburn's session parser.
func parseSource(path string, raw []byte) ([]model.UsageEvent, []model.SessionMeta) {
	if isV2MessagesPath(path) {
		return parseV2Session(path, raw)
	}
	if strings.HasSuffix(path, ".jsonl") {
		return parseCLISession(path, raw)
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, nil
	}
	if isArray(record["history"]) && stringValue(record["sessionId"]) != "" {
		return parseWorkspaceSession(path, record)
	}
	if isArray(record["chat"]) && len(record["metadata"]) > 0 {
		return parseChatFile(path, raw)
	}
	return parseModernExecution(path, raw)
}

// isV2MessagesPath reports whether path is a v2 IDE event log.
func isV2MessagesPath(path string) bool {
	if filepath.Base(path) != "messages.jsonl" {
		return false
	}
	return strings.HasPrefix(filepath.Base(filepath.Dir(path)), "sess_")
}

// --- CLI sessions ---------------------------------------------------------

type kiroCLIMeta struct {
	SessionID    string              `json:"session_id"`
	CWD          string              `json:"cwd"`
	CreatedAt    string              `json:"created_at"`
	UpdatedAt    string              `json:"updated_at"`
	Title        string              `json:"title"`
	SessionState kiroCLISessionState `json:"session_state"`
}

type kiroCLISessionState struct {
	RTSModelState        kiroCLIRTSModelState    `json:"rts_model_state"`
	ConversationMetadata kiroCLIConversationMeta `json:"conversation_metadata"`
}

type kiroCLIRTSModelState struct {
	ModelInfo struct {
		ModelID string `json:"model_id"`
	} `json:"model_info"`
}

type kiroCLIConversationMeta struct {
	UserTurnMetadatas []kiroCLITurnMeta `json:"user_turn_metadatas"`
}

type kiroCLITurnMeta struct {
	EndTimestamp  json.RawMessage `json:"end_timestamp"`
	MeteringUsage []kiroCLIMeter  `json:"metering_usage"`
}

type kiroCLIMeter struct {
	Value json.RawMessage `json:"value"`
	Unit  string          `json:"unit"`
}

type kiroCLIEntry struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type kiroCLIData struct {
	Content []kiroCLIContent `json:"content"`
	Meta    struct {
		Timestamp json.RawMessage `json:"timestamp"`
	} `json:"meta"`
}

type kiroCLIContent struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// parseCLISession groups a CLI JSONL log into prompt turns. A turn is emitted
// once it has assistant text; its tokens are estimated from the turn's user and
// tool-result characters, and its credits come from the companion metadata.
func parseCLISession(path string, raw []byte) ([]model.UsageEvent, []model.SessionMeta) {
	meta := readCLIMeta(path)
	sessionID := firstNonEmpty(meta.SessionID, strings.TrimSuffix(filepath.Base(path), ".jsonl"))
	session := kiroSession{id: sessionID, project: firstNonEmpty(meta.CWD, defaultCLIProject)}
	modelID := normalizeKiroModelID(meta.SessionState.RTSModelState.ModelInfo.ModelID)
	turns := meta.SessionState.ConversationMetadata.UserTurnMetadatas

	boundaries := &harness.BoundaryState{}
	events := []model.UsageEvent{}
	seen := make(map[string]bool)
	turnIndex := 0
	inputChars, outputChars := 0, 0
	turnStart := time.Time{}
	firstPrompt := true

	flushTurn := func() {
		if outputChars == 0 {
			// codeburn leaves the turn index untouched here: the turn produced
			// no assistant text, so it is not a billable turn.
			return
		}
		key := fmt.Sprintf("kiro-cli:%s:%d", sessionID, turnIndex)
		if seen[key] {
			turnIndex++
			return
		}
		var ts time.Time
		var credits float64
		if turnIndex < len(turns) {
			ts = parseKiroTimestamp(turns[turnIndex].EndTimestamp)
			for _, meter := range turns[turnIndex].MeteringUsage {
				credits += numberValue(meter.Value)
			}
		}
		if ts.IsZero() {
			ts = turnStart
		}
		if ts.IsZero() {
			ts = parseKiroTimestampString(meta.CreatedAt)
		}
		if ts.IsZero() {
			turnIndex++
			return
		}
		seen[key] = true
		events = append(events, session.event(key, ts, modelID, estimateTokens(inputChars), estimateTokens(outputChars), 0, credits, boundaries.Apply(key)))
		session.note(ts)
		turnIndex++
	}

	for _, line := range splitJSONLines(raw) {
		var entry kiroCLIEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		switch entry.Kind {
		case "Prompt":
			if !firstPrompt {
				flushTurn()
				inputChars, outputChars = 0, 0
			}
			firstPrompt = false
			var data kiroCLIData
			if err := json.Unmarshal(entry.Data, &data); err != nil {
				continue
			}
			for _, item := range data.Content {
				if item.Kind != "text" {
					continue
				}
				text := extractText(item.Data)
				if session.title == "" {
					session.title = text
				}
				inputChars += charCount(text)
			}
			if ts := parseKiroTimestamp(data.Meta.Timestamp); !ts.IsZero() {
				turnStart = ts
			}
		case "AssistantMessage":
			var data kiroCLIData
			if err := json.Unmarshal(entry.Data, &data); err != nil {
				continue
			}
			for _, item := range data.Content {
				if item.Kind == "text" {
					outputChars += charCount(extractText(item.Data))
				}
			}
		case "ToolResults":
			var data kiroCLIData
			if err := json.Unmarshal(entry.Data, &data); err != nil {
				continue
			}
			for _, item := range data.Content {
				inputChars += charCount(extractText(item.Data))
			}
		case "Clear":
			// A context clear belongs to the next request.
			boundaries.Pending = model.BoundaryClear
		}
	}
	flushTurn()
	return events, session.metas()
}

// readCLIMeta reads the companion <name>.json next to a CLI log.
func readCLIMeta(path string) kiroCLIMeta {
	raw, err := os.ReadFile(strings.TrimSuffix(path, ".jsonl") + ".json")
	if err != nil {
		return kiroCLIMeta{}
	}
	var meta kiroCLIMeta
	if json.Unmarshal(raw, &meta) != nil {
		return kiroCLIMeta{}
	}
	return meta
}

// --- v2 IDE sessions ------------------------------------------------------

type kiroV2Session struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	ModelID        string   `json:"modelId"`
	WorkspacePaths []string `json:"workspacePaths"`
}

type kiroV2Event struct {
	Timestamp string        `json:"timestamp"`
	Payload   kiroV2Payload `json:"payload"`
}

type kiroV2Payload struct {
	Type                string              `json:"type"`
	ExecutionID         string              `json:"executionId"`
	OperationType       string              `json:"operationType"`
	ToolName            string              `json:"toolName"`
	Name                string              `json:"name"`
	Content             json.RawMessage     `json:"content"`
	PromptTurnSummaries []kiroV2TurnSummary `json:"promptTurnSummaries"`
}

type kiroV2TurnSummary struct {
	UsedTools []string        `json:"usedTools"`
	Usage     json.RawMessage `json:"usage"`
}

// parseV2Session reads an event-sourced v2 IDE log. Turns run from turn_start
// to turn_end and share an executionId; reasoning text is counted separately
// from assistant text.
func parseV2Session(path string, raw []byte) ([]model.UsageEvent, []model.SessionMeta) {
	dir := filepath.Dir(path)
	meta := readV2Meta(dir)
	sessionID := firstNonEmpty(meta.ID, filepath.Base(dir))
	project := defaultIDEProject
	if len(meta.WorkspacePaths) > 0 && strings.TrimSpace(meta.WorkspacePaths[0]) != "" {
		project = meta.WorkspacePaths[0]
	}
	session := kiroSession{id: sessionID, project: project}
	modelID := normalizeKiroModelID(meta.ModelID)

	boundaries := &harness.BoundaryState{}
	events := []model.UsageEvent{}
	seen := make(map[string]bool)
	var (
		inTurn                      bool
		execID                      string
		turnStart                   time.Time
		pendingUserText             string
		turnUserChars               int
		outputChars, reasoningChars int
		toolResultChars             int
		turnCredits                 float64
		sawTool                     bool
	)

	resetTurn := func() {
		inTurn = false
		execID = ""
		turnStart = time.Time{}
		pendingUserText = ""
		turnUserChars, outputChars, reasoningChars, toolResultChars = 0, 0, 0, 0
		turnCredits = 0
		sawTool = false
	}
	flushTurn := func() {
		if !inTurn {
			resetTurn()
			return
		}
		if outputChars > 0 || reasoningChars > 0 || sawTool {
			key := fmt.Sprintf("kiro-v2:%s:%s", sessionID, execID)
			if execID == "" {
				key = fmt.Sprintf("kiro-v2:%s:%d", sessionID, len(events))
			}
			if ts := turnStart; !ts.IsZero() && !seen[key] {
				seen[key] = true
				events = append(events, session.event(key, ts, modelID,
					estimateTokens(turnUserChars+toolResultChars),
					estimateTokens(outputChars),
					estimateTokens(reasoningChars),
					turnCredits, boundaries.Apply(key)))
				session.note(ts)
			}
		}
		resetTurn()
	}

	for _, line := range splitJSONLines(raw) {
		var evt kiroV2Event
		if err := json.Unmarshal(line, &evt); err != nil {
			continue
		}
		payload := evt.Payload
		switch payload.Type {
		case "user":
			if inTurn {
				flushTurn()
			}
			text := extractText(payload.Content)
			if session.title == "" {
				session.title = text
			}
			pendingUserText = text
			turnUserChars = charCount(text)
		case "turn_start":
			if inTurn {
				flushTurn()
			}
			inTurn = true
			execID = payload.ExecutionID
			turnStart = parseKiroTimestampString(evt.Timestamp)
			turnUserChars = charCount(pendingUserText)
			pendingUserText = ""
		case "assistant":
			text := extractText(payload.Content)
			if payload.OperationType == "Reasoning" {
				reasoningChars += charCount(text)
			} else {
				outputChars += charCount(text)
			}
			if !inTurn && charCount(text) > 0 {
				inTurn = true
			}
			if turnStart.IsZero() {
				turnStart = parseKiroTimestampString(evt.Timestamp)
			}
		case "tool_call":
			sawTool = true
			if !inTurn {
				inTurn = true
			}
			if turnStart.IsZero() {
				turnStart = parseKiroTimestampString(evt.Timestamp)
			}
		case "tool_result":
			toolResultChars += charCount(extractText(payload.Content))
		case "usage_summary":
			for _, summary := range payload.PromptTurnSummaries {
				if len(summary.UsedTools) > 0 {
					sawTool = true
				}
				turnCredits += numberValue(summary.Usage)
			}
		case "turn_end":
			flushTurn()
		}
	}
	flushTurn()
	return events, session.metas()
}

// readV2Meta reads the session.json companion of a v2 event log.
func readV2Meta(dir string) kiroV2Session {
	raw, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil {
		return kiroV2Session{}
	}
	var meta kiroV2Session
	if json.Unmarshal(raw, &meta) != nil {
		return kiroV2Session{}
	}
	return meta
}

// --- v1 IDE workspace sessions --------------------------------------------

type kiroWorkspaceSession struct {
	SessionID          string          `json:"sessionId"`
	SelectedModel      string          `json:"selectedModel"`
	WorkspaceDirectory string          `json:"workspaceDirectory"`
	History            []kiroWSHistory `json:"history"`
}

type kiroWSHistory struct {
	ExecutionID string        `json:"executionId"`
	Message     kiroWSMessage `json:"message"`
}

type kiroWSMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// parseWorkspaceSession reads a v1 workspace-session document. Items that
// reference an execution file are counted by that file instead, so a session
// made only of such stubs is skipped.
func parseWorkspaceSession(path string, record map[string]json.RawMessage) ([]model.UsageEvent, []model.SessionMeta) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	var doc kiroWorkspaceSession
	if json.Unmarshal(raw, &doc) != nil || doc.SessionID == "" {
		return nil, nil
	}
	project := firstNonEmpty(doc.WorkspaceDirectory, projectForV1(path))
	session := kiroSession{id: doc.SessionID, project: project}
	modelID := normalizeKiroModelID(doc.SelectedModel)

	inputChars, outputChars := 0, 0
	hasExecutionRefs, hasRealAssistantContent := false, false
	for _, item := range doc.History {
		execBacked := strings.TrimSpace(item.ExecutionID) != ""
		if execBacked {
			hasExecutionRefs = true
		}
		text := extractText(item.Message.Content)
		switch strings.ToLower(strings.TrimSpace(item.Message.Role)) {
		case "user":
			if text == "" {
				continue
			}
			inputChars += charCount(text)
			if session.title == "" {
				session.title = text
			}
		case "assistant":
			if execBacked || text == "" || text == "On it." {
				continue
			}
			outputChars += charCount(text)
			hasRealAssistantContent = true
		}
	}
	if hasExecutionRefs && !hasRealAssistantContent {
		return nil, nil
	}
	if inputChars == 0 && outputChars == 0 {
		return nil, nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, nil
	}
	ts := st.ModTime().UTC()
	key := "kiro:ws-session:" + doc.SessionID
	event := session.event(key, ts, modelID, estimateTokens(inputChars), estimateTokens(outputChars), 0, 0, "")
	session.note(ts)
	return []model.UsageEvent{event}, session.metas()
}

// --- v1 chat files --------------------------------------------------------

type kiroChatFile struct {
	ExecutionID string `json:"executionId"`
	Chat        []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"chat"`
	Metadata struct {
		ModelID    string          `json:"modelId"`
		WorkflowID string          `json:"workflowId"`
		StartTime  json.RawMessage `json:"startTime"`
	} `json:"metadata"`
}

// parseChatFile reads a v1 .chat document: every human turn feeds the input
// estimate, every bot turn the output estimate.
func parseChatFile(path string, raw []byte) ([]model.UsageEvent, []model.SessionMeta) {
	var doc kiroChatFile
	if json.Unmarshal(raw, &doc) != nil {
		return nil, nil
	}
	sessionID := firstNonEmpty(doc.Metadata.WorkflowID, strings.TrimSuffix(filepath.Base(path), ".chat"))
	session := kiroSession{id: sessionID, project: projectForV1(path)}
	modelID := normalizeKiroModelID(doc.Metadata.ModelID)

	inputChars, outputChars := 0, 0
	for _, msg := range doc.Chat {
		switch strings.ToLower(strings.TrimSpace(msg.Role)) {
		case "human":
			if strings.HasPrefix(msg.Content, "<identity>") {
				continue
			}
			inputChars += charCount(msg.Content)
			if session.title == "" {
				session.title = msg.Content
			}
		case "bot":
			if strings.TrimSpace(msg.Content) == "" {
				continue
			}
			outputChars += charCount(msg.Content)
		}
	}
	if outputChars == 0 {
		return nil, nil
	}
	ts := parseKiroTimestamp(doc.Metadata.StartTime)
	if ts.IsZero() {
		return nil, nil
	}
	key := "kiro:" + sessionID + ":" + firstNonEmpty(doc.ExecutionID, filepath.Base(path))
	event := session.event(key, ts, modelID, estimateTokens(inputChars), estimateTokens(outputChars), 0, 0, "")
	session.note(ts)
	return []model.UsageEvent{event}, session.metas()
}

// --- v1 execution documents ----------------------------------------------

// parseModernExecution reads a v1 execution document: a transcript plus an
// optional usage summary. Documents carrying an "executions" array are indexes
// of other documents and are ignored, like codeburn.
func parseModernExecution(path string, raw []byte) ([]model.UsageEvent, []model.SessionMeta) {
	var record map[string]json.RawMessage
	if json.Unmarshal(raw, &record) != nil {
		return nil, nil
	}
	if isArray(record["executions"]) {
		return nil, nil
	}
	modelID := modernExecutionModel(record)
	execID := firstNonEmpty(stringValue(record["executionId"]), stringValue(record["id"]), strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	sessionID := firstNonEmpty(
		stringValue(record["sessionId"]),
		stringValue(record["chatSessionId"]),
		stringValue(record["conversationId"]),
		stringValue(record["workflowId"]),
		metadataString(record, "workflowId", "sessionId"),
		filepath.Base(filepath.Dir(path)),
		execID,
	)
	session := kiroSession{id: sessionID, project: projectForV1(path)}

	inputChars, outputChars := 0, 0
	hasOutputActivity := false
	directInput := extractText(firstRaw(record, "prompt", "input", "userMessage", "user_message", "request"))
	directOutput := extractText(firstRaw(record, "response", "output", "assistantMessage", "assistant_message", "result"))
	if directInput != "" {
		inputChars += charCount(directInput)
		session.title = directInput
	}
	if directOutput != "" {
		outputChars += charCount(directOutput)
		hasOutputActivity = true
	}
	if toolUseRE.MatchString(directOutput) || hasStructuredTools(record) {
		hasOutputActivity = true
	}
	for _, key := range modernConversationKeys {
		for _, item := range conversationArray(record, key) {
			var msg map[string]json.RawMessage
			if json.Unmarshal(item, &msg) != nil {
				continue
			}
			text := extractText(firstRaw(msg, "content", "text", "message", "value", "parts"))
			role := strings.ToLower(strings.TrimSpace(firstNonEmpty(stringValue(msg["role"]), stringValue(msg["type"]), stringValue(msg["author"]))))
			if hasStructuredTools(msg) {
				hasOutputActivity = true
			}
			switch role {
			case "human", "user":
				inputChars += charCount(text)
				if session.title == "" {
					session.title = text
				}
			case "bot", "assistant", "ai", "model":
				outputChars += charCount(text)
				if charCount(text) > 0 {
					hasOutputActivity = true
				}
			case "tool", "system":
				inputChars += charCount(text)
			}
		}
	}
	credits := 0.0
	if entries := usageSummary(record); len(entries) > 0 {
		for _, entry := range entries {
			credits += numberValue(entry["usage"])
			if isArray(entry["usedTools"]) {
				hasOutputActivity = true
			}
		}
	}
	if !hasOutputActivity {
		return nil, nil
	}
	ts := parseKiroTimestamp(firstRaw(record, "startTime", "createdAt", "timestamp"))
	if ts.IsZero() {
		ts = metadataTimestamp(record)
	}
	if ts.IsZero() {
		return nil, nil
	}
	key := "kiro:" + sessionID + ":" + execID
	event := session.event(key, ts, modelID, estimateTokens(inputChars), estimateTokens(outputChars), 0, credits, "")
	session.note(ts)
	return []model.UsageEvent{event}, session.metas()
}

// modernExecutionModel resolves the model id from the document or its metadata.
func modernExecutionModel(record map[string]json.RawMessage) string {
	id := stringValue(firstRaw(record, "modelId", "modelID", "modelName"))
	if id == "" {
		if raw := record["model"]; len(raw) > 0 {
			var obj map[string]json.RawMessage
			if json.Unmarshal(raw, &obj) == nil {
				id = firstNonEmpty(stringValue(obj["id"]), stringValue(obj["name"]))
			} else {
				id = stringValue(raw)
			}
		}
	}
	if id == "" {
		id = metadataString(record, "modelId", "modelID", "modelName")
	}
	return normalizeKiroModelID(id)
}

// conversationArray returns the transcript array stored at data.context[key] or
// data[key].
func conversationArray(record map[string]json.RawMessage, key string) []json.RawMessage {
	if context, ok := record["context"]; ok {
		var obj map[string]json.RawMessage
		if json.Unmarshal(context, &obj) == nil {
			if arr := rawArray(obj[key]); arr != nil {
				return arr
			}
		}
	}
	return rawArray(record[key])
}

// usageSummary returns the document's usage summary entries.
func usageSummary(record map[string]json.RawMessage) []map[string]json.RawMessage {
	arr := rawArray(record["usageSummary"])
	out := make([]map[string]json.RawMessage, 0, len(arr))
	for _, item := range arr {
		var entry map[string]json.RawMessage
		if json.Unmarshal(item, &entry) == nil {
			out = append(out, entry)
		}
	}
	return out
}

// hasStructuredTools reports whether a record or message carries tool calls.
func hasStructuredTools(record map[string]json.RawMessage) bool {
	if name := firstNonEmpty(stringValue(record["toolName"]), stringValue(record["tool_name"])); name != "" {
		return true
	}
	for _, key := range []string{"toolCalls", "tool_calls", "tools"} {
		if arr := rawArray(record[key]); len(arr) > 0 {
			return true
		}
	}
	return false
}

// metadataTimestamp reads a timestamp out of the record's metadata object.
func metadataTimestamp(record map[string]json.RawMessage) time.Time {
	raw := metadataRaw(record, "startTime", "createdAt", "timestamp")
	return parseKiroTimestamp(raw)
}

// metadataString reads a string field out of the record's metadata object.
func metadataString(record map[string]json.RawMessage, keys ...string) string {
	return stringValue(metadataRaw(record, keys...))
}

// metadataRaw returns the first present key of the record's metadata object.
func metadataRaw(record map[string]json.RawMessage, keys ...string) json.RawMessage {
	var meta map[string]json.RawMessage
	if json.Unmarshal(record["metadata"], &meta) != nil {
		return nil
	}
	return firstRaw(meta, keys...)
}

// --- v1 project resolution ------------------------------------------------

// projectForV1 resolves the workspace a v1 document belongs to: the
// workspace.json folder behind the enclosing 32-character workspace hash, or
// the base64 workspace path for workspace-session documents. It falls back to
// the enclosing directory name.
func projectForV1(path string) string {
	dir := filepath.Dir(path)
	if base := filepath.Base(dir); strings.EqualFold(filepath.Base(filepath.Dir(dir)), workspaceSessionsDirName) {
		if decoded := decodeWorkspaceName(base); decoded != "" {
			return decoded
		}
	}
	hash := ""
	for probe := dir; ; {
		name := filepath.Base(probe)
		if hash32RE.MatchString(name) {
			hash = name
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	if hash == "" {
		return filepath.Base(dir)
	}
	for _, agentDir := range defaultAgentDirs() {
		storage := filepath.Join(filepath.Dir(filepath.Dir(agentDir)), workspaceStorageDirName)
		if project := workspaceFolder(filepath.Join(storage, hash)); project != "" {
			return project
		}
	}
	return hash
}

// workspaceFolder reads the folder a v1 workspace.json points at.
func workspaceFolder(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "workspace.json"))
	if err != nil {
		return ""
	}
	var doc struct {
		Folder string `json:"folder"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.Folder == "" {
		return ""
	}
	folder := strings.TrimPrefix(doc.Folder, "file://")
	if decoded, err := url.PathUnescape(folder); err == nil {
		folder = decoded
	}
	folder = strings.TrimSuffix(folder, "/")
	if folder == "" {
		return ""
	}
	return folder
}

// decodeWorkspaceName base64-decodes a workspace-sessions directory name.
func decodeWorkspaceName(name string) string {
	normalized := strings.ReplaceAll(name, "_", "=")
	decoded, err := base64.StdEncoding.DecodeString(normalized)
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(string(decoded))
	if text == "" {
		return ""
	}
	return strings.TrimSuffix(text, "/")
}

// --- shared helpers -------------------------------------------------------

// kiroSession accumulates the metadata of the session a source belongs to.
type kiroSession struct {
	id      string
	title   string
	project string
	started time.Time
	updated time.Time
}

// note folds an event timestamp into the session window.
func (s *kiroSession) note(ts time.Time) {
	if s.started.IsZero() || ts.Before(s.started) {
		s.started = ts
	}
	if ts.After(s.updated) {
		s.updated = ts
	}
}

// event builds a usage event for this session.
func (s *kiroSession) event(key string, ts time.Time, modelID string, input, output, reasoning int64, credits float64, boundary model.Boundary) model.UsageEvent {
	event := model.UsageEvent{
		Harness:     model.Kiro,
		DedupKey:    key,
		SessionID:   s.id,
		ProjectPath: s.project,
		Timestamp:   ts,
		Model:       modelID,
		Provider:    providerName,
		Tokens:      model.Tokens{Input: input, Output: output, Reasoning: reasoning},
		Boundary:    boundary,
	}
	if credits > 0 {
		event.Bill = &model.Bill{Amount: credits, Unit: creditsUnit}
	}
	return event
}

// metas returns the session row, or nil when the source produced no events.
func (s *kiroSession) metas() []model.SessionMeta {
	if s.id == "" || s.started.IsZero() {
		return nil
	}
	return []model.SessionMeta{{
		Harness:   model.Kiro,
		SessionID: s.id,
		Title:     harness.Title(s.title),
		Project:   s.project,
		StartedAt: s.started,
		UpdatedAt: s.updated,
	}}
}

// normalizeKiroModelID dashes dotted versions and maps "auto" to kiro-auto.
func normalizeKiroModelID(id string) string {
	id = strings.TrimSpace(id)
	id = modelVersionRE.ReplaceAllString(id, "$1-$2")
	if id == "" || strings.EqualFold(id, "auto") {
		return autoModelID
	}
	return id
}

// estimateTokens mirrors codeburn's ceil(chars / 4) transcript estimate.
func estimateTokens(chars int) int64 {
	if chars <= 0 {
		return 0
	}
	return int64((chars + charsPerToken - 1) / charsPerToken)
}

// charCount counts characters the way codeburn's string length does.
func charCount(s string) int { return utf8.RuneCountInString(s) }

// extractText flattens a JSON value into text: strings as-is, arrays joined by
// newlines, and objects by their first text-bearing field.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return extractTextValue(value)
}

func extractTextValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if text := extractTextValue(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		for _, key := range []string{"content", "text", "message", "value", "parts", "entries", "data"} {
			if item, ok := v[key]; ok {
				if text := extractTextValue(item); text != "" {
					return text
				}
			}
		}
	}
	return ""
}

// stringValue reads a JSON string.
func stringValue(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return ""
	}
	return text
}

// numberValue reads a non-negative finite number.
func numberValue(raw json.RawMessage) float64 {
	if len(raw) == 0 {
		return 0
	}
	var value float64
	if json.Unmarshal(raw, &value) != nil {
		return 0
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0
	}
	if value > maxSafeInteger {
		value = maxSafeInteger
	}
	return value
}

// firstRaw returns the first present field of a record.
func firstRaw(record map[string]json.RawMessage, keys ...string) json.RawMessage {
	for _, key := range keys {
		if raw, ok := record[key]; ok && len(raw) > 0 {
			return raw
		}
	}
	return nil
}

// rawArray decodes a JSON array of raw values (nil when the value is not one).
func rawArray(raw json.RawMessage) []json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return nil
	}
	return arr
}

// isArray reports whether a JSON value is an array.
func isArray(raw json.RawMessage) bool { return rawArray(raw) != nil }

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// splitJSONLines returns the non-blank lines of a JSONL document.
func splitJSONLines(raw []byte) [][]byte {
	var out [][]byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if line = bytes.TrimSpace(line); len(line) > 0 {
			out = append(out, line)
		}
	}
	return out
}

// parseKiroTimestamp accepts a numeric (seconds or milliseconds) or textual
// timestamp, and drops values below codeburn's reasonable-timestamp floor.
func parseKiroTimestamp(raw json.RawMessage) time.Time {
	if len(raw) == 0 {
		return time.Time{}
	}
	var number float64
	if err := json.Unmarshal(raw, &number); err == nil {
		return timeFromMillis(kiroMillis(number))
	}
	return parseKiroTimestampString(stringValue(raw))
}

// parseKiroTimestampString is parseKiroTimestamp for string fields.
func parseKiroTimestampString(text string) time.Time {
	text = strings.TrimSpace(text)
	if text == "" {
		return time.Time{}
	}
	if number, err := strconv.ParseFloat(text, 64); err == nil {
		return timeFromMillis(kiroMillis(number))
	}
	ts, err := time.Parse(time.RFC3339Nano, text)
	if err != nil || ts.UnixMilli() < int64(minReasonableMS) {
		return time.Time{}
	}
	return ts.UTC()
}

// kiroMillis normalizes a numeric timestamp to milliseconds: values below the
// reasonable-timestamp floor are read as seconds.
func kiroMillis(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return 0
	}
	if value < minReasonableMS {
		value *= 1000
	}
	if value < minReasonableMS {
		return 0
	}
	return value
}

func timeFromMillis(ms float64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(ms)).UTC()
}

// fingerprint hashes the first 4KB of a file.
func fingerprint(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	buf := make([]byte, fingerprintBytes)
	n, err := io.ReadFull(file, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", err
	}
	sum := sha1.Sum(buf[:n])
	return hex.EncodeToString(sum[:]), nil
}
