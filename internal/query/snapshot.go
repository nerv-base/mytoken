package query

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/zzstar101/mytoken/internal/store"
)

// Envelope is a consistent multi-field read of the index: totals, the cache
// ledger and the freshness signals all come from one read-only transaction, so
// a snapshot can never pair today's totals with yesterday's cache numbers.
//
// It is additive to Service: existing callers keep using Totals/CacheTotals.
type Envelope struct {
	Totals    Totals
	Freshness Freshness
	// Groups holds the extra rankings requested through EnvelopeOptions, in the
	// order they were asked for. Empty unless options requested them.
	Groups []Group
	// Quota holds the persisted Codex quota observations when EnvelopeOptions
	// asked for them, read in the same transaction as the totals.
	Quota []QuotaObservation
}

// GroupDim names one extra ranking an Envelope can carry.
type GroupDim string

const (
	// GroupByProject ranks the filter's events by canonical project identity.
	// The key is the stored `project` dimension, which the store already
	// resolved to the repository root when the row was written; nothing here
	// re-derives or guesses a project.
	GroupByProject GroupDim = "project"
	// GroupBySession ranks the filter's root sessions by usage.
	GroupBySession GroupDim = "session"
	// GroupByBranch ranks the filter's events by repository and the branch the
	// log itself recorded for each request. The branch is metadata attached to a
	// request, never the working tree's current HEAD, so a checkout that moves
	// does not relabel history. The key is the canonical project joined with the
	// branch (see BranchKey): a bare branch name is not a group because two
	// repositories can share it. A request whose log stated no branch is kept as
	// an explicit unknown bucket.
	GroupByBranch GroupDim = "branch"
)

// Group is one extra ranking computed inside the envelope's transaction, so it
// always describes exactly the same events and filters as Totals.
//
// Buckets is set for GroupByProject and Sessions for GroupBySession. Total is
// the number of sessions the ranking was cut from, which is how a truncated
// session list stays honest about what it left out.
type Group struct {
	Dim      GroupDim     `json:"dim"`
	Buckets  []Bucket     `json:"buckets,omitempty"`
	Sessions []SessionRow `json:"sessions,omitempty"`
	Total    int          `json:"total,omitempty"`
}

// EnvelopeOptions requests extra rankings. The zero value reads exactly the
// envelope Totals/Cache/Freshness, so existing callers are unaffected.
type EnvelopeOptions struct {
	// Groups lists the dimensions to rank, in output order. An unknown
	// dimension is an error rather than a silently empty ranking.
	Groups []GroupDim
	// SessionLimit caps a GroupBySession ranking; 0 means every root session.
	SessionLimit int
	// Quota reads the persisted Codex quota observations for f.InstanceIDs in
	// the same transaction, so a quota view and the totals it accompanies
	// always describe one index state. Off by default: a caller that does not
	// display quota does not pay for the read.
	Quota bool
}

// ReadEnvelope reads one consistent envelope for f. The handle may be a
// read-only store (store.OpenReadOnly) — nothing here writes.
func ReadEnvelope(ctx context.Context, st *store.Store, f Filter, opts ...EnvelopeOptions) (Envelope, error) {
	var out Envelope
	var o EnvelopeOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	s := &service{st: st}
	tx, err := st.ReadDB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	d, err := s.loadTx(ctx, tx, f)
	if err != nil {
		return out, err
	}
	out.Totals = totalsOf(d)
	if out.Freshness, err = freshnessTx(ctx, tx); err != nil {
		return out, err
	}
	if out.Groups, err = s.groupsOf(ctx, tx, d, f, o); err != nil {
		return out, err
	}
	if o.Quota {
		if out.Quota, err = readQuotaTx(ctx, tx, f.InstanceIDs); err != nil {
			return out, err
		}
	}
	return out, tx.Commit()
}

// groupsOf ranks the requested dimensions from an already-loaded data set. It
// takes the read transaction because the branch dimension lives in its own
// metadata table and needs a second read of the same snapshot; every other
// dimension is answered from d alone.
func (s *service) groupsOf(ctx context.Context, tx *sql.Tx, d data, f Filter, o EnvelopeOptions) ([]Group, error) {
	if len(o.Groups) == 0 {
		return nil, nil
	}
	out := make([]Group, 0, len(o.Groups))
	for _, dim := range o.Groups {
		switch dim {
		case GroupByProject:
			out = append(out, Group{Dim: dim, Buckets: bucketsOf(d, "project")})
		case GroupBySession:
			rows, total := sessionsOf(d, SortTokens, o.SessionLimit, 0)
			out = append(out, Group{Dim: dim, Sessions: rows, Total: total})
		case GroupByBranch:
			bd, err := s.loadTx(ctx, tx, f, loadMode{branch: true})
			if err != nil {
				return nil, err
			}
			// The branch read selects its own columns, so reuse the session
			// metadata already loaded: root-session counting must not depend on
			// which dimension happened to be read.
			bd.meta = d.meta
			out = append(out, Group{Dim: dim, Buckets: bucketsOf(bd, "branch")})
		default:
			return nil, fmt.Errorf("query: unknown group dimension %q", dim)
		}
	}
	return out, nil
}

// Freshness is derived from read-only signals only. Every field is optional:
// an index that never recorded a timezone or holds no events reports zero
// values instead of inventing a default.
type Freshness struct {
	// EventsThrough is max(events.timestamp); nil when the index holds no events.
	EventsThrough *time.Time
	// SessionsThrough is max(sessions.updated_at); nil when no session has one.
	SessionsThrough *time.Time
	// RollupZone is the persisted daily_timezone setting; "" when never recorded.
	RollupZone string
	// LocalZone is the zone this process would write rollups in.
	LocalZone string
	// RollupsCurrent reports whether the persisted rollup zone matches LocalZone.
	// A never-recorded zone is reported as current: nothing to invalidate yet.
	RollupsCurrent bool
	// StatsTimezone is the persisted stats_timezone setting, empty when absent.
	StatsTimezone string
	// StatsTimezoneRecorded is false when no stats_timezone key exists, which is
	// different from a recorded empty value and must be reported as such.
	StatsTimezoneRecorded bool
}

func freshnessTx(ctx context.Context, tx *sql.Tx) (Freshness, error) {
	fr := Freshness{LocalZone: store.LocalDayZone(), RollupsCurrent: true}
	var zone string
	err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='daily_timezone'`).Scan(&zone)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fr, err
	}
	fr.RollupZone = zone
	fr.RollupsCurrent = zone == "" || zone == fr.LocalZone
	var stats string
	err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='stats_timezone'`).Scan(&stats)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fr, err
	}
	if err == nil {
		fr.StatsTimezone = stats
		fr.StatsTimezoneRecorded = true
	}
	var events sql.NullInt64
	if err = tx.QueryRowContext(ctx, `SELECT max(timestamp) FROM events`).Scan(&events); err != nil {
		return fr, err
	}
	if events.Valid {
		t := time.Unix(0, events.Int64).UTC()
		fr.EventsThrough = &t
	}
	var sessions sql.NullInt64
	if err = tx.QueryRowContext(ctx, `SELECT max(updated_at) FROM sessions`).Scan(&sessions); err != nil {
		return fr, err
	}
	if sessions.Valid && sessions.Int64 != 0 {
		t := time.Unix(0, sessions.Int64).UTC()
		fr.SessionsThrough = &t
	}
	return fr, nil
}
