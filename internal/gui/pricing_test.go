package gui

import (
	"context"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/query"
)

// oneProvider is demoSettings with a single provider, so a rendered pricing
// card has exactly one multiplier row to drive.
type oneProvider struct{ *demoSettings }

func (oneProvider) Providers(context.Context) ([]string, error) { return []string{"kami-cn"}, nil }

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
			want:      map[string]string{"kami-cn": formatMultiplier(1.0 / 3.0), "nerv-base": "20"},
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

// TestMultiplierApplyWritesTheTypedText drives the real pricing card's
// multiplier field: it is a plain text field with an explicit Apply, so the
// number stored is exactly the number typed. A number control was rejected
// because its step rounds a typed value and its range clamps one, either of
// which would change what is billed (docs/ROADMAP.md).
func TestMultiplierApplyWritesTheTypedText(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	st := &demoSettings{}
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)
	s := NewState(NewDemoService(now), Hooks{Settings: oneProvider{st}, Now: func() time.Time { return now },
		Progress: func() (int, int) { return 10, 10 }}, nil)

	tt := nativeTester(t, 620, 520, true, func(c *ui.Context) {
		pal := applyTheme(c)
		ui.Column(c).Padding(14).Children(func() { s.pricingCards(c, pal) })
	})
	if _, ok := tt.Find("kami-cn multiplier"); !ok {
		t.Fatalf("the multiplier field has no accessible label naming its provider; texts %v", tt.Texts())
	}
	if err := tt.Click("kami-cn multiplier"); err != nil {
		t.Fatalf("click the multiplier field: %v", err)
	}
	if !tt.Focused("kami-cn multiplier") {
		t.Fatal("the multiplier field did not take the focus")
	}
	tt.Key(ui.Cmd, ui.KeyA)
	tt.Type("0.0001234")
	// Typing alone must not write: the field waits for Apply, so an
	// unfinished number can never change what is billed.
	if got := providerMultiplier(st.rules, "kami-cn"); got != 1 {
		t.Fatalf("typing wrote the rule before Apply: %v (%v)", got, st.rules)
	}
	if err := tt.Click("Apply kami-cn multiplier"); err != nil {
		t.Fatalf("the multiplier row has no Apply button: %v", err)
	}
	if got := providerMultiplier(st.rules, "kami-cn"); got != 0.0001234 {
		t.Fatalf("Apply stored %v, want the typed 0.0001234 (rules %v)", got, st.rules)
	}
	if len(s.pr.pending) != 0 {
		t.Errorf("the write was not confirmed by a reload: pending %v", s.pr.pending)
	}
}

// TestMultiplierFlushDrainsPastASameValueFirst is the stall this drain used to
// have: the first parked edit already equals the persisted rule, so it makes no
// write and its reload never comes. Advancing the wait generation for it left
// every edit queued behind it parked forever.
func TestMultiplierFlushDrainsPastASameValueFirst(t *testing.T) {
	st := &demoSettings{rules: []query.PriceRule{
		{Provider: "kami-cn", Multiplier: 0.3, Source: "user"},
		{Provider: "nerv-base", Multiplier: 2, Source: "user"},
	}}
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)
	s := NewState(NewDemoService(now), Hooks{Settings: st, Now: func() time.Time { return now },
		Progress: func() (int, int) { return 10, 10 }}, nil)
	s.loadPricing()

	// A write is in flight, and the user edits two providers: the first edit
	// happens to be the value already stored, the second is a real change.
	s.pr.busy = true
	s.queueMultiplier("kami-cn", 0.3)
	s.queueMultiplier("nerv-base", 3)

	// The write lands and its reload comes back.
	s.pr.busy = false
	s.loadPricing()
	s.flushMultipliers()
	if got := providerMultiplier(st.rules, "kami-cn"); got != 0.3 {
		t.Fatalf("the same-value edit changed the rule: %v (%v)", got, st.rules)
	}
	if got := providerMultiplier(st.rules, "nerv-base"); got != 3 {
		t.Fatalf("the edit behind a same-value first edit stalled: nerv-base = %v (%v)", got, st.rules)
	}
	if len(s.pr.parked) != 0 {
		t.Errorf("an edit stayed parked: %v", s.pr.parked)
	}
	// Another frame must not resurrect or rewrite anything.
	s.flushMultipliers()
	if got := providerMultiplier(st.rules, "nerv-base"); got != 3 {
		t.Errorf("a later frame rewrote nerv-base: %v (%v)", got, st.rules)
	}
}

// TestMultiplierCardKeepsPrecisionAndLargeValues renders the card with a
// multiplier of arbitrary precision and one above any input range: rendering
// and an unrelated edit must neither round nor clamp them, in the rules or in
// the field.
func TestMultiplierCardKeepsPrecisionAndLargeValues(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	st := &demoSettings{rules: []query.PriceRule{
		{Provider: "kami-cn", Multiplier: 0.0001234, Source: "user"},
		{Provider: "nerv-base", Multiplier: 250, Source: "user"},
	}}
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)
	s := NewState(NewDemoService(now), Hooks{Settings: st, Now: func() time.Time { return now },
		Progress: func() (int, int) { return 10, 10 }}, nil)
	s.loadPricing()

	if got := s.pr.mult["kami-cn"]; got != "0.0001234" {
		t.Errorf("the card renders kami-cn as %q, want the exact 0.0001234", got)
	}
	if got := s.pr.mult["nerv-base"]; got != "250" {
		t.Errorf("the card renders nerv-base as %q, want the unclamped 250", got)
	}
	if got := s.pr.multVal["kami-cn"]; got != 0.0001234 {
		t.Errorf("the card's value for kami-cn is %v, want 0.0001234", got)
	}
	if got := s.pr.multVal["nerv-base"]; got != 250 {
		t.Errorf("the card's value for nerv-base is %v, want 250", got)
	}

	tt := nativeTester(t, 720, 560, true, func(c *ui.Context) {
		pal := applyTheme(c)
		ui.Column(c).Padding(14).Children(func() { s.pricingCards(c, pal) })
	})
	// The field shows the exact text: copying the focused field's content is
	// the closest read of what a user sees.
	tt.Click("nerv-base multiplier")
	tt.Key(ui.Cmd, ui.KeyA)
	tt.Command("copy")
	tt.Frame()
	if got := tt.Clipboard(); got != "250" {
		t.Errorf("the field for a 250× multiplier holds %q, want 250", got)
	}
	tt.Click("kami-cn multiplier")
	tt.Key(ui.Cmd, ui.KeyA)
	tt.Command("copy")
	tt.Frame()
	if got := tt.Clipboard(); got != "0.0001234" {
		t.Errorf("the field for a 0.0001234× multiplier holds %q, want the exact 0.0001234", got)
	}
	// Rendering and focusing must not have written anything.
	tt.Frame()
	for _, r := range st.rules {
		if r.Provider == "kami-cn" && r.Multiplier != 0.0001234 {
			t.Fatalf("rendering rewrote kami-cn to %v (%v)", r.Multiplier, st.rules)
		}
		if r.Provider == "nerv-base" && r.Multiplier != 250 {
			t.Fatalf("rendering clamped nerv-base to %v (%v)", r.Multiplier, st.rules)
		}
	}

	// An unrelated edit writes its own provider and leaves the others alone.
	s.submitMultiplier("openai", "1.5")
	if got := providerMultiplier(st.rules, "openai"); got != 1.5 {
		t.Fatalf("the unrelated edit was not written: %v (%v)", got, st.rules)
	}
	if got := providerMultiplier(st.rules, "kami-cn"); got != 0.0001234 {
		t.Errorf("an unrelated edit rounded kami-cn to %v (%v)", got, st.rules)
	}
	if got := providerMultiplier(st.rules, "nerv-base"); got != 250 {
		t.Errorf("an unrelated edit clamped nerv-base to %v (%v)", got, st.rules)
	}
}

// TestMultiplierEditsWaitForTheWriteInFlight covers the save timing the number
// input needs: an edit made while a write is running is parked, and written
// only once the reload that follows that write has landed, so a step is
// applied rather than dropped and never from a rule list older than the write.
func TestMultiplierEditsWaitForTheWriteInFlight(t *testing.T) {
	st := &demoSettings{rules: []query.PriceRule{{Provider: "kami-cn", Multiplier: 0.3, Source: "user"}}}
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)
	s := NewState(NewDemoService(now), Hooks{Settings: st, Now: func() time.Time { return now },
		Progress: func() (int, int) { return 10, 10 }}, nil)
	s.loadPricing()
	if got := s.pr.multVal["kami-cn"]; got != 0.3 {
		t.Fatalf("the input starts at %v, want the rule's 0.3", got)
	}

	// While a write runs a step is parked: the store is untouched.
	s.pr.busy = true
	s.queueMultiplier("kami-cn", 0.4)
	if got := providerMultiplier(st.rules, "kami-cn"); got != 0.3 {
		t.Fatalf("a parked edit wrote through the write in flight: %v", got)
	}
	// Its reload has not landed yet, so the parked step waits.
	s.flushMultipliers()
	if got := providerMultiplier(st.rules, "kami-cn"); got != 0.3 {
		t.Fatalf("the parked step was written before the reload landed: %v", got)
	}
	if s.pr.parked["kami-cn"] != 0.4 {
		t.Fatalf("the parked step was dropped instead of kept: %v", s.pr.parked)
	}
	// A second step while the same write runs replaces the parked one.
	s.queueMultiplier("kami-cn", 0.5)

	// The write lands and its reload comes back: now the parked step applies.
	s.pr.busy = false
	s.loadPricing()
	s.flushMultipliers()
	if got := providerMultiplier(st.rules, "kami-cn"); got != 0.5 {
		t.Fatalf("the parked step did not reach the rules: %v (%v)", got, st.rules)
	}
	if len(s.pr.parked) != 0 {
		t.Errorf("the parked edit stayed parked: %v", s.pr.parked)
	}
	if len(s.pr.pending) != 0 {
		t.Errorf("the written value was never confirmed by a reload: %v", s.pr.pending)
	}
	if got := s.pr.multVal["kami-cn"]; got != 0.5 {
		t.Errorf("the input shows %v, want the written 0.5", got)
	}
}

// TestCatalogComboboxMapsTheChosenModel drives the unpriced row's catalog
// search: the native combo runs our query as the user types, and the chosen
// hit is what gets mapped.
func TestCatalogComboboxMapsTheChosenModel(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	st := &demoSettings{unpriced: []query.UnpricedModel{{
		Model: "claude-opus-5-5-high", Provider: "kami-cn", Requests: 42, Tokens: 2_720_000,
		Suggestions: []string{"deepseek-chat"},
	}}}
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)
	s := NewState(NewDemoService(now), Hooks{Settings: st, Now: func() time.Time { return now },
		Progress: func() (int, int) { return 10, 10 }}, nil)
	s.loadPricing()
	s.pr.open = "kami-cn/claude-opus-5-5-high"

	tt := nativeTester(t, 640, 560, true, func(c *ui.Context) {
		pal := applyTheme(c)
		ui.Column(c).Padding(14).Children(func() { s.pricingCards(c, pal) })
	})
	if err := tt.Click("Search the price list"); err != nil {
		t.Fatalf("click the catalog box: %v", err)
	}
	if !tt.Focused("Search the price list") {
		t.Fatal("the catalog box did not take the focus")
	}
	tt.Type("opus")
	if !tt.HasText("claude-opus-4-5") || !tt.HasText("claude-opus-4-1") {
		t.Fatalf("typing opus listed %v, want the catalog hits", tt.Texts())
	}
	if tt.HasText("claude-sonnet-4-5") {
		t.Error("a model the query does not match was listed")
	}
	if err := tt.Click("claude-opus-4-5"); err != nil {
		t.Fatalf("click the hit: %v", err)
	}
	want := query.ModelAlias{From: "claude-opus-5-5-high", To: "claude-opus-4-5", Provider: "kami-cn"}
	mapped := false
	for _, a := range st.aliases {
		if a == want {
			mapped = true
		}
	}
	if !mapped {
		t.Fatalf("the chosen hit was not mapped: aliases %v", st.aliases)
	}
	if s.pr.open != "" {
		t.Errorf("the catalog box stayed open after the choice: %q", s.pr.open)
	}
}
