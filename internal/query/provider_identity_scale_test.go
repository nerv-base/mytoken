package query

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

// idScaleStore commits one recognized endpoint plus n unresolved events in
// chunks, so the large fixture stays cheap. Every unresolved event shares its
// (harness, instance, pricing name, base URL) tuple and differs only in session
// id, which is exactly the shape that used to explode into one identity per
// session.
func idScaleStore(t *testing.T, sessions int) (Service, *store.Store) {
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
	events := []model.UsageEvent{{
		Harness: model.Codex, DedupKey: "scale-known", SessionID: "s-known", Timestamp: idDay(0),
		ProjectPath: "/p/one", Model: "gpt-5", Provider: "price-a", BaseURL: idEndpointMain,
		Tokens: model.Tokens{Input: 1},
	}}
	for i := 0; i < sessions; i++ {
		events = append(events, model.UsageEvent{
			Harness: model.Codex, DedupKey: fmt.Sprintf("scale-%05d", i), SessionID: fmt.Sprintf("s-scale-%05d", i),
			Timestamp: idDay(0), ProjectPath: "/p/one", Model: "gpt-5", Provider: "price-a", BaseURL: idEndpointRaw,
			Tokens: model.Tokens{Input: 1},
		})
	}
	resolutions := make([]store.Resolution, len(events))
	for i := range resolutions {
		resolutions[i] = store.Resolution{Provider: "price-a", Attrib: model.AttribLog, Cost: 0.001, Priced: true}
	}
	const chunk = 100
	for i := 0; i < len(events); i += chunk {
		j := min(i+chunk, len(events))
		if err = st.Commit(ctx, model.Codex, fmt.Sprintf("src-%05d", i), harness.Batch{Events: events[i:j]}, resolutions[i:j]); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	return NewService(st), st
}

// providerIdentityPredicate runs on the caller's transaction, so this helper
// mirrors the cache queries' read transaction.
func identityPredicateArgs(t *testing.T, q Service, id string) (string, int) {
	t.Helper()
	svc, ok := q.(*service)
	if !ok {
		t.Fatalf("unexpected service type %T", q)
	}
	tx, err := svc.st.DB().BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	predicate, args, restricted, err := providerIdentityPredicate(context.Background(), tx, Filter{ProviderIDs: []string{id}})
	if err != nil {
		t.Fatal(err)
	}
	if !restricted {
		t.Fatal("an identity filter must restrict the scan")
	}
	return predicate, len(args)
}

// sqliteMaxBoundParams binary-searches how many bound parameters the driver
// accepts, so the scalability argument rests on a measured number rather than an
// assumed one.
func sqliteMaxBoundParams(t *testing.T, db *sql.DB) int {
	t.Helper()
	fits := func(n int) bool {
		q := "SELECT COUNT(*) FROM (SELECT 1 WHERE 1 IN (" + strings.TrimSuffix(strings.Repeat("?,", n), ",") + "))"
		args := make([]any, n)
		for i := range args {
			args[i] = 1
		}
		var count int
		return db.QueryRow(q, args...).Scan(&count) == nil
	}
	lo, hi := 1, 1<<16
	if !fits(lo) {
		t.Fatal("driver accepts no bound parameters")
	}
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if fits(mid) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// TestProviderIdentityPredicateBoundIndependentOfSessions pins the scalability
// property: the predicate's bound-parameter count is bounded by the dictionary
// (distinct endpoint spellings) plus the requested identities, never by the
// number of sessions, and the unresolved identity itself is one row per
// (harness, instance, pricing name, base URL) tuple rather than one per session.
// The previous per-session
// (harness,instance_id,session_id,resolved_provider,base_url) tuple list grew
// with the session count and would eventually exceed the engine's
// bound-parameter limit.
func TestProviderIdentityPredicateBoundIndependentOfSessions(t *testing.T) {
	const sessions = 10000
	q, st := idScaleStore(t, sessions)

	groups, err := q.ProviderGroups(context.Background(), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	// One recognized endpoint plus the single unresolved tuple: 10000 sessions
	// must not become 10000 provider rows.
	if len(groups) != 2 {
		t.Fatalf("expected 2 identities, got %d", len(groups))
	}
	endpointID, unresolvedID := "", ""
	for _, g := range groups {
		if g.Identified {
			endpointID = g.Key
		} else if unresolvedID == "" {
			unresolvedID = g.Key
		}
	}
	if endpointID == "" || unresolvedID == "" {
		t.Fatalf("fixture must contain both identity kinds: %+v", groups)
	}
	for _, g := range groups {
		if !g.Identified && (g.Requests != int64(sessions) || g.Sessions != int64(sessions)) {
			t.Fatalf("one unresolved tuple must hold all %d sessions: %+v", sessions, g)
		}
	}

	// One endpoint spelling -> one bound parameter; one unresolved identity ->
	// its four identity columns. Neither depends on the 10000 sessions.
	if _, n := identityPredicateArgs(t, q, endpointID); n != 1 {
		t.Fatalf("endpoint identity used %d bound parameters, want 1", n)
	}
	if _, n := identityPredicateArgs(t, q, unresolvedID); n != 4 {
		t.Fatalf("unresolved identity used %d bound parameters, want 4", n)
	}
	// The endpoint predicate must not enumerate sessions either.
	predicate, _ := identityPredicateArgs(t, q, endpointID)
	if strings.Contains(predicate, "session_id") {
		t.Fatalf("endpoint predicate should depend on base_url alone: %s", predicate)
	}

	limit := sqliteMaxBoundParams(t, st.DB())
	t.Logf("measured SQLite bound-parameter limit: %d; a per-session 5-column tuple list would overflow at %d sessions, this fixture has %d",
		limit, limit/5+1, sessions)
	if limit/5+1 > sessions {
		t.Fatalf("fixture is too small to demonstrate the regression the fix prevents: limit=%d sessions=%d", limit, sessions)
	}
}
