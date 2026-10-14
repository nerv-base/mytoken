package gui

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"

	"github.com/zzstar101/mytoken/internal/budget"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/reporttime"
)

// Budget is the settings page's view of the budget service: the persisted
// rules, the last evaluation, and a way to check now. nil hides the card.
type Budget interface {
	// LoadConfig returns the persisted rules.
	LoadConfig(ctx context.Context) (budget.Config, error)
	// SaveConfig validates and persists cfg. Saving alone notifies nobody.
	SaveConfig(ctx context.Context, cfg budget.Config) error
	// Status is the last evaluation snapshot.
	Status() budget.Status
	// Evaluate runs one check now.
	Evaluate(ctx context.Context) error
}

// Calendar is the persisted stats timezone every calendar period resolves in.
// nil hides the timezone row.
type Calendar interface {
	// Timezone returns the effective zone name and any problem with the
	// stored value. An empty name means the calendar is unresolved.
	Timezone(ctx context.Context) (string, error)
	// SetTimezone validates and persists an explicit change.
	SetTimezone(ctx context.Context, name string) error
	// TimezoneState reports whether a fixed calendar is configured, so the
	// card can ask for a choice and show the system zone as a preview only.
	TimezoneState() TimezoneState
}

// TimezoneState mirrors the app's calendar state through reporttime, the one
// shared definition: whether a fixed calendar is configured, what to report
// when it is not, and the system zone as a clearly-marked preview only.
type TimezoneState = reporttime.TimezoneState

// scopeKinds is the editor's order; a draft holds an index into it.
var scopeKinds = []budget.ScopeKind{budget.ScopeGlobal, budget.ScopeProject, budget.ScopeProvider}

// budgetState is the settings page's budget section: the persisted rules, the
// one rule being edited, and the last delivery report.
type budgetState struct {
	loaded  bool
	cfg     budget.Config
	st      budget.Status
	err     error
	busy    bool
	message string
	open    bool
	draft   draft

	// scope choices, from the last 90 days of usage.
	scopes    bool
	projects  []scopeOption
	providers []scopeOption

	tzLoaded    bool
	zone        string
	zoneErr     error
	zoneIn      string
	tzMsg       string
	needsZone   bool
	zonePreview string
}

// draft is the rule editor's working copy: text in, parsed on save.
type draft struct {
	id         string
	label      string
	scopeIdx   int
	scopeID    string
	period     string
	currency   string
	limit      string
	thresholds string
	enabled    bool
	err        string
}

// scopeOption is one stable identity a rule can name.
type scopeOption struct {
	ID    string
	Label string
}

// budgetCard draws the budget section: the rules, the editor, what the last
// check found, and the stats timezone the periods are resolved in.
func (s *State) budgetCard(c *ui.Context, pal palette) {
	if s.Hooks.Budget == nil && s.Hooks.Calendar == nil {
		return
	}
	if !s.bg.loaded && s.Hooks.Budget != nil {
		s.bg.loaded = true // once; the load sets it again with the data
		s.loadBudget()
	}
	if !s.bg.tzLoaded && s.Hooks.Calendar != nil {
		s.bg.tzLoaded = true
		s.loadZone()
	}
	if s.Hooks.Budget != nil {
		s.budgetRulesCard(c, pal)
	}
	if s.Hooks.Calendar != nil {
		s.budgetZoneCard(c, pal)
	}
}

// BudgetChanged refreshes the card after the service evaluated. The service
// calls it through post, so it runs on the UI thread. The card reloads when it
// is on screen; otherwise the next draw picks up the new snapshot. Nothing
// here writes back to the service, so the card cannot feed itself.
func (s *State) BudgetChanged() {
	s.bg.loaded = false
	if s.Hooks.Budget != nil && s.page == "settings" {
		s.loadBudget()
	}
}

// loadBudget refreshes the rules and, when the editor that shows them is
// open, the scope choices. The choices cost two aggregate queries over 90 days
// of usage each time they load, so they are loaded once with the card and
// after that only while an editor is open: no other part of the page reads
// them, and a reload every couple of seconds on the settings page must not
// pay for a picker nobody has open.
func (s *State) loadBudget() {
	b := s.Hooks.Budget
	if b == nil {
		return
	}
	s.run("budget", func(ctx context.Context) func() {
		cfg, err := b.LoadConfig(ctx)
		st := b.Status()
		return func() {
			s.bg.cfg, s.bg.st, s.bg.err, s.bg.loaded = cfg, st, err, true
		}
	})
	if s.bg.open || !s.bg.scopes {
		s.bg.scopes = true
		s.loadScopeOptions()
	}
}

// loadScopeOptions loads the identities the rule editor's scope picker offers.
func (s *State) loadScopeOptions() {
	s.run("budget-scopes", func(ctx context.Context) func() {
		projects, providers := s.scopeOptions(ctx)
		return func() { s.bg.projects, s.bg.providers = projects, providers }
	})
}

// scopeOptions lists the stable identities a rule can name, seen in the last
// 90 days: project paths, and the endpoint identity keys the usage pages use.
func (s *State) scopeOptions(ctx context.Context) (projects, providers []scopeOption) {
	if s.Q == nil {
		return nil, nil
	}
	now := s.Hooks.Now()
	f := query.Filter{Range: query.Range{From: now.AddDate(0, 0, -90), To: now}}
	if bs, err := s.Q.ByProject(ctx, f); err == nil {
		for _, b := range bs {
			if b.Key != "" {
				projects = append(projects, scopeOption{ID: b.Key, Label: shortPath(b.Key)})
			}
		}
	}
	if gs, err := s.Q.ProviderGroups(ctx, f); err == nil {
		for _, g := range gs {
			if g.Key != "" {
				providers = append(providers, scopeOption{ID: g.Key, Label: g.Label})
			}
		}
	}
	return sortedOptions(projects), sortedOptions(providers)
}

// sortedOptions sorts by label and drops duplicate labels: the picker chooses
// by label, so two options must never read the same.
func sortedOptions(in []scopeOption) []scopeOption {
	sort.Slice(in, func(i, j int) bool { return in[i].Label < in[j].Label })
	out := make([]scopeOption, 0, len(in))
	seen := map[string]bool{}
	for _, o := range in {
		if seen[o.Label] {
			continue
		}
		seen[o.Label] = true
		out = append(out, o)
	}
	return out
}

func (s *State) loadZone() {
	cal := s.Hooks.Calendar
	if cal == nil {
		return
	}
	s.run("budget-zone", func(ctx context.Context) func() {
		name, err := cal.Timezone(ctx)
		st := cal.TimezoneState()
		return func() {
			s.bg.zone, s.bg.zoneErr = name, err
			s.bg.needsZone, s.bg.zonePreview = st.NeedsSelection, st.SystemPreview
			if s.bg.zoneIn == "" || s.bg.zoneIn == s.bg.zone {
				s.bg.zoneIn = name
			}
		}
	})
}

func (s *State) budgetRulesCard(c *ui.Context, pal palette) {
	card(c, pal, tr("budgetTitle"), func() {
		if ui.Button(c, tr("budgetAdd")).Clicked() {
			s.budgetEdit(budget.Rule{})
		}
	}, func() {
		if s.bg.message != "" {
			ui.Row(c).Padding(8, 10).Radius(10).Background(Taki.Alpha(0.12)).Children(func() {
				ui.Text(c, s.bg.message).FontSize(12).TextColor(pal.ink)
			})
		}
		if s.bg.err != nil {
			ui.Text(c, tr("failed")+": "+s.bg.err.Error()).FontSize(11.5).TextColor(pal.muted)
		}
		ui.Text(c, tr("budgetSource")).FontSize(11.5).TextColor(pal.muted)
		if len(s.bg.cfg.Rules) == 0 {
			ui.Text(c, tr("budgetNone")).FontSize(12).TextColor(pal.muted)
			ui.Text(c, tr("budgetNoneSub")).FontSize(11.5).TextColor(pal.muted)
		}
		evals := s.budgetEvals()
		for i := range s.bg.cfg.Rules {
			r := s.bg.cfg.Rules[i]
			var ev *budget.Evaluation
			if e, ok := evals[r.ID]; ok {
				ev = &e
			}
			ui.Divider(c)
			s.budgetRuleRow(c, pal, r, ev)
		}
		if s.bg.open {
			ui.Divider(c)
			s.budgetEditor(c, pal)
		}
		s.budgetReport(c, pal)
	})
}

func (s *State) budgetEvals() map[string]budget.Evaluation {
	m := make(map[string]budget.Evaluation, len(s.bg.st.Evaluations))
	for _, ev := range s.bg.st.Evaluations {
		m[ev.RuleID] = ev
	}
	return m
}

// budgetRuleRow is one configured rule: what it watches, how it is doing, and
// the switch, edit and delete controls.
func (s *State) budgetRuleRow(c *ui.Context, pal palette, r budget.Rule, ev *budget.Evaluation) {
	title := r.Label
	if title == "" {
		title = scopeText(r.Scope)
	}
	if r.Disabled {
		title += " · " + tr("budgetPaused")
	}
	sub := fmt.Sprintf("%s · %s · %s %s · %s",
		scopeText(r.Scope), r.Period, r.Currency, trimNum(r.Limit, 2), thresholdText(r))
	if ev != nil {
		sub += " · " + evaluationText(*ev)
	}
	settingRow(c, pal, title, sub, func() {
		ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
			on := !r.Disabled
			if ui.Switch(c, &on).Changed() {
				s.budgetToggle(r, on)
			}
			if ui.Button(c, tr("budgetEdit")).Clicked() {
				s.budgetEdit(r)
			}
			if ui.Button(c, tr("remove")).Clicked() {
				s.budgetDelete(r.ID)
			}
		})
	})
}

// evaluationText says what the last check found for one rule: its estimate,
// and plainly when that estimate is a lower bound or missing entirely.
func evaluationText(ev budget.Evaluation) string {
	if !ev.DataAvailable {
		if ev.SkippedReason != "" {
			return ev.SkippedReason
		}
		return tr("budgetNoData")
	}
	out := fmt.Sprintf(tr("budgetEstimate"), ev.Currency, ev.Estimated, ev.Currency, ev.Limit, ev.Percent)
	if ev.UnknownPricing {
		out += " " + fmt.Sprintf(tr("budgetUnknown"), ev.UnpricedRequests)
	}
	return out
}

// reportLine is one line of the delivery report; Warn lines are drawn in the
// alert colour.
type reportLine struct {
	Text string
	Warn bool
}

// reportLines is the report's text, free of rendering: what the last check
// did, every refused notification, and the at-most-once contract. A delivery
// whose estimate excluded unpriced requests says so, because that estimate is
// only a lower bound.
func reportLines(st budget.Status) []reportLine {
	line := tr("budgetNever")
	if !st.At.IsZero() {
		line = tr("budgetCheck") + " " + st.At.Format("15:04:05")
	}
	if st.Notify {
		line += " · " + tr("budgetNotify")
	} else {
		line += " · " + tr("budgetNoNotif")
	}
	out := []reportLine{{Text: line}}
	if st.Err != nil {
		out = append(out, reportLine{Text: tr("failed") + ": " + st.Err.Error(), Warn: true})
	}
	for _, d := range st.Deliveries {
		label := d.Notification.Label
		if label == "" {
			label = scopeText(d.Notification.Scope)
		}
		cur := reportLine{Text: fmt.Sprintf("%s %.0f%% · %s", label, d.Notification.Threshold, tr("budgetSent"))}
		switch d.Outcome {
		case budget.DeliveryFailed:
			cur = reportLine{Text: fmt.Sprintf("%s %.0f%% · %s: %v", label, d.Notification.Threshold, tr("budgetFail"), d.Err), Warn: true}
		case budget.DeliveryNoNotifier:
			cur = reportLine{Text: fmt.Sprintf("%s %.0f%% · %s", label, d.Notification.Threshold, tr("budgetNoNotif"))}
		}
		if d.Notification.UnknownPricing {
			cur.Text += " · " + tr("budgetLowerBound")
		}
		out = append(out, cur)
	}
	for _, sk := range st.Skipped {
		out = append(out, reportLine{Text: scopeText(sk.Rule.Scope) + " · " + sk.Reason})
	}
	out = append(out, reportLine{Text: tr("budgetOnce")})
	return out
}

// budgetReport is the delivery report: what the last check did, including
// every refused notification, and the at-most-once contract.
func (s *State) budgetReport(c *ui.Context, pal palette) {
	ui.Divider(c)
	for _, line := range reportLines(s.bg.st) {
		col := pal.muted
		if line.Warn {
			col = Taki
		}
		ui.Text(c, line.Text).FontSize(11.5).TextColor(col)
	}
}

// budgetEditor is the rule form. It writes a whole rule at once, so a
// half-typed limit is never persisted.
func (s *State) budgetEditor(c *ui.Context, pal palette) {
	d := &s.bg.draft
	head := tr("budgetAdd")
	if d.id != "" {
		head = tr("budgetEdit")
	}
	kicker(c, pal, head)
	if d.err != "" {
		ui.Text(c, d.err).FontSize(11.5).TextColor(Taki)
	}
	settingRow(c, pal, tr("budgetName"), tr("budgetNameSub"), func() {
		ui.TextInput(c, &d.label).Width(200)
	})
	settingRow(c, pal, tr("budgetScope"), tr("budgetScopeSub"), func() {
		if segmented(c, pal, &d.scopeIdx, tr("budgetScopeGlobal"), tr("budgetScopeProject"), tr("budgetScopeProvider")) {
			d.scopeID = ""
		}
	})
	if d.scopeIdx != 0 {
		kind := scopeKinds[d.scopeIdx]
		opts := s.bg.projects
		if kind == budget.ScopeProvider {
			opts = s.bg.providers
		}
		labels := make([]string, 0, len(opts))
		for _, o := range opts {
			labels = append(labels, o.Label)
		}
		settingRow(c, pal, tr("budgetScopeID"), tr("budgetScopeIDSub"), func() {
			ui.Column(c).Gap(6).AlignItems(ui.End).Children(func() {
				if len(labels) > 0 {
					sel := ""
					for _, o := range opts {
						if o.ID == d.scopeID {
							sel = o.Label
							break
						}
					}
					if ui.Select(c, &sel, labels).Changed() {
						for _, o := range opts {
							if o.Label == sel {
								d.scopeID = o.ID
								break
							}
						}
					}
				}
				ui.TextInput(c, &d.scopeID).Width(260)
			})
		})
	}
	settingRow(c, pal, tr("budgetPeriod"), tr("budgetPeriodSub"), func() {
		ui.Select(c, &d.period, []string{string(reporttime.Day), string(reporttime.Week), string(reporttime.Month)})
	})
	settingRow(c, pal, tr("budgetLimit"), tr("budgetLimitSub"), func() {
		ui.TextInput(c, &d.limit).Width(110).Placeholder("50")
	})
	settingRow(c, pal, tr("budgetThresholds"), tr("budgetThresholdsSub"), func() {
		ui.TextInput(c, &d.thresholds).Width(150).Placeholder("80,100")
	})
	settingRow(c, pal, tr("budgetCurrency"), tr("budgetCurrencySub"), func() {
		ui.TextInput(c, &d.currency).Width(70)
	})
	settingRow(c, pal, tr("budgetEnabled"), "", func() {
		on := d.enabled
		if ui.Switch(c, &on).Changed() {
			d.enabled = on
		}
	})
	ui.Row(c).Gap(8).Padding(6, 0).Children(func() {
		if ui.Button(c, tr("budgetSave")).Clicked() {
			s.budgetSave()
		}
		if ui.Button(c, tr("cancel")).Clicked() {
			s.bg.open, s.bg.draft = false, draft{}
		}
	})
}

func (s *State) budgetEdit(r budget.Rule) {
	d := draft{
		id:         r.ID,
		label:      r.Label,
		scopeIdx:   scopeIndex(r.Scope.Kind),
		scopeID:    r.Scope.ID,
		period:     string(r.Period),
		currency:   r.Currency,
		limit:      trimNum(r.Limit, 2),
		thresholds: formatThresholds(r.Thresholds),
		enabled:    !r.Disabled,
	}
	if d.period == "" {
		d.period = string(reporttime.Day)
	}
	if d.currency == "" {
		d.currency = budget.LocalCurrency
	}
	s.bg.open, s.bg.draft, s.bg.message = true, d, ""
	// The picker is on screen now: bring its choices up to date.
	s.loadScopeOptions()
}

// budgetSave writes the drafted rule. A change to the rule's identity (scope,
// period, currency, limit or thresholds) starts a new generation, which is
// what Evaluate needs to accept it — and which is why such a change can
// notify again for a threshold already reached this period. Pausing and
// renaming keep the generation, so they never re-notify.
func (s *State) budgetSave() {
	b := s.Hooks.Budget
	if b == nil {
		return
	}
	r, err := s.bg.draft.rule(s.bg.cfg)
	if err != nil {
		s.bg.draft.err = err.Error()
		return
	}
	cfg := s.bg.cfg
	rules := append([]budget.Rule(nil), cfg.Rules...)
	if old, ok := cfg.Find(r.ID); ok {
		r.Version = old.Version
		if r.Fingerprint() != old.Fingerprint() {
			r.Version++
		}
		for i := range rules {
			if rules[i].ID == r.ID {
				rules[i] = r
				break
			}
		}
	} else {
		r.Version = 1
		rules = append(rules, r)
	}
	cfg.Rules = rules
	s.bg.open, s.bg.draft = false, draft{}
	s.budgetWrite(cfg, tr("budgetSaved"))
}

// budgetToggle pauses or resumes a rule. It keeps the generation, so the
// ledger's watermark and reservations survive.
func (s *State) budgetToggle(r budget.Rule, on bool) {
	cfg := s.bg.cfg
	rules := append([]budget.Rule(nil), cfg.Rules...)
	for i := range rules {
		if rules[i].ID == r.ID {
			rules[i].Disabled = !on
		}
	}
	cfg.Rules = rules
	s.budgetWrite(cfg, tr("budgetSaved"))
}

func (s *State) budgetDelete(id string) {
	cfg := s.bg.cfg
	rules := make([]budget.Rule, 0, len(cfg.Rules))
	for _, r := range cfg.Rules {
		if r.ID != id {
			rules = append(rules, r)
		}
	}
	cfg.Rules = rules
	s.budgetWrite(cfg, tr("budgetDeleted"))
}

// budgetWrite persists cfg and checks at once, so the card shows the effect
// without waiting for the next store commit.
func (s *State) budgetWrite(cfg budget.Config, msg string) {
	b := s.Hooks.Budget
	if b == nil || s.bg.busy {
		return
	}
	s.bg.busy = true
	s.run("budget-write", func(ctx context.Context) func() {
		err := b.SaveConfig(ctx, cfg)
		if err == nil {
			err = b.Evaluate(ctx)
		}
		st := b.Status()
		return func() {
			s.bg.busy = false
			s.bg.st = st
			if err != nil {
				s.bg.message = tr("failed") + ": " + err.Error()
				return
			}
			s.bg.cfg, s.bg.message = cfg, msg
		}
	})
}

// budgetZoneCard is the stats timezone: the zone every calendar period is
// resolved in, persisted under stats_timezone for the snapshot worker to read.
// While no fixed calendar is chosen it says so and shows the machine's zone as
// a preview only: the app never claims a fixed zone it did not persist.
func (s *State) budgetZoneCard(c *ui.Context, pal palette) {
	card(c, pal, tr("budgetTz"), nil, func() {
		ui.Text(c, tr("budgetTzSub")).FontSize(11.5).TextColor(pal.muted)
		if s.bg.needsZone {
			msg := tr("budgetTzNeeds")
			if s.bg.zonePreview != "" {
				msg += " " + fmt.Sprintf(tr("budgetTzPreview"), s.bg.zonePreview)
			}
			ui.Text(c, msg).FontSize(11.5).TextColor(Taki)
		}
		if s.bg.zoneErr != nil {
			ui.Text(c, tr("budgetTzErr")+": "+s.bg.zoneErr.Error()).FontSize(11.5).TextColor(Taki)
		}
		if s.bg.tzMsg != "" {
			ui.Text(c, s.bg.tzMsg).FontSize(11.5).TextColor(pal.ink)
		}
		shown := s.bg.zone
		if shown == "" {
			shown = tr("budgetTzUnset")
		}
		settingRow(c, pal, tr("budgetTz"), shown, func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.TextInput(c, &s.bg.zoneIn).Width(200).Placeholder("Asia/Shanghai")
				if ui.Button(c, tr("budgetTzApply")).Clicked() {
					s.budgetSetZone()
				}
			})
		})
	})
}

func (s *State) budgetSetZone() {
	cal := s.Hooks.Calendar
	if cal == nil || s.bg.busy {
		return
	}
	name := strings.TrimSpace(s.bg.zoneIn)
	s.bg.busy = true
	s.run("budget-zone-write", func(ctx context.Context) func() {
		err := cal.SetTimezone(ctx, name)
		return func() {
			s.bg.busy = false
			if err != nil {
				s.bg.tzMsg = tr("budgetTzBad") + ": " + err.Error()
				return
			}
			// Re-read the state instead of assuming the typed text: the saved
			// name is the validated one the app adopted.
			s.bg.tzMsg = tr("budgetTzSaved")
			s.loadZone()
			// The dashboard and the tray resolve their calendar days in the
			// stats zone too, so the change reloads them, not only the card.
			s.Reload()
			s.loadBudget()
		}
	})
}

// rule builds the rule the draft describes, validating every field before
// anything is persisted.
func (d draft) rule(cfg budget.Config) (budget.Rule, error) {
	if d.scopeIdx < 0 || d.scopeIdx >= len(scopeKinds) {
		return budget.Rule{}, errors.New("bad scope")
	}
	scope := budget.Scope{Kind: scopeKinds[d.scopeIdx]}
	if scope.Kind != budget.ScopeGlobal {
		scope.ID = strings.TrimSpace(d.scopeID)
		if scope.ID == "" {
			return budget.Rule{}, errors.New("a project or provider rule needs a stable id")
		}
	}
	limit, err := strconv.ParseFloat(strings.TrimSpace(d.limit), 64)
	if err != nil || math.IsNaN(limit) || math.IsInf(limit, 0) || limit <= 0 {
		return budget.Rule{}, fmt.Errorf("limit %q is not a positive amount", d.limit)
	}
	currency, err := budget.ParseCurrency(d.currency)
	if err != nil {
		return budget.Rule{}, err
	}
	thresholds, err := parseThresholds(d.thresholds)
	if err != nil {
		return budget.Rule{}, err
	}
	period := reporttime.Kind(d.period)
	id := d.id
	if id == "" {
		id = nextRuleID(cfg, scope, period)
	}
	r := budget.Rule{
		ID:         id,
		Version:    1,
		Scope:      scope,
		Period:     period,
		Currency:   currency,
		Limit:      limit,
		Thresholds: thresholds,
		Disabled:   !d.enabled,
		Label:      strings.TrimSpace(d.label),
	}
	if err := r.Validate(); err != nil {
		return budget.Rule{}, err
	}
	return r, nil
}

// parseThresholds reads "80, 100" strictly: every value must be a number
// above 0 and at most 100, and none may repeat. Empty means the defaults.
func parseThresholds(text string) ([]float64, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '，'
	})
	out := make([]float64, 0, len(fields))
	seen := make(map[float64]bool, len(fields))
	for _, f := range fields {
		v, err := strconv.ParseFloat(f, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("threshold %q is not a number", f)
		}
		if v <= 0 || v > 100 {
			return nil, fmt.Errorf("threshold %s must be above 0 and at most 100", trimNum(v, 2))
		}
		if seen[v] {
			return nil, fmt.Errorf("threshold %s is repeated", trimNum(v, 2))
		}
		seen[v] = true
		out = append(out, v)
	}
	return out, nil
}

func formatThresholds(ts []float64) string {
	parts := make([]string, 0, len(ts))
	for _, t := range ts {
		parts = append(parts, trimNum(t, 2))
	}
	return strings.Join(parts, ", ")
}

// thresholdText is what a row shows: the rule's own thresholds, or the
// defaults when it has none.
func thresholdText(r budget.Rule) string {
	parts := make([]string, 0, 2)
	for _, t := range r.EffectiveThresholds() {
		parts = append(parts, trimNum(t, 1))
	}
	return strings.Join(parts, "/") + "%"
}

// nextRuleID makes a stable, readable id from the scope and period, adding a
// suffix when the user already has one like it.
func nextRuleID(cfg budget.Config, scope budget.Scope, period reporttime.Kind) string {
	base := "budget-" + ruleIDPart(scope.String()) + "-" + string(period)
	id := base
	for i := 2; ; i++ {
		if _, taken := cfg.Find(id); !taken {
			return id
		}
		id = fmt.Sprintf("%s-%d", base, i)
	}
}

// ruleIDPart keeps a generated rule id readable and free of characters that
// read oddly in a settings key or a filename. A project id is a path, so its
// separators collapse into one dash.
func ruleIDPart(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_'
		if !ok {
			if b.Len() == 0 || lastDash {
				continue
			}
			b.WriteByte('-')
			lastDash = true
			continue
		}
		b.WriteRune(r)
		lastDash = false
	}
	return strings.Trim(b.String(), "-")
}

func scopeIndex(k budget.ScopeKind) int {
	for i, s := range scopeKinds {
		if s == k {
			return i
		}
	}
	return 0
}

func scopeTitle(k budget.ScopeKind) string {
	switch k {
	case budget.ScopeProject:
		return tr("budgetScopeProject")
	case budget.ScopeProvider:
		return tr("budgetScopeProvider")
	}
	return tr("budgetScopeGlobal")
}

// scopeText names a scope for a row: the stable id is what identifies it, so
// it is always shown next to the kind.
func scopeText(s budget.Scope) string {
	if s.Kind == budget.ScopeGlobal || s.ID == "" {
		return scopeTitle(s.Kind)
	}
	return scopeTitle(s.Kind) + " " + s.ID
}
