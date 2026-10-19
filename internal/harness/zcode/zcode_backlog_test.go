package zcode

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
)

// ---------------------------------------------------------------------------
// the rowid watermark must never be the only thing that decides "nothing left"
//
// `SELECT ... WHERE rowid > ? ORDER BY rowid LIMIT 20000` is a one-shot window:
// once the cursor's watermark reaches the last row of that window, the next
// scan of the very same file has to know whether the window was the whole
// ledger or only its first page. The unchanged-database fast path answers
// "nothing changed" from the file signature alone, so without an explicit
// backlog state a static ledger larger than the window stops at the window.
// ---------------------------------------------------------------------------

// backlogStateKey is the persisted state field that says "rows beyond the
// watermark are still waiting". The test reads the raw state blob so it also
// compiles — and fails — against a revision that does not persist the marker.
const backlogStateKey = "d"

// cursorState decodes the cursor's persisted state blob.
func cursorState(t *testing.T, cur harness.Cursor) map[string]json.RawMessage {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(cur.Extra), &raw); err != nil {
		t.Fatalf("cursor %+v carries no readable state: %v", cur, err)
	}
	return raw
}

// backlogMarked reports whether the cursor explicitly records that rows beyond
// its watermark have not been read yet. Without it, a parse that hits the row
// window and produces no error is indistinguishable from a drained ledger.
func backlogMarked(t *testing.T, cur harness.Cursor) bool {
	t.Helper()
	raw := cursorState(t, cur)
	b, ok := raw[backlogStateKey]
	if !ok {
		return false
	}
	var drain bool
	if err := json.Unmarshal(b, &drain); err != nil {
		t.Fatalf("cursor backlog flag %s is not a boolean: %v", b, err)
	}
	return drain
}

// insertCompletedRowsWithPrefix appends n completed rows with the given id
// prefix, so a rewritten ledger can be told apart from the one it replaced.
func insertCompletedRowsWithPrefix(t testing.TB, path, prefix string, n int) {
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
		if _, err := stmt.Exec(fmt.Sprintf("%s%05d", prefix, i), "s", 1772359200000+int64(i), "m", "completed", 1, 0); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close statement: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// maxRowidOf reads the table's current rowid high-water mark.
func maxRowidOf(t testing.TB, path string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	var max int64
	if err := db.QueryRow(`SELECT COALESCE(MAX(rowid),0) FROM "model_usage"`).Scan(&max); err != nil {
		t.Fatalf("read MAX(rowid): %v", err)
	}
	return max
}

// A static ledger larger than the row window must drain across scans. The
// database never changes, so the unchanged fast path has to be suppressed until
// the backlog is gone — otherwise every row past the window is lost for good.
func TestStaticLedgerLargerThanRowCapDrainsAcrossScans(t *testing.T) {
	const rows = maxRowsPerParse + 123
	dir := t.TempDir()
	path := writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`)
	insertCompletedRows(t, path, rows)

	ctx := context.Background()
	p := NewWithRoots(dir)
	src := harness.Source{Path: path, Kind: kindSQLite}

	seen := map[string]int{}
	cur := harness.Cursor{}
	scans := 0
	for scans = 1; scans <= 8; scans++ {
		b, err := p.Parse(ctx, src, cur)
		if err != nil {
			t.Fatalf("scan %d of a static %d-row ledger: %v", scans, rows, err)
		}
		for _, e := range b.Events {
			seen[e.DedupKey]++
		}
		if scans == 1 {
			if len(b.Events) != maxRowsPerParse {
				t.Fatalf("first scan read %d rows, want the %d-row window", len(b.Events), maxRowsPerParse)
			}
			if !backlogMarked(t, b.Next) {
				t.Fatalf("after reading %d of %d rows of an unchanged ledger the cursor state is %s with no backlog marker: the next scan takes the unchanged fast path and rows %d..%d are never counted",
					len(b.Events), rows, b.Next.Extra, len(b.Events)+1, rows)
			}
		}
		cur = b.Next
		if len(b.Events) == 0 {
			break
		}
	}
	if scans > 8 {
		t.Fatalf("a static %d-row ledger did not drain within 8 scans", rows)
	}
	if len(seen) != rows {
		var missing []string
		for i := 1; i <= rows && len(missing) < 5; i++ {
			key := fmt.Sprintf("zcode:u%05d", i)
			if seen[key] == 0 {
				missing = append(missing, key)
			}
		}
		t.Fatalf("a static %d-row ledger reported %d distinct rows in %d scans; first missing %v", rows, len(seen), scans, missing)
	}
	for key, n := range seen {
		if n != 1 {
			t.Fatalf("row %s was reported %d times while draining; zcode:<id> dedup must stay stable", key, n)
		}
	}
	if backlogMarked(t, cur) {
		t.Fatalf("cursor state %s still records a backlog after all %d rows were read", cur.Extra, rows)
	}

	// Draining is not a permanent cost: once the ledger is drained the
	// unchanged-database fast path must come back.
	quiet, err := p.Parse(ctx, src, cur)
	if err != nil {
		t.Fatalf("Parse(drained ledger, unchanged): %v", err)
	}
	if len(quiet.Events) != 0 || quiet.Next != cur {
		t.Fatalf("a drained, unchanged ledger re-emitted %d events and moved the cursor %+v -> %+v", len(quiet.Events), cur, quiet.Next)
	}
}

// A cursor persisted by an older revision may already have watermarked at the
// row window of a ledger that has not changed since. The version migration must
// therefore drain the tail instead of trusting the old watermark.
func TestVersionMigrationDrainsAStaticLedgerPastTheRowCap(t *testing.T) {
	const rows = maxRowsPerParse + 77
	dir := t.TempDir()
	path := writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`)
	insertCompletedRows(t, path, rows)

	ctx := context.Background()
	p := NewWithRoots(dir)
	src := harness.Source{Path: path, Kind: kindSQLite}

	first, err := p.Parse(ctx, src, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse(%d completed rows): %v", rows, err)
	}
	if len(first.Events) != maxRowsPerParse {
		t.Fatalf("first scan read %d rows, want the %d-row window", len(first.Events), maxRowsPerParse)
	}

	// The legacy cursor carries this file's current signature (so the fast path
	// would match) and a v2 state that had watermarked at the window already.
	legacy := first.Next
	legacy.Extra = fmt.Sprintf(`{"v":2,"r":%d}`, maxRowsPerParse)

	seen := map[string]int{}
	cur := legacy
	for i := 0; i < 6; i++ {
		b, err := p.Parse(ctx, src, cur)
		if err != nil {
			t.Fatalf("scan %d of the v2 migration: %v", i+1, err)
		}
		for _, e := range b.Events {
			seen[e.DedupKey]++
		}
		cur = b.Next
		if len(b.Events) == 0 {
			break
		}
	}
	if len(seen) != rows {
		var missing []string
		for i := 1; i <= rows && len(missing) < 5; i++ {
			key := fmt.Sprintf("zcode:u%05d", i)
			if seen[key] == 0 {
				missing = append(missing, key)
			}
		}
		t.Fatalf("a v2 cursor migrating off a static %d-row ledger reported %d distinct rows; first missing %v", rows, len(seen), missing)
	}
	for key, n := range seen {
		if n != 1 {
			t.Fatalf("row %s was reported %d times across the v2 migration", key, n)
		}
	}
}

// A row that cannot be scanned is not a row that does not exist. Skipping it
// and advancing the watermark hides it below the new watermark forever, so the
// scan must fail and the round must keep the caller's cursor.
func TestScanFailureIsReportedAndKeepsTheCursor(t *testing.T) {
	dir := t.TempDir()
	path := writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`,
		// started_at is declared INTEGER, so SQLite stores this one as TEXT and
		// scanning it into sql.NullInt64 fails deterministically.
		`INSERT INTO model_usage (id,session_id,started_at,model_id,status,input_tokens,output_tokens) VALUES
		   ('poison','s','not-a-number','m','completed',10,1),
		   ('u2','s',1772359201000,'m','completed',20,2),
		   ('u3','s',1772359202000,'m','completed',30,3)`)

	ctx := context.Background()
	p := NewWithRoots(dir)
	src := harness.Source{Path: path, Kind: kindSQLite}
	cur := harness.Cursor{}
	b, err := p.Parse(ctx, src, cur)
	if err == nil {
		t.Fatalf("a row that failed to scan was skipped and the cursor advanced to %+v (events %v): the row is now below the watermark and no later scan can reach it",
			b.Next, keys(b.Events))
	}
	if len(b.Events) != 0 {
		t.Fatalf("a failed scan still reported %d events, want none", len(b.Events))
	}
	if b.Next != cur {
		t.Fatalf("cursor advanced to %+v despite the failed scan, want the caller's %+v", b.Next, cur)
	}

	// Once the row is readable again the same cursor must count every row.
	execDB(t, path, `UPDATE model_usage SET started_at=1772359200000 WHERE id='poison'`)
	retry, err := p.Parse(ctx, src, b.Next)
	if err != nil {
		t.Fatalf("Parse(after repairing the row): %v", err)
	}
	if got := len(retry.Events); got != 3 {
		t.Fatalf("after repairing the row the retry reported %d events (%v), want all 3", got, keys(retry.Events))
	}
	for _, want := range []string{"zcode:poison", "zcode:u2", "zcode:u3"} {
		if _, ok := eventByKeyIn(retry.Events, want); !ok {
			t.Fatalf("row %s missing from the recovered batch %v", want, keys(retry.Events))
		}
	}
}

// SQLite hands rowids back out after a table is emptied in place, so a ledger
// whose history was cleared and rewritten can have MAX(rowid) exactly equal to
// the watermark while every row under it is new. `rowid > watermark` sees
// nothing, so the watermark alone cannot decide that the ledger was drained.
func TestReplacedRowidsAtTheWatermarkAreNotSkipped(t *testing.T) {
	const rows = 5
	dir := t.TempDir()
	path := writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`)
	insertCompletedRows(t, path, rows)

	ctx := context.Background()
	p := NewWithRoots(dir)
	src := harness.Source{Path: path, Kind: kindSQLite}
	first, err := p.Parse(ctx, src, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse(first ledger): %v", err)
	}
	if len(first.Events) != rows {
		t.Fatalf("first ledger reported %d events, want %d", len(first.Events), rows)
	}

	execDB(t, path, `DELETE FROM model_usage`)
	insertCompletedRowsWithPrefix(t, path, "n", rows)
	if max := maxRowidOf(t, path); max != int64(rows) {
		t.Fatalf("fixture is wrong: MAX(rowid) after the in-place rewrite is %d, want the previous watermark %d", max, rows)
	}

	second, err := p.Parse(ctx, src, first.Next)
	if err != nil {
		t.Fatalf("Parse(replaced ledger): %v", err)
	}
	var missing []string
	for i := 1; i <= rows; i++ {
		key := fmt.Sprintf("zcode:n%05d", i)
		if _, ok := eventByKeyIn(second.Events, key); !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) != 0 {
		t.Fatalf("a ledger replaced in place (MAX(rowid)=%d == watermark) reported %d events and missed %v: `rowid > watermark` cannot see rows that reuse a rowid at or below it",
			rows, len(second.Events), missing)
	}

	third, err := p.Parse(ctx, src, second.Next)
	if err != nil {
		t.Fatalf("Parse(replaced ledger, unchanged): %v", err)
	}
	if len(third.Events) != 0 || third.Next != second.Next {
		t.Fatalf("the recovered ledger re-emitted %d events and moved the cursor %+v -> %+v", len(third.Events), second.Next, third.Next)
	}
}

// The row at the watermark vanishing cannot be told apart from that rowid being
// reused by a new row, so recovery must be conservative: rescan the ledger and
// stay idempotent. Losing the replacement row is not an acceptable answer.
func TestReusedWatermarkRowTriggersAnIdempotentRescan(t *testing.T) {
	const rows = 5
	dir := t.TempDir()
	path := writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`)
	insertCompletedRows(t, path, rows)

	ctx := context.Background()
	p := NewWithRoots(dir)
	src := harness.Source{Path: path, Kind: kindSQLite}
	first, err := p.Parse(ctx, src, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse(first ledger): %v", err)
	}
	if len(first.Events) != rows {
		t.Fatalf("first ledger reported %d events, want %d", len(first.Events), rows)
	}

	execDB(t, path, `DELETE FROM model_usage WHERE id='u00005'`)
	execDB(t, path, `INSERT INTO model_usage
	   (id,session_id,started_at,model_id,status,input_tokens,output_tokens)
	   VALUES ('u00006','s',1772359206000,'m','completed',1,0)`)
	// SQLite assigns MAX(rowid)+1, so deleting the last row makes the next insert
	// reuse its rowid: the new row sits exactly at the watermark.
	if max := maxRowidOf(t, path); max != int64(rows) {
		t.Fatalf("fixture is wrong: MAX(rowid) after the replacement is %d, want the previous watermark %d", max, rows)
	}

	second, err := p.Parse(ctx, src, first.Next)
	if err != nil {
		t.Fatalf("Parse(after the watermark row vanished): %v", err)
	}
	if _, ok := eventByKeyIn(second.Events, "zcode:u00006"); !ok {
		t.Fatalf("the row that reused the watermark rowid was skipped: keys = %v", keys(second.Events))
	}
	counted := map[string]int{}
	for _, e := range second.Events {
		counted[e.DedupKey]++
	}
	for key, n := range counted {
		if n != 1 {
			t.Fatalf("the recovery scan reported %s %d times in one batch; recovery must stay idempotent", key, n)
		}
	}

	third, err := p.Parse(ctx, src, second.Next)
	if err != nil {
		t.Fatalf("Parse(after recovery): %v", err)
	}
	if len(third.Events) != 0 || third.Next != second.Next {
		t.Fatalf("the recovered ledger re-emitted %d events and moved the cursor %+v -> %+v", len(third.Events), second.Next, third.Next)
	}
}

// Control: normal growth above an intact watermark row must stay incremental.
// Verifying the watermark row must not turn every scan into a full rescan.
func TestWatermarkRowStillCountsAppendedRowsIncrementally(t *testing.T) {
	const rows = 5
	dir := t.TempDir()
	path := writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`)
	insertCompletedRows(t, path, rows)

	ctx := context.Background()
	p := NewWithRoots(dir)
	src := harness.Source{Path: path, Kind: kindSQLite}
	first, err := p.Parse(ctx, src, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse(first ledger): %v", err)
	}

	execDB(t, path, `INSERT INTO model_usage
	   (id,session_id,started_at,model_id,status,input_tokens,output_tokens)
	   VALUES ('u00006','s',1772359206000,'m','completed',1,0)`)
	second, err := p.Parse(ctx, src, first.Next)
	if err != nil {
		t.Fatalf("Parse(after the append): %v", err)
	}
	if got, want := keys(second.Events), []string{"zcode:u00006"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("an append above an intact watermark row reported %v, want exactly %v: verifying the watermark must not force a full rescan", got, want)
	}
}
