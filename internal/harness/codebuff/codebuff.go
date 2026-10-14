// Package codebuff parses Codebuff (formerly Manicode) chat transcripts.
//
// Codebuff keeps one directory per chat under a channel root:
//
//	<channel>/projects/<project>/chats/<chatId>/chat-messages.json
//	<channel>/projects/<project>/chats/<chatId>/run-state.json
//
// The channels are ~/.config/manicode (stable), ~/.config/manicode-dev and
// ~/.config/manicode-staging. $CODEBUFF_DATA_DIR replaces all three with a
// single root; MYTOKEN_CODEBUFF_DIRS (filepath.SplitList) replaces the root
// list outright. A root may be a channel directory itself or a directory that
// contains the channel directories (~/.config).
//
// chat-messages.json is a JSON array that Codebuff rewrites in place, so Parse
// re-derives the whole chat whenever the file's size, mtime or 4KB fingerprint
// changes and relies on stable DedupKeys; an unchanged file yields no events.
//
// Mapping (docs/HARNESS.md §4, docs/SPEC.md §4):
//
//   - SessionID is the channel-qualified chat id, because chat ids are ISO
//     timestamps that legitimately repeat across channels. Project is
//     basename(cwd) from run-state.json (falling back to the project directory
//     name) and ProjectPath is that cwd.
//   - Title is the first user message, normalized by harness.Title.
//   - Model resolution: metadata.model, metadata.modelId, metadata.codebuff.model,
//     the model stashed on the last assistant entry of
//     runState.sessionState.mainAgentState.messageHistory, then
//     "codebuff-<agentType>", then "codebuff".
//   - Usage comes from metadata.usage / metadata.codebuff.usage, else from the
//     same stashed history entry. Input/Output/CacheRead/CacheWrite follow
//     Codebuff's own field names (codeburn normalizeUsage). Input is reduced by
//     cache read+write per SPEC §4 unless the record uses Anthropic-native
//     input_tokens, which is already exclusive. Codebuff records no reasoning
//     tokens, so Reasoning stays 0.
//   - credits are Codebuff's own recorded per-message charge, so they become
//     Bill{Unit: "credits"}. They are never converted to USD: codeburn's
//     credits*$0.01 is that project's conservative estimate, not a recorded
//     charge, and MyToken does not store estimates.
//   - RequestID stays empty: the format carries no upstream request/response
//     id, and msg.id is a local message id (SPEC §4 forbids using one). No
//     compact boundary and no parent/child relationship are recorded either.
//   - DedupKey is codebuff:<sessionID>:<message id>, falling back to the
//     message's array index (the array is append-only, so an index is stable).
//
// This parser has NOT been validated against real Codebuff logs: the fixture
// under testdata/conformance/codebuff is synthetic, built from codeburn's
// documented format (docs/providers/codebuff.md) and its test fixtures. The
// field mapping follows codeburn's providers/codebuff.ts
// (https://github.com/getagentseal/codeburn, MIT — see THIRD_PARTY_NOTICES).
package codebuff

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

const (
	kindJSON         = "json"
	rootsEnv         = "MYTOKEN_CODEBUFF_DIRS"
	dataDirEnv       = "CODEBUFF_DATA_DIR"
	chatMessagesName = "chat-messages.json"
	runStateName     = "run-state.json"
	projectsDirName  = "projects"
	chatsDirName     = "chats"
	creditsUnit      = "credits"

	fingerprintBytes = 4096
	readBuffer       = 64 << 10
)

// channelNames are the channel directories Codebuff writes to.
var channelNames = []string{"manicode", "manicode-dev", "manicode-staging"}

// Parser implements harness.Parser for Codebuff chat transcripts.
type Parser struct {
	roots []string
	// prov recomputes this parser's default-root provenance from the current
	// environment. It is set only on the registry singleton; a parser built for
	// explicit roots reports those roots instead.
	prov func() ([]harness.Root, error)
}

var _ harness.Parser = (*Parser)(nil)

func init() {
	harness.Register(New())
}

// New returns a parser watching Codebuff's default channel roots.
func New() harness.Parser {
	p := NewWithRoots(defaultRoots()...).(*Parser)
	p.prov = rootProvenance
	return p
}

// NewWithRoot returns a parser for a single root.
func NewWithRoot(root string) harness.Parser {
	return NewWithRoots(root)
}

// NewWithRoots returns a parser for the given roots. Empty entries are
// dropped and duplicates are removed.
func NewWithRoots(roots ...string) harness.Parser {
	if len(roots) == 0 {
		roots = defaultRoots()
	}
	seen := make(map[string]bool, len(roots))
	out := make([]string, 0, len(roots))
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
		out = append(out, root)
	}
	return &Parser{roots: out}
}

// rootProvenance reports Codebuff's default roots and where each came from.
// MYTOKEN_CODEBUFF_DIRS replaces the whole set, CODEBUFF_DATA_DIR is an explicit
// harness-specific override, and the channel directories are platform defaults.
func rootProvenance() ([]harness.Root, error) {
	if v, ok := harness.Getenv(rootsEnv); ok {
		dirs, err := harness.SplitEnvList(rootsEnv, v)
		if err != nil {
			return nil, err
		}
		return harness.EnvRoots(rootsEnv, dirs...), nil
	}
	if dir, ok := harness.Getenv(dataDirEnv); ok {
		return harness.EnvRoots(dataDirEnv, dir), nil
	}
	config := filepath.Join(harness.Home(), ".config")
	out := make([]string, 0, len(channelNames))
	for _, name := range channelNames {
		out = append(out, filepath.Join(config, name))
	}
	return harness.BuiltinRoots(out...), nil
}

// defaultRoots returns $MYTOKEN_CODEBUFF_DIRS, else $CODEBUFF_DATA_DIR, else the
// three channel directories under ~/.config. A malformed explicit override
// yields no roots: it is never replaced by the built-in default.
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

// Harness identifies this parser.
func (p *Parser) Harness() model.Harness { return model.Codebuff }

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

// Roots returns the configured roots.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

// Discover finds every chat-messages.json below the configured roots.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	seen := map[string]bool{}
	var sources []harness.Source
	for _, root := range p.roots {
		for _, channel := range channelDirs(root) {
			projects := filepath.Join(channel, projectsDirName)
			_ = filepath.WalkDir(projects, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() || d.Name() != chatMessagesName {
					return nil
				}
				path = filepath.Clean(path)
				if seen[path] {
					return nil
				}
				seen[path] = true
				sources = append(sources, harness.Source{Path: path, Kind: kindJSON})
				return nil
			})
		}
	}
	sort.Slice(sources, func(i, j int) bool {
		return filepath.ToSlash(sources[i].Path) < filepath.ToSlash(sources[j].Path)
	})
	return sources, nil
}

// channelDirs returns the channel directories below root: root itself when it
// holds a projects/ directory, plus the known channel names under root.
func channelDirs(root string) []string {
	seen := map[string]bool{}
	var dirs []string
	add := func(dir string) {
		dir = filepath.Clean(dir)
		if seen[dir] {
			return
		}
		st, err := os.Stat(filepath.Join(dir, projectsDirName))
		if err != nil || !st.IsDir() {
			return
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}
	add(root)
	for _, name := range channelNames {
		add(filepath.Join(root, name))
	}
	return dirs
}

// Parse reads one chat transcript.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if src.Kind != "" && src.Kind != kindJSON {
		return harness.Batch{Next: cur}, nil
	}
	st, err := os.Stat(src.Path)
	if err != nil || st.IsDir() {
		return harness.Batch{Next: cur}, nil
	}
	fingerprint, err := fileFingerprint(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	if cur.Fingerprint == fingerprint && cur.Size == st.Size() && cur.ModTime.Equal(st.ModTime()) {
		return harness.Batch{Next: cur}, nil
	}
	next := harness.Cursor{Size: st.Size(), ModTime: st.ModTime(), Fingerprint: fingerprint}

	raw, err := os.ReadFile(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	var messages []chatMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		// A half-written array is retried on the next scan instead of being
		// consumed: keep the old cursor.
		return harness.Batch{Next: cur}, nil
	}

	chatDir := filepath.Dir(src.Path)
	channel, projectName, chatID := sessionIdentity(src.Path)
	sessionID := chatID
	if channel != "" {
		sessionID = channel + "/" + chatID
	}

	runState := readRunState(chatDir)
	cwd := extractCwd(runState)
	project := strings.TrimSpace(filepath.Base(cwd))
	if cwd == "" || project == "." || project == string(filepath.Separator) {
		project = projectName
	}
	fallbackTS := timestampFromChatID(chatID)

	var (
		events    []model.UsageEvent
		title     string
		startedAt time.Time
		updatedAt time.Time
	)
	for i, msg := range messages {
		variant := strings.ToLower(strings.TrimSpace(firstNonEmpty(msg.Variant, msg.Role)))
		ts := coerceTimestamp(msg.Timestamp)
		if ts.IsZero() && msg.Metadata != nil {
			ts = coerceTimestamp(msg.Metadata.Timestamp)
		}
		if ts.IsZero() {
			ts = fallbackTS
		}
		if !ts.IsZero() {
			if startedAt.IsZero() || ts.Before(startedAt) {
				startedAt = ts
			}
			if ts.After(updatedAt) {
				updatedAt = ts
			}
		}
		if variant == "user" {
			if title == "" {
				title = harness.Title(msg.Content)
			}
			continue
		}
		switch variant {
		case "ai", "agent", "assistant":
		default:
			continue
		}

		meta := msg.Metadata
		stashed := stashedUsage{}
		if meta != nil && meta.RunState != nil {
			stashed = stashedFromHistory(meta.RunState)
		}
		usage := usageTotals{}
		if meta != nil {
			usage = parseUsage(meta.Usage)
			if usage.isZero() && meta.Codebuff != nil {
				usage = parseUsage(meta.Codebuff.Usage)
			}
		}
		if usage.isZero() {
			usage = stashed.usage
		}
		credits, hasCredits := numberFromRaw(msg.Credits)
		if hasCredits && credits < 0 {
			credits, hasCredits = 0, false
		}
		if usage.isZero() && !hasCredits {
			// Mode dividers and empty framing blocks carry no usage at all.
			continue
		}
		if ts.IsZero() {
			continue
		}

		key := strings.TrimSpace(msg.ID)
		if key == "" {
			key = strconv.Itoa(i)
		}
		event := model.UsageEvent{
			Harness:     model.Codebuff,
			DedupKey:    "codebuff:" + sessionID + ":" + key,
			SessionID:   sessionID,
			ProjectPath: cwd,
			Timestamp:   ts.UTC(),
			Model:       resolveModel(meta, stashed),
			Tokens: model.Tokens{
				Input:      usage.input,
				Output:     usage.output,
				CacheRead:  usage.cacheRead,
				CacheWrite: usage.cacheWrite,
			},
		}
		if hasCredits {
			event.Bill = &model.Bill{Amount: credits, Unit: creditsUnit}
		}
		events = append(events, event)
	}

	sort.Slice(events, func(i, j int) bool {
		if !events[i].Timestamp.Equal(events[j].Timestamp) {
			return events[i].Timestamp.Before(events[j].Timestamp)
		}
		return events[i].DedupKey < events[j].DedupKey
	})

	sessions := []model.SessionMeta{{
		Harness:   model.Codebuff,
		SessionID: sessionID,
		Title:     title,
		Project:   project,
		StartedAt: startedAt.UTC(),
		UpdatedAt: updatedAt.UTC(),
	}}
	return harness.Batch{Events: events, Sessions: sessions, Next: next}, nil
}

// chatMessage is one entry of chat-messages.json.
type chatMessage struct {
	ID        string          `json:"id"`
	Variant   string          `json:"variant"`
	Role      string          `json:"role"`
	Content   string          `json:"content"`
	Timestamp json.RawMessage `json:"timestamp"`
	Credits   json.RawMessage `json:"credits"`
	Metadata  *chatMetadata   `json:"metadata"`
}

type chatMetadata struct {
	Model     string          `json:"model"`
	ModelID   string          `json:"modelId"`
	Timestamp json.RawMessage `json:"timestamp"`
	Usage     json.RawMessage `json:"usage"`
	Codebuff  *codebuffBlock  `json:"codebuff"`
	RunState  *runState       `json:"runState"`
}

type codebuffBlock struct {
	Model string          `json:"model"`
	Usage json.RawMessage `json:"usage"`
}

type runState struct {
	SessionState *sessionState `json:"sessionState"`
	FileContext  *cwdHolder    `json:"fileContext"`
	Cwd          string        `json:"cwd"`
}

type sessionState struct {
	ProjectContext *cwdHolder      `json:"projectContext"`
	Cwd            string          `json:"cwd"`
	MainAgentState *mainAgentState `json:"mainAgentState"`
}

type cwdHolder struct {
	Cwd string `json:"cwd"`
}

type mainAgentState struct {
	AgentType      string         `json:"agentType"`
	MessageHistory []historyEntry `json:"messageHistory"`
}

type historyEntry struct {
	Role            string           `json:"role"`
	ProviderOptions *providerOptions `json:"providerOptions"`
}

type providerOptions struct {
	Codebuff *codebuffBlock `json:"codebuff"`
}

// usageTotals is one usage object after normalization.
type usageTotals struct {
	input      int64
	output     int64
	cacheRead  int64
	cacheWrite int64
	// exclusive is true when the record used Anthropic-native input_tokens,
	// which already excludes cached tokens.
	exclusive bool
}

func (u usageTotals) isZero() bool {
	return u.input == 0 && u.output == 0 && u.cacheRead == 0 && u.cacheWrite == 0
}

// parseUsage normalizes one Codebuff usage object. The alias list mirrors
// codeburn's normalizeUsage (src/providers/codebuff.ts:144-163).
func parseUsage(raw json.RawMessage) usageTotals {
	obj := decodeObject(raw)
	if obj == nil {
		return usageTotals{}
	}
	input, inputKey := firstNumber(obj, "inputTokens", "input_tokens", "promptTokens", "prompt_tokens")
	output, _ := firstNumber(obj, "outputTokens", "output_tokens", "completionTokens", "completion_tokens")
	cacheRead, _ := firstNumber(obj, "cacheReadInputTokens", "cache_read_input_tokens",
		"promptTokensDetails.cachedTokens", "prompt_tokens_details.cached_tokens")
	cacheWrite, _ := firstNumber(obj, "cacheCreationInputTokens", "cache_creation_input_tokens")
	u := usageTotals{input: input, output: output, cacheRead: cacheRead, cacheWrite: cacheWrite}
	// inputTokens/promptTokens/prompt_tokens are totals that include the cached
	// part; SPEC §4 wants Input without cache read/write.
	u.exclusive = inputKey == "input_tokens"
	if !u.exclusive {
		u.input = maxInt64(u.input-u.cacheRead-u.cacheWrite, 0)
	}
	return u
}

// stashedUsage is the usage Codebuff leaves on the last assistant entry of
// runState.sessionState.mainAgentState.messageHistory.
type stashedUsage struct {
	model string
	usage usageTotals
}

func stashedFromHistory(rs *runState) stashedUsage {
	if rs == nil || rs.SessionState == nil || rs.SessionState.MainAgentState == nil {
		return stashedUsage{}
	}
	history := rs.SessionState.MainAgentState.MessageHistory
	for i := len(history) - 1; i >= 0; i-- {
		entry := history[i]
		if !strings.EqualFold(strings.TrimSpace(entry.Role), "assistant") ||
			entry.ProviderOptions == nil || entry.ProviderOptions.Codebuff == nil {
			continue
		}
		usage := parseUsage(entry.ProviderOptions.Codebuff.Usage)
		if usage.isZero() {
			continue
		}
		return stashedUsage{model: entry.ProviderOptions.Codebuff.Model, usage: usage}
	}
	return stashedUsage{}
}

// resolveModel walks Codebuff's model fallback chain.
func resolveModel(meta *chatMetadata, stashed stashedUsage) string {
	if meta != nil {
		if m := strings.TrimSpace(meta.Model); m != "" {
			return m
		}
		if m := strings.TrimSpace(meta.ModelID); m != "" {
			return m
		}
		if meta.Codebuff != nil {
			if m := strings.TrimSpace(meta.Codebuff.Model); m != "" {
				return m
			}
		}
	}
	if m := strings.TrimSpace(stashed.model); m != "" {
		return m
	}
	if meta != nil && meta.RunState != nil && meta.RunState.SessionState != nil &&
		meta.RunState.SessionState.MainAgentState != nil {
		if agentType := strings.TrimSpace(meta.RunState.SessionState.MainAgentState.AgentType); agentType != "" {
			return "codebuff-" + agentType
		}
	}
	return "codebuff"
}

// extractCwd mirrors codeburn's extractCwd (src/providers/codebuff.ts:180-190).
func extractCwd(rs *runState) string {
	if rs == nil {
		return ""
	}
	if rs.SessionState != nil {
		if rs.SessionState.ProjectContext != nil && rs.SessionState.ProjectContext.Cwd != "" {
			return rs.SessionState.ProjectContext.Cwd
		}
		if rs.FileContext != nil && rs.FileContext.Cwd != "" {
			return rs.FileContext.Cwd
		}
		if rs.SessionState.Cwd != "" {
			return rs.SessionState.Cwd
		}
	}
	return rs.Cwd
}

// readRunState loads the sibling run-state.json; a missing or malformed file
// just means the chat falls back to its directory name.
func readRunState(chatDir string) *runState {
	raw, err := os.ReadFile(filepath.Join(chatDir, runStateName))
	if err != nil {
		return nil
	}
	var rs runState
	if err := json.Unmarshal(raw, &rs); err != nil {
		return nil
	}
	return &rs
}

// sessionIdentity derives the channel, project directory name and chat id from
// a chat-messages.json path. The channel is only reported when the path names
// one of Codebuff's channels, so session ids stay stable for custom roots.
func sessionIdentity(path string) (channel, project, chatID string) {
	chatDir := filepath.Dir(path)
	chatID = filepath.Base(chatDir)
	chatsDir := filepath.Dir(chatDir)
	projectDir := filepath.Dir(chatsDir)
	projectsDir := filepath.Dir(projectDir)
	if filepath.Base(chatsDir) == chatsDirName && filepath.Base(projectsDir) == projectsDirName {
		project = filepath.Base(projectDir)
		if name := filepath.Base(filepath.Dir(projectsDir)); isChannel(name) {
			channel = name
		}
	}
	if project == "" {
		project = filepath.Base(filepath.Dir(filepath.Dir(chatDir)))
	}
	return channel, project, chatID
}

func isChannel(name string) bool {
	for _, channel := range channelNames {
		if name == channel {
			return true
		}
	}
	return false
}

// chatIDTimeRe rewrites the chat id's dashed clock ("2026-04-14T10-00-00.000Z")
// into an ISO timestamp.
var chatIDTimeRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})T(\d{2})-(\d{2})-(\d{2})(.*)$`)

func timestampFromChatID(chatID string) time.Time {
	m := chatIDTimeRe.FindStringSubmatch(chatID)
	if m == nil {
		return time.Time{}
	}
	return parseTimestampString(m[1] + "T" + m[2] + ":" + m[3] + ":" + m[4] + m[5])
}

// coerceTimestamp accepts the epoch-millisecond numbers and timestamp strings
// Codebuff writes.
func coerceTimestamp(raw json.RawMessage) time.Time {
	if len(raw) == 0 {
		return time.Time{}
	}
	var number float64
	if err := json.Unmarshal(raw, &number); err == nil {
		if math.IsNaN(number) || math.IsInf(number, 0) || number <= 0 {
			return time.Time{}
		}
		return time.UnixMilli(int64(math.Round(number))).UTC()
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return time.Time{}
	}
	return parseTimestampString(text)
}

var timestampLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func parseTimestampString(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	for _, layout := range timestampLayouts {
		if ts, err := time.Parse(layout, value); err == nil {
			return ts.UTC()
		}
	}
	return time.Time{}
}

// numberFromRaw reads a JSON number, numeric string or null.
func numberFromRaw(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var number float64
	if err := json.Unmarshal(raw, &number); err == nil {
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return 0, false
		}
		return number, true
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, false
	}
	number, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, false
	}
	return number, true
}

func decodeObject(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	if len(obj) == 0 {
		return nil
	}
	return obj
}

// firstNumber returns the first present key (dotted paths allowed) with its
// name.
func firstNumber(obj map[string]any, keys ...string) (int64, string) {
	for _, key := range keys {
		if value, ok := lookupNumber(obj, key); ok {
			return value, key
		}
	}
	return 0, ""
}

func lookupNumber(obj map[string]any, path string) (int64, bool) {
	var current any = obj
	for _, part := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return 0, false
		}
		current, ok = object[part]
		if !ok {
			return 0, false
		}
	}
	switch value := current.(type) {
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return 0, false
		}
		if value < 0 {
			return 0, true
		}
		return int64(math.Round(value)), true
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
			return 0, false
		}
		if number < 0 {
			return 0, true
		}
		return int64(math.Round(number)), true
	}
	return 0, false
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// fileFingerprint is the sha1 of the first 4KB, used to notice in-place
// rewrites that do not change the file size.
func fileFingerprint(path string) (string, error) {
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
