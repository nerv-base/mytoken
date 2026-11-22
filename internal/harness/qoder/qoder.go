// Package qoder parses Qoder coding-agent session logs.
//
// Qoder (qoder.com) and Qoder CN (qoder.com.cn) are two builds of the same
// Claude-Code-derived agent. Both write one plain JSONL transcript per session,
// appended while the session runs, so this parser uses internal/harness/sessionlog
// for discovery, byte-offset cursors and fingerprint checks and adds only the
// Qoder record schema.
//
// # Log layout
//
//	~/.qoder-cn/projects/<project-slug>/<session-id>.jsonl
//	~/.qoder-cn/projects/<project-slug>/<session-id>/subagents/agent-*.jsonl
//	~/.qoder/projects/<project-slug>/<session-id>.jsonl
//
// The CN build defaults to ~/.qoder-cn, the international build to ~/.qoder.
// The two layouts are identical. A subagent transcript lives in a directory
// named after its parent session, so its own id is the file stem and its
// ParentID is the directory above "subagents".
//
// # Roots
//
// In priority order:
//
//	MYTOKEN_QODER_DIRS      replaces the whole root set (path list)
//	QODERCN_CONFIG_DIR      Qoder CN config root; transcripts live under projects/
//	QODER_CONFIG_DIR        Qoder config root; transcripts live under projects/
//	~/.qoder-cn/projects    built-in CN default
//	~/.qoder/projects       built-in international default
//
// A non-blank MYTOKEN_QODER_DIRS that names no directory is a hard error and
// never falls back to the defaults. Only *.jsonl files whose first record names
// a session are opened; credentials, state files and the desktop database are
// never read.
//
// # Record schema
//
// Every line is a JSON object with a type and usually a sessionId. The records
// that matter here are:
//
//	workspace-directories  {"directories":["/path/to/project"]}
//	runtime-config         {"model":"qfmodel","contextWindow":200000,"timestamp":<ms>}
//	user                   {"message":{"role":"user","content":[...]}, "origin":{"kind":"human"}?}
//	assistant              {"message":{"id":"...","role":"assistant","model":"...","content":[...],"usage":{...}}}
//
// An assistant message is streamed as several records that share one
// message.id: each record carries the content blocks produced in that fragment,
// and only the fragment that closes the message carries message.usage. Counting
// every record would double count, so the parser merges the fragments of a
// message.id and emits one event per id.
//
// # Counting rules
//
// Qoder's plan-billed transcripts record `credits` but write zero into every
// token field. That is a property of the plan accounting, not an empty request:
// the usage object still carries `context_usage_ratio`, and runtime-config names
// the model's context window. To give the dashboard a usable shape without
// pretending to know more than the log does, the parser:
//
//   - uses input_tokens/output_tokens/cache_* directly when the log has them;
//   - otherwise estimates input as round(context_usage_ratio × contextWindow),
//     falling back to the prompt characters read so far divided by four;
//   - estimates output from the assistant content characters of the message
//     divided by four;
//   - always preserves the native charge as Bill{Amount: credits, Unit: "credit"}.
//
// Credits are never converted to USD. The estimate is deliberately labelled as
// a shape, not a measurement, and follows the same character rule as Kiro.
//
// # Privacy
//
// Only usage numbers, model names, timestamps and identifiers leave this parser.
// Prompt text, assistant text, thinking, tool arguments and every absolute path
// are decoded and dropped: the working directory is reduced to its base name.
// The single exception is the session title, the first user message by design
// (see harness.SessionMeta).
package qoder

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nerv-base/mytoken/internal/harness"
	"github.com/nerv-base/mytoken/internal/harness/sessionlog"
	"github.com/nerv-base/mytoken/internal/model"
)

// HarnessID is this parser's harness id.
const HarnessID = model.Harness("qoder")

// dedupPrefix namespaces every usage event this parser emits.
const dedupPrefix = "qoder:"

// providerName is Qoder's provider id in the logs.
const providerName = "qoder"

// Environment overrides, in the official priority order.
const (
	qoderDirsEnv     = "MYTOKEN_QODER_DIRS"
	qoderCNConfigEnv = "QODERCN_CONFIG_DIR"
	qoderConfigEnv   = "QODER_CONFIG_DIR"
	configDirCN      = ".qoder-cn"
	configDir        = ".qoder"
	projectsDirName  = "projects"
	subagentsDirName = "subagents"
	kindJSONL        = "jsonl"
	charsPerToken    = 4
	creditsUnit      = "credit"
	unknownModel     = "unknown"
	parserVersion    = 1
)

// Parser reads Qoder session logs. It holds no mutable state and is safe for
// concurrent use.
type Parser struct {
	roots []string
	// prov recomputes this parser's default-root provenance from the current
	// environment. It is set only on the registry singleton.
	prov func() ([]harness.Root, error)
}

// New returns the parser for the default roots (see the package doc).
func New() *Parser {
	p := NewWithRoots()
	p.prov = rootProvenance
	return p
}

// NewWithRoot returns a parser rooted at root. It exists for tests and for
// tools that keep logs outside the home directory.
func NewWithRoot(root string) *Parser { return NewWithRoots(root) }

// NewWithRoots returns a parser over the given roots. An empty list falls back
// to the default roots.
func NewWithRoots(roots ...string) *Parser {
	cp := make([]string, 0, len(roots))
	for _, r := range roots {
		if r != "" {
			cp = append(cp, r)
		}
	}
	if len(cp) == 0 {
		cp = defaultRoots()
	}
	return &Parser{roots: cp}
}

// rootProvenance reports Qoder's default roots and where they came from.
func rootProvenance() ([]harness.Root, error) {
	if v, ok := harness.Getenv(qoderDirsEnv); ok {
		dirs, err := harness.SplitEnvList(qoderDirsEnv, v)
		if err != nil {
			return nil, err
		}
		return harness.EnvRoots(qoderDirsEnv, dirs...), nil
	}
	var roots []harness.Root
	if v, ok := harness.Getenv(qoderCNConfigEnv); ok {
		roots = append(roots, harness.EnvRoots(qoderCNConfigEnv, filepath.Join(expandHome(v), projectsDirName))...)
	}
	if v, ok := harness.Getenv(qoderConfigEnv); ok {
		roots = append(roots, harness.EnvRoots(qoderConfigEnv, filepath.Join(expandHome(v), projectsDirName))...)
	}
	if len(roots) > 0 {
		return roots, nil
	}
	home := harness.Home()
	return harness.BuiltinRoots(
		filepath.Join(home, configDirCN, projectsDirName),
		filepath.Join(home, configDir, projectsDirName),
	), nil
}

// expandHome expands a leading "~" the way the Qoder CLI does.
func expandHome(p string) string {
	home := harness.Home()
	switch {
	case p == "~":
		return home
	case strings.HasPrefix(p, "~/"), strings.HasPrefix(p, `~\`):
		if home != "" {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// defaultRoots returns the paths of rootProvenance.
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

// Harness implements harness.Parser.
func (p *Parser) Harness() model.Harness { return HarnessID }

// ParserVersion implements harness.Versioned.
func (p *Parser) ParserVersion() int { return parserVersion }

var _ harness.SourceSessions = (*Parser)(nil)

// SourceSessions implements harness.SourceSessions. A session log's id is its
// file stem; the first record's sessionId is reported too in case a writer
// named the file differently.
func (p *Parser) SourceSessions(ctx context.Context, src harness.Source) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	add(sessionIDFromPath(src.Path))
	if line, ok := sessionlog.FirstLine(src.Path); ok {
		var r qoderRecord
		if json.Unmarshal(line, &r) == nil {
			add(r.SessionID)
		}
	}
	return out, nil
}

// WithRoots implements harness.Rooted. It returns a fresh parser over exactly
// the given roots; an explicit empty list stays empty.
func (p *Parser) WithRoots(roots ...string) harness.Parser {
	for _, r := range roots {
		if strings.TrimSpace(r) != "" {
			return NewWithRoots(roots...)
		}
	}
	return &Parser{}
}

// Roots returns the directories to watch.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

var (
	_ harness.Parser     = (*Parser)(nil)
	_ harness.Rooted     = (*Parser)(nil)
	_ harness.Provenance = (*Parser)(nil)
)

func init() { harness.Register(New()) }

// Discover walks every root and returns one Source per session log, sorted by
// path so scans are deterministic.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	return sessionlog.Walk(ctx, p.roots, qoderLogKind)
}

// qoderRecordTypes are the record types a Qoder transcript may start with.
// Discovery requires both a known type and a sessionId, so a stray JSONL file
// (a log sink, a cache dump) is not mistaken for a session.
var qoderRecordTypes = map[string]bool{
	"workspace-directories": true,
	"runtime-config":        true,
	"worktree-state":        true,
	"user":                  true,
	"assistant":             true,
	"attachment":            true,
	"last-prompt":           true,
	"active-leaf":           true,
}

// qoderLogKind classifies a JSONL file by its first record.
func qoderLogKind(path string) (string, bool) {
	line, ok := sessionlog.FirstLine(path)
	if !ok {
		return "", false
	}
	var h struct {
		Type      string `json:"type"`
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(line, &h) != nil || h.SessionID == "" || !qoderRecordTypes[h.Type] {
		return "", false
	}
	return kindJSONL, true
}

// Parse reads one session log incrementally from cur.Offset. A trailing
// incomplete line is never consumed and never counted.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if err := ctx.Err(); err != nil {
		return harness.Batch{}, err
	}
	st, err := os.Stat(src.Path)
	if err != nil {
		return harness.Batch{}, err
	}
	fp, err := sessionlog.Fingerprint(src.Path)
	if err != nil {
		return harness.Batch{}, err
	}
	if sessionlog.Unchanged(cur, st, fp) {
		return harness.Batch{Next: cur}, nil
	}
	off := sessionlog.Rewind(cur, st.Size(), fp)
	f, err := os.Open(src.Path)
	if err != nil {
		return harness.Batch{}, err
	}
	defer f.Close()
	if off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return harness.Batch{}, err
		}
	}
	s := newSession(src.Path)
	if off > 0 {
		s.restore(cur.Extra)
	}
	n, _ := sessionlog.ReadLines(f, s.consume)
	b := s.batch()
	b.Next = harness.Cursor{
		Offset:      off + n,
		Size:        st.Size(),
		ModTime:     st.ModTime(),
		Fingerprint: fp,
		Extra:       s.stateJSON(),
	}
	return b, nil
}

// ---------------------------------------------------------------------------
// session accumulation (one file == one session)
// ---------------------------------------------------------------------------

// session accumulates the events and metadata of a single session log.
type session struct {
	id       string
	parentID string
	project  string // working directory base name only
	title    string
	started  time.Time
	updated  time.Time
	// model and contextWindow come from runtime-config; they describe the
	// model that produced the transcript and the denominator of
	// context_usage_ratio.
	model         string
	contextWindow int64
	// inputChars counts prompt and tool-result characters in this session. It
	// is only a fallback when the log has no context_usage_ratio.
	inputChars int64
	// pending holds the assistant content characters of message ids that have
	// not produced an event yet. A streamed message's fragments arrive before
	// the fragment that carries usage, so the total is known by then.
	pending map[string]int64
	events  *sessionlog.Events
}

func newSession(path string) *session {
	id := sessionIDFromPath(path)
	if id == "" {
		id = filepath.Base(filepath.Dir(path))
	}
	return &session{
		id:       id,
		parentID: parentFromPath(path),
		pending:  make(map[string]int64),
		events:   sessionlog.NewEvents(HarnessID, dedupPrefix),
	}
}

// sessionIDFromPath returns the file stem of a transcript.
func sessionIDFromPath(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// parentFromPath returns the parent session id of a subagent transcript:
// <parent-session-id>/subagents/agent-*.jsonl. A main transcript returns "".
func parentFromPath(path string) string {
	dir := filepath.Dir(path)
	if filepath.Base(dir) != subagentsDirName {
		return ""
	}
	return filepath.Base(filepath.Dir(dir))
}

// cursorState is the part of a session that must survive between incremental
// parses. It is stored in harness.Cursor.Extra as JSON.
type cursorState struct {
	ID            string           `json:"id,omitempty"`
	ParentID      string           `json:"parentId,omitempty"`
	Project       string           `json:"project,omitempty"`
	Title         string           `json:"title,omitempty"`
	Started       string           `json:"startedAt,omitempty"`
	Updated       string           `json:"updatedAt,omitempty"`
	Model         string           `json:"model,omitempty"`
	ContextWindow int64            `json:"contextWindow,omitempty"`
	InputChars    int64            `json:"inputChars,omitempty"`
	Pending       map[string]int64 `json:"pending,omitempty"`
}

// restore loads the state saved in a previous cursor. A malformed payload
// leaves the fresh session untouched.
func (s *session) restore(extra string) {
	if extra == "" {
		return
	}
	var st cursorState
	if json.Unmarshal([]byte(extra), &st) != nil {
		return
	}
	if st.ID != "" {
		s.id = st.ID
	}
	if st.ParentID != "" {
		s.parentID = st.ParentID
	}
	s.project = st.Project
	s.title = st.Title
	s.started = parseStoredTime(st.Started)
	s.updated = parseStoredTime(st.Updated)
	s.model = st.Model
	s.contextWindow = st.ContextWindow
	s.inputChars = st.InputChars
	for id, chars := range st.Pending {
		s.pending[id] = chars
	}
}

// stateJSON serializes the cursor state.
func (s *session) stateJSON() string {
	st := cursorState{
		ID:            s.id,
		ParentID:      s.parentID,
		Project:       s.project,
		Title:         s.title,
		Started:       formatStoredTime(s.started),
		Updated:       formatStoredTime(s.updated),
		Model:         s.model,
		ContextWindow: s.contextWindow,
		InputChars:    s.inputChars,
	}
	if len(s.pending) > 0 {
		st.Pending = s.pending
	}
	raw, _ := json.Marshal(st)
	return string(raw)
}

func parseStoredTime(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}
	}
	return t
}

func formatStoredTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func (s *session) consume(line []byte) error {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil
	}
	var r qoderRecord
	if json.Unmarshal(line, &r) != nil {
		return nil // partial or foreign record: skip
	}
	if ts := sessionlog.ParseStamp(r.Timestamp); !ts.IsZero() {
		if s.started.IsZero() || ts.Before(s.started) {
			s.started = ts
		}
		if ts.After(s.updated) {
			s.updated = ts
		}
	}
	switch r.Type {
	case "workspace-directories":
		if s.project == "" && len(r.Directories) > 0 {
			s.project = baseName(r.Directories[0])
		}
	case "runtime-config":
		if r.Model != "" {
			s.model = r.Model
		}
		if r.ContextWindow > 0 {
			s.contextWindow = int64(r.ContextWindow)
		}
	case "user":
		s.consumeUser(r)
	case "assistant":
		s.consumeAssistant(r)
	}
	return nil
}

func (s *session) consumeUser(r qoderRecord) {
	m := r.Message
	if m == nil {
		return
	}
	if s.project == "" && r.Cwd != "" {
		s.project = baseName(r.Cwd)
	}
	s.inputChars += contentChars(m.Content)
	if s.title != "" {
		return
	}
	text := ""
	if r.HumanInput != nil && strings.TrimSpace(r.HumanInput.Text) != "" {
		text = r.HumanInput.Text
	} else {
		text = firstText(m.Content)
	}
	if sessionlog.TitleCandidate(text) {
		s.title = text
	}
}

func (s *session) consumeAssistant(r qoderRecord) {
	m := r.Message
	if m == nil {
		return
	}
	if s.project == "" && r.Cwd != "" {
		s.project = baseName(r.Cwd)
	}
	if m.Model != "" {
		s.model = m.Model
	}
	key := m.ID
	if key == "" {
		key = r.UUID
	}
	chars := contentChars(m.Content)
	if key != "" {
		s.pending[key] += chars
	}
	u := m.Usage
	if u == nil {
		return
	}
	if key == "" {
		key = s.id + "#" + string(r.Timestamp)
	}
	tokens := u.tokens()
	if tokens.IsZero() {
		tokens = model.Tokens{
			Input:  s.estimateInput(u.ContextUsageRatio),
			Output: estimateTokens(s.pending[key]),
		}
	}
	credits := float64(u.Credits)
	if tokens.IsZero() && credits <= 0 {
		delete(s.pending, key)
		return
	}
	mdl := sessionlog.FirstNonEmpty(m.Model, s.model, unknownModel)
	ev := model.UsageEvent{
		SessionID:   s.id,
		ParentID:    s.parentID,
		ProjectPath: s.project,
		Timestamp:   sessionlog.ParseStamp(r.Timestamp),
		Model:       mdl,
		Provider:    providerName,
		Tokens:      tokens,
		RequestID:   sessionlog.FirstNonEmpty(u.RequestID, r.RequestTokenAnchor.RequestID),
		GitBranch:   r.GitBranch,
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = s.updated
	}
	if credits > 0 {
		ev.Bill = &model.Bill{Amount: credits, Unit: creditsUnit}
	}
	s.events.Add(ev, key)
	delete(s.pending, key)
}

// estimateInput reconstructs the prompt size from the context ratio when the
// log writes one, falling back to prompt characters.
func (s *session) estimateInput(ratio float64) int64 {
	if ratio > 0 && s.contextWindow > 0 {
		if n := int64(math.Round(ratio * float64(s.contextWindow))); n > 0 {
			return n
		}
	}
	return estimateTokens(s.inputChars)
}

// batch finalizes the accumulated state into a Batch.
func (s *session) batch() harness.Batch {
	s.events.Fill(func(ev *model.UsageEvent) {
		if ev.SessionID == "" {
			ev.SessionID = s.id
		}
		if ev.ParentID == "" {
			ev.ParentID = s.parentID
		}
		if ev.ProjectPath == "" {
			ev.ProjectPath = s.project
		}
	})
	updated := s.updated
	if updated.IsZero() {
		updated = s.started
	}
	return harness.Batch{
		Events: s.events.List(),
		Sessions: []model.SessionMeta{{
			Harness:   HarnessID,
			SessionID: s.id,
			ParentID:  s.parentID,
			Title:     harness.Title(s.title),
			Project:   s.project,
			StartedAt: s.started,
			UpdatedAt: updated,
		}},
	}
}

// baseName returns the last element of a path written on any platform.
func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// estimateTokens applies the same character rule as Kiro: four characters per
// token, rounded up.
func estimateTokens(chars int64) int64 {
	if chars <= 0 {
		return 0
	}
	return (chars + charsPerToken - 1) / charsPerToken
}

// ---------------------------------------------------------------------------
// record schema
// ---------------------------------------------------------------------------

// qoderRecord is one JSONL line of a Qoder session log. Fields of every record
// type live in one struct because the log interleaves them.
type qoderRecord struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId"`
	UUID      string          `json:"uuid"`
	Timestamp json.RawMessage `json:"timestamp"`
	Cwd       string          `json:"cwd"`
	GitBranch string          `json:"gitBranch"`

	Message    *qoderMessage `json:"message"`
	HumanInput *struct {
		Text string `json:"text"`
	} `json:"humanInput"`
	Origin *struct {
		Kind string `json:"kind"`
	} `json:"origin"`

	Directories []string `json:"directories"`

	Model         string         `json:"model"`
	ContextWindow sessionlog.Num `json:"contextWindow"`

	RequestTokenAnchor struct {
		RequestID string `json:"requestId"`
	} `json:"requestTokenAnchor"`
}

// qoderMessage is the message body of a user or assistant record.
type qoderMessage struct {
	ID      string         `json:"id"`
	Role    string         `json:"role"`
	Model   string         `json:"model"`
	Content []contentBlock `json:"content"`
	Usage   *qoderUsage    `json:"usage"`
}

// contentBlock is one element of message.content. Only the text needed for the
// session title and for character-based token estimates is decoded.
type contentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	Input    json.RawMessage `json:"input"`
	Content  json.RawMessage `json:"content"`
}

// qoderUsage is the usage object on the assistant fragment that closes a
// streamed message.
type qoderUsage struct {
	Input         sessionlog.Num `json:"input_tokens"`
	Output        sessionlog.Num `json:"output_tokens"`
	CacheRead     sessionlog.Num `json:"cache_read_input_tokens"`
	CacheWrite    sessionlog.Num `json:"cache_creation_input_tokens"`
	CacheCreation struct {
		Ephemeral1h sessionlog.Num `json:"ephemeral_1h_input_tokens"`
		Ephemeral5m sessionlog.Num `json:"ephemeral_5m_input_tokens"`
	} `json:"cache_creation"`
	Credits           sessionlog.Fnum `json:"credits"`
	OriginalCredits   sessionlog.Fnum `json:"original_credits"`
	Billable          bool            `json:"billable"`
	RequestID         string          `json:"request_id"`
	ContextUsageRatio float64         `json:"context_usage_ratio"`
}

// tokens converts the log's token fields. Plan-billed Qoder logs leave all of
// them zero; the caller estimates in that case.
func (u *qoderUsage) tokens() model.Tokens {
	input := sessionlog.Clamp(u.Input)
	output := sessionlog.Clamp(u.Output)
	cacheRead := sessionlog.Clamp(u.CacheRead)
	cacheWrite := sessionlog.Clamp(u.CacheWrite)
	oneHour := sessionlog.Clamp(u.CacheCreation.Ephemeral1h)
	fiveMinute := sessionlog.Clamp(u.CacheCreation.Ephemeral5m)
	if cacheWrite == 0 && (oneHour > 0 || fiveMinute > 0) {
		cacheWrite = oneHour + fiveMinute
	}
	if oneHour > cacheWrite {
		cacheWrite = oneHour
	}
	return model.Tokens{
		Input:        input,
		Output:       output,
		CacheRead:    cacheRead,
		CacheWrite:   cacheWrite,
		CacheWrite1h: oneHour,
	}
}

// contentChars counts the text-bearing parts of a message. Tool arguments are
// counted as JSON text because that is what the model wrote; images and other
// binary payloads are not.
func contentChars(blocks []contentBlock) int64 {
	var n int64
	for _, b := range blocks {
		switch b.Type {
		case "text":
			n += runeCount(b.Text)
		case "thinking":
			n += runeCount(b.Thinking)
		case "tool_use":
			n += rawTextChars(b.Input)
		case "tool_result":
			n += rawContentChars(b.Content)
		}
	}
	return n
}

// firstText joins the text blocks of a message for a session title.
func firstText(blocks []contentBlock) string {
	var b strings.Builder
	for _, c := range blocks {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

// rawContentChars counts a tool_result content field, which may be a string or
// an array of content blocks.
func rawContentChars(raw json.RawMessage) int64 {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return runeCount(s)
		}
		return 0
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		return contentChars(blocks)
	}
	return 0
}

// rawTextChars counts a tool_use input payload as text. Scalars and objects
// both serialize to the JSON the model emitted; null is not content.
func rawTextChars(raw json.RawMessage) int64 {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	return runeCount(string(raw))
}

func runeCount(s string) int64 { return int64(utf8.RuneCountInString(s)) }
