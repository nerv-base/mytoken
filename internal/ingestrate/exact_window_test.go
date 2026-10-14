package ingestrate

import (
	"errors"
	"math"
	"testing"
	"time"
)

// The window is the exact half-open trailing interval (now-Window, now], not the
// 60 whole seconds ending at floor(now). A sample observed 900ms into a second
// is 59.100s old when now is exactly 60.000s after the sample's second began, so
// it is still inside the window.
func TestExactTrailingWindowSubsecondExpiry(t *testing.T) {
	tr := newTracker(t, Window)
	mustObserve(t, tr, 600, base.Add(900*time.Millisecond), ClassLive)

	checkSnapshot(t, tr.Snapshot(base.Add(Window)), Snapshot{
		Available: true,
		Window:    Window,
		Tokens:    600,
		Rate:      10,
	})

	// Exactly Window old: the interval is open at its left edge, so the sample
	// has just left while telemetry is still fresh.
	checkSnapshot(t, tr.Snapshot(base.Add(Window+900*time.Millisecond)), Snapshot{
		Available: true,
		Window:    Window,
	})

	// One nanosecond past that, freshness lapses too.
	checkSnapshot(t, tr.Snapshot(base.Add(Window+900*time.Millisecond+time.Nanosecond)), Snapshot{
		Reason: ReasonStale,
		Window: Window,
	})
}

// Two samples in the same whole second expire at their own timestamps, not
// together with the second's bucket.
func TestExactSameSecondExpiry(t *testing.T) {
	tr := newTracker(t, Window)
	mustObserve(t, tr, 300, base.Add(100*time.Millisecond), ClassLive)
	mustObserve(t, tr, 300, base.Add(900*time.Millisecond), ClassLive)

	// now-60s = base+0.5s: the 0.1s sample has expired, the 0.9s one has not.
	checkSnapshot(t, tr.Snapshot(base.Add(60*time.Second+500*time.Millisecond)), Snapshot{
		Available: true,
		Window:    Window,
		Tokens:    300,
		Rate:      5,
	})
}

// The oldest and newest samples of one whole second are individually retained:
// both are inside a window that starts just before the older one.
func TestExactSameSecondOldestNewest(t *testing.T) {
	tr := newTracker(t, Window)
	mustObserve(t, tr, 300, base.Add(100*time.Millisecond), ClassLive)
	mustObserve(t, tr, 300, base.Add(900*time.Millisecond), ClassLive)

	checkSnapshot(t, tr.Snapshot(base.Add(950*time.Millisecond)), Snapshot{
		Available: true,
		Window:    Window,
		Tokens:    600,
		Rate:      10,
	})

	// now-60s = base+0.05s: 0.1s and 0.9s are 59.950s and 59.150s old.
	checkSnapshot(t, tr.Snapshot(base.Add(60*time.Second+50*time.Millisecond)), Snapshot{
		Available: true,
		Window:    Window,
		Tokens:    600,
		Rate:      10,
	})

	// now-60s = base+0.1s: the older sample is exactly Window old and has left.
	checkSnapshot(t, tr.Snapshot(base.Add(60*time.Second+100*time.Millisecond)), Snapshot{
		Available: true,
		Window:    Window,
		Tokens:    300,
		Rate:      5,
	})
}

// A gap longer than the window is not evidence that the next report is in the
// future: after a long idle period a fresh, valid live observation is accepted
// and counted, and the stale entries have left the window.
func TestLongInactivityRecoveryWithoutFutureRule(t *testing.T) {
	tr := newTracker(t, Window)
	mustObserve(t, tr, 600, base, ClassLive)

	// Two windows of inactivity: nothing about this timestamp proves it is a
	// future observation, so it must be accepted rather than refused.
	mustObserve(t, tr, 600, base.Add(2*Window), ClassLive)

	checkSnapshot(t, tr.Snapshot(base.Add(2*Window)), Snapshot{
		Available: true,
		Window:    Window,
		Tokens:    600,
		Rate:      10,
	})
}

// Once a live delta cannot be represented the tracker never vouches for a
// window again: the overflow state is sticky, including an overflow discovered
// while summing several still-separate timestamps.
func TestWindowSumOverflowIsSticky(t *testing.T) {
	tr := newTracker(t, Window)
	half := int64(math.MaxInt64/2 + 1)
	mustObserve(t, tr, half, base, ClassLive)
	mustObserve(t, tr, half, base.Add(time.Second), ClassLive)

	checkSnapshot(t, tr.Snapshot(base.Add(time.Second)), Snapshot{Reason: ReasonOverflow, Window: Window})

	// Both contributing samples have left the window, but the count that was
	// lost is unrecoverable, so the answer stays unavailable instead of
	// silently reverting to a partial 0/s.
	checkSnapshot(t, tr.Snapshot(base.Add(Window+time.Second)), Snapshot{Reason: ReasonOverflow, Window: Window})

	if err := tr.Observe(1, base.Add(Window+2*time.Second), ClassLive); !errors.Is(err, ErrOverflow) {
		t.Fatalf("Observe after sum overflow = %v, want ErrOverflow", err)
	}
}
