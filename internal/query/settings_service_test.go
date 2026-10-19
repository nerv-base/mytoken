package query

import (
	"context"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/pricing"
	"github.com/zzstar101/mytoken/internal/store"
	"path/filepath"
	"testing"
)

// TestPriceRuleEffectiveFrom covers dated rules end to end: storage keeps them,
// and Evaluate picks the rule that was effective when the event happened.

// TestPriceRulesReadOldFormat checks that rules written before From existed
// load losslessly (zero From = always effective) and that provider-wide rates
// still act as the fallback for models without their own rule.

func TestSettingsRepriceAndAliases(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := pricing.New("")
	s := NewSettings(st, p)
	zero := 0.0
	events := []model.UsageEvent{{Harness: model.Codex, SessionID: "s", DedupKey: "a", Model: "unlisted", Tokens: model.Tokens{Input: 1000000}}, {Harness: model.Codex, SessionID: "s", DedupKey: "b", Model: "unlisted", CostUSD: &zero}}
	if err = st.Commit(ctx, model.Codex, "log", harness.Batch{Events: events}, nil); err != nil {
		t.Fatal(err)
	}
	ch, stop := st.Subscribe()
	defer stop()
	rate := 2.0
	if err = s.SetPriceRules(ctx, []PriceRule{{Model: "unlisted", Input: &rate}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	default:
		t.Fatal("no notification")
	}
	total, err := NewService(st).Totals(ctx, Filter{})
	if err != nil || total.Unpriced != 0 || total.CostUSD != 2 {
		t.Fatal(total, err)
	}
	if err = s.SetPriceRules(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.SetModelAliases(ctx, []ModelAlias{{From: "unlisted", To: "gpt-4o"}}); err != nil {
		t.Fatal(err)
	}
	_, _, items, err := NewService(st).Session(ctx, model.Codex, "s")
	if err != nil || items[0].Model != "gpt-4o" || !items[0].Priced {
		t.Fatal(items, err)
	}
	if err = s.SetModelAliases(ctx, nil); err != nil {
		t.Fatal(err)
	}
	total, err = NewService(st).Totals(ctx, Filter{})
	if err != nil || total.Unpriced != 1 || total.CostUSD != 0 {
		t.Fatal(total, err)
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err = s.SetPriceRules(cancelCtx, []PriceRule{{Model: "unlisted", Input: &rate}}); err == nil {
		t.Fatal("cancelled mutation succeeded")
	}
	rules, _ := s.PriceRules(ctx)
	if len(rules) != 0 {
		t.Fatal(rules)
	}
}
