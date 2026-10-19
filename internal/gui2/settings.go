package gui

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/query"
)

// settingsState is the Settings page's data and what is being edited.
type settingsState struct {
	group string // "general", "sources", "prices", "data"
	job   job    // loading
	act   job    // an action the user took

	ready    bool
	err      string // the last load or action error
	notice   string // the last action's result
	sources  []Source
	unpriced []query.UnpricedModel
	aliases  []query.ModelAlias
	custom   []CustomPrice
	db       DBInfo

	showMissing bool
	fxRate      float64 // the rate field under a non-USD currency
	login       bool    // the launch-at-login switch

	// The alias being added and the custom price being added.
	aliasFrom, aliasTo string
	priceModel         string
	price              [4]float64

	// Confirmations.
	confirmRebuild bool
	confirmDelete  bool
	deleteWhat     string // "alias:<from>" or "price:<model>"
}

var settingGroups = []struct {
	id string
	ic *ui.SVG
}{{"general", icGeneral}, {"sources", icSources}, {"prices", icPrice}, {"data", icDatabase}}

func (a *App) loadSettings() {
	d := a.d
	a.run(&a.set.job, func(ctx context.Context) func() {
		var s settingsState
		var errs []string
		if d.Sources != nil {
			var err error
			if s.sources, err = d.Sources.List(ctx); err != nil {
				errs = append(errs, err.Error())
			}
		}
		if d.Prices != nil {
			var err error
			if s.unpriced, err = d.Prices.Unpriced(ctx); err != nil {
				errs = append(errs, err.Error())
			}
			if s.aliases, err = d.Prices.Aliases(ctx); err != nil {
				errs = append(errs, err.Error())
			}
			if s.custom, err = d.Prices.CustomPrices(ctx); err != nil {
				errs = append(errs, err.Error())
			}
		}
		if d.Maint != nil {
			var err error
			if s.db, err = d.Maint.Info(ctx); err != nil {
				errs = append(errs, err.Error())
			}
		}
		return func() {
			st := &a.set
			st.ready = true
			st.sources, st.unpriced, st.aliases, st.custom, st.db = s.sources, s.unpriced, s.aliases, s.custom, s.db
			if d.Login != nil {
				st.login = d.Login.OpenAtLogin()
			}
			if len(errs) > 0 {
				st.err = strings.Join(errs, "; ")
			}
		}
	})
}

// act runs a settings action in the background, then shows its outcome
// and reloads what it changed. ok is shown when it succeeds.
func (a *App) act(ok string, work func(ctx context.Context) error) {
	a.set.err, a.set.notice = "", ""
	a.run(&a.set.act, func(ctx context.Context) func() {
		err := work(ctx)
		return func() {
			if err != nil {
				a.set.err = err.Error()
			} else {
				a.set.notice = ok
			}
			a.loadSettings()
		}
	})
}

func (a *App) settingsPage(c *ui.Context, pal palette) {
	st := &a.set
	if st.group == "" {
		st.group = "general"
	}
	ui.Row(c).Grow(1).Basis(0).FillWidth().MinHeight(0).Children(func() {
		ui.Column(c).Width(200).FillHeight().Shrink(0).Padding(0, 10, 14, 16).Gap(10).Children(func() {
			pageHeader(c, pal, tr("settings"), nil)
			ui.Sidebar(c, &st.group, func() {
				for _, g := range settingGroups {
					ui.SidebarItem(c, g.id, g.ic, tr("set_"+g.id))
				}
			}).Grow(1).Background(pal.bg).Padding(0, 0).Label(tr("settings"))
		})
		ui.Scroll(c.Key("set-" + st.group)).Grow(1).Basis(0).FillHeight().Children(func() {
			ui.Column(c).Padding(44, 28, 28, 12).Gap(16).MaxWidth(820).FillWidth().Children(func() {
				if st.err != "" {
					errorLine(c, pal, st.err)
				}
				if st.notice != "" {
					ui.Row(c).Gap(8).AlignItems(ui.Center).Padding(8, 10).Radius(8).Background(pal.good.Alpha(0.12)).Children(func() {
						ui.Icon(c, icCheck).FontSize(14).TextColor(pal.good)
						ui.Text(c, st.notice).FontSize(12).TextColor(pal.ink)
					})
				}
				switch st.group {
				case "sources":
					a.sourcesSettings(c, pal)
				case "prices":
					a.priceSettings(c, pal)
				case "data":
					a.dataSettings(c, pal)
				default:
					a.generalSettings(c, pal)
				}
			})
		})
	})
	a.confirmDialogs(c)
}

// settingRow is a label with a description on the left and a control on
// the right.
func settingRow(c *ui.Context, pal palette, label, desc string, control func()) {
	ui.Row(c).Gap(16).AlignItems(ui.Center).Padding(4, 0).Children(func() {
		ui.Column(c).Gap(3).Grow(1).Basis(0).MinWidth(0).Children(func() {
			ui.Text(c, label).FontSize(13).FontWeight(560).TextColor(pal.ink)
			if desc != "" {
				caption(c, pal, desc)
			}
		})
		control()
	})
}

// choice is a Select over keys shown by their labels; it reports the key
// chosen, or "" when nothing changed.
func choice(c *ui.Context, label string, keys []string, labelOf func(string) string, current string) (string, bool) {
	labels := make([]string, len(keys))
	sel := ""
	for i, k := range keys {
		labels[i] = labelOf(k)
		if k == current {
			sel = labels[i]
		}
	}
	if sel == "" && len(labels) > 0 {
		sel = labels[0]
	}
	if ui.Select(c, &sel, labels).Label(label).Width(180).Changed() {
		for i, l := range labels {
			if l == sel {
				return keys[i], true
			}
		}
	}
	return "", false
}

func (a *App) setPref(key, value string) {
	if a.d.Prefs == nil {
		return
	}
	if err := a.d.Prefs.SetPref(key, value); err != nil {
		a.set.err = err.Error()
	}
}

func (a *App) generalSettings(c *ui.Context, pal palette) {
	prefs := a.d.Prefs
	get := func(k string) string {
		if prefs == nil {
			return ""
		}
		return prefs.Pref(k)
	}
	card(c, pal).Children(func() {
		cardTitle(c, pal, tr("set_general"), nil)
		settingRow(c, pal, tr("language"), "", func() {
			if k, ok := choice(c, tr("language"), []string{"", "zh", "en"}, func(k string) string { return tr("lang_" + k) }, get(PrefLang)); ok {
				a.setPref(PrefLang, k)
				setLang(k)
			}
		})
		ui.Divider(c)
		settingRow(c, pal, tr("appearance"), "", func() {
			if k, ok := choice(c, tr("appearance"), []string{"", "light", "dark"}, func(k string) string { return tr("look_" + k) }, get(PrefAppearance)); ok {
				a.setPref(PrefAppearance, k)
			}
		})
		ui.Divider(c)
		settingRow(c, pal, tr("timezone"), trf("timezoneHint", systemZone()), func() {
			if k, ok := choice(c, tr("timezone"), timezones, zoneLabel, get(PrefTimezone)); ok {
				a.setPref(PrefTimezone, k)
				a.reloadAll()
			}
		})
		ui.Divider(c)
		settingRow(c, pal, tr("currency"), tr("currencyHint"), func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				cur := get(PrefCurrency)
				if k, ok := choice(c, tr("currency"), currencyCodes, currencyLabel, cur); ok {
					a.setPref(PrefCurrency, k)
					a.applyCurrency()
				}
				if cur != "" && cur != "USD" {
					caption(c, pal, "1 USD =")
					if ui.NumberInput(c, &a.set.fxRate, 0.0001, 1e6, 0.01).Width(110).Changed() && a.set.fxRate > 0 {
						a.setPref(PrefFXRate, strconv.FormatFloat(a.set.fxRate, 'f', -1, 64))
						a.applyCurrency()
					}
					caption(c, pal, cur)
				}
			})
		})
		ui.Divider(c)
		if a.d.Login != nil {
			settingRow(c, pal, tr("openAtLogin"), tr("openAtLoginHint"), func() {
				if ui.Switch(c, &a.set.login).Changed() {
					on := a.set.login
					a.act(tr("saved"), func(context.Context) error { return a.d.Login.SetOpenAtLogin(on) })
				}
			})
			ui.Divider(c)
		}
		settingRow(c, pal, tr("trayTitle"), tr("trayTitleHint"), func() {
			sel := trayItems(get(PrefTrayTitle))
			ui.Row(c).Gap(12).Children(func() {
				for _, it := range trayTitleItems {
					on := slices.Contains(sel, it)
					if ui.Checkbox(c.Key("tt"+it), &on, tr("tray_"+it)).Changed() {
						var next []string
						for _, x := range trayTitleItems {
							if (x == it && on) || (x != it && slices.Contains(sel, x)) {
								next = append(next, x)
							}
						}
						v := strings.Join(next, ",")
						if v == "" {
							v = "none"
						}
						a.setPref(PrefTrayTitle, v)
						a.updateTray()
					}
				}
			})
		})
	})
	card(c, pal).Children(func() {
		cardTitle(c, pal, tr("about"), nil)
		settingRow(c, pal, "MyToken", tr("aboutHint"), func() {
			caption(c, pal, a.d.Version)
		})
		settingRow(c, pal, tr("pricesFrom"), tr("pricesFromHint"), func() {
			if a.d.Prices != nil {
				info := a.d.Prices.Catalog()
				caption(c, pal, trf("catalogInfo", info.Version, info.Models))
			}
		})
	})
}

func (a *App) sourcesSettings(c *ui.Context, pal palette) {
	st := &a.set
	if a.d.Sources == nil {
		emptyState(c, pal, tr("unavailable"), "")
		return
	}
	var found, missing []Source
	for _, s := range st.sources {
		if s.Detected || s.Override != "" || s.Requests > 0 || s.Problem != "" {
			found = append(found, s)
		} else {
			missing = append(missing, s)
		}
	}
	card(c, pal).Gap(6).Children(func() {
		cardTitle(c, pal, trf("sourcesFound", len(found)), func() {
			if ui.Button(c, tr("rescan")).Disabled(st.act.running).Clicked() {
				a.act(tr("rescanDone"), a.d.Sources.Rescan)
			}
		})
		caption(c, pal, tr("sourcesHint"))
		if !st.ready {
			caption(c, pal, tr("loading"))
		}
		for i, s := range found {
			if i > 0 {
				ui.Divider(c)
			}
			a.sourceRow(c.Key(string(s.Harness)), pal, s)
		}
	})
	if len(missing) > 0 {
		card(c, pal).Gap(6).Children(func() {
			cardTitle(c, pal, trf("sourcesMissing", len(missing)), func() {
				ui.Switch(c, &st.showMissing).Label(tr("show"))
			})
			caption(c, pal, tr("sourcesMissingHint"))
			if !st.showMissing {
				return
			}
			for i, s := range missing {
				if i > 0 {
					ui.Divider(c)
				}
				a.sourceRow(c.Key(string(s.Harness)), pal, s)
			}
		})
	}
}

func (a *App) sourceRow(c *ui.Context, pal palette, s Source) {
	st := &a.set
	now := a.now()
	h := s.Harness
	ui.Row(c).Gap(12).AlignItems(ui.Center).Padding(6, 0).Children(func() {
		toolBadge(c, h, 28)
		ui.Column(c).Gap(3).Grow(1).Basis(0).MinWidth(0).Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, h.DisplayName()).FontSize(13.5).FontWeight(600).TextColor(pal.ink)
				switch {
				case s.Problem != "":
					pill(c, tr("srcProblem"), pal.warn, pal.warn.Alpha(0.14))
				case s.Override != "":
					pill(c, tr("srcCustom"), pal.accent, pal.accent.Alpha(0.12))
				case s.Detected:
					pill(c, tr("srcDetected"), pal.good, pal.good.Alpha(0.14))
				}
			})
			dir := strings.Join(s.Roots, ", ")
			if s.Override != "" {
				dir = s.Override
			}
			if dir != "" {
				caption(c, pal, dir).FontSize(11).NoWrap().Ellipsis("…")
			}
			if s.Problem != "" {
				caption(c, pal, s.Problem).FontSize(11).TextColor(pal.warn)
			} else if s.Requests > 0 {
				caption(c, pal, trf("srcStats", fmtInt(s.Requests), fmtAgo(now, s.LastSeen))).FontSize(11)
			}
		})
		// A folder is only asked for when detection missed the tool, or to
		// change one the user chose before.
		if a.d.PickFolder != nil && (!s.Detected || s.Override != "" || s.Problem != "") {
			if ui.Button(c, tr("chooseDir")).Disabled(st.act.running).Clicked() {
				start := s.Override
				if start == "" && len(s.Roots) > 0 {
					start = s.Roots[0]
				}
				if dir, ok := a.d.PickFolder(start); ok {
					a.act(trf("dirSet", h.DisplayName()), func(ctx context.Context) error {
						return a.d.Sources.SetRoot(ctx, h, dir)
					})
				}
			}
		}
		if s.Override != "" {
			if ui.Button(c, tr("autoDir")).Disabled(st.act.running).Clicked() {
				a.act(trf("dirAuto", h.DisplayName()), func(ctx context.Context) error {
					return a.d.Sources.SetRoot(ctx, h, "")
				})
			}
		}
		on := s.Enabled
		if ui.Switch(c, &on).Label(h.DisplayName()).Disabled(st.act.running).Changed() {
			// Show the change at once; the reload confirms it.
			for i := range st.sources {
				if st.sources[i].Harness == h {
					st.sources[i].Enabled = on
				}
			}
			a.act("", func(ctx context.Context) error {
				return a.d.Sources.SetEnabled(ctx, h, on)
			})
		}
	})
}

func (a *App) priceSettings(c *ui.Context, pal palette) {
	st := &a.set
	p := a.d.Prices
	if p == nil {
		emptyState(c, pal, tr("unavailable"), "")
		return
	}
	card(c, pal).Children(func() {
		cardTitle(c, pal, tr("pricesFrom"), nil)
		info := p.Catalog()
		caption(c, pal, tr("pricesFromHint"))
		caption(c, pal, trf("catalogInfo", info.Version, info.Models))
	})
	card(c, pal).Gap(8).Children(func() {
		cardTitle(c, pal, trf("unpricedModels", len(st.unpriced)), nil)
		if len(st.unpriced) == 0 {
			caption(c, pal, tr("allPriced"))
			return
		}
		caption(c, pal, tr("unpricedHint"))
		for i, u := range st.unpriced {
			if i > 0 {
				ui.Divider(c)
			}
			ui.Column(c.Key(u.Provider+"/"+u.Model)).Gap(6).Padding(4, 0).Children(func() {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Text(c, u.Model).FontSize(13).FontWeight(600).TextColor(pal.ink).NoWrap().Ellipsis("…").Shrink(1).MinWidth(0)
					if u.Provider != "" {
						caption(c, pal, u.Provider)
					}
					ui.Spacer(c)
					caption(c, pal, trf("rankSub", fmtTokens(u.Tokens), fmtInt(u.Requests))).NoWrap()
				})
				ui.Row(c).Gap(6).Wrap().AlignItems(ui.Center).Children(func() {
					for _, sug := range u.Suggestions {
						if ui.Button(c.Key(sug), trf("priceAs", sug)).Disabled(st.act.running).Clicked() {
							a.addAlias(query.ModelAlias{From: u.Model, To: sug, Provider: u.Provider})
						}
					}
					if ui.Button(c, tr("customPrice")).Clicked() {
						st.priceModel = u.Model
					}
				})
			})
		}
	})
	card(c, pal).Gap(8).Children(func() {
		cardTitle(c, pal, tr("aliases"), nil)
		caption(c, pal, tr("aliasesHint"))
		for _, al := range st.aliases {
			ui.Row(c.Key(al.From)).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, al.From).FontSize(13).TextColor(pal.ink).NoWrap().Ellipsis("…").Shrink(1).MinWidth(0)
				caption(c, pal, "→")
				ui.Text(c, al.To).FontSize(13).FontWeight(600).TextColor(pal.ink).NoWrap().Ellipsis("…").Shrink(1).MinWidth(0)
				ui.Spacer(c)
				if ui.Button(c, tr("delete")).Disabled(st.act.running).Clicked() {
					st.deleteWhat, st.confirmDelete = "alias:"+al.From, true
				}
			})
		}
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.TextInput(c, &st.aliasFrom).Placeholder(tr("aliasFrom")).Label(tr("aliasFrom")).Grow(1).Basis(0)
			caption(c, pal, "→")
			names := a.catalogMatches(st.aliasTo)
			ui.Autocomplete(c, &st.aliasTo, names).Placeholder(tr("aliasTo")).Label(tr("aliasTo")).Grow(1).Basis(0)
			if ui.PrimaryButton(c, tr("add")).Disabled(st.act.running || strings.TrimSpace(st.aliasFrom) == "" || strings.TrimSpace(st.aliasTo) == "").Clicked() {
				a.addAlias(query.ModelAlias{From: strings.TrimSpace(st.aliasFrom), To: strings.TrimSpace(st.aliasTo)})
			}
		})
	})
	card(c, pal).Gap(8).Children(func() {
		cardTitle(c, pal, tr("customPrices"), nil)
		caption(c, pal, tr("customPricesHint"))
		for _, cp := range st.custom {
			ui.Row(c.Key(cp.Model)).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, cp.Model).FontSize(13).FontWeight(600).TextColor(pal.ink).NoWrap().Ellipsis("…").Shrink(1).MinWidth(0)
				ui.Spacer(c)
				caption(c, pal, trf("priceLine", trimZero(fmtF(cp.Input)), trimZero(fmtF(cp.Output)), trimZero(fmtF(cp.CacheRead)), trimZero(fmtF(cp.CacheWrite)))).FontFeatures("tnum").NoWrap()
				if ui.Button(c, tr("delete")).Disabled(st.act.running).Clicked() {
					st.deleteWhat, st.confirmDelete = "price:"+cp.Model, true
				}
			})
		}
		ui.Column(c).Gap(8).Padding(8, 0, 0, 0).Children(func() {
			ui.TextInput(c, &st.priceModel).Placeholder(tr("model")).Label(tr("model")).FillWidth()
			ui.Row(c).Gap(8).Children(func() {
				for i, k := range []string{"input", "output", "cacheRead", "cacheWrite"} {
					ui.Column(c.Key(k)).Gap(4).Grow(1).Basis(0).Children(func() {
						caption(c, pal, tr(k)+" $/M").FontSize(11)
						ui.NumberInput(c, &st.price[i], 0, 1000, 0.05).Label(tr(k)).FillWidth()
					})
				}
			})
			ui.Row(c).Children(func() {
				ui.Spacer(c)
				if ui.PrimaryButton(c, tr("savePrice")).Disabled(st.act.running || strings.TrimSpace(st.priceModel) == "").Clicked() {
					a.addCustomPrice(CustomPrice{Model: strings.TrimSpace(st.priceModel), Input: st.price[0], Output: st.price[1], CacheRead: st.price[2], CacheWrite: st.price[3]})
				}
			})
		})
	})
}

// catalogMatches looks up official model names for the alias box. It runs
// synchronously only on a short, cached query: the catalog is in memory.
func (a *App) catalogMatches(q string) []string {
	q = strings.TrimSpace(q)
	if len(q) < 2 || a.d.Prices == nil {
		return nil
	}
	names, _ := a.d.Prices.SearchCatalog(a.ctx, q, 12)
	return names
}

func (a *App) addAlias(al query.ModelAlias) {
	st := &a.set
	// A new alias of the same model replaces the old one.
	list := slices.DeleteFunc(slices.Clone(st.aliases), func(x query.ModelAlias) bool { return x.From == al.From })
	list = append(list, al)
	st.aliasFrom, st.aliasTo = "", ""
	a.act(trf("aliasAdded", al.From, al.To), func(ctx context.Context) error {
		return a.d.Prices.SetAliases(ctx, list)
	})
}

func (a *App) addCustomPrice(cp CustomPrice) {
	st := &a.set
	list := slices.DeleteFunc(slices.Clone(st.custom), func(x CustomPrice) bool { return x.Model == cp.Model })
	list = append(list, cp)
	st.priceModel, st.price = "", [4]float64{}
	a.act(trf("priceSaved", cp.Model), func(ctx context.Context) error {
		return a.d.Prices.SetCustomPrices(ctx, list)
	})
}

func (a *App) dataSettings(c *ui.Context, pal palette) {
	st := &a.set
	m := a.d.Maint
	if m == nil {
		emptyState(c, pal, tr("unavailable"), "")
		return
	}
	db := st.db
	card(c, pal).Children(func() {
		cardTitle(c, pal, tr("database"), func() {
			if a.d.Reveal != nil && db.Path != "" {
				if ui.Button(c, tr("reveal")).Clicked() {
					a.d.Reveal(db.Path)
				}
			}
		})
		caption(c, pal, db.Path).NoWrap().Ellipsis("…")
		ui.Row(c).Gap(12).Children(func() {
			kpi(c, pal, tr("dbSize"), fmtBytes(db.Bytes))
			kpi(c, pal, tr("requests"), fmtInt(db.Events))
			kpi(c, pal, tr("sessionsN"), fmtInt(db.Sessions))
		})
		if !db.Oldest.IsZero() {
			caption(c, pal, trf("dbRange", fmtDateYear(db.Oldest), fmtDateYear(db.Newest)))
		}
	})
	card(c, pal).Children(func() {
		cardTitle(c, pal, tr("maintenance"), nil)
		if a.d.Sources != nil {
			settingRow(c, pal, tr("rescan"), tr("rescanHint"), func() {
				if ui.Button(c, tr("rescan")).Disabled(st.act.running).Clicked() {
					a.act(tr("rescanDone"), a.d.Sources.Rescan)
				}
			})
			ui.Divider(c)
		}
		if a.d.Export != nil && a.d.SaveFile != nil {
			settingRow(c, pal, tr("export"), tr("exportHint"), func() {
				if ui.Button(c, tr("exportCSV")).Disabled(st.act.running).Clicked() {
					if path, ok := a.d.SaveFile("mytoken-requests-" + a.now().Format("20060102") + ".csv"); ok {
						a.act(trf("exported", path), func(ctx context.Context) error { return a.d.Export.Export(ctx, path) })
					}
				}
			})
			ui.Divider(c)
		}
		settingRow(c, pal, tr("compact"), tr("compactHint"), func() {
			if ui.Button(c, tr("compact")).Disabled(st.act.running).Clicked() {
				a.act(tr("compactDone"), m.Compact)
			}
		})
		ui.Divider(c)
		settingRow(c, pal, tr("rebuild"), tr("rebuildHint"), func() {
			if ui.Button(c, tr("rebuild")).Disabled(st.act.running).Clicked() {
				st.confirmRebuild = true
			}
		})
		if st.act.running {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Spinner(c)
				caption(c, pal, tr("working"))
			})
		}
	})
}

func (a *App) confirmDialogs(c *ui.Context) {
	st := &a.set
	if st.confirmRebuild {
		if ui.AlertDialog(c, &st.confirmRebuild, tr("rebuildAsk"), tr("rebuildAskHint"), tr("cancel"), tr("rebuild")) == 1 {
			a.act(tr("rebuildDone"), a.d.Maint.Rebuild)
		}
	}
	if st.confirmDelete {
		what, name, _ := strings.Cut(st.deleteWhat, ":")
		if ui.AlertDialog(c, &st.confirmDelete, trf("deleteAsk", name), tr("deleteAskHint"), tr("cancel"), tr("delete")) == 1 {
			switch what {
			case "alias":
				list := slices.DeleteFunc(slices.Clone(st.aliases), func(x query.ModelAlias) bool { return x.From == name })
				a.act(trf("deleted", name), func(ctx context.Context) error { return a.d.Prices.SetAliases(ctx, list) })
			case "price":
				list := slices.DeleteFunc(slices.Clone(st.custom), func(x CustomPrice) bool { return x.Model == name })
				a.act(trf("deleted", name), func(ctx context.Context) error { return a.d.Prices.SetCustomPrices(ctx, list) })
			}
		}
	}
}
