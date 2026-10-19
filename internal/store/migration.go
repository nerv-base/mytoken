package store

import (
	"context"
	"fmt"
)

func (s *Store) migrate() error {
	identityMigration, err := s.prepareProviderMigration(context.Background())
	if err != nil {
		return err
	}
	// The endpoint-aware rollups are derived from event_dimensions and the cache
	// columns, so they are only correct once the rest of the schema exists. They
	// ride along in this transaction when the identity migration runs here, and
	// get their own backed-up transaction on an index that already carries the
	// identity marker.
	rollupMigration, err := s.prepareProviderRollup(context.Background(), identityMigration)
	if err != nil {
		return err
	}
	// The instance dimension rebuilds primary keys, the dictionary uniqueness key,
	// sessions, cursors and every derived rollup, so it runs last: it needs the
	// base schema and both endpoint rollups to exist. It takes its own fixed
	// pre-migration backup when the index already holds history.
	instanceMigration, err := s.prepareInstanceMigration(context.Background())
	if err != nil {
		return err
	}
	// An index that already carries the instance dimension can still be missing
	// one of the incremental steps (signals, cache ledger, endpoint rollups). The
	// repair has to reproduce that index's own generation, otherwise it would
	// recreate the compatibility view or a rollup without instance_id and leave
	// the file internally inconsistent. A genuine legacy index reports false, so
	// the byte-for-byte legacy generation is preserved.
	instanceAware, err := s.hasInstanceColumn(context.Background(), "event_data")
	if err != nil {
		return err
	}
	sc := legacyRollupSchema
	if instanceAware {
		sc = instanceRollupSchema
	}
	// 0.5 removes the relay and cache-ledger tables and columns. Keep exactly one
	// pre-migration copy before the destructive step, then do the whole cleanup
	// in the migration transaction below.
	cleanup, err := s.needsCleanup(context.Background())
	if err != nil {
		return err
	}
	if cleanup {
		if err = s.backupBeforeCleanup(context.Background()); err != nil {
			return err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The baseline is captured before the additive columns are initialized. On a
	// genuine legacy index those UPDATE statements are the first writes to the
	// history table, so a trigger attached to events can rewrite historical
	// tokens or cost while raw_project/priced are filled in. A baseline taken
	// after them would already contain the rewritten values and could not prove
	// that the original history survived. historyDigest normalizes the absent
	// priced column to the value the additive step is about to initialize it to,
	// so this ordering does not mistake that legitimate step for a rewrite. The
	// instance migration re-keys the same facts, so it is covered by the same
	// per-request digest.
	var before [32]byte
	if identityMigration || rollupMigration || instanceMigration {
		if before, err = historyDigest(tx); err != nil {
			return err
		}
	}
	for _, table := range []string{"events", "sessions"} {
		rows, err := tx.Query("PRAGMA table_info(" + table + ")")
		if err != nil {
			return err
		}
		columns := map[string]bool{}
		for rows.Next() {
			var id, notnull, pk int
			var name, typ string
			var def any
			if err = rows.Scan(&id, &name, &typ, &notnull, &def, &pk); err != nil {
				rows.Close()
				return err
			}
			columns[name] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		additions := map[string]string{"raw_project": "TEXT NOT NULL DEFAULT ''"}
		if table == "events" {
			additions["priced"] = "INTEGER NOT NULL DEFAULT 0"
		}
		for name, decl := range additions {
			if !columns[name] {
				if _, err = tx.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, name, decl)); err != nil {
					return err
				}
				if name == "raw_project" {
					_, err = tx.Exec("UPDATE " + table + " SET raw_project=project")
				} else {
					_, err = tx.Exec("UPDATE events SET priced=1 WHERE log_cost IS NOT NULL OR cost!=0")
				}
				if err != nil {
					return err
				}
			}
		}
		if table == "sessions" {
			if _, err = tx.Exec("CREATE INDEX IF NOT EXISTS " + table + "_raw_project ON " + table + "(raw_project)"); err != nil {
				return err
			}
		}
	}
	// The baseline was taken before the additive columns existed (see above), so
	// the digest below compares the pre-migration history with the migrated one.
	compacted, err := migrateCompact(tx)
	if err != nil {
		return err
	}
	// The per-request branch metadata is additive and starts empty: it carries
	// no history and no pricing, so it is created here without a backup, a
	// digest or a reprice. A request whose log never stated a usable branch
	// simply has no row, and readers report that as unknown; a request whose
	// replays stated contradictory branches keeps a row with an empty branch,
	// which readers also report as unknown.
	if err = migrateEventBranches(tx); err != nil {
		return err
	}
	cacheWrite1h, err := migrateCacheWrite1h(tx)
	if err != nil {
		return err
	}
	if err = migrateSignals(tx, sc); err != nil {
		return err
	}
	if cacheWrite1h {
		if err = rebuildEventsView(tx, sc); err != nil {
			return err
		}
	}
	if err = migrateEventIndexes(tx); err != nil {
		return err
	}
	if err = migrateDaily(tx, sc); err != nil {
		return err
	}
	if identityMigration {
		if err = migrateProviderEndpoints(tx); err != nil {
			return err
		}
	}
	if rollupMigration {
		if err = migrateProviderRollup(tx, sc); err != nil {
			return err
		}
	}
	// The instance dimension is introduced last, after every derived table exists,
	// so one transaction re-keys facts, dictionary, sessions, cursors and all four
	// rollups together.
	if instanceMigration {
		if err = s.migrateInstance(context.Background(), tx); err != nil {
			return err
		}
	}
	if cleanup {
		if err = dropObsolete(context.Background(), tx, sc); err != nil {
			return err
		}
	}
	// A history-rebuilding migration re-keys the fact rows, which can strand the
	// branch metadata written against the old keys. Pruning here (and only here)
	// keeps the table bounded without paying for it on an ordinary open.
	if identityMigration || rollupMigration || instanceMigration {
		if _, err = pruneEventBranches(context.Background(), tx); err != nil {
			return err
		}
	}
	// Quota values are server facts now, cached under quota.plan.*. Rows an older
	// build published from local rollouts under quota.codex.* would otherwise
	// survive forever and could be mistaken for a quota again, so they are
	// deleted once, in the migration transaction.
	if _, err = tx.Exec(`DELETE FROM settings WHERE key LIKE 'quota.codex.%'`); err != nil {
		return err
	}
	if identityMigration || rollupMigration || instanceMigration {
		after, err := historyDigest(tx)
		if err != nil {
			return err
		}
		if before != after {
			return fmt.Errorf("migration history invariant failed; transaction rolled back")
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if compacted || cleanup {
		if _, err = s.db.Exec("VACUUM"); err != nil {
			return err
		}
	}
	s.projects = newProjectResolver()
	if err = s.EnsureLocalDays(context.Background()); err != nil {
		return err
	}
	return s.NormalizeProjects(context.Background())
}
