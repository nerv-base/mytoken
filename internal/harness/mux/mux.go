// Package mux parses coder/mux chat transcripts.
//
// Mux keeps one append-only JSONL transcript per workspace, with subagent
// transcripts nested inside the workspace they belong to:
//
//	<root>/sessions/<workspaceID>/chat.jsonl
//	<root>/sessions/<workspaceID>/subagent-transcripts/<childTaskID>/chat.jsonl
//	<root>/config.json
//
// Roots come from MYTOKEN_MUX_DIRS (filepath.SplitList), else $CODEBURN_MUX_DIR,
// else $MUX_ROOT, else ~/.mux plus the ~/.mux-dev and ~/.cmux directories the
// upstream docs list.
//
// Mapping (docs/HARNESS.md §4, docs/SPEC.md §4):
//
//   - SessionID is the workspace id for a workspace transcript and the child
//     task id for a subagent transcript, whose ParentID is that workspace.
//   - Project and ProjectPath come from config.json's project -> workspace map
//     (basename(projectPath), and projectPath itself). A subagent transcript
//     uses its parent workspace for that lookup, and a workspace missing from
//     the map falls back to its id and no project path.
//   - Title is the session's first user message, reported only by the batch that
//     first sees one (cursor Extra remembers it), so later incremental batches
//     never overwrite the title with a later user message.
//   - Usage: upstream documents that inputTokens includes cache read and cache
//     creation, and that outputTokens includes reasoning. Input therefore
//     subtracts cachedInputTokens and anthropic cacheCreationInputTokens, Output
//     subtracts reasoningTokens, and Reasoning carries reasoningTokens; every
//     value is clamped at zero.
//   - Model is metadata.model without its "provider:" prefix; Provider is that
//     prefix when the log records one.
//   - RequestID stays empty: chat.jsonl has no upstream request/response id, and
//     message ids are local (SPEC §4 forbids using one). No compact boundary and
//     no base URL are recorded.
//   - DedupKey is mux:<sessionID>:<message id>; a message without an id falls
//     back to a sha1 of its raw line, which stays stable when the append-only
//     file is reread.
//   - session-usage.json and analytics/ are derived from the same usage as
//     chat.jsonl, so they are deliberately not read (reading them double counts).
//
// This parser has NOT been validated against real mux data: the fixture under
// testdata/conformance/mux is synthetic, built from codeburn's format docs
// (docs/providers/mux.md) and its test fixtures. The field mapping follows
// codeburn's providers/mux.ts
// (https://github.com/getagentseal/codeburn, MIT — see THIRD_PARTY_NOTICES).
package mux

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

const (
	kindJSONL         = "jsonl"
	rootsEnv          = "MYTOKEN_MUX_DIRS"
	dirEnv            = "CODEBURN_MUX_DIR"
	rootEnv           = "MUX_ROOT"
	sessionsDirName   = "sessions"
	subagentDirName   = "subagent-transcripts"
	chatFileName      = "chat.jsonl"
	configFileName    = "config.json"
	titleFallbackLen  = 500
	fingerprintBytes  = 4096
	readBuffer        = 64 << 10
	maxSessionMessage = 500
)

// Parser implements harness.Parser for mux chat transcripts.
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

// New returns a parser watching mux's default roots.
func New() harness.Parser {
	p := NewWithRoots(defaultRoots()...).(*Parser)
	p.prov = rootProvenance
	return p
}

// NewWithRoot returns a parser for a single root.
func NewWithRoot(root string) harness.Parser {
	return NewWithRoots(root)
}

// NewWithRoots returns a parser for the given roots. Empty entries are dropped
// and duplicates are removed.
func NewWithRoots(roots ...string) harness.Parser {
	if len(roots) == 0 {
		roots = defaultRoots()
	}
	seen := make(map[string]bool, len(roots))
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		root = expandHome(strings.TrimSpace(root))
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

// rootProvenance reports mux's default roots and where each came from.
// MYTOKEN_MUX_DIRS replaces the whole set and CODEBURN_MUX_DIR/MUX_ROOT are
// explicit harness-specific bases; the ~/.mux* directories are defaults.
func rootProvenance() ([]harness.Root, error) {
	if v, ok := harness.Getenv(rootsEnv); ok {
		dirs, err := harness.SplitEnvList(rootsEnv, v)
		if err != nil {
			return nil, err
		}
		return harness.EnvRoots(rootsEnv, dirs...), nil
	}
	for _, key := range []string{dirEnv, rootEnv} {
		if dir, ok := harness.Getenv(key); ok {
			return harness.EnvRoots(key, expandHome(dir)), nil
		}
	}
	home := harness.Home()
	return harness.BuiltinRoots(
		filepath.Join(home, ".mux"),
		filepath.Join(home, ".mux-dev"),
		filepath.Join(home, ".cmux"),
	), nil
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

// Harness identifies this parser.
func (p *Parser) Harness() model.Harness { return model.Mux }

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

// Discover finds every chat.jsonl below the configured roots, including the
// nested subagent transcripts.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	seen := map[string]bool{}
	var sources []harness.Source
	for _, root := range p.roots {
		sessions := filepath.Join(root, sessionsDirName)
		_ = filepath.WalkDir(sessions, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || d.Name() != chatFileName {
				return nil
			}
			path = filepath.Clean(path)
			if seen[path] {
				return nil
			}
			seen[path] = true
			sources = append(sources, harness.Source{Path: path, Kind: kindJSONL})
			return nil
		})
	}
	sort.Slice(sources, func(i, j int) bool {
		return filepath.ToSlash(sources[i].Path) < filepath.ToSlash(sources[j].Path)
	})
	return sources, nil
}

// Parse reads the new complete lines of one transcript.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if src.Kind != "" && src.Kind != kindJSONL {
		return harness.Batch{Next: cur}, nil
	}
	file, err := os.Open(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil || st.IsDir() {
		return harness.Batch{Next: cur}, nil
	}
	fingerprint, err := fileFingerprint(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	offset := cur.Offset
	rewritten := cur.Fingerprint != "" && cur.Fingerprint != fingerprint
	if rewritten || offset > st.Size() {
		offset = 0
	}
	if !rewritten && cur.Fingerprint == fingerprint && cur.Size == st.Size() &&
		cur.ModTime.Equal(st.ModTime()) && offset == st.Size() {
		return harness.Batch{Next: cur}, nil
	}

	state := decodeState(cur.Extra)
	if rewritten {
		// A rewrite invalidates what the previous read reported.
		state = muxState{}
	}

	if offset > 0 {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return harness.Batch{Next: cur}, nil
		}
	}

	sessionID, parentID := sessionIdentity(src.Path)
	project, projectPath := projectLabels(src.Path, sessionID, parentID)

	var (
		reader    = bufio.NewReaderSize(file, readBuffer)
		consumed  = offset
		events    []model.UsageEvent
		startedAt time.Time
		updatedAt time.Time
		lines     int
	)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			// The trailing partial line is left for the next scan.
			break
		}
		consumed += int64(len(line))
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" {
			continue
		}
		var message muxMessage
		if json.Unmarshal([]byte(trimmed), &message) != nil {
			continue
		}
		lines++
		ts := messageTimestamp(&message)
		if !ts.IsZero() {
			if startedAt.IsZero() || ts.Before(startedAt) {
				startedAt = ts
			}
			if ts.After(updatedAt) {
				updatedAt = ts
			}
		}
		if strings.EqualFold(strings.TrimSpace(message.Role), "user") {
			if !state.TitleReported {
				if text := userText(message.Parts); text != "" {
					state.Title = harness.Title(text)
					state.TitleReported = true
				}
			}
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(message.Role), "assistant") ||
			message.Metadata == nil || len(message.Metadata.Usage) == 0 {
			continue
		}
		usage := parseUsage(message.Metadata)
		if usage.isZero() {
			continue
		}
		if ts.IsZero() {
			continue
		}
		modelName, provider := splitModel(message.Metadata.Model)
		events = append(events, model.UsageEvent{
			Harness:   model.Mux,
			DedupKey:  "mux:" + sessionID + ":" + messageKey(message, trimmed),
			SessionID: sessionID,
			ParentID:  parentID,
			Timestamp: ts.UTC(),
			Model:     modelName,
			Provider:  provider,
			Tokens: model.Tokens{
				Input:      usage.input,
				Output:     usage.output,
				CacheRead:  usage.cacheRead,
				CacheWrite: usage.cacheWrite,
				Reasoning:  usage.reasoning,
			},
			ProjectPath: projectPath,
		})
	}

	next := harness.Cursor{
		Offset:      consumed,
		Size:        st.Size(),
		ModTime:     st.ModTime(),
		Fingerprint: fingerprint,
		Extra:       state.encode(),
	}
	sessions := []model.SessionMeta(nil)
	if lines > 0 {
		sessions = append(sessions, model.SessionMeta{
			Harness:   model.Mux,
			SessionID: sessionID,
			ParentID:  parentID,
			Title:     state.Title,
			Project:   project,
			StartedAt: startedAt.UTC(),
			UpdatedAt: updatedAt.UTC(),
		})
	}
	return harness.Batch{Events: events, Sessions: sessions, Next: next}, nil
}

// muxState is the parser state kept across incremental batches.
type muxState struct {
	Title         string `json:"title,omitempty"`
	TitleReported bool   `json:"titleReported,omitempty"`
}

func (s muxState) encode() string {
	if s.Title == "" && !s.TitleReported {
		return ""
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(raw)
}

func decodeState(raw string) muxState {
	if strings.TrimSpace(raw) == "" {
		return muxState{}
	}
	var state muxState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return muxState{}
	}
	return state
}

// muxMessage is one line of chat.jsonl.
type muxMessage struct {
	ID        string          `json:"id"`
	Role      string          `json:"role"`
	Parts     []muxPart       `json:"parts"`
	CreatedAt json.RawMessage `json:"createdAt"`
	Metadata  *muxMetadata    `json:"metadata"`
}

type muxPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ToolName string `json:"toolName"`
}

type muxMetadata struct {
	Model            string            `json:"model"`
	Timestamp        json.RawMessage   `json:"timestamp"`
	Usage            json.RawMessage   `json:"usage"`
	ProviderMetadata *providerMetadata `json:"providerMetadata"`
}

type providerMetadata struct {
	Anthropic *anthropicMetadata `json:"anthropic"`
}

type anthropicMetadata struct {
	CacheCreationInputTokens json.RawMessage `json:"cacheCreationInputTokens"`
}

// usageTotals is one usage object after normalization.
type usageTotals struct {
	input      int64
	output     int64
	cacheRead  int64
	cacheWrite int64
	reasoning  int64
}

func (u usageTotals) isZero() bool {
	return u.input == 0 && u.output == 0 && u.cacheRead == 0 && u.cacheWrite == 0 && u.reasoning == 0
}

// parseUsage normalizes mux's usage object. Upstream reports inputTokens as a
// total that includes cache read and creation, and outputTokens as a total that
// includes reasoning.
func parseUsage(meta *muxMetadata) usageTotals {
	usage := decodeObject(meta.Usage)
	if usage == nil {
		return usageTotals{}
	}
	input := firstNumber(usage, "inputTokens", "input_tokens", "promptTokens", "prompt_tokens")
	output := firstNumber(usage, "outputTokens", "output_tokens", "completionTokens", "completion_tokens")
	cacheRead := firstNumber(usage, "cachedInputTokens", "cached_input_tokens",
		"cacheReadInputTokens", "cache_read_input_tokens")
	reasoning := firstNumber(usage, "reasoningTokens", "reasoning_tokens")
	cacheWrite := int64(0)
	if meta.ProviderMetadata != nil && meta.ProviderMetadata.Anthropic != nil {
		cacheWrite = rawNumber(meta.ProviderMetadata.Anthropic.CacheCreationInputTokens)
	}
	totals := usageTotals{
		input:      maxInt64(input-cacheRead-cacheWrite, 0),
		output:     maxInt64(output-reasoning, 0),
		cacheRead:  cacheRead,
		cacheWrite: cacheWrite,
		reasoning:  minInt64(reasoning, output),
	}
	return totals
}

// userText joins the text parts of a user message the way upstream does.
func userText(parts []muxPart) string {
	var texts []string
	for _, part := range parts {
		if part.Type != "text" {
			continue
		}
		if text := strings.TrimSpace(part.Text); text != "" {
			texts = append(texts, text)
		}
	}
	joined := strings.Join(texts, " ")
	if len(joined) > titleFallbackLen {
		joined = joined[:titleFallbackLen]
	}
	return joined
}

// messageKey prefers the message id and falls back to a hash of the raw line.
func messageKey(message muxMessage, raw string) string {
	if id := strings.TrimSpace(message.ID); id != "" {
		return id
	}
	sum := sha1.Sum([]byte(raw))
	return "h" + hex.EncodeToString(sum[:8])
}

// splitModel splits mux's "provider:modelId" model strings at the first colon.
func splitModel(value string) (modelName, provider string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown", ""
	}
	if i := strings.Index(value, ":"); i >= 0 {
		name := strings.TrimSpace(value[i+1:])
		if name == "" {
			name = "unknown"
		}
		return name, strings.TrimSpace(value[:i])
	}
	return value, ""
}

// sessionIdentity derives the session (and parent) ids from a transcript path.
func sessionIdentity(path string) (sessionID, parentID string) {
	dir := filepath.Dir(path)
	sessionID = filepath.Base(dir)
	parent := filepath.Dir(dir)
	if filepath.Base(parent) == subagentDirName {
		parentID = filepath.Base(filepath.Dir(parent))
	}
	return sessionID, parentID
}

// projectLabels resolves the workspace's project label and path from the
// config.json that sits next to the sessions directory. A subagent transcript
// belongs to its parent workspace, so the parent id is looked up first (and is
// also the fallback label); a workspace missing from the map keeps its id.
func projectLabels(path, sessionID, parentID string) (project, projectPath string) {
	fallback := sessionID
	if parentID != "" {
		fallback = parentID
	}
	root := sessionsRoot(path)
	if root == "" {
		return fallback, ""
	}
	projects := readProjectMap(filepath.Join(root, configFileName))
	for _, key := range []string{fallback, sessionID} {
		if entry, ok := projects[key]; ok {
			return entry.label, entry.path
		}
	}
	return fallback, ""
}

// sessionsRoot returns the root directory that contains the sessions/ tree of a
// transcript path.
func sessionsRoot(path string) string {
	dir := filepath.Dir(path)
	for {
		if filepath.Base(dir) == sessionsDirName {
			return filepath.Dir(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

type projectEntry struct {
	label string
	path  string
}

// readProjectMap parses config.json's [projectPath, {workspaces:[{id}]}] pairs.
func readProjectMap(path string) map[string]projectEntry {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var config struct {
		Projects []json.RawMessage `json:"projects"`
	}
	if json.Unmarshal(raw, &config) != nil {
		return nil
	}
	out := make(map[string]projectEntry, len(config.Projects))
	for _, entry := range config.Projects {
		var pair []json.RawMessage
		if json.Unmarshal(entry, &pair) != nil || len(pair) != 2 {
			continue
		}
		var projectPath string
		if json.Unmarshal(pair[0], &projectPath) != nil {
			continue
		}
		var holder struct {
			Workspaces []struct {
				ID string `json:"id"`
			} `json:"workspaces"`
		}
		if json.Unmarshal(pair[1], &holder) != nil {
			continue
		}
		label := strings.TrimSpace(filepath.Base(projectPath))
		if label == "" || label == "." {
			label = strings.TrimSpace(projectPath)
		}
		for _, workspace := range holder.Workspaces {
			id := strings.TrimSpace(workspace.ID)
			if id == "" {
				continue
			}
			out[id] = projectEntry{label: label, path: projectPath}
		}
	}
	return out
}

// messageTimestamp prefers metadata.timestamp (epoch milliseconds) and falls
// back to createdAt.
func messageTimestamp(message *muxMessage) time.Time {
	if message.Metadata != nil {
		if ts := epochMillis(message.Metadata.Timestamp); !ts.IsZero() {
			return ts
		}
	}
	return parseTimestampString(stringFromRaw(message.CreatedAt))
}

func epochMillis(raw json.RawMessage) time.Time {
	value, ok := rawNumberOK(raw)
	if !ok || value <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(math.Round(value))).UTC()
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

func stringFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return ""
	}
	return text
}

func decodeObject(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var object map[string]any
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return nil
	}
	return object
}

// firstNumber returns the first present key's non-negative value.
func firstNumber(object map[string]any, keys ...string) int64 {
	for _, key := range keys {
		if value, ok := numberValue(object[key]); ok {
			return value
		}
	}
	return 0
}

func numberValue(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return 0, false
		}
		if typed < 0 {
			return 0, true
		}
		return int64(math.Round(typed)), true
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
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

func rawNumber(raw json.RawMessage) int64 {
	value, _ := rawNumberOK(raw)
	if value < 0 {
		return 0
	}
	return int64(math.Round(value))
}

func rawNumberOK(raw json.RawMessage) (float64, bool) {
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
	if json.Unmarshal(raw, &text) != nil {
		return 0, false
	}
	number, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, false
	}
	return number, true
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// expandHome resolves a leading ~/ against the user's home directory.
func expandHome(path string) string {
	if path == "~" {
		return harness.Home()
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		return filepath.Join(harness.Home(), path[2:])
	}
	return path
}

// fileFingerprint is the sha1 of the first 4KB, used to notice rewrites that do
// not change the file size.
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
