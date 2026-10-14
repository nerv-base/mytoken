package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
)

// ErrInvalidRecentWindow reports a CommitManyOutcomeRecent call whose window
// cannot describe a closed interval of time.
var ErrInvalidRecentWindow = errors.New("store: invalid recent window")

// CommitOutcome reports what one CommitManyOutcome call inserted for the first
// time. Writes is aligned with the writes argument: entry i describes writes[i].
type CommitOutcome struct {
	Writes []WriteOutcome
}

// WriteOutcome is one write's newly inserted events and the output tokens they
// carried.
type WriteOutcome struct {
	// NewEvents counts the write's events whose (harness, instance, dedup_key)
	// did not exist before this commit. A key repeated inside the commit is
	// counted once, and a key the index already held is never counted, however
	// much the commit changed its row.
	NewEvents int64
	// NewOutputTokens is the output tokens carried by those new events. It is
	// output only: input, cache and reasoning tokens are not new output.
	NewOutputTokens int64
	// NewRecentOutputTokens is the subset of NewOutputTokens carried by new
	// events whose own timestamp falls inside the caller's window. It is zero
	// for every entry point other than CommitManyOutcomeRecent.
	NewRecentOutputTokens int64
}

// recentWindow is the closed interval (since, at] a recent report counts
// against. ok is false when the caller did not ask for the recent split, in
// which case no event is ever recent and CommitManyOutcome keeps its exact
// previous report.
type recentWindow struct {
	ok    bool
	since time.Time
	at    time.Time
}

// eligible reports whether an event timestamp is demonstrably recent. A zero
// timestamp never is: a missing event time cannot be presumed live.
func (w recentWindow) eligible(ts time.Time) bool {
	if !w.ok || ts.IsZero() {
		return false
	}
	return ts.After(w.since) && !ts.After(w.at)
}

// commitCollector accumulates one write's newly inserted events while its
// transaction is still open.
type commitCollector struct {
	newEvents             int64
	newOutputTokens       int64
	newRecentOutputTokens int64
	recent                recentWindow
}

// keyLookupChunk bounds how many dedup keys one existence query binds.
const keyLookupChunk = 400

// CommitManyOutcomeRecent persists writes exactly like CommitManyOutcome and
// additionally splits each write's newly inserted output tokens by event
// timestamp: NewRecentOutputTokens counts only new events whose own timestamp
// lies inside the closed window (since, at].
//
// The store never reads a clock and never re-dates a row: the caller supplies
// the commit instant. That keeps a live-rate reader honest when a watched file
// has historical records appended to it — the rows are stored and counted as
// new, but their timestamps place them outside the window, so they cannot
// inflate a rate that is supposed to describe the last few seconds of
// ingestion. Events with a zero (missing) timestamp or a timestamp after at are
// never recent, though they are still committed.
//
// since and at must be non-zero and at must be after since; otherwise the
// window cannot be a live observation and ErrInvalidRecentWindow is returned
// before anything is written.
func (s *Store) CommitManyOutcomeRecent(ctx context.Context, writes []Write, since, at time.Time) (CommitOutcome, error) {
	if since.IsZero() || at.IsZero() || !at.After(since) {
		return CommitOutcome{}, ErrInvalidRecentWindow
	}
	return s.commitMany(ctx, writes, true, recentWindow{ok: true, since: since, at: at})
}

// existingKeys returns the subset of keys event_data already holds for h and
// instance, read through the commit's own transaction so it sees rows written
// earlier in that transaction. The lookup runs before the upsert, so a key it
// does not report is exactly a key that upsert is about to insert.
func existingKeys(ctx context.Context, prepare func(string) (*sql.Stmt, error), h model.Harness, instance string, keys []string) (map[string]bool, error) {
	existing := make(map[string]bool, len(keys))
	for start := 0; start < len(keys); start += keyLookupChunk {
		end := start + keyLookupChunk
		if end > len(keys) {
			end = len(keys)
		}
		part := keys[start:end]
		query := "SELECT dedup_key FROM event_data WHERE harness=? AND instance_id=? AND dedup_key IN (" + strings.TrimSuffix(strings.Repeat("?,", len(part)), ",") + ")"
		stmt, err := prepare(query)
		if err != nil {
			return nil, err
		}
		args := make([]any, 0, len(part)+2)
		args = append(args, h, instance)
		for _, key := range part {
			args = append(args, key)
		}
		rows, err := stmt.QueryContext(ctx, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				rows.Close()
				return nil, err
			}
			existing[key] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return existing, nil
}

// addOutput sums output tokens without wrapping past MaxInt64; a wrapped sum
// would reach the rate tracker as a negative delta.
func addOutput(total, add int64) int64 {
	if add <= 0 {
		return total
	}
	if total > math.MaxInt64-add {
		return math.MaxInt64
	}
	return total + add
}
