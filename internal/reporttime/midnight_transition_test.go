package reporttime_test

import (
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/reporttime"
)

// transitionInstant builds the instant at a UTC wall-clock reading and presents
// it in loc. A local midnight that occurs twice (a fall-back at 00:00) or is
// skipped (a spring-forward at 00:00) cannot be built with time.Date, so an
// expected boundary in those zones is expressed as an instant instead.
func transitionInstant(loc *time.Location, y int, mo time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, mo, d, h, mi, s, 0, time.UTC).In(loc)
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// periodCase is one adversarial resolution: the local calendar situation, the
// instant to resolve at, and the boundaries the contract requires.
type periodCase struct {
	name      string
	loc       *time.Location
	now       time.Time
	start     time.Time
	end       time.Time
	prevStart time.Time
	prevEnd   time.Time
}

// assertPeriodContract checks every clause of the period contract for one case:
// the period contains now, it is non-empty and its boundaries are the required
// instants, elapsed never goes negative or past the period, the previous period
// is a full positive period ending exactly at the current start, and the
// comparison is the elapsed duration capped at the previous period's end.
func assertPeriodContract(t *testing.T, st reporttime.State, tc periodCase) {
	t.Helper()
	if !st.Current.Window().Contains(st.Now) {
		t.Errorf("Current = [%s, %s) does not contain now %s",
			st.Current.Start.Format(time.RFC3339), st.Current.End.Format(time.RFC3339), st.Now.Format(time.RFC3339))
	}
	if st.Current.Window().IsEmpty() {
		t.Errorf("Current = [%s, %s) is empty", st.Current.Start.Format(time.RFC3339), st.Current.End.Format(time.RFC3339))
	}
	assertTime(t, "Current.Start", st.Current.Start, tc.start)
	assertTime(t, "Current.End", st.Current.End, tc.end)

	if st.Elapsed.Duration() < 0 {
		t.Errorf("Elapsed = %s, want no negative elapsed", st.Elapsed.Duration())
	}
	if st.Elapsed.Duration() > st.Current.Window().Duration() {
		t.Errorf("Elapsed = %s exceeds the period %s", st.Elapsed.Duration(), st.Current.Window().Duration())
	}
	assertTime(t, "Elapsed.End", st.Elapsed.End, st.Now)

	assertTime(t, "Previous.End (contiguous with Current.Start)", st.Previous.End, tc.start)
	assertTime(t, "Previous.End", st.Previous.End, tc.prevEnd)
	assertTime(t, "Previous.Start", st.Previous.Start, tc.prevStart)
	if st.Previous.Window().Duration() <= 0 {
		t.Errorf("Previous = [%s, %s) is not a full period",
			st.Previous.Start.Format(time.RFC3339), st.Previous.End.Format(time.RFC3339))
	}

	assertTime(t, "Comparison.Start", st.Comparison.Start, st.Previous.Start)
	if st.Comparison.End.After(st.Previous.End) {
		t.Errorf("Comparison ends at %s, past Previous.End %s",
			st.Comparison.End.Format(time.RFC3339), st.Previous.End.Format(time.RFC3339))
	}
	if st.Comparison.Start.After(st.Comparison.End) {
		t.Errorf("Comparison = [%s, %s) is inverted",
			st.Comparison.Start.Format(time.RFC3339), st.Comparison.End.Format(time.RFC3339))
	}
	assertDuration(t, "Comparison",
		st.Comparison.Duration(),
		minDuration(st.Elapsed.Duration(), st.Previous.Window().Duration()))

	if got := st.Now.Location().String(); got != tc.loc.String() {
		t.Errorf("Now location = %q, want %q", got, tc.loc)
	}
}

// TestDayBoundariesAcrossMidnightTransitions covers day periods whose local
// midnight does not exist (spring-forward at 00:00), occurs twice (fall-back at
// 00:00) or does not exist at all because the whole date was skipped. In every
// case the boundary must be the earliest instant whose local calendar date is
// the selected one, so the period contains now and boundaries stay aligned to
// the local calendar instead of drifting by the transition offset.
func TestDayBoundariesAcrossMidnightTransitions(t *testing.T) {
	sp := loadLoc(t, "America/Sao_Paulo")
	hav := loadLoc(t, "America/Havana")
	apia := loadLoc(t, "Pacific/Apia")
	gaza := loadLoc(t, "Asia/Gaza")
	amman := loadLoc(t, "Asia/Amman")
	beirut := loadLoc(t, "Asia/Beirut")
	scl := loadLoc(t, "America/Santiago")
	cairo := loadLoc(t, "Africa/Cairo")
	asu := loadLoc(t, "America/Asuncion")

	cases := []periodCase{
		{
			// Brazil 2018: clocks jump from 2018-11-04 00:00 -03 to 01:00 -02,
			// so the date begins at the transition instant.
			name:      "sao paulo 2018-11-04 midnight does not exist",
			loc:       sp,
			now:       time.Date(2018, 11, 4, 10, 0, 0, 0, sp),
			start:     time.Date(2018, 11, 4, 1, 0, 0, 0, sp),
			end:       time.Date(2018, 11, 5, 0, 0, 0, 0, sp),
			prevStart: time.Date(2018, 11, 3, 0, 0, 0, 0, sp),
			prevEnd:   time.Date(2018, 11, 4, 1, 0, 0, 0, sp),
		},
		{
			// The last hour before that skip still belongs to 2018-11-03, and
			// the day ends at the transition instant rather than at 23:00.
			name:      "sao paulo 2018-11-03 evening before the skipped midnight",
			loc:       sp,
			now:       time.Date(2018, 11, 3, 23, 30, 0, 0, sp),
			start:     time.Date(2018, 11, 3, 0, 0, 0, 0, sp),
			end:       time.Date(2018, 11, 4, 1, 0, 0, 0, sp),
			prevStart: time.Date(2018, 11, 2, 0, 0, 0, 0, sp),
			prevEnd:   time.Date(2018, 11, 3, 0, 0, 0, 0, sp),
		},
		{
			name:      "sao paulo 2011-10-16 midnight does not exist",
			loc:       sp,
			now:       time.Date(2011, 10, 16, 12, 0, 0, 0, sp),
			start:     time.Date(2011, 10, 16, 1, 0, 0, 0, sp),
			end:       time.Date(2011, 10, 17, 0, 0, 0, 0, sp),
			prevStart: time.Date(2011, 10, 15, 0, 0, 0, 0, sp),
			prevEnd:   time.Date(2011, 10, 16, 1, 0, 0, 0, sp),
		},
		{
			// Cuba 2018: 2018-03-11 00:00 -05 jumps to 01:00 -04.
			name:      "havana 2018-03-11 midnight does not exist",
			loc:       hav,
			now:       time.Date(2018, 3, 11, 12, 0, 0, 0, hav),
			start:     time.Date(2018, 3, 11, 1, 0, 0, 0, hav),
			end:       time.Date(2018, 3, 12, 0, 0, 0, 0, hav),
			prevStart: time.Date(2018, 3, 10, 0, 0, 0, 0, hav),
			prevEnd:   time.Date(2018, 3, 11, 1, 0, 0, 0, hav),
		},
		{
			name:      "havana 2018-03-10 evening before the skipped midnight",
			loc:       hav,
			now:       time.Date(2018, 3, 10, 23, 30, 0, 0, hav),
			start:     time.Date(2018, 3, 10, 0, 0, 0, 0, hav),
			end:       time.Date(2018, 3, 11, 1, 0, 0, 0, hav),
			prevStart: time.Date(2018, 3, 9, 0, 0, 0, 0, hav),
			prevEnd:   time.Date(2018, 3, 10, 0, 0, 0, 0, hav),
		},
		{
			// Cuba 2018: 01:00 -04 falls back to 00:00 -05, so local midnight
			// happens twice. The period starts at the first occurrence and is a
			// 25-hour day.
			name:      "havana 2018-11-04 repeated midnight first occurrence",
			loc:       hav,
			now:       transitionInstant(hav, 2018, 11, 4, 4, 30, 0), // 00:30 -04
			start:     time.Date(2018, 11, 4, 0, 0, 0, 0, hav),
			end:       time.Date(2018, 11, 5, 0, 0, 0, 0, hav),
			prevStart: time.Date(2018, 11, 3, 0, 0, 0, 0, hav),
			prevEnd:   time.Date(2018, 11, 4, 0, 0, 0, 0, hav),
		},
		{
			name:      "havana 2018-11-04 repeated midnight second occurrence",
			loc:       hav,
			now:       transitionInstant(hav, 2018, 11, 4, 5, 30, 0), // 00:30 -05
			start:     time.Date(2018, 11, 4, 0, 0, 0, 0, hav),
			end:       time.Date(2018, 11, 5, 0, 0, 0, 0, hav),
			prevStart: time.Date(2018, 11, 3, 0, 0, 0, 0, hav),
			prevEnd:   time.Date(2018, 11, 4, 0, 0, 0, 0, hav),
		},
		{
			// Samoa skipped 2011-12-30 entirely when it crossed the date line:
			// 2011-12-29 24:00 -10 became 2011-12-31 00:00 +14. The day of
			// 2011-12-29 therefore runs to that transition instant.
			name:      "apia 2011-12-29 before the skipped date",
			loc:       apia,
			now:       time.Date(2011, 12, 29, 23, 30, 0, 0, apia),
			start:     time.Date(2011, 12, 29, 0, 0, 0, 0, apia),
			end:       transitionInstant(apia, 2011, 12, 30, 10, 0, 0), // 2011-12-31 00:00 +14
			prevStart: time.Date(2011, 12, 28, 0, 0, 0, 0, apia),
			prevEnd:   time.Date(2011, 12, 29, 0, 0, 0, 0, apia),
		},
		{
			name:      "apia 2011-12-31 after the skipped date",
			loc:       apia,
			now:       time.Date(2011, 12, 31, 12, 0, 0, 0, apia),
			start:     transitionInstant(apia, 2011, 12, 30, 10, 0, 0),
			end:       time.Date(2012, 1, 1, 0, 0, 0, 0, apia),
			prevStart: time.Date(2011, 12, 29, 0, 0, 0, 0, apia),
			// The nonexistent 2011-12-30 is skipped: the previous full period
			// is 2011-12-29, still ending exactly where 2011-12-31 begins.
			prevEnd: transitionInstant(apia, 2011, 12, 30, 10, 0, 0),
		},
		{
			name:      "apia exactly at the date-line transition",
			loc:       apia,
			now:       transitionInstant(apia, 2011, 12, 30, 10, 0, 0),
			start:     transitionInstant(apia, 2011, 12, 30, 10, 0, 0),
			end:       time.Date(2012, 1, 1, 0, 0, 0, 0, apia),
			prevStart: time.Date(2011, 12, 29, 0, 0, 0, 0, apia),
			prevEnd:   transitionInstant(apia, 2011, 12, 30, 10, 0, 0),
		},
		{
			// Gaza 2018: 01:00 +03 falls back to 00:00 +02, so midnight occurs
			// twice. time.Date picks the later occurrence; the period must still
			// start at the earlier one, otherwise now is before its own period
			// and elapsed goes negative.
			name:      "gaza 2018-10-27 repeated midnight first occurrence",
			loc:       gaza,
			now:       transitionInstant(gaza, 2018, 10, 26, 21, 30, 0), // 00:30 +03
			start:     transitionInstant(gaza, 2018, 10, 26, 21, 0, 0),  // 00:00 +03
			end:       time.Date(2018, 10, 28, 0, 0, 0, 0, gaza),
			prevStart: time.Date(2018, 10, 26, 0, 0, 0, 0, gaza),
			prevEnd:   transitionInstant(gaza, 2018, 10, 26, 21, 0, 0),
		},
		{
			name:      "gaza 2018-10-27 repeated midnight second occurrence",
			loc:       gaza,
			now:       transitionInstant(gaza, 2018, 10, 26, 22, 30, 0), // 00:30 +02
			start:     transitionInstant(gaza, 2018, 10, 26, 21, 0, 0),
			end:       time.Date(2018, 10, 28, 0, 0, 0, 0, gaza),
			prevStart: time.Date(2018, 10, 26, 0, 0, 0, 0, gaza),
			prevEnd:   transitionInstant(gaza, 2018, 10, 26, 21, 0, 0),
		},
		{
			name:      "amman 2018-10-26 repeated midnight",
			loc:       amman,
			now:       transitionInstant(amman, 2018, 10, 25, 21, 30, 0), // 00:30 +03
			start:     transitionInstant(amman, 2018, 10, 25, 21, 0, 0),  // 00:00 +03
			end:       time.Date(2018, 10, 27, 0, 0, 0, 0, amman),
			prevStart: time.Date(2018, 10, 25, 0, 0, 0, 0, amman),
			prevEnd:   transitionInstant(amman, 2018, 10, 25, 21, 0, 0),
		},
		{
			// Beirut 2018: 00:00 +02 jumps to 01:00 +03. time.Date normalizes
			// the start forward correctly, but Step 1 of AddDate used to move the
			// end to the next day's 01:00 as well.
			name:      "beirut 2018-03-25 midnight does not exist",
			loc:       beirut,
			now:       time.Date(2018, 3, 25, 12, 0, 0, 0, beirut),
			start:     time.Date(2018, 3, 25, 1, 0, 0, 0, beirut),
			end:       time.Date(2018, 3, 26, 0, 0, 0, 0, beirut),
			prevStart: time.Date(2018, 3, 24, 0, 0, 0, 0, beirut),
			prevEnd:   time.Date(2018, 3, 25, 1, 0, 0, 0, beirut),
		},
		{
			name:      "santiago 2018-08-12 midnight does not exist",
			loc:       scl,
			now:       time.Date(2018, 8, 12, 12, 0, 0, 0, scl),
			start:     time.Date(2018, 8, 12, 1, 0, 0, 0, scl),
			end:       time.Date(2018, 8, 13, 0, 0, 0, 0, scl),
			prevStart: time.Date(2018, 8, 11, 0, 0, 0, 0, scl),
			prevEnd:   time.Date(2018, 8, 12, 1, 0, 0, 0, scl),
		},
		{
			name:      "cairo 2023-04-28 midnight does not exist",
			loc:       cairo,
			now:       time.Date(2023, 4, 28, 12, 0, 0, 0, cairo),
			start:     time.Date(2023, 4, 28, 1, 0, 0, 0, cairo),
			end:       time.Date(2023, 4, 29, 0, 0, 0, 0, cairo),
			prevStart: time.Date(2023, 4, 27, 0, 0, 0, 0, cairo),
			prevEnd:   time.Date(2023, 4, 28, 1, 0, 0, 0, cairo),
		},
		{
			// Paraguay 2023: 2023-10-01 00:00 -04 jumps to 01:00 -03.
			name:      "asuncion 2023-10-01 midnight does not exist",
			loc:       asu,
			now:       time.Date(2023, 10, 1, 12, 0, 0, 0, asu),
			start:     time.Date(2023, 10, 1, 1, 0, 0, 0, asu),
			end:       time.Date(2023, 10, 2, 0, 0, 0, 0, asu),
			prevStart: time.Date(2023, 9, 30, 0, 0, 0, 0, asu),
			prevEnd:   time.Date(2023, 10, 1, 1, 0, 0, 0, asu),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := mustResolve(t, tc.now, reporttime.Day, tc.loc)
			assertPeriodContract(t, st, tc)
		})
	}

	// The Havana repeated-midnight day is 25 hours, not a nominal 24.
	fall := mustResolve(t, time.Date(2018, 11, 4, 12, 0, 0, 0, hav), reporttime.Day, hav)
	assertDuration(t, "havana 25-hour day", fall.Current.Window().Duration(), 25*time.Hour)
}

// TestWeekBoundariesAcrossMidnightTransitions covers weeks whose Monday
// midnight does not exist, plus a week containing a skipped date. Week steps are
// calendar steps on the Monday date, so the boundaries stay local midnights (or
// the transition instant that replaces a missing one).
func TestWeekBoundariesAcrossMidnightTransitions(t *testing.T) {
	sp := loadLoc(t, "America/Sao_Paulo")
	tehran := loadLoc(t, "Asia/Tehran")
	apia := loadLoc(t, "Pacific/Apia")

	cases := []periodCase{
		{
			// The Sunday 2018-11-04 transition sits inside this week; both
			// boundaries are ordinary midnights and the week is 167 hours.
			name:      "sao paulo week containing the skipped midnight",
			loc:       sp,
			now:       time.Date(2018, 11, 4, 12, 0, 0, 0, sp),
			start:     time.Date(2018, 10, 29, 0, 0, 0, 0, sp),
			end:       time.Date(2018, 11, 5, 0, 0, 0, 0, sp),
			prevStart: time.Date(2018, 10, 22, 0, 0, 0, 0, sp),
			prevEnd:   time.Date(2018, 10, 29, 0, 0, 0, 0, sp),
		},
		{
			// Iran 2016: the Monday that starts this week has no local midnight,
			// so the week begins at 2016-03-21 01:00 +0430.
			name:      "tehran week whose monday midnight does not exist",
			loc:       tehran,
			now:       time.Date(2016, 3, 23, 12, 0, 0, 0, tehran),
			start:     time.Date(2016, 3, 21, 1, 0, 0, 0, tehran),
			end:       time.Date(2016, 3, 28, 0, 0, 0, 0, tehran),
			prevStart: time.Date(2016, 3, 14, 0, 0, 0, 0, tehran),
			prevEnd:   time.Date(2016, 3, 21, 1, 0, 0, 0, tehran),
		},
		{
			name:      "apia week containing the skipped date",
			loc:       apia,
			now:       time.Date(2011, 12, 29, 12, 0, 0, 0, apia),
			start:     time.Date(2011, 12, 26, 0, 0, 0, 0, apia),
			end:       time.Date(2012, 1, 2, 0, 0, 0, 0, apia),
			prevStart: time.Date(2011, 12, 19, 0, 0, 0, 0, apia),
			prevEnd:   time.Date(2011, 12, 26, 0, 0, 0, 0, apia),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := mustResolve(t, tc.now, reporttime.Week, tc.loc)
			assertPeriodContract(t, st, tc)
			if got := st.Current.Start.Weekday(); got != time.Monday {
				t.Errorf("week start weekday = %s, want Monday", got)
			}
		})
	}

	// The week containing the Sao Paulo spring-forward is 167 hours.
	week := mustResolve(t, time.Date(2018, 11, 4, 12, 0, 0, 0, sp), reporttime.Week, sp)
	assertDuration(t, "sao paulo 167-hour week", week.Current.Window().Duration(), 167*time.Hour)
}

// TestMonthBoundariesAcrossMidnightTransitions covers months whose first
// midnight does not exist or occurs twice, and a month containing a skipped
// date. Month steps are calendar steps on the first-of-month date.
func TestMonthBoundariesAcrossMidnightTransitions(t *testing.T) {
	asu := loadLoc(t, "America/Asuncion")
	gaza := loadLoc(t, "Asia/Gaza")
	amman := loadLoc(t, "Asia/Amman")
	apia := loadLoc(t, "Pacific/Apia")

	cases := []periodCase{
		{
			// 2023-10-01 00:00 -04 does not exist; the month starts at 01:00.
			// A late instant in the month used to fall outside the month
			// entirely because the end was also shifted an hour.
			name:      "asuncion 2023-10 first midnight does not exist",
			loc:       asu,
			now:       time.Date(2023, 10, 31, 12, 0, 0, 0, asu),
			start:     time.Date(2023, 10, 1, 1, 0, 0, 0, asu),
			end:       time.Date(2023, 11, 1, 0, 0, 0, 0, asu),
			prevStart: time.Date(2023, 9, 1, 0, 0, 0, 0, asu),
			prevEnd:   time.Date(2023, 10, 1, 1, 0, 0, 0, asu),
		},
		{
			// Gaza 2004: 2004-10-01 01:00 +03 falls back to 00:00 +02, so the
			// first midnight occurs twice. The month starts at the first one.
			name:      "gaza 2004-10 first midnight occurs twice",
			loc:       gaza,
			now:       transitionInstant(gaza, 2004, 9, 30, 21, 30, 0), // 2004-10-01 00:30 +03
			start:     transitionInstant(gaza, 2004, 9, 30, 21, 0, 0),  // 2004-10-01 00:00 +03
			end:       time.Date(2004, 11, 1, 0, 0, 0, 0, gaza),
			prevStart: time.Date(2004, 9, 1, 0, 0, 0, 0, gaza),
			prevEnd:   transitionInstant(gaza, 2004, 9, 30, 21, 0, 0),
		},
		{
			name:      "amman 2016-04 first midnight does not exist",
			loc:       amman,
			now:       time.Date(2016, 4, 15, 12, 0, 0, 0, amman),
			start:     time.Date(2016, 4, 1, 1, 0, 0, 0, amman),
			end:       time.Date(2016, 5, 1, 0, 0, 0, 0, amman),
			prevStart: time.Date(2016, 3, 1, 0, 0, 0, 0, amman),
			prevEnd:   time.Date(2016, 4, 1, 1, 0, 0, 0, amman),
		},
		{
			// December 2011 in Samoa contains only 30 existing dates because
			// 2011-12-30 never happened; the month still spans the real month.
			name:      "apia 2011-12 month containing the skipped date",
			loc:       apia,
			now:       time.Date(2011, 12, 29, 12, 0, 0, 0, apia),
			start:     time.Date(2011, 12, 1, 0, 0, 0, 0, apia),
			end:       time.Date(2012, 1, 1, 0, 0, 0, 0, apia),
			prevStart: time.Date(2011, 11, 1, 0, 0, 0, 0, apia),
			prevEnd:   time.Date(2011, 12, 1, 0, 0, 0, 0, apia),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := mustResolve(t, tc.now, reporttime.Month, tc.loc)
			assertPeriodContract(t, st, tc)
		})
	}
}

// TestPeriodContractAcrossTransitionWindows samples every 15 minutes across a
// 72-hour window around each adversarial transition and re-checks the whole
// contract at every sample, including that consecutive windows tile without a
// gap or an overlap.
func TestPeriodContractAcrossTransitionWindows(t *testing.T) {
	sp := loadLoc(t, "America/Sao_Paulo")
	hav := loadLoc(t, "America/Havana")
	apia := loadLoc(t, "Pacific/Apia")
	gaza := loadLoc(t, "Asia/Gaza")
	tehran := loadLoc(t, "Asia/Tehran")
	asu := loadLoc(t, "America/Asuncion")

	centers := []struct {
		name   string
		loc    *time.Location
		center time.Time
	}{
		{"sao paulo spring forward", sp, time.Date(2018, 11, 4, 12, 0, 0, 0, sp)},
		{"havana spring forward", hav, time.Date(2018, 3, 11, 12, 0, 0, 0, hav)},
		{"havana fall back", hav, time.Date(2018, 11, 4, 12, 0, 0, 0, hav)},
		{"apia skipped date", apia, transitionInstant(apia, 2011, 12, 30, 10, 0, 0)},
		{"gaza repeated midnight", gaza, time.Date(2018, 10, 27, 12, 0, 0, 0, gaza)},
		{"tehran monday gap", tehran, time.Date(2016, 3, 21, 12, 0, 0, 0, tehran)},
		{"asuncion first-of-month gap", asu, time.Date(2023, 10, 1, 12, 0, 0, 0, asu)},
	}

	kinds := []reporttime.Kind{reporttime.Day, reporttime.Week, reporttime.Month}
	const step = 15 * time.Minute
	const span = 36 * time.Hour

	for _, c := range centers {
		for _, kind := range kinds {
			t.Run(c.name+"/"+string(kind), func(t *testing.T) {
				var prev reporttime.Period
				first := true
				for now := c.center.Add(-span); !now.After(c.center.Add(span)); now = now.Add(step) {
					st := mustResolve(t, now, kind, c.loc)
					cur := st.Current
					if !cur.Window().Contains(st.Now) {
						t.Fatalf("at %s: period [%s, %s) does not contain now %s",
							now.Format(time.RFC3339), cur.Start.Format(time.RFC3339), cur.End.Format(time.RFC3339), st.Now.Format(time.RFC3339))
					}
					if cur.Window().IsEmpty() {
						t.Fatalf("at %s: period is empty", now.Format(time.RFC3339))
					}
					if d := st.Elapsed.Duration(); d < 0 {
						t.Fatalf("at %s: negative elapsed %s", now.Format(time.RFC3339), d)
					} else if d > cur.Window().Duration() {
						t.Fatalf("at %s: elapsed %s exceeds the period %s", now.Format(time.RFC3339), d, cur.Window().Duration())
					}
					if !st.Elapsed.End.Equal(st.Now) {
						t.Fatalf("at %s: Elapsed.End = %s, want now", now.Format(time.RFC3339), st.Elapsed.End.Format(time.RFC3339))
					}
					if !st.Previous.End.Equal(cur.Start) {
						t.Fatalf("at %s: previous period ends at %s, current starts at %s",
							now.Format(time.RFC3339), st.Previous.End.Format(time.RFC3339), cur.Start.Format(time.RFC3339))
					}
					if st.Previous.Window().Duration() <= 0 {
						t.Fatalf("at %s: previous period is not positive", now.Format(time.RFC3339))
					}
					if !st.Comparison.Start.Equal(st.Previous.Start) || st.Comparison.End.After(st.Previous.End) {
						t.Fatalf("at %s: comparison [%s, %s) escapes previous [%s, %s)",
							now.Format(time.RFC3339),
							st.Comparison.Start.Format(time.RFC3339), st.Comparison.End.Format(time.RFC3339),
							st.Previous.Start.Format(time.RFC3339), st.Previous.End.Format(time.RFC3339))
					}
					if got := st.Comparison.Duration(); got != minDuration(st.Elapsed.Duration(), st.Previous.Window().Duration()) {
						t.Fatalf("at %s: comparison = %s, want the capped elapsed duration", now.Format(time.RFC3339), got)
					}
					if !first && !samePeriod(prev, cur) {
						// The sample is 15 minutes after the previous one; the
						// only way the period can differ is a real boundary, and
						// then the new period's previous must be the old period.
						if !samePeriod(st.Previous, prev) {
							t.Fatalf("at %s: period changed to [%s, %s) but its previous is [%s, %s), not the preceding period [%s, %s)",
								now.Format(time.RFC3339),
								cur.Start.Format(time.RFC3339), cur.End.Format(time.RFC3339),
								st.Previous.Start.Format(time.RFC3339), st.Previous.End.Format(time.RFC3339),
								prev.Start.Format(time.RFC3339), prev.End.Format(time.RFC3339))
						}
					}
					prev, first = cur, false
				}
			})
		}
	}
}

func samePeriod(a, b reporttime.Period) bool {
	return a.Kind == b.Kind && a.Start.Equal(b.Start) && a.End.Equal(b.End)
}

// TestPeriodIDExactInstants pins the identity contract: the ID is built from the
// actual start instant, so it differs across zones, kinds and starts even when
// the nominal local calendar date is the same, and two periods whose starts
// differ by a single nanosecond are still distinct.
func TestPeriodIDExactInstants(t *testing.T) {
	sp := loadLoc(t, "America/Sao_Paulo")
	utc := time.UTC

	gap := mustResolve(t, time.Date(2018, 11, 4, 12, 0, 0, 0, sp), reporttime.Day, sp)
	start := time.Date(2018, 11, 4, 1, 0, 0, 0, sp)
	if !gap.Current.Start.Equal(start) {
		t.Fatalf("Current.Start = %s, want the transition instant %s",
			gap.Current.Start.Format(time.RFC3339), start.Format(time.RFC3339))
	}
	if id := gap.Current.ID(); !strings.Contains(id, start.Format(time.RFC3339)) {
		t.Errorf("period ID %q does not encode the actual start instant %s", id, start.Format(time.RFC3339))
	}

	// The same nominal calendar date with a different actual start is a
	// different period.
	utcDay := mustResolve(t, time.Date(2018, 11, 4, 12, 0, 0, 0, utc), reporttime.Day, utc)
	if utcDay.Current.ID() == gap.Current.ID() {
		t.Errorf("period ID %q collides across zones", gap.Current.ID())
	}

	// Adjacent periods differ, even though Previous.End equals Current.Start.
	if gap.Current.Previous().ID() == gap.Current.ID() {
		t.Errorf("adjacent periods share ID %q", gap.Current.ID())
	}

	// The identity is exact at instant granularity, not only at whole seconds.
	a := reporttime.Period{Kind: reporttime.Day, Start: start, End: start.Add(24 * time.Hour)}
	b := reporttime.Period{Kind: reporttime.Day, Start: start.Add(time.Nanosecond), End: start.Add(24 * time.Hour)}
	if a.ID() == b.ID() {
		t.Errorf("period ID %q collides for starts one nanosecond apart", a.ID())
	}
}
