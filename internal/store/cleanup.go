package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cleanupVersionKey records that the 0.5 schema cleanup (relay tables, cache
// ledger tables and columns, obsolete settings) has run on this index.
const cleanupVersionKey = "cleanup_0_5"

// obsoleteTables are the relay reconciliation and cache ledger tables that 0.5
// removed. They are dropped, not migrated: no history lives in them that the
// fact tables do not already carry.
var obsoleteTables = []string{
	"relay_balances", "relay_bill_data", "relay_bill_dimensions", "relay_bills",
	"relay_cursors", "relay_daily", "relay_origins", "relay_sites",
	"cache_daily", "provider_cache_daily",
}

// obsoleteColumns are the cache-miss ledger columns of event_data. They were
// derived data, so dropping them loses no history.
var obsoleteColumns = []string{"cache_expected", "cache_miss", "miss_cause", "miss_cost"}

// migrateCacheWrite1h adds the 1-hour cache-write tier to an index written
// before 0.5. The value is a subset of cache_write, so an old row stays correct
// with the zero default: its whole cache write keeps the 5-minute price.
func migrateCacheWrite1h(tx *sql.Tx) (bool, error) {
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM pragma_table_info('event_data') WHERE name='cache_write_1h'`).Scan(&n); err != nil {
		return false, err
	}
	if n == 1 {
		return false, nil
	}
	_, err := tx.Exec(`ALTER TABLE event_data ADD COLUMN cache_write_1h INTEGER NOT NULL DEFAULT 0`)
	return true, err
}

// migrateDecodeMs adds the measured generation window to an index written before
// a reader could report one. The column is evidence, not a token class: an old
// row keeps the zero default, which means "this log did not time it", and nothing
// is recounted or repriced by adding it.
func migrateDecodeMs(tx *sql.Tx) (bool, error) {
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM pragma_table_info('event_data') WHERE name='decode_ms'`).Scan(&n); err != nil {
		return false, err
	}
	if n == 1 {
		return false, nil
	}
	_, err := tx.Exec(`ALTER TABLE event_data ADD COLUMN decode_ms INTEGER NOT NULL DEFAULT 0`)
	return true, err
}

// rebuildEventsView recreates the compatibility view without the cache-ledger
// tail and reinstalls its write triggers.
func rebuildEventsView(tx *sql.Tx, sc rollupSchema) error {
	if _, err := tx.Exec(`DROP VIEW IF EXISTS events; ` + eventsViewHead(sc.instance) + eventsViewSignalsTail + eventsViewJoin); err != nil {
		return err
	}
	return createEventsTriggers(tx, sc)
}

// needsCleanup reports whether the index still carries 0.5-obsolete tables,
// columns or settings. A marker short-circuits the probe once cleanup ran.
func (s *Store) needsCleanup(ctx context.Context) (bool, error) {
	var done string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, cleanupVersionKey).Scan(&done)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if done == "1" {
		return false, nil
	}
	for _, table := range obsoleteTables {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name=?`, table).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return true, nil
		}
	}
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(event_data)`)
	if err != nil {
		return false, err
	}
	have := map[string]bool{}
	for rows.Next() {
		var id, nn, pk int
		var name, typ string
		var def any
		if err := rows.Scan(&id, &name, &typ, &nn, &def, &pk); err != nil {
			rows.Close()
			return false, err
		}
		have[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	for _, c := range obsoleteColumns {
		if have[c] {
			return true, nil
		}
	}
	// Obsolete settings on their own still deserve one cleanup pass.
	var stale int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM settings WHERE key LIKE 'relay%' OR key LIKE 'budget%' OR key LIKE 'instance.%' OR key LIKE 'quota.claude.consent.%' OR key LIKE 'quota.claude.latest.%'`).Scan(&stale); err != nil {
		return false, err
	}
	return stale > 0, nil
}

// backupBeforeCleanup keeps exactly one pre-migration copy next to the index.
// Older pre-migration copies written by this step are removed; backups from
// other migration steps are left alone because they may be operator records.
func (s *Store) backupBeforeCleanup(ctx context.Context) error {
	var path string
	if err := s.db.QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		return err
	}
	if path == "" || path == ":memory:" {
		return nil
	}
	dest := path + ".pre-migration.db"
	if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
		return err
	}
	matches, _ := filepath.Glob(path + ".pre-migration-*.db")
	for _, m := range matches {
		_ = os.Remove(m)
	}
	return s.Backup(ctx, dest)
}

// dropObsolete removes the tables, columns and settings that 0.5 deleted, then
// rebuilds the events view and its triggers in the reduced shape.
func dropObsolete(ctx context.Context, tx *sql.Tx, sc rollupSchema) error {
	var b strings.Builder
	b.WriteString(`DROP VIEW IF EXISTS events;
 DROP TRIGGER IF EXISTS cache_insert; DROP TRIGGER IF EXISTS cache_update; DROP TRIGGER IF EXISTS cache_delete;
 DROP TRIGGER IF EXISTS provider_cache_insert; DROP TRIGGER IF EXISTS provider_cache_update; DROP TRIGGER IF EXISTS provider_cache_delete;`)
	for _, table := range obsoleteTables {
		var kind string
		err := tx.QueryRowContext(ctx, `SELECT type FROM sqlite_master WHERE name=?`, table).Scan(&kind)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return err
		}
		verb := "TABLE"
		if kind == "view" {
			verb = "VIEW"
		}
		b.WriteString(" DROP " + verb + " IF EXISTS " + table + ";")
	}
	if _, err := tx.ExecContext(ctx, b.String()); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(event_data)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var id, nn, pk int
		var name, typ string
		var def any
		if err := rows.Scan(&id, &name, &typ, &nn, &def, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range obsoleteColumns {
		if !have[c] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE event_data DROP COLUMN `+c); err != nil {
			return fmt.Errorf("drop obsolete column %s: %w", c, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key LIKE 'relay%' OR key LIKE 'budget%' OR key LIKE 'instance.%' OR key LIKE 'quota.claude.consent.%' OR key LIKE 'quota.claude.latest.%' OR key IN ('ledger_version')`); err != nil {
		return err
	}
	if err := stripSettingMultipliers(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,'1') ON CONFLICT(key) DO UPDATE SET value=excluded.value`, cleanupVersionKey); err != nil {
		return err
	}
	return rebuildEventsView(tx, sc)
}

// stripSettingMultipliers rewrites price-rules rows in place: the deleted
// multiplier field and import-only rules go away, and every surviving rule is
// attributed to the user. Explicit rates and dates are kept.
func stripSettingMultipliers(ctx context.Context, tx *sql.Tx) error {
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='price-rules'`).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var rules []map[string]any
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		return nil // an unreadable row is left untouched; readers ignore it too
	}
	changed := false
	kept := rules[:0]
	for _, r := range rules {
		if _, ok := r["multiplier"]; ok {
			delete(r, "multiplier")
			changed = true
		}
		// A rule that only carried a gateway multiplier has no unit price left;
		// it cannot price a token and is dropped. Explicit rates are kept.
		priced := false
		for _, key := range []string{"input", "output", "cacheRead", "cacheWrite"} {
			if _, ok := r[key]; ok {
				priced = true
			}
		}
		if !priced {
			changed = true
			continue
		}
		if src, _ := r["source"].(string); src != "" && src != "user" {
			r["source"] = "user"
			changed = true
		}
		kept = append(kept, r)
	}
	rules = kept
	if !changed {
		return nil
	}
	next, err := json.Marshal(rules)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE settings SET value=? WHERE key='price-rules'`, string(next))
	return err
}
