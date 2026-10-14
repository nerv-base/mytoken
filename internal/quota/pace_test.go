package quota

import (
	"testing"
	"time"
)

func sampleAt(t *testing.T, kind WindowKind, obs string, used float64, reset string) Sample {
	t.Helper()
	s := Sample{Kind: kind, UsedPercent: used}
	if obs != "" {
		s.ObservedAt = at(t, obs)
		s.HasObservedAt = true
	}
	if reset != "" {
		s.ResetsAt = at(t, reset)
		s.HasResetsAt = true
	}
	return s
}

func TestEstimatePaceBasic(t *testing.T) {
	prev := sampleAt(t, WindowPrimary, "2026-01-02T03:00:00Z", 10, "2026-01-02T08:00:00Z")
	next := sampleAt(t, WindowPrimary, "2026-01-02T04:00:00Z", 20, "2026-01-02T08:00:00Z")

	p := EstimatePace(prev, next, PaceOptions{})
	if !p.Available || p.Reason != ReasonNone {
		t.Fatalf("pace = %+v, want available", p)
	}
	if p.Elapsed != time.Hour {
		t.Errorf("Elapsed = %v, want 1h", p.Elapsed)
	}
	if p.UsedDeltaPercent != 10 {
		t.Errorf("UsedDeltaPercent = %v, want 10", p.UsedDeltaPercent)
	}
	if p.PercentPerHour != 10 {
		t.Errorf("PercentPerHour = %v, want 10", p.PercentPerHour)
	}
	if p.PercentPerDay != 240 {
		t.Errorf("PercentPerDay = %v, want 240", p.PercentPerDay)
	}
	if p.RemainingPercent != 80 {
		t.Errorf("RemainingPercent = %v, want 80", p.RemainingPercent)
	}
	if p.TimeToExhaustion != 8*time.Hour {
		t.Errorf("TimeToExhaustion = %v, want 8h", p.TimeToExhaustion)
	}
	if p.ExhaustsAt == nil || !p.ExhaustsAt.Equal(at(t, "2026-01-02T12:00:00Z")) {
		t.Errorf("ExhaustsAt = %v, want 2026-01-02T12:00:00Z", p.ExhaustsAt)
	}
	if !p.ResetsBeforeExhaustion {
		t.Error("ResetsBeforeExhaustion = false, want true (reset 08:00 precedes exhaustion 12:00)")
	}
	if p.EvenPercentPerHour != 20 {
		t.Errorf("EvenPercentPerHour = %v, want 20", p.EvenPercentPerHour)
	}
}

func TestEstimatePaceExhaustsBeforeReset(t *testing.T) {
	prev := sampleAt(t, WindowPrimary, "2026-01-02T03:00:00Z", 0, "2026-01-02T08:00:00Z")
	next := sampleAt(t, WindowPrimary, "2026-01-02T04:00:00Z", 50, "2026-01-02T08:00:00Z")

	p := EstimatePace(prev, next, PaceOptions{})
	if !p.Available {
		t.Fatalf("pace = %+v, want available", p)
	}
	if p.ExhaustsAt == nil || !p.ExhaustsAt.Equal(at(t, "2026-01-02T05:00:00Z")) {
		t.Errorf("ExhaustsAt = %v, want 2026-01-02T05:00:00Z", p.ExhaustsAt)
	}
	if p.ResetsBeforeExhaustion {
		t.Error("ResetsBeforeExhaustion = true, want false (exhaustion 05:00 precedes reset 08:00)")
	}
	if p.EvenPercentPerHour != 12.5 {
		t.Errorf("EvenPercentPerHour = %v, want 12.5", p.EvenPercentPerHour)
	}
}

func TestEstimatePaceZeroConsumption(t *testing.T) {
	prev := sampleAt(t, WindowPrimary, "2026-01-02T03:00:00Z", 30, "2026-01-02T08:00:00Z")
	next := sampleAt(t, WindowPrimary, "2026-01-02T04:00:00Z", 30, "2026-01-02T08:00:00Z")

	p := EstimatePace(prev, next, PaceOptions{})
	if !p.Available || p.Reason != ReasonNone {
		t.Fatalf("pace = %+v, want available for a flat window", p)
	}
	if p.PercentPerHour != 0 || p.PercentPerDay != 0 {
		t.Errorf("rate = %v/h %v/d, want 0", p.PercentPerHour, p.PercentPerDay)
	}
	if p.ExhaustsAt != nil || p.TimeToExhaustion != 0 {
		t.Errorf("exhaustion = %v/%v, want none at zero consumption", p.ExhaustsAt, p.TimeToExhaustion)
	}
	if p.ResetsBeforeExhaustion {
		t.Error("ResetsBeforeExhaustion = true, want false without an exhaustion estimate")
	}
	if p.EvenPercentPerHour != 17.5 {
		t.Errorf("EvenPercentPerHour = %v, want 17.5", p.EvenPercentPerHour)
	}
}

func TestEstimatePaceRejectsDecreasingUsage(t *testing.T) {
	prev := sampleAt(t, WindowPrimary, "2026-01-02T03:00:00Z", 30, "2026-01-02T08:00:00Z")
	next := sampleAt(t, WindowPrimary, "2026-01-02T04:00:00Z", 20, "2026-01-02T08:00:00Z")

	p := EstimatePace(prev, next, PaceOptions{})
	if p.Available || p.Reason != ReasonDecreasingUsage {
		t.Fatalf("pace = %+v, want unavailable/decreasing_usage", p)
	}
}

func TestEstimatePaceRejectsZeroElapsed(t *testing.T) {
	ts := "2026-01-02T03:00:00Z"
	prev := sampleAt(t, WindowPrimary, ts, 10, "2026-01-02T08:00:00Z")
	next := sampleAt(t, WindowPrimary, ts, 20, "2026-01-02T08:00:00Z")

	p := EstimatePace(prev, next, PaceOptions{})
	if p.Available || p.Reason != ReasonZeroElapsed {
		t.Fatalf("pace = %+v, want unavailable/zero_elapsed", p)
	}
}

func TestEstimatePaceRejectsResetRollover(t *testing.T) {
	// The earlier reset already passed before the later observation: the window
	// rolled over, so the two samples belong to different quota periods.
	prev := sampleAt(t, WindowPrimary, "2026-01-02T03:00:00Z", 90, "2026-01-02T04:00:00Z")
	next := sampleAt(t, WindowPrimary, "2026-01-02T05:00:00Z", 10, "2026-01-02T09:00:00Z")

	p := EstimatePace(prev, next, PaceOptions{})
	if p.Available || p.Reason != ReasonResetRollover {
		t.Fatalf("pace = %+v, want unavailable/reset_rollover", p)
	}
}

func TestEstimatePaceRejectsResetMismatch(t *testing.T) {
	// Different reset instants while the earlier reset is still in the future:
	// the two samples disagree about which window they describe.
	prev := sampleAt(t, WindowPrimary, "2026-01-02T03:00:00Z", 10, "2026-01-02T08:00:00Z")
	next := sampleAt(t, WindowPrimary, "2026-01-02T05:00:00Z", 20, "2026-01-02T09:00:00Z")

	p := EstimatePace(prev, next, PaceOptions{})
	if p.Available || p.Reason != ReasonResetMismatch {
		t.Fatalf("pace = %+v, want unavailable/reset_mismatch", p)
	}
}

func TestEstimatePaceRejectsUnknownReset(t *testing.T) {
	prev := sampleAt(t, WindowPrimary, "2026-01-02T03:00:00Z", 10, "2026-01-02T08:00:00Z")
	next := sampleAt(t, WindowPrimary, "2026-01-02T04:00:00Z", 20, "")

	p := EstimatePace(prev, next, PaceOptions{})
	if p.Available || p.Reason != ReasonResetUnknown {
		t.Fatalf("pace = %+v, want unavailable/reset_unknown", p)
	}
}

func TestEstimatePaceRejectsUnknownTimestamp(t *testing.T) {
	prev := sampleAt(t, WindowPrimary, "", 10, "2026-01-02T08:00:00Z")
	next := sampleAt(t, WindowPrimary, "2026-01-02T04:00:00Z", 20, "2026-01-02T08:00:00Z")

	p := EstimatePace(prev, next, PaceOptions{})
	if p.Available || p.Reason != ReasonNoTimestamp {
		t.Fatalf("pace = %+v, want unavailable/no_trustworthy_timestamp", p)
	}
}

func TestEstimatePaceRejectsWindowMismatch(t *testing.T) {
	prev := sampleAt(t, WindowPrimary, "2026-01-02T03:00:00Z", 10, "2026-01-02T08:00:00Z")
	next := sampleAt(t, WindowSecondary, "2026-01-02T04:00:00Z", 20, "2026-01-02T08:00:00Z")

	p := EstimatePace(prev, next, PaceOptions{})
	if p.Available || p.Reason != ReasonWindowMismatch {
		t.Fatalf("pace = %+v, want unavailable/window_mismatch", p)
	}
}

func TestEstimatePaceRejectsMalformedSample(t *testing.T) {
	prev := sampleAt(t, WindowPrimary, "2026-01-02T03:00:00Z", 10, "2026-01-02T08:00:00Z")
	next := sampleAt(t, WindowPrimary, "2026-01-02T04:00:00Z", 150, "2026-01-02T08:00:00Z")

	p := EstimatePace(prev, next, PaceOptions{})
	if p.Available || p.Reason != ReasonMalformed {
		t.Fatalf("pace = %+v, want unavailable/malformed for an out-of-range percent", p)
	}
}

func TestEstimatePaceRejectsTooShortInterval(t *testing.T) {
	prev := sampleAt(t, WindowPrimary, "2026-01-02T03:00:00Z", 10, "2026-01-02T08:00:00Z")
	next := sampleAt(t, WindowPrimary, "2026-01-02T03:00:00.5Z", 11, "2026-01-02T08:00:00Z")

	p := EstimatePace(prev, next, PaceOptions{})
	if p.Available || p.Reason != ReasonOutlier {
		t.Fatalf("pace = %+v, want unavailable/outlier below the minimum interval", p)
	}
}

func TestEstimatePaceRejectsImplausibleRate(t *testing.T) {
	prev := sampleAt(t, WindowPrimary, "2026-01-02T03:00:00Z", 0, "2026-01-02T08:00:00Z")
	next := sampleAt(t, WindowPrimary, "2026-01-02T03:00:01Z", 5, "2026-01-02T08:00:00Z")

	p := EstimatePace(prev, next, PaceOptions{})
	if p.Available || p.Reason != ReasonOutlier {
		t.Fatalf("pace = %+v, want unavailable/outlier above the maximum rate", p)
	}
}

func TestEstimatePaceRejectsIntervalBeyondMaxElapsed(t *testing.T) {
	prev := sampleAt(t, WindowPrimary, "2026-01-02T00:00:00Z", 10, "2026-01-02T20:00:00Z")
	next := sampleAt(t, WindowPrimary, "2026-01-02T03:00:00Z", 20, "2026-01-02T20:00:00Z")

	p := EstimatePace(prev, next, PaceOptions{MaxElapsed: time.Hour})
	if p.Available || p.Reason != ReasonOutlier {
		t.Fatalf("pace = %+v, want unavailable/outlier beyond MaxElapsed", p)
	}
}

func TestEstimatePaceDefaultMaxElapsedIsDisabled(t *testing.T) {
	prev := sampleAt(t, WindowPrimary, "2026-01-02T00:00:00Z", 10, "2026-01-06T00:00:00Z")
	next := sampleAt(t, WindowPrimary, "2026-01-05T00:00:00Z", 20, "2026-01-06T00:00:00Z")

	p := EstimatePace(prev, next, PaceOptions{})
	if !p.Available {
		t.Fatalf("pace = %+v, want available with no default MaxElapsed", p)
	}
	if p.Elapsed != 72*time.Hour {
		t.Errorf("Elapsed = %v, want 72h", p.Elapsed)
	}
}

func TestEstimatePaceWithoutResetOmitsEvenLine(t *testing.T) {
	prev := sampleAt(t, WindowPrimary, "2026-01-02T03:00:00Z", 10, "")
	next := sampleAt(t, WindowPrimary, "2026-01-02T04:00:00Z", 20, "")

	p := EstimatePace(prev, next, PaceOptions{})
	if !p.Available {
		t.Fatalf("pace = %+v, want available without reset metadata", p)
	}
	if p.EvenPercentPerHour != 0 {
		t.Errorf("EvenPercentPerHour = %v, want 0 without reset metadata", p.EvenPercentPerHour)
	}
	if p.ExhaustsAt == nil || !p.ExhaustsAt.Equal(at(t, "2026-01-02T12:00:00Z")) {
		t.Errorf("ExhaustsAt = %v, want 2026-01-02T12:00:00Z", p.ExhaustsAt)
	}
}

func TestPaceDefaults(t *testing.T) {
	if DefaultMinElapsed != time.Second {
		t.Errorf("DefaultMinElapsed = %v, want 1s", DefaultMinElapsed)
	}
	if DefaultMaxPercentPerHour != 3600 {
		t.Errorf("DefaultMaxPercentPerHour = %v, want 3600", DefaultMaxPercentPerHour)
	}
	if DefaultMaxElapsed != 0 {
		t.Errorf("DefaultMaxElapsed = %v, want 0 (disabled)", DefaultMaxElapsed)
	}
}
