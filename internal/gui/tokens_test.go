package gui

import (
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
	if h.app.tokRange != len(tokDays)-1 {
		t.Fatalf("chose %q, tokRange is %d", tr("rangeAll"), h.app.tokRange)
	}
	if !h.app.tokAll.ready || len(h.app.tokAll.days) <= 90 {
		t.Fatalf("all-time series not read: ready=%v days=%d", h.app.tokAll.ready, len(h.app.tokAll.days))
	}
	if !tt.HasText(tr("tokAllSum")) {
		t.Fatalf("all-time sum is not labelled %q", tr("tokAllSum"))
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
