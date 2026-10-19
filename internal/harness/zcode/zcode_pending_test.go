package zcode

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
)

// ---------------------------------------------------------------------------
// the in-flight (pending) set below the watermark
//
// A row below the rowid watermark is only ever revisited because its rowid is
// in the cursor's pending set. Losing an id from that set is therefore
// permanent: the rowid is no longer "> watermark" and no longer "IN (pending)",
// so the row is never read again, and the usage it eventually records is never
// counted — not late, never.
// ---------------------------------------------------------------------------

// pendingEvictionRows is deliberately larger than the 1024-row bound the old
// cursor state imposed on the pending set, so at least one in-flight row is
// evicted by that bound.
const pendingEvictionRows = 1500

// pendingChunkRows is larger still: it exceeds both that bound and the historic
// SQLITE_MAX_VARIABLE_NUMBER of 999, so a scan has to revisit the pending set in
// more than one bounded statement instead of one giant IN-list.
const pendingChunkRows = 2500

// insertRunningRows appends n in-flight rows whose ids sort in insertion order
// ("p00001" ...), so rowid == insertion order and the oldest rows are exactly
// the ones an eviction bound drops first.
func insertRunningRows(t testing.TB, path string, n int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO model_usage
	   (id,session_id,started_at,model_id,status,input_tokens,output_tokens)
	   VALUES (?,?,?,?,?,?,?)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for i := 1; i <= n; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("p%05d", i), "s", 1772359200000+int64(i), "m", "running", 10, 1); err != nil {
			t.Fatalf("insert running row %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close statement: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// completeRow flips one in-flight row to the only status whose usage counts.
func completeRow(t testing.TB, path, id string) {
	t.Helper()
	execDB(t, path, fmt.Sprintf(`UPDATE model_usage SET status='completed' WHERE id='%s'`, id))
}

// pendingOf decodes the pending rowids the cursor carries, failing the test when
// the cursor is not a state this revision can read.
func pendingOf(t *testing.T, cur harness.Cursor) []int64 {
	t.Helper()
	state := decodeState(cur)
	if state.Version != cursorVersion {
		t.Fatalf("cursor %+v is not a v%d state: decoded %+v", cur, cursorVersion, state)
	}
	return state.Pending
}

// A turn that completes after its rowid has fallen off the pending set used to
// be counted never. The scan must keep every in-flight rowid it observed, or it
// must be able to rediscover the row some other way; capping the set does
// neither.
func TestPendingRowsBeyondTheEvictionBoundAreStillCounted(t *testing.T) {
	dir := t.TempDir()
	path := writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`)
	insertRunningRows(t, path, pendingEvictionRows)

	ctx := context.Background()
	p := NewWithRoots(dir)
	src := harness.Source{Path: path, Kind: kindSQLite}
	first, err := p.Parse(ctx, src, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse(%d in-flight rows): %v", pendingEvictionRows, err)
	}
	if len(first.Events) != 0 {
		t.Fatalf("in-flight rows produced events: %v", keys(first.Events))
	}
	if got := len(pendingOf(t, first.Next)); got != pendingEvictionRows {
		t.Fatalf("cursor tracks %d of %d in-flight rowids; an untracked rowid below the watermark can never be revisited",
			got, pendingEvictionRows)
	}

	// The oldest row (rowid 1) is exactly the one an eviction bound drops first;
	// the middle and the newest rows are the controls.
	for _, id := range []string{"p00001", "p00700", "p01500"} {
		completeRow(t, path, id)
	}
	second, err := p.Parse(ctx, src, first.Next)
	if err != nil {
		t.Fatalf("Parse(after completions): %v", err)
	}
	want := []string{"zcode:p00001", "zcode:p00700", "zcode:p01500"}
	if got := keys(second.Events); !reflect.DeepEqual(got, want) {
		t.Fatalf("DedupKeys after %d in-flight rows completed = %v, want %v: a rowid evicted from the pending set stays uncounted forever",
			pendingEvictionRows, got, want)
	}

	// The same row must not be counted twice, and the now-quiet ledger must not
	// move its cursor.
	third, err := p.Parse(ctx, src, second.Next)
	if err != nil {
		t.Fatalf("Parse(unchanged): %v", err)
	}
	if len(third.Events) != 0 {
		t.Fatalf("already counted rows were emitted again: %v", keys(third.Events))
	}
	if third.Next != second.Next {
		t.Fatalf("cursor moved on an unchanged ledger:\n got %+v\nwant %+v", third.Next, second.Next)
	}
}

// A pending set larger than SQLite's historic statement-parameter limit must
// still be revisited in full: every in-flight rowid is examined in bounded
// batches, so neither the oldest nor the newest end of the set is starved.
func TestPendingSetAboveTheParameterLimitIsFullyRevisited(t *testing.T) {
	dir := t.TempDir()
	path := writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`)
	insertRunningRows(t, path, pendingChunkRows)

	ctx := context.Background()
	p := NewWithRoots(dir)
	src := harness.Source{Path: path, Kind: kindSQLite}
	first, err := p.Parse(ctx, src, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse(%d in-flight rows): %v", pendingChunkRows, err)
	}
	if got := len(pendingOf(t, first.Next)); got != pendingChunkRows {
		t.Fatalf("cursor tracks %d of %d in-flight rowids (the re-read must cover every one)", got, pendingChunkRows)
	}

	for _, id := range []string{"p00001", "p01250", "p02500"} {
		completeRow(t, path, id)
	}
	second, err := p.Parse(ctx, src, first.Next)
	if err != nil {
		t.Fatalf("Parse(after completions): %v", err)
	}
	want := []string{"zcode:p00001", "zcode:p01250", "zcode:p02500"}
	if got := keys(second.Events); !reflect.DeepEqual(got, want) {
		t.Fatalf("DedupKeys = %v, want %v (a %d-row pending set must be revisited end to end)",
			got, want, pendingChunkRows)
	}
}

// A big in-flight set must not hold the watermark back: new rows are read on
// their own, so they keep arriving while the old turns stay in flight, and the
// whole thing stays idempotent from a zero cursor.
func TestLargePendingSetDoesNotStarveNewRows(t *testing.T) {
	dir := t.TempDir()
	path := writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`)
	insertRunningRows(t, path, pendingEvictionRows)

	ctx := context.Background()
	p := NewWithRoots(dir)
	src := harness.Source{Path: path, Kind: kindSQLite}
	first, err := p.Parse(ctx, src, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// One old turn completes while a brand-new completed turn arrives: both must
	// be reported by the same scan, ahead of the still-running rows.
	completeRow(t, path, "p00001")
	execDB(t, path, `INSERT INTO model_usage
	   (id,session_id,started_at,model_id,status,input_tokens,output_tokens)
	   VALUES ('n1','s',1772359400000,'m','completed',7,3)`)

	second, err := p.Parse(ctx, src, first.Next)
	if err != nil {
		t.Fatalf("Parse(incremental): %v", err)
	}
	want := []string{"zcode:p00001", "zcode:n1"}
	if got := keys(second.Events); !reflect.DeepEqual(got, want) {
		t.Fatalf("DedupKeys = %v, want %v (the new row must not wait behind the in-flight set)", got, want)
	}
	if got := eventByKey(t, second.Events, "zcode:n1"); got.Tokens.Input != 7 || got.Tokens.Output != 3 {
		t.Errorf("n1 Tokens = %+v, want input 7 output 3", got.Tokens)
	}

	// Re-parsing from a zero cursor must agree with the incremental result: the
	// stable DedupKey keeps the revisit idempotent.
	fromZero := parseRoot(t, dir, nil)
	if got := keys(fromZero.Events); !reflect.DeepEqual(got, keys(second.Events)) {
		t.Fatalf("re-parse from zero = %v, want %v", got, keys(second.Events))
	}
}

// A pending row that reaches a terminal status can never become usage, so it
// must leave the set; otherwise a long-lived ledger accumulates rowids that are
// revisited on every scan forever.
func TestPendingDropsRowsThatBecameTerminal(t *testing.T) {
	dir := t.TempDir()
	path := writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`)
	insertRunningRows(t, path, 3)

	ctx := context.Background()
	p := NewWithRoots(dir)
	src := harness.Source{Path: path, Kind: kindSQLite}
	first, err := p.Parse(ctx, src, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := pendingOf(t, first.Next), []int64{1, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pending = %v, want %v", got, want)
	}

	execDB(t, path, `UPDATE model_usage SET status='failed' WHERE id='p00002'`)
	second, err := p.Parse(ctx, src, first.Next)
	if err != nil {
		t.Fatalf("Parse(after failure): %v", err)
	}
	if len(second.Events) != 0 {
		t.Fatalf("a failed row produced events: %v", keys(second.Events))
	}
	if got, want := pendingOf(t, second.Next), []int64{1, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pending = %v, want %v (a terminal row must not be revisited)", got, want)
	}
}

// A failed scan must leave the whole cursor — pending set included — exactly as
// it was, and the preserved set must still catch the completion that follows.
func TestPendingSurvivesAFailedScanAndIsRetried(t *testing.T) {
	dir := t.TempDir()
	path := writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`)
	insertRunningRows(t, path, pendingEvictionRows)

	ctx := context.Background()
	p := NewWithRoots(dir)
	src := harness.Source{Path: path, Kind: kindSQLite}
	first, err := p.Parse(ctx, src, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	healthy, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}

	// Break the schema probe: PRAGMA table_info over a view whose body cannot be
	// resolved fails at runtime, which is not "this database has no columns".
	execDB(t, path,
		`DROP TABLE model_usage`,
		`CREATE VIEW model_usage AS SELECT * FROM a_table_that_does_not_exist`)
	broken, err := p.Parse(ctx, src, first.Next)
	if err == nil {
		t.Fatalf("a failed scan was reported as success (events %v, cursor %+v)", keys(broken.Events), broken.Next)
	}
	if len(broken.Events) != 0 {
		t.Fatalf("failed scan produced %d events, want none", len(broken.Events))
	}
	if broken.Next != first.Next {
		t.Fatalf("failed scan moved the cursor:\n got %+v\nwant %+v (the pending set is the only record of every in-flight row)", broken.Next, first.Next)
	}

	// Restore the ledger and complete the row an eviction bound would have
	// dropped: the preserved cursor must still count it.
	removeSidecars(t, path)
	if err := os.WriteFile(path, healthy, 0o600); err != nil {
		t.Fatalf("restore the ledger: %v", err)
	}
	completeRow(t, path, "p00001")
	retry, err := p.Parse(ctx, src, first.Next)
	if err != nil {
		t.Fatalf("Parse(retry): %v", err)
	}
	if _, ok := eventByKeyIn(retry.Events, "zcode:p00001"); !ok {
		t.Fatalf("after a failed scan the retry lost the evicted in-flight row: keys = %v", keys(retry.Events))
	}
}

// BenchmarkPendingRevisit measures what keeping every in-flight rowid costs. A
// changed ledger re-reads the whole tracked set on every scan, in bounded
// chunks, and the cursor must carry those ids because nothing else records a row
// at or below the watermark. The cursor-bytes metric is that state's size on the
// wire.
func BenchmarkPendingRevisit(b *testing.B) {
	for _, inFlight := range []int{0, pendingBatch, pendingChunkRows} {
		b.Run(fmt.Sprintf("inflight=%d", inFlight), func(b *testing.B) {
			dir := b.TempDir()
			path := writeDB(b, dir, dbName, baseSchema,
				`INSERT INTO session (id,directory) VALUES ('s','/w')`)
			insertRunningRows(b, path, inFlight)

			ctx := context.Background()
			p := NewWithRoots(dir)
			src := harness.Source{Path: path, Kind: kindSQLite}
			first, err := p.Parse(ctx, src, harness.Cursor{})
			if err != nil {
				b.Fatalf("Parse: %v", err)
			}
			// Write once so the unchanged-signature fast path cannot hide the
			// cost: parsing with the now-stale cursor re-reads the tracked set on
			// every iteration, which is the steady state under a live ledger.
			execDB(b, path, `INSERT INTO model_usage
			   (id,session_id,started_at,model_id,status,input_tokens,output_tokens)
			   VALUES ('n1','s',1772359400000,'m','completed',7,3)`)
			b.ResetTimer()
			// ReportMetric after ResetTimer: ResetTimer deletes reported metrics.
			b.ReportMetric(float64(len(first.Next.Extra)), "cursor-bytes")
			for i := 0; i < b.N; i++ {
				if _, err := p.Parse(ctx, src, first.Next); err != nil {
					b.Fatalf("Parse: %v", err)
				}
			}
		})
	}
}
