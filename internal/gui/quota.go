package gui

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
)

// Quotas come in two shapes. Plan windows (Claude's 5 hours, Codex's
// week, Cursor's month) have a cap and a reset: a ring shows how much is
// used, with a tick where the time is, so "used faster than time" is
// visible at a glance. Credit balances (WorkBuddy, Qoder) have no fixed
// cap — packs are topped up, gifted and expire — so a percent would lie:
// they show what is left, what the last two weeks used day by day, and how
// long the balance lasts at that rate.

// windowLabel names a window; credits with a cap are an allowance being
// used up, not a balance.
func windowLabel(w QuotaWindow) string {
	if !w.Balance && strings.EqualFold(w.Name, "credits") {
		return tr("creditsUsed")
	}
	return windowName(w.Name)
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
	switch strings.ToLower(n) {
	case "credits":
		return tr("balance")
	case "add-on credits":
		return tr("addOnCredits")
	case "dollar usage":
		return tr("dollarUsage")
	}
	return n
}

// usedFrac is how much of a capped window is used, from 0 to 1.
func usedFrac(w QuotaWindow) float64 {
	if w.UsedPercent == 0 && w.Limit > 0 {
		return w.Used / w.Limit
	}
	return w.UsedPercent / 100
}

// pace is how far through its window the time is, from 0 to 1; -1 when
// the window's length is unknown.
func pace(w QuotaWindow, now time.Time) float64 {
	if w.ResetsAt.IsZero() {
		return -1
	}
	var length time.Duration
	switch w.Name {
	case "5h":
		length = 5 * time.Hour
	case "day":
		length = 24 * time.Hour
	case "week":
		length = 7 * 24 * time.Hour
	case "month":
		length = w.ResetsAt.Sub(w.ResetsAt.AddDate(0, -1, 0))
	default:
		return -1
	}
	return clamp01(1 - float64(w.ResetsAt.Sub(now))/float64(length))
}

func ahead(w QuotaWindow, now time.Time) bool {
	p := pace(w, now)
	return p >= 0 && usedFrac(w) > p+0.08
}

// unitName is a unit as the user reads it.
func unitName(u string) string {
	switch u {
	case "credits":
		return tr("unitCredits")
	case "requests":
		return tr("unitRequests")
	}
	return u
}

// isMoney tells units shown with a currency sign and no unit after.
func isMoney(u string) bool { return u == "usd" || u == "cny" }

// fmtAmount formats an amount of a unit compactly.
func fmtAmount(v float64, unit string) string {
	switch unit {
	case "usd":
		return fmtUSD(v)
	case "cny":
		// Yuan as the account holds them, whatever currency is shown.
		return "¥" + strconv.FormatFloat(v, 'f', 2, 64)
	}
	if v >= 10000 {
		return fmtTokens(int64(v))
	}
	if v != math.Trunc(v) && v < 100 {
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	return fmtInt(int64(math.Round(v)))
}

// windowValue is "412 / 600 requests", or "" with only a percent.
func windowValue(w QuotaWindow) string {
	if w.Limit <= 0 {
		return ""
	}
	if isMoney(w.Unit) {
		return fmtAmount(w.Used, w.Unit) + " / " + fmtAmount(w.Limit, w.Unit)
	}
	return fmtAmount(w.Used, w.Unit) + " / " + fmtAmount(w.Limit, w.Unit) + " " + unitName(w.Unit)
}

// balanceOf is a quota's credit balance window, if it has one.
func balanceOf(q Quota) (QuotaWindow, bool) {
	for _, w := range q.Windows {
		if w.Balance {
			return w, true
		}
	}
	return QuotaWindow{}, false
}

// runway is the average daily use over the last week of known days and
// how many days the balance lasts at it; days < 0 when there is no use.
func runway(q Quota, w QuotaWindow) (perDay, days float64) {
	n, sum := 0, 0.0
	for i := len(q.Daily) - 1; i >= 0 && n < 7; i-- {
		if q.Daily[i].Known {
			sum += q.Daily[i].Used
			n++
		}
	}
	if n == 0 || sum <= 0 {
		return 0, -1
	}
	perDay = sum / float64(n)
	return perDay, w.Remaining / perDay
}

// urgency orders quotas: the nearest to running out first.
func urgency(q Quota, now time.Time) float64 {
	u := 0.0
	for _, w := range q.Windows {
		if w.Balance {
			_, days := runway(q, w)
			switch {
			case days < 0:
				u = max(u, 0.2)
			case days < 3:
				u = max(u, 0.95)
			case days < 7:
				u = max(u, 0.8)
			default:
				u = max(u, 0.4)
			}
			continue
		}
		f := usedFrac(w)
		if ahead(w, now) {
			f = max(f, 0.75)
		}
		u = max(u, f)
	}
	return u
}

// primary is the window a tile leads with: the tightest capped one.
func primary(q Quota) int {
	best, at := -1.0, 0
	for i, w := range q.Windows {
		if w.Balance {
			continue
		}
		if f := usedFrac(w); f > best {
			best, at = f, i
		}
	}
	return at
}

func (pal palette) meterColor(w QuotaWindow, now time.Time) ui.Color {
	f := usedFrac(w)
	if f >= 0.7 {
		return pal.usageColor(f)
	}
	if ahead(w, now) {
		return pal.warn
	}
	return pal.good
}

const quotaCols = 3

// quotaColsFor lays n accounts out without a lone tile on a row of three:
// four go two by two.
func quotaColsFor(n int) int {
	if n <= 2 || n == 4 {
		return 2
	}
	return quotaCols
}

func (a *App) quotaCard(c *ui.Context, pal palette) {
	now := a.now()
	qs := append([]Quota(nil), a.ov.quotas...)
	sort.SliceStable(qs, func(i, j int) bool { return urgency(qs[i], now) > urgency(qs[j], now) })
	low := 0
	for _, q := range qs {
		if urgency(q, now) >= 0.85 {
			low++
		}
	}
	shown := qs
	if !a.quotaAll && len(qs) > 2*quotaCols {
		shown = qs[:2*quotaCols]
	}
	el := a.rcard(c, pal, "quota", 3, false)
	rv := a.mo.cur
	el.Children(func() {
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			ui.Text(c, tr("planQuota")).FontSize(14).FontWeight(650).TextColor(pal.ink)
			ui.Spacer(c)
			ui.Text(c, trn("accountsN", len(qs))).FontSize(12.5).TextColor(pal.muted)
			if low > 0 {
				ui.Text(c, trf("nearlyOutN", low)).FontSize(12).FontWeight(650).TextColor(pal.bad).Padding(3, 9).Radius(10).Background(pal.bad.Alpha(0.12)).NoWrap()
			}
		})
		cols := quotaColsFor(len(shown))
		ui.Column(c).Gap(12).Children(func() {
			for r := 0; r*cols < len(shown); r++ {
				ui.Row(c.Key("qr" + itoa(r))).Gap(12).AlignItems(ui.Stretch).
					Transition(ui.ElementTransition{Animation: &snappy, Enter: &ui.Motion{Y: -8, Collapse: true}, Exit: &ui.Motion{Collapse: true}}).
					Children(func() {
						for k := r * cols; k < r*cols+cols; k++ {
							if k < len(shown) {
								a.mo.cur = rv
								a.quotaTile(c.Key("qt-"+shown[k].ID), pal, shown[k], k, now)
							} else {
								ui.Box(c).Grow(1).Basis(0)
							}
						}
					})
			}
		})
		a.mo.cur = rv
		if len(qs) > 2*quotaCols {
			label := trf("showAllN", len(qs))
			if a.quotaAll {
				label = tr("collapse")
			}
			more := ui.Row(c.Key("more")).Gap(6).AlignItems(ui.Center).Padding(2, 2).Cursor(ui.CursorPointer)
			press(more)
			col := pal.accent
			if more.Hovered() {
				col = pal.accent.Mix(pal.ink, 0.25)
			}
			more.Children(func() {
				ui.Text(c, label).FontSize(12.5).FontWeight(600).TextColor(more.AnimateColor("c", col, quick)).ContentTransition(ui.ContentTransition{})
				ic := ui.Box(c.Key("chev")).Center()
				ic.AnimatePose(ui.Pose{Rotate: map[bool]float32{false: 90, true: -90}[a.quotaAll]}, snappy)
				ic.Children(func() { ui.Icon(c, icChevron).FontSize(13).TextColor(pal.accent) })
			})
			if more.Clicked() {
				a.quotaAll = !a.quotaAll
			}
		}
	})
}

// quotaTile is one account.
func (a *App) quotaTile(c *ui.Context, pal palette, q Quota, order int, now time.Time) {
	rv := a.mo.cur
	tile := ui.Column(c).Grow(1).Basis(0).MinWidth(0).Padding(14, 16, 16, 16).Gap(12).Radius(14)
	bg := pal.raised
	if tile.Hovered() {
		bg = pal.hover
	}
	tile.Background(tile.AnimateColor("bg", bg, quick)).Border(1, pal.line)
	delay := rv.delay + time.Duration(120+60*order)*time.Millisecond
	tile.Children(func() {
		// The name never gives way: the plan shortens first, and where the
		// figures come from sits on its own line.
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			quotaBadge(c, pal, q, 28)
			ui.Column(c).Gap(2).Grow(1).Basis(0).MinWidth(0).Children(func() {
				ui.Row(c).Gap(7).AlignItems(ui.Center).MinWidth(0).Children(func() {
					ui.Text(c, q.Name).FontSize(14).FontWeight(650).TextColor(pal.ink).NoWrap().Shrink(0)
					if q.Plan != "" {
						ui.Text(c, q.Plan).FontSize(11).FontWeight(650).TextColor(pal.muted).Padding(2, 7).Radius(6).Background(pal.well).
							SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
					}
				})
				if !q.FetchedAt.IsZero() {
					ui.Text(c, tr("viaAPI")+" · "+fmtAgo(now, q.FetchedAt)).FontSize(11).TextColor(pal.faint).SingleLine().Ellipsis("…")
				}
			})
		})
		if bw, ok := balanceOf(q); ok {
			a.balanceBody(c, pal, q, bw, delay, now)
		} else if len(q.Windows) > 0 {
			a.windowRing(c, pal, q.Windows[primary(q)], delay, now)
		}
		pi := primary(q)
		first := true
		for i, w := range q.Windows {
			if w.Balance {
				if !first {
					extraBalance(c, pal, w)
				}
				first = false
				continue
			}
			if i == pi && !hasBalance(q) {
				continue
			}
			a.quotaBar(c.Key("qb-"+itoa(i)), pal, w, delay+time.Duration(80*(i+1))*time.Millisecond, now)
		}
		if q.SignIn {
			ui.Row(c).Gap(7).AlignItems(ui.Center).Padding(8, 10).Radius(8).Background(pal.warn.Alpha(0.1)).Children(func() {
				ui.Box(c).Size(6, 6).Radius(3).Background(pal.warn).Shrink(0)
				ui.Text(c, trf("signInAgain", q.Name)).FontSize(12).TextColor(pal.warn).MaxLines(2)
			})
		} else if q.Err != "" {
			ui.Text(c, q.Err).FontSize(11.5).TextColor(pal.warn).MaxLines(2)
		}
	})
}

// extraBalance is a further balance of an account, such as an add-on pack.
func extraBalance(c *ui.Context, pal palette, w QuotaWindow) {
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Text(c, windowName(w.Name)).FontSize(12).TextColor(pal.ink).SingleLine().Shrink(1).MinWidth(0)
		ui.Spacer(c)
		v := fmtAmount(w.Remaining, w.Unit)
		if !isMoney(w.Unit) {
			v += " " + unitName(w.Unit)
		}
		ui.Text(c, v).FontSize(12).FontWeight(650).TextColor(pal.ink).FontFeatures("tnum").NoWrap()
		if !w.ExpiresAt.IsZero() {
			ui.Text(c, trf("expiresOn", fmtDate(w.ExpiresAt))).FontSize(11.5).TextColor(pal.faint).NoWrap()
		}
	})
}

// quotaKey is a quota account's provider without its region, "trae-cn"
// → "trae".
func quotaKey(id string) string {
	for _, suf := range []string{"-cn", "-global", "-ai"} {
		id = strings.TrimSuffix(id, suf)
	}
	return id
}

func hasBalance(q Quota) bool { _, ok := balanceOf(q); return ok }

// windowRing leads a tile with its tightest window: a ring, a tick where
// the time is, and what is left.
func (a *App) windowRing(c *ui.Context, pal palette, w QuotaWindow, delay time.Duration, now time.Time) {
	rv := a.mo.cur
	f := usedFrac(w)
	col := pal.meterColor(w, now)
	ui.Row(c).Gap(14).AlignItems(ui.Center).Children(func() {
		ring := ui.Box(c.Key("ring")).Size(64, 64).Shrink(0).Center()
		target := float32(0)
		if rv.seen {
			target = float32(f)
		}
		an := snappy
		if !rv.settled {
			an = delayed(bouncy, delay)
		}
		v := ring.AnimateTo("v", target, an)
		pc := float32(pace(w, now))
		ink := pal.ink
		ring.Draw(func(p *ui.Painter, r ui.Rect) {
			cx, cy := r.X+r.W/2, r.Y+r.H/2
			const sw = 7
			rad := (min(r.W, r.H) - sw) / 2
			p.StrokePath(ringPath(cx, cy, rad), sw, col.Alpha(0.15))
			if v > 0.003 {
				p.StrokePathConicGradient(ringPath(cx, cy, rad).Trim(0, min(v, 0.999)), sw, ui.ConicGradient{
					Stops: []ui.GradientStop{{Color: col.Alpha(0.55), At: 0}, {Color: col, At: min(v, 1)}}, Oklab: true,
				})
			}
			if pc > 0 && v > 0.003 {
				x0, y0 := pointOn(cx, cy, rad-sw/2-2, pc)
				x1, y1 := pointOn(cx, cy, rad+sw/2+2, pc)
				seg(p, x0, y0, x1, y1, 2, ink.Alpha(0.8*min(v/max(pc, 0.01), 1)))
			}
		}).Children(func() {
			a.num(c, "qp", f*100, func(v float64) string { return fmt.Sprintf("%.0f%%", v) }, func(e ui.Element) ui.Element {
				return e.Font(display).FontSize(16).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum")
			})
		})
		ui.Column(c).Gap(3).Grow(1).MinWidth(0).Children(func() {
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, windowLabel(w)).FontSize(13.5).FontWeight(600).TextColor(pal.ink).SingleLine().Shrink(1).MinWidth(0)
				switch {
				case f >= 0.85:
					chip(c, tr("nearlyOut"), pal.bad)
				case ahead(w, now):
					chip(c, tr("aheadOfPace"), pal.warn)
				}
			})
			if v := windowValue(w); v != "" {
				ui.Text(c, v).FontSize(12).TextColor(pal.muted).FontFeatures("tnum").SingleLine()
			}
			if !w.ResetsAt.IsZero() {
				ui.Text(c, fmtIn(now, w.ResetsAt)).FontSize(11.5).TextColor(pal.faint).SingleLine()
			}
		})
	})
}

func chip(c *ui.Context, s string, col ui.Color) {
	ui.Text(c, s).FontSize(10.5).FontWeight(700).TextColor(col).Padding(1, 6).Radius(6).Background(col.Alpha(0.14)).NoWrap()
}

// quotaBar is a secondary window: its name, how much is used and a slim
// bar with the time's tick.
func (a *App) quotaBar(c *ui.Context, pal palette, w QuotaWindow, delay time.Duration, now time.Time) {
	rv := a.mo.cur
	f := usedFrac(w)
	col := pal.meterColor(w, now)
	ui.Column(c).Gap(6).Children(func() {
		ui.Row(c).Gap(8).Children(func() {
			ui.Text(c, windowLabel(w)).FontSize(12).TextColor(pal.ink).SingleLine().Shrink(1).MinWidth(0)
			ui.Spacer(c)
			ui.Text(c, fmt.Sprintf("%.0f%%", f*100)).FontSize(12).FontWeight(650).TextColor(col).FontFeatures("tnum").NoWrap()
			if !w.ResetsAt.IsZero() {
				ui.Text(c, fmtIn(now, w.ResetsAt)).FontSize(11.5).TextColor(pal.faint).NoWrap()
			}
		})
		track := ui.Box(c.Key("track")).FillWidth().Height(5).Radius(3)
		target := float32(0)
		if rv.seen {
			target = float32(f)
		}
		an := snappy
		if !rv.settled {
			an = delayed(gentle, delay)
		}
		v := track.AnimateTo("v", target, an)
		pc := float32(pace(w, now))
		ink := pal.ink
		track.Draw(func(p *ui.Painter, r ui.Rect) {
			p.Fill(r, col.Alpha(0.15), 2.5)
			if v > 0 {
				p.Fill(ui.Rect{X: r.X, Y: r.Y, W: r.W * min(v, 1), H: r.H}, col, 2.5)
			}
			if pc > 0 {
				x := r.X + r.W*pc
				p.Fill(ui.Rect{X: x - 1, Y: r.Y - 3, W: 2, H: r.H + 6}, ink.Alpha(0.7), 1)
			}
		})
	})
}

// balanceBody is a credit balance: what is left, the last two weeks' use
// day by day, and how long it lasts.
func (a *App) balanceBody(c *ui.Context, pal palette, q Quota, w QuotaWindow, delay time.Duration, now time.Time) {
	rv := a.mo.cur
	perDay, days := runway(q, w)
	ui.Row(c).Gap(14).AlignItems(ui.End).Children(func() {
		ui.Column(c).Gap(2).Grow(1).MinWidth(0).Children(func() {
			ui.Text(c, tr("balance")).FontSize(12).TextColor(pal.muted)
			ui.Row(c).Gap(5).AlignItems(ui.End).Children(func() {
				a.num(c, "bal", w.Remaining, func(v float64) string { return fmtAmount(v, w.Unit) }, func(e ui.Element) ui.Element {
					return e.Font(display).FontSize(26).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum").LetterSpacing(-0.4).NoWrap()
				})
				if !isMoney(w.Unit) {
					ui.Text(c, unitName(w.Unit)).FontSize(12.5).FontWeight(600).TextColor(pal.muted).Padding(0, 0, 4, 0).NoWrap()
				}
			})
		})
		// The last 14 days of use, once there is any to show.
		known := false
		for _, d := range q.Daily {
			known = known || (d.Known && d.Used > 0)
		}
		if !known {
			return
		}
		bars := ui.Box(c.Key("days")).Size(126, 40).Shrink(0)
		target := float32(0)
		if rv.seen {
			target = 1
		}
		an := ui.Animation{Duration: 500 * time.Millisecond, Ease: easeOut}
		if !rv.settled {
			an = delayed(ui.Animation{Duration: 900 * time.Millisecond, Ease: easeOut}, delay)
		}
		prog := bars.AnimateTo("p", target, an)
		daily := q.Daily
		peak := 0.0
		for _, d := range daily {
			peak = max(peak, d.Used)
		}
		accent, well, faint := pal.accent, pal.well, pal.faint
		bars.Tooltip(tr("dailyUse14"))
		bars.Draw(func(p *ui.Painter, r ui.Rect) {
			n := 14
			gap := float32(3)
			bw := (r.W - gap*float32(n-1)) / float32(n)
			off := n - len(daily)
			for i := range n {
				x := r.X + float32(i)*(bw+gap)
				j := i - off
				if j < 0 || !daily[j].Known || peak <= 0 {
					p.Fill(ui.Rect{X: x, Y: r.Y + r.H - 2, W: bw, H: 2}, well, 1)
					continue
				}
				k := float32(outQuint(float64(prog)*1.6 - float64(i)/float64(n)*0.6))
				h := max(float32(daily[j].Used/peak)*(r.H-2)*k, 2)
				col := accent.Alpha(0.55)
				if j == len(daily)-1 {
					col = accent
				}
				p.Fill(ui.Rect{X: x, Y: r.Y + r.H - h, W: bw, H: h}, col, min(bw/2, 2))
			}
			_ = faint
		})
	})
	ui.Column(c).Gap(3).Children(func() {
		switch {
		case days < 0 && len(q.Daily) == 0:
			ui.Text(c, tr("historyBuilding")).FontSize(12).TextColor(pal.muted).SingleLine()
		case days < 0:
			ui.Text(c, tr("noRecentUse")).FontSize(12).TextColor(pal.muted).SingleLine()
		default:
			col := pal.good
			switch {
			case days < 3:
				col = pal.bad
			case days < 7:
				col = pal.warn
			}
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, trf("avgPerDay", fmtAmount(perDay, w.Unit))).FontSize(12).TextColor(pal.muted).SingleLine().Shrink(1).MinWidth(0)
				ui.Text(c, trn("runwayDays", int(math.Floor(days)))).FontSize(12).FontWeight(650).TextColor(col).NoWrap()
			})
		}
		if !w.ExpiresAt.IsZero() {
			ui.Text(c, trf("expiresOn", fmtDate(w.ExpiresAt))).FontSize(11.5).TextColor(pal.faint).SingleLine()
		}
	})
}

// quotaBadge is a quota's provider mark.
func quotaBadge(c *ui.Context, pal palette, q Quota, box float32) ui.Element {
	if h, ok := quotaHarness[quotaKey(q.ID)]; ok {
		return toolBadge(c, h, box)
	}
	m, ok := quotaIcons[quotaKey(q.ID)]
	if ok && m.tile {
		return ui.Image(c, m.svg).Size(box, box).Shrink(0)
	}
	col := pal.accent
	return ui.Box(c).Size(box, box).Radius(box * 0.28).Center().Shrink(0).Background(pal.well).Children(func() {
		switch {
		case !ok:
			name := []rune(q.Name)
			if len(name) == 0 {
				name = []rune("?")
			}
			ui.Text(c, string(name[0])).FontSize(box * 0.5).Bold().TextColor(col)
		case m.colored:
			ui.Image(c, m.svg).Size(box*0.6, box*0.6).Shrink(0)
		default:
			ui.Icon(c, m.svg).FontSize(box * 0.6).TextColor(pal.ink).Shrink(0)
		}
	})
}
