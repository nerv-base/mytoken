package budget_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/budget"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/reporttime"
	"github.com/zzstar101/mytoken/internal/store"
)

// TestQueryCostsCountsEstimatesOnly is the provenance test: the budget must
// read the local price-table estimate (cost_estimate) and never the amounts a
// log or a relay reported as billed (cost_log).
func TestQueryCostsCountsEstimatesOnly(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	now := time.Date(2026, 3, 9, 12, 0, 0, 0, time.Local)
	const base = "https://api.example.com/v1"
	billed := 2.0

	event := func(key, session, project string, at time.Time) model.UsageEvent {
		return model.UsageEvent{
			Harness: model.Codex, DedupKey: key, SessionID: session, ProjectPath: project,
			Timestamp: at, Model: "gpt-5", BaseURL: base, Tokens: model.Tokens{Input: 10},
		}
	}
	// Project paths are stored after filepath.Clean, so a literal Unix-looking
	// "/p/one" becomes "\p\one" on Windows and no longer matches a filter built
	// from the same literal. Build every project path (and the log path) with the
	// platform separator so the stored row and the scope agree everywhere.
	projectOne := filepath.FromSlash("/p/one")
	projectTwo := filepath.FromSlash("/p/two")
	projectMissing := filepath.FromSlash("/p/missing")

	estimated := event("estimated", "s1", projectOne, now.Add(-time.Hour))
	billedEvent := event("billed", "s1", projectOne, now.Add(-2*time.Hour))
	billedEvent.CostUSD = &billed
	unpriced := event("unpriced", "s2", projectTwo, now.Add(-3*time.Hour))
	other := event("other", "s3", projectTwo, now.Add(-4*time.Hour))

	writes := []store.Write{{
		Harness:    model.Codex,
		InstanceID: store.DefaultInstanceID,
		Path:       filepath.FromSlash("/tmp/fixture.jsonl"),
		Batch:      harness.Batch{Events: []model.UsageEvent{estimated, billedEvent, unpriced, other}},
		Resolutions: []store.Resolution{
			{Provider: "openai", Attrib: model.AttribLog, Cost: 1.5, Priced: true},
			{Provider: "openai", Attrib: model.AttribLog, Cost: 0, Priced: true}, // log reported 2.00
			{Provider: "openai", Attrib: model.AttribLog, Priced: false},         // unknown price
			{Provider: "openai", Attrib: model.AttribLog, Cost: 3, Priced: true},
		},
	}}
	if err := st.CommitMany(ctx, writes); err != nil {
		t.Fatalf("CommitMany: %v", err)
	}

	source := budget.QueryCosts{Q: query.NewService(st)}
	period := mustPeriod(now, reporttime.Day, time.Local)

	// Global: 1.50 + 3.00 estimated. The billed 2.00 is a different quantity
	// and must not appear; one request has no known price.
	got, ok, err := source.EstimatedSpend(ctx, globalScope(), period)
	if err != nil || !ok {
		t.Fatalf("EstimatedSpend(global) = %+v, %v, %v; want data", got, ok, err)
	}
	if got.Amount != 4.5 {
		t.Fatalf("global estimate = %v, want 4.5 (billed 2.00 must not be blended in)", got.Amount)
	}
	if got.Unpriced != 1 {
		t.Fatalf("global unpriced = %d, want 1", got.Unpriced)
	}

	// Project scope keys on the project path, not a display label.
	one, ok, err := source.EstimatedSpend(ctx, budget.Scope{Kind: budget.ScopeProject, ID: projectOne}, period)
	if err != nil || !ok || one.Amount != 1.5 {
		t.Fatalf("EstimatedSpend(%s) = %+v, %v, %v; want 1.5", projectOne, one, ok, err)
	}
	if _, ok, err := source.EstimatedSpend(ctx, budget.Scope{Kind: budget.ScopeProject, ID: projectMissing}, period); err != nil || ok {
		t.Fatalf("missing project: ok=%v err=%v, want no data", ok, err)
	}

	// Provider scope keys on the same stable identity the providers page uses.
	groups, err := query.NewService(st).ProviderGroups(ctx, query.Filter{Range: query.Range{From: period.Start, To: period.End}})
	if err != nil || len(groups) != 1 {
		t.Fatalf("ProviderGroups = %+v, %v; want one group", groups, err)
	}
	prov, ok, err := source.EstimatedSpend(ctx, budget.Scope{Kind: budget.ScopeProvider, ID: groups[0].Key}, period)
	if err != nil || !ok || prov.Amount != 4.5 || prov.Unpriced != 1 {
		t.Fatalf("EstimatedSpend(%s) = %+v, %v, %v; want 4.5 with 1 unpriced", groups[0].Key, prov, ok, err)
	}
	if _, ok, err := source.EstimatedSpend(ctx, budget.Scope{Kind: budget.ScopeProvider, ID: "endpoint:v1:nope"}, period); err != nil || ok {
		t.Fatalf("unknown provider: ok=%v err=%v, want no data", ok, err)
	}

	// A period with no events is no data, never a zero estimate.
	empty := mustPeriod(now.AddDate(0, 0, -40), reporttime.Day, time.Local)
	if spend, ok, err := source.EstimatedSpend(ctx, globalScope(), empty); err != nil || ok {
		t.Fatalf("empty period = %+v, %v, %v; want no data", spend, ok, err)
	}
}
