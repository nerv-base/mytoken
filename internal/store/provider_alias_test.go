package store

import (
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/provider"
	"testing"
)

func TestProviderAliasIsDisplayOnly(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	base := "https://api.example.com/v1"
	if err = s.Commit(ctx, model.Codex, "fixture", harness.Batch{Events: []model.UsageEvent{{SessionID: "s", DedupKey: "r", BaseURL: base}}}, []Resolution{{Provider: "price-account", Cost: 3, Priced: true}}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetSetting(ctx, "pricing_fingerprint", "original"); err != nil {
		t.Fatal(err)
	}
	id, _ := provider.EndpointID(base)
	changed, cancel := s.Subscribe()
	defer cancel()
	if err = s.SetProviderAlias(ctx, id, "  Friendly Relay  "); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("alias change did not notify observers")
	}
	if got, err := s.Setting(ctx, "provider.alias."+id); err != nil || got != "Friendly Relay" {
		t.Fatalf("alias=%q %v", got, err)
	}
	if got, _ := s.Setting(ctx, "pricing_fingerprint"); got != "original" {
		t.Fatal("alias invalidated pricing")
	}
	var name string
	var cost float64
	if err = s.DB().QueryRow("SELECT resolved_provider,cost FROM events").Scan(&name, &cost); err != nil || name != "price-account" || cost != 3 {
		t.Fatalf("history rewritten: %q %g %v", name, cost, err)
	}
	if err = s.SetProviderAlias(ctx, id, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Setting(ctx, "provider.alias."+id); got != "" {
		t.Fatal("alias not cleared")
	}
	for _, bad := range []struct{ id, label string }{{"pricing_fingerprint", "hijack"}, {id, "bad\nlabel"}, {"endpoint:v1:missing", "unknown"}} {
		if err = s.SetProviderAlias(ctx, bad.id, bad.label); err == nil {
			t.Fatalf("invalid alias accepted: %+v", bad)
		}
	}
}
