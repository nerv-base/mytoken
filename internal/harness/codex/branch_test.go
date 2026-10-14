package codex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
)

// branchFiller keeps the fingerprinted prefix of a rollout stable while it is
// appended to, so an incremental parse really resumes instead of rereading.
const branchFiller = `{"timestamp":"2026-01-02T05:00:00.000Z","type":"event_msg","payload":{"type":"agent_message","message":"filler"}}` + "\n"

// branchSessionMeta renders the rollout header, optionally carrying the git
// object Codex records there. Its shape mirrors the real session_meta: only
// `branch` is ever read from it.
func branchSessionMeta(session, git string) string {
	p := `{"id":"` + session + `","session_id":"` + session + `","cwd":"/tmp/repo","model_provider":"openai"`
	if git != "" {
		p += `,"git":` + git
	}
	return `{"timestamp":"2026-01-02T03:00:00.000Z","type":"session_meta","payload":` + p + `}}` + "\n"
}

func branchUsage(session, response, ts string, input, output int) string {
	u := fmt.Sprintf(`{"input_tokens":%d,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":%d,"reasoning_output_tokens":0,"total_tokens":%d}`, input, output, input+output)
	return `{"timestamp":"` + ts + `","type":"token_usage_record","payload":{"thread_id":"` + session + `","session_id":"` + session + `","response_id":"` + response + `","usage":` + u + `,"thread_token_usage":` + u + `}}` + "\n"
}

func writeBranchRollout(t *testing.T, body string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "rollout-2026-01-02T03-00-00-s1.jsonl")
	if err := os.WriteFile(path, []byte(strings.Repeat(branchFiller, 80)+body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func appendBranchRollout(t *testing.T, path, body string) {
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

// TestGitBranchFromSessionMeta checks that the branch a rollout records once,
// in its header, is attached to every request the rollout reports.
func TestGitBranchFromSessionMeta(t *testing.T) {
	dir, path := writeBranchRollout(t,
		branchSessionMeta("s1", `{"branch":"release/1.0","repository_url":"git@github.com:secret/private-repo.git","commit_hash":"deadbeef"}`)+
			branchUsage("s1", "resp1", "2026-01-02T03:01:00.000Z", 100, 10)+
			branchUsage("s1", "resp2", "2026-01-02T03:02:00.000Z", 150, 15))
	b := parseFile(t, NewWithRoots(dir), path, harness.Cursor{})
	if len(b.Events) != 2 {
		t.Fatalf("events = %v, want 2", keys(b))
	}
	for i, e := range b.Events {
		if e.GitBranch != "release/1.0" {
			t.Errorf("event %d branch = %q, want release/1.0", i, e.GitBranch)
		}
	}
}

// TestGitBranchUnknownWhenHeaderHasNoBranch checks that a rollout whose header
// omits the git object reports requests with no branch instead of a guess.
func TestGitBranchUnknownWhenHeaderHasNoBranch(t *testing.T) {
	dir, path := writeBranchRollout(t,
		branchSessionMeta("s1", "")+branchUsage("s1", "resp1", "2026-01-02T03:01:00.000Z", 100, 10))
	b := parseFile(t, NewWithRoots(dir), path, harness.Cursor{})
	if len(b.Events) != 1 || b.Events[0].GitBranch != "" {
		t.Fatalf("events = %+v, want one request with no branch", b.Events)
	}
}

// TestGitBranchSurvivesIncrementalParse checks that the header's branch, read
// in an earlier chunk, still describes requests appended later: the cursor
// carries the context forward instead of losing it at the chunk boundary.
func TestGitBranchSurvivesIncrementalParse(t *testing.T) {
	dir, path := writeBranchRollout(t,
		branchSessionMeta("s1", `{"branch":"feature/y"}`)+
			branchUsage("s1", "resp1", "2026-01-02T03:01:00.000Z", 100, 10))
	p := NewWithRoots(dir)
	first := parseFile(t, p, path, harness.Cursor{})
	if len(first.Events) != 1 || first.Events[0].GitBranch != "feature/y" {
		t.Fatalf("first pass = %+v, want one feature/y request", first.Events)
	}
	if !strings.Contains(first.Next.Extra, "feature/y") {
		t.Fatalf("cursor state dropped the branch: %s", first.Next.Extra)
	}

	appendBranchRollout(t, path, branchUsage("s1", "resp2", "2026-01-02T03:02:00.000Z", 150, 15))
	second := parseFile(t, p, path, first.Next)
	if len(second.Events) != 1 || second.Events[0].GitBranch != "feature/y" {
		t.Fatalf("resumed = %+v, want one feature/y request", second.Events)
	}
}

// TestGitRemoteNeverPersisted is the privacy check: the rollout's git object
// also carries a repository URL and a commit hash, and neither may end up in
// the parsed event or in the cursor state the store persists.
func TestGitRemoteNeverPersisted(t *testing.T) {
	dir, path := writeBranchRollout(t,
		branchSessionMeta("s1", `{"repository_url":"git@github.com:secret/private-repo.git","commit_hash":"deadbeefcafe"}`)+
			branchUsage("s1", "resp1", "2026-01-02T03:01:00.000Z", 100, 10))
	b := parseFile(t, NewWithRoots(dir), path, harness.Cursor{})
	if len(b.Events) != 1 || b.Events[0].GitBranch != "" {
		t.Fatalf("events = %+v, want one request with no branch", b.Events)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"github.com", "private-repo", "deadbeefcafe"} {
		if strings.Contains(string(raw), secret) || strings.Contains(b.Next.Extra, secret) {
			t.Errorf("parsed output leaked %q: %s / %s", secret, raw, b.Next.Extra)
		}
	}
}

// TestDecodePayloadReadsGitBranchInLenientMode checks that the retyped-field
// fallback still finds the branch, so a slightly odd rollout is not silently
// downgraded to "unknown".
func TestDecodePayloadReadsGitBranchInLenientMode(t *testing.T) {
	pl, ok := decodePayload(json.RawMessage(`{"type":123,"id":"d","cwd":"/tmp/d","git":{"branch":"hotfix/z","repository_url":"git@example.com:x.git"}}`))
	if !ok {
		t.Fatal("decodePayload failed")
	}
	if pl.Git == nil || pl.Git.Branch != "hotfix/z" {
		t.Fatalf("git = %+v, want hotfix/z", pl.Git)
	}
	if pl, _ := decodePayload(json.RawMessage(`{"type":"session_meta","git":"none"}`)); pl.Git != nil {
		t.Errorf("a non-object git field must decode as absent, got %+v", pl.Git)
	}
}
