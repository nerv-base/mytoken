package gui

import (
	_ "embed"
	"math"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/shader"
)

// The live card's glow: a ring of light that flows around the card and an
// orb of drifting light that breathes, livelier the busier the session.
//
// The shaders in glow/ are MyGo's chat-glow example (examples/chat-glow,
// MIT License, Copyright (c) 2026 MyGo contributors; see glow/LICENSE). Both are
// paint-only and ask for their next frame themselves, at 30 fps, and only
// while a session is running; at rest they draw once and stop.

var (
	//go:embed glow/palette.kage
	glowPalette string
	//go:embed glow/ring.kage
	glowRingSource string
	//go:embed glow/orb.kage
	glowOrbSource string

	glowRing = shader.MustCompile("package main\n" + glowPalette + glowRingSource)
	glowOrb  = shader.MustCompile("package main\n" + glowPalette + glowOrbSource)

	ringTime, ringStrength, ringPad, ringRadius = glowRing.Uniform("Time"), glowRing.Uniform("Strength"), glowRing.Uniform("Pad"), glowRing.Uniform("Radius")
	orbPhase, orbEnergy                         = glowOrb.Uniform("Phase"), glowOrb.Uniform("Energy")

	ringValues = glowRing.NewValues()
	orbValues  = glowOrb.NewValues()

	glowEpoch = time.Now()
)

const glowFrame = time.Second / 30

func glowSecs(p *ui.Painter) float32 { return float32(p.Now().Sub(glowEpoch).Seconds()) }

// drawRing draws the flowing ring inset pad in r around a card of corner
// radius rad; strength runs from 0 (off) to 1.
func drawRing(p *ui.Painter, r ui.Rect, pad, rad float32, strength float32, live bool) {
	if strength <= 0.01 {
		return
	}
	if !p.Accelerated() {
		return
	}
	t := glowSecs(p)
	if !live {
		t = 0
	}
	s := strength * (0.55 + 0.15*float32(math.Sin(float64(t)*1.3)))
	glowRing.Draw(p, r, shader.Options{Values: ringValues.
		Float(ringTime, t).Float(ringStrength, s).Float(ringPad, pad).Float(ringRadius, rad)})
	if live {
		p.After(glowFrame)
	}
}

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
