// Package gui is MyToken's native interface: the main window and the tray
// panel, drawn with MyGo's ui package.
//
// Look: opaque, quiet surfaces; color is reserved for money, quota and the
// tools themselves. Motion is limited to a few places where it carries
// meaning (quota rings, rolling figures, charts morphing between ranges, the
// tray's burn band) and is paint-only wherever MyGo allows it.
package gui

import (
	"embed"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/model"
)

// Faces. display is for big figures and titles; text for everything else.
const (
	display  = "SF Pro Display, system-ui"
	textFace = "SF Pro Text, system-ui"
)

// palette is one appearance's colors. The surfaces are quiet graphite or
// paper; the one accent is a calm blue; color otherwise belongs to the
// tools (their brands) and to state (good, warn, bad).
type palette struct {
	dark bool
	// bg is the window, side the sidebar, card a raised surface, raised a
	// tile inside a card, well a recessed track (bars, rings, inputs).
	bg, side, card, raised, well ui.Color
	// ink, muted and faint color text; line colors hairlines and grids.
	ink, muted, faint, line ui.Color
	hover, selected         ui.Color
	accent, accent2         ui.Color // accent2 is the accent's cooler partner in gradients
	good, warn, bad         ui.Color
	tip                     ui.Color // a tooltip's background
}

func paletteFor(dark bool) palette {
	if dark {
		return palette{
			dark: true,
			bg:   ui.Hex("#121214"), side: ui.Hex("#161619"), card: ui.Hex("#1C1C20"),
			raised: ui.RGBA(255, 255, 255, 0.035), well: ui.RGBA(255, 255, 255, 0.07),
			ink: ui.Hex("#EDEDF0"), muted: ui.RGBA(235, 235, 245, 0.56), faint: ui.RGBA(235, 235, 245, 0.32),
			line:  ui.RGBA(255, 255, 255, 0.06),
			hover: ui.RGBA(255, 255, 255, 0.05), selected: ui.RGBA(122, 162, 247, 0.16),
			accent: ui.Hex("#7AA2F7"), accent2: ui.Hex("#5FC9C0"),
			good: ui.Hex("#4CC38A"), warn: ui.Hex("#E5B454"), bad: ui.Hex("#EE6D6D"),
			tip: ui.Hex("#2A2A2F"),
		}
	}
	return palette{
		bg: ui.Hex("#F4F4F6"), side: ui.Hex("#EDEDF0"), card: ui.Hex("#FFFFFF"),
		raised: ui.RGBA(20, 20, 40, 0.025), well: ui.RGBA(20, 20, 40, 0.06),
		ink: ui.Hex("#1A1B1F"), muted: ui.RGBA(40, 42, 52, 0.60), faint: ui.RGBA(40, 42, 52, 0.36),
		line:  ui.RGBA(20, 20, 40, 0.07),
		hover: ui.RGBA(20, 20, 40, 0.04), selected: ui.RGBA(59, 111, 224, 0.10),
		accent: ui.Hex("#3B6FE0"), accent2: ui.Hex("#169C93"),
		good: ui.Hex("#1F9D62"), warn: ui.Hex("#C98A12"), bad: ui.Hex("#D9534F"),
		tip: ui.Hex("#FFFFFF"),
	}
}

// applyTheme sets MyToken's theme on c and returns the palette. dark
// overrides the system appearance when set.
func applyTheme(c *ui.Context, appearance string) palette {
	base := c.Theme()
	dark := base.Dark
	switch appearance {
	case "light":
		dark = false
	case "dark":
		dark = true
	}
	pal := paletteFor(dark)
	t := *base
	t.Dark = dark
	t.Accent = pal.accent
	t.AccentHover = pal.accent.Mix(ui.RGB(255, 255, 255), 0.12)
	t.AccentPressed = pal.accent.Mix(ui.RGB(0, 0, 0), 0.12)
	t.AccentText = ui.RGB(255, 255, 255)
	t.Text = pal.ink
	t.TextMuted = pal.muted
	t.Background = pal.bg
	t.Radius = 8
	t.Font = textFace
	if dark {
		t.Surface = ui.RGBA(255, 255, 255, 0.07)
		t.SurfaceHover = ui.RGBA(255, 255, 255, 0.11)
		t.SurfacePressed = ui.RGBA(255, 255, 255, 0.15)
		t.Border = ui.RGBA(255, 255, 255, 0.10)
	} else {
		t.Surface = ui.RGBA(255, 255, 255, 1)
		t.SurfaceHover = ui.Hex("#F4F4F7")
		t.SurfacePressed = ui.Hex("#EAEAEE")
		t.Border = ui.RGBA(20, 20, 40, 0.12)
	}
	c.SetTheme(&t)
	return pal
}

// usageColor is the color of a quota or share that is frac used.
func (p palette) usageColor(frac float64) ui.Color {
	switch {
	case frac >= 0.85:
		return p.bad
	case frac >= 0.7:
		return p.warn
	}
	return p.good
}

// series is the palette of chart series that are not tools (models,
// providers, projects), in the order they are given: muted, evenly spaced
// hues that sit together.
var series = []ui.Color{
	ui.Hex("#6F96EA"), ui.Hex("#4FB3A9"), ui.Hex("#D9A54C"), ui.Hex("#D97A8C"),
	ui.Hex("#9A86DB"), ui.Hex("#5BAAD1"), ui.Hex("#C9845A"), ui.Hex("#8BAF6A"),
}

func seriesAt(i int) ui.Color { return series[((i%len(series))+len(series))%len(series)] }

// otherColor colors the "everything else" bucket.
var otherColor = ui.Hex("#8E8E99")

// harnessColor gives each tool a stable color: a hue near its brand's, at
// one shared lightness and softness, so that tools side by side in a bar or
// a legend read as one family instead of a row of loud logos.
func harnessColor(h model.Harness) ui.Color {
	switch h {
	case model.ClaudeCode:
		return ui.Hex("#D98B6F")
	case model.ClaudeDesktop:
		return ui.Hex("#C9A27C")
	case model.Codex:
		return ui.Hex("#5AB497")
	case model.DSH:
		return ui.Hex("#7F98EC")
	case model.Pi:
		return ui.Hex("#56B2B8")
	case model.Gemini:
		return ui.Hex("#6AA5DD")
	case model.OpenCode:
		return ui.Hex("#9B8BD8")
	case model.Crush:
		return ui.Hex("#D985A0")
	case model.Cline:
		return ui.Hex("#8E96A8")
	case model.Roo:
		return ui.Hex("#CF8FB5")
	case model.Kilo:
		return ui.Hex("#D3AE62")
	case model.OpenClaude:
		return ui.Hex("#8FBA74")
	case model.Qwen:
		return ui.Hex("#8A8FE0")
	case model.Kimi:
		return ui.Hex("#6E9FE6")
	case model.Grok:
		return ui.Hex("#9097A3")
	case model.OpenClaw:
		return ui.Hex("#DE8A84")
	case model.Goose:
		return ui.Hex("#7FA88A")
	case model.Droid:
		return ui.Hex("#E09A6A")
	case model.Forge:
		return ui.Hex("#BF9676")
	case model.WorkBuddy:
		return ui.Hex("#5FBB9D")
	case model.CodeBuddy:
		return ui.Hex("#9A8FDF")
	case model.Hermes:
		return ui.Hex("#C9B16A")
	case model.Amp:
		return ui.Hex("#E0907F")
	case model.Kiro:
		return ui.Hex("#A98BE0")
	case model.Zed:
		return ui.Hex("#7095E0")
	case model.Antigravity:
		return ui.Hex("#73A0E8")
	case model.Codebuff:
		return ui.Hex("#67BFA0")
	case model.Mux:
		return ui.Hex("#8A95A6")
	case model.ZCode:
		return ui.Hex("#8D88E6")
	case model.MiniMaxCode:
		return ui.Hex("#E29B6E")
	case model.MiMoCode:
		return ui.Hex("#E5A877")
	case model.StepCode:
		return ui.Hex("#6F92E3")
	}
	return ui.Hex("#9297A3")
}

// Tool marks are the tools' own logos (the MIT-licensed @lobehub/icons set
// plus Droid's and OpenClaude's brand assets; brand/README.md lists every
// source). Colored logos keep their colors; one-color ones take the tool's
// color. Tools without a bundled mark show their initial.
//
//go:embed brand/*.svg
var brandFS embed.FS

type brandMark struct {
	svg     *ui.SVG
	colored bool
	tile    bool // the logo is its own rounded tile
}

func loadBrand(name string) *ui.SVG {
	b, err := brandFS.ReadFile("brand/" + name + ".svg")
	if err != nil {
		panic(err)
	}
	return ui.MustParseSVG(b)
}

// quotaHarness maps quota accounts that belong to a tracked tool to it, so
// they wear the tool's badge.
var quotaHarness = map[string]model.Harness{
	"codex":     model.Codex,
	"workbuddy": model.WorkBuddy,
	"codebuddy": model.CodeBuddy,
	"kimi":      model.Kimi,
	"deepseek":  model.DSH,
}

// quotaIcons are the marks of quota accounts without a tracked tool.
var quotaIcons = map[string]brandMark{
	"claude":  {loadBrand("claude-color"), true, false},
	"cursor":  {loadBrand("cursor"), false, false},
	"trae":    {loadBrand("trae-color"), true, false},
	"qoder":   {loadBrand("qoder-color"), true, false},
	"copilot": {loadBrand("githubcopilot"), false, false},
	"zcode":   {loadBrand("zai"), false, false},
}

var harnessIcons = func() map[model.Harness]brandMark {
	load := loadBrand
	return map[model.Harness]brandMark{
		model.ClaudeCode:    {load("claudecode-color"), true, false},
		model.Codex:         {load("openai"), false, false},
		model.Gemini:        {load("gemini-color"), true, false},
		model.DSH:           {load("deepseek-color"), true, false},
		model.OpenCode:      {load("opencode"), false, false},
		model.Cline:         {load("cline"), false, false},
		model.Roo:           {load("roocode"), false, false},
		model.Kilo:          {load("kilocode"), false, false},
		model.Pi:            {load("pi"), false, false},
		model.Crush:         {icon(`<path d="M12 19.5s-7.5-4.4-7.5-10A4.2 4.2 0 0 1 12 7a4.2 4.2 0 0 1 7.5 2.5c0 5.6-7.5 10-7.5 10z"/>`), false, false},
		model.ClaudeDesktop: {load("claude-color"), true, false},
		model.CodeBuddy:     {load("codebuddy-color"), true, true},
		model.Hermes:        {load("hermesagent"), false, false},
		model.Qwen:          {load("qwen-color"), true, false},
		model.Kimi:          {load("kimi-color"), true, false},
		model.Grok:          {load("grok"), false, false},
		model.OpenClaw:      {load("openclaw-color"), true, false},
		model.Goose:         {load("goose"), false, false},
		model.WorkBuddy:     {load("workbuddy-color"), true, true},
		model.Droid:         {load("droid-color"), true, true},
		model.OpenClaude:    {load("openclaude-color"), true, false},
		model.Forge:         {load("forge"), false, false},
		model.Amp:           {load("amp-color"), true, false},
		model.Kiro:          {load("kiro-color"), true, true},
		model.Antigravity:   {load("antigravity-color"), true, false},
		model.Zed:           {load("zed"), false, false},
		model.Mux:           {load("mux"), false, false},
		model.ZCode:         {load("zai"), false, false},
		model.MiniMaxCode:   {load("minimax-color"), true, false},
		model.MiMoCode:      {load("xiaomimimo"), false, false},
		model.StepCode:      {load("stepfun-color"), true, false},
	}
}()

// toolBadge is a tool's logo on a tinted rounded square.
func toolBadge(c *ui.Context, h model.Harness, box float32) ui.Element {
	m, ok := harnessIcons[h]
	if ok && m.tile {
		return ui.Image(c, m.svg).Size(box, box).Shrink(0)
	}
	col := harnessColor(h)
	return ui.Box(c).Size(box, box).Radius(box * 0.28).Center().Shrink(0).Background(col.Alpha(0.14)).Children(func() {
		switch {
		case !ok:
			name := []rune(h.DisplayName())
			if len(name) == 0 {
				name = []rune("?")
			}
			ui.Text(c, string(name[0])).FontSize(box * 0.5).Bold().TextColor(col)
		case m.colored:
			ui.Image(c, m.svg).Size(box*0.58, box*0.58).Shrink(0)
		default:
			ui.Icon(c, m.svg).FontSize(box * 0.58).TextColor(col).Shrink(0)
		}
	})
}

// icon parses a 24×24 stroked icon drawn in currentColor.
func icon(shapes string) *ui.SVG {
	return ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">` + shapes + `</svg>`))
}

var (
	icOverview = icon(`<rect x="3" y="3" width="7" height="9" rx="2"/><rect x="14" y="3" width="7" height="5" rx="2"/><rect x="14" y="12" width="7" height="9" rx="2"/><rect x="3" y="16" width="7" height="5" rx="2"/>`)
	icSessions = icon(`<path d="M4 6h16M4 12h16M4 18h10"/>`)
	icRanking  = icon(`<path d="M8 21V11M16 21V7M12 21V3M4 21h16"/>`)
	icProjects = icon(`<path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/>`)
	icSettings = icon(`<circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.8-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1.1-1.5 1.7 1.7 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.8 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.5-1.1 1.7 1.7 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.8.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.8V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1z"/>`)
	icGeneral  = icon(`<path d="M4 21v-7M4 10V3M12 21v-9M12 8V3M20 21v-5M20 12V3M1 14h6M9 8h6M17 16h6"/>`)
	icSources  = icon(`<ellipse cx="12" cy="5" rx="8" ry="3"/><path d="M4 5v6c0 1.7 3.6 3 8 3s8-1.3 8-3V5M4 11v6c0 1.7 3.6 3 8 3s8-1.3 8-3v-6"/>`)
	icPrice    = icon(`<path d="M12 2v20M17 5H9.5a3.5 3.5 0 0 0 0 7h5a3.5 3.5 0 0 1 0 7H6"/>`)
	icDatabase = icon(`<path d="M21 12a9 9 0 0 1-15.5 6.2L3 16"/><path d="M3 12A9 9 0 0 1 18.5 5.8L21 8"/><path d="M3 21v-5h5M21 3v5h-5"/>`)
	icSearch   = icon(`<circle cx="11" cy="11" r="7"/><path d="m20 20-3.5-3.5"/>`)
	icFolder   = icon(`<path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/>`)
	icClock    = icon(`<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>`)
	icWindow   = icon(`<rect x="3" y="4" width="18" height="16" rx="3"/><path d="M3 9h18"/>`)
	icPower    = icon(`<path d="M12 3v9"/><path d="M6.4 6.4a8 8 0 1 0 11.2 0"/>`)
	icRefresh  = icon(`<path d="M21 12a9 9 0 0 1-15.5 6.2L3 16"/><path d="M3 12A9 9 0 0 1 18.5 5.8L21 8"/><path d="M3 21v-5h5M21 3v5h-5"/>`)
	icChevron  = icon(`<path d="m9 6 6 6-6 6"/>`)
	icBack     = icon(`<path d="m15 6-6 6 6 6"/>`)
	icBolt     = icon(`<path d="M13 2 4 14h7l-1 8 9-12h-7z"/>`)
	icCheck    = icon(`<path d="m5 12 5 5 9-10"/>`)
	icAlert    = icon(`<path d="M12 9v4M12 17h.01"/><path d="M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z"/>`)
	icTrash    = icon(`<path d="M3 6h18M8 6V4h8v2M6 6l1 14h10l1-14"/>`)
)
