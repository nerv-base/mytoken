package gui

import (
	"math"
	"time"

	"github.com/egoist/mygo/ui"
)

// card is a raised, opaque surface.
func card(c *ui.Context, pal palette) ui.Element {
	e := ui.Column(c).Padding(18, 20).Gap(14).Radius(14).Background(pal.card).MinWidth(0)
	if pal.dark {
		return e.Border(1, pal.line)
	}
	return e.Border(1, pal.line).Shadow(0, 1, 2, 0, ui.RGBA(20, 20, 40, 0.04))
}

// cardTitle is a card's heading with optional content on the right.
func cardTitle(c *ui.Context, pal palette, title string, right func()) {
	ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
		ui.Text(c, title).FontSize(13).FontWeight(650).TextColor(pal.ink)
		ui.Spacer(c)
		if right != nil {
			right()
		}
	})
}

// caption is small muted text.
func caption(c *ui.Context, pal palette, s string) ui.Element {
	return ui.Text(c, s).FontSize(12).TextColor(pal.muted)
}

// figure is a big number that rolls to a new value.
func figure(c *ui.Context, pal palette, key, s string, size float32) ui.Element {
	return ui.Text(c.Key(key), s).Font(display).FontSize(size).FontWeight(700).TextColor(pal.ink).
		FontFeatures("tnum").LetterSpacing(-size * 0.02).NoWrap().
		ContentTransition(ui.ContentTransition{Numeric: true})
}

// pageHeader is a page's title row with controls on the right.
func pageHeader(c *ui.Context, pal palette, title string, right func()) {
	ui.Row(c).Gap(12).AlignItems(ui.Center).Height(36).Children(func() {
		ui.Text(c, title).Font(display).FontSize(22).FontWeight(700).TextColor(pal.ink)
		ui.Spacer(c)
		if right != nil {
			right()
		}
	})
}

// rangePicker chooses among the shared ranges; it reports a change.
func rangePicker(c *ui.Context, sel *int) bool {
	return ui.Segmented(c, sel, tr("range7"), tr("range30"), tr("range90"), tr("rangeAll")).Label(tr("range")).Changed()
}

// swatch is a legend's colored square.
func swatch(c *ui.Context, col ui.Color) ui.Element {
	return ui.Box(c).Size(9, 9).Radius(2.5).Background(col).Shrink(0)
}

// share is a labeled horizontal bar with a value, for rankings.
func shareRow(c *ui.Context, pal palette, lead func(), label, value, sub string, frac float64, col ui.Color) ui.Element {
	return ui.Column(c).Gap(6).Children(func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			if lead != nil {
				lead()
			}
			ui.Text(c, label).FontSize(13).TextColor(pal.ink).NoWrap().Ellipsis("…").Shrink(1).MinWidth(0)
			ui.Spacer(c)
			if sub != "" {
				ui.Text(c, sub).FontSize(12).TextColor(pal.muted).NoWrap().FontFeatures("tnum")
			}
			ui.Text(c, value).FontSize(13).FontWeight(600).TextColor(pal.ink).NoWrap().FontFeatures("tnum")
		})
		bar(c, pal, frac, col).Height(5)
	})
}

// barMove is how bars glide to new lengths.
var barMove = ui.ElementTransition{Duration: 420 * time.Millisecond, Animation: &ui.Animation{Spring: ui.SpringOptions{Duration: 420 * time.Millisecond}}}

// bar is a track filled to frac.
func bar(c *ui.Context, pal palette, frac float64, col ui.Color) ui.Element {
	frac = min(max(frac, 0), 1)
	return ui.Box(c).FillWidth().Radius(3).Background(pal.well).Children(func() {
		if frac > 0 {
			ui.Box(c.Key("fill")).HeightPercent(100).WidthPercent(float32(frac * 100)).Radius(3).Background(col).Transition(barMove)
		}
	})
}

// ringPath is a full circle starting at the top and running clockwise, so
// that Trim(0, f) is the arc of a fraction f.
func ringPath(cx, cy, r float32) *ui.Path {
	const k = 0.5522847498
	p := &ui.Path{}
	p.MoveTo(cx, cy-r)
	p.CubeTo(cx+r*k, cy-r, cx+r, cy-r*k, cx+r, cy)
	p.CubeTo(cx+r, cy+r*k, cx+r*k, cy+r, cx, cy+r)
	p.CubeTo(cx-r*k, cy+r, cx-r, cy+r*k, cx-r, cy)
	p.CubeTo(cx-r, cy-r*k, cx-r*k, cy-r, cx, cy-r)
	return p
}

// ring is a quota ring: the arc springs to its value and brightens along
// its sweep, in the color of how much is used.
func ring(c *ui.Context, pal palette, key string, frac float64, size, width float32) ui.Element {
	frac = min(max(frac, 0), 1)
	box := ui.Box(c.Key(key)).Size(size, size).Shrink(0)
	v := box.AnimateTo("v", float32(frac), ui.Animation{Spring: ui.SpringOptions{Duration: 900 * time.Millisecond, Bounce: 0.18}})
	col := pal.usageColor(frac)
	well := pal.well
	box.Draw(func(p *ui.Painter, r ui.Rect) {
		cx, cy := r.X+r.W/2, r.Y+r.H/2
		rad := (min(r.W, r.H) - width) / 2
		p.StrokePath(ringPath(cx, cy, rad), width, well)
		if v <= 0.002 {
			return
		}
		end := min(max(v, 0.002), 1)
		p.StrokePathConicGradient(ringPath(cx, cy, rad).Trim(0, end), width, ui.ConicGradient{
			Stops: []ui.GradientStop{{Color: col.Alpha(0.45), At: 0}, {Color: col, At: end}},
			Oklab: true,
		})
		// A round cap at the head, a little brighter.
		hx, hy := pointOn(cx, cy, rad, end)
		p.FillPath((&ui.Path{}).Circle(hx, hy, width/2), col)
	})
	return box
}

// pointOn is the point at fraction f clockwise from the top of a circle.
func pointOn(cx, cy, r, f float32) (float32, float32) {
	a := float64(f) * 2 * math.Pi
	return cx + r*float32(math.Sin(a)), cy - r*float32(math.Cos(a))
}

// emptyState is a centered hint for a page or card without data.
func emptyState(c *ui.Context, pal palette, title, hint string) {
	ui.Column(c).Grow(1).Center().Gap(6).Padding(40).Children(func() {
		ui.Text(c, title).FontSize(15).FontWeight(600).TextColor(pal.ink)
		if hint != "" {
			ui.Text(c, hint).FontSize(12.5).TextColor(pal.muted).TextAlign(ui.Center).MaxWidth(360)
		}
	})
}

// errorLine shows an error in a card.
func errorLine(c *ui.Context, pal palette, msg string) {
	if msg == "" {
		return
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Padding(8, 10).Radius(8).Background(pal.bad.Alpha(0.10)).Children(func() {
		ui.Icon(c, icAlert).FontSize(14).TextColor(pal.bad)
		ui.Text(c, msg).FontSize(12).TextColor(pal.bad).Shrink(1).MinWidth(0)
	})
}

// pill is a small rounded tag.
func pill(c *ui.Context, text string, fg, bg ui.Color) ui.Element {
	return ui.Text(c, text).FontSize(11).FontWeight(600).TextColor(fg).Padding(2, 7).Radius(6).Background(bg).NoWrap()
}
