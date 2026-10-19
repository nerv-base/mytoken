package gui

import (
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/chart"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

func (a *App) overviewPage(c *ui.Context, pal palette) {
	ov := &a.ov
	pageHeader(c, pal, tr("overview"), func() {
		caption(c, pal, tr("costNote"))
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
		a.todayCard(c, pal).Grow(1.35).Basis(0)
		a.monthCard(c, pal).Grow(1).Basis(0)
	})
	if len(ov.quotas) > 0 {
		a.quotaCard(c, pal)
	}
	a.dailyCard(c, pal)
	k := a.chartRange
	ui.Row(c).Gap(16).AlignItems(ui.Stretch).Children(func() {
		a.toolShare(c, pal, ov.byTool[k], ov.totals[k].CostUSD).Grow(1).Basis(0)
		a.modelShare(c, pal, ov.byModel[k], ov.totals[k].CostUSD).Grow(1).Basis(0)
		a.tokensCard(c, pal, ov.totals[k]).Grow(1).Basis(0)
	})
}

func (a *App) todayCard(c *ui.Context, pal palette) ui.Element {
	ov := &a.ov
	return card(c, pal).Gap(10).Children(func() {
		caption(c, pal, tr("todaySpend")+" · "+tr("apiEquivalent"))
		ui.Row(c).Gap(14).AlignItems(ui.End).Children(func() {
			figure(c, pal, "today", fmtUSD(ov.today.CostUSD), 46)
			delta(c, pal, ov.today.CostUSD, ov.yesterday.CostUSD, tr("vsYesterday")).MarginY(8)
		})
		ui.Row(c).Gap(22).Children(func() {
			stat(c, pal, tr("tokens"), fmtTokens(shown(ov.today.Tokens)))
			stat(c, pal, tr("requests"), fmtInt(ov.today.Requests))
			stat(c, pal, tr("sessionsN"), fmtInt(ov.today.Sessions))
			stat(c, pal, tr("hitRate"), fmtPct(ov.today.Tokens.HitRate()))
		})
		if ov.today.Unpriced > 0 {
			caption(c, pal, trf("unpricedN", ov.today.Unpriced)).FontSize(11)
		}
	})
}

func (a *App) monthCard(c *ui.Context, pal palette) ui.Element {
	ov := &a.ov
	now := a.now()
	return card(c, pal).Gap(10).Children(func() {
		caption(c, pal, fmtMonth(now)+" · "+tr("apiEquivalent"))
		ui.Row(c).Gap(14).AlignItems(ui.End).Children(func() {
			figure(c, pal, "month", fmtUSD(ov.month.CostUSD), 34)
			delta(c, pal, ov.month.CostUSD, ov.lastMonth.CostUSD, tr("vsLastMonth")).MarginY(5)
		})
		// The month's progress: spent so far against the forecast.
		days := float64(monthStart(now).AddDate(0, 1, -1).Day())
		elapsed := (float64(now.Day()-1) + float64(now.Hour())/24) / days
		ui.Column(c).Gap(6).Children(func() {
			ui.Row(c).Children(func() {
				caption(c, pal, tr("forecast"))
				ui.Spacer(c)
				ui.Text(c, fmtUSD(ov.forecast)).FontSize(13).FontWeight(600).TextColor(pal.ink).FontFeatures("tnum")
			})
			frac := 0.0
			if ov.forecast > 0 {
				frac = ov.month.CostUSD / ov.forecast
			}
			ui.Box(c).FillWidth().Height(6).Radius(3).Background(pal.well).Children(func() {
				ui.Box(c.Key("spent")).HeightPercent(100).WidthPercent(float32(min(frac, 1) * 100)).Radius(3).Background(pal.accent).Transition(barMove)
				// A tick where the month's days are.
				ui.Box(c).Absolute().LeftPercent(float32(elapsed*100)).Top(-3).Size(2, 12).Radius(1).Background(pal.ink.Alpha(0.5))
			})
			caption(c, pal, trf("monthDay", now.Day(), int(days))).FontSize(11)
		})
	})
}

// delta compares a value with an earlier one.
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
	ui.Column(c).Gap(2).Children(func() {
		ui.Text(c, value).FontSize(15).FontWeight(650).TextColor(pal.ink).FontFeatures("tnum").NoWrap()
		caption(c, pal, label).FontSize(11)
	})
}

// quotaCard shows every plan quota as rings.
func (a *App) quotaCard(c *ui.Context, pal palette) {
	now := a.now()
	card(c, pal).Children(func() {
		cardTitle(c, pal, tr("planQuota"), nil)
		ui.Row(c).Gap(28).Wrap().Children(func() {
			for _, q := range a.ov.quotas {
				ui.Column(c.Key("q-" + q.ID)).Gap(12).Children(func() {
					ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
						ui.Text(c, q.Name).FontSize(13).FontWeight(650).TextColor(pal.ink)
						if q.Plan != "" {
							pill(c, q.Plan, pal.accent, pal.accent.Alpha(0.12))
						}
						src := tr("viaAPI")
						if q.Source == "local-log" {
							src = tr("viaLog")
						}
						caption(c, pal, src+" · "+fmtAgo(now, q.FetchedAt)).FontSize(11)
					})
					ui.Row(c).Gap(22).Children(func() {
						for i, w := range q.Windows {
							quotaWindow(c.Key(i), pal, q.ID+"-"+w.Name, w, now)
						}
					})
					if q.Err != "" {
						caption(c, pal, q.Err).FontSize(11).TextColor(pal.warn).MaxWidth(320)
					}
				})
			}
		})
	})
}

func quotaWindow(c *ui.Context, pal palette, key string, w QuotaWindow, now time.Time) {
	frac := w.UsedPercent / 100
	ui.Column(c).Gap(6).AlignItems(ui.Center).Children(func() {
		ring(c, pal, key, frac, 76, 8).Children(func() {
			ui.Column(c).Fill().Center().Children(func() {
				ui.Text(c, fmt.Sprintf("%.0f%%", w.UsedPercent)).FontSize(16).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum")
			})
		})
		ui.Text(c, windowName(w.Name)).FontSize(12).FontWeight(600).TextColor(pal.ink)
		if !w.ResetsAt.IsZero() {
			caption(c, pal, fmtIn(now, w.ResetsAt)).FontSize(11)
		}
	})
}

func windowName(n string) string {
	switch n {
	case "5h":
		return tr("win5h")
	case "week":
		return tr("winWeek")
	case "month":
		return tr("winMonth")
	case "day":
		return tr("winDay")
	}
	return n
}

// dailyCard is the stacked daily cost chart. Its bars are keyed by their
// distance from today, so that switching the range morphs them in place.
func (a *App) dailyCard(c *ui.Context, pal palette) {
	ov := &a.ov
	n := chartDays[a.chartRange]
	days := ov.days
	if len(days) > n {
		days = days[len(days)-n:]
	}
	card(c, pal).Children(func() {
		cardTitle(c, pal, tr("dailySpend"), func() {
			ui.Segmented(c, &a.chartRange, tr("range7"), tr("range30"), tr("range90")).Label(tr("range"))
		})
		ui.Row(c).Gap(14).Wrap().Children(func() {
			for _, h := range ov.tools {
				ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
					swatch(c, harnessColor(h))
					caption(c, pal, h.DisplayName())
				})
			}
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				swatch(c, otherColor)
				caption(c, pal, tr("other"))
			})
		})
		labels := make([]string, len(days))
		hi := 0.0
		for i, d := range days {
			labels[i] = d.day.Format("01-02")
			hi = max(hi, d.cost)
		}
		if hi == 0 {
			hi = 1
		}
		x := chart.NewBand(labels...).Padding(0.28, 0.04)
		y := chart.NewLinear(0, hi).Nice(4)
		y.Format = fmtUSDShort
		yt := y.Ticks(4)
		step := map[int]int{7: 1, 30: 5, 90: 15}[n]
		var xt []chart.Tick
		for i, t := range x.Ticks() {
			if (len(days)-1-i)%step == 0 {
				d := days[i].day
				t.Label = fmtDate(d)
				if n == 7 {
					t.Label = weekday(d)
				}
				xt = append(xt, t)
			}
		}
		chart.Plot(c, chart.Axes{Left: yt, Bottom: xt, Label: func(_ chart.Side, t chart.Tick) {
			ui.Text(c, t.Label).NoWrap().FontSize(11).TextColor(pal.muted).FontFeatures("tnum")
		}}, func(area ui.Element) {
			line := pal.line
			area.Draw(func(p *ui.Painter, r ui.Rect) {
				for _, t := range yt {
					p.Line(r.X, chart.Y(r, t.At), r.X+r.W, chart.Y(r, t.At), 1, line)
				}
			})
			for i, d := range days {
				back := len(days) - 1 - i
				lo := 0.0
				seg := func(k int, v float64, col ui.Color) {
					if v <= 0 {
						return
					}
					chart.Rect(c.Key(fmt.Sprintf("s%d-%d", back, k)), x.Band(i), y.Span(lo, lo+v)).Background(col).PassThrough().Transition(barMove)
					lo += v
				}
				for k, v := range d.tools {
					seg(k, v, harnessColor(ov.tools[k]))
				}
				seg(len(d.tools), d.other, otherColor)
				// The column under the pointer explains the day.
				hit := chart.Rect(c.Key(fmt.Sprintf("hit%d", back)), x.Band(i), y.Span(0, y.Hi)).Tooltip(dayTip(d, ov.tools))
				if hit.Hovered() {
					hit.Background(pal.hover)
				}
			}
		}).Label(tr("dailySpend")).Height(220).GapX(8).GapY(6)
	})
}

func dayTip(d dayCost, tools []model.Harness) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s", fmtDateYear(d.day), fmtUSD(d.cost))
	for k, v := range d.tools {
		if v > 0 {
			fmt.Fprintf(&b, "\n%s  %s", tools[k].DisplayName(), fmtUSD(v))
		}
	}
	if d.other > 0.005 {
		fmt.Fprintf(&b, "\n%s  %s", tr("other"), fmtUSD(d.other))
	}
	return b.String()
}

const shareRows = 6

func (a *App) toolShare(c *ui.Context, pal palette, rows []query.Bucket, total float64) ui.Element {
	return card(c, pal).Children(func() {
		cardTitle(c, pal, tr("byTool"), nil)
		if len(rows) == 0 {
			caption(c, pal, tr("noUsage"))
		}
		for i, b := range rows {
			if i == shareRows {
				break
			}
			h := model.Harness(b.Key)
			shareRow(c.Key(b.Key), pal, func() { toolBadge(c, h, 18) }, h.DisplayName(), fmtUSD(b.CostUSD), fmtPct(frac(b.CostUSD, total)), frac(b.CostUSD, total), harnessColor(h))
		}
	})
}

func (a *App) modelShare(c *ui.Context, pal palette, rows []query.Bucket, total float64) ui.Element {
	return card(c, pal).Children(func() {
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
			shareRow(c.Key(b.Key), pal, nil, label, value, fmtPct(frac(b.CostUSD, total)), frac(b.CostUSD, total), seriesAt(i))
		}
	})
}

// tokensCard splits the range's tokens by class and shows the hit rate.
func (a *App) tokensCard(c *ui.Context, pal palette, t query.Totals) ui.Element {
	return card(c, pal).Children(func() {
		cardTitle(c, pal, tr("tokens"), func() {
			ui.Text(c, fmtTokens(shown(t.Tokens))).FontSize(13).FontWeight(600).TextColor(pal.ink).FontFeatures("tnum")
		})
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			ring(c, pal.withUsage(pal.accent), "hit", t.Tokens.HitRate(), 44, 6)
			ui.Column(c).Gap(1).Children(func() {
				ui.Text(c, fmtPct(t.Tokens.HitRate())).FontSize(18).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum")
				caption(c, pal, tr("hitRateLong")).FontSize(11)
			})
		})
		tk := t.Tokens
		classes := []struct {
			key string
			v   int64
			col ui.Color
		}{
			{"input", tk.Input, ui.Hex("#5AA9D6")},
			{"output", tk.Output, ui.Hex("#FF7A92")},
			{"cacheRead", tk.CacheRead, ui.Hex("#4FC27A")},
			{"cacheWrite", tk.CacheWrite, ui.Hex("#F0B83C")},
			{"reasoning", tk.Reasoning, ui.Hex("#7B6FD0")},
		}
		total := float64(tk.Total())
		ui.Row(c).FillWidth().Height(8).Radius(4).Clip().Background(pal.well).Children(func() {
			for _, k := range classes {
				if k.v > 0 {
					ui.Box(c.Key(k.key)).HeightPercent(100).WidthPercent(float32(float64(k.v) / total * 100)).Background(k.col).Transition(barMove)
				}
			}
		})
		ui.Column(c).Gap(6).Children(func() {
			for _, k := range classes {
				if k.v == 0 && k.key == "reasoning" {
					continue
				}
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					swatch(c, k.col)
					caption(c, pal, tr(k.key))
					ui.Spacer(c)
					ui.Text(c, fmtTokens(k.v)).FontSize(12).TextColor(pal.ink).FontFeatures("tnum")
				})
			}
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
