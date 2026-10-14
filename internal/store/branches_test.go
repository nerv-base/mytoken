package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

func branchWrite(instance, dedup, branch string) Write {
	e := model.UsageEvent{
		Harness:   model.Codex,
		DedupKey:  dedup,
		SessionID: "s-" + instance,
		Model:     "gpt-5",
		Tokens:    model.Tokens{Input: 10, Output: 2},
		GitBranch: branch,
	}
	return Write{
		Harness: model.Codex, InstanceID: instance, Path: "log/" + instance,
		Batch: harness.Batch{Events: []model.UsageEvent{e}},
	}
}

func branchRows(t *testing.T, st *Store) map[string]string {
	t.Helper()
	rows, err := st.DB().Query(`SELECT instance_id||'/'||dedup_key, branch FROM event_branches`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// factFingerprint summarizes the usage facts a replay may rewrite. It pins the
// fact count and the token/cost columns, so a branch test can prove that
// resolving branch ambiguity never touches usage.
func factFingerprint(t *testing.T, st *Store) string {
	t.Helper()
	var n, in, out, priced int64
	var cost float64
	if err := st.DB().QueryRow(`SELECT count(*),coalesce(sum(input),0),coalesce(sum(output),0),coalesce(sum(cost),0),coalesce(sum(priced),0) FROM event_data`).Scan(&n, &in, &out, &cost, &priced); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("n=%d in=%d out=%d cost=%g priced=%d", n, in, out, cost, priced)
}

// TestEventBranchesKeyedByStableIdentity checks that the recorded branch is
// stored against the same identity the request itself uses, so a replay
// updates it in place instead of accumulating rows. Restating the same branch
// is idempotent; restating a different branch is contradictory evidence and
// resolves to unknown rather than to the later value.
func TestEventBranchesKeyedByStableIdentity(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err = st.CommitMany(ctx, []Write{branchWrite("default", "k1", "main"), branchWrite("default", "k2", "")}); err != nil {
		t.Fatal(err)
	}
	if got := branchRows(t, st); len(got) != 1 || got["default/k1"] != "main" {
		t.Fatalf("rows = %v, want only default/k1=main (an absent branch writes nothing)", got)
	}
	// The same branch read twice is a no-op and still leaves exactly one row.
	if err = st.CommitMany(ctx, []Write{branchWrite("default", "k1", "main")}); err != nil {
		t.Fatal(err)
	}
	if got := branchRows(t, st); len(got) != 1 || got["default/k1"] != "main" {
		t.Fatalf("rows after an idempotent replay = %v, want default/k1=main", got)
	}
	// A different branch for the same request is a contradiction, not a
	// correction: the row stays but can no longer name a branch.
	if err = st.CommitMany(ctx, []Write{branchWrite("default", "k1", "release/2")}); err != nil {
		t.Fatal(err)
	}
	if got := branchRows(t, st); len(got) != 1 || got["default/k1"] != branchUnknown {
		t.Fatalf("rows = %v, want default/k1 recorded as unknown", got)
	}
}

// TestEventBranchContradictionStaysUnknown is the replay-order contract: when
// two replays state different non-empty branches for one request, the
// historical evidence is contradictory. Processing order cannot pick a winner,
// so the branch reads back unknown — and a third replay restating the first
// branch must not toggle it back to a guess. The usage fact is untouched.
func TestEventBranchContradictionStaysUnknown(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err = st.CommitMany(ctx, []Write{branchWrite("default", "k1", "feature/a")}); err != nil {
		t.Fatal(err)
	}
	before := factFingerprint(t, st)
	if err = st.CommitMany(ctx, []Write{branchWrite("default", "k1", "feature/b")}); err != nil {
		t.Fatal(err)
	}
	if got := branchRows(t, st); len(got) != 1 || got["default/k1"] != branchUnknown {
		t.Fatalf("rows after A then B = %v, want one row recorded as unknown", got)
	}
	if err = st.CommitMany(ctx, []Write{branchWrite("default", "k1", "feature/a")}); err != nil {
		t.Fatal(err)
	}
	if got := branchRows(t, st); len(got) != 1 || got["default/k1"] != branchUnknown {
		t.Fatalf("rows after A then B then A = %v, want the contradiction to stay sticky unknown", got)
	}
	if after := factFingerprint(t, st); after != before {
		t.Fatalf("usage fact changed: before %s after %s", before, after)
	}
}

// TestEventBranchMissingReplayKeepsEvidence checks that a replay which states
// no branch never erases a branch already recorded: a later omission is not
// evidence against the earlier statement.
func TestEventBranchMissingReplayKeepsEvidence(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err = st.CommitMany(ctx, []Write{branchWrite("default", "k1", "main")}); err != nil {
		t.Fatal(err)
	}
	if err = st.CommitMany(ctx, []Write{branchWrite("default", "k1", "")}); err != nil {
		t.Fatal(err)
	}
	if got := branchRows(t, st); len(got) != 1 || got["default/k1"] != "main" {
		t.Fatalf("rows after a branchless replay = %v, want main kept", got)
	}
}

// TestEventBranchUnusableMetadataIsUnknown checks the ingestion bound: branch
// text that is not a branch (invalid UTF-8, control characters, or an unbounded
// blob) records no evidence instead of being sanitized into a label, and the
// request's usage still lands. Such text is not contradictory evidence either:
// a later replay stating a real branch is still recorded.
func TestEventBranchUnusableMetadataIsUnknown(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	bad := map[string]string{
		"newline": "ma\nin",
		"nul":     "ma\x00in",
		"utf8":    string([]byte{0xff, 0xfe, 0xfd}),
		"long":    strings.Repeat("x", branchLabelMax+1),
		"escape":  "main\x1b[31m",
	}
	writes := make([]Write, 0, len(bad))
	for key, branch := range bad {
		writes = append(writes, branchWrite("default", key, branch))
	}
	if err = st.CommitMany(ctx, writes); err != nil {
		t.Fatalf("a request with unusable branch metadata must still be ingested: %v", err)
	}
	if got := branchRows(t, st); len(got) != 0 {
		t.Fatalf("rows = %v, want no recorded branch evidence", got)
	}
	var facts int
	if err = st.DB().QueryRow(`SELECT count(*) FROM event_data`).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if facts != len(bad) {
		t.Fatalf("event_data rows = %d, want %d (usage is never dropped)", facts, len(bad))
	}
	if err = st.CommitMany(ctx, []Write{branchWrite("default", "newline", "main")}); err != nil {
		t.Fatal(err)
	}
	if got := branchRows(t, st); len(got) != 1 || got["default/newline"] != "main" {
		t.Fatalf("rows = %v, want the later usable branch recorded", got)
	}
	// The bound admits a full 256-byte ref and rejects one byte more.
	atBound := strings.Repeat("y", branchLabelMax)
	if err = st.CommitMany(ctx, []Write{branchWrite("default", "at-bound", atBound)}); err != nil {
		t.Fatal(err)
	}
	if got := branchRows(t, st); got["default/at-bound"] != atBound {
		t.Fatalf("a %d-byte branch must be accepted, rows = %v", branchLabelMax, got)
	}
}

// TestEventBranchesSeparateInstancesSameDedup checks that the same dedup key in
// two data sources keeps two independent branches: the identity includes the
// instance, so one source's branch never describes another's request.
func TestEventBranchesSeparateInstancesSameDedup(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err = st.CommitMany(ctx, []Write{branchWrite("work", "shared", "main"), branchWrite("home", "shared", "feature/x")}); err != nil {
		t.Fatal(err)
	}
	got := branchRows(t, st)
	if len(got) != 2 || got["work/shared"] != "main" || got["home/shared"] != "feature/x" {
		t.Fatalf("rows = %v, want one branch per instance", got)
	}
}

// TestEventBranchCommitIsAtomic proves the metadata rides in the fact's
// transaction: when the branch write cannot land, the request it describes must
// not land either.
func TestEventBranchCommitIsAtomic(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	// Simulate an index whose branch table is missing, so the branch upsert
	// fails part-way through the transaction.
	if _, err = st.DB().Exec(`DROP TABLE event_branches`); err != nil {
		t.Fatal(err)
	}
	if err = st.CommitMany(ctx, []Write{branchWrite("default", "k1", "main")}); err == nil {
		t.Fatal("commit succeeded without the branch table; the write is not atomic")
	}
	var n int
	if err = st.DB().QueryRow(`SELECT count(*) FROM event_data`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("event_data rows = %d, want 0 after a rolled-back commit", n)
	}
}

// TestEventBranchesPruneDropsOrphans checks the maintenance step: metadata for
// a request that no longer exists is removed, while live metadata is kept.
func TestEventBranchesPruneDropsOrphans(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err = st.CommitMany(ctx, []Write{branchWrite("default", "live", "main")}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`INSERT INTO event_branches(harness,instance_id,dedup_key,branch) VALUES('codex','default','ghost','gone')`); err != nil {
		t.Fatal(err)
	}
	n, err := st.PruneEventBranches(ctx)
	if err != nil || n != 1 {
		t.Fatalf("pruned %d rows err=%v, want 1", n, err)
	}
	if got := branchRows(t, st); len(got) != 1 || got["default/live"] != "main" {
		t.Fatalf("rows = %v, want only the live request", got)
	}
}

// TestEventBranchesSurviveRebuild checks that a full replay (which resets every
// cursor) does not drop the recorded branch: the metadata is keyed by the fact,
// not by the cursor.
func TestEventBranchesSurviveRebuild(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err = st.CommitMany(ctx, []Write{branchWrite("default", "k1", "main")}); err != nil {
		t.Fatal(err)
	}
	if err = st.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	if got := branchRows(t, st); len(got) != 1 || got["default/k1"] != "main" {
		t.Fatalf("rows after rebuild = %v, want the recorded branch kept", got)
	}
}

// TestEventBranchesMissingOnOldIndex checks the compatibility contract: an
// index written before the table existed stays readable, and the probe that
// readers use reports its absence instead of failing.
func TestEventBranchesMissingOnOldIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = st.CommitMany(ctx, []Write{branchWrite("default", "k1", "main")}); err != nil {
		t.Fatal(err)
	}
	// An older index simply does not have the table.
	if _, err = st.DB().Exec(`DROP TABLE event_branches`); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("a read-only handle must still open an index without the branch table: %v", err)
	}
	defer ro.Close()
	tx, err := ro.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	present, err := EventBranchesPresent(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatal("EventBranchesPresent reported the dropped table as present")
	}
}
