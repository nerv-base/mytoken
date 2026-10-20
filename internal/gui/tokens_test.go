package gui

import (
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

// TestTokenCardRanges checks the token card offers the four ranges and that
// choosing All reads the whole history, which the overview itself does not
// carry.
func TestTokenCardRanges(t *testing.T) {
	h := newHarness(t)
	tt := ui.NewTester(h.app.MainView, 1280, 1640)
	settle(t, h, tt)
	for _, label := range []string{tr("range7"), tr("range30"), tr("range90"), tr("rangeAll")} {
		if !tt.HasText(label) {
			t.Fatalf("token card has no %q tab", label)
		}
	}
	if h.app.tokAll.ready {
		t.Fatal("all-time series read before its tab was chosen")
	}
	if err := tt.Click(tr("rangeAll")); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	h.drain(t)
	tt.Frame()
	if h.app.tokRange != len(rangeDays)-1 {
		t.Fatalf("chose %q, tokRange is %d", tr("rangeAll"), h.app.tokRange)
	}
	if !h.app.tokAll.ready || len(h.app.tokAll.days) <= 90 {
		t.Fatalf("all-time series not read: ready=%v days=%d", h.app.tokAll.ready, len(h.app.tokAll.days))
	}
	if !tt.HasText(tr("tokAllSum")) {
		t.Fatalf("all-time sum is not labelled %q", tr("tokAllSum"))
	}
	// The bars are folded once with the read: no history read next frame, and
	// no frame folds it into the card's range buffer.
	var sum int64
	for _, d := range h.app.tokAll.days {
		sum += d.tokens.Total()
	}
	if got := h.app.tokAll.series.sum; got != sum {
		t.Fatalf("folded sum %d, want %d", got, sum)
	}
	if len(h.app.tokAll.series.bars) == 0 {
		t.Fatal("all-time series has no bars")
	}
	// The card draws the bars folded with the read: given no days of its own,
	// all time still has its sum and bars.
	if got := h.app.tokSeries(nil); got.sum != sum || len(got.bars) != len(h.app.tokAll.series.bars) {
		t.Fatalf("all time redrew %d bars, sum %d", len(got.bars), got.sum)
	}
	if err := tt.Click(tr("range7")); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	h.drain(t)
	tt.Frame()
	if h.app.tokRange != 0 {
		t.Fatalf("chose %q, tokRange is %d", tr("range7"), h.app.tokRange)
	}
	if !tt.HasText(trf("tokRangeSum", 7)) {
		t.Fatalf("7-day sum is not labelled %q", trf("tokRangeSum", 7))
	}
	// A range folds its days into the reused buffer, one bar a day.
	if len(h.app.tokScratch) != 7 {
		t.Fatalf("7-day range folded %d bars", len(h.app.tokScratch))
	}
	days := h.app.ov.days[len(h.app.ov.days)-7:]
	if got := h.app.tokSeries(days); len(got.bars) != 7 || &got.bars[0] != &h.app.tokScratch[0] {
		t.Fatalf("a range folded %d bars into a fresh slice", len(got.bars))
	}
}

// TestTokenBars checks that a long series folds into fewer, wider bars
// without losing a token, and that the newest bar ends on the newest day.
func TestTokenBars(t *testing.T) {
	day := time.Date(2026, 10, 19, 0, 0, 0, 0, time.Local)
	mk := func(n int) []dayCost {
		out := make([]dayCost, n)
		for i := range out {
			out[i] = dayCost{day: day.AddDate(0, 0, -(n - 1 - i))}
			out[i].tokens.Input = int64(i + 1)
		}
		return out
	}
	cases := []struct {
		days, bars int
	}{
		{0, 0}, {1, 1}, {7, 7}, {90, 90}, {180, 180}, {181, 26}, {400, 58}, {1095, 157},
	}
	for _, c := range cases {
		days := mk(c.days)
		bars := tokenBars(days)
		if len(bars) != c.bars {
			t.Errorf("%d days: %d bars, want %d", c.days, len(bars), c.bars)
		}
		var got, want int64
		for _, b := range bars {
			got += b.tokens.Total()
		}
		for _, d := range days {
			want += d.tokens.Total()
		}
		if got != want {
			t.Errorf("%d days: bars total %d, want %d", c.days, got, want)
		}
		if len(bars) > 0 && !bars[len(bars)-1].end.Equal(day) {
			t.Errorf("%d days: last bar ends %v, want %v", c.days, bars[len(bars)-1].end, day)
		}
	}
}

// TestTokenBarLabels checks bars say what they cover: the day, the week's
// span, or the month's name, and spell the year out only when a series crosses
// one.
func TestTokenBarLabels(t *testing.T) {
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.Local) }
	one := tokenBar{start: day(2026, 3, 5), end: day(2026, 3, 5), unit: dayBar}
	week := tokenBar{start: day(2026, 3, 5), end: day(2026, 3, 11), unit: weekBar}
	month := tokenBar{start: day(2026, 3, 5), end: day(2026, 3, 28), unit: monthBar}

	if got := tokenBarLabel(one, false); strings.Contains(got, "2026") {
		t.Errorf("single-year day label %q spells the year", got)
	}
	if got := tokenBarLabel(one, true); !strings.Contains(got, "2026") {
		t.Errorf("cross-year day label %q hides the year", got)
	}
	if got := tokenBarLabel(week, false); !strings.Contains(got, " – ") {
		t.Errorf("week label %q is not a span", got)
	}
	if got := tokenBarLabel(week, true); !strings.Contains(got, "2026") {
		t.Errorf("cross-year week label %q hides the year", got)
	}
	// A month bar is named by its month, not by the days it happens to cover.
	if got := tokenBarLabel(month, false); got != fmtMonthYear(month.start) {
		t.Errorf("month label %q, want %q", got, fmtMonthYear(month.start))
	}
	if got := tokenBarTick(one, true); !strings.Contains(got, "2026") {
		t.Errorf("cross-year tick %q hides the year", got)
	}

	if !spansYears([]tokenBar{{start: day(2025, 12, 1), end: day(2026, 1, 1), unit: monthBar}}) {
		t.Error("a series from December to January does not span years")
	}
	if spansYears([]tokenBar{one, week}) {
		t.Error("a series within one year spans years")
	}
	if spansYears(nil) {
		t.Error("an empty series spans years")
	}
}

// TestTokenAllFollowsData checks the all-time series follows new data only
// while its card is on screen: no other page draws it, and the whole history
// is not cheap to read.
func TestTokenAllFollowsData(t *testing.T) {
	h := newHarness(t)
	tt := ui.NewTester(h.app.MainView, 1280, 1640)
	settle(t, h, tt)
	if err := tt.Click(tr("rangeAll")); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	h.drain(t)
	tt.Frame()
	if !h.app.tokAll.ready {
		t.Fatal("all-time series not read")
	}

	// err marks whether a read landed, since a good read clears it.
	h.app.goTo("ranking")
	h.drain(t)
	h.app.tokAll.err = "stale"
	h.app.dataChanged()
	h.drain(t)
	if h.app.tokAll.err != "stale" {
		t.Fatal("all-time series was read while its card was off screen")
	}

	h.app.goTo("overview")
	h.drain(t)
	h.app.dataChanged()
	h.drain(t)
	if h.app.tokAll.err != "" {
		t.Fatal("all-time series did not follow new data")
	}
}
