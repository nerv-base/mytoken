package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the instance preflight to the *whole* shape the marker claims,
// not just the presence of an instance_id column. Each fixture starts from a real
// migrated index, damages exactly one clause of one object with the same DDL text
// the instance migration itself would have written, and requires Open to refuse
// before anything is rewritten.
//
// A malformed instance schema that carries every instance_id column but keeps a
// legacy primary key, drops the dimension unique key, substitutes a view for a
// table (or a table for the events view), or loses the instance-aware maintenance
// triggers is indistinguishable from a complete migration by column existence
// alone, so a column-only preflight accepts it and then serves or rewrites a
// schema nobody built.

// migratedIndex returns the path of a real legacy index that has been opened once,
// so it carries the full instance schema and the instance marker.
func migratedIndex(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.db")
	writeLegacyPhysicalDatabase(t, path)
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open legacy index: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close migrated index: %v", err)
	}
	return path
}

// rawIndex opens the migrated index for direct schema surgery. The pool is pinned
// to a single connection so PRAGMA legacy_alter_table survives across the
// statements of one table swap.
func rawIndex(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw index: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func execAll(t *testing.T, db *sql.DB, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("exec %q: %v", statement, err)
		}
	}
}

func tableColumnList(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	rows, err := db.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		t.Fatalf("read columns of %s: %v", table, err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column of %s: %v", table, err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read columns of %s: %v", table, err)
	}
	return strings.Join(columns, ",")
}

// rebuildTable rewrites one stored table definition with a single clause changed
// and copies every row. legacy_alter_table keeps the final rename from re-parsing
// (and rejecting) the maintenance triggers that still point at the old name,
// which is exactly the state a half-finished rebuild leaves behind.
func rebuildTable(t *testing.T, db *sql.DB, table, old, replacement string) {
	t.Helper()
	var ddl string
	if err := db.QueryRow("SELECT sql FROM sqlite_master WHERE name=?", table).Scan(&ddl); err != nil {
		t.Fatalf("read DDL of %s: %v", table, err)
	}
	if !strings.Contains(ddl, old) {
		t.Fatalf("DDL of %s does not contain %q:\n%s", table, old, ddl)
	}
	mutated := strings.Replace(ddl, old, replacement, 1)
	head := strings.Index(mutated, "CREATE TABLE")
	if head < 0 {
		t.Fatalf("DDL of %s is not a CREATE TABLE:\n%s", table, ddl)
	}
	at := head + strings.Index(mutated[head:], table)
	if at < head {
		t.Fatalf("DDL of %s does not name the table right after CREATE TABLE:\n%s", table, ddl)
	}
	tmp := table + "__preflight"
	mutated = mutated[:at] + tmp + mutated[at+len(table):]
	columns := tableColumnList(t, db, table)
	execAll(t, db,
		"PRAGMA legacy_alter_table=ON",
		mutated,
		"INSERT INTO "+tmp+"("+columns+") SELECT "+columns+" FROM "+table,
		"DROP TABLE "+table,
		"ALTER TABLE "+tmp+" RENAME TO "+table,
	)
}

// substituteView replaces a table with a view over a renamed backing table, so
// the object keeps every column (including instance_id) but is no longer writable
// as a table. A preflight that only asks sqlite_master for "a table or a view"
// accepts it.
func substituteView(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	execAll(t, db,
		"PRAGMA legacy_alter_table=ON",
		"ALTER TABLE "+table+" RENAME TO "+table+"__backing",
		"CREATE VIEW "+table+" AS SELECT * FROM "+table+"__backing",
	)
}

// indexFacts is the invariant a rejected Open must not disturb: the request count
// and the input/cost totals of the history that is already on disk. Two fixtures
// damage the events view itself (a view substituted for event_data, a table
// substituted for the events view), so an unreadable read surface is reported as
// the stable observable instead of failing the fixture.
func indexFacts(t *testing.T, path string) string {
	t.Helper()
	db := openReadOnly(t, path)
	var requests, input int64
	var cost float64
	err := db.QueryRow(`SELECT count(*),coalesce(sum(input),0),coalesce(sum(cost),0) FROM events`).Scan(&requests, &input, &cost)
	if err != nil {
		return "unreadable: " + err.Error()
	}
	return fmt.Sprintf("requests=%d input=%d cost=%g", requests, input, cost)
}

func assertInstancePreflightRejects(t *testing.T, path string, wants ...string) {
	t.Helper()
	before := indexFacts(t, path)
	st, err := Open(path)
	if err == nil {
		st.Close()
		t.Fatalf("Open accepted a malformed instance schema; want rejection mentioning %v", wants)
	}
	if !strings.Contains(err.Error(), "instance schema is inconsistent") {
		t.Fatalf("Open error = %v; want an instance schema inconsistency", err)
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Open error = %v; want it to mention %q", err, want)
		}
	}
	if after := indexFacts(t, path); after != before {
		t.Fatalf("Open rejection rewrote history: %s -> %s", before, after)
	}
}

// legacyClause turns an instance key clause into the pre-instance one the same
// table carried before the migration.
func legacyClause(clause string) string {
	return strings.Replace(clause, "instance_id,", "", 1)
}

func TestInstancePreflightRejectsLegacyKeys(t *testing.T) {
	cases := []struct{ name, table, clause, want string }{
		{"event-data", "event_data", "PRIMARY KEY(harness,instance_id,dedup_key)", "event_data"},
		{"sessions", "sessions", "PRIMARY KEY(harness,instance_id,session_id)", "sessions"},
		{"cursors", "cursors", "PRIMARY KEY(harness,instance_id,path)", "cursors"},
		{"daily-usage", "daily_usage", "PRIMARY KEY(day,harness,instance_id,", "daily_usage"},
		{"cache-daily", "cache_daily", "PRIMARY KEY(day,harness,instance_id,", "cache_daily"},
		{"provider-daily", "provider_daily", "PRIMARY KEY(day,harness,instance_id,", "provider_daily"},
		{"provider-cache-daily", "provider_cache_daily", "PRIMARY KEY(day,harness,instance_id,", "provider_cache_daily"},
		{"event-dimensions-unique", "event_dimensions", "UNIQUE(harness,instance_id,", "event_dimensions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := migratedIndex(t)
			db := rawIndex(t, path)
			rebuildTable(t, db, tc.table, tc.clause, legacyClause(tc.clause))
			db.Close()
			assertInstancePreflightRejects(t, path, tc.want, "instance_id")
		})
	}
}

func TestInstancePreflightRejectsViewSubstitutions(t *testing.T) {
	cases := []struct {
		name  string
		want  string
		apply func(t *testing.T, db *sql.DB)
	}{
		{
			name: "event-data-is-a-view",
			want: "event_data",
			apply: func(t *testing.T, db *sql.DB) {
				substituteView(t, db, "event_data")
			},
		},
		{
			name: "daily-usage-is-a-view",
			want: "daily_usage",
			apply: func(t *testing.T, db *sql.DB) {
				substituteView(t, db, "daily_usage")
			},
		},
		{
			name: "events-is-not-a-view",
			want: "events",
			apply: func(t *testing.T, db *sql.DB) {
				execAll(t, db,
					"PRAGMA legacy_alter_table=ON",
					"DROP VIEW events",
					"CREATE TABLE events(id INTEGER PRIMARY KEY,harness TEXT NOT NULL,instance_id TEXT NOT NULL,dedup_key TEXT NOT NULL)",
				)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := migratedIndex(t)
			db := rawIndex(t, path)
			tc.apply(t, db)
			db.Close()
			assertInstancePreflightRejects(t, path, tc.want)
		})
	}
}

func TestInstancePreflightRejectsNonInstanceEventsView(t *testing.T) {
	path := migratedIndex(t)
	db := rawIndex(t, path)
	execAll(t, db,
		"PRAGMA legacy_alter_table=ON",
		"DROP VIEW events",
		legacyEventsViewHead+eventsViewCacheTail+eventsViewJoin,
	)
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := createEventsTriggers(tx, instanceRollupSchema); err != nil {
		tx.Rollback()
		t.Fatalf("recreate the instance write triggers over the legacy view: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	db.Close()
	assertInstancePreflightRejects(t, path, "events", "instance_id")
}

func TestInstancePreflightRejectsMissingMaintenanceTriggers(t *testing.T) {
	for _, trigger := range []string{"daily_delete", "cache_update", "provider_daily_insert", "provider_cache_delete"} {
		t.Run(trigger, func(t *testing.T) {
			path := migratedIndex(t)
			db := rawIndex(t, path)
			execAll(t, db, "DROP TRIGGER "+trigger)
			db.Close()
			assertInstancePreflightRejects(t, path, trigger)
		})
	}
}

func TestInstancePreflightRejectsNonInstanceMaintenanceTriggers(t *testing.T) {
	path := migratedIndex(t)
	db := rawIndex(t, path)
	execAll(t, db, "DROP TRIGGER daily_insert", "DROP TRIGGER daily_update", "DROP TRIGGER daily_delete")
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := installRollupTriggers(tx, "daily", "daily_usage", legacyDailyDimensions, dailyMetrics, legacyDimensionColumns, func(prefix string) string {
		return dailyValues(prefix, legacyDailyDimensions)
	}, nil); err != nil {
		tx.Rollback()
		t.Fatalf("install legacy daily triggers: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	db.Close()
	assertInstancePreflightRejects(t, path, "daily_insert", "instance")
}

func TestInstancePreflightRejectsMissingEventsWriteTriggers(t *testing.T) {
	for _, trigger := range []string{"events_insert", "events_update", "events_delete"} {
		t.Run(trigger, func(t *testing.T) {
			path := migratedIndex(t)
			db := rawIndex(t, path)
			execAll(t, db, "DROP TRIGGER "+trigger)
			db.Close()
			assertInstancePreflightRejects(t, path, trigger)
		})
	}
}

// TestInstancePreflightAllowsProviderRollupRebuild is the guard against blanket
// rejection: when the provider rollup marker is absent and only its two tables
// and triggers are gone, that is the documented recoverable shape the rollup
// migration rebuilds, so Open must succeed and produce complete instance-aware
// rollups again.
func TestInstancePreflightAllowsProviderRollupRebuild(t *testing.T) {
	path := migratedIndex(t)
	db := rawIndex(t, path)
	execAll(t, db,
		"DROP TRIGGER provider_daily_insert", "DROP TRIGGER provider_daily_update", "DROP TRIGGER provider_daily_delete",
		"DROP TRIGGER provider_cache_insert", "DROP TRIGGER provider_cache_update", "DROP TRIGGER provider_cache_delete",
		"DROP TABLE provider_daily", "DROP TABLE provider_cache_daily",
		"DELETE FROM settings WHERE key='"+providerRollupVersion+"'",
	)
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open rejected a legitimate provider rollup rebuild: %v", err)
	}
	defer st.Close()

	if got, err := st.Setting(context.Background(), providerRollupVersion); err != nil || got != "1" {
		t.Fatalf("provider rollup marker = %q (err %v); want \"1\"", got, err)
	}
	if _, err := os.Stat(path + ".pre-provider-endpoint-rollup-v1.db"); err != nil {
		t.Fatalf("provider rollup rebuild did not back up before rebuilding: %v", err)
	}
	assertFactsUnchanged(t, openReadOnly(t, path))
	assertAllRollupsMatchFacts(t, st)

	commitInstance(t, st, "alpha", "log", "preflight-alpha", "s1", "", 7)
	assertAllRollupsMatchFacts(t, st)
	if got := count(t, st, `SELECT sum(input) FROM provider_daily WHERE instance_id='alpha'`); got != 7 {
		t.Fatalf("provider_daily input for instance alpha = %d; want 7", got)
	}
}
