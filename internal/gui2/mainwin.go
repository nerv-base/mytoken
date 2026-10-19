package gui

import (
	"github.com/egoist/mygo/ui"
)

// Window geometry the app's main.go uses.
const (
	// TrafficLightX and TrafficLightY place macOS's window buttons inside
	// the sidebar, above the logo.
	TrafficLightX = 18
	TrafficLightY = 18
	sidebarWidth  = 208
	dragHeight    = 30
)

var navItems = []struct {
	id string
	ic *ui.SVG
}{{"overview", icOverview}, {"sessions", icSessions}, {"ranking", icRanking}, {"projects", icProjects}, {"settings", icSettings}}

func (a *App) appearance() string {
	if a.d.Prefs == nil {
		return ""
	}
	return a.d.Prefs.Pref(PrefAppearance)
}

// MainView is the main window's content.
func (a *App) MainView(c *ui.Context) {
	pal := applyTheme(c, a.appearance())
	c.Root().Background(pal.bg)
	ui.Row(c).Fill().Children(func() {
		a.sidebar(c, pal)
		ui.Box(c).Width(1).FillHeight().Background(pal.line).Shrink(0)
		ui.Column(c.Key("page-" + a.page)).Grow(1).Basis(0).FillHeight().MinWidth(0).Children(func() {
			// The window drags by the strip above the page, as by a title bar.
			bar := c.TitleBar()
			ui.Box(c).FillWidth().Height(max(dragHeight, bar.Height)).Shrink(0).DragWindow()
			switch a.page {
			case "sessions":
				a.sessionsPage(c, pal)
			case "ranking":
				scrollPage(c, func() { a.rankingPage(c, pal) })
			case "projects":
				a.projectsPage(c, pal)
			case "settings":
				a.settingsPage(c, pal)
			default:
				scrollPage(c, func() { a.overviewPage(c, pal) })
			}
		})
	})
}

// scrollPage is the scrolling body of a page.
func scrollPage(c *ui.Context, body func()) {
	ui.Scroll(c).Grow(1).Basis(0).FillWidth().Children(func() {
		ui.Column(c).Padding(0, 28, 28, 28).Gap(16).MaxWidth(1240).FillWidth().Children(body)
	})
}

func (a *App) sidebar(c *ui.Context, pal palette) {
	bar := c.TitleBar()
	top := float32(22)
	if bar.Left > 0 {
		// The window buttons sit at the top left: the logo goes below them.
		top = TrafficLightY + 30
	}
	ui.Column(c).Width(sidebarWidth).FillHeight().Shrink(0).Background(pal.side).Padding(top, 10, 14, 10).Gap(14).Children(func() {
		ui.Row(c).Gap(9).AlignItems(ui.Center).Padding(0, 10).DragWindow().Children(func() {
			logo(c, pal, 22)
			ui.Text(c, "MyToken").Font(display).FontSize(16).FontWeight(700).TextColor(pal.ink)
		})
		page := a.page
		if ui.Sidebar(c, &page, func() {
			for _, it := range navItems {
				ui.SidebarItem(c, it.id, it.ic, tr(it.id))
			}
		}).Grow(1).Background(pal.side).Padding(0, 0).Label(tr("navigation")).Changed() {
			a.goTo(page)
		}
		a.sidebarFooter(c, pal)
	})
}

// sidebarFooter keeps today's spend in sight on every page.
func (a *App) sidebarFooter(c *ui.Context, pal palette) {
	if !a.ov.ready {
		return
	}
	ui.Column(c).Gap(2).Padding(10, 12).Radius(10).Background(pal.card.Alpha(0.6)).Children(func() {
		caption(c, pal, tr("today")+" · "+tr("apiEquivalent")).FontSize(11)
		figure(c, pal, "side-today", fmtUSD(a.ov.today.CostUSD), 18)
		if n := len(a.ov.active); n > 0 {
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Box(c).Size(6, 6).Radius(3).Background(pal.good)
				caption(c, pal, trf("activeN", n)).FontSize(11)
			})
		}
	})
}

// logo is the app's mark: a rounded square with a rising bar.
func logo(c *ui.Context, pal palette, size float32) ui.Element {
	accent := pal.accent
	return ui.Box(c).Size(size, size).Shrink(0).Draw(func(p *ui.Painter, r ui.Rect) {
		p.FillGradient(r, ui.LinearGradient{From: accent.Mix(ui.RGB(255, 255, 255), 0.25), To: accent.Mix(ui.RGB(0, 0, 0), 0.1), Angle: 160, Oklab: true}, r.W*0.28)
		w := r.W * 0.13
		base := r.Y + r.H*0.76
		for i, h := range []float32{0.22, 0.36, 0.52} {
			x := r.X + r.W*0.24 + float32(i)*r.W*0.2
			p.Fill(ui.Rect{X: x, Y: base - r.H*h, W: w, H: r.H * h}, ui.RGBA(255, 255, 255, 0.92), w/2)
		}
	})
}
