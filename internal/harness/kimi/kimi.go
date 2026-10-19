// Package kimi parses Kimi CLI wire logs.
//
// Roots: $MYTOKEN_KIMI_DIRS when set replaces the whole set. Otherwise both
// layouts are watched — the legacy share directory ($KIMI_SHARE_DIR, else
// ~/.kimi) and the Kimi Code home ($KIMI_CODE_HOME, else ~/.kimi-code) — each
// contributing its sessions directory:
//
//	<share>/sessions/<work-dir>/<session>/wire.jsonl
//	<share>/sessions/<work-dir>/<session>/subagents/<agent>/wire.jsonl
//	<code-home>/sessions/<work-dir-key>/<session>/agents/main/wire.jsonl
//	<code-home>/sessions/<work-dir-key>/<session>/agents/<agent>/wire.jsonl
//
// A subagent log is attributed to its own session, with the surrounding session
// as its parent. The legacy layout nests agents under subagents/; the Kimi Code
// CLI (wire protocol 1.1) calls the session's own agent "main" and puts every
// agent under agents/, so an agent directory name repeats across sessions and a
// protocol 1.1 subagent id is qualified by its parent session id.
//
// Project names come from <share>/kimi.json (`work_dirs[].path`, keyed by md5 of
// the path, with an optional "<kaos>_<md5>" key for non-local kaos); when the
// work directory has no entry the directory name itself is used. A protocol 1.1
// log states its working directory in config.update.cwd, of which only the
// final path component is kept.
//
// Token normalization (docs/ARCHITECTURE.md#token-normalization): a legacy `StatusUpdate` envelope
// carries token_usage/usage with input_cache_read (CacheRead) and
// input_cache_creation (CacheWrite) reported separately from the fresh input.
// When the log reports only the combined `input`, the cache counts are
// subtracted from it. Protocol 1.1 writes a durable `usage.record` per request
// ("turn" or "session" scope), which is the authoritative measurement, so the
// usage repeated on the matching context loop step is deliberately not read. Kimi reports no reasoning or cost
// split, and the model falls back to the configured default from
// $KIMI_MODEL_NAME / <share>/config.toml.
//
// Only metadata is read: no message text, system prompt or tool payload ever
// leaves this package, except the first user message, which becomes the
// <=60 rune title.
package kimi

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
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
	kindJSONL        = "jsonl"
	fingerprintN     = 4096
	sessionsDirName  = "sessions"
	subagentsDirName = "subagents"
	agentsDirName    = "agents"
	mainAgentName    = "main"
	wireName         = "wire.jsonl"
	kimiDirName      = ".kimi"
	kimiCodeDirName  = ".kimi-code"
	autoModel        = "kimi-auto"
)

// Parser implements harness.Parser for the Kimi CLI.
type Parser struct {
	roots []string
	// prov recomputes this parser's default-root provenance from the current
	// environment. It is set only on the registry singleton; a parser built for
	// explicit roots reports those roots instead.
	prov func() ([]harness.Root, error)
}

// New returns a parser for the standard Kimi locations.
func New() *Parser { return &Parser{roots: DefaultRoots(), prov: rootProvenance} }

// NewWithRoots returns a parser reading the given sessions roots (tests).
func NewWithRoots(roots ...string) *Parser {
	return &Parser{roots: append([]string(nil), roots...)}
}

func init() { harness.Register(New()) }

// rootProvenance reports Kimi's default roots and where each came from.
// MYTOKEN_KIMI_DIRS replaces the whole set. Otherwise the legacy share
// directory and the Kimi Code home are both watched, each replaced by its own
// explicit variable: KIMI_SHARE_DIR for the legacy root and KIMI_CODE_HOME for
// the Kimi Code root.
func rootProvenance() ([]harness.Root, error) {
	if v, ok := harness.Getenv("MYTOKEN_KIMI_DIRS"); ok {
		dirs, err := harness.SplitEnvList("MYTOKEN_KIMI_DIRS", v)
		if err != nil {
			return nil, err
		}
		return harness.EnvRoots("MYTOKEN_KIMI_DIRS", dirs...), nil
	}
	var roots []harness.Root
	if v, ok := harness.Getenv("KIMI_SHARE_DIR"); ok {
		roots = append(roots, harness.EnvRoots("KIMI_SHARE_DIR", filepath.Join(filepath.Clean(v), sessionsDirName))...)
	} else {
		roots = append(roots, harness.BuiltinRoots(filepath.Join(harness.Home(), kimiDirName, sessionsDirName))...)
	}
	if v, ok := harness.Getenv("KIMI_CODE_HOME"); ok {
		roots = append(roots, harness.EnvRoots("KIMI_CODE_HOME", filepath.Join(filepath.Clean(v), sessionsDirName))...)
	} else {
		roots = append(roots, harness.BuiltinRoots(filepath.Join(harness.Home(), kimiCodeDirName, sessionsDirName))...)
	}
	return dedupeRoots(roots), nil
}

// dedupeRoots collapses roots that name the same directory. The first
// occurrence keeps its position, but an explicit override replaces the built-in
// default it shadows so the provenance still names the variable the user set.
func dedupeRoots(roots []harness.Root) []harness.Root {
	out := make([]harness.Root, 0, len(roots))
	at := make(map[string]int, len(roots))
	for _, r := range roots {
		key := filepath.Clean(r.Path)
		if i, ok := at[key]; ok {
			if out[i].Origin != harness.OriginEnv && r.Origin == harness.OriginEnv {
				out[i] = r
			}
			continue
		}
		at[key] = len(out)
		out = append(out, r)
	}
	return out
}

// DefaultRoots returns the Kimi sessions roots for this machine.
func DefaultRoots() []string {
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
func (p *Parser) Harness() model.Harness { return model.Kimi }

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

// Roots implements harness.Parser.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

// Discover implements harness.Parser.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	var out []harness.Source
	seen := map[string]bool{}
	add := func(path string) {
		if !seen[path] {
			seen[path] = true
			out = append(out, harness.Source{Path: path, Kind: kindJSONL})
		}
	}
	for _, root := range p.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		workDirs, err := os.ReadDir(root)
		if err != nil {
			continue // missing root is not an error
		}
		for _, workDir := range workDirs {
			if !workDir.IsDir() {
				continue
			}
			workDirPath := filepath.Join(root, workDir.Name())
			sessionDirs, err := os.ReadDir(workDirPath)
			if err != nil {
				continue
			}
			for _, sessionDir := range sessionDirs {
				if !sessionDir.IsDir() {
					continue
				}
				sessionPath := filepath.Join(workDirPath, sessionDir.Name())
				if isFile(filepath.Join(sessionPath, wireName)) {
					add(filepath.Join(sessionPath, wireName))
				}
				addAgentLogs(sessionPath, subagentsDirName, add)
				addAgentLogs(sessionPath, agentsDirName, add)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// addAgentLogs adds <session>/<group>/<agent>/wire.jsonl for every agent
// directory through add. The legacy layout uses subagents/ and protocol 1.1
// uses agents/; a group that does not exist is simply absent.
func addAgentLogs(sessionPath, group string, add func(string)) {
	agents, err := os.ReadDir(filepath.Join(sessionPath, group))
	if err != nil {
		return
	}
	for _, agent := range agents {
		if !agent.IsDir() {
			continue
		}
		path := filepath.Join(sessionPath, group, agent.Name(), wireName)
		if isFile(path) {
			add(path)
		}
	}
}

// state is the incremental parse state persisted in Cursor.Extra: facts that
// live in already-consumed bytes, the running record counter used to key legacy
// records that carry no message_id, and the occurrence counts that keep the
// content-addressed protocol 1.1 keys unique within a file when a record states
// no time of its own.
type state struct {
	Title     string         `json:"t,omitempty"`
	Project   string         `json:"p,omitempty"`
	StartedAt string         `json:"s,omitempty"`
	UpdatedAt string         `json:"u,omitempty"`
	Index     int            `json:"i,omitempty"`
	Seen      map[string]int `json:"c,omitempty"`
}

type wireRecord struct {
	Timestamp json.RawMessage `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	Message   *wireMessage    `json:"message"`
	// Time is the protocol 1.1 timestamp in epoch milliseconds.
	Time json.RawMessage `json:"time"`
	// raw is the line this record was decoded from; protocol 1.1 usage records
	// carry no id, so their dedup key digests the line itself.
	raw []byte
}

type wireMessage struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// inline reports whether the record uses the protocol 1.1 layout: a top-level
// type whose payload is the record's own fields, with no payload or message
// wrapper.
func (r *wireRecord) inline() bool {
	return r.Type != "" && r.Message == nil && len(r.Payload) == 0
}

// envelope returns the record type and payload, unwrapping the optional
// "message" wrapper the CLI uses on some records and reading protocol 1.1
// records inline.
func (r *wireRecord) envelope() (string, map[string]json.RawMessage) {
	if r.inline() {
		return r.Type, objectFields(r.raw)
	}
	typ, raw := r.Type, r.Payload
	if r.Message != nil {
		typ, raw = r.Message.Type, r.Message.Payload
	}
	if typ == "" {
		return "", nil
	}
	return typ, objectFields(raw)
}

// when returns the record's timestamp: protocol 1.1 carries epoch milliseconds
// in "time", the legacy layout epoch seconds in "timestamp".
func (r *wireRecord) when() (time.Time, bool) {
	if t, ok := epochMillis(r.Time); ok {
		return t, true
	}
	return parseTime(rawTime(r.Timestamp))
}

// hasMillisTime reports whether the record states its own protocol 1.1 time. A
// record that does identifies itself, so two byte-identical lines are the same
// durable record written twice rather than two separate calls.
func (r *wireRecord) hasMillisTime() bool {
	_, ok := epochMillis(r.Time)
	return ok
}

// objectFields decodes a raw JSON object into its fields, tolerating absent and
// non-object payloads.
func objectFields(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	return fields
}

// Parse implements harness.Parser.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	b := harness.Batch{}
	f, err := os.Open(src.Path)
	if err != nil {
		return b, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return b, err
	}
	fp, err := fingerprint(f)
	if err != nil {
		return b, err
	}
	// Unchanged source: hand the cursor back untouched.
	if cur.Fingerprint != "" && cur.Fingerprint == fp && cur.Size == st.Size() && cur.ModTime.Equal(st.ModTime()) {
		return harness.Batch{Next: cur}, nil
	}

	root, rel := p.locate(src.Path)
	workDir, sessionID, parentID := sourceIDs(rel)
	shareDir := shareDirOf(root)
	fallbackProject := projectName(shareDir, workDir)
	start := cur.Offset
	st8 := state{}
	if cur.Fingerprint != "" && cur.Fingerprint != fp {
		start = 0 // content rewritten => reread from 0, rely on DedupKey
	} else if cur.Extra != "" {
		_ = json.Unmarshal([]byte(cur.Extra), &st8)
	}
	if start < 0 || start > st.Size() {
		start = 0
		st8 = state{}
	}
	project := st8.Project
	if project == "" {
		project = fallbackProject
	}
	configured := configuredModel(shareDir)

	seen := st8.Seen
	if seen == nil {
		seen = map[string]int{}
	}
	acc := &accumulator{
		primary:         sessionID,
		parent:          parentID,
		project:         project,
		fallbackProject: fallbackProject,
		title:           st8.Title,
		index:           st8.Index,
		events:          map[string]int{},
		seen:            seen,
		configured:      configured,
	}
	if t, ok := parseTime(st8.StartedAt); ok {
		acc.started = t
	}
	if t, ok := parseTime(st8.UpdatedAt); ok {
		acc.updated = t
	}

	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return b, err
	}
	br := bufio.NewReaderSize(f, 1<<20)
	off := start
	for {
		if err := ctx.Err(); err != nil {
			return b, err
		}
		line, rerr := br.ReadBytes('\n')
		if rerr != nil {
			break // trailing bytes without '\n' are not consumed
		}
		off += int64(len(line))
		line = trimEOL(line)
		if len(line) == 0 {
			continue
		}
		var rec wireRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // tolerate unknown/partial records
		}
		rec.raw = line
		acc.handle(&b, &rec)
	}

	b.Sessions = acc.session()
	b.Next = harness.Cursor{
		Offset:      off,
		Size:        st.Size(),
		ModTime:     st.ModTime().UTC(),
		Fingerprint: fp,
		Extra:       acc.encodeState(),
	}
	return b, nil
}

// accumulator collects one batch for a single wire log.
type accumulator struct {
	primary string
	parent  string
	// project is the effective project name; fallbackProject is the name derived
	// from the work directory, which a protocol 1.1 config.update may replace.
	project         string
	fallbackProject string
	title           string
	started         time.Time
	updated         time.Time
	index           int
	configured      string
	events          map[string]int
	// seen counts how often each content-addressed usage key without a time of
	// its own occurred in the consumed bytes, so a repeated line in a later
	// increment stays distinct instead of being merged on equal bytes alone.
	seen map[string]int
}

func (a *accumulator) handle(b *harness.Batch, rec *wireRecord) {
	typ, payload := rec.envelope()
	if typ == "" || payload == nil {
		return
	}
	if typ == "metadata" {
		// Protocol 1.1 keeps the session start in created_at (epoch ms); the
		// legacy metadata record's timestamp stays ignored.
		if rec.inline() {
			if t, ok := epochMillis(payload["created_at"]); ok {
				a.observe(t)
			}
		}
		return
	}
	ts, ok := rec.when()
	if !ok {
		ts = a.started // a record without a timestamp inherits the session start
	}
	a.observe(ts)

	switch typ {
	case "TurnBegin", "SteerInput":
		if a.title == "" {
			if t := userText(payload); t != "" {
				a.title = harness.Title(t)
			}
		}
	case "turn.prompt":
		if a.title == "" {
			if t := promptText(payload); t != "" {
				a.title = harness.Title(t)
			}
		}
	case "config.update":
		a.setProject(stringField(payload, "cwd"))
	case "StatusUpdate":
		a.handleStatus(b, payload, ts)
	case "usage.record":
		// usage.record is durable and authoritative; the usage repeated on the
		// matching context loop step is deliberately not read.
		a.handleUsageRecord(b, payload, ts, rec)
	}
}

func (a *accumulator) handleStatus(b *harness.Batch, payload map[string]json.RawMessage, ts time.Time) {
	usage := usageOf(payload)
	if usage == nil {
		return
	}
	cacheRead := numberField(usage, "input_cache_read", "cache_read_input_tokens", "cached_input_tokens")
	cacheWrite := numberField(usage, "input_cache_creation", "cache_creation_input_tokens")
	input := numberField(usage, "input_other", "input_tokens")
	if input == 0 {
		input = numberField(usage, "input") - cacheRead - cacheWrite
		if input < 0 {
			input = 0
		}
	}
	output := numberField(usage, "output", "output_tokens")
	if input == 0 && output == 0 && cacheRead == 0 && cacheWrite == 0 {
		return // no usage recorded for this record
	}

	id := stringField(payload, "message_id")
	index := a.index
	a.index++
	key := "kimi:" + a.primary + ":" + firstNonEmpty(id, strconv.Itoa(index))
	a.emit(b, key, firstNonEmpty(
		stringField(payload, "model"),
		stringField(payload, "model_name"),
		a.configured,
	), id, model.Tokens{
		Input:      input,
		Output:     output,
		CacheRead:  cacheRead,
		CacheWrite: cacheWrite,
	}, ts)
}

// handleUsageRecord records one protocol 1.1 usage.record, the durable and
// authoritative measurement of one request. Its token classes map directly onto
// the normalized ones. A record whose scope this parser cannot place is skipped
// before any dedup state is consumed.
func (a *accumulator) handleUsageRecord(b *harness.Batch, payload map[string]json.RawMessage, ts time.Time, rec *wireRecord) {
	if !usageScopeIsCounted(payload) {
		return
	}
	usage := objectFields(payload["usage"])
	if usage == nil {
		return
	}
	tok := model.Tokens{
		Input:      numberField(usage, "inputOther"),
		Output:     numberField(usage, "output"),
		CacheRead:  numberField(usage, "inputCacheRead"),
		CacheWrite: numberField(usage, "inputCacheCreation"),
	}
	if tok.IsZero() {
		return
	}
	a.emit(b, a.usageKey(rec.raw, rec.hasMillisTime()), firstNonEmpty(stringField(payload, "model"), a.configured), "", tok, ts)
}

// usageScopeIsCounted reports whether a usage.record belongs in the ledger.
// Every usage.record is one request, so a scope upstream defines is counted,
// not filtered. usageAgentModel.ts:27-43 (packages/agent-core-v2/src/session/
// usage) accumulates each UsageRecord into byModel unconditionally, and its
// record() only labels that one request "turn" or "session" from source.type —
// the two scopes are alternatives for a request, not a cumulative and a
// per-turn view of the same one. context-projector.ts:393-401 (apps/vis/server)
// adds usage.record to the cumulative byModel ledger for both scopes and
// defaults an absent scope to "session". The field is optional, so an absent
// scope and the legacy empty string are counted as well. Only a value this
// parser cannot place — an unknown string, or a non-string — is conservatively
// ignored, and it is ignored before any dedup state is consumed. A present null
// is such a non-string: the schema types the field as an optional string union,
// so null is an illegal value, not an absent field, and it must not reach the
// absent-field default.
func usageScopeIsCounted(payload map[string]json.RawMessage) bool {
	raw, ok := payload["usageScope"]
	if !ok || len(raw) == 0 {
		return true
	}
	var scope *string
	if err := json.Unmarshal(raw, &scope); err != nil || scope == nil {
		return false
	}
	switch strings.TrimSpace(*scope) {
	case "", "turn", "session":
		return true
	default:
		return false
	}
}

// usageKey returns the dedup key of a protocol 1.1 usage.record. Those records
// carry no message id, so the key digests the raw line: the same record keeps
// its key when the file is reread after a rewrite or parsed in a later
// increment, which is what stops it from being counted twice. A record that
// states its own time is already identified by it, so a repeated byte-identical
// line collapses onto the same key. A record without a usable time is
// indistinguishable from an equal call, so the occurrence counter, persisted in
// the cursor, keeps the two apart rather than merging them on no evidence.
func (a *accumulator) usageKey(raw []byte, identified bool) string {
	sum := sha1.Sum(raw)
	base := "kimi:" + a.primary + ":u" + hex.EncodeToString(sum[:8])
	if identified {
		return base
	}
	n := a.seen[base]
	a.seen[base] = n + 1
	if n == 0 {
		return base
	}
	return base + "#" + strconv.Itoa(n)
}

// setProject records the working directory a protocol 1.1 log states, keeping
// only its final path component so a full path can never reach an event. A
// project already taken from config.update in an earlier batch is not replaced.
func (a *accumulator) setProject(cwd string) {
	if a.project != "" && a.project != a.fallbackProject {
		return
	}
	if name := projectFromCwd(cwd); name != "" {
		a.project = name
	}
}

// emit appends an event, merging it into an earlier one when the key was
// already seen in this batch: the largest token count wins and the newest
// timestamp is kept, so a re-read record cannot shrink or duplicate a report.
func (a *accumulator) emit(b *harness.Batch, key, modelName, requestID string, tok model.Tokens, ts time.Time) {
	ev := model.UsageEvent{
		Harness:     model.Kimi,
		DedupKey:    key,
		RequestID:   requestID,
		SessionID:   a.primary,
		ParentID:    a.parent,
		ProjectPath: a.project,
		Timestamp:   ts,
		Model:       modelName,
		Provider:    "kimi",
		Tokens:      tok,
	}
	if i, ok := a.events[key]; ok {
		prev := &b.Events[i]
		prev.Tokens = maxTokens(prev.Tokens, ev.Tokens)
		if ev.Timestamp.After(prev.Timestamp) {
			prev.Timestamp = ev.Timestamp
		}
		return
	}
	a.events[key] = len(b.Events)
	b.Events = append(b.Events, ev)
}

func (a *accumulator) observe(ts time.Time) {
	if ts.IsZero() {
		return
	}
	if a.started.IsZero() || ts.Before(a.started) {
		a.started = ts
	}
	if ts.After(a.updated) {
		a.updated = ts
	}
}

func (a *accumulator) session() []model.SessionMeta {
	if a.started.IsZero() && a.updated.IsZero() && a.title == "" && a.project == "" {
		return nil
	}
	started := a.started
	if started.IsZero() {
		started = a.updated
	}
	updated := a.updated
	if updated.IsZero() {
		updated = started
	}
	return []model.SessionMeta{{
		Harness:   model.Kimi,
		SessionID: a.primary,
		ParentID:  a.parent,
		Title:     a.title,
		Project:   a.project,
		StartedAt: started,
		UpdatedAt: updated,
	}}
}

func (a *accumulator) encodeState() string {
	s := state{Title: a.title, Project: a.project, Index: a.index, Seen: a.seen}
	if !a.started.IsZero() {
		s.StartedAt = a.started.UTC().Format(time.RFC3339Nano)
	}
	if !a.updated.IsZero() {
		s.UpdatedAt = a.updated.UTC().Format(time.RFC3339Nano)
	}
	// state carries a map, so it is not comparable: an empty encoding is the
	// "{}" the omitempty tags produce when nothing was learned.
	raw, err := json.Marshal(s)
	if err != nil || string(raw) == "{}" {
		return ""
	}
	return string(raw)
}

// locate returns the sessions root that owns path plus the path relative to it.
func (p *Parser) locate(path string) (string, string) {
	for _, root := range p.roots {
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return root, rel
	}
	return filepath.Dir(filepath.Dir(path)), filepath.Base(path)
}

// sourceIDs derives the work directory, session id and parent session id from
// a path relative to the sessions root.
func sourceIDs(rel string) (workDir, sessionID, parentID string) {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 2 {
		return "", "", ""
	}
	workDir = parts[0]
	sessionID = parts[1]
	if len(parts) < 4 {
		return workDir, sessionID, ""
	}
	switch parts[2] {
	case subagentsDirName:
		// <work-dir>/<session>/subagents/<agent>/wire.jsonl
		sessionID, parentID = parts[3], parts[1]
	case agentsDirName:
		// <work-dir-key>/<session>/agents/main/wire.jsonl is the session's own
		// agent, so it stays the session. Every other directory is a sub-agent:
		// its name repeats across sessions, so the id is qualified by the
		// parent session to stay unique.
		if parts[3] == mainAgentName {
			sessionID, parentID = parts[1], ""
		} else {
			sessionID, parentID = parts[1]+":"+parts[3], parts[1]
		}
	}
	return workDir, sessionID, parentID
}

// shareDirOf returns the Kimi home that owns a sessions root.
func shareDirOf(root string) string {
	if filepath.Base(root) == sessionsDirName {
		return filepath.Dir(root)
	}
	return root
}

// projectName maps a work directory name to a project name via kimi.json.
func projectName(shareDir, workDir string) string {
	raw, err := os.ReadFile(filepath.Join(shareDir, "kimi.json"))
	if err != nil {
		return workDir
	}
	var doc struct {
		WorkDirs []struct {
			Path string `json:"path"`
			Kaos string `json:"kaos"`
		} `json:"work_dirs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return workDir
	}
	for _, wd := range doc.WorkDirs {
		if wd.Path == "" {
			continue
		}
		name := pathBase(wd.Path)
		if name == "" {
			continue
		}
		sum := md5.Sum([]byte(wd.Path))
		hash := hex.EncodeToString(sum[:])
		if workDir == hash || (wd.Kaos != "" && wd.Kaos != "local" && workDir == wd.Kaos+"_"+hash) {
			return name
		}
	}
	return workDir
}

// pathBase mirrors projectNameFromPath: the last path component, else "kimi".
func pathBase(p string) string {
	cleaned := strings.TrimRight(p, `/\`)
	if base := filepath.Base(cleaned); base != "" && base != "." && base != string(filepath.Separator) {
		return base
	}
	if cleaned != "" {
		return cleaned
	}
	return "kimi"
}

var (
	reDefaultModel = regexp.MustCompile(`(?m)^\s*default_model\s*=\s*"?([^"\r\n]+?)"?\s*$`)
	reModelSection = regexp.MustCompile(`(?m)^\s*\[models\.(?:"([^"]+)"|'([^']+)'|([^\]]+))\]\s*$`)
	reModelField   = regexp.MustCompile(`(?m)^\s*model\s*=\s*"?([^"\r\n]+?)"?\s*$`)
)

// configuredModel resolves the model used when a record does not name one:
// $KIMI_MODEL_NAME, else the config.toml default_model, else "kimi-auto".
func configuredModel(shareDir string) string {
	if v := strings.TrimSpace(os.Getenv("KIMI_MODEL_NAME")); v != "" {
		return v
	}
	raw, err := os.ReadFile(filepath.Join(shareDir, "config.toml"))
	if err != nil {
		return autoModel
	}
	text := string(raw)
	m := reDefaultModel.FindStringSubmatch(text)
	if m == nil {
		return autoModel
	}
	key := strings.TrimSpace(m[1])
	if key == "" {
		return autoModel
	}
	for _, loc := range reModelSection.FindAllStringSubmatchIndex(text, -1) {
		name := ""
		for i := 2; i <= 6; i += 2 {
			if loc[i] >= 0 {
				name = text[loc[i]:loc[i+1]]
				break
			}
		}
		if strings.TrimSpace(name) != key {
			continue
		}
		if id := reModelField.FindStringSubmatch(text[loc[1]:]); id != nil {
			if v := strings.TrimSpace(id[1]); v != "" {
				return v
			}
		}
	}
	return key
}

// usageOf unwraps the token usage object of a payload.
func usageOf(payload map[string]json.RawMessage) map[string]json.RawMessage {
	for _, key := range []string{"token_usage", "usage"} {
		if fields := objectFields(payload[key]); fields != nil {
			return fields
		}
	}
	return nil
}

// promptText returns the first text part of a protocol 1.1 turn.prompt. Only
// that part becomes the session title; later parts and the surrounding
// conversation stay out of the parsed output.
func promptText(payload map[string]json.RawMessage) string {
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload["input"], &parts); err != nil {
		return ""
	}
	for _, p := range parts {
		if t := strings.TrimSpace(p.Text); t != "" {
			return t
		}
	}
	return ""
}

// projectFromCwd maps a stated working directory to a bare project name,
// keeping only the last path component. It returns "" when nothing usable
// remains, so a path that is only a root can never leak into an event.
func projectFromCwd(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return ""
	}
	base := filepath.Base(filepath.Clean(cwd))
	if base == "" || base == "." || strings.ContainsAny(base, `/\`) {
		return ""
	}
	return base
}

// userText extracts the user input of a TurnBegin/SteerInput envelope.
func userText(payload map[string]json.RawMessage) string {
	raw, ok := payload["user_input"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var out []string
	for _, p := range parts {
		if t := strings.TrimSpace(p.Text); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, " ")
}

// numberField returns the first strictly positive numeric field, accepting
// numeric strings, like the reference implementation.
func numberField(fields map[string]json.RawMessage, keys ...string) int64 {
	for _, key := range keys {
		raw, ok := fields[key]
		if !ok || len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var n float64
		if err := json.Unmarshal(raw, &n); err == nil {
			if n > 0 {
				return int64(n)
			}
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil && v > 0 {
				return int64(v)
			}
		}
	}
	return 0
}

func stringField(fields map[string]json.RawMessage, key string) string {
	raw, ok := fields[key]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

func maxTokens(a, b model.Tokens) model.Tokens {
	if b.Input > a.Input {
		a.Input = b.Input
	}
	if b.Output > a.Output {
		a.Output = b.Output
	}
	if b.CacheRead > a.CacheRead {
		a.CacheRead = b.CacheRead
	}
	if b.CacheWrite > a.CacheWrite {
		a.CacheWrite = b.CacheWrite
	}
	if b.Reasoning > a.Reasoning {
		a.Reasoning = b.Reasoning
	}
	return a
}

func fingerprint(f *os.File) (string, error) {
	buf := make([]byte, fingerprintN)
	n, err := f.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return "", err
	}
	sum := sha1.Sum(buf[:n])
	return hex.EncodeToString(sum[:]), nil
}

func trimEOL(line []byte) []byte {
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
	}
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	return line
}

// rawTime renders a timestamp field that may be an ISO string or an epoch
// number (seconds or milliseconds) as a string for parseTime.
func rawTime(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		ms := int64(n)
		if ms > 0 && ms < 1_000_000_000_000 {
			ms *= 1000
		}
		if ms <= 0 {
			return ""
		}
		return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
	}
	return ""
}

// epochMillis decodes an epoch-millisecond field, the unit protocol 1.1 uses
// for both "time" and metadata.created_at. Unlike rawTime it never guesses
// seconds, so a small value still means milliseconds; a non-numeric value falls
// back to the ISO layouts parseTime accepts.
func epochMillis(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, false
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		if n <= 0 {
			return time.Time{}, false
		}
		return time.UnixMilli(int64(n)).UTC(), true
	}
	return parseTime(rawTime(raw))
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999Z0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func isFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
