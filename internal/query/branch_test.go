package query

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

func branchEvent(dedup, project, branch, modelName string, at time.Time, tokens model.Tokens) model.UsageEvent {
	return model.UsageEvent{
		Harness: model.Codex, DedupKey: dedup, SessionID: "s-" + dedup,
		ProjectPath: project, GitBranch: branch, Model: modelName,
		Timestamp: at, Tokens: tokens,
	}
}

func branchBuckets(t *testing.T, st *store.Store, f Filter) (Envelope, []Bucket) {
	t.Helper()
	env, err := ReadEnvelope(context.Background(), st, f, EnvelopeOptions{Groups: []GroupDim{GroupByBranch}})
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Groups) != 1 || env.Groups[0].Dim != GroupByBranch {
		t.Fatalf("groups = %+v, want one branch group", env.Groups)
	}
	return env, env.Groups[0].Buckets
}

func bucketByKey(t *testing.T, buckets []Bucket, project, branch string) Bucket {
	t.Helper()
	key := BranchKey(project, branch)
	for _, b := range buckets {
		if b.Key == key {
			return b
		}
	}
	t.Fatalf("no bucket for %q; have %+v", key, buckets)
	return Bucket{}
}

// TestBranchGroupsRepositoryAndBranch checks the grouping identity: the same
// branch name in two repositories is two answers, an unrecorded branch is its
// own explicit bucket, and the group totals add up to the period totals.
func TestBranchGroupsRepositoryAndBranch(t *testing.T) {
	root := t.TempDir()
	alpha := filepath.Join(root, "alpha")
	beta := filepath.Join(root, "beta")
	for _, p := range []string{alpha, beta} {
		if err := os.MkdirAll(filepath.Join(p, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(filepath.Join(root, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.Local)
	events := []model.UsageEvent{
		branchEvent("a1", alpha, "main", "gpt-5", day.Add(time.Hour), model.Tokens{Input: 100}),
		branchEvent("b1", beta, "main", "gpt-5", day.Add(2*time.Hour), model.Tokens{Input: 10}),
		branchEvent("a2", alpha, "feature/x", "gpt-5", day.Add(3*time.Hour), model.Tokens{Input: 5}),
		branchEvent("a3", alpha, "", "gpt-5", day.Add(4*time.Hour), model.Tokens{Input: 1}),
	}
	if err = st.Commit(context.Background(), model.Codex, "log", harness.Batch{Events: events}, nil); err != nil {
		t.Fatal(err)
	}

	f := Filter{Range: Range{From: day, To: day.AddDate(0, 0, 1)}}
	env, buckets := branchBuckets(t, st, f)
	if len(buckets) != 4 {
		t.Fatalf("buckets = %+v, want 4 (two repositories, three branches)", buckets)
	}
	// The same branch name is not merged across repositories.
	if got := bucketByKey(t, buckets, alpha, "main"); got.Tokens.Input != 100 || got.Requests != 1 {
		t.Errorf("alpha/main = %+v, want its own 100-token request", got)
	}
	if got := bucketByKey(t, buckets, beta, "main"); got.Tokens.Input != 10 || got.Requests != 1 {
		t.Errorf("beta/main = %+v, want its own 10-token request", got)
	}
	// No branch recorded is a result, not a dropped request.
	unknown := bucketByKey(t, buckets, alpha, "")
	if unknown.Requests != 1 || unknown.Tokens.Input != 1 {
		t.Errorf("unknown bucket = %+v, want the 1-token request kept", unknown)
	}
	// A branch alone is not a group: the key must carry the repository too.
	if bytes := []byte(buckets[0].Key); len(bytes) == 0 {
		t.Fatal("empty bucket key")
	}
	if project, branch := SplitBranchKey(unknown.Key); project != alpha || branch != "" {
		t.Errorf("SplitBranchKey(%q) = %q,%q", unknown.Key, project, branch)
	}

	// Grouping must not change what the period measured: the buckets are a
	// partition, so their sums are the totals.
	var requests int64
	var tokens model.Tokens
	var cost float64
	for _, b := range buckets {
		requests += b.Requests
		tokens = tokens.Add(b.Tokens)
		cost += b.CostUSD
	}
	if requests != env.Totals.Requests || tokens != env.Totals.Tokens || cost != env.Totals.CostUSD {
		t.Fatalf("groups sum to %d/%+v/%v, totals are %d/%+v/%v", requests, tokens, cost, env.Totals.Requests, env.Totals.Tokens, env.Totals.CostUSD)
	}

	// The same filter without groups reports the identical totals, so adding a
	// breakdown cannot disturb cost or cache semantics.
	plain, err := ReadEnvelope(context.Background(), st, f)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Totals != env.Totals {
		t.Fatalf("grouped envelope changed totals: %+v vs %+v", env.Totals, plain.Totals)
	}
}

// TestBranchFoldsWorktreesAndKeepsProvenance checks the repository half of the
// key: two checkouts of one repository are one repository bucket, because the
// store already resolved the linked worktree to its main repository at write
// time, and their branches still stay apart.
func TestBranchFoldsWorktreesAndKeepsProvenance(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "repos", "demo")
	work := filepath.Join(root, "worktrees", "id", "demo")
	for _, p := range []string{filepath.Join(main, ".git", "worktrees", "id"), work} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(work, ".git"), []byte("gitdir: "+filepath.Join(main, ".git", "worktrees", "id")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.Local)
	events := []model.UsageEvent{
		branchEvent("m1", main, "main", "gpt-5", day.Add(time.Hour), model.Tokens{Input: 30}),
		branchEvent("w1", work, "feature/wt", "gpt-5", day.Add(2*time.Hour), model.Tokens{Input: 7}),
	}
	if err = st.Commit(context.Background(), model.Codex, "log", harness.Batch{Events: events}, nil); err != nil {
		t.Fatal(err)
	}
	_, buckets := branchBuckets(t, st, Filter{Range: Range{From: day, To: day.AddDate(0, 0, 1)}})
	if len(buckets) != 2 {
		t.Fatalf("buckets = %+v, want the two branches of one repository", buckets)
	}
	for _, b := range buckets {
		project, _ := SplitBranchKey(b.Key)
		if project != main {
			t.Errorf("bucket %q is not folded into %q", b.Key, main)
		}
	}
	if got := bucketByKey(t, buckets, main, "feature/wt"); got.Tokens.Input != 7 {
		t.Errorf("worktree branch = %+v, want 7 tokens under the main repository", got)
	}
	// The raw path stays as provenance in the store, so folding never loses
	// where the usage actually came from.
	var raw string
	if err = st.DB().QueryRow(`SELECT raw_project FROM events WHERE dedup_key='w1'`).Scan(&raw); err != nil || raw != work {
		t.Fatalf("raw_project = %q err=%v, want %q", raw, err, work)
	}
}

// TestBranchGroupHonoursFilters checks that the branch ranking is computed with
// the same intersected filters as the totals instead of over the whole index.
func TestBranchGroupHonoursFilters(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.Local)
	later := day.AddDate(0, 0, 5)
	events := []model.UsageEvent{
		branchEvent("a", repo, "main", "gpt-5", day.Add(time.Hour), model.Tokens{Input: 10}),
		branchEvent("b", repo, "main", "o3", day.Add(2*time.Hour), model.Tokens{Input: 20}),
		branchEvent("c", repo, "main", "gpt-5", later, model.Tokens{Input: 40}),
		branchEvent("d", repo, "feature", "gpt-5", day.Add(3*time.Hour), model.Tokens{Input: 5}),
	}
	if err = st.Commit(context.Background(), model.Codex, "log", harness.Batch{Events: events}, nil); err != nil {
		t.Fatal(err)
	}
	// Model and time both narrow the ranking; the untouched branch disappears
	// rather than being reported with the unfiltered total.
	env, buckets := branchBuckets(t, st, Filter{
		Range:  Range{From: day, To: day.AddDate(0, 0, 1)},
		Models: []string{"gpt-5"},
	})
	if len(buckets) != 2 {
		t.Fatalf("buckets = %+v, want main and feature within the day", buckets)
	}
	if got := bucketByKey(t, buckets, repo, "main"); got.Tokens.Input != 10 || got.Requests != 1 {
		t.Errorf("filtered main = %+v, want only the gpt-5 request inside the day", got)
	}
	if env.Totals.Requests != 2 {
		t.Errorf("filtered totals = %+v, want 2 requests", env.Totals)
	}
}

// TestBranchOnIndexWithoutBranchTable covers an index written before branch
// metadata existed: a read-only report must still work and answer "unknown" for
// every request instead of failing or silently dropping the breakdown.
func TestBranchOnIndexWithoutBranchTable(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "index.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.Local)
	events := []model.UsageEvent{
		branchEvent("a", repo, "main", "gpt-5", day.Add(time.Hour), model.Tokens{Input: 10}),
		branchEvent("b", repo, "feature", "gpt-5", day.Add(2*time.Hour), model.Tokens{Input: 20}),
	}
	if err = st.Commit(context.Background(), model.Codex, "log", harness.Batch{Events: events}, nil); err != nil {
		t.Fatal(err)
	}
	// An older index has no event_branches table at all.
	if _, err = st.DB().Exec(`DROP TABLE event_branches`); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := store.OpenReadOnly(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	env, buckets := branchBuckets(t, ro, Filter{Range: Range{From: day, To: day.AddDate(0, 0, 1)}})
	if len(buckets) != 1 {
		t.Fatalf("buckets = %+v, want a single unknown bucket", buckets)
	}
	project, branch := SplitBranchKey(buckets[0].Key)
	if project != repo || branch != "" {
		t.Errorf("bucket key = %q,%q, want the repository with an unknown branch", project, branch)
	}
	if buckets[0].Requests != 2 || env.Totals.Requests != 2 {
		t.Fatalf("unknown bucket requests = %d, totals = %d, want 2 kept", buckets[0].Requests, env.Totals.Requests)
	}
}
