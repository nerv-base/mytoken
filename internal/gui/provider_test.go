package gui

import (
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/provider"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/store"
)

// providerFixture is a real store and query over it, so these tests see the
// same identities, aliases and prices the app does.
type providerFixture struct {
	st *store.Store
	q  query.Service
}

func newProviderFixture(t *testing.T) *providerFixture {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &providerFixture{st: st, q: query.NewService(st)}
}

// commit records one usage event with its resolution, dated inside the
// default span.
func (f *providerFixture) commit(t *testing.T, h model.Harness, e model.UsageEvent, r store.Resolution) {
	t.Helper()
	if e.Timestamp.IsZero() {
		e.Timestamp = providerTestNow()
	}
	if err := f.st.Commit(t.Context(), h, "fixture", harness.Batch{Events: []model.UsageEvent{e}}, []store.Resolution{r}); err != nil {
		t.Fatal(err)
	}
}

func providerTestNow() time.Time { return time.Date(2026, 5, 20, 15, 0, 0, 0, time.Local) }

// fixtureGroups are the endpoint groups the query exposes, which is what the
// GUI must present.
func fixtureGroups(t *testing.T, f *providerFixture) []query.ProviderGroup {
	t.Helper()
	gs, err := f.q.ProviderGroups(t.Context(), query.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	return gs
}

// shortTail is the part of a display identity key that tells two endpoints
// apart: the hash after the versioned prefix.
func shortTail(key string) string { return key[strings.LastIndex(key, ":")+1:] }

// providerGUI is a synchronous state over q, as tests and snapshots build it.
func providerGUI(t *testing.T, q query.Service, hooks Hooks) *State {
	t.Helper()
	if hooks.Now == nil {
		hooks.Now = providerTestNow
	}
	s := NewState(q, hooks, nil)
	s.Start()
	return s
}

// renderProviderTab draws the ranking page on its Providers tab.
func renderProviderTab(t *testing.T, s *State) *ui.Tester {
	t.Helper()
	s.SetPage("ranking")
	s.rankTab = 1
	tt := ui.NewTester(s.MainView, 1400, 1000)
	tt.Frame()
	return tt
}

// TestProviderTabShowsEndpointIdentities pins the merge rules the GUI has to
// present: one endpoint spelled two ways is one group, another tenant path is
// another, and an endpoint with no stable identity stays its own group.
func TestProviderTabShowsEndpointIdentities(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	f := newProviderFixture(t)
	f.commit(t, model.Codex, model.UsageEvent{DedupKey: "a", SessionID: "s1", Model: "m", BaseURL: "https://API.example.com:443/tenant/A/", Tokens: model.Tokens{Input: 10}}, store.Resolution{Provider: "alpha", Cost: 2, Priced: true})
	f.commit(t, model.ClaudeCode, model.UsageEvent{DedupKey: "b", SessionID: "s2", Model: "m", BaseURL: "https://api.example.com/tenant/A", Tokens: model.Tokens{Input: 5}}, store.Resolution{Provider: "beta", Cost: 1, Priced: true})
	f.commit(t, model.Codex, model.UsageEvent{DedupKey: "c", SessionID: "s3", Model: "m", BaseURL: "https://api.example.com/tenant/B", Tokens: model.Tokens{Input: 3}}, store.Resolution{Provider: "alpha", Cost: 1, Priced: true})
	f.commit(t, model.Codex, model.UsageEvent{DedupKey: "d", SessionID: "s4", Model: "m", BaseURL: "http://127.0.0.1:8080/v1", Tokens: model.Tokens{Input: 7}}, store.Resolution{Provider: "local"})

	gs := fixtureGroups(t, f)
	if len(gs) != 3 {
		t.Fatalf("endpoint groups = %d, want 3 (merged spelling, other path, unresolved): %+v", len(gs), gs)
	}
	tt := renderProviderTab(t, providerGUI(t, f.q, Hooks{}))
	for _, g := range gs {
		if !tt.HasText(g.Label) {
			t.Errorf("group %q (%s) is not shown", g.Label, shortTail(g.Key))
		}
		if !tt.HasText(shortTail(g.Key)[:8]) {
			t.Errorf("group %q does not show its stable identity %s", g.Label, shortTail(g.Key))
		}
	}
}

// TestProviderTabShowsMemberProvenance pins that a merged group keeps naming
// the original harness/provider pairs behind it.
func TestProviderTabShowsMemberProvenance(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	f := newProviderFixture(t)
	const base = "https://api.example.com/v1"
	f.commit(t, model.Codex, model.UsageEvent{DedupKey: "a", SessionID: "s1", Model: "m", BaseURL: base, Tokens: model.Tokens{Input: 10}}, store.Resolution{Provider: "alpha", Cost: 2, Priced: true})
	f.commit(t, model.ClaudeCode, model.UsageEvent{DedupKey: "b", SessionID: "s2", Model: "m", BaseURL: base, Tokens: model.Tokens{Input: 5}}, store.Resolution{Provider: "beta", Cost: 1, Priced: true})

	gs := fixtureGroups(t, f)
	if len(gs) != 1 || len(gs[0].Members) != 2 {
		t.Fatalf("want one group with two members: %+v", gs)
	}
	tt := renderProviderTab(t, providerGUI(t, f.q, Hooks{}))
	for _, want := range []string{"Codex", "Claude Code", "alpha", "beta"} {
		if !tt.HasText(want) {
			t.Errorf("member provenance %q is not shown", want)
		}
	}
}

// TestProviderTabLabelsUnknownsHonestly pins that endpoints without a stable
// identity are labelled as unidentified rather than dressed up as identified
// ones, and that the identity of an unknown endpoint is the raw endpoint — its
// harness, instance, resolved provider and base URL — so the sessions behind
// one loopback URL are one unknown endpoint, not one row per session, while a
// different loopback URL stays its own unknown endpoint.
func TestProviderTabLabelsUnknownsHonestly(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	f := newProviderFixture(t)
	const loopback = "http://127.0.0.1:8080/v1"
	f.commit(t, model.Codex, model.UsageEvent{DedupKey: "a", SessionID: "s1", Model: "m", BaseURL: loopback, Tokens: model.Tokens{Input: 4}}, store.Resolution{Provider: "local"})
	f.commit(t, model.Codex, model.UsageEvent{DedupKey: "b", SessionID: "s2", Model: "m", BaseURL: loopback, Tokens: model.Tokens{Input: 6}}, store.Resolution{Provider: "local"})
	f.commit(t, model.Codex, model.UsageEvent{DedupKey: "c", SessionID: "s3", Model: "m", BaseURL: "http://127.0.0.1:8081/v1", Tokens: model.Tokens{Input: 2}}, store.Resolution{Provider: "local"})

	var unknown []query.ProviderGroup
	for _, g := range fixtureGroups(t, f) {
		if !g.Identified {
			unknown = append(unknown, g)
		}
	}
	if len(unknown) != 2 {
		t.Fatalf("two loopback endpoints must stay two unidentified groups: %+v", unknown)
	}
	if unknown[0].Key == unknown[1].Key {
		t.Fatal("two unresolved identities collided")
	}
	var merged *query.ProviderGroup
	for i := range unknown {
		if unknown[i].Sessions == 2 {
			merged = &unknown[i]
		}
	}
	if merged == nil || merged.Requests != 2 {
		t.Fatalf("the sessions behind one loopback URL did not merge into one endpoint: %+v", unknown)
	}
	tt := renderProviderTab(t, providerGUI(t, f.q, Hooks{}))
	if !tt.HasText("Unidentified") {
		t.Error("an unresolved endpoint is not labelled as unidentified")
	}
	for _, g := range unknown {
		if !tt.HasText(shortTail(g.Key)[:8]) {
			t.Errorf("unresolved group %q is not shown", g.Label)
		}
	}
}

// TestProviderTabKeepsDuplicateAliasesApart pins that rows stay distinct when
// two endpoints share one display name: selection is by the stable key.
func TestProviderTabKeepsDuplicateAliasesApart(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	f := newProviderFixture(t)
	const baseA, baseB = "https://a.example.com/v1", "https://b.example.com/v1"
	f.commit(t, model.Codex, model.UsageEvent{DedupKey: "a", SessionID: "s1", Model: "m", BaseURL: baseA, Tokens: model.Tokens{Input: 10}}, store.Resolution{Provider: "alpha", Cost: 1, Priced: true})
	f.commit(t, model.Codex, model.UsageEvent{DedupKey: "b", SessionID: "s2", Model: "m", BaseURL: baseB, Tokens: model.Tokens{Input: 5}}, store.Resolution{Provider: "beta", Cost: 1, Priced: true})
	for _, base := range []string{baseA, baseB} {
		id, ok := provider.EndpointID(base)
		if !ok {
			t.Fatalf("%s has no endpoint id", base)
		}
		if err := f.st.SetProviderAlias(t.Context(), id, "Shared"); err != nil {
			t.Fatal(err)
		}
	}
	gs := fixtureGroups(t, f)
	if len(gs) != 2 {
		t.Fatalf("groups = %d, want 2", len(gs))
	}
	for _, g := range gs {
		if g.Label != "Shared" {
			t.Fatalf("alias not applied: %q", g.Label)
		}
	}
	tt := renderProviderTab(t, providerGUI(t, f.q, Hooks{}))
	for _, g := range gs {
		if !tt.HasText(shortTail(g.Key)[:8]) {
			t.Errorf("row for %s is not distinguishable from its twin", g.Key)
		}
	}
}

// TestOverviewProviderCardUsesEndpointGroups pins that the overview's provider
// card speaks in endpoint groups too, not attribution names.
func TestOverviewProviderCardUsesEndpointGroups(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	f := newProviderFixture(t)
	const base = "https://api.example.com/v1"
	f.commit(t, model.Codex, model.UsageEvent{DedupKey: "a", SessionID: "s1", Model: "m", BaseURL: base, Tokens: model.Tokens{Input: 10}}, store.Resolution{Provider: "alpha", Cost: 1, Priced: true})
	id, ok := provider.EndpointID(base)
	if !ok {
		t.Fatal("no endpoint id")
	}
	if err := f.st.SetProviderAlias(t.Context(), id, "Friendly"); err != nil {
		t.Fatal(err)
	}
	s := providerGUI(t, f.q, Hooks{})
	tt := ui.NewTester(s.MainView, 1400, 1000)
	tt.Frame()
	if !tt.HasText("Friendly") {
		t.Error("the overview provider card does not show the endpoint group's display name")
	}
	if !tt.HasText(shortTail(id)[:8]) {
		t.Error("the overview provider card does not show the group's stable identity")
	}
}
