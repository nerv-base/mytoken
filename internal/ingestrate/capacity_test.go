package ingestrate

import (
	"errors"
	"testing"
	"time"
)

// fillCapacity stores Capacity distinct in-window timestamps, one millisecond
// apart, each carrying one token.
func fillCapacity(t *testing.T, tr *Tracker) {
	t.Helper()
	for i := 0; i < Capacity; i++ {
		mustObserve(t, tr, 1, base.Add(time.Duration(i)*time.Millisecond), ClassLive)
	}
}

// A nonzero delta for a new timestamp while every slot is taken is refused with
// an explicit error, makes the window unavailable instead of silently partial,
// and recovers once the unstored timestamp leaves the window.
func TestCapacityRefusalAndRecovery(t *testing.T) {
	tr := newTracker(t, Window)
	fillCapacity(t, tr)
	lostAt := base.Add(Capacity * time.Millisecond)

	err := tr.Observe(1, lostAt, ClassLive)
	if !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("Observe over capacity = %v, want ErrCapacityExceeded", err)
	}

	// Still inside the window: the tracker must not report the partial sum of
	// the 4096 stored timestamps as if it were the whole window.
	checkSnapshot(t, tr.Snapshot(lostAt), Snapshot{Reason: ReasonCapacityExceeded, Window: Window})

	// One nanosecond before the loss leaves the window the window is still
	// incomplete, even though a heartbeat keeps telemetry fresh.
	justBefore := lostAt.Add(Window - time.Nanosecond)
	mustObserve(t, tr, 0, justBefore, ClassLive)
	checkSnapshot(t, tr.Snapshot(justBefore), Snapshot{Reason: ReasonCapacityExceeded, Window: Window})

	// Exactly Window after the loss, the lost timestamp is outside the trailing
	// window and the tracker recovers on its own.
	atBoundary := lostAt.Add(Window)
	mustObserve(t, tr, 0, atBoundary, ClassLive)
	checkSnapshot(t, tr.Snapshot(atBoundary), Snapshot{Available: true, Window: Window})
}

// Expired timestamps are discarded before the capacity check, so capacity
// recovers as the window slides rather than refusing forever.
func TestCapacityDiscardsExpiredBeforeRefusing(t *testing.T) {
	tr := newTracker(t, Window)
	fillCapacity(t, tr)

	// Window (plus a nanosecond) after the oldest stored timestamp, every stored
	// timestamp is outside the new window and the slot is free again.
	at := base.Add((Capacity - 1) * time.Millisecond).Add(Window + time.Nanosecond)
	if err := tr.Observe(5, at, ClassLive); err != nil {
		t.Fatalf("Observe after the window slid = %v, want nil", err)
	}
	checkSnapshot(t, tr.Snapshot(at), Snapshot{Available: true, Window: Window, Tokens: 5, Rate: 5.0 / 60})
}

// A zero-delta heartbeat is telemetry, not a sample: it is accepted while the
// window is full and never occupies a slot.
func TestHeartbeatDoesNotConsumeCapacity(t *testing.T) {
	tr := newTracker(t, Window)
	fillCapacity(t, tr)

	heartbeatAt := base.Add(Capacity * time.Millisecond)
	mustObserve(t, tr, 0, heartbeatAt, ClassLive)
	// Nothing has been lost yet, so the stored window is still exact and the
	// heartbeat itself contributed no tokens.
	checkSnapshot(t, tr.Snapshot(heartbeatAt), Snapshot{
		Available: true,
		Window:    Window,
		Tokens:    Capacity,
		Rate:      float64(Capacity) / 60,
	})

	// The heartbeat did not take a slot, so a nonzero delta at that timestamp is
	// still a new timestamp with no room.
	if err := tr.Observe(1, heartbeatAt, ClassLive); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("Observe at heartbeat timestamp = %v, want ErrCapacityExceeded", err)
	}
}

// Coalescing is by exact timestamp, so many deltas reported at one timestamp
// consume a single slot and never hit the capacity limit.
func TestSameTimestampCoalescingDoesNotConsumeCapacity(t *testing.T) {
	tr := newTracker(t, Window)
	for i := 0; i < Capacity+10; i++ {
		mustObserve(t, tr, 1, base, ClassLive)
	}
	checkSnapshot(t, tr.Snapshot(base), Snapshot{
		Available: true,
		Window:    Window,
		Tokens:    Capacity + 10,
		Rate:      float64(Capacity+10) / 60,
	})
}
