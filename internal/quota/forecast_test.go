package quota

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// forecastTestsUseOnlySyntheticPercentages: no test here reads a real rollout,
// reaches a network, or touches a credential. Every sample is a fabricated
// Report literal.

func forecastInstant(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts.UTC()
}

// forecastReport builds a successful Codex report with one primary reading.
func forecastReport(observed time.Time, used float64, reset *time.Time, source, limitID, limitName string) *Report {
	return &Report{
		Source:     source,
		ObservedAt: ptrTime(observed),
		Line:       1,
		LimitID:    limitID,
		LimitName:  limitName,
		Primary: &Window{
			Kind:        WindowPrimary,
			Status:      StatusOK,
			UsedPercent: ptrFloat(used),
			ResetsAt:    reset,
		},
		Status: StatusOK,
	}
}

func forecastPublish(t *testing.T, store memSettings, rep *Report, now time.Time) *Latest {
	t.Helper()
	if err := PublishLatest(context.Background(), store, "default", rep, now); err != nil {
		t.Fatalf("PublishLatest: %v", err)
	}
	latest, ok, err := LoadLatest(context.Background(), store, "default")
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	if !ok || latest == nil {
		t.Fatalf("LoadLatest: ok=%v latest=%v", ok, latest)
	}
	return latest
}

func forecastClose(got, want float64) bool {
	d := got - want
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

func TestPublishLatestForecastsTwoObservations(t *testing.T) {
	store := memSettings{}
	t0 := forecastInstant(t, "2026-01-02T03:04:05Z")
	reset := t0.Add(3 * time.Hour)

	first := forecastReport(t0, 10, ptrTime(reset), "rollout.jsonl", "codex-primary", "Codex")
	latest := forecastPublish(t, store, first, t0)
	if got := len(latest.Observations); got != 1 {
		t.Fatalf("first publish retained %d observations, want 1", got)
	}
	if latest.Forecast == nil || latest.Forecast.Available {
		t.Fatalf("one observation produced a forecast: %+v", latest.Forecast)
	}
	if latest.Forecast.Reason != ReasonInsufficientSamples {
		t.Fatalf("one observation reason = %q, want %q", latest.Forecast.Reason, ReasonInsufficientSamples)
	}

	second := forecastReport(t0.Add(time.Hour), 40, ptrTime(reset), "rollout.jsonl", "codex-primary", "Codex")
	now := t0.Add(time.Hour)
	latest = forecastPublish(t, store, second, now)

	if got := len(latest.Observations); got != 2 {
		t.Fatalf("second publish retained %d observations, want 2: %+v", got, latest.Observations)
	}
	// The original source and observed instant survive storage, so the pair the
	// rate was computed from can be audited later.
	for i, want := range []time.Time{t0, now} {
		o := latest.Observations[i]
		if o.Source != "rollout.jsonl" || !o.ObservedAt.Equal(want) {
			t.Fatalf("observation %d = %+v, want source rollout.jsonl at %s", i, o, want)
		}
	}
	fc := latest.Forecast
	if fc == nil || !fc.Available {
		t.Fatalf("two observations did not produce a forecast: %+v", fc)
	}
	if fc.ObservedAt == nil || !fc.ObservedAt.Equal(now) || !fc.GeneratedAt.Equal(now) {
		t.Fatalf("forecast instants = %v/%v, want %s", fc.ObservedAt, fc.GeneratedAt, now)
	}
	if len(fc.Windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(fc.Windows))
	}
	w := fc.Windows[0]
	if w.Window != WindowPrimary || !w.Available || w.Reason != ReasonNone || w.Observations != 2 {
		t.Fatalf("window forecast = %+v", w)
	}
	if w.Source != "rollout.jsonl" || w.LimitID != "codex-primary" || w.LimitName != "Codex" {
		t.Fatalf("window identity = %+v", w)
	}
	if w.FirstObservedAt == nil || !w.FirstObservedAt.Equal(t0) || w.LastObservedAt == nil || !w.LastObservedAt.Equal(now) {
		t.Fatalf("window instants = %v/%v", w.FirstObservedAt, w.LastObservedAt)
	}
	// 10% -> 40% over one hour, 3h of window left with 60% remaining.
	if !forecastClose(w.Pace.PercentPerHour, 30) || !forecastClose(w.Pace.UsedDeltaPercent, 30) {
		t.Fatalf("pace rate = %+v", w.Pace)
	}
	if !forecastClose(w.Pace.RemainingPercent, 60) || !forecastClose(w.Pace.EvenPercentPerHour, 30) {
		t.Fatalf("pace remaining = %+v", w.Pace)
	}
	if !w.Pace.Available || w.Pace.Reason != ReasonNone {
		t.Fatalf("pace unavailable: %+v", w.Pace)
	}
	if len(fc.Limitations) == 0 {
		t.Fatal("forecast reported no limitations")
	}

	// A consumer holding only the stored samples recomputes the same answer at
	// its own instant, and the pace ages out with the samples it came from.
	recomputed := Forecasts(latest, now, ForecastOptions{})
	if recomputed == nil || !recomputed.Available {
		t.Fatalf("Forecasts recompute = %+v", recomputed)
	}
	aged := Forecasts(latest, now.Add(7*time.Hour), ForecastOptions{})
	if aged == nil || aged.Available || aged.Reason != ReasonStale {
		t.Fatalf("aged forecast = %+v, want stale", aged)
	}
}

func TestRepeatedImportIsNotANewObservation(t *testing.T) {
	store := memSettings{}
	t0 := forecastInstant(t, "2026-01-02T03:04:05Z")
	reset := t0.Add(4 * time.Hour)
	rep := forecastReport(t0, 10, ptrTime(reset), "rollout.jsonl", "codex-primary", "Codex")

	latest := forecastPublish(t, store, rep, t0)
	before := latest.Observations
	// The same report read twice: the second publish is a later ingest of the
	// same instant, and must not become a second sample.
	latest = forecastPublish(t, store, rep, t0.Add(time.Hour))
	if len(latest.Observations) != 1 {
		t.Fatalf("repeated import retained %d observations: %+v", len(latest.Observations), latest.Observations)
	}
	if !latest.Observations[0].ObservedAt.Equal(before[0].ObservedAt) {
		t.Fatalf("repeated import moved the sample: %+v", latest.Observations)
	}
	if latest.Forecast.Available || latest.Forecast.Reason != ReasonInsufficientSamples {
		t.Fatalf("repeated import produced a forecast: %+v", latest.Forecast)
	}

	// A backfill older than the retained sample is not progress either, and a
	// sample that falls between the pair cannot rewrite the interval in use.
	older := forecastReport(t0.Add(-time.Hour), 5, ptrTime(reset), "rollout.jsonl", "codex-primary", "Codex")
	latest = forecastPublish(t, store, older, t0.Add(2*time.Hour))
	if len(latest.Observations) != 1 || !latest.Observations[0].ObservedAt.Equal(t0) {
		t.Fatalf("older backfill changed the set: %+v", latest.Observations)
	}

	mid := forecastReport(t0.Add(30*time.Minute), 30, ptrTime(reset), "rollout.jsonl", "codex-primary", "Codex")
	newest := forecastReport(t0.Add(time.Hour), 40, ptrTime(reset), "rollout.jsonl", "codex-primary", "Codex")
	latest = forecastPublish(t, store, newest, t0.Add(time.Hour))
	latest = forecastPublish(t, store, mid, t0.Add(2*time.Hour))
	if len(latest.Observations) != 2 {
		t.Fatalf("observations = %+v", latest.Observations)
	}
	if !latest.Observations[0].ObservedAt.Equal(t0) || !latest.Observations[1].ObservedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("out-of-order sample displaced the pair: %+v", latest.Observations)
	}
	// The pair only slides forward: the third distinct, strictly newer sample
	// drops the oldest and keeps exactly two.
	third := forecastReport(t0.Add(2*time.Hour), 55, ptrTime(reset), "rollout.jsonl", "codex-primary", "Codex")
	latest = forecastPublish(t, store, third, t0.Add(2*time.Hour))
	if len(latest.Observations) != 2 {
		t.Fatalf("observations = %+v", latest.Observations)
	}
	if !latest.Observations[0].ObservedAt.Equal(t0.Add(time.Hour)) || !latest.Observations[1].ObservedAt.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("pair did not slide: %+v", latest.Observations)
	}
}

func TestResetWithholdsTheForecast(t *testing.T) {
	store := memSettings{}
	t0 := forecastInstant(t, "2026-01-02T03:04:05Z")
	// The first sample's window resets before the second sample is taken, so
	// the two percentages describe different windows.
	first := forecastReport(t0, 90, ptrTime(t0.Add(10*time.Minute)), "rollout.jsonl", "codex-primary", "Codex")
	second := forecastReport(t0.Add(time.Hour), 5, ptrTime(t0.Add(4*time.Hour)), "rollout.jsonl", "codex-primary", "Codex")

	latest := forecastPublish(t, store, first, t0)
	latest = forecastPublish(t, store, second, t0.Add(time.Hour))

	if latest.Forecast.Available {
		t.Fatalf("rolled-over samples produced a forecast: %+v", latest.Forecast)
	}
	w := latest.Forecast.Windows[0]
	if w.Reason != ReasonResetRollover {
		t.Fatalf("reason = %q, want %q", w.Reason, ReasonResetRollover)
	}
	if w.Pace.Available {
		t.Fatalf("pace was guessed across a reset: %+v", w.Pace)
	}

	// A window that simply loses its reset instant is reset-ambiguous too.
	unknown := forecastReport(t0.Add(2*time.Hour), 60, nil, "rollout.jsonl", "codex-primary", "Codex")
	latest = forecastPublish(t, store, unknown, t0.Add(2*time.Hour))
	if latest.Forecast.Available || latest.Forecast.Windows[0].Reason != ReasonResetUnknown {
		t.Fatalf("missing reset produced a forecast: %+v", latest.Forecast)
	}
}

func TestMismatchedLimitIDsWithholdTheForecast(t *testing.T) {
	store := memSettings{}
	t0 := forecastInstant(t, "2026-01-02T03:04:05Z")
	reset := t0.Add(4 * time.Hour)
	// Same human-readable limit name, different limit id: the percentages are
	// not comparable, and the name must not be used to paper over that.
	first := forecastReport(t0, 10, ptrTime(reset), "rollout.jsonl", "codex-primary-a", "Codex")
	second := forecastReport(t0.Add(time.Hour), 40, ptrTime(reset), "rollout.jsonl", "codex-primary-b", "Codex")

	latest := forecastPublish(t, store, first, t0)
	latest = forecastPublish(t, store, second, t0.Add(time.Hour))

	if latest.Forecast.Available || latest.Forecast.Windows[0].Reason != ReasonLimitMismatch {
		t.Fatalf("mismatched ids produced a forecast: %+v", latest.Forecast)
	}
	if latest.Forecast.Windows[0].Pace.Available {
		t.Fatalf("pace was guessed across limit identities: %+v", latest.Forecast.Windows[0].Pace)
	}
}

func TestSourceIsolationWithholdsTheForecast(t *testing.T) {
	store := memSettings{}
	t0 := forecastInstant(t, "2026-01-02T03:04:05Z")
	reset := t0.Add(4 * time.Hour)
	first := forecastReport(t0, 10, ptrTime(reset), "rollout.jsonl", "codex-primary", "Codex")
	second := forecastReport(t0.Add(time.Hour), 40, ptrTime(reset), "other-account.jsonl", "codex-primary", "Codex")

	latest := forecastPublish(t, store, first, t0)
	latest = forecastPublish(t, store, second, t0.Add(time.Hour))

	if latest.Forecast.Available || latest.Forecast.Windows[0].Reason != ReasonSourceMismatch {
		t.Fatalf("cross-source samples produced a forecast: %+v", latest.Forecast)
	}
	if latest.Forecast.Windows[0].Source != "" {
		t.Fatalf("mismatched source was still reported: %+v", latest.Forecast.Windows[0])
	}
}

func TestStaleAndFutureSamplesWithholdTheForecast(t *testing.T) {
	store := memSettings{}
	t0 := forecastInstant(t, "2026-01-02T03:04:05Z")
	reset := t0.Add(6 * time.Hour)
	first := forecastReport(t0, 10, ptrTime(reset), "rollout.jsonl", "codex-primary", "Codex")
	second := forecastReport(t0.Add(time.Hour), 20, ptrTime(reset), "rollout.jsonl", "codex-primary", "Codex")

	latest := forecastPublish(t, store, first, t0)
	latest = forecastPublish(t, store, second, t0.Add(time.Hour))

	// A pair that is still computable at the newest instant is not fresh now.
	stale := Forecasts(latest, t0.Add(time.Hour).Add(DefaultForecastMaxAge+time.Minute), ForecastOptions{})
	if stale.Available || stale.Windows[0].Reason != ReasonStale {
		t.Fatalf("stale samples produced a fresh forecast: %+v", stale)
	}
	// The same pair asked about a future instant is not a future forecast.
	future := Forecasts(latest, t0, ForecastOptions{})
	if future.Available || future.Windows[0].Reason != ReasonFutureObservation {
		t.Fatalf("future samples produced a forecast: %+v", future)
	}
}

func TestUntrustworthySamplesAreDropped(t *testing.T) {
	t0 := forecastInstant(t, "2026-01-02T03:04:05Z")
	reset := t0.Add(4 * time.Hour)
	good := Observation{Window: WindowPrimary, Source: "rollout.jsonl", ObservedAt: t0, UsedPercent: 10}

	kept := RetainObservations(nil,
		Observation{Window: "", ObservedAt: t0, UsedPercent: 10},
		Observation{Window: WindowPrimary, UsedPercent: 10},
		Observation{Window: WindowPrimary, ObservedAt: forecastInstant(t, "1999-12-31T23:59:59Z"), UsedPercent: 10},
		Observation{Window: WindowPrimary, ObservedAt: forecastInstant(t, "2100-01-01T00:00:00Z"), UsedPercent: 10},
		Observation{Window: WindowPrimary, ObservedAt: t0, UsedPercent: 101},
		Observation{Window: WindowPrimary, ObservedAt: t0, UsedPercent: -1},
		good,
	)
	if len(kept) != 1 || kept[0].ObservedAt != t0 {
		t.Fatalf("kept = %+v, want only the trustworthy sample", kept)
	}

	// A stored Latest written before retention existed still loads, and reports
	// that there is nothing to forecast rather than inventing a pace.
	legacy, err := json.Marshal(map[string]any{
		"harness":     HarnessCodex,
		"instance":    "default",
		"source":      "rollout.jsonl",
		"ingested_at": t0,
		"report":      forecastReport(t0, 10, ptrTime(reset), "rollout.jsonl", "codex-primary", "Codex"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(legacy), "observations") || strings.Contains(string(legacy), "forecast") {
		t.Fatal("legacy payload unexpectedly carries forecast state")
	}
	store := memSettings{SettingKey("default"): string(legacy)}
	loaded, ok, err := LoadLatest(context.Background(), store, "default")
	if err != nil || !ok {
		t.Fatalf("legacy payload: ok=%v err=%v", ok, err)
	}
	if len(loaded.Observations) != 0 {
		t.Fatalf("legacy payload invented observations: %+v", loaded.Observations)
	}
	fc := Forecasts(loaded, t0, ForecastOptions{})
	if fc == nil || fc.Available || fc.Reason != ReasonNoObservation {
		t.Fatalf("legacy forecast = %+v", fc)
	}
}

func TestForecastSamplesStayBoundedAndPayloadSmall(t *testing.T) {
	store := memSettings{}
	t0 := forecastInstant(t, "2026-01-02T03:04:05Z")
	reset := t0.Add(12 * time.Hour)
	var latest *Latest
	for i := 0; i < 24; i++ {
		at := t0.Add(time.Duration(i) * time.Hour)
		rep := forecastReport(at, float64(i), ptrTime(reset), "rollout.jsonl", "codex-primary", "Codex")
		latest = forecastPublish(t, store, rep, at)
	}
	if len(latest.Observations) != retainedPerWindow {
		t.Fatalf("observations = %d, want %d", len(latest.Observations), retainedPerWindow)
	}
	if last := latest.Observations[len(latest.Observations)-1].ObservedAt; !last.Equal(t0.Add(23 * time.Hour)) {
		t.Fatalf("newest retained = %s", last)
	}
	raw := store[SettingKey("default")]
	if len(raw) > maxLatestBytes {
		t.Fatalf("stored payload = %d bytes, over the bound", len(raw))
	}
}
