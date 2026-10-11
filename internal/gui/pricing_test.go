package gui

import (
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/query"
)

// TestProviderMultiplier pins which rule the pricing card shows as a provider's
// multiplier: the user's own undated provider-wide rule, else the cc-switch
// import, else 1. Relay-synced ratios, dated history and per-model rules are
// never the displayed value.
func TestProviderMultiplier(t *testing.T) {
	dated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	rate := 1.5

	tests := []struct {
		name     string
		rules    []query.PriceRule
		provider string
		want     float64
	}{
		{"no rules at all", nil, "kami-cn", 1},
		{"user undated provider-wide", []query.PriceRule{{Provider: "kami-cn", Multiplier: 0.3, Source: "user"}}, "kami-cn", 0.3},
		{"empty source counts as the user's own", []query.PriceRule{{Provider: "kami-cn", Multiplier: 0.42}}, "kami-cn", 0.42},
		{"cc-switch import when the user set nothing", []query.PriceRule{{Provider: "kami-cn", Multiplier: 0.5, Source: "cc-switch"}}, "kami-cn", 0.5},
		{"user beats cc-switch", []query.PriceRule{
			{Provider: "kami-cn", Multiplier: 0.5, Source: "cc-switch"},
			{Provider: "kami-cn", Multiplier: 0.3, Source: "user"},
		}, "kami-cn", 0.3},
		{"user beats a relay ratio", []query.PriceRule{
			{Provider: "kami-cn", Multiplier: 2, Source: "relay:https://api.example.com"},
			{Provider: "kami-cn", Multiplier: 0.3, Source: "user"},
		}, "kami-cn", 0.3},
		{"cc-switch beats a relay ratio", []query.PriceRule{
			{Provider: "kami-cn", Multiplier: 2, Source: "relay:https://api.example.com"},
			{Provider: "kami-cn", Multiplier: 0.25, Source: "cc-switch"},
		}, "kami-cn", 0.25},
		{"a relay ratio alone is not the user's multiplier", []query.PriceRule{
			{Provider: "kami-cn", Multiplier: 2.2, Source: "relay:https://api.example.com"},
		}, "kami-cn", 1},
		{"dated user rule is history, not the current setting", []query.PriceRule{
			{Provider: "kami-cn", Multiplier: 0.3, Source: "user", From: dated},
		}, "kami-cn", 1},
		{"dated cc-switch rule is history too", []query.PriceRule{
			{Provider: "kami-cn", Multiplier: 0.3, Source: "cc-switch", From: dated},
		}, "kami-cn", 1},
		{"an undated cc-switch rule still shows under dated history", []query.PriceRule{
			{Provider: "kami-cn", Multiplier: 0.3, Source: "user", From: dated},
			{Provider: "kami-cn", Multiplier: 0.5, Source: "cc-switch"},
		}, "kami-cn", 0.5},
		{"per-model rule is not provider-wide", []query.PriceRule{
			{Provider: "kami-cn", Model: "step-5-preview", Multiplier: 0.3, Source: "user"},
		}, "kami-cn", 1},
		{"unset multiplier is no rule", []query.PriceRule{
			{Provider: "kami-cn", Multiplier: 0, Source: "user"},
		}, "kami-cn", 1},
		{"negative multiplier is no rule", []query.PriceRule{
			{Provider: "kami-cn", Multiplier: -0.3, Source: "user"},
		}, "kami-cn", 1},
		{"another provider's rule is none of our business", []query.PriceRule{
			{Provider: "nerv-base", Multiplier: 0.3, Source: "user"},
		}, "kami-cn", 1},
		{"the newest user rule wins", []query.PriceRule{
			{Provider: "kami-cn", Multiplier: 0.3, Source: "user"},
			{Provider: "kami-cn", Multiplier: 0.7, Source: "user"},
		}, "kami-cn", 0.7},
		{"the newest cc-switch rule wins", []query.PriceRule{
			{Provider: "kami-cn", Multiplier: 0.5, Source: "cc-switch"},
			{Provider: "kami-cn", Multiplier: 0.4, Source: "cc-switch"},
		}, "kami-cn", 0.4},
		{"a provider-wide user rule with custom rates still shows", []query.PriceRule{
			{Provider: "kami-cn", Multiplier: 0.2, Input: &rate, Output: &rate, Source: "user"},
		}, "kami-cn", 0.2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := providerMultiplier(tc.rules, tc.provider); got != tc.want {
				t.Fatalf("providerMultiplier(%q) = %v, want %v", tc.provider, got, tc.want)
			}
		})
	}
}

// TestProviderMultipliers covers the map the pricing card renders: one entry per
// provider that actually overrides 1×, formatted like setMultiplier's message.
func TestProviderMultipliers(t *testing.T) {
	tests := []struct {
		name      string
		rules     []query.PriceRule
		providers []string
		want      map[string]string
	}{
		{"no providers", nil, nil, map[string]string{}},
		{
			name: "only real overrides get an entry",
			rules: []query.PriceRule{
				{Provider: "kami-cn", Multiplier: 1.0 / 3.0, Source: "user"},
				{Provider: "nerv-base", Multiplier: 20, Source: "cc-switch"},
				{Provider: "relay-only", Multiplier: 2.2, Source: "relay:https://api.example.com"},
				{Provider: "explicit-one", Multiplier: 1, Source: "user"},
			},
			providers: []string{"kami-cn", "nerv-base", "relay-only", "explicit-one", "never-configured"},
			want:      map[string]string{"kami-cn": "0.333", "nerv-base": "20"},
		},
		{
			name:      "a provider with no rules keeps the 1 placeholder",
			rules:     []query.PriceRule{{Provider: "kami-cn", Multiplier: 0.3, Source: "user"}},
			providers: []string{"kami-cn", "nerv-base"},
			want:      map[string]string{"kami-cn": "0.3"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := providerMultipliers(tc.rules, tc.providers)
			if got == nil {
				t.Fatal("providerMultipliers returned a nil map; the card indexes it directly")
			}
			if len(got) != len(tc.want) {
				t.Fatalf("providerMultipliers = %v, want %v", got, tc.want)
			}
			for prov, want := range tc.want {
				if got[prov] != want {
					t.Fatalf("providerMultipliers[%q] = %q, want %q (full map %v)", prov, got[prov], want, got)
				}
			}
		})
	}
}

// TestLoadPricingMultiplierCard runs the real load path (a State in sync mode,
// so the load applies inline) against rules shaped like the ones a synced relay
// leaves behind: only the user's own and the cc-switch import may reach the
// multiplier inputs.
func TestLoadPricingMultiplierCard(t *testing.T) {
	st := &demoSettings{rules: []query.PriceRule{
		{Provider: "kami-cn", Multiplier: 2.2, Source: "relay:https://api.example.com"},
		{Provider: "nerv-base", Multiplier: 0.5, Source: "cc-switch"},
		{Provider: "openai", Multiplier: 0.3, Source: "user"},
	}}
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)
	s := NewState(NewDemoService(now), Hooks{Settings: st, Now: func() time.Time { return now },
		Progress: func() (int, int) { return 10, 10 }}, nil)

	s.loadPricing()
	if !s.pr.loaded {
		t.Fatal("loadPricing did not load the pricing card")
	}
	if got := s.pr.mult["kami-cn"]; got != "" {
		t.Errorf("relay ratio reached the multiplier input: kami-cn = %q, want no entry (the 1 placeholder)", got)
	}
	if got := s.pr.mult["nerv-base"]; got != "0.5" {
		t.Errorf("cc-switch multiplier = %q, want 0.5", got)
	}
	if got := s.pr.mult["openai"]; got != "0.3" {
		t.Errorf("user multiplier = %q, want 0.3", got)
	}
	if _, ok := s.pr.mult["anthropic"]; ok {
		t.Errorf("a provider with no rule got an entry: %v", s.pr.mult)
	}
}
