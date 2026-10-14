package query

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/provider"
)

// TestProfileProviderQueries is a read-only profiling aid for the endpoint-aware
// read paths. It prints EXPLAIN QUERY PLAN for the exact SQL the service issues
// plus per-case wall time, so a change in the query plan can be attributed to a
// concrete statement instead of guessed at. Opt-in like the strict budget:
//
//	MYTOKEN_PROFILE_PERF=1 go test ./internal/query -run '^TestProfileProviderQueries$' -count=1 -v
//
// The store has a single-connection pool, so plan inspection always uses a
// short-lived transaction that is closed before any Service call runs.
func TestProfileProviderQueries(t *testing.T) {
	if os.Getenv("MYTOKEN_PROFILE_PERF") == "" {
		t.Skip("set MYTOKEN_PROFILE_PERF=1 to dump provider query plans and timings")
	}
	q, _ := benchmarkFixture(t)
	s := q.(*service)
	ctx := context.Background()
	endpointID, _ := provider.EndpointID(benchmarkEndpoints[0])
	idFilter := Filter{ProviderIDs: []string{endpointID}}

	tx, err := s.st.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	usage, usageArgs := eventAggregateSQLMode(idFilter, false, true)
	pred, predArgs, restricted, err := providerIdentityPredicate(ctx, tx, idFilter)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("identity predicate restricted=%v args=%d: %s", restricted, len(predArgs), pred)
	cacheQ, cacheArgs := cacheRollupFromEventsWhere(idFilter, pred, predArgs)

	for _, c := range []struct {
		name  string
		query string
		args  []any
	}{
		{"TotalsIDFilter eventAggregateSQLMode", usage, usageArgs},
		{"CacheTotalsIDFilter cacheRollupFromEventsWhere", cacheQ, cacheArgs},
		{"daily_usage rollup shape", "SELECT " + usageColumns + " FROM daily_usage", nil},
		{"event_data scan only", "SELECT count(*),sum(input) FROM event_data", nil},
		{"event_data group by dimension", "SELECT dimension_id,sum(input) FROM event_data GROUP BY dimension_id", nil},
	} {
		rows, err := tx.QueryContext(ctx, "EXPLAIN QUERY PLAN "+c.query, c.args...)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		t.Logf("--- plan: %s", c.name)
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			t.Logf("    %s", detail)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []queryCase{
		{"TotalsIDFilter", func() error { _, e := q.Totals(ctx, idFilter); return e }},
		{"ProviderGroups", func() error { _, e := q.ProviderGroups(ctx, Filter{}); return e }},
		{"ProviderGroupsIDFilter", func() error { _, e := q.ProviderGroups(ctx, idFilter); return e }},
		{"CacheTotalsIDFilter", func() error { _, e := q.CacheTotals(ctx, idFilter); return e }},
	} {
		at := time.Now()
		if err := tc.run(); err != nil {
			t.Fatal(err)
		}
		t.Logf("TIMING %-26s %s", tc.name, time.Since(at))
	}

	// Candidate rewrites of the fact aggregate: group by the integer dimension
	// key instead of the 7 text columns plus a Go-computed day bucket.
	const metrics = `sum(e.input),sum(e.output),sum(e.cache_read),sum(e.cache_write),sum(e.reasoning),` +
		`sum(CASE WHEN e.priced THEN e.cost ELSE 0 END),` +
		`sum(CASE WHEN e.log_cost IS NOT NULL THEN e.cost ELSE 0 END),` +
		`sum(CASE WHEN e.log_cost IS NULL AND e.priced THEN e.cost ELSE 0 END),count(*),sum(NOT e.priced)`
	const dims = `d.harness,d.session_id,d.project,d.model,d.base_url,d.resolved_provider,d.attrib`
	const join = ` FROM event_data e JOIN event_dimensions d ON d.id=e.dimension_id`
	const noJoinMetrics = `sum(input),sum(output),sum(cache_read),sum(cache_write),sum(reasoning),` +
		`sum(CASE WHEN priced THEN cost ELSE 0 END),` +
		`sum(CASE WHEN log_cost IS NOT NULL THEN cost ELSE 0 END),` +
		`sum(CASE WHEN log_cost IS NULL AND priced THEN cost ELSE 0 END),count(*),sum(NOT priced)`

	// Isolate the raw fact shapes without the identity dictionary scan or the Go
	// displayProvider hashing, to size each contribution to the 100ms budget.
	for _, m := range []struct {
		name  string
		query string
		args  []any
	}{
		{"raw event-aggregate (day bucket, full group by)", usage, usageArgs},
		{"scan event_data only", "SELECT count(*),sum(input) FROM event_data", nil},
		{"event_data group by dimension_id", "SELECT dimension_id,sum(input) FROM event_data GROUP BY dimension_id", nil},
		{"events view scan (join, no group)", "SELECT count(*) FROM events", nil},
		{"daily_usage rollup scan", "SELECT count(*) FROM daily_usage", nil},
		{"A group-by dimension_id + join + metrics", "SELECT " + dims + "," + metrics + join + " GROUP BY e.dimension_id", nil},
		{"A2 same, base_url filter", "SELECT " + dims + "," + metrics + join + " WHERE d.base_url IN (?,?) GROUP BY e.dimension_id", []any{benchmarkEndpoints[0], benchmarkEndpoints[1]}},
		{"A3 no-join metrics group by dimension_id", "SELECT dimension_id," + noJoinMetrics + " FROM event_data GROUP BY dimension_id", nil},
		{"B text group by without day bucket", "SELECT harness,session_id,project,model,base_url,resolved_provider,attrib," + noJoinMetrics + " FROM events GROUP BY harness,session_id,project,model,base_url,resolved_provider,attrib", nil},
		{"B2 text group by + base_url filter", "SELECT harness,session_id,project,model,base_url,resolved_provider,attrib," + noJoinMetrics + " FROM events WHERE base_url IN (?,?) GROUP BY harness,session_id,project,model,base_url,resolved_provider,attrib", []any{benchmarkEndpoints[0], benchmarkEndpoints[1]}},
		{"C row count for base_url filter via join", "SELECT count(*)" + join + " WHERE d.base_url IN (?,?)", []any{benchmarkEndpoints[0], benchmarkEndpoints[1]}},
		{"D mytoken_day scalar cost", "SELECT sum(mytoken_day(timestamp)) FROM event_data", nil},
		{"E distinct dimension ids for endpoint", "SELECT count(*) FROM event_dimensions WHERE base_url IN (?,?)", []any{benchmarkEndpoints[0], benchmarkEndpoints[1]}},
	} {
		at := time.Now()
		rows, err := s.st.DB().QueryContext(ctx, m.query, m.args...)
		if err != nil {
			t.Fatalf("%s: %v", m.name, err)
		}
		count := 0
		for rows.Next() {
			count++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
		t.Logf("TIMING %-44s rows=%d %s", m.name, count, time.Since(at))
	}

	for _, c := range []struct {
		name  string
		query string
	}{
		{"A group-by dimension_id + join", "SELECT " + dims + "," + metrics + join + " GROUP BY e.dimension_id"},
		{"B text group by without day", "SELECT harness,session_id,project,model,base_url,resolved_provider,attrib," + noJoinMetrics + " FROM events GROUP BY harness,session_id,project,model,base_url,resolved_provider,attrib"},
		{"C row count for base_url filter", "SELECT count(*)" + join + " WHERE d.base_url IN (?,?)"},
	} {
		rows, err := s.st.DB().QueryContext(ctx, "EXPLAIN QUERY PLAN "+c.query, usageArgs...)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		t.Logf("--- plan: %s", c.name)
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			t.Logf("    %s", detail)
		}
		rows.Close()
	}

	fmt.Println("profile complete")
}
