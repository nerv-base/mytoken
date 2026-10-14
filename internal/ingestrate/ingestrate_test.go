package ingestrate

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"
)

// base is an arbitrary fixed wall clock. Every test moves time explicitly; none
// of them sleep.
var base = time.Unix(1_700_000_000, 0).UTC()

func newTracker(t *testing.T, freshness time.Duration) *Tracker {
	t.Helper()
	tr, err := New(freshness)
	if err != nil {
		t.Fatalf("New(%s) = %v, want nil error", freshness, err)
	}
	return tr
}

func mustObserve(t *testing.T, tr *Tracker, delta int64, at time.Time, class Class) {
	t.Helper()
	if err := tr.Observe(delta, at, class); err != nil {
		t.Fatalf("Observe(%d, %s, %s) = %v, want nil", delta, at, class, err)
	}
}

func checkSnapshot(t *testing.T, got, want Snapshot) {
	t.Helper()
	if got != want {
		t.Fatalf("Snapshot = %+v, want %+v", got, want)
	}
}

func TestNew(t *testing.T) {
	for _, tt := range []struct {
		name      string
		freshness time.Duration
		wantErr   bool
	}{
		{"zero", 0, true},
		{"negative", -time.Second, true},
		{"above window", Window + time.Nanosecond, true},
		{"window", Window, false},
		{"one second", time.Second, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tr, err := New(tt.freshness)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("New(%s) = %v, want error", tt.freshness, tr)
				}
				return
			}
			if err != nil {
				t.Fatalf("New(%s) = %v, want nil error", tt.freshness, err)
			}
		})
	}
}

func TestUnavailableWithoutLiveSignal(t *testing.T) {
	unavailable := Snapshot{Reason: ReasonNoLiveSignal, Window: Window}
	for _, tt := range []struct {
		name string
		feed func(t *testing.T, tr *Tracker)
	}{
		{"cold", func(*testing.T, *Tracker) {}},
		{"backfill only", func(t *testing.T, tr *Tracker) { mustObserve(t, tr, 600, base, ClassBackfill) }},
		{"unknown only", func(t *testing.T, tr *Tracker) { mustObserve(t, tr, 600, base, ClassUnknown) }},
		{"negative delta", func(t *testing.T, tr *Tracker) {
			if err := tr.Observe(-1, base, ClassLive); !errors.Is(err, ErrNegativeDelta) {
				t.Fatalf("Observe(-1) = %v, want ErrNegativeDelta", err)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tr := newTracker(t, Window)
			tt.feed(t, tr)
			checkSnapshot(t, tr.Snapshot(base), unavailable)
		})
	}
}

// A fresh live heartbeat with no tokens is known telemetry at zero, not an
// unknown window.
func TestWarmEmptyHeartbeatIsAvailable(t *testing.T) {
	tr := newTracker(t, Window)
	mustObserve(t, tr, 0, base, ClassLive)
	checkSnapshot(t, tr.Snapshot(base), Snapshot{Available: true, Window: Window})
}

// The rate is always the window count divided by the full 60 seconds, never by
// the time since the tracker started.
func TestRateIsWindowCountOverSixtySeconds(t *testing.T) {
	for _, tt := range []struct {
		name   string
		delta  int64
		now    time.Duration
		tokens int64
		rate   float64
	}{
		{"immediate", 600, 0, 600, 10},
		{"ten seconds of history", 300, 10 * time.Second, 300, 5},
		{"one token", 1, 500 * time.Millisecond, 1, 1.0 / 60},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tr := newTracker(t, Window)
			mustObserve(t, tr, tt.delta, base, ClassLive)
			checkSnapshot(t, tr.Snapshot(base.Add(tt.now)), Snapshot{
				Available: true,
				Window:    Window,
				Tokens:    tt.tokens,
				Rate:      tt.rate,
			})
		})
	}
}

// The trailing interval is the 60 whole seconds ending at floor(now): a sample
// observed exactly 60.000s before now has expired, one observed at 59.999s has
// not. Freshness is inclusive at its configured timeout.
func TestTrailingWindowAndFreshnessBoundary(t *testing.T) {
	tr := newTracker(t, Window)
	mustObserve(t, tr, 600, base, ClassLive)
	for _, tt := range []struct {
		name string
		now  time.Time
		want Snapshot
	}{
		{"observed now", base, Snapshot{Available: true, Window: Window, Tokens: 600, Rate: 10}},
		{"one second later", base.Add(time.Second), Snapshot{Available: true, Window: Window, Tokens: 600, Rate: 10}},
		{"59s inclusive", base.Add(59 * time.Second), Snapshot{Available: true, Window: Window, Tokens: 600, Rate: 10}},
		{"59.999s inclusive", base.Add(59*time.Second + 999*time.Millisecond), Snapshot{Available: true, Window: Window, Tokens: 600, Rate: 10}},
		{"exactly 60s expired but fresh", base.Add(Window), Snapshot{Available: true, Window: Window}},
		{"60s plus 1ns stale", base.Add(Window + time.Nanosecond), Snapshot{Reason: ReasonStale, Window: Window}},
	} {
		t.Run(tt.name, func(t *testing.T) { checkSnapshot(t, tr.Snapshot(tt.now), tt.want) })
	}
}

func TestFreshnessBoundaryIsInclusive(t *testing.T) {
	tr := newTracker(t, 30*time.Second)
	mustObserve(t, tr, 0, base, ClassLive)
	checkSnapshot(t, tr.Snapshot(base.Add(30*time.Second)), Snapshot{Available: true, Window: Window})
	checkSnapshot(t, tr.Snapshot(base.Add(30*time.Second+time.Nanosecond)), Snapshot{Reason: ReasonStale, Window: Window})
}

// A sub-second sample expires at its own timestamp, not with the whole second it
// started in: at now = base+60.000s it is only 59.100s old and still counts.
func TestSubSecondSampleExpiresAtOwnTimestamp(t *testing.T) {
	tr := newTracker(t, Window)
	mustObserve(t, tr, 600, base.Add(900*time.Millisecond), ClassLive)
	if got := tr.Snapshot(base.Add(59*time.Second + 999*time.Millisecond)); got.Tokens != 600 {
		t.Fatalf("tokens at 59.999s = %d, want 600", got.Tokens)
	}
	if got := tr.Snapshot(base.Add(Window)); got.Tokens != 600 {
		t.Fatalf("tokens at 60.000s = %d, want 600 (the sample is 59.100s old)", got.Tokens)
	}
	if got := tr.Snapshot(base.Add(Window + 900*time.Millisecond)); got.Tokens != 0 {
		t.Fatalf("tokens at 60.900s = %d, want 0 (the sample is exactly %s old)", got.Tokens, Window)
	}
}

func TestBackfillAndUnknownNeverCountAsLive(t *testing.T) {
	t.Run("mixed with heartbeat", func(t *testing.T) {
		tr := newTracker(t, Window)
		mustObserve(t, tr, 900, base, ClassBackfill)
		mustObserve(t, tr, 900, base, ClassUnknown)
		mustObserve(t, tr, 0, base, ClassLive)
		checkSnapshot(t, tr.Snapshot(base), Snapshot{Available: true, Window: Window})
	})
	t.Run("backfill does not move the clock", func(t *testing.T) {
		tr := newTracker(t, Window)
		mustObserve(t, tr, 900, base.Add(time.Hour), ClassBackfill)
		mustObserve(t, tr, 600, base, ClassLive)
		checkSnapshot(t, tr.Snapshot(base), Snapshot{Available: true, Window: Window, Tokens: 600, Rate: 10})
	})
}

func TestHeartbeatRefreshesStaleTelemetry(t *testing.T) {
	tr := newTracker(t, 5*time.Second)
	mustObserve(t, tr, 600, base, ClassLive)
	mustObserve(t, tr, 0, base.Add(4*time.Second), ClassLive)
	checkSnapshot(t, tr.Snapshot(base.Add(9*time.Second)), Snapshot{Available: true, Window: Window, Tokens: 600, Rate: 10})
}

// observation is one scripted Observe call for the policy tables.
type observation struct {
	delta int64
	at    time.Time
	class Class
}

// Rejected observations leave the tracker untouched: no tokens, no clock move.
func TestTimeAndDeltaPolicy(t *testing.T) {
	live600 := []observation{{600, base, ClassLive}}
	for _, tt := range []struct {
		name    string
		feeds   []observation
		call    func(tr *Tracker) error
		wantErr error
		now     time.Time
		want    Snapshot
	}{
		{
			name:    "negative delta",
			feeds:   live600,
			call:    func(tr *Tracker) error { return tr.Observe(-1, base, ClassLive) },
			wantErr: ErrNegativeDelta,
			now:     base,
			want:    Snapshot{Available: true, Window: Window, Tokens: 600, Rate: 10},
		},
		{
			name:    "zero time",
			call:    func(tr *Tracker) error { return tr.Observe(1, time.Time{}, ClassLive) },
			wantErr: ErrInvalidTime,
			now:     base,
			want:    Snapshot{Reason: ReasonNoLiveSignal, Window: Window},
		},
		{
			name:    "invalid class",
			call:    func(tr *Tracker) error { return tr.Observe(1, base, Class("watch")) },
			wantErr: ErrInvalidClass,
			now:     base,
			want:    Snapshot{Reason: ReasonNoLiveSignal, Window: Window},
		},
		{
			name:    "out of order",
			feeds:   live600,
			call:    func(tr *Tracker) error { return tr.Observe(1, base.Add(-time.Second), ClassLive) },
			wantErr: ErrOutOfOrder,
			now:     base,
			want:    Snapshot{Available: true, Window: Window, Tokens: 600, Rate: 10},
		},
		{
			name:  "inactivity longer than the window is not a future report",
			feeds: live600,
			call:  func(tr *Tracker) error { return tr.Observe(600, base.Add(2*Window), ClassLive) },
			now:   base.Add(2 * Window),
			// The first sample has left the window; 600 proves the fresh
			// post-inactivity observation was accepted and counted rather than
			// refused as "future".
			want: Snapshot{Available: true, Window: Window, Tokens: 600, Rate: 10},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tr := newTracker(t, Window)
			for _, f := range tt.feeds {
				mustObserve(t, tr, f.delta, f.at, f.class)
			}
			err := tt.call(tr)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Observe = %v, want nil", err)
				}
			} else if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Observe = %v, want %v", err, tt.wantErr)
			}
			checkSnapshot(t, tr.Snapshot(tt.now), tt.want)
		})
	}
}

func TestSnapshotClockContract(t *testing.T) {
	t.Run("snapshot advances the clock", func(t *testing.T) {
		tr := newTracker(t, Window)
		tr.Snapshot(base)
		if err := tr.Observe(1, base.Add(-time.Second), ClassLive); !errors.Is(err, ErrOutOfOrder) {
			t.Fatalf("Observe before the snapshot's now = %v, want ErrOutOfOrder", err)
		}
	})
	t.Run("regression is unavailable, not zero", func(t *testing.T) {
		tr := newTracker(t, Window)
		mustObserve(t, tr, 600, base, ClassLive)
		checkSnapshot(t, tr.Snapshot(base), Snapshot{Available: true, Window: Window, Tokens: 600, Rate: 10})
		checkSnapshot(t, tr.Snapshot(base.Add(-time.Second)), Snapshot{Reason: ReasonClockRegression, Window: Window})
	})
	t.Run("future stamp cannot fabricate zero", func(t *testing.T) {
		tr := newTracker(t, Window)
		// The first stamp establishes the clock; a snapshot behind it is a
		// regression, never a fabricated 0/s.
		mustObserve(t, tr, 600, base.Add(time.Hour), ClassLive)
		checkSnapshot(t, tr.Snapshot(base), Snapshot{Reason: ReasonClockRegression, Window: Window})
	})
}

func TestOverflowNeverFabricates(t *testing.T) {
	t.Run("per bucket", func(t *testing.T) {
		tr := newTracker(t, Window)
		mustObserve(t, tr, math.MaxInt64, base, ClassLive)
		if err := tr.Observe(1, base, ClassLive); !errors.Is(err, ErrOverflow) {
			t.Fatalf("Observe over MaxInt64 = %v, want ErrOverflow", err)
		}
		checkSnapshot(t, tr.Snapshot(base), Snapshot{Reason: ReasonOverflow, Window: Window})
		if err := tr.Observe(0, base, ClassLive); !errors.Is(err, ErrOverflow) {
			t.Fatalf("heartbeat after overflow = %v, want ErrOverflow", err)
		}
	})
	t.Run("window sum", func(t *testing.T) {
		tr := newTracker(t, Window)
		half := int64(math.MaxInt64/2 + 1)
		mustObserve(t, tr, half, base, ClassLive)
		mustObserve(t, tr, half, base.Add(time.Second), ClassLive)
		checkSnapshot(t, tr.Snapshot(base.Add(time.Second)), Snapshot{Reason: ReasonOverflow, Window: Window})
	})
}

// Pre-epoch stamps fold into the ring instead of indexing out of range.
func TestPreEpochTimeStaysInRange(t *testing.T) {
	at := time.Unix(-1, 0).UTC()
	tr := newTracker(t, Window)
	mustObserve(t, tr, 7, at, ClassLive)
	checkSnapshot(t, tr.Snapshot(at), Snapshot{Available: true, Window: Window, Tokens: 7, Rate: 7.0 / 60})
}

func TestConcurrentObserveAndSnapshot(t *testing.T) {
	tr := newTracker(t, Window)
	const writers, perWriter = 8, 1000
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if err := tr.Observe(1, base, ClassLive); err != nil {
					t.Errorf("Observe = %v, want nil", err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			tr.Snapshot(base)
		}
	}()
	wg.Wait()
	checkSnapshot(t, tr.Snapshot(base), Snapshot{
		Available: true,
		Window:    Window,
		Tokens:    writers * perWriter,
		Rate:      float64(writers*perWriter) / 60,
	})
}
