package query

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

// A route that never logs a cache field is silent about the cache, not proof of
// a miss. Its prompt tokens stay in the totals but leave the hit-rate
// denominator, and Totals.CacheHit must agree with Tokens.HitRate because both
// are the one formula MyToken shows everywhere.
func TestHitRateIgnoresRoutesThatNeverReportCache(t *testing.T) {
	st, e := store.Open(filepath.Join(t.TempDir(), "db"))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	ctx := context.Background()
	day := time.Date(2026, 2, 1, 0, 0, 0, 0, time.Local)
	events := []model.UsageEvent{
		// A route that reports cache: 100 input, 300 read.
		{Harness: model.Codex, DedupKey: "a", SessionID: "s1", Timestamp: day.Add(time.Hour), Model: "gpt-5", Tokens: model.Tokens{Input: 100, CacheRead: 300}},
	}
	if e = st.Commit(ctx, model.Codex, "test", harness.Batch{Events: events}, nil); e != nil {
		t.Fatal(e)
	}
	// A relay that logs no cache field at all, twice.
	relay := []model.UsageEvent{
		{Harness: model.DSH, DedupKey: "b", SessionID: "s2", Timestamp: day.Add(2 * time.Hour), Model: "relay", Tokens: model.Tokens{Input: 400}},
		{Harness: model.DSH, DedupKey: "c", SessionID: "s2", Timestamp: day.Add(3 * time.Hour), Model: "relay", Tokens: model.Tokens{Input: 200}},
	}
	if e = st.Commit(ctx, model.DSH, "test", harness.Batch{Events: relay}, nil); e != nil {
		t.Fatal(e)
	}
	q := NewService(st)
	tot, e := q.Totals(ctx, Filter{})
	if e != nil {
		t.Fatal(e)
	}
	if tot.Tokens.Input != 700 || tot.Tokens.CacheRead != 300 {
		t.Fatalf("every token must still be counted: %+v", tot.Tokens)
	}
	if tot.Tokens.Total() != 1000 {
		t.Fatalf("Total must not drop the silent input: %d", tot.Tokens.Total())
	}
	if tot.Tokens.CacheMissing != 600 {
		t.Fatalf("silent route input = %d, want 600", tot.Tokens.CacheMissing)
	}
	if tot.CacheHit != 0.75 {
		t.Fatalf("hit rate = %v, want 0.75 (300/(100+300), the 600 silent tokens out)", tot.CacheHit)
	}
	if tot.Tokens.HitRate() != tot.CacheHit {
		t.Fatalf("Totals.CacheHit %v disagrees with Tokens.HitRate %v", tot.CacheHit, tot.Tokens.HitRate())
	}
}
