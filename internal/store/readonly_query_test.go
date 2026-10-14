// Package store_test is an external test package on purpose: it may import
// internal/query, which itself imports internal/store, without an import cycle.
// That lets the read-only handle be exercised through the real query service —
// the same code path a snapshot reader would use — instead of only through raw
// SQL.
package store_test

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/store"
)

func commitRO(t *testing.T, st *store.Store, key, session, modelName string, input, output, cacheRead int64) {
	t.Helper()
	e := model.UsageEvent{
		Harness:   model.Codex,
		DedupKey:  key,
		SessionID: session,
		Model:     modelName,
		Provider:  "p",
		BaseURL:   "https://api.example.com",
		Timestamp: time.Date(2026, 3, 4, 12, 30, 0, 0, time.Local),
		Tokens: model.Tokens{
			Input:     input,
			Output:    output,
			CacheRead: cacheRead,
		},
	}
	if err := st.CommitMany(context.Background(), []store.Write{{
		Harness: model.Codex,
		Path:    "log",
		Batch:   harness.Batch{Events: []model.UsageEvent{e}, Next: harness.Cursor{Offset: input}},
	}}); err != nil {
		t.Fatalf("commit %q: %v", key, err)
	}
}

// The read-only handle must be a drop-in store for query.NewService: every read
// the service performs over it has to agree with the writable handle on the
// same fixture. If the read-only open skipped a step the writable open
// performs (a migration, EnsureLocalDays, NormalizeProjects), the numbers would
// diverge here.
func TestQueryServiceOverReadOnlyHandleMatchesWritableHandle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mytoken.db")

	rw, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rw.Close()

	commitRO(t, rw, "k1", "s1", "alpha", 100, 10, 40)
	commitRO(t, rw, "k2", "s1", "alpha", 200, 20, 0)
	commitRO(t, rw, "k3", "s2", "beta", 300, 30, 60)

	ro, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()
	if !ro.ReadOnly() {
		t.Fatal("ReadOnly() = false for a handle from OpenReadOnly")
	}
	if state := ro.ReadOnlySchema().State; state != store.SchemaOK {
		t.Fatalf("schema state = %q, want %q", state, store.SchemaOK)
	}

	rwSvc := query.NewService(rw)
	roSvc := query.NewService(ro)
	filter := query.Filter{}

	rwTotals, err := rwSvc.Totals(ctx, filter)
	if err != nil {
		t.Fatalf("writable Totals: %v", err)
	}
	roTotals, err := roSvc.Totals(ctx, filter)
	if err != nil {
		t.Fatalf("read-only Totals: %v", err)
	}
	if !reflect.DeepEqual(rwTotals, roTotals) {
		t.Fatalf("Totals differ:\n writable: %+v\n read-only: %+v", rwTotals, roTotals)
	}
	// Guard against comparing two empty results.
	if roTotals.Requests != 3 {
		t.Fatalf("read-only Totals.Requests = %d, want 3", roTotals.Requests)
	}
	if got := roTotals.Tokens.Input; got != 600 {
		t.Fatalf("read-only Totals.Tokens.Input = %d, want 600", got)
	}

	rwDaily, err := rwSvc.Daily(ctx, filter)
	if err != nil {
		t.Fatalf("writable Daily: %v", err)
	}
	roDaily, err := roSvc.Daily(ctx, filter)
	if err != nil {
		t.Fatalf("read-only Daily: %v", err)
	}
	if !reflect.DeepEqual(rwDaily, roDaily) {
		t.Fatalf("Daily differ:\n writable: %+v\n read-only: %+v", rwDaily, roDaily)
	}

	rwByModel, err := rwSvc.ByModel(ctx, filter)
	if err != nil {
		t.Fatalf("writable ByModel: %v", err)
	}
	roByModel, err := roSvc.ByModel(ctx, filter)
	if err != nil {
		t.Fatalf("read-only ByModel: %v", err)
	}
	if !reflect.DeepEqual(rwByModel, roByModel) {
		t.Fatalf("ByModel differ:\n writable: %+v\n read-only: %+v", rwByModel, roByModel)
	}

	rwCache, err := rwSvc.CacheTotals(ctx, filter)
	if err != nil {
		t.Fatalf("writable CacheTotals: %v", err)
	}
	roCache, err := roSvc.CacheTotals(ctx, filter)
	if err != nil {
		t.Fatalf("read-only CacheTotals: %v", err)
	}
	if !reflect.DeepEqual(rwCache, roCache) {
		t.Fatalf("CacheTotals differ:\n writable: %+v\n read-only: %+v", rwCache, roCache)
	}

	rwSessions, rwTotal, err := rwSvc.Sessions(ctx, filter, query.SortRecent, 10, 0)
	if err != nil {
		t.Fatalf("writable Sessions: %v", err)
	}
	roSessions, roTotal, err := roSvc.Sessions(ctx, filter, query.SortRecent, 10, 0)
	if err != nil {
		t.Fatalf("read-only Sessions: %v", err)
	}
	if rwTotal != roTotal || !reflect.DeepEqual(rwSessions, roSessions) {
		t.Fatalf("Sessions differ: writable total=%d %+v, read-only total=%d %+v", rwTotal, rwSessions, roTotal, roSessions)
	}
}

// The read-only handle must also serve Subscribe, because the query service
// registers for commit signals; on a read-only handle it simply never fires.
func TestReadOnlyHandleSubscribeIsUsable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mytoken.db")
	rw, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	commitRO(t, rw, "k1", "s1", "alpha", 10, 1, 0)

	ro, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()

	ch, unsubscribe := ro.Subscribe()
	if ch == nil {
		t.Fatal("Subscribe returned a nil channel")
	}
	unsubscribe()
}
