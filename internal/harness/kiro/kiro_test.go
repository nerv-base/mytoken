package kiro

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

func TestNormalizeModelID(t *testing.T) {
	cases := map[string]string{
		"claude-sonnet-4.5": "claude-sonnet-4-5",
		"gpt-5.5-mini":      "gpt-5-5-mini",
		"auto":              autoModelID,
		"":                  autoModelID,
		"  AUTO  ":          autoModelID,
		"claude-haiku-4-6":  "claude-haiku-4-6",
	}
	for in, want := range cases {
		if got := normalizeKiroModelID(in); got != want {
			t.Errorf("normalizeKiroModelID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEstimateTokens(t *testing.T) {
	cases := map[int]int64{0: 0, -3: 0, 1: 1, 4: 1, 5: 2, 400: 100, 401: 101}
	for in, want := range cases {
		if got := estimateTokens(in); got != want {
			t.Errorf("estimateTokens(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestExtractText(t *testing.T) {
	cases := map[string]string{
		`"plain"`:                       "plain",
		`["a","b"]`:                     "a\nb",
		`[{"type":"text","text":"hi"}]`: "hi",
		`{"content":[{"text":"deep"}]}`: "deep",
		`""`:                            "",
		`null`:                          "",
	}
	for in, want := range cases {
		if got := extractText(json.RawMessage(in)); got != want {
			t.Errorf("extractText(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestParseTimestamp(t *testing.T) {
	want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		raw  string
		want time.Time
	}{
		{`1767323045`, want},
		{`1767323045000`, want},
		{`"1767323045"`, want},
		{`"2026-01-02T03:04:05Z"`, want},
		{`12345`, time.Time{}},
		{`0`, time.Time{}},
		{`"not a time"`, time.Time{}},
		{`"2020-01-02T03:04:05Z"`, time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)},
		{`"1999-01-02T03:04:05Z"`, time.Time{}},
	}
	for _, tc := range cases {
		if got := parseKiroTimestamp(json.RawMessage(tc.raw)); !got.Equal(tc.want) {
			t.Errorf("parseKiroTimestamp(%s) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestCharCount(t *testing.T) {
	if got := charCount("héllo"); got != 5 {
		t.Errorf("charCount = %d, want 5", got)
	}
	if got := charCount(""); got != 0 {
		t.Errorf("charCount(\"\") = %d, want 0", got)
	}
}

func TestDiscoverLayouts(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("sessions/cli/cli-log.jsonl", "{}\n")
	write("sessions/cli/cli-log.json", "{}")
	write("sessions/hash/sess_alpha/messages.jsonl", "{}\n")
	write("sessions/hash/other/messages.jsonl", "{}\n")
	write("a1b2c3d4e5f60718293a4b5c6d7e8f90/exec-0001.chat", "{}")
	write("a1b2c3d4e5f60718293a4b5c6d7e8f90/nested/exec-0002", "{}")
	write("a1b2c3d4e5f60718293a4b5c6d7e8f90/notes.txt", "{}")
	write("workspace-sessions/YWJj_/session-a.json", "{}")
	write("workspace-sessions/YWJj_/sessions.json", "{}")

	srcs, err := NewWithRoots(root).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, src := range srcs {
		rel, err := filepath.Rel(root, src.Path)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, filepath.ToSlash(rel)+" ("+src.Kind+")")
	}
	sort.Strings(got)
	want := []string{
		"a1b2c3d4e5f60718293a4b5c6d7e8f90/exec-0001.chat (json)",
		"a1b2c3d4e5f60718293a4b5c6d7e8f90/nested/exec-0002 (json)",
		"sessions/cli/cli-log.jsonl (jsonl)",
		"sessions/hash/sess_alpha/messages.jsonl (jsonl)",
		"workspace-sessions/YWJj_/session-a.json (json)",
	}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("Discover found %d sources (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("source %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWorkspaceSessionUsesFileModTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session-a.json")
	doc := `{"sessionId":"ws-fixture-1","selectedModel":"auto","workspaceDirectory":"/home/user/proj",` +
		`"history":[{"message":{"role":"user","content":"fixture: list the local totals"}},` +
		`{"message":{"role":"assistant","content":"listed the local totals"}}]}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	batch, err := NewWithRoots(dir).Parse(context.Background(), harness.Source{Path: path, Kind: kindJSON}, harness.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(batch.Events))
	}
	event := batch.Events[0]
	if event.DedupKey != "kiro:ws-session:ws-fixture-1" {
		t.Errorf("DedupKey = %q", event.DedupKey)
	}
	if event.Model != autoModelID {
		t.Errorf("Model = %q, want %q", event.Model, autoModelID)
	}
	if event.ProjectPath != "/home/user/proj" {
		t.Errorf("ProjectPath = %q", event.ProjectPath)
	}
	if !event.Timestamp.Equal(when) {
		t.Errorf("Timestamp = %v, want %v", event.Timestamp, when)
	}
	if event.Tokens.Input == 0 || event.Tokens.Output == 0 {
		t.Errorf("tokens = %+v, want non-zero input and output", event.Tokens)
	}
	if len(batch.Sessions) != 1 || batch.Sessions[0].SessionID != "ws-fixture-1" {
		t.Errorf("sessions = %+v", batch.Sessions)
	}
}

func TestWorkspaceSessionOfExecutionStubsIsSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session-b.json")
	doc := `{"sessionId":"ws-fixture-2","workspaceDirectory":"/home/user/proj",` +
		`"history":[{"executionId":"exec-1","message":{"role":"user","content":"hello"}},` +
		`{"executionId":"exec-2","message":{"role":"assistant","content":"world"}}]}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	batch, err := NewWithRoots(dir).Parse(context.Background(), harness.Source{Path: path, Kind: kindJSON}, harness.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Events) != 0 || len(batch.Sessions) != 0 {
		t.Errorf("execution-backed stub produced %d events / %d sessions", len(batch.Events), len(batch.Sessions))
	}
}

func TestUnchangedSourceReemitsNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exec-0001.chat")
	doc := `{"executionId":"exec-1","chat":[{"role":"human","content":"hello"},{"role":"bot","content":"world"}],` +
		`"metadata":{"modelId":"claude-sonnet-4.5","workflowId":"wf-1","startTime":1767323080000}}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	parser := NewWithRoots(dir)
	src := harness.Source{Path: path, Kind: kindJSON}
	first, err := parser.Parse(context.Background(), src, harness.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 1 {
		t.Fatalf("cold parse produced %d events, want 1", len(first.Events))
	}
	// Chat documents carry no metering usage, so the event has no bill.
	if first.Events[0].Bill != nil {
		t.Errorf("bill = %+v, want none for a chat document", first.Events[0].Bill)
	}
	if first.Events[0].Model != "claude-sonnet-4-5" {
		t.Errorf("model = %q, want claude-sonnet-4-5", first.Events[0].Model)
	}
	second, err := parser.Parse(context.Background(), src, first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 0 {
		t.Errorf("unchanged source re-emitted %d events", len(second.Events))
	}
	if second.Next != first.Next {
		t.Errorf("cursor moved on an unchanged source:\n was %+v\n got %+v", first.Next, second.Next)
	}
}

func TestExecutionIndexAndEmptyPromptAreSkipped(t *testing.T) {
	dir := t.TempDir()
	for name, doc := range map[string]string{
		"index":  `{"sessionId":"s-1","executions":[{"id":"e-1"}]}`,
		"prompt": `{"executionId":"e-2","sessionId":"s-2","modelId":"claude-sonnet-4.5","prompt":"only a prompt"}`,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		batch, err := NewWithRoots(dir).Parse(context.Background(), harness.Source{Path: path, Kind: kindJSON}, harness.Cursor{})
		if err != nil {
			t.Fatal(err)
		}
		if len(batch.Events) != 0 {
			t.Errorf("%s produced %d events, want 0", name, len(batch.Events))
		}
	}
}

func TestHarnessAndRoots(t *testing.T) {
	parser := NewWithRoots("  ", "/tmp/kiro-a", "/tmp/kiro-a", "/tmp/kiro-b")
	if got := parser.Harness(); got != model.Kiro {
		t.Errorf("Harness = %q", got)
	}
	roots := parser.Roots()
	// Roots are cleaned, so on Windows the slashes turn into backslashes.
	if len(roots) != 2 || roots[0] != filepath.Clean("/tmp/kiro-a") || roots[1] != filepath.Clean("/tmp/kiro-b") {
		t.Errorf("Roots = %v", roots)
	}
}
