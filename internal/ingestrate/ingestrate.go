// Package ingestrate tracks how many output tokens enter the live index over a
// trailing 60-second window. It is the numerator behind a "tokens right now"
// readout (tray, statusline): it measures committed output tokens arriving per
// second, not model generation speed, and it has no clock, scanner, store or UI
// dependency of its own.
//
// # Caller contract
//
// Observe is the only writer. The caller reports a successful, committed output
// token delta together with the time it observed/ingested that delta and an
// explicit classification:
//
//   - ClassLive: the delta is output tokens that just entered the live index.
//     Live reports are counted in the window and mark telemetry as known.
//   - ClassBackfill: the delta came from historical backfill. It is ignored
//     completely: it never counts, never refreshes telemetry and never moves the
//     tracker's clock.
//   - ClassUnknown: the caller cannot tell live from backfill. Same as backfill:
//     ignored, so uncertainty is never promoted to a live rate.
//
// The tracker assumes each committed delta is reported exactly once. It does not
// deduplicate and cannot detect a replayed report, so exactly-once delivery and
// any dedup by request/event identity remain the ingestion layer's job. A delta
// must be output tokens only, already committed to the index, and must not be a
// cumulative counter value: passing a running total double counts.
//
// # Time contract
//
// The tracker has no clock of its own and nothing external to compare a report
// against. It keeps one monotone "present": the newest time it has been told,
// advanced by every accepted live observation and by the now passed to Snapshot.
// Report times must therefore be non-decreasing.
//
// Observe refuses a malformed or late report:
//
//   - a zero time is ErrInvalidTime;
//   - a negative delta is ErrNegativeDelta;
//   - a time before the tracker's present is ErrOutOfOrder, so a late or
//     duplicate report can never rewrite an already-published window. That
//     rejection is strict and is also what keeps the retained timestamps sorted,
//     so the oldest can be discarded in constant time; a caller that may receive
//     late reports must buffer or sort them before calling Observe.
//
// There is deliberately no "too far ahead" rule. A gap longer than Window does
// not prove that the next report is in the future: a paused caller and a bogus
// timestamp are indistinguishable here, and refusing a fresh, valid observation
// after inactivity would silently drop real tokens. A report stamped ahead of a
// later Snapshot's now simply falls outside that snapshot's window and is
// counted once now reaches it. The tracker never invents a future, a stale
// window or a zero from inactivity; a Snapshot whose now precedes the present is
// reported as ReasonClockRegression instead.
//
// Because Observe and Snapshot share the one present, callers must serialize
// them: a Snapshot(now) makes any later Observe stamped before that now out of
// order. Stamp observations with the same clock as now, and pass a now that is
// not later than the observations already accepted.
//
// Before the first accepted observation or Snapshot there is no present, so the
// first stamp the caller supplies establishes the clock.
//
// # Snapshot contract
//
// Snapshot takes an explicit now (so callers and tests never sleep) and reports:
//
//   - Available, plus Reason when it is false;
//   - Tokens: live output tokens observed inside the trailing window;
//   - Window: always Window (60s);
//   - Rate: Tokens divided by the full 60 seconds, never by the time since the
//     tracker started or since the first sample. A burst of 300 tokens 10
//     seconds ago reads 5/s, not 30/s.
//
// When Available is false, Tokens and Rate are zero: the tracker reports unknown
// rather than a number it cannot stand behind. Availability requires fresh live
// telemetry: a live observation, even one with a zero delta (a heartbeat),
// establishes known telemetry, so an actively observed window with no tokens
// reads as an available 0/s instead of an unknown. Without any live signal the
// reason is ReasonNoLiveSignal; once the newest live observation is older than
// the configured freshness timeout the reason is ReasonStale. Freshness is a
// caller choice (positive, at most Window) and not a baked-in UI policy.
//
// # Window semantics
//
// The window is the exact half-open trailing interval (now-Window, now]: a
// sample observed exactly Window before now has left it, one observed a
// nanosecond later has not, and no counted sample is ever Window old or older.
// Sub-second precision is preserved, so samples inside one whole second expire
// at their own timestamps rather than together at a bucket boundary.
//
// The window is kept as a bounded FIFO of distinct commit timestamps, each
// carrying the sum of the nonzero deltas reported at exactly that timestamp
// (equal timestamps coalesce). A zero-delta heartbeat never occupies a slot, and
// expired timestamps are discarded from the front, so memory stays constant and
// no per-event queue grows. If a nonzero delta arrives for a new timestamp while
// all Capacity slots hold samples that would survive expiry, Observe reports
// ErrCapacityExceeded rather than discarding the delta and returning a silently
// partial number. The window then reads ReasonCapacityExceeded until that lost
// timestamp itself leaves the window, after which the tracker recovers on its
// own; capacity therefore recovers as the window slides, provided arrivals
// return to fewer than Capacity distinct timestamps per window.
//
// A token count that cannot be represented is reported as
// ReasonOverflow/ErrOverflow, never wrapped to a negative or partial rate.
// Overflow is sticky: once any live delta could not be represented it can never
// be recovered, so the tracker stays unavailable until it is replaced via New.
//
// # Errors and mutation
//
// An error means the reported delta was not counted: its tokens are not added
// and neither the clock (present) nor telemetry (lastLive) moves. Two errors
// additionally record that the window can no longer be trusted, because a real
// live delta was lost rather than refused as malformed:
//
//   - ErrOverflow permanently disables the tracker (sticky overflow), so from
//     then on it reports ReasonOverflow instead of any number;
//   - ErrCapacityExceeded marks the window incomplete until the unstored
//     timestamp leaves the trailing window.
//
// All other errors (ErrInvalidClass, ErrInvalidTime, ErrNegativeDelta,
// ErrOutOfOrder) are pure refusals that change nothing at all. A
// capacity-refused call also discards no timestamps, so samples a later Snapshot
// still has to count are never lost to a failed Observe.
//
// A Tracker is safe for concurrent use by multiple goroutines.
package ingestrate

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

// Window is the fixed trailing interval the rate always averages over.
const Window = 60 * time.Second

// Capacity is the number of distinct nonzero commit timestamps retained for the
// trailing window. It bounds a Tracker's memory at a fixed size. A caller whose
// committed output arrives at more than Capacity distinct timestamps inside
// Window receives ErrCapacityExceeded for the surplus, and the window reads as
// unavailable until the loss ages out.
const Capacity = 4096

// Class describes what an observed output-token delta is.
type Class string

const (
	// ClassLive is a committed delta of output tokens entering the live index.
	ClassLive Class = "live"
	// ClassBackfill is a historical delta; it never counts as live.
	ClassBackfill Class = "backfill"
	// ClassUnknown is a delta the caller cannot classify; it never counts as
	// live.
	ClassUnknown Class = "unknown"
)

// Reason explains why a Snapshot is unavailable. The empty Reason means the
// snapshot is available.
type Reason string

const (
	// ReasonNoLiveSignal means no live observation has ever been reported.
	ReasonNoLiveSignal Reason = "no_live_signal"
	// ReasonStale means the newest live observation is older than the
	// configured freshness timeout.
	ReasonStale Reason = "stale_telemetry"
	// ReasonClockRegression means the snapshot's now precedes the tracker's
	// present, so no trustworthy window can be reported.
	ReasonClockRegression Reason = "clock_regression"
	// ReasonOverflow means a token count could not be represented; reporting a
	// partial or wrapped rate would be a fabrication. It is sticky.
	ReasonOverflow Reason = "counter_overflow"
	// ReasonCapacityExceeded means a live delta could not be stored because the
	// window already holds Capacity distinct timestamps, so no complete window
	// sum can be reported until that lost timestamp leaves the window.
	ReasonCapacityExceeded Reason = "capacity_exceeded"
)

// Errors returned by Observe. A returned error means the reported delta was not
// counted: its tokens were not added and the clock and telemetry did not move.
// ErrOverflow and ErrCapacityExceeded additionally make the tracker unavailable,
// as documented on the package.
var (
	ErrInvalidClass     = errors.New("ingestrate: invalid observation class")
	ErrInvalidTime      = errors.New("ingestrate: zero observation time")
	ErrNegativeDelta    = errors.New("ingestrate: negative token delta")
	ErrOutOfOrder       = errors.New("ingestrate: observation time precedes the tracker's present")
	ErrOverflow         = errors.New("ingestrate: token counter overflow")
	ErrCapacityExceeded = errors.New("ingestrate: too many distinct timestamps in the window")
)

// Snapshot is a read-only answer about the trailing window.
type Snapshot struct {
	// Available reports whether live telemetry is fresh enough to trust.
	Available bool
	// Reason is empty when Available, otherwise it explains the gap.
	Reason Reason
	// Tokens is the live output-token count inside the trailing window. It is
	// zero whenever Available is false.
	Tokens int64
	// Window is the trailing interval the count and rate cover; always Window.
	Window time.Duration
	// Rate is Tokens per second over the whole Window. It is zero whenever
	// Available is false.
	Rate float64
}

// entry is one distinct commit timestamp and the live output tokens reported at
// exactly that timestamp. Entries are held oldest first and never empty.
type entry struct {
	at     time.Time
	tokens int64
}

// Tracker accumulates live output tokens over the trailing window. The zero
// Tracker is not usable; construct one with New.
type Tracker struct {
	mu        sync.Mutex
	freshness time.Duration
	ring      [Capacity]entry
	head      int // ring index of the oldest retained timestamp
	count     int // number of retained timestamps
	present   time.Time
	lastLive  time.Time
	overflow  bool
	lostAt    time.Time // newest timestamp whose nonzero delta could not be stored
}

// New returns a Tracker whose telemetry is trusted while the newest live
// observation is at most freshness old. freshness must be positive and at most
// Window; the caller picks it from its own ingestion cadence.
func New(freshness time.Duration) (*Tracker, error) {
	if freshness <= 0 {
		return nil, fmt.Errorf("ingestrate: freshness must be positive, got %s", freshness)
	}
	if freshness > Window {
		return nil, fmt.Errorf("ingestrate: freshness %s exceeds the %s window", freshness, Window)
	}
	return &Tracker{freshness: freshness}, nil
}

// Observe reports one committed output-token delta observed at at and classified
// as class. Backfill and unknown reports are ignored and return nil. A returned
// error means the delta was not counted: its tokens were not added and neither
// the clock nor telemetry moved. ErrOverflow and ErrCapacityExceeded also record
// that the window is no longer trustworthy, as documented on the package.
func (t *Tracker) Observe(delta int64, at time.Time, class Class) error {
	switch class {
	case ClassLive:
	case ClassBackfill, ClassUnknown:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidClass, class)
	}
	if at.IsZero() {
		return ErrInvalidTime
	}
	if delta < 0 {
		return fmt.Errorf("%w: %d", ErrNegativeDelta, delta)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.overflow {
		return ErrOverflow
	}
	if !t.present.IsZero() && at.Before(t.present) {
		return fmt.Errorf("%w: %s before %s", ErrOutOfOrder, at, t.present)
	}
	// Timestamps this call's own time would expire. A refused call discards
	// nothing, so the capacity decision is made against the post-expiry count
	// without mutating the ring first: pruning here would be unsafe, because a
	// refused call leaves present unchanged and a later Snapshot whose now still
	// precedes at would have to count those timestamps.
	cutoff := at.Add(-Window)
	expired := t.expiredLocked(cutoff)
	if delta > 0 && t.count-expired == Capacity {
		// A surviving tail can absorb this delta without a new slot.
		coalesces := t.count > expired && t.ring[(t.head+t.count-1)%Capacity].at.Equal(at)
		if !coalesces {
			if at.After(t.lostAt) {
				t.lostAt = at
			}
			return fmt.Errorf("%w: %d distinct timestamps in the last %s", ErrCapacityExceeded, Capacity, Window)
		}
	}
	t.pruneLocked(cutoff)
	if delta == 0 {
		// A heartbeat is known telemetry, not a sample: it refreshes freshness
		// and moves the clock without occupying a slot.
		t.present = at
		t.lastLive = at
		return nil
	}
	if t.count > 0 {
		if tail := &t.ring[(t.head+t.count-1)%Capacity]; tail.at.Equal(at) {
			if delta > math.MaxInt64-tail.tokens {
				t.overflow = true
				return fmt.Errorf("%w: %d + %d at %s", ErrOverflow, tail.tokens, delta, at)
			}
			tail.tokens += delta
			t.present = at
			t.lastLive = at
			return nil
		}
	}
	if t.count == Capacity {
		if at.After(t.lostAt) {
			t.lostAt = at
		}
		return fmt.Errorf("%w: %d distinct timestamps in the last %s", ErrCapacityExceeded, Capacity, Window)
	}
	t.ring[(t.head+t.count)%Capacity] = entry{at: at, tokens: delta}
	t.count++
	t.present = at
	t.lastLive = at
	return nil
}

// Snapshot answers for the trailing window ending at now, which must not precede
// the tracker's present. Snapshot never counts a token observed after now.
func (t *Tracker) Snapshot(now time.Time) Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	snap := Snapshot{Window: Window}
	if t.overflow {
		snap.Reason = ReasonOverflow
		return snap
	}
	if now.Before(t.present) {
		snap.Reason = ReasonClockRegression
		return snap
	}
	t.present = now
	t.pruneLocked(now.Add(-Window))
	if t.lastLive.IsZero() {
		snap.Reason = ReasonNoLiveSignal
		return snap
	}
	if now.Sub(t.lastLive) > t.freshness {
		snap.Reason = ReasonStale
		return snap
	}
	if !t.lostAt.IsZero() && now.Before(t.lostAt.Add(Window)) {
		snap.Reason = ReasonCapacityExceeded
		return snap
	}
	tokens, ok := t.sumLocked()
	if !ok {
		t.overflow = true
		snap.Reason = ReasonOverflow
		return snap
	}
	snap.Available = true
	snap.Tokens = tokens
	snap.Rate = float64(tokens) / Window.Seconds()
	return snap
}

// expiredLocked counts the leading timestamps at or before cutoff, i.e. the
// timestamps pruneLocked would discard.
func (t *Tracker) expiredLocked(cutoff time.Time) int {
	n := 0
	for n < t.count && !t.ring[(t.head+n)%Capacity].at.After(cutoff) {
		n++
	}
	return n
}

// pruneLocked discards leading timestamps at or before cutoff, which is Window
// before some reference time, leaving only timestamps strictly inside the
// trailing window. Because entries are ordered oldest first and strictly
// increasing, this is the exact expiry rule and it costs one step per discarded
// entry.
func (t *Tracker) pruneLocked(cutoff time.Time) {
	for t.count > 0 {
		if t.ring[t.head].at.After(cutoff) {
			return
		}
		t.ring[t.head] = entry{}
		t.head = (t.head + 1) % Capacity
		t.count--
	}
}

// sumLocked totals the retained entries. After pruneLocked every retained
// timestamp is inside the trailing window, so this is the exact window sum. It
// reports false on overflow instead of returning a wrapped total.
func (t *Tracker) sumLocked() (int64, bool) {
	var total int64
	for i := 0; i < t.count; i++ {
		tokens := t.ring[(t.head+i)%Capacity].tokens
		if tokens > math.MaxInt64-total {
			return 0, false
		}
		total += tokens
	}
	return total, true
}
