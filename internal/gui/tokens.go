package gui

import (
	"context"
	"slices"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

// tokDays are the token card's range choices, in days; 0 is all time.
var tokDays = [4]int{7, 30, 90, 0}

// tokenAll is the token card's all-time series. The overview carries only
// the last 90 days, so the whole history is read when All is chosen.
type tokenAll struct {
	ready bool
	err   string
	days  []dayCost
}

// tokenBar is one bar of the token card: a day, a week or a month,
// depending on how long the chosen range is.
type tokenBar struct {
	start, end time.Time
	tokens     model.Tokens
}

// tokenUsageCard counts every token, cache reads included: today, this
// month and the chosen range's sum and daily average, over bars split by
// kind.
func (a *App) tokenUsageCard(c *ui.Context, pal palette) {
	ov := &a.ov
	el := a.rcard(c, pal, "token-usage", 2, false)
	rv := a.mo.cur
	el.Children(func() {
		cardTitle(c, pal, tr("tokenUsage"), func() {
			if a.pills(c, pal, "tok-range", &a.tokRange, tr("range7"), tr("range30"), tr("range90"), tr("rangeAll")) && tokDays[a.tokRange] == 0 {
				a.loadTokenAll()
			}
		})
		n := tokDays[a.tokRange]
		days := ov.days
		if n == 0 {
			days = a.tokAll.days
		} else if len(days) > n {
			days = days[len(days)-n:]
		}
		if n == 0 && a.tokAll.err != "" {
			errorLine(c, pal, a.tokAll.err)
		}
		count := func(t model.Tokens) int64 { return t.Total() }
		var sum int64
		active := 0
		for _, d := range days {
			v := count(d.tokens)
			sum += v
			if v > 0 {
				active++
			}
		}
		avg := int64(0)
		if active > 0 {
			avg = sum / int64(active)
		}
		rangeLabel := trf("tokRangeSum", n)
		if n == 0 {
			rangeLabel = tr("tokAllSum")
		}
		tokFmt := func(v float64) string { return fmtTokens(int64(v)) }
		// All time is read on demand: until it lands, its two figures say so
		// instead of claiming zero.
		sumFmt := tokFmt
		if n == 0 && !a.tokAll.ready && a.tokAll.err == "" {
			sumFmt = func(float64) string { return "…" }
		}
		ui.Row(c).Gap(28).Children(func() {
			a.statNum(c, pal, "tok-today", tr("today"), float64(count(ov.today.Tokens)), tokFmt)
			a.statNum(c, pal, "tok-month", tr("thisMonth"), float64(count(ov.month.Tokens)), tokFmt)
			a.statNum(c, pal, "tok-range-sum", rangeLabel, float64(sum), sumFmt)
			a.statNum(c, pal, "tok-avg", tr("tokAvg"), float64(avg), sumFmt)
		})

		// Kinds bottom to top, cache reads (usually most of it) on top.
		type kind struct {
			key string
			col ui.Color
			of  func(model.Tokens) int64
		}
		kinds := []kind{
			{"input", pal.accent, func(t model.Tokens) int64 { return t.Input }},
			{"output", pal.accent2, func(t model.Tokens) int64 { return t.Output + t.Reasoning }},
			{"cacheWrite", ui.Hex("#D9A54C"), func(t model.Tokens) int64 { return t.CacheWrite }},
			{"cacheRead", pal.accent.Mix(pal.card, 0.6), func(t model.Tokens) int64 { return t.CacheRead }},
		}
		bars := tokenBars(days)
		peak := int64(0)
		for _, b := range bars {
			peak = max(peak, count(b.tokens))
		}
		top, step := niceTop(float64(peak))

		box := ui.Box(c.Key("tok-bars-" + itoa(n))).FillWidth().Height(170)
		target := float32(0)
		if rv.seen {
			target = 1
		}
		grow := box.AnimateTo("g", target, delayed(ui.Animation{Duration: 900 * time.Millisecond, Ease: easeOut}, rv.delay+150*time.Millisecond))
		scale := box.AnimateTo("top", float32(top), gentle)
		px, _, over := box.PointerPosition()
		line, faint := pal.line, pal.faint
		box.Draw(func(p *ui.Painter, r ui.Rect) {
			ch := r.H - 18
			left := float32(44)
			plot := ui.Rect{X: r.X + left, Y: r.Y, W: r.W - left, H: ch}
			// grid
			for v := 0.0; v <= top+step/2; v += step {
				y := plot.Y + plot.H*float32(1-v/float64(scale))
				if y < plot.Y-1 {
					break
				}
				p.Fill(ui.Rect{X: plot.X, Y: y, W: plot.W, H: 1}, line.Alpha(0.6), 0)
				l := fmtTokens(int64(v))
				w, _ := p.MeasureText(0, ui.Span{Text: l, Size: 10})
				p.Text(plot.X-8-w, y-6, l, 10, faint)
			}
			if len(bars) == 0 {
				return
			}
			slot := plot.W / float32(len(bars))
			bw := max(min(slot*0.62, 22), 2)
			hover := -1
			if over && px >= left {
				hover = min(int((px-left)/slot), len(bars)-1)
			}
			every := max(len(bars)/6, 1)
			for i, b := range bars {
				x := plot.X + slot*float32(i) + (slot-bw)/2
				y := plot.Y + plot.H
				alpha := float32(1)
				if hover >= 0 && hover != i {
					alpha = 0.45
				}
				for _, k := range kinds {
					v := float32(k.of(b.tokens)) / scale * plot.H * grow
					if v <= 0 {
						continue
					}
					y -= v
					p.Fill(ui.Rect{X: x, Y: y, W: bw, H: v}, k.col.Alpha(alpha), min(bw/2, 3))
				}
				if (len(bars)-1-i)%every == 0 {
					l := fmtDate(b.start)
					w, _ := p.MeasureText(0, ui.Span{Text: l, Size: 10})
					p.Text(min(max(x+bw/2-w/2, plot.X), plot.X+plot.W-w), plot.Y+plot.H+5, l, 10, faint)
				}
			}
			if hover >= 0 {
				b := bars[hover]
				lines := []string{tokenBarLabel(b) + " · " + fmtTokens(count(b.tokens))}
				for i := len(kinds) - 1; i >= 0; i-- {
					lines = append(lines, tr(kinds[i].key)+"  "+fmtTokens(kinds[i].of(b.tokens)))
				}
				x := plot.X + slot*float32(hover) + slot/2
				tooltip(p, pal, x, r.Y+8, r, lines, 1)
			}
		})
		ui.Row(c).Gap(16).Wrap().Children(func() {
			for _, k := range kinds {
				ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
					ui.Box(c).Size(8, 8).Radius(2).Background(k.col)
					caption(c, pal, tr(k.key)).FontSize(11.5)
				})
			}
		})
	})
}

// loadTokenAll reads the token card's whole history. The overview itself
// keeps 90 days, so the All range is read when it is chosen.
func (a *App) loadTokenAll() {
	q := a.d.Query
	if q == nil {
		return
	}
	a.run(&a.tokAllJob, func(ctx context.Context) func() {
		pts, err := q.Daily(ctx, query.Filter{})
		days := make([]dayCost, len(pts))
		for i, p := range pts {
			days[i] = dayCost{day: p.Day, cost: p.CostUSD, tokens: p.Tokens}
		}
		return func() {
			if err != nil {
				a.tokAll.err = err.Error()
				return
			}
			a.tokAll = tokenAll{ready: true, days: days}
		}
	})
}

// tokenBars folds a daily series into bars the chart can draw: one per day
// while the range is short, then whole weeks, then calendar months, so an
// all-time history stays readable instead of collapsing into a smear.
func tokenBars(days []dayCost) []tokenBar {
	switch {
	case len(days) == 0:
		return nil
	case len(days) <= 180:
		out := make([]tokenBar, len(days))
		for i, d := range days {
			out[i] = tokenBar{start: d.day, end: d.day, tokens: d.tokens}
		}
		return out
	case len(days) <= 3*365:
		// Weeks counted back from the newest day, so the newest bar is the
		// last seven days whatever the series skips.
		last := days[len(days)-1].day
		var out []tokenBar
		i, end := len(days)-1, last
		for i >= 0 {
			start := end.AddDate(0, 0, -6)
			b := tokenBar{start: start, end: end}
			for ; i >= 0 && !days[i].day.Before(start); i-- {
				b.tokens = b.tokens.Add(days[i].tokens)
			}
			if i < 0 {
				b.start = days[0].day
			}
			out = append(out, b)
			end = start.AddDate(0, 0, -1)
		}
		slices.Reverse(out)
		return out
	default:
		var out []tokenBar
		for _, d := range days {
			if n := len(out); n > 0 && sameMonth(out[n-1].start, d.day) {
				out[n-1].end = d.day
				out[n-1].tokens = out[n-1].tokens.Add(d.tokens)
				continue
			}
			out = append(out, tokenBar{start: d.day, end: d.day, tokens: d.tokens})
		}
		return out
	}
}

func sameMonth(a, b time.Time) bool {
	ay, am, _ := a.Date()
	by, bm, _ := b.Date()
	return ay == by && am == bm
}

// tokenBarLabel is the bar's date, or the span it covers.
func tokenBarLabel(b tokenBar) string {
	if b.start.Equal(b.end) {
		return fmtDate(b.start)
	}
	return fmtDate(b.start) + " – " + fmtDate(b.end)
}
