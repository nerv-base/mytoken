package gui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/query"
)

// The provider views speak in endpoint groups, not attribution names. A group
// is one stable endpoint identity (query.ProviderGroup): usage merged across
// the harnesses and providers that resolved to it, with those members kept for
// provenance. Its Key is the identity — never a display name — so two
// endpoints that share a label stay apart everywhere.
//
// An alias renames a group for display only. It is offered for identified
// endpoints only, and it never reaches pricing: endpoint IDs are not billing
// accounts and are never passed to the price APIs.

// providerView is the providers tab's selection and its alias editor. The
// selection is the group's stable Key, so a rename cannot move it.
type providerView struct {
	key   string // the selected endpoint group's Key, "" for none
	alias string // what is being typed into the alias field
	msg   string // what the last alias write said
	err   string // what it said when it failed
	busy  bool   // a write is in flight
}

// providerKeyShort is a compact stand-in for a group's stable key: its tail,
// cut to ten characters. It is a truncation, not an identity — two different
// keys can share it — so it is only for tight rows; the detail view prints the
// whole key. Once an alias is set the group no longer exposes its raw
// endpoint, so this is the closest thing the compact rows have to a name.
func providerKeyShort(key string) string {
	if i := strings.LastIndex(key, ":"); i >= 0 {
		key = key[i+1:]
	}
	if len(key) > 10 {
		return key[:10] + "…"
	}
	return key
}

// providerLabel is a group's display name; never empty.
func providerLabel(g query.ProviderGroup) string {
	if l := strings.TrimSpace(g.Label); l != "" {
		return l
	}
	return providerKeyShort(g.Key)
}

// providerIdentity is the caption that keeps look-alike groups apart: the
// stable key's short form, and an honest note when the endpoint could not be
// identified. The short form is only a hint; the detail shows the whole key.
func providerIdentity(g query.ProviderGroup) string {
	if g.Identified {
		return tr("providerIdentity") + " " + providerKeyShort(g.Key)
	}
	return tr("providerUnidentified") + " " + providerKeyShort(g.Key)
}

// providerMembersText names the harness/provider pairs behind a group.
func providerMembersText(g query.ProviderGroup) string {
	parts := make([]string, 0, len(g.Members))
	for _, m := range g.Members {
		name := m.Harness.DisplayName()
		if m.Provider == "" {
			parts = append(parts, name)
			continue
		}
		parts = append(parts, name+"·"+m.Provider)
	}
	return strings.Join(parts, "  ·  ")
}

// providerDrill is the sessions page's provider drilldown: the stable id the
// list is narrowed to and the display name to show for it.
type providerDrill struct {
	Key   string
	Label string
}

// showProviderSessions opens the sessions page narrowed to one endpoint. The
// filter carries the group's stable id, never its display name: a name may be
// shared by several endpoints, and the sessions listed must keep the provider
// they were actually priced under.
func (s *State) showProviderSessions(g query.ProviderGroup) {
	if g.Key == "" {
		return
	}
	s.provFilter = providerDrill{Key: g.Key, Label: providerLabel(g)}
	s.page = "sessions"
	s.clearSelection()
	s.loadSessions()
}

// clearProviderSessions drops the drilldown and lists every session again.
func (s *State) clearProviderSessions() {
	if s.provFilter.Key == "" {
		return
	}
	s.provFilter = providerDrill{}
	s.clearSelection()
	s.loadSessions()
}

// providerFilterBanner names the endpoint the sessions list is narrowed to.
func (s *State) providerFilterBanner(c *ui.Context, pal palette) {
	pane(c, pal).Padding(9, 14, 9, 14).Margin(0, 8, 8, 8).Children(func() {
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			kicker(c, pal, tr("providerSessionsFilter"))
			ui.Text(c, s.provFilter.Label).FontSize(12.5).FontWeight(650).TextColor(pal.ink).SingleLine().Ellipsis("…")
			ui.Text(c, providerKeyShort(s.provFilter.Key)).FontSize(11).TextColor(pal.muted).FontFeatures("tnum").Shrink(0)
			ui.Spacer(c)
			if ui.Button(c, tr("providerSessionsClear")).Clicked() {
				s.clearProviderSessions()
			}
		})
	})
}

// selectedProvider is the group the user picked, if it is still in view.
func (s *State) selectedProvider() (query.ProviderGroup, bool) {
	if s.pv.key == "" {
		return query.ProviderGroup{}, false
	}
	for _, g := range s.ov.Providers {
		if g.Key == s.pv.key {
			return g, true
		}
	}
	return query.ProviderGroup{}, false
}

// selectProvider picks a group by its stable Key. A rename never changes the
// Key, so the selection survives it.
func (s *State) selectProvider(key string) {
	if key == "" || s.pv.key == key {
		return
	}
	s.pv.key = key
	s.pv.alias, s.pv.msg, s.pv.err = "", "", ""
}

// setProviderAlias sets or clears a group's display name through the store.
// Unidentified groups have no identity to attach a name to, and no name ever
// reaches the pricing APIs.
func (s *State) setProviderAlias(g query.ProviderGroup, label string) {
	if !g.Identified || s.Hooks.SetProviderAlias == nil || s.pv.busy {
		return
	}
	key, want := g.Key, strings.TrimSpace(label)
	s.pv.busy, s.pv.msg, s.pv.err = true, "", ""
	s.run("provider-alias", func(ctx context.Context) func() {
		err := s.Hooks.SetProviderAlias(ctx, key, want)
		return func() {
			// The busy flag is global: the write is over whoever is on screen
			// now. Its outcome belongs to the group it was started for — the
			// user may have selected another one while it was in flight, and
			// that editor's text and status are not ours to overwrite.
			s.pv.busy = false
			if err != nil {
				if s.pv.key == key {
					s.pv.err = err.Error()
				}
				return
			}
			// The write landed: refresh the span so the new name is drawn
			// everywhere, whichever group is selected now.
			s.loadSpan(Span(s.span))
			if s.pv.key != key {
				return
			}
			s.pv.alias = ""
			if want == "" {
				s.pv.msg = tr("providerAliasCleared")
			} else {
				s.pv.msg = trf("providerAliasSaved", want)
			}
		}
	})
}

// providersTab is the ranking page's Providers tab.
func (s *State) providersTab(c *ui.Context, pal palette) {
	gs := s.ov.Providers
	if s.ov.ProviderErr != nil {
		pane(c, pal).Padding(40).Children(func() {
			emptyState(c, pal, artPicks, tr("failed"), s.ov.ProviderErr.Error())
		})
		return
	}
	if len(gs) == 0 {
		pane(c, pal).Padding(40).Children(func() { emptyState(c, pal, artStaff, tr("noData"), tr("noDataSub")) })
		return
	}
	// The top three on a stage, then every endpoint in a table.
	pane(c, pal).Clip().Padding(22, 26, 0, 26).Children(func() {
		ui.Row(c).Gap(18).AlignItems(ui.End).Children(func() {
			for _, i := range []int{1, 0, 2} {
				if i >= len(gs) {
					ui.Box(c.Key(i)).Grow(1).Basis(0)
					continue
				}
				s.providerPodium(c.Key(gs[i].Key), pal, gs[i], i)
			}
		})
	})
	card(c, pal, "", nil, func() { s.providerTable(c, pal, gs) })
	if g, ok := s.selectedProvider(); ok {
		s.providerDetail(c, pal, g)
	}
}

// providerPodium is one of the top three endpoint groups: name, identity and
// figures over a lit step.
func (s *State) providerPodium(c *ui.Context, pal palette, g query.ProviderGroup, i int) {
	col := bandAt(i)
	step := []float32{96, 72, 56}[i]
	ui.Column(c).Grow(1).Basis(0).Gap(8).Children(func() {
		ui.Column(c).Gap(6).Padding(0, 4).Children(func() {
			ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
				dot(c, col, 7)
				ui.Text(c, providerLabel(g)).FontSize(13.5).FontWeight(680).TextColor(pal.ink).SingleLine().Ellipsis("…").Shrink(1)
				if !g.Identified {
					chipOn(c, pal, tr("unknownProvider"), Soyo).Shrink(0)
				}
				if i == 0 {
					ui.Icon(c, icSparkle).FontSize(13).TextColor(Soyo).Shrink(0)
				}
			})
			ui.Text(c, providerIdentity(g)).FontSize(10.5).TextColor(pal.faint).FontFeatures("tnum").SingleLine().Ellipsis("…")
			bigNumber(c, pal, fmtTokens(g.Tokens.Total()), []float32{52, 40, 36}[i])
			ui.Row(c).Gap(10).Children(func() {
				ui.Text(c, fmtCostOf(g.CostUSD, g.Requests, g.Unpriced)).FontSize(11.5).TextColor(pal.muted).FontFeatures("tnum")
				ui.Text(c, trf("requestsN", fmtInt(g.Requests))).FontSize(11.5).TextColor(pal.muted)
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

// providerTable ranks the endpoint groups; a row selects one by its Key.
func (s *State) providerTable(c *ui.Context, pal palette, gs []query.ProviderGroup) {
	var all int64
	for _, g := range gs {
		all += g.Tokens.Total()
	}
	head := func(t string, w float32) {
		ui.Text(c, t).FontSize(11).FontWeight(650).TextColor(pal.muted).Width(w).TextAlign(ui.End)
	}
	ui.Row(c).Gap(12).Padding(0, 4, 6, 4).Children(func() {
		ui.Text(c, "#").FontSize(11).TextColor(pal.muted).Width(22)
		ui.Text(c, "").Grow(1)
		head(tr("share"), 54)
		head(tr("requests"), 70)
		head(tr("cost"), 74)
		head(tr("tokens"), 80)
	})
	ui.Divider(c)
	for i, g := range gs {
		g := g
		row := ui.Row(c.Key(g.Key)).Gap(12).Padding(8, 4).Radius(8).AlignItems(ui.Center).
			Cursor(ui.CursorPointer).Role(ui.RoleButton).Transition(hoverFade).
			OnClick(func() { s.selectProvider(g.Key) })
		if s.pv.key == g.Key {
			row.Background(pal.raised)
		} else if row.Hovered() {
			row.Background(pal.hover)
		}
		row.Children(func() {
			num := ui.Text(c, fmt.Sprint(i+1)).Font(display).FontSize(15).FontWeight(700).TextColor(pal.muted).Width(22).FontFeatures("tnum")
			if i < 3 {
				num.TextColor(bandAt(i))
			}
			ui.Column(c).Grow(1).Basis(0).Gap(4).Children(func() {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					dot(c, bandAt(i), 8)
					ui.Text(c, providerLabel(g)).FontSize(13).FontWeight(600).TextColor(pal.ink).SingleLine().Ellipsis("…")
					if !g.Identified {
						chipOn(c, pal, tr("unknownProvider"), Soyo).Shrink(0)
					}
				})
				ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
					ui.Text(c, providerIdentity(g)).FontSize(10.5).TextColor(pal.faint).FontFeatures("tnum").SingleLine().Ellipsis("…").Shrink(1)
					if members := providerMembersText(g); members != "" {
						ui.Text(c, members).FontSize(10.5).TextColor(pal.muted).SingleLine().Ellipsis("…").Shrink(1)
					}
				})
				classBar(c, pal, g.Tokens, 4)
			})
			share := 0.0
			if all > 0 {
				share = float64(g.Tokens.Total()) / float64(all)
			}
			ui.Text(c, fmtPct(share)).FontSize(12).TextColor(pal.muted).FontFeatures("tnum").Width(54).TextAlign(ui.End)
			ui.Text(c, fmtInt(g.Requests)).FontSize(12).TextColor(pal.muted).FontFeatures("tnum").Width(70).TextAlign(ui.End)
			ui.Text(c, fmtCostOf(g.CostUSD, g.Requests, g.Unpriced)).FontSize(12).TextColor(pal.ink).FontFeatures("tnum").Width(74).TextAlign(ui.End)
			ui.Text(c, fmtTokens(g.Tokens.Total())).FontSize(13).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum").Width(80).TextAlign(ui.End)
		})
	}
}

// providerDetail shows the picked group: its identity, its figures, the
// harness/provider members behind it, and the display-name editor.
func (s *State) providerDetail(c *ui.Context, pal palette, g query.ProviderGroup) {
	card(c, pal, providerLabel(g), func() {
		if g.Identified {
			chipOn(c, pal, tr("providerIdentified"), Rana)
		} else {
			chipOn(c, pal, tr("providerUnidentifiedBadge"), Soyo)
		}
	}, func() {
		ui.Row(c).Gap(14).AlignItems(ui.Center).Children(func() {
			ui.Text(c, providerIdentity(g)).FontSize(11.5).TextColor(pal.muted).FontFeatures("tnum").SingleLine().Ellipsis("…").Shrink(1)
			ui.Spacer(c)
			ui.Text(c, trf("sessionsN", g.Sessions)).FontSize(11.5).TextColor(pal.muted).FontFeatures("tnum")
			ui.Text(c, trf("requestsN", fmtInt(g.Requests))).FontSize(11.5).TextColor(pal.muted).FontFeatures("tnum")
			ui.Text(c, fmtCostOf(g.CostUSD, g.Requests, g.Unpriced)).FontSize(11.5).TextColor(pal.ink).FontFeatures("tnum")
			ui.Text(c, fmtTokens(g.Tokens.Total())).FontSize(12.5).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum")
		})
		// The identity above is a ten-character truncation, so two keys can
		// share it. The whole stable key is printed here and selectable, so a
		// look-alike pair stays tellable apart.
		ui.Text(c, tr("providerStableKey")+"  "+g.Key).FontSize(10.5).TextColor(pal.faint).FontFeatures("tnum").Wrap().Selectable()
		kicker(c, pal, tr("providerMembers"))
		ui.Column(c).Gap(6).Children(func() {
			for _, m := range g.Members {
				ui.Row(c.Key(string(m.Harness) + "/" + m.Provider)).Gap(9).AlignItems(ui.Center).Children(func() {
					harnessMark(c, m.Harness, 15)
					ui.Text(c, m.Harness.DisplayName()).FontSize(12.5).FontWeight(600).TextColor(pal.ink)
					if m.Provider != "" {
						ui.Text(c, "·").FontSize(12).TextColor(pal.faint)
						ui.Text(c, m.Provider).FontSize(12.5).TextColor(pal.muted)
					}
				})
			}
		})
		// The sessions this endpoint served, narrowed by its stable id.
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			if ui.Button(c, tr("providerSessions")).Clicked() {
				s.showProviderSessions(g)
			}
			ui.Text(c, tr("providerSessionsSub")).FontSize(11).TextColor(pal.muted).SingleLine().Ellipsis("…").Shrink(1)
		})
		ui.Text(c, tr("providerBoundary")).FontSize(11).TextColor(pal.muted).Wrap()
		ui.Divider(c)
		s.providerAliasEditor(c, pal, g)
	})
}

// providerAliasEditor renames an identified endpoint for display only.
func (s *State) providerAliasEditor(c *ui.Context, pal palette, g query.ProviderGroup) {
	kicker(c, pal, tr("providerAlias"))
	if !g.Identified {
		ui.Text(c, tr("providerAliasUnidentified")).FontSize(11.5).TextColor(pal.muted).Wrap()
		return
	}
	if s.Hooks.SetProviderAlias == nil {
		ui.Text(c, tr("providerAliasUnavailable")).FontSize(11.5).TextColor(pal.muted).Wrap()
		return
	}
	ui.Text(c, tr("providerAliasSub")).FontSize(11).TextColor(pal.muted).Wrap()
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.TextInput(c, &s.pv.alias).Width(260).Placeholder(providerLabel(g)).OnSubmit(func() {
			s.setProviderAlias(g, s.pv.alias)
		})
		if s.pv.busy {
			ui.Text(c, tr("providerAliasSaving")).FontSize(11.5).TextColor(pal.muted)
			return
		}
		if ui.Button(c, tr("providerAliasSave")).Clicked() {
			s.setProviderAlias(g, s.pv.alias)
		}
		if ui.Button(c, tr("providerAliasClear")).Clicked() {
			s.setProviderAlias(g, "")
		}
	})
	if s.pv.err != "" {
		ui.Text(c, s.pv.err).FontSize(11.5).TextColor(Anon).Wrap()
	} else if s.pv.msg != "" {
		ui.Text(c, s.pv.msg).FontSize(11.5).TextColor(Rana).Wrap()
	}
}

// providerList is the overview's provider card: the same groups, compact.
func (s *State) providerList(c *ui.Context, pal palette, gs []query.ProviderGroup, err error, limit int) {
	if err != nil {
		ui.Text(c, tr("failed")+": "+err.Error()).FontSize(11.5).TextColor(pal.muted).Wrap()
		return
	}
	if len(gs) == 0 {
		ui.Text(c, tr("noData")).FontSize(12).TextColor(pal.muted)
		return
	}
	var top int64
	for _, g := range gs {
		if v := g.Tokens.Total(); v > top {
			top = v
		}
	}
	ui.Column(c).Gap(10).Children(func() {
		for i, g := range gs {
			if i >= limit {
				break
			}
			col := bandAt(i)
			ui.Column(c.Key(g.Key)).Gap(4).Children(func() {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					dot(c, col, 8)
					ui.Text(c, providerLabel(g)).FontSize(12.5).FontWeight(600).TextColor(pal.ink).SingleLine().Ellipsis("…").Shrink(1)
					if !g.Identified {
						chipOn(c, pal, tr("unknownProvider"), Soyo).Shrink(0)
					}
					ui.Spacer(c)
					ui.Text(c, fmtTokens(g.Tokens.Total())).FontSize(12.5).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum")
				})
				ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
					ui.Text(c, providerIdentity(g)).FontSize(10).TextColor(pal.faint).FontFeatures("tnum").SingleLine().Ellipsis("…").Shrink(1)
					if members := providerMembersText(g); members != "" {
						ui.Text(c, members).FontSize(10).TextColor(pal.muted).SingleLine().Ellipsis("…").Shrink(1)
					}
				})
				rankBar(c, pal, float64(g.Tokens.Total())/float64(top), col, 4)
			})
		}
	})
}

var providerStrs = map[string][2]string{
	"providerIdentity":          {"稳定 ID", "Stable ID"},
	"providerStableKey":         {"完整稳定 ID（可选中复制）", "Full stable ID (selectable)"},
	"providerIdentified":        {"已识别端点", "Identified endpoint"},
	"providerUnidentifiedBadge": {"未识别", "Unidentified"},
	"providerUnidentified":      {"未识别端点", "Unidentified endpoint"},
	"providerMembers":           {"成员来源", "Members"},
	"providerBoundary":          {"端点身份不是计费账户：这里只按端点合并用量，余额从不合并，价格仍按模型各自计算。", "An endpoint identity is not a billing account: usage merges by endpoint here, balances never merge, and prices stay per model."},
	"providerAlias":             {"显示名", "Display name"},
	"providerAliasSub":          {"只改显示；身份、归因、计费和价格都不变。", "Display only: identity, attribution, billing and prices stay as they are."},
	"providerAliasSave":         {"保存", "Save"},
	"providerAliasClear":        {"清除", "Clear"},
	"providerAliasSaved":        {"显示名已改为 %s", "Display name set to %s"},
	"providerAliasCleared":      {"显示名已清除，恢复端点名", "Display name cleared, back to the endpoint"},
	"providerAliasUnidentified": {"这个端点没有稳定身份，不能起显示名。", "This endpoint has no stable identity, so it can't be renamed."},
	"providerAliasUnavailable":  {"这个版本没有接入显示名存储。", "This build has no display-name store wired in."},
	"providerAliasSaving":       {"保存中…", "Saving…"},
}

func init() {
	for k, v := range providerStrs {
		strs[k] = v
	}
}
