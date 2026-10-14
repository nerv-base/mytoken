package query

import (
	"encoding/json"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/provider"
	"github.com/zzstar101/mytoken/internal/store"
	"strings"
	"testing"
)

func TestProviderGroupAliasChangesOnlyLabel(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := t.Context()
	base := "https://api.example.com/v1"
	if err = st.Commit(ctx, model.Codex, "fixture", harness.Batch{Events: []model.UsageEvent{{DedupKey: "one", SessionID: "s", BaseURL: base}}}, []store.Resolution{{Provider: "original", Cost: 2, Priced: true}}); err != nil {
		t.Fatal(err)
	}
	q := NewService(st)
	before, err := q.ProviderGroups(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := provider.EndpointID(base)
	if err = st.SetProviderAlias(ctx, id, "Friendly"); err != nil {
		t.Fatal(err)
	}
	after, err := q.ProviderGroups(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || len(after) != 1 || after[0].Label != "Friendly" {
		t.Fatalf("alias not shown: %+v", after)
	}
	after[0].Label = before[0].Label
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatalf("alias changed non-label data: %s vs %s", a, b)
	}
	if err = st.SetProviderAlias(ctx, id, ""); err != nil {
		t.Fatal(err)
	}
	cleared, err := q.ProviderGroups(ctx, Filter{})
	if err != nil || len(cleared) != 1 || cleared[0].Label != base {
		t.Fatalf("alias not reset: %+v %v", cleared, err)
	}
}

// An unknown endpoint cannot establish equivalence, so it stays isolated by its
// (harness, instance, pricing name, base URL) tuple - but not by session: two
// sessions that logged the same unknown endpoint are one provider identity, not
// one row per session.
func TestProviderGroupsUnknownIsolationAndCostProvenance(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := t.Context()
	logCost := 7.0
	events := []model.UsageEvent{
		{DedupKey: "one", SessionID: "s1", Model: "alias", BaseURL: "https://api.example.com/v1", CostUSD: &logCost},
		{DedupKey: "two", SessionID: "s1", Model: "alias", BaseURL: "https://api.example.com/v1"},
		{DedupKey: "three", SessionID: "s1", Model: "alias", BaseURL: "https://api.example.com/v1"},
		{DedupKey: "four", SessionID: "s1", Model: "alias", BaseURL: "https://api.example.com/v1?key=private-secret"},
		{DedupKey: "five", SessionID: "s2", Model: "alias", BaseURL: "https://api.example.com/v1?key=private-secret"},
	}
	resolutions := []store.Resolution{{Provider: "original"}, {Provider: "original", Cost: 2, Priced: true}, {Provider: "original"}, {Provider: "original"}, {Provider: "original"}}
	if err = st.Commit(ctx, model.Codex, "fixture", harness.Batch{Events: events}, resolutions); err != nil {
		t.Fatal(err)
	}
	if err = st.SetSetting(ctx, "model-aliases", `[{"from":"alias","to":"canonical","provider":"original"}]`); err != nil {
		t.Fatal(err)
	}
	groups, err := NewService(st).ProviderGroups(ctx, Filter{Models: []string{"canonical"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("the recognized endpoint and the one unknown tuple must stay apart: %+v", groups)
	}
	var unknown *ProviderGroup
	for i := range groups {
		g := groups[i]
		if g.Identified {
			if g.Requests != 3 || g.CostUSD != 9 || g.CostLogUSD != 7 || g.CostEstimateUSD != 2 || g.Unpriced != 1 {
				t.Fatalf("cost provenance changed: %+v", g)
			}
			continue
		}
		unknown = &groups[i]
	}
	if unknown == nil || unknown.Requests != 2 || unknown.Sessions != 2 {
		t.Fatalf("two sessions of one unknown endpoint must merge into 2 requests over 2 sessions: %+v", unknown)
	}
	raw, err := json.Marshal(groups)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private-secret") {
		t.Fatal("raw unknown endpoint secret exposed")
	}
}

func TestProviderGroupsPreservePricingBoundaries(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := t.Context()
	for i, item := range []struct {
		h          model.Harness
		name, base string
	}{
		{model.Codex, "account-a", "https://API.example.com:443/v1/"},
		{model.ClaudeCode, "account-b", "https://api.example.com/v1"},
		{model.Codex, "account-a", "https://api.example.com/tenant-b"},
		{model.Codex, "local", "http://localhost:8080"},
		{model.ClaudeCode, "local", "http://localhost:8080"},
	} {
		e := model.UsageEvent{DedupKey: item.name + item.base, SessionID: "s", Model: "gpt-5", Provider: item.name, BaseURL: item.base, Tokens: model.Tokens{Input: int64(i + 1)}}
		if err = st.Commit(ctx, item.h, item.name, harness.Batch{Events: []model.UsageEvent{e}}, []store.Resolution{{Provider: item.name, Attrib: model.AttribLog, Cost: float64(i + 1), Priced: true}}); err != nil {
			t.Fatal(err)
		}
	}
	q := NewService(st)
	groups, err := q.ProviderGroups(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 4 {
		t.Fatalf("groups=%d want 4", len(groups))
	}
	id, _ := provider.EndpointID("https://api.example.com/v1")
	var requests, input int64
	var cost float64
	found := false
	for _, g := range groups {
		requests += g.Requests
		input += g.Tokens.Input
		cost += g.CostUSD
		if g.Key == id {
			found = true
			if g.Label != "https://api.example.com/v1" || g.Requests != 2 || g.CostUSD != 3 || g.Sessions != 2 || len(g.Members) != 2 {
				t.Fatalf("merged group: %+v", g)
			}
		}
	}
	if !found || requests != 5 || input != 15 || cost != 15 {
		t.Fatalf("totals changed: found=%v requests=%d input=%d cost=%g", found, requests, input, cost)
	}
	filtered, err := q.ProviderGroups(ctx, Filter{Providers: []string{"account-b"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].Requests != 1 || filtered[0].CostUSD != 2 {
		t.Fatalf("original name filter changed: %+v", filtered)
	}
	var names int
	if err = st.DB().QueryRow("SELECT count(DISTINCT resolved_provider) FROM events").Scan(&names); err != nil || names != 3 {
		t.Fatalf("pricing names rewritten: %d %v", names, err)
	}
}
