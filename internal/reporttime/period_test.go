package reporttime_test

import (
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // pin the IANA database so America/New_York DST rules are stable

	"github.com/zzstar101/mytoken/internal/reporttime"
)

func loadLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("time.LoadLocation(%q): %v", name, err)
	}
	return loc
}

func mustResolve(t *testing.T, now time.Time, kind reporttime.Kind, loc *time.Location) reporttime.State {
	t.Helper()
	st, err := reporttime.Resolve(now, kind, loc)
	if err != nil {
		t.Fatalf("Resolve(%s, %s, %v): %v", now.Format(time.RFC3339), kind, loc, err)
	}
	return st
}

func assertTime(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	if !got.Equal(want) {
		t.Errorf("%s = %s, want %s", what, got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if got.Location().String() != want.Location().String() {
		t.Errorf("%s location = %q, want %q", what, got.Location(), want.Location())
	}
}

func assertDuration(t *testing.T, what string, got, want time.Duration) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %s, want %s", what, got, want)
	}
}

func TestResolveDayBoundaries(t *testing.T) {
	utc := time.UTC
	ny := loadLoc(t, "America/New_York")
	cases := []struct {
		name       string
		now        time.Time
		loc        *time.Location
		start, end time.Time
	}{
		{
			name:  "midday",
			now:   time.Date(2026, 6, 15, 13, 45, 30, 123, utc),
			loc:   utc,
			start: time.Date(2026, 6, 15, 0, 0, 0, 0, utc),
			end:   time.Date(2026, 6, 16, 0, 0, 0, 0, utc),
		},
		{
			name:  "exact midnight",
			now:   time.Date(2026, 6, 15, 0, 0, 0, 0, utc),
			loc:   utc,
			start: time.Date(2026, 6, 15, 0, 0, 0, 0, utc),
			end:   time.Date(2026, 6, 16, 0, 0, 0, 0, utc),
		},
		{
			name:  "year rollover",
			now:   time.Date(2026, 12, 31, 23, 59, 59, 0, utc),
			loc:   utc,
			start: time.Date(2026, 12, 31, 0, 0, 0, 0, utc),
			end:   time.Date(2027, 1, 1, 0, 0, 0, 0, utc),
		},
		{
			name:  "leap february 29",
			now:   time.Date(2024, 2, 29, 8, 0, 0, 0, utc),
			loc:   utc,
			start: time.Date(2024, 2, 29, 0, 0, 0, 0, utc),
			end:   time.Date(2024, 3, 1, 0, 0, 0, 0, utc),
		},
		{
			name:  "non-leap february 28",
			now:   time.Date(2025, 2, 28, 8, 0, 0, 0, utc),
			loc:   utc,
			start: time.Date(2025, 2, 28, 0, 0, 0, 0, utc),
			end:   time.Date(2025, 3, 1, 0, 0, 0, 0, utc),
		},
		{
			name:  "new york spring forward day",
			now:   time.Date(2026, 3, 8, 23, 30, 0, 0, ny),
			loc:   ny,
			start: time.Date(2026, 3, 8, 0, 0, 0, 0, ny),
			end:   time.Date(2026, 3, 9, 0, 0, 0, 0, ny),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := mustResolve(t, tc.now, reporttime.Day, tc.loc)
			if st.Kind != reporttime.Day {
				t.Errorf("State.Kind = %q, want %q", st.Kind, reporttime.Day)
			}
			if st.Current.Kind != reporttime.Day {
				t.Errorf("Current.Kind = %q, want %q", st.Current.Kind, reporttime.Day)
			}
			assertTime(t, "Current.Start", st.Current.Start, tc.start)
			assertTime(t, "Current.End", st.Current.End, tc.end)
			assertTime(t, "Now", st.Now, tc.now.In(tc.loc))
			assertTime(t, "Elapsed.Start", st.Elapsed.Start, tc.start)
			assertTime(t, "Elapsed.End", st.Elapsed.End, st.Now)
		})
	}
}

func TestWeekStartsMonday(t *testing.T) {
	ny := loadLoc(t, "America/New_York")
	cases := []struct {
		name       string
		now        time.Time
		start, end time.Time
	}{
		{
			name:  "wednesday",
			now:   time.Date(2026, 10, 14, 15, 0, 0, 0, ny),
			start: time.Date(2026, 10, 12, 0, 0, 0, 0, ny),
			end:   time.Date(2026, 10, 19, 0, 0, 0, 0, ny),
		},
		{
			name:  "sunday belongs to the week that started monday",
			now:   time.Date(2026, 10, 18, 23, 59, 59, 0, ny),
			start: time.Date(2026, 10, 12, 0, 0, 0, 0, ny),
			end:   time.Date(2026, 10, 19, 0, 0, 0, 0, ny),
		},
		{
			name:  "monday midnight is its own week start",
			now:   time.Date(2026, 10, 12, 0, 0, 0, 0, ny),
			start: time.Date(2026, 10, 12, 0, 0, 0, 0, ny),
			end:   time.Date(2026, 10, 19, 0, 0, 0, 0, ny),
		},
		{
			name:  "sunday across year rollover",
			now:   time.Date(2027, 1, 3, 10, 0, 0, 0, ny),
			start: time.Date(2026, 12, 28, 0, 0, 0, 0, ny),
			end:   time.Date(2027, 1, 4, 0, 0, 0, 0, ny),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := mustResolve(t, tc.now, reporttime.Week, ny)
			assertTime(t, "Current.Start", st.Current.Start, tc.start)
			assertTime(t, "Current.End", st.Current.End, tc.end)
			if got := st.Current.Start.Weekday(); got != time.Monday {
				t.Errorf("week start weekday = %s, want Monday", got)
			}
		})
	}
}

func TestMonthBoundaries(t *testing.T) {
	ny := loadLoc(t, "America/New_York")
	cases := []struct {
		name       string
		now        time.Time
		start, end time.Time
	}{
		{
			name:  "mid month",
			now:   time.Date(2026, 10, 14, 15, 0, 0, 0, ny),
			start: time.Date(2026, 10, 1, 0, 0, 0, 0, ny),
			end:   time.Date(2026, 11, 1, 0, 0, 0, 0, ny),
		},
		{
			name:  "first of month",
			now:   time.Date(2026, 10, 1, 0, 0, 0, 0, ny),
			start: time.Date(2026, 10, 1, 0, 0, 0, 0, ny),
			end:   time.Date(2026, 11, 1, 0, 0, 0, 0, ny),
		},
		{
			name:  "december into january",
			now:   time.Date(2026, 12, 31, 23, 0, 0, 0, ny),
			start: time.Date(2026, 12, 1, 0, 0, 0, 0, ny),
			end:   time.Date(2027, 1, 1, 0, 0, 0, 0, ny),
		},
		{
			name:  "leap february",
			now:   time.Date(2024, 2, 29, 12, 0, 0, 0, ny),
			start: time.Date(2024, 2, 1, 0, 0, 0, 0, ny),
			end:   time.Date(2024, 3, 1, 0, 0, 0, 0, ny),
		},
		{
			name:  "non-leap february",
			now:   time.Date(2025, 2, 28, 12, 0, 0, 0, ny),
			start: time.Date(2025, 2, 1, 0, 0, 0, 0, ny),
			end:   time.Date(2025, 3, 1, 0, 0, 0, 0, ny),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := mustResolve(t, tc.now, reporttime.Month, ny)
			assertTime(t, "Current.Start", st.Current.Start, tc.start)
			assertTime(t, "Current.End", st.Current.End, tc.end)
		})
	}
}

func TestDSTDurationsAreNotFixed24h(t *testing.T) {
	ny := loadLoc(t, "America/New_York")

	// 2026-03-08 02:00 EST jumps to 03:00 EDT: a 23-hour local day.
	spring := mustResolve(t, time.Date(2026, 3, 8, 23, 30, 0, 0, ny), reporttime.Day, ny)
	assertDuration(t, "spring-forward day", spring.Current.Window().Duration(), 23*time.Hour)
	assertDuration(t, "spring-forward elapsed", spring.Elapsed.Duration(), 22*time.Hour+30*time.Minute)
	if _, off := spring.Current.Start.Zone(); off != -5*3600 {
		t.Errorf("spring day start offset = %d, want -18000 (EST)", off)
	}
	if _, off := spring.Current.End.Zone(); off != -4*3600 {
		t.Errorf("spring day end offset = %d, want -14400 (EDT)", off)
	}
	// Previous day is a normal 24-hour day, so the comparison window is not
	// truncated by the shorter current day.
	assertDuration(t, "comparison before a 23h day", spring.Comparison.Duration(), 22*time.Hour+30*time.Minute)
	assertTime(t, "comparison start", spring.Comparison.Start, time.Date(2026, 3, 7, 0, 0, 0, 0, ny))
	assertTime(t, "comparison end", spring.Comparison.End, time.Date(2026, 3, 7, 22, 30, 0, 0, ny))

	// 2026-11-01 02:00 EDT falls back to 01:00 EST: a 25-hour local day.
	fall := mustResolve(t, time.Date(2026, 11, 1, 12, 0, 0, 0, ny), reporttime.Day, ny)
	assertDuration(t, "fall-back day", fall.Current.Window().Duration(), 25*time.Hour)
	assertDuration(t, "fall-back elapsed", fall.Elapsed.Duration(), 13*time.Hour)

	// The week containing the spring-forward day is 167 hours, but its
	// boundaries are still local midnights.
	week := mustResolve(t, time.Date(2026, 3, 4, 9, 0, 0, 0, ny), reporttime.Week, ny)
	assertTime(t, "dst week start", week.Current.Start, time.Date(2026, 3, 2, 0, 0, 0, 0, ny))
	assertTime(t, "dst week end", week.Current.End, time.Date(2026, 3, 9, 0, 0, 0, 0, ny))
	assertDuration(t, "dst week", week.Current.Window().Duration(), 167*time.Hour)
}

func TestElapsedAndComparison(t *testing.T) {
	ny := loadLoc(t, "America/New_York")

	// At an exact boundary nothing of the period has elapsed yet; both the
	// elapsed window and the comparison window are empty.
	midnight := mustResolve(t, time.Date(2026, 6, 15, 0, 0, 0, 0, ny), reporttime.Day, ny)
	assertDuration(t, "midnight elapsed", midnight.Elapsed.Duration(), 0)
	if !midnight.Elapsed.IsEmpty() {
		t.Error("Elapsed at exact midnight is not empty")
	}
	assertTime(t, "midnight elapsed start", midnight.Elapsed.Start, midnight.Current.Start)
	assertTime(t, "midnight elapsed end", midnight.Elapsed.End, midnight.Now)
	assertTime(t, "midnight comparison start", midnight.Comparison.Start, midnight.Previous.Start)
	assertTime(t, "midnight comparison end", midnight.Comparison.End, midnight.Previous.Start)
	assertDuration(t, "midnight comparison", midnight.Comparison.Duration(), 0)

	// Previous month (April, 30 days) is longer than the elapsed part of May.
	partial := mustResolve(t, time.Date(2026, 5, 10, 6, 0, 0, 0, ny), reporttime.Month, ny)
	assertTime(t, "partial previous start", partial.Previous.Start, time.Date(2026, 4, 1, 0, 0, 0, 0, ny))
	assertTime(t, "partial previous end", partial.Previous.End, time.Date(2026, 5, 1, 0, 0, 0, 0, ny))
	assertTime(t, "partial comparison start", partial.Comparison.Start, partial.Previous.Start)
	assertTime(t, "partial comparison end", partial.Comparison.End, time.Date(2026, 4, 10, 6, 0, 0, 0, ny))
	assertDuration(t, "partial comparison == elapsed", partial.Comparison.Duration(), partial.Elapsed.Duration())

	// Previous month (February, 28 days) is shorter than the elapsed part of
	// March, so the comparison is capped at the previous period's end.
	clamped := mustResolve(t, time.Date(2026, 3, 31, 12, 0, 0, 0, ny), reporttime.Month, ny)
	assertTime(t, "clamped previous start", clamped.Previous.Start, time.Date(2026, 2, 1, 0, 0, 0, 0, ny))
	assertTime(t, "clamped previous end", clamped.Previous.End, time.Date(2026, 3, 1, 0, 0, 0, 0, ny))
	assertTime(t, "clamped comparison start", clamped.Comparison.Start, clamped.Previous.Start)
	assertTime(t, "clamped comparison end", clamped.Comparison.End, clamped.Previous.End)
	assertDuration(t, "clamped comparison is the whole previous month", clamped.Comparison.Duration(), 28*24*time.Hour)
	if clamped.Elapsed.Duration() <= clamped.Previous.Window().Duration() {
		t.Fatalf("test setup: elapsed %s should exceed the previous month %s",
			clamped.Elapsed.Duration(), clamped.Previous.Window().Duration())
	}
}

func TestPreviousPeriod(t *testing.T) {
	utc := time.UTC
	ny := loadLoc(t, "America/New_York")

	day := mustResolve(t, time.Date(2026, 1, 1, 10, 0, 0, 0, utc), reporttime.Day, utc)
	assertTime(t, "previous day start", day.Previous.Start, time.Date(2025, 12, 31, 0, 0, 0, 0, utc))
	assertTime(t, "previous day end", day.Previous.End, time.Date(2026, 1, 1, 0, 0, 0, 0, utc))

	// 2026-01-01 is a Thursday; its week starts Monday 2025-12-29.
	week := mustResolve(t, time.Date(2026, 1, 1, 10, 0, 0, 0, utc), reporttime.Week, utc)
	assertTime(t, "current week start", week.Current.Start, time.Date(2025, 12, 29, 0, 0, 0, 0, utc))
	assertTime(t, "previous week start", week.Previous.Start, time.Date(2025, 12, 22, 0, 0, 0, 0, utc))
	assertTime(t, "previous week end", week.Previous.End, time.Date(2025, 12, 29, 0, 0, 0, 0, utc))

	month := mustResolve(t, time.Date(2026, 1, 15, 10, 0, 0, 0, utc), reporttime.Month, utc)
	assertTime(t, "previous month start", month.Previous.Start, time.Date(2025, 12, 1, 0, 0, 0, 0, utc))
	assertTime(t, "previous month end", month.Previous.End, time.Date(2026, 1, 1, 0, 0, 0, 0, utc))

	// March clamps back to a 28-day February and keeps its zone.
	march := mustResolve(t, time.Date(2026, 3, 15, 10, 0, 0, 0, ny), reporttime.Month, ny)
	assertDuration(t, "previous february", march.Previous.Window().Duration(), 28*24*time.Hour)
	assertTime(t, "previous february start", march.Previous.Start, time.Date(2026, 2, 1, 0, 0, 0, 0, ny))
	assertTime(t, "previous february end", march.Previous.End, time.Date(2026, 3, 1, 0, 0, 0, 0, ny))
}

func TestEquivalentInstantInDifferentInputLocation(t *testing.T) {
	ny := loadLoc(t, "America/New_York")
	tokyo := loadLoc(t, "Asia/Tokyo")

	now := time.Date(2026, 3, 8, 23, 30, 0, 0, ny)
	want := mustResolve(t, now, reporttime.Day, ny)
	got := mustResolve(t, now.In(tokyo), reporttime.Day, ny)

	assertTime(t, "Current.Start", got.Current.Start, want.Current.Start)
	assertTime(t, "Current.End", got.Current.End, want.Current.End)
	assertTime(t, "Elapsed.Start", got.Elapsed.Start, want.Elapsed.Start)
	assertTime(t, "Elapsed.End", got.Elapsed.End, want.Elapsed.End)
	assertTime(t, "Comparison.Start", got.Comparison.Start, want.Comparison.Start)
	assertTime(t, "Comparison.End", got.Comparison.End, want.Comparison.End)
	if got.Current.ID() != want.Current.ID() {
		t.Errorf("period ID = %q, want %q", got.Current.ID(), want.Current.ID())
	}
	if got.Now.Location().String() != "America/New_York" {
		t.Errorf("Now location = %q, want America/New_York", got.Now.Location())
	}

	// The same wall clock in a different zone is a different period.
	utc := time.UTC
	other := mustResolve(t, time.Date(2026, 3, 8, 23, 30, 0, 0, utc), reporttime.Day, utc)
	if other.Current.ID() == want.Current.ID() {
		t.Errorf("period ID %q collides across zones", other.Current.ID())
	}
}

func TestPeriodIDIdentity(t *testing.T) {
	ny := loadLoc(t, "America/New_York")
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, ny)

	nine := mustResolve(t, now, reporttime.Day, ny)
	// Same instant delivered in another input location: same identity.
	utcInput := mustResolve(t, now.In(time.UTC), reporttime.Day, ny)
	if nine.Current.ID() != utcInput.Current.ID() {
		t.Errorf("same period has different IDs: %q vs %q", nine.Current.ID(), utcInput.Current.ID())
	}
	// Moving within the day keeps the identity.
	late := mustResolve(t, time.Date(2026, 10, 14, 23, 0, 0, 0, ny), reporttime.Day, ny)
	if nine.Current.ID() != late.Current.ID() {
		t.Errorf("same day has different IDs: %q vs %q", nine.Current.ID(), late.Current.ID())
	}
	// Kind and zone are part of the identity.
	week := mustResolve(t, now, reporttime.Week, ny)
	if week.Current.ID() == nine.Current.ID() {
		t.Error("day and week share a period ID")
	}
	utc := time.UTC
	utcDay := mustResolve(t, time.Date(2026, 10, 14, 9, 0, 0, 0, utc), reporttime.Day, utc)
	if utcDay.Current.ID() == nine.Current.ID() {
		t.Error("period ID collides across zones")
	}

	id := nine.Current.ID()
	for _, want := range []string{"day", "America/New_York", "2026-10-14"} {
		if !strings.Contains(id, want) {
			t.Errorf("period ID %q does not contain %q", id, want)
		}
	}
}

func TestRejectsNilLocationAndInvalidKind(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	if _, err := reporttime.Resolve(now, reporttime.Day, nil); err == nil {
		t.Error("Resolve(nil location): want error, got nil")
	}
	for _, kind := range []reporttime.Kind{"", "quarter", "d", "days", "DAY", "Month"} {
		if kind.Valid() {
			t.Errorf("Kind(%q).Valid() = true, want false", kind)
		}
		if _, err := reporttime.Resolve(now, kind, time.UTC); err == nil {
			t.Errorf("Resolve(kind=%q): want error, got nil", kind)
		}
	}
	for _, kind := range []reporttime.Kind{reporttime.Day, reporttime.Week, reporttime.Month} {
		if !kind.Valid() {
			t.Errorf("Kind(%q).Valid() = false, want true", kind)
		}
	}
}

func TestParseKind(t *testing.T) {
	cases := map[string]reporttime.Kind{
		"day":    reporttime.Day,
		" Week ": reporttime.Week,
		"MONTH":  reporttime.Month,
	}
	for in, want := range cases {
		got, err := reporttime.ParseKind(in)
		if err != nil {
			t.Errorf("ParseKind(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseKind(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"", "quarter", "d", "days", "mo", "week!"} {
		if got, err := reporttime.ParseKind(in); err == nil {
			t.Errorf("ParseKind(%q) = %q, want error", in, got)
		}
	}
}

func TestWindowIncludesStartExcludesEnd(t *testing.T) {
	utc := time.UTC
	st := mustResolve(t, time.Date(2026, 6, 15, 12, 0, 0, 0, utc), reporttime.Day, utc)
	w := st.Current.Window()
	if !w.Contains(st.Current.Start) {
		t.Error("window does not contain its start")
	}
	if w.Contains(st.Current.End) {
		t.Error("window contains its exclusive end")
	}
	if !w.Contains(st.Current.End.Add(-time.Nanosecond)) {
		t.Error("window does not contain the instant before its end")
	}
	if w.Contains(st.Current.Start.Add(-time.Nanosecond)) {
		t.Error("window contains the instant before its start")
	}
}

func TestResolveDoesNotMutateGlobalLocal(t *testing.T) {
	prev := time.Local
	marker := time.FixedZone("ReportTimeMarker", 3*3600)
	time.Local = marker
	defer func() { time.Local = prev }()

	ny := loadLoc(t, "America/New_York")
	now := time.Date(2026, 3, 8, 23, 30, 0, 0, ny)
	if _, err := reporttime.Resolve(now, reporttime.Day, ny); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if time.Local != marker {
		t.Fatalf("time.Local = %v, want the untouched marker zone", time.Local)
	}
	// A nil location must never fall back to time.Local.
	if _, err := reporttime.Resolve(now, reporttime.Day, nil); err == nil {
		t.Fatal("Resolve(nil location) fell back instead of failing")
	}
	if time.Local != marker {
		t.Fatalf("time.Local = %v after rejected call, want the untouched marker zone", time.Local)
	}
}
