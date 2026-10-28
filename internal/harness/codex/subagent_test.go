package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
)

// TestSubagentUsageStaysInItsOwnSession guards the subagent session collapse.
// Codex stamps a subagent rollout's token_usage_record with the *parent*
// thread's session_id, so honouring it reassigned the whole rollout to the
// parent: the subagent never became a session of its own and its usage was
// added to the parent's. session_meta is the rollout's own statement of
// identity and it is always the first line, so it is the only id read here.
func TestSubagentUsageStaysInItsOwnSession(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions", "2026", "01", "02")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-01-02T04-00-00-019bbb00-2222-7333-8444-555566667777.jsonl")
	if err := os.WriteFile(path, []byte(subagentRolloutBody), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewWithRoots(filepath.Dir(dir))
	b := parseFile(t, p, path, harness.Cursor{})

	const wantSession, wantParent = "019bbb00-2222-7333-8444-555566667777", "019aaa00-1111-7222-8333-444455556666"
	if len(b.Events) == 0 {
		t.Fatal("no events parsed from the subagent rollout")
	}
	for i, ev := range b.Events {
		if ev.SessionID != wantSession {
			t.Errorf("event %d session = %q, want %q: the record's parent session_id must not be taken", i, ev.SessionID, wantSession)
		}
		if ev.ParentID != wantParent {
			t.Errorf("event %d parent = %q, want %q", i, ev.ParentID, wantParent)
		}
	}
	if len(b.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(b.Sessions))
	}
	if got := b.Sessions[0].SessionID; got != wantSession {
		t.Errorf("sessionId = %q, want %q", got, wantSession)
	}
	if got := b.Sessions[0].ParentID; got != wantParent {
		t.Errorf("parentId = %q, want %q", got, wantParent)
	}
}

// TestSubagentUsageRecordFallsBackToSessionMetaID covers a rollout whose
// token_usage_record names an id session_meta never stated: the record is not
// allowed to invent an identity, so the id session_meta did state wins.
func TestSubagentUsageRecordFallsBackToSessionMetaID(t *testing.T) {
	body := strings.Replace(subagentRolloutBody, `"session_id":"`+subagentParent+`"`, `"session_id":"019ddd00-4444-7555-8666-777788889999"`, 1)
	dir := filepath.Join(t.TempDir(), "sessions", "2026", "01", "02")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-01-02T04-00-00-019bbb00-2222-7333-8444-555566667777.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewWithRoots(filepath.Dir(dir))
	b := parseFile(t, p, path, harness.Cursor{})
	for i, ev := range b.Events {
		if ev.SessionID != subagentSession {
			t.Errorf("event %d session = %q, want %q", i, ev.SessionID, subagentSession)
		}
	}
}

// TestSessionMetaStampsReplayedHistory covers a subagent rollout that replays
// the parent's usage: every record carries the parent's session_id, and the
// session is still the subagent's own. The dedup that keeps the replay from
// being counted twice lives in the key, which is scoped to the parent — so the
// parent's own rollout collapses it downstream without the subagent losing its
// identity.
func TestSessionMetaStampsReplayedHistory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions", "2026", "01", "02")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-01-02T04-00-00-019bbb00-2222-7333-8444-555566667777.jsonl")
	if err := os.WriteFile(path, []byte(subagentReplayedBody), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewWithRoots(filepath.Dir(dir))
	b := parseFile(t, p, path, harness.Cursor{})
	if len(b.Events) != 1 {
		t.Fatalf("events = %d, want 1 (%v)", len(b.Events), keys(b))
	}
	// The key is scoped to the parent, which is what makes the parent's own
	// rollout collapse this replay instead of double-counting it.
	if want := "codex:" + subagentParent + ":1000-500-0-56-34-1056"; b.Events[0].DedupKey != want {
		t.Errorf("dedupKey = %q, want %q", b.Events[0].DedupKey, want)
	}
	if got := b.Events[0].SessionID; got != subagentSession {
		t.Errorf("session = %q, want %q", got, subagentSession)
	}
	if got := b.Events[0].ParentID; got != subagentParent {
		t.Errorf("parent = %q, want %q", got, subagentParent)
	}
	if len(b.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(b.Sessions))
	}
	if got := b.Sessions[0].SessionID; got != subagentSession {
		t.Errorf("sessionId = %q, want %q", got, subagentSession)
	}
	if got := b.Sessions[0].ParentID; got != subagentParent {
		t.Errorf("parentId = %q, want %q", got, subagentParent)
	}
}

const (
	subagentSession = "019bbb00-2222-7333-8444-555566667777"
	subagentParent  = "019aaa00-1111-7222-8333-444455556666"
)

// subagentRolloutBody is the shape a real Codex Desktop subagent rollout has:
// session_meta names its own id and its parent, then a token_usage_record
// carries the parent's session_id — the line that caused the collapse.
var subagentRolloutBody = `{"timestamp":"2026-01-02T04:00:00.000Z","type":"session_meta","payload":{"session_id":"` + subagentSession + `","id":"` + subagentSession + `","cwd":"/Users/demo/codex-proj","model_provider":"custom","originator":"Codex Desktop","thread_source":"subagent","source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + subagentParent + `","depth":1,"agent_nickname":"Demo"}}}}}
{"timestamp":"2026-01-02T04:00:00.100Z","type":"turn_context","payload":{"turn_id":"019bbb00-bbbb-7000-8000-000000000001","cwd":"/Users/demo/codex-proj","model":"gpt-6.1-sol"}}
{"timestamp":"2026-01-02T04:00:00.200Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Sub rollout prompt"}]}}
{"timestamp":"2026-01-02T04:00:20.000Z","type":"token_usage_record","payload":{"thread_id":"` + subagentSession + `","session_id":"` + subagentParent + `","response_id":"resp_agent_1","usage":{"input_tokens":400,"cached_input_tokens":100,"cache_write_input_tokens":0,"output_tokens":30,"reasoning_output_tokens":10,"total_tokens":430},"thread_token_usage":{"input_tokens":500,"cached_input_tokens":100,"cache_write_input_tokens":0,"output_tokens":40,"reasoning_output_tokens":10,"total_tokens":540}}}
`

// subagentReplayedBody is the same rollout with the parent's cumulative
// snapshot still attached to its own thread id, the state a resumed rollout
// writes before its first turn_context.
var subagentReplayedBody = `{"timestamp":"2026-01-02T04:00:00.000Z","type":"session_meta","payload":{"session_id":"` + subagentSession + `","id":"` + subagentSession + `","cwd":"/Users/demo/codex-proj","model_provider":"custom","thread_source":"subagent","source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + subagentParent + `","depth":1,"agent_nickname":"Demo"}}}}}
{"timestamp":"2026-01-02T04:00:00.100Z","type":"token_usage_record","payload":{"thread_id":"` + subagentSession + `","session_id":"` + subagentParent + `","response_id":"resp_parent_1","usage":{"input_tokens":1000,"cached_input_tokens":500,"cache_write_input_tokens":0,"output_tokens":56,"reasoning_output_tokens":34,"total_tokens":1056},"thread_token_usage":{"input_tokens":1000,"cached_input_tokens":500,"cache_write_input_tokens":0,"output_tokens":56,"reasoning_output_tokens":34,"total_tokens":1056}}}
`

// TestSubagentRolloutBodyIsValidJSON keeps the fixtures above parseable, so a
// typo in them fails here rather than as a confusing parse error.
func TestSubagentRolloutBodyIsValidJSON(t *testing.T) {
	for name, body := range map[string]string{"subagentRolloutBody": subagentRolloutBody, "subagentReplayedBody": subagentReplayedBody} {
		t.Run(name, func(t *testing.T) {
			for i, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
				var probe map[string]any
				if err := json.Unmarshal([]byte(line), &probe); err != nil {
					t.Errorf("line %d is not valid JSON: %v", i+1, err)
				}
			}
		})
	}
}
