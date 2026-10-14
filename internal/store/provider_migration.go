package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/zzstar101/mytoken/internal/provider"
)

const providerIdentityVersion = "provider.identity.v1"

// providerIndexInconsistent reports a marker/table mismatch instead of guessing.
// Neither branch is safe to self-heal: running the migration over a partially
// applied index can rewrite history, and skipping it leaves endpoint identity
// unavailable, so both are reported to the operator with the recovery steps.
func providerIndexInconsistent(detail string) error {
	return fmt.Errorf("provider identity index is inconsistent: %s; refusing to guess, to reprice, or to skip the migration. Nothing was rewritten and no backup was replaced. Recovery needs an explicit operator decision", detail)
}

// checkProviderIndex rejects a partially applied identity migration before any
// work starts, so a mismatch is reported with an actionable error instead of
// surfacing later as a raw DDL failure or as a silently skipped migration.
func (s *Store) checkProviderIndex(ctx context.Context, marker string) error {
	var kind string
	err := s.db.QueryRowContext(ctx, "SELECT type FROM sqlite_master WHERE name='provider_endpoints'").Scan(&kind)
	if err == sql.ErrNoRows {
		kind = ""
	} else if err != nil {
		return err
	}
	switch marker {
	case "":
		// Any object with the identity table's name and no marker is a partial or
		// foreign state: a table that the migration cannot safely re-create, or a
		// view/index/trigger that would make the CREATE TABLE fail later, after a
		// backup has already been written. Reject every kind, not just "table".
		if kind != "" {
			return providerIndexInconsistent(fmt.Sprintf("%s provider_endpoints exists but the migration marker %s is absent", kind, providerIdentityVersion))
		}
	case "1":
		if kind != "table" {
			return providerIndexInconsistent("the migration marker " + providerIdentityVersion + " is set but table provider_endpoints is missing")
		}
	default:
		return providerIndexInconsistent(fmt.Sprintf("the migration marker %s has unexpected value %q", providerIdentityVersion, marker))
	}
	return nil
}

// prepareProviderMigration runs before the historical schema migration starts.
// A prior backup is never silently reused: it may belong to a failed attempt
// while newer events have since arrived. Recovery requires an explicit choice.
func (s *Store) prepareProviderMigration(ctx context.Context) (bool, error) {
	done, err := s.Setting(ctx, providerIdentityVersion)
	if err != nil {
		return false, err
	}
	if err = s.checkProviderIndex(ctx, done); err != nil {
		return false, err
	}
	if done == "1" {
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
	// Fresh indexes and in-memory test indexes have no on-disk history to protect.
	if count > 0 && path != "" {
		destination := path + ".pre-provider-v1.db"
		if err = s.Backup(ctx, destination); err != nil {
			return false, fmt.Errorf(`provider migration stopped before any history was migrated: could not write a consistent pre-migration backup to %s: %w

No event, session or price row was changed and the identity marker was not set; the migration transaction never started. The preflight check does not promise a byte-identical file: opening the index has already applied MyToken's bootstrap DDL (journal mode, and any base table or index created if absent), so only the history is guaranteed untouched. An existing %s is never overwritten, reused or deleted automatically, because it may be a partial artifact of an interrupted attempt.

Recovery requires an explicit operator decision. Stop every MyToken process first, then preserve the database together with its -wal and -shm files and the backup, and verify both independently (open the database and run PRAGMA quick_check; open the backup read-only and check it holds the expected history). Only then choose deliberately between repairing the cause of the failed backup, moving the old backup aside to retry, or restoring the backup. Do not restart while this state is unverified.`,
				destination, err, filepath.Base(destination))
		}
	}
	return true, nil
}

// legacyPricedExpression is exactly the predicate migration.go uses to
// initialize `priced` on a genuine pre-0.4 index. A baseline taken before that
// column exists must hash the value the legitimate additive step will write,
// not a hardcoded 0: a row whose historical cost is authoritative (or whose
// estimate is nonzero) is initialized to 1, and comparing it as 0 would report
// the migration's own initialization as history being rewritten.
const legacyPricedExpression = `CASE WHEN log_cost IS NOT NULL OR cost!=0 THEN 1 ELSE 0 END`

// historyDigest checks every request and token class, the exact historical cost
// values and the pricing flags, not floating-point SUMs whose addition order can
// hide changes. The hash stream itself makes dropped or duplicated rows
// detectable, so no separate row count is needed. When `priced` does not exist
// yet (a genuine legacy physical table), the absent flag is normalized to the
// value the additive migration will initialize it to.
func historyDigest(tx *sql.Tx) ([32]byte, error) {
	var out [32]byte
	priced := "priced"
	var hasPriced int
	if err := tx.QueryRow(`SELECT count(*) FROM pragma_table_info('events') WHERE name='priced'`).Scan(&hasPriced); err != nil {
		return out, err
	}
	if hasPriced == 0 {
		priced = legacyPricedExpression
	}
	rows, err := tx.Query(`SELECT harness,dedup_key,timestamp,input,output,cache_read,cache_write,reasoning,cost,log_cost,` + priced + ` FROM events ORDER BY harness,dedup_key`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	for rows.Next() {
		values := make([]any, 11)
		dest := make([]any, len(values))
		for i := range values {
			dest[i] = &values[i]
		}
		if err = rows.Scan(dest...); err != nil {
			return out, err
		}
		if err = encoder.Encode(values); err != nil {
			return out, err
		}
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	copy(out[:], hash.Sum(nil))
	return out, nil
}

func migrateProviderEndpoints(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE provider_endpoints(raw_endpoint TEXT PRIMARY KEY,provider_id TEXT NOT NULL,endpoint TEXT NOT NULL); CREATE INDEX provider_endpoint_id ON provider_endpoints(provider_id)`); err != nil {
		return err
	}
	rows, err := tx.Query("SELECT DISTINCT base_url FROM events WHERE base_url!=''")
	if err != nil {
		return err
	}
	var raw []string
	for rows.Next() {
		var base string
		if err = rows.Scan(&base); err != nil {
			rows.Close()
			return err
		}
		raw = append(raw, base)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, base := range raw {
		endpoint, ok := provider.NormalizeEndpoint(base)
		if !ok {
			continue
		}
		id, _ := provider.EndpointID(base)
		if _, err = tx.Exec("INSERT INTO provider_endpoints(raw_endpoint,provider_id,endpoint) VALUES(?,?,?)", base, id, endpoint); err != nil {
			return err
		}
	}
	_, err = tx.Exec("INSERT INTO settings(key,value) VALUES(?,'1') ON CONFLICT(key) DO UPDATE SET value=excluded.value", providerIdentityVersion)
	return err
}
