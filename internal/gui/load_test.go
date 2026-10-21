package gui

import (
	"math"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

type rateSpec struct {
	age       time.Duration // before now
	out       int64
	reasoning int64
	decodeMS  int64 // the request's own generation window, when the log timed it
}

// rateEvents builds events in log order, oldest first.
func rateEvents(now time.Time, specs ...rateSpec) []query.AttributedEvent {
	out := make([]query.AttributedEvent, 0, len(specs))
	for _, s := range specs {
		e := query.AttributedEvent{UsageEvent: model.UsageEvent{Timestamp: now.Add(-s.age), DecodeMS: s.decodeMS}}
		e.Tokens = model.Tokens{Output: s.out, Reasoning: s.reasoning}
		out = append(out, e)
	}
	return out
}

func TestOutputRateSteadySpacing(t *testing.T) {
	now := time.Now()
	// Six requests four seconds apart, two hundred output tokens each.
	ev := rateEvents(now,
		rateSpec{age: 20 * time.Second, out: 200}, rateSpec{age: 16 * time.Second, out: 200},
		rateSpec{age: 12 * time.Second, out: 200}, rateSpec{age: 8 * time.Second, out: 200},
		rateSpec{age: 4 * time.Second, out: 200}, rateSpec{age: 0, out: 200})
	rate, requests := outputRate(ev, now)
	if requests != 5 {
		t.Fatalf("requests = %d, want 5", requests)
	}
	if math.Abs(rate-50) > 1e-9 {
		t.Fatalf("rate = %v, want 50", rate)
	}
}

func TestOutputRateCountsReasoning(t *testing.T) {
	now := time.Now()
	// Reasoning is generated output too: 200 output + 200 reasoning over 8 s.
	ev := rateEvents(now, rateSpec{age: 8 * time.Second}, rateSpec{age: 0, out: 200, reasoning: 200})
	rate, _ := outputRate(ev, now)
	if math.Abs(rate-50) > 1e-9 {
		t.Fatalf("rate = %v, want 50", rate)
	}
}

// A codex turn can land several records inside one millisecond; dividing a
// request's tokens by that gap used to read millions of tokens per second.
func TestOutputRateRepairsMillisecondTimestamps(t *testing.T) {
	now := time.Now()
	ev := rateEvents(now, rateSpec{age: 60 * time.Second}, rateSpec{age: 59*time.Second - time.Millisecond, out: 4131})
	rate, _ := outputRate(ev, now)
	if rate <= 0 {
		t.Fatalf("rate = %v, want a positive estimate", rate)
	}
	if rate > 300 {
		t.Fatalf("rate = %v, want at most 300 tok/s", rate)
	}
}

// One pathological pair must not poison a session whose other requests are
// spaced plausibly.
func TestOutputRateOneBadPairDoesNotPoison(t *testing.T) {
	now := time.Now()
	ev := rateEvents(now,
		rateSpec{age: 24 * time.Second, out: 200},
		rateSpec{age: 20 * time.Second, out: 200},
		rateSpec{age: 16 * time.Second, out: 200},
		rateSpec{age: 12 * time.Second, out: 4000},
		rateSpec{age: 12*time.Second - time.Millisecond, out: 4000},
		rateSpec{age: 8 * time.Second, out: 200},
		rateSpec{age: 4 * time.Second, out: 200},
		rateSpec{age: 0, out: 200})
	rate, _ := outputRate(ev, now)
	if rate > 300 {
		t.Fatalf("rate = %v, want at most 300 tok/s", rate)
	}
	// The ordinary pairs still read near 50 tok/s, and the 4000-token request
	// counts as one fast request rather than as nothing at all.
	if rate < 50 {
		t.Fatalf("rate = %v, want at least the ordinary 50 tok/s", rate)
	}
}

func TestOutputRateSameTimestamp(t *testing.T) {
	now := time.Now()
	ev := rateEvents(now, rateSpec{age: 0, out: 300}, rateSpec{age: 0, out: 300})
	rate, requests := outputRate(ev, now)
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
	if rate <= 0 || rate > 300 {
		t.Fatalf("rate = %v, want a positive estimate of at most 300 tok/s", rate)
	}
}

// A pause is not generation: a pair of requests split by one says nothing
// about speed now, so it is dropped rather than reported as a slow request.
func TestOutputRateDropsPause(t *testing.T) {
	now := time.Now()
	ev := rateEvents(now, rateSpec{age: 9 * time.Minute}, rateSpec{age: 0, out: 90})
	rate, _ := outputRate(ev, now)
	if rate != 0 {
		t.Fatalf("rate = %v, want 0: a nine-minute pause is not a speed", rate)
	}
}

// The number is about now, not about the last ten minutes: a session whose
// newest request is minutes old shows no speed, though it is still counted
// among the card's requests.
func TestOutputRateGoesQuietWhenNothingArrives(t *testing.T) {
	now := time.Now()
	ev := rateEvents(now,
		rateSpec{age: 6 * time.Minute, out: 200},
		rateSpec{age: 5 * time.Minute, out: 200},
		rateSpec{age: 4 * time.Minute, out: 200})
	rate, requests := outputRate(ev, now)
	if rate != 0 {
		t.Fatalf("rate = %v, want 0 for requests four minutes old", rate)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2: the count still covers ten minutes", requests)
	}
}

// A measured window goes quiet the same way, from the same clock.
func TestOutputRateMeasurementGoesQuiet(t *testing.T) {
	now := time.Now()
	ev := rateEvents(now,
		rateSpec{age: 3 * time.Minute, out: 250, decodeMS: 1000},
		rateSpec{age: 2 * time.Minute, out: 250, decodeMS: 1000})
	if rate, _ := outputRate(ev, now); rate != 0 {
		t.Fatalf("rate = %v, want 0 for a window that ended two minutes ago", rate)
	}
}

func TestOutputRateIgnoresOldRequests(t *testing.T) {
	now := time.Now()
	ev := rateEvents(now, rateSpec{age: 30 * time.Minute, out: 500}, rateSpec{age: 20 * time.Minute, out: 500})
	rate, requests := outputRate(ev, now)
	if rate != 0 || requests != 0 {
		t.Fatalf("rate = %v, requests = %d; want 0, 0", rate, requests)
	}
}

func TestOutputRateDegenerate(t *testing.T) {
	now := time.Now()
	if rate, requests := outputRate(nil, now); rate != 0 || requests != 0 {
		t.Fatalf("empty: rate = %v, requests = %d; want 0, 0", rate, requests)
	}
	// A single request has no gap to learn from.
	if rate, _ := outputRate(rateEvents(now, rateSpec{age: 0, out: 500}), now); rate != 0 {
		t.Fatalf("one request: rate = %v, want 0", rate)
	}
	// Input-only records generate no output tokens.
	if rate, _ := outputRate(rateEvents(now, rateSpec{age: 4 * time.Second}, rateSpec{age: 0}), now); rate != 0 {
		t.Fatalf("no output: rate = %v, want 0", rate)
	}
}

// Timestamps that go backwards are repaired, never divided into negative time.
func TestOutputRateClockRegression(t *testing.T) {
	now := time.Now()
	ev := rateEvents(now, rateSpec{age: 8 * time.Second, out: 200}, rateSpec{age: 0, out: 900}, rateSpec{age: 4 * time.Second, out: 200})
	rate, _ := outputRate(ev, now)
	if math.IsNaN(rate) || math.IsInf(rate, 0) {
		t.Fatalf("rate = %v, want a finite estimate", rate)
	}
	if rate <= 0 || rate > 300 {
		t.Fatalf("rate = %v, want a positive estimate of at most 300 tok/s", rate)
	}
}

// A log that timed its own stream (DSH) is read the way that tool reads itself:
// output tokens over the decode window, not over the gap between requests. Here
// the two requests are five seconds apart, but each spent one second decoding.
func TestOutputRateUsesMeasuredDecodeWindow(t *testing.T) {
	now := time.Now()
	ev := rateEvents(now,
		rateSpec{age: 5 * time.Second, out: 250, decodeMS: 1000},
		rateSpec{age: 0, out: 250, decodeMS: 1000})
	rate, requests := outputRate(ev, now)
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
	if math.Abs(rate-250) > 1e-9 {
		t.Fatalf("rate = %v, want 250 (the decode window, not the 100 tok/s the gap implies)", rate)
	}
}

// The measured window counts the tokens the harness counted as output, which
// excludes reasoning — the same number DSH prints beside it.
func TestOutputRateMeasurementExcludesReasoning(t *testing.T) {
	now := time.Now()
	ev := rateEvents(now,
		rateSpec{age: 2 * time.Second, out: 100, reasoning: 900, decodeMS: 1000},
		rateSpec{age: 0, out: 100, reasoning: 900, decodeMS: 1000})
	rate, _ := outputRate(ev, now)
	if math.Abs(rate-100) > 1e-9 {
		t.Fatalf("rate = %v, want 100 (counted reasoning would read 1000)", rate)
	}
}

// An estimate must never overrule a measurement: as soon as one request in the
// window reports its own decode window, the gaps are not consulted.
func TestOutputRateMeasurementWinsOverGaps(t *testing.T) {
	now := time.Now()
	ev := rateEvents(now,
		rateSpec{age: 6 * time.Second, out: 200},
		rateSpec{age: 3 * time.Second, out: 200, decodeMS: 2000},
		rateSpec{age: 0, out: 200})
	rate, _ := outputRate(ev, now)
	if math.Abs(rate-100) > 1e-9 {
		t.Fatalf("rate = %v, want 100 (200 tokens over the 2 s the log timed)", rate)
	}
}

// Measurements age out of the window like everything else.
func TestOutputRateMeasuredIgnoredWhenOld(t *testing.T) {
	now := time.Now()
	ev := rateEvents(now,
		rateSpec{age: 30 * time.Minute, out: 250, decodeMS: 1000},
		rateSpec{age: 29 * time.Minute, out: 250, decodeMS: 1000})
	rate, _ := outputRate(ev, now)
	if rate != 0 {
		t.Fatalf("rate = %v, want 0 (both requests are outside the window)", rate)
	}
}
