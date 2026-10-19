package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/model"
)

// legacyFactSnapshot is the per-request oracle for the legacy physical fixture:
// every raw key, timestamp, token class, cost and priced flag.
var legacyFactSnapshot = []struct {
	key                          string
	in, out, read, write, reason int
	cost, logCost                sql.NullFloat64
	priced                       int
}{
	{"req-est", 21, 22, 23, 24, 25, sql.NullFloat64{Float64: 0.75, Valid: true}, sql.NullFloat64{}, 1},
	{"req-free", 1, 2, 3, 4, 5, sql.NullFloat64{Float64: 0, Valid: true}, sql.NullFloat64{}, 0},
	{"req-log", 11, 12, 13, 14, 15, sql.NullFloat64{Float64: 2.5, Valid: true}, sql.NullFloat64{Float64: 2.5, Valid: true}, 1},
}

// assertLegacyFactsPreserved compares every request, not just the sums, and
// requires the instance dimension to be the reserved default for old input.
func assertLegacyFactsPreserved(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT dedup_key,instance_id,input,output,cache_read,cache_write,reasoning,cost,log_cost,priced FROM events ORDER BY dedup_key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var n int
	for rows.Next() {
		var key, instance string
		var in, out, read, write, reason, priced int
		var cost, logCost sql.NullFloat64
		if err = rows.Scan(&key, &instance, &in, &out, &read, &write, &reason, &cost, &logCost, &priced); err != nil {
			t.Fatal(err)
		}
		if n >= len(legacyFactSnapshot) {
			t.Fatalf("extra fact %q", key)
		}
		want := legacyFactSnapshot[n]
		n++
		if key != want.key || instance != DefaultInstanceID || in != want.in || out != want.out || read != want.read || write != want.write || reason != want.reason || cost != want.cost || logCost != want.logCost || priced != want.priced {
			t.Fatalf("fact %d changed: got (%s,%s,%d,%d,%d,%d,%d,%+v,%+v,%d) want %+v", n, key, instance, in, out, read, write, reason, cost, logCost, priced, want)
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != len(legacyFactSnapshot) {
		t.Fatalf("facts = %d, want %d", n, len(legacyFactSnapshot))
	}
}

func assertInstanceSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range []string{"event_data", "event_dimensions", "sessions", "cursors", "daily_usage", "provider_daily"} {
		var n int
		if err := db.QueryRow("SELECT count(*) FROM pragma_table_info(?) WHERE name='instance_id'", table).Scan(&n); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if n != 1 {
			t.Fatalf("%s has no instance_id column", table)
		}
	}
}

// A genuine legacy disk file must be backed up before the instance dimension is
// introduced, keep every fact under the reserved default instance, and stay
// idempotent on reopen.
func TestInstanceMigrationLegacyDiskBacksUpAndPreservesFacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-instance.db")
	writeLegacyPhysicalDatabase(t, path)

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	backupPath := path + ".pre-instance-v1.db"
	if _, err = os.Stat(backupPath); err != nil {
		t.Fatalf("missing pre-instance backup: %v", err)
	}
	// The backup must be the pre-mutation snapshot, not the migrated index.
	old := openReadOnly(t, backupPath)
	var kind string
	if err = old.QueryRow("SELECT type FROM sqlite_master WHERE name='events'").Scan(&kind); err != nil || kind != "table" {
		t.Fatalf("backup is not a legacy snapshot: %q %v", kind, err)
	}
	var dict int
	if err = old.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='event_data'").Scan(&dict); err != nil || dict != 0 {
		t.Fatalf("backup contains the instance schema: %d %v", dict, err)
	}
	assertFactsUnchanged(t, old)

	assertInstanceSchema(t, st.DB())
	assertLegacyFactsPreserved(t, st.DB())
	if marker, err := st.Setting(context.Background(), instanceVersionKey); err != nil || marker != "1" {
		t.Fatalf("instance marker = %q err=%v", marker, err)
	}
	if n := count(t, st, `SELECT count(*) FROM sessions WHERE instance_id<>?`, DefaultInstanceID); n != 0 {
		t.Fatalf("%d sessions left outside the default instance", n)
	}
	if n := count(t, st, `SELECT count(*) FROM event_data WHERE instance_id<>?`, DefaultInstanceID); n != 0 {
		t.Fatalf("%d facts left outside the default instance", n)
	}
	// Derived rollups must be split by instance too, even with one instance.
	for _, table := range []string{"daily_usage", "provider_daily"} {
		if n := count(t, st, `SELECT count(*) FROM `+table+` WHERE instance_id<>?`, DefaultInstanceID); n != 0 {
			t.Fatalf("%s rows outside the default instance: %d", table, n)
		}
	}
	// Reopening a completed migration must not reserve the backup again.
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("idempotent reopen: %v", err)
	}
	reopened.Close()
}

// A brand new database has no history to protect, so it must not leave a disk
// backup behind.
func TestInstanceMigrationSkipsBackupOnFreshDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fresh.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := os.Stat(path + ".pre-instance-v1.db"); !os.IsNotExist(err) {
		t.Fatalf("fresh database wrote a backup: %v", err)
	}
	assertInstanceSchema(t, st.DB())

	mem, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	assertInstanceSchema(t, mem.DB())
}

// A marker without the schema, an unknown marker, and an instance column without
// a marker are all partial states. Each must fail loudly instead of guessing.
func TestInstanceMigrationRejectsPartialState(t *testing.T) {
	t.Run("marker-without-column", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "partial.db")
		writeLegacyPhysicalDatabase(t, path)
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO settings VALUES('` + instanceVersionKey + `','1')`); err != nil {
			db.Close()
			t.Fatal(err)
		}
		db.Close()
		st, err := Open(path)
		if err == nil {
			st.Close()
			t.Fatal("marker without the instance schema was accepted")
		}
		if !strings.Contains(err.Error(), "instance") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("unknown-marker", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "unknown.db")
		writeLegacyPhysicalDatabase(t, path)
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO settings VALUES('` + instanceVersionKey + `','7')`); err != nil {
			db.Close()
			t.Fatal(err)
		}
		db.Close()
		if st, err := Open(path); err == nil {
			st.Close()
			t.Fatal("unknown instance marker value was accepted")
		}
	})
	t.Run("column-without-marker", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "unmarked.db")
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		st.Close()
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`DELETE FROM settings WHERE key='` + instanceVersionKey + `'`); err != nil {
			db.Close()
			t.Fatal(err)
		}
		db.Close()
		if st, err := Open(path); err == nil {
			st.Close()
			t.Fatal("instance schema without its marker was accepted")
		}
	})
}

// A fault raised while the instance rebuild is in flight must abort Open and
// leave the on-disk database exactly as it was: legacy schema, original facts,
// no marker, no instance column.
//
// The fault is injected at the marker stage — the moment the migration commits
// to rebuilding. The trigger fires on the INSERT of the instance marker itself
// and rewrites one fact's cost in event_data, so the migration's own per-request
// digest, not a SQL resolution error, is what rejects it: SQLite accepts a body
// naming a table created later in the same transaction, and by the marker insert
// migrateCompact has already built event_data.
//
// The trigger deliberately adopts the name of one of the twelve rollup
// maintenance triggers that migrateInstance drops immediately after that marker
// insert. The name is what keeps the injection honest: SQLite re-parses every
// trigger body on ALTER TABLE ... RENAME TO, which is how the rebuild swaps each
// re-keyed table, so an injected trigger under an arbitrary name that names
// event_data aborts the rebuild with "error in trigger ...: no such table:
// main.event_data" before the digest is ever compared (an older fixture failed
// this way), and a trigger named daily_insert is dropped earlier still by
// migrateCompact. Adopting a name that the rebuild itself removes lets the
// rewrite survive into the rebuilt table, which is exactly what the invariant
// must catch.
func TestInstanceMigrationFaultRollsBackFactsAndDDL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance-rollback.db")
	writeLegacyPhysicalDatabase(t, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// The trigger corrupts history while the instance rebuild is in flight. The
	// whole transaction must roll back.
	if _, err = db.Exec(`CREATE TRIGGER provider_cache_delete AFTER INSERT ON settings WHEN NEW.key='` + instanceVersionKey + `' BEGIN UPDATE event_data SET cost=cost+1 WHERE dedup_key='req-log'; END;`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err == nil {
		st.Close()
		t.Fatal("instance migration committed a rewritten history")
	}
	if !strings.Contains(err.Error(), "instance migration history invariant failed") {
		t.Fatalf("instance fault error = %v; want the per-request history invariant to reject it", err)
	}
	// The rewrite changed a value, not the number of facts, so the digest must
	// report the same count on both sides.
	if !strings.Contains(err.Error(), "(3 facts before, 3 after)") {
		t.Fatalf("instance fault error = %v; want the same fact count on both sides of the digest", err)
	}
	raw := openReadOnly(t, path)
	assertNoIdentityMigrationArtifacts(t, raw)
	assertFactsUnchanged(t, raw)
	// No instance marker and no instance column survived the rollback.
	var n int
	if err = raw.QueryRow(`SELECT count(*) FROM settings WHERE key=?`, instanceVersionKey).Scan(&n); err != nil || n != 0 {
		t.Fatalf("instance marker survived rollback: %d %v", n, err)
	}
	if err = raw.QueryRow(`SELECT count(*) FROM pragma_table_info('events') WHERE name='instance_id'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("instance column survived rollback: %d %v", n, err)
	}
	if _, statErr := os.Stat(path + ".pre-instance-v1.db"); statErr != nil {
		t.Fatalf("retained backup missing: %v", statErr)
	}
}

// The invariant oracle is per request: every raw key, timestamp, token class,
// cost, log cost and priced flag participates, so a rewrite that keeps the sums
// identical is still detected. Compare against groups collapsed by SUM.
// instance_id is deliberately absent from the digest because the before
// snapshot is taken while the legacy table has no such column; the assignment
// of old rows to the reserved default is asserted separately.
func TestInstanceFactStampsIsPerRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance-stamps.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	commitInstance(t, st, "", "log", "k-a", "s1", "", 10)
	commitInstance(t, st, "alpha", "log", "k-b", "s1", "", 20)

	stamp := func() ([32]byte, int64) {
		t.Helper()
		tx, err := st.DB().Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		h, n, err := instanceFactStamps(tx)
		if err != nil {
			t.Fatal(err)
		}
		return h, n
	}
	prev, prevCount := stamp()
	if prevCount != 2 {
		t.Fatalf("facts = %d, want 2", prevCount)
	}
	if again, againCount := stamp(); again != prev || againCount != prevCount {
		t.Fatal("instanceFactStamps is not stable for unchanged facts")
	}
	for _, m := range []struct {
		name string
		sql  string
	}{
		{"token class", `UPDATE event_data SET input=input+1 WHERE harness='codex' AND instance_id='default' AND dedup_key='k-a'`},
		{"timestamp", `UPDATE event_data SET timestamp=timestamp+1 WHERE harness='codex' AND instance_id='default' AND dedup_key='k-a'`},
		{"cost", `UPDATE event_data SET cost=cost+1 WHERE harness='codex' AND instance_id='default' AND dedup_key='k-a'`},
		{"log cost", `UPDATE event_data SET log_cost=1 WHERE harness='codex' AND instance_id='default' AND dedup_key='k-a'`},
		{"priced flag", `UPDATE event_data SET priced=1-priced WHERE harness='codex' AND instance_id='default' AND dedup_key='k-a'`},
		{"raw dedup key", `UPDATE event_data SET dedup_key='k-c' WHERE harness='codex' AND instance_id='default' AND dedup_key='k-a'`},
	} {
		if _, err := st.DB().Exec(m.sql); err != nil {
			t.Fatalf("%s: %v", m.name, err)
		}
		got, gotCount := stamp()
		if got == prev {
			t.Fatalf("%s rewrite did not change the fact digest", m.name)
		}
		prev, prevCount = got, gotCount
	}
	if _, err := st.DB().Exec(`DELETE FROM event_data WHERE harness='codex' AND instance_id='alpha' AND dedup_key='k-b'`); err != nil {
		t.Fatal(err)
	}
	if got, gotCount := stamp(); got == prev || gotCount != prevCount-1 {
		t.Fatalf("dropping one request did not change the fact digest (count %d)", gotCount)
	}
}

// A leftover instance backup from an interrupted attempt must not block the
// migration: it is never overwritten, reused or deleted, and the new snapshot
// goes to the next free, timestamped name.
func TestInstanceMigrationStepsOverLeftoverBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance-nobackup.db")
	writeLegacyPhysicalDatabase(t, path)
	leftover := path + ".pre-instance-v1.db"
	content := []byte("leftover from an interrupted attempt")
	if err := os.WriteFile(leftover, content, 0600); err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256(content)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("a leftover backup must not block the instance migration: %v", err)
	}
	defer st.Close()

	// The migration ran to completion.
	if marker, err := st.Setting(context.Background(), instanceVersionKey); err != nil || marker != "1" {
		t.Fatalf("instance marker: %q %v", marker, err)
	}
	// The leftover is byte-for-byte unchanged.
	got, err := os.ReadFile(leftover)
	if err != nil {
		t.Fatalf("the leftover backup disappeared: %v", err)
	}
	if sha256.Sum256(got) != wantHash {
		t.Fatalf("the leftover backup changed:\n%s", got)
	}
	fresh := freshMigrationBackups(t, path, ".pre-instance-v1.db")
	if len(fresh) != 1 {
		t.Fatalf("want exactly one fresh instance backup, got %v", fresh)
	}
	assertSQLiteIntegrity(t, fresh[0])
}

// After the migration, updates, deletes and reprice passes must keep every
// rollup in step with the facts, per instance.
func TestInstanceRollupParityAfterUpdateDeleteAndReprice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "parity.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	commitInstance(t, st, "alpha", "log", "k1", "s1", "", 10)
	commitInstance(t, st, "beta", "log", "k2", "s1", "", 20)
	commitInstance(t, st, "beta", "log", "k3", "s1", "", 30)

	if _, err = st.DB().Exec(`UPDATE events SET input=100 WHERE dedup_key='k2'`); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`DELETE FROM event_data WHERE instance_id='alpha'`); err != nil {
		t.Fatal(err)
	}
	if err = st.RecomputeCosts(context.Background(), func(e model.UsageEvent) float64 { return 1 }); err != nil {
		t.Fatal(err)
	}
	assertAllRollupsMatchFacts(t, st)
}

// A timezone change rebuilds all four rollups; the rebuild must not collapse
// instances or lose the partial-day attribution.
func TestInstanceEnsureLocalDaysKeepsInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zone.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	commitInstance(t, st, "alpha", "log", "k1", "s1", "", 10)
	commitInstance(t, st, "beta", "log", "k2", "s1", "", 20)
	if err = st.SetSetting(context.Background(), "daily_timezone", "stale-zone"); err != nil {
		t.Fatal(err)
	}
	if err = st.EnsureLocalDays(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertAllRollupsMatchFacts(t, st)
	var zones int
	if err = st.DB().QueryRow(`SELECT count(*) FROM daily_usage WHERE instance_id='alpha'`).Scan(&zones); err != nil || zones != 1 {
		t.Fatalf("alpha daily rows after zone rebuild = %d err=%v", zones, err)
	}
}

// assertAllRollupsMatchFacts diffs every derived rollup against an independent
// GROUP BY over the events view, in both directions, so a rollup can neither
// invent a row nor drop one.
func assertAllRollupsMatchFacts(t *testing.T, st *Store) {
	t.Helper()
	pairs := []struct{ name, table, oracle string }{
		{
			"daily_usage",
			`SELECT day,harness,instance_id,session_id,resolved_provider,model,project,attrib,input,output,cache_read,cache_write,reasoning,cost,cost_log,cost_estimate,requests,unpriced FROM daily_usage`,
			`SELECT mytoken_day(timestamp),harness,instance_id,session_id,resolved_provider,model,project,attrib,sum(input),sum(output),sum(cache_read),sum(cache_write),sum(reasoning),sum(CASE WHEN priced THEN cost ELSE 0 END),sum(CASE WHEN log_cost IS NOT NULL THEN cost ELSE 0 END),sum(CASE WHEN log_cost IS NULL AND priced THEN cost ELSE 0 END),count(*),sum(NOT priced) FROM events GROUP BY 1,2,3,4,5,6,7,8`,
		},
		{
			"provider_daily",
			`SELECT day,harness,instance_id,session_id,resolved_provider,model,project,base_url,attrib,input,output,cache_read,cache_write,reasoning,cost,cost_log,cost_estimate,requests,unpriced FROM provider_daily`,
			`SELECT mytoken_day(timestamp),harness,instance_id,session_id,resolved_provider,model,project,base_url,attrib,sum(input),sum(output),sum(cache_read),sum(cache_write),sum(reasoning),sum(CASE WHEN priced THEN cost ELSE 0 END),sum(CASE WHEN log_cost IS NOT NULL THEN cost ELSE 0 END),sum(CASE WHEN log_cost IS NULL AND priced THEN cost ELSE 0 END),count(*),sum(NOT priced) FROM events GROUP BY 1,2,3,4,5,6,7,8,9`,
		},
	}
	for _, p := range pairs {
		for _, q := range []string{
			"SELECT count(*) FROM (" + p.table + " EXCEPT " + p.oracle + ")",
			"SELECT count(*) FROM (" + p.oracle + " EXCEPT " + p.table + ")",
		} {
			var n int
			if err := st.DB().QueryRow(q).Scan(&n); err != nil {
				t.Fatalf("%s parity: %v", p.name, err)
			}
			if n != 0 {
				t.Fatalf("%s diverges from the facts by %d rows", p.name, n)
			}
		}
	}
}
