// Package stepcode parses Step Code coding-agent session logs.
//
// Step Code is a product facade over an upstream Pi, and its session format is
// Pi-derived: a plain, uncompressed JSONL file per session, resumed by byte
// offset. This parser therefore does not copy Pi's parser; it reuses
// internal/harness/sessionlog for the cursor model, discovery, JSON scalars and
// the usage ledger, and adds only the Step Code schema — which records exist,
// which of them account for a real model call, and how a forked log names its
// parent.
//
// # Log layout
//
//	~/.stepcode/agent/sessions/<cwd-slug>/<timestamp>_<session-id>.jsonl
//
// Step Code writes one directory per working directory ("--home-me-proj--") and
// one file per session, named "<ISO timestamp>_<session id>.jsonl". A session
// that was forked from another one stores that parent log's path in its header.
//
// # Roots
//
// The roots mirror the official resolver
// (packages/coding-agent/src/config.ts and src/step/environment.ts), in this
// order:
//
//	MYTOKEN_STEP_CODE_DIRS              replaces the whole root set
//	STEP_CODING_AGENT_SESSION_DIR       the session root itself
//	STEP_CODING_AGENT_DIR/sessions      the agent root, sessions live under it
//	~/.stepcode/agent/sessions          the built-in default
//
// A non-blank MYTOKEN_STEP_CODE_DIRS that names no directory is a hard error;
// it is never replaced by the defaults. A "~/" prefix in either Step variable is
// expanded, as the official expandTildePath does.
//
// Only session logs are read. Step Code's agent directory also holds auth.json,
// config.toml, settings.json, models.json, telemetry and logs; none of them is a
// ".jsonl" file with a session header first, and none of them is ever opened.
//
// # Record schema
//
// Line 1 is the session header:
//
//	{"type":"session","version":3,"id":"<uuidv7>","timestamp":"<RFC3339>",
//	 "cwd":"/path","parentSession":"/abs/path/to/parent.jsonl"?}
//
// A file whose first line is not that header is not a session log and is not
// discovered. A header version above CURRENT_SESSION_VERSION (3, at
// packages/coding-agent/src/core/session-manager.ts:30) is treated the same way:
// the format changed, and this parser must not guess at it.
//
// Every later line is {"type":"...","id":"<hex>","parentId":"<id|null>",
// "timestamp":"<RFC3339>", ...} with one of these bodies:
//
//	message         {"message":<AgentMessage>}       one user/assistant/toolResult turn
//	compaction      {"summary":...,"usage":<Usage>}  a compaction model call
//	branch_summary  {"fromId":...,"usage":<Usage>}   a branch-summary model call
//	session_info    {"name":"..."}                   overrides the session title
//	custom          {"customType":"...","data":...}  extension record
//	thinking_level_change, model_change, label, custom_message
//
// # Counting rules
//
// Exactly one class of record accounts for a model call: a message whose role is
// "assistant" and whose usage is non-zero. Usage on any other message role
// (toolResult carries one) is display data that the product copies off the
// assistant turn, so counting it would double count. A "compaction" and a
// "branch_summary" record each carry the usage of their own model call, and both
// are counted — once each.
//
// The "custom" record with customType "step-auto-clm-usage" is the product's
// own aggregate of a session's usage; it is a display record and is never
// counted. Unknown custom types are ignored for the same reason: a parser that
// counted every usage object it finds would double count.
//
// A compaction and a branch summary name no model of their own, so both inherit
// the model and provider of the most recent request seen before them (the same
// attribution the product's own stats use); a session whose log holds no request
// yet reports the model as "unknown".
//
// # Privacy
//
// Only usage numbers, model and provider names, timestamps and identifiers leave
// this parser. Prompt text, assistant text, tool payloads, summaries, image data
// and every absolute path are decoded and dropped: the working directory is
// reduced to its base name, and a fork's parent log path is reduced to the
// session id inside it. The single exception is the session title, which is the
// first user message by design (see harness.SessionMeta).
package stepcode

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/sessionlog"
	"github.com/zzstar101/mytoken/internal/model"
)

// HarnessID is this parser's harness id. It is deliberately not one of the
// built-in model.Harness constants: internal/model is a shared file this change
// is not allowed to touch, so the id is declared here and must be published
// (model.Known) by the application wiring. See docs/harness-verification/step-code.md.
const HarnessID = model.Harness("step-code")

// dedupPrefix namespaces every usage event this parser emits. Step Code is a new
// harness with no stored history, so a prefixed key can never collide with a key
// another parser already wrote.
const dedupPrefix = "step-code:"

// Environment overrides, in the official priority order. The two Step Code
// variables are shared with ordinary Pi invocations, which is exactly why this
// parser — not the Pi parser — is the one that reads them.
const (
	// stepDirsEnv is the MyToken-side override that replaces the whole root set.
	stepDirsEnv = "MYTOKEN_STEP_CODE_DIRS"
	// stepSessionDirEnv names the session root itself.
	stepSessionDirEnv = "STEP_CODING_AGENT_SESSION_DIR"
	// stepAgentDirEnv names the agent root; sessions live under it.
	stepAgentDirEnv = "STEP_CODING_AGENT_DIR"
)

const (
	// configDirName is the Step Code entrypoint's CONFIG_DIR_NAME.
	configDirName = ".stepcode"
	agentDirName  = "agent"
	// sessionDirName is the child of the agent root that holds session logs.
	sessionDirName = "sessions"

	// currentSessionVersion is the schema version this parser understands
	// (CURRENT_SESSION_VERSION in packages/coding-agent/src/core/session-manager.ts).
	currentSessionVersion = 3

	kindJSONL = "jsonl"
)

// Parser reads Step Code session logs. It holds no mutable state and is safe for
// concurrent use.
type Parser struct {
	roots []string
	// prov recomputes this parser's default-root provenance from the current
	// environment. It is set only on the registry singleton; a parser built for
	// explicit roots reports those roots instead.
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
// to the default root.
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

// rootProvenance reports Step Code's default roots and where they came from.
func rootProvenance() ([]harness.Root, error) {
	// The MyToken list override replaces the whole default set. A malformed list
	// is a hard error and never falls back to the defaults.
	if v, ok := harness.Getenv(stepDirsEnv); ok {
		dirs, err := harness.SplitEnvList(stepDirsEnv, v)
		if err != nil {
			return nil, err
		}
		return harness.EnvRoots(stepDirsEnv, dirs...), nil
	}
	// STEP_CODING_AGENT_SESSION_DIR names the session root itself and wins over
	// the agent root.
	if v, ok := harness.Getenv(stepSessionDirEnv); ok {
		return harness.EnvRoots(stepSessionDirEnv, expandHome(v)), nil
	}
	// STEP_CODING_AGENT_DIR names the agent root, so sessions live under it.
	if v, ok := harness.Getenv(stepAgentDirEnv); ok {
		return harness.EnvRoots(stepAgentDirEnv, filepath.Join(expandHome(v), sessionDirName)), nil
	}
	return harness.BuiltinRoots(filepath.Join(harness.Home(), configDirName, agentDirName, sessionDirName)), nil
}

// expandHome expands a leading "~" the way the official expandTildePath does.
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

// WithRoots implements harness.Rooted. It returns a fresh parser over exactly
// the given roots; an explicit empty list stays empty instead of falling back to
// this harness's default roots.
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

// The full harness surface: a parser (Harness/Roots/Discover/Parse), a rooted
// parser (WithRoots) and a provenance reporter (RootProvenance).
var (
	_ harness.Parser     = (*Parser)(nil)
	_ harness.Rooted     = (*Parser)(nil)
	_ harness.Provenance = (*Parser)(nil)
)

func init() { harness.Register(New()) }

// Discover walks every root and returns one Source per session log, sorted by
// path so scans are deterministic.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	return sessionlog.Walk(ctx, p.roots, stepLogKind)
}

// stepLogKind classifies a JSONL file by its first line. Only a real session
// header makes it a session log: Step Code writes the header as the file's first
// record, so a debug sink, an artifact mirror or a credentials file that happens
// to be JSONL is left alone.
func stepLogKind(path string) (string, bool) {
	line, ok := sessionlog.FirstLine(path)
	if !ok {
		return "", false
	}
	var h struct {
		Type    string `json:"type"`
		Version int    `json:"version"`
		ID      string `json:"id"`
	}
	if json.Unmarshal(line, &h) != nil {
		return "", false
	}
	if h.Type != "session" || h.ID == "" || h.Version > currentSessionVersion {
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
	s := newSession(sessionlog.RootFor(p.roots, src.Path), src.Path)
	if off > 0 {
		s.events.RestoreBoundary(cur.Extra)
	}
	n, _ := sessionlog.ReadLines(f, s.consume)
	b := s.batch()
	b.Next = harness.Cursor{
		Offset:      off + n,
		Size:        st.Size(),
		ModTime:     st.ModTime(),
		Fingerprint: fp,
		Extra:       s.events.BoundaryJSON(),
	}
	return b, nil
}

// ---------------------------------------------------------------------------
// session accumulation (one file == one session)
// ---------------------------------------------------------------------------

// session accumulates the events and metadata of a single session log.
type session struct {
	id        string
	parentID  string
	project   string // the working directory's base name only
	name      string // session_info name, when the log sets one
	userTitle string // first user message, used as the title when it does not
	startedAt time.Time
	updatedAt time.Time
	// The most recent request's model and provider. A compaction and a branch
	// summary name neither, so they inherit these.
	lastModel    string
	lastProvider string
	events       *sessionlog.Events
}

func newSession(root, path string) *session {
	// Fallbacks for a log whose header line has not been written yet: the file
	// name carries the id.
	id := idFromSessionPath(path)
	if id == "" {
		id = filepath.Base(filepath.Dir(path))
	}
	return &session{id: id, events: sessionlog.NewEvents(HarnessID, dedupPrefix)}
}

func (s *session) consume(line []byte) error {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil
	}
	var r stepRecord
	if err := json.Unmarshal(line, &r); err != nil {
		// A record we cannot fully model must not hide a marker we need: if the
		// envelope says it is a compaction, still record the boundary and skip
		// the rest.
		if sessionlog.EnvelopeType(line) == "compaction" {
			s.events.MarkPending(model.BoundaryCompact)
		}
		return nil // partial or foreign record: skip
	}
	if t := sessionlog.ParseStamp(r.Timestamp); !t.IsZero() && t.After(s.updatedAt) {
		s.updatedAt = t
	}
	switch r.Type {
	case "session":
		if r.ID != "" {
			s.id = r.ID
		}
		if r.Cwd != "" {
			// Only the base name is kept: which projects a user works in is not
			// this parser's business.
			s.project = baseName(r.Cwd)
		}
		if ps := r.ParentSession; ps != "" {
			// A fork header points at the parent log's path, not its id.
			if id := idFromSessionPath(ps); id != "" {
				s.parentID = id
			}
		}
		if t := sessionlog.ParseStamp(r.Timestamp); !t.IsZero() && (s.startedAt.IsZero() || t.Before(s.startedAt)) {
			s.startedAt = t
		}

	case "message":
		s.consumeMessage(r)

	case "compaction":
		// The compaction's own model call happens *at* the compaction point, so
		// it belongs to whatever boundary was already in force; the marker it
		// leaves is claimed by the first request after it.
		s.consumeOwnedUsage(r)
		s.events.MarkPending(model.BoundaryCompact)

	case "branch_summary":
		// A branch summary is a model call of its own (its record carries its own
		// usage), and unlike a compaction it does not bound a request.
		s.consumeOwnedUsage(r)

	case "session_info":
		if n := strings.TrimSpace(r.Name); n != "" {
			s.name = n
		}
	}
	return nil
}

// consumeMessage folds one "message" record into the session. Only an assistant
// message accounts for a model call; a toolResult message carries a copy of the
// turn's usage for display and is never counted.
func (s *session) consumeMessage(r stepRecord) {
	m := r.Message
	if m == nil {
		return
	}
	if mt := sessionlog.Millis(m.Timestamp); !mt.IsZero() && mt.After(s.updatedAt) {
		s.updatedAt = mt
	}
	switch {
	case strings.EqualFold(m.Role, "assistant") && !m.Usage.IsZero():
		ts := sessionlog.Millis(m.Timestamp)
		if ts.IsZero() {
			ts = sessionlog.ParseStamp(r.Timestamp)
		}
		if ts.IsZero() {
			ts = s.startedAt
		}
		mdl := sessionlog.FirstNonEmpty(m.Model, m.ResponseModel)
		if mdl != "" {
			s.lastModel = mdl
		}
		if m.Provider != "" {
			s.lastProvider = m.Provider
		}
		s.events.Add(model.UsageEvent{
			RequestID:   m.ResponseID,
			SessionID:   s.id,
			ParentID:    s.parentID,
			ProjectPath: s.project,
			Timestamp:   ts,
			Model:       mdl,
			Provider:    m.Provider,
			Tokens:      m.Usage.Tokens(sessionlog.ReasoningInOutput),
			CostUSD:     m.Usage.CostUSD(),
		}, sessionlog.FirstNonEmpty(m.ResponseID, r.ID, s.id+"#"+string(r.Timestamp)))

	case strings.EqualFold(m.Role, "user") && s.userTitle == "":
		if txt := sessionlog.FirstText(m.Content); sessionlog.TitleCandidate(txt) {
			s.userTitle = txt
		}
	}
}

// consumeOwnedUsage counts the usage of a record that *is* a model call of its
// own (a compaction or a branch summary). Such a record names no model and no
// provider, so both are inherited from the most recent request before it; a
// record with no usage at all is a marker only.
func (s *session) consumeOwnedUsage(r stepRecord) {
	u := r.Usage
	if u == nil || u.IsZero() {
		return
	}
	ts := sessionlog.ParseStamp(r.Timestamp)
	if ts.IsZero() {
		ts = s.updatedAt
	}
	s.events.Add(model.UsageEvent{
		SessionID:   s.id,
		ParentID:    s.parentID,
		ProjectPath: s.project,
		Timestamp:   ts,
		Model:       sessionlog.FirstNonEmpty(s.lastModel, "unknown"),
		Provider:    s.lastProvider,
		Tokens:      u.Tokens(sessionlog.ReasoningInOutput),
		CostUSD:     u.CostUSD(),
	}, sessionlog.FirstNonEmpty(r.ID, s.id+"#"+string(r.Timestamp)))
}

// batch finalizes the accumulated state into a Batch. Session fields are
// backfilled onto every event because the header is normally line 1 but a
// truncated log may not contain it yet.
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
	updated := s.updatedAt
	if updated.IsZero() {
		updated = s.startedAt
	}
	return harness.Batch{
		Events: s.events.List(),
		Sessions: []model.SessionMeta{{
			Harness:   HarnessID,
			SessionID: s.id,
			ParentID:  s.parentID,
			Title:     harness.Title(sessionlog.FirstNonEmpty(s.name, s.userTitle)),
			Project:   s.project,
			StartedAt: s.startedAt,
			UpdatedAt: updated,
		}},
	}
}

// baseName returns the last element of a path written on any platform. Step Code
// logs travel between machines, so a Windows path must reduce to its directory
// name too rather than survive whole as one un-splittable element.
func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// idFromSessionPath returns the session id embedded in a session log's name,
// "<timestamp>_<session-id>.jsonl". A path that does not carry one yields "".
func idFromSessionPath(p string) string {
	base := strings.TrimSuffix(filepath.Base(p), ".jsonl")
	i := strings.LastIndex(base, "_")
	if i < 0 {
		return ""
	}
	if id := base[i+1:]; looksLikeSessionID(id) {
		return id
	}
	return ""
}

// looksLikeSessionID reports whether s has the shape of the UUID Step Code names
// sessions with: 8-4-4-4-12 hex digits.
func looksLikeSessionID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range []byte(s) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// record schema
// ---------------------------------------------------------------------------

// stepRecord is one JSONL line of a Step Code session log. The fields of every
// record type live in one struct because the log interleaves them; a field that
// a record does not use is simply absent from its JSON.
type stepRecord struct {
	Type          string          `json:"type"`
	Version       int             `json:"version"`
	ID            string          `json:"id"`
	ParentID      string          `json:"parentId"`
	Timestamp     json.RawMessage `json:"timestamp"`
	Cwd           string          `json:"cwd"`
	ParentSession string          `json:"parentSession"`
	Message       *stepMessage    `json:"message"`

	// "usage" is the record's own model call: it appears on a compaction and on
	// a branch_summary, and on a custom extension record. Message usage lives
	// under "message" instead.
	Usage *sessionlog.Usage `json:"usage"`

	// session_info
	Name string `json:"name"`

	// The product's aggregate/display extensions ("customType") and every other
	// record body (summary, fromId, customType, details, data, ...) are decoded
	// into nothing: none of them may reach an event, so none of them is kept.
}

// stepMessage is an AgentMessage. Only the fields a usage event needs are
// decoded: content is read for the session title and then dropped, and tool
// call arguments, results and images are never modelled at all.
type stepMessage struct {
	Role          string               `json:"role"`
	Provider      string               `json:"provider"`
	Model         string               `json:"model"`
	ResponseModel string               `json:"responseModel"`
	ResponseID    string               `json:"responseId"`
	Timestamp     sessionlog.Num       `json:"timestamp"`
	Usage         *sessionlog.Usage    `json:"usage"`
	Content       []sessionlog.Content `json:"content"`
}
