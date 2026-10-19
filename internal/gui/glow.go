package gui

import (
	_ "embed"
	"math"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/shader"
)

// The running session's orb: drifting light that breathes, livelier the
// busier the session.
//
// The shader in glow/ is MyGo's chat-glow example (examples/chat-glow,
// MIT License, Copyright (c) 2026 MyGo contributors; see glow/LICENSE). It is
// paint-only and asks for its next frame itself, only
// while a session is running; at rest it draws once and stops.

var (
	//go:embed glow/palette.kage
	glowPalette string
	//go:embed glow/orb.kage
	glowOrbSource string

	glowOrb = shader.MustCompile("package main\n" + glowPalette + glowOrbSource)

	orbPhase, orbEnergy = glowOrb.Uniform("Phase"), glowOrb.Uniform("Energy")

	orbValues = glowOrb.NewValues()

	glowEpoch = time.Now()
)

// glowFrame paces the orb: its light drifts slowly, 24 frames a second
// is smooth and costs less than the display's rate.
const glowFrame = time.Second / 24

func glowSecs(p *ui.Painter) float32 { return float32(p.Now().Sub(glowEpoch).Seconds()) }

// drawOrb draws the orb in r; energy runs from 0 (resting) to 1 (busy).
// While live it breathes: it swells and settles every few seconds, its
// light brightening and its blobs folding further on each breath.
func drawOrb(p *ui.Painter, r ui.Rect, energy float32, live bool, pal palette) {
	if !p.Accelerated() {
		d := min(r.W, r.H) / 1.25
		p.FillGradient(ui.Rect{X: r.X + (r.W-d)/2, Y: r.Y + (r.H-d)/2, W: d, H: d},
			ui.LinearGradient{From: pal.accent2, To: pal.accent, Angle: 135, Oklab: true}, d/2)
		return
	}
	secs := glowSecs(p)
	t, e, k := float32(3), float32(0.15), float32(1)
	if live {
		// One breath every 4.2 s, a little quicker when busy.
		period := 4.2 - 1.2*float64(energy)
		b := float32(0.5 - 0.5*math.Cos(2*math.Pi*float64(secs)/period)) // 0..1..0
		t = secs * (1.1 + 0.8*energy)
		e = 0.35 + 0.35*energy + 0.3*b
		k = 0.955 + 0.045*b
	}
	d := min(r.W, r.H) * k
	rr := ui.Rect{X: r.X + (r.W-d)/2, Y: r.Y + (r.H-d)/2, W: d, H: d}
	glowOrb.Draw(p, rr, shader.Options{Values: orbValues.Float(orbPhase, t).Float(orbEnergy, e)})
	if live {
		p.After(glowFrame)
	}
}
