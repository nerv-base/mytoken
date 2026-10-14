package gui

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/zzstar101/mytoken/internal/budget"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/reporttime"
)

// settingsFrameBudget is the settings page's per-frame budget for the work
// MyToken does on the UI thread: building the view. The dataset below is a
// grown-up index (24 providers, 6 aliases, 12 unpriced models, 3 sources, 6
// budget rules) and the median build measured 0.19-0.37 ms on an M-series Mac
// (up to ~0.9 ms at p90 when the machine is busy), so the budget keeps a wide
// margin for shared CI while still failing on a regression that would matter.
// The headless tester rasterizes on the CPU (glass, shadows, text at scale 2)
// at a cost the GPU does not pay, so this times the view callback, never the
// whole tester frame.
const settingsFrameBudget = 5 * time.Millisecond

// testClock is a clock the test moves by hand, so reload pacing is decided by
// the test and not by how long the machine took.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time { return c.t }

// bigSettings is the demo settings service with a grown-up pricing table.
type bigSettings struct {
	*demoSettings
	providers []string
	rules     []query.PriceRule
	aliases   []query.ModelAlias
	unpriced  []query.UnpricedModel
}

func (b *bigSettings) Providers(context.Context) ([]string, error) { return b.providers, nil }
func (b *bigSettings) PriceRules(context.Context) ([]query.PriceRule, error) {
	return b.rules, nil
}
func (b *bigSettings) ModelAliases(context.Context) ([]query.ModelAlias, error) {
	return b.aliases, nil
}
func (b *bigSettings) UnpricedModels(context.Context) ([]query.UnpricedModel, error) {
	return b.unpriced, nil
}

func makeBigSettings(providers, aliases, unpriced int) *bigSettings {
	b := &bigSettings{demoSettings: NewDemoSettings().(*demoSettings)}
	for i := 0; i < providers; i++ {
		b.providers = append(b.providers, fmt.Sprintf("provider-%02d", i))
	}
	for i := 0; i < 6; i++ {
		b.rules = append(b.rules, query.PriceRule{
			Provider: fmt.Sprintf("provider-%02d", i), Multiplier: 0.3 + float64(i)/10, Source: "user"})
	}
	for i := 0; i < aliases; i++ {
		b.aliases = append(b.aliases, query.ModelAlias{
			From: fmt.Sprintf("alias-%03d", i), To: fmt.Sprintf("catalog-%03d", i),
			Provider: fmt.Sprintf("provider-%02d", i%max(providers, 1))})
	}
	for i := 0; i < unpriced; i++ {
		b.unpriced = append(b.unpriced, query.UnpricedModel{
			Model: fmt.Sprintf("unpriced-%03d-high", i), Provider: fmt.Sprintf("provider-%02d", i%max(providers, 1)),
			Requests: int64(10 + i), Tokens: int64(1_000_000 * (i + 1)),
			Suggestions: []string{"claude-opus-4-5", "claude-sonnet-4-5", "gpt-5-codex"}})
	}
	return b
}

func bigBudget(rules int) *fakeBudget {
	cfg := budget.Config{}
	for i := 0; i < rules; i++ {
		cfg.Rules = append(cfg.Rules, budget.Rule{
			ID: fmt.Sprintf("rule-%03d", i), Version: 1,
			Scope:  budget.Scope{Kind: budget.ScopeGlobal},
			Period: reporttime.Month, Currency: "USD", Limit: float64(100 + i),
			Label: fmt.Sprintf("monthly cap %d", i)})
	}
	return &fakeBudget{cfg: cfg}
}

// bigState mirrors the running app: every settings hook non-nil, a sizeable
// pricing table and a handful of sources.
func bigState(tb testing.TB, q query.Service, clock *testClock) *State {
	tb.Helper()
	return bigStateWith(tb, q, clock, makeBigSettings(24, 6, 12))
}

// bigStateWith is bigState with the pricing service supplied by the test, so a
// counting wrapper can observe what the settings page loads.
func bigStateWith(tb testing.TB, q query.Service, clock *testClock, settings query.Settings) *State {
	tb.Helper()
	fs := &fakeSources{}
	for i := 0; i < 3; i++ {
		fs.info = append(fs.info, SourceInfo{
			ID: fmt.Sprintf("inst-%02d", i), Label: fmt.Sprintf("instance %02d", i),
			Harness: model.Harness("claude"), Roots: []string{fmt.Sprintf("~/inst-%02d/logs", i)},
			Source: "user", Kind: "instance", Scanned: true})
	}
	s := NewState(q, Hooks{
		Settings:     settings,
		Relays:       NewDemoRelays(clock.t),
		Budget:       bigBudget(6),
		Calendar:     &fakeCalendar{zone: "Asia/Shanghai"},
		Sources:      fs,
		ClaudeQuota:  &fakeClaudeQuota{},
		QuotaStore:   fakeQuotaStore{},
		Now:          clock.now,
		Version:      "0.4.8",
		DataDir:      "~/Library/Application Support/MyToken",
		Progress:     func() (int, int) { return 10, 10 },
		CheckUpdates: func() {},
		AutoUpdates:  func() bool { return true },
	}, nil)
	s.Start()
	return s
}

// countingQuery counts the two aggregate queries the rule editor's scope
// picker needs, and answers them with fixed identities. Other pages aggregate
// over the shown span too, so only the picker's own 90-day window is counted
// (budget.go scopeOptions is the only caller that asks for it).
type countingQuery struct {
	query.Service
	now            func() time.Time
	byProject      int
	providerGroups int
}

func (q *countingQuery) scopes(f query.Filter) bool {
	return f.Range.From.Equal(q.now().AddDate(0, 0, -90))
}

func (q *countingQuery) ByProject(_ context.Context, f query.Filter) ([]query.Bucket, error) {
	if q.scopes(f) {
		q.byProject++
	}
	return []query.Bucket{{Key: "proj-a", Label: "a"}, {Key: "proj-b", Label: "b"}}, nil
}

func (q *countingQuery) ProviderGroups(_ context.Context, f query.Filter) ([]query.ProviderGroup, error) {
	if q.scopes(f) {
		q.providerGroups++
	}
	return []query.ProviderGroup{{Key: "endpoint:v1:x", Label: "x"}}, nil
}

// buildMeter records how long the view callback takes, frame by frame.
type buildMeter struct {
	d     time.Duration
	calls int
}

func (m *buildMeter) reset() { m.d, m.calls = 0, 0 }

func metered(s *State, m *buildMeter) func(*ui.Context) {
	return func(c *ui.Context) {
		start := time.Now()
		s.MainView(c)
		m.d += time.Since(start)
		m.calls++
	}
}

func settingsTester(tb testing.TB, s *State, m *buildMeter) *ui.Tester {
	tb.Helper()
	tt := ui.NewTester(metered(s, m), 1280, 860)
	tt.SetScale(1)
	tt.SetTitleBar(ui.TitleBar{Height: 66, Left: 82})
	tt.SetPreferences(ui.Preferences{ReduceMotion: true})
	return tt
}

// pageBuilds renders a page for n settled frames and returns each frame's view
// build time, dropping the frames that drew no view at all.
func pageBuilds(tb testing.TB, s *State, page string, warm, n int) []time.Duration {
	tb.Helper()
	m := &buildMeter{}
	tt := settingsTester(tb, s, m)
	s.SetPage(page)
	for i := 0; i < warm; i++ {
		tt.Frame()
	}
	var out []time.Duration
	for i := 0; i < n; i++ {
		m.reset()
		tt.Frame()
		if m.calls > 0 {
			out = append(out, m.d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// TestSettingsFrameBudget times the settings page's view build, and overview
// and ranking for comparison, on a dataset the size of a real index. It
// reports the numbers always and fails over budget only under
// MYTOKEN_STRICT_PERF=1 (shared CI runners are noisy).
func TestSettingsFrameBudget(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)}
	s := bigState(t, NewDemoService(clock.t), clock)
	// One pass over every page first: fonts and shared caches are built once.
	for _, page := range []string{"overview", "ranking", "settings"} {
		pageBuilds(t, s, page, 15, 1)
	}
	for _, page := range []string{"overview", "ranking", "settings"} {
		got := pageBuilds(t, s, page, 5, 12)
		if len(got) < 8 {
			t.Fatalf("%s: only %d of 12 frames built the view; the harness no longer measures a frame", page, len(got))
		}
		med, max := got[len(got)/2], got[len(got)-1]
		t.Logf("%-8s view build: med %-10v p90 %-10v max %-10v", page, med, got[len(got)*9/10], max)
		if page != "settings" {
			continue
		}
		if med > settingsFrameBudget && os.Getenv("MYTOKEN_STRICT_PERF") == "1" {
			t.Fatalf("settings view build %v over budget %v", med, settingsFrameBudget)
		}
	}
}

// TestSettingsScopeOptionsLoadOnce pins the cost that used to run on every
// settings-page reload: the rule editor's scope choices, two aggregate
// queries over 90 days of usage each. The page draws them only inside the
// editor, so nothing else may pay for them.
func TestSettingsScopeOptionsLoadOnce(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)}
	q := &countingQuery{Service: NewDemoService(clock.t), now: clock.now}
	s := bigState(t, q, clock)
	s.SetPage("settings")
	m := &buildMeter{}
	tt := settingsTester(t, s, m)
	for i := 0; i < 3; i++ {
		tt.Frame()
	}
	if q.byProject != 1 || q.providerGroups != 1 {
		t.Fatalf("the page loaded the scope choices %d/%d times, want 1/1", q.byProject, q.providerGroups)
	}
	// Scanner commits reload the visible page (state.go reloadGap). With no
	// editor open the choices are not on screen: they must not be queried.
	for i := 0; i < 5; i++ {
		clock.t = clock.t.Add(3 * time.Second)
		s.changed()
		tt.Frame()
	}
	if q.byProject != 1 || q.providerGroups != 1 {
		t.Fatalf("a closed editor re-queried the scope choices: %d/%d", q.byProject, q.providerGroups)
	}
	// Opening the editor needs them, and it refreshes them.
	s.budgetEdit(budget.Rule{})
	tt.Frame()
	if q.byProject != 2 || q.providerGroups != 2 {
		t.Fatalf("opening the editor queried the scope choices %d/%d times, want 2/2", q.byProject, q.providerGroups)
	}
	if len(s.bg.projects) == 0 || len(s.bg.providers) == 0 {
		t.Fatalf("the editor has no scope choices: %d projects, %d providers", len(s.bg.projects), len(s.bg.providers))
	}
}

func BenchmarkSettingsFrame(b *testing.B) {
	clock := &testClock{t: time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)}
	s := bigState(b, NewDemoService(clock.t), clock)
	pageBuilds(b, s, "settings", 15, 1)
	m := &buildMeter{}
	tt := settingsTester(b, s, m)
	s.SetPage("settings")
	b.ResetTimer()
	for b.Loop() {
		m.reset()
		tt.Frame()
	}
}
