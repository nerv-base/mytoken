package gui

import (
	"time"

	"github.com/egoist/mygo/ui"
)

// ---------- running now ----------

// deckState is the running-sessions deck: one session in front, the rest
// stacked behind it.
type deckState struct {
	front string // the session in front, by sessionKey
	dir   int    // which way the last switch went: 1 forward, -1 back
	hover bool   // the pointer rests on the deck: hold it still
	armed bool   // an advance is scheduled
}

// deckEvery is how long a session stays in front before the next comes.
const deckEvery = 6 * time.Second

// deckPeek is how far each card behind shows under the one before.
const deckPeek = 9

// liveCard shows what is running now as a deck of cards, one session per
// card. The front card rises into place as cards do when the page opens;
// the deck turns by itself, or by its dots and arrows, and holds still
// under the pointer. A ring of light flows around it while anything runs.
//
// It shows money and requests, which the logs record, and not output
// speed, which they do not (it would be a guess from gaps).
func (a *App) liveCard(c *ui.Context, pal palette) {
	act := a.ov.active
	live := len(act) > 0
	reduce := c.Preferences().ReduceMotion
	const pad = 14
	outer := ui.Box(c.Key("live-wrap")).Margin(-pad).Padding(pad).Grow(1.2).Basis(0).MinWidth(0)
	glow := outer.AnimateTo("glow", map[bool]float32{false: 0, true: 1}[live], gentle)
	moving := live && !reduce
	outer.Draw(func(p *ui.Painter, r ui.Rect) { drawRing(p, r, pad, 16, glow, moving) })
	var perHour float64
	for _, s := range act {
		perHour += s.perHour
	}
	idx := a.deckIndex()
	outer.Children(func() {
		el := a.rcard(c, pal, "live", 1, false).FillWidth().FillHeight().Gap(14)
		el.Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, tr("activeNow")).FontSize(14).FontWeight(650).TextColor(pal.ink)
				if live {
					liveDot(c, pal)
					ui.Text(c.Key("n"), trf("sessionsLive", len(act))+" · "+trf("perHour", fmtUSD(perHour))).
						FontSize(12.5).TextColor(pal.muted).FontFeatures("tnum").SingleLine().
						ContentTransition(ui.ContentTransition{})
				}
				ui.Spacer(c)
				if len(act) > 1 {
					a.deckPager(c, pal, len(act), idx)
				}
			})
			if !live {
				idleBody(c, pal)
				return
			}
			a.deckView(c, pal, idx, moving)
		})
	})
	if len(act) > 1 && !reduce {
		a.armDeck()
	}
}

// deckIndex is the front session's place among the running ones; a front
// session that stopped gives way to the first.
func (a *App) deckIndex() int {
	act := a.ov.active
	for i, s := range act {
		if sessionKey(s.row.Harness, s.row.SessionID) == a.deck.front {
			return i
		}
	}
	if len(act) > 0 {
		a.deck.front = sessionKey(act[0].row.Harness, act[0].row.SessionID)
	}
	return 0
}

// deckGo brings the i-th running session to the front.
func (a *App) deckGo(i, dir int) {
	act := a.ov.active
	if len(act) == 0 {
		return
	}
	i = ((i % len(act)) + len(act)) % len(act)
	a.deck.front = sessionKey(act[i].row.Harness, act[i].row.SessionID)
	a.deck.dir = dir
}

// armDeck schedules the next turn of the deck.
func (a *App) armDeck() {
	if a.deck.armed || a.d.Post == nil {
		return
	}
	a.deck.armed = true
	time.AfterFunc(deckEvery, func() {
		a.d.Post(func() {
			a.deck.armed = false
			if a.stopped || a.page != "overview" || a.deck.hover || len(a.ov.active) < 2 {
				return
			}
			a.deckGo(a.deckIndex()+1, 1)
		})
	})
}

// deckPager is the deck's arrows and one dot per session, the front's long.
func (a *App) deckPager(c *ui.Context, pal palette, n, idx int) {
	arrow := func(key string, dir int) {
		b := ui.Box(c.Key(key)).Size(22, 22).Radius(11).Cursor(ui.CursorPointer).AlignItems(ui.Center).Justify(ui.Center)
		bg := pal.well.Alpha(0)
		if b.Hovered() {
			bg = pal.well
		}
		b.Background(b.AnimateColor("bg", bg, quick))
		press(b)
		b.Children(func() {
			ic := ui.Icon(c, icChevron).FontSize(12).TextColor(pal.muted)
			if dir < 0 {
				ic.Transform(ui.Rotation(180))
			}
		})
		if b.Clicked() {
			a.deckGo(idx+dir, dir)
		}
	}
	ui.Row(c.Key("pager")).Gap(4).AlignItems(ui.Center).Shrink(0).Children(func() {
		arrow("prev", -1)
		ui.Row(c).Gap(4).AlignItems(ui.Center).Padding(0, 2).Children(func() {
			for i := range n {
				on := i == idx
				d := ui.Box(c.Key("dot" + itoa(i))).Height(6).Radius(3).Cursor(ui.CursorPointer)
				w := d.AnimateTo("w", map[bool]float32{false: 6, true: 18}[on], snappy)
				col := pal.faint.Alpha(0.6)
				if on {
					col = pal.accent
				}
				d.Width(w).Background(d.AnimateColor("c", col, quick))
				if d.Clicked() && !on {
					dir := 1
					if i < idx {
						dir = -1
					}
					a.deckGo(i, dir)
				}
			}
		})
		arrow("next", 1)
	})
}

// deck stacks the running sessions: the front one whole, up to two more
// peeking under it.
func (a *App) deckView(c *ui.Context, pal palette, idx int, moving bool) {
	act := a.ov.active
	behind := min(len(act)-1, 2)
	front := pal.card.Mix(pal.ink, 0.035)
	if !pal.dark {
		front = ui.Hex("#FAFAFC")
	}
	// The cards behind are painted under the front one, which stays in the
	// flow so that the one leaving can lift away over the deck.
	peek := deckPeek * float32(behind)
	box := ui.Column(c.Key("deck")).FillWidth().Grow(1).Padding(0, 0, peek, 0)
	a.deck.hover = box.Hovered()
	shown := box.AnimateTo("behind", float32(behind), gentle)
	dark, card0 := pal.dark, pal.card
	box.Draw(func(p *ui.Painter, r ui.Rect) {
		for k := 2; k >= 1; k-- {
			o := clamp01(float64(shown) - float64(k-1))
			if o <= 0 {
				continue
			}
			kf := float32(k)
			back, edge := front.Mix(card0, 0.3*kf), ui.RGBA(20, 20, 40, float32(o)*(0.09-0.02*kf))
			if dark {
				back, edge = front.Mix(card0, 0.25*kf), ui.RGBA(255, 255, 255, float32(o)*(0.10-0.025*kf))
			}
			rr := ui.Rect{X: r.X + 14*kf, Y: r.Y + deckPeek*kf*float32(o), W: r.W - 28*kf, H: r.H - peek}
			p.Fill(ui.Rect{X: rr.X - 1, Y: rr.Y - 1, W: rr.W + 2, H: rr.H + 2}, edge, 15)
			p.Fill(rr, back.Alpha(float32(o)), 14)
		}
	})
	box.Children(func() {
		s := act[idx]
		key := sessionKey(s.row.Harness, s.row.SessionID)
		dir := float32(a.deck.dir)
		if dir == 0 {
			dir = 1
		}
		card := ui.Column(c.Key("front-"+key)).FillWidth().Grow(1).MinHeight(150).
			Radius(14).Background(front).Border(1, pal.line).Padding(16, 18).Gap(12).Cursor(ui.CursorPointer).
			Transition(ui.ElementTransition{
				Animation: &gentle,
				// The next card comes up off the deck as cards rise into a
				// page, over the one it replaces (two faces crossfading read
				// as a blur).
				Enter: &ui.Motion{Y: 30 * dir, Scale: 0.96, Opacity: 0.45},
			})
		if !pal.dark {
			card.Shadow(0, 6, 18, 0, ui.RGBA(20, 20, 40, 0.07))
		}
		card.Children(func() { a.deckFront(c, pal, s, moving) })
		if card.Clicked() {
			a.showSession(s.row)
		}
	})
}

// deckFront is one running session: what, where, and what it costs.
func (a *App) deckFront(c *ui.Context, pal palette, s activeSession, moving bool) {
	key := sessionKey(s.row.Harness, s.row.SessionID)
	ui.Row(c).Gap(16).AlignItems(ui.Center).Grow(1).Children(func() {
		orb := ui.Box(c.Key("orb")).Size(92, 92).Shrink(0)
		// How lively the orb is follows how busy the session is: its
		// requests in the last ten minutes, which the logs record.
		e := orb.AnimateTo("e", float32(min(float64(s.requests)/30, 1)), gentle)
		orb.Draw(func(p *ui.Painter, r ui.Rect) { drawOrb(p, r, e, moving, pal) })
		ui.Column(c).Gap(10).Grow(1).Basis(0).MinWidth(0).Children(func() {
			ui.Column(c).Gap(3).Children(func() {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					toolBadge(c, s.row.Harness, 20)
					ui.Text(c, sessionTitle(s.row)).FontSize(15).FontWeight(650).TextColor(pal.ink).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
				})
				sub := s.model
				if p := projectName(s.row.Project); p != "" {
					sub = p + " · " + sub
				}
				sub += " · " + fmtAgo(a.now(), s.last)
				ui.Text(c, sub).FontSize(12).TextColor(pal.muted).SingleLine().Ellipsis("…")
			})
			ui.Row(c).Gap(20).AlignItems(ui.Start).Children(func() {
				ui.Column(c).Gap(2).Shrink(0).Children(func() {
					ui.Text(c, tr("thisSession")).FontSize(11.5).TextColor(pal.muted).NoWrap().Padding(0, 0, 2, 0)
					a.num(c, "cost-"+key, s.row.CostUSD, fmtUSD, func(e ui.Element) ui.Element {
						return e.Font(display).FontSize(26).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum").LetterSpacing(-0.5).NoWrap()
					})
				})
				deckStat(c, pal, tr("perHourLabel"), fmtUSD(s.perHour))
				deckStat(c, pal, tr("hitRate"), fmtPct(s.hit))
				deckStat(c, pal, tr("requests10m"), itoaN(s.requests))
			})
		})
	})
}

func deckStat(c *ui.Context, pal palette, label, value string) {
	ui.Column(c).Gap(4).MinWidth(0).Children(func() {
		ui.Text(c, label).FontSize(11.5).TextColor(pal.muted).SingleLine()
		ui.Text(c, value).Font(display).FontSize(15).FontWeight(650).TextColor(pal.ink).FontFeatures("tnum").NoWrap().
			ContentTransition(ui.ContentTransition{Numeric: true})
	})
}

// idleBody is the deck's place when nothing runs: a quiet disc.
func idleBody(c *ui.Context, pal palette) {
	ui.Row(c).Gap(16).AlignItems(ui.Center).Grow(1).Children(func() {
		well, faint := pal.well, pal.faint
		ui.Box(c).Size(92, 92).Shrink(0).Opacity(0.9).Draw(func(p *ui.Painter, r ui.Rect) {
			cx, cy := r.X+r.W/2, r.Y+r.H/2
			rad := min(r.W, r.H)/2 - 6
			p.Fill(ui.Rect{X: cx - rad, Y: cy - rad, W: rad * 2, H: rad * 2}, well, rad)
			p.StrokePath(ringPath(cx, cy, rad-8), 2, faint.Alpha(0.5))
			p.Fill(ui.Rect{X: cx - 4, Y: cy - 4, W: 8, H: 8}, faint, 4)
		})
		ui.Column(c).Gap(3).MinWidth(0).Grow(1).Children(func() {
			ui.Text(c, tr("noActiveTitle")).FontSize(15).FontWeight(650).TextColor(pal.ink)
			ui.Text(c, tr("noActiveHint")).FontSize(12.5).TextColor(pal.muted).MaxLines(2)
		})
	})
}

func itoaN(n int) string {
	if n < 0 {
		return "—"
	}
	return fmtInt(int64(n))
}
