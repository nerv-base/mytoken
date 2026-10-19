package zcode

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

// withoutRowidSchema is the documented ledger shape declared WITHOUT ROWID. It
// has no rowid watermark, so a scan cannot page through it by rowid and must
// count every completed row it has not reported yet.
const withoutRowidSchema = `
CREATE TABLE model_usage (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  started_at INTEGER NOT NULL,
  model_id TEXT NOT NULL,
  status TEXT NOT NULL,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE TABLE session (
  id TEXT PRIMARY KEY,
  directory TEXT NOT NULL
);`

// insertCompletedRows appends n completed rows whose ids sort in insertion
// order ("u00001" ...), so a capped read would keep returning the same prefix.
func insertCompletedRows(t *testing.T, path string, n int) {
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
		if _, err := stmt.Exec(fmt.Sprintf("u%05d", i), "s", 1772359200000+int64(i), "m", "completed", 1, 0); err != nil {
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

// eventByKeyIn reports the event with the given DedupKey and whether it exists.
func eventByKeyIn(events []model.UsageEvent, key string) (model.UsageEvent, bool) {
	for _, e := range events {
		if e.DedupKey == key {
			return e, true
		}
	}
	return model.UsageEvent{}, false
}

// A WITHOUT ROWID ledger larger than maxRowsPerParse must still be counted in
// full: a "status = ? ORDER BY id LIMIT 20000" query always returns the same
// 20000 rows, so every row past the cap would stay invisible forever.
func TestWithoutRowidCountsRowsPastTheRowCap(t *testing.T) {
	const rows = maxRowsPerParse + 25
	dir := t.TempDir()
	path := writeDB(t, dir, dbName, withoutRowidSchema,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`)
	insertCompletedRows(t, path, rows)

	ctx := context.Background()
	p := NewWithRoots(dir)
	src := harness.Source{Path: path, Kind: kindSQLite}
	first, err := p.Parse(ctx, src, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse(%d completed rows): %v", rows, err)
	}
	if len(first.Events) != rows {
		last := fmt.Sprintf("zcode:u%05d", rows)
		_, present := eventByKeyIn(first.Events, last)
		t.Fatalf("WITHOUT ROWID parse read %d of %d completed rows (last row %s present: %t): every row past maxRowsPerParse=%d is silently undercounted",
			len(first.Events), rows, last, present, maxRowsPerParse)
	}
	seen := map[string]int{}
	for _, e := range first.Events {
		seen[e.DedupKey]++
	}
	for _, want := range []string{"zcode:u00001", fmt.Sprintf("zcode:u%05d", rows)} {
		if seen[want] == 0 {
			t.Fatalf("completed row %s missing from the batch of %d rows", want, len(first.Events))
		}
	}
	if len(seen) != rows {
		t.Fatalf("batch reported %d distinct DedupKeys, want %d (duplicates break idempotence)", len(seen), rows)
	}

	// An unchanged ledger must stay quiet and keep its cursor.
	again, err := p.Parse(ctx, src, first.Next)
	if err != nil {
		t.Fatalf("Parse(unchanged): %v", err)
	}
	if len(again.Events) != 0 || again.Next != first.Next {
		t.Fatalf("unchanged WITHOUT ROWID ledger re-emitted %d events and moved the cursor %+v -> %+v",
			len(again.Events), first.Next, again.Next)
	}

	// A row whose id sorts before every existing row must still be counted: the
	// primary key is not a monotonic cursor.
	execDB(t, path, `INSERT INTO model_usage
	   (id,session_id,started_at,model_id,status,input_tokens,output_tokens)
	   VALUES ('a00001','s',1772359100000,'m','completed',7,3)`)
	third, err := p.Parse(ctx, src, again.Next)
	if err != nil {
		t.Fatalf("Parse(after late insert): %v", err)
	}
	if _, present := eventByKeyIn(third.Events, "zcode:a00001"); !present {
		t.Fatalf("a row inserted with an id below the existing ones was skipped: keys = %v", keys(third.Events))
	}
}

// A schema probe that fails at runtime says nothing about the database being a
// ZCode ledger: treating it as "incompatible schema" advances the cursor and
// loses every row the failed probe never saw.
func TestFailedSchemaProbeIsReportedAndKeepsTheCursor(t *testing.T) {
	dir := t.TempDir()
	// PRAGMA table_info(model_usage) fails: the view's body cannot be resolved,
	// so the probe cannot report the columns. That is a probe failure, not a
	// database that lacks the columns.
	path := writeDB(t, dir, dbName,
		`CREATE VIEW model_usage AS SELECT * FROM a_table_that_does_not_exist`,
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL)`,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`)

	cur := harness.Cursor{Fingerprint: "previous-scan", Size: 11, ModTime: time.Unix(1, 0)}
	b, err := NewWithRoots(dir).Parse(context.Background(), harness.Source{Path: path, Kind: kindSQLite}, cur)
	if err == nil {
		t.Fatalf("a failed PRAGMA table_info probe was swallowed as \"incompatible schema\": Parse returned %d events and advanced the cursor to %+v; want an error and the unchanged cursor %+v",
			len(b.Events), b.Next, cur)
	}
	if len(b.Events) != 0 {
		t.Fatalf("failed probe produced %d events, want none", len(b.Events))
	}
	if b.Next != cur {
		t.Fatalf("cursor advanced to %+v despite a failed schema probe, want %+v", b.Next, cur)
	}
}

// Control for the test above: a database that genuinely lacks the usage table
// or columns is still a non-ledger — no error, and the cursor advances so the
// file is not reopened on every scan.
func TestGenuinelyMissingUsageColumnsStillAdvance(t *testing.T) {
	dir := t.TempDir()
	path := writeDB(t, dir, dbName,
		`CREATE TABLE model_usage (id TEXT PRIMARY KEY, session_id TEXT)`,
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL)`)
	cur := harness.Cursor{}
	b, err := NewWithRoots(dir).Parse(context.Background(), harness.Source{Path: path, Kind: kindSQLite}, cur)
	if err != nil {
		t.Fatalf("Parse(incomplete schema): %v, want no error", err)
	}
	if len(b.Events) != 0 {
		t.Fatalf("incomplete schema produced %d events, want none", len(b.Events))
	}
	if b.Next == cur {
		t.Fatal("cursor did not advance for a database without the required columns")
	}
}

// A write-ahead log that cannot be read must never be replaced by the stale
// checkpointed snapshot: the rows it still holds are not in the main database
// file, and restoring the permissions does not change the file's size or
// modification time, so the cursor signature would keep hiding them.
func TestUnreadableWALNeverFallsBackToTheStaleSnapshot(t *testing.T) {
	// Windows has no Unix permission bits: os.Chmod(0o000) does not make a file
	// unreadable there, so the test cannot establish its premise.
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based unreadability does not exist on Windows: mode bits are not enforced")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod cannot make the write-ahead log unreadable")
	}
	dir := t.TempDir()
	path := writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`)
	keeper := openWALDB(t, path)
	defer keeper.Close()

	// u1 lives only in the log: the main database file is still the snapshot
	// taken before the insert.
	if _, err := keeper.Exec(`INSERT INTO model_usage
	   (id,session_id,started_at,model_id,status,input_tokens,output_tokens)
	   VALUES ('u1','s',1772359200000,'m','completed',10,1)`); err != nil {
		t.Fatalf("seed the write-ahead log: %v", err)
	}
	walPath := path + "-wal"
	if _, err := os.Stat(walPath); err != nil {
		t.Fatalf("expected a hot write-ahead log at %s: %v", walPath, err)
	}
	// Premise of the test: ignoring the log really does hide u1.
	if db, err := openWithPragmas(path, "immutable=1"); err == nil {
		var n int
		qerr := db.QueryRow(`SELECT COUNT(*) FROM model_usage`).Scan(&n)
		db.Close()
		if qerr != nil {
			t.Fatalf("count the checkpointed snapshot: %v", qerr)
		}
		if n != 0 {
			t.Fatalf("fixture is wrong: the checkpointed snapshot already holds %d rows", n)
		}
	}

	if err := os.Chmod(walPath, 0); err != nil {
		t.Fatalf("make the write-ahead log unreadable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(walPath, 0o600) })

	ctx := context.Background()
	p := NewWithRoots(dir)
	src := harness.Source{Path: path, Kind: kindSQLite}
	b, err := p.Parse(ctx, src, harness.Cursor{})
	// A retryable error or an empty batch are both fine; reporting the stale
	// snapshot, or moving the cursor, is not.
	t.Logf("parse with an unreadable write-ahead log: %d events, error = %v, cursor = %+v", len(b.Events), err, b.Next)
	if len(b.Events) != 0 {
		t.Fatalf("parse reported %d events from the stale snapshot while the write-ahead log was unreadable", len(b.Events))
	}
	if b.Next != (harness.Cursor{}) {
		t.Fatalf("cursor advanced to %+v while the write-ahead log was unreadable (parse error: %v): the log-only row would be skipped for good", b.Next, err)
	}

	if err := os.Chmod(walPath, 0o600); err != nil {
		t.Fatalf("restore the write-ahead log: %v", err)
	}
	retry, err := p.Parse(ctx, src, b.Next)
	if err != nil {
		t.Fatalf("Parse after the write-ahead log became readable again: %v", err)
	}
	if got, want := keys(retry.Events), []string{"zcode:u1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the write-ahead log became readable again DedupKeys = %v, want %v", got, want)
	}
}
