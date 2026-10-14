package gui

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"

	"github.com/zzstar101/mytoken/internal/query"
)

// pricingState backs the settings page's pricing cards: models without a
// price (with one-click mappings), the mappings, and provider multipliers.
type pricingState struct {
	loaded    bool
	rules     []query.PriceRule
	aliases   []query.ModelAlias
	unpriced  []query.UnpricedModel
	providers []string
	err       error

	open   string   // the unpriced model whose catalog search is open
	needle string   // its search text
	lastQ  string   // the last search sent
	hits   []string // its results
	mult   map[string]string
	// multVal is the number each multiplier input edits, derived from the
	// same rules as mult but as a value the number input can hold.
	multVal map[string]float64
	busy    bool
	// pending holds a value written for a provider until a reload shows it
	// stored: while it is set the input keeps the number the user typed
	// instead of being reset to the one the rules still carry.
	pending map[string]float64
	// typed holds the text of an edit the field has not lost: it keeps the
	// field from falling back to the stored value before the explicit Apply
	// that submits the text has been handled, and is dropped once the rules
	// carry what was typed.
	typed map[string]string
	// parked holds edits made while a write was in flight, applied once the
	// reload after that write lands, so a step is applied rather than
	// dropped and from rules that include the write's own result.
	parked map[string]float64
	// gen counts pricing reloads and parkedGen is the reload a parked edit
	// waits for, so an edit is not written from rules older than the write
	// that pushed it aside.
	gen       int
	parkedGen int
	message   string
}

func (s *State) settings() query.Settings { return s.Hooks.Settings }

func (s *State) loadPricing() {
	st := s.settings()
	if st == nil {
		return
	}
	s.run("pricing", func(ctx context.Context) func() {
		rules, err := st.PriceRules(ctx)
		aliases, err2 := st.ModelAliases(ctx)
		unpriced, err3 := st.UnpricedModels(ctx)
		providers, err4 := st.Providers(ctx)
		for _, e := range []error{err2, err3, err4} {
			if err == nil {
				err = e
			}
		}
		return func() {
			p := &s.pr
			p.rules, p.aliases, p.unpriced, p.providers, p.err, p.loaded = rules, aliases, unpriced, providers, err, true
			p.mult = providerMultipliers(rules, providers)
			p.multVal = providerMultiplierValues(rules, providers)
			p.gen++
			// A value the user just submitted is what the input shows until
			// the rules that will carry it come back.
			for prov, m := range p.pending {
				if providerMultiplier(rules, prov) == m {
					delete(p.pending, prov)
					continue
				}
				p.multVal[prov] = m
			}
			for prov, m := range p.parked {
				p.multVal[prov] = m
			}
			// An edit is no longer unsaved once the rules carry what was
			// typed; until then the field keeps the text.
			for prov, text := range p.typed {
				if m, ok := parseMultiplier(text); ok && providerMultiplier(rules, prov) == m {
					delete(p.typed, prov)
				}
			}
		}
	})
}

// providerMultiplier returns the multiplier the pricing card shows for a
// provider: the user's own undated provider-wide rule, else the one imported
// from cc-switch, else 1.
//
// Rules synced from a relay ("relay:*") are deliberately ignored. They are the
// list-price ratios a scan found on the site, not something typed in this box:
// showing one would make an untouched field look edited, and submitting it
// would freeze a synced ratio into a user rule that no longer tracks the site.
// A dated rule is history and a per-model rule is a narrower scope, so neither
// is the provider's multiplier either. The pricer keeps the same authority
// order for the multiplier it applies (user over relay over cc-switch, see
// sourceRank in internal/pricing/rules.go).
func providerMultiplier(rules []query.PriceRule, provider string) float64 {
	var user, imported float64
	for _, r := range rules {
		if r.Provider != provider || r.Model != "" || r.Multiplier <= 0 || !r.From.IsZero() {
			continue
		}
		switch r.Source {
		case "user", "":
			user = r.Multiplier // the newest user rule wins, as in the pricer's ruleAt
		case "cc-switch":
			imported = r.Multiplier
		}
	}
	if user > 0 {
		return user
	}
	if imported > 0 {
		return imported
	}
	return 1
}

// formatMultiplier renders a multiplier so the text the card shows and the
// number the rules store round-trip exactly. A multiplier is money: rounding
// it to the input's step, or cutting it to a fixed number of decimals, would
// change what is billed, which docs/ROADMAP.md forbids.
func formatMultiplier(m float64) string {
	return strconv.FormatFloat(m, 'f', -1, 64)
}

// parseMultiplier reads a typed multiplier: an optional trailing "x" is
// accepted, and the value must be a positive finite number. Nothing is
// clamped to a control's range and nothing is rounded to a step, so the text
// a user typed is what gets stored.
func parseMultiplier(text string) (float64, bool) {
	m, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "x")), 64)
	if err != nil || m <= 0 || math.IsInf(m, 0) || math.IsNaN(m) {
		return 0, false
	}
	return m, true
}

// providerMultipliers renders providerMultiplier for every provider, leaving
// out the 1× default so the card's field keeps its "1" placeholder. The value
// is exact: an arbitrary multiplier such as 0.0001234 and one above any
// control's range must survive rendering unchanged.
func providerMultipliers(rules []query.PriceRule, providers []string) map[string]string {
	out := make(map[string]string, len(providers))
	for _, prov := range providers {
		if m := providerMultiplier(rules, prov); m != 1 {
			out[prov] = formatMultiplier(m)
		}
	}
	return out
}

// providerMultiplierValues is providerMultiplier as the value a number input
// holds: every provider gets its multiplier, the 1× default included.
func providerMultiplierValues(rules []query.PriceRule, providers []string) map[string]float64 {
	out := make(map[string]float64, len(providers))
	for _, prov := range providers {
		m := providerMultiplier(rules, prov)
		if m <= 0 {
			m = 1
		}
		out[prov] = m
	}
	return out
}

// holdMultiplier remembers the value submitted for a provider until a reload
// confirms it, allocating the map the first time one is needed.
func (s *State) holdMultiplier(provider string, m float64) {
	if s.pr.pending == nil {
		s.pr.pending = map[string]float64{}
	}
	s.pr.pending[provider] = m
}

// queueMultiplier applies a multiplier the user just set: straight away when
// no write is running, or once the one in flight has landed.
func (s *State) queueMultiplier(provider string, m float64) {
	if m <= 0 || math.IsInf(m, 0) || math.IsNaN(m) {
		s.pr.message = tr("badMultiplier")
		return
	}
	if s.pr.busy {
		if s.pr.parked == nil {
			s.pr.parked = map[string]float64{}
			s.pr.parkedGen = s.pr.gen
		}
		s.pr.parked[provider] = m
		return
	}
	s.holdMultiplier(provider, m)
	s.setMultiplier(provider, formatMultiplier(m))
}

// submitMultiplier applies the text a user typed into a provider's multiplier
// field. The text is what is stored: no step rounding and no clamping.
func (s *State) submitMultiplier(provider, text string) {
	m, ok := parseMultiplier(text)
	if !ok {
		s.pr.message = tr("badMultiplier")
		return
	}
	s.holdMultiplierText(provider, text)
	s.queueMultiplier(provider, m)
}

// holdMultiplierText remembers the text a field holds until what it says is
// stored, so a frame that rebuilds the card cannot fall back to the old rule
// before the edit is submitted or confirmed.
func (s *State) holdMultiplierText(provider, text string) {
	if s.pr.typed == nil {
		s.pr.typed = map[string]string{}
	}
	s.pr.typed[provider] = text
}

// flushMultipliers writes one parked edit, but only once the reload that
// follows the write which parked it has landed: writing sooner would send the
// rule list from before that write.
//
// A parked value the rules already carry is dropped without a write, because
// there is nothing to write and no reload would follow. That drop must not
// consume the reload the remaining parked edits wait for: dropping it used to
// advance parkedGen anyway, so a same-value first edit stranded every edit
// queued behind it.
func (s *State) flushMultipliers() {
	p := &s.pr
	if p.busy || len(p.parked) == 0 || p.gen == p.parkedGen {
		return
	}
	provs := make([]string, 0, len(p.parked))
	for prov := range p.parked {
		provs = append(provs, prov)
	}
	sort.Strings(provs)
	for _, prov := range provs {
		m, ok := p.parked[prov]
		if !ok {
			continue
		}
		delete(p.parked, prov)
		if providerMultiplier(p.rules, prov) == m {
			continue
		}
		p.parkedGen = p.gen
		s.holdMultiplier(prov, m)
		s.setMultiplier(prov, formatMultiplier(m))
		return
	}
}

// mutate runs a settings change off the UI thread, then reloads everything
// (costs and model names may all have moved).
func (s *State) mutate(fn func(ctx context.Context, st query.Settings) (string, error)) {
	st := s.settings()
	if st == nil || s.pr.busy {
		return
	}
	s.pr.busy = true
	s.run("pricing-write", func(ctx context.Context) func() {
		msg, err := fn(ctx, st)
		return func() {
			s.pr.busy = false
			s.pr.message = msg
			if err != nil {
				s.pr.message = tr("failed") + ": " + err.Error()
				// Nothing carries the value the user typed, so let the input
				// fall back to what the rules do say.
				s.pr.pending = nil
				s.pr.typed = nil
			}
			s.loadPricing()
			s.Reload()
		}
	})
}

func (s *State) addAlias(from, provider, to string) {
	aliases := append([]query.ModelAlias(nil), s.pr.aliases...)
	aliases = append(aliases, query.ModelAlias{From: from, To: to, Provider: provider})
	s.pr.open = ""
	s.mutate(func(ctx context.Context, st query.Settings) (string, error) {
		return trf("mapped", from, to), st.SetModelAliases(ctx, aliases)
	})
}

func (s *State) removeAlias(i int) {
	aliases := append([]query.ModelAlias(nil), s.pr.aliases[:i]...)
	aliases = append(aliases, s.pr.aliases[i+1:]...)
	s.mutate(func(ctx context.Context, st query.Settings) (string, error) {
		return "", st.SetModelAliases(ctx, aliases)
	})
}

func (s *State) setMultiplier(provider, text string) {
	m, ok := parseMultiplier(text)
	if !ok {
		s.pr.message = tr("badMultiplier")
		return
	}
	var rules []query.PriceRule
	found := false
	for _, r := range s.pr.rules {
		// Only the undated user rule is ours to edit; cc-switch and relay:*
		// rules (and dated history) are kept as they are — user rules win anyway.
		if r.Provider == provider && r.Model == "" && r.Input == nil && r.Output == nil &&
			(r.Source == "user" || r.Source == "") && r.From.IsZero() {
			found = true
			if m == 1 {
				continue // 1× is no rule at all
			}
			r.Multiplier, r.Source = m, "user"
		}
		rules = append(rules, r)
	}
	if !found && m != 1 {
		rules = append(rules, query.PriceRule{Provider: provider, Multiplier: m, Source: "user"})
	}
	s.mutate(func(ctx context.Context, st query.Settings) (string, error) {
		return trf("multiplierSet", provider, formatMultiplier(m)), st.SetPriceRules(ctx, rules)
	})
}

func (s *State) searchCatalog() {
	st := s.settings()
	q := strings.TrimSpace(s.pr.needle)
	if st == nil || q == s.pr.lastQ {
		return
	}
	s.pr.lastQ = q
	if q == "" {
		s.pr.hits = nil
		return
	}
	s.run("catalog", func(ctx context.Context) func() {
		hits, _ := st.SearchCatalog(ctx, q, 8)
		return func() {
			if strings.TrimSpace(s.pr.needle) == q {
				s.pr.hits = hits
			}
		}
	})
}

// pricingCards are the settings page's pricing section.
func (s *State) pricingCards(c *ui.Context, pal palette) {
	if s.settings() == nil {
		return
	}
	if !s.pr.loaded {
		s.pr.loaded = true // once: the load sets it again with the data
		s.loadPricing()
	}
	p := &s.pr

	// Models without a price, each with suggested mappings.
	card(c, pal, tr("unpricedTitle"), func() {
		if ui.Button(c, tr("importCCSwitch")).Clicked() {
			s.mutate(func(ctx context.Context, st query.Settings) (string, error) {
				n, err := st.ImportCCSwitch(ctx)
				return trf("imported", n), err
			})
		}
	}, func() {
		if p.message != "" {
			ui.Row(c).Padding(8, 10).Radius(10).Background(Taki.Alpha(0.12)).Children(func() {
				ui.Text(c, p.message).FontSize(12).TextColor(pal.ink)
			})
		}
		if len(p.unpriced) == 0 {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				dot(c, Rana, 8)
				ui.Text(c, tr("allPriced")).FontSize(12.5).TextColor(pal.muted)
			})
			return
		}
		ui.Text(c, tr("unpricedSub")).FontSize(12).TextColor(pal.muted)
		for i, m := range p.unpriced {
			if i > 0 {
				ui.Divider(c)
			}
			s.unpricedRow(c.Key("u/"+m.Provider+"/"+m.Model), pal, m)
		}
	})

	if len(p.aliases) > 0 {
		card(c, pal, tr("aliasesTitle"), nil, func() {
			for i, a := range p.aliases {
				ui.Row(c.Key("a/"+a.Provider+"/"+a.From)).Gap(10).Padding(4, 0).AlignItems(ui.Center).Children(func() {
					ui.Text(c, a.From).FontSize(13).FontWeight(600).TextColor(pal.ink).SingleLine().Shrink(1).Ellipsis("…")
					ui.Text(c, "→").FontSize(13).TextColor(pal.muted)
					ui.Text(c, a.To).FontSize(13).FontWeight(650).TextColor(Taki).SingleLine().Shrink(1).Ellipsis("…")
					if a.Provider != "" {
						chipOn(c, pal, "@"+a.Provider, Tomori)
					}
					ui.Spacer(c)
					ui.Text(c, tr("remove")).FontSize(12).TextColor(Anon).Cursor(ui.CursorPointer).Role(ui.RoleButton).OnClick(func() { s.removeAlias(i) })
				})
			}
		})
	}

	// Relays often bill a fraction of list price: one multiplier per provider.
	if len(p.providers) > 0 {
		s.flushMultipliers()
		card(c, pal, tr("multipliersTitle"), nil, func() {
			ui.Text(c, tr("multipliersSub")).FontSize(12).TextColor(pal.muted)
			provs := append([]string(nil), p.providers...)
			sort.Strings(provs)
			for _, prov := range provs {
				row := ui.Row(c.Key("m/"+prov)).Gap(10).Padding(3, 0).AlignItems(ui.Center)
				row.Children(func() {
					dot(c, bandAt(len(prov)), 8)
					ui.Text(c, prov).FontSize(13).FontWeight(600).TextColor(pal.ink).Grow(1).Basis(0).SingleLine().Ellipsis("…")
					stored := p.mult[prov]
					if stored == "" {
						stored = "1"
					}
					// A multiplier is money, so the field keeps the exact text
					// the rules carry and an explicit Apply writes it. A
					// number control was tried and rejected: its range clamps
					// an out-of-range multiplier and its step rounds a typed
					// one, either of which would change what is billed.
					txt := ui.Local(row, "multiplier", func() string { return stored })
					if txt == nil {
						return
					}
					in := ui.TextInput(c, txt).Label(trf("multiplierLabel", prov)).Placeholder("1")
					if in.Changed() {
						s.holdMultiplierText(prov, *txt)
					}
					// Apply is checked before the fallback below, so the click
					// that submits the text cannot be preceded by the field
					// losing the focus and reverting to the stored value.
					if ui.Button(c, tr("apply")).Label(trf("multiplierApply", prov)).Clicked() ||
						in.Shortcut(0, ui.KeyEnter) {
						s.submitMultiplier(prov, *txt)
					} else if len(p.typed) == 0 && len(p.pending) == 0 && len(p.parked) == 0 && *txt != stored {
						*txt = stored
					}
					ui.Text(c, "×").FontSize(13).TextColor(pal.muted)
				})
			}
			custom := 0
			for _, r := range p.rules {
				if r.Model != "" {
					custom++
				}
			}
			if custom > 0 {
				ui.Text(c, trf("customPrices", custom)).FontSize(11.5).TextColor(pal.muted)
			}
		})
	}
}

func (s *State) unpricedRow(c *ui.Context, pal palette, m query.UnpricedModel) {
	p := &s.pr
	key := m.Provider + "/" + m.Model
	ui.Column(c).Gap(8).Padding(4, 0).Children(func() {
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			dot(c, Soyo, 8)
			if m.Model == "" {
				ui.Text(c, tr("noModelName")).FontSize(13.5).FontWeight(650).TextColor(pal.muted).SingleLine()
			} else {
				ui.Text(c, m.Model).FontSize(13.5).FontWeight(650).TextColor(pal.ink).SingleLine().Shrink(1).Ellipsis("…")
			}
			if m.Provider != "" {
				ui.Text(c, "@"+m.Provider).FontSize(12).TextColor(pal.muted).SingleLine()
			}
			ui.Spacer(c)
			ui.Text(c, fmtTokens(m.Tokens)+" · "+trf("nReq", fmtInt(m.Requests))).FontSize(12).TextColor(pal.muted).FontFeatures("tnum").SingleLine()
		})
		if m.Model == "" {
			// Nothing to map: the log never named a model.
			ui.Text(c, tr("noModelNameSub")).FontSize(12).TextColor(pal.muted)
			return
		}
		ui.Row(c).Gap(6).AlignItems(ui.Center).Wrap().Children(func() {
			if len(m.Suggestions) > 0 {
				ui.Text(c, tr("mapTo")).FontSize(12).TextColor(pal.muted)
			}
			for _, sug := range m.Suggestions {
				suggestion(c.Key(sug), pal, sug, func() { s.addAlias(m.Model, m.Provider, sug) })
			}
			label := tr("other")
			if p.open == key {
				label = tr("cancel")
			}
			ui.Text(c, label).FontSize(12).FontWeight(600).TextColor(Tomori).Cursor(ui.CursorPointer).Role(ui.RoleButton).Padding(3, 6).OnClick(func() {
				if p.open == key {
					p.open = ""
					return
				}
				p.open, p.needle, p.lastQ, p.hits = key, "", "", nil
			})
		})
		if p.open == key {
			// The catalog query stays ours; the box is the native combo so
			// the list opens, highlights and chooses the way other pickers do.
			cb := ui.ComboboxBase(c, &p.needle)
			in := cb.Input.Label(tr("searchCatalog")).Placeholder(tr("searchCatalog"))
			if th := c.Theme(); th != nil {
				in.Padding(th.Space(1.5), th.Space(2.5)).Radius(th.Radius).Background(th.Surface).Border(1, th.Border)
			}
			in.FillWidth()
			if cb.Filtering() {
				s.searchCatalog()
			}
			cb.Popup(func(panel ui.Element) {
				panel.Padding(4).Radius(10).Background(pal.raised).Border(1, pal.edge).Shadow(0, 8, 22, -6, pal.lift).Children(func() {
					for _, h := range p.hits {
						item := cb.Item(h).Padding(5, 8).Radius(6)
						if item.Highlighted() {
							item.Background(pal.hover)
						}
						item.Children(func() {
							ui.Text(c, h).FontSize(12).FontWeight(600).TextColor(pal.ink).SingleLine().Ellipsis("…")
						})
					}
				})
			})
			if h, ok := cb.Chosen(); ok {
				p.open, p.needle, p.lastQ, p.hits = "", "", "", nil
				s.addAlias(m.Model, m.Provider, h)
			} else if len(p.hits) == 0 && strings.TrimSpace(p.needle) != "" {
				ui.Text(c, tr("noMatch")).FontSize(12).TextColor(pal.muted)
			}
		}
	})
}

// suggestion is a clickable catalog model pill.
func suggestion(c *ui.Context, pal palette, name string, onClick func()) {
	e := ui.Row(c).Padding(3, 10).Radius(99).Shrink(0).Cursor(ui.CursorPointer).Role(ui.RoleButton).Label(name)
	bg := Taki.Alpha(0.12)
	if e.Hovered() {
		bg = Taki.Alpha(0.22)
	}
	e.Background(bg).OnClick(onClick).Children(func() {
		ink := Taki.Mix(ui.RGB(0, 0, 0), 0.25)
		if pal.dark {
			ink = Taki.Mix(ui.RGB(255, 255, 255), 0.4)
		}
		ui.Text(c, name).FontSize(12).FontWeight(600).TextColor(ink).SingleLine()
	})
}
