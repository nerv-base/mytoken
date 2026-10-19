package kimi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
	"github.com/zzstar101/mytoken/internal/model"
)

// protocol11Fixture is the Kimi Code CLI layout: a session directory holds
// agents/main/wire.jsonl for the session itself and agents/<agent>/wire.jsonl
// for every sub-agent. It lives under the legacy fixture's seed/ directory,
// which harnesstest never copies into a parsed root, so the legacy fixture and
// its golden file stay byte-for-byte unchanged.
const protocol11Fixture = "testdata/conformance/kimi/seed/protocol11"

func protocol11Case() harnesstest.Case {
	return harnesstest.Case{
		Name:    string(model.Kimi),
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: protocol11Fixture,
	}
}

func TestProtocol11Conformance(t *testing.T) {
	harnesstest.Run(t, protocol11Case())
}

func BenchmarkProtocol11Conformance(b *testing.B) {
	harnesstest.Bench(b, protocol11Case())
}

// TestRootProvenanceCoversLegacyAndKimiCode pins the default root set: both the
// legacy share directory and the Kimi Code home are watched, each explicit
// variable replaces only its own default, MYTOKEN_KIMI_DIRS replaces both, and
// two variables naming the same directory never yield a duplicate path.
func TestRootProvenanceCoversLegacyAndKimiCode(t *testing.T) {
	home := harness.Home()
	legacy := filepath.Join(home, ".kimi", "sessions")
	code := filepath.Join(home, ".kimi-code", "sessions")
	share := t.TempDir()
	codeHome := t.TempDir()

	for _, tc := range []struct {
		name string
		env  map[string]string
		want []harness.Root
	}{
		{
			name: "defaults cover both layouts",
			want: []harness.Root{
				{Path: legacy, Origin: harness.OriginBuiltin},
				{Path: code, Origin: harness.OriginBuiltin},
			},
		},
		{
			name: "kimi share dir replaces the legacy default only",
			env:  map[string]string{"KIMI_SHARE_DIR": share},
			want: []harness.Root{
				{Path: filepath.Join(share, "sessions"), Origin: harness.OriginEnv, Env: "KIMI_SHARE_DIR"},
				{Path: code, Origin: harness.OriginBuiltin},
			},
		},
		{
			name: "kimi code home replaces the new default only",
			env:  map[string]string{"KIMI_CODE_HOME": codeHome},
			want: []harness.Root{
				{Path: legacy, Origin: harness.OriginBuiltin},
				{Path: filepath.Join(codeHome, "sessions"), Origin: harness.OriginEnv, Env: "KIMI_CODE_HOME"},
			},
		},
		{
			name: "both overrides apply independently",
			env:  map[string]string{"KIMI_SHARE_DIR": share, "KIMI_CODE_HOME": codeHome},
			want: []harness.Root{
				{Path: filepath.Join(share, "sessions"), Origin: harness.OriginEnv, Env: "KIMI_SHARE_DIR"},
				{Path: filepath.Join(codeHome, "sessions"), Origin: harness.OriginEnv, Env: "KIMI_CODE_HOME"},
			},
		},
		{
			name: "mytoken dirs replaces the whole set",
			env: map[string]string{
				"MYTOKEN_KIMI_DIRS": share,
				"KIMI_SHARE_DIR":    codeHome,
				"KIMI_CODE_HOME":    codeHome,
			},
			want: []harness.Root{{Path: share, Origin: harness.OriginEnv, Env: "MYTOKEN_KIMI_DIRS"}},
		},
		{
			// KIMI_CODE_HOME replaces the new default with the legacy one: both
			// roots name the same directory, so they collapse into a single
			// root that still carries the override's provenance.
			name: "an override shadowing the legacy default collapses to one root",
			env:  map[string]string{"KIMI_CODE_HOME": filepath.Join(home, ".kimi")},
			want: []harness.Root{
				{Path: legacy, Origin: harness.OriginEnv, Env: "KIMI_CODE_HOME"},
			},
		},
		{
			name: "a blank override is absent",
			env:  map[string]string{"KIMI_SHARE_DIR": "   ", "KIMI_CODE_HOME": "  "},
			want: []harness.Root{
				{Path: legacy, Origin: harness.OriginBuiltin},
				{Path: code, Origin: harness.OriginBuiltin},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blankKimiEnv(t)
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			got, err := rootProvenance()
			if err != nil {
				t.Fatalf("rootProvenance: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("roots = %+v, want %+v", got, tc.want)
			}
			seen := map[string]bool{}
			for _, r := range got {
				key := filepath.Clean(r.Path)
				if seen[key] {
					t.Errorf("duplicate root path %q in %+v", r.Path, got)
				}
				if strings.TrimSpace(r.Path) == "" {
					t.Errorf("empty root path in %+v", got)
				}
				seen[key] = true
			}
		})
	}
}

func TestRootProvenanceMalformedDirsIsAnError(t *testing.T) {
	blankKimiEnv(t)
	t.Setenv("MYTOKEN_KIMI_DIRS", string(filepath.ListSeparator))
	roots, err := rootProvenance()
	var ele *harness.EnvListError
	if !errors.As(err, &ele) || ele.Env != "MYTOKEN_KIMI_DIRS" {
		t.Fatalf("rootProvenance error = %v (%T), want *harness.EnvListError naming MYTOKEN_KIMI_DIRS", err, err)
	}
	if len(roots) != 0 {
		t.Errorf("a malformed MYTOKEN_KIMI_DIRS fell back to roots %+v", roots)
	}
}

func TestSourceIDs(t *testing.T) {
	for _, tc := range []struct {
		rel                        string
		workDir, session, parentID string
	}{
		{"wd_a/session-legacy/wire.jsonl", "wd_a", "session-legacy", ""},
		{"wd_a/session-legacy/subagents/agent-a/wire.jsonl", "wd_a", "agent-a", "session-legacy"},
		{"wd_a/session-new/agents/main/wire.jsonl", "wd_a", "session-new", ""},
		{"wd_a/session-new/agents/agent-0/wire.jsonl", "wd_a", "session-new:agent-0", "session-new"},
	} {
		workDir, session, parentID := sourceIDs(tc.rel)
		if workDir != tc.workDir || session != tc.session || parentID != tc.parentID {
			t.Errorf("sourceIDs(%q) = (%q, %q, %q), want (%q, %q, %q)",
				tc.rel, workDir, session, parentID, tc.workDir, tc.session, tc.parentID)
		}
	}
}

func TestDiscoverFindsBothLayoutsInOneRoot(t *testing.T) {
	root := t.TempDir()
	want := []string{
		"wd_legacy/session-old/subagents/agent-a/wire.jsonl",
		"wd_legacy/session-old/subagents/agent-b/wire.jsonl",
		"wd_legacy/session-old/wire.jsonl",
		"wd_new/session-fresh/agents/agent-0/wire.jsonl",
		"wd_new/session-fresh/agents/main/wire.jsonl",
	}
	for _, rel := range want {
		writeTreeFile(t, root, rel, "{}\n")
	}
	// An agent directory without a wire.jsonl is not a source.
	if err := os.MkdirAll(filepath.Join(root, "wd_new", "session-fresh", "agents", "agent-1"), 0o755); err != nil {
		t.Fatal(err)
	}

	srcs, err := NewWithRoots(root).Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var got []string
	for _, s := range srcs {
		if s.Kind != kindJSONL {
			t.Errorf("source %s has kind %q, want %q", s.Path, s.Kind, kindJSONL)
		}
		got = append(got, relTo(root, s.Path))
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sources = %v, want %v", got, want)
	}

	missing, err := NewWithRoots(filepath.Join(t.TempDir(), "no-such-root")).Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover on a missing root: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("Discover on a missing root returned %d sources: %v", len(missing), missing)
	}
}

// TestProtocol11TimestampsAreMilliseconds pins the protocol 1.1 unit: "time" is
// epoch milliseconds even when the value is small enough that the legacy
// seconds heuristic would rescale it.
func TestProtocol11TimestampsAreMilliseconds(t *testing.T) {
	root := t.TempDir()
	const rel = "wd_z/session-ms/agents/main/wire.jsonl"
	line := `{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":1,"output":1},"time":1500}`
	writeTreeFile(t, root, rel, line+"\n")

	b := parsePath(t, root, filepath.Join(root, filepath.FromSlash(rel)), harness.Cursor{})
	if len(b.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(b.Events))
	}
	if got, want := b.Events[0].Timestamp.UTC(), time.UnixMilli(1500).UTC(); !got.Equal(want) {
		t.Errorf("event time = %s, want %s (epoch milliseconds)", got, want)
	}
}

// TestProtocol11TrailingPartialLineIsNotConsumed keeps the cursor honest when a
// log ends mid-record: the incomplete line is reread once it is complete,
// neither lost nor counted twice.
func TestProtocol11TrailingPartialLineIsNotConsumed(t *testing.T) {
	root := t.TempDir()
	const rel = "wd_p/session-partial/agents/main/wire.jsonl"
	path := filepath.Join(root, filepath.FromSlash(rel))
	pad := `{"type":"ignored","filler":"` + strings.Repeat("x", 5000) + `"}`
	first := `{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":3,"output":1},"time":1742400300000}`
	second := `{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":4,"output":2},"time":1742400301000}`
	prefix := pad + "\n" + first + "\n"
	writeTreeFile(t, root, rel, prefix+second[:len(second)/2])

	b := parsePath(t, root, path, harness.Cursor{})
	if len(b.Events) != 1 {
		t.Fatalf("a partial trailing line produced %d events, want 1", len(b.Events))
	}
	if got, want := b.Next.Offset, int64(len(prefix)); got != want {
		t.Errorf("cursor offset = %d, want %d (the partial line must not be consumed)", got, want)
	}

	if err := os.WriteFile(path, []byte(prefix+second+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	next := parsePath(t, root, path, b.Next)
	if len(next.Events) != 1 {
		t.Fatalf("completing the line produced %d events, want 1: %v", len(next.Events), keysOf(next.Events))
	}
	if next.Events[0].DedupKey == b.Events[0].DedupKey {
		t.Errorf("the record completed by the increment reused key %q", b.Events[0].DedupKey)
	}
}

// TestParseProtocol11UsageRecordIsAuthoritative pins the protocol 1.1 metering
// rule: usage.record is the only source of tokens, and the context loop step
// that carries the same usage must not be counted a second time.
func TestParseProtocol11UsageRecordIsAuthoritative(t *testing.T) {
	events, _, _ := parseProtocol11(t)

	var main []model.UsageEvent
	for _, e := range events {
		if e.SessionID == "session-20250319-160000" {
			main = append(main, e)
		}
	}
	if len(main) != 2 {
		t.Fatalf("parent session produced %d events, want 2 (the matching step.end must not double count)\n%s",
			len(main), eventSummary(events))
	}
	sort.Slice(main, func(i, j int) bool { return main[i].Timestamp.Before(main[j].Timestamp) })

	if got, want := main[0].Tokens, (model.Tokens{Input: 10, Output: 5}); got != want {
		t.Errorf("first turn tokens = %+v, want %+v", got, want)
	}
	if got, want := main[0].Model, "kimi-k2"; got != want {
		t.Errorf("first turn model = %q, want %q", got, want)
	}
	if got, want := main[0].Timestamp.UTC(), time.UnixMilli(1742400001302).UTC(); !got.Equal(want) {
		t.Errorf("first turn time = %s, want %s", got, want)
	}
	if main[0].ParentID != "" {
		t.Errorf("main agent event has parent %q, want none", main[0].ParentID)
	}
	if got, want := main[1].Tokens, (model.Tokens{Input: 100, Output: 20, CacheRead: 40, CacheWrite: 8}); got != want {
		t.Errorf("second turn tokens = %+v, want %+v", got, want)
	}
	if got, want := main[1].Model, "kimi-k2-turbo"; got != want {
		t.Errorf("second turn model = %q, want %q", got, want)
	}
	for _, e := range events {
		if e.Harness != model.Kimi {
			t.Errorf("event %s has harness %q, want %q", e.DedupKey, e.Harness, model.Kimi)
		}
	}
}

// TestParseProtocol11SubagentsAndSessionMeta covers the sub-agent session ids,
// the parent linkage, the cross-session uniqueness of a repeated agent
// directory name, and the session metadata the protocol 1.1 records carry.
func TestParseProtocol11SubagentsAndSessionMeta(t *testing.T) {
	events, sessions, _ := parseProtocol11(t)

	meta := map[string]model.SessionMeta{}
	for _, s := range sessions {
		meta[s.SessionID] = s
	}
	if len(meta) != 4 {
		t.Fatalf("sessions = %d (%v), want 4", len(meta), meta)
	}
	byEvent := map[string]model.UsageEvent{}
	for _, e := range events {
		byEvent[e.SessionID] = e
	}
	for _, tc := range []struct {
		id, parent string
		tokens     model.Tokens
	}{
		{"session-20250319-160000:agent-0", "session-20250319-160000", model.Tokens{Input: 70, Output: 9, CacheRead: 20, CacheWrite: 3}},
		{"session-20250319-170000:agent-0", "session-20250319-170000", model.Tokens{Input: 15, Output: 2}},
	} {
		e, ok := byEvent[tc.id]
		if !ok {
			t.Errorf("no event for subagent session %q", tc.id)
			continue
		}
		if e.ParentID != tc.parent {
			t.Errorf("%s parent = %q, want %q", tc.id, e.ParentID, tc.parent)
		}
		if e.Tokens != tc.tokens {
			t.Errorf("%s tokens = %+v, want %+v", tc.id, e.Tokens, tc.tokens)
		}
		if s, ok := meta[tc.id]; !ok || s.ParentID != tc.parent {
			t.Errorf("session %q parent = %q, want %q", tc.id, s.ParentID, tc.parent)
		}
	}
	if meta["session-20250319-160000"].ParentID != "" {
		t.Errorf("the main agent session has parent %q, want none", meta["session-20250319-160000"].ParentID)
	}
	for id, tc := range map[string]struct {
		title, project string
		started        time.Time
	}{
		"session-20250319-160000":         {"first prompt for the new layout", "proj-one", time.UnixMilli(1742399999000).UTC()},
		"session-20250319-160000:agent-0": {"first child task", "proj-one", time.UnixMilli(1742400005000).UTC()},
		"session-20250319-170000":         {"second parent session", "other-work", time.UnixMilli(1742400100000).UTC()},
		"session-20250319-170000:agent-0": {"same-named child in another session", "other-work", time.UnixMilli(1742400105000).UTC()},
	} {
		s := meta[id]
		if s.Title != tc.title {
			t.Errorf("%s title = %q, want %q", id, s.Title, tc.title)
		}
		if s.Project != tc.project {
			t.Errorf("%s project = %q, want %q", id, s.Project, tc.project)
		}
		if !s.StartedAt.UTC().Equal(tc.started) {
			t.Errorf("%s startedAt = %s, want %s", id, s.StartedAt.UTC(), tc.started)
		}
	}
}

// TestProtocol11PrivacyFieldsNeverReachOutput keeps the conversation, the
// system prompt, the tool arguments and the full working directory out of every
// parsed value, including the resume cursor.
func TestProtocol11PrivacyFieldsNeverReachOutput(t *testing.T) {
	events, sessions, cursors := parseProtocol11(t)
	blob := mustJSON(t, events)
	blob = append(blob, mustJSON(t, sessions)...)
	blob = append(blob, mustJSON(t, cursors)...)
	for _, secret := range []string{
		harnesstest.SentinelText,
		"/Users/demo",
		"systemPrompt",
		"tool.call",
	} {
		if bytes.Contains(blob, []byte(secret)) {
			t.Errorf("private fixture text %q reached Event/Session/Cursor JSON: …%s…", secret, around(blob, secret))
		}
	}
}

// TestProtocol11DedupKeysAreStableAndDoNotCollide pins the cursor contract for
// records that carry no message id: the key must survive an incremental append
// and a rewrite, and two identical records that state no time of their own must
// not collapse, because equal bytes alone are not evidence that they are the
// same call.
func TestProtocol11DedupKeysAreStableAndDoNotCollide(t *testing.T) {
	root := t.TempDir()
	const rel = "wd_x/session-d/agents/main/wire.jsonl"
	path := filepath.Join(root, filepath.FromSlash(rel))
	meta := `{"type":"metadata","protocol_version":"1.1","created_at":1742400200000}`
	original := `{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":11,"output":2},"time":1742400201302}`
	inserted := `{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":5,"output":1},"time":1742400201200}`

	writeTreeFile(t, root, rel, meta+"\n"+original+"\n")
	first := parsePath(t, root, path, harness.Cursor{})
	if len(first.Events) != 1 {
		t.Fatalf("first parse produced %d events, want 1", len(first.Events))
	}
	key := first.Events[0].DedupKey

	// Rewriting an earlier line changes the fingerprint, so the reader starts
	// over; the untouched usage.record must keep the key it already had even
	// though another record now sits before it.
	writeTreeFile(t, root, rel, meta+"\n"+inserted+"\n"+original+"\n")
	rewritten := parsePath(t, root, path, first.Next)
	if len(rewritten.Events) != 2 {
		t.Fatalf("after rewrite produced %d events, want 2", len(rewritten.Events))
	}
	if !hasKey(rewritten.Events, key) {
		t.Errorf("a rewrite moved the dedup key of an unchanged usage.record: had %q, now %v", key, keysOf(rewritten.Events))
	}

	// Two byte-identical records that carry no time in one file, split across
	// an increment: nothing says they are the same call, so the second must get
	// its own key instead of being merged into the first.
	const rel2 = "wd_y/session-e/agents/main/wire.jsonl"
	root2 := t.TempDir()
	path2 := filepath.Join(root2, filepath.FromSlash(rel2))
	timeless := `{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":11,"output":2}}`
	pad := `{"type":"ignored","filler":"` + strings.Repeat("x", 5000) + `"}`
	writeTreeFile(t, root2, rel2, pad+"\n"+timeless+"\n")
	third := parsePath(t, root2, path2, harness.Cursor{})
	if len(third.Events) != 1 {
		t.Fatalf("padded parse produced %d events, want 1", len(third.Events))
	}
	if err := os.WriteFile(path2, []byte(pad+"\n"+timeless+"\n"+timeless+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fourth := parsePath(t, root2, path2, third.Next)
	if len(fourth.Events) != 1 {
		t.Fatalf("an appended duplicate produced %d events, want 1", len(fourth.Events))
	}
	if fourth.Events[0].DedupKey == third.Events[0].DedupKey {
		t.Errorf("two byte-identical time-less usage records collided on key %q", third.Events[0].DedupKey)
	}
}

// TestProtocol11IdenticalUsageRecordsWithTimeCollapse pins the other half of the
// same rule. A durable usage.record that states its own epoch-millisecond time
// identifies itself: two byte-identical lines are one record written twice, so
// they must produce one event. Downstream the shared key makes the replay an
// upsert of the same row, not a second fact that double counts.
func TestProtocol11IdenticalUsageRecordsWithTimeCollapse(t *testing.T) {
	root := t.TempDir()
	const rel = "wd_dup/session-dup/agents/main/wire.jsonl"
	path := filepath.Join(root, filepath.FromSlash(rel))
	meta := `{"type":"metadata","protocol_version":"1.1","created_at":1742400700000}`
	rec := `{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":12,"output":3},"usageScope":"turn","time":1742400701500}`
	writeTreeFile(t, root, rel, meta+"\n"+rec+"\n"+rec+"\n")

	b := parsePath(t, root, path, harness.Cursor{})
	if len(b.Events) != 1 {
		t.Fatalf("two identical time-bearing usage.records produced %d events, want 1:%s", len(b.Events), eventSummary(b.Events))
	}
	key := b.Events[0].DedupKey

	// Unchanged source: the cursor comes back untouched and nothing is reread.
	if again := parsePath(t, root, path, b.Next); len(again.Events) != 0 {
		t.Errorf("re-reading an unchanged file produced %d events", len(again.Events))
	}

	// A third identical copy is the same durable record again and must keep the
	// key, so the store updates one row instead of inserting a second.
	if err := os.WriteFile(path, []byte(meta+"\n"+rec+"\n"+rec+"\n"+rec+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	next := parsePath(t, root, path, b.Next)
	if len(next.Events) != 1 {
		t.Fatalf("an appended duplicate produced %d events, want 1:%s", len(next.Events), eventSummary(next.Events))
	}
	if next.Events[0].DedupKey != key {
		t.Errorf("an identical time-bearing record minted a new key %q, want %q", next.Events[0].DedupKey, key)
	}

	// A rewrite that changes the fingerprint rereads from the start; the
	// unchanged record must keep the key it already had.
	inserted := `{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":4,"output":1},"usageScope":"turn","time":1742400701200}`
	writeTreeFile(t, root, rel, meta+"\n"+inserted+"\n"+rec+"\n"+rec+"\n")
	rewritten := parsePath(t, root, path, next.Next)
	if !hasKey(rewritten.Events, key) {
		t.Errorf("a rewrite moved the dedup key of an unchanged usage.record: want %q, got %v", key, keysOf(rewritten.Events))
	}
}

// TestProtocol11IdenticalUsageRecordsWithoutTimeStayDistinct guards the
// conservative half of the collapse rule: a record with no usable time gives no
// evidence that two identical lines are the same call, so real measurements are
// never merged on equal bytes alone.
func TestProtocol11IdenticalUsageRecordsWithoutTimeStayDistinct(t *testing.T) {
	root := t.TempDir()
	const rel = "wd_dup2/session-dup2/agents/main/wire.jsonl"
	path := filepath.Join(root, filepath.FromSlash(rel))
	meta := `{"type":"metadata","protocol_version":"1.1","created_at":1742400800000}`
	rec := `{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":12,"output":3},"usageScope":"turn"}`
	writeTreeFile(t, root, rel, meta+"\n"+rec+"\n"+rec+"\n")

	b := parsePath(t, root, path, harness.Cursor{})
	if len(b.Events) != 2 {
		t.Fatalf("two identical time-less usage.records produced %d events, want 2:%s", len(b.Events), eventSummary(b.Events))
	}
	if b.Events[0].DedupKey == b.Events[1].DedupKey {
		t.Errorf("two time-less usage records collided on key %q", b.Events[0].DedupKey)
	}

	// Rereading the same bytes from scratch yields the same two keys.
	fresh := parsePath(t, root, path, harness.Cursor{})
	if got, want := keysOf(fresh.Events), keysOf(b.Events); !reflect.DeepEqual(got, want) {
		t.Errorf("keys moved on a fresh read: got %v, want %v", got, want)
	}
}

// TestProtocol11UsageScopeCountsEveryRequest pins the protocol 1.1 usageScope
// contract against the upstream implementation: every usage.record is one
// request, so every record with a scope upstream defines is counted, whatever
// the scope says. packages/agent-core-v2/src/session/usage/usageAgentModel.ts:
// 27-43 accumulates each UsageRecord into byModel unconditionally, and
// record() only labels that one request "turn" or "session" from source.type;
// apps/vis/server/src/lib/context-projector.ts:393-401 adds every usage.record
// to the cumulative byModel ledger for both scopes, defaulting an absent scope
// to "session". "turn" and "session" are therefore not a cumulative and a
// per-turn view of the same request. The field is optional, so an absent scope
// and the legacy empty string count too. Only a value this parser cannot place
// — an unknown string, or a non-string — stays conservatively ignored. An
// explicit JSON null is one of those non-string values: the upstream schema
// types usageScope as an optional string union, so a present null is not an
// absent field, it is an illegal value, and it must be ignored like any other
// non-string rather than falling back to the absent-field default.
func TestProtocol11UsageScopeCountsEveryRequest(t *testing.T) {
	root := t.TempDir()
	const rel = "wd_scope/session-scope/agents/main/wire.jsonl"
	path := filepath.Join(root, filepath.FromSlash(rel))
	lines := []string{
		`{"type":"metadata","protocol_version":"1.1","created_at":1742400500000}`,
		`{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":10,"output":2},"usageScope":"turn","time":1742400500100}`,
		`{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":20,"output":4},"time":1742400500200}`,
		`{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":30,"output":6},"usageScope":"","time":1742400500300}`,
		`{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":40,"output":8},"usageScope":"session","time":1742400500400}`,
		`{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":1000,"output":500},"usageScope":"background","time":1742400500500}`,
		`{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":2000,"output":9},"usageScope":7,"time":1742400500600}`,
		`{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":3000,"output":9},"usageScope":true,"time":1742400500700}`,
		`{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":4000,"output":9},"usageScope":{"kind":"turn"},"time":1742400500800}`,
		`{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":5000,"output":9},"usageScope":["turn"],"time":1742400500900}`,
		// A present null is an illegal non-string, not an absent field: it must
		// not fall back to the absent-scope default and must not be counted.
		`{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":6000,"output":9},"usageScope":null,"time":1742400501000}`,
	}
	writeTreeFile(t, root, rel, strings.Join(lines, "\n")+"\n")

	b := parsePath(t, root, path, harness.Cursor{})
	// turn + absent + empty + session are four separate requests and all count.
	want := map[int64]int64{10: 2, 20: 4, 30: 6, 40: 8}
	if len(b.Events) != len(want) {
		t.Fatalf("usage.record scopes produced %d events, want %d:%s", len(b.Events), len(want), eventSummary(b.Events))
	}
	for _, e := range b.Events {
		if out, ok := want[e.Tokens.Input]; !ok || e.Tokens.Output != out {
			t.Errorf("unexpected event for scope filtering: %+v (want every upstream scope counted, %v)", e.Tokens, want)
		}
	}

	// Nothing changed, so the same cursor yields no new events.
	if again := parsePath(t, root, path, b.Next); len(again.Events) != 0 {
		t.Errorf("re-reading an unchanged file produced %d events", len(again.Events))
	}
}

// TestProtocol11SessionScopeIsIndependentRequest pins why counting a "session"
// record does not double count its "turn" sibling. Upstream labels one request
// with exactly one of the two scopes, so two records are two requests even when
// they report the same numbers at the same instant: both must be emitted, with
// their own keys, and neither may be merged into the other.
func TestProtocol11SessionScopeIsIndependentRequest(t *testing.T) {
	root := t.TempDir()
	const rel = "wd_scope2/session-scope2/agents/main/wire.jsonl"
	path := filepath.Join(root, filepath.FromSlash(rel))
	meta := `{"type":"metadata","protocol_version":"1.1","created_at":1742400900000}`
	turn := `{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":120,"output":30},"usageScope":"turn","time":1742400900100}`
	sess := `{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":120,"output":30},"usageScope":"session","time":1742400900100}`
	writeTreeFile(t, root, rel, meta+"\n"+turn+"\n"+sess+"\n")

	b := parsePath(t, root, path, harness.Cursor{})
	if len(b.Events) != 2 {
		t.Fatalf("one turn and one session request produced %d events, want 2:%s", len(b.Events), eventSummary(b.Events))
	}
	if b.Events[0].DedupKey == b.Events[1].DedupKey {
		t.Errorf("the turn and session requests collided on key %q", b.Events[0].DedupKey)
	}
	var input, output int64
	for _, e := range b.Events {
		input += e.Tokens.Input
		output += e.Tokens.Output
	}
	if input != 240 || output != 60 {
		t.Errorf("turn+session totals are %d/%d, want 240/60 (each request counted exactly once)", input, output)
	}

	// A durable request is read once; an unchanged file adds nothing.
	if again := parsePath(t, root, path, b.Next); len(again.Events) != 0 {
		t.Errorf("re-reading an unchanged file produced %d events", len(again.Events))
	}
}

// TestProtocol11UnknownScopeDoesNotConsumeDedupState checks that skipping a
// usage.record this parser cannot place is not just a dropped event: the record
// must also leave the persisted occurrence counts alone, so a later increment
// reads exactly the state it would have read had the record never existed. An
// explicit JSON null belongs in that ignored set as a non-string value.
func TestProtocol11UnknownScopeDoesNotConsumeDedupState(t *testing.T) {
	root := t.TempDir()
	const rel = "wd_ignored/session-ignored/agents/main/wire.jsonl"
	path := filepath.Join(root, filepath.FromSlash(rel))
	meta := `{"type":"metadata","protocol_version":"1.1","created_at":1742400600000}`
	unknown := []string{
		`{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":900,"output":300},"usageScope":"background"}`,
		`{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":800,"output":200},"usageScope":"subagent"}`,
		`{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":700,"output":100},"usageScope":3}`,
		`{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":600,"output":100},"usageScope":false}`,
		`{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":500,"output":100},"usageScope":{"turn":true}}`,
		`{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":450,"output":100},"usageScope":null}`,
	}
	body := meta + "\n" + strings.Join(unknown, "\n") + "\n"
	writeTreeFile(t, root, rel, body)

	b := parsePath(t, root, path, harness.Cursor{})
	if len(b.Events) != 0 {
		t.Fatalf("unplaceable scopes produced %d events, want 0:%s", len(b.Events), eventSummary(b.Events))
	}
	if seen := decodeSeen(t, b.Next); len(seen) != 0 {
		t.Errorf("ignored usage.records advanced the occurrence counts: %v", seen)
	}

	// A counted record appended after the ignored ones must key exactly as it
	// would in a file that never contained them.
	turn := `{"type":"usage.record","model":"kimi-k2","usage":{"inputOther":50,"output":5},"usageScope":"turn","time":1742400601000}`
	if err := os.WriteFile(path, []byte(body+turn+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	next := parsePath(t, root, path, b.Next)
	if len(next.Events) != 1 {
		t.Fatalf("the increment produced %d events, want 1:%s", len(next.Events), eventSummary(next.Events))
	}
	refRoot := t.TempDir()
	writeTreeFile(t, refRoot, rel, meta+"\n"+turn+"\n")
	ref := parsePath(t, refRoot, filepath.Join(refRoot, filepath.FromSlash(rel)), harness.Cursor{})
	if len(ref.Events) != 1 {
		t.Fatalf("reference parse produced %d events, want 1", len(ref.Events))
	}
	if next.Events[0].DedupKey != ref.Events[0].DedupKey {
		t.Errorf("ignored scopes changed the counted key: got %q, want %q", next.Events[0].DedupKey, ref.Events[0].DedupKey)
	}
}

// decodeSeen reads the occurrence counts a parser persists in the cursor.
func decodeSeen(t *testing.T, cur harness.Cursor) map[string]int {
	t.Helper()
	if cur.Extra == "" {
		return nil
	}
	var st state
	if err := json.Unmarshal([]byte(cur.Extra), &st); err != nil {
		t.Fatalf("cursor extra %q: %v", cur.Extra, err)
	}
	return st.Seen
}

func parseProtocol11(t *testing.T) ([]model.UsageEvent, []model.SessionMeta, map[string]harness.Cursor) {
	t.Helper()
	ctx := context.Background()
	p := NewWithRoots(protocol11Fixture)
	srcs, err := p.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover(%s): %v", protocol11Fixture, err)
	}
	var events []model.UsageEvent
	var sessions []model.SessionMeta
	cursors := map[string]harness.Cursor{}
	for _, src := range srcs {
		b, err := p.Parse(ctx, src, harness.Cursor{})
		if err != nil {
			t.Fatalf("Parse(%s): %v", src.Path, err)
		}
		events = append(events, b.Events...)
		sessions = append(sessions, b.Sessions...)
		cursors[relTo(protocol11Fixture, src.Path)] = b.Next
	}
	return events, sessions, cursors
}

func parsePath(t *testing.T, root, path string, cur harness.Cursor) harness.Batch {
	t.Helper()
	b, err := NewWithRoots(root).Parse(context.Background(), harness.Source{Path: path, Kind: kindJSONL}, cur)
	if err != nil {
		t.Fatalf("Parse(%s): %v", path, err)
	}
	return b
}

func blankKimiEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"MYTOKEN_KIMI_DIRS", "KIMI_SHARE_DIR", "KIMI_CODE_HOME"} {
		t.Setenv(name, "")
	}
}

func writeTreeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func relTo(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func around(blob []byte, needle string) string {
	i := bytes.Index(blob, []byte(needle))
	if i < 0 {
		return ""
	}
	lo, hi := i-60, i+len(needle)+60
	if lo < 0 {
		lo = 0
	}
	if hi > len(blob) {
		hi = len(blob)
	}
	return string(blob[lo:hi])
}

func hasKey(events []model.UsageEvent, key string) bool {
	for _, e := range events {
		if e.DedupKey == key {
			return true
		}
	}
	return false
}

func keysOf(events []model.UsageEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.DedupKey)
	}
	sort.Strings(out)
	return out
}

func eventSummary(events []model.UsageEvent) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("\n  ")
		b.WriteString(e.DedupKey)
		b.WriteString(" ")
		b.WriteString(e.SessionID)
		fmt.Fprintf(&b, " %+v", e.Tokens)
	}
	return b.String()
}
