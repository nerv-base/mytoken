package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/provider"
	"github.com/zzstar101/mytoken/internal/store"
)

// factsOracleSetting forces every identity query onto the pure fact path by
// making the stored daily zone differ from the local zone: scanUsageMode and
// cacheSource then never read the endpoint-aware rollups. The same filter
// answered before and after the swap must be identical, because the rollups are
// only a derived summary of the facts.
const factsOracleSetting = "provider-rollup-facts-oracle"

func withFactsOracle(t *testing.T, st *store.Store) func() {
	t.Helper()
	ctx := context.Background()
	var saved sql.NullString
	if err := st.DB().QueryRowContext(ctx, `SELECT value FROM settings WHERE key='daily_timezone'`).Scan(&saved); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	if err := st.Exec(ctx, "INSERT INTO settings(key,value) VALUES('daily_timezone',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", factsOracleSetting); err != nil {
		t.Fatal(err)
	}
	return func() {
		var err error
		if saved.Valid {
			err = st.Exec(ctx, "INSERT INTO settings(key,value) VALUES('daily_timezone',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", saved.String)
		} else {
			err = st.Exec(ctx, "DELETE FROM settings WHERE key='daily_timezone'")
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// answers is every read API that routes through a rollup when a filter is
// endpoint-aware, in one comparable value.
type answers struct {
	Totals         Totals
	Daily          []Point
	Hourly         []Point
	ProviderGroups []ProviderGroup
	ByProvider     []Bucket
	ByModel        []Bucket
	ByProject      []Bucket
	ByHarness      []Bucket
	Sessions       []SessionRow
	CacheTotals    CacheTotals
	CacheDaily     []CachePoint
	CacheByModel   []CacheBucket
	CacheByProject []CacheBucket
}

func collectAnswers(t *testing.T, q Service, f Filter) string {
	t.Helper()
	ctx := context.Background()
	var a answers
	var err error
	if a.Totals, err = q.Totals(ctx, f); err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if a.Daily, err = q.Daily(ctx, f); err != nil {
		t.Fatalf("Daily: %v", err)
	}
	if a.Hourly, err = q.Hourly(ctx, f); err != nil {
		t.Fatalf("Hourly: %v", err)
	}
	if a.ProviderGroups, err = q.ProviderGroups(ctx, f); err != nil {
		t.Fatalf("ProviderGroups: %v", err)
	}
	if a.ByProvider, err = q.ByProvider(ctx, f); err != nil {
		t.Fatalf("ByProvider: %v", err)
	}
	if a.ByModel, err = q.ByModel(ctx, f); err != nil {
		t.Fatalf("ByModel: %v", err)
	}
	if a.ByProject, err = q.ByProject(ctx, f); err != nil {
		t.Fatalf("ByProject: %v", err)
	}
	if a.ByHarness, err = q.ByHarness(ctx, f); err != nil {
		t.Fatalf("ByHarness: %v", err)
	}
	if a.Sessions, _, err = q.Sessions(ctx, f, SortRecent, 50, 0); err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if a.CacheTotals, err = q.CacheTotals(ctx, f); err != nil {
		t.Fatalf("CacheTotals: %v", err)
	}
	if a.CacheDaily, err = q.CacheDaily(ctx, f); err != nil {
		t.Fatalf("CacheDaily: %v", err)
	}
	if a.CacheByModel, err = q.CacheBy(ctx, f, CacheByModel); err != nil {
		t.Fatalf("CacheByModel: %v", err)
	}
	if a.CacheByProject, err = q.CacheBy(ctx, f, CacheByProject); err != nil {
		t.Fatalf("CacheByProject: %v", err)
	}
	out, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// assertRollupAnswersMatchFacts runs one filter through the rollup-backed path
// and then through the forced fact path.
func assertRollupAnswersMatchFacts(t *testing.T, q Service, st *store.Store, label string, f Filter) {
	t.Helper()
	fromRollups := collectAnswers(t, q, f)
	restore := withFactsOracle(t, st)
	fromFacts := collectAnswers(t, q, f)
	restore()
	if fromRollups != fromFacts {
		t.Fatalf("%s: the endpoint-aware rollups disagree with the facts\nrollup: %s\nfacts:  %s", label, fromRollups, fromFacts)
	}
}

func providerIDKeys(t *testing.T, q Service) (mainID, altID, unresolved string) {
	t.Helper()
	mainID, ok := provider.EndpointID(idEndpointMain)
	if !ok {
		t.Fatalf("main endpoint %q is not recognized", idEndpointMain)
	}
	altID, ok = provider.EndpointID(idEndpointAlt)
	if !ok {
		t.Fatalf("alt endpoint %q is not recognized", idEndpointAlt)
	}
	groups, err := q.ProviderGroups(context.Background(), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if !g.Identified && unresolved == "" {
			unresolved = g.Key
		}
	}
	if unresolved == "" {
		t.Fatal("fixture has no unresolved identity to select")
	}
	return mainID, altID, unresolved
}

// TestProviderRollupParityWithFacts covers whole-day rollup arms, partial-day
// fact edges, mixed ranges, unresolved identities and unknown IDs.
func TestProviderRollupParityWithFacts(t *testing.T) {
	q, st := idStore(t, idFixtureEvents())
	mainID, altID, unresolved := providerIDKeys(t, q)
	// Local midnight, so the rollup arms line up with whole local days and the
	// edges are genuinely partial days.
	day := floor(idDay(0), false)

	probes := []struct {
		label  string
		filter Filter
	}{
		{"all", Filter{}},
		{"one endpoint", Filter{ProviderIDs: []string{mainID}}},
		{"two endpoints", Filter{ProviderIDs: []string{mainID, altID}}},
		{"unresolved identity", Filter{ProviderIDs: []string{unresolved}}},
		{"unknown identity", Filter{ProviderIDs: []string{"endpoint:v1:0000000000000000"}}},
		{"many unknown identities", Filter{ProviderIDs: []string{"endpoint:v1:0", "endpoint:v1:1", "endpoint:v1:2", "unresolved:v1:3"}}},
		{"whole local day", Filter{ProviderIDs: []string{mainID}, Range: Range{From: day.AddDate(0, 0, 1), To: day.AddDate(0, 0, 2)}}},
		{"two whole local days", Filter{ProviderIDs: []string{mainID, altID}, Range: Range{From: day, To: day.AddDate(0, 0, 2)}}},
		{"single partial day", Filter{ProviderIDs: []string{mainID}, Range: Range{From: day.Add(10*time.Hour + 30*time.Minute), To: day.Add(14 * time.Hour)}}},
		{"leading edge and whole day", Filter{ProviderIDs: []string{mainID}, Range: Range{From: day.Add(10*time.Hour + 30*time.Minute), To: day.AddDate(0, 0, 2)}}},
		{"whole day and trailing edge", Filter{ProviderIDs: []string{mainID}, Range: Range{From: day.AddDate(0, 0, 1), To: day.AddDate(0, 0, 2).Add(11 * time.Hour)}}},
		{"edges around whole days", Filter{ProviderIDs: []string{mainID, altID}, Range: Range{From: day.Add(10*time.Hour + 30*time.Minute), To: day.AddDate(0, 0, 3).Add(30 * time.Minute)}}},
		{"same local day partial", Filter{ProviderIDs: []string{mainID}, Range: Range{From: day.Add(10*time.Hour + 30*time.Minute), To: day.Add(11*time.Hour + 30*time.Minute)}}},
		{"empty interval", Filter{ProviderIDs: []string{mainID}, Range: Range{From: day.Add(2 * time.Hour), To: day.Add(2 * time.Hour)}}},
		{"reversed interval", Filter{ProviderIDs: []string{mainID}, Range: Range{From: day.AddDate(0, 0, 2), To: day}}},
		{"narrow trailing edge only", Filter{ProviderIDs: []string{mainID}, Range: Range{From: day.Add(9*time.Hour + 30*time.Minute), To: day.Add(11 * time.Hour)}}},
		{"harness and endpoint", Filter{Harnesses: []model.Harness{model.ClaudeCode}, ProviderIDs: []string{mainID}}},
		{"pricing name and endpoint", Filter{Providers: []string{"name-a"}, ProviderIDs: []string{mainID}}},
		{"project and endpoint", Filter{Project: "/p/one", ProviderIDs: []string{mainID, altID}}},
	}
	for _, p := range probes {
		assertRollupAnswersMatchFacts(t, q, st, p.label, p.filter)
	}
}

// TestProviderRollupParityOnDSTBoundary exercises a local day that is 23 hours
// long, so the whole-day arm and the partial edges meet at a DST transition.
func TestProviderRollupParityOnDSTBoundary(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no tzdata available: %v", err)
	}
	old := time.Local
	defer func() { time.Local = old }()
	time.Local = loc

	// 2026-03-08 02:00 EST springs forward to 03:00 EDT.
	dstDay := time.Date(2026, 3, 8, 0, 0, 0, 0, loc)
	events := []idEvent{
		{h: model.Codex, source: "src-a", session: "s-dst", model: "m", provider: "name-a", base: idEndpointMain, project: "/p/one", at: dstDay.AddDate(0, 0, -1).Add(23*time.Hour + 30*time.Minute), input: 10},
		{h: model.Codex, source: "src-b", session: "s-dst", model: "m", provider: "name-a", base: idEndpointMain, project: "/p/one", at: dstDay.Add(time.Hour + 30*time.Minute), input: 20},
		{h: model.Codex, source: "src-c", session: "s-dst", model: "m", provider: "name-a", base: idEndpointMain, project: "/p/one", at: dstDay.Add(3*time.Hour + 30*time.Minute), input: 30},
		{h: model.Codex, source: "src-d", session: "s-dst", model: "m", provider: "name-b", base: idEndpointAlt, project: "/p/one", at: dstDay.Add(23*time.Hour + 30*time.Minute), input: 40},
		{h: model.Codex, source: "src-e", session: "s-dst", model: "m", provider: "name-a", base: idEndpointMain, project: "/p/one", at: dstDay.AddDate(0, 0, 1).Add(time.Hour), input: 50},
		{h: model.Codex, source: "src-f", session: "s-dst-raw", model: "m", provider: "name-a", base: idEndpointRaw, project: "/p/one", at: dstDay.Add(2 * time.Hour), input: 60},
	}
	q, st := idStore(t, events)
	mainID, altID, _ := providerIDKeys(t, q)

	probes := []struct {
		label  string
		filter Filter
	}{
		{"whole DST day", Filter{ProviderIDs: []string{mainID}, Range: Range{From: dstDay, To: dstDay.AddDate(0, 0, 1)}}},
		{"partial inside DST day", Filter{ProviderIDs: []string{mainID}, Range: Range{From: dstDay.Add(time.Hour), To: dstDay.Add(4 * time.Hour)}}},
		{"across DST day", Filter{ProviderIDs: []string{mainID, altID}, Range: Range{From: dstDay.AddDate(0, 0, -1).Add(23 * time.Hour), To: dstDay.AddDate(0, 0, 1).Add(30 * time.Minute)}}},
		{"edge before DST day", Filter{ProviderIDs: []string{mainID}, Range: Range{From: dstDay.AddDate(0, 0, -1).Add(23 * time.Hour), To: dstDay.Add(30 * time.Minute)}}},
		{"edge after DST day", Filter{ProviderIDs: []string{mainID}, Range: Range{From: dstDay.Add(23 * time.Hour), To: dstDay.AddDate(0, 0, 1).Add(2 * time.Hour)}}},
	}
	for _, p := range probes {
		assertRollupAnswersMatchFacts(t, q, st, p.label, p.filter)
	}
}

// TestProviderRollupParityAcrossManySessions keeps the fixture wide enough that
// the rollup groups, not the fact rows, dominate the answer.
func TestProviderRollupParityAcrossManySessions(t *testing.T) {
	base := floor(idDay(0), false)
	var events []idEvent
	bases := []string{idEndpointMain, idEndpointAlt, idEndpointRaw}
	for i := 0; i < 40; i++ {
		events = append(events, idEvent{
			h:        model.Codex,
			source:   fmt.Sprintf("src-%02d", i),
			session:  fmt.Sprintf("s-%02d", i%7),
			model:    fmt.Sprintf("m-%d", i%3),
			provider: fmt.Sprintf("name-%d", i%4),
			base:     bases[i%len(bases)],
			project:  fmt.Sprintf("/p/%d", i%2),
			at:       base.Add(time.Duration(i%30) * time.Hour),
			input:    int64(10 * (i + 1)),
			read:     int64(i),
			write:    int64(i % 5),
		})
	}
	q, st := idStore(t, events)
	mainID, altID, unresolved := providerIDKeys(t, q)
	for _, f := range []Filter{
		{},
		{ProviderIDs: []string{mainID}},
		{ProviderIDs: []string{mainID, altID, unresolved}},
		{ProviderIDs: []string{unresolved}, Range: Range{From: base.Add(3 * time.Hour), To: base.AddDate(0, 0, 2)}},
		{ProviderIDs: []string{mainID}, Range: Range{From: base, To: base.AddDate(0, 0, 2)}},
	} {
		assertRollupAnswersMatchFacts(t, q, st, "wide fixture", f)
	}
}

// TestProviderRollupIsActuallyRead keeps the parity tests from passing
// vacuously: tampering with the derived tables must change the answers that an
// endpoint-filtered query returns.
func TestProviderRollupIsActuallyRead(t *testing.T) {
	ctx := context.Background()
	q, st := idStore(t, idFixtureEvents())
	mainID, _, _ := providerIDKeys(t, q)

	filtered := func() string { return collectAnswers(t, q, Filter{ProviderIDs: []string{mainID}}) }
	// ProviderGroups is always identity-aware (it needs base_url to build the
	// identity keys), even for an empty filter, so it reads provider_daily by
	// design. Everything else with an empty filter must stay on daily_usage.
	unfiltered := func() string {
		var a struct {
			Totals         Totals
			Daily          []Point
			ByProvider     []Bucket
			ByModel        []Bucket
			ByProject      []Bucket
			ByHarness      []Bucket
			Sessions       []SessionRow
			CacheTotals    CacheTotals
			CacheDaily     []CachePoint
			CacheByModel   []CacheBucket
			CacheByProject []CacheBucket
		}
		var err error
		if a.Totals, err = q.Totals(ctx, Filter{}); err != nil {
			t.Fatal(err)
		}
		if a.Daily, err = q.Daily(ctx, Filter{}); err != nil {
			t.Fatal(err)
		}
		if a.ByProvider, err = q.ByProvider(ctx, Filter{}); err != nil {
			t.Fatal(err)
		}
		if a.ByModel, err = q.ByModel(ctx, Filter{}); err != nil {
			t.Fatal(err)
		}
		if a.ByProject, err = q.ByProject(ctx, Filter{}); err != nil {
			t.Fatal(err)
		}
		if a.ByHarness, err = q.ByHarness(ctx, Filter{}); err != nil {
			t.Fatal(err)
		}
		if a.Sessions, _, err = q.Sessions(ctx, Filter{}, SortRecent, 50, 0); err != nil {
			t.Fatal(err)
		}
		if a.CacheTotals, err = q.CacheTotals(ctx, Filter{}); err != nil {
			t.Fatal(err)
		}
		if a.CacheDaily, err = q.CacheDaily(ctx, Filter{}); err != nil {
			t.Fatal(err)
		}
		if a.CacheByModel, err = q.CacheBy(ctx, Filter{}, CacheByModel); err != nil {
			t.Fatal(err)
		}
		if a.CacheByProject, err = q.CacheBy(ctx, Filter{}, CacheByProject); err != nil {
			t.Fatal(err)
		}
		out, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}

	before, plainBefore := filtered(), unfiltered()
	if err := st.Exec(ctx, "UPDATE provider_daily SET input = input + 1000000"); err != nil {
		t.Fatal(err)
	}
	if err := st.Exec(ctx, "UPDATE provider_cache_daily SET requests = requests + 5"); err != nil {
		t.Fatal(err)
	}
	after, plainAfter := filtered(), unfiltered()
	if before == after {
		t.Fatal("tampering with provider_daily/provider_cache_daily did not change the endpoint-filtered answers; the derived rollups are not being read")
	}
	if plainBefore != plainAfter {
		t.Fatal("tampering with provider_daily changed an unfiltered query; an unfiltered query must read daily_usage/cache_daily, not the endpoint-aware rollups")
	}
}
