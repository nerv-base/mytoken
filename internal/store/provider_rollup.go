package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
)

// providerRollupVersion marks the endpoint-aware derived rollups. It is a
// separate marker from providerIdentityVersion because the two migrations have
// different preconditions: the identity index can be created from any pre-0.4
// index, while the rollups are only correct once event_dimensions and the cache
// columns exist. Keeping the markers apart lets an index that already carries
// the identity index receive the rollups explicitly, instead of silently
// claiming the derived summaries are complete.
const providerRollupVersion = "provider.endpoint-rollup.v1"

// providerDailyDimensions mirrors dailyDimensions with base_url added, and
// providerCacheDimensions mirrors cacheDimensions with base_url added. The
// rollups are derived metadata: they carry the same additive metrics as
// daily_usage and cache_daily, so every consumer sums them identically. The
// original rollups keep their primary keys and consumers untouched, and the
// instance generation adds instance_id in the same position as the base rollups.
const legacyProviderDailyDimensions = "day,harness,session_id,resolved_provider,model,project,base_url,attrib"
const legacyProviderCacheDimensions = "day,harness,session_id,resolved_provider,model,project,base_url,cause"

var providerDailyDimensions = withInstance(legacyProviderDailyDimensions)
var providerCacheDimensions = withInstance(legacyProviderCacheDimensions)

var providerRollupTables = []string{"provider_daily", "provider_cache_daily"}

// providerDailyValues builds the per-event contribution for provider_daily. It
// is dailyValues with base_url inserted, so the token, cost-source, request and
// unpriced expressions stay byte-for-byte the accounting expressions daily_usage
// uses; only the grouping key is widened.
func providerDailyValues(prefix, dims string) string {
	parts := dimValues(prefix, dims, nil)
	parts = append(parts, dailyMetricValues(prefix)...)
	return strings.Join(parts, ",")
}

// providerCacheValues is cacheValues with base_url inserted.
func providerCacheValues(prefix, dims string) string {
	p := prefix + "."
	parts := dimValues(prefix, dims, map[string]string{"cause": p + "miss_cause"})
	parts = append(parts,
		"1,("+p+"cache_expected>=0),max("+p+"cache_expected,0)",
		"CASE WHEN "+p+"cache_expected>=0 THEN "+p+"cache_read ELSE 0 END,"+p+"cache_miss,"+p+"miss_cost")
	return strings.Join(parts, ",")
}

// providerDailyRollupInsert rebuilds provider_daily from scratch over the events
// view. The events view exposes base_url from event_dimensions, so the backfill
// reads no new state and never writes events.
func providerDailyRollupInsert(dims string) string {
	return "INSERT INTO provider_daily(" + dims + "," + dailyMetrics + ") SELECT " + bareDimList(dims, nil) +
		",sum(input),sum(output),sum(cache_read),sum(cache_write),sum(reasoning)," +
		"sum(CASE WHEN priced THEN cost ELSE 0 END)," +
		"sum(CASE WHEN log_cost IS NOT NULL THEN cost ELSE 0 END)," +
		"sum(CASE WHEN log_cost IS NULL AND priced THEN cost ELSE 0 END)," +
		"count(*),sum(NOT priced) FROM events GROUP BY " + groupByPositions(dims)
}

// providerCacheRollupInsert rebuilds provider_cache_daily from scratch over the
// events view, including the cache verdict columns.
func providerCacheRollupInsert(dims string) string {
	return "INSERT INTO provider_cache_daily(" + dims + "," + cacheMetrics + ") SELECT " +
		bareDimList(dims, map[string]string{"cause": "miss_cause"}) +
		",count(*),sum(cache_expected>=0),sum(max(cache_expected,0))," +
		"sum(CASE WHEN cache_expected>=0 THEN cache_read ELSE 0 END),sum(cache_miss),sum(miss_cost) FROM events GROUP BY " + groupByPositions(dims)
}

// migrateProviderDaily creates provider_daily and its event_data maintenance
// triggers, mirroring migrateDaily. The primary key is daily_usage's key plus
// base_url, so one pricing provider that appears behind several tenants stays
// split exactly as the fact scan splits it.
func migrateProviderDaily(tx *sql.Tx, sc rollupSchema) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS provider_daily (
 ` + dimColumnDecls(sc.providerDaily) + `,
 ` + dailyMetricDecls + `,
 PRIMARY KEY(` + sc.providerDaily + `)) WITHOUT ROWID;`); err != nil {
		return err
	}
	return installRollupTriggers(tx, "provider_daily", "provider_daily", sc.providerDaily, dailyMetrics, sc.dimensionColumns, func(prefix string) string {
		return providerDailyValues(prefix, sc.providerDaily)
	}, nil)
}

// migrateProviderCacheDaily creates provider_cache_daily and its triggers,
// mirroring migrateCacheDaily. The cache verdicts live in the same group as
// cache_daily plus base_url, so an endpoint-filtered cache query never has to
// merge two tenants or read facts.
func migrateProviderCacheDaily(tx *sql.Tx, sc rollupSchema) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS provider_cache_daily (
 ` + dimColumnDecls(sc.providerCache) + `,
 requests INTEGER NOT NULL,cacheable INTEGER NOT NULL,expected INTEGER NOT NULL,read INTEGER NOT NULL,miss INTEGER NOT NULL,miss_cost REAL NOT NULL,
 PRIMARY KEY(` + sc.providerCache + `)) WITHOUT ROWID;`); err != nil {
		return err
	}
	return installRollupTriggers(tx, "provider_cache", "provider_cache_daily", sc.providerCache, cacheMetrics, sc.dimensionColumns, func(prefix string) string {
		return providerCacheValues(prefix, sc.providerCache)
	}, map[string]string{"cause": "OLD.miss_cause"})
}

// migrateProviderRollup creates both endpoint-aware rollups, backfills them from
// the events view and records the marker. It is additive: it creates derived
// tables and triggers only, and never rewrites an event, a session, an endpoint
// dictionary row or a price.
func migrateProviderRollup(tx *sql.Tx, sc rollupSchema) error {
	if err := migrateProviderDaily(tx, sc); err != nil {
		return err
	}
	if err := migrateProviderCacheDaily(tx, sc); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM provider_daily; " + providerDailyRollupInsert(sc.providerDaily)); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM provider_cache_daily; " + providerCacheRollupInsert(sc.providerCache)); err != nil {
		return err
	}
	_, err := tx.Exec("INSERT INTO settings(key,value) VALUES(?,'1') ON CONFLICT(key) DO UPDATE SET value=excluded.value", providerRollupVersion)
	return err
}

func providerRollupInconsistent(detail string) error {
	return fmt.Errorf("provider endpoint rollup is inconsistent: %s; refusing to guess, to reprice, or to skip the migration. Nothing was rewritten and no backup was replaced. Recovery needs an explicit operator decision", detail)
}

// checkProviderRollupIndex rejects a partially applied rollup migration before
// any work starts. A marker without its tables would otherwise surface as a raw
// "no such table" error from a read path, and tables without a marker cannot be
// trusted to hold a complete backfill.
func (s *Store) checkProviderRollupIndex(ctx context.Context, marker string) error {
	kinds := map[string]string{}
	for _, table := range providerRollupTables {
		var kind string
		err := s.db.QueryRowContext(ctx, "SELECT type FROM sqlite_master WHERE name=?", table).Scan(&kind)
		if err == sql.ErrNoRows {
			kind = ""
		} else if err != nil {
			return err
		}
		kinds[table] = kind
	}
	switch marker {
	case "":
		for _, table := range providerRollupTables {
			if kinds[table] != "" {
				return providerRollupInconsistent(fmt.Sprintf("%s %s exists but the rollup marker %s is absent", kinds[table], table, providerRollupVersion))
			}
		}
	case "1":
		for _, table := range providerRollupTables {
			if kinds[table] != "table" {
				return providerRollupInconsistent("the rollup marker " + providerRollupVersion + " is set but table " + table + " is missing")
			}
		}
	default:
		return providerRollupInconsistent(fmt.Sprintf("the rollup marker %s has unexpected value %q", providerRollupVersion, marker))
	}
	return nil
}

// prepareProviderRollup runs before the migration transaction starts and reports
// whether the rollups still have to be built.
//
// When the identity migration is pending too, the rollups are created inside
// that migration's transaction and are covered by its backup and its history
// digest, so no second backup is taken. On an index that already carries the
// identity marker, the rollup migration runs on its own: it writes its own
// backup under a distinct name, because the identity backup no longer covers the
// history that has accumulated since. A pre-existing backup is never overwritten,
// reused or deleted, exactly as for the identity migration.
func (s *Store) prepareProviderRollup(ctx context.Context, identityPending bool) (bool, error) {
	marker, err := s.Setting(ctx, providerRollupVersion)
	if err != nil {
		return false, err
	}
	if err = s.checkProviderRollupIndex(ctx, marker); err != nil {
		return false, err
	}
	if marker == "1" {
		return false, nil
	}
	if identityPending {
		return true, nil
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
		destination := path + ".pre-provider-endpoint-rollup-v1.db"
		if err = s.Backup(ctx, destination); err != nil {
			return false, fmt.Errorf(`provider endpoint rollup migration stopped before any history was migrated: could not write a consistent pre-migration backup to %s: %w

No event, session, price or identity row was changed and the rollup marker was not set; the migration transaction never started. The preflight check does not promise a byte-identical file: opening the index has already applied MyToken's bootstrap DDL (journal mode, and any base table or index created if absent), so only the history is guaranteed untouched. An existing %s is never overwritten, reused or deleted automatically, because it may be a partial artifact of an interrupted attempt.

Recovery requires an explicit operator decision. Stop every MyToken process first, then preserve the database together with its -wal and -shm files and the backup, and verify both independently (open the database and run PRAGMA quick_check; open the backup read-only and check it holds the expected history). Only then choose deliberately between repairing the cause of the failed backup, moving the old backup aside to retry, or restoring the backup. Do not restart while this state is unverified.`,
				destination, err, filepath.Base(destination))
		}
	}
	return true, nil
}

// rebuildProviderRollups drops and rebuilds both endpoint-aware rollups from the
// events view. Callers hold the writer lock; triggers must already be recreated.
func rebuildProviderRollups(ctx context.Context, tx *sql.Tx, sc rollupSchema) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM provider_daily; "+providerDailyRollupInsert(sc.providerDaily)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM provider_cache_daily; "+providerCacheRollupInsert(sc.providerCache)); err != nil {
		return err
	}
	return nil
}
