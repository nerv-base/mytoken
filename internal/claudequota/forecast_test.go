package claudequota

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/quota"
)

// These tests use only a synthetic token, a fake transport and an in-memory
// store: no test here reaches the network, reads a real credential file, or
// touches a real keychain.

// forecastClock is an injectable clock so a test can control the throttle gap
// and the staleness of a reading without sleeping.
type forecastClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *forecastClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *forecastClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// forecastTransport answers each request with the next utilisation, keeping a
// fixed reset instant so two samples describe the same window.
func forecastTransport(reset string, used ...float64) *fakeTransport {
	var calls atomic.Int64
	return &fakeTransport{fn: func(req *http.Request) (*http.Response, error) {
		n := int(calls.Add(1)) - 1
		if n >= len(used) {
			n = len(used) - 1
		}
		body := fmt.Sprintf(`{"five_hour":{"utilization":%s,"resets_at":%q}}`,
			strconv.FormatFloat(used[n], 'f', -1, 64), reset)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	}}
}

func failingTransport(status int) *fakeTransport {
	return &fakeTransport{fn: func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"nope"}`)),
			Request:    req,
		}, nil
	}}
}

func forecastStart(t *testing.T, store *fakeStore, src Source, tp Transport, clock *forecastClock) *Service {
	t.Helper()
	svc := New(Options{Store: store, Sources: []Source{src}, Transport: tp, Now: clock.now})
	if _, err := svc.Enable(context.Background(), "claude", ""); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	return svc
}

func forecastWindow(t *testing.T, fc *quota.Forecast, kind string) quota.WindowForecast {
	t.Helper()
	if fc == nil {
		t.Fatal("nil forecast")
	}
	for _, w := range fc.Windows {
		if w.Window == quota.WindowKind(kind) {
			return w
		}
	}
	t.Fatalf("forecast has no %s window: %+v", kind, fc.Windows)
	return quota.WindowForecast{}
}

func forecastClose(t *testing.T, got, want float64) {
	t.Helper()
	if d := got - want; d > 1e-6 || d < -1e-6 {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTwoRefreshesProduceAForecast(t *testing.T) {
	store := newFakeStore()
	var reads atomic.Int64
	src := countedSource(t, "claude", t.TempDir(), testToken, nil, &reads)
	clock := &forecastClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	tp := forecastTransport("2026-03-01T18:00:00Z", 10, 25)
	svc := forecastStart(t, store, src, tp, clock)
	ctx := context.Background()

	first, err := svc.Refresh(ctx, "claude")
	if err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	if len(first.Observations) != 1 || first.Forecast.Available {
		t.Fatalf("one reading must not be a rate: %+v / %+v", first.Observations, first.Forecast)
	}
	if first.Forecast.Reason != quota.ReasonInsufficientSamples {
		t.Fatalf("one reading reason = %q", first.Forecast.Reason)
	}

	clock.advance(15 * time.Minute)
	second, err := svc.Refresh(ctx, "claude")
	if err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	if second.Status != StatusOK || !second.FetchedAt.Equal(clock.now()) {
		t.Fatalf("second reading = %+v", second)
	}
	if len(second.Observations) != 2 {
		t.Fatalf("observations = %+v", second.Observations)
	}
	if o := second.Observations[0]; o.Source != src.Ref || !o.ObservedAt.Equal(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("first sample was not preserved: %+v", o)
	}
	fc := second.Forecast
	if fc == nil || !fc.Available {
		t.Fatalf("two readings did not produce a forecast: %+v", fc)
	}
	if len(fc.Limitations) == 0 {
		t.Fatal("forecast reported no limitations")
	}
	w := forecastWindow(t, fc, "five_hour")
	if !w.Available || w.Reason != quota.ReasonNone || w.Observations != 2 || w.Source != src.Ref {
		t.Fatalf("window forecast = %+v", w)
	}
	if w.FirstObservedAt == nil || !w.FirstObservedAt.Equal(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)) ||
		w.LastObservedAt == nil || !w.LastObservedAt.Equal(clock.now()) {
		t.Fatalf("window instants = %v/%v", w.FirstObservedAt, w.LastObservedAt)
	}
	// 10% -> 25% over 15 minutes is 60%/h; 75% remains with 5.75h to reset.
	forecastClose(t, w.Pace.PercentPerHour, 60)
	forecastClose(t, w.Pace.PercentPerDay, 1440)
	forecastClose(t, w.Pace.UsedDeltaPercent, 15)
	forecastClose(t, w.Pace.RemainingPercent, 75)
	forecastClose(t, w.Pace.EvenPercentPerHour, 75/5.75)
	if !w.Pace.Available || w.Pace.TimeToExhaustion != 75*time.Minute {
		t.Fatalf("pace = %+v", w.Pace)
	}
	if w.Pace.ExhaustsAt == nil || !w.Pace.ExhaustsAt.Equal(time.Date(2026, 3, 1, 13, 30, 0, 0, time.UTC)) {
		t.Fatalf("exhaustion instant = %v", w.Pace.ExhaustsAt)
	}
	// The window exhausts at 13:30, hours before the 18:00 reset, so the
	// projection is actionable and the reset does not supersede it.
	if w.Pace.ResetsBeforeExhaustion {
		t.Fatal("the reset was reported as superseding an earlier exhaustion")
	}

	// The reading on the wire carries the same forecast, and a consumer can
	// recompute it from the reading alone.
	cached, err := svc.Snapshot(ctx, "claude")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if cached.Forecast == nil || !cached.Forecast.Available {
		t.Fatalf("cached forecast = %+v", cached.Forecast)
	}
	if got := Forecasts(cached, clock.now()); got == nil || !got.Available {
		t.Fatalf("Forecasts = %+v", got)
	}
	raw := store.get(LatestKey("claude"))
	if raw == "" || !strings.Contains(raw, "five_hour") {
		t.Fatalf("reading was not persisted: %q", raw)
	}
	if strings.Contains(raw, testToken) {
		t.Fatal("the persisted reading leaked the access token")
	}
}

func TestFailedRefreshAddsNoSampleAndKeepsThePair(t *testing.T) {
	store := newFakeStore()
	var reads atomic.Int64
	src := countedSource(t, "claude", t.TempDir(), testToken, nil, &reads)
	clock := &forecastClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	tp := forecastTransport("2026-03-01T18:00:00Z", 10, 25)
	svc := forecastStart(t, store, src, tp, clock)
	ctx := context.Background()

	if _, err := svc.Refresh(ctx, "claude"); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	clock.advance(15 * time.Minute)
	good, err := svc.Refresh(ctx, "claude")
	if err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	if !good.Forecast.Available || len(good.Observations) != 2 {
		t.Fatalf("good reading = %+v", good)
	}

	// A failed attempt repeats the previous windows without re-measuring them.
	// Repeating a reading must never look like a second sample.
	svc.transport = failingTransport(http.StatusInternalServerError)
	clock.advance(15 * time.Minute)
	failed, err := svc.Refresh(ctx, "claude")
	if err == nil {
		t.Fatal("expected the failing refresh to report an error")
	}
	if failed.Status != StatusUnavailable || reasonOf(err) != ReasonHTTPStatus {
		t.Fatalf("failed reading = %+v / %v", failed, err)
	}
	if len(failed.Observations) != 2 || !failed.Observations[0].ObservedAt.Equal(good.Observations[0].ObservedAt) {
		t.Fatalf("failure changed the samples: %+v", failed.Observations)
	}
	if failed.Forecast == nil || !failed.Forecast.Available || failed.Forecast.Windows[0].Observations != 2 {
		t.Fatalf("failure discarded a still-fresh pair: %+v", failed.Forecast)
	}

	// Once the pair itself is older than the reading's staleness budget, the
	// forecast is withheld instead of being presented as actionable.
	clock.advance(45 * time.Minute)
	stale, err := svc.Snapshot(ctx, "claude")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if stale.Status != StatusStale {
		t.Fatalf("reading status = %q, want %q", stale.Status, StatusStale)
	}
	if stale.Forecast == nil || stale.Forecast.Available {
		t.Fatalf("stale pair produced a forecast: %+v", stale.Forecast)
	}
	if got := forecastWindow(t, stale.Forecast, "five_hour"); got.Reason != quota.ReasonStale {
		t.Fatalf("stale reason = %q", got.Reason)
	}
}

func TestDisabledInstancePublishesNothingAndHasNoForecast(t *testing.T) {
	store := newFakeStore()
	var reads atomic.Int64
	src := countedSource(t, "claude", t.TempDir(), testToken, nil, &reads)
	clock := &forecastClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	tp := forecastTransport("2026-03-01T18:00:00Z", 10, 25)
	svc := forecastStart(t, store, src, tp, clock)
	ctx := context.Background()

	if _, err := svc.Refresh(ctx, "claude"); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	clock.advance(15 * time.Minute)
	if _, err := svc.Refresh(ctx, "claude"); err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	calls := tp.count()
	if err := svc.Disable(ctx, "claude"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if tp.count() != calls || reads.Load() != 2 {
		t.Fatalf("disable reached the network or the credential (%d/%d)", tp.count(), reads.Load())
	}
	if raw := store.get(LatestKey("claude")); raw != "" {
		t.Fatalf("disable left a reading behind: %q", raw)
	}
	q, err := svc.Snapshot(ctx, "claude")
	if err != nil {
		t.Fatalf("Snapshot after disable: %v", err)
	}
	if q.Status != StatusUnavailable || len(q.Observations) != 0 || q.Forecast != nil {
		t.Fatalf("disabled reading = %+v", q)
	}
	// Even with nothing retained, a caller always gets a reasoned forecast.
	fc := Forecasts(q, clock.now())
	if fc == nil || fc.Available || fc.Reason != quota.ReasonNoObservation {
		t.Fatalf("disabled forecast = %+v", fc)
	}
	// A disabled instance cannot publish: no consent means no refresh at all.
	clock.advance(15 * time.Minute)
	if _, err := svc.Refresh(ctx, "claude"); !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("refresh after disable = %v, want not enabled", err)
	}
	if tp.count() != calls || reads.Load() != 2 {
		t.Fatalf("disabled refresh reached the network or the credential (%d/%d)", tp.count(), reads.Load())
	}
}

func TestExpiredConsentNeverTouchesTheCachedReading(t *testing.T) {
	store := newFakeStore()
	var reads atomic.Int64
	src := countedSource(t, "claude", t.TempDir(), testToken, nil, &reads)
	clock := &forecastClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	tp := forecastTransport("2026-03-01T18:00:00Z", 10, 25)
	svc := forecastStart(t, store, src, tp, clock)
	ctx := context.Background()

	if _, err := svc.Refresh(ctx, "claude"); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	calls := tp.count()
	before := store.get(LatestKey("claude"))

	// Consent from another schema version is not consent: the attempt is
	// refused before any read or request, and the cached reading it would have
	// merged against is left exactly as it was.
	raw, err := encodeConsent(Consent{
		Version:    ConsentVersion + 1,
		Instance:   "claude",
		SourceKind: src.Kind,
		SourceRef:  src.Ref,
		Target:     Target,
		GrantedAt:  clock.now(),
	})
	if err != nil {
		t.Fatalf("encodeConsent: %v", err)
	}
	if err := store.SetSetting(ctx, ConsentKey("claude"), raw); err != nil {
		t.Fatal(err)
	}
	clock.advance(15 * time.Minute)
	if _, err := svc.Refresh(ctx, "claude"); reasonOf(err) != ReasonConsentInvalid {
		t.Fatalf("expired consent refresh = %v, want consent invalid", err)
	}
	if tp.count() != calls || reads.Load() != 1 {
		t.Fatalf("expired consent reached the network or the credential (%d/%d)", tp.count(), reads.Load())
	}
	if got := store.get(LatestKey("claude")); got != before {
		t.Fatalf("expired consent rewrote the cached reading: %q -> %q", before, got)
	}
}
