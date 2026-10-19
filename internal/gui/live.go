package gui

import (
	"math"
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
// card, in the same quiet card as the rest of the page. The front card
// rises into place as cards do when the page opens; the deck turns by
// itself, or by its dots and arrows, and holds still under the pointer.
//
// It shows money and requests, which the logs record, and not output
// speed, which they do not (it would be a guess from gaps).
func (a *App) liveCard(c *ui.Context, pal palette) {
	act := a.ov.active
	live := len(act) > 0
	reduce := c.Preferences().ReduceMotion
	var perHour float64
	for _, s := range act {
		perHour += s.perHour
	}
	idx := a.deckIndex()
	el := a.rcard(c, pal, "live", 1, false).Grow(1).Basis(0).MinWidth(0).Gap(14)
	el.Children(func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, tr("activeNow")).FontSize(14).FontWeight(650).TextColor(pal.ink)
			if live {
				liveDot(c, pal)
				ui.Text(c.Key("n"), trf("sessionsLive", len(act))+" · "+trf("perHour", fmtUSD(perHour))).
					FontSize(12.5).TextColor(pal.muted).FontFeatures("tnum").SingleLine().Shrink(1).MinWidth(0).
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
		a.deckView(c, pal, idx, !reduce)
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
	// Opaque, as the quota tiles look, so the cards behind stay behind.
	front := pal.card.Mix(pal.ink, 0.025)
	// The cards behind are painted under the front one, which stays in the
	// flow so that the one leaving can lift away over the deck.
	peek := deckPeek * float32(behind)
	box := ui.Column(c.Key("deck")).FillWidth().Grow(1).Padding(0, 0, peek, 0)
	a.deck.hover = box.Hovered()
	shown := box.AnimateTo("behind", float32(behind), gentle)
	dark, card0, line := pal.dark, pal.card, pal.line
	box.Draw(func(p *ui.Painter, r ui.Rect) {
		for k := 2; k >= 1; k-- {
			o := clamp01(float64(shown) - float64(k-1))
			if o <= 0 {
				continue
			}
			kf := float32(k)
			back, edge := front.Mix(card0, 0.35*kf), line.Alpha(float32(o)*(1-0.3*kf))
			_ = dark
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
		card := ui.Column(c.Key("front-"+key)).FillWidth().Grow(1).
			Radius(14).Border(1, pal.line).Padding(14, 16, 12, 16).Gap(12).Cursor(ui.CursorPointer).
			Transition(ui.ElementTransition{
				Animation: &gentle,
				// The next card comes up off the deck as cards rise into a
				// page, over the one it replaces (two faces crossfading read
				// as a blur).
				Enter: &ui.Motion{Y: 30 * dir, Scale: 0.96, Opacity: 0.45},
			})
		bg := front
		if card.Hovered() {
			bg = front.Mix(pal.ink, 0.03)
		}
		card.Background(card.AnimateColor("bg", bg, quick))
		hov := card.Hovered()
		card.Children(func() { a.deckFront(c, pal, s, moving, hov) })
		if card.Clicked() {
			a.showSession(s.row)
		}
	})
}

// deckFront is one running session: its orb of light, breathing faster the
// busier the session; what and where; what it costs; and its requests of
// the last minutes as beats drifting left. Pointing at the card lifts the
// orb a little (motion.dev's whileHover).
func (a *App) deckFront(c *ui.Context, pal palette, s activeSession, moving, hovered bool) {
	key := sessionKey(s.row.Harness, s.row.SessionID)
	ui.Row(c).Gap(18).AlignItems(ui.Center).Children(func() {
		orb := ui.Box(c.Key("orb")).Size(84, 84).Shrink(0)
		// Liveliness is the session's requests in the last ten minutes.
		e := orb.AnimateTo("e", float32(min(float64(s.requests)/30, 1)), gentle)
		orb.AnimatePose(ui.Pose{Scale: map[bool]float32{false: 1, true: 1.06}[hovered]}, bouncy)
		orb.Draw(func(p *ui.Painter, r ui.Rect) { drawOrb(p, r, e, moving, pal) })
		ui.Column(c).Gap(12).Grow(1).Basis(0).MinWidth(0).Children(func() {
			ui.Column(c).Gap(3).Children(func() {
				ui.Row(c).Gap(7).AlignItems(ui.Center).MinWidth(0).Children(func() {
					toolBadge(c, s.row.Harness, 18)
					ui.Text(c, sessionTitle(s.row)).FontSize(14.5).FontWeight(650).TextColor(pal.ink).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
				})
				sub := s.model
				if p := projectName(s.row.Project); p != "" {
					sub = p + " · " + sub
				}
				sub += " · " + fmtAgo(a.now(), s.last)
				ui.Text(c, sub).FontSize(12).TextColor(pal.muted).SingleLine().Ellipsis("…")
			})
			// What it cost and how fast it writes, then the pace in one
			// quiet line.
			ui.Column(c).Gap(4).Children(func() {
				ui.Row(c).Gap(22).AlignItems(ui.End).Children(func() {
					ui.Row(c).Gap(8).AlignItems(ui.End).Children(func() {
						a.num(c, "cost-"+key, s.row.CostUSD, fmtUSD, func(e ui.Element) ui.Element {
							return e.Font(display).FontSize(24).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum").LetterSpacing(-0.4).NoWrap()
						})
						ui.Text(c, tr("thisSession")).FontSize(12).TextColor(pal.muted).NoWrap().Padding(0, 0, 4, 0)
					})
					// Output speed, estimated from the gaps between requests.
					ui.Row(c).Gap(6).AlignItems(ui.End).Children(func() {
						v := "—"
						if s.rate > 0 {
							v = fmtTokens(int64(math.Round(s.rate)))
						}
						ui.Text(c.Key("tps"), v).Font(display).FontSize(24).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum").LetterSpacing(-0.4).NoWrap().
							ContentTransition(ui.ContentTransition{Numeric: true})
						ui.Text(c, tr("tpsUnit")).FontSize(12).TextColor(pal.muted).NoWrap().Padding(0, 0, 4, 0)
					})
				})
				ui.Text(c.Key("pace"), trf("perHour", fmtUSD(s.perHour))+"  ·  "+tr("hitRate")+" "+fmtPct(s.hit)+"  ·  "+trf("reqIn10m", s.requests)).
					FontSize(12).TextColor(pal.muted).FontFeatures("tnum").SingleLine().Ellipsis("…").
					ContentTransition(ui.ContentTransition{Numeric: true})
			})
		})
	})
	beats(c, pal, s, a.now(), moving)
}

// beats is the session's requests over the running window, one tick each,
// as tall as what it cost; they drift left as time passes and the newest
// glows.
func beats(c *ui.Context, pal palette, s activeSession, now time.Time, moving bool) {
	// The orb's own light: periwinkle into violet, the newest in rose.
	from, to, newest := ui.Hex("#8C9EFF"), ui.Hex("#B97CF0"), ui.Hex("#E58AD6")
	faint, line := pal.faint, pal.line
	peak := 0.0
	for _, b := range s.pulses {
		peak = max(peak, b.cost)
	}
	box := ui.Box(c.Key("beats")).FillWidth().Height(44)
	in := box.AnimateTo("in", 1, ui.Animation{Duration: 700 * time.Millisecond, Ease: easeOut})
	built := time.Now()
	box.Draw(func(p *ui.Painter, r ui.Rect) {
		// Drift with the frame clock from the moment the view was built.
		t := now
		if d := p.Now().Sub(built); moving && d > 0 && d < 10*time.Minute {
			t = now.Add(d)
		}
		const label = 13
		h := r.H - label
		base := r.Y + h
		p.Fill(ui.Rect{X: r.X, Y: base, W: r.W, H: 1}, line, 0)
		win := float32(activeWindow)
		for i, b := range s.pulses {
			age := float32(t.Sub(b.at))
			if age < 0 {
				age = 0
			}
			if age > win {
				continue
			}
			x := r.X + r.W*(1-age/win)
			v := float32(0.25)
			if peak > 0 {
				v = 0.18 + 0.82*float32(math.Sqrt(b.cost/peak))
			}
			bh := max((h-4)*v*in, 2)
			a := 0.35 + 0.55*(1-age/win)
			col := from.Mix(to, (x-r.X)/r.W)
			p.Fill(ui.Rect{X: x - 1.5, Y: base - bh, W: 3, H: bh}, col.Alpha(a), 1.5)
			if i == len(s.pulses)-1 && moving {
				k := float32(p.Now().UnixMilli()%1800) / 1800
				rad := 3 + 5*k
				p.FillPath((&ui.Path{}).Circle(x, base-bh, rad), newest.Alpha(0.3*(1-k)))
				p.FillPath((&ui.Path{}).Circle(x, base-bh, 2.5), newest)
			}
		}
		left := trf("minutesAgo", int(activeWindow/time.Minute))
		p.Text(r.X, base+2, left, 10, faint)
		w, _ := p.MeasureText(0, ui.Span{Text: tr("now"), Size: 10})
		p.Text(r.X+r.W-w, base+2, tr("now"), 10, faint)
		if moving {
			p.After(liveFrame)
		}
	})
}

// idleBody is the deck's place when nothing runs: the orb's place, dark.
func idleBody(c *ui.Context, pal palette) {
	ui.Row(c).Gap(16).AlignItems(ui.Center).Grow(1).Children(func() {
		well, faint := pal.well, pal.faint
		ui.Box(c).Size(84, 84).Shrink(0).Draw(func(p *ui.Painter, r ui.Rect) {
			cx, cy := r.X+r.W/2, r.Y+r.H/2
			p.FillPath((&ui.Path{}).Circle(cx, cy, 30), well)
			p.StrokePath(ringPath(cx, cy, 24), 1.5, faint.Alpha(0.4))
			p.FillPath((&ui.Path{}).Circle(cx, cy, 4), faint)
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
