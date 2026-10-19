package gui

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

var rankDims = []string{"dimTool", "dimModel", "dimProvider"}

func (a *App) rankingPage(c *ui.Context, pal palette) {
	rk := &a.rk
	pageHeader(c, pal, tr("ranking"), func() {
		if rangePicker(c, &a.rangeSel) {
			a.rangeChanged()
		}
	})
	errorLine(c, pal, rk.err)
	if !rk.ready {
		emptyState(c, pal, tr("loading"), "")
		return
	}
	a.heatmapCard(c, pal)
	a.rcard(c, pal, "ranking1", 0, false).Children(func() {
		cardTitle(c, pal, tr("costRanking"), func() {
			ui.Segmented(c, &a.rkDim, tr(rankDims[0]), tr(rankDims[1]), tr(rankDims[2])).Label(tr("rankBy"))
		})
		rows := rk.dims[a.rkDim]
		if len(rows) == 0 {
			caption(c, pal, tr("noUsage"))
			return
		}
		top := rows[0].CostUSD
		for i, b := range rows {
			a.rankRow(c.Key(fmt.Sprint(a.rkDim, b.Key)), pal, i, b, top, rk.totalCost)
		}
	})
}

func (a *App) rankRow(c *ui.Context, pal palette, i int, b query.Bucket, top, total float64) {
	label := b.Label
	if label == "" {
		label = b.Key
	}
	if label == "" {
		label = tr("unknown")
	}
	col := seriesAt(i)
	var lead func()
	if a.rkDim == 0 {
		h := model.Harness(b.Key)
		label = h.DisplayName()
		col = harnessColor(h)
		lead = func() { toolBadge(c, h, 22) }
	}
	ui.Row(c).Gap(12).AlignItems(ui.Center).Padding(4, 0).Children(func() {
		rankNo(c, pal, i+1)
		ui.Column(c).Gap(6).Grow(1).Basis(0).MinWidth(0).Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				if lead != nil {
					lead()
				}
				ui.Text(c, label).FontSize(13.5).FontWeight(600).TextColor(pal.ink).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
				ui.Spacer(c)
				caption(c, pal, trf("rankSub", fmtTokens(shown(b.Tokens)), fmtInt(b.Requests))).NoWrap().FontFeatures("tnum")
			})
			bar(c, pal, frac(b.CostUSD, top), col).Height(6)
		})
		ui.Column(c).Gap(2).AlignItems(ui.End).Width(96).Shrink(0).Children(func() {
			ui.Text(c, costText(b.CostUSD, b.Unpriced, b.Requests)).FontSize(14).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum").NoWrap()
			caption(c, pal, fmtPct(frac(b.CostUSD, total))).FontSize(11).FontFeatures("tnum")
		})
	})
}

// rankNo is a rank number; the first three stand out.
func rankNo(c *ui.Context, pal palette, n int) {
	bg, fg := pal.well, pal.muted
	switch n {
	case 1:
		bg, fg = ui.Hex("#F0B83C").Alpha(0.22), ui.Hex("#B7801A")
	case 2:
		bg, fg = ui.Hex("#9AA3B2").Alpha(0.22), ui.Hex("#687284")
	case 3:
		bg, fg = ui.Hex("#D08A5B").Alpha(0.22), ui.Hex("#A0623A")
	}
	if pal.dark && n <= 3 {
		fg = fg.Mix(ui.RGB(255, 255, 255), 0.35)
	}
	ui.Column(c).Size(26, 26).Radius(8).Background(bg).Center().Shrink(0).Children(func() {
		ui.Text(c, fmt.Sprint(n)).FontSize(12).FontWeight(700).TextColor(fg).FontFeatures("tnum")
	})
}

const cellGap = 3

// heatmapCard is a year of daily cost, a week per column, with streaks.
func (a *App) heatmapCard(c *ui.Context, pal palette) {
	rk := &a.rk
	a.rcard(c, pal, "ranking2", 1, false).Children(func() {
		cardTitle(c, pal, tr("yearActivity"), nil)
		ui.Row(c).Gap(28).Wrap().Children(func() {
			streakStat(c, pal, tr("streakNow"), trf("daysN", rk.streak))
			streakStat(c, pal, tr("streakBest"), trf("daysN", rk.best))
			streakStat(c, pal, tr("activeDays"), trf("daysN", rk.activeDays))
			if rk.topDay.CostUSD > 0 {
				streakStat(c, pal, tr("topDay"), fmtUSD(rk.topDay.CostUSD)+" · "+fmtDate(rk.topDay.Day))
			}
		})
		levels := heatLevels(rk.year)
		ramp := heatRamp(pal)
		weeks := (len(rk.year) + 6) / 7
		// The weeks share the card's width; cells stay square.
		ui.Column(c).FillWidth().Gap(4).Children(func() {
			ui.Row(c).FillWidth().Height(14).Gap(cellGap).Children(func() {
				last := time.Month(0)
				for w := range weeks {
					label := ""
					if i := w * 7; i < len(rk.year) {
						if m := rk.year[i].Day.Month(); m != last {
							last = m
							if w < weeks-2 {
								label = fmtMonthShort(rk.year[i].Day)
							}
						}
					}
					ui.Box(c.Key(w)).Grow(1).Basis(0).Height(14).Children(func() {
						if label != "" {
							caption(c, pal, label).FontSize(10).Absolute().Left(0).Top(0).NoWrap()
						}
					})
				}
			})
			a.heatGrid(c, pal, rk.year, levels, ramp, weeks)
		})
		ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
			ui.Spacer(c)
			caption(c, pal, tr("less")).FontSize(10)
			for _, col := range ramp {
				ui.Box(c).Size(10, 10).Radius(2.5).Background(col)
			}
			caption(c, pal, tr("more")).FontSize(10)
		})
	})
}

// heatGrid paints the year's cells. They come in as a wave from the oldest
// week to today, then breathe: a slow swell of light runs across the
// active days and today's cell pulses. Hovering a day names it.
func (a *App) heatGrid(c *ui.Context, pal palette, year []query.Point, levels []int, ramp [5]ui.Color, weeks int) {
	box := ui.Box(c.Key("heat-grid")).FillWidth().AspectRatio(float32(weeks) / 7)
	rv := a.mo.cur
	target := float32(0)
	if rv.seen {
		target = 1
	}
	in := box.AnimateTo("in", target, delayed(ui.Animation{Duration: 1600 * time.Millisecond, Ease: easeOut}, rv.delay+100*time.Millisecond))
	still := c.Preferences().ReduceMotion
	if still {
		in = 1
	}
	px, py, over := box.PointerPosition()
	accent, ink := pal.accent, pal.ink
	dark := pal.dark
	box.Draw(func(p *ui.Painter, r ui.Rect) {
		if weeks == 0 {
			return
		}
		size := min((r.W-cellGap*float32(weeks-1))/float32(weeks), (r.H-cellGap*6)/7)
		slot := size + cellGap
		t := float64(p.Now().UnixMilli()%600000) / 1000
		breathing := !still && in >= 1
		hover := -1
		if over {
			w, d := int(px/slot), int(py/slot)
			if w >= 0 && w < weeks && d >= 0 && d < 7 && w*7+d < len(year) {
				hover = w*7 + d
			}
		}
		last := len(year) - 1
		for i, pt := range year {
			w, d := i/7, i%7
			// Entrance: the oldest week first, a column at a time.
			k := clamp01((float64(in)*1.5 - float64(w)/float64(weeks)) * 2)
			if k <= 0 {
				continue
			}
			k = outQuint(k)
			s := size * float32(0.55+0.45*k)
			x := r.X + float32(w)*slot + (size-s)/2
			y := r.Y + float32(d)*slot + (size-s)/2
			col := ramp[levels[i]]
			if breathing && levels[i] > 0 {
				// A swell of light travels from left to right every
				// seven seconds, stronger on the busier days.
				ph := t/7 - (float64(w)+float64(d)*0.35)/float64(weeks)*1.3
				swell := math.Pow(0.5+0.5*math.Sin(2*math.Pi*ph), 3)
				col = col.Mix(accent, float32(swell*0.10*float64(levels[i])))
				if dark {
					col = col.Mix(ink, float32(swell*0.05*float64(levels[i])))
				}
			}
			p.Fill(ui.Rect{X: x, Y: y, W: s, H: s}, col.Alpha(float32(k)), min(3, s/3))
			if i == last && pt.CostUSD > 0 && breathing {
				// Today breathes.
				b := float32(0.5 + 0.5*math.Sin(2*math.Pi*t/2.6))
				g := 1.5 + 2.5*b
				p.Stroke(ui.Rect{X: x - g, Y: y - g, W: s + 2*g, H: s + 2*g}, accent.Alpha(0.15+0.35*(1-b)), 3+g, 1.5)
			}
			if i == hover {
				p.Stroke(ui.Rect{X: x - 1.5, Y: y - 1.5, W: s + 3, H: s + 3}, ink.Alpha(0.7), 4, 1.5)
			}
		}
		if hover >= 0 {
			pt := year[hover]
			hx := r.X + float32(hover/7)*slot + size/2
			lines := []string{fmtDateYear(pt.Day), fmtUSD(pt.CostUSD) + "  ·  " + fmtTokens(shown(pt.Tokens)) + " Token"}
			ty := r.Y + float32(hover%7)*slot - 4
			if ty > r.Y+r.H/2 {
				ty -= 48
			} else {
				ty += size + 8
			}
			tooltip(p, pal, hx, ty, r, lines, 1)
		}
		if in < 1 || breathing {
			p.After(33 * time.Millisecond)
		}
	})
}

func streakStat(c *ui.Context, pal palette, label, value string) {
	ui.Column(c).Gap(2).Children(func() {
		ui.Text(c, value).FontSize(16).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum").NoWrap()
		caption(c, pal, label).FontSize(11)
	})
}

// heatLevels buckets each day's cost into 0 (none) .. 4 by the quartiles
// of the days with any cost.
func heatLevels(days []query.Point) []int {
	var costs []float64
	for _, p := range days {
		if p.CostUSD > 0 {
			costs = append(costs, p.CostUSD)
		}
	}
	sort.Float64s(costs)
	q := func(f float64) float64 {
		if len(costs) == 0 {
			return 0
		}
		return costs[min(int(f*float64(len(costs))), len(costs)-1)]
	}
	q1, q2, q3 := q(0.25), q(0.5), q(0.75)
	out := make([]int, len(days))
	for i, p := range days {
		switch v := p.CostUSD; {
		case v <= 0 && p.Tokens.Total() == 0:
			out[i] = 0
		case v <= q1:
			out[i] = 1
		case v <= q2:
			out[i] = 2
		case v <= q3:
			out[i] = 3
		default:
			out[i] = 4
		}
	}
	return out
}

func heatRamp(pal palette) [5]ui.Color {
	a := pal.accent
	base := pal.well
	return [5]ui.Color{base, base.Mix(a, 0.28), base.Mix(a, 0.5), base.Mix(a, 0.75), a}
}
