package quota

import (
	"sort"
	"time"
)

// Forecast defaults.
const (
	// DefaultForecastMaxAge is how old the newest retained observation may be
	// before a forecast is withheld as stale. A percentage-per-hour rate
	// extrapolated across a whole day of silence is not a current pace, and
	// nothing else on hand can date it better than the observation instant.
	DefaultForecastMaxAge = 6 * time.Hour

	// DefaultForecastSkew is the clock difference tolerated between a source
	// that stamps an observation and the process that reads it. Beyond it, an
	// observation dated in the future is reported as such instead of being
	// treated as fresh.
	DefaultForecastSkew = 2 * time.Minute

	// retainedPerWindow is how many observations are kept for one window. Two
	// are exactly what a pace needs; a third would only be history.
	retainedPerWindow = 2

	// maxRetainedObservations is the hard bound on one instance's retained
	// observations, so a pathological stream of window kinds can never grow a
	// stored Latest without limit.
	maxRetainedObservations = 16
)

// ForecastOptions tunes how a forecast is derived and when it is withheld. The
// zero value applies the defaults.
type ForecastOptions struct {
	// Pace bounds the two-sample comparison. The zero value applies the pace
	// defaults.
	Pace PaceOptions

	// MaxAge is how old the newest retained observation may be. Zero uses
	// DefaultForecastMaxAge; negative disables the check.
	MaxAge time.Duration

	// FutureSkew is how far ahead of the asking instant an observation may be
	// and still count as present. Zero uses DefaultForecastSkew.
	FutureSkew time.Duration
}

func (o ForecastOptions) withDefaults() ForecastOptions {
	if o.MaxAge == 0 {
		o.MaxAge = DefaultForecastMaxAge
	}
	if o.FutureSkew == 0 {
		o.FutureSkew = DefaultForecastSkew
	}
	return o
}

// Observation is one retained, trustworthy sample of a single quota window.
//
// It is deliberately smaller than a Report: it keeps the numbers a pace needs
// (the used percentage, its instant, and the reset the sample belongs to) plus
// the original source string and the limit identity, so two samples can be
// compared without the log and without guessing who produced them. It holds no
// credential, no log text and no token count: the rate is a percentage per
// unit time and nothing else.
//
// UsedPercent and ObservedAt are plain values because a sample without them is
// never retained. The optional fields are pointers so an unstated remaining
// percentage or reset stays distinguishable from zero.
type Observation struct {
	// Window is the window kind this sample describes.
	Window WindowKind `json:"window"`

	// LimitID and LimitName identify the quota bucket the source named. Two
	// samples of one window kind with different ids are different buckets.
	LimitID   string `json:"limit_id,omitempty"`
	LimitName string `json:"limit_name,omitempty"`

	// Source is the provenance label the writer recorded (a rollout path, or a
	// credential source reference). It is not a secret.
	Source string `json:"source,omitempty"`

	// ObservedAt is the instant the source stated for this sample.
	ObservedAt time.Time `json:"observed_at"`

	// UsedPercent is the stated fraction of the window already consumed.
	UsedPercent float64 `json:"used_percent"`

	// RemainingPercent is the stated remaining fraction, nil when unstated.
	RemainingPercent *float64 `json:"remaining_percent,omitempty"`

	// ResetsAt is the reset instant of the period this sample belongs to, nil
	// when unstated.
	ResetsAt *time.Time `json:"resets_at,omitempty"`
}

// Sample converts a retained observation into the value EstimatePace consumes.
func (o Observation) Sample() Sample {
	s := Sample{
		Kind:          o.Window,
		LimitID:       o.LimitID,
		ObservedAt:    o.ObservedAt,
		HasObservedAt: !o.ObservedAt.IsZero(),
		UsedPercent:   o.UsedPercent,
	}
	if o.RemainingPercent != nil {
		s.RemainingPercent = *o.RemainingPercent
		s.HasRemainingPercent = true
	}
	if o.ResetsAt != nil {
		s.ResetsAt = *o.ResetsAt
		s.HasResetsAt = true
	}
	return s
}

// WindowForecast is the forecast for one window of one instance. Pace is the
// two-sample estimate when the pair was comparable; Available states whether
// the forecast as a whole may be acted on, which is stricter than Pace being
// computable: a comparable pair that is stale or dated in the future still
// carries its Pace for display but is not Available.
type WindowForecast struct {
	// Window is the window kind.
	Window WindowKind `json:"window"`

	// LimitID and LimitName identify the quota bucket the samples named.
	LimitID   string `json:"limit_id,omitempty"`
	LimitName string `json:"limit_name,omitempty"`

	// Source is the provenance the retained samples agreed on, empty when they
	// did not come from one source.
	Source string `json:"source,omitempty"`

	// Available reports whether this window's forecast may be acted on.
	Available bool `json:"available"`

	// Reason explains an unavailable forecast, and is ReasonNone when
	// Available.
	Reason Reason `json:"reason,omitempty"`

	// Observations is how many samples were retained for this window. It is the
	// count that makes "one observation is not a rate" checkable by a reader.
	Observations int `json:"observations"`

	// FirstObservedAt and LastObservedAt bound the retained pair.
	FirstObservedAt *time.Time `json:"first_observed_at,omitempty"`
	LastObservedAt  *time.Time `json:"last_observed_at,omitempty"`

	// UsedPercent and RemainingPercent are the newest retained sample's level.
	UsedPercent      float64 `json:"used_percent"`
	RemainingPercent float64 `json:"remaining_percent"`

	// Pace is the underlying two-sample estimate. It is the zero Pace (not
	// available) when fewer than two comparable samples were retained.
	Pace Pace `json:"pace"`
}

// Forecast is the derived pace picture for one instance. It is a plain data
// structure with JSON tags so a CLI or GUI can serialize and show it directly;
// it carries no credential and no log text.
type Forecast struct {
	// Available reports whether at least one window has an actionable forecast.
	Available bool `json:"available"`

	// Reason is the first blocking reason when nothing is available, and
	// ReasonNone when something is.
	Reason Reason `json:"reason,omitempty"`

	// GeneratedAt is the instant the forecast was computed at. It is zero when
	// the caller asked without a clock.
	GeneratedAt time.Time `json:"generated_at,omitempty"`

	// ObservedAt is the newest instant among the retained observations, so a
	// consumer can see how old the input is. Nil when nothing is retained.
	ObservedAt *time.Time `json:"observed_at,omitempty"`

	// Windows holds one entry per window with a retained observation.
	Windows []WindowForecast `json:"windows,omitempty"`

	// Limitations are the fixed caveats that apply to every forecast.
	Limitations []string `json:"limitations,omitempty"`
}

// forecastLimitations is the fixed, non-negotiable caveat list. It is data
// rather than prose in a comment so it reaches the UI that shows the numbers.
var forecastLimitations = []string{
	"pace is a straight line through the two most recent samples, so bursty usage is not modelled",
	"a window reset discards the sample pair, so no forecast is stated until a sample from the new period arrives",
	"exhaustion is projected only while usage rises and the projection lands before the reset; otherwise the reset is reported instead",
}

// ForecastLimitations returns the fixed caveats that accompany every forecast.
// The returned slice is a copy, so a caller cannot alter the list for others.
func ForecastLimitations() []string {
	return append([]string(nil), forecastLimitations...)
}

// RetainObservations folds the observations of one published report into the
// bounded set already retained and returns the result.
//
// The rules are the whole contract of retention:
//
//   - untrustworthy input (no window, no instant, an instant outside the
//     trusted epoch, or an out-of-range percentage) is dropped, not repaired;
//   - a sample stating an instant already retained for that window is a repeat
//     of one import, not a new observation, and leaves the set unchanged;
//   - at most two observations are kept per window, and only a sample strictly
//     newer than the window's newest retained sample advances the pair, so the
//     pair is always an increasing interval in observation time;
//   - the whole set is bounded, so a stored Latest can never grow into history.
func RetainObservations(prev []Observation, add ...Observation) []Observation {
	out := make([]Observation, 0, len(prev)+len(add))
	for _, o := range prev {
		if trustworthyObservation(o) {
			out = append(out, o)
		}
	}
	sortObservations(out)
	for _, o := range add {
		if !trustworthyObservation(o) {
			continue
		}
		out = retainOne(out, o)
	}
	if len(out) > maxRetainedObservations {
		out = append([]Observation(nil), out[len(out)-maxRetainedObservations:]...)
	}
	return out
}

// retainOne adds one trustworthy observation to a set sorted by
// sortObservations.
func retainOne(sorted []Observation, o Observation) []Observation {
	lo, hi := windowRange(sorted, o.Window)
	if hi > lo {
		// Only a strictly newer sample advances a window's pair. An instant
		// already retained is the same import read again, an older one is a
		// backfill, and one that falls between the pair is out of order: none
		// of them is progress, and none may displace the pair a rate is using.
		if !o.ObservedAt.After(sorted[hi-1].ObservedAt) {
			return sorted
		}
	}
	if hi-lo >= retainedPerWindow {
		sorted = append(sorted[:lo], sorted[lo+1:]...)
	}
	sorted = append(sorted, o)
	sortObservations(sorted)
	return sorted
}

// trustworthyObservation reports whether a sample may be retained at all.
func trustworthyObservation(o Observation) bool {
	if o.Window == "" || o.ObservedAt.IsZero() {
		return false
	}
	if o.ObservedAt.Before(trustedEpochMin) || !o.ObservedAt.Before(trustedEpochMax) {
		return false
	}
	if !validPercent(o.UsedPercent) {
		return false
	}
	if o.RemainingPercent != nil && !validPercent(*o.RemainingPercent) {
		return false
	}
	return true
}

// sortObservations orders samples by window, then by observation instant, then
// by identity, so a stored payload is byte-stable for equal input.
func sortObservations(obs []Observation) {
	sort.SliceStable(obs, func(i, j int) bool {
		a, b := obs[i], obs[j]
		if a.Window != b.Window {
			return a.Window < b.Window
		}
		if !a.ObservedAt.Equal(b.ObservedAt) {
			return a.ObservedAt.Before(b.ObservedAt)
		}
		if a.LimitID != b.LimitID {
			return a.LimitID < b.LimitID
		}
		return a.Source < b.Source
	})
}

// windowRange returns the half-open range of one window inside a sorted set.
func windowRange(sorted []Observation, kind WindowKind) (int, int) {
	lo := sort.Search(len(sorted), func(i int) bool { return sorted[i].Window >= kind })
	hi := lo
	for hi < len(sorted) && sorted[hi].Window == kind {
		hi++
	}
	return lo, hi
}

// Forecasts derives the pace forecast a stored Latest carries.
//
// It is the entry point a CLI or GUI should call: it reads only the retained
// observations, never a credential, the network or the clock (a zero now falls
// back to IngestedAt, and with neither there is simply no age to judge). Pass
// the zero ForecastOptions for the defaults.
//
// The rate is a percentage of the window per unit time, derived only from the
// reported percentages and the instants they were observed at. Tokens and cost
// are never observed here and are never inferred from a percentage.
func Forecasts(latest *Latest, now time.Time, opts ForecastOptions) *Forecast {
	var obs []Observation
	if latest != nil {
		obs = latest.Observations
		if now.IsZero() {
			now = latest.IngestedAt
		}
	}
	return ForecastObservations(obs, now, opts)
}

// ForecastObservations derives a forecast from retained observations alone, for
// callers that hold observations without a Latest envelope.
//
// It never returns nil: an absent forecast is a Forecast with Available=false
// and a Reason, so a consumer always has something to render. Every window with
// observations gets an entry, including one that cannot be forecast, because
// "one sample so far" is information rather than silence.
func ForecastObservations(observations []Observation, now time.Time, opts ForecastOptions) *Forecast {
	opts = opts.withDefaults()
	fc := &Forecast{GeneratedAt: now, Limitations: ForecastLimitations()}

	kept := make([]Observation, 0, len(observations))
	for _, o := range observations {
		if trustworthyObservation(o) {
			kept = append(kept, o)
		}
	}
	sortObservations(kept)
	if len(kept) == 0 {
		fc.Reason = ReasonNoObservation
		return fc
	}
	newest := kept[0].ObservedAt
	for _, o := range kept[1:] {
		if o.ObservedAt.After(newest) {
			newest = o.ObservedAt
		}
	}
	fc.ObservedAt = &newest

	for _, kind := range windowKinds(kept) {
		fc.Windows = append(fc.Windows, forecastWindow(kept, kind, now, opts))
	}
	fc.Reason = fc.Windows[0].Reason
	for _, wf := range fc.Windows {
		if wf.Available {
			fc.Available, fc.Reason = true, ReasonNone
			break
		}
	}
	return fc
}

// windowKinds lists the distinct window kinds of a sorted set, in order.
func windowKinds(sorted []Observation) []WindowKind {
	var out []WindowKind
	for _, o := range sorted {
		if len(out) == 0 || out[len(out)-1] != o.Window {
			out = append(out, o.Window)
		}
	}
	return out
}

// forecastWindow derives one window's verdict from its retained samples.
func forecastWindow(sorted []Observation, kind WindowKind, now time.Time, opts ForecastOptions) WindowForecast {
	lo, hi := windowRange(sorted, kind)
	bucket := sorted[lo:hi]
	last := bucket[len(bucket)-1]
	wf := WindowForecast{
		Window:          kind,
		LimitID:         last.LimitID,
		LimitName:       last.LimitName,
		Source:          last.Source,
		Observations:    len(bucket),
		FirstObservedAt: timePtr(bucket[0].ObservedAt),
		LastObservedAt:  timePtr(last.ObservedAt),
		UsedPercent:     last.UsedPercent,
	}
	wf.RemainingPercent = 100 - last.UsedPercent
	if last.RemainingPercent != nil {
		wf.RemainingPercent = *last.RemainingPercent
	}
	if len(bucket) > 1 && bucket[0].Source != last.Source {
		wf.Source = ""
		wf.Reason = ReasonSourceMismatch
		return wf
	}
	if len(bucket) < 2 {
		wf.Reason = ReasonInsufficientSamples
		return wf
	}
	prev := bucket[len(bucket)-2]
	wf.Pace = EstimatePace(prev.Sample(), last.Sample(), opts.Pace)
	switch {
	case !wf.Pace.Available:
		wf.Reason = wf.Pace.Reason
	case !now.IsZero() && last.ObservedAt.After(now.Add(opts.FutureSkew)):
		wf.Reason = ReasonFutureObservation
	case !now.IsZero() && now.Sub(last.ObservedAt) > opts.MaxAge:
		wf.Reason = ReasonStale
	default:
		wf.Available = true
	}
	return wf
}

// reportObservations extracts every trustworthy window sample a Report carries.
// A report that is not ok (stale, superseded, unusable) contributes nothing: a
// historical value must never be retained as if it were a fresh measurement.
func reportObservations(rep *Report) []Observation {
	if rep == nil {
		return nil
	}
	out := make([]Observation, 0, 2)
	for _, kind := range []WindowKind{WindowPrimary, WindowSecondary} {
		if o, ok := reportObservation(rep, kind); ok {
			out = append(out, o)
		}
	}
	return out
}

func reportObservation(rep *Report, kind WindowKind) (Observation, bool) {
	if rep == nil || rep.Status != StatusOK || rep.ObservedAt == nil {
		return Observation{}, false
	}
	at := rep.ObservedAt.UTC()
	if at.IsZero() || at.Before(trustedEpochMin) || !at.Before(trustedEpochMax) {
		return Observation{}, false
	}
	s, ok := rep.Sample(kind)
	if !ok {
		return Observation{}, false
	}
	o := Observation{
		Window:      kind,
		LimitID:     s.LimitID,
		LimitName:   rep.LimitName,
		Source:      rep.Source,
		ObservedAt:  at,
		UsedPercent: s.UsedPercent,
	}
	if s.HasRemainingPercent {
		remaining := s.RemainingPercent
		o.RemainingPercent = &remaining
	}
	if s.HasResetsAt {
		reset := s.ResetsAt
		o.ResetsAt = &reset
	}
	return o, true
}

func timePtr(t time.Time) *time.Time { return &t }
