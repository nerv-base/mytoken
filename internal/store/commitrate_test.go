package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

var rateStamp = time.Unix(1_700_000_000, 0).UTC()

func rateEvent(key string, output int64) model.UsageEvent {
	return model.UsageEvent{DedupKey: key, SessionID: "s", Model: "gpt-5", Timestamp: rateStamp, Tokens: model.Tokens{Output: output}}
}

func rateWrite(events ...model.UsageEvent) Write {
	return Write{Harness: model.Codex, Path: "src", Batch: harness.Batch{Events: events}}
}

func openRateStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func countRateRows(t *testing.T, st *Store) int {
	t.Helper()
	var rows int
	if err := st.DB().QueryRow("SELECT count(*) FROM events").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestCommitManyOutcomeCountsFirstInsertsOnly(t *testing.T) {
	ctx := context.Background()
	st := openRateStore(t)

	out, err := st.CommitManyOutcome(ctx, []Write{rateWrite(rateEvent("k1", 100))})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Writes) != 1 || out.Writes[0].NewEvents != 1 || out.Writes[0].NewOutputTokens != 100 {
		t.Fatalf("first commit: %+v", out)
	}
	// The same event again is not new output even though the commit rewrites
	// its row.
	out, err = st.CommitManyOutcome(ctx, []Write{rateWrite(rateEvent("k1", 100))})
	if err != nil {
		t.Fatal(err)
	}
	if out.Writes[0].NewEvents != 0 || out.Writes[0].NewOutputTokens != 0 {
		t.Fatalf("duplicate commit: %+v", out)
	}
	// A revised output count on an existing key is still not new ingestion.
	out, err = st.CommitManyOutcome(ctx, []Write{rateWrite(rateEvent("k1", 150))})
	if err != nil {
		t.Fatal(err)
	}
	if out.Writes[0].NewEvents != 0 || out.Writes[0].NewOutputTokens != 0 {
		t.Fatalf("revised commit: %+v", out)
	}
	// A key repeated inside one commit counts once, at its first occurrence.
	out, err = st.CommitManyOutcome(ctx, []Write{rateWrite(rateEvent("dup", 7), rateEvent("dup", 11))})
	if err != nil {
		t.Fatal(err)
	}
	if out.Writes[0].NewEvents != 1 || out.Writes[0].NewOutputTokens != 7 {
		t.Fatalf("repeat inside commit: %+v", out)
	}
	// Output only: input, cache and reasoning tokens are not new output.
	event := rateEvent("in", 42)
	event.Tokens = model.Tokens{Input: 1000, Output: 42, CacheRead: 500, CacheWrite: 400, Reasoning: 300}
	out, err = st.CommitManyOutcome(ctx, []Write{rateWrite(event)})
	if err != nil {
		t.Fatal(err)
	}
	if out.Writes[0].NewEvents != 1 || out.Writes[0].NewOutputTokens != 42 {
		t.Fatalf("output only: %+v", out)
	}
	if rows := countRateRows(t, st); rows != 3 {
		t.Fatalf("rows=%d", rows)
	}
}

func TestCommitManyOutcomeAlignsWithWrites(t *testing.T) {
	ctx := context.Background()
	st := openRateStore(t)

	out, err := st.CommitManyOutcome(ctx, []Write{
		rateWrite(rateEvent("a", 10), rateEvent("b", 20)),
		rateWrite(rateEvent("c", 30)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Writes) != 2 {
		t.Fatalf("writes=%d", len(out.Writes))
	}
	if out.Writes[0].NewEvents != 2 || out.Writes[0].NewOutputTokens != 30 {
		t.Fatalf("write 0: %+v", out.Writes[0])
	}
	if out.Writes[1].NewEvents != 1 || out.Writes[1].NewOutputTokens != 30 {
		t.Fatalf("write 1: %+v", out.Writes[1])
	}
}

// TestCommitManyOutcomeSpansFlushBoundary repeats a key in a later 128-event
// flush of the same commit: the index already holds it by then, and it must not
// be counted twice.
func TestCommitManyOutcomeSpansFlushBoundary(t *testing.T) {
	ctx := context.Background()
	st := openRateStore(t)

	events := make([]model.UsageEvent, 0, 201)
	for i := 0; i < 200; i++ {
		events = append(events, rateEvent(fmt.Sprintf("e%d", i), 1))
	}
	events = append(events, rateEvent("e0", 1)) // the first key, repeated
	out, err := st.CommitManyOutcome(ctx, []Write{rateWrite(events...)})
	if err != nil {
		t.Fatal(err)
	}
	if out.Writes[0].NewEvents != 200 || out.Writes[0].NewOutputTokens != 200 {
		t.Fatalf("outcome: %+v", out.Writes[0])
	}
}

func TestCommitManyOutcomeFailedCommitReportsNothing(t *testing.T) {
	ctx := context.Background()
	st := openRateStore(t)

	bad := rateEvent("bad", 999)
	bad.SessionID = ""
	out, err := st.CommitManyOutcome(ctx, []Write{rateWrite(rateEvent("ok", 5)), rateWrite(bad)})
	if err == nil {
		t.Fatal("want error")
	}
	for _, write := range out.Writes {
		if write.NewEvents != 0 || write.NewOutputTokens != 0 {
			t.Fatalf("failed commit reported %+v", write)
		}
	}
	if rows := countRateRows(t, st); rows != 0 {
		t.Fatalf("rolled back commit left %d rows", rows)
	}
}

func TestCommitManyStaysCompatible(t *testing.T) {
	ctx := context.Background()
	st := openRateStore(t)

	if err := st.CommitMany(ctx, []Write{rateWrite(rateEvent("k", 5))}); err != nil {
		t.Fatal(err)
	}
	if err := st.CommitMany(ctx, []Write{rateWrite(rateEvent("k", 9))}); err != nil {
		t.Fatal(err)
	}
	if rows := countRateRows(t, st); rows != 1 {
		t.Fatalf("rows=%d", rows)
	}
	if err := st.Commit(ctx, model.Codex, "s", harness.Batch{Events: []model.UsageEvent{rateEvent("k2", 1)}}, nil); err != nil {
		t.Fatal(err)
	}
	if rows := countRateRows(t, st); rows != 2 {
		t.Fatalf("rows=%d", rows)
	}
}

// atRateEvent is rateEvent stamped at an explicit instant.
func atRateEvent(key string, output int64, at time.Time) model.UsageEvent {
	e := rateEvent(key, output)
	e.Timestamp = at
	return e
}

// TestCommitManyOutcomeRecentSeparatesEligibleRows proves the additive report:
// every new row is counted in NewOutputTokens, but only the ones whose own
// timestamp falls inside the caller's trailing window are recent. Historical,
// missing and future timestamps are stored, never presumed live, and the window
// edge is half-open exactly like the rate tracker's.
func TestCommitManyOutcomeRecentSeparatesEligibleRows(t *testing.T) {
	ctx := context.Background()
	st := openRateStore(t)
	at := rateStamp.Add(10 * time.Second)
	since := at.Add(-time.Minute)

	events := []model.UsageEvent{
		atRateEvent("history", 500, at.Add(-2*time.Hour)),
		atRateEvent("fresh", 200, at.Add(-time.Second)),
		atRateEvent("ahead", 400, at.Add(time.Hour)),
		atRateEvent("missing", 300, time.Time{}),
		atRateEvent("edge", 100, since),
		atRateEvent("inside", 50, since.Add(time.Nanosecond)),
	}
	out, err := st.CommitManyOutcomeRecent(ctx, []Write{rateWrite(events...)}, since, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Writes) != 1 {
		t.Fatalf("writes=%d", len(out.Writes))
	}
	got := out.Writes[0]
	if got.NewEvents != 6 || got.NewOutputTokens != 1550 {
		t.Fatalf("new rows: %+v", got)
	}
	if got.NewRecentOutputTokens != 250 {
		t.Fatalf("eligible output: %+v, want 250 (fresh+inside)", got)
	}
	// History stays stored: this refines the report, it never filters a commit.
	if rows := countRateRows(t, st); rows != 6 {
		t.Fatalf("rows=%d, want every event stored", rows)
	}

	// The pre-existing entry point keeps its exact report, with the additive
	// field left at zero.
	plain, err := st.CommitManyOutcome(ctx, []Write{rateWrite(atRateEvent("another", 7, at))})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Writes[0].NewOutputTokens != 7 || plain.Writes[0].NewRecentOutputTokens != 0 {
		t.Fatalf("CommitManyOutcome = %+v", plain.Writes[0])
	}
}

// TestCommitManyOutcomeRecentStaysIndexAligned checks the per-write alignment
// and the dedup rules the live rate depends on: a key the index already holds
// contributes nothing, a key repeated inside one commit counts once, and a
// failed commit reports nothing at all.
func TestCommitManyOutcomeRecentStaysIndexAligned(t *testing.T) {
	ctx := context.Background()
	st := openRateStore(t)
	at := rateStamp.Add(10 * time.Second)
	since := at.Add(-time.Minute)

	out, err := st.CommitManyOutcomeRecent(ctx, []Write{
		rateWrite(atRateEvent("old", 500, at.Add(-time.Hour)), atRateEvent("new", 100, at)),
		rateWrite(atRateEvent("newer", 25, at)),
	}, since, at)
	if err != nil {
		t.Fatal(err)
	}
	if out.Writes[0].NewRecentOutputTokens != 100 || out.Writes[1].NewRecentOutputTokens != 25 {
		t.Fatalf("alignment: %+v", out.Writes)
	}

	// The same fresh key again is not new ingestion, however much its row moves.
	out, err = st.CommitManyOutcomeRecent(ctx, []Write{rateWrite(atRateEvent("new", 999, at))}, since, at)
	if err != nil {
		t.Fatal(err)
	}
	if out.Writes[0].NewEvents != 0 || out.Writes[0].NewRecentOutputTokens != 0 {
		t.Fatalf("duplicate: %+v", out.Writes[0])
	}
	// A key repeated inside one commit counts once, at its first occurrence.
	out, err = st.CommitManyOutcomeRecent(ctx, []Write{rateWrite(
		atRateEvent("dup", 7, at), atRateEvent("dup", 11, at),
	)}, since, at)
	if err != nil {
		t.Fatal(err)
	}
	if out.Writes[0].NewEvents != 1 || out.Writes[0].NewRecentOutputTokens != 7 {
		t.Fatalf("repeat inside commit: %+v", out.Writes[0])
	}
	// A rolled-back commit reports nothing, so no token can leak into the rate.
	bad := atRateEvent("bad", 60, at)
	bad.SessionID = ""
	out, err = st.CommitManyOutcomeRecent(ctx, []Write{rateWrite(bad)}, since, at)
	if err == nil {
		t.Fatal("want error")
	}
	for _, write := range out.Writes {
		if write.NewEvents != 0 || write.NewOutputTokens != 0 || write.NewRecentOutputTokens != 0 {
			t.Fatalf("failed commit reported %+v", write)
		}
	}
	if rows := countRateRows(t, st); rows != 4 {
		t.Fatalf("rows=%d", rows)
	}
}
