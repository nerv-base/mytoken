package relay

import (
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/pricing"
)

// TestModelFilterScopes: evidence is scoped the way a rule is looked up, so a
// scoped provider entry only matches the harness that used the model, while a
// bare entry matches that provider everywhere.
func TestModelFilterScopes(t *testing.T) {
	f := NewModelFilter()
	f.Add(string(model.Pi), "kami", "Claude-Opus-4-6:latest")
	cases := []struct {
		provider, model string
		want            bool
	}{
		{"pi/kami", "claude-opus-4-6", true}, // the scope that used it
		{"pi/kami", "claude-opus-4-6-20260101", true},
		{"pi/kami", "claude-sonnet-4-5", false}, // never used
		{"claude-code/kami", "claude-opus-4-6", false},
		{"pi/other", "claude-opus-4-6", false},
		{"kami", "claude-opus-4-6", true}, // bare name: any harness
		{"kami", "claude-sonnet-4-5", false},
	}
	for _, c := range cases {
		if got := f.Allows(c.provider, c.model); got != c.want {
			t.Errorf("Allows(%q,%q) = %v, want %v", c.provider, c.model, got, c.want)
		}
	}
	if f.Empty() {
		t.Fatal("a filter with evidence is not empty")
	}
}

// TestModelFilterBillsOverrideScope: a model the key's own bills charged is
// relevant no matter which provider a rule would name, because the site said
// so itself.
func TestModelFilterBillsOverrideScope(t *testing.T) {
	f := NewModelFilter()
	f.Add(string(model.Pi), "kami", "claude-opus-4-6")
	f.AddModel("gpt-5-codex")
	if !f.Allows("pi/kami", "GPT-5-Codex") {
		t.Fatal("a billed model must be allowed")
	}
	if !f.Allows("claude-code/kami", "gpt-5-codex") {
		t.Fatal("a billed model must be allowed under any harness")
	}
	if f.Allows("pi/kami", "gpt-6") {
		t.Fatal("an unused model must not be allowed")
	}
}

// TestModelFilterKinds pins the three "no evidence" answers: an empty filter
// allows nothing, a nil filter allows everything, and AllowAll is explicit.
func TestModelFilterKinds(t *testing.T) {
	empty := NewModelFilter()
	if !empty.Empty() || empty.Allows("pi/kami", "claude-opus-4-6") {
		t.Fatal("an empty filter must allow nothing")
	}
	zero := ModelFilter{}
	if zero.Allows("pi/kami", "claude-opus-4-6") || !zero.Empty() {
		t.Fatal("a zero filter must allow nothing")
	}
	var nilFilter *ModelFilter
	if !nilFilter.Allows("pi/kami", "anything") || nilFilter.Empty() {
		t.Fatal("a nil filter means no filtering")
	}
	all := NewModelFilter()
	all.AllowAll()
	if all.Empty() || !all.Allows("pi/kami", "anything") {
		t.Fatal("AllowAll must allow everything")
	}
	// Blank names are not evidence.
	blank := NewModelFilter()
	blank.Add(string(model.Pi), "kami", "   ")
	blank.AddModel("")
	if !blank.Empty() || blank.Allows("pi/kami", "x") {
		t.Fatal("blank model names must be ignored")
	}
}

// TestRulesFromSnapshotFiltered: only the models each provider really served
// become rules, and the provider-wide multiplier survives the filter.
func TestRulesFromSnapshotFiltered(t *testing.T) {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	site := Site{Origin: "https://relay.example", KeyID: "abc123", Providers: []string{"pi/kami", "pi/backup"}}
	snap := Snapshot{At: at, Ratios: map[string]Ratios{
		"claude-sonnet-4-5": {Model: 2, Completion: 5},
		"gpt-5-codex":       {Model: 3, Completion: 9},
	}}
	used := NewModelFilter()
	used.Add(string(model.Pi), "kami", "claude-sonnet-4-5")
	used.Add(string(model.Pi), "backup", "gpt-5-codex")
	rules := RulesFromSnapshot(site, snap, "", used)
	if len(rules) != 2 {
		t.Fatalf("rules=%+v", rules)
	}
	byProvider := map[string]pricing.Rule{}
	for _, r := range rules {
		byProvider[r.Provider] = r
	}
	if r, ok := byProvider["pi/kami"]; !ok || r.Model != "claude-sonnet-4-5" {
		t.Fatalf("kami=%+v", byProvider["pi/kami"])
	}
	if r, ok := byProvider["pi/backup"]; !ok || r.Model != "gpt-5-codex" {
		t.Fatalf("backup=%+v", byProvider["pi/backup"])
	}
	// Nothing used at all: no rules, and the empty-model ratio is skipped as
	// before (it carries no model name to key a rule on).
	if got := RulesFromSnapshot(site, snap, "", NewModelFilter()); len(got) != 0 {
		t.Fatalf("rules=%+v, want none", got)
	}
	// The provider-wide multiplier is not per-model and is always emitted.
	m := 0.4
	got := RulesFromSnapshot(site, Snapshot{At: at, Multiplier: &m}, "", NewModelFilter())
	if len(got) != 2 || got[0].Model != "" || got[0].Multiplier != 0.4 {
		t.Fatalf("multiplier rules=%+v", got)
	}
}

// TestMergeRelayRulesPrunesUnusedModels: the models a fresh pass names say
// which models a key still prices, so a stored rule for a model the site no
// longer serves — or that the model filter dropped as unused — is dropped.
func TestMergeRelayRulesPrunesUnusedModels(t *testing.T) {
	site := Site{Origin: "https://relay.example", KeyID: "abc123", Providers: []string{"pi/kami"}}
	source := RuleSource(site.Origin, site.KeyID)
	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	// A snapshot of the two models the key still serves, priced as they were.
	snap := Snapshot{At: t0, Ratios: map[string]Ratios{
		"claude-sonnet-4-5": {Model: 2, Completion: 5, Cache: 0.1},
		"gpt-5-codex":       {Model: 3, Completion: 9, Cache: 0.1},
	}}
	all := NewModelFilter()
	all.AllowAll()
	// What an earlier sync stored: the same two models, plus one the site has
	// since dropped (or that the model filter now counts as unused).
	stale := pricing.Rule{Provider: "pi/kami", Model: "unused-model", Multiplier: 1, Source: source}
	old := append(RulesFromSnapshot(site, snap, "", all), stale)
	if len(old) != 3 {
		t.Fatalf("old=%d %+v", len(old), old)
	}
	fresh := RulesFromSnapshot(site, snap, "", modelFilterFor(site.Providers, "claude-sonnet-4-5", "gpt-5-codex"))
	merged := MergeRelayRules(old, fresh)
	if len(merged) != 2 {
		t.Fatalf("merged=%d %+v", len(merged), merged)
	}
	for _, r := range merged {
		if r.Model == "unused-model" {
			t.Fatalf("an unused model kept its rule: %+v", r)
		}
	}
	// Same prices, so the kept rules are the stored ones, not new copies.
	if merged[0].Model != "claude-sonnet-4-5" || !merged[0].From.Equal(t0) {
		t.Fatalf("kept rule rewritten: %+v", merged[0])
	}
	// A price list that failed to load yields no snapshot rules, so the sync
	// feeds back what it already stored for that key: the merge then sees every
	// stored rule as fresh and unchanged, and drops nothing — not even a model
	// the site has since retired.
	merged = MergeRelayRules(old, old)
	if len(merged) != 3 {
		t.Fatalf("keep-set merge=%d %+v", len(merged), merged)
	}
	// A site that reports only a multiplier retracts every per-model rule it
	// used to publish: its prices are the catalog times that multiplier now.
	m := 0.5
	wide := RulesFromSnapshot(site, Snapshot{At: t0, Multiplier: &m}, "", nil)
	merged = MergeRelayRules(old, wide)
	if len(merged) != 1 || merged[0].Model != "" || merged[0].Multiplier != 0.5 {
		t.Fatalf("multiplier-only merge=%+v", merged)
	}
}

func modelFilterFor(providers []string, models ...string) *ModelFilter {
	f := NewModelFilter()
	for _, p := range providers {
		h, name := SplitProvider(p)
		for _, m := range models {
			f.Add(string(h), name, m)
		}
	}
	return f
}
