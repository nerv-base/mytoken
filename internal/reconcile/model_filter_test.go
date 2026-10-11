package reconcile

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/pricing"
	"github.com/zzstar101/mytoken/internal/relay"
	"github.com/zzstar101/mytoken/internal/source"
	"github.com/zzstar101/mytoken/internal/store"
)

// TestUsedModels: the filter a sync builds from local usage and stored bills,
// scoped the way a price rule is looked up.
func TestUsedModels(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	events := []model.UsageEvent{
		{DedupKey: "1", SessionID: "s1", Timestamp: now.Add(-time.Hour), Model: "claude-opus-4-6", Tokens: model.Tokens{Input: 1}},
		{DedupKey: "2", SessionID: "s2", Timestamp: now.Add(-time.Hour), Model: "gpt-5-codex", Tokens: model.Tokens{Input: 1}},
		// Outside the window: it must not keep a model priced.
		{DedupKey: "3", SessionID: "s3", Timestamp: now.AddDate(0, 0, -200), Model: "ancient", Tokens: model.Tokens{Input: 1}},
	}
	res := make([]store.Resolution, len(events))
	for i := range events {
		res[i] = store.Resolution{Provider: "kami", Cost: 0.1, Priced: true}
	}
	if err = st.Commit(ctx, model.Pi, "log", harness.Batch{Events: events}, res); err != nil {
		t.Fatal(err)
	}
	// A model only the site's own bills charged is priced too, whatever the
	// local harnesses did.
	if err = st.PutRelayBills(ctx, []relay.Bill{
		{Origin: "https://relay.example", KeyID: "k1", ID: 1, At: now.Add(-time.Hour), Type: "consume", Model: "billed-only", ChargedUSD: 1},
	}); err != nil {
		t.Fatal(err)
	}
	f, err := usedModels(ctx, st, "https://relay.example", "k1", now.AddDate(0, 0, -modelWindowDays))
	if err != nil {
		t.Fatal(err)
	}
	if !f.Allows("pi/kami", "claude-opus-4-6") || !f.Allows("pi/kami", "gpt-5-codex") {
		t.Fatalf("used models not allowed: %+v", f)
	}
	if !f.Allows("pi/kami", "billed-only") {
		t.Fatal("a billed model must be priced")
	}
	if f.Allows("pi/kami", "ancient") {
		t.Fatal("a model last used outside the window must not be priced")
	}
	if f.Allows("pi/kami", "never-used") {
		t.Fatal("an unused model must not be priced")
	}
	// The pi harness sent it, so only the pi scope matches.
	if f.Allows("claude-code/kami", "claude-opus-4-6") {
		t.Fatal("another harness must not match a scoped rule")
	}
	if !f.Allows("kami", "claude-opus-4-6") {
		t.Fatal("a bare provider name must match any harness")
	}
}

// TestUsedModelsAliases: an alias is resolved before a rule is looked up, so a
// snapshot model is recorded under both spellings.
func TestUsedModelsAliases(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	events := []model.UsageEvent{
		{DedupKey: "1", SessionID: "s1", Timestamp: now.Add(-time.Hour), Model: "opus-latest", Tokens: model.Tokens{Input: 1}},
	}
	res := []store.Resolution{{Provider: "kami", Cost: 0.1, Priced: true}}
	if err = st.Commit(ctx, model.Pi, "log", harness.Batch{Events: events}, res); err != nil {
		t.Fatal(err)
	}
	if err = st.SetSetting(ctx, "model-aliases", `[{"provider":"kami","from":"opus-latest","to":"claude-opus-4-6"}]`); err != nil {
		t.Fatal(err)
	}
	f, err := usedModels(ctx, st, "https://relay.example", "k1", now.AddDate(0, 0, -modelWindowDays))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"opus-latest", "claude-opus-4-6"} {
		if !f.Allows("pi/kami", name) {
			t.Fatalf("%q must be priced: the alias maps to it", name)
		}
	}
}

// TestUsedModelsWithoutEvidence: no usage and no bills means the usage is not
// visible (a fresh install, a rollup that has not caught up), so every model is
// allowed rather than none — the worst case is the behaviour before filtering.
func TestUsedModelsWithoutEvidence(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	f, err := usedModels(ctx, st, "https://relay.example", "k1", time.Now().AddDate(0, 0, -modelWindowDays))
	if err != nil {
		t.Fatal(err)
	}
	if f.Empty() || !f.Allows("pi/kami", "claude-opus-4-6") {
		t.Fatalf("filter=%+v", f)
	}
}

// TestKeepRelayRules: what a sync feeds back when the price list did not load,
// so the merge does not read the missing models as models the key dropped.
func TestKeepRelayRules(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	in, out := 2.0, 10.0
	rules := `[
		{"provider":"pi/kami","model":"claude-opus-4-6","multiplier":1,"input":2,"output":10,"source":"relay:https://relay.example#k1","from":"2026-03-01T00:00:00Z"},
		{"provider":"pi/kami","model":"gpt-5-codex","multiplier":1,"input":3,"output":27,"source":"relay:https://relay.example#k1"},
		{"provider":"pi/kami","model":"other-key","source":"relay:https://relay.example#k2"},
		{"provider":"pi/kami","model":"by-hand","input":9,"source":"user"},
		{"provider":"pi/kami","model":"legacy","source":"relay:https://relay.example"}
	]`
	if err = st.SetSetting(ctx, priceRulesSetting, rules); err != nil {
		t.Fatal(err)
	}
	kept := keepRelayRules(ctx, st, "relay:https://relay.example#k1")
	if len(kept) != 2 {
		t.Fatalf("kept=%+v", kept)
	}
	if kept[0].Model != "claude-opus-4-6" || !kept[0].From.Equal(at) || kept[0].Input == nil || *kept[0].Input != in || *kept[0].Output != out {
		t.Fatalf("first=%+v", kept[0])
	}
	if kept[1].Model != "gpt-5-codex" || !kept[1].From.IsZero() {
		t.Fatalf("second=%+v", kept[1])
	}
	// Nothing stored for the source: nothing to keep, and no error.
	if got := keepRelayRules(ctx, st, "relay:https://other.example#k9"); len(got) != 0 {
		t.Fatalf("kept=%+v", got)
	}
	// A malformed row must not break a sync that only needs the rules it can
	// read; the merge then simply drops what it cannot see.
	if err = st.SetSetting(ctx, priceRulesSetting, "not json"); err != nil {
		t.Fatal(err)
	}
	if got := keepRelayRules(ctx, st, "relay:https://relay.example#k1"); len(got) != 0 {
		t.Fatalf("kept=%+v", got)
	}
	if _, err = st.Setting(ctx, priceRulesSetting); err != nil {
		t.Fatal(err)
	}
}

// TestSyncPricesOnlyUsedModels: end to end, a gateway that lists two models
// prices the one local usage served, and the rules reach the appender.
func TestSyncPricesOnlyUsedModels(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	events := []model.UsageEvent{
		{DedupKey: "1", SessionID: "s1", Timestamp: now.Add(-time.Hour), Model: "claude-sonnet-4-5", Tokens: model.Tokens{Input: 1}},
	}
	res := []store.Resolution{{Provider: "proxy", Cost: 0.1, Priced: true}}
	if err = st.Commit(ctx, model.Pi, "log", harness.Batch{Events: events}, res); err != nil {
		t.Fatal(err)
	}
	cred := source.Credential{Origin: "https://relay.example", KeyID: "k1", Provider: "proxy", Secret: source.NewSecret("sk-x")}
	site := relay.Site{Origin: cred.Origin, KeyID: cred.KeyID, Kind: relay.KindNewAPI, Layers: relay.LayerRatio, Providers: []string{"proxy"}}
	if err = st.PutRelaySite(ctx, site); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{snap: relay.Snapshot{At: now, Ratios: map[string]relay.Ratios{
		"claude-sonnet-4-5": {Model: 2, Completion: 5, Cache: 0.1},
		"never-used-model":  {Model: 9, Completion: 45},
	}}}
	rules := &recordingRules{st: st}
	syncer := NewSyncer(st, func(context.Context) ([]source.Credential, error) { return []source.Credential{cred}, nil },
		func(context.Context, relay.Site, source.Credential) (relay.Client, error) { return client, nil }, rules)
	if err = syncer.Sync(ctx, site.Origin, site.KeyID); err != nil {
		t.Fatal(err)
	}
	if client.ratioCalls != 1 {
		t.Fatalf("snapshot calls=%d", client.ratioCalls)
	}
	var got []string
	for _, r := range rules.rules {
		got = append(got, r.Model)
	}
	if len(got) != 1 || got[0] != "claude-sonnet-4-5" {
		t.Fatalf("rules=%v, want only the used model", got)
	}
	// And they are stored the way the settings service stores them, so the next
	// sync merges against them (and keepRelayRules can read them back).
	kept := keepRelayRules(ctx, st, relay.RuleSource(site.Origin, site.KeyID))
	if len(kept) != 1 || kept[0].Model != "claude-sonnet-4-5" || kept[0].Provider != "proxy" {
		t.Fatalf("kept=%+v", kept)
	}
}

// recordingRules keeps the rules a sync produced and stores them the way the
// settings service does, so a later sync merges against what it wrote.
type recordingRules struct {
	st    *store.Store
	rules []pricing.Rule
}

func (r *recordingRules) AppendRelayRules(ctx context.Context, _ string, rules []pricing.Rule) error {
	r.rules = append(r.rules, rules...)
	rows := make([]storedRule, 0, len(r.rules))
	for _, rule := range r.rules {
		rows = append(rows, storedRule{Provider: rule.Provider, Model: rule.Model, Multiplier: rule.Multiplier, Input: rule.Input, Output: rule.Output, CacheRead: rule.CacheRead, CacheWrite: rule.CacheWrite, Source: rule.Source, From: rule.From})
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	return r.st.SetSetting(ctx, priceRulesSetting, string(raw))
}
