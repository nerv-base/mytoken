package query

import (
	"context"
	"fmt"
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
