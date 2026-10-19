package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// roListing snapshots a directory as "name:size" pairs, so a test can prove a
// read-only open created, removed or resized nothing.
func roListing(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		info, err := e.Info()
		if err != nil {
			t.Fatalf("Info(%s): %v", e.Name(), err)
		}
		out = append(out, fmt.Sprintf("%s:%d", e.Name(), info.Size()))
	}
	sort.Strings(out)
	return out
}

func roDigest(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// roDirState snapshots a directory as "name:size:sha256" for every file. Unlike
// roListing's name:size pairs it also covers file *content*, which is the only
// way to see a write into an existing sidecar (the wal-index read mark) or into
// the database itself.
func roDirState(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		info, err := e.Info()
		if err != nil {
			t.Fatalf("Info(%s): %v", e.Name(), err)
		}
		out = append(out, fmt.Sprintf("%s:%d:%s", e.Name(), info.Size(), roDigest(t, filepath.Join(dir, e.Name()))))
	}
	sort.Strings(out)
	return out
}

// roFileNames lists a directory's entry names, ignoring their sizes. SQLite may
// legally rewrite or resize the -wal/-shm sidecars while a reader attaches (and
// Windows keeps a different WAL size from Unix), so the set of files - nothing
// created, nothing removed - is the portable "the reader disturbed no file"
// invariant. Content and application state are compared separately.
func roFileNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// roWALSlackBytes is how much larger than the database file SQLite's own WAL
// bookkeeping may legitimately leave the -wal after a read-only attach. Windows
// keeps a full checkpoint image in the WAL where Unix truncates it, and the exact
// byte count varies with timing, so tests bound the WAL instead of pinning its
// size: growth beyond one database image plus this slack is the signature of an
// application write (a migration, a reprice, a rollup rebuild).
const roWALSlackBytes = 1 << 20

// roRequireWALBounded asserts that the -wal still exists and did not grow beyond
// one database image plus slack, without asserting an exact byte size.
func roRequireWALBounded(t *testing.T, path string) {
	t.Helper()
	wal, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatalf("the read-only open removed the writer's -wal: %v", err)
	}
	db, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if bound := db.Size() + roWALSlackBytes; wal.Size() > bound {
		t.Fatalf("-wal = %d bytes, want at most %d (database %d + slack %d): a read-only open must not write application data",
			wal.Size(), bound, db.Size(), roWALSlackBytes)
	}
}

func roMustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s must exist: %v", path, err)
	}
}

// roValue renders one scanned SQLite value deterministically; byte slices are
// hashed because event payloads are the only large columns.
func roValue(v any) string {
	if b, ok := v.([]byte); ok {
		sum := sha256.Sum256(b)
		return fmt.Sprintf("blob:%d:%s", len(b), hex.EncodeToString(sum[:]))
	}
	return fmt.Sprintf("%v", v)
}

// roApplicationState is a portable digest of the application data a read-only
// open must never change: the schema objects and every committed row of every
// table. A migration, a reprice, a rollup rebuild or an inserted marker all show
// up as a difference. SQLite's own -wal/-shm reader coordination is deliberately
// excluded: it is not application data and its byte layout is platform- and
// timing-dependent.
func roApplicationState(t *testing.T, db *sql.DB) string {
	t.Helper()
	ctx := context.Background()

	// The store opens with MaxOpenConns(1), so each result set is closed before
	// the next query is issued.
	tables := make([]string, 0, 16)
	lines := make([]string, 0, 32)
	schema, err := db.QueryContext(ctx, "SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_master ORDER BY type, name")
	if err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	for schema.Next() {
		var typ, name, tbl, ddl string
		if err := schema.Scan(&typ, &name, &tbl, &ddl); err != nil {
			schema.Close()
			t.Fatalf("scan sqlite_master: %v", err)
		}
		lines = append(lines, fmt.Sprintf("object %s %s %s %s", typ, name, tbl, ddl))
		if typ == "table" {
			tables = append(tables, name)
		}
	}
	if err := schema.Err(); err != nil {
		schema.Close()
		t.Fatalf("sqlite_master rows: %v", err)
	}
	schema.Close()

	for _, table := range tables {
		rows, err := db.QueryContext(ctx, `SELECT * FROM "`+strings.ReplaceAll(table, `"`, `""`)+`"`)
		if err != nil {
			t.Fatalf("read table %s: %v", table, err)
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatalf("columns of %s: %v", table, err)
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				t.Fatalf("scan %s: %v", table, err)
			}
			var b strings.Builder
			b.WriteString(table)
			for i, col := range cols {
				fmt.Fprintf(&b, " %s=%s", col, roValue(vals[i]))
			}
			lines = append(lines, b.String())
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("%s rows: %v", table, err)
		}
		rows.Close()
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// roDiffOffsets lists every byte position at which two file images differ. A -1
// entry means the two images have different lengths.
func roDiffOffsets(before, after []byte) []int {
	var out []int
	n := len(before)
	if len(after) < n {
		n = len(after)
	}
	for i := 0; i < n; i++ {
		if before[i] != after[i] {
			out = append(out, i)
		}
	}
	if len(before) != len(after) {
		out = append(out, -1)
	}
	return out
}

func roMustEqual[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func roMustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s exists (stat err = %v), want it absent", path, err)
	}
}

func roRequireReason(t *testing.T, err error, want ReadOnlyReason) *ReadOnlyError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a %s error, got nil", want)
	}
	var roErr *ReadOnlyError
	if !errors.As(err, &roErr) {
		t.Fatalf("error %v (%T) is not a *ReadOnlyError", err, err)
	}
	if roErr.Reason != want {
		t.Fatalf("reason = %q, want %q (detail: %s)", roErr.Reason, want, roErr.Detail)
	}
	got, ok := ReadOnlyReasonOf(err)
	if !ok || got != want {
		t.Fatalf("ReadOnlyReasonOf = (%q,%v), want (%q,true)", got, ok, want)
	}
	return roErr
}

// roLiveStore opens a writable store and keeps it open, so the WAL sidecars
// exist exactly as they do while the app is running.
func roLiveStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mytoken.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { st.Close() })
	return st, path
}

func roCommit(t *testing.T, st *Store, key, session string, input int64) {
	t.Helper()
	e := model.UsageEvent{
		Harness:   model.Codex,
		DedupKey:  key,
		SessionID: session,
		Model:     "m",
		Provider:  "p",
		BaseURL:   "https://api.example.com",
		Timestamp: time.Date(2026, 3, 4, 12, 0, 0, 0, time.Local),
		Tokens:    model.Tokens{Input: input},
	}
	if err := st.CommitMany(context.Background(), []Write{{
		Harness: model.Codex,
		Path:    "log",
		Batch:   harness.Batch{Events: []model.UsageEvent{e}, Next: harness.Cursor{Offset: input}},
	}}); err != nil {
		t.Fatalf("commit %q: %v", key, err)
	}
}

func roCountEvents(t *testing.T, st *Store) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := st.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("BeginTx(ReadOnly): %v", err)
	}
	defer tx.Rollback()
	var n int64
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

func roCopyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

// ---------------------------------------------------------------------------
// T1 missing database
// ---------------------------------------------------------------------------

func TestOpenReadOnlyMissingDatabaseCreatesNothing(t *testing.T) {
	ctx := context.Background()

	t.Run("missing file in an existing directory", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "mytoken.db")
		before := roListing(t, dir)

		_, err := OpenReadOnly(ctx, path)
		roRequireReason(t, err, ReasonMissingDatabase)

		roMustEqual(t, "directory listing", strings.Join(roListing(t, dir), ","), strings.Join(before, ","))
		roMustNotExist(t, path)
		roMustNotExist(t, path+"-wal")
		roMustNotExist(t, path+"-shm")
	})

	t.Run("missing parent directory is not created", func(t *testing.T) {
		dir := t.TempDir()
		parent := filepath.Join(dir, "Application Support", "MyToken")
		path := filepath.Join(parent, "mytoken.db")
		before := roListing(t, dir)

		_, err := OpenReadOnly(ctx, path)
		roRequireReason(t, err, ReasonMissingDatabase)

		roMustNotExist(t, parent)
		roMustEqual(t, "directory listing", strings.Join(roListing(t, dir), ","), strings.Join(before, ","))
	})

	t.Run("empty path", func(t *testing.T) {
		_, err := OpenReadOnly(ctx, "")
		if err == nil {
			t.Fatal("expected an error for an empty path")
		}
	})
}

func TestOpenReadOnlyRejectsNonRegularFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	_, err := OpenReadOnly(ctx, dir)
	roRequireReason(t, err, ReasonNotRegularFile)
}

// ---------------------------------------------------------------------------
// corrupt / unreadable
// ---------------------------------------------------------------------------

func TestOpenReadOnlyCorruptDatabaseIsTypedAndUntouched(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "mytoken.db")
	if err := os.WriteFile(path, bytes.Repeat([]byte("this is not a sqlite database!"), 128), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := roDigest(t, path)
	before := roListing(t, dir)

	_, err := OpenReadOnly(ctx, path)
	roRequireReason(t, err, ReasonCorrupt)

	roMustEqual(t, "db digest", roDigest(t, path), digest)
	roMustEqual(t, "directory listing", strings.Join(roListing(t, dir), ","), strings.Join(before, ","))
}

func TestOpenReadOnlyTruncatedDatabaseIsTyped(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "mytoken.db")
	roRollbackDB(t, path)
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, full[:512], 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = OpenReadOnly(ctx, path)
	roRequireReason(t, err, ReasonCorrupt)
}

// ---------------------------------------------------------------------------
// T2 legacy marker-less schema
// ---------------------------------------------------------------------------

// roRollbackDB creates a current-schema index that is not in WAL format: the
// ordinary open runs the migrations once, then the journal mode is switched
// back to DELETE, which checkpoints and removes the WAL and shared-memory
// sidecars. Such a file is readable read-only without creating anything.
func roRollbackDB(t *testing.T, path string) {
	t.Helper()
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	roCommit(t, st, "k1", "s1", 10)
	var mode string
	if err := st.DB().QueryRowContext(context.Background(), "PRAGMA journal_mode=DELETE").Scan(&mode); err != nil {
		t.Fatalf("journal_mode=DELETE: %v", err)
	}
	if mode != "delete" {
		t.Fatalf("journal_mode = %q, want delete", mode)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// roLegacyDB builds a pre-marker database: it has the settings table and an
// events read surface, but none of the schema-generation markers. It is a
// rollback-journal file, so reading it needs no -shm sidecar.
func roLegacyDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		"PRAGMA journal_mode=DELETE",
		"CREATE TABLE settings(key TEXT PRIMARY KEY, value TEXT)",
		"CREATE TABLE events(timestamp INTEGER, input INTEGER)",
		"INSERT INTO events(timestamp,input) VALUES(1,1)",
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// A settings table of the wrong shape cannot answer the marker queries: the
// generation is undecidable, which must be its own state rather than an
// optimistic "no markers, so treat it as empty".
func TestOpenReadOnlyUnclassifiableSchemaIsUnreadable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "mytoken.db")
	roLegacyDB(t, path)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// Replace the settings table with one that cannot be read as key/value.
	for _, q := range []string{
		"DROP TABLE settings",
		"CREATE TABLE settings(unrelated TEXT)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()

	digest := roDigest(t, path)
	_, err = OpenReadOnly(ctx, path)
	roErr := roRequireReason(t, err, ReasonUnavailable)
	roMustEqual(t, "schema state", roErr.Schema.State, SchemaUnreadable)
	roMustEqual(t, "db digest", roDigest(t, path), digest)
}

func TestOpenReadOnlyLegacySchemaIsUpgradeRequiredAndUntouched(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "mytoken.db")
	roLegacyDB(t, path)

	digest := roDigest(t, path)
	before := roListing(t, dir)

	_, err := OpenReadOnly(ctx, path)
	roErr := roRequireReason(t, err, ReasonSchemaUpgradeRequired)
	roMustEqual(t, "schema state", roErr.Schema.State, SchemaUpgradeRequired)
	if len(roErr.Schema.Missing) == 0 {
		t.Fatal("upgrade_required report lists no missing marker")
	}

	// No migration, no backup, no marker write.
	roMustEqual(t, "db digest", roDigest(t, path), digest)
	roMustEqual(t, "directory listing", strings.Join(roListing(t, dir), ","), strings.Join(before, ","))
	roMustNotExist(t, path+"-wal")
	roMustNotExist(t, path+"-shm")

	// The settings row count is unchanged (still zero: no marker was written).
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM settings").Scan(&n); err != nil {
		t.Fatal(err)
	}
	roMustEqual(t, "settings rows", n, 0)
}

// A legacy database with no settings table at all is still upgrade_required,
// never "ok" and never a read path that assumes the new tables exist.
func TestOpenReadOnlyPreSettingsSchemaIsUpgradeRequired(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "mytoken.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE events(timestamp INTEGER)"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	digest := roDigest(t, path)
	_, err = OpenReadOnly(ctx, path)
	roErr := roRequireReason(t, err, ReasonSchemaUpgradeRequired)
	roMustEqual(t, "schema state", roErr.Schema.State, SchemaUpgradeRequired)
	roMustEqual(t, "db digest", roDigest(t, path), digest)
}

// ---------------------------------------------------------------------------
// T3 unknown / newer marker value
// ---------------------------------------------------------------------------

func TestOpenReadOnlyNewerSchemaIsReportedWithoutTouchingBytes(t *testing.T) {
	ctx := context.Background()
	st, path := roLiveStore(t)
	roCommit(t, st, "k1", "s1", 10)
	if err := st.SetSetting(ctx, instanceVersionKey, "instance.v2"); err != nil {
		t.Fatal(err)
	}

	digest := roDigest(t, path)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mtime := info.ModTime()

	_, err = OpenReadOnly(ctx, path)
	roErr := roRequireReason(t, err, ReasonSchemaNewer)
	roMustEqual(t, "schema state", roErr.Schema.State, SchemaNewer)
	roMustEqual(t, "marker value", roErr.Schema.Markers[instanceVersionKey], "instance.v2")

	roMustEqual(t, "db digest", roDigest(t, path), digest)
	info2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	roMustEqual(t, "db mtime", info2.ModTime().Equal(mtime), true)

	// The unknown marker is reported, not repaired or downgraded.
	got, err := st.Setting(ctx, instanceVersionKey)
	if err != nil {
		t.Fatal(err)
	}
	roMustEqual(t, "stored marker", got, "instance.v2")
}

func TestOpenReadOnlyUnknownProviderMarkerIsNewer(t *testing.T) {
	ctx := context.Background()
	st, path := roLiveStore(t)
	if err := st.SetSetting(ctx, providerIdentityVersion, "provider.identity.v2"); err != nil {
		t.Fatal(err)
	}

	_, err := OpenReadOnly(ctx, path)
	roErr := roRequireReason(t, err, ReasonSchemaNewer)
	roMustEqual(t, "marker value", roErr.Schema.Markers[providerIdentityVersion], "provider.identity.v2")
}
func TestOpenReadOnlyAppliesReadOnlyPragmas(t *testing.T) {
	ctx := context.Background()
	st, path := roLiveStore(t)
	roCommit(t, st, "k1", "s1", 10)

	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()

	var queryOnly int
	if err := ro.DB().QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil {
		t.Fatal(err)
	}
	roMustEqual(t, "query_only", queryOnly, 1)

	var busyTimeout int
	if err := ro.DB().QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	roMustEqual(t, "busy_timeout", busyTimeout, readOnlyBusyTimeoutMS)

	// The journal mode is never forced by the reader: the file keeps whichever
	// mode it already had.
	var journalMode string
	if err := ro.DB().QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	roMustEqual(t, "journal_mode", journalMode, "wal")

	roMustEqual(t, "schema state", ro.ReadOnlySchema().State, SchemaOK)
}

// ---------------------------------------------------------------------------
// T4 WAL freshness and the sidecar rule
// ---------------------------------------------------------------------------

func TestOpenReadOnlySeesCommittedWALFromLiveWriter(t *testing.T) {
	ctx := context.Background()
	st, path := roLiveStore(t)
	roCommit(t, st, "k1", "s1", 10)

	// Application state and the database image captured before the reader
	// attaches: attaching must not change a single committed row or schema
	// object. An exact -wal byte size cannot express that (Windows keeps a WAL
	// that Unix truncates, and SQLite rewrites the sidecars as reader
	// coordination), so application state and the file set are compared and the
	// WAL is only bounded.
	dir := filepath.Dir(path)
	stateBeforeOpen := roApplicationState(t, st.DB())
	digestBeforeOpen := roDigest(t, path)
	filesBeforeOpen := roFileNames(t, dir)

	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	roMustEqual(t, "ReadOnly()", ro.ReadOnly(), true)
	roMustEqual(t, "schema state", ro.ReadOnlySchema().State, SchemaOK)
	roMustEqual(t, "events seen", roCountEvents(t, ro), int64(1))
	// This is the assertion a read-only open that migrated, repriced or inserted
	// through another connection would break.
	roMustEqual(t, "application state after open", roApplicationState(t, st.DB()), stateBeforeOpen)
	roMustEqual(t, "db digest after open", roDigest(t, path), digestBeforeOpen)
	roMustEqual(t, "files after open", strings.Join(roFileNames(t, dir), ","), strings.Join(filesBeforeOpen, ","))

	// The writer commits again while the read-only handle is open: a
	// WAL-fresh reader must see it, which immutable=1 would not.
	roCommit(t, st, "k2", "s1", 20)
	roMustEqual(t, "events seen after commit", roCountEvents(t, ro), int64(2))

	// Closing the read-only handle must leave the writer's committed data and the
	// database bytes untouched.
	stateBeforeClose := roApplicationState(t, st.DB())
	digestBeforeClose := roDigest(t, path)
	filesBeforeClose := roFileNames(t, dir)
	if err := ro.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	roMustEqual(t, "db digest after close", roDigest(t, path), digestBeforeClose)
	roMustEqual(t, "application state after close", roApplicationState(t, st.DB()), stateBeforeClose)
	roMustEqual(t, "files after close", strings.Join(roFileNames(t, dir), ","), strings.Join(filesBeforeClose, ","))
	roRequireWALBounded(t, path)
}

// A cleanly closed WAL database has no -shm. SQLite creates the sidecars while
// attaching read-only, which the corrected contract allows: that is SQLite's own
// reader coordination, not index data. The read must still see the committed
// frames and must leave the database file byte-identical.
func TestOpenReadOnlyReadsCleanlyClosedWALDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "mytoken.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	roCommit(t, st, "k1", "s1", 10)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	roMustNotExist(t, path+"-shm")
	dbBefore := roDigest(t, path)

	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("OpenReadOnly on a cleanly closed WAL database: %v", err)
	}
	roMustEqual(t, "ReadOnly()", ro.ReadOnly(), true)
	roMustEqual(t, "schema state", ro.ReadOnlySchema().State, SchemaOK)
	roMustEqual(t, "events seen", roCountEvents(t, ro), int64(1))
	roMustEqual(t, "db digest while open", roDigest(t, path), dbBefore)
	if err := ro.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	roMustEqual(t, "db digest after close", roDigest(t, path), dbBefore)
}

// A WAL database copied without its -shm sidecar still holds committed frames in
// the -wal file. Refusing it would report a perfectly good index as unreadable,
// so the corrected contract reads it and shows those frames.
func TestOpenReadOnlyReadsWALWithoutSharedMemory(t *testing.T) {
	ctx := context.Background()
	st, path := roLiveStore(t)
	roCommit(t, st, "k1", "s1", 10)

	// A copy of a live database directory taken without its -shm: the WAL still
	// holds committed frames, but there is no shared memory file.
	dir := t.TempDir()
	copyPath := filepath.Join(dir, "mytoken.db")
	roCopyFile(t, path, copyPath)
	roCopyFile(t, path+"-wal", copyPath+"-wal")
	roMustNotExist(t, copyPath+"-shm")

	ro, err := OpenReadOnly(ctx, copyPath)
	if err != nil {
		t.Fatalf("OpenReadOnly on a WAL database without -shm: %v", err)
	}
	defer ro.Close()
	roMustEqual(t, "events seen", roCountEvents(t, ro), int64(1))
}

// roForeignWALWriter builds a current-schema index and then keeps a WAL write
// connection open on it that this package did not create: the same shape as a
// writer in another process, whose live -wal/-shm sidecars this process cannot
// see the owner of.
func roForeignWALWriter(t *testing.T, path string) *sql.DB {
	t.Helper()
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	roCommit(t, st, "k1", "s1", 10)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	roMustNotExist(t, path+"-shm")

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("foreign open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec("PRAGMA journal_mode=WAL; INSERT INTO settings(key,value) VALUES('foreign','1')"); err != nil {
		t.Fatalf("foreign WAL write: %v", err)
	}
	// The write made this connection the owner of live sidecars.
	if _, err := os.Stat(path + "-shm"); err != nil {
		t.Fatalf("expected the foreign writer to have created -shm: %v", err)
	}
	return db
}

// A read-only reader must be able to read an index that a writer in another
// process is holding open: that is the normal shape of "the app owns the index,
// the statusline reads it". The old ownership rule refused exactly this state,
// which is what made snapshot/statusline/report/doctor unusable. The reader must
// see the foreign writer's committed WAL frames (an immutable-style open would
// hide them) and must not change the database or the committed WAL frames.
func TestOpenReadOnlyReadsForeignWriterWAL(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "mytoken.db")
	roForeignWALWriter(t, path)
	dbBefore := roDigest(t, path)
	walBefore := roDigest(t, path+"-wal")

	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("OpenReadOnly with a foreign writer: %v", err)
	}
	defer ro.Close()

	var v string
	if err := ro.DB().QueryRowContext(ctx, "SELECT value FROM settings WHERE key = 'foreign'").Scan(&v); err != nil {
		t.Fatalf("read the foreign writer's committed frame: %v", err)
	}
	roMustEqual(t, "foreign setting", v, "1")
	roMustEqual(t, "events seen", roCountEvents(t, ro), int64(1))
	roMustEqual(t, "db bytes", roDigest(t, path), dbBefore)
	roMustEqual(t, "committed wal bytes", roDigest(t, path+"-wal"), walBefore)
}

// Sidecars left behind by an earlier read-only attach have no live writer at
// all. They must stay readable: the old ownership rule refused this state too,
// so a second read of the same cleanly closed index failed where the first
// succeeded.
func TestOpenReadOnlyReadsSidecarsWithNoLiveWriter(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "mytoken.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	roCommit(t, st, "k1", "s1", 10)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	roMustNotExist(t, path+"-shm")

	first, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("first read-only open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + "-shm"); err != nil {
		t.Fatalf("expected the read-only attach to have created -shm: %v", err)
	}

	second, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("read-only open with sidecars and no live writer: %v", err)
	}
	defer second.Close()
	roMustEqual(t, "events seen", roCountEvents(t, second), int64(1))
}

// A read-only connection into an existing -shm is not entirely passive: SQLite
// records a wal-index read mark in the shared memory. That is the one write the
// contract allows, it is bounded to the read-mark slot, and the database and
// WAL images must stay byte-identical.
func TestOpenReadOnlyReadOnlyReaderWritesOnlyTheWALReadMark(t *testing.T) {
	ctx := context.Background()
	st, path := roLiveStore(t)
	roCommit(t, st, "k1", "s1", 10)
	dir := filepath.Dir(path)

	shmBefore, err := os.ReadFile(path + "-shm")
	if err != nil {
		t.Fatalf("read -shm: %v", err)
	}
	dbBefore := roDigest(t, path)
	walBefore := roDigest(t, path+"-wal")
	filesBefore := roFileNames(t, dir)

	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	roMustEqual(t, "events seen", roCountEvents(t, ro), int64(1))

	shmAfter, err := os.ReadFile(path + "-shm")
	if err != nil {
		t.Fatalf("read -shm after: %v", err)
	}
	roMustEqual(t, "database bytes", roDigest(t, path), dbBefore)
	roMustEqual(t, "wal bytes", roDigest(t, path+"-wal"), walBefore)
	roMustEqual(t, "shm size", len(shmAfter), len(shmBefore))
	for _, off := range roDiffOffsets(shmBefore, shmAfter) {
		if off < 96 || off >= 120 {
			t.Fatalf("read-only open changed -shm byte %d; only the wal-index read mark (offset 104) may change", off)
		}
	}

	if err := ro.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Closing must not create or remove a file. The -wal/-shm sizes are not
	// portable (see roFileNames/roRequireWALBounded); the byte-level assertions
	// above already pin the database and the committed WAL frames.
	roMustEqual(t, "files after close", strings.Join(roFileNames(t, dir), ","), strings.Join(filesBefore, ","))
	roRequireWALBounded(t, path)
}

// The read-only handle pins the sidecars it attached to: while it is open the
// last writer cannot checkpoint the WAL out from under it, so its view stays
// valid and no file appears or disappears.
func TestOpenReadOnlyHandleSurvivesWriterClose(t *testing.T) {
	ctx := context.Background()
	st, path := roLiveStore(t)
	roCommit(t, st, "k1", "s1", 10)
	dir := filepath.Dir(path)

	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()
	roMustEqual(t, "events seen", roCountEvents(t, ro), int64(1))

	if err := st.Close(); err != nil {
		t.Fatalf("writer Close: %v", err)
	}
	if _, err := os.Stat(path + "-shm"); err != nil {
		t.Fatalf("writer close removed -shm while a read-only handle was open: %v", err)
	}

	before := roListing(t, dir)
	roMustEqual(t, "events seen after writer close", roCountEvents(t, ro), int64(1))
	roMustEqual(t, "directory listing", strings.Join(roListing(t, dir), ","), strings.Join(before, ","))
}

// A rollback-journal database needs no shared memory, so it must stay readable
// without creating anything - and it stays readable for every writer state,
// since there is no -shm/-wal for a writer to remove.
func TestOpenReadOnlyReadsRollbackJournalDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "mytoken.db")
	roRollbackDB(t, path)
	roMustNotExist(t, path+"-shm")
	roMustNotExist(t, path+"-wal")

	before := roListing(t, dir)
	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("OpenReadOnly on a rollback-journal database: %v", err)
	}
	roMustEqual(t, "schema state", ro.ReadOnlySchema().State, SchemaOK)
	roMustEqual(t, "ReadOnly()", ro.ReadOnly(), true)
	roMustEqual(t, "events seen", roCountEvents(t, ro), int64(1))
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	roMustEqual(t, "directory listing", strings.Join(roListing(t, dir), ","), strings.Join(before, ","))
}

// Read-only file permissions on the database and its sidecars must still be
// readable, and must still not be the reason a WAL database is refused.
func TestOpenReadOnlyWithReadOnlyFilePermissions(t *testing.T) {
	ctx := context.Background()
	st, path := roLiveStore(t)
	roCommit(t, st, "k1", "s1", 10)

	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path+"-wal", 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path+"-shm", 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Chmod(path, 0o600)
		os.Chmod(path+"-wal", 0o600)
		os.Chmod(path+"-shm", 0o600)
	})

	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("OpenReadOnly with 0444 files: %v", err)
	}
	defer ro.Close()
	roMustEqual(t, "events seen", roCountEvents(t, ro), int64(1))
}

func TestOpenReadOnlyUnreadableDatabaseIsTyped(t *testing.T) {
	// Windows has no Unix permission bits: os.Chmod(0o000) does not make a file
	// unreadable there, so "permission_denied from an unreadable file" is a
	// concept that does not exist on this platform.
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based unreadability does not exist on Windows: mode bits are not enforced")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	ctx := context.Background()
	st, path := roLiveStore(t)
	roCommit(t, st, "k1", "s1", 10)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o600) })

	ro, err := OpenReadOnly(ctx, path)
	if ro != nil {
		// Never leak a handle on the unexpected-success path: an open database
		// left behind breaks the TempDir RemoveAll cleanup as well as the
		// assertion below.
		defer ro.Close()
	}
	roRequireReason(t, err, ReasonPermissionDenied)
}

// ---------------------------------------------------------------------------
// bounded locking contention
// ---------------------------------------------------------------------------

func TestOpenReadOnlyLockedDatabaseIsBoundedAndTyped(t *testing.T) {
	ctx := context.Background()
	st, path := roLiveStore(t)
	roCommit(t, st, "k1", "s1", 10)
	if err := st.Exec(ctx, "PRAGMA locking_mode=EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	// Take the exclusive lock and hold it for the duration of the test.
	if err := st.Exec(ctx, "INSERT INTO settings VALUES('exclusive','1')"); err != nil {
		t.Fatal(err)
	}

	old := readOnlyBusyTimeoutMS
	readOnlyBusyTimeoutMS = 150
	t.Cleanup(func() { readOnlyBusyTimeoutMS = old })

	before := roListing(t, filepath.Dir(path))
	start := time.Now()
	_, err := OpenReadOnly(ctx, path)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a lock error from an exclusively locked database")
	}
	reason, ok := ReadOnlyReasonOf(err)
	if !ok || (reason != ReasonLocked && reason != ReasonUnavailable) {
		t.Fatalf("reason = %q (%v), want %q or %q", reason, err, ReasonLocked, ReasonUnavailable)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("locking contention took %s, want a bounded wait", elapsed)
	}
	roMustEqual(t, "directory listing", strings.Join(roListing(t, filepath.Dir(path)), ","), strings.Join(before, ","))
}

// ---------------------------------------------------------------------------
// T5 no application writes
// ---------------------------------------------------------------------------

// The reader performs no application write. With a live writer already holding
// the sidecars, no file may be created or removed and the application state must
// be identical: the same database bytes, the same schema objects and the same
// committed rows. SQLite's own -wal/-shm resizing is reader coordination rather
// than a write, and its exact size is platform- and timing-dependent (Windows
// keeps a full-size WAL where Unix truncates it), so the WAL is bounded rather
// than pinned.
func TestOpenReadOnlyPerformsNoApplicationWrite(t *testing.T) {
	ctx := context.Background()
	st, path := roLiveStore(t)
	roCommit(t, st, "k1", "s1", 10)
	roCommit(t, st, "k2", "s2", 20)

	dir := filepath.Dir(path)
	digest := roDigest(t, path)
	state := roApplicationState(t, st.DB())
	files := roFileNames(t, dir)
	roMustExist(t, path+"-wal")

	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}

	// A full read sweep, the way a snapshot would touch the database.
	for _, table := range []string{
		"events", "sessions", "cursors", "settings", "event_data", "event_dimensions",
		"daily_usage", "provider_configs", "provider_endpoints",
		"provider_daily",
	} {
		var n int64
		if err := ro.DB().QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
	}
	var check string
	if err := ro.DB().QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil {
		t.Fatalf("quick_check: %v", err)
	}
	roMustEqual(t, "quick_check", check, "ok")

	// Writes are refused by the connection itself, not merely discouraged.
	if err := ro.Exec(ctx, "INSERT INTO settings VALUES('ro-write','1')"); err == nil {
		t.Fatal("write through the read-only handle succeeded")
	}
	if err := ro.SetSetting(ctx, "ro-write", "1"); err == nil {
		t.Fatal("SetSetting through the read-only handle succeeded")
	}
	var queryOnly int
	if err := ro.DB().QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil {
		t.Fatal(err)
	}
	roMustEqual(t, "PRAGMA query_only", queryOnly, 1)

	if err := ro.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	roMustEqual(t, "db digest", roDigest(t, path), digest)
	roMustEqual(t, "application state", roApplicationState(t, st.DB()), state)
	roMustEqual(t, "files", strings.Join(roFileNames(t, dir), ","), strings.Join(files, ","))
	// The -wal must not be truncated away, but its exact size is not portable:
	// Windows keeps a full-size WAL where Unix truncates it. Bound it instead of
	// pinning it, and rely on the application-state comparison above to catch an
	// application write regardless of where SQLite buffered it.
	roRequireWALBounded(t, path)
}

// ---------------------------------------------------------------------------
// schema reporting helpers
// ---------------------------------------------------------------------------

func TestInspectSchemaReadOnlyCurrentSchema(t *testing.T) {
	ctx := context.Background()
	st, path := roLiveStore(t)
	roCommit(t, st, "k1", "s1", 10)

	report, err := InspectSchemaReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("InspectSchemaReadOnly: %v", err)
	}
	roMustEqual(t, "schema state", report.State, SchemaOK)
	for _, key := range []string{providerIdentityVersion, providerRollupVersion, instanceVersionKey} {
		if report.Markers[key] != "1" {
			t.Fatalf("marker %s = %q, want \"1\" (markers: %v)", key, report.Markers[key], report.Markers)
		}
	}
	roMustEqual(t, "missing markers", len(report.Missing), 0)
}

func TestInspectSchemaReadOnlyMissingDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "mytoken.db")

	_, err := InspectSchemaReadOnly(ctx, path)
	roRequireReason(t, err, ReasonMissingDatabase)
	roMustNotExist(t, path)
}

// A schema marker that is present but whose table shape does not match must be
// upgrade_required, never "ok" and never silently repaired.
func TestOpenReadOnlyInconsistentInstanceSchemaIsUpgradeRequired(t *testing.T) {
	ctx := context.Background()
	st, path := roLiveStore(t)
	roCommit(t, st, "k1", "s1", 10)

	// Drop a required rollup trigger: the marker says the instance schema is
	// complete, but the shape no longer is.
	if err := st.Exec(ctx, "DROP TRIGGER daily_insert"); err != nil {
		t.Fatal(err)
	}
	digest := roDigest(t, path)

	_, err := OpenReadOnly(ctx, path)
	roErr := roRequireReason(t, err, ReasonSchemaUpgradeRequired)
	roMustEqual(t, "schema state", roErr.Schema.State, SchemaUpgradeRequired)
	roMustEqual(t, "db digest", roDigest(t, path), digest)

	// The missing trigger was reported, not recreated.
	var n int
	if err := st.DB().QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name='daily_insert'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	roMustEqual(t, "recreated trigger", n, 0)
}

// ---------------------------------------------------------------------------
// error type
// ---------------------------------------------------------------------------

func TestReadOnlyErrorIsTyped(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "mytoken.db")

	_, err := OpenReadOnly(ctx, path)
	var roErr *ReadOnlyError
	if !errors.As(err, &roErr) {
		t.Fatalf("error %v is not a *ReadOnlyError", err)
	}
	roMustEqual(t, "reason", roErr.Reason, ReasonMissingDatabase)
	roMustEqual(t, "path", roErr.Path, path)
	if !strings.Contains(roErr.Error(), "mytoken.db") {
		t.Fatalf("Error() = %q, want it to name the database", roErr.Error())
	}
	if !errors.Is(err, ErrReadOnlyMissingDatabase) {
		t.Fatal("errors.Is(err, ErrReadOnlyMissingDatabase) = false")
	}
	if errors.Is(err, ErrReadOnlyCorrupt) {
		t.Fatal("a missing database matched the corrupt sentinel")
	}
}
