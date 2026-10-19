package gui

import (
	_ "embed"
	"math"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/shader"
)

// The live card's glow: a ring of light that flows around the card and an
// orb whose light drifts faster the faster tokens come out. Both are
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

// drawOrb draws the orb in r; energy runs from 0 (resting) to 1.
func drawOrb(p *ui.Painter, r ui.Rect, energy float32, live bool, pal palette) {
	if !p.Accelerated() {
		d := min(r.W, r.H) / 1.25
		p.FillGradient(ui.Rect{X: r.X + (r.W-d)/2, Y: r.Y + (r.H-d)/2, W: d, H: d},
			ui.LinearGradient{From: pal.accent2, To: pal.accent, Angle: 135, Oklab: true}, d/2)
		return
	}
	t := glowSecs(p) * (0.6 + 0.9*energy)
	if !live {
		t = 3
	}
	glowOrb.Draw(p, r, shader.Options{Values: orbValues.Float(orbPhase, t).Float(orbEnergy, energy)})
	if live {
		p.After(glowFrame)
	}
}
