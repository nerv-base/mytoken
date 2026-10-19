package gui

import (
	"fmt"
	"math"
	"strings"

	"github.com/egoist/mygo/ui"
)

// The tray panel's size.
const (
	TrayWidth  = 340
	TrayHeight = 500
)

// PanelView is the tray panel: today's spend, the plan quotas and the
// sessions running now. Its window is translucent; the panel draws on the
// desktop's frosted material.
func (a *App) PanelView(c *ui.Context) {
	pal := applyTheme(c, a.appearance())
	ov := &a.ov
	now := a.now()
	c.Root().Background(ui.Color{})
	// A tint over the window's frosted material keeps text readable where
	// the material is missing or too light.
	ui.Column(c).Fill().Radius(12).Background(pal.bg.Alpha(0.82)).Padding(14, 14, 12, 14).Gap(12).Children(func() {
		a.panelHeader(c, pal)
		if !ov.ready {
			emptyState(c, pal, tr("loading"), "")
			return
		}
		if len(ov.quotas) > 0 {
			panelBox(c, pal).Gap(9).Children(func() {
				for _, q := range ov.quotas {
					for i, w := range q.Windows {
						if i == 2 {
							break
						}
						frac := w.UsedPercent / 100
						ui.Column(c.Key(q.ID + w.Name)).Gap(4).Children(func() {
							ui.Row(c).Gap(6).Children(func() {
								ui.Text(c, q.Name+" · "+windowName(w.Name)).FontSize(12).TextColor(pal.ink).NoWrap()
								ui.Spacer(c)
								ui.Text(c, fmt.Sprintf("%.0f%%", w.UsedPercent)).FontSize(12).FontWeight(650).TextColor(pal.usageColor(frac)).FontFeatures("tnum")
								if !w.ResetsAt.IsZero() {
									caption(c, pal, fmtIn(now, w.ResetsAt)).FontSize(11).NoWrap()
								}
							})
							bar(c, pal, frac, pal.usageColor(frac)).Height(5)
						})
					}
				}
			})
		}
		panelBox(c, pal).Gap(8).Grow(1).Basis(0).MinHeight(0).Children(func() {
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, tr("activeNow")).FontSize(12).FontWeight(650).TextColor(pal.ink)
				ui.Spacer(c)
				if len(ov.active) > 0 {
					caption(c, pal, tr("rateEstimated")).FontSize(10.5)
				}
			})
			if len(ov.active) == 0 {
				ui.Column(c).Grow(1).Center().Children(func() {
					caption(c, pal, tr("noActive"))
				})
				return
			}
			for _, s := range ov.active {
				a.panelSession(c.Key(sessionKey(s.row.Harness, s.row.SessionID)), pal, s)
			}
		})
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			caption(c, pal, trf("monthSoFar", fmtUSD(ov.month.CostUSD))).FontSize(11.5)
			ui.Spacer(c)
			if a.d.OpenMain != nil && ui.Button(c, tr("openMain")).Clicked() {
				if a.d.HidePanel != nil {
					a.d.HidePanel()
				}
				a.d.OpenMain()
			}
			if a.d.Quit != nil {
				q := ui.Button(c, tr("quit")).Tooltip(tr("quit"))
				if q.Clicked() {
					a.d.Quit()
				}
			}
		})
	})
}

// panelBox is a translucent group in the panel.
func panelBox(c *ui.Context, pal palette) ui.Element {
	bg := ui.RGBA(255, 255, 255, 0.55)
	line := ui.RGBA(0, 0, 0, 0.06)
	if pal.dark {
		bg, line = ui.RGBA(255, 255, 255, 0.06), ui.RGBA(255, 255, 255, 0.08)
	}
	return ui.Column(c).Padding(12).Radius(12).Background(bg).Border(1, line)
}

// panelHeader is today's spend over a band of light that flows faster the
// faster tokens are being spent, and rests when nothing is running.
func (a *App) panelHeader(c *ui.Context, pal palette) {
	ov := &a.ov
	var rate float64
	for _, s := range ov.active {
		rate += s.rate
	}
	accent := pal.accent
	box := panelBox(c, pal).Gap(4).Clip()
	reduce := c.Preferences().ReduceMotion
	box.Draw(func(p *ui.Painter, r ui.Rect) {
		// The band: a soft accent glow along the top edge.
		phase := float32(0.5)
		if rate > 0 && !reduce {
			// One sweep every 6 s at rest speed, down to 1.2 s at 200 tok/s.
			period := 6 - 4.8*math.Min(rate/200, 1)
			t := float64(p.Now().UnixMilli()%int64(period*1000)) / (period * 1000)
			phase = float32(t)
			p.AnimationFrame()
		}
		w := r.W * 0.6
		x := r.X - w + (r.W+w)*phase
		band := ui.Rect{X: x, Y: r.Y, W: w, H: 3}
		p.FillGradient(band, ui.LinearGradient{Angle: 90, Stops: []ui.GradientStop{
			{Color: accent.Alpha(0), At: 0}, {Color: accent.Alpha(0.9), At: 0.5}, {Color: accent.Alpha(0), At: 1},
		}}, 1.5)
		glow := ui.Rect{X: x, Y: r.Y, W: w, H: r.H}
		p.FillGradient(glow, ui.LinearGradient{Angle: 90, Stops: []ui.GradientStop{
			{Color: accent.Alpha(0), At: 0}, {Color: accent.Alpha(0.09), At: 0.5}, {Color: accent.Alpha(0), At: 1},
		}}, 0)
	})
	box.Children(func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			logo(c, pal, 16)
			caption(c, pal, tr("todaySpend")+" · "+tr("apiEquivalent")).FontSize(11.5)
		})
		ui.Row(c).Gap(10).AlignItems(ui.End).Children(func() {
			figure(c, pal, "tray-today", fmtUSD(ov.today.CostUSD), 32)
			delta(c, pal, ov.today.CostUSD, ov.yesterday.CostUSD, tr("vsYesterday")).MarginY(5)
		})
		ui.Row(c).Gap(14).Children(func() {
			caption(c, pal, trf("tokensN", fmtTokens(shown(ov.today.Tokens)))).FontSize(11.5)
			caption(c, pal, tr("hitRate")+" "+fmtPct(ov.today.Tokens.HitRate())).FontSize(11.5)
			if rate > 0 {
				caption(c, pal, fmtRate(rate)).FontSize(11.5).TextColor(accent)
			}
		})
	})
}

func (a *App) panelSession(c *ui.Context, pal palette, s activeSession) {
	ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
		toolBadge(c, s.row.Harness, 24)
		ui.Column(c).Gap(2).Grow(1).Basis(0).MinWidth(0).Children(func() {
			ui.Text(c, sessionTitle(s.row)).FontSize(12.5).FontWeight(600).TextColor(pal.ink).NoWrap().Ellipsis("…")
			sub := s.model
			if p := projectName(s.row.Project); p != "" {
				sub = p + " · " + sub
			}
			caption(c, pal, sub).FontSize(10.5).NoWrap().Ellipsis("…")
		})
		ui.Column(c).Gap(2).AlignItems(ui.End).Shrink(0).Children(func() {
			rate := "—"
			if s.rate > 0 {
				rate = fmtRate(s.rate)
			}
			ui.Text(c, rate).FontSize(12).FontWeight(650).TextColor(pal.ink).FontFeatures("tnum").NoWrap()
			caption(c, pal, trf("perHour", fmtUSD(s.perHour))+" · "+fmtPct(s.hit)).FontSize(10.5).FontFeatures("tnum").NoWrap()
		})
	})
}

// TrayTitle is the menu bar text the user chose.
func (a *App) TrayTitle() string {
	ov := &a.ov
	if !ov.ready {
		return ""
	}
	var parts []string
	for _, item := range trayItems(a.pref(PrefTrayTitle)) {
		switch item {
		case "cost":
			parts = append(parts, fmtUSD(ov.today.CostUSD))
		case "rate":
			var rate float64
			for _, s := range ov.active {
				rate += s.rate
			}
			if rate > 0 {
				parts = append(parts, fmtRate(rate))
			}
		case "hit":
			if ov.today.Tokens.Total() > 0 {
				parts = append(parts, fmtPct(ov.today.Tokens.HitRate()))
			}
		case "quota":
			best := -1.0
			for _, q := range ov.quotas {
				for _, w := range q.Windows {
					best = max(best, w.UsedPercent)
				}
			}
			if best >= 0 {
				parts = append(parts, fmt.Sprintf("%.0f%%", best))
			}
		}
	}
	return strings.Join(parts, " · ")
}

// trayTitleItems are what the menu bar text can show, in display order.
var trayTitleItems = []string{"cost", "rate", "hit", "quota"}

// trayItems parses the stored menu bar selection: a comma list, "" for the
// default (today's cost), "none" for an icon only.
func trayItems(v string) []string {
	switch v {
	case "":
		return []string{"cost"}
	case "none":
		return nil
	}
	var out []string
	for _, it := range trayTitleItems {
		for _, p := range strings.Split(v, ",") {
			if p == it {
				out = append(out, it)
			}
		}
	}
	return out
}

// updateTray sets the menu-bar text only when it changed: the status item
// re-lays itself out on every set, and refreshes mostly change nothing.
func (a *App) updateTray() {
	if a.d.SetTrayTitle == nil {
		return
	}
	t := a.TrayTitle()
	if a.trayTitleSet && t == a.trayTitle {
		return
	}
	a.trayTitle, a.trayTitleSet = t, true
	a.d.SetTrayTitle(t)
}
