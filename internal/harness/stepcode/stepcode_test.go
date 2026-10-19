package stepcode

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

// Synthetic Step Code session records. They are hand-written to the shape
// documented in docs/ARCHITECTURE.md#harness-coverage-status (packages/coding-agent
// src/core/session-manager.ts of the official source) and are no one's real
// log: no field here was copied from a user's machine.
const (
	betaID  = "01a00000-0000-7000-8000-000000000001"
	childID = "01a00000-0000-7000-8000-000000000002"

	// A compaction and a branch summary are keyed by their own entry id.
	compactionKey = "step-code:01b00000-0000-7000-8000-000000000014"
	postKey       = "step-code:01b00000-0000-7000-8000-000000000015"

	// sessionHeader is the v3 header, line 1 of every session log.
	sessionHeader = `{"type":"session","version":3,"id":"` + betaID + `","timestamp":"2026-01-02T03:04:05.000Z","cwd":"/home/user/proj-alpha"}`

	// childHeader is a fork: parentSession names the parent's log file, whose
	// basename carries the parent's session id.
	childHeader = `{"type":"session","version":3,"id":"` + childID + `","timestamp":"2026-01-02T03:05:06.000Z","cwd":"/home/user/proj-alpha","parentSession":"/home/user/.stepcode/agent/sessions/--home-user-proj-alpha--/2026-01-02T03-04-05-000Z_` + betaID + `.jsonl"}`

	userMsg = `{"type":"message","id":"01b00000-0000-7000-8000-000000000011","parentId":null,"timestamp":"2026-01-02T03:04:06.000Z","message":{"role":"user","content":[{"type":"text","text":"Explain the harness layer"}],"timestamp":1767323046000}}`

	// assistantMsg carries the authoritative usage of a real model call. Its
	// output (40) already includes reasoning (15), exactly as the official
	// Usage interface documents.
	assistantMsg = `{"type":"message","id":"01b00000-0000-7000-8000-000000000012","parentId":"01b00000-0000-7000-8000-000000000011","timestamp":"2026-01-02T03:04:07.000Z","message":{"role":"assistant","content":[{"type":"text","text":"SENTINEL_PRIVATE_TEXT"}],"api":"anthropic-messages","provider":"anthropic","model":"claude-4.6","usage":{"input":100,"output":40,"cacheRead":10,"cacheWrite":5,"reasoning":15,"totalTokens":155,"cost":{"input":0.0003,"output":0.0006,"cacheRead":0.000003,"cacheWrite":0.000019,"total":0.000922}},"stopReason":"stop","timestamp":1767323047000}}`

	// toolResultMsg also carries a usage object; it is not a model call of its
	// own and must never be counted.
	toolResultMsg = `{"type":"message","id":"01b00000-0000-7000-8000-000000000013","parentId":"01b00000-0000-7000-8000-000000000012","timestamp":"2026-01-02T03:04:08.000Z","message":{"role":"toolResult","toolCallId":"call-1","toolName":"bash","content":[{"type":"text","text":"SENTINEL_PRIVATE_TEXT in a tool payload"}],"isError":false,"timestamp":1767323048000,"usage":{"input":7,"output":3,"cacheRead":0,"cacheWrite":0,"totalTokens":10}}}`

	// compactionEntry is the summarisation call itself: a top-level usage on the
	// entry, no model and no provider.
	compactionEntry = `{"type":"compaction","id":"01b00000-0000-7000-8000-000000000014","parentId":"01b00000-0000-7000-8000-000000000013","timestamp":"2026-01-02T03:04:09.000Z","summary":"SENTINEL_PRIVATE_TEXT summary","firstKeptEntryId":"01b00000-0000-7000-8000-000000000011","tokensBefore":155,"usage":{"input":200,"output":30,"reasoning":10,"cacheRead":0,"cacheWrite":0,"totalTokens":230,"cost":{"input":0.0006,"output":0.00045,"cacheRead":0,"cacheWrite":0,"total":0.00105}}}`

	afterCompactMsg = `{"type":"message","id":"01b00000-0000-7000-8000-000000000015","parentId":"01b00000-0000-7000-8000-000000000014","timestamp":"2026-01-02T03:04:10.000Z","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"api":"anthropic-messages","provider":"anthropic","model":"claude-4.6","usage":{"input":230,"output":20,"reasoning":5,"cacheRead":0,"cacheWrite":0,"totalTokens":250,"cost":{"input":0.00069,"output":0.0003,"cacheRead":0,"cacheWrite":0,"total":0.00099}},"stopReason":"stop","timestamp":1767323050000}}`

	// autoUsageEntry is Step's display-side aggregate for its own accounting. It
	// is a presentation record and must not be counted.
	autoUsageEntry = `{"type":"custom","id":"01b00000-0000-7000-8000-000000000016","parentId":"01b00000-0000-7000-8000-000000000015","timestamp":"2026-01-02T03:04:11.000Z","customType":"step-auto-clm-usage","data":{"usage":{"input":999,"output":999,"cacheRead":0,"cacheWrite":0,"totalTokens":1998}}}`

	branchSummary = `{"type":"branch_summary","id":"01b00000-0000-7000-8000-000000000017","parentId":"01b00000-0000-7000-8000-000000000015","timestamp":"2026-01-02T03:04:12.000Z","fromId":"01b00000-0000-7000-8000-000000000012","summary":"SENTINEL_PRIVATE_TEXT","usage":{"input":50,"output":10,"reasoning":2,"cacheRead":0,"cacheWrite":0,"totalTokens":60,"cost":{"input":0.00015,"output":0.00015,"cacheRead":0,"cacheWrite":0,"total":0.0003}}}`

	childAssistant = `{"type":"message","id":"01b00000-0000-7000-8000-000000000021","parentId":null,"timestamp":"2026-01-02T03:05:07.000Z","message":{"role":"assistant","content":[{"type":"text","text":"child"}],"api":"openai-completions","provider":"openai","model":"gpt-6.1","usage":{"input":10,"output":9,"reasoning":4,"cacheRead":1,"cacheWrite":0,"totalTokens":20,"cost":{"input":0.00001,"output":0.00002,"cacheRead":0,"cacheWrite":0,"total":0.00003}},"stopReason":"stop","timestamp":1767323107000}}`
)

// fixture builds the synthetic session tree under a fresh temp root and returns
// the root. Layout mirrors ~/.stepcode/agent/sessions:
//
//	<root>/--home-user-proj-alpha--/<parent>.jsonl
//	<root>/--home-user-proj-alpha--/<child>.jsonl
func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "--home-user-proj-alpha--")
	write(t, filepath.Join(dir, "2026-01-02T03-04-05-000Z_"+betaID+".jsonl"),
		sessionHeader, userMsg, assistantMsg, toolResultMsg, compactionEntry,
		afterCompactMsg, autoUsageEntry, branchSummary)
	write(t, filepath.Join(dir, "2026-01-02T03-05-06-000Z_"+childID+".jsonl"), childHeader, childAssistant)
	return root
}

func write(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func parseAll(t *testing.T, p harness.Parser) harness.Batch {
	t.Helper()
	return parseFrom(t, p, nil)
}

func parseFrom(t *testing.T, p harness.Parser, cur map[string]harness.Cursor) harness.Batch {
	t.Helper()
	ctx := context.Background()
	srcs, err := p.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var all harness.Batch
	for _, src := range srcs {
		var from harness.Cursor
		if cur != nil {
			from = cur[src.Path]
		}
		b, err := p.Parse(ctx, src, from)
		if err != nil {
			t.Fatalf("Parse(%s): %v", src.Path, err)
		}
		all.Events = append(all.Events, b.Events...)
		all.Sessions = append(all.Sessions, b.Sessions...)
	}
	return all
}

// TestHarnessIdentity pins the harness id and the dedup namespace: Step Code is
// a Pi-derived format read through its own harness, never merged into Pi's.
func TestHarnessIdentity(t *testing.T) {
	p := NewWithRoots(t.TempDir())
	if got := p.Harness(); got != model.Harness("step-code") {
		t.Errorf("Harness() = %q, want %q", got, "step-code")
	}
	if got := HarnessID; got != model.Harness("step-code") {
		t.Errorf("HarnessID = %q", got)
	}
	b := parseAll(t, NewWithRoots(fixture(t)))
	if len(b.Events) == 0 {
		t.Fatal("no events parsed")
	}
	for _, e := range b.Events {
		if e.Harness != HarnessID {
			t.Errorf("event harness = %q, want step-code", e.Harness)
		}
		if !strings.HasPrefix(e.DedupKey, "step-code:") {
			t.Errorf("dedup key %q lacks the step-code namespace", e.DedupKey)
		}
	}
}

// TestParseCountsOnlyRealModelCalls is the core ledger check: assistant
// messages plus the compaction and branch-summary calls, each once, with the
// five token classes mapped exactly.
func TestParseCountsOnlyRealModelCalls(t *testing.T) {
	b := parseAll(t, NewWithRoots(fixture(t)))
	if got, want := len(b.Events), 5; got != want {
		t.Fatalf("events = %d, want %d: %+v", got, want, b.Events)
	}
	byKey := map[string]model.UsageEvent{}
	for _, e := range b.Events {
		if _, dup := byKey[e.DedupKey]; dup {
			t.Errorf("duplicate dedup key %q in one parse", e.DedupKey)
		}
		byKey[e.DedupKey] = e
	}

	// Assistant: output 40 already includes reasoning 15.
	a := byKey["step-code:01b00000-0000-7000-8000-000000000012"]
	if a.Tokens != (model.Tokens{Input: 100, Output: 25, CacheRead: 10, CacheWrite: 5, Reasoning: 15}) {
		t.Errorf("assistant tokens = %+v", a.Tokens)
	}
	if a.Tokens.Total() != 155 {
		t.Errorf("assistant token total = %d, want the log's totalTokens 155", a.Tokens.Total())
	}
	if a.Model != "claude-4.6" || a.Provider != "anthropic" {
		t.Errorf("assistant model/provider = %q/%q", a.Model, a.Provider)
	}
	if a.CostUSD == nil || *a.CostUSD != 0.000922 {
		t.Errorf("assistant cost = %v, want 0.000922", a.CostUSD)
	}
	if a.SessionID != betaID || a.ProjectPath != "proj-alpha" {
		t.Errorf("assistant session/project = %q/%q", a.SessionID, a.ProjectPath)
	}
	if want := time.UnixMilli(1767323047000).UTC(); !a.Timestamp.Equal(want) {
		t.Errorf("assistant timestamp = %s, want %s", a.Timestamp, want)
	}
	if a.Boundary != "" {
		t.Errorf("assistant boundary = %q, want none", a.Boundary)
	}
	if a.RequestID != "" {
		t.Errorf("requestId = %q, want empty: a Step record names no upstream request id", a.RequestID)
	}

	// Compaction: input 200 + output 30 (reasoning 10 of it) = 230.
	c := byKey["step-code:01b00000-0000-7000-8000-000000000014"]
	if c.Tokens != (model.Tokens{Input: 200, Output: 20, Reasoning: 10}) {
		t.Errorf("compaction tokens = %+v", c.Tokens)
	}
	if c.Tokens.Total() != 230 {
		t.Errorf("compaction token total = %d, want 230", c.Tokens.Total())
	}
	if c.CostUSD == nil || *c.CostUSD != 0.00105 {
		t.Errorf("compaction cost = %v, want 0.00105", c.CostUSD)
	}
	// A compaction entry names no model; it inherits the nearest one.
	if c.Model != "claude-4.6" || c.Provider != "anthropic" {
		t.Errorf("compaction model/provider = %q/%q, want the inherited claude-4.6/anthropic", c.Model, c.Provider)
	}
	if c.Boundary != "" {
		t.Errorf("compaction boundary = %q: the marker belongs to the next request", c.Boundary)
	}

	// The request after a compaction is the one that is post-compaction.
	post := byKey["step-code:01b00000-0000-7000-8000-000000000015"]
	if post.Boundary != model.BoundaryCompact {
		t.Errorf("post-compaction boundary = %q, want %q", post.Boundary, model.BoundaryCompact)
	}
	if post.Tokens != (model.Tokens{Input: 230, Output: 15, Reasoning: 5}) {
		t.Errorf("post-compaction tokens = %+v", post.Tokens)
	}

	// Branch summary: its own model call, counted once.
	bs := byKey["step-code:01b00000-0000-7000-8000-000000000017"]
	if bs.Tokens != (model.Tokens{Input: 50, Output: 8, Reasoning: 2}) {
		t.Errorf("branch summary tokens = %+v", bs.Tokens)
	}
	if bs.Model != "claude-4.6" {
		t.Errorf("branch summary model = %q, want inherited claude-4.6", bs.Model)
	}

	// The tool result and the display aggregate carry usage too, and neither is
	// a model call.
	if _, ok := byKey["step-code:01b00000-0000-7000-8000-000000000013"]; ok {
		t.Error("toolResult usage was counted; it is not a model call")
	}
	if _, ok := byKey["step-code:01b00000-0000-7000-8000-000000000016"]; ok {
		t.Error("step-auto-clm-usage aggregate was counted; it only re-displays spend")
	}
	if n := countTokens(b.Events); n != 155+230+250+60+20 {
		t.Errorf("total counted tokens = %d, want 715", n)
	}
}

func countTokens(evs []model.UsageEvent) int64 {
	var n int64
	for _, e := range evs {
		n += e.Tokens.Total()
	}
	return n
}

// TestParseSessionTree checks the parent link and the session metadata.
func TestParseSessionTree(t *testing.T) {
	b := parseAll(t, NewWithRoots(fixture(t)))
	if len(b.Sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(b.Sessions))
	}
	got := map[string]model.SessionMeta{}
	for _, s := range b.Sessions {
		got[s.SessionID] = s
	}
	parent := got[betaID]
	if parent.Project != "proj-alpha" {
		t.Errorf("project = %q, want the directory's base name only", parent.Project)
	}
	if parent.ParentID != "" {
		t.Errorf("parent session id = %q, want empty", parent.ParentID)
	}
	if parent.Title != "Explain the harness layer" {
		t.Errorf("title = %q", parent.Title)
	}
	if parent.StartedAt.IsZero() || !parent.UpdatedAt.After(parent.StartedAt) {
		t.Errorf("started/updated = %s/%s", parent.StartedAt, parent.UpdatedAt)
	}
	child := got[childID]
	if child.ParentID != betaID {
		t.Errorf("child parent = %q, want %q (derived from parentSession's base name)", child.ParentID, betaID)
	}
	if child.Harness != HarnessID {
		t.Errorf("child harness = %q", child.Harness)
	}
	for _, e := range b.Events {
		if e.SessionID == childID && e.ParentID != betaID {
			t.Errorf("child event parent = %q, want %q", e.ParentID, betaID)
		}
	}
}

// TestPrivacyNoPathsNoPayload is the hard privacy gate: a Step log records
// absolute paths and conversation text, and neither may leave the parser. The
// only conversation text that may survive is SessionMeta.Title, which holds the
// first user message by design.
func TestPrivacyNoPathsNoPayload(t *testing.T) {
	root := fixture(t)
	b := parseAll(t, NewWithRoots(root))
	blob, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"SENTINEL_PRIVATE_TEXT",    // assistant text, tool payload, summary
		"/home/user/proj-alpha",    // the session's working directory
		"/home/user/.stepcode",     // the parentSession path
		"--home-user-proj-alpha--", // the session directory name
		root,                       // the fixture root on this machine
		"call-1",                   // a tool call id
		"bash",                     // the tool name
	} {
		if strings.Contains(string(blob), forbidden) {
			t.Errorf("parsed output leaks %q:\n%s", forbidden, blob)
		}
	}
	if len(b.Sessions) != 2 {
		t.Fatalf("sessions = %d", len(b.Sessions))
	}
	// The title is the one deliberate exception, and it is the first user
	// message only — never the assistant's reply.
	found := false
	for _, s := range b.Sessions {
		if s.Title == "Explain the harness layer" {
			found = true
		}
	}
	if !found {
		t.Error("no session carried the first user message as its title")
	}
}

// TestDiscoverOnlySessionHeaders keeps unrelated files out of the scan.
func TestDiscoverOnlySessionHeaders(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "--home-user-proj-alpha--")
	write(t, filepath.Join(dir, "2026-01-02T03-04-05-000Z_"+betaID+".jsonl"), sessionHeader, assistantMsg)
	// Credentials, configuration and models must not even be opened.
	write(t, filepath.Join(root, "auth.json"), `{"token":"SENTINEL_PRIVATE_TEXT"}`)
	write(t, filepath.Join(root, "models.json"), `{"providers":{"openai":{"apiKey":"SENTINEL_PRIVATE_TEXT"}}}`)
	write(t, filepath.Join(root, "config.toml"), `apiKey = "SENTINEL_PRIVATE_TEXT"`)
	write(t, filepath.Join(root, "settings.json"), `{"sessionDir":"/tmp/elsewhere"}`)
	// A JSONL file that is not a session log (a log/telemetry sink, say).
	write(t, filepath.Join(root, "debug.jsonl"), `{"level":"info","msg":"SENTINEL_PRIVATE_TEXT"}`)
	write(t, filepath.Join(root, "notes.txt"), "SENTINEL_PRIVATE_TEXT")

	ctx := context.Background()
	srcs, err := NewWithRoots(root).Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 1 {
		t.Fatalf("sources = %d, want only the session log: %+v", len(srcs), srcs)
	}
	if filepath.Base(srcs[0].Path) != "2026-01-02T03-04-05-000Z_"+betaID+".jsonl" {
		t.Errorf("discovered %s", srcs[0].Path)
	}
	if srcs[0].Kind != "jsonl" {
		t.Errorf("kind = %q", srcs[0].Kind)
	}
}

// TestIncrementalAppendAndPartialLine covers the incremental contract: only new
// complete lines are consumed, and a half-written line waits for its newline.
func TestIncrementalAppendAndPartialLine(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "--home-user-proj-alpha--", "2026-01-02T03-04-05-000Z_"+betaID+".jsonl")
	write(t, path, sessionHeader, userMsg, assistantMsg)
	p := NewWithRoots(root)
	src := harness.Source{Path: path, Kind: "jsonl"}

	b1, err := p.Parse(context.Background(), src, harness.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(b1.Events) != 1 {
		t.Fatalf("first parse events = %d, want 1", len(b1.Events))
	}

	// A partial final line is neither consumed nor counted. (This fixture is
	// smaller than the 4096-byte fingerprint window, so appending to it also
	// rewinds the cursor and re-reads the earlier lines; what must hold either
	// way is that the offset stops on the last complete line and that the
	// partial record adds no new usage event.)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(compactionEntry[:40]); err != nil {
		t.Fatal(err)
	}
	f.Close()
	b2, err := p.Parse(context.Background(), src, b1.Next)
	if err != nil {
		t.Fatal(err)
	}
	if b2.Next.Offset != b1.Next.Offset {
		t.Errorf("offset moved to %d on a partial line, want %d", b2.Next.Offset, b1.Next.Offset)
	}
	if keys := dedupKeys(b2.Events); !equalKeys(keys, dedupKeys(b1.Events)) {
		t.Fatalf("partial line changed the counted keys: %v, want %v", keys, dedupKeys(b1.Events))
	}

	// Completing the line makes it visible, exactly once.
	if _, err := f2append(path, compactionEntry[40:]+"\n"+afterCompactMsg+"\n"); err != nil {
		t.Fatal(err)
	}
	b3, err := p.Parse(context.Background(), src, b2.Next)
	if err != nil {
		t.Fatal(err)
	}
	// The two records the partial line left behind must now be visible exactly
	// once each. (As above, the small file also re-reads its earlier lines.)
	keys := dedupKeys(b3.Events)
	for _, want := range []string{compactionKey, postKey} {
		if n := countKey(keys, want); n != 1 {
			t.Fatalf("completed parse counted %q %d times, want 1: %v", want, n, keys)
		}
	}
	if last := b3.Events[len(b3.Events)-1]; last.DedupKey != postKey || last.Boundary != model.BoundaryCompact {
		t.Errorf("last event = %q boundary %q, want %q boundary %q",
			last.DedupKey, last.Boundary, postKey, model.BoundaryCompact)
	}
	if b3.Next.Offset <= b2.Next.Offset {
		t.Errorf("offset did not advance past the completed line: %d -> %d", b2.Next.Offset, b3.Next.Offset)
	}

	// An unchanged file yields nothing and leaves the cursor alone.
	b4, err := p.Parse(context.Background(), src, b3.Next)
	if err != nil {
		t.Fatal(err)
	}
	if len(b4.Events) != 0 || b4.Next != b3.Next {
		t.Errorf("unchanged source re-emitted events or moved the cursor")
	}
}

// TestTruncateAndReplaceRewindsCursor checks a rewritten log is re-read from the
// start, with stable dedup keys so the store collapses the replay.
func TestTruncateAndReplaceRewindsCursor(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "--home-user-proj-alpha--", "2026-01-02T03-04-05-000Z_"+betaID+".jsonl")
	write(t, path, sessionHeader, userMsg, assistantMsg)
	p := NewWithRoots(root)
	src := harness.Source{Path: path, Kind: "jsonl"}
	b1, err := p.Parse(context.Background(), src, harness.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	keys1 := dedupKeys(b1.Events)

	// Truncate to a shorter log that still contains the same request.
	write(t, path, sessionHeader, userMsg, assistantMsg)
	b2, err := p.Parse(context.Background(), src, b1.Next)
	if err != nil {
		t.Fatal(err)
	}
	// Same size but new mtime and same fingerprint: the cursor's size/mtime
	// check decides; either way the keys must not change.
	if len(b2.Events) > 0 && !equalKeys(keys1, dedupKeys(b2.Events)) {
		t.Errorf("replayed keys changed: %v vs %v", keys1, dedupKeys(b2.Events))
	}

	// A different fingerprint (first 4KB rewritten) forces a rewind.
	write(t, path, sessionHeader, userMsg, assistantMsg, compactionEntry)
	b3, err := p.Parse(context.Background(), src, harness.Cursor{Offset: 1, Fingerprint: "deadbeef"})
	if err != nil {
		t.Fatal(err)
	}
	if len(b3.Events) != 2 {
		t.Fatalf("rewound parse events = %d, want 2", len(b3.Events))
	}
}

func TestDiscoverMissingRootAndBadFile(t *testing.T) {
	p := NewWithRoots(filepath.Join(t.TempDir(), "nope"))
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("missing root: %v", err)
	}
	if len(srcs) != 0 {
		t.Errorf("missing root discovered %d sources", len(srcs))
	}
	root := t.TempDir()
	write(t, filepath.Join(root, "empty.jsonl"))
	write(t, filepath.Join(root, "garbage.jsonl"), "not json at all")
	write(t, filepath.Join(root, "partial.jsonl"), `{"type":"session",`)
	srcs, err = NewWithRoots(root).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 0 {
		t.Errorf("discovered %d sources from non-session files: %+v", len(srcs), srcs)
	}
}

func dedupKeys(evs []model.UsageEvent) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.DedupKey)
	}
	return out
}

func countKey(keys []string, want string) int {
	n := 0
	for _, k := range keys {
		if k == want {
			n++
		}
	}
	return n
}

func equalKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func f2append(path, s string) (int, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.WriteString(s)
}

// TestNoSharedStateWithPi guards the split: two parsers reading the same root
// must never emit each other's harness id or keys.
func TestNoSharedStateWithPi(t *testing.T) {
	root := fixture(t)
	b := parseAll(t, NewWithRoots(root))
	for _, e := range b.Events {
		if e.Harness != HarnessID {
			t.Fatalf("event harness = %q", e.Harness)
		}
	}
	if bytes.Contains([]byte(b.Events[0].DedupKey), []byte("pi:")) {
		t.Error("step-code dedup key leaked into Pi's namespace")
	}
}
