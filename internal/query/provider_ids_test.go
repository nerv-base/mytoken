package query

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/provider"
	"github.com/zzstar101/mytoken/internal/store"
)

const (
	idEndpointMain = "https://api.example.com/v1"
	idEndpointAlt  = "https://api.example.com/v1/alt"
	// A raw URL with a query string cannot establish endpoint equivalence, so it
	// must produce an unresolved (harness, instance, pricing name, base URL)
	// identity instead of a shared endpoint one.
	idEndpointRaw = "https://api.example.com/v1?key=secret"
)

type idEvent struct {
	h        model.Harness
	source   string
	session  string
	model    string
	provider string
	base     string
	project  string
	at       time.Time
	input    int64
	output   int64
	read     int64
	write    int64
}

func idDay(n int) time.Time { return time.Date(2026, 3, 2+n, 9, 0, 0, 0, time.Local) }

// idProject spells a fixture/filter project path the way the host filesystem
// does. The store cleans a stored ProjectPath with filepath semantics while
// Filter.Project is compared verbatim, so a literal "/p/..." would match on
// Unix but never on Windows, where the stored value is `\p\...`.
func idProject(elem string) string { return filepath.FromSlash("/p/" + elem) }

// idFixtureEvents crosses pricing names, harnesses and paths over one endpoint,
// keeps a second path separate, and splits unknown URLs by pricing name.
func idFixtureEvents() []idEvent {
	return []idEvent{
		{h: model.Codex, source: "src-a", session: "s-main", model: "gpt-5", provider: "name-a", base: idEndpointMain, project: idProject("one"), at: idDay(0).Add(time.Hour), input: 100, read: 50, write: 10},
		{h: model.ClaudeCode, source: "src-b", session: "s-alt", model: "sonnet", provider: "name-b", base: idEndpointMain, project: idProject("two"), at: idDay(1).Add(time.Hour), input: 200, read: 80},
		{h: model.Codex, source: "src-c", session: "s-path", model: "gpt-5", provider: "name-c", base: idEndpointAlt, project: idProject("one"), at: idDay(0).Add(2 * time.Hour), input: 400},
		{h: model.Codex, source: "src-d", session: "s-unknown", model: "gpt-5", provider: "name-d", base: idEndpointRaw, project: idProject("one"), at: idDay(0).Add(3 * time.Hour), input: 800, read: 20},
		{h: model.Codex, source: "src-e", session: "s-unknown", model: "gpt-5", provider: "name-d", base: idEndpointRaw, project: idProject("one"), at: idDay(1).Add(2 * time.Hour), input: 1600},
		{h: model.Codex, source: "src-f", session: "s-unknown-2", model: "gpt-5", provider: "name-e", base: idEndpointRaw, project: idProject("one"), at: idDay(0).Add(4 * time.Hour), input: 300},
		{h: model.Codex, source: "src-g", session: "s-main", model: "gpt-5", provider: "name-a", base: idEndpointAlt, project: idProject("one"), at: idDay(0).Add(5 * time.Hour), input: 800},
	}
}

func idCost(e model.UsageEvent) (float64, bool) {
	v := float64(e.Tokens.Input)*3e-6 + float64(e.Tokens.CacheRead)*0.3e-6 + float64(e.Tokens.CacheWrite)*3.75e-6
	return v, v != 0
}

// idStore commits the given events into a fresh store, one commit per event so
// each resolution lines up with its event.
func idStore(t *testing.T, events []idEvent) (Service, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err = st.EnsurePrices(ctx, "provider-id-fixture", idCost); err != nil {
		t.Fatal(err)
	}
	for i, e := range events {
		h := e.h
		if h == "" {
			h = model.Codex
		}
		ue := model.UsageEvent{Harness: h, DedupKey: fmt.Sprintf("d-%03d", i), SessionID: e.session, Timestamp: e.at, ProjectPath: e.project, Model: e.model, Provider: e.provider, BaseURL: e.base, Tokens: model.Tokens{Input: e.input, Output: e.output, CacheRead: e.read, CacheWrite: e.write}}
		if err = st.Commit(ctx, h, e.source, harness.Batch{Events: []model.UsageEvent{ue}}, []store.Resolution{{Provider: e.provider, Attrib: model.AttribLog, Cost: 0.001, Priced: true}}); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	return NewService(st), st
}

// idGroups returns the ProviderGroups keyed by their requests count so the
// exact ID the API returned can be fed back into Filter.ProviderIDs.
func idKeyByRequests(t *testing.T, q Service, identified bool, requests int64) string {
	t.Helper()
	groups, err := q.ProviderGroups(context.Background(), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, g := range groups {
		if g.Identified == identified && g.Requests == requests {
			found = append(found, g.Key)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one group identified=%v requests=%d, got %v", identified, requests, found)
	}
	return found[0]
}

func TestProviderIDsSelectEndpointAcrossPricingNames(t *testing.T) {
	ctx := context.Background()
	q, _ := idStore(t, idFixtureEvents())
	mainID, _ := provider.EndpointID(idEndpointMain)
	altID, _ := provider.EndpointID(idEndpointAlt)
	if mainID == altID {
		t.Fatal("different paths must not share an endpoint ID")
	}

	cases := []struct {
		name string
		ids  []string
		want Totals
	}{
		{"main", []string{mainID}, Totals{Requests: 2, Sessions: 2, Tokens: model.Tokens{Input: 300, CacheRead: 130, CacheWrite: 10}}},
		{"alt path", []string{altID}, Totals{Requests: 2, Sessions: 2, Tokens: model.Tokens{Input: 1200}}},
		{"union", []string{mainID, altID}, Totals{Requests: 4, Sessions: 3, Tokens: model.Tokens{Input: 1500, CacheRead: 130, CacheWrite: 10}}},
	}
	for _, tc := range cases {
		got, err := q.Totals(ctx, Filter{ProviderIDs: tc.ids})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got.Requests != tc.want.Requests || got.Sessions != tc.want.Sessions || got.Tokens != tc.want.Tokens {
			t.Fatalf("%s: totals %+v want %+v", tc.name, got, tc.want)
		}
	}

	all, err := q.Totals(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if all.Requests != 7 || all.Tokens.Input != 4200 || all.Sessions != 5 {
		t.Fatalf("unfiltered totals changed: %+v", all)
	}

	groups, err := q.ProviderGroups(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g.Key == mainID {
			if !g.Identified || g.Requests != 2 || g.Sessions != 2 || len(g.Members) != 2 {
				t.Fatalf("cross-name endpoint group: %+v", g)
			}
			if g.Label != idEndpointMain {
				t.Fatalf("label %q", g.Label)
			}
		}
		if g.Key == altID && g.Requests != 2 {
			t.Fatalf("alt path group %+v", g)
		}
	}
}

func TestProviderIDsUnknownExactSelectionAndInvalidIDs(t *testing.T) {
	ctx := context.Background()
	q, _ := idStore(t, idFixtureEvents())
	// Unknown URLs stay unresolved; each returned key must select exactly its
	// own events.
	unknownTwo := idKeyByRequests(t, q, false, 2)
	unknownOne := idKeyByRequests(t, q, false, 1)
	mainID, _ := provider.EndpointID(idEndpointMain)

	got, err := q.Totals(ctx, Filter{ProviderIDs: []string{unknownTwo}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 2 || got.Sessions != 1 || got.Tokens.Input != 2400 {
		t.Fatalf("unknown id selection: %+v", got)
	}
	if got, err = q.Totals(ctx, Filter{ProviderIDs: []string{unknownOne}}); err != nil || got.Requests != 1 || got.Tokens.Input != 300 {
		t.Fatalf("other unknown id: %+v %v", got, err)
	}

	// Exact keys are stable: the groups query returns the same keys again.
	again := idKeyByRequests(t, q, false, 2)
	if again != unknownTwo {
		t.Fatalf("unknown key not stable: %q vs %q", again, unknownTwo)
	}

	for _, bad := range []string{"", "endpoint:v1:does-not-exist", "name-a", "unresolved:v1:00"} {
		tot, err := q.Totals(ctx, Filter{ProviderIDs: []string{bad}})
		if err != nil {
			t.Fatalf("%q: %v", bad, err)
		}
		if tot.Requests != 0 || tot.Tokens.Total() != 0 || tot.Sessions != 0 {
			t.Fatalf("invalid id %q matched data: %+v", bad, tot)
		}
		rows, total, err := q.Sessions(ctx, Filter{ProviderIDs: []string{bad}}, SortRecent, 10, 0)
		if err != nil || total != 0 || len(rows) != 0 {
			t.Fatalf("invalid id %q sessions: %+v %d %v", bad, rows, total, err)
		}
		groups, err := q.ProviderGroups(ctx, Filter{ProviderIDs: []string{bad}})
		if err != nil || len(groups) != 0 {
			t.Fatalf("invalid id %q groups: %+v %v", bad, groups, err)
		}
	}
	// A mix of unknown and known IDs keeps only the known matches.
	mixed, err := q.Totals(ctx, Filter{ProviderIDs: []string{"endpoint:v1:does-not-exist", mainID}})
	if err != nil || mixed.Requests != 2 || mixed.Tokens.Input != 300 {
		t.Fatalf("mixed ids: %+v %v", mixed, err)
	}
}

func TestProviderIDsIntersectNameHarnessModelProjectAndDate(t *testing.T) {
	ctx := context.Background()
	q, _ := idStore(t, idFixtureEvents())
	mainID, _ := provider.EndpointID(idEndpointMain)
	bogus := "endpoint:v1:does-not-exist"

	cases := []struct {
		name string
		f    Filter
		req  int64
		in   int64
	}{
		{"ids only", Filter{ProviderIDs: []string{mainID}}, 2, 300},
		{"pricing name only", Filter{Providers: []string{"name-a"}}, 2, 900},
		{"ids and name", Filter{ProviderIDs: []string{mainID}, Providers: []string{"name-a"}}, 1, 100},
		{"ids and other name", Filter{ProviderIDs: []string{mainID}, Providers: []string{"name-b"}}, 1, 200},
		{"ids and harness", Filter{ProviderIDs: []string{mainID}, Harnesses: []model.Harness{model.ClaudeCode}}, 1, 200},
		{"ids and model", Filter{ProviderIDs: []string{mainID}, Models: []string{"gpt-5"}}, 1, 100},
		{"ids and project", Filter{ProviderIDs: []string{mainID}, Project: idProject("two")}, 1, 200},
		{"ids and date", Filter{ProviderIDs: []string{mainID}, Range: Range{From: idDay(0), To: idDay(1)}}, 1, 100},
		{"ids and invalid id", Filter{ProviderIDs: []string{mainID, bogus}, Harnesses: []model.Harness{model.ClaudeCode}}, 1, 200},
	}
	for _, tc := range cases {
		got, err := q.Totals(ctx, tc.f)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got.Requests != tc.req || got.Tokens.Input != tc.in {
			t.Fatalf("%s: %+v want requests=%d input=%d", tc.name, got, tc.req, tc.in)
		}
	}
}

func TestProviderIDsPreserveModelAliases(t *testing.T) {
	ctx := context.Background()
	q, st := idStore(t, idFixtureEvents())
	mainID, _ := provider.EndpointID(idEndpointMain)
	if err := st.SetSetting(ctx, "model-aliases", `[{"from":"gpt-5","to":"canonical","provider":"name-a"}]`); err != nil {
		t.Fatal(err)
	}
	got, err := q.Totals(ctx, Filter{ProviderIDs: []string{mainID}, Models: []string{"canonical"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 1 || got.Tokens.Input != 100 {
		t.Fatalf("alias not applied under id filter: %+v", got)
	}
	// The pre-alias name must not match the aliased event any more.
	got, err = q.Totals(ctx, Filter{ProviderIDs: []string{mainID}, Models: []string{"gpt-5"}})
	if err != nil || got.Requests != 0 {
		t.Fatalf("pre-alias name still matched: %+v %v", got, err)
	}
}

func TestProviderIDsSeriesBucketsAndSessions(t *testing.T) {
	ctx := context.Background()
	q, _ := idStore(t, idFixtureEvents())
	mainID, _ := provider.EndpointID(idEndpointMain)
	altID, _ := provider.EndpointID(idEndpointAlt)
	rng := Range{From: idDay(0), To: idDay(2)}

	// The range zero-fills whole days, so the series is [day0, day1, day2].
	daily, err := q.Daily(ctx, Filter{ProviderIDs: []string{mainID}, Range: rng})
	if err != nil || len(daily) != 3 || daily[0].Tokens.Input != 100 || daily[1].Tokens.Input != 200 || daily[2].Tokens.Input != 0 {
		t.Fatalf("daily by id: %+v %v", daily, err)
	}
	dailyAlt, err := q.Daily(ctx, Filter{ProviderIDs: []string{altID}, Range: rng})
	if err != nil || len(dailyAlt) != 3 || dailyAlt[0].Tokens.Input != 1200 || dailyAlt[1].Tokens.Input != 0 || dailyAlt[2].Tokens.Input != 0 {
		t.Fatalf("daily alt by id: %+v %v", dailyAlt, err)
	}
	hourly, err := q.Hourly(ctx, Filter{ProviderIDs: []string{mainID}, Range: Range{From: idDay(0), To: idDay(0).Add(24 * time.Hour)}})
	if err != nil || len(hourly) != 24 {
		t.Fatalf("hourly by id: %+v %v", hourly, err)
	}
	var hourTotal int64
	var hourBuckets int
	for _, b := range hourly {
		if b.Tokens.Input != 0 {
			hourBuckets++
			hourTotal += b.Tokens.Input
		}
	}
	if hourTotal != 100 || hourBuckets != 1 {
		t.Fatalf("hourly by id: total=%d buckets=%d %+v", hourTotal, hourBuckets, hourly)
	}

	buckets, err := q.ByProvider(ctx, Filter{ProviderIDs: []string{mainID}})
	if err != nil || len(buckets) != 2 {
		t.Fatalf("by provider by id: %+v %v", buckets, err)
	}
	for _, b := range buckets {
		switch b.Key {
		case "name-a":
			if b.Requests != 1 || b.Tokens.Input != 100 {
				t.Fatalf("bucket name-a: %+v", b)
			}
		case "name-b":
			if b.Requests != 1 || b.Tokens.Input != 200 {
				t.Fatalf("bucket name-b: %+v", b)
			}
		default:
			t.Fatalf("unexpected bucket %+v", b)
		}
	}

	// Ranking buckets other than provider go through the same load path and must
	// narrow identically.
	if byModel, err := q.ByModel(ctx, Filter{ProviderIDs: []string{mainID}}); err != nil || len(byModel) != 2 || byModel[0].Requests+byModel[1].Requests != 2 {
		t.Fatalf("by model by id: %+v %v", byModel, err)
	}
	if byProject, err := q.ByProject(ctx, Filter{ProviderIDs: []string{mainID}}); err != nil || len(byProject) != 2 {
		t.Fatalf("by project by id: %+v %v", byProject, err)
	}
	if byHarness, err := q.ByHarness(ctx, Filter{ProviderIDs: []string{mainID}}); err != nil || len(byHarness) != 2 {
		t.Fatalf("by harness by id: %+v %v", byHarness, err)
	}

	rows, total, err := q.Sessions(ctx, Filter{ProviderIDs: []string{mainID}}, SortTokens, 10, 0)
	if err != nil || total != 2 || len(rows) != 2 {
		t.Fatalf("sessions by id: %+v %d %v", rows, total, err)
	}
	for _, r := range rows {
		if r.SessionID == "s-main" {
			if r.Requests != 1 || r.Tokens.Input != 100 {
				t.Fatalf("session s-main must be narrowed to the endpoint: %+v", r)
			}
		} else if r.SessionID != "s-alt" {
			t.Fatalf("unexpected session %+v", r)
		}
	}
	// Detail selection is unchanged: it has no Filter and still shows both events.
	detail, _, own, err := q.Session(ctx, model.Codex, "s-main")
	if err != nil || detail.Requests != 2 || len(own) != 2 {
		t.Fatalf("session detail changed: %+v %d %v", detail, len(own), err)
	}
	// SessionCache takes no Filter either, so it keeps the whole session.
	cache, err := q.SessionCache(ctx, model.Codex, "s-main")
	if err != nil || len(cache) != 2 {
		t.Fatalf("session cache changed: %+v %v", cache, err)
	}
}

func TestProviderIDsFilterCacheQueries(t *testing.T) {
	ctx := context.Background()
	q, _ := idStore(t, idFixtureEvents())
	mainID, _ := provider.EndpointID(idEndpointMain)
	altID, _ := provider.EndpointID(idEndpointAlt)
	unknownTwo := idKeyByRequests(t, q, false, 2)
	unknownOne := idKeyByRequests(t, q, false, 1)

	whole, err := q.CacheTotals(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if whole.Requests != 7 {
		t.Fatalf("unfiltered cache totals: %+v", whole)
	}
	var req, expected, read, miss int64
	var usd float64
	for _, id := range []string{mainID, altID, unknownTwo, unknownOne} {
		part, err := q.CacheTotals(ctx, Filter{ProviderIDs: []string{id}})
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		req += part.Requests
		expected += part.Expected
		read += part.Read
		miss += part.Miss
		usd += part.USD
	}
	if req != whole.Requests || expected != whole.Expected || read != whole.Read || miss != whole.Miss || math.Abs(usd-whole.USD) > 1e-9 {
		t.Fatalf("cache split mismatch: whole=%+v parts req=%d expected=%d read=%d miss=%d usd=%g", whole, req, expected, read, miss, usd)
	}

	invalid, err := q.CacheTotals(ctx, Filter{ProviderIDs: []string{"endpoint:v1:does-not-exist"}})
	if err != nil || invalid.Requests != 0 || invalid.Expected != 0 || invalid.Read != 0 || invalid.Miss != 0 {
		t.Fatalf("invalid id cache totals: %+v %v", invalid, err)
	}
	daily, err := q.CacheDaily(ctx, Filter{ProviderIDs: []string{"endpoint:v1:does-not-exist"}})
	if err != nil || len(daily) != 0 {
		t.Fatalf("invalid id cache daily: %+v %v", daily, err)
	}
	by, err := q.CacheBy(ctx, Filter{ProviderIDs: []string{"endpoint:v1:does-not-exist"}}, CacheByProvider)
	if err != nil || len(by) != 0 {
		t.Fatalf("invalid id cache by: %+v %v", by, err)
	}

	mainCache, err := q.CacheTotals(ctx, Filter{ProviderIDs: []string{mainID}})
	if err != nil || mainCache.Requests != 2 {
		t.Fatalf("cache totals by id: %+v %v", mainCache, err)
	}
	mainBy, err := q.CacheBy(ctx, Filter{ProviderIDs: []string{mainID}}, CacheByProvider)
	if err != nil || len(mainBy) != 2 {
		t.Fatalf("cache by provider by id: %+v %v", mainBy, err)
	}
	var byReq int64
	for _, b := range mainBy {
		byReq += b.Requests
	}
	if byReq != 2 {
		t.Fatalf("cache by provider requests: %d", byReq)
	}
	sessions, err := q.CacheBy(ctx, Filter{ProviderIDs: []string{mainID}}, CacheBySession)
	if err != nil || len(sessions) != 2 {
		t.Fatalf("cache by session by id: %+v %v", sessions, err)
	}
	steps, err := q.CacheDaily(ctx, Filter{ProviderIDs: []string{mainID}})
	if err != nil || len(steps) != 2 {
		t.Fatalf("cache daily by id: %+v %v", steps, err)
	}
}

// TestCacheTimezoneOverrideReadsFacts guards the events-based cache rollup,
// which an ID filter always uses and which a display-timezone override also
// selects. Its columns must match cache_daily (day,cause,requests,...) or the
// wrapping query fails.
func TestCacheTimezoneOverrideReadsFacts(t *testing.T) {
	ctx := context.Background()
	q, st := idStore(t, idFixtureEvents())
	mainID, _ := provider.EndpointID(idEndpointMain)

	rollup, err := q.CacheTotals(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	rollupMain, err := q.CacheTotals(ctx, Filter{ProviderIDs: []string{mainID}})
	if err != nil {
		t.Fatal(err)
	}

	zone := "UTC"
	if store.LocalDayZone() == "UTC" {
		zone = "Asia/Tokyo"
	}
	if err = st.SetSetting(ctx, "daily_timezone", zone); err != nil {
		t.Fatal(err)
	}

	facts, err := q.CacheTotals(ctx, Filter{})
	if err != nil {
		t.Fatalf("cache totals over events rollup: %v", err)
	}
	if fmt.Sprint(facts) != fmt.Sprint(rollup) {
		t.Fatalf("timezone override changed totals: %+v vs %+v", facts, rollup)
	}
	factsMain, err := q.CacheTotals(ctx, Filter{ProviderIDs: []string{mainID}})
	if err != nil {
		t.Fatalf("id-filtered cache totals over events rollup: %v", err)
	}
	if fmt.Sprint(factsMain) != fmt.Sprint(rollupMain) {
		t.Fatalf("id-filtered totals differ under timezone override: %+v vs %+v", factsMain, rollupMain)
	}
	if _, err = q.CacheDaily(ctx, Filter{}); err != nil {
		t.Fatalf("cache daily over events rollup: %v", err)
	}
	if _, err = q.CacheBy(ctx, Filter{}, CacheByProvider); err != nil {
		t.Fatalf("cache by provider over events rollup: %v", err)
	}
}

// TestProviderIDsEquivalentToEndpointOnlyStore checks that selecting an ID
// returns the same totals, series, sessions and cache ledger as a store that
// only ever contained that endpoint.
func TestProviderIDsEquivalentToEndpointOnlyStore(t *testing.T) {
	ctx := context.Background()
	all := idFixtureEvents()
	mainID, _ := provider.EndpointID(idEndpointMain)

	var mainOnly []idEvent
	var unknownOnly []idEvent
	for _, e := range all {
		switch e.base {
		case idEndpointMain:
			mainOnly = append(mainOnly, e)
		case idEndpointRaw:
			if e.session == "s-unknown" {
				unknownOnly = append(unknownOnly, e)
			}
		}
	}
	full, _ := idStore(t, all)
	reducedMain, _ := idStore(t, mainOnly)
	reducedUnknown, _ := idStore(t, unknownOnly)
	unknownID := idKeyByRequests(t, full, false, 2)

	rng := Range{From: idDay(0), To: idDay(2)}
	compareTotals(t, "main", mustTotals(t, full, Filter{ProviderIDs: []string{mainID}}), mustTotals(t, reducedMain, Filter{}))
	compareTotals(t, "unknown", mustTotals(t, full, Filter{ProviderIDs: []string{unknownID}}), mustTotals(t, reducedUnknown, Filter{}))

	gotDaily, err := full.Daily(ctx, Filter{ProviderIDs: []string{mainID}, Range: rng})
	if err != nil {
		t.Fatal(err)
	}
	wantDaily, err := reducedMain.Daily(ctx, Filter{Range: rng})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(gotDaily) != fmt.Sprint(wantDaily) {
		t.Fatalf("daily mismatch: %v vs %v", gotDaily, wantDaily)
	}

	gotSessions, gotTotal, err := full.Sessions(ctx, Filter{ProviderIDs: []string{mainID}}, SortRecent, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantSessions, wantTotal, err := reducedMain.Sessions(ctx, Filter{}, SortRecent, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if gotTotal != wantTotal || fmt.Sprint(sessionProjection(gotSessions)) != fmt.Sprint(sessionProjection(wantSessions)) {
		t.Fatalf("sessions mismatch: %+v/%d vs %+v/%d", gotSessions, gotTotal, wantSessions, wantTotal)
	}

	gotCache, err := full.CacheTotals(ctx, Filter{ProviderIDs: []string{mainID}})
	if err != nil {
		t.Fatal(err)
	}
	wantCache, err := reducedMain.CacheTotals(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(gotCache) != fmt.Sprint(wantCache) {
		t.Fatalf("cache totals mismatch: %+v vs %+v", gotCache, wantCache)
	}
}

func mustTotals(t *testing.T, q Service, f Filter) Totals {
	t.Helper()
	tot, err := q.Totals(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	return tot
}

// sessionProjection drops SessionMeta timestamps, which are store-level session
// bookkeeping rather than part of the filtered data selection.
func sessionProjection(rows []SessionRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%s/%s tokens=%+v cost=%g requests=%d unpriced=%d", r.Harness, r.SessionID, r.Tokens, r.CostUSD, r.Requests, r.Unpriced))
	}
	return out
}

func compareTotals(t *testing.T, label string, got, want Totals) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s totals mismatch: %+v vs %+v", label, got, want)
	}
}

func TestProviderGroupsRespectsProviderIDs(t *testing.T) {
	ctx := context.Background()
	q, _ := idStore(t, idFixtureEvents())
	mainID, _ := provider.EndpointID(idEndpointMain)
	unknownTwo := idKeyByRequests(t, q, false, 2)

	groups, err := q.ProviderGroups(ctx, Filter{ProviderIDs: []string{mainID}})
	if err != nil || len(groups) != 1 {
		t.Fatalf("groups by id: %+v %v", groups, err)
	}
	if groups[0].Key != mainID || !groups[0].Identified || groups[0].Requests != 2 || groups[0].Sessions != 2 {
		t.Fatalf("group by id: %+v", groups[0])
	}
	unknown, err := q.ProviderGroups(ctx, Filter{ProviderIDs: []string{unknownTwo}})
	if err != nil || len(unknown) != 1 || unknown[0].Identified || unknown[0].Requests != 2 {
		t.Fatalf("unknown group by id: %+v %v", unknown, err)
	}
	// The pricing-name filter stays orthogonal to the ID filter.
	both, err := q.ProviderGroups(ctx, Filter{ProviderIDs: []string{mainID}, Providers: []string{"name-b"}})
	if err != nil || len(both) != 1 || both[0].Requests != 1 {
		t.Fatalf("id and pricing name: %+v %v", both, err)
	}
	if len(both[0].Members) != 1 || both[0].Members[0].Provider != "name-b" || both[0].Members[0].Harness != model.ClaudeCode {
		t.Fatalf("id and pricing name members: %+v", both[0].Members)
	}
}
