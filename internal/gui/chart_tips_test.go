package gui

import (
	"image"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/reconcile"
)

// This file covers the chart-tip convergence: the data tips whose point
// follows chart.Select's choice, on the tray's hourly bars, the session
// timeline and the reconcile waterfall. The tests are about the contract
// the app needs — the arrows move the choice, the tip follows it, and every
// point describes itself for assistive technology — not about pixels. The
// heatmap is not here: its cells are a week-by-weekday grid, which
// chart.Select, one ascending list along x, cannot express.

// chartTipTester lays out one chart and settles it, so that the elements
// have bounds and the entrance animations have a second frame.
func chartTipTester(t *testing.T, w, h int, view func(*ui.Context)) *ui.Tester {
	t.Helper()
	tt := ui.NewTester(view, w, h)
	tt.SetScale(1)
	tt.SetPreferences(ui.Preferences{ReduceMotion: true})
	for i := 0; i < 4; i++ {
		tt.Frame()
	}
	return tt
}

// hourlyPoints is n consecutive hours with distinct token counts.
func hourlyPoints(n int) []query.Point {
	base := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	pts := make([]query.Point, n)
	for i := range pts {
		pts[i] = query.Point{
			Day:     base.Add(time.Duration(i) * time.Hour),
			Tokens:  model.Tokens{Input: int64(1000 * (i + 1)), Output: int64(100 * (i + 1))},
			CostUSD: 0.01 * float64(i+1),
		}
	}
	return pts
}

// daysOf builds n days with a local estimate, a charge, and a multiplier
// difference on every other day.
func daysOf(n int) []reconcile.DayLine {
	base := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	days := make([]reconcile.DayLine, n)
	for i := range days {
		days[i] = reconcile.DayLine{
			Day:        base.AddDate(0, 0, i),
			Count:      int64(i + 1),
			LocalUSD:   1 + 0.5*float64(i),
			ChargedUSD: 1.2 + 0.4*float64(i),
		}
		if i%2 == 0 {
			diff := 0.05 * float64(i+1)
			days[i].MultiplierDiffUSD = &diff
		}
	}
	return days
}

// eventsOf builds n requests a minute apart, with distinct token counts.
func eventsOf(n int) []query.AttributedEvent {
	base := time.Date(2026, 3, 2, 13, 0, 5, 0, time.UTC)
	evs := make([]query.AttributedEvent, n)
	for i := range evs {
		evs[i] = query.AttributedEvent{
			UsageEvent: model.UsageEvent{
				Timestamp: base.Add(time.Duration(i) * time.Minute),
				Model:     "claude-sonnet-4-5",
				Tokens:    model.Tokens{Input: int64(500 * (i + 1)), Output: int64(50 * (i + 1))},
			},
			ResolvedProvider: "anthropic",
			Cost:             0.02 * float64(i+1),
			Priced:           true,
		}
	}
	return evs
}

// tipVisible reports whether the collected tip title s is on screen.
func tipVisible(t *testing.T, tt *ui.Tester, s string) bool {
	t.Helper()
	_, ok := tt.Find(s)
	return ok
}

// The tray's hourly bars take the keyboard: the arrows move the choice and
// the tip follows it.
func TestMiniBarsArrowKeysMoveSelectionAndTip(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	pts := hourlyPoints(8)
	tt := chartTipTester(t, 320, 90, func(c *ui.Context) {
		pal := applyTheme(c)
		ui.Column(c).Padding(14).Children(func() {
			miniBars(c, pal, pts, 62)
		})
	})
	tt.Key(0, ui.KeyTab)
	if !tt.Focused(tr("last24h")) {
		t.Fatal("Tab does not focus the hourly chart")
	}
	// Right from no choice takes the first hour.
	tt.Key(0, ui.KeyRight)
	if !tipVisible(t, tt, pts[0].Day.Format("15:00")) {
		t.Fatal("Right did not show the tip of the first hour")
	}
	// And keeps taking the next.
	tt.Key(0, ui.KeyRight)
	tt.Key(0, ui.KeyRight)
	if !tipVisible(t, tt, pts[2].Day.Format("15:00")) {
		t.Fatalf("Right twice more did not show the tip of the third hour")
	}
	if tipVisible(t, tt, pts[0].Day.Format("15:00")) {
		t.Error("the first hour's tip stayed after the choice moved on")
	}
	// The pointer chose the hour it was over before, and still does.
	tt.Move(20+14, 40)
	if !tipVisible(t, tt, pts[0].Day.Format("15:00")) {
		t.Error("the pointer over the first hour does not show its tip")
	}
	tt.Move(280+14, 40)
	if !tipVisible(t, tt, pts[7].Day.Format("15:00")) {
		t.Error("the pointer over the last hour does not show its tip")
	}
	if tipVisible(t, tt, pts[0].Day.Format("15:00")) {
		t.Error("the pointer moved off the first hour but its tip stayed")
	}
	// Leaving the chart clears the choice, as it did.
	tt.Move(20+14, 200)
	if tipVisible(t, tt, pts[7].Day.Format("15:00")) {
		t.Error("the pointer left the chart but its tip stayed")
	}
}

// Every hour has its own description, which is what the arrows read out.
func TestMiniBarsDescribesEveryHour(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	pts := hourlyPoints(8)
	tt := chartTipTester(t, 320, 90, func(c *ui.Context) {
		pal := applyTheme(c)
		ui.Column(c).Padding(14).Children(func() {
			miniBars(c, pal, pts, 62)
		})
	})
	tt.Key(0, ui.KeyTab)
	if !tt.Focused(tr("last24h")) {
		t.Fatal("Tab does not focus the hourly chart")
	}
	tt.Announcements()
	for range pts {
		tt.Key(0, ui.KeyRight)
	}
	got := tt.Announcements()
	if len(got) != len(pts) {
		t.Fatalf("Right over %d hours announced %d of them: %q", len(pts), len(got), got)
	}
	seen := map[string]bool{}
	for i, s := range got {
		if strings.TrimSpace(s) == "" {
			t.Errorf("hour %d is described by nothing", i)
		}
		if seen[s] {
			t.Errorf("hour %d repeats the description %q", i, s)
		}
		seen[s] = true
		if want := pts[i].Day.Format("15:00"); !strings.Contains(s, want) {
			t.Errorf("hour %d is described as %q, which does not name %s", i, s, want)
		}
	}
}

// The reconcile waterfall takes the keyboard too, and its tip names the day
// it chose.
func TestDayPairsArrowKeysMoveSelectionAndTip(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	days := daysOf(6)
	tt := chartTipTester(t, 360, 120, func(c *ui.Context) {
		pal := applyTheme(c)
		ui.Column(c).Padding(14).Children(func() {
			dayPairs(c, pal, days, 90)
		})
	})
	tt.Key(0, ui.KeyTab)
	if !tt.Focused(tr("relayByDay")) {
		t.Fatal("Tab does not focus the by-day chart")
	}
	tt.Announcements()
	tt.Key(0, ui.KeyRight)
	if !tipVisible(t, tt, days[0].Day.Format("2006-01-02")) {
		t.Fatal("Right did not show the first day's tip")
	}
	tt.Key(0, ui.KeyRight)
	tt.Key(0, ui.KeyRight)
	if !tipVisible(t, tt, days[2].Day.Format("2006-01-02")) {
		t.Fatal("Right twice more did not show the third day's tip")
	}
	if tipVisible(t, tt, days[0].Day.Format("2006-01-02")) {
		t.Error("the first day's tip stayed after the choice moved on")
	}
	got := tt.Announcements()
	if len(got) != 3 {
		t.Fatalf("three rights announced %d times: %q", len(got), got)
	}
	distinct := map[string]bool{}
	for i, s := range got {
		if strings.TrimSpace(s) == "" {
			t.Errorf("day %d is described by nothing", i)
		}
		if distinct[s] {
			t.Errorf("day %d repeats the description %q", i, s)
		}
		distinct[s] = true
	}
	// The pointer still chooses the day it is over: six even slots.
	tt.Move(14+60, 60)
	if !tipVisible(t, tt, days[1].Day.Format("2006-01-02")) {
		t.Errorf("the pointer over the second slot does not show the second day's tip")
	}
}

// The reconcile waterfall has no tip: it dims every bar but the one the
// pointer is over. That highlight is a fill alpha, so the only way to pin the
// mapping is pixels. It also pins the coordinate space: PointerPosition is
// relative to the element's own box while the Draw rect is in window
// coordinates, so a hit test that subtracts the element's own X dims the
// wrong bar.
func TestWaterfallDimsOnlyTheBarUnderThePointer(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	rep := reconcile.Report{
		LocalUSD:   10,
		ChargedUSD: 12.5,
		// Three lines, each with a non-zero difference: a line whose charge
		// equals its local cost is dropped, and the bar count must be exact
		// for the slot maths below.
		Lines: []reconcile.Line{
			{Category: reconcile.PriceDiff, LocalUSD: 2, ChargedUSD: 3},
			{Category: reconcile.TokenSemantics, LocalUSD: 2, ChargedUSD: 2.5},
			{Category: reconcile.BillOnly, LocalUSD: 0, ChargedUSD: 1},
		},
	}
	const (
		pad    = 20  // the column's padding: the element starts at this X
		winW   = 460 // so the element is winW-2*pad wide
		height = 110 // the waterfall's own height
	)
	tt := chartTipTester(t, winW, 160, func(c *ui.Context) {
		pal := applyTheme(c)
		ui.Column(c).Padding(pad).Children(func() {
			waterfall(c, pal, rep, height)
		})
	})
	// The waterfall builds relayLocal, one bar per line, then relayCharged.
	const steps = 5
	plotX := float32(pad)
	plotW := float32(winW - 2*pad)
	slot := plotW / steps
	plotY, plotH := float32(pad+18), float32(height-30-18)
	// Band centres are inside the bars; the sampled column is 5px wide.
	bars := func(img *image.RGBA, base *image.RGBA) []int {
		changed := make([]int, steps)
		for i := range changed {
			cx := int(plotX + slot*float32(i) + slot/2)
			for x := cx - 2; x <= cx+2; x++ {
				for y := int(plotY); y < int(plotY+plotH); y++ {
					if img.RGBAAt(x, y) != base.RGBAAt(x, y) {
						changed[i]++
					}
				}
			}
		}
		return changed
	}
	tt.Move(2, 2) // outside the element: nothing is dimmed
	base := tt.Image()
	// Just inside the second slot's left edge. The element starts at x=pad,
	// so a hit test that subtracts pad lands in the previous slot.
	tt.Move(plotX+2*slot+1, float32(pad+height/2))
	tt.Frame()
	got := bars(tt.Image(), base)
	for i, n := range got {
		if i == 2 {
			if n != 0 {
				t.Errorf("the bar under the pointer changed %d pixels, want none", n)
			}
			continue
		}
		if n == 0 {
			t.Errorf("bar %d was not dimmed while the pointer was over bar 2", i)
		}
	}
}

// The session timeline takes the keyboard, and its tip names the request it
// chose.
func TestTimelineArrowKeysMoveSelectionAndTip(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	evs := eventsOf(5)
	tt := chartTipTester(t, 420, 140, func(c *ui.Context) {
		pal := applyTheme(c)
		ui.Column(c).Padding(14).Children(func() {
			timeline(c, pal, evs, []string{"claude-sonnet-4-5"}, 170)
		})
	})
	tt.Key(0, ui.KeyTab)
	if !tt.Focused(tr("timeline")) {
		t.Fatal("Tab does not focus the timeline")
	}
	tt.Announcements()
	tt.Key(0, ui.KeyRight)
	if !tipVisible(t, tt, evs[0].Timestamp.Local().Format("15:04:05")) {
		t.Fatal("Right did not show the first request's tip")
	}
	tt.Key(0, ui.KeyRight)
	tt.Key(0, ui.KeyRight)
	if !tipVisible(t, tt, evs[2].Timestamp.Local().Format("15:04:05")) {
		t.Fatalf("Right twice more did not show the third request's tip")
	}
	if tipVisible(t, tt, evs[0].Timestamp.Local().Format("15:04:05")) {
		t.Error("the first request's tip stayed after the choice moved on")
	}
	got := tt.Announcements()
	if len(got) != 3 {
		t.Fatalf("three rights announced %d times: %q", len(got), got)
	}
	distinct := map[string]bool{}
	for i, s := range got {
		if strings.TrimSpace(s) == "" {
			t.Errorf("request %d is described by nothing", i)
		}
		if distinct[s] {
			t.Errorf("request %d repeats the description %q", i, s)
		}
		distinct[s] = true
	}
}
