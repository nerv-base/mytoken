package ingestrate

import (
	"math/rand"
	"testing"
	"time"
)

// TestExactWindowMatchesReferenceModel cross-checks the bounded timestamp queue
// against a brute-force model that keeps every sample. The clock moves by
// random sub-second amounts, so samples share whole seconds and straddle window
// boundaries, which is exactly what a whole-second ring gets wrong.
func TestExactWindowMatchesReferenceModel(t *testing.T) {
	rng := rand.New(rand.NewSource(20240517))
	tr := newTracker(t, Window)

	type sample struct {
		at     time.Time
		tokens int64
	}
	var model []sample

	check := func(step int, now time.Time) {
		t.Helper()
		var want int64
		for _, s := range model {
			if s.at.After(now.Add(-Window)) && !s.at.After(now) {
				want += s.tokens
			}
		}
		got := tr.Snapshot(now)
		wantSnap := Snapshot{Available: true, Window: Window, Tokens: want, Rate: float64(want) / Window.Seconds()}
		if got != wantSnap {
			t.Fatalf("step %d: Snapshot(%s) = %+v, want %+v", step, now, got, wantSnap)
		}
	}

	now := base
	for step := 0; step < 800; step++ {
		// Advance by 0..3s, then observe and snapshot at that instant.
		now = now.Add(time.Duration(rng.Intn(3000)) * time.Millisecond)
		delta := int64(rng.Intn(1000) + 1)
		if err := tr.Observe(delta, now, ClassLive); err != nil {
			t.Fatalf("step %d: Observe(%d, %s) = %v, want nil", step, delta, now, err)
		}
		model = append(model, sample{at: now, tokens: delta})
		check(step, now)

		// Snapshot again up to a second later, still inside freshness.
		now = now.Add(time.Duration(rng.Intn(1000)) * time.Millisecond)
		check(step, now)
	}
}
