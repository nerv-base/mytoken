package gui

import (
	"fmt"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/chart"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

// projectsPage lists projects by cost beside the chosen one.
func (a *App) projectsPage(c *ui.Context, pal palette) {
	ui.Row(c).Grow(1).Basis(0).FillWidth().MinHeight(0).Children(func() {
		ui.Column(c).Width(sessionListWidth).FillHeight().Shrink(0).Padding(0, 14, 14, 20).Gap(10).Children(func() {
			pageHeader(c, pal, tr("projects"), nil)
			if rangePicker(c, &a.rangeSel) {
				a.rangeChanged()
			}
			a.projectList(c, pal)
		})
		ui.Box(c).Width(1).FillHeight().Background(pal.line).Shrink(0)
		ui.Column(c).Grow(1).Basis(0).FillHeight().MinWidth(0).Children(func() {
			a.projectPane(c, pal)
		})
	})
}

func projectLabel(b query.Bucket) string {
	if b.Key == "" {
		return tr("noProject")
	}
	if n := projectName(b.Key); n != "" {
		return n
	}
	return b.Key
}

func (a *App) projectList(c *ui.Context, pal palette) {
	pj := &a.pj
	errorLine(c, pal, pj.err)
	if !pj.ready {
		emptyState(c, pal, tr("loading"), "")
		return
	}
	if len(pj.rows) == 0 {
		emptyState(c, pal, tr("noUsage"), "")
		return
	}
	caption(c, pal, trf("projectsCount", len(pj.rows))).FontSize(11)
	sel := -1
	for i, b := range pj.rows {
		if b.Key == a.pjSel && a.pjSel != "" {
			sel = i
		}
	}
	a.pjList.Selected = &sel
	a.pjList.Key = func(i int) any { return pj.rows[i].Key }
	a.pjList.Label = func(i int) string { return projectLabel(pj.rows[i]) }
	l := ui.List(c, &a.pjList, len(pj.rows), func(i int) {
		b := pj.rows[i]
		ui.Column(c).Gap(6).Padding(9, 10).Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, icFolder).FontSize(14).TextColor(pal.muted)
				ui.Text(c, projectLabel(b)).FontSize(13).FontWeight(560).TextColor(pal.ink).SingleLine().Ellipsis("…").Grow(1).Basis(0).MinWidth(0)
				ui.Text(c, costText(b.CostUSD, b.Unpriced, b.Requests)).FontSize(13).FontWeight(600).TextColor(pal.ink).FontFeatures("tnum").NoWrap()
			})
			bar(c, pal, frac(b.CostUSD, pj.maxCost), pal.accent).Height(4)
			caption(c, pal, trf("projectSub", fmtInt(b.Sessions), fmtTokens(shown(b.Tokens)))).FontSize(11)
		})
	}).Grow(1).Basis(0).FillWidth().Label(tr("projects"))
	if l.Changed() && sel >= 0 && sel < len(pj.rows) {
		a.pjSel = pj.rows[sel].Key
		a.pjd = projectDetail{key: a.pjSel}
		a.loadProjectDetail()
	}
}

func (a *App) projectPane(c *ui.Context, pal palette) {
	d := &a.pjd
	if a.pjSel == "" {
		emptyState(c, pal, tr("pickProject"), "")
		return
	}
	ui.Scroll(c.Key("pj-" + a.pjSel)).Grow(1).Basis(0).FillWidth().Children(func() {
		ui.Column(c).Padding(0, 28, 28, 24).Gap(16).MaxWidth(1000).FillWidth().Children(func() {
			ui.Column(c).Gap(3).Children(func() {
				name := projectName(a.pjSel)
				if a.pjSel == "" || name == "" {
					name = tr("noProject")
				}
				ui.Text(c, name).Font(display).FontSize(20).FontWeight(700).TextColor(pal.ink).SingleLine().Ellipsis("…")
				caption(c, pal, a.pjSel).SingleLine().Ellipsis("…")
			})
			errorLine(c, pal, d.err)
			if !d.ready {
				emptyState(c, pal, tr("loading"), "")
				return
			}
			t := d.total
			ui.Row(c).Gap(12).Children(func() {
				kpi(c, pal, tr("apiEquivalent"), fmtUSD(t.CostUSD))
				kpi(c, pal, tr("tokens"), fmtTokens(shown(t.Tokens)))
				kpi(c, pal, tr("sessionsN"), fmtInt(t.Sessions))
				kpi(c, pal, tr("hitRate"), fmtPct(t.Tokens.HitRate()))
			})
			if len(d.days) > 1 {
				a.projectChart(c, pal)
			}
			ui.Row(c).Gap(16).AlignItems(ui.Stretch).Children(func() {
				card(c, pal).Grow(1).Basis(0).Children(func() {
					cardTitle(c, pal, tr("byModel"), nil)
					for i, b := range d.byModel {
						if i == shareRows {
							break
						}
						shareRow(c.Key(b.Key), pal, nil, b.Key, costText(b.CostUSD, b.Unpriced, b.Requests), fmtPct(frac(b.CostUSD, t.CostUSD)), frac(b.CostUSD, t.CostUSD), seriesAt(i))
					}
				})
				card(c, pal).Grow(1).Basis(0).Children(func() {
					cardTitle(c, pal, tr("byTool"), nil)
					for i, b := range d.byTool {
						if i == shareRows {
							break
						}
						h := model.Harness(b.Key)
						shareRow(c.Key(b.Key), pal, func() { toolBadge(c, h, 18) }, h.DisplayName(), fmtUSD(b.CostUSD), fmtPct(frac(b.CostUSD, t.CostUSD)), frac(b.CostUSD, t.CostUSD), harnessColor(h))
					}
				})
			})
			if len(d.sessions) > 0 {
				card(c, pal).Gap(4).Children(func() {
					cardTitle(c, pal, tr("topSessions"), nil)
					now := a.now()
					for i, r := range d.sessions {
						if i == 12 {
							break
						}
						row := ui.Row(c.Key(sessionKey(r.Harness, r.SessionID))).Gap(10).Padding(7, 8).Radius(8).AlignItems(ui.Center).Cursor(ui.CursorPointer).Children(func() {
							toolBadge(c, r.Harness, 20)
							ui.Text(c, sessionTitle(r)).FontSize(13).TextColor(pal.ink).SingleLine().Ellipsis("…").Grow(1).Basis(0).MinWidth(0)
							caption(c, pal, fmtAgo(now, r.UpdatedAt)).NoWrap()
							ui.Text(c, costText(r.CostUSD, r.Unpriced, r.Requests)).FontSize(13).FontWeight(600).TextColor(pal.ink).FontFeatures("tnum").Width(80).TextAlign(ui.End)
						})
						if row.Hovered() {
							row.Background(pal.hover)
						}
						if row.Clicked() {
							a.showSession(r)
						}
					}
				})
			}
		})
	})
}

// showSession opens a session on the Sessions page.
func (a *App) showSession(r query.SessionRow) {
	a.ssQuery = ""
	a.ssSel = -1
	a.page = "sessions"
	a.openSession(r)
	a.loadSessions()
}

func (a *App) projectChart(c *ui.Context, pal palette) {
	d := &a.pjd
	days := d.days
	labels := make([]string, len(days))
	hi := 0.0
	for i, p := range days {
		labels[i] = p.Day.Format("2006-01-02")
		hi = max(hi, p.CostUSD)
	}
	if hi == 0 {
		hi = 1
	}
	x := chart.NewBand(labels...).Padding(0.25, 0.04)
	y := chart.NewLinear(0, hi).Nice(3)
	y.Format = fmtUSDShort
	yt := y.Ticks(3)
	step := max(len(days)/6, 1)
	var xt []chart.Tick
	for i, t := range x.Ticks() {
		if (len(days)-1-i)%step == 0 {
			t.Label = fmtDate(days[i].Day)
			xt = append(xt, t)
		}
	}
	line, accent := pal.line, pal.accent
	card(c, pal).Children(func() {
		cardTitle(c, pal, tr("dailySpend"), nil)
		chart.Plot(c, chart.Axes{Left: yt, Bottom: xt, Label: func(_ chart.Side, t chart.Tick) {
			ui.Text(c, t.Label).NoWrap().FontSize(11).TextColor(pal.muted).FontFeatures("tnum")
		}}, func(area ui.Element) {
			area.Draw(func(p *ui.Painter, r ui.Rect) {
				for _, t := range yt {
					p.Line(r.X, chart.Y(r, t.At), r.X+r.W, chart.Y(r, t.At), 1, line)
				}
			})
			for i, p := range days {
				if p.CostUSD <= 0 {
					continue
				}
				chart.Rect(c.Key(fmt.Sprint("d", len(days)-1-i)), x.Band(i), y.Span(0, p.CostUSD)).Radius(2).Background(accent).
					Transition(barMove).Tooltip(fmtDateYear(p.Day) + "  " + fmtUSD(p.CostUSD))
			}
		}).Label(tr("dailySpend")).Height(150).GapX(8).GapY(6)
	})
}
