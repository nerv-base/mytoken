package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
)

// branchLine renders a minimal assistant transcript line. The real transcript
// records gitBranch per request, which is the only branch evidence this parser
// is allowed to use: it is what the log said, not what the working tree says.
func branchLine(session, request, branch, ts string) string {
	field := ""
	if branch != "" {
		field = `"gitBranch":"` + branch + `",`
	}
	return `{"type":"assistant","isSidechain":false,` + field +
		`"message":{"id":"` + request + `","model":"claude-opus-4-6","role":"assistant","usage":{"input_tokens":10,"output_tokens":2}},` +
		`"requestId":"` + request + `","uuid":"u-` + request + `","sessionId":"` + session + `","cwd":"/tmp/repo","timestamp":"` + ts + `"}` + "\n"
}

// branchFiller keeps the fingerprinted prefix stable while a transcript is
// appended to, exactly like the real append-only log's own early lines.
const branchFiller = `{"type":"file-history-snapshot","messageId":"snap","timestamp":"2026-01-02T03:04:00.000Z"}` + "\n"

func writeBranchFile(t *testing.T, body string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "sess-b.jsonl")
	if err := os.WriteFile(path, []byte(strings.Repeat(branchFiller, 80)+body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func appendBranchFile(t *testing.T, path, body string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestGitBranchRecordedPerRequest checks that each request carries the branch
// its own line recorded, so a session that switches branch is reported as two
// branches instead of one session-wide answer.
func TestGitBranchRecordedPerRequest(t *testing.T) {
	dir, path := writeBranchFile(t,
		branchLine("s", "r1", "main", "2026-01-02T03:04:05.000Z")+
			branchLine("s", "r2", "feature/x", "2026-01-02T03:04:06.000Z"))
	b := parseFile(t, NewWithRoots(dir), path, harness.Cursor{})
	if len(b.Events) != 2 {
		t.Fatalf("events = %v, want 2", keys(b))
	}
	if got := eventByKey(t, b, "claude:r1:r1").GitBranch; got != "main" {
		t.Errorf("first request branch = %q, want main", got)
	}
	if got := eventByKey(t, b, "claude:r2:r2").GitBranch; got != "feature/x" {
		t.Errorf("second request branch = %q, want feature/x", got)
	}
}

// TestGitBranchAbsentIsUnknown checks that a transcript which never states a
// branch yields requests with no branch rather than inventing one from the
// working tree or from a later session.
func TestGitBranchAbsentIsUnknown(t *testing.T) {
	dir, path := writeBranchFile(t, branchLine("s", "r1", "", "2026-01-02T03:04:05.000Z"))
	b := parseFile(t, NewWithRoots(dir), path, harness.Cursor{})
	if len(b.Events) != 1 || b.Events[0].GitBranch != "" {
		t.Fatalf("events = %+v, want one request with no branch", b.Events)
	}
}

// TestGitBranchSurvivesIncrementalParse checks the hard part of the cursor
// contract: a branch observed before the cursor is remembered for lines that
// follow it, and is never applied backwards to a request that was already
// emitted under a different branch.
func TestGitBranchSurvivesIncrementalParse(t *testing.T) {
	dir, path := writeBranchFile(t, branchLine("s", "r1", "main", "2026-01-02T03:04:05.000Z"))
	p := NewWithRoots(dir)
	first := parseFile(t, p, path, harness.Cursor{})
	if got := first.Events[0].GitBranch; got != "main" {
		t.Fatalf("first pass branch = %q, want main", got)
	}

	// Append a request whose line omits gitBranch, then one that switches.
	appendBranchFile(t, path, branchLine("s", "r2", "", "2026-01-02T03:04:06.000Z")+branchLine("s", "r3", "release/2", "2026-01-02T03:04:07.000Z"))

	second := parseFile(t, p, path, first.Next)
	if len(second.Events) != 2 {
		t.Fatalf("resumed events = %v, want 2", keys(second))
	}
	// r2 omitted the field, so the last branch seen before it still applies.
	if got := eventByKey(t, second, "claude:r2:r2").GitBranch; got != "main" {
		t.Errorf("carried branch = %q, want main", got)
	}
	// r3 states its own branch and must not change anything already stored.
	if got := eventByKey(t, second, "claude:r3:r3").GitBranch; got != "release/2" {
		t.Errorf("switched branch = %q, want release/2", got)
	}
	if got := eventByKey(t, second, "claude:r2:r2").GitBranch; got != "main" {
		t.Errorf("r2 branch changed after r3: %q", got)
	}
	// A fresh parse must agree with the incremental one: no backward bleed.
	oneShot := parseFile(t, p, path, harness.Cursor{})
	if got := eventByKey(t, oneShot, "claude:r2:r2").GitBranch; got != "main" {
		t.Errorf("cold parse r2 branch = %q, want main", got)
	}
}

// TestGitBranchCursorIsBackwardCompatible checks that a cursor written before
// the branch field existed still resumes: the state decodes, the offset is
// honoured, and the new field simply starts empty.
func TestGitBranchCursorIsBackwardCompatible(t *testing.T) {
	dir, path := writeBranchFile(t, branchLine("s", "r1", "main", "2026-01-02T03:04:05.000Z")+branchLine("s", "r2", "main", "2026-01-02T03:04:06.000Z"))
	p := NewWithRoots(dir)
	first := parseFile(t, p, path, harness.Cursor{})
	if strings.Contains(first.Next.Extra, "gitBranch") {
		t.Errorf("cursor state embeds the raw transcript field: %s", first.Next.Extra)
	}
	appendBranchFile(t, path, branchLine("s", "r3", "main", "2026-01-02T03:04:07.000Z"))
	// An old cursor has no branch in its JSON blob; resume must still work and
	// still report the branch of the appended line.
	legacy := first.Next
	legacy.Extra = `{"p":"/tmp/repo"}`
	got := parseFile(t, p, path, legacy)
	if len(got.Events) != 1 || got.Events[0].GitBranch != "main" {
		t.Fatalf("legacy cursor resume = %+v, want the appended main request", got.Events)
	}
}
