package gui

import (
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/zzstar101/mytoken/internal/model"
)

// tokenUsageCard counts every token, cache reads included: today, this
// month and the range's daily average, over daily bars split by kind.
func (a *App) tokenUsageCard(c *ui.Context, pal palette) {
	ov := &a.ov
	el := a.rcard(c, pal, "token-usage", 2, false)
	rv := a.mo.cur
	n := chartDays[a.tokRange]
	days := ov.days
	if len(days) > n {
		days = days[len(days)-n:]
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
	el.Children(func() {
		cardTitle(c, pal, tr("tokenUsage"), func() {
			a.pills(c, pal, "tok-range", &a.tokRange, tr("range7"), tr("range30"), tr("range90"))
		})
		tokFmt := func(v float64) string { return fmtTokens(int64(v)) }
		ui.Row(c).Gap(28).Children(func() {
			a.statNum(c, pal, "tok-today", tr("today"), float64(count(ov.today.Tokens)), tokFmt)
			a.statNum(c, pal, "tok-month", tr("thisMonth"), float64(count(ov.month.Tokens)), tokFmt)
			a.statNum(c, pal, "tok-range-sum", trf("tokRangeSum", n), float64(sum), tokFmt)
			a.statNum(c, pal, "tok-avg", tr("tokAvg"), float64(avg), tokFmt)
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
		peak := int64(0)
		for _, d := range days {
			peak = max(peak, count(d.tokens))
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
			if len(days) == 0 {
				return
			}
			slot := plot.W / float32(len(days))
			bw := max(min(slot*0.62, 22), 2)
			hover := -1
			if over && px >= left {
				hover = min(int((px-left)/slot), len(days)-1)
			}
			every := max(len(days)/6, 1)
			for i, d := range days {
				x := plot.X + slot*float32(i) + (slot-bw)/2
				y := plot.Y + plot.H
				alpha := float32(1)
				if hover >= 0 && hover != i {
					alpha = 0.45
				}
				for _, k := range kinds {
					v := float32(k.of(d.tokens)) / scale * plot.H * grow
					if v <= 0 {
						continue
					}
					y -= v
					p.Fill(ui.Rect{X: x, Y: y, W: bw, H: v}, k.col.Alpha(alpha), min(bw/2, 3))
				}
				if (len(days)-1-i)%every == 0 {
					l := fmtDate(d.day)
					w, _ := p.MeasureText(0, ui.Span{Text: l, Size: 10})
					p.Text(min(max(x+bw/2-w/2, plot.X), plot.X+plot.W-w), plot.Y+plot.H+5, l, 10, faint)
				}
			}
			if hover >= 0 {
				d := days[hover]
				lines := []string{fmtDate(d.day) + " · " + fmtTokens(count(d.tokens))}
				for i := len(kinds) - 1; i >= 0; i-- {
					lines = append(lines, tr(kinds[i].key)+"  "+fmtTokens(kinds[i].of(d.tokens)))
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
