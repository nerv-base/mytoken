package gui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/zzstar101/mytoken/internal/budget"
	"github.com/zzstar101/mytoken/internal/reporttime"
)

// fakeBudget is the settings page's budget service, without a store: it proves
// the card reads the rules, the last status and the timezone through the
// injected interfaces and nothing else.
type fakeBudget struct {
	cfg    budget.Config
	st     budget.Status
	loads  int
	saved  []budget.Config
	checks int
	err    error
}

func (f *fakeBudget) LoadConfig(context.Context) (budget.Config, error) {
	f.loads++
	return f.cfg, f.err
}

func (f *fakeBudget) SaveConfig(_ context.Context, cfg budget.Config) error {
	if f.err != nil {
		return f.err
	}
	f.saved = append(f.saved, cfg)
	f.cfg = cfg
	return nil
}

func (f *fakeBudget) Status() budget.Status { return f.st }

func (f *fakeBudget) Evaluate(context.Context) error {
	f.checks++
	return nil
}

type fakeCalendar struct {
	zone    string
	err     error
	setErr  error
	needs   bool
	problem string
	preview string
}

func (f *fakeCalendar) Timezone(context.Context) (string, error) { return f.zone, f.err }

func (f *fakeCalendar) TimezoneState() TimezoneState {
	return TimezoneState{
		Configured:     f.zone != "" && !f.needs,
		Name:           f.zone,
		NeedsSelection: f.needs,
		Problem:        f.problem,
		SystemPreview:  f.preview,
	}
}

func (f *fakeCalendar) SetTimezone(_ context.Context, name string) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.zone = name
	return nil
}

func renderMain(t *testing.T, s *State) {
	t.Helper()
	tt := ui.NewTester(func(c *ui.Context) { s.MainView(c) }, 1280, 860)
	tt.SetScale(1)
	tt.SetPreferences(ui.Preferences{ReduceMotion: true})
	for range 3 {
		tt.Frame()
	}
}

// TestSettingsPageRendersBudgetCard renders the settings page in sync mode
// with injected budget and calendar hooks: every branch of the report (a
// delivered check, a failed delivery, a skipped rule, an unknown price) must
// draw without panicking.
func TestSettingsPageRendersBudgetCard(t *testing.T) {
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)
	rule := budget.Rule{ID: "rule-1", Version: 1, Scope: budget.Scope{Kind: budget.ScopeGlobal}, Period: reporttime.Day, Currency: "USD", Limit: 10, Label: "daily"}
	fb := &fakeBudget{
		cfg: budget.Config{Rules: []budget.Rule{rule}},
		st: budget.Status{
			At: now, Timezone: "Asia/Shanghai", Notify: true,
			Evaluations: []budget.Evaluation{{
				RuleID: "rule-1", RuleVersion: 1, Label: "daily", Scope: rule.Scope, Period: reporttime.Day,
				PeriodID: "p", Currency: "USD", Limit: 10, Estimated: 9, Percent: 90,
				DataAvailable: true, UnknownPricing: true, Crossed: []float64{80},
			}},
			Deliveries: []budget.Delivery{
				{Notification: budget.Notification{ID: "n1", RuleID: "rule-1", Threshold: 80, Currency: "USD", Limit: 10, Estimated: 9, Percent: 90, UnknownPricing: true}, Outcome: budget.DeliveryDelivered},
				{Notification: budget.Notification{ID: "n2", RuleID: "rule-1", Threshold: 100, Currency: "USD", Limit: 10, Estimated: 12, Percent: 120}, Outcome: budget.DeliveryFailed, Err: errors.New("notifications denied")},
			},
			Skipped: []budget.SkippedRule{{Rule: budget.Rule{ID: "eur", Currency: "EUR"}, Reason: budget.SkipForeignCurrency}},
		},
	}
	fc := &fakeCalendar{zone: "Asia/Shanghai"}
	s := NewState(NewDemoService(now), Hooks{
		Settings: NewDemoSettings(),
		Budget:   fb,
		Calendar: fc,
		Now:      func() time.Time { return now },
		Progress: func() (int, int) { return 10, 10 },
	}, nil)
	s.Start()
	s.page = "settings"
	renderMain(t, s)

	if !s.bg.loaded || fb.loads == 0 {
		t.Fatalf("budget card did not load (loaded=%v loads=%d)", s.bg.loaded, fb.loads)
	}
	if len(s.bg.cfg.Rules) != 1 || s.bg.cfg.Rules[0].ID != "rule-1" {
		t.Fatalf("card rules = %+v, want the persisted rule", s.bg.cfg.Rules)
	}
	if len(s.bg.st.Deliveries) != 2 || s.bg.st.Deliveries[1].Outcome != budget.DeliveryFailed {
		t.Fatalf("card status = %+v, want the service's last snapshot", s.bg.st)
	}
	if s.bg.zone != "Asia/Shanghai" {
		t.Fatalf("card zone = %q, want the persisted calendar zone", s.bg.zone)
	}
	if !strings.Contains(s.bg.zoneIn, "Asia/Shanghai") {
		t.Fatalf("zone input = %q, want the current zone prefilled", s.bg.zoneIn)
	}

	// The report must say a failed delivery was not delivered, must call out
	// an estimate that excluded unpriced requests, and must not claim
	// exactly-once.
	report := reportText(s.bg.st)
	if !strings.Contains(report, tr("budgetFail")) {
		t.Fatalf("report = %q, want the failed-delivery wording", report)
	}
	if !strings.Contains(report, tr("budgetOnce")) {
		t.Fatalf("report = %q, want the at-most-once wording", report)
	}
	if !strings.Contains(report, tr("budgetLowerBound")) {
		t.Fatalf("report = %q, want the lower-bound wording for unpriced requests", report)
	}
	if !strings.Contains(report, budget.SkipForeignCurrency) {
		t.Fatalf("report = %q, want the skipped-rule reason", report)
	}
}

// TestSettingsPageShowsUnconfiguredCalendar renders the card with no usable
// fixed calendar: it must ask for an IANA choice, show the system zone only as
// a marked preview, and never present that zone as the stats timezone.
func TestSettingsPageShowsUnconfiguredCalendar(t *testing.T) {
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.UTC)
	fc := &fakeCalendar{
		zone:    "",
		err:     errors.New("stats_timezone: Not/AZone"),
		needs:   true,
		problem: "stats_timezone: Not/AZone",
		preview: "Europe/Berlin",
	}
	s := NewState(NewDemoService(now), Hooks{
		Settings: NewDemoSettings(),
		Calendar: fc,
		Now:      func() time.Time { return now },
	}, nil)
	s.Start()
	s.page = "settings"
	tt := ui.NewTester(func(c *ui.Context) { s.MainView(c) }, 1280, 860)
	tt.SetScale(1)
	tt.SetPreferences(ui.Preferences{ReduceMotion: true})
	for range 3 {
		tt.Frame()
	}

	if s.bg.zone != "" {
		t.Fatalf("card zone = %q, want empty: nothing is configured", s.bg.zone)
	}
	if !s.bg.needsZone || s.bg.zonePreview != "Europe/Berlin" {
		t.Fatalf("card state = needs %v preview %q, want a preview only", s.bg.needsZone, s.bg.zonePreview)
	}
	texts := strings.Join(tt.Texts(), "\n")
	if !strings.Contains(texts, tr("budgetTzNeeds")) {
		t.Fatalf("card does not ask for a timezone choice; texts %v", tt.Texts())
	}
	if !strings.Contains(texts, fmt.Sprintf(tr("budgetTzPreview"), "Europe/Berlin")) {
		t.Fatalf("card does not mark the system zone as a preview; texts %v", tt.Texts())
	}
	if !strings.Contains(texts, tr("budgetTzUnset")) {
		t.Fatalf("card claims a configured zone; texts %v", tt.Texts())
	}
}

func reportText(st budget.Status) string {
	var b strings.Builder
	for _, line := range reportLines(st) {
		b.WriteString(line.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestEvaluationTextCoversMissingAndUnknownData: a rule with no data, a
// skipped rule and a lower-bound estimate each say so in plain words.
func TestEvaluationTextCoversMissingAndUnknownData(t *testing.T) {
	if got := evaluationText(budget.Evaluation{DataAvailable: false}); got != tr("budgetNoData") {
		t.Fatalf("no-data evaluation = %q, want %q", got, tr("budgetNoData"))
	}
	if got := evaluationText(budget.Evaluation{SkippedReason: budget.SkipDisabled}); got != budget.SkipDisabled {
		t.Fatalf("skipped evaluation = %q, want the reason", got)
	}
	got := evaluationText(budget.Evaluation{DataAvailable: true, Currency: "USD", Estimated: 9, Limit: 10, Percent: 90, UnknownPricing: true, UnpricedRequests: 3})
	if !strings.Contains(got, "9.00") || !strings.Contains(got, "10.00") || !strings.Contains(got, "90%") {
		t.Fatalf("estimate text = %q", got)
	}
	if !strings.Contains(got, trf("budgetUnknown", 3)) {
		t.Fatalf("estimate text = %q, want the unpriced-request count", got)
	}
	plain := evaluationText(budget.Evaluation{DataAvailable: true, Currency: "USD", Estimated: 1, Limit: 2, Percent: 50})
	if strings.Contains(plain, "lower bound") || strings.Contains(plain, tr("budgetUnknown")) {
		t.Fatalf("fully priced estimate = %q, want no unknown-price note", plain)
	}
}

// TestBudgetCardAbsentWithoutHooks: a build with no budget service draws no
// card and does not panic.
func TestBudgetCardAbsentWithoutHooks(t *testing.T) {
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)
	s := NewState(NewDemoService(now), Hooks{Settings: NewDemoSettings(), Now: func() time.Time { return now }}, nil)
	s.Start()
	s.page = "settings"
	renderMain(t, s)
	if s.bg.loaded {
		t.Fatal("budget card loaded with no budget hook")
	}
}

func TestBudgetChangedReloadsWhenVisible(t *testing.T) {
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)
	fb := &fakeBudget{}
	s := NewState(NewDemoService(now), Hooks{Settings: NewDemoSettings(), Budget: fb, Now: func() time.Time { return now }}, nil)
	s.Start()
	s.page = "settings"
	s.BudgetChanged()
	if !s.bg.loaded || fb.loads == 0 {
		t.Fatalf("BudgetChanged did not reload the visible card (loaded=%v loads=%d)", s.bg.loaded, fb.loads)
	}
	// Off the settings page it only marks the card stale.
	s.page = "overview"
	before := fb.loads
	s.BudgetChanged()
	if s.bg.loaded || fb.loads != before {
		t.Fatalf("hidden card reloaded: loaded=%v loads=%d", s.bg.loaded, fb.loads)
	}
}

func TestParseThresholdsIsStrict(t *testing.T) {
	got, err := parseThresholds(" 80, 100 ")
	if err != nil || len(got) != 2 || got[0] != 80 || got[1] != 100 {
		t.Fatalf("parseThresholds(80, 100) = %v, %v", got, err)
	}
	if got, err := parseThresholds("50;75 90，95"); err != nil || len(got) != 4 {
		t.Fatalf("parseThresholds(mixed separators) = %v, %v", got, err)
	}
	if got, err := parseThresholds(""); err != nil || got != nil {
		t.Fatalf("parseThresholds(empty) = %v, %v; want nil for the defaults", got, err)
	}
	for _, bad := range []string{"abc", "0", "-5", "101", "80,80", "NaN", "Inf"} {
		if _, err := parseThresholds(bad); err == nil {
			t.Fatalf("parseThresholds(%q) = nil error, want a rejection", bad)
		}
	}
}

func TestDraftRuleValidates(t *testing.T) {
	base := draft{label: "daily", scopeIdx: 0, period: "day", currency: "USD", limit: "10", thresholds: "80, 100", enabled: true}
	cfg := budget.Config{}
	r, err := base.rule(cfg)
	if err != nil {
		t.Fatalf("rule: %v", err)
	}
	if r.Scope.Kind != budget.ScopeGlobal || r.Period != reporttime.Day || r.Limit != 10 || r.Version != 1 || r.Disabled {
		t.Fatalf("rule = %+v", r)
	}
	if r.ID == "" || !strings.Contains(r.ID, "global") {
		t.Fatalf("generated id = %q, want a readable stable id", r.ID)
	}
	if _, found := cfg.Find(r.ID); found {
		t.Fatalf("Find(%q) found the rule in the old config", r.ID)
	}

	project := base
	project.scopeIdx = 1
	if _, err := project.rule(cfg); err == nil {
		t.Fatal("project rule without a stable id was accepted")
	}
	project.scopeID = "/p/one"
	if r, err := project.rule(cfg); err != nil || r.Scope.ID != "/p/one" {
		t.Fatalf("project rule = %+v, %v", r, err)
	}

	// A lowercase or padded currency is normalized, not rejected: the same
	// code in another case can never blend two currencies.
	lower := base
	lower.currency = " usd "
	if r, err := lower.rule(cfg); err != nil || r.Currency != "USD" {
		t.Fatalf("lowercase currency = %+v, %v; want the canonical USD", r, err)
	}

	for name, mut := range map[string]func(*draft){
		"zero limit":      func(d *draft) { d.limit = "0" },
		"negative limit":  func(d *draft) { d.limit = "-3" },
		"junk limit":      func(d *draft) { d.limit = "ten" },
		"short ccy":       func(d *draft) { d.currency = "us" },
		"digit ccy":       func(d *draft) { d.currency = "US1" },
		"empty ccy":       func(d *draft) { d.currency = "" },
		"bad threshold":   func(d *draft) { d.thresholds = "0" },
		"unknown period":  func(d *draft) { d.period = "quarter" },
		"bad scope index": func(d *draft) { d.scopeIdx = 9 },
	} {
		d := base
		mut(&d)
		if _, err := d.rule(cfg); err == nil {
			t.Fatalf("draft %s was accepted", name)
		}
	}

	paused := base
	paused.enabled = false
	r, err = paused.rule(cfg)
	if err != nil || !r.Disabled {
		t.Fatalf("paused rule = %+v, %v; want Disabled", r, err)
	}
	// Empty thresholds mean the 80/100 defaults.
	plain := base
	plain.thresholds = ""
	r, err = plain.rule(cfg)
	if err != nil {
		t.Fatalf("rule with default thresholds: %v", err)
	}
	if got := r.EffectiveThresholds(); len(got) != 2 || got[0] != 80 || got[1] != 100 {
		t.Fatalf("EffectiveThresholds = %v, want 80/100", got)
	}
}

func TestNextRuleIDAvoidsCollisions(t *testing.T) {
	scope := budget.Scope{Kind: budget.ScopeProject, ID: "/p/one"}
	first := nextRuleID(budget.Config{}, scope, reporttime.Week)
	if strings.Contains(first, ":") || strings.Contains(first, "/") {
		t.Fatalf("id %q is not filesystem- and settings-friendly", first)
	}
	cfg := budget.Config{Rules: []budget.Rule{{ID: first, Version: 1, Scope: scope, Period: reporttime.Week, Currency: "USD", Limit: 1}}}
	if second := nextRuleID(cfg, scope, reporttime.Week); second == first {
		t.Fatalf("collision: %q reused", second)
	}
}

func TestThresholdTextShowsDefaults(t *testing.T) {
	r := budget.Rule{ID: "r", Version: 1, Scope: budget.Scope{Kind: budget.ScopeGlobal}, Period: reporttime.Day, Currency: "USD", Limit: 10}
	if got := thresholdText(r); got != "80/100%" {
		t.Fatalf("thresholdText = %q, want 80/100%%", got)
	}
	r.Thresholds = []float64{50}
	if got := thresholdText(r); got != "50%" {
		t.Fatalf("thresholdText = %q, want 50%%", got)
	}
}
