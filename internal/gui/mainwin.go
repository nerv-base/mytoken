package gui

import (
	_ "embed"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/zzstar101/mytoken/internal/model"
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
	ui.Column(c).Width(sidebarWidth).FillHeight().Shrink(0).Background(pal.side).Padding(top, 10, 14, 10).Gap(18).Children(func() {
		ui.Row(c).Gap(9).AlignItems(ui.Center).Padding(0, 10).DragWindow().Children(func() {
			logo(c, pal, 22)
			ui.Text(c, "MyToken").Font(display).FontSize(16).FontWeight(700).TextColor(pal.ink)
		})
		a.nav(c, pal)
		ui.Scroll(c).Grow(1).Basis(0).FillWidth().Children(func() {
			a.sideToday(c, pal)
		})
		a.sidebarFooter(c, pal)
	})
}

// nav is the page list: an icon, a name and, for some pages, a live value;
// the selection slides from page to page.
func (a *App) nav(c *ui.Context, pal palette) {
	list := ui.Column(c.Key("nav")).Gap(2).Role(ui.RoleList).Label(tr("navigation"))
	mk := a.slide(c, list, "nav", "nav-"+a.page)
	sel := pal.selected
	list.Draw(func(p *ui.Painter, r ui.Rect) {
		if mk.W > 0 {
			p.Fill(ui.Rect{X: r.X + mk.X, Y: r.Y + mk.Y, W: mk.W, H: mk.H}, sel, 9)
		}
	})
	list.Children(func() {
		for _, it := range navItems {
			on := a.page == it.id
			row := ui.Row(c.Key(it.id)).Gap(10).AlignItems(ui.Center).Padding(7, 10).Radius(9).Cursor(ui.CursorPointer)
			a.mark("nav-"+it.id, row)
			if row.Hovered() && !on {
				row.Background(pal.hover.Alpha(0.6))
			}
			ink, icCol := pal.muted, pal.muted
			if on {
				ink, icCol = pal.ink, pal.accent
			} else if row.Hovered() {
				ink = pal.ink
			}
			row.Children(func() {
				ui.Icon(c, it.ic).FontSize(16).TextColor(row.AnimateColor("ic", icCol, quick)).Shrink(0)
				ui.Text(c, tr(it.id)).FontSize(13.5).FontWeight(map[bool]int{true: 650, false: 500}[on]).TextColor(row.AnimateColor("tx", ink, quick)).SingleLine().Shrink(1).MinWidth(0)
				ui.Spacer(c)
				a.navValue(c, pal, it.id)
			})
			if row.Clicked() {
				a.goTo(it.id)
			}
		}
	})
}

// navValue is a page's live value beside its name.
func (a *App) navValue(c *ui.Context, pal palette, page string) {
	if !a.ov.ready {
		return
	}
	switch page {
	case "overview":
		ui.Text(c, fmtUSDShort(a.ov.today.CostUSD)).FontSize(12).TextColor(pal.muted).FontFeatures("tnum").NoWrap().
			ContentTransition(ui.ContentTransition{Numeric: true})
	case "sessions":
		if n := len(a.ov.active); n > 0 {
			ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
				liveDot(c, pal)
				ui.Text(c, itoa(n)).FontSize(12).TextColor(pal.muted).FontFeatures("tnum").NoWrap()
			})
		}
	}
}

// liveDot is the green dot of something running, breathing unless motion
// is reduced.
func liveDot(c *ui.Context, pal palette) {
	good := pal.good
	still := c.Preferences().ReduceMotion
	ui.Box(c).Size(10, 10).Shrink(0).Draw(func(p *ui.Painter, r ui.Rect) {
		cx, cy := r.X+r.W/2, r.Y+r.H/2
		if !still {
			k := float32(p.Now().UnixMilli()%2000) / 2000
			rad := 3 + 2.5*k
			p.Fill(ui.Rect{X: cx - rad, Y: cy - rad, W: rad * 2, H: rad * 2}, good.Alpha(0.35*(1-k)), rad)
			p.After(time.Second / 20)
		}
		p.Fill(ui.Rect{X: cx - 3, Y: cy - 3, W: 6, H: 6}, good, 3)
	})
}

// sideToday lists today's spend by tool under the pages.
func (a *App) sideToday(c *ui.Context, pal palette) {
	if !a.ov.ready || len(a.ov.todayTools) == 0 {
		return
	}
	ui.Column(c).Gap(4).Children(func() {
		ui.Text(c, tr("today")).FontSize(11).FontWeight(650).TextColor(pal.faint).Padding(0, 10, 4, 10)
		for _, b := range a.ov.todayTools {
			if b.CostUSD <= 0 {
				continue
			}
			h := model.Harness(b.Key)
			ui.Row(c.Key("st-"+b.Key)).Gap(9).AlignItems(ui.Center).Padding(4, 10).
				Transition(ui.ElementTransition{Animation: &snappy, Position: true, Enter: &ui.Motion{Opacity: 0, Collapse: true}, Exit: &ui.Motion{Opacity: 0, Collapse: true}}).
				Children(func() {
					toolBadge(c, h, 18)
					ui.Text(c, h.DisplayName()).FontSize(12.5).TextColor(pal.ink.Alpha(0.85)).SingleLine().Shrink(1).MinWidth(0)
					ui.Spacer(c)
					ui.Text(c, fmtUSDShort(b.CostUSD)).FontSize(12).TextColor(pal.muted).FontFeatures("tnum").NoWrap().
						ContentTransition(ui.ContentTransition{Numeric: true})
				})
		}
	})
}

// sidebarFooter says what is running, or that nothing is.
func (a *App) sidebarFooter(c *ui.Context, pal palette) {
	if !a.ov.ready {
		return
	}
	act := a.ov.active
	ui.Row(c).Gap(8).AlignItems(ui.Center).Padding(10, 12).Radius(10).Background(pal.card.Alpha(0.6)).Children(func() {
		if len(act) == 0 {
			ui.Box(c).Size(6, 6).Radius(3).Background(pal.faint).Shrink(0)
			caption(c, pal, tr("idle")).FontSize(11.5)
			return
		}
		liveDot(c, pal)
		caption(c, pal, trf("sessionsLive", len(act))).FontSize(11.5).SingleLine()
	})
}

// logo is the app's mark, the app icon without its tile's margin: the
// live orb in a quota ring (resources/icon-src/render.py draws both).
func logo(c *ui.Context, pal palette, size float32) ui.Element {
	return ui.Image(c, logoBitmap).Size(size, size).Shrink(0)
}

//go:embed logo.png
var logoPNG []byte

var logoBitmap = func() *ui.Bitmap {
	b, err := ui.DecodeBitmap(logoPNG)
	if err != nil {
		panic(err)
	}
	return b
}()
