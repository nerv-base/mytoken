package gui

import (
	"context"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/query"
)

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("zone data for %s is unavailable: %v", name, err)
	}
	return loc
}

// The day a span starts on is the calendar day of the stats timezone, not the
// machine's: the same instant falls on a different local date in each zone.
func TestRangeOfResolvesDayInStatsZone(t *testing.T) {
	shanghai := mustZone(t, "Asia/Shanghai")
	// 2026-05-20 15:00 UTC is already 2026-05-20 23:00 in Shanghai, so the
	// Shanghai day is [05-19 16:00 UTC, 05-20 16:00 UTC).
	now := time.Date(2026, 5, 20, 15, 0, 0, 0, time.UTC)

	r := rangeOf(SpanToday, now, shanghai)
	if want := time.Date(2026, 5, 19, 16, 0, 0, 0, time.UTC); !r.From.Equal(want) {
		t.Fatalf("today from = %s, want %s (midnight in %s)", r.From, want, shanghai)
	}
	if want := time.Date(2026, 5, 20, 16, 0, 0, 0, time.UTC); !r.To.Equal(want) {
		t.Fatalf("today to = %s, want %s (next midnight in %s)", r.To, want, shanghai)
	}

	// A span of n days still covers exactly n calendar days of that zone.
	for _, tc := range []struct {
		span Span
		days int
	}{
		{Span7, 7}, {Span30, 30}, {Span90, 90},
	} {
		got := rangeOf(tc.span, now, shanghai)
		if d := got.To.Sub(got.From); d != time.Duration(tc.days)*24*time.Hour {
			t.Fatalf("span %v covers %s, want %d*24h", tc.span, d, tc.days)
		}
		if want := time.Date(2026, 5, 20-tc.days+1, 0, 0, 0, 0, shanghai); !got.From.Equal(want) {
			t.Fatalf("span %v from = %s, want %s", tc.span, got.From, want)
		}
	}

	// All keeps its "no lower bound" contract.
	if all := rangeOf(SpanAll, now, shanghai); !all.From.IsZero() || !all.To.IsZero() {
		t.Fatalf("all = %+v, want the zero range", all)
	}
}

// A day across a DST transition is 23 or 25 hours long, and the span boundaries
// follow it rather than adding a fixed 24h to midnight.
func TestRangeOfHandlesDSTBoundaries(t *testing.T) {
	ny := mustZone(t, "America/New_York")
	// 2026-03-08 is the US spring-forward day: midnight EST, then 23 hours.
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, ny)

	r := rangeOf(SpanToday, now, ny)
	if want := time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC); !r.From.Equal(want) {
		t.Fatalf("dst day from = %s, want %s", r.From, want)
	}
	if want := time.Date(2026, 3, 9, 4, 0, 0, 0, time.UTC); !r.To.Equal(want) {
		t.Fatalf("dst day to = %s, want %s", r.To, want)
	}
	if d := r.To.Sub(r.From); d != 23*time.Hour {
		t.Fatalf("dst day lasts %s, want 23h", d)
	}

	// The day before the transition is a plain 24 hours.
	before := rangeOf(SpanToday, now.AddDate(0, 0, -1), ny)
	if d := before.To.Sub(before.From); d != 24*time.Hour {
		t.Fatalf("day before dst lasts %s, want 24h", d)
	}

	// Seven days that contain the transition are one hour short.
	week := rangeOf(Span7, now, ny)
	if want := time.Date(2026, 3, 2, 5, 0, 0, 0, time.UTC); !week.From.Equal(want) {
		t.Fatalf("dst week from = %s, want %s", week.From, want)
	}
	if d := week.To.Sub(week.From); d != 7*24*time.Hour-time.Hour {
		t.Fatalf("dst week lasts %s, want 167h", d)
	}

	// 2026-03-08 is a Sunday: the week starts on the Monday before it.
	if got, want := weekStart(now, ny), time.Date(2026, 3, 2, 5, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("weekStart = %s, want %s", got, want)
	}
	// The heat window starts 52 weeks before it, in the same zone.
	if got, want := heatStart(now, ny), time.Date(2025, 3, 3, 5, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("heatStart = %s, want %s", got, want)
	}
}

// A caller with no location (a demo, a settings-less app) keeps the machine
// zone the overview used before the setting existed.
func TestRangeOfNilLocationIsMachineZone(t *testing.T) {
	now := time.Date(2026, 5, 20, 15, 0, 0, 0, time.UTC)
	if statsLoc(nil) != time.Local {
		t.Fatal("a nil location must mean the machine zone")
	}
	if got, want := rangeOf(Span7, now, nil), rangeOf(Span7, now, time.Local); got != want {
		t.Fatalf("nil location range = %+v, want the machine-zone range %+v", got, want)
	}
	if got, want := heatStart(now, nil), heatStart(now, time.Local); !got.Equal(want) {
		t.Fatalf("nil location heatStart = %s, want %s", got, want)
	}
}

// The state reads the zone through its hook on every load, so a nil hook or a
// nil result degrades to the machine zone instead of yielding a nil location.
func TestStateLocationFallsBackToMachineZone(t *testing.T) {
	utc := time.UTC
	for _, tc := range []struct {
		name string
		hook func() *time.Location
		want *time.Location
	}{
		{"no hook", nil, time.Local},
		{"nil result", func() *time.Location { return nil }, time.Local},
		{"zone", func() *time.Location { return utc }, utc},
	} {
		s := NewState(nil, Hooks{Location: tc.hook}, nil)
		if got := s.location(); got != tc.want {
			t.Fatalf("%s: location = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// rangeRecorder remembers the ranges the dashboard's loads ask for, so a test
// can see which calendar day it was just reloaded for.
type rangeRecorder struct {
	query.Service
	totals []query.Range
	daily  []query.Range
}

func (r *rangeRecorder) Totals(ctx context.Context, f query.Filter) (query.Totals, error) {
	r.totals = append(r.totals, f.Range)
	return r.Service.Totals(ctx, f)
}

func (r *rangeRecorder) Daily(ctx context.Context, f query.Filter) ([]query.Point, error) {
	r.daily = append(r.daily, f.Range)
	return r.Service.Daily(ctx, f)
}

// Saving a stats timezone reloads every calendar view, not only the budget card:
// the dashboard's day and the heatmap's week move to the new zone at once.
func TestTimezoneChangeReloadsDashboardInNewZone(t *testing.T) {
	now := time.Date(2026, 5, 20, 15, 0, 0, 0, time.UTC)
	rec := &rangeRecorder{Service: NewDemoService(now)}
	fc := &fakeCalendar{zone: "UTC"}
	s := NewState(rec, Hooks{
		Settings: NewDemoSettings(),
		Calendar: fc,
		Location: func() *time.Location {
			if loc, err := time.LoadLocation(fc.zone); err == nil {
				return loc
			}
			return time.UTC
		},
		Now: func() time.Time { return now },
	}, nil)
	s.span = int(SpanToday)
	s.Start()

	gen := s.gen
	s.bg.zoneIn = "Asia/Shanghai"
	s.budgetSetZone()

	if fc.zone != "Asia/Shanghai" {
		t.Fatalf("calendar zone = %q, want the saved zone", fc.zone)
	}
	if s.gen <= gen {
		t.Fatal("saving a timezone must reload the dashboard and the tray")
	}
	if len(rec.totals) == 0 || len(rec.daily) == 0 {
		t.Fatalf("dashboard did not reload (totals=%d daily=%d)", len(rec.totals), len(rec.daily))
	}
	wantDay := time.Date(2026, 5, 19, 16, 0, 0, 0, time.UTC) // midnight in Shanghai
	// The loads also ask for the previous day (the comparison), so look for
	// the range that starts on the new zone's current midnight.
	found := false
	for _, got := range rec.totals {
		if got.From.Equal(wantDay) {
			found = true
		}
	}
	if !found {
		t.Fatalf("dashboard ranges after the zone change = %+v, want one starting %s", rec.totals, wantDay)
	}
	heat := rec.daily[len(rec.daily)-1]
	if heat.From.IsZero() || heat.To.IsZero() {
		t.Fatalf("heatmap range = %+v, want the 52-week window in the new zone", heat)
	}
	if want := time.Date(2026, 5, 20, 16, 0, 0, 0, time.UTC); !heat.To.Equal(want) {
		t.Fatalf("heatmap end = %s, want the new zone's next midnight %s", heat.To, want)
	}
	if heat.From.Location().String() != "Asia/Shanghai" {
		t.Fatalf("heatmap start zone = %s, want Asia/Shanghai", heat.From.Location())
	}
}
