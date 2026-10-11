package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/relay"
)

// TestRelayModelScopes lists which (harness, provider, model) scopes were used
// since a moment, so a gateway's price list can be trimmed to those models.
func TestRelayModelScopes(t *testing.T) {
	ctx := context.Background()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	recent := now.Add(-90 * 24 * time.Hour)
	events := []model.UsageEvent{
		{DedupKey: "a", SessionID: "s1", Timestamp: now.Add(-time.Hour), Model: "claude-sonnet-4-5", Tokens: model.Tokens{Input: 1}},
		{DedupKey: "b", SessionID: "s1", Timestamp: now.Add(-2 * time.Hour), Model: "claude-sonnet-4-5", Tokens: model.Tokens{Input: 1}},
		{DedupKey: "c", SessionID: "s2", Timestamp: now.Add(-time.Hour), Model: "gpt-5-codex", Tokens: model.Tokens{Input: 1}},
		// Older than the window: it must not keep a model alive.
		{DedupKey: "d", SessionID: "s3", Timestamp: recent.Add(-24 * time.Hour), Model: "ancient-model", Tokens: model.Tokens{Input: 1}},
	}
	res := make([]Resolution, len(events))
	for i := range events {
		res[i] = Resolution{Provider: "kamiapi", Cost: 0.1, Priced: true}
	}
	if err = st.Commit(ctx, model.ClaudeCode, "log", harness.Batch{Events: events}, res); err != nil {
		t.Fatal(err)
	}
	scopes, err := st.RelayModelScopes(ctx, recent)
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 2 {
		t.Fatalf("scopes=%+v", scopes)
	}
	if scopes[0].Model != "claude-sonnet-4-5" || scopes[0].Harness != string(model.ClaudeCode) || scopes[0].Provider != "kamiapi" {
		t.Fatalf("first=%+v", scopes[0])
	}
	if scopes[1].Model != "gpt-5-codex" {
		t.Fatalf("second=%+v", scopes[1])
	}
	// An older window sees everything.
	all, err := st.RelayModelScopes(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("all=%+v", all)
	}
}

// TestRelayBillModels lists the distinct models a key was billed for, so a
// snapshot can price a model whose price list arrived after the events.
func TestRelayBillModels(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	bills := []relay.Bill{
		{Origin: "https://relay.example", KeyID: "k1", ID: 1, At: at, Type: "consume", Model: "claude-sonnet-4-5", ChargedUSD: 1},
		{Origin: "https://relay.example", KeyID: "k1", ID: 2, At: at.Add(time.Hour), Type: "consume", Model: "claude-sonnet-4-5", ChargedUSD: 2},
		{Origin: "https://relay.example", KeyID: "k1", ID: 3, At: at, Type: "consume", Model: "", ChargedUSD: 3},
		{Origin: "https://relay.example", KeyID: "k2", ID: 4, At: at, Type: "consume", Model: "other-key-model", ChargedUSD: 4},
	}
	if err = st.PutRelayBills(ctx, bills); err != nil {
		t.Fatal(err)
	}
	got, err := st.RelayBillModels(ctx, "https://relay.example", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "claude-sonnet-4-5" {
		t.Fatalf("models=%v", got)
	}
	got, err = st.RelayBillModels(ctx, "https://relay.example", "k2")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "other-key-model" {
		t.Fatalf("models=%v", got)
	}
	if got, err = st.RelayBillModels(ctx, "https://relay.example", "none"); err != nil || len(got) != 0 {
		t.Fatalf("models=%v err=%v", got, err)
	}
}

// TestRelayModelScopesTiming measures the query that decides which of a
// gateway's models get a price rule, so the 20ms budget stays honest.
func TestRelayModelScopesTiming(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	ctx := context.Background()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	recent := now.Add(-90 * 24 * time.Hour)
	// One row per request over 2000 models and 20 providers. Model and provider
	// are de-correlated on purpose: taking both from i% would pin the provider
	// to the model and collapse the table to one group per model, which is
	// neither the real shape nor the expensive one.
	const models, providers, rows = 2000, 20, 200000
	events := make([]model.UsageEvent, 0, rows)
	res := make([]Resolution, 0, rows)
	for i := 0; i < rows; i++ {
		events = append(events, model.UsageEvent{
			DedupKey:  fmt.Sprintf("k%d", i),
			SessionID: fmt.Sprintf("s%d", i%1000),
			Timestamp: now.Add(-time.Duration(i%3600) * time.Second),
			Model:     fmt.Sprintf("model-%d", (i/providers)%models),
		})
		res = append(res, Resolution{Provider: fmt.Sprintf("relay-%d", i%providers), Cost: 0.1, Priced: true})
	}
	if err = st.Commit(ctx, model.ClaudeCode, "log", harness.Batch{Events: events}, res); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	scopes, err := st.RelayModelScopes(ctx, recent)
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != models*providers {
		t.Fatalf("scopes=%d, want %d", len(scopes), models*providers)
	}
	t.Logf("%d scopes over %d requests: %v", len(scopes), rows, time.Since(start))
}
