package store

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/ledger"
	"github.com/zzstar101/mytoken/internal/model"
)

// Derived cache-ledger columns on event_data. cache_expected=-1 marks a whole
// session Uncacheable (no cache activity at all); cache_expected>=0 is inside the
// ledger, where 0 is the cold-start first request. cache_miss/miss_cause/miss_cost
// are the verdict and the extra USD a miss cost (MissUSD, see docs/CACHE.md §1).
// These columns are derived: commit() and the events view never write them; they
// are maintained by ledger.Analyze plus the cache_daily triggers.
const (
	ledgerVersionKey = "ledger_version"
	cacheColumns     = "cache_expected,cache_miss,miss_cause,miss_cost"
)

var ledgerVersion = strconv.Itoa(ledger.Version)

var cacheColumnDecls = []struct{ name, decl string }{
	{"cache_expected", "INTEGER NOT NULL DEFAULT 0"},
	{"cache_miss", "INTEGER NOT NULL DEFAULT 0"},
	{"miss_cause", "TEXT NOT NULL DEFAULT ''"},
	{"miss_cost", "REAL NOT NULL DEFAULT 0"},
}

// cache_daily is a separate rollup (not folded into daily_usage) keyed by
// day × harness × instance × session × resolved_provider × model × project × cause.
// Keeping the cause dimension isolated leaves daily_usage's primary key and
// consumers untouched; keeping the instance dimension isolated stops two data
// sources that share a session id from sharing a ledger group. requests counts
// every event of the group; cacheable counts events in the ledger
// (cache_expected>=0). Because it is a rollup rather than per-event, per-event
// database growth stays tiny. See docs/CACHE.md §5.
const (
	legacyCacheDimensions = "day,harness,session_id,resolved_provider,model,project,cause"
	cacheMetrics          = "requests,cacheable,expected,read,miss,miss_cost"
)

// cacheDimensions is the instance-aware generation of legacyCacheDimensions.
var cacheDimensions = withInstance(legacyCacheDimensions)

// cacheValues builds the per-event rollup contribution for cache_daily. Dimension
// columns (harness, instance, session_id, resolved_provider, model, project) are
// rewritten to the event_dimensions alias by the callers (trigger context) exactly
// as aggregate.go's dailyValues does. expected uses max(cache_expected,0) so the
// uncacheable sentinel (-1) never subtracts from the ledger denominator.
func cacheValues(prefix, dims string) string {
	p := prefix + "."
	parts := dimValues(prefix, dims, map[string]string{"cause": p + "miss_cause"})
	parts = append(parts,
		"1,("+p+"cache_expected>=0),max("+p+"cache_expected,0)",
		"CASE WHEN "+p+"cache_expected>=0 THEN "+p+"cache_read ELSE 0 END,"+p+"cache_miss,"+p+"miss_cost")
	return strings.Join(parts, ",")
}

// cacheRollupInsert rebuilds cache_daily from scratch over the events view (which
// already exposes the cache columns). Used after a full ledger rebuild.
func cacheRollupInsert(dims string) string {
	return "INSERT INTO cache_daily(" + dims + "," + cacheMetrics + ") SELECT " +
		bareDimList(dims, map[string]string{"cause": "miss_cause"}) +
		",count(*),sum(cache_expected>=0),sum(max(cache_expected,0))," +
		"sum(CASE WHEN cache_expected>=0 THEN cache_read ELSE 0 END),sum(cache_miss),sum(miss_cost) FROM events GROUP BY " + groupByPositions(dims)
}

// migrateCache adds the derived cache columns, extends the events view and its
// INSTEAD OF triggers to expose them, then creates cache_daily with its own
// triggers and backfills the rollup. Idempotent: it returns early when every
// column is already present. Runs inside the caller's migration transaction.
func migrateCache(tx *sql.Tx, sc rollupSchema) error {
	rows, err := tx.Query(`PRAGMA table_info(event_data)`)
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var id, nn, pk int
		var name, typ string
		var def any
		if err = rows.Scan(&id, &name, &typ, &nn, &def, &pk); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	changed := false
	for _, col := range cacheColumnDecls {
		if !columns[col.name] {
			if _, err = tx.Exec("ALTER TABLE event_data ADD COLUMN " + col.name + " " + col.decl); err != nil {
				return err
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	// DROP VIEW events also drops its INSTEAD OF triggers, so both are recreated.
	if _, err = tx.Exec(`DROP VIEW events; ` + eventsViewHead(sc.instance) + eventsViewCacheTail + eventsViewJoin); err != nil {
		return err
	}
	if err = createEventsTriggers(tx, sc); err != nil {
		return err
	}
	if err = migrateCacheDaily(tx, sc); err != nil {
		return err
	}
	// Backfill: every migrated event carries the default cache_expected=0, so the
	// rollup initially treats them all as cacheable with expected=0. The first
	// full ledger rebuild (ensureLedgerVersion) corrects both event_data and this
	// rollup. The INSERT...SELECT writes cache_daily directly, so no per-row
	// trigger fires here.
	if _, err = tx.Exec("DELETE FROM cache_daily; " + cacheRollupInsert(sc.cache)); err != nil {
		return err
	}
	return nil
}

// migrateCacheDaily creates cache_daily and the cache_insert/update/delete
// triggers on event_data that keep it in sync, mirroring the daily_usage triggers
// in aggregate.go. CREATE ... IF NOT EXISTS keeps it idempotent, so the full
// rebuild drops these triggers and calls this to recreate them.
func migrateCacheDaily(tx *sql.Tx, sc rollupSchema) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS cache_daily (
 ` + dimColumnDecls(sc.cache) + `,
 requests INTEGER NOT NULL,cacheable INTEGER NOT NULL,expected INTEGER NOT NULL,read INTEGER NOT NULL,miss INTEGER NOT NULL,miss_cost REAL NOT NULL,
 PRIMARY KEY(` + sc.cache + `)) WITHOUT ROWID;`); err != nil {
		return err
	}
	return installRollupTriggers(tx, "cache", "cache_daily", sc.cache, cacheMetrics, sc.dimensionColumns, func(prefix string) string {
		return cacheValues(prefix, sc.cache)
	}, map[string]string{"cause": "OLD.miss_cause"})
}

// ensureLedgerVersion runs a full rebuild once whenever the stored ledger version
// differs from the compiled ledger.Version (first 0.3 open, or a future algorithm
// bump). Independent of the pricing fingerprint so a stale ledger is always healed.
func (s *Store) ensureLedgerVersion(ctx context.Context) error {
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", ledgerVersionKey).Scan(&v)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if v == ledgerVersion {
		return nil
	}
	return s.rebuildLedger(ctx)
}

// rebuildLedger re-Analyzes every session's whole chain and rewrites the derived
// cache columns plus cache_daily in bulk. Triggers are dropped around the bulk
// UPDATE so the (cache-only) writes do not fire per-row daily/cache upserts, then
// both trigger sets are recreated. Runs under the writer lock held by the caller.
func (s *Store) rebuildLedger(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "CREATE TEMP TABLE _cache_stage(harness TEXT NOT NULL,instance_id TEXT NOT NULL,dedup_key TEXT NOT NULL,cache_expected INTEGER NOT NULL,cache_miss INTEGER NOT NULL,miss_cause TEXT NOT NULL,miss_cost REAL NOT NULL)"); err != nil {
		return err
	}
	defer tx.ExecContext(ctx, "DROP TABLE IF EXISTS _cache_stage")
	rows, err := tx.QueryContext(ctx, "SELECT DISTINCT harness,instance_id,session_id FROM event_dimensions ORDER BY harness,instance_id,session_id")
	if err != nil {
		return err
	}
	var sessions [][3]string
	for rows.Next() {
		var h, instance, sid string
		if err = rows.Scan(&h, &instance, &sid); err != nil {
			rows.Close()
			return err
		}
		sessions = append(sessions, [3]string{h, instance, sid})
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, sess := range sessions {
		if err = s.analyzeSession(ctx, tx, sess[0], sess[1], sess[2]); err != nil {
			return err
		}
	}
	// The endpoint-aware rollups are dropped here as well: provider_daily does
	// not depend on the cache columns, so its data stays valid and only its
	// triggers are recreated, but provider_cache_daily is derived from those
	// columns and is rebuilt from the updated facts below.
	for _, t := range []string{"cache_insert", "cache_update", "cache_delete", "daily_insert", "daily_update", "daily_delete", "provider_daily_insert", "provider_daily_update", "provider_daily_delete", "provider_cache_insert", "provider_cache_update", "provider_cache_delete"} {
		if _, err = tx.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+t); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE event_data SET cache_expected=s.cache_expected,cache_miss=s.cache_miss,miss_cause=s.miss_cause,miss_cost=s.miss_cost FROM _cache_stage s WHERE s.harness=event_data.harness AND s.instance_id=event_data.instance_id AND s.dedup_key=event_data.dedup_key`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM cache_daily; "+cacheRollupInsert(cacheDimensions)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM provider_cache_daily; "+providerCacheRollupInsert(providerCacheDimensions)); err != nil {
		return err
	}
	if err = migrateCacheDaily(tx, instanceRollupSchema); err != nil {
		return err
	}
	if err = migrateDaily(tx, instanceRollupSchema); err != nil {
		return err
	}
	if err = migrateProviderDaily(tx, instanceRollupSchema); err != nil {
		return err
	}
	if err = migrateProviderCacheDaily(tx, instanceRollupSchema); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO settings VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", ledgerVersionKey, ledgerVersion); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.notify()
	return nil
}

// stageRow is one pending per-request verdict destined for the _cache_stage table.
type stageRow struct {
	harness, instance, key string
	expected, miss         int64
	cause                  string
	missCost               float64
}

// flushStage writes staged verdicts into _cache_stage using chunked multi-row
// INSERTs, staying under SQLite's bound-parameter limit (7 cols × 400 rows).
func flushStage(ctx context.Context, tx *sql.Tx, rows []stageRow) error {
	const cols, maxRows = 7, 400
	for start := 0; start < len(rows); start += maxRows {
		end := start + maxRows
		if end > len(rows) {
			end = len(rows)
		}
		var sb strings.Builder
		sb.WriteString("INSERT INTO _cache_stage VALUES ")
		args := make([]any, 0, (end-start)*cols)
		for i := start; i < end; i++ {
			if i > start {
				sb.WriteByte(',')
			}
			sb.WriteString("(?,?,?,?,?,?,?)")
			r := rows[i]
			args = append(args, r.harness, r.instance, r.key, r.expected, r.miss, r.cause, r.missCost)
		}
		if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

// analyzeSession loads one session's whole chain in time order, runs ledger.Analyze
// over it, computes MissUSD from the local cost function, and stages each event's
// derived verdict into _cache_stage via chunked inserts. The chain is scoped by
// harness *and* instance, so two sources that reuse a session id never share a
// ledger.
func (s *Store) analyzeSession(ctx context.Context, tx *sql.Tx, harness, instance, sessionID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT e.dedup_key,e.timestamp,d.resolved_provider,d.model,e.input,e.cache_read,e.cache_write,e.boundary FROM event_dimensions d JOIN event_data e ON e.dimension_id=d.id WHERE d.harness=? AND d.instance_id=? AND d.session_id=? ORDER BY e.timestamp,e.dedup_key`, harness, instance, sessionID)
	if err != nil {
		return err
	}
	var keys []string
	var reqs []ledger.Request
	for rows.Next() {
		var key, provider, mdl, boundary string
		var ts, input, cacheRead, cacheWrite int64
		if err = rows.Scan(&key, &ts, &provider, &mdl, &input, &cacheRead, &cacheWrite, &boundary); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, key)
		reqs = append(reqs, ledger.Request{At: time.Unix(0, ts).UTC(), Harness: harness, Provider: provider, Model: mdl, Input: input, CacheRead: cacheRead, CacheWrite: cacheWrite, Boundary: model.Boundary(boundary)})
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	entries := ledger.Analyze(reqs)
	staged := make([]stageRow, 0, len(entries))
	for i, entry := range entries {
		req := reqs[i]
		expected := entry.Expected
		if entry.Uncacheable {
			expected = -1
		}
		var missCost float64
		if entry.Miss > 0 {
			missCost = s.missUSD(model.UsageEvent{Harness: model.Harness(harness), Model: req.Model, Provider: req.Provider, Timestamp: req.At, Tokens: model.Tokens{Input: req.Input, CacheRead: req.CacheRead, CacheWrite: req.CacheWrite}}, entry.Miss)
		}
		staged = append(staged, stageRow{harness, instance, keys[i], expected, entry.Miss, string(entry.Cause), missCost})
	}
	return flushStage(ctx, tx, staged)
}

// missUSD is MissUSD from docs/CACHE.md §1: cost(e) − cost(Counterfactual(e,miss))
// under the local estimate price function, independent of any authoritative log
// cost. Clamped to zero because a miss cannot save money.
func (s *Store) missUSD(e model.UsageEvent, miss int64) float64 {
	if s.costFn == nil || miss <= 0 {
		return 0
	}
	real, _ := s.costFn(e)
	cached, _ := s.costFn(ledger.Counterfactual(e, miss))
	if d := real - cached; d > 0 {
		return d
	}
	return 0
}

// recomputeSessions re-Analyzes the touched sessions' whole chains inside the
// caller's commit transaction and patches only the events whose verdict changed,
// so the cache_daily triggers fire for the minimum number of rows. Appending new
// events leaves earlier verdicts unchanged, so the patch is normally tiny. The
// touched key carries the instance, so two instances that reuse a session id are
// analyzed separately.
func (s *Store) recomputeSessions(ctx context.Context, tx *sql.Tx, touched map[string][3]string) error {
	if len(touched) == 0 {
		return nil
	}
	var args []any
	for _, sess := range touched {
		rows, err := tx.QueryContext(ctx, `SELECT e.dedup_key,e.timestamp,d.resolved_provider,d.model,e.input,e.cache_read,e.cache_write,e.boundary,e.cache_expected,e.cache_miss,e.miss_cause,e.miss_cost FROM event_dimensions d JOIN event_data e ON e.dimension_id=d.id WHERE d.harness=? AND d.instance_id=? AND d.session_id=? ORDER BY e.timestamp,e.dedup_key`, sess[0], sess[1], sess[2])
		if err != nil {
			return err
		}
		var keys []string
		var reqs []ledger.Request
		var oldExpected, oldMiss []int64
		var oldCause []string
		var oldCost []float64
		for rows.Next() {
			var key, provider, mdl, boundary, cause string
			var ts, input, cacheRead, cacheWrite, exp, miss int64
			var cost float64
			if err = rows.Scan(&key, &ts, &provider, &mdl, &input, &cacheRead, &cacheWrite, &boundary, &exp, &miss, &cause, &cost); err != nil {
				rows.Close()
				return err
			}
			keys = append(keys, key)
			reqs = append(reqs, ledger.Request{At: time.Unix(0, ts).UTC(), Harness: sess[0], Provider: provider, Model: mdl, Input: input, CacheRead: cacheRead, CacheWrite: cacheWrite, Boundary: model.Boundary(boundary)})
			oldExpected = append(oldExpected, exp)
			oldMiss = append(oldMiss, miss)
			oldCause = append(oldCause, cause)
			oldCost = append(oldCost, cost)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		entries := ledger.Analyze(reqs)
		for i, entry := range entries {
			req := reqs[i]
			expected := entry.Expected
			if entry.Uncacheable {
				expected = -1
			}
			missCost := oldCost[i]
			if entry.Miss > 0 {
				missCost = s.missUSD(model.UsageEvent{Harness: model.Harness(sess[0]), Model: req.Model, Provider: req.Provider, Timestamp: req.At, Tokens: model.Tokens{Input: req.Input, CacheRead: req.CacheRead, CacheWrite: req.CacheWrite}}, entry.Miss)
			}
			if expected == oldExpected[i] && entry.Miss == oldMiss[i] && string(entry.Cause) == oldCause[i] && missCost == oldCost[i] {
				continue
			}
			args = append(args, sess[0], sess[1], keys[i], expected, entry.Miss, string(entry.Cause), missCost)
		}
	}
	if len(args) == 0 {
		return nil
	}
	// Chunk the patch: one combined VALUES list can exceed SQLite's bound-parameter
	// limit on a bulk import (many sessions, many changed rows at once).
	const cols, maxRows = 7, 400 // 400*7 = 2800 params, safely under the 32766 default
	for start := 0; start < len(args); start += maxRows * cols {
		end := start + maxRows*cols
		if end > len(args) {
			end = len(args)
		}
		n := (end - start) / cols
		q := `WITH patch(harness,instance_id,dedup_key,cache_expected,cache_miss,miss_cause,miss_cost) AS (VALUES ` + strings.TrimSuffix(strings.Repeat("(?,?,?,?,?,?,?),", n), ",") + `) UPDATE event_data SET cache_expected=patch.cache_expected,cache_miss=patch.cache_miss,miss_cause=patch.miss_cause,miss_cost=patch.miss_cost FROM patch WHERE patch.harness=event_data.harness AND patch.instance_id=event_data.instance_id AND patch.dedup_key=event_data.dedup_key`
		if _, err := tx.ExecContext(ctx, q, args[start:end]...); err != nil {
			return err
		}
	}
	return nil
}

// recomputeMissCost recomputes miss_cost for every cache_miss>0 event after a price
// change. Unlike replacePricing's cost loop, it must include log-priced events,
// because MissUSD always uses the local estimate for both the real and the
// counterfactual event. Runs inside replacePricing's transaction.
func (s *Store) recomputeMissCost(ctx context.Context, tx *sql.Tx, cost func(model.UsageEvent) (float64, bool)) error {
	if cost == nil {
		return nil
	}
	var afterInstance, afterKey string
	for {
		rows, err := tx.QueryContext(ctx, `SELECT instance_id,dedup_key,harness,timestamp,resolved_provider,model,input,cache_read,cache_write,cache_miss FROM events WHERE cache_expected>=0 AND cache_miss>0 AND (instance_id,dedup_key)>(?,?) ORDER BY instance_id,dedup_key LIMIT 512`, afterInstance, afterKey)
		if err != nil {
			return err
		}
		var args []any
		count := 0
		for rows.Next() {
			var key, harness, instance, provider, mdl string
			var ts, input, cacheRead, cacheWrite, miss int64
			if err = rows.Scan(&instance, &key, &harness, &ts, &provider, &mdl, &input, &cacheRead, &cacheWrite, &miss); err != nil {
				rows.Close()
				return err
			}
			afterInstance, afterKey = instance, key
			count++
			e := model.UsageEvent{Harness: model.Harness(harness), Model: mdl, Provider: provider, Timestamp: time.Unix(0, ts).UTC(), Tokens: model.Tokens{Input: input, CacheRead: cacheRead, CacheWrite: cacheWrite}}
			real, _ := cost(e)
			cached, _ := cost(ledger.Counterfactual(e, miss))
			next := real - cached
			if next < 0 {
				next = 0
			}
			args = append(args, harness, instance, key, next)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(args) > 0 {
			q := `WITH patch(harness,instance_id,dedup_key,miss_cost) AS (VALUES ` + strings.TrimSuffix(strings.Repeat("(?,?,?,?),", len(args)/4), ",") + `) UPDATE event_data SET miss_cost=patch.miss_cost FROM patch WHERE patch.harness=event_data.harness AND patch.instance_id=event_data.instance_id AND patch.dedup_key=event_data.dedup_key`
			if _, err = tx.ExecContext(ctx, q, args...); err != nil {
				return err
			}
		}
		if count < 512 {
			break
		}
	}
	return nil
}
