package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"time"

	"modernc.org/sqlite"
)

func init() {
	sqlite.MustRegisterScalarFunction("mytoken_day", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		n, ok := args[0].(int64)
		if !ok {
			return nil, fmt.Errorf("invalid event timestamp %T", args[0])
		}
		t := time.Unix(0, n).In(time.Local)
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local).UnixNano(), nil
	})
	sqlite.MustRegisterScalarFunction("mytoken_hour", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		n, ok := args[0].(int64)
		if !ok {
			return nil, fmt.Errorf("invalid event timestamp %T", args[0])
		}
		t := time.Unix(0, n).In(time.Local)
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.Local).UnixNano(), nil
	})
}

// rollupSchema names one schema generation's column sets. Every derived rollup
// and every maintenance trigger is built from it, so the legacy generation (no
// instance dimension) and the instance generation share one implementation and
// cannot drift apart.
type rollupSchema struct {
	instance         bool
	dimensionColumns string
	factColumns      string
	conflict         string
	daily            string
	cache            string
	providerDaily    string
	providerCache    string
}

var legacyRollupSchema = rollupSchema{
	dimensionColumns: legacyDimensionColumns,
	factColumns:      legacySignalFactColumns,
	conflict:         "harness,dedup_key",
	daily:            legacyDailyDimensions,
	cache:            legacyCacheDimensions,
	providerDaily:    legacyProviderDailyDimensions,
	providerCache:    legacyProviderCacheDimensions,
}

var instanceRollupSchema = rollupSchema{
	instance:         true,
	dimensionColumns: dimensionColumns,
	factColumns:      factColumns,
	conflict:         "harness,instance_id,dedup_key",
	daily:            dailyDimensions,
	cache:            cacheDimensions,
	providerDaily:    providerDailyDimensions,
	providerCache:    providerCacheDimensions,
}

const legacyDailyDimensions = "day,harness,session_id,resolved_provider,model,project,attrib"

var dailyDimensions = withInstance(legacyDailyDimensions)

const dailyMetrics = "input,output,cache_read,cache_write,reasoning,cost,cost_log,cost_estimate,requests,unpriced"

const dailyMetricDecls = `input INTEGER NOT NULL,output INTEGER NOT NULL,cache_read INTEGER NOT NULL,cache_write INTEGER NOT NULL,reasoning INTEGER NOT NULL,cost REAL NOT NULL,cost_log REAL NOT NULL,cost_estimate REAL NOT NULL,requests INTEGER NOT NULL,unpriced INTEGER NOT NULL`

// dimExpr maps one rollup dimension to its per-event expression. `day` is the
// only dimension that is not a stored column: it is derived from the timestamp.
func dimExpr(prefix, dim string) string {
	p := prefix + "."
	if dim == "day" {
		return "mytoken_day(" + p + "timestamp)"
	}
	return p + dim
}

// dimValues builds the dimension half of a rollup contribution in the order the
// dimension list declares, so it stays aligned with the table's primary key.
func dimValues(prefix, dims string, overrides map[string]string) []string {
	var out []string
	for _, d := range strings.Split(dims, ",") {
		if v, ok := overrides[d]; ok {
			out = append(out, v)
			continue
		}
		out = append(out, dimExpr(prefix, d))
	}
	return out
}

// bareDimList is the SELECT-side dimension list used when backfilling a rollup
// from the events view: unqualified columns, `day` derived from timestamp.
func bareDimList(dims string, overrides map[string]string) string {
	var out []string
	for _, d := range strings.Split(dims, ",") {
		if v, ok := overrides[d]; ok {
			out = append(out, v)
			continue
		}
		if d == "day" {
			out = append(out, "mytoken_day(timestamp)")
			continue
		}
		out = append(out, d)
	}
	return strings.Join(out, ",")
}

// subqueryDimList is the row-value list in a rollup trigger's delete branch. It
// reads the trigger row for columns that live on event_data (timestamp,
// miss_cause) and the joined event_dimensions row for every dimension.
func subqueryDimList(prefix, dims string, fromFact map[string]string) string {
	var out []string
	for _, d := range strings.Split(dims, ",") {
		if v, ok := fromFact[d]; ok {
			out = append(out, v)
			continue
		}
		if d == "day" {
			out = append(out, "mytoken_day("+prefix+".timestamp)")
			continue
		}
		out = append(out, d)
	}
	return strings.Join(out, ",")
}

func groupByPositions(dims string) string {
	var out []string
	for i := 1; i <= strings.Count(dims, ",")+1; i++ {
		out = append(out, fmt.Sprint(i))
	}
	return strings.Join(out, ",")
}

// dimColumnDecls renders the NOT NULL column declarations of a rollup's
// dimensions. Every dimension except `day` is a TEXT key.
func dimColumnDecls(dims string) string {
	var out []string
	for _, d := range strings.Split(dims, ",") {
		if d == "day" {
			out = append(out, "day INTEGER NOT NULL")
			continue
		}
		out = append(out, d+" TEXT NOT NULL")
	}
	return strings.Join(out, ",")
}

func dailyMetricValues(prefix string) []string {
	p := prefix + "."
	return []string{p + "input", p + "output", p + "cache_read", p + "cache_write", p + "reasoning",
		"CASE WHEN " + p + "priced THEN " + p + "cost ELSE 0 END",
		"CASE WHEN " + p + "log_cost IS NOT NULL THEN " + p + "cost ELSE 0 END",
		"CASE WHEN " + p + "log_cost IS NULL AND " + p + "priced THEN " + p + "cost ELSE 0 END",
		"1", "NOT " + p + "priced"}
}

func dailyValues(prefix, dims string) string {
	parts := dimValues(prefix, dims, nil)
	parts = append(parts, dailyMetricValues(prefix)...)
	return strings.Join(parts, ",")
}

// dailyRollupInsert rebuilds daily_usage from scratch over the events view.
func dailyRollupInsert(dims string) string {
	return "INSERT INTO daily_usage(" + dims + "," + dailyMetrics + ") SELECT " + bareDimList(dims, nil) +
		",sum(input),sum(output),sum(cache_read),sum(cache_write),sum(reasoning)," +
		"sum(CASE WHEN priced THEN cost ELSE 0 END)," +
		"sum(CASE WHEN log_cost IS NOT NULL THEN cost ELSE 0 END)," +
		"sum(CASE WHEN log_cost IS NULL AND priced THEN cost ELSE 0 END)," +
		"count(*),sum(NOT priced) FROM events GROUP BY " + groupByPositions(dims)
}

// rollupTriggerChange mirrors the original change() closure: the contribution is
// computed from the trigger row, then every event_dimensions column reference is
// rewritten to the joined dictionary row so nothing is stored twice.
func rollupTriggerChange(table, dims, metrics, dimensionColumns string, values func(string) string, prefix string, subtract bool) string {
	var add, sub []string
	for _, col := range strings.Split(metrics, ",") {
		add = append(add, col+"="+table+"."+col+"+excluded."+col)
		sub = append(sub, col+"="+table+"."+col+"-excluded."+col)
	}
	updates := add
	if subtract {
		updates = sub
	}
	v := values(prefix)
	for _, col := range strings.Split(dimensionColumns, ",") {
		v = strings.ReplaceAll(v, prefix+"."+col, "d."+col)
	}
	return "INSERT INTO " + table + "(" + dims + "," + metrics + ") SELECT " + v + " FROM event_dimensions d WHERE d.id=" + prefix + ".dimension_id ON CONFLICT(" + dims + ") DO UPDATE SET " + strings.Join(updates, ",") + ";"
}

// installRollupTriggers creates the insert/update/delete maintenance triggers of
// one rollup table over event_data. fromFact overrides dimensions whose value
// lives on the trigger row rather than on the dictionary (cache `cause`).
func installRollupTriggers(tx *sql.Tx, name, table, dims, metrics, dimensionColumns string, values func(string) string, fromFact map[string]string) error {
	change := func(prefix string, subtract bool) string {
		return rollupTriggerChange(table, dims, metrics, dimensionColumns, values, prefix, subtract)
	}
	remove := change("OLD", true) + "DELETE FROM " + table + " WHERE requests=0 AND (" + dims + ")=(SELECT " + subqueryDimList("OLD", dims, fromFact) + " FROM event_dimensions WHERE id=OLD.dimension_id);"
	for _, trigger := range []struct{ suffix, body string }{
		{"insert", "AFTER INSERT ON event_data BEGIN " + change("NEW", false)},
		{"delete", "AFTER DELETE ON event_data BEGIN " + remove},
		{"update", "AFTER UPDATE ON event_data BEGIN " + remove + change("NEW", false)},
	} {
		if _, err := tx.Exec("CREATE TRIGGER IF NOT EXISTS " + name + "_" + trigger.suffix + " " + trigger.body + " END"); err != nil {
			return err
		}
	}
	return nil
}

func migrateDaily(tx *sql.Tx, sc rollupSchema) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS daily_usage (
 ` + dimColumnDecls(sc.daily) + `,
 ` + dailyMetricDecls + `,
 PRIMARY KEY(` + sc.daily + `)) WITHOUT ROWID;
 DROP INDEX IF EXISTS events_model_time; DROP INDEX IF EXISTS events_provider_time;`)
	if err != nil {
		return err
	}
	return installRollupTriggers(tx, "daily", "daily_usage", sc.daily, dailyMetrics, sc.dimensionColumns, func(prefix string) string {
		return dailyValues(prefix, sc.daily)
	}, nil)
}

// LocalDayZone identifies the timezone rules used by local-day rollups.
func LocalDayZone() string {
	// Name plus seasonal offsets also identifies fixed zones and /etc/localtime.
	var b strings.Builder
	b.WriteString(time.Local.String())
	for _, year := range []int{1970, 2000, 2020, time.Now().Year(), time.Now().Year() + 1} {
		for _, month := range []time.Month{time.January, time.April, time.July, time.October} {
			name, offset := time.Date(year, month, 1, 0, 0, 0, 0, time.Local).Zone()
			fmt.Fprintf(&b, "/%s:%d", name, offset)
		}
	}
	return b.String()
}

// EnsureLocalDays rebuilds only derived summaries when the local timezone changes.
// Called at database open, never by queries or display-timezone overrides.
func (s *Store) EnsureLocalDays(ctx context.Context) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	key := LocalDayZone()
	if s.aggregateZone == key {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old string
	err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='daily_timezone'`).Scan(&old)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if old != key {
		if _, err = tx.ExecContext(ctx, "DELETE FROM daily_usage; "+dailyRollupInsert(dailyDimensions)); err != nil {
			return err
		}
		// Every rollup that buckets by local day follows the same boundary, so a
		// zone change rebuilds all four together. Leaving a cache rollup on the
		// old boundary would let an endpoint-filtered query disagree with the
		// unfiltered one, which is exactly the divergence these rollups exist to
		// avoid. The endpoint-aware rollups are rebuilt from the events view too,
		// so the backfill still reads no state that the change did not touch.
		if _, err = tx.ExecContext(ctx, "DELETE FROM cache_daily; "+cacheRollupInsert(cacheDimensions)); err != nil {
			return err
		}
		if err = rebuildProviderRollups(ctx, tx, instanceRollupSchema); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO settings VALUES('daily_timezone',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err == nil {
		s.aggregateZone = key
	}
	return err
}

// RefreshDaily rebuilds derived rollups while retaining all historical events.
func (s *Store) RefreshDaily(ctx context.Context) error {
	s.writer.Lock()
	s.aggregateZone = ""
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key='daily_timezone'`)
	s.writer.Unlock()
	if err != nil {
		return err
	}
	return s.EnsureLocalDays(ctx)
}
