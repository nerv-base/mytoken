package gui

import (
	"fmt"
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
	card(c, pal).Children(func() {
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

const cell, cellGap = 13, 3

// heatmapCard is a year of daily cost, a week per column, with streaks.
func (a *App) heatmapCard(c *ui.Context, pal palette) {
	rk := &a.rk
	card(c, pal).Children(func() {
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
		// The weeks share the card's width; cells keep a fixed height.
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
			ui.Row(c).FillWidth().Gap(cellGap).Children(func() {
				for w := range weeks {
					ui.Column(c.Key(w)).Grow(1).Basis(0).Gap(cellGap).Children(func() {
						for d := range 7 {
							i := w*7 + d
							if i >= len(rk.year) {
								ui.Box(c.Key(d)).FillWidth().Height(cell)
								continue
							}
							p := rk.year[i]
							ui.Box(c.Key(d)).FillWidth().Height(cell).Radius(3).Background(ramp[levels[i]]).
								Tooltip(fmtDateYear(p.Day) + "  " + fmtUSD(p.CostUSD) + "  " + fmtTokens(shown(p.Tokens)))
						}
					})
				}
			})
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
