package gui

import (
	"fmt"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/chart"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

const sessionListWidth = 380

// sessionsPage is a searchable list of sessions beside the chosen one.
func (a *App) sessionsPage(c *ui.Context, pal palette) {
	ui.Row(c).Grow(1).Basis(0).FillWidth().MinHeight(0).Children(func() {
		ui.Column(c).Width(sessionListWidth).FillHeight().Shrink(0).Padding(0, 14, 14, 20).Gap(10).Children(func() {
			pageHeader(c, pal, tr("sessions"), nil)
			if rangePicker(c, &a.rangeSel) {
				a.rangeChanged()
			}
			if ui.SearchField(c, &a.ssQuery).Placeholder(tr("searchSessions")).FillWidth().Changed() {
				a.filterSessions()
				a.ssSel = -1
			}
			a.sessionFilters(c)
			a.sessionList(c, pal)
		})
		ui.Box(c).Width(1).FillHeight().Background(pal.line).Shrink(0)
		ui.Column(c).Grow(1).Basis(0).FillHeight().MinWidth(0).Children(func() {
			a.sessionPane(c, pal)
		})
	})
}

// sessionFilters is the order switch and the tool picker under the search.
func (a *App) sessionFilters(c *ui.Context) {
	ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
		if ui.Segmented(c, &a.ssSort, tr("sortRecent"), tr("sortCost")).Changed() {
			a.ssSel = -1
			a.loadSessions()
		}
		ui.Spacer(c)
		keys := []string{""}
		for _, h := range a.ss.tools {
			keys = append(keys, string(h))
		}
		name := func(k string) string {
			if k == "" {
				return tr("allTools")
			}
			return model.Harness(k).DisplayName()
		}
		if k, ok := choice(c, "", keys, name, a.ssTool); ok {
			a.ssTool = k
			a.filterSessions()
			a.ssSel = -1
		}
	})
}

// rangeChanged reloads what the shared range governs.
func (a *App) rangeChanged() {
	a.ssSel = -1
	a.pjSel = ""
	a.reloadPage()
}

func (a *App) sessionList(c *ui.Context, pal palette) {
	s := &a.ss
	if s.err != "" {
		errorLine(c, pal, s.err)
	}
	if !s.ready {
		emptyState(c, pal, tr("loading"), "")
		return
	}
	caption(c, pal, a.sessionCount()).FontSize(11)
	if len(s.shown) == 0 {
		emptyState(c, pal, tr("noSessions"), "")
		return
	}
	now := a.now()
	a.ssList.Selected = &a.ssSel
	a.ssList.Key = func(i int) any {
		r := s.rows[s.shown[i]]
		return sessionKey(r.Harness, r.SessionID)
	}
	a.ssList.Label = func(i int) string { return sessionTitle(s.rows[s.shown[i]]) }
	l := ui.List(c, &a.ssList, len(s.shown), func(i int) {
		r := s.rows[s.shown[i]]
		ui.Row(c).Gap(10).Padding(9, 10).AlignItems(ui.Center).Children(func() {
			toolBadge(c, r.Harness, 26)
			ui.Column(c).Gap(3).Grow(1).Basis(0).MinWidth(0).Children(func() {
				ui.Text(c, sessionTitle(r)).FontSize(13).FontWeight(560).TextColor(pal.ink).SingleLine().Ellipsis("…")
				live := isLive(now, r)
				sub := fmtAgo(now, r.UpdatedAt)
				if live {
					sub = tr("running")
				}
				if p := projectName(r.Project); p != "" {
					sub = p + " · " + sub
				}
				ui.Row(c).Gap(5).AlignItems(ui.Center).MinWidth(0).Children(func() {
					if live {
						liveDot(c, pal)
					}
					caption(c, pal, sub).FontSize(11).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
				})
			})
			ui.Column(c).Gap(3).AlignItems(ui.End).Shrink(0).Children(func() {
				ui.Text(c, costText(r.CostUSD, r.Unpriced, r.Requests)).FontSize(13).FontWeight(600).TextColor(pal.ink).FontFeatures("tnum").NoWrap()
				caption(c, pal, fmtTokens(shown(r.Tokens))).FontSize(11).FontFeatures("tnum").NoWrap()
			})
		})
	}).Grow(1).Basis(0).FillWidth().Label(tr("sessions"))
	if l.Changed() && a.ssSel >= 0 && a.ssSel < len(s.shown) {
		a.openSession(s.rows[s.shown[a.ssSel]])
	}
}

func (a *App) sessionCount() string {
	s := &a.ss
	if len(s.shown) != len(s.rows) {
		return trf("sessionsMatch", len(s.shown), len(s.rows))
	}
	if s.total > len(s.rows) {
		return trf("sessionsNewest", len(s.rows), s.total)
	}
	return trn("sessionsCount", s.total)
}

// sessionTitle is a session's title, or its tool and id without one.
func sessionTitle(r query.SessionRow) string {
	if r.Title != "" {
		return r.Title
	}
	id := r.SessionID
	if len(id) > 8 {
		id = id[:8]
	}
	return r.Harness.DisplayName() + " · " + id
}

// projectName is the last element of a project path.
func projectName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			if i == len(p)-1 {
				p = p[:i]
				continue
			}
			return p[i+1:]
		}
	}
	return p
}

// costText is a cost, saying when none of it could be priced.
func costText(cost float64, unpriced, requests int64) string {
	if requests > 0 && unpriced == requests {
		return tr("unpriced")
	}
	return fmtUSD(cost)
}

func (a *App) sessionPane(c *ui.Context, pal palette) {
	d := &a.sd
	if d.key == "" {
		emptyState(c, pal, tr("pickSession"), tr("pickSessionHint"))
		return
	}
	r := d.row
	ui.Scroll(c.Key("sd-" + d.key)).Grow(1).Basis(0).FillWidth().Children(func() {
		ui.Column(c).Padding(0, 28, 28, 24).Gap(16).MaxWidth(1240).FillWidth().AlignSelf(ui.Center).Children(func() {
			ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
				toolBadge(c, r.Harness, 34)
				ui.Column(c).Gap(3).Grow(1).Basis(0).MinWidth(0).Children(func() {
					ui.Text(c, sessionTitle(r)).Font(display).FontSize(19).FontWeight(700).TextColor(pal.ink).SingleLine().Ellipsis("…")
					meta := r.Harness.DisplayName()
					if r.Project != "" {
						meta += " · " + r.Project
					}
					caption(c, pal, meta).SingleLine().Ellipsis("…")
				})
				if isLive(a.now(), r) {
					livePill(c, pal, trf("runningAgo", fmtAgo(a.now(), r.UpdatedAt)))
				}
			})
			errorLine(c, pal, d.err)
			ui.Row(c).Gap(12).Children(func() {
				kpi(c, pal, tr("apiEquivalent"), costText(r.CostUSD, r.Unpriced, r.Requests))
				kpi(c, pal, tr("tokens"), fmtTokens(shown(r.Tokens)))
				kpi(c, pal, tr("requests"), fmtInt(r.Requests))
				kpi(c, pal, tr("hitRate"), fmtPct(r.Tokens.HitRate()))
				kpi(c, pal, tr("duration"), fmtDuration(d.span))
			})
			end := fmtDateTime(r.UpdatedAt)
			if isLive(a.now(), r) {
				end = tr("now")
			}
			caption(c, pal, fmtDateTime(r.StartedAt)+" → "+end).FontSize(11)
			if !d.ready {
				emptyState(c, pal, tr("loading"), "")
				return
			}
			if len(d.events) > 1 {
				a.sessionChart(c, pal)
			}
			if len(r.Breakdown) > 0 {
				a.rcard(c, pal, "sessions1", 0, false).Children(func() {
					cardTitle(c, pal, tr("byModel"), nil)
					for i, b := range r.Breakdown {
						label := b.Model
						if b.Provider != "" {
							label += "  ·  " + b.Provider
						}
						shareRow(c.Key(i), pal, nil, label, costText(b.CostUSD, b.Unpriced, b.Requests), fmtTokens(shown(b.Tokens)), frac(b.CostUSD, r.CostUSD), seriesAt(i))
					}
				})
			}
			if len(d.children) > 0 {
				a.rcard(c, pal, "sessions2", 1, false).Children(func() {
					cardTitle(c, pal, trf("subagentsN", len(d.children)), nil)
					for i, k := range d.children {
						ui.Row(c.Key(i)).Gap(10).AlignItems(ui.Center).Children(func() {
							ui.Text(c, sessionTitle(k)).FontSize(13).TextColor(pal.ink).SingleLine().Ellipsis("…").Grow(1).Basis(0).MinWidth(0)
							caption(c, pal, fmtTokens(shown(k.Tokens)))
							ui.Text(c, costText(k.CostUSD, k.Unpriced, k.Requests)).FontSize(13).FontWeight(600).TextColor(pal.ink).FontFeatures("tnum")
						})
					}
				})
			}
			a.requestTable(c, pal)
		})
	})
}

func kpi(c *ui.Context, pal palette, label, value string) {
	ui.Column(c).Gap(4).Padding(12, 14).Radius(12).Background(pal.card).Border(1, pal.line).Grow(1).Basis(0).MinWidth(0).Children(func() {
		caption(c, pal, label).FontSize(11).NoWrap()
		ui.Text(c, value).FontSize(18).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum").SingleLine().Ellipsis("…")
	})
}

// sessionChart is the session's running cost over its requests.
func (a *App) sessionChart(c *ui.Context, pal palette) {
	d := &a.sd
	n := len(d.events)
	xs := make([]float64, n)
	t0 := d.events[0].Timestamp
	for i, e := range d.events {
		xs[i] = e.Timestamp.Sub(t0).Seconds()
	}
	hi := d.cum[n-1]
	if hi <= 0 {
		hi = 1
	}
	span := max(xs[n-1], 1)
	x := chart.NewLinear(0, span)
	y := chart.NewLinear(0, hi).Nice(4)
	y.Format = fmtUSDShort
	yt := y.Ticks(4)
	var xt []chart.Tick
	for _, f := range []float64{0, 0.5, 1} {
		xt = append(xt, chart.Tick{At: x.At(span * f), Label: d.events[0].Timestamp.Add(time.Duration(span*f) * time.Second).Format("15:04")})
	}
	accent, line := pal.accent, pal.line
	a.rcard(c, pal, "sessions3", 2, false).Children(func() {
		cardTitle(c, pal, tr("costOverTime"), nil)
		chart.Plot(c, chart.Axes{Left: yt, Bottom: xt, Label: func(_ chart.Side, t chart.Tick) {
			ui.Text(c, t.Label).NoWrap().FontSize(11).TextColor(pal.muted).FontFeatures("tnum")
		}}, func(area ui.Element) {
			area.Draw(func(p *ui.Painter, r ui.Rect) {
				for _, t := range yt {
					p.Line(r.X, chart.Y(r, t.At), r.X+r.W, chart.Y(r, t.At), 1, line)
				}
				// A step line: the cost rises at each request.
				path := &ui.Path{}
				fill := &ui.Path{}
				px := func(i int) float32 { return chart.X(r, x.At(xs[i])) }
				py := func(v float64) float32 { return chart.Y(r, y.At(v)) }
				path.MoveTo(px(0), py(0))
				fill.MoveTo(px(0), py(0))
				prev := 0.0
				for i := range xs {
					path.LineTo(px(i), py(prev))
					path.LineTo(px(i), py(d.cum[i]))
					fill.LineTo(px(i), py(prev))
					fill.LineTo(px(i), py(d.cum[i]))
					prev = d.cum[i]
				}
				fill.LineTo(px(n-1), py(0))
				fill.Close()
				p.FillPathGradient(fill, ui.LinearGradient{From: accent.Alpha(0.22), To: accent.Alpha(0.02), Angle: 180})
				p.StrokePath(path, 2, accent)
			})
		}).Label(tr("costOverTime")).Height(160).GapX(8).GapY(6)
	})
}

// requestTable lists the session's requests, newest first.
func (a *App) requestTable(c *ui.Context, pal palette) {
	d := &a.sd
	n := len(d.events)
	if n == 0 {
		return
	}
	a.rcard(c, pal, "sessions4", 3, false).Gap(8).Children(func() {
		cardTitle(c, pal, trn("requestsN", n), nil)
		head := func() {
			ui.Row(c).Gap(10).Padding(0, 4).Children(func() {
				col := func(s string, w float32) {
					caption(c, pal, s).FontSize(11).Width(w).TextAlign(ui.End).NoWrap()
				}
				caption(c, pal, tr("time")).FontSize(11).Width(64).NoWrap()
				caption(c, pal, tr("model")).FontSize(11).Grow(1).Basis(0).NoWrap()
				col(tr("input"), 70)
				col(tr("output"), 70)
				col(tr("cacheRead"), 80)
				col(tr("cost"), 70)
			})
		}
		head()
		// The table holds at most a screenful; long sessions scroll in it.
		ui.List(c, &a.sdEvents, n, func(i int) {
			e := d.events[n-1-i]
			ui.Row(c).Gap(10).Padding(5, 4).AlignItems(ui.Center).Children(func() {
				num := func(s string, w float32) {
					ui.Text(c, s).FontSize(12).TextColor(pal.ink).FontFeatures("tnum").Width(w).TextAlign(ui.End).NoWrap()
				}
				caption(c, pal, e.Timestamp.Format("15:04:05")).FontFeatures("tnum").Width(64).NoWrap()
				ui.Text(c, e.Model).FontSize(12).TextColor(pal.ink).Grow(1).Basis(0).MinWidth(0).SingleLine().Ellipsis("…")
				num(fmtTokens(e.Tokens.Input), 70)
				num(fmtTokens(e.Tokens.Output+e.Tokens.Reasoning), 70)
				num(fmtTokens(e.Tokens.CacheRead), 80)
				cost := fmtUSD(e.Cost)
				if !e.Priced {
					cost = "—"
				}
				num(cost, 70)
			})
		}).Height(float32(min(n, 12))*30 + 4).FillWidth().Label(tr("requests"))
	})
}

var _ = fmt.Sprint

// isLive tells whether a session made a request in the last minutes, as
// the overview's running sessions are picked.
func isLive(now time.Time, r query.SessionRow) bool {
	return now.Sub(r.UpdatedAt) <= activeWindow
}

// livePill is a green pill with a breathing dot: running.
func livePill(c *ui.Context, pal palette, text string) {
	ui.Row(c.Key("live-pill")).Gap(6).AlignItems(ui.Center).Padding(4, 10, 4, 8).Radius(12).Background(pal.good.Alpha(0.12)).Shrink(0).Children(func() {
		liveDot(c, pal)
		ui.Text(c, text).FontSize(12).FontWeight(600).TextColor(pal.good).NoWrap()
	})
}
