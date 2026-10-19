package gui

import (
	"fmt"
	"math"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

func (a *App) overviewPage(c *ui.Context, pal palette) {
	ov := &a.ov
	now := a.now()
	pageHeader(c, pal, tr("overview"), func() {
		caption(c, pal, fmtDate(now)+" "+weekday(now)+" · "+tr("costNoteShort"))
	})
	if ov.err != "" {
		errorLine(c, pal, ov.err)
	}
	if !ov.ready {
		emptyState(c, pal, tr("loading"), "")
		return
	}
	if ov.days == nil && ov.month.Requests == 0 && ov.lastMonth.Requests == 0 {
		emptyState(c, pal, tr("noData"), tr("noDataHint"))
		return
	}
	ui.Row(c).Gap(16).AlignItems(ui.Stretch).Children(func() {
		a.todayCard(c, pal).Grow(1).Basis(0)
		a.liveCard(c, pal)
	})
	a.spendCard(c, pal)
	if len(ov.quotas) > 0 {
		a.quotaCard(c, pal)
	}
	ui.Row(c).Gap(12).AlignItems(ui.Center).Padding(8, 2, 0, 2).Children(func() {
		ui.Text(c, tr("breakdown")).Font(display).FontSize(16).FontWeight(700).TextColor(pal.ink)
		ui.Spacer(c)
		a.pills(c, pal, "ov-range", &a.chartRange, tr("range7"), tr("range30"), tr("range90"))
	})
	k := a.chartRange
	ui.Row(c).Gap(16).AlignItems(ui.Stretch).Children(func() {
		a.toolShare(c, pal, ov.byTool[k], ov.totals[k].CostUSD).Grow(1).Basis(0)
		a.modelShare(c, pal, ov.byModel[k], ov.totals[k].CostUSD).Grow(1).Basis(0)
		a.tokensCard(c, pal, ov.totals[k]).Grow(1).Basis(0)
	})
}

// ---------- today ----------

func (a *App) todayCard(c *ui.Context, pal palette) ui.Element {
	ov := &a.ov
	el := a.rcard(c, pal, "today", 0, true)
	rv := a.mo.cur
	return el.Children(func() {
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			ui.Text(c, tr("todaySpend")+" · "+tr("apiEquivalent")).FontSize(13).TextColor(pal.muted)
			ui.Spacer(c)
			deltaPill(c, pal, ov.today.CostUSD, ov.yesterday.CostUSD, tr("vsYesterday"))
		})
		a.heroNum(c, pal, "today-cost", ov.today.CostUSD, fmtUSD, 44)
		ui.Row(c).Gap(12).Children(func() {
			a.statNum(c, pal, "today-tok", tr("tokens"), float64(shown(ov.today.Tokens)), func(v float64) string { return fmtTokens(int64(v)) })
			a.statNum(c, pal, "today-req", tr("requests"), float64(ov.today.Requests), func(v float64) string { return fmtInt(int64(math.Round(v))) })
			a.statNum(c, pal, "today-ses", tr("sessionsN"), float64(ov.today.Sessions), func(v float64) string { return fmtInt(int64(math.Round(v))) })
			a.statNum(c, pal, "today-hit", tr("hitRate"), ov.today.Tokens.HitRate(), fmtPct)
		})
		// Today, hour by hour, added up.
		now := a.now()
		cum := []float64{0}
		for h := 0; h <= now.Hour() && h < 24; h++ {
			cum = append(cum, cum[len(cum)-1]+ov.hours[h].cost)
		}
		frac := (float64(now.Hour()) + float64(now.Minute())/60) / 24
		box := ui.Box(c.Key("today-line")).FillWidth().Height(70)
		target := float32(0)
		if rv.seen {
			target = 1
		}
		prog := box.AnimateTo("p", target, delayed(ui.Animation{Duration: 1200 * time.Millisecond, Ease: easeOut}, rv.delay+200*time.Millisecond))
		accent, accent2, line, faint, card := pal.accent, pal.accent2, pal.line, pal.faint, pal.card
		labels := []string{"0", "6", "12", "18", "24"}
		box.Draw(func(p *ui.Painter, r ui.Rect) {
			ch := r.H - 16
			p.Fill(ui.Rect{X: r.X, Y: r.Y + ch, W: r.W, H: 1}, line, 0)
			for i, l := range labels {
				x := r.X + r.W*float32(i)/4
				w, _ := p.MeasureText(0, ui.Span{Text: l, Size: 10.5})
				x = min(max(x-w/2, r.X), r.X+r.W-w)
				p.Text(x, r.Y+ch+4, l, 10.5, faint)
			}
			top := cum[len(cum)-1]
			if top <= 0 {
				return
			}
			n := len(cum)
			xs := make([]float32, n)
			ys := make([]float32, n)
			for i := range n {
				f := float64(i) / 24
				if i == n-1 {
					f = frac
				}
				xs[i] = r.X + r.W*float32(f)
				ys[i] = r.Y + 4 + (ch-4)*float32(1-cum[i]/top)
			}
			var path ui.Path
			path.MoveTo(xs[0], ys[0])
			for i := 1; i < n; i++ {
				path.LineTo(xs[i], ys[i])
			}
			clip := ui.Rect{X: r.X, Y: r.Y, W: (xs[n-1] - r.X + 8) * prog, H: r.H}
			p.Clip(clip, 0, func() {
				fill := path
				fill.LineTo(xs[n-1], r.Y+ch)
				fill.LineTo(xs[0], r.Y+ch)
				fill.Close()
				p.FillPathGradient(&fill, ui.LinearGradient{From: accent.Alpha(0.22), To: accent.Alpha(0), Angle: 180, Oklab: true})
				p.StrokePathGradient(&path, 2, ui.LinearGradient{From: accent2, To: accent, Angle: 90, Oklab: true})
			})
			if prog > 0.95 {
				x, y := xs[n-1], ys[n-1]
				p.FillPath((&ui.Path{}).Circle(x, y, 4.5), card)
				p.FillPath((&ui.Path{}).Circle(x, y, 3), accent)
			}
		})
	})
}

// deltaPill compares a value with an earlier one: more is warm, less cool.
func deltaPill(c *ui.Context, pal palette, now, before float64, label string) {
	if before <= 0 {
		return
	}
	d := now/before - 1
	col, arrow := pal.muted, "→"
	switch {
	case d > 0.005:
		col, arrow = pal.warn, "↑"
	case d < -0.005:
		col, arrow = pal.good, "↓"
	}
	ui.Text(c, fmt.Sprintf("%s %.0f%% %s", arrow, math.Abs(d)*100, label)).FontSize(12).FontWeight(650).TextColor(col).
		Padding(3, 9).Radius(10).Background(col.Alpha(0.12)).NoWrap()
}

// delta is deltaPill as plain text, for the tray.
func delta(c *ui.Context, pal palette, now, before float64, label string) ui.Element {
	if before <= 0 {
		return ui.Text(c, "").Width(0)
	}
	d := now/before - 1
	col, sign := pal.muted, ""
	switch {
	case d > 0.005:
		col, sign = pal.warn, "+"
	case d < -0.005:
		col = pal.good
	}
	return ui.Text(c, fmt.Sprintf("%s%.0f%% %s", sign, d*100, label)).FontSize(12).FontWeight(600).TextColor(col).NoWrap()
}

func stat(c *ui.Context, pal palette, label, value string) {
	ui.Column(c).Gap(3).Children(func() {
		ui.Text(c, label).FontSize(12).TextColor(pal.muted).NoWrap()
		ui.Text(c, value).Font(display).FontSize(16).FontWeight(650).TextColor(pal.ink).FontFeatures("tnum").NoWrap()
	})
}

// ---------- spend ----------

// spendCard answers whether the month runs above or below the last, and
// when today's money went.
func (a *App) spendCard(c *ui.Context, pal palette) {
	el := a.rcard(c, pal, "spend", 2, false)
	rv := a.mo.cur
	el.Children(func() {
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			ui.Text(c, tr("spend")).FontSize(14).FontWeight(650).TextColor(pal.ink)
			ui.Spacer(c)
			if a.pills(c, pal, "spend-tab", &a.spendTab, tr("burnup"), tr("todayClock")) {
				delete(a.mo.built, "burnup")
				delete(a.mo.built, "clock")
			}
		})
		tab := ui.Column(c.Key("spend-" + itoa(a.spendTab))).Gap(14).
			Transition(ui.ElementTransition{Duration: 240 * time.Millisecond, Enter: &ui.Motion{Y: 6}, Exit: &ui.Motion{}})
		tab.Children(func() {
			a.mo.cur = rv
			if a.spendTab == 1 {
				a.clock(c, pal)
			} else {
				a.burnup(c, pal)
			}
		})
	})
}

// drawIn reports whether a chart keyed key should be drawn in: its card is
// in view and it was built the frame before, so that its animation starts
// from nothing.
func (a *App) drawIn(c *ui.Context, key string) bool {
	if !a.mo.built[key] {
		a.mo.built[key] = true
		c.Invalidate()
		return false
	}
	return a.mo.cur.seen || c.Preferences().ReduceMotion
}

// burnup is this month's cost added up day by day, last month's as a
// ghost behind it and a dashed forecast to the month's end; under it one
// capsule shows which tools the money went to.
func (a *App) burnup(c *ui.Context, pal palette) {
	ov := &a.ov
	rv := a.mo.cur
	now := a.now()
	mon := monthStart(now)
	lastMon := mon.AddDate(0, -1, 0)
	monthDays := mon.AddDate(0, 1, -1).Day()
	cur, last := ov.monthCum, ov.lastCum
	dayNo := len(cur)
	if dayNo == 0 {
		return
	}
	spent := cur[dayNo-1]
	lastDays := len(last)
	lastTotal := 0.0
	if lastDays > 0 {
		lastTotal = last[lastDays-1]
	}
	forecast := max(ov.forecast, spent)
	ui.Row(c).Gap(36).AlignItems(ui.End).Wrap().Children(func() {
		ui.Column(c).Gap(4).Children(func() {
			ui.Text(c, trf("monthSpent", fmtMonth(now))).FontSize(12.5).TextColor(pal.muted)
			a.heroNum(c, pal, "bu-spent", spent, fmtUSD, 32)
		})
		if lastDays > 0 && lastTotal > 0 {
			same := last[min(dayNo, lastDays)-1]
			d := spent - same
			ui.Column(c).Gap(4).Padding(0, 0, 4, 0).Children(func() {
				ui.Text(c, trf("vsSamePeriod", fmtMonth(lastMon))).FontSize(12.5).TextColor(pal.muted)
				col, word := pal.good, tr("deltaLess")
				if d > 0 {
					col, word = pal.warn, tr("deltaMore")
				}
				ui.Row(c).Gap(6).AlignItems(ui.End).Children(func() {
					a.num(c, "bu-delta", math.Abs(d), fmtUSD, func(e ui.Element) ui.Element {
						return e.Font(display).FontSize(17).FontWeight(650).TextColor(col).FontFeatures("tnum").NoWrap()
					})
					if same > 0 {
						ui.Text(c, fmt.Sprintf("%s %.0f%%", word, math.Abs(d)/same*100)).FontSize(12.5).FontWeight(600).TextColor(col).NoWrap().Padding(0, 0, 2, 0)
					}
				})
			})
		}
		ui.Column(c).Gap(4).Padding(0, 0, 4, 0).Children(func() {
			ui.Text(c, tr("atThisPace")).FontSize(12.5).TextColor(pal.muted)
			a.num(c, "bu-fc", forecast, fmtUSD, func(e ui.Element) ui.Element {
				return e.Font(display).FontSize(17).FontWeight(650).TextColor(pal.ink).FontFeatures("tnum").NoWrap()
			})
		})
		if lastTotal > 0 {
			ui.Column(c).Gap(4).Padding(0, 0, 4, 0).Children(func() {
				ui.Text(c, trf("monthTotal", fmtMonth(lastMon))).FontSize(12.5).TextColor(pal.muted)
				ui.Text(c, fmtUSD(lastTotal)).Font(display).FontSize(17).FontWeight(650).TextColor(pal.muted).FontFeatures("tnum").NoWrap()
			})
		}
	})

	box := ui.Box(c.Key("bu-chart")).FillWidth().Height(240)
	target := float32(0)
	if a.drawIn(c, "burnup") {
		target = 1
	}
	prog := box.AnimateTo("p", target, delayed(ui.Animation{Duration: 1400 * time.Millisecond, Ease: easeOut}, rv.delay+150*time.Millisecond))
	ghost := box.AnimateTo("g", target, delayed(ui.Animation{Duration: 900 * time.Millisecond, Ease: easeOut}, rv.delay))
	const right, bottom = 52, 22
	hov := -1
	if px, _, over := box.PointerPosition(); over {
		w := box.LayoutBounds().W - right
		if w > 0 {
			hov = int(math.Round(float64(px/w*float32(monthDays-1)))) + 1
			hov = max(1, min(monthDays, hov))
		}
	}
	hx := box.AnimateTo("hx", float32(max(hov, dayNo)), snappy)
	ha := box.AnimateTo("ha", map[bool]float32{false: 0, true: 1}[hov > 0], quick)
	breathe := len(ov.active) > 0 && !c.Preferences().ReduceMotion
	lastName, thisName := fmtMonth(lastMon), fmtMonth(now)
	peak := max(forecast, lastTotal) * 1.05
	top, step := niceTop(peak)
	box.Draw(func(p *ui.Painter, r ui.Rect) {
		pw, ph := r.W-right, r.H-bottom
		x0, y0 := r.X, r.Y+6
		ph -= 6
		xAt := func(day float64) float32 { return x0 + pw*float32((day-1)/float64(monthDays-1)) }
		yAt := func(v float64) float32 { return y0 + ph - ph*float32(v/top) }
		for v := 0.0; v <= top+step/2; v += step {
			y := yAt(v)
			p.Fill(ui.Rect{X: x0, Y: y, W: pw, H: 1}, pal.line, 0)
			p.Text(x0+pw+10, y-7, fmtUSDShort(v), 10.5, pal.faint)
		}
		for _, d := range []int{1, 8, 15, 22, monthDays} {
			l := trf("dayN", d)
			w, _ := p.MeasureText(0, ui.Span{Text: l, Size: 10.5})
			x := min(max(xAt(float64(d))-w/2, x0), x0+pw-w)
			p.Text(x, y0+ph+7, l, 10.5, pal.faint)
		}

		// last month, a ghost
		if gn := int(float32(lastDays) * ghost); gn > 1 {
			var gp ui.Path
			gp.MoveTo(xAt(1), yAt(0))
			for i := range gn {
				gp.LineTo(xAt(float64(min(i+1, monthDays))), yAt(last[i]))
			}
			p.StrokePath(&gp, 1.5, pal.muted.Alpha(0.45))
			if ghost > 0.98 {
				x, y := xAt(float64(min(lastDays, monthDays))), yAt(lastTotal)
				w, _ := p.MeasureText(0, ui.Span{Text: lastName, Size: 11})
				p.Text(x-w-4, y+6, lastName, 11, pal.muted)
			}
		}

		// this month, drawn in
		xs := make([]float32, dayNo)
		ys := make([]float32, dayNo)
		for i := range dayNo {
			xs[i], ys[i] = xAt(float64(i+1)), yAt(cur[i])
		}
		var line ui.Path
		line.MoveTo(xAt(1), yAt(0))
		for i := range dayNo {
			line.LineTo(xs[i], ys[i])
		}
		trim := min(prog*1.15, 1)
		clip := ui.Rect{X: r.X, Y: r.Y, W: (xs[dayNo-1] - r.X + 10) * trim, H: r.H}
		p.Clip(clip, 0, func() {
			fill := line
			fill.LineTo(xs[dayNo-1], y0+ph)
			fill.LineTo(xAt(1), y0+ph)
			fill.Close()
			p.FillPathGradient(&fill, ui.LinearGradient{From: pal.accent.Alpha(0.24), To: pal.accent.Alpha(0), Angle: 180, Oklab: true})
			p.StrokePathGradient(&line, 2.5, ui.LinearGradient{From: pal.accent2, To: pal.accent, Angle: 90, Oklab: true})
		})

		// the forecast, dashed, once the line arrives
		if prog > 0.85 && dayNo < monthDays {
			al := float32(clamp01(float64(prog-0.85) / 0.15))
			xa, ya := xs[dayNo-1], ys[dayNo-1]
			xb, yb := xAt(float64(monthDays)), yAt(forecast)
			const n = 28
			for i := 0; i < n; i += 2 {
				f0, f1 := float32(i)/n, float32(i+1)/n
				var d ui.Path
				d.MoveTo(xa+(xb-xa)*f0, ya+(yb-ya)*f0)
				d.LineTo(xa+(xb-xa)*f1, ya+(yb-ya)*f1)
				p.StrokePath(&d, 1.75, pal.accent.Alpha(0.6*al))
			}
			p.FillPath((&ui.Path{}).Circle(xb, yb, 4), pal.card)
			p.StrokePath((&ui.Path{}).Circle(xb, yb, 4), 1.75, pal.accent.Alpha(0.75*al))
			lbl := tr("projected") + " " + fmtUSDShort(forecast)
			tw, _ := p.MeasureText(0, ui.Span{Text: lbl, Size: 11.5})
			p.Text(xb-tw-8, yb-22, lbl, 11.5, pal.accent.Alpha(al))
		}
		if prog > 0.85 {
			// today
			xa, ya := xs[dayNo-1], ys[dayNo-1]
			if breathe {
				t := float64(p.Now().UnixMilli()%2400) / 2400
				k := float32(0.5 + 0.5*math.Sin(t*2*math.Pi))
				p.FillPath((&ui.Path{}).Circle(xa, ya, 7+5*k), pal.accent.Alpha(0.22*(1-k)))
				p.After(glowFrame)
			}
			p.FillPath((&ui.Path{}).Circle(xa, ya, 5), pal.card)
			p.FillPath((&ui.Path{}).Circle(xa, ya, 3.2), pal.accent)
		}

		// hover: a rule and the two months side by side
		if ha > 0.01 {
			d := float64(hx)
			x := xAt(d)
			p.Fill(ui.Rect{X: x - 0.5, Y: y0, W: 1, H: ph}, pal.ink.Alpha(0.18*ha), 0)
			di := int(math.Round(d))
			lines := []string{fmt.Sprintf("%s %s", thisName, trf("dayN", di))}
			if di <= dayNo {
				lines = append(lines, tr("thisMonth")+"  "+fmtUSD(cur[di-1]))
			} else {
				perDay := (forecast - spent) / float64(max(monthDays-dayNo, 1))
				lines = append(lines, tr("projected")+"  "+fmtUSD(spent+perDay*float64(di-dayNo)))
			}
			if di <= lastDays {
				lines = append(lines, lastName+"  "+fmtUSD(last[di-1]))
			}
			tooltip(p, pal, x, y0+4, r, lines, ha)
		}
	})

	// where it went: one capsule, segments growing in turn
	var shownTools []query.Bucket
	other, total := 0.0, 0.0
	for i, b := range ov.monthTools {
		total += b.CostUSD
		if i < 5 {
			shownTools = append(shownTools, b)
		} else {
			other += b.CostUSD
		}
	}
	if total <= 0 {
		return
	}
	cp := ui.Box(c.Key("bu-cap")).FillWidth().Height(10)
	cv := cp.AnimateTo("p", target, delayed(ui.Animation{Duration: 1100 * time.Millisecond, Ease: easeOut}, rv.delay+450*time.Millisecond))
	well := pal.well
	cp.Draw(func(p *ui.Painter, r ui.Rect) {
		p.Clip(r, 5, func() {
			p.Fill(r, well, 5)
			x := r.X
			for _, b := range shownTools {
				w := r.W * float32(b.CostUSD/total) * cv
				p.Fill(ui.Rect{X: x, Y: r.Y, W: max(w-2, 0), H: r.H}, harnessColor(model.Harness(b.Key)), 0)
				x += w
			}
			if other > 0 {
				p.Fill(ui.Rect{X: x, Y: r.Y, W: r.W * float32(other/total) * cv, H: r.H}, otherColor, 0)
			}
		})
	})
	ui.Row(c).Gap(20).Wrap().Children(func() {
		legend := func(col ui.Color, name string, v float64) {
			ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
				ui.Box(c).Size(8, 8).Radius(4).Background(col)
				ui.Text(c, name).FontSize(12.5).TextColor(pal.muted).NoWrap()
				ui.Text(c, fmtUSD(v)).FontSize(12.5).FontWeight(650).TextColor(pal.ink).FontFeatures("tnum").NoWrap()
			})
		}
		for _, b := range shownTools {
			h := model.Harness(b.Key)
			legend(harnessColor(h), h.DisplayName(), b.CostUSD)
		}
		if other > 0 {
			legend(otherColor, tr("other"), other)
		}
	})
}

// tooltip paints a small card of lines beside x, the first line muted.
func tooltip(p *ui.Painter, pal palette, x, y float32, bounds ui.Rect, lines []string, alpha float32) {
	tw := float32(0)
	for _, l := range lines {
		w, _ := p.MeasureText(0, ui.Span{Text: l, Size: 12})
		tw = max(tw, w)
	}
	tw += 24
	th := float32(14 + 18*len(lines))
	tx := x + 12
	if tx+tw > bounds.X+bounds.W {
		tx = x - 12 - tw
	}
	box := ui.Rect{X: tx, Y: y, W: tw, H: th}
	p.Fill(ui.Rect{X: box.X, Y: box.Y + 2, W: box.W, H: box.H}, ui.RGBA(0, 0, 0, 0.12*alpha), 10)
	p.Fill(box, pal.tip.Alpha(0.97*alpha), 10)
	for i, l := range lines {
		col := pal.ink
		if i == 0 {
			col = pal.muted
		}
		p.Text(tx+12, y+8+float32(i)*18, l, 12, col.Alpha(alpha))
	}
}

// niceTop rounds v up to a tidy axis top and returns its step.
func niceTop(v float64) (float64, float64) {
	if v <= 0 {
		return 1, 0.25
	}
	mag := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		step := m * mag / 4
		if top := step * 4; top >= v {
			return top, step
		}
	}
	return 10 * mag, 2.5 * mag
}

// seg strokes a straight segment in any direction (Painter.Line only
// draws axis-aligned ones).
func seg(p *ui.Painter, x0, y0, x1, y1, w float32, col ui.Color) {
	var path ui.Path
	path.MoveTo(x0, y0)
	path.LineTo(x1, y1)
	p.StrokePath(&path, w, col)
}

// clock is today on a 24-hour dial: each hour a bar outward, split by
// tool; pointing at an hour tells what it cost.
func (a *App) clock(c *ui.Context, pal palette) {
	ov := &a.ov
	rv := a.mo.cur
	now := a.now()
	hours := ov.hours
	peak, total, busiest, first, active := 0.0, 0.0, -1, -1, 0
	for h, d := range hours {
		total += d.cost
		if d.cost > 0 {
			active++
			if first < 0 {
				first = h
			}
			if busiest < 0 || d.cost > hours[busiest].cost {
				busiest = h
			}
		}
		peak = max(peak, d.cost)
	}
	ui.Row(c).Gap(36).AlignItems(ui.Center).Children(func() {
		const size = 268
		box := ui.Box(c.Key("clock")).Size(size, size).Shrink(0)
		target := float32(0)
		if a.drawIn(c, "clock") {
			target = 1
		}
		prog := box.AnimateTo("p", target, delayed(ui.Animation{Duration: 1100 * time.Millisecond, Ease: easeOut}, rv.delay+100*time.Millisecond))
		hov := -1
		const r0, r1 = 64, 126
		if px, py, over := box.PointerPosition(); over {
			dx, dy := float64(px-size/2), float64(py-size/2)
			if d := math.Hypot(dx, dy); d > r0-16 && d < r1+12 {
				ang := math.Atan2(dx, -dy)
				if ang < 0 {
					ang += 2 * math.Pi
				}
				hov = int(ang/(2*math.Pi)*24) % 24
			}
		}
		hl := box.AnimateTo("hl", map[bool]float32{false: 0, true: 1}[hov >= 0], quick)
		nowHour := now.Hour()
		tools := ov.hourTools
		box.Draw(func(p *ui.Painter, r ui.Rect) {
			cx, cy := r.X+r.W/2, r.Y+r.H/2
			p.StrokePath(ringPath(cx, cy, r0-12), 1, pal.line)
			p.StrokePath(ringPath(cx, cy, r1+6), 1, pal.line)
			for h, d := range hours {
				ang := float64(h)/24*2*math.Pi + math.Pi/24
				dx, dy := float32(math.Sin(ang)), -float32(math.Cos(ang))
				k := float32(outQuint(float64(prog)*1.5 - float64(h)/24*0.5))
				dim := float32(1)
				if hov >= 0 && h != hov {
					dim = 1 - 0.6*hl
				}
				if d.cost <= 0 || peak <= 0 {
					col := pal.well
					if h > nowHour {
						col = pal.well.Alpha(0.5)
					}
					seg(p, cx+dx*r0, cy+dy*r0, cx+dx*(r0+5), cy+dy*(r0+5), 6, col)
					continue
				}
				at := float32(r0)
				span := (r1 - r0) * k
				put := func(v float64, col ui.Color) {
					l := float32(v/peak) * span
					if l < 0.5 {
						return
					}
					e := max(l-1.5, 0.5)
					seg(p, cx+dx*at, cy+dy*at, cx+dx*(at+e), cy+dy*(at+e), 9, col.Alpha(dim))
					at += l
				}
				for i, v := range d.tools {
					put(v, harnessColor(tools[i]))
				}
				put(d.other, otherColor)
			}
			// the hour hand: now
			fr := (float64(now.Hour()) + float64(now.Minute())/60) / 24
			hx, hy := pointOn(cx, cy, r1+6, float32(fr))
			p.FillPath((&ui.Path{}).Circle(hx, hy, 4.5), pal.card)
			p.FillPath((&ui.Path{}).Circle(hx, hy, 3), pal.accent)
			for _, l := range []int{0, 6, 12, 18} {
				ang := float64(l) / 24 * 2 * math.Pi
				x, y := cx+float32(math.Sin(ang))*(r0-24), cy-float32(math.Cos(ang))*(r0-24)
				s := fmt.Sprint(l)
				w, _ := p.MeasureText(0, ui.Span{Text: s, Size: 10.5})
				p.Text(x-w/2, y-7, s, 10.5, pal.faint)
			}
		}).Children(func() {
			ui.Column(c).Fill().Center().Gap(1).Children(func() {
				if hov >= 0 {
					ui.Text(c, fmt.Sprintf("%02d:00", hov)).FontSize(11.5).TextColor(pal.muted)
					ui.Text(c, fmtUSD(hours[hov].cost)).Font(display).FontSize(20).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum")
					return
				}
				ui.Text(c, tr("today")).FontSize(11.5).TextColor(pal.muted)
				a.heroNum(c, pal, "clk-total", total, fmtUSD, 20)
			})
		})
		ui.Column(c).Gap(18).Grow(1).MinWidth(0).Children(func() {
			ui.Row(c).Gap(28).Wrap().Children(func() {
				if busiest >= 0 {
					stat(c, pal, tr("busiestHour"), fmt.Sprintf("%02d:00 · %s", busiest, fmtUSD(hours[busiest].cost)))
					stat(c, pal, tr("startedAt"), fmt.Sprintf("%02d:00", first))
					stat(c, pal, tr("activeHours"), trf("hoursN", active))
				}
				stat(c, pal, tr("thisHour"), fmtUSD(hours[nowHour].cost))
			})
			ui.Column(c).Gap(10).Children(func() {
				for _, b := range ov.todayTools {
					if b.CostUSD <= 0 {
						continue
					}
					h := model.Harness(b.Key)
					ui.Row(c.Key(b.Key)).Gap(10).AlignItems(ui.Center).Children(func() {
						toolBadge(c, h, 20)
						ui.Text(c, h.DisplayName()).FontSize(13).TextColor(pal.ink).SingleLine().Shrink(1).MinWidth(0)
						ui.Spacer(c)
						ui.Text(c, fmtPct(frac(b.CostUSD, total))).FontSize(12).TextColor(pal.muted).FontFeatures("tnum").NoWrap()
						ui.Text(c, fmtUSD(b.CostUSD)).FontSize(13).FontWeight(650).TextColor(pal.ink).FontFeatures("tnum").NoWrap().MinWidth(64).TextAlign(ui.End)
					})
				}
				if total == 0 {
					caption(c, pal, tr("noUsageToday"))
				}
			})
		})
	})
}

func outQuint(x float64) float64 { x = clamp01(x); return 1 - math.Pow(1-x, 5) }

// ---------- breakdown ----------

const shareRows = 6

func (a *App) toolShare(c *ui.Context, pal palette, rows []query.Bucket, total float64) ui.Element {
	return a.rcard(c, pal, "by-tool", 4, true).Children(func() {
		cardTitle(c, pal, tr("byTool"), nil)
		if len(rows) == 0 {
			caption(c, pal, tr("noUsage"))
		}
		for i, b := range rows {
			if i == shareRows {
				break
			}
			h := model.Harness(b.Key)
			shareRow(c.Key(b.Key), pal, func() { toolBadge(c, h, 18) }, h.DisplayName(), fmtUSD(b.CostUSD), fmtPct(frac(b.CostUSD, total)), frac(b.CostUSD, total), harnessColor(h)).
				Transition(ui.ElementTransition{Animation: &snappy, Position: true})
		}
	})
}

func (a *App) modelShare(c *ui.Context, pal palette, rows []query.Bucket, total float64) ui.Element {
	return a.rcard(c, pal, "by-model", 5, true).Children(func() {
		cardTitle(c, pal, tr("byModel"), nil)
		if len(rows) == 0 {
			caption(c, pal, tr("noUsage"))
		}
		for i, b := range rows {
			if i == shareRows {
				break
			}
			label := b.Label
			if label == "" {
				label = b.Key
			}
			value := fmtUSD(b.CostUSD)
			if b.Unpriced == b.Requests && b.Requests > 0 {
				value = tr("unpriced")
			}
			shareRow(c.Key(b.Key), pal, nil, label, value, fmtPct(frac(b.CostUSD, total)), frac(b.CostUSD, total), seriesAt(i)).
				Transition(ui.ElementTransition{Animation: &snappy, Position: true})
		}
	})
}

type tokenClass struct {
	key string
	v   int64
	col ui.Color
}

// tokenClasses are the kinds of tokens and their colors: cache reads, by
// far the most, stay quiet.
func tokenClasses(pal palette, tk model.Tokens) []tokenClass {
	return []tokenClass{
		{"input", tk.Input, pal.accent},
		{"output", tk.Output, pal.accent2},
		{"cacheRead", tk.CacheRead, pal.muted.Alpha(0.35)},
		{"cacheWrite", tk.CacheWrite, ui.Hex("#D9A54C")},
		{"reasoning", tk.Reasoning, ui.Hex("#9A86DB")},
	}
}

// tokensCard is the range's tokens by kind around the cache hit rate.
func (a *App) tokensCard(c *ui.Context, pal palette, t query.Totals) ui.Element {
	el := a.rcard(c, pal, "tokens", 6, true)
	rv := a.mo.cur
	return el.Children(func() {
		cardTitle(c, pal, tr("tokens"), func() {
			ui.Text(c, fmtTokens(shown(t.Tokens))).FontSize(13).FontWeight(600).TextColor(pal.ink).FontFeatures("tnum")
		})
		classes := tokenClasses(pal, t.Tokens)
		total := float64(t.Tokens.Total())
		ui.Row(c).Gap(18).AlignItems(ui.Center).Children(func() {
			donut := ui.Box(c.Key("donut")).Size(108, 108).Shrink(0)
			target := float32(0)
			if rv.seen {
				target = 1
			}
			prog := donut.AnimateTo("p", target, delayed(ui.Animation{Duration: 1000 * time.Millisecond, Ease: easeOut}, rv.delay+150*time.Millisecond))
			fr := make([]float32, len(classes))
			for i, k := range classes {
				if total > 0 {
					fr[i] = donut.AnimateTo("f"+k.key, float32(float64(k.v)/total), gentle)
				}
			}
			well := pal.well
			donut.Draw(func(p *ui.Painter, r ui.Rect) {
				cx, cy := r.X+r.W/2, r.Y+r.H/2
				const w = 11
				rad := (min(r.W, r.H) - w) / 2
				p.StrokePath(ringPath(cx, cy, rad), w, well)
				at := float32(0)
				for i, k := range classes {
					f := fr[i] * prog
					if f > 0.004 {
						p.StrokePath(ringPath(cx, cy, rad).Trim(at+0.002, at+f-0.002), w, k.col)
					}
					at += f
				}
			}).Children(func() {
				ui.Column(c).Fill().Center().Children(func() {
					a.num(c, "hit", t.Tokens.HitRate(), fmtPct, func(e ui.Element) ui.Element {
						return e.Font(display).FontSize(19).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum")
					})
					ui.Text(c, tr("hitRate")).FontSize(10.5).TextColor(pal.muted)
				})
			})
			ui.Column(c).Gap(7).Grow(1).MinWidth(0).Children(func() {
				for _, k := range classes {
					if k.v == 0 && k.key == "reasoning" {
						continue
					}
					ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
						ui.Box(c).Size(8, 8).Radius(4).Background(k.col).Shrink(0)
						ui.Text(c, tr(k.key)).FontSize(12).TextColor(pal.muted).SingleLine().Shrink(1).MinWidth(0)
						ui.Spacer(c)
						ui.Text(c, fmtTokens(k.v)).FontSize(12).TextColor(pal.ink).FontFeatures("tnum").NoWrap()
					})
				}
			})
		})
	})
}

func frac(v, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return v / total
}

// withUsage returns a palette whose usage colors are all col, for rings
// that show something other than a quota.
func (p palette) withUsage(col ui.Color) palette {
	p.good, p.warn, p.bad = col, col, col
	return p
}
