package query

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

// mergeEvent is one fixture request logged under an explicit data-source
// instance with an explicit resolved pricing provider.
type mergeEvent struct {
	instance string
	session  string
	provider string
	base     string
	input    int64
	at       time.Time
}

// mergeStore commits one event per Write so each resolution lines up with its
// event and the instance id is explicit.
func mergeStore(t *testing.T, events []mergeEvent) Service {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err = st.EnsurePrices(ctx, "provider-merge-fixture", idCost); err != nil {
		t.Fatal(err)
	}
	for i, e := range events {
		at := e.at
		if at.IsZero() {
			at = idDay(0).Add(time.Duration(i) * time.Hour)
		}
		ue := model.UsageEvent{
			Harness: model.Codex, DedupKey: fmt.Sprintf("m-%03d", i), SessionID: e.session,
			Timestamp: at, ProjectPath: idProject("merge"), Model: "gpt-5",
			Provider: e.provider, BaseURL: e.base, Tokens: model.Tokens{Input: e.input},
		}
		if err = st.CommitMany(ctx, []store.Write{{
			Harness:     model.Codex,
			InstanceID:  e.instance,
			Path:        "src",
			Batch:       harness.Batch{Events: []model.UsageEvent{ue}},
			Resolutions: []store.Resolution{{Provider: e.provider, Attrib: model.AttribLog, Cost: 0.001, Priced: true}},
		}}); err != nil {
			t.Fatalf("commit %d (instance %q): %v", i, e.instance, err)
		}
	}
	return NewService(st)
}

// mergedFixture is the real-world shape that produced one 未知供应商 row per
// session: an empty base_url (so no endpoint identity exists), one data-source
// instance and one resolved pricing provider, logged by three sessions.
func mergedFixture() []mergeEvent {
	return []mergeEvent{
		{instance: "nerv-base", session: "sess-1", provider: "nerv-base", base: "", input: 100},
		{instance: "nerv-base", session: "sess-2", provider: "nerv-base", base: "", input: 200},
		{instance: "nerv-base", session: "sess-3", provider: "nerv-base", base: "", input: 300},
	}
}

// Three sessions of one unresolved endpoint are ONE provider identity. This is
// the regression: the ranking tab used to list the same provider once per
// session, each with its own 未识别端点 hash.
func TestProviderGroupsMergeSessionsOfOneUnresolvedEndpoint(t *testing.T) {
	ctx := context.Background()
	q := mergeStore(t, mergedFixture())

	groups, err := q.ProviderGroups(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("three sessions of one unresolved endpoint must be one identity: %+v", groups)
	}
	g := groups[0]
	if g.Identified {
		t.Fatalf("an empty base_url must stay unidentified: %+v", g)
	}
	if g.Requests != 3 || g.Sessions != 3 || g.Tokens.Input != 600 {
		t.Fatalf("merged group = %+v, want 3 requests over 3 sessions with 600 input tokens", g)
	}
	if g.Label != "nerv-base" {
		t.Fatalf("merged label = %q, want the resolved pricing provider", g.Label)
	}
}

// The merged identity must still be usable for drill-down: selecting it returns
// every session's facts, and the SQL predicate must describe the four-column
// identity rather than the old five-column one. A fix that changed only
// displayProvider would leave session_id in the predicate and re-grow its
// bound-parameter list with the session count (see the scale test).
func TestProviderIDSelectsEverySessionOfAMergedUnresolvedEndpoint(t *testing.T) {
	ctx := context.Background()
	q := mergeStore(t, mergedFixture())

	groups, err := q.ProviderGroups(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("fixture must yield one merged identity: %+v", groups)
	}
	key := groups[0].Key

	tot, err := q.Totals(ctx, Filter{ProviderIDs: []string{key}})
	if err != nil {
		t.Fatal(err)
	}
	if tot.Requests != 3 || tot.Sessions != 3 || tot.Tokens.Input != 600 {
		t.Fatalf("merged identity drill-down = %+v, want all three sessions", tot)
	}

	// The cache ledger goes through providerIdentityPredicate instead of the Go
	// selector, so it independently proves the tuple matches all three sessions.
	cache, err := q.CacheTotals(ctx, Filter{ProviderIDs: []string{key}})
	if err != nil {
		t.Fatal(err)
	}
	if cache.Requests != 3 {
		t.Fatalf("cache drill-down lost sessions: %+v", cache)
	}

	rows, total, err := q.Sessions(ctx, Filter{ProviderIDs: []string{key}}, SortRecent, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(rows) != 3 {
		t.Fatalf("session drill-down = %d rows (total %d), want all three", len(rows), total)
	}

	// The predicate is the four-column tuple: session_id would make the bound
	// list grow with the session count again.
	predicate, bound := identityPredicateArgs(t, q, key)
	if strings.Contains(predicate, "session_id") {
		t.Fatalf("unresolved predicate still enumerates sessions: %s", predicate)
	}
	if bound != 4 {
		t.Fatalf("unresolved predicate bound %d parameters, want 4", bound)
	}

	// A different unresolved identity must still select nothing.
	other := providerIDUnresolvedPrefix + "00"
	if tot, err = q.Totals(ctx, Filter{ProviderIDs: []string{other}}); err != nil || tot.Requests != 0 {
		t.Fatalf("foreign identity matched data: %+v %v", tot, err)
	}
}

// The pricing name is part of the unresolved identity: two resolved providers
// that share the instance, base URL and session shape stay two rows.
func TestProviderGroupsKeepUnresolvedPricingNamesApart(t *testing.T) {
	ctx := context.Background()
	q := mergeStore(t, []mergeEvent{
		{instance: "nerv-base", session: "sess-1", provider: "acct-a", base: ""},
		{instance: "nerv-base", session: "sess-2", provider: "acct-b", base: ""},
	})

	groups, err := q.ProviderGroups(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("two pricing names must stay two identities: %+v", groups)
	}
	labels := map[string]bool{}
	for _, g := range groups {
		if g.Identified || g.Requests != 1 || g.Sessions != 1 {
			t.Fatalf("unexpected unresolved group: %+v", g)
		}
		labels[g.Label] = true
	}
	if !labels["acct-a"] || !labels["acct-b"] {
		t.Fatalf("pricing names lost: %+v", labels)
	}
}

// Endpoint identity is untouched: two recognized base URLs are two identities
// even when instance, session shape and pricing name are identical.
func TestProviderGroupsKeepRealEndpointsApart(t *testing.T) {
	ctx := context.Background()
	q := mergeStore(t, []mergeEvent{
		{instance: "nerv-base", session: "sess-1", provider: "acct-a", base: idEndpointMain},
		{instance: "nerv-base", session: "sess-2", provider: "acct-a", base: idEndpointAlt},
	})

	groups, err := q.ProviderGroups(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("two recognized endpoints must stay two identities: %+v", groups)
	}
	for _, g := range groups {
		if !g.Identified || g.Requests != 1 || g.Sessions != 1 {
			t.Fatalf("unexpected endpoint group: %+v", g)
		}
	}
}
