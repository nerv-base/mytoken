package query

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

// hourlyUsageFixture spans two local days, four clock hours and two harnesses.
// The two Codex events in the first hour share a session, model and provider so
// the fact aggregate already folds them together; the last Claude Code request
// has no price, so the unpriced count is exercised but contributes no cost.
func hourlyUsageFixture(t *testing.T) (Service, time.Time) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.Local)
	codex := []model.UsageEvent{
		{DedupKey: "c1", SessionID: "s1", Timestamp: day.Add(time.Hour + 5*time.Minute), Model: "gpt-5", Tokens: model.Tokens{Input: 100}},
		{DedupKey: "c2", SessionID: "s1", Timestamp: day.Add(time.Hour + 45*time.Minute), Model: "gpt-5", Tokens: model.Tokens{Input: 50, CacheRead: 10}},
		{DedupKey: "c3", SessionID: "s1", Timestamp: day.Add(25*time.Hour + 10*time.Minute), Model: "o3", Tokens: model.Tokens{Output: 20}},
	}
	if err = st.Commit(ctx, model.Codex, "codex-log", harness.Batch{Events: codex}, []store.Resolution{
		{Provider: "provider-a", Attrib: model.AttribLog, Cost: 1, Priced: true},
		{Provider: "provider-a", Attrib: model.AttribLog, Cost: 0.5, Priced: true},
		{Provider: "provider-a", Attrib: model.AttribLog, Cost: 2, Priced: true},
	}); err != nil {
		t.Fatal(err)
	}
	claude := []model.UsageEvent{
		{DedupKey: "a1", SessionID: "s2", Timestamp: day.Add(3*time.Hour + 30*time.Minute), Model: "sonnet", Tokens: model.Tokens{Input: 7}},
		{DedupKey: "a2", SessionID: "s2", Timestamp: day.Add(26*time.Hour + 40*time.Minute), Model: "sonnet", Tokens: model.Tokens{Input: 9}},
		{DedupKey: "a3", SessionID: "s2", Timestamp: day.Add(26*time.Hour + 41*time.Minute), Model: "sonnet", Tokens: model.Tokens{Input: 11}},
	}
	if err = st.Commit(ctx, model.ClaudeCode, "claude-log", harness.Batch{Events: claude}, []store.Resolution{
		{Provider: "provider-b", Attrib: model.AttribLog, Cost: 0.25, Priced: true},
		{Provider: "provider-b", Attrib: model.AttribLog, Cost: 1.25, Priced: true},
		{Provider: "provider-b", Attrib: model.AttribLog},
	}); err != nil {
		t.Fatal(err)
	}
	return NewService(st), day
}

// Every hourly row must land in an Hourly point with the same tokens and cost,
// and the rows must add up to Totals. That is the contract the dashboard relies
// on: the same read, priced and attributed once, sliced at two widths.
func TestHourlyUsageMatchesHourlySeriesAndTotals(t *testing.T) {
	ctx := context.Background()
	q, day := hourlyUsageFixture(t)
	f := Filter{Range: Range{From: day, To: day.Add(48 * time.Hour)}}

	rows, err := q.HourlyUsage(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	points, err := q.Hourly(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	totals, err := q.Totals(ctx, f)
	if err != nil {
		t.Fatal(err)
	}

	// Rows come back sorted by Day, then Harness, Model, Provider, and only
	// hours that carry usage get a row.
	if len(rows) != 4 {
		t.Fatalf("hourly usage rows = %d, want 4 (%+v)", len(rows), rows)
	}
	for i := 1; i < len(rows); i++ {
		a, b := rows[i-1], rows[i]
		if a.Day.After(b.Day) ||
			(a.Day.Equal(b.Day) && a.Harness > b.Harness) ||
			(a.Day.Equal(b.Day) && a.Harness == b.Harness && a.Model > b.Model) ||
			(a.Day.Equal(b.Day) && a.Harness == b.Harness && a.Model == b.Model && a.Provider > b.Provider) {
			t.Fatalf("rows not sorted: %+v before %+v", a, b)
		}
	}

	byHour := map[int64]UsageRow{}
	var totalTokens model.Tokens
	var totalCost float64
	var totalRequests, totalUnpriced int64
	for _, r := range rows {
		if r.Day.Location() != time.Local || r.Day.Minute() != 0 || r.Day.Second() != 0 || r.Day.Nanosecond() != 0 {
			t.Fatalf("row Day %v is not a local clock-hour start", r.Day)
		}
		if _, dup := byHour[r.Day.UnixNano()]; dup {
			t.Fatalf("duplicate row for hour %v", r.Day)
		}
		byHour[r.Day.UnixNano()] = r
		totalTokens = totalTokens.Add(r.Tokens)
		totalCost += r.CostUSD
		totalRequests += r.Requests
		totalUnpriced += r.Unpriced
	}

	// Hourly is zero-filled; every nonzero point must equal the matching row,
	// and every row must be reachable from a point.
	nonzero := int64(0)
	var seriesTokens model.Tokens
	var seriesCost float64
	for _, p := range points {
		seriesTokens = seriesTokens.Add(p.Tokens)
		seriesCost += p.CostUSD
		r, ok := byHour[p.Day.UnixNano()]
		if p.Tokens.IsZero() && p.CostUSD == 0 {
			if ok {
				t.Fatalf("hour %v has zero usage in Hourly but a row in HourlyUsage", p.Day)
			}
			continue
		}
		nonzero++
		if !ok {
			t.Fatalf("Hourly point %v has usage but HourlyUsage has no row", p.Day)
		}
		if r.Tokens != p.Tokens || math.Abs(r.CostUSD-p.CostUSD) > 1e-9 {
			t.Fatalf("hour %v: row tokens/cost %+v/$%v != point %+v/$%v", p.Day, r.Tokens, r.CostUSD, p.Tokens, p.CostUSD)
		}
	}
	if nonzero != int64(len(rows)) {
		t.Fatalf("nonzero Hourly points = %d, HourlyUsage rows = %d", nonzero, len(rows))
	}

	if totalTokens != seriesTokens || math.Abs(totalCost-seriesCost) > 1e-9 {
		t.Fatalf("rows total %+v/$%v != Hourly total %+v/$%v", totalTokens, totalCost, seriesTokens, seriesCost)
	}
	if totalTokens != totals.Tokens || math.Abs(totalCost-totals.CostUSD) > 1e-9 {
		t.Fatalf("rows total %+v/$%v != Totals %+v/$%v", totalTokens, totalCost, totals.Tokens, totals.CostUSD)
	}
	if totalRequests != totals.Requests || totalUnpriced != totals.Unpriced {
		t.Fatalf("rows requests/unpriced %d/%d != Totals %d/%d", totalRequests, totalUnpriced, totals.Requests, totals.Unpriced)
	}
	if totals.Unpriced != 1 {
		t.Fatalf("fixture unpriced requests = %d, want 1", totals.Unpriced)
	}
}

// HourlyUsage narrows by the same Filter as Usage: a harness filter, a
// provider filter and half-open range boundaries must all apply before the
// hour bucket is formed.
func TestHourlyUsageFilters(t *testing.T) {
	ctx := context.Background()
	q, day := hourlyUsageFixture(t)

	rows, err := q.HourlyUsage(ctx, Filter{Harnesses: []model.Harness{model.ClaudeCode}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("claude rows = %d, want 2 (%+v)", len(rows), rows)
	}
	for _, r := range rows {
		if r.Harness != model.ClaudeCode || r.Provider != "provider-b" {
			t.Fatalf("harness filter leaked a row: %+v", r)
		}
	}

	rows, err = q.HourlyUsage(ctx, Filter{Providers: []string{"provider-a"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("provider-a rows = %d, want 2 (%+v)", len(rows), rows)
	}

	// [day+1h, day+2h) covers the two folded Codex events but neither the 3h
	// Claude request nor the next day's rows.
	rows, err = q.HourlyUsage(ctx, Filter{Range: Range{From: day.Add(time.Hour), To: day.Add(2 * time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].Day.Equal(day.Add(time.Hour)) || rows[0].Requests != 2 || rows[0].Tokens.Input != 150 {
		t.Fatalf("one-hour range rows = %+v", rows)
	}
}
