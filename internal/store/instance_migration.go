package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"
)

// instanceVersionKey marks the schema generation that carries the instance
// dimension. It is deliberately its own marker: the migration rebuilds primary
// keys, so its preconditions (every base table present, provider rollups built)
// are stricter than the identity or rollup migrations, and a partially applied
// state must never be mistaken for a complete one.
const instanceVersionKey = "instance_schema_version"
const instanceSchemaVersion = "instance.v1"

// instanceTables are the base and derived tables this migration itself owns and
// that must all carry the instance dimension once the marker is set. The two
// provider rollups are deliberately excluded: they are governed by their own
// marker (providerRollupVersion), which is what rejects a half-applied endpoint
// rollup, so dropping just those two tables is a recoverable shape that the
// provider rollup migration rebuilds rather than a corrupt instance schema.
var instanceTables = []string{"event_data", "event_dimensions", "sessions", "cursors", "daily_usage", "cache_daily"}

// The canonical instance-aware DDL. The base migrations are reproduced byte for
// byte with instance_id added directly after harness (or after dedup_key for a
// fact table), so a migrated index differs from a pre-migration one only by the
// instance dimension and its primary keys.
const instanceEventDimensionsDDL = `CREATE TABLE event_dimensions__instance(
 id INTEGER PRIMARY KEY,harness TEXT NOT NULL,instance_id TEXT NOT NULL,session_id TEXT NOT NULL,parent_id TEXT NOT NULL,project TEXT NOT NULL,
 model TEXT NOT NULL,provider TEXT NOT NULL,base_url TEXT NOT NULL,resolved_provider TEXT NOT NULL,attrib TEXT NOT NULL,raw_project TEXT NOT NULL,
 UNIQUE(` + dimensionColumns + `))`

const instanceEventDataDDL = `CREATE TABLE event_data__instance(
 harness TEXT NOT NULL,instance_id TEXT NOT NULL DEFAULT '` + DefaultInstanceID + `',dedup_key TEXT NOT NULL,dimension_id INTEGER NOT NULL REFERENCES event_dimensions(id),
 timestamp INTEGER NOT NULL,input INTEGER NOT NULL,output INTEGER NOT NULL,cache_read INTEGER NOT NULL,cache_write INTEGER NOT NULL,reasoning INTEGER NOT NULL,
 log_cost REAL,cost REAL NOT NULL,priced INTEGER NOT NULL,
 request_id TEXT NOT NULL DEFAULT '',boundary TEXT NOT NULL DEFAULT '',bill_amount REAL,bill_unit TEXT NOT NULL DEFAULT '',
 cache_expected INTEGER NOT NULL DEFAULT 0,cache_miss INTEGER NOT NULL DEFAULT 0,miss_cause TEXT NOT NULL DEFAULT '',miss_cost REAL NOT NULL DEFAULT 0,
 PRIMARY KEY(harness,instance_id,dedup_key))`

const instanceSessionsDDL = `CREATE TABLE sessions__instance(
 harness TEXT NOT NULL,instance_id TEXT NOT NULL DEFAULT '` + DefaultInstanceID + `',session_id TEXT NOT NULL,parent_id TEXT NOT NULL DEFAULT '',
 title TEXT NOT NULL DEFAULT '',project TEXT NOT NULL DEFAULT '',raw_project TEXT NOT NULL DEFAULT '',
 started_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,PRIMARY KEY(harness,instance_id,session_id))`

const instanceCursorsDDL = `CREATE TABLE cursors__instance(
 harness TEXT NOT NULL,instance_id TEXT NOT NULL DEFAULT '` + DefaultInstanceID + `',path TEXT NOT NULL,cursor TEXT NOT NULL,PRIMARY KEY(harness,instance_id,path))`

var instanceIndexes = []string{
	`CREATE INDEX events_time ON event_data(timestamp,output,input,cache_read,cache_write)`,
	`CREATE INDEX events_session ON event_data(dimension_id,timestamp)`,
	`CREATE INDEX dimensions_session ON event_dimensions(harness,instance_id,session_id)`,
	`CREATE INDEX dimensions_raw_project ON event_dimensions(raw_project,project)`,
	`CREATE INDEX sessions_parent ON sessions(harness,instance_id,parent_id)`,
	`CREATE INDEX sessions_raw_project ON sessions(raw_project)`,
}

func instanceSchemaInconsistent(detail string) error {
	return fmt.Errorf("instance schema is inconsistent: %s; refusing to guess, to reprice, or to skip the migration. Nothing was rewritten and no backup was replaced. Recovery needs an explicit operator decision", detail)
}

// objectKind reports the sqlite_master type of a named object, or "" when no
// object carries that name. A view is not a table: the preflight asks this
// instead of counting "table or view", because that older probe could not tell a
// rebuilt table from a view substituted for it.
func (s *Store) objectKind(ctx context.Context, name string) (string, error) {
	var kind string
	err := s.db.QueryRowContext(ctx, "SELECT type FROM sqlite_master WHERE name=?", name).Scan(&kind)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return kind, nil
}

// primaryKeyColumns returns the declared primary key columns in key order.
func (s *Store) primaryKeyColumns(ctx context.Context, table string) (string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?) WHERE pk>0 ORDER BY pk", table)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", err
		}
		columns = append(columns, name)
	}
	return strings.Join(columns, ","), rows.Err()
}

// hasUniqueKeyOn reports whether the table carries a UNIQUE constraint or unique
// index (other than its primary key) over the given column.
func (s *Store) hasUniqueKeyOn(ctx context.Context, table, column string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM pragma_index_list(?) WHERE "unique"=1 AND origin<>'pk'`, table)
	if err != nil {
		return false, err
	}
	var indexes []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return false, err
		}
		indexes = append(indexes, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, index := range indexes {
		columns, err := s.indexColumns(ctx, index)
		if err != nil {
			return false, err
		}
		for _, name := range columns {
			if name == column {
				return true, nil
			}
		}
	}
	return false, nil
}

func (s *Store) indexColumns(ctx context.Context, index string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM pragma_index_info(?) ORDER BY seqno", index)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, rows.Err()
}

// triggerSQL returns the stored definition of a trigger, or "" when it is absent.
func (s *Store) triggerSQL(ctx context.Context, name string) (string, error) {
	var ddl string
	err := s.db.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?", name).Scan(&ddl)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return ddl, nil
}

// instanceRollupTriggers maps each derived rollup to the prefix of the three
// maintenance triggers that keep it in step with event_data.
var instanceRollupTriggers = []struct{ prefix, table string }{
	{"daily", "daily_usage"},
	{"cache", "cache_daily"},
	{"provider_daily", "provider_daily"},
	{"provider_cache", "provider_cache_daily"},
}

func (s *Store) hasInstanceColumn(ctx context.Context, table string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info(?) WHERE name='instance_id'", table).Scan(&n)
	return n == 1, err
}

// checkInstanceSchema rejects a partially applied instance migration before any
// work starts: a marker without the column would surface later as a raw SQL
// error, and an instance column without the marker cannot be trusted to be
// complete (the primary keys may never have been rebuilt). With the marker set,
// the whole rebuilt shape is verified, not just the presence of the columns.
func (s *Store) checkInstanceSchema(ctx context.Context, marker string) error {
	switch marker {
	case "":
		for _, table := range instanceTables {
			kind, err := s.objectKind(ctx, table)
			if err != nil {
				return err
			}
			if kind == "" {
				continue
			}
			if kind != "table" {
				return instanceSchemaInconsistent(table + " is a " + kind + " but the marker " + instanceSchemaVersion + " is absent")
			}
			has, err := s.hasInstanceColumn(ctx, table)
			if err != nil {
				return err
			}
			if has {
				return instanceSchemaInconsistent(table + " carries the instance dimension but the marker " + instanceSchemaVersion + " is absent")
			}
		}
	case "1":
		return s.checkInstanceSchemaComplete(ctx)
	default:
		return instanceSchemaInconsistent(fmt.Sprintf("the marker %s has unexpected value %q", instanceSchemaVersion, marker))
	}
	return nil
}

// checkInstanceSchemaComplete verifies the whole shape the instance marker
// claims. The migration rebuilds primary keys, the dimension unique key and the
// maintenance triggers, so an index that carries every instance_id column but
// keeps a legacy key, drops the dimension unique key, substitutes a view for a
// table (or a table for the events view), or has lost its instance-aware triggers
// describes a rebuild that never finished and must not be served or rewritten.
//
// An absent rollup table is not a defect here: the provider rollups have their
// own marker and their own migration, and the instance migration drops and
// recreates all four derived tables. Requiring them unconditionally would reject
// the documented recoverable shape (marker absent, tables gone) that the rollup
// migration rebuilds.
func (s *Store) checkInstanceSchemaComplete(ctx context.Context) error {
	for _, table := range instanceTables {
		kind, err := s.objectKind(ctx, table)
		if err != nil {
			return err
		}
		if kind == "" {
			return instanceSchemaInconsistent("the marker " + instanceSchemaVersion + " is set but table " + table + " is missing")
		}
		if kind != "table" {
			return instanceSchemaInconsistent("the marker " + instanceSchemaVersion + " is set but " + table + " is a " + kind + ", not a table")
		}
		has, err := s.hasInstanceColumn(ctx, table)
		if err != nil {
			return err
		}
		if !has {
			return instanceSchemaInconsistent("the marker " + instanceSchemaVersion + " is set but " + table + " has no instance_id column")
		}
	}
	for _, table := range []string{"event_data", "sessions", "cursors", "daily_usage", "cache_daily"} {
		if err := s.requireInstanceKey(ctx, table); err != nil {
			return err
		}
	}
	unique, err := s.hasUniqueKeyOn(ctx, "event_dimensions", "instance_id")
	if err != nil {
		return err
	}
	if !unique {
		return instanceSchemaInconsistent("the marker " + instanceSchemaVersion + " is set but event_dimensions has no unique key on instance_id")
	}
	kind, err := s.objectKind(ctx, "events")
	if err != nil {
		return err
	}
	if kind != "view" {
		return instanceSchemaInconsistent("the marker " + instanceSchemaVersion + " is set but the events read surface is " + describeObject(kind) + ", not a view")
	}
	has, err := s.hasInstanceColumn(ctx, "events")
	if err != nil {
		return err
	}
	if !has {
		return instanceSchemaInconsistent("the marker " + instanceSchemaVersion + " is set but the events view does not expose instance_id")
	}
	for _, group := range instanceRollupTriggers {
		kind, err := s.objectKind(ctx, group.table)
		if err != nil {
			return err
		}
		if kind == "" {
			continue
		}
		if kind != "table" {
			return instanceSchemaInconsistent("the marker " + instanceSchemaVersion + " is set but " + group.table + " is a " + kind + ", not a table")
		}
		if err := s.requireInstanceKey(ctx, group.table); err != nil {
			return err
		}
		for _, suffix := range []string{"insert", "update", "delete"} {
			name := group.prefix + "_" + suffix
			ddl, err := s.triggerSQL(ctx, name)
			if err != nil {
				return err
			}
			if ddl == "" {
				return instanceSchemaInconsistent("the marker " + instanceSchemaVersion + " is set but maintenance trigger " + name + " is missing")
			}
			if !strings.Contains(ddl, "instance_id") {
				return instanceSchemaInconsistent("the marker " + instanceSchemaVersion + " is set but maintenance trigger " + name + " does not maintain the instance dimension")
			}
		}
	}
	// The write triggers decode the compatibility view back into event_data.
	// events_delete only removes a row by rowid and carries no instance
	// reference, so only its existence is required.
	for _, name := range []string{"events_insert", "events_update"} {
		ddl, err := s.triggerSQL(ctx, name)
		if err != nil {
			return err
		}
		if ddl == "" {
			return instanceSchemaInconsistent("the marker " + instanceSchemaVersion + " is set but the events write trigger " + name + " is missing")
		}
		if !strings.Contains(ddl, "NEW.instance_id") {
			return instanceSchemaInconsistent("the marker " + instanceSchemaVersion + " is set but the events write trigger " + name + " does not write instance_id")
		}
	}
	if ddl, err := s.triggerSQL(ctx, "events_delete"); err != nil {
		return err
	} else if ddl == "" {
		return instanceSchemaInconsistent("the marker " + instanceSchemaVersion + " is set but the events write trigger events_delete is missing")
	}
	return nil
}

// requireInstanceKey rejects a table whose primary key does not include the
// instance dimension, which is what lets equal dedup keys coexist per instance.
func (s *Store) requireInstanceKey(ctx context.Context, table string) error {
	key, err := s.primaryKeyColumns(ctx, table)
	if err != nil {
		return err
	}
	if !strings.Contains(","+key+",", ",instance_id,") {
		return instanceSchemaInconsistent("the marker " + instanceSchemaVersion + " is set but " + table + " does not key on instance_id (primary key " + key + ")")
	}
	return nil
}

func describeObject(kind string) string {
	if kind == "" {
		return "missing"
	}
	return "a " + kind
}

// prepareInstanceMigration runs before the migration transaction starts and
// reports whether the instance dimension still has to be introduced.
//
// A populated index is copied to a fixed, distinct pre-migration artifact first,
// exactly like the identity and rollup migrations: the destination is never
// overwritten, reused or deleted, and a backup failure stops before any history
// is touched. A newly created (empty) index has no history to protect and is
// migrated in place without a disk backup.
func (s *Store) prepareInstanceMigration(ctx context.Context) (bool, error) {
	marker, err := s.Setting(ctx, instanceVersionKey)
	if err != nil {
		return false, err
	}
	if err = s.checkInstanceSchema(ctx, marker); err != nil {
		return false, err
	}
	if marker == "1" {
		return false, nil
	}
	var count int64
	if err = s.db.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&count); err != nil {
		return false, err
	}
	var path string
	if err = s.db.QueryRowContext(ctx, "SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
		return false, err
	}
	if count > 0 && path != "" {
		destination := path + ".pre-instance-v1.db"
		written, err := s.BackupFreshDestination(ctx, destination)
		if err != nil {
			return false, migrationBackupFailure("instance", "No event, session, cursor or rollup row was changed and the instance marker was not set", destination, written, err)
		}
	}
	return true, nil
}

// instanceFactStamps is the per-request history oracle for the instance rebuild:
// every raw dedup key, timestamp, token class, cost and priced flag, in a stable
// order. It reads event_data directly (not the view), so it works before and
// after the rebuild and is never satisfied by a mere SUM.
func instanceFactStamps(tx *sql.Tx) ([32]byte, int64, error) {
	rows, err := tx.Query(`SELECT harness,dedup_key,timestamp,input,output,cache_read,cache_write,reasoning,cost,log_cost,priced FROM event_data ORDER BY harness,dedup_key`)
	if err != nil {
		return [32]byte{}, 0, err
	}
	defer rows.Close()
	h := sha256.New()
	var n int64
	for rows.Next() {
		var harness, key string
		var ts, in, out, read, write, reason int64
		var cost float64
		var logCost sql.NullFloat64
		var priced int
		if err = rows.Scan(&harness, &key, &ts, &in, &out, &read, &write, &reason, &cost, &logCost, &priced); err != nil {
			return [32]byte{}, 0, err
		}
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%v\x00%v\x00%d\n", harness, key, ts, in, out, read, write, reason, cost, logCost, priced)
		n++
	}
	if err = rows.Err(); err != nil {
		return [32]byte{}, 0, err
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, n, nil
}

// requireColumns fails loudly when the base schema is not the complete one every
// supported release produced. A partial table must never be "repaired" silently.
func requireColumns(tx *sql.Tx, table string, columns ...string) error {
	rows, err := tx.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var id, nn, pk int
		var name, typ string
		var def any
		if err = rows.Scan(&id, &name, &typ, &nn, &def, &pk); err != nil {
			return err
		}
		have[name] = true
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for _, c := range columns {
		if !have[c] {
			return instanceSchemaInconsistent(fmt.Sprintf("%s is missing the column %s", table, c))
		}
	}
	return nil
}

// migrateInstance introduces the instance dimension in one transaction. It
// rewrites no fact value: every existing row is copied verbatim under the
// reserved default instance, and the primary keys, dictionary uniqueness, views
// and maintenance triggers are rebuilt around that copy. Any failure rolls the
// whole transaction back, so facts, DDL and marker move together.
func (s *Store) migrateInstance(ctx context.Context, tx *sql.Tx) error {
	if err := requireColumns(tx, "event_data",
		"harness", "dedup_key", "dimension_id", "timestamp", "input", "output", "cache_read", "cache_write", "reasoning", "log_cost", "cost", "priced",
		"request_id", "boundary", "bill_amount", "bill_unit", "cache_expected", "cache_miss", "miss_cause", "miss_cost"); err != nil {
		return err
	}
	if err := requireColumns(tx, "event_dimensions",
		"id", "harness", "session_id", "parent_id", "project", "model", "provider", "base_url", "resolved_provider", "attrib", "raw_project"); err != nil {
		return err
	}
	if err := requireColumns(tx, "sessions",
		"harness", "session_id", "parent_id", "title", "project", "raw_project", "started_at", "updated_at"); err != nil {
		return err
	}
	if err := requireColumns(tx, "cursors", "harness", "path", "cursor"); err != nil {
		return err
	}

	before, beforeCount, err := instanceFactStamps(tx)
	if err != nil {
		return err
	}
	// The marker is written inside this transaction before the rebuild, so it is
	// never visible without the complete schema: a failure below rolls it back
	// together with the facts and the DDL. The history digest is checked at the
	// end, so a fault triggered by this very write is still caught.
	if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES(?,'1') ON CONFLICT(key) DO UPDATE SET value=excluded.value", instanceVersionKey); err != nil {
		return err
	}

	// The compatibility view (and its INSTEAD OF triggers) and the maintenance
	// triggers all reference the tables being rebuilt. The view drop takes its
	// triggers with it; the maintenance triggers live on event_data and must be
	// dropped explicitly, because a trigger referencing a table that is briefly
	// absent fails the schema reparse that ALTER TABLE ... RENAME performs. Every
	// one of them is recreated below.
	if _, err = tx.ExecContext(ctx, `DROP VIEW IF EXISTS events;
 DROP TRIGGER IF EXISTS daily_insert; DROP TRIGGER IF EXISTS daily_update; DROP TRIGGER IF EXISTS daily_delete;
 DROP TRIGGER IF EXISTS cache_insert; DROP TRIGGER IF EXISTS cache_update; DROP TRIGGER IF EXISTS cache_delete;
 DROP TRIGGER IF EXISTS provider_daily_insert; DROP TRIGGER IF EXISTS provider_daily_update; DROP TRIGGER IF EXISTS provider_daily_delete;
 DROP TRIGGER IF EXISTS provider_cache_insert; DROP TRIGGER IF EXISTS provider_cache_update; DROP TRIGGER IF EXISTS provider_cache_delete;`); err != nil {
		return err
	}

	// event_dimensions first: event_data's foreign key names it, and both are
	// recreated with the same names. Copying the integer id keeps every
	// event_data.dimension_id valid.
	steps := []struct {
		table, ddl, columns, selectList, from string
	}{
		{
			table: "event_dimensions", ddl: instanceEventDimensionsDDL,
			columns:    "id,harness,instance_id,session_id,parent_id,project,model,provider,base_url,resolved_provider,attrib,raw_project",
			selectList: "id,harness,'" + DefaultInstanceID + "',session_id,parent_id,project,model,provider,base_url,resolved_provider,attrib,raw_project",
			from:       "event_dimensions",
		},
		{
			table: "event_data", ddl: instanceEventDataDDL,
			columns:    factColumns + ",cache_expected,cache_miss,miss_cause,miss_cost",
			selectList: "harness,'" + DefaultInstanceID + "',dedup_key,dimension_id,timestamp,input,output,cache_read,cache_write,reasoning,log_cost,cost,priced,request_id,boundary,bill_amount,bill_unit,cache_expected,cache_miss,miss_cause,miss_cost",
			from:       "event_data",
		},
		{
			table: "sessions", ddl: instanceSessionsDDL,
			columns:    "harness,instance_id,session_id,parent_id,title,project,raw_project,started_at,updated_at",
			selectList: "harness,'" + DefaultInstanceID + "',session_id,parent_id,title,project,raw_project,started_at,updated_at",
			from:       "sessions",
		},
		{
			table: "cursors", ddl: instanceCursorsDDL,
			columns:    "harness,instance_id,path,cursor",
			selectList: "harness,'" + DefaultInstanceID + "',path,cursor",
			from:       "cursors",
		},
	}
	for _, step := range steps {
		target := step.table + "__instance"
		if _, err = tx.ExecContext(ctx, step.ddl); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO "+target+"("+step.columns+") SELECT "+step.selectList+" FROM "+step.from); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DROP TABLE "+step.table); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "ALTER TABLE "+target+" RENAME TO "+step.table); err != nil {
			return err
		}
	}
	for _, index := range instanceIndexes {
		if _, err = tx.ExecContext(ctx, index); err != nil {
			return err
		}
	}

	// Recreate the compatibility view and its write triggers, then every derived
	// rollup with its instance-aware key and maintenance triggers.
	if _, err = tx.ExecContext(ctx, eventsViewHead(true)+eventsViewCacheTail+eventsViewJoin); err != nil {
		return err
	}
	if err = createEventsTriggers(tx, instanceRollupSchema); err != nil {
		return err
	}
	for _, table := range []string{"daily_usage", "cache_daily", "provider_daily", "provider_cache_daily"} {
		if _, err = tx.ExecContext(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
			return err
		}
	}
	if err = migrateDaily(tx, instanceRollupSchema); err != nil {
		return err
	}
	if err = migrateCacheDaily(tx, instanceRollupSchema); err != nil {
		return err
	}
	if err = migrateProviderDaily(tx, instanceRollupSchema); err != nil {
		return err
	}
	if err = migrateProviderCacheDaily(tx, instanceRollupSchema); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO daily_usage("+dailyDimensions+","+dailyMetrics+") SELECT "+bareDimList(dailyDimensions, nil)+
		",sum(input),sum(output),sum(cache_read),sum(cache_write),sum(reasoning),"+
		"sum(CASE WHEN priced THEN cost ELSE 0 END),"+
		"sum(CASE WHEN log_cost IS NOT NULL THEN cost ELSE 0 END),"+
		"sum(CASE WHEN log_cost IS NULL AND priced THEN cost ELSE 0 END),"+
		"count(*),sum(NOT priced) FROM events GROUP BY "+groupByPositions(dailyDimensions)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM cache_daily; "+cacheRollupInsert(cacheDimensions)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM provider_daily; "+providerDailyRollupInsert(providerDailyDimensions)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM provider_cache_daily; "+providerCacheRollupInsert(providerCacheDimensions)); err != nil {
		return err
	}

	// Per-request invariant: the rebuild must be a pure re-keying. Compare the
	// ordered per-request digest and the row count, then prove no row lost its
	// instance and no fact points at a missing dictionary row.
	after, afterCount, err := instanceFactStamps(tx)
	if err != nil {
		return err
	}
	if before != after || beforeCount != afterCount {
		return fmt.Errorf("instance migration history invariant failed (%d facts before, %d after); transaction rolled back", beforeCount, afterCount)
	}
	var stray, orphans int64
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM event_data WHERE instance_id IS NULL OR instance_id=''").Scan(&stray); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM event_data e LEFT JOIN event_dimensions d ON d.id=e.dimension_id WHERE d.id IS NULL").Scan(&orphans); err != nil {
		return err
	}
	if stray != 0 || orphans != 0 {
		return fmt.Errorf("instance migration integrity failed: %d facts without an instance, %d facts without a dimension; transaction rolled back", stray, orphans)
	}
	return nil
}
