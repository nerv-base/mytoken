package qoder

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nerv-base/mytoken/internal/harness"
	"github.com/nerv-base/mytoken/internal/model"
)

func TestNewWithRoots(t *testing.T) {
	dir := t.TempDir()
	p := NewWithRoots(dir)
	if got := p.Roots(); len(got) != 1 || got[0] != dir {
		t.Fatalf("Roots() = %v, want [%s]", got, dir)
	}
	if p.Harness() != model.Qoder {
		t.Fatalf("Harness() = %q, want %q", p.Harness(), model.Qoder)
	}
}

func TestWithRootsEmptyStaysEmpty(t *testing.T) {
	p := NewWithRoots(t.TempDir())
	q, ok := p.WithRoots().(*Parser)
	if !ok {
		t.Fatalf("WithRoots() returned %T", p.WithRoots())
	}
	if got := q.Roots(); len(got) != 0 {
		t.Fatalf("WithRoots() roots = %v, want empty", got)
	}
}

func TestRootProvenancePrecedence(t *testing.T) {
	home := t.TempDir()
	cn := filepath.Join(home, "cn")
	intl := filepath.Join(home, "intl")
	for _, name := range []string{qoderDirsEnv, qoderCNConfigEnv, qoderConfigEnv} {
		t.Setenv(name, "")
	}

	t.Setenv(qoderCNConfigEnv, cn)
	t.Setenv(qoderConfigEnv, intl)
	roots, err := rootProvenance()
	if err != nil {
		t.Fatalf("rootProvenance: %v", err)
	}
	if len(roots) != 2 || roots[0].Path != filepath.Join(cn, projectsDirName) || roots[1].Path != filepath.Join(intl, projectsDirName) {
		t.Fatalf("rootProvenance = %+v, want CN then international projects dirs", roots)
	}
	if roots[0].Origin != harness.OriginEnv || roots[0].Env != qoderCNConfigEnv {
		t.Fatalf("CN root provenance = %+v", roots[0])
	}

	one := filepath.Join(home, "one")
	two := filepath.Join(home, "two")
	t.Setenv(qoderDirsEnv, one+string(os.PathListSeparator)+two)
	roots, err = rootProvenance()
	if err != nil {
		t.Fatalf("rootProvenance: %v", err)
	}
	if len(roots) != 2 || roots[0].Path != one || roots[1].Path != two {
		t.Fatalf("MYTOKEN_QODER_DIRS roots = %+v", roots)
	}

	t.Setenv(qoderDirsEnv, string(os.PathListSeparator))
	if _, err := rootProvenance(); err == nil {
		t.Fatal("rootProvenance accepted a list with no usable directory")
	}
}

func TestRootProvenanceDefaults(t *testing.T) {
	for _, name := range []string{qoderDirsEnv, qoderCNConfigEnv, qoderConfigEnv} {
		t.Setenv(name, "")
	}
	roots, err := rootProvenance()
	if err != nil {
		t.Fatalf("rootProvenance: %v", err)
	}
	want := []string{
		filepath.Join(harness.Home(), configDirCN, projectsDirName),
		filepath.Join(harness.Home(), configDir, projectsDirName),
	}
	if len(roots) != len(want) {
		t.Fatalf("rootProvenance = %+v, want %v", roots, want)
	}
	for i := range want {
		if roots[i].Path != want[i] || roots[i].Origin != harness.OriginBuiltin {
			t.Fatalf("root %d = %+v, want builtin %s", i, roots[i], want[i])
		}
	}
}

func TestParseExplicitTokens(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, projectsDirName, "-tmp-project")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"type":"workspace-directories","sessionId":"s1","directories":["/tmp/project"]}`,
		`{"type":"runtime-config","sessionId":"s1","model":"qfmodel","contextWindow":200000,"timestamp":1792113670600}`,
		`{"type":"user","uuid":"u1","timestamp":"2026-10-16T01:21:22Z","message":{"role":"user","content":[{"type":"text","text":"hello"}]},"origin":{"kind":"human"},"humanInput":{"text":"hello"},"cwd":"/tmp/project","sessionId":"s1"}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-10-16T01:21:23Z","message":{"id":"m1","role":"assistant","model":"qfmodel","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":10,"cache_creation_input_tokens":5,"cache_creation":{"ephemeral_1h_input_tokens":3,"ephemeral_5m_input_tokens":2},"credits":0.5,"request_id":"r1","context_usage_ratio":0.5}},"cwd":"/tmp/project","sessionId":"s1"}`,
	}
	path := filepath.Join(sessionDir, "s1.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := NewWithRoots(root)
	srcs, err := p.Discover(context.Background())
	if err != nil || len(srcs) != 1 {
		t.Fatalf("Discover = %v, %v", srcs, err)
	}
	batch, err := p.Parse(context.Background(), srcs[0], harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(batch.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(batch.Events))
	}
	ev := batch.Events[0]
	want := model.Tokens{Input: 100, Output: 20, CacheRead: 10, CacheWrite: 5, CacheWrite1h: 3}
	if ev.Tokens != want {
		t.Fatalf("tokens = %+v, want %+v", ev.Tokens, want)
	}
	if ev.Bill == nil || ev.Bill.Amount != 0.5 || ev.Bill.Unit != creditsUnit {
		t.Fatalf("bill = %+v, want 0.5 credit", ev.Bill)
	}
	if ev.RequestID != "r1" || ev.Model != "qfmodel" || ev.SessionID != "s1" {
		t.Fatalf("event identity = %+v", ev)
	}
	if len(batch.Sessions) != 1 || batch.Sessions[0].Title != "hello" || batch.Sessions[0].Project != "project" {
		t.Fatalf("sessions = %+v", batch.Sessions)
	}
}

func TestParseEstimatesZeroTokenPlanRows(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, projectsDirName, "-x")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"type":"workspace-directories","sessionId":"s2","directories":["/x"]}`,
		`{"type":"runtime-config","sessionId":"s2","model":"qfmodel","contextWindow":100000,"timestamp":1792113670600}`,
		`{"type":"user","uuid":"u1","timestamp":"2026-10-16T01:21:22Z","message":{"role":"user","content":[{"type":"text","text":"0123456789"}]},"origin":{"kind":"human"},"humanInput":{"text":"0123456789"},"sessionId":"s2"}`,
		`{"type":"assistant","uuid":"a0","timestamp":"2026-10-16T01:21:23Z","message":{"id":"m1","role":"assistant","model":"qfmodel","content":[{"type":"text","text":"abcd"}]},"sessionId":"s2"}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-10-16T01:21:24Z","message":{"id":"m1","role":"assistant","model":"qfmodel","content":[],"usage":{"input_tokens":0,"output_tokens":0,"credits":0.25,"request_id":"r2","context_usage_ratio":0.4}},"sessionId":"s2"}`,
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "s2.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewWithRoots(root)
	srcs, err := p.Discover(context.Background())
	if err != nil || len(srcs) != 1 {
		t.Fatalf("Discover = %v, %v", srcs, err)
	}
	batch, err := p.Parse(context.Background(), srcs[0], harness.Cursor{})
	if err != nil || len(batch.Events) != 1 {
		t.Fatalf("Parse = %+v, %v", batch, err)
	}
	// 0.4 × 100000 for input; the assistant fragment's four characters estimate
	// one output token.
	if got := batch.Events[0].Tokens; got.Input != 40000 || got.Output != 1 {
		t.Fatalf("estimated tokens = %+v, want input 40000 output 1", got)
	}
}

func TestSourceSessions(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, projectsDirName, "-x")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sessionDir, "file-name.jsonl")
	line := `{"type":"workspace-directories","sessionId":"header-id","directories":["/x"]}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	ids, err := NewWithRoots(root).SourceSessions(context.Background(), harness.Source{Path: path})
	if err != nil {
		t.Fatalf("SourceSessions: %v", err)
	}
	if len(ids) != 2 || ids[0] != "file-name" || ids[1] != "header-id" {
		t.Fatalf("SourceSessions = %v, want [file-name header-id]", ids)
	}
}
