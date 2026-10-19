package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/zzstar101/mytoken/internal/model"
)

// HarnessParserVersionKey is the settings key recording which parser generation
// last ingested a harness's sources.
func HarnessParserVersionKey(h model.Harness) string {
	return "harness." + string(h) + ".parser_version"
}

// HarnessParserVersion reports the parser generation that last ingested h. It
// returns 0 when this database never recorded one: a database written before
// parser versions existed, or a harness that has not been seen yet. An
// unreadable marker is also reported as 0 because re-reading the harness is
// safe and rewrites the marker.
func (s *Store) HarnessParserVersion(ctx context.Context, h model.Harness) (int, error) {
	raw, err := s.Setting(ctx, HarnessParserVersionKey(h))
	if err != nil || raw == "" {
		return 0, err
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, nil
	}
	return v, nil
}

// RecordHarnessParserVersion records version for h without touching its rows.
// It is for a database that predates parser versions: those rows were written
// by InitialParserVersion, so when that is still the current generation there is
// nothing to re-read, only a marker to adopt.
func (s *Store) RecordHarnessParserVersion(ctx context.Context, h model.Harness, version int) error {
	if strings.TrimSpace(string(h)) == "" {
		return errors.New("store: empty harness")
	}
	if version <= 0 {
		return fmt.Errorf("store: invalid parser version %d for harness %q", version, h)
	}
	return s.SetSetting(ctx, HarnessParserVersionKey(h), strconv.Itoa(version))
}

// ResetScope names the rows a parser-version reset may delete: the sources the
// caller verified still exist and will re-read, and the sessions their events
// can carry. A source missing from Paths is treated as gone from disk: its
// events and its cursor stay. A session missing from Sessions is treated as
// belonging to such a source: its events stay.
//
// The zero value is a valid scope for a harness with no source left on disk. It
// deletes nothing and only records the marker, because there is nothing the
// scan can replace.
type ResetScope struct {
	Paths    []string
	Sessions []string
}

// sqlPlaceholders renders "?,?,?" for an IN list. Callers must not pass n<=0.
func sqlPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// resetChunk bounds the variables one DELETE binds. A parser generation can be
// raised long after a harness accumulated thousands of sources, and SQLite's
// host-parameter limit is a build option rather than a guarantee, so the scope
// is applied in batches inside the same transaction.
const resetChunk = 500

// ResetHarness removes the rows a new parser generation is about to replace and
// records that generation. Only rows named by scope go: a source the harness
// rotated away is history whose only copy is this database, so its events and
// cursor survive as stale-format facts rather than disappearing.
//
// The caller is responsible for computing scope from a discovery that still
// sees the sources. It then lets the ordinary scan read them again, exactly as
// on a fresh database.
//
// The deletes and the marker are one transaction, so a crash leaves either the
// old rows with the old marker (and the next start resets again) or the scoped
// state with the new marker (and the next scan re-reads the sources whose
// cursors went). It can never leave a recorded version next to rows that were
// neither kept deliberately nor about to be replaced.
//
// The event rows are the source of truth: daily_usage and provider_daily are
// maintained by DELETE triggers on event_data, and branch metadata is pruned
// after the facts disappear. Sessions are derived from events, so a session row
// is dropped only when no event of it remains; a session still backing a fact of
// a vanished source keeps its title and timestamps. event_dimensions rows no
// longer referenced by a fact are dropped too.
func (s *Store) ResetHarness(ctx context.Context, h model.Harness, version int, scope ResetScope) error {
	if strings.TrimSpace(string(h)) == "" {
		return errors.New("store: empty harness")
	}
	if version <= 0 {
		return fmt.Errorf("store: invalid parser version %d for harness %q", version, h)
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for start := 0; start < len(scope.Sessions); start += resetChunk {
		end := start + resetChunk
		if end > len(scope.Sessions) {
			end = len(scope.Sessions)
		}
		chunk := scope.Sessions[start:end]
		args := make([]any, 0, 2+len(chunk))
		args = append(args, string(h), string(h))
		for _, session := range chunk {
			args = append(args, session)
		}
		query := `DELETE FROM event_data WHERE harness=? AND dimension_id IN (` +
			`SELECT id FROM event_dimensions WHERE harness=? AND session_id IN (` +
			sqlPlaceholders(len(chunk)) + `))`
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE harness=? AND NOT EXISTS (`+
		`SELECT 1 FROM event_data e JOIN event_dimensions d ON d.id=e.dimension_id `+
		`WHERE e.harness=? AND d.harness=? AND d.instance_id=sessions.instance_id AND d.session_id=sessions.session_id)`,
		string(h), string(h), string(h)); err != nil {
		return err
	}
	for start := 0; start < len(scope.Paths); start += resetChunk {
		end := start + resetChunk
		if end > len(scope.Paths) {
			end = len(scope.Paths)
		}
		chunk := scope.Paths[start:end]
		args := make([]any, 0, 1+len(chunk))
		args = append(args, string(h))
		for _, path := range chunk {
			args = append(args, path)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM cursors WHERE harness=? AND path IN (`+sqlPlaceholders(len(chunk))+`)`, args...); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM event_dimensions WHERE harness=? AND NOT EXISTS (SELECT 1 FROM event_data d WHERE d.dimension_id=event_dimensions.id)`, string(h)); err != nil {
		return err
	}
	if _, err := pruneEventBranches(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, HarnessParserVersionKey(h), strconv.Itoa(version)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.projects = newProjectResolver()
	s.notify()
	return nil
}
