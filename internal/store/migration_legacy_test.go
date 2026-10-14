package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/sqlitedsn"
)

// legacyPhysicalSchema is the real pre-0.4 on-disk index: a physical events table
// with no raw_project and no priced column, matching exactly what store.go created
// before the provider identity work. legacyProviderFixture in
// provider_migration_test.go only opens the current schema and then drops one new
// table, so it never exercises a genuine legacy file.
const legacyPhysicalSchema = `
CREATE TABLE events(
 harness TEXT NOT NULL,dedup_key TEXT NOT NULL,session_id TEXT NOT NULL,parent_id TEXT NOT NULL DEFAULT '',project TEXT NOT NULL DEFAULT '',timestamp INTEGER NOT NULL,
 model TEXT NOT NULL,provider TEXT NOT NULL DEFAULT '',base_url TEXT NOT NULL DEFAULT '',input INTEGER NOT NULL,output INTEGER NOT NULL,cache_read INTEGER NOT NULL,
 cache_write INTEGER NOT NULL,reasoning INTEGER NOT NULL,log_cost REAL,resolved_provider TEXT NOT NULL,attrib TEXT NOT NULL,cost REAL NOT NULL,
 PRIMARY KEY(harness,dedup_key));
CREATE TABLE sessions(harness TEXT NOT NULL,session_id TEXT NOT NULL,parent_id TEXT NOT NULL DEFAULT '',title TEXT NOT NULL DEFAULT '',project TEXT NOT NULL DEFAULT '',started_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,PRIMARY KEY(harness,session_id));
CREATE TABLE settings(key TEXT PRIMARY KEY,value TEXT NOT NULL);`

// legacyPhysicalRows: one authoritative log cost, one local estimate, one free
// request, so initializing priced exercises both branches.
const legacyPhysicalRows = `
INSERT INTO events VALUES
 ('codex','req-log','s1','','proj-a',1700000000000000000,'gpt-x','acct','https://api.example.com/v1',11,12,13,14,15,2.5,'acct','log',2.5),
 ('codex','req-est','s1','','proj-a',1700000000000000001,'gpt-x','acct','https://api.example.com/v1',21,22,23,24,25,NULL,'acct','est',0.75),
 ('codex','req-free','s2','','proj-a',1700000000000000002,'free','','',1,2,3,4,5,NULL,'','',0);
INSERT INTO sessions VALUES('codex','s1','','title','proj-a',1700000000000000000,1700000000000000001),('codex','s2','','','proj-a',1700000000000000002,1700000000000000002);
INSERT INTO settings VALUES('pricing_fingerprint','keep-me');`

const legacyTotals = `SELECT count(*),sum(input),sum(output),sum(cache_read),sum(cache_write),sum(reasoning),sum(cost),sum(log_cost) FROM events`

type legacyTotalsRow struct {
	requests, in, out, read, write, reason int
	cost, logCost                          sql.NullFloat64
}

func writeLegacyPhysicalDatabase(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(legacyPhysicalSchema + legacyPhysicalRows); err != nil {
		t.Fatal(err)
	}
}

func openReadOnly(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqlitedsn.URI(path, "mode=ro"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func readLegacyTotals(t *testing.T, db *sql.DB) legacyTotalsRow {
	t.Helper()
	var row legacyTotalsRow
	if err := db.QueryRow(legacyTotals).Scan(&row.requests, &row.in, &row.out, &row.read, &row.write, &row.reason, &row.cost, &row.logCost); err != nil {
		t.Fatal(err)
	}
	return row
}

func assertFactsUnchanged(t *testing.T, db *sql.DB) {
	t.Helper()
	got := readLegacyTotals(t, db)
	want := legacyTotalsRow{requests: 3, in: 33, out: 36, read: 39, write: 42, reason: 45}
	want.cost = sql.NullFloat64{Float64: 3.25, Valid: true}
	want.logCost = sql.NullFloat64{Float64: 2.5, Valid: true}
	if got != want {
		t.Fatalf("history facts changed: got %+v want %+v", got, want)
	}
}

func assertNoIdentityMigrationArtifacts(t *testing.T, db *sql.DB) {
	t.Helper()
	var kind string
	var n int
	if err := db.QueryRow("SELECT type FROM sqlite_master WHERE name='events'").Scan(&kind); err != nil || kind != "table" {
		t.Fatalf("legacy events table not restored verbatim: %q %v", kind, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='event_data'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("event_data survived rollback: %d %v", n, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='provider_endpoints'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("provider_endpoints survived rollback: %d %v", n, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM settings WHERE key='provider.identity.v1'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("identity marker survived rollback: %d %v", n, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM pragma_table_info('events') WHERE name IN ('priced','raw_project')").Scan(&n); err != nil || n != 0 {
		t.Fatalf("additive columns survived rollback: %d %v", n, err)
	}
}

// A genuine legacy file must keep every token class, request and historical cost,
// initialize priced for the rows that carry authoritative history, and leave a
// backup that is a byte-for-byte usable pre-migration snapshot.
func TestLegacyPhysicalMigrationPreservesFactsAndBacksUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	writeLegacyPhysicalDatabase(t, path)

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	backup := path + ".pre-provider-v1.db"
	if _, err = os.Stat(backup); err != nil {
		t.Fatalf("missing pre-migration backup: %v", err)
	}
	// The backup must be the *pre-migration* file, not a copy of the rewritten index.
	old := openReadOnly(t, backup)
	var kind string
	if err = old.QueryRow("SELECT type FROM sqlite_master WHERE name='events'").Scan(&kind); err != nil || kind != "table" {
		t.Fatalf("backup is not a legacy snapshot: %q %v", kind, err)
	}
	var dict int
	if err = old.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='event_data'").Scan(&dict); err != nil || dict != 0 {
		t.Fatalf("backup contains post-migration schema: %d %v", dict, err)
	}
	assertFactsUnchanged(t, old)

	assertFactsUnchanged(t, st.DB())
	var marker string
	if marker, err = st.Setting(context.Background(), providerIdentityVersion); err != nil || marker != "1" {
		t.Fatalf("identity marker: %q %v", marker, err)
	}
	var fingerprint string
	if err = st.DB().QueryRow("SELECT value FROM settings WHERE key='pricing_fingerprint'").Scan(&fingerprint); err != nil || fingerprint != "keep-me" {
		t.Fatalf("historical pricing checkpoint changed: %q %v", fingerprint, err)
	}
	// priced must be initialized for authoritative costs only, per row.
	flags := map[string]int{}
	rows, err := st.DB().Query("SELECT dedup_key,priced FROM events ORDER BY dedup_key")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var key string
		var priced int
		if err = rows.Scan(&key, &priced); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		flags[key] = priced
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"req-log": 1, "req-est": 1, "req-free": 0}
	for key, value := range want {
		if flags[key] != value {
			t.Fatalf("priced[%s]=%d want %d (all %v)", key, flags[key], value, flags)
		}
	}
	// Reopening a completed migration must not try to reserve the backup again.
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("idempotent reopen: %v", err)
	}
	reopened.Close()
}

// When the legacy file already carries priced, the migration must preserve the
// stored flags exactly: it must not re-derive or "heal" them from cost/log_cost.
func TestLegacyPhysicalMigrationKeepsExistingPricedFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-priced.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE events(
 harness TEXT NOT NULL,dedup_key TEXT NOT NULL,session_id TEXT NOT NULL,parent_id TEXT NOT NULL DEFAULT '',project TEXT NOT NULL DEFAULT '',timestamp INTEGER NOT NULL,
 model TEXT NOT NULL,provider TEXT NOT NULL DEFAULT '',base_url TEXT NOT NULL DEFAULT '',input INTEGER NOT NULL,output INTEGER NOT NULL,cache_read INTEGER NOT NULL,
 cache_write INTEGER NOT NULL,reasoning INTEGER NOT NULL,log_cost REAL,resolved_provider TEXT NOT NULL,attrib TEXT NOT NULL,cost REAL NOT NULL,priced INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(harness,dedup_key));
CREATE TABLE sessions(harness TEXT NOT NULL,session_id TEXT NOT NULL,parent_id TEXT NOT NULL DEFAULT '',title TEXT NOT NULL DEFAULT '',project TEXT NOT NULL DEFAULT '',started_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,PRIMARY KEY(harness,session_id));
CREATE TABLE settings(key TEXT PRIMARY KEY,value TEXT NOT NULL);
INSERT INTO events VALUES
 ('codex','flag-explicit-zero','s1','','proj-a',1700000000000000000,'gpt-x','acct','https://api.example.com/v1',11,12,13,14,15,NULL,'acct','est',9.99,0),
 ('codex','flag-explicit-one','s1','','proj-a',1700000000000000001,'gpt-x','acct','https://api.example.com/v1',21,22,23,24,25,NULL,'acct','est',0,1),
 ('codex','flagged-log-cost','s1','','proj-a',1700000000000000002,'gpt-x','acct','https://api.example.com/v1',31,32,33,34,35,1.5,'acct','log',1.5,0);
INSERT INTO settings VALUES('pricing_fingerprint','keep-flags');`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got := map[string]int{}
	rows, err := st.DB().Query("SELECT dedup_key,priced FROM events")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var key string
		var priced int
		if err = rows.Scan(&key, &priced); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		got[key] = priced
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"flag-explicit-zero": 0, "flag-explicit-one": 1, "flagged-log-cost": 0}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("priced[%s]=%d want %d (all %v): migration re-derived stored flags", key, got[key], value, got)
		}
	}
	var fingerprint string
	if err = st.DB().QueryRow("SELECT value FROM settings WHERE key='pricing_fingerprint'").Scan(&fingerprint); err != nil || fingerprint != "keep-flags" {
		t.Fatalf("pricing checkpoint changed: %q %v", fingerprint, err)
	}
}

// An invariant violation while rewriting a genuine legacy file must leave the
// original file exactly as it was. Nothing is deleted or repriced.
func TestLegacyPhysicalMigrationInvariantFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-rollback.db")
	writeLegacyPhysicalDatabase(t, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TRIGGER corrupt_identity_migration AFTER INSERT ON settings WHEN NEW.key='provider.identity.v1' BEGIN UPDATE events SET cost=cost+1; END;`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err == nil {
		st.Close()
		t.Fatal("invariant violation was committed on a legacy index")
	}
	raw := openReadOnly(t, path)
	assertNoIdentityMigrationArtifacts(t, raw)
	assertFactsUnchanged(t, raw)
	// The failure happened before the transaction, so the backup is retained for
	// the operator and the live file was never rewritten.
	if _, statErr := os.Stat(path + ".pre-provider-v1.db"); statErr != nil {
		t.Fatalf("retained backup missing: %v", statErr)
	}
}

// A trigger that fires while the additive columns are initialized is a history
// rewrite, not a legitimate migration step. The baseline must be captured before
// the first UPDATE events SET raw_project=... / SET priced=...; a baseline taken
// after those statements already contains whatever the trigger changed, so the
// invariant would compare the corrupted state with itself and pass.
func TestLegacyPhysicalMigrationDetectsTriggeredInitRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-trigger.db")
	writeLegacyPhysicalDatabase(t, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// The trigger rewrites both an exact historical cost and a token class. It
	// fires on the raw_project and priced initialization UPDATEs, not on the
	// provider identity step, so only a pre-mutation baseline can see it.
	if _, err = db.Exec(`CREATE TRIGGER rewrite_on_init AFTER UPDATE ON events BEGIN UPDATE events SET cost=cost+1,input=input+1; END;`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err == nil {
		st.Close()
		t.Fatal("history rewritten while initializing additive columns was committed")
	}
	if !strings.Contains(err.Error(), "invariant") {
		t.Fatalf("unexpected failure, want an invariant error: %v", err)
	}
	// Open must fail and roll back: the live file keeps the legacy physical table
	// and the original facts.
	raw := openReadOnly(t, path)
	assertNoIdentityMigrationArtifacts(t, raw)
	assertFactsUnchanged(t, raw)
	// The retained pre-migration backup must preserve the original history too.
	backupPath := path + ".pre-provider-v1.db"
	if _, statErr := os.Stat(backupPath); statErr != nil {
		t.Fatalf("retained backup missing: %v", statErr)
	}
	backup := openReadOnly(t, backupPath)
	var kind string
	if err = backup.QueryRow("SELECT type FROM sqlite_master WHERE name='events'").Scan(&kind); err != nil || kind != "table" {
		t.Fatalf("backup is not a legacy snapshot: %q %v", kind, err)
	}
	assertFactsUnchanged(t, backup)
}

// A leftover backup must not block the migration even on the genuine legacy
// schema: the migration runs, the leftover is left byte-for-byte alone, and the
// fresh snapshot is a complete legacy snapshot too.
func TestLegacyPhysicalStepsOverLeftoverBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-nobackup.db")
	writeLegacyPhysicalDatabase(t, path)
	leftover := path + ".pre-provider-v1.db"
	content := []byte("leftover from an interrupted attempt")
	if err := os.WriteFile(leftover, content, 0600); err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256(content)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("a leftover backup must not block a legacy migration: %v", err)
	}
	defer st.Close()

	var kind string
	if err = st.DB().QueryRow("SELECT type FROM sqlite_master WHERE name='provider_endpoints'").Scan(&kind); err != nil || kind != "table" {
		t.Fatalf("identity migration did not run: %q %v", kind, err)
	}
	got, err := os.ReadFile(leftover)
	if err != nil {
		t.Fatalf("the leftover backup disappeared: %v", err)
	}
	if sha256.Sum256(got) != wantHash {
		t.Fatalf("the leftover backup changed:\n%s", got)
	}
	fresh := freshMigrationBackups(t, path, ".pre-provider-v1.db")
	if len(fresh) != 1 {
		t.Fatalf("want exactly one fresh pre-migration backup, got %v", fresh)
	}
	// The fresh artifact preserves the original legacy history.
	backup := openReadOnly(t, fresh[0])
	if err = backup.QueryRow("SELECT type FROM sqlite_master WHERE name='events'").Scan(&kind); err != nil || kind != "table" {
		t.Fatalf("fresh backup is not a legacy snapshot: %q %v", kind, err)
	}
	assertFactsUnchanged(t, backup)
	assertSQLiteIntegrity(t, fresh[0])
}
