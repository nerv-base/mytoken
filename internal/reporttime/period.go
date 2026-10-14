// Package reporttime computes reporting and budget periods in one fixed,
// caller-supplied timezone.
//
// It is pure calculation: no persistence, no global state, and it never reads
// or writes time.Local. Windows are half-open: [Start, End). Boundaries are
// calendar boundaries: a day across a daylight-saving change is 23h or 25h and
// a week is 167h or 169h rather than a fixed 24h/7d.
//
// A boundary is the earliest instant whose wall-clock reading in the location
// is at or after the nominal local midnight of the selected calendar date. It
// cannot be built with time.Date alone, because time.Date normalizes a missing
// or repeated local midnight to some other instant. Boundaries are instead
// derived from the location's zone transitions, which handles the three
// pathological cases:
//
//   - midnight does not exist (clocks jump 00:00 -> 01:00): the boundary is the
//     transition instant;
//   - midnight occurs twice (clocks fall back 01:00 -> 00:00): the boundary is
//     the earlier occurrence;
//   - the whole date was skipped (a date-line change): the date has no
//     instants, its period is empty, and Previous skips it.
//
// The search around the nominal midnight is bounded: 15-minute steps over a
// 40-day window at most, refined by bisection. It never examines every second
// and never walks the location's transition history. A location whose zone data
// does not match those assumptions is not claimed correct; if no transition can
// explain the date, the nominal time.Date result is returned rather than
// scanning further.
//
// A caller must pass a valid non-nil *time.Location and a canonical Kind.
// There is no silent fallback to time.Local or UTC; nil and invalid periods
// are rejected with an error.
package reporttime

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Kind identifies a calendar period. Only these canonical values are valid;
// ParseKind normalizes user input, but Resolve and Valid never guess.
type Kind string

const (
	// Day is one local calendar day: local midnight to the next local midnight.
	Day Kind = "day"
	// Week is one local calendar week starting Monday and ending the next Monday.
	Week Kind = "week"
	// Month is one local calendar month: the 1st to the 1st of the next month.
	Month Kind = "month"
)

// Valid reports whether k is one of the canonical kinds, Day, Week or Month.
// It is exact: "DAY" and "Month" are not valid.
func (k Kind) Valid() bool {
	return k == Day || k == Week || k == Month
}

// ParseKind normalizes user input (surrounding space and case) into a canonical
// Kind. Anything else is an error; nothing defaults to a period.
func ParseKind(s string) (Kind, error) {
	k := Kind(strings.ToLower(strings.TrimSpace(s)))
	if !k.Valid() {
		return "", fmt.Errorf("reporttime: invalid period kind %q (want day, week or month)", s)
	}
	return k, nil
}

// Window is a half-open interval [Start, End) over absolute instants. End is
// exclusive. The two ends always share one *time.Location.
type Window struct {
	Start, End time.Time
}

// Duration is the real elapsed time between the boundaries. Because the ends
// are absolute instants, a local day that spans a DST transition measures 23h
// or 25h, and a week containing one measures 167h or 169h. Duration is never a
// nominal "24h/7d/30d".
func (w Window) Duration() time.Duration { return w.End.Sub(w.Start) }

// IsEmpty reports whether the window covers no instants (Start is at or after
// End), which is the case for an elapsed window taken exactly on a boundary.
func (w Window) IsEmpty() bool { return !w.Start.Before(w.End) }

// Contains reports whether t falls in [Start, End). End itself is not included.
func (w Window) Contains(t time.Time) bool {
	return !t.Before(w.Start) && t.Before(w.End)
}

// Period is one full calendar period in one timezone. Start is inclusive, End
// exclusive, and both are the earliest instants of local calendar boundaries in
// Start's location.
type Period struct {
	Kind       Kind
	Start, End time.Time
}

// Location is the timezone the boundaries were built in. It is the location of
// Start, so a Period carries its own zone and needs no external loc.
func (p Period) Location() *time.Location { return p.Start.Location() }

// Window returns the period as a half-open [Start, End) interval.
func (p Period) Window() Window { return Window{Start: p.Start, End: p.End} }

// ID is a stable period identity: zone name, kind and the actual start instant.
// It is safe to store or use as a dedup key, and intentionally differs across
// timezones, kinds and period starts so two different periods can never be
// reused as one. The key is the instant rather than a nominal calendar date, so
// a period that starts at a DST transition instant (a missing or repeated
// midnight) is never confused with one that starts at a nominal midnight on the
// same local date. RFC3339Nano keeps the identity exact at instant granularity
// while printing no fractional part for the whole-second instants that zone
// transitions and calendar crossings produce.
func (p Period) ID() string {
	return fmt.Sprintf("period:v1:%s:%s:%s", p.Kind, p.Location(), p.Start.Format(time.RFC3339Nano))
}

// Previous returns the full calendar period immediately before p in the same
// timezone: the day before, the Monday-to-Monday week before, or the month
// before. Stepping is calendar arithmetic on the nominal date, never AddDate on
// the boundary instant, so it cannot drift across a transition and months keep
// their real length (February is 28 or 29 days). A date that never existed
// (a date-line change) has an empty period, so Previous skips it and still ends
// exactly at p.Start; at most maxSkippedDates calendar steps are examined.
// Previous of a Period with an invalid Kind is the zero Period.
func (p Period) Previous() Period {
	if !p.Kind.Valid() {
		return Period{}
	}
	loc := p.Location()
	y, m, d := p.Start.In(loc).Date()
	end := p.Start

	switch p.Kind {
	case Day:
		start := previousStart(end, loc, func(i int) (int, time.Month, int) {
			return calendarDate(y, m, d-i)
		})
		return Period{Kind: Day, Start: start, End: end}
	case Week:
		// Derive the nominal Monday from the local weekday, so a week whose
		// Monday date was skipped still steps back exactly one calendar week.
		offset := (int(p.Start.In(loc).Weekday()) + 6) % 7
		start := previousStart(end, loc, func(i int) (int, time.Month, int) {
			return calendarDate(y, m, d-offset-7*i)
		})
		return Period{Kind: Week, Start: start, End: end}
	default: // Month
		start := previousStart(end, loc, func(i int) (int, time.Month, int) {
			return calendarDate(y, m-time.Month(i), 1)
		})
		return Period{Kind: Month, Start: start, End: end}
	}
}

// previousStart returns the boundary of the most recent calendar occurrence of
// the kind's start date that is strictly before end. at(i) yields the i-th
// occurrence going back; whole skipped dates cost at most maxSkippedDates
// steps.
func previousStart(end time.Time, loc *time.Location, at func(i int) (int, time.Month, int)) time.Time {
	for i := 1; i <= maxSkippedDates; i++ {
		y, m, d := at(i)
		if candidate := localBoundary(y, m, d, loc); candidate.Before(end) {
			return candidate
		}
	}
	y, m, d := at(1)
	return localBoundary(y, m, d, loc)
}

// State is one period resolved at one instant.
type State struct {
	Kind Kind
	// Now is now normalized into the requested location.
	Now time.Time
	// Current is the full current period.
	Current Period
	// Elapsed is the part of Current that has happened: [Current.Start, Now).
	// It is measured as real elapsed duration, not as a percentage of the
	// period, so a DST transition inside the period does not distort it.
	Elapsed Window
	// Previous is the full previous period, whatever its length.
	Previous Period
	// Comparison is the same elapsed duration applied to Previous:
	// [Previous.Start, Previous.Start+Elapsed], capped at Previous.End because
	// the previous period may be shorter than the elapsed part of the current
	// one (for example comparing 31 elapsed days of March against February).
	Comparison Window
}

// Location is the timezone the state was resolved in.
func (s State) Location() *time.Location { return s.Current.Location() }

// Resolve computes the current full boundaries, the elapsed part of the current
// period, the previous full boundaries and the capped comparison window for the
// period containing now.
//
// loc must be a valid non-nil *time.Location; a nil location is an error and is
// never replaced by time.Local or UTC. kind must be canonical (Day, Week or
// Month); anything else is an error. now is converted into loc, so passing the
// same instant in a different input location yields the same result.
//
// Resolve has no side effects: it does not touch time.Local and holds no
// package state. Persistence of a fixed reporting timezone belongs in the
// integration layer, not here.
func Resolve(now time.Time, kind Kind, loc *time.Location) (State, error) {
	if loc == nil {
		return State{}, errors.New("reporttime: nil location; caller must supply a valid *time.Location")
	}
	if !kind.Valid() {
		return State{}, fmt.Errorf("reporttime: invalid period kind %q (want day, week or month)", kind)
	}
	current := periodContaining(now, kind, loc)
	previous := current.Previous()

	nowLocal := now.In(loc)
	// Elapsed is the real elapsed duration between the two instants, so it stays
	// correct across a DST transition. It is deliberately not a percent of the
	// period: percentage would turn 23h/25h days into a lie.
	elapsed := nowLocal.Sub(current.Start)
	if elapsed < 0 {
		// Current.Start is the earliest instant of the selected local calendar
		// date, so it cannot follow now. Keep the no-negative-elapsed contract
		// even if a location's zone data ever defeats the boundary search.
		elapsed = 0
	}
	comparisonEnd := previous.Start.Add(elapsed)
	if comparisonEnd.After(previous.End) {
		comparisonEnd = previous.End
	}
	return State{
		Kind:       kind,
		Now:        nowLocal,
		Current:    current,
		Elapsed:    Window{Start: current.Start, End: current.Start.Add(elapsed)},
		Previous:   previous,
		Comparison: Window{Start: previous.Start, End: comparisonEnd},
	}, nil
}

// periodContaining builds the full period of kind that contains now, in loc.
// The period is built from the nominal calendar date of now in loc, not from
// now's instant, so every period is keyed to a local calendar date.
func periodContaining(now time.Time, kind Kind, loc *time.Location) Period {
	t := now.In(loc)
	y, m, d := t.Date()
	switch kind {
	case Day:
		return dayPeriod(y, m, d, loc)
	case Week:
		// Monday is the first day of the week: Sunday (Weekday 0) is offset 6.
		offset := (int(t.Weekday()) + 6) % 7
		my, mm, md := calendarDate(y, m, d-offset)
		return weekPeriod(my, mm, md, loc)
	case Month:
		return monthPeriod(y, m, loc)
	}
	return Period{}
}

func dayPeriod(y int, m time.Month, d int, loc *time.Location) Period {
	ny, nm, nd := calendarDate(y, m, d+1)
	return Period{Kind: Day, Start: localBoundary(y, m, d, loc), End: localBoundary(ny, nm, nd, loc)}
}

func weekPeriod(y int, m time.Month, d int, loc *time.Location) Period {
	ny, nm, nd := calendarDate(y, m, d+7)
	return Period{Kind: Week, Start: localBoundary(y, m, d, loc), End: localBoundary(ny, nm, nd, loc)}
}

func monthPeriod(y int, m time.Month, loc *time.Location) Period {
	ny, nm, _ := calendarDate(y, m+1, 1)
	return Period{Kind: Month, Start: localBoundary(y, m, 1, loc), End: localBoundary(ny, nm, 1, loc)}
}

// calendarDate normalizes a nominal calendar date through time.Date in UTC,
// which has no transitions, so day and month arithmetic never drifts. It steps
// calendar dates, not instants.
func calendarDate(y int, m time.Month, d int) (int, time.Month, int) {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Date()
}

// Boundary search bounds. The spans are tried in order around the nominal
// midnight, and the whole search is bounded by them: no per-second walk and no
// scan of the location's transition history.
const (
	boundaryStep    = 15 * time.Minute
	maxSkippedDates = 4
	maxTransitions  = 64
)

var boundarySpans = [...]time.Duration{72 * time.Hour, 8 * 24 * time.Hour, 40 * 24 * time.Hour}

// wallClock returns t's local wall-clock reading in loc, encoded as a UTC Time
// so two readings compare directly. The zone offset is deliberately dropped:
// this is a calendar reading, not an instant.
func wallClock(t time.Time, loc *time.Location) time.Time {
	lt := t.In(loc)
	return time.Date(lt.Year(), lt.Month(), lt.Day(), lt.Hour(), lt.Minute(), lt.Second(), lt.Nanosecond(), time.UTC)
}

// offsetAt returns t's zone offset in seconds east of UTC.
func offsetAt(t time.Time) int {
	_, offset := t.Zone()
	return offset
}

// bisectOffsetChange returns the first instant in (lo, hi] whose offset differs
// from base. lo and hi must straddle exactly one transition.
func bisectOffsetChange(lo, hi time.Time, base int) time.Time {
	for hi.Sub(lo) > time.Nanosecond {
		mid := lo.Add(hi.Sub(lo) / 2)
		if offsetAt(mid) == base {
			lo = mid
		} else {
			hi = mid
		}
	}
	return hi
}

// zoneTransitions returns the instants in (lo, hi] at which loc changes offset.
// It walks fixed steps and refines each step exactly, so its cost is bounded by
// the span rather than by the location's history. A midpoint check catches the
// rare case of two transitions inside one step. complete is false when the
// bound maxTransitions is hit, so the caller can decline to answer from a span
// it could not enumerate.
func zoneTransitions(lo, hi time.Time, loc *time.Location) (transitions []time.Time, complete bool) {
	out := make([]time.Time, 0, 4)
	for a := lo; a.Before(hi); {
		if len(out) >= maxTransitions {
			return out, false
		}
		b := a.Add(boundaryStep)
		if b.After(hi) {
			b = hi
		}
		before, after := offsetAt(a), offsetAt(b)
		switch {
		case before != after:
			out = append(out, bisectOffsetChange(a, b, before))
		default:
			if mid := a.Add(b.Sub(a) / 2); offsetAt(mid) != before {
				first := bisectOffsetChange(a, mid, before)
				out = append(out, first)
				out = append(out, bisectOffsetChange(first, b, offsetAt(first)))
			}
		}
		a = b
	}
	return out, true
}

// localBoundary returns the earliest instant whose wall-clock reading in loc is
// at or after the start of the local calendar date y-m-d. That is the date's
// reporting boundary whether the nominal midnight exists once, twice, or not at
// all.
func localBoundary(y int, m time.Month, d int, loc *time.Location) time.Time {
	target := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	candidate := time.Date(y, m, d, 0, 0, 0, 0, loc)

	// Fast path: time.Date landed exactly on the first instant of the date, as
	// it does for every ordinary midnight and for the first of two repeats.
	if !wallClock(candidate, loc).Before(target) && wallClock(candidate.Add(-time.Nanosecond), loc).Before(target) {
		return candidate
	}
	for _, span := range boundarySpans {
		if boundary, ok := boundaryWithin(target, loc, candidate.Add(-span), candidate.Add(span)); ok {
			return boundary
		}
	}
	// No transition in a 40-day window changed which local date an instant
	// belongs to, so the nominal construction is the best answer available.
	return candidate
}

// boundaryWithin returns the earliest instant in [lo, hi] whose wall-clock
// reading in loc is at or after target. ok is false when the span cannot be
// enumerated or contains no such crossing.
func boundaryWithin(target time.Time, loc *time.Location, lo, hi time.Time) (time.Time, bool) {
	transitions, complete := zoneTransitions(lo, hi, loc)
	if !complete {
		return time.Time{}, false
	}

	var best time.Time
	found := false
	consider := func(t time.Time) {
		if !found || t.Before(best) {
			best, found = t, true
		}
	}

	// On an interval with a constant offset the crossing is exactly target
	// minus that offset, and it counts only if it falls inside the interval.
	points := make([]time.Time, 0, len(transitions)+1)
	points = append(points, lo)
	points = append(points, transitions...)
	for i, start := range points {
		end := hi
		if i+1 < len(points) {
			end = points[i+1]
		}
		crossing := target.Add(-time.Duration(offsetAt(start)) * time.Second)
		if !crossing.Before(start) && crossing.Before(end) {
			consider(crossing)
		}
	}
	// A forward jump can swallow midnight, in which case the first instant after
	// the gap is the boundary. This also covers a date skipped entirely.
	for _, t := range transitions {
		if offsetAt(t) > offsetAt(t.Add(-time.Nanosecond)) &&
			!wallClock(t, loc).Before(target) &&
			wallClock(t.Add(-time.Nanosecond), loc).Before(target) {
			consider(t)
		}
	}
	if !found {
		return time.Time{}, false
	}
	return best.In(loc), true
}
