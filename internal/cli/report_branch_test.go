package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

type reportBranchJSON struct {
	Groups []struct {
		Dim     string `json:"dim"`
		Note    string `json:"note"`
		Total   int    `json:"total"`
		Current []struct {
			Key      string `json:"key"`
			Project  string `json:"project"`
			Branch   string `json:"branch"`
			Requests int64  `json:"requests"`
		} `json:"current"`
	} `json:"groups"`
	Current struct {
		Totals struct {
			Requests int64 `json:"requests"`
		} `json:"totals"`
	} `json:"current"`
}

// reportBranchWrite is snapWrite with an explicit recorded branch and project,
// so the grouping can be exercised without any real log.
func reportBranchWrite(project, instanceID, dedup, session, branch string) store.Write {
	at := time.Now().Add(-2 * time.Minute)
	e := model.UsageEvent{
		Harness: model.Codex, DedupKey: dedup, SessionID: session, Model: "gpt-5",
		Provider: "p", BaseURL: "https://api.example.com", ProjectPath: project,
		GitBranch: branch, Timestamp: at, Tokens: model.Tokens{Input: 20, Output: 4},
	}
	return store.Write{
		Harness: model.Codex, InstanceID: instanceID, Path: "fixture/" + instanceID,
		Batch: harness.Batch{
			Events:   []model.UsageEvent{e},
			Sessions: []model.SessionMeta{{Harness: model.Codex, SessionID: session, Title: session, Project: project, StartedAt: at, UpdatedAt: at}},
			Next:     harness.Cursor{Offset: 1},
		},
	}
}

func TestReportByBranchGroupsRepositoryAndBranch(t *testing.T) {
	root := t.TempDir()
	alpha := filepath.Join(root, "alpha")
	beta := filepath.Join(root, "beta")
	for _, p := range []string{alpha, beta} {
		if err := os.MkdirAll(filepath.Join(p, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	seeded := snapSeed(t, nil,
		reportBranchWrite(alpha, "a", "k1", "s1", "main"),
		reportBranchWrite(alpha, "a", "k2", "s2", "feature/x"),
		reportBranchWrite(alpha, "a", "k3", "s3", ""),
		reportBranchWrite(beta, "b", "k4", "s4", "main"),
	)
	before := snapFingerprint(t, seeded)

	out, errout, code := runCLI(t, "report", "day", "--by", "branch")
	if code != 0 {
		t.Fatalf("report day --by branch: code=%d stderr=%s", code, errout)
	}
	for _, want := range []string{"REPOSITORY", "BRANCH", "main", "feature/x", reportUnknownBranch} {
		if !strings.Contains(out, want) {
			t.Errorf("branch table lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, alpha) || !strings.Contains(out, beta) {
		t.Errorf("branch table lost the repositories:\n%s", out)
	}

	out, errout, code = runCLI(t, "report", "day", "--by", "branch", "--json")
	if code != 0 {
		t.Fatalf("report day --by branch --json: code=%d stderr=%s", code, errout)
	}
	var rep reportBranchJSON
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("invalid JSON %q: %v", out, err)
	}
	if len(rep.Groups) != 1 || rep.Groups[0].Dim != "branch" {
		t.Fatalf("groups = %+v, want one branch group", rep.Groups)
	}
	g := rep.Groups[0]
	if !strings.Contains(g.Note, "never the working tree's current HEAD") {
		t.Errorf("branch note does not rule out HEAD inference: %q", g.Note)
	}
	if len(g.Current) != 4 {
		t.Fatalf("rows = %+v, want 4 (a repository per branch name plus an unknown row)", g.Current)
	}
	seen := map[string]bool{}
	for _, r := range g.Current {
		seen[r.Project+"/"+r.Branch] = true
		if r.Key != r.Project+"@"+r.Branch {
			t.Errorf("row key = %q, want %q", r.Key, r.Project+"@"+r.Branch)
		}
	}
	// One branch name in two repositories is two rows, and the unrecorded
	// branch is its own row rather than dropped usage.
	for _, want := range []string{alpha + "/main", beta + "/main", alpha + "/feature/x", alpha + "/" + reportUnknownBranch} {
		if !seen[want] {
			t.Errorf("missing row %q in %+v", want, g.Current)
		}
	}
	var requests int64
	for _, r := range g.Current {
		requests += r.Requests
	}
	if requests != rep.Current.Totals.Requests || requests != 4 {
		t.Fatalf("grouped requests = %d, period totals = %d, want 4", requests, rep.Current.Totals.Requests)
	}

	// The read-only report must not rewrite or rescan the index.
	if got := snapFingerprint(t, seeded); got != before {
		t.Fatalf("report --by branch rewrote the index:\nbefore %s\nafter  %s", before, got)
	}

	// The other dimensions still work together, and the project note now
	// spells out the canonical repository folding.
	if out, errout, code = runCLI(t, "report", "day", "--by", "session", "--by", "project", "--by", "branch", "--json"); code != 0 {
		t.Fatalf("report --by session,project,branch: code=%d stderr=%s", code, errout)
	}
	var multi struct {
		Groups []struct {
			Dim string `json:"dim"`
		} `json:"groups"`
	}
	if err := json.Unmarshal([]byte(out), &multi); err != nil {
		t.Fatalf("invalid JSON %q: %v", out, err)
	}
	if len(multi.Groups) != 3 {
		t.Fatalf("groups = %+v, want session, project and branch", multi.Groups)
	}
	out, _, code = runCLI(t, "report", "day", "--by", "project")
	if code != 0 || !strings.Contains(out, "canonical repository") {
		t.Errorf("--by project no longer documents repository folding (code=%d):\n%s", code, out)
	}
	if _, errout, code = runCLI(t, "report", "day", "--by", "bogus"); code != 2 || !strings.Contains(errout, "want session, project or branch") {
		t.Fatalf("--by bogus: code=%d stderr=%s, want a usage error", code, errout)
	}
	if _, errout, code = runCLI(t, "report", "day", "--by", "branch", "--format", "csv"); code != 0 {
		t.Fatalf("report --by branch --format csv: code=%d stderr=%s", code, errout)
	}
}
