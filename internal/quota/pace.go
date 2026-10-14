package quota

import (
	"math"
	"time"
)

// Pace defaults.
const (
	// DefaultMinElapsed is the shortest interval between two observations that
	// EstimatePace will accept. Below it, the ratio of a small usage delta to a
	// tiny interval is noise rather than a rate.
	DefaultMinElapsed = time.Second

	// DefaultMaxElapsed is disabled: the caller decides when two observations
	// are too far apart to describe the current pace.
	DefaultMaxElapsed time.Duration = 0

	// DefaultMaxPercentPerHour rejects a rate above 100% per minute.
	DefaultMaxPercentPerHour = 3600
)

// maxForecastHours bounds the projection horizon at 100 years, so a very slow
// rate cannot overflow a time.Duration.
const maxForecastHours = float64(100 * 365 * 24)

// Sample is one observation of a single quota window, as extracted from a
// Report. The Has* flags exist because a zero time.Time, and a zero percent,
// are legitimate values that must not be confused with "not stated".
type Sample struct {
	Kind WindowKind

	// LimitID is the rate-limit identity the observation was reported under,
	// empty when the record did not state one. Two samples of the same kind
	// from different limits are different quota buckets and are not comparable.
	LimitID string

	ObservedAt    time.Time
	HasObservedAt bool

	UsedPercent      float64
	RemainingPercent float64

	// HasRemainingPercent reports whether the log stated the remaining
	// percentage. When false, EstimatePace derives it from UsedPercent.
	HasRemainingPercent bool

	ResetsAt    time.Time
	HasResetsAt bool
}

// PaceOptions bounds the comparison. The zero value applies the defaults.
type PaceOptions struct {
	// MinElapsed is the shortest accepted interval. Zero uses
	// DefaultMinElapsed; negative disables the check.
	MinElapsed time.Duration

	// MaxElapsed is the longest accepted interval. Zero (the default) disables
	// the check; positive enforces it.
	MaxElapsed time.Duration

	// MaxPercentPerHour rejects implausible rates. Zero uses
	// DefaultMaxPercentPerHour; negative disables the check.
	MaxPercentPerHour float64
}

// Pace is a consumption-rate estimate derived from exactly two observations of
// the same window. It is a pure calculation: it reads no log, touches no
// clock, and never guesses a missing input.
type Pace struct {
	// Available reports whether the two samples were comparable.
	Available bool   `json:"available"`
	Reason    Reason `json:"reason,omitempty"`

	Elapsed          time.Duration `json:"elapsed"`
	UsedDeltaPercent float64       `json:"used_delta_percent"`
	RemainingPercent float64       `json:"remaining_percent"`

	// PercentPerHour is the measured rate over the interval. Zero is a real
	// measurement (nothing was consumed); read Available to tell it apart from
	// "no estimate".
	PercentPerHour float64 `json:"percent_per_hour"`
	PercentPerDay  float64 `json:"percent_per_day"`

	// EvenPercentPerHour is the rate that would exhaust the window exactly when
	// it resets, based on the later sample. It is 0 when no reset is known.
	EvenPercentPerHour float64 `json:"even_percent_per_hour,omitempty"`

	// TimeToExhaustion and ExhaustsAt project the measured rate forward. They
	// are zero/nil when nothing is being consumed, or when the horizon is too
	// far away to be meaningful.
	TimeToExhaustion time.Duration `json:"time_to_exhaustion,omitempty"`
	ExhaustsAt       *time.Time    `json:"exhausts_at,omitempty"`

	// ResetsBeforeExhaustion reports that the window resets before the
	// projected exhaustion, so the projection is not actionable.
	ResetsBeforeExhaustion bool `json:"resets_before_exhaustion,omitempty"`
}

func unavailable(reason Reason) Pace {
	return Pace{Available: false, Reason: reason}
}

func validPercent(p float64) bool {
	return !math.IsNaN(p) && !math.IsInf(p, 0) && p >= 0 && p <= 100
}

// EstimatePace derives a consumption rate from two observations of the same
// quota window.
//
// It returns an unavailable Pace with an explicit Reason for every pair it
// cannot soundly compare: mismatched windows, an observation timestamp or
// reset it does not know, a reset rollover, disagreeing resets, a
// non-positive or implausibly short interval, decreasing usage, and
// out-of-range percentages.
func EstimatePace(prev, next Sample, opts PaceOptions) Pace {
	minElapsed := opts.MinElapsed
	if minElapsed == 0 {
		minElapsed = DefaultMinElapsed
	}
	maxPercentPerHour := opts.MaxPercentPerHour
	if maxPercentPerHour == 0 {
		maxPercentPerHour = DefaultMaxPercentPerHour
	}

	if prev.Kind != next.Kind {
		return unavailable(ReasonWindowMismatch)
	}
	// The same window kind is still a different quota bucket when the two
	// observations name different limits. An identity known on only one side is
	// not enough to prove they match, so that is refused too.
	if prev.LimitID != next.LimitID && (prev.LimitID != "" || next.LimitID != "") {
		return unavailable(ReasonLimitMismatch)
	}
	if !prev.HasObservedAt || !next.HasObservedAt {
		return unavailable(ReasonNoTimestamp)
	}
	if !validPercent(prev.UsedPercent) || !validPercent(next.UsedPercent) {
		return unavailable(ReasonMalformed)
	}

	// Reset metadata may be absent from both observations; the rate is still
	// measurable, but nothing can be said about the even-use line. It must not
	// be absent from only one, because then the two samples disagree about
	// which period they describe.
	if prev.HasResetsAt != next.HasResetsAt {
		return unavailable(ReasonResetUnknown)
	}
	haveReset := prev.HasResetsAt
	if haveReset {
		if !prev.ResetsAt.After(next.ObservedAt) {
			// The first observation's reset already elapsed by the time of the
			// second, so the usage counter restarted between them.
			return unavailable(ReasonResetRollover)
		}
		if !prev.ResetsAt.Equal(next.ResetsAt) {
			return unavailable(ReasonResetMismatch)
		}
		if !next.ResetsAt.After(next.ObservedAt) {
			return unavailable(ReasonResetRollover)
		}
	}

	elapsed := next.ObservedAt.Sub(prev.ObservedAt)
	switch {
	case elapsed == 0:
		return unavailable(ReasonZeroElapsed)
	case elapsed < 0:
		return unavailable(ReasonOutlier)
	case minElapsed > 0 && elapsed < minElapsed:
		return unavailable(ReasonOutlier)
	case opts.MaxElapsed > 0 && elapsed > opts.MaxElapsed:
		return unavailable(ReasonOutlier)
	}

	delta := next.UsedPercent - prev.UsedPercent
	if delta < 0 {
		return unavailable(ReasonDecreasingUsage)
	}

	rate := delta / elapsed.Hours()
	if maxPercentPerHour > 0 && rate > maxPercentPerHour {
		return unavailable(ReasonOutlier)
	}

	remaining := 100 - next.UsedPercent
	if next.HasRemainingPercent {
		remaining = next.RemainingPercent
	}
	remaining = math.Max(0, math.Min(100, remaining))

	p := Pace{
		Available:        true,
		Reason:           ReasonNone,
		Elapsed:          elapsed,
		UsedDeltaPercent: delta,
		RemainingPercent: remaining,
		PercentPerHour:   rate,
		PercentPerDay:    rate * 24,
	}

	hoursToReset := 0.0
	if haveReset {
		hoursToReset = next.ResetsAt.Sub(next.ObservedAt).Hours()
		if hoursToReset > 0 {
			p.EvenPercentPerHour = remaining / hoursToReset
		}
	}

	if rate > 0 && remaining > 0 {
		if hours := remaining / rate; hours <= maxForecastHours {
			p.TimeToExhaustion = time.Duration(hours * float64(time.Hour))
			at := next.ObservedAt.Add(p.TimeToExhaustion)
			p.ExhaustsAt = &at
			if haveReset && at.After(next.ResetsAt) {
				p.ResetsBeforeExhaustion = true
			}
		}
	}
	return p
}
