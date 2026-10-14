package attrib

import (
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

func TestResolverStartupPreservesHistoricalPricingIdentity(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := t.Context()
	events := []model.UsageEvent{
		{DedupKey: "with-endpoint", SessionID: "s", Model: "gpt-5", Provider: "custom", BaseURL: "https://api.example.com/v1", Tokens: model.Tokens{Input: 10}},
		{DedupKey: "without-endpoint", SessionID: "s", Model: "gpt-5", Provider: "custom", Tokens: model.Tokens{Input: 20}},
	}
	resolutions := []store.Resolution{{Provider: "custom", Attrib: model.AttribLog, Cost: 1.25, Priced: true}, {Provider: "custom", Attrib: model.AttribLog, Cost: 2.5, Priced: true}}
	if err = st.Commit(ctx, model.Codex, "fixture", harness.Batch{Events: events}, resolutions); err != nil {
		t.Fatal(err)
	}
	if err = st.SetSetting(ctx, "pricing_fingerprint", "historical-price-rules"); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewWithSources(st)
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	if got, err := st.Setting(ctx, "pricing_fingerprint"); err != nil || got != "historical-price-rules" {
		t.Fatalf("startup invalidated historical prices: %q %v", got, err)
	}
	var n int
	var cost float64
	if err = st.DB().QueryRow("SELECT count(*),sum(cost) FROM events WHERE resolved_provider='custom' AND attrib=?", model.AttribLog).Scan(&n, &cost); err != nil || n != 2 || cost != 3.75 {
		t.Fatalf("historical attribution/cost changed: %d %g %v", n, cost, err)
	}
	// New events still use the current resolver; freezing history must not regress it.
	got, _ := resolver.Resolve(ctx, events[0])
	if got != "api.example.com" {
		t.Fatalf("new attribution regressed: %q", got)
	}
}
