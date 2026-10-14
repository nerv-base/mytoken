package query

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

// instEvent is one fixture request in one explicit instance.
type instEvent struct {
	instance string
	session  string
	parent   string
	base     string
	provider string
	at       time.Time
	input    int64
	cost     float64
}

// instFixture is the same harness, the same dedup keys, the same session ids,
// the same paths and the same endpoints logged twice under two explicit
// instance ids, plus one legacy write with no instance at all. Every aggregate
// below must therefore separate alpha from beta, and the legacy write must land
// on the reserved default instance with today's semantics.
func instFixture() []instEvent {
	known := idEndpointMain
	unknown := idEndpointRaw
	return []instEvent{
		{"alpha", "s1", "", known, "price-a", idDay(0).Add(time.Hour), 100, 1},
		{"alpha", "s1", "", unknown, "price-a", idDay(0).Add(2 * time.Hour), 200, 2},
		{"alpha", "c1", "s1", known, "price-a", idDay(0).Add(3 * time.Hour), 300, 4},
		{"beta", "s1", "", known, "price-b", idDay(0).Add(time.Hour), 400, 8},
		// Same unknown URL, same session id and same pricing name as alpha: only
		// the instance can keep these two unknowns apart.
		{"beta", "s1", "", unknown, "price-a", idDay(0).Add(2 * time.Hour), 500, 16},
		{"beta", "c2", "s1", known, "price-b", idDay(0).Add(3 * time.Hour), 600, 32},
		{"", "s0", "", known, "price-a", idDay(1).Add(time.Hour), 700, 64},
	}
}

func instStore(t *testing.T, events []instEvent) (Service, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err = st.EnsurePrices(ctx, "instance-fixture", idCost); err != nil {
		t.Fatal(err)
	}
	for i, e := range events {
		ue := model.UsageEvent{Harness: model.Codex, DedupKey: fmt.Sprintf("d-%03d", i), SessionID: e.session, ParentID: e.parent, Timestamp: e.at, ProjectPath: "/p/one", Model: "gpt-5", Provider: e.provider, BaseURL: e.base, Tokens: model.Tokens{Input: e.input}}
		if err = st.CommitMany(ctx, []store.Write{{
			Harness:     model.Codex,
			InstanceID:  e.instance,
			Path:        "src",
			Batch:       harness.Batch{Events: []model.UsageEvent{ue}},
			Resolutions: []store.Resolution{{Provider: e.provider, Attrib: model.AttribLog, Cost: e.cost, Priced: true}},
		}}); err != nil {
			t.Fatalf("commit %d (instance %q): %v", i, e.instance, err)
		}
	}
	return NewService(st), st
}

// A full-day range keeps the daily and cache queries on their rollup path.
func instRange() Range {
	return Range{
		From: time.Date(2026, 3, 2, 0, 0, 0, 0, time.Local),
		To:   time.Date(2026, 3, 5, 0, 0, 0, 0, time.Local),
	}
}

// Equal logs in two instances must be two facts, not one overwritten fact, and
// the unfiltered aggregate must count both while each instance filter sees only
// its own. The legacy write keeps its prior meaning on the reserved default.
func TestInstanceTotalsSeparateIdenticalLogs(t *testing.T) {
	ctx := context.Background()
	q, _ := instStore(t, instFixture())

	all, err := q.Totals(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if all.Requests != 7 {
		t.Fatalf("unfiltered requests = %d, want 7 (2 alpha + 2 beta + 2 children + 1 default)", all.Requests)
	}
	if all.Sessions != 3 {
		t.Fatalf("unfiltered root sessions = %d, want 3 (alpha s1, beta s1, default s0)", all.Sessions)
	}

	alpha, err := q.Totals(ctx, Filter{InstanceIDs: []string{"alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	if alpha.Requests != 3 || alpha.Sessions != 1 {
		t.Fatalf("alpha totals = %+v, want 3 requests over 1 root session", alpha)
	}
	// 100 + 200 + 300 input, and alpha is the only instance holding those rows.
	if alpha.Tokens.Input != 600 {
		t.Fatalf("alpha input = %d, want 600", alpha.Tokens.Input)
	}

	beta, err := q.Totals(ctx, Filter{InstanceIDs: []string{"beta"}})
	if err != nil {
		t.Fatal(err)
	}
	if beta.Requests != 3 || beta.Tokens.Input != 1500 {
		t.Fatalf("beta totals = %+v, want 3 requests / 1500 input", beta)
	}

	// The reserved default still carries the old input with prior semantics.
	def, err := q.Totals(ctx, Filter{InstanceIDs: []string{store.DefaultInstanceID}})
	if err != nil {
		t.Fatal(err)
	}
	if def.Requests != 1 || def.Tokens.Input != 700 {
		t.Fatalf("default totals = %+v, want 1 request / 700 input", def)
	}

	// Orthogonal: two ids select the union, never a merge of identities.
	both, err := q.Totals(ctx, Filter{InstanceIDs: []string{"alpha", "beta"}})
	if err != nil {
		t.Fatal(err)
	}
	if both.Requests != 6 || both.Tokens.Input != 2100 {
		t.Fatalf("alpha+beta totals = %+v, want 6 requests / 2100 input", both)
	}
}

// Filters have to reach every aggregate surface, not only Totals.
func TestInstanceFiltersAcrossSeriesCacheGroupsAndSessions(t *testing.T) {
	ctx := context.Background()
	q, _ := instStore(t, instFixture())
	alpha := Filter{Range: instRange(), InstanceIDs: []string{"alpha"}}

	series, err := q.Daily(ctx, alpha)
	if err != nil {
		t.Fatal(err)
	}
	var seriesTokens int64
	for _, p := range series {
		seriesTokens += p.Tokens.Total()
	}
	if seriesTokens != 600 {
		t.Fatalf("alpha series tokens = %d, want 600", seriesTokens)
	}

	cache, err := q.CacheTotals(ctx, alpha)
	if err != nil {
		t.Fatal(err)
	}
	if cache.Requests != 3 {
		t.Fatalf("alpha cache requests = %d, want 3", cache.Requests)
	}
	allCache, err := q.CacheTotals(ctx, Filter{Range: instRange()})
	if err != nil {
		t.Fatal(err)
	}
	if allCache.Requests != 7 {
		t.Fatalf("unfiltered cache requests = %d, want 7", allCache.Requests)
	}

	groups, err := q.ProviderGroups(ctx, alpha)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g.Requests != 3 && g.Requests != 2 && g.Requests != 1 {
			t.Fatalf("alpha provider group leaked another instance: %+v", g)
		}
	}
	var groupRequests int64
	for _, g := range groups {
		groupRequests += g.Requests
	}
	if groupRequests != 3 {
		t.Fatalf("alpha provider groups cover %d requests, want 3", groupRequests)
	}

	rows, n, err := q.Sessions(ctx, Filter{}, SortRecent, 10, 0)
	if err != nil || n != 3 {
		t.Fatalf("sessions = %d %v", n, err)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if r.InstanceID == "" {
			t.Fatalf("session row without instance provenance: %+v", r)
		}
		if seen[r.InstanceID+"\x00"+r.SessionID] {
			t.Fatalf("session row merged across instances: %+v", r)
		}
		seen[r.InstanceID+"\x00"+r.SessionID] = true
	}
	if !seen["alpha\x00s1"] || !seen["beta\x00s1"] || !seen[store.DefaultInstanceID+"\x00s0"] {
		t.Fatalf("session rows did not keep instances apart: %+v", rows)
	}

	alphaRows, n, err := q.Sessions(ctx, Filter{InstanceIDs: []string{"alpha"}}, SortRecent, 10, 0)
	if err != nil || n != 1 || alphaRows[0].InstanceID != "alpha" {
		t.Fatalf("alpha sessions = %+v %d %v", alphaRows, n, err)
	}

	// A partial local day cannot be served from daily_usage, so this drives the
	// facts arm of the aggregate SQL: the instance dimension has to survive
	// there as well (eventAggregateSQLMode groups by instance_id), otherwise two
	// instances collapse into one bucket.
	partial := Range{From: idDay(0).Add(30 * time.Minute), To: idDay(0).Add(90 * time.Minute)}
	alphaPartial := Filter{Range: partial, InstanceIDs: []string{"alpha"}}
	pt, err := q.Totals(ctx, alphaPartial)
	if err != nil {
		t.Fatal(err)
	}
	if pt.Requests != 1 || pt.Tokens.Input != 100 {
		t.Fatalf("alpha partial-day totals = %+v, want 1 request / 100 input", pt)
	}
	allPartial, err := q.Totals(ctx, Filter{Range: partial})
	if err != nil {
		t.Fatal(err)
	}
	if allPartial.Requests != 2 || allPartial.Tokens.Input != 500 {
		t.Fatalf("unfiltered partial-day totals = %+v, want 2 requests / 500 input", allPartial)
	}
	hourly, err := q.Hourly(ctx, Filter{Range: partial})
	if err != nil {
		t.Fatal(err)
	}
	// Series stay a time axis: with no instance filter the two instances' rows in
	// the same hour intentionally collapse into one union point (two zero-filled
	// hours in range), and Filter.InstanceIDs is what separates them.
	if len(hourly) != 2 {
		t.Fatalf("unfiltered partial-day hourly = %+v, want 2 zero-filled union points", hourly)
	}
	var hourlyInput int64
	for _, p := range hourly {
		hourlyInput += p.Tokens.Input
	}
	if hourlyInput != 500 {
		t.Fatalf("unfiltered partial-day hourly input = %d, want 500", hourlyInput)
	}
	alphaHourly, err := q.Hourly(ctx, alphaPartial)
	if err != nil {
		t.Fatal(err)
	}
	var alphaHourlyInput int64
	for _, p := range alphaHourly {
		alphaHourlyInput += p.Tokens.Input
	}
	if len(alphaHourly) != 2 || alphaHourlyInput != 100 {
		t.Fatalf("alpha partial-day hourly = %+v, want alpha's 100 input only", alphaHourly)
	}
}

// The legacy Session(h,id) signature keeps working when exactly one instance
// holds that session, and must fail loudly instead of picking the first match
// when several do.
func TestInstanceSessionLookupIsAmbiguousNotFirstMatch(t *testing.T) {
	ctx := context.Background()
	q, _ := instStore(t, instFixture())

	if _, _, _, err := q.Session(ctx, model.Codex, "s1"); !errors.Is(err, ErrAmbiguousInstance) {
		t.Fatalf("Session(codex, s1) err = %v, want ErrAmbiguousInstance", err)
	}
	if _, err := q.SessionCache(ctx, model.Codex, "s1"); !errors.Is(err, ErrAmbiguousInstance) {
		t.Fatalf("SessionCache(codex, s1) err = %v, want ErrAmbiguousInstance", err)
	}
	// A session that exists in one instance only keeps the old behaviour.
	row, children, events, err := q.Session(ctx, model.Codex, "s0")
	if err != nil {
		t.Fatal(err)
	}
	if row.InstanceID != store.DefaultInstanceID || row.SessionID != "s0" || len(children) != 0 || len(events) != 1 {
		t.Fatalf("legacy single-instance lookup changed: %+v %+v %+v", row, children, events)
	}

	// The explicit API is unambiguous.
	row, children, events, err = SessionIn(ctx, q, model.Codex, "alpha", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if row.InstanceID != "alpha" || row.SessionID != "s1" || len(children) != 1 || children[0].InstanceID != "alpha" || children[0].SessionID != "c1" {
		t.Fatalf("SessionIn(alpha, s1) = %+v %+v", row, children)
	}
	if len(events) != 2 {
		t.Fatalf("SessionIn(alpha, s1) events = %d, want 2", len(events))
	}
	for _, e := range events {
		if e.InstanceID != "alpha" {
			t.Fatalf("SessionIn(alpha) leaked %q: %+v", e.InstanceID, e)
		}
	}

	beta, betaChildren, betaEvents, err := SessionIn(ctx, q, model.Codex, "beta", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if beta.InstanceID != "beta" || len(betaChildren) != 1 || betaChildren[0].SessionID != "c2" || len(betaEvents) != 2 {
		t.Fatalf("SessionIn(beta, s1) = %+v %+v %+v", beta, betaChildren, betaEvents)
	}
	// The two roots are genuinely different rows: neither children set mixes.
	for _, c := range betaChildren {
		if c.SessionID == "c1" {
			t.Fatal("beta children contain alpha's child")
		}
	}

	steps, err := SessionCacheIn(ctx, q, model.Codex, "alpha", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 {
		t.Fatalf("SessionCacheIn(alpha, s1) steps = %d, want 2", len(steps))
	}

	// CacheBySession must not merge two instances' identical harness/session.
	buckets, err := q.CacheBy(ctx, Filter{Range: instRange()}, CacheBySession)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, b := range buckets {
		if keys[b.Key] {
			t.Fatalf("cache bucket key %q returned twice", b.Key)
		}
		keys[b.Key] = true
	}
	// Five distinct (instance, session) pairs: alpha s1/c1, beta s1/c2 and the
	// default s0. Only the default keeps the historical key shape.
	if len(keys) != 5 {
		t.Fatalf("cache session buckets = %v, want 5 distinct keys", keys)
	}
	if !keys["codex/s0"] || !keys["instance:alpha:codex/s1"] || !keys["instance:beta:codex/s1"] {
		t.Fatalf("cache session keys lost their instance provenance: %v", keys)
	}
	if keys["codex/s1"] {
		t.Fatal("cache session keys merged the alpha and beta sessions")
	}
}

// A recognized endpoint may still merge display groups across instances; an
// endpoint that cannot establish equivalence must not, or two unrelated
// unknowns would be summed into one identity.
func TestInstanceUnresolvedIdentityIsolatedKnownMerged(t *testing.T) {
	ctx := context.Background()
	q, _ := instStore(t, instFixture())

	groups, err := q.ProviderGroups(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	identified, unresolved := 0, 0
	for _, g := range groups {
		if g.Identified {
			identified++
		} else {
			unresolved++
		}
	}
	if identified != 1 || unresolved != 2 {
		t.Fatalf("groups = %+v, want 1 merged endpoint group and 2 isolated unresolved groups", groups)
	}

	alpha, err := q.ProviderGroups(ctx, Filter{InstanceIDs: []string{"alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(alpha) != 2 {
		t.Fatalf("alpha groups = %+v, want the shared endpoint plus alpha's unknown", alpha)
	}

	// A known endpoint ID stays selectable and returns both instances' facts.
	var endpointID string
	for _, g := range groups {
		if g.Identified {
			endpointID = g.Key
		}
	}
	if endpointID == "" {
		t.Fatal("no identified endpoint group")
	}
	known, err := q.ProviderGroups(ctx, Filter{ProviderIDs: []string{endpointID}})
	if err != nil {
		t.Fatal(err)
	}
	// Five requests share the recognized endpoint: alpha's root and child,
	// beta's root and child, and the default write. Merging display groups
	// across instances is correct here because the endpoint is the same.
	if len(known) != 1 || known[0].Requests != 5 || known[0].Sessions != 3 {
		t.Fatalf("known endpoint group = %+v, want all 5 requests over 3 sessions across instances", known)
	}
	// The unresolved ID of one instance must not select the other's facts.
	var unresolvedID string
	for _, g := range groups {
		if !g.Identified && g.Requests == 1 {
			unresolvedID = g.Key
		}
	}
	if unresolvedID == "" {
		t.Fatal("no single-request unresolved group")
	}
	one, err := q.ProviderGroups(ctx, Filter{ProviderIDs: []string{unresolvedID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].Requests != 1 || one[0].Identified {
		t.Fatalf("unresolved ID selected %+v, want exactly one instance's request", one)
	}
}
