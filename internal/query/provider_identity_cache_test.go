package query

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/provider"
)

// idSharedBaseEvents pins the identity bug fixed alongside these tests: two
// unresolved identities can share harness, session_id and base_url and differ
// only in the resolved pricing provider name. The cache ledger carries
// resolved_provider, so selecting one identity must not read the other's facts.
// A second session with the same pricing name is a separate unresolved identity
// (genuine per-session isolation), and a recognized endpoint must keep merging
// its pricing names.
func idSharedBaseEvents() []idEvent {
	return []idEvent{
		{h: model.Codex, source: "shared-a1", session: "s-shared", model: "gpt-5", provider: "price-a", base: idEndpointRaw, project: "/p/one", at: idDay(0), input: 10, read: 5},
		{h: model.Codex, source: "shared-a2", session: "s-shared", model: "gpt-5", provider: "price-a", base: idEndpointRaw, project: "/p/one", at: idDay(0).Add(time.Hour), input: 11, read: 6},
		{h: model.Codex, source: "shared-b1", session: "s-shared", model: "gpt-5", provider: "price-b", base: idEndpointRaw, project: "/p/one", at: idDay(0).Add(2 * time.Hour), input: 20, read: 7},
		{h: model.Codex, source: "shared-b2", session: "s-shared", model: "gpt-5", provider: "price-b", base: idEndpointRaw, project: "/p/one", at: idDay(0).Add(3 * time.Hour), input: 21, read: 8},
		{h: model.Codex, source: "shared-b3", session: "s-shared", model: "gpt-5", provider: "price-b", base: idEndpointRaw, project: "/p/one", at: idDay(1), input: 22, read: 9},
		{h: model.Codex, source: "shared-o1", session: "s-other", model: "gpt-5", provider: "price-a", base: idEndpointRaw, project: "/p/one", at: idDay(1).Add(time.Hour), input: 30, read: 10},
		{h: model.ClaudeCode, source: "shared-k1", session: "s-known", model: "sonnet", provider: "price-a", base: idEndpointMain, project: "/p/two", at: idDay(0), input: 40, read: 11},
	}
}

// idSubset keeps the events selected by pred, preserving order.
func idSubset(events []idEvent, pred func(idEvent) bool) []idEvent {
	var out []idEvent
	for _, e := range events {
		if pred(e) {
			out = append(out, e)
		}
	}
	return out
}

func addCacheTotals(dst *CacheTotals, src CacheTotals) {
	dst.Requests += src.Requests
	dst.Cacheable += src.Cacheable
	dst.Expected += src.Expected
	dst.Read += src.Read
	dst.Miss += src.Miss
	dst.USD += src.USD
}

// cacheIdentityKeys returns every display identity of the fixture, keyed by
// request count so a caller can name the two shared-base identities.
func cacheIdentityKeys(t *testing.T, q Service) []ProviderGroup {
	t.Helper()
	groups, err := q.ProviderGroups(context.Background(), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 4 {
		t.Fatalf("fixture identities changed: %+v", groups)
	}
	return groups
}

func TestProviderIDCachePartitionsSharedSessionBaseURL(t *testing.T) {
	ctx := context.Background()
	all := idSharedBaseEvents()
	q, _ := idStore(t, all)
	cacheIdentityKeys(t, q)

	idA := idKeyByRequests(t, q, false, 2)        // s-shared / price-a
	idB := idKeyByRequests(t, q, false, 3)        // s-shared / price-b
	idO := idKeyByRequests(t, q, false, 1)        // s-other / price-a
	idK, _ := provider.EndpointID(idEndpointMain) // recognized endpoint

	if idA == idB || idA == idO || idB == idO {
		t.Fatalf("shared base URL must not collapse identities: %q %q %q", idA, idB, idO)
	}

	// The Go-side path already partitions; the cache ledger must agree exactly.
	cases := []struct {
		name string
		id   string
		want int64
	}{{"shared price-a", idA, 2}, {"shared price-b", idB, 3}, {"other session", idO, 1}, {"known endpoint", idK, 1}}
	for _, tc := range cases {
		tot, err := q.Totals(ctx, Filter{ProviderIDs: []string{tc.id}})
		if err != nil {
			t.Fatalf("%s totals: %v", tc.name, err)
		}
		if tot.Requests != tc.want {
			t.Fatalf("%s totals leaked: %+v want %d", tc.name, tot, tc.want)
		}
		got, err := q.CacheTotals(ctx, Filter{ProviderIDs: []string{tc.id}})
		if err != nil {
			t.Fatalf("%s cache totals: %v", tc.name, err)
		}
		if got.Requests != tc.want {
			t.Fatalf("%s cache totals leaked: %+v want %d requests", tc.name, got, tc.want)
		}
	}

	// Selecting one unresolved identity must equal a store that only ever held it.
	reducedA, _ := idStore(t, idSubset(all, func(e idEvent) bool {
		return e.session == "s-shared" && e.provider == "price-a"
	}))
	wantA, err := reducedA.CacheTotals(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	gotA, err := q.CacheTotals(ctx, Filter{ProviderIDs: []string{idA}})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(gotA) != fmt.Sprint(wantA) {
		t.Fatalf("shared-base identity cache mismatch: %+v vs reduced store %+v", gotA, wantA)
	}

	// Union over every identity must reproduce the unfiltered ledger, with no
	// request counted twice.
	whole, err := q.CacheTotals(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var union CacheTotals
	for _, g := range cacheIdentityKeys(t, q) {
		part, err := q.CacheTotals(ctx, Filter{ProviderIDs: []string{g.Key}})
		if err != nil {
			t.Fatalf("%s: %v", g.Key, err)
		}
		addCacheTotals(&union, part)
	}
	if union.Requests != whole.Requests || union.Expected != whole.Expected || union.Read != whole.Read || union.Miss != whole.Miss || math.Abs(union.USD-whole.USD) > 1e-9 {
		t.Fatalf("cache identities overlap: union=%+v whole=%+v", union, whole)
	}
}

func cacheTotalsByDay(points []CachePoint) map[int64]CacheTotals {
	out := map[int64]CacheTotals{}
	for _, p := range points {
		tot := out[p.Day.UnixNano()]
		addCacheTotals(&tot, p.CacheTotals)
		out[p.Day.UnixNano()] = tot
	}
	return out
}

func TestProviderIDCacheSeriesAndProjectionsPartitionSharedBase(t *testing.T) {
	ctx := context.Background()
	all := idSharedBaseEvents()
	q, _ := idStore(t, all)
	idA := idKeyByRequests(t, q, false, 2)
	idB := idKeyByRequests(t, q, false, 3)
	idO := idKeyByRequests(t, q, false, 1)
	idK, _ := provider.EndpointID(idEndpointMain)

	// CacheDaily is the cache series: the two shared-base identities must split
	// their days without overlap and each must match its own reduced store.
	reducedA, _ := idStore(t, idSubset(all, func(e idEvent) bool {
		return e.session == "s-shared" && e.provider == "price-a"
	}))
	wantDaily, err := reducedA.CacheDaily(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	gotDaily, err := q.CacheDaily(ctx, Filter{ProviderIDs: []string{idA}})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(gotDaily) != fmt.Sprint(wantDaily) {
		t.Fatalf("cache series leaked across identities: %+v vs %+v", gotDaily, wantDaily)
	}
	byDayA := cacheTotalsByDay(gotDaily)
	if byDayA[idDay(1).UnixNano()].Requests != 0 {
		t.Fatalf("price-a series must not include price-b's second day: %+v", gotDaily)
	}

	wholeDaily, err := q.CacheDaily(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	wholeByDay := cacheTotalsByDay(wholeDaily)
	union := map[int64]CacheTotals{}
	for _, id := range []string{idA, idB, idO, idK} {
		points, err := q.CacheDaily(ctx, Filter{ProviderIDs: []string{id}})
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		for day, tot := range cacheTotalsByDay(points) {
			acc := union[day]
			addCacheTotals(&acc, tot)
			union[day] = acc
		}
	}
	for day, want := range wholeByDay {
		got := union[day]
		if got.Requests != want.Requests || got.Expected != want.Expected || got.Read != want.Read || got.Miss != want.Miss || math.Abs(got.USD-want.USD) > 1e-9 {
			t.Fatalf("cache series overlap on day %d: union=%+v whole=%+v", day, got, want)
		}
	}

	// Every cache projection goes through the same identity predicate.
	for _, dim := range []CacheDim{CacheByProvider, CacheByModel, CacheByHarness, CacheByProject, CacheBySession} {
		wholeBy, err := q.CacheBy(ctx, Filter{}, dim)
		if err != nil {
			t.Fatalf("%s: %v", dim, err)
		}
		wholeReq := int64(0)
		for _, b := range wholeBy {
			wholeReq += b.Requests
		}
		var unionReq int64
		for _, id := range []string{idA, idB, idO, idK} {
			by, err := q.CacheBy(ctx, Filter{ProviderIDs: []string{id}}, dim)
			if err != nil {
				t.Fatalf("%s %s: %v", dim, id, err)
			}
			var req int64
			for _, b := range by {
				req += b.Requests
			}
			unionReq += req
		}
		if unionReq != wholeReq {
			t.Fatalf("%s identities overlap: union=%d whole=%d", dim, unionReq, wholeReq)
		}
		wantBy, err := reducedA.CacheBy(ctx, Filter{}, dim)
		if err != nil {
			t.Fatal(err)
		}
		gotBy, err := q.CacheBy(ctx, Filter{ProviderIDs: []string{idA}}, dim)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(gotBy) != fmt.Sprint(wantBy) {
			t.Fatalf("%s leaked across identities: %+v vs %+v", dim, gotBy, wantBy)
		}
	}
}

// TestProviderIDCacheFilterSemanticsPreserved pins that the identity selector
// narrows a cache query and does nothing else: every non-identity filter keeps
// exactly the semantics it had before, so an ID filter can never silently drop
// rows the ordinary filter would have returned.
func TestProviderIDCacheFilterSemanticsPreserved(t *testing.T) {
	ctx := context.Background()
	all := idSharedBaseEvents()
	q, st := idStore(t, all)
	idA := idKeyByRequests(t, q, false, 2)
	if err := st.SetSetting(ctx, "model-aliases", `[{"from":"gpt-5","to":"canonical","provider":"price-a"}]`); err != nil {
		t.Fatal(err)
	}
	reducedA, _ := idStore(t, idSubset(all, func(e idEvent) bool {
		return e.session == "s-shared" && e.provider == "price-a"
	}))

	for _, f := range []Filter{
		{Models: []string{"gpt-5"}},
		{Harnesses: []model.Harness{model.Codex}},
		{Project: "/p/one"},
		{Models: []string{"gpt-5"}, Harnesses: []model.Harness{model.Codex}, Project: "/p/one"},
	} {
		got := f
		got.ProviderIDs = []string{idA}
		gotTotals, err := q.CacheTotals(ctx, got)
		if err != nil {
			t.Fatal(err)
		}
		wantTotals, err := reducedA.CacheTotals(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(gotTotals) != fmt.Sprint(wantTotals) {
			t.Fatalf("identity filter changed cache semantics for %+v: %+v vs %+v", f, gotTotals, wantTotals)
		}
	}

	// Model aliases resolve on the load path (Totals) but never on the cache
	// path, which reads facts directly. The identity selector must preserve that
	// pre-existing divergence rather than start expanding (or start ignoring)
	// aliases in the cache ledger.
	alias := Filter{ProviderIDs: []string{idA}, Models: []string{"canonical"}}
	if tot, err := q.Totals(ctx, alias); err != nil || tot.Requests != 2 {
		t.Fatalf("alias under Totals: %+v %v", tot, err)
	}
	if tot, err := q.CacheTotals(ctx, alias); err != nil || tot.Requests != 0 {
		t.Fatalf("cache path must keep raw model semantics: %+v %v", tot, err)
	}
	raw := Filter{ProviderIDs: []string{idA}, Models: []string{"gpt-5"}}
	if tot, err := q.Totals(ctx, raw); err != nil || tot.Requests != 0 {
		t.Fatalf("pre-alias name must not match under Totals: %+v %v", tot, err)
	}
	if tot, err := q.CacheTotals(ctx, raw); err != nil || tot.Requests != 2 {
		t.Fatalf("cache path must keep raw model semantics: %+v %v", tot, err)
	}
}
