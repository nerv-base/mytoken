package gui

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/provider"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/store"
)

// providerErrService fails the provider breakdown while everything else works.
type providerErrService struct {
	query.Service
	err error
}

func (s *providerErrService) ProviderGroups(context.Context, query.Filter) ([]query.ProviderGroup, error) {
	return nil, s.err
}

// providerGateService holds the provider breakdown until it is released or its
// context is canceled.
type providerGateService struct {
	query.Service
	groups atomic.Int32
	gate   chan struct{}
}

func (s *providerGateService) ProviderGroups(ctx context.Context, f query.Filter) ([]query.ProviderGroup, error) {
	s.groups.Add(1)
	if s.gate != nil {
		select {
		case <-s.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.Service.ProviderGroups(ctx, f)
}

// TestProviderErrorIsCarriedNotSwallowed pins that a failed provider query is
// reported, not turned into an empty provider list that would read as zero
// usage.
func TestProviderErrorIsCarriedNotSwallowed(t *testing.T) {
	now := providerTestNow()
	boom := errors.New("provider index unavailable")
	ov, err := loadSpan(t.Context(), &providerErrService{Service: NewDemoService(now), err: boom}, SpanAll, now, time.Local)
	if err != nil {
		t.Fatalf("a provider error must not fail the whole overview load: %v", err)
	}
	if !errors.Is(ov.ProviderErr, boom) {
		t.Fatalf("ProviderErr = %v, want %v", ov.ProviderErr, boom)
	}
	if len(ov.Providers) != 0 {
		t.Fatalf("a failed query returned %d groups", len(ov.Providers))
	}
}

// TestProviderErrorIsShownInGUI pins that both provider views say what went
// wrong instead of drawing an empty breakdown.
func TestProviderErrorIsShownInGUI(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	now := providerTestNow()
	boom := errors.New("provider index unavailable")
	s := providerGUI(t, &providerErrService{Service: NewDemoService(now), err: boom}, Hooks{})

	tt := renderProviderTab(t, s)
	if !tt.HasText(tr("failed")) {
		t.Error("the providers tab does not report the failed query")
	}
	if !tt.HasText(boom.Error()) {
		t.Error("the providers tab does not say why the query failed")
	}
	if tt.HasText(tr("noData")) {
		t.Error("a failed provider query is drawn as no data, which reads as zero usage")
	}

	s.SetPage("overview")
	tt.Frame()
	if !tt.HasText(boom.Error()) {
		t.Error("the overview provider card does not report the failed query")
	}
}

// TestProviderGroupsCanceledLoadIsNotAFailure pins that cancellation is not an
// error: a load canceled while the provider query runs applies nothing.
func TestProviderGroupsCanceledLoadIsNotAFailure(t *testing.T) {
	now := providerTestNow()
	q := &providerGateService{Service: NewDemoService(now), gate: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var ov Overview
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		ov, err = loadSpan(ctx, q, SpanAll, now, time.Local)
	}()
	waitFor(t, func() bool { return q.groups.Load() == 1 })
	cancel()
	<-done

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(ov.Providers) != 0 {
		t.Errorf("a canceled load applied %d provider groups", len(ov.Providers))
	}
}

// TestSpanSwitchCancelsProviderLoad pins that the provider breakdown obeys the
// same cancellation rule as the rest of the span: the abandoned span's groups
// are dropped, and cancellation never surfaces as a provider failure.
func TestSpanSwitchCancelsProviderLoad(t *testing.T) {
	now := providerTestNow()
	q := &providerGateService{Service: NewDemoService(now), gate: make(chan struct{})}
	l := newLoop()
	s := NewState(q, Hooks{Now: func() time.Time { return now }}, l.post)

	l.do(func() { s.span = int(Span90); s.setSpan() })
	waitFor(t, func() bool { return q.groups.Load() == 1 })
	l.do(func() { s.span = int(SpanAll); s.setSpan() })
	close(q.gate)
	waitFor(t, func() bool {
		var ok bool
		l.do(func() { ok = s.spans[SpanAll].ok })
		return ok
	})
	l.do(func() {
		if s.spans[Span90].ok {
			t.Error("the abandoned provider load was applied")
		}
		if s.ov.ProviderErr != nil {
			t.Errorf("a canceled provider load surfaced as a failure: %v", s.ov.ProviderErr)
		}
		if s.ov.Span != SpanAll {
			t.Errorf("ov.Span = %v", s.ov.Span)
		}
	})
}

// providerAliasCall is one write the GUI asked for.
type providerAliasCall struct{ id, label string }

// renameFixture is one identified endpoint with usage, ready to rename.
func renameFixture(t *testing.T) (*providerFixture, query.ProviderGroup, string) {
	t.Helper()
	f := newProviderFixture(t)
	const base = "https://api.example.com/v1"
	f.commit(t, model.Codex, model.UsageEvent{
		DedupKey: "a", SessionID: "s1", Model: "m", BaseURL: base, Tokens: model.Tokens{Input: 10},
	}, store.Resolution{Provider: "alpha", Cost: 2, Priced: true})
	gs := fixtureGroups(t, f)
	if len(gs) != 1 || !gs[0].Identified {
		t.Fatalf("want one identified endpoint group: %+v", gs)
	}
	return f, gs[0], base
}

// TestProviderAliasSaveAndClearKeepsIdentityFilterAndPrices pins the alias
// contract the GUI must honor: the write is addressed by the stable Key, it
// changes the display name only, and the identity, the usage filter and the
// prices are untouched. Clearing restores the endpoint name.
func TestProviderAliasSaveAndClearKeepsIdentityFilterAndPrices(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	f, g, base := renameFixture(t)
	before, err := f.q.Totals(t.Context(), query.Filter{ProviderIDs: []string{g.Key}})
	if err != nil {
		t.Fatal(err)
	}

	var calls []providerAliasCall
	s := providerGUI(t, f.q, Hooks{SetProviderAlias: func(ctx context.Context, id, label string) error {
		calls = append(calls, providerAliasCall{id: id, label: label})
		return f.st.SetProviderAlias(ctx, id, label)
	}})
	s.selectProvider(g.Key)
	if s.pv.key != g.Key {
		t.Fatalf("selection = %q, want the stable key %q", s.pv.key, g.Key)
	}
	s.pv.alias = "Friendly"
	tt := renderProviderTab(t, s)
	if err := tt.Click("Save"); err != nil {
		t.Fatalf("click Save: %v", err)
	}
	if len(calls) != 1 || calls[0].id != g.Key || calls[0].label != "Friendly" {
		t.Fatalf("alias write = %+v, want one write addressed to %q", calls, g.Key)
	}

	gs := fixtureGroups(t, f)
	if len(gs) != 1 || gs[0].Key != g.Key {
		t.Fatalf("the identity moved after a rename: %+v", gs)
	}
	if gs[0].Label != "Friendly" {
		t.Fatalf("label = %q, want Friendly", gs[0].Label)
	}
	if !tt.HasText("Friendly") || !tt.HasText(shortTail(g.Key)[:8]) {
		t.Error("the renamed row must still show its stable identity")
	}
	after, err := f.q.Totals(t.Context(), query.Filter{ProviderIDs: []string{g.Key}})
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("usage for %s changed across a rename: %+v -> %+v", g.Key, before, after)
	}

	tt.Frame()
	if err := tt.Click("Clear"); err != nil {
		t.Fatalf("click Clear: %v", err)
	}
	gs = fixtureGroups(t, f)
	if len(gs) != 1 || gs[0].Key != g.Key {
		t.Fatalf("the identity moved after clearing: %+v", gs)
	}
	if gs[0].Label == "Friendly" {
		t.Fatal("the alias was not cleared")
	}
	if want, ok := provider.NormalizeEndpoint(base); ok && gs[0].Label != want {
		t.Errorf("label after clearing = %q, want the endpoint %q", gs[0].Label, want)
	}
}

// TestProviderAliasOnlyForIdentifiedEndpoints pins that an endpoint without a
// stable identity cannot be named, and that the UI says so rather than
// offering dead controls.
func TestProviderAliasOnlyForIdentifiedEndpoints(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	f := newProviderFixture(t)
	f.commit(t, model.Codex, model.UsageEvent{
		DedupKey: "a", SessionID: "s1", Model: "m", BaseURL: "http://127.0.0.1:8080/v1", Tokens: model.Tokens{Input: 4},
	}, store.Resolution{Provider: "local"})
	gs := fixtureGroups(t, f)
	if len(gs) != 1 || gs[0].Identified {
		t.Fatalf("want one unidentified group: %+v", gs)
	}
	g := gs[0]

	var calls int
	s := providerGUI(t, f.q, Hooks{SetProviderAlias: func(context.Context, string, string) error {
		calls++
		return nil
	}})
	s.selectProvider(g.Key)
	s.setProviderAlias(g, "Nope")
	if calls != 0 {
		t.Fatalf("an unidentified endpoint was renamed (%d writes)", calls)
	}
	tt := renderProviderTab(t, s)
	if !tt.HasText(tr("providerAliasUnidentified")) {
		t.Error("the UI does not explain why an unidentified endpoint cannot be renamed")
	}
	if tt.HasText(tr("providerAliasSave")) || tt.HasText(tr("providerAliasClear")) {
		t.Error("the alias controls are offered for an endpoint with no stable identity")
	}
}

// TestProviderAliasUnavailableWithoutStore pins that a build without the
// alias store hides the editor instead of offering a control that cannot work.
func TestProviderAliasUnavailableWithoutStore(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	f, g, _ := renameFixture(t)
	s := providerGUI(t, f.q, Hooks{})
	s.selectProvider(g.Key)
	tt := renderProviderTab(t, s)
	if !tt.HasText(tr("providerAliasUnavailable")) {
		t.Error("the UI does not say that no display-name store is wired in")
	}
	if tt.HasText(tr("providerAliasSave")) || tt.HasText(tr("providerAliasClear")) {
		t.Error("the alias controls are offered without a store to write to")
	}
}

// TestProviderAliasErrorIsShownAndRetryable pins the failure path: a rejected
// write is reported, leaves the display name alone, and does not wedge the
// editor.
func TestProviderAliasErrorIsShownAndRetryable(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	f, g, _ := renameFixture(t)
	boom := errors.New("alias store is down")
	fail := true
	s := providerGUI(t, f.q, Hooks{SetProviderAlias: func(ctx context.Context, id, label string) error {
		if fail {
			return boom
		}
		return f.st.SetProviderAlias(ctx, id, label)
	}})
	s.selectProvider(g.Key)
	s.pv.alias = "Friendly"
	tt := renderProviderTab(t, s)
	if err := tt.Click("Save"); err != nil {
		t.Fatalf("click Save: %v", err)
	}
	if s.pv.err != boom.Error() {
		t.Fatalf("pv.err = %q, want %q", s.pv.err, boom.Error())
	}
	if s.pv.busy {
		t.Error("the editor stayed busy after a failed write")
	}
	if !tt.HasText(boom.Error()) {
		t.Error("a failed alias write is not shown")
	}
	if label := fixtureGroups(t, f)[0].Label; label == "Friendly" {
		t.Fatal("a failed write changed the display name")
	}

	fail = false
	tt.Frame()
	if err := tt.Click("Save"); err != nil {
		t.Fatalf("click Save again: %v", err)
	}
	if s.pv.err != "" {
		t.Errorf("pv.err after a successful retry = %q", s.pv.err)
	}
	if !tt.HasText(trf("providerAliasSaved", "Friendly")) {
		t.Error("a successful retry is not confirmed")
	}
	if fixtureGroups(t, f)[0].Label != "Friendly" {
		t.Error("the retry did not apply the display name")
	}
}

// pricingProviderSpy records what the pricing pickers are given.
type pricingProviderSpy struct {
	query.Settings
	provs []string
	asked atomic.Int32
}

func (p *pricingProviderSpy) Providers(context.Context) ([]string, error) {
	p.asked.Add(1)
	return p.provs, nil
}

// TestAliasNeverReachesPricingPickers pins the boundary: renaming an endpoint
// never moves an endpoint identity or an alias into the pricing views, which
// speak in attribution provider names.
func TestAliasNeverReachesPricingPickers(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	f, g, _ := renameFixture(t)
	spy := &pricingProviderSpy{Settings: NewDemoSettings(), provs: []string{"alpha"}}
	s := providerGUI(t, f.q, Hooks{Settings: spy, SetProviderAlias: f.st.SetProviderAlias})
	s.selectProvider(g.Key)
	s.pv.alias = "Friendly"
	tt := renderProviderTab(t, s)
	if err := tt.Click("Save"); err != nil {
		t.Fatalf("click Save: %v", err)
	}
	if got := fixtureGroups(t, f)[0].Label; got != "Friendly" {
		t.Fatalf("the alias was not applied: %q", got)
	}

	s.loadPricing()
	if spy.asked.Load() == 0 {
		t.Fatal("the pricing pickers were never loaded")
	}
	if got := s.pr.providers; len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("pricing providers = %v, want the attribution provider [alpha]", got)
	}
	for _, p := range s.pr.providers {
		if strings.Contains(p, "endpoint:v1:") || p == "Friendly" {
			t.Errorf("an endpoint identity or alias reached the pricing pickers: %q", p)
		}
	}

	s.SetPage("settings")
	tt.Frame()
	if tt.HasText("Friendly") {
		t.Error("the settings page shows an endpoint alias")
	}
	if tt.HasText(shortTail(g.Key)[:8]) {
		t.Error("the settings page shows an endpoint identity")
	}
}

// TestPagesRenderWithDemoEndpointGroups smoke-renders every page with the demo
// data the app ships, so the provider change cannot break a page that no
// focused test covers. It writes no snapshots. The demo providers have no
// endpoint URLs, so the demo tab must label them unidentified rather than
// inventing an endpoint identity.
func TestPagesRenderWithDemoEndpointGroups(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	now := providerTestNow()
	s := NewState(NewDemoService(now), Hooks{
		Settings: NewDemoSettings(),
		Relays:   NewDemoRelays(now),
		Now:      func() time.Time { return now },
		DataDir:  "~/Library/Application Support/MyToken",
		Progress: func() (int, int) { return 10, 10 },
	}, nil)
	s.Start()
	tt := ui.NewTester(s.MainView, 1280, 860)
	for _, page := range []string{"overview", "sessions", "ranking", "projects", "cache", "relays", "settings"} {
		s.page = page
		tt.Frame()
		if len(tt.Texts()) == 0 {
			t.Errorf("page %q rendered no text", page)
		}
	}
	s.page = "ranking"
	s.rankTab = 1
	tt.Frame()
	if !tt.HasText(tr("providerUnidentified")) {
		t.Error("the demo providers tab does not label its synthetic providers honestly")
	}
}

// twoRenameFixture is two identified endpoints with usage, for the
// selection-change tests below.
func twoRenameFixture(t *testing.T) (*providerFixture, query.ProviderGroup, query.ProviderGroup) {
	t.Helper()
	f := newProviderFixture(t)
	const baseA, baseB = "https://a.example.com/v1", "https://b.example.com/v1"
	f.commit(t, model.Codex, model.UsageEvent{
		DedupKey: "a", SessionID: "s1", Model: "m", BaseURL: baseA, Tokens: model.Tokens{Input: 10},
	}, store.Resolution{Provider: "alpha", Cost: 2, Priced: true})
	f.commit(t, model.Codex, model.UsageEvent{
		DedupKey: "b", SessionID: "s2", Model: "m", BaseURL: baseB, Tokens: model.Tokens{Input: 5},
	}, store.Resolution{Provider: "beta", Cost: 1, Priced: true})
	gs := fixtureGroups(t, f)
	if len(gs) != 2 || !gs[0].Identified || !gs[1].Identified || gs[0].Key == gs[1].Key {
		t.Fatalf("want two identified endpoints: %+v", gs)
	}
	return f, gs[0], gs[1]
}

// groupLabel is the display name the store currently reports for one key.
func groupLabel(t *testing.T, f *providerFixture, key string) string {
	t.Helper()
	for _, g := range fixtureGroups(t, f) {
		if g.Key == key {
			return g.Label
		}
	}
	return ""
}

// TestProviderDetailShowsFullStableKeyWhenShortPrefixesCollide pins that the
// compact label is a truncation, not an identity: two keys whose ten-character
// prefix is the same stay tellable apart because the detail shows the whole
// stable key. A real sha256 pair cannot collide there, so the pair is
// synthetic — the label must not claim what it cannot prove.
func TestProviderDetailShowsFullStableKeyWhenShortPrefixesCollide(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	const keyA = "endpoint:v1:0123456789aaaaaaaa"
	const keyB = "endpoint:v1:0123456789bbbbbbbb"
	if providerKeyShort(keyA) != providerKeyShort(keyB) {
		t.Fatalf("setup: the compact labels differ (%q vs %q), so this pair does not exercise a collision",
			providerKeyShort(keyA), providerKeyShort(keyB))
	}
	now := providerTestNow()
	s := providerGUI(t, NewDemoService(now), Hooks{})
	s.ov.Providers = []query.ProviderGroup{
		{Bucket: query.Bucket{Key: keyA, Label: "a.example.com"}, Identified: true,
			Members: []query.ProviderMember{{Harness: model.Codex, Provider: "alpha"}}},
		{Bucket: query.Bucket{Key: keyB, Label: "b.example.com"}, Identified: true,
			Members: []query.ProviderMember{{Harness: model.Codex, Provider: "beta"}}},
	}

	s.selectProvider(keyA)
	tt := renderProviderTab(t, s)
	if !tt.HasText(keyA) {
		t.Errorf("the detail does not show the full stable key %q", keyA)
	}
	if tt.HasText(keyB) {
		t.Errorf("the detail for %s shows the other endpoint's key %q", providerKeyShort(keyA), keyB)
	}

	s.selectProvider(keyB)
	tt.Frame()
	if !tt.HasText(keyB) {
		t.Errorf("the detail does not show the full stable key %q", keyB)
	}
	if tt.HasText(keyA) {
		t.Errorf("the detail for %s shows the other endpoint's key %q", providerKeyShort(keyB), keyA)
	}
}

// TestProviderAliasCompletionAfterSelectionChangeLeavesTheOtherEditor pins the
// async rule: a write started for one group finishes on the UI thread and must
// release the shared busy state and refresh the data, but must never write its
// result — the cleared editor or the confirmation — into the group the user
// has since selected.
func TestProviderAliasCompletionAfterSelectionChangeLeavesTheOtherEditor(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	f, a, b := twoRenameFixture(t)

	entered := make(chan struct{})
	gate := make(chan struct{})
	l := newLoop()
	s := NewState(f.q, Hooks{
		Now: func() time.Time { return providerTestNow() },
		SetProviderAlias: func(ctx context.Context, id, label string) error {
			close(entered)
			<-gate
			return f.st.SetProviderAlias(ctx, id, label)
		},
	}, l.post)
	gs := fixtureGroups(t, f)
	l.do(func() { s.ov.Providers = gs })

	// Save A, then pick B while A's write is still in flight.
	l.do(func() {
		s.selectProvider(a.Key)
		s.pv.alias = "Friendly"
		s.setProviderAlias(a, "Friendly")
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the alias write never started")
	}
	l.do(func() {
		s.selectProvider(b.Key)
		s.pv.alias = "Bee"
		s.pv.msg = "B was here"
	})
	close(gate)
	waitFor(t, func() bool {
		var released bool
		l.do(func() { released = !s.pv.busy })
		return released
	})

	l.do(func() {
		if s.pv.key != b.Key {
			t.Errorf("selection = %q, want %q", s.pv.key, b.Key)
		}
		if s.pv.alias != "Bee" {
			t.Errorf("A's completion cleared B's editor: pv.alias = %q, want Bee", s.pv.alias)
		}
		if s.pv.msg != "B was here" {
			t.Errorf("A's confirmation was written into B's status: pv.msg = %q", s.pv.msg)
		}
		if s.pv.err != "" {
			t.Errorf("A's success was reported as B's error: pv.err = %q", s.pv.err)
		}
	})
	// The editor's text is bound to s.pv.alias, and the headless tester does
	// not register input values as labels, so the "not cleared" check is on
	// the bound field; the status line below is a real drawn label.
	tt := renderProviderTab(t, s)
	if !tt.HasText("B was here") {
		t.Error("B's own status line is missing from the editor")
	}
	if tt.HasText(trf("providerAliasSaved", "Friendly")) {
		t.Error("A's confirmation is shown in B's editor")
	}
	if label := groupLabel(t, f, a.Key); label != "Friendly" {
		t.Errorf("A's write did not land: label = %q", label)
	}
}

// TestProviderAliasFailureAfterSelectionChangeIsNotTheOtherGroupsError pins the
// failure half of the same rule: A's rejected write releases the shared busy
// state but must not surface as B's error.
func TestProviderAliasFailureAfterSelectionChangeIsNotTheOtherGroupsError(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	f, a, b := twoRenameFixture(t)
	boom := errors.New("alias store is down")

	entered := make(chan struct{})
	gate := make(chan struct{})
	l := newLoop()
	s := NewState(f.q, Hooks{
		Now: func() time.Time { return providerTestNow() },
		SetProviderAlias: func(context.Context, string, string) error {
			close(entered)
			<-gate
			return boom
		},
	}, l.post)
	gs := fixtureGroups(t, f)
	l.do(func() { s.ov.Providers = gs })

	l.do(func() {
		s.selectProvider(a.Key)
		s.pv.alias = "Friendly"
		s.setProviderAlias(a, "Friendly")
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the alias write never started")
	}
	l.do(func() {
		s.selectProvider(b.Key)
		s.pv.alias = "Bee"
		s.pv.msg = "B was here"
	})
	close(gate)
	waitFor(t, func() bool {
		var released bool
		l.do(func() { released = !s.pv.busy })
		return released
	})

	l.do(func() {
		if s.pv.err != "" {
			t.Errorf("A's failure is shown as B's error: pv.err = %q", s.pv.err)
		}
		if s.pv.alias != "Bee" || s.pv.msg != "B was here" {
			t.Errorf("A's failure changed B's editor: alias %q msg %q", s.pv.alias, s.pv.msg)
		}
	})
	tt := renderProviderTab(t, s)
	if tt.HasText(boom.Error()) {
		t.Error("A's failure is shown in B's editor")
	}
}
