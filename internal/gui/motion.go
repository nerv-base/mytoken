package gui

import (
	"math"
	"time"

	"github.com/egoist/mygo/ui"
)

// Motion, in the spirit of motion.dev: cards rise into place as they come
// into view (whileInView), staggered; numbers count instead of jumping;
// selections slide from one item to the next (layoutId); what is pressed
// squashes a little (whileTap). Springs keep their velocity when a value
// changes mid-flight. Nothing moves at rest: the only continuous motion is
// the running sessions' beats and the heatmap's slow breath.

var (
	easeOut  = ui.CubicBezier(0.22, 1, 0.36, 1)
	expoOut  = ui.CubicBezier(0.16, 1, 0.3, 1)
	snappy   = ui.Animation{Spring: ui.SpringOptions{Duration: 420 * time.Millisecond, Bounce: 0.18}}
	gentle   = ui.Animation{Spring: ui.SpringOptions{Duration: 750 * time.Millisecond, Bounce: 0.1}}
	bouncy   = ui.Animation{Spring: ui.SpringOptions{Duration: 1100 * time.Millisecond, Bounce: 0.22}}
	quick    = ui.Animation{Duration: 160 * time.Millisecond, Ease: ui.EaseOut}
	stagger  = 60 * time.Millisecond
	revealIn = 900 * time.Millisecond
)

func delayed(a ui.Animation, d time.Duration) ui.Animation {
	a.Delay = d
	return a
}

// motion is what the views remember between frames to animate.
type motion struct {
	seen  map[string]time.Time // when each card came into view
	built map[string]bool
	marks map[string]ui.Rect // items' boxes in the frame before, for slide
	slid  map[string]bool
	cur   reveal // the card being built
}

// reveal is what one card knows about its entrance while it builds.
type reveal struct {
	seen    bool          // it has come into view
	at      time.Time     // when its entrance starts (after its stagger)
	delay   time.Duration // its stagger
	settled bool          // its entrance is over: changes now animate at once
}

// reset replays the entrances, as when a page is entered.
func (m *motion) reset() {
	m.seen = map[string]time.Time{}
	m.built = map[string]bool{}
	m.cur = reveal{}
}

func (m *motion) init() {
	if m.seen == nil {
		m.reset()
		m.marks = map[string]ui.Rect{}
		m.slid = map[string]bool{}
	}
}

// rcard is a card that rises into place as it first comes into view,
// staggered by order, and lifts a little under the pointer when lift.
func (a *App) rcard(c *ui.Context, pal palette, key string, order int, lift bool) ui.Element {
	m := &a.mo
	m.init()
	el := card(c.Key(key), pal)
	now := time.Now()
	seenAt, seen := m.seen[key]
	if !seen && el.InView() {
		seenAt, seen = now, true
		m.seen[key] = now
	}
	if !m.built[key] {
		// InView answers for the frame before: ask for one more.
		m.built[key] = true
		c.Invalidate()
	}
	delay := time.Duration(order) * stagger
	rv := reveal{seen: seen, delay: delay}
	if seen {
		rv.at = seenAt.Add(delay)
		rv.settled = now.After(rv.at.Add(revealIn))
	}
	pose := ui.Pose{Y: 28, Fade: 1, Scale: 0.975}
	anim := delayed(gentle, delay)
	reduce := c.Preferences().ReduceMotion
	if reduce {
		pose, anim = ui.Pose{}, ui.Animation{}
	}
	if seen {
		pose = ui.Pose{}
		if rv.settled {
			anim = snappy
			if lift && el.Hovered() && !reduce {
				pose = ui.Pose{Y: -2}
			}
		}
	}
	el.AnimatePose(pose, anim)
	m.cur = rv
	return el
}

// num shows a number that counts up as its card comes in and glides to a
// new value when it changes.
func (a *App) num(c *ui.Context, key string, v float64, format func(float64) string, style func(ui.Element) ui.Element) {
	m := &a.mo
	box := ui.Box(c.Key(key))
	target := float32(0)
	if m.cur.seen || c.Preferences().ReduceMotion {
		target = float32(v)
	}
	an := ui.Animation{Duration: 700 * time.Millisecond, Ease: expoOut}
	if !m.cur.settled {
		an = ui.Animation{Duration: 1300 * time.Millisecond, Ease: expoOut, Delay: m.cur.delay + 120*time.Millisecond}
	}
	shown := box.AnimateTo("n", target, an)
	if math.Abs(float64(shown)-v) < math.Abs(v)*1e-6 {
		shown = float32(v)
	}
	val := float64(shown)
	if float32(v) == shown {
		val = v
	}
	box.Children(func() { style(ui.Text(c, format(val))) })
}

// heroNum is a big counting figure.
func (a *App) heroNum(c *ui.Context, pal palette, key string, v float64, format func(float64) string, size float32) {
	a.num(c, key, v, format, func(e ui.Element) ui.Element {
		return e.Font(display).FontSize(size).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum").LetterSpacing(-size * 0.02).NoWrap()
	})
}

// statNum is a small label over a counting figure.
func (a *App) statNum(c *ui.Context, pal palette, key, label string, v float64, format func(float64) string) {
	ui.Column(c).Gap(3).Grow(1).Basis(0).MinWidth(0).Children(func() {
		ui.Text(c, label).FontSize(12).TextColor(pal.muted).SingleLine()
		a.num(c, key, v, format, func(e ui.Element) ui.Element {
			return e.Font(display).FontSize(16).FontWeight(650).TextColor(pal.ink).FontFeatures("tnum").NoWrap()
		})
	})
}

// slide is where the selection mark of a group shows this frame, relative
// to holder: it springs from the last item chosen to item's box, as
// measured in the frame before. Items record their boxes with mark.
func (a *App) slide(c *ui.Context, holder ui.Element, group, item string) ui.Rect {
	m := &a.mo
	m.init()
	box, ok := m.marks[item]
	hb := holder.LayoutBounds()
	if !ok || box.W == 0 {
		c.Invalidate()
		return ui.Rect{}
	}
	t := ui.Rect{X: box.X - hb.X, Y: box.Y - hb.Y, W: box.W, H: box.H}
	var out ui.Rect
	vals := []*float32{&out.X, &out.Y, &out.W, &out.H}
	for k, v := range []float32{t.X, t.Y, t.W, t.H} {
		key := group + string(rune('a'+k))
		if !m.slid[group] || c.Preferences().ReduceMotion {
			*vals[k] = holder.Jump(key, v)
		} else {
			*vals[k] = holder.AnimateTo(key, v, snappy)
		}
	}
	m.slid[group] = true
	return out
}

// mark records an item's box for slide.
func (a *App) mark(item string, el ui.Element) {
	a.mo.init()
	a.mo.marks[item] = el.LayoutBounds()
}

// press squashes what is pressed, as whileTap.
func press(el ui.Element) ui.Element {
	pose := ui.Pose{}
	if el.Pressed() {
		pose = ui.Pose{Scale: 0.95}
	}
	return el.AnimatePose(pose, snappy)
}

// pills is a segmented control whose selection slides between segments;
// it reports a change.
func (a *App) pills(c *ui.Context, pal palette, group string, sel *int, labels ...string) bool {
	changed := false
	row := ui.Row(c.Key(group)).Padding(3).Gap(2).Radius(10).Background(pal.well).Shrink(0)
	mk := a.slide(c, row, group, group+"-"+itoa(*sel))
	knob := pal.card
	if pal.dark {
		knob = ui.RGBA(255, 255, 255, 0.13)
	}
	shadow := !pal.dark
	row.Draw(func(p *ui.Painter, r ui.Rect) {
		if mk.W > 0 {
			rr := ui.Rect{X: r.X + mk.X, Y: r.Y + mk.Y, W: mk.W, H: mk.H}
			if shadow {
				p.Fill(ui.Rect{X: rr.X, Y: rr.Y + 0.5, W: rr.W, H: rr.H}, ui.RGBA(20, 20, 40, 0.10), 8)
			}
			p.Fill(rr, knob, 8)
		}
	})
	row.Children(func() {
		for i, l := range labels {
			b := ui.Box(c.Key(i)).Padding(5, 12).Radius(8).Cursor(ui.CursorPointer)
			col := pal.muted
			if *sel == i {
				col = pal.ink
			} else if b.Hovered() {
				col = pal.ink.Alpha(0.8)
			}
			press(b)
			a.mark(group+"-"+itoa(i), b)
			b.Children(func() {
				ui.Text(c, l).FontSize(12.5).FontWeight(600).TextColor(b.AnimateColor("tc", col, quick)).NoWrap()
			})
			if b.Clicked() && *sel != i {
				*sel = i
				changed = true
			}
		}
	})
	return changed
}

func itoa(i int) string {
	if i >= 0 && i < 10 {
		return string(rune('0' + i))
	}
	return string(rune('a'+i%26)) + itoa(i/26)
}

// ---------- curves for what Draw animates itself (paint-only) ----------

func clamp01(x float64) float64 { return math.Max(0, math.Min(1, x)) }

func lerp(a, b, k float64) float64 { return a + (b-a)*k }

// liveFrame is how often the slow, continuous motions (the running
// sessions' ripples and beats, the breathing dots) repaint: they drift
// too slowly to need every display frame, and the window costs CPU per
// frame.
const liveFrame = time.Second / 15
