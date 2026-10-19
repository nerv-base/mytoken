package gui

import (
	"fmt"
	"os"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

// rankingPage ranks providers, models and harnesses over the chosen span.
func (s *State) rankingPage(c *ui.Context, pal palette) {
	ov := s.ov
	pageHeader(c, pal, "ranking", "", func() { s.spanSwitch(c, pal) })
	ui.Scroll(c).Grow(1).Children(func() {
		ui.Column(c).Padding(8, 8, 16, 8).Gap(12).Children(func() {
			ui.Row(c).Children(func() {
				segmentedGroup(c, pal, &s.rankTab, tr("rankingBy"), tr("byModel"), tr("byProvider"), tr("byHarness"))
			})
			if s.rankTab == 1 {
				// Providers are endpoint identities, not attribution names:
				// this tab renders query.ProviderGroup rows with their stable
				// Key, their provenance members and a display-only alias.
				s.providersTab(c, pal)
				return
			}
			var bs []query.Bucket
			switch s.rankTab {
			case 2:
				bs = ov.Harnesses
			default:
				bs = ov.Models
			}
			if len(bs) == 0 {
				pane(c, pal).Padding(40).Children(func() { emptyState(c, pal, artStaff, tr("noData"), tr("noDataSub")) })
				return
			}
			// The top three on a stage: lit steps with big numerals, then
			// everyone in a table.
			pane(c, pal).Clip().Padding(22, 26, 0, 26).Children(func() {
				ui.Row(c).Gap(18).AlignItems(ui.End).Children(func() {
					for _, i := range []int{1, 0, 2} {
						if i >= len(bs) {
							ui.Box(c.Key(i)).Grow(1).Basis(0)
							continue
						}
						s.podiumPlace(c.Key(bs[i].Key), pal, bs[i], i)
					}
				})
			})
			card(c, pal, "", func() { classLegend(c, pal) }, func() {
				s.rankTable(c, pal, bs)
			})
		})
	})
}

// rankTable is the leaderboard, drawn as the native table so its rows scroll,
// hover and name themselves for screen readers; sorting and the column order
// stay in the page.
func (s *State) rankTable(c *ui.Context, pal palette, bs []query.Bucket) {
	var all int64
	for _, b := range bs {
		all += b.Tokens.Total()
	}
	holder := ui.Box(c).FillWidth().Height(min(float32(420), 34+44*float32(len(bs))))
	ls := ui.Local(holder, "rankList", func() ui.ListState { return ui.ListState{} })
	if ls == nil {
		return
	}
	ls.Key = func(row int) any { return bs[row].Key }
	ls.Label = func(row int) string {
		if row < 0 || row >= len(bs) {
			return ""
		}
		if bs[row].Label != "" {
			return bs[row].Label
		}
		return bs[row].Key
	}
	ls.RoundRows = true
	cols := []ui.TableColumn{
		{Title: "#", ID: "rank", Width: 34, Align: ui.End, Fixed: true},
		{Title: "", ID: "name", MinWidth: 140},
		{Title: tr("share"), ID: "share", Width: 54, Align: ui.End, Fixed: true},
		{Title: tr("requests"), ID: "requests", Width: 70, Align: ui.End, Fixed: true},
		{Title: tr("cost"), ID: "cost", Width: 74, Align: ui.End, Fixed: true},
		{Title: tr("tokens"), ID: "tokens", Width: 80, Align: ui.End, Fixed: true},
	}
	holder.Children(func() {
		ui.Table(c, ls, cols, len(bs), func(row, col int) {
			b := bs[row]
			switch col {
			case 0:
				num := ui.Text(c, fmt.Sprint(row+1)).Font(display).FontSize(15).FontWeight(700).TextColor(pal.muted).FontFeatures("tnum")
				if row < 3 {
					num.TextColor(bandAt(row))
				}
			case 1:
				ui.Column(c).Gap(5).Children(func() {
					ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
						if s.rankTab == 2 {
							harnessMark(c, model.Harness(b.Key), 15)
						} else {
							dot(c, bandAt(row), 8)
						}
						label := b.Label
						if label == "" {
							label = b.Key
						}
						ui.Text(c, label).FontSize(13).FontWeight(600).TextColor(pal.ink).SingleLine().Ellipsis("…")
					})
					classBar(c, pal, b.Tokens, 4)
				})
			case 2:
				share := 0.0
				if all > 0 {
					share = float64(b.Tokens.Total()) / float64(all)
				}
				ui.Text(c, fmtPct(share)).FontSize(12).TextColor(pal.muted).FontFeatures("tnum")
			case 3:
				ui.Text(c, fmtInt(b.Requests)).FontSize(12).TextColor(pal.muted).FontFeatures("tnum")
			case 4:
				ui.Text(c, fmtCostOf(b.CostUSD, b.Requests, b.Unpriced)).FontSize(12).TextColor(pal.ink).FontFeatures("tnum")
			case 5:
				ui.Text(c, fmtTokens(b.Tokens.Total())).FontSize(13).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum")
			}
		})
	})
}

// projectsPage shows usage per working directory.
func (s *State) projectsPage(c *ui.Context, pal palette) {
	ov := s.ov
	pageHeader(c, pal, "projects", "", func() { s.spanSwitch(c, pal) })
	ui.Scroll(c).Grow(1).Children(func() {
		ui.Column(c).Padding(8, 8, 16, 8).Gap(12).Children(func() {
			if len(ov.Projects) == 0 {
				pane(c, pal).Padding(40).Children(func() { emptyState(c, pal, artStaff, tr("noData"), tr("noDataSub")) })
				return
			}
			var top int64 = 1
			for _, b := range ov.Projects {
				if v := b.Tokens.Total(); v > top {
					top = v
				}
			}
			var all int64
			for _, b := range ov.Projects {
				all += b.Tokens.Total()
			}
			ui.Grid(c).Columns(2).Gap(14).Children(func() {
				for i, b := range ov.Projects {
					col := bandAt(i)
					share := float64(b.Tokens.Total()) / float64(max(all, 1))
					card := pane(c.Key(b.Key), pal).Padding(18, 22, 18, 22).Gap(10).Transition(hoverFade)
					if card.Hovered() {
						card.Border(1, col.Alpha(0.45))
					}
					card.Children(func() {
						ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
							ui.Text(c, fmt.Sprintf("%02d", i+1)).Font(display).FontSize(13).FontWeight(750).TextColor(col).FontFeatures("tnum")
							ui.Spacer(c)
							ui.Text(c, fmtPct(share)).FontSize(11).FontWeight(650).TextColor(pal.muted).FontFeatures("tnum")
						})
						ui.Row(c).Gap(12).AlignItems(ui.End).Children(func() {
							ui.Column(c).Grow(1).Basis(0).Gap(3).Children(func() {
								name := baseName(b.Key)
								if name == "" {
									name = tr("untitled")
								}
								ui.Text(c, name).FontSize(17).FontWeight(720).TextColor(pal.ink).SingleLine().Ellipsis("…")
								ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
									ui.Icon(c, icFolder).FontSize(11).TextColor(pal.muted)
									ui.Text(c, shortPath(b.Key)).FontSize(11).TextColor(pal.muted).SingleLine().Ellipsis("…")
								})
							})
							bigNumber(c, pal, fmtTokens(b.Tokens.Total()), 40)
						})
						rankBar(c, pal, float64(b.Tokens.Total())/float64(top), col, 4)
						ui.Row(c).Gap(22).Children(func() {
							miniStat(c, pal, tr("cost"), fmtCostOf(b.CostUSD, b.Requests, b.Unpriced))
							miniStat(c, pal, tr("sessions"), fmtInt(b.Sessions))
							miniStat(c, pal, tr("requests"), fmtInt(b.Requests))
						})
					})
				}
			})
		})
	})
}

// updateRows are the daily update check and the button that checks now.
func (s *State) updateRows(c *ui.Context, pal palette) {
	sub := tr("autoUpdateSub")
	if s.Hooks.CanUpdate != nil && !s.Hooks.CanUpdate() {
		sub = tr("noSelfUpdate")
	}
	settingRow(c, pal, tr("autoUpdate"), sub, func() {
		on := s.Hooks.AutoUpdates != nil && s.Hooks.AutoUpdates()
		if ui.Switch(c, &on).Changed() && s.Hooks.SetAutoUpdates != nil {
			s.Hooks.SetAutoUpdates(on)
		}
	})
	ui.Divider(c)
	settingRow(c, pal, tr("checkUpdates"), fmt.Sprintf(tr("currentVersion"), s.Hooks.Version), func() {
		if ui.Button(c, tr("checkUpdates")).Clicked() {
			s.Hooks.CheckUpdates()
		}
	})
}

// settingsPage holds the few preferences there are.
func (s *State) settingsPage(c *ui.Context, pal palette) {
	pageHeader(c, pal, "settings", "", nil)
	ui.Scroll(c).Grow(1).Children(func() {
		ui.Row(c).Padding(8, 8, 16, 8).Gap(14).AlignItems(ui.Start).Children(func() {
			ui.Column(c).Grow(1).Basis(0).Gap(14).Children(func() {
				card(c, pal, "", nil, func() {
					settingRow(c, pal, tr("launchAtLogin"), tr("launchAtLoginSub"), func() {
						on := s.Hooks.OpenAtLogin != nil && s.Hooks.OpenAtLogin()
						if ui.Switch(c, &on).Changed() && s.Hooks.SetOpenAtLogin != nil {
							s.Hooks.SetOpenAtLogin(on)
						}
					})
					if s.Hooks.CheckUpdates != nil {
						ui.Divider(c)
						s.updateRows(c, pal)
					}
					ui.Divider(c)
					settingRow(c, pal, tr("rebuild"), tr("rebuildSub"), func() {
						if ui.Button(c, tr("rebuild")).Clicked() {
							s.rebuild()
						}
					})
					if s.Hooks.DataDir != "" {
						ui.Divider(c)
						settingRow(c, pal, tr("dataDir"), s.Hooks.DataDir, nil)
					}
				})
			})
			ui.Column(c).Width(360).Shrink(0).Gap(14).Children(func() {
				// Liner notes.
				pane(c, pal).Clip().Padding(22, 22, 20, 22).Gap(10).Draw(func(p *ui.Painter, r ui.Rect) {
					softGlow(p, r.X+r.W*0.85, r.Y+r.H*0.1, r.W*0.5, Tomori.Alpha(0.14))
					for i, col := range Band {
						if i >= 5 {
							break
						}
						star(p, r.X+r.W-28-float32(i)*15, r.Y+r.H-22-float32(i%2)*8, 3+float32(i%3), col.Alpha(0.6))
					}
				}).Children(func() {
					kicker(c, pal, tr("about"))
					logo(c, pal, 30)
					ui.Text(c, aboutVersion(s.Hooks.Version)+" · MIT").FontSize(12).TextColor(pal.muted).FontFeatures("tnum")
					ui.Text(c, "春日影は、もう演奏しない").FontSize(13.5).FontWeight(600).TextColor(pal.ink.Alpha(0.8))
					ui.Text(c, "Built with MyGo").FontSize(11).TextColor(pal.muted)
				})
			})
		})
	})
}

func settingRow(c *ui.Context, pal palette, title, sub string, control func()) {
	ui.Row(c.Key(title)).Gap(12).Padding(6, 0).AlignItems(ui.Center).Children(func() {
		ui.Column(c).Grow(1).Basis(0).Gap(2).Children(func() {
			ui.Text(c, title).FontSize(13.5).FontWeight(650).TextColor(pal.ink)
			ui.Text(c, sub).FontSize(11.5).TextColor(pal.muted)
		})
		if control != nil {
			control()
		}
	})
}

func aboutVersion(v string) string {
	if v == "" {
		return "MyToken"
	}
	return "MyToken " + v
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// podiumPlace is one of the top three: name and figures over a lit step
// whose height follows the place, its numeral painted large.
func (s *State) podiumPlace(c *ui.Context, pal palette, b query.Bucket, i int) {
	col := bandAt(i)
	step := []float32{96, 72, 56}[i]
	ui.Column(c).Grow(1).Basis(0).Gap(8).Children(func() {
		ui.Column(c).Gap(6).Padding(0, 4).Children(func() {
			ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
				if s.rankTab == 2 {
					harnessMark(c, model.Harness(b.Key), 16)
				} else {
					dot(c, col, 7)
				}
				label := b.Label
				if label == "" {
					label = b.Key
				}
				ui.Text(c, label).FontSize(13.5).FontWeight(680).TextColor(pal.ink).SingleLine().Ellipsis("…").Shrink(1)
				if i == 0 {
					ui.Icon(c, icSparkle).FontSize(13).TextColor(Soyo).Shrink(0)
				}
			})
			bigNumber(c, pal, fmtTokens(b.Tokens.Total()), []float32{52, 40, 36}[i])
			ui.Row(c).Gap(10).Children(func() {
				ui.Text(c, fmtCostOf(b.CostUSD, b.Requests, b.Unpriced)).FontSize(11.5).TextColor(pal.muted).FontFeatures("tnum")
				ui.Text(c, trf("requestsN", fmtInt(b.Requests))).FontSize(11.5).TextColor(pal.muted)
			})
		})
		el := ui.Box(c).FillWidth().Height(step + 14)
		grow := entrance(el, time.Duration(600+i*140)*time.Millisecond)
		el.Draw(func(p *ui.Painter, r ui.Rect) {
			h := step * grow
			sr := ui.Rect{X: r.X, Y: r.Y + r.H - h, W: r.W, H: h + 20}
			if i == 0 {
				softGlow(p, sr.X+sr.W/2, sr.Y, sr.W*0.55, col.Alpha(0.22))
			}
			p.FillGradient(sr, ui.LinearGradient{From: col.Alpha(0.30), To: col.Alpha(0.04), Angle: 180}, 14)
			p.Fill(ui.Rect{X: sr.X + 14, Y: sr.Y, W: sr.W - 28, H: 2}, col.Alpha(0.9), 1)
			n := ui.Span{Text: fmt.Sprint(i + 1), Size: []float32{60, 50, 40}[i], Font: display, Weight: 800, Color: col.Alpha(0.85)}
			w, _ := p.MeasureText(0, n)
			p.Clip(r, 0, func() { p.RichText(sr.X+(sr.W-w)/2, sr.Y+4, 0, n) })
		})
	})
}
