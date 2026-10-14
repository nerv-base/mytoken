package store

import (
	"context"
	"database/sql"
	"unicode"
	"unicode/utf8"
)

// eventBranchSchema is the additive per-request branch metadata table.
//
// A branch is not a dimension of the usage fact: it is evidence a log stated
// about one request, keyed by the same stable (harness, instance, dedup key)
// identity the fact itself uses. Keeping it in its own table means adding it
// never rewrites event_data, the dimension dictionary, the sessions table or
// any rollup, so an index that gains the table keeps its existing history and
// its existing prices untouched. A request whose log stated no usable branch
// simply has no row here, which readers report as unknown rather than as a
// dropped request. A request whose replays stated contradictory branches keeps
// a row whose branch is empty, which readers report the same way (see
// branchUnknown).
const eventBranchSchema = `CREATE TABLE IF NOT EXISTS event_branches(
 harness TEXT NOT NULL,
 instance_id TEXT NOT NULL,
 dedup_key TEXT NOT NULL,
 branch TEXT NOT NULL,
 PRIMARY KEY(harness,instance_id,dedup_key))`

// branchUnknown is the recorded marker for branch evidence that cannot be
// resolved to one branch: a replay stated a non-empty branch that contradicts
// the branch already recorded for the same request. It is the empty string
// because that is already what readers report for "no branch known" (the branch
// join coalesces a missing row to an empty string), so ambiguity needs no new
// column and no reader change. A row with an empty branch means "evidence was
// recorded and stayed ambiguous"; the absence of a row means "no usable branch
// was ever stated". Both read as unknown, and neither is a guess.
const branchUnknown = ""

// branchSQL records the branch a log stated for one request. It is written in
// the same transaction as the fact row it describes, so metadata and usage can
// never disagree. A replay that restates the same branch is a no-op, so reading
// the same log twice is idempotent. A replay that restates a different
// non-empty branch is a contradiction that processing order cannot resolve, so
// the row becomes branchUnknown and stays there: the update's WHERE clause
// refuses to move a row that is already ambiguous, which is what makes the
// sequence A, B, A read back as unknown instead of toggling to A on the third
// replay. A known branch is never erased by a replay that states nothing,
// because the caller does not execute this statement for an empty or unusable
// branch.
const branchSQL = `INSERT INTO event_branches(harness,instance_id,dedup_key,branch) VALUES(?,?,?,?) ON CONFLICT(harness,instance_id,dedup_key) DO UPDATE SET branch='` + branchUnknown + `' WHERE event_branches.branch!=excluded.branch AND event_branches.branch!='` + branchUnknown + `'`

// branchLabelMax bounds how long a log-provided branch string may be before it
// stops counting as branch evidence. Git itself refuses a ref name longer than
// 255 bytes (git-check-ref-format), so 256 bytes accepts any real branch, even
// one fully qualified as refs/heads/..., while rejecting the arbitrary blob a
// broken parser might place in the branch slot.
const branchLabelMax = 256

// branchEvidence decides whether a log-provided string may be recorded as
// branch evidence. Empty text means "the log did not say"; unusable text
// (invalid UTF-8, control characters, or more than branchLabelMax bytes) means
// "the log said something that is not a branch". Both report false, so the
// caller records no branch and still records the usage fact. Unusable text is
// never repaired or truncated: doing so could map two distinct branches onto
// one label and invent an identity the log never stated.
func branchEvidence(branch string) (string, bool) {
	if branch == "" || len(branch) > branchLabelMax || !utf8.ValidString(branch) {
		return "", false
	}
	for _, r := range branch {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return branch, true
}

// migrateEventBranches creates the additive table. It carries no history and no
// pricing, so it is created inside the existing migration transaction without
// its own backup, digest or reprice.
func migrateEventBranches(tx *sql.Tx) error {
	_, err := tx.Exec(eventBranchSchema)
	return err
}

// pruneEventBranches drops branch rows whose fact row no longer exists. Only a
// history-rebuilding migration can delete a fact row, so this is the
// maintenance step that keeps the table from growing unreachable rows.
func pruneEventBranches(ctx context.Context, tx *sql.Tx) (int64, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM event_branches WHERE NOT EXISTS (SELECT 1 FROM event_data d WHERE d.harness=event_branches.harness AND d.instance_id=event_branches.instance_id AND d.dedup_key=event_branches.dedup_key)`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneEventBranches removes branch metadata whose request is no longer in the
// index and reports how many rows it dropped. Queries never see an orphan row
// (they join from the facts), so this is housekeeping, not a correctness step.
func (s *Store) PruneEventBranches(ctx context.Context) (int64, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n, err := pruneEventBranches(ctx, tx)
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// EventBranchesPresent reports whether the index carries the additive
// event_branches table. It is a pure read, so it works on a read-only handle and
// inside a caller-owned read transaction. An index written before the table
// existed reports false, and a reader then reports every request's branch as
// unknown instead of failing: old databases stay usable.
func EventBranchesPresent(ctx context.Context, tx *sql.Tx) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('event_branches')`).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}
