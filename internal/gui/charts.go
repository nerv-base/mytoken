package gui

import (
	"math"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/chart"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

// niceMax rounds v up to 1, 2, 2.5 or 5 times a power of ten.
func costOfEvent(e query.AttributedEvent) string {
	if !e.Priced {
		return "—"
	}
	return fmtCost(e.Cost)
}

func niceMax(v float64) float64 {
	if v <= 0 {
		return 1
	}
	e := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if v <= m*e {
			return m * e
		}
	}
	return 10 * e
}

// tooltipBox paints a small glassy tip with lines of text near x, y, kept
// inside bounds.
func tooltipBox(p *ui.Painter, pal palette, bounds ui.Rect, x, y float32, title string, rows [][2]string, colors []ui.Color) {
	const pad, lh = 10, 18
	w := float32(0)
	tw, _ := p.MeasureText(0, ui.Span{Text: title, Size: 12, Weight: 700})
	w = tw
	for _, r := range rows {
		a, _ := p.MeasureText(0, ui.Span{Text: r[0], Size: 11.5})
		b, _ := p.MeasureText(0, ui.Span{Text: r[1], Size: 11.5, Weight: 650})
		if a+b+28 > w {
			w = a + b + 28
		}
	}
	w += pad * 2
	h := float32(pad*2+16) + float32(len(rows))*lh
	bx, by := x+14, y-h/2
	if bx+w > bounds.X+bounds.W {
		bx = x - 14 - w
	}
	if by < bounds.Y {
		by = bounds.Y
	}
	if by+h > bounds.Y+bounds.H {
		by = bounds.Y + bounds.H - h
	}
	r := ui.Rect{X: bx, Y: by, W: w, H: h}
	bg := ui.RGBA(255, 255, 255, 0.94)
	if pal.dark {
		bg = ui.RGBA(36, 32, 50, 0.94)
	}
	p.Shadow(r, 10, 0, 6, 18, 0, ui.RGBA(0, 0, 0, 0.18))
	p.Fill(r, bg, 10)
	p.Stroke(r, pal.faint, 10, 1)
	p.RichText(bx+pad, by+pad, 0, ui.Span{Text: title, Size: 12, Weight: 700, Color: pal.ink})
	for i, row := range rows {
		ry := by + pad + 20 + float32(i)*lh
		if i < len(colors) {
			p.Fill(ui.Rect{X: bx + pad, Y: ry + 4, W: 8, H: 8}, colors[i], 4)
		}
		p.RichText(bx+pad+14, ry, 0, ui.Span{Text: row[0], Size: 11.5, Color: pal.muted})
		vw, _ := p.MeasureText(0, ui.Span{Text: row[1], Size: 11.5, Weight: 650, Features: "tnum"})
		p.RichText(bx+w-pad-vw, ry, 0, ui.Span{Text: row[1], Size: 11.5, Weight: 650, Color: pal.ink, Features: "tnum"})
	}
}

// classRows lists a Tokens value per class, for tooltips.
func classRows(t model.Tokens) ([][2]string, []ui.Color) {
	var rows [][2]string
	var cols []ui.Color
	for i := len(classes) - 1; i >= 0; i-- {
		k := classes[i]
		if v := k.get(t); v > 0 {
			rows = append(rows, [2]string{tr(k.key), fmtTokens(v)})
			cols = append(cols, k.color)
		}
	}
	return rows, cols
}

// tipCard is a chart tip: a glassy card with a title and label/value rows,
// built as an element so chart.Anchor can place it by the point it
// describes. The color column always takes its room, so the values line up
// whether or not a row has a dot.
func tipCard(c *ui.Context, pal palette, title string, rows [][2]string, colors []ui.Color) {
	bg := ui.RGBA(255, 255, 255, 0.94)
	if pal.dark {
		bg = ui.RGBA(36, 32, 50, 0.94)
	}
	ui.Column(c).Padding(10).Radius(10).Gap(4).Background(bg).Border(1, pal.faint).
		Shadow(0, 6, 18, 0, ui.RGBA(0, 0, 0, 0.18)).Children(func() {
		ui.Text(c, title).FontSize(12).FontWeight(700).TextColor(pal.ink).NoWrap()
		for i, row := range rows {
			col := ui.Transparent
			if i < len(colors) {
				col = colors[i]
			}
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Box(c).Size(8, 8).Radius(4).Background(col).Shrink(0)
				ui.Text(c, row[0]).FontSize(11.5).TextColor(pal.muted).NoWrap()
				ui.Spacer(c)
				ui.Text(c, row[1]).FontSize(11.5).FontWeight(650).TextColor(pal.ink).FontFeatures("tnum").NoWrap()
			})
		}
	})
}

// tickLabel draws one chart axis tick in the quiet tick type: the left axis
// shows token counts, the bottom axis the band's own label.
func tickLabel(c *ui.Context, pal palette, side chart.Side, t chart.Tick) {
	s := t.Label
	if side == chart.Left {
		s = fmtTokens(int64(t.Value))
	}
	ui.Text(c, s).FontSize(10.5).TextColor(pal.muted).FontFeatures("tnum").NoWrap()
}

// tipAlign tells chart.Anchor which way to grow a tip from the point at y
// of n, the i-th: by the first third of the points it grows right, by the
// last left, and between them centered, so it stays inside the chart, and
// it grows up from a high point and down from a low one.
func tipAlign(i, n int, y float32) (ui.Align, ui.Align) {
	h := ui.Center
	switch {
	case i*3 < n:
		h = ui.Start
	case i*3 >= n*2:
		h = ui.End
	}
	if y > 0.5 {
		return h, ui.End
	}
	return h, ui.Start
}

// stackedBars draws one bar per point, stacked by token class, on a band
// and a linear scale with a y axis, sparse date labels and a tip by the
// point the pointer or the keyboard chooses.
func stackedBars(c *ui.Context, pal palette, pts []query.Point, hourly bool, height float32, name string) ui.Element {
	el := ui.Box(c).FillWidth().Height(height)
	grow := entrance(el, 700*time.Millisecond)
	if len(pts) == 0 {
		return el
	}
	var mx float64
	for _, pt := range pts {
		mx = math.Max(mx, float64(pt.Tokens.Total()))
	}
	labels := make([]string, len(pts))
	every := int(math.Ceil(float64(len(pts)) / 8))
	for i, pt := range pts {
		if i%every == 0 || i == len(pts)-1 && len(pts) <= 8 {
			if hourly {
				labels[i] = pt.Day.Format("15h")
			} else {
				labels[i] = pt.Day.Format("1/2")
			}
		}
	}
	x := chart.NewBand(labels...).Padding(0.5, 0)
	y := chart.NewLinear(0, math.Max(mx, 1)).Nice(4)
	series := make([][]float64, len(classes))
	for ci, k := range classes {
		row := make([]float64, len(pts))
		for i, pt := range pts {
			row[i] = float64(k.get(pt.Tokens))
		}
		series[ci] = row
	}
	stacked := chart.Stack(series, chart.StackStandard)
	ticks := y.Ticks(4)
	xs := make([]float32, len(pts))
	for i := range pts {
		xs[i] = x.At(i)
	}
	axes := chart.Axes{
		Bottom: x.Ticks(),
		Label:  func(side chart.Side, t chart.Tick) { tickLabel(c, pal, side, t) },
	}
	// An all-zero span keeps the empty axis, as it drew no y labels before.
	if !hourly && mx > 0 {
		axes.Left = ticks
	}
	el.Children(func() {
		chart.Plot(c, axes, func(area ui.Element) {
			area.Label(name)
			sel := ui.Local(area, "chartSel", func() int { return -1 })
			if sel == nil {
				none := -1
				sel = &none
			}
			chart.Select(c, area, sel, xs, func(i int) string {
				return pts[i].Day.Format("2006-01-02 Mon") + " · " + fmtTokens(pts[i].Tokens.Total())
			})
			area.Draw(func(p *ui.Painter, r ui.Rect) {
				hi := *sel
				if hi >= len(pts) {
					hi = -1
				}
				if !hourly && mx > 0 {
					for _, t := range ticks {
						if t.Value <= 0 {
							continue
						}
						yy := chart.Y(r, t.At)
						p.Line(r.X, yy, r.X+r.W, yy, 1, pal.faint)
					}
				}
				p.Line(r.X, r.Y+r.H, r.X+r.W, r.Y+r.H, 1, pal.faint.Alpha(0.16))
				if hi >= 0 {
					sp := x.Band(hi)
					p.Fill(ui.Rect{X: r.X + sp.Lo*r.W, Y: r.Y, W: (sp.Hi - sp.Lo) * r.W, H: r.H}, pal.hover, 6)
				}
				for i, pt := range pts {
					total := float64(pt.Tokens.Total())
					if total <= 0 {
						continue
					}
					sp := x.Band(i)
					bw := max(min((sp.Hi-sp.Lo)*r.W, 22), 2)
					bx := r.X + sp.Mid()*r.W - bw/2
					top := chart.Y(r, y.At(total*float64(grow)))
					if r.Y+r.H-top < 2 {
						top = r.Y + r.H - 2
					}
					// Draw the whole bar rounded, clipping the stack inside it.
					rad := float32(math.Min(float64(bw)/2, 5))
					p.Clip(ui.Rect{X: bx, Y: top, W: bw, H: r.Y + r.H - top}, rad, func() {
						for ci, k := range classes {
							seg := stacked[ci][i]
							if seg.Hi <= seg.Lo {
								continue
							}
							y0 := chart.Y(r, y.At(seg.Lo*float64(grow)))
							y1 := chart.Y(r, y.At(seg.Hi*float64(grow)))
							col := k.color
							if hi >= 0 && hi != i {
								col = col.Alpha(0.45)
							}
							p.Fill(ui.Rect{X: bx, Y: y1, W: bw, H: y0 - y1 + 0.5}, col, 0)
						}
					})
				}
			})
			if hi := *sel; hi >= 0 && hi < len(pts) {
				pt := pts[hi]
				title := pt.Day.Format("2006-01-02 Mon")
				if hourly {
					title = pt.Day.Format("01-02 15:00")
				}
				rows, cols := classRows(pt.Tokens)
				rows = append([][2]string{{tr("tokens"), fmtTokens(pt.Tokens.Total())}, {tr("cost"), fmtCost(pt.CostUSD)}}, rows...)
				cols = append([]ui.Color{ui.Transparent, ui.Transparent}, cols...)
				yf := y.At(float64(pt.Tokens.Total())) * grow
				h, v := tipAlign(hi, len(pts), yf)
				chart.Anchor(c, x.At(hi), max(yf, 0.06), h, v, func() { tipCard(c, pal, title, rows, cols) })
			}
		}).FillWidth().Height(height)
	})
	return el
}

// heroWave draws the days as one smooth melody across the hero's foot: a
// line in the band's colors over five faint staff lines, rising in when the
// page opens, with the day the pointer or the keyboard chooses called out.
func heroWave(c *ui.Context, pal palette, pts []query.Point, height float32, name string) ui.Element {
	el := ui.Box(c).FillWidth().Height(height)
	grow := entrance(el, 1000*time.Millisecond)
	if len(pts) < 2 {
		return el
	}
	var mx float64
	for _, pt := range pts {
		mx = math.Max(mx, float64(pt.Tokens.Total()))
	}
	if mx == 0 {
		mx = 1
	}
	n := len(pts)
	// The line runs from edge to edge; days sit at even steps, inset a
	// little so the first and last dot stay inside the plot.
	const padF, spanF = float32(0.032), float32(0.936)
	vals := make([]float32, n)
	xs := make([]float32, n)
	for i, pt := range pts {
		vals[i] = float32(float64(pt.Tokens.Total()) / mx)
		xs[i] = padF + spanF*float32(i)/float32(n-1)
	}
	yf := func(v float32) float32 { return 0.05 + 0.78*v*grow }
	el.Label(name)
	sel := ui.Local(el, "chartSel", func() int { return -1 })
	if sel == nil {
		none := -1
		sel = &none
	}
	chart.Select(c, el, sel, xs, func(i int) string {
		return pts[i].Day.Format("2006-01-02 Mon") + " · " + fmtTokens(pts[i].Tokens.Total())
	})
	el.Draw(func(p *ui.Painter, r ui.Rect) {
		for i := 0; i < 5; i++ {
			y := r.Y + 14 + (r.H-18)*float32(i)/4
			p.Line(r.X, y, r.X+r.W, y, 1, pal.faint.Alpha(0.6))
		}
		// Flat lead-in and lead-out, so the line reaches both edges.
		lineX := make([]float32, 0, n+2)
		lineY := make([]float32, 0, n+2)
		lineX, lineY = append(lineX, 0), append(lineY, yf(vals[0]))
		for i := range vals {
			lineX, lineY = append(lineX, xs[i]), append(lineY, yf(vals[i]))
		}
		lineX, lineY = append(lineX, 1), append(lineY, yf(vals[n-1]))
		line := chart.Line(r, lineX, lineY, chart.CurveMonotone)
		area := chart.Area(r, lineX, nil, lineY, chart.CurveMonotone)
		p.FillPathGradient(area, ui.LinearGradient{From: Tomori.Alpha(0.16), To: Taki.Alpha(0.16), Angle: 90, Oklab: true})
		p.FillPathGradient(area, ui.LinearGradient{From: pal.base.Alpha(0), To: pal.base.Alpha(0.35), Angle: 180})
		p.StrokePathGradient(line, 2.2, ui.LinearGradient{From: Tomori, To: Anon, Angle: 90, Oklab: true})
		hi := n - 1
		if *sel >= 0 && *sel < n {
			hi = *sel
			p.Line(chart.X(r, xs[hi]), r.Y+8, chart.X(r, xs[hi]), r.Y+r.H, 1, pal.ink.Alpha(0.18))
		}
		x, y := chart.X(r, xs[hi]), chart.Y(r, yf(vals[hi]))
		softGlow(p, x, y, 14, Anon.Alpha(0.35))
		p.FillPath(new(ui.Path).Circle(x, y, 4.5), Anon)
		p.FillPath(new(ui.Path).Circle(x, y, 2), ui.RGB(255, 255, 255))
	})
	if hi := *sel; hi >= 0 && hi < n {
		pt := pts[hi]
		y := yf(vals[hi])
		h, v := tipAlign(hi, n, y)
		chart.Anchor(c, xs[hi], max(y, 0.06), h, v, func() {
			tipCard(c, pal, pt.Day.Format("2006-01-02 Mon"),
				[][2]string{{tr("tokens"), fmtTokens(pt.Tokens.Total())}, {tr("cost"), fmtCost(pt.CostUSD)}}, nil)
		})
	}
	return el
}

// miniBars draws compact bars with no axes (the tray's 24 hours), with a tip
// by the hour the pointer or the keyboard chooses.
func miniBars(c *ui.Context, pal palette, pts []query.Point, height float32) ui.Element {
	el := ui.Box(c).FillWidth().Height(height)
	grow := entrance(el, 600*time.Millisecond)
	n := len(pts)
	if n == 0 {
		return el
	}
	var mx float64
	for _, pt := range pts {
		mx = math.Max(mx, float64(pt.Tokens.Total()))
	}
	if mx == 0 {
		mx = 1
	}
	// The hours are evenly spaced, so each one's share of the width is its
	// selection: the same rule the pointer used to hit them by.
	xs := make([]float32, n)
	for i := range xs {
		xs[i] = (float32(i) + 0.5) / float32(n)
	}
	el.Label(tr("last24h"))
	sel := ui.Local(el, "chartSel", func() int { return -1 })
	if sel == nil {
		none := -1
		sel = &none
	}
	chart.Select(c, el, sel, xs, func(i int) string {
		return pts[i].Day.Format("15:00") + " · " + tr("tokens") + " " + fmtTokens(pts[i].Tokens.Total()) +
			" · " + tr("cost") + " " + fmtCost(pts[i].CostUSD)
	})
	el.Draw(func(p *ui.Painter, r ui.Rect) {
		plotH := r.H - 16
		slot := r.W / float32(n)
		bw := slot * 0.56
		hi := *sel
		if hi >= n {
			hi = -1
		}
		// A quiet hour is a dot on the floor; a busy one rises as a level
		// meter in the band's colors.
		for i, pt := range pts {
			cx := r.X + slot*(float32(i)+0.5)
			x := cx - bw/2
			h := float32(float64(pt.Tokens.Total())/mx) * plotH * grow
			if h <= 0 {
				p.FillPath(new(ui.Path).Circle(cx, r.Y+plotH-1.5, 1.5), pal.faint)
				continue
			}
			if h < bw {
				h = bw
			}
			g := ui.LinearGradient{From: Tomori, To: Taki, Angle: 180, Oklab: true}
			if i == hi {
				g = ui.LinearGradient{From: Anon, To: Soyo, Angle: 180, Oklab: true}
			} else if hi >= 0 {
				g = ui.LinearGradient{From: Tomori.Alpha(0.5), To: Taki.Alpha(0.5), Angle: 180, Oklab: true}
			}
			p.FillGradient(ui.Rect{X: x, Y: r.Y + plotH - h, W: bw, H: h}, g, bw/2)
		}
		for _, i := range []int{0, n / 2, n - 1} {
			lab := pts[i].Day.Format("15:00")
			w, _ := p.MeasureText(0, ui.Span{Text: lab, Size: 10})
			x := r.X + slot*(float32(i)+0.5) - w/2
			x = float32(math.Max(float64(r.X), math.Min(float64(x), float64(r.X+r.W-w))))
			p.RichText(x, r.Y+plotH+3, 0, ui.Span{Text: lab, Size: 10, Color: pal.muted, Features: "tnum"})
		}
	})
	if hi := *sel; hi >= 0 && hi < n {
		pt := pts[hi]
		plotH := max(height-16, 0)
		yf := float32(0)
		if height > 0 {
			yf = float32(float64(pt.Tokens.Total())/mx) * plotH * grow / height
		}
		h, v := tipAlign(hi, n, yf)
		el.Children(func() {
			chart.Anchor(c, xs[hi], max(yf, 0.06), h, v, func() {
				tipCard(c, pal, pt.Day.Format("15:00"),
					[][2]string{{tr("tokens"), fmtTokens(pt.Tokens.Total())}, {tr("cost"), fmtCost(pt.CostUSD)}}, nil)
			})
		})
	}
	return el
}

// sparkline draws a smooth line with a fading fill.
func sparkline(c *ui.Context, vals []float64, col ui.Color, height float32) ui.Element {
	return ui.Box(c).FillWidth().Height(height).Draw(func(p *ui.Painter, r ui.Rect) {
		n := len(vals)
		if n < 2 {
			return
		}
		mx := 0.0
		for _, v := range vals {
			mx = math.Max(mx, v)
		}
		if mx == 0 {
			mx = 1
		}
		xs := make([]float32, n)
		ys := make([]float32, n)
		for i, v := range vals {
			xs[i] = float32(i) / float32(n-1)
			ys[i] = 0.05 + 0.9*float32(v/mx)
		}
		line := chart.Line(r, xs, ys, chart.CurveMonotone)
		area := chart.Area(r, xs, nil, ys, chart.CurveMonotone)
		p.FillPathGradient(area, ui.LinearGradient{From: col.Alpha(0.35), To: col.Alpha(0), Angle: 180})
		p.StrokePath(line, 2, col)
		xl, yl := chart.X(r, xs[n-1]), chart.Y(r, ys[n-1])
		p.FillPath(new(ui.Path).Circle(xl, yl, 3.5), col)
		p.FillPath(new(ui.Path).Circle(xl, yl, 1.6), ui.RGB(255, 255, 255))
	})
}

// donutGap is the room the ring leaves between slices, in the chart
// package's padRadius units (see chart.Sector).
const donutGap = 0.025

// donutSlices maps the token classes that have something to show onto the
// slices chart.Pie hands back, together with the class index each slice
// draws. Classes without tokens are left out: chart.Pie spends its pad on
// every element it is given, so a zero class would take a gap out of the
// ring and shift the classes after it.
func donutSlices(t model.Tokens) ([]chart.Slice, []int) {
	vals := make([]float64, 0, len(classes))
	idx := make([]int, 0, len(classes))
	for i, k := range classes {
		if v := k.get(t); v > 0 {
			vals = append(vals, float64(v))
			idx = append(idx, i)
		}
	}
	return chart.Pie(vals, donutGap), idx
}

// donutHover returns the class the point falls in, or -1 for the hole, the
// outside of the ring and a ring with nothing to show.
func donutHover(t model.Tokens, x, y, inner, outer float32) int {
	slices, idx := donutSlices(t)
	i, ok := chart.SliceAt(slices, x, y, inner, outer)
	if !ok {
		return -1
	}
	return idx[i]
}

// donut draws the token-class mix with the total in the middle.
func donut(c *ui.Context, pal palette, t model.Tokens, size float32) ui.Element {
	el := ui.Box(c).Size(size, size).Shrink(0)
	hx, hy, hover := el.PointerPosition()
	sweep := entrance(el, 900*time.Millisecond)
	slices, idx := donutSlices(t)
	el.Draw(func(p *ui.Painter, r ui.Rect) {
		cx, cy := r.X+r.W/2, r.Y+r.H/2
		ro := r.W / 2
		ri := ro * 0.68
		total := float64(t.Total())
		p.FillPath(chart.Sector(cx, cy, ri, ro, chart.Slice{Start: 0, End: 2 * math.Pi}), pal.well)
		hi := -1
		if hover {
			hi = donutHover(t, hx-cx, hy-cy, ri, ro)
		}
		for n, s := range slices {
			outer := ro
			if class := idx[n]; class == hi {
				outer += 4
			}
			// The entrance sweep grows the ring clockwise from twelve
			// o'clock; the gap keeps its width rather than its angle.
			p.FillPath(chart.Sector(cx, cy, ri, outer, chart.Slice{
				Start: s.Start * float64(sweep),
				End:   s.End * float64(sweep),
				Pad:   donutGap,
			}), classes[idx[n]].color)
		}
		// Center label.
		big, small := fmtTokens(t.Total()), tr("tokens")
		if hi >= 0 {
			k := classes[hi]
			big, small = fmtPct(float64(k.get(t))/total), tr(k.key)
		}
		fig := figure(pal, big, size*0.2)
		w, h := p.MeasureText(0, fig...)
		p.RichText(cx-w/2, cy-h/2-6, 0, fig...)
		w2, _ := p.MeasureText(0, ui.Span{Text: small, Size: 11})
		p.RichText(cx-w2/2, cy+h/2-4, 0, ui.Span{Text: small, Size: 11, Color: pal.muted})
	})
	return el
}

// heatLevel maps a value to 0..4 against quartiles of the nonzero days.
func heatColor(pal palette, v, mx float64) ui.Color {
	if v <= 0 {
		return pal.well
	}
	f := math.Sqrt(v / mx)
	switch {
	case f < 0.25:
		return Tomori.Alpha(0.35)
	case f < 0.5:
		return Tomori.Mix(Taki, 0.4).Alpha(0.65)
	case f < 0.75:
		return Taki.Alpha(0.85)
	}
	return Anon
}

// heatmap draws one column per week (Monday on top), like a contribution graph.
func heatmap(c *ui.Context, pal palette, days []query.Point, height float32) ui.Element {
	el := ui.Box(c).FillWidth().Height(height)
	hx, hy, hover := el.PointerPosition()
	el.Draw(func(p *ui.Painter, r ui.Rect) {
		if len(days) == 0 {
			return
		}
		mx := 0.0
		for _, d := range days {
			mx = math.Max(mx, float64(d.Tokens.Total()))
		}
		top := float32(16)
		labW := float32(22)
		gap := float32(3)
		// Cells are sized by height; as many recent weeks as fit are shown.
		cell := (r.H-top)/7 - gap
		weeks := (len(days) + 6) / 7
		if fit := int((r.W - labW) / (cell + gap)); fit < weeks && fit > 0 {
			days = days[(weeks-fit)*7:]
			weeks = fit
		}
		if c := (r.W-labW)/float32(weeks) - gap; c < cell {
			cell = c
		}
		ox := r.X + labW + ((r.W-labW)-float32(weeks)*(cell+gap))/2
		var hit *query.Point
		var hitX, hitY float32
		lastMonth := -1
		lastLabX := float32(-1e9)
		for i := range days {
			d := &days[i]
			w, wd := i/7, i%7
			x := ox + float32(w)*(cell+gap)
			y := r.Y + top + float32(wd)*(cell+gap)
			if wd == 0 && int(d.Day.Month()) != lastMonth {
				lastMonth = int(d.Day.Month())
				// A month stub in the first column would collide with the next.
				if x-lastLabX >= 28 && (w > 0 || d.Day.Day() <= 17) {
					lastLabX = x
					lab := d.Day.Format("Jan")
					if Lang() == "zh" {
						lab = d.Day.Format("1月")
					}
					p.RichText(x, r.Y, 0, ui.Span{Text: lab, Size: 10, Color: pal.muted})
				}
			}
			cr := ui.Rect{X: x, Y: y, W: cell, H: cell}
			p.Fill(cr, heatColor(pal, float64(d.Tokens.Total()), mx), cell*0.28)
			if hover && hx+r.X >= x && hx+r.X < x+cell+gap && hy+r.Y >= y && hy+r.Y < y+cell+gap {
				hit, hitX, hitY = d, x+cell/2, y+cell/2
				p.Stroke(cr, pal.ink.Alpha(0.6), cell*0.28, 1.5)
			}
		}
		wdLabels := []string{"Mon", "", "Wed", "", "Fri", "", ""}
		if Lang() == "zh" {
			wdLabels = []string{"一", "", "三", "", "五", "", ""}
		}
		for i, l := range wdLabels {
			if l != "" {
				p.RichText(r.X, r.Y+top+float32(i)*(cell+gap)+cell/2-7, 0, ui.Span{Text: l, Size: 10, Color: pal.muted})
			}
		}
		if hit != nil {
			tooltipBox(p, pal, r, hitX, hitY, hit.Day.Format("2006-01-02 Mon"),
				[][2]string{{tr("tokens"), fmtTokens(hit.Tokens.Total())}, {tr("cost"), fmtCost(hit.CostUSD)}}, nil)
		}
	})
	return el
}

// heatLegend is the less…more scale under the heatmap.
func heatLegend(c *ui.Context, pal palette) {
	ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
		ui.Text(c, tr("less")).FontSize(10.5).TextColor(pal.muted)
		for _, f := range []float64{0, 0.04, 0.2, 0.5, 1} {
			ui.Box(c).Size(10, 10).Radius(3).Background(heatColor(pal, f, 1))
		}
		ui.Text(c, tr("more")).FontSize(10.5).TextColor(pal.muted)
	})
}

// rankBar draws a rounded progress track for a share in 0..1.
func rankBar(c *ui.Context, pal palette, frac float64, col ui.Color, height float32) ui.Element {
	el := ui.Box(c).FillWidth().Height(height)
	g := entrance(el, 600*time.Millisecond)
	return el.Draw(func(p *ui.Painter, r ui.Rect) {
		p.Fill(r, pal.well, r.H/2)
		w := r.W * g * float32(frac)
		if w > 0 && w < r.H {
			w = r.H
		}
		if w > 0 {
			p.FillGradient(ui.Rect{X: r.X, Y: r.Y, W: w, H: r.H}, ui.LinearGradient{From: col.Alpha(0.75), To: col, Angle: 90}, r.H/2)
		}
	})
}

// classBar is a single horizontal bar split by token class.
func classBar(c *ui.Context, pal palette, t model.Tokens, height float32) ui.Element {
	return ui.Box(c).FillWidth().Height(height).Draw(func(p *ui.Painter, r ui.Rect) {
		p.Fill(r, pal.well, r.H/2)
		total := float64(t.Total())
		if total == 0 {
			return
		}
		p.Clip(r, r.H/2, func() {
			x := r.X
			for _, k := range classes {
				w := float32(float64(k.get(t))/total) * r.W
				if w <= 0 {
					continue
				}
				p.Fill(ui.Rect{X: x, Y: r.Y, W: w + 0.5, H: r.H}, k.color, 0)
				x += w
			}
		})
	})
}

// timeline draws a session's requests as dots over time, sized by tokens,
// with a tip by the request the pointer or the keyboard chooses.
func timeline(c *ui.Context, pal palette, evs []query.AttributedEvent, models []string, height float32) ui.Element {
	el := ui.Box(c).FillWidth().Height(height)
	if len(evs) == 0 {
		return el
	}
	t0, t1 := evs[0].Timestamp, evs[len(evs)-1].Timestamp
	span := t1.Sub(t0).Seconds()
	if span <= 0 {
		span = 1
	}
	var mx float64
	for _, e := range evs {
		mx = math.Max(mx, float64(e.Tokens.Total()))
	}
	colOf := func(m string) ui.Color {
		for i, x := range models {
			if x == m {
				return bandAt(i)
			}
		}
		return pal.muted
	}
	// Each request's place across the plot, as the fraction chart.Select
	// reads from the pointer and chart.Anchor writes a tip back to.
	xs := make([]float32, len(evs))
	for i, e := range evs {
		xs[i] = float32(e.Timestamp.Sub(t0).Seconds() / span)
	}
	el.Children(func() {
		// The plot is inset in the element, so the selection area is exactly
		// the plot and a tip anchored in it lands on its request.
		hit := ui.Box(c).Absolute().Left(6).Top(6).Right(6).Bottom(26)
		hit.Label(tr("timeline"))
		sel := ui.Local(hit, "chartSel", func() int { return -1 })
		if sel == nil {
			none := -1
			sel = &none
		}
		chart.Select(c, hit, sel, xs, func(i int) string {
			e := evs[i]
			s := e.Timestamp.Local().Format("15:04:05") + " · " + e.Model
			if e.ResolvedProvider != "" {
				s += " · " + e.ResolvedProvider
			}
			return s + " · " + tr("tokens") + " " + fmtTokens(e.Tokens.Total()) + " · " + tr("cost") + " " + costOfEvent(e)
		})
		el.Draw(func(p *ui.Painter, r ui.Rect) {
			plot := ui.Rect{X: r.X + 6, Y: r.Y + 6, W: r.W - 12, H: r.H - 26}
			hi := *sel
			if hi >= len(evs) {
				hi = -1
			}
			// The pointer only calls out a request it is near, so a tip
			// never hangs over empty time; the keyboard is not gated, so
			// arrowing still names the request it selected.
			show := hi >= 0
			if show && plot.W > 0 {
				if px, _, over := hit.PointerPosition(); over && chart.Nearest(xs, px/plot.W) == hi {
					show = math.Abs(float64(xs[hi]*plot.W-px)) < 24
				}
			}
			for i := 0; i <= 2; i++ {
				y := plot.Y + plot.H*float32(i)/2
				p.Line(plot.X, y, plot.X+plot.W, y, 1, pal.faint)
			}
			// Cumulative line.
			cum := new(ui.Path)
			var acc, all float64
			for _, e := range evs {
				all += float64(e.Tokens.Total())
			}
			for i, e := range evs {
				acc += float64(e.Tokens.Total())
				x := plot.X + plot.W*float32(e.Timestamp.Sub(t0).Seconds()/span)
				y := plot.Y + plot.H*(1-float32(acc/all))
				if i == 0 {
					cum.MoveTo(x, y)
				} else {
					cum.LineTo(x, y)
				}
			}
			// The running total as a soft wash behind the requests.
			area := new(ui.Path)
			acc = 0
			for i, e := range evs {
				acc += float64(e.Tokens.Total())
				x := plot.X + plot.W*float32(e.Timestamp.Sub(t0).Seconds()/span)
				y := plot.Y + plot.H*(1-float32(acc/all))
				if i == 0 {
					area.MoveTo(x, plot.Y+plot.H).LineTo(x, y)
				} else {
					area.LineTo(x, y)
				}
			}
			area.LineTo(plot.X+plot.W, plot.Y+plot.H).Close()
			p.FillPathGradient(area, ui.LinearGradient{From: Tomori.Alpha(0.10), To: Tomori.Alpha(0), Angle: 180})
			p.StrokePath(cum, 1.25, Tomori.Alpha(0.45))
			// Each request is a stem from the floor with a ringed head, like a
			// note on a staff.
			for i, e := range evs {
				x := plot.X + plot.W*float32(e.Timestamp.Sub(t0).Seconds()/span)
				v := float64(e.Tokens.Total())
				y := plot.Y + plot.H*(1-float32(v/mx))
				rad := float32(2 + 3.2*math.Sqrt(v/mx))
				col := colOf(e.Model)
				if show && i != hi {
					col = col.Alpha(0.5)
				}
				p.Line(x, plot.Y+plot.H, x, y+rad, 1, col.Alpha(0.35))
				if show && i == hi {
					p.FillPath(new(ui.Path).Circle(x, y, rad+5), col.Alpha(0.18))
				}
				p.FillPath(new(ui.Path).Circle(x, y, rad+1.5), pal.raised)
				p.FillPath(new(ui.Path).Circle(x, y, rad), col)
			}
			for _, lab := range []struct {
				t time.Time
				a float32
			}{{t0, 0}, {t1, 1}} {
				s := lab.t.Local().Format("15:04")
				w, _ := p.MeasureText(0, ui.Span{Text: s, Size: 10.5})
				p.RichText(plot.X+(plot.W-w)*lab.a, plot.Y+plot.H+6, 0, ui.Span{Text: s, Size: 10.5, Color: pal.muted, Features: "tnum"})
			}
		})
		if hi := *sel; hi >= 0 && hi < len(evs) {
			e := evs[hi]
			// A tip stays finite when no request has tokens; the dots keep
			// their own arithmetic.
			yf := float32(1)
			if mx > 0 {
				yf = 1 - float32(float64(e.Tokens.Total())/mx)
			}
			h, v := tipAlign(hi, len(evs), yf)
			rows, cols := classRows(e.Tokens)
			prov := e.ResolvedProvider
			if l := attribLabel(e.Attrib); l != "" {
				prov += " · " + l
			}
			rows = append([][2]string{{prov, e.Model}, {tr("cost"), costOfEvent(e)}}, rows...)
			cols = append([]ui.Color{colOf(e.Model), ui.Transparent}, cols...)
			hit.Children(func() {
				chart.Anchor(c, xs[hi], max(yf, 0.06), h, v, func() {
					tipCard(c, pal, e.Timestamp.Local().Format("15:04:05"), rows, cols)
				})
			})
		}
	})
	return el
}
