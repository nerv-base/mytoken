package gui

import (
	"context"
	"slices"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

// How long a history may get before the chart folds it: a bar a day reads
// fine up to half a year, a bar a week up to three years, and calendar months
// beyond that.
const (
	dailyBarsMax  = 180
	weeklyBarsMax = 3 * 365
)

// tokenAll is the token card's all-time series. The overview carries only the
// last 90 days, so the whole history is read when All is chosen, and folded
// into bars once with the read rather than on every frame.
type tokenAll struct {
	ready  bool
	err    string
	days   []dayCost
	series tokenSeries
}

// tokenSeries is a range's bars with the two figures the card sums from them.
type tokenSeries struct {
	bars   []tokenBar
	sum    int64
	active int // days that carried tokens
}

// barUnit is how much of the calendar one bar covers.
type barUnit int

const (
	dayBar barUnit = iota
	weekBar
	monthBar
)

// tokenBar is one bar of the token card: a day, a week or a month,
// depending on how long the chosen range is.
type tokenBar struct {
	start, end time.Time
	tokens     model.Tokens
	unit       barUnit
}

// tokAllTime reports whether the token card is showing all time, the one range
// the overview cannot serve. The card and the overview's reload path both ask
// here, so the table's "0 means all time" is read in one place.
func (a *App) tokAllTime() bool { return rangeDays[a.tokRange] == 0 }

// tokenUsageCard counts every token, cache reads included: today, this
// month and the chosen range's sum and daily average, over bars split by
// kind.
func (a *App) tokenUsageCard(c *ui.Context, pal palette) {
	ov := &a.ov
	el := a.rcard(c, pal, "token-usage", 2, false)
	rv := a.mo.cur
	count := func(t model.Tokens) int64 { return t.Total() }
	el.Children(func() {
		cardTitle(c, pal, tr("tokenUsage"), func() {
			if a.pills(c, pal, "tok-range", &a.tokRange, rangeLabels()...) && a.tokAllTime() {
				a.loadTokenAll()
			}
		})
		// Read after the pills, so the frame a range is picked already draws
		// the range that was picked.
		all := a.tokAllTime()
		n := rangeDays[a.tokRange]
		if all && a.tokAll.err != "" {
			errorLine(c, pal, a.tokAll.err)
		}
		days := ov.days
		if all {
			days = a.tokAll.days
		} else if len(days) > n {
			days = days[len(days)-n:]
		}
		// All time arrives folded with its read; the three ranges fold their
		// handful of days into a reused buffer, so a rebuilt frame allocates
		// nothing and never walks the whole history.
		series := a.tokSeries(days)
		avg := int64(0)
		if series.active > 0 {
			avg = series.sum / int64(series.active)
		}
		rangeLabel := trf("tokRangeSum", n)
		if all {
			rangeLabel = tr("tokAllSum")
		}
		tokFmt := func(v float64) string { return fmtTokens(int64(v)) }
		// All time is read on demand: until it lands, its two figures say so
		// instead of claiming a zero that would be wrong.
		sumFmt := tokFmt
		if all && !a.tokAll.ready {
			sumFmt = func(float64) string { return tr("loading") }
		}
		ui.Row(c).Gap(28).Children(func() {
			a.statNum(c, pal, "tok-today", tr("today"), float64(count(ov.today.Tokens)), tokFmt)
			a.statNum(c, pal, "tok-month", tr("thisMonth"), float64(count(ov.month.Tokens)), tokFmt)
			a.statNum(c, pal, "tok-range-sum", rangeLabel, float64(series.sum), sumFmt)
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
		bars := series.bars
		everyYear := spansYears(bars)
		peak := int64(0)
		for _, b := range bars {
			peak = max(peak, count(b.tokens))
		}
		top, step := niceTop(float64(peak))

		box := ui.Box(c.Key("tok-bars-" + itoa(a.tokRange))).FillWidth().Height(170)
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
					l := tokenBarTick(b, everyYear)
					w, _ := p.MeasureText(0, ui.Span{Text: l, Size: 10})
					p.Text(min(max(x+bw/2-w/2, plot.X), plot.X+plot.W-w), plot.Y+plot.H+5, l, 10, faint)
				}
			}
			if hover >= 0 {
				b := bars[hover]
				lines := []string{tokenBarLabel(b, everyYear) + " · " + fmtTokens(count(b.tokens))}
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

// tokSeries is the series the card draws for days. All time comes folded from
// its read; the three ranges fold their handful of days into a buffer that
// outlives the frame, because the view is rebuilt every frame.
func (a *App) tokSeries(days []dayCost) tokenSeries {
	if a.tokAllTime() {
		if !a.tokAll.ready {
			// The read is in flight: there is nothing to fold yet.
			return tokenSeries{}
		}
		return a.tokAll.series
	}
	s := foldSeries(a.tokScratch, days)
	a.tokScratch = s.bars
	return s
}

// loadTokenAll reads the token card's whole history. The overview itself
// keeps 90 days, so the All range is read when it is chosen; both the read and
// the folding run off the UI thread.
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
		series := foldSeries(nil, days)
		return func() {
			if err != nil {
				// Keep whatever was read last: the error line says it is stale,
				// and ready stays false so the figures do not claim a zero.
				a.tokAll.err = err.Error()
				return
			}
			a.tokAll = tokenAll{ready: true, days: days, series: series}
		}
	})
}

// tokenBars folds a daily series into bars, into a fresh slice.
func tokenBars(days []dayCost) []tokenBar { return foldSeries(nil, days).bars }

// foldSeries folds a daily series into bars the chart can draw: one per day
// while the range is short, then whole weeks, then calendar months, so an
// all-time history stays readable instead of collapsing into a smear. It sums
// the range in the same pass and writes the bars into buf, which lets the card
// reuse one buffer frame after frame. No tokens are lost to the folding.
func foldSeries(buf []tokenBar, days []dayCost) tokenSeries {
	s := tokenSeries{}
	for _, d := range days {
		v := d.tokens.Total()
		s.sum += v
		if v > 0 {
			s.active++
		}
	}
	buf = buf[:0]
	switch {
	case len(days) <= dailyBarsMax:
		for _, d := range days {
			buf = append(buf, tokenBar{start: d.day, end: d.day, tokens: d.tokens, unit: dayBar})
		}
	case len(days) <= weeklyBarsMax:
		// Weeks counted back from the newest day, so the newest bar is the
		// last seven days whatever the series skips.
		i, end := len(days)-1, days[len(days)-1].day
		for i >= 0 {
			start := end.AddDate(0, 0, -6)
			b := tokenBar{start: start, end: end, unit: weekBar}
			for ; i >= 0 && !days[i].day.Before(start); i-- {
				b.tokens = b.tokens.Add(days[i].tokens)
			}
			if i < 0 {
				b.start = days[0].day
			}
			buf = append(buf, b)
			end = start.AddDate(0, 0, -1)
		}
		slices.Reverse(buf)
	default:
		// Calendar months, so a bar reads as "March" however sparse the month
		// was.
		for _, d := range days {
			if n := len(buf); n > 0 && monthStart(buf[n-1].start).Equal(monthStart(d.day)) {
				buf[n-1].end = d.day
				buf[n-1].tokens = buf[n-1].tokens.Add(d.tokens)
				continue
			}
			buf = append(buf, tokenBar{start: d.day, end: d.day, tokens: d.tokens, unit: monthBar})
		}
	}
	s.bars = buf
	return s
}

// spansYears reports whether the bars cross a calendar year, where a bare
// "Mar 5" would be ambiguous.
func spansYears(bars []tokenBar) bool {
	if len(bars) == 0 {
		return false
	}
	return bars[0].start.Year() != bars[len(bars)-1].end.Year()
}

// tokenBarLabel is the bar's date, or the span it covers. A series that
// crosses a year spells the year out.
func tokenBarLabel(b tokenBar, year bool) string {
	switch {
	case b.unit == monthBar:
		return fmtMonthYear(b.start)
	case b.start.Equal(b.end):
		if year {
			return fmtDateYear(b.start)
		}
		return fmtDate(b.start)
	case year:
		return fmtDateYear(b.start) + " – " + fmtDateYear(b.end)
	}
	return fmtDate(b.start) + " – " + fmtDate(b.end)
}

// tokenBarTick is the axis label under a bar: its day, its week's first day or
// its month.
func tokenBarTick(b tokenBar, year bool) string {
	switch {
	case b.unit == monthBar:
		return fmtMonthYear(b.start)
	case year:
		return fmtDateYear(b.start)
	}
	return fmtDate(b.start)
}
