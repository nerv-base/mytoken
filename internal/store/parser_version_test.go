package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

// seedSource commits one event, its session and its cursor for h, so a reset
// has every derived row to remove. The dedup key is explicit because the caller
// usually needs to tell the rows apart afterwards.
func seedSource(t *testing.T, st *store.Store, h model.Harness, path, session, key string, input int64, at time.Time) {
	t.Helper()
	batch := harness.Batch{
		Events: []model.UsageEvent{{
			Harness: h, DedupKey: key, SessionID: session,
			Timestamp: at, Model: "m", GitBranch: "main",
			Tokens: model.Tokens{Input: input},
		}},
		Sessions: []model.SessionMeta{{
			Harness: h, SessionID: session, StartedAt: at, UpdatedAt: at,
		}},
		Next: harness.Cursor{Offset: input, Size: input, ModTime: at},
	}
	if err := st.Commit(context.Background(), h, path, batch, nil); err != nil {
		t.Fatal(err)
	}
}

// seedHarness commits one event, session and cursor for h on its own path.
func seedHarness(t *testing.T, st *store.Store, h model.Harness, at time.Time) {
	t.Helper()
	seedSource(t, st, h, "source-"+string(h), string(h)+"-session", string(h)+"-request", 7, at)
}

func storeCount(t *testing.T, st *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func eventKeyExists(t *testing.T, st *store.Store, key string) bool {
	t.Helper()
	return storeCount(t, st, `SELECT count(*) FROM event_data WHERE harness=? AND dedup_key=?`, model.ClaudeCode, key) == 1
}

// TestResetHarnessDeletesOnlyScopedRows is the core contract: a reset covers the
// sources the scan is about to re-read, and everything else — a source the
// harness deleted, a different harness — survives. It also pins the derived-row
// rules: an orphaned session and dimension go, a session still backing a kept
// event stays, and the rollups follow the facts.
func TestResetHarnessDeletesOnlyScopedRows(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	at := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)

	pathA := "/logs/alpha-a.jsonl"
	pathB := "/logs/alpha-b.jsonl"
	seedSource(t, st, model.ClaudeCode, pathA, "alpha-a", "alpha-a", 1, at)
	seedSource(t, st, model.ClaudeCode, pathB, "alpha-b", "alpha-b", 2, at)
	seedHarness(t, st, model.Codex, at)

	if got, err := st.HarnessParserVersion(ctx, model.ClaudeCode); err != nil || got != 0 {
		t.Fatalf("initial version=%d err=%v, want 0", got, err)
	}
	// Only A is scoped: it exists on disk and will be re-read. B stands in for a
	// source the harness removed, so its row, session and cursor stay.
	if err := st.ResetHarness(ctx, model.ClaudeCode, 2, store.ResetScope{
		Paths:    []string{pathA},
		Sessions: []string{"alpha-a"},
	}); err != nil {
		t.Fatal(err)
	}

	if got := storeCount(t, st, `SELECT count(*) FROM event_data WHERE harness=?`, model.ClaudeCode); got != 1 {
		t.Errorf("alpha events=%d, want 1", got)
	}
	if eventKeyExists(t, st, "alpha-a") {
		t.Error("scoped event alpha-a survived")
	}
	if !eventKeyExists(t, st, "alpha-b") {
		t.Error("unscoped event alpha-b was deleted")
	}
	if got := storeCount(t, st, `SELECT count(*) FROM sessions WHERE harness=?`, model.ClaudeCode); got != 1 {
		t.Errorf("alpha sessions=%d, want 1", got)
	}
	if got := storeCount(t, st, `SELECT count(*) FROM sessions WHERE harness=? AND session_id=?`, model.ClaudeCode, "alpha-a"); got != 0 {
		t.Errorf("orphaned session alpha-a=%d, want 0", got)
	}
	if got := storeCount(t, st, `SELECT count(*) FROM sessions WHERE harness=? AND session_id=?`, model.ClaudeCode, "alpha-b"); got != 1 {
		t.Errorf("kept session alpha-b=%d, want 1", got)
	}
	if got := storeCount(t, st, `SELECT count(*) FROM cursors WHERE harness=?`, model.ClaudeCode); got != 1 {
		t.Errorf("alpha cursors=%d, want 1", got)
	}
	if got := storeCount(t, st, `SELECT count(*) FROM cursors WHERE harness=? AND path=?`, model.ClaudeCode, pathA); got != 0 {
		t.Errorf("scoped cursor=%d, want 0", got)
	}
	if got := storeCount(t, st, `SELECT count(*) FROM cursors WHERE harness=? AND path=?`, model.ClaudeCode, pathB); got != 1 {
		t.Errorf("unscoped cursor=%d, want 1", got)
	}
	if got := storeCount(t, st, `SELECT count(*) FROM event_dimensions WHERE harness=? AND session_id=?`, model.ClaudeCode, "alpha-a"); got != 0 {
		t.Errorf("orphaned dimension=%d, want 0", got)
	}
	if got := storeCount(t, st, `SELECT count(*) FROM event_dimensions WHERE harness=? AND session_id=?`, model.ClaudeCode, "alpha-b"); got != 1 {
		t.Errorf("kept dimension=%d, want 1 (still referenced)", got)
	}
	if got := storeCount(t, st, `SELECT count(*) FROM event_branches WHERE harness=?`, model.ClaudeCode); got != 1 {
		t.Errorf("alpha branches=%d, want 1", got)
	}

	// The untouched harness keeps everything.
	for _, q := range []struct {
		name  string
		query string
		want  int
	}{
		{"codex events", `SELECT count(*) FROM event_data WHERE harness='codex'`, 1},
		{"codex sessions", `SELECT count(*) FROM sessions WHERE harness='codex'`, 1},
		{"codex cursors", `SELECT count(*) FROM cursors WHERE harness='codex'`, 1},
		{"codex branches", `SELECT count(*) FROM event_branches WHERE harness='codex'`, 1},
	} {
		if got := storeCount(t, st, q.query); got != q.want {
			t.Errorf("%s=%d, want %d", q.name, got, q.want)
		}
	}

	// A fact deletion feeds the rollup triggers, so both rollups lose exactly
	// the removed request instead of keeping a stale aggregate.
	if got := storeCount(t, st, `SELECT coalesce(sum(requests),0) FROM daily_usage`); got != 2 {
		t.Errorf("daily requests=%d, want 2", got)
	}
	if got := storeCount(t, st, `SELECT coalesce(sum(requests),0) FROM provider_daily`); got != 2 {
		t.Errorf("provider daily requests=%d, want 2", got)
	}
	if got, err := st.HarnessParserVersion(ctx, model.ClaudeCode); err != nil || got != 2 {
		t.Fatalf("recorded version=%d err=%v, want 2", got, err)
	}
	if got, err := st.HarnessParserVersion(ctx, model.Codex); err != nil || got != 0 {
		t.Fatalf("untouched version=%d err=%v, want 0", got, err)
	}
	if key := store.HarnessParserVersionKey(model.ClaudeCode); key != "harness.claude-code.parser_version" {
		t.Errorf("key=%q", key)
	}
	var raw string
	if err := st.DB().QueryRow(`SELECT value FROM settings WHERE key=?`, store.HarnessParserVersionKey(model.ClaudeCode)).Scan(&raw); err != nil || raw != "2" {
		t.Errorf("stored marker=%q err=%v, want 2", raw, err)
	}

	// An empty scope is what a harness whose logs all rotated away looks like:
	// nothing may be deleted, but the generation is still recorded.
	if err := st.ResetHarness(ctx, model.ClaudeCode, 3, store.ResetScope{}); err != nil {
		t.Fatal(err)
	}
	if got := storeCount(t, st, `SELECT count(*) FROM event_data WHERE harness=?`, model.ClaudeCode); got != 1 {
		t.Errorf("events after empty reset=%d, want 1", got)
	}
	if got := storeCount(t, st, `SELECT count(*) FROM sessions WHERE harness=?`, model.ClaudeCode); got != 1 {
		t.Errorf("sessions after empty reset=%d, want 1", got)
	}
	if got := storeCount(t, st, `SELECT count(*) FROM cursors WHERE harness=?`, model.ClaudeCode); got != 1 {
		t.Errorf("cursors after empty reset=%d, want 1", got)
	}
	if got, err := st.HarnessParserVersion(ctx, model.ClaudeCode); err != nil || got != 3 {
		t.Fatalf("version after empty reset=%d err=%v, want 3", got, err)
	}

	// A scope naming a source that no longer has rows is harmless and
	// idempotent, so a retried reset cannot fail.
	missing := store.ResetScope{Paths: []string{"/logs/missing.jsonl"}, Sessions: []string{"missing"}}
	if err := st.ResetHarness(ctx, model.ClaudeCode, 4, missing); err != nil {
		t.Fatal(err)
	}
	if err := st.ResetHarness(ctx, model.ClaudeCode, 4, missing); err != nil {
		t.Fatal(err)
	}
	if got := storeCount(t, st, `SELECT count(*) FROM event_data WHERE harness=?`, model.ClaudeCode); got != 1 {
		t.Errorf("events after missing scope=%d, want 1", got)
	}

	// Adopting a marker must not touch the rows of a database that predates
	// parser versions: the rows were written by the initial parser.
	if err := st.RecordHarnessParserVersion(ctx, model.Codex, 1); err != nil {
		t.Fatal(err)
	}
	if got := storeCount(t, st, `SELECT count(*) FROM event_data WHERE harness=?`, model.Codex); got != 1 {
		t.Errorf("codex events after adoption=%d, want 1", got)
	}
	if got, err := st.HarnessParserVersion(ctx, model.Codex); err != nil || got != 1 {
		t.Fatalf("adopted version=%d err=%v, want 1", got, err)
	}
	if err := st.RecordHarnessParserVersion(ctx, "", 1); err == nil {
		t.Error("adoption accepted empty harness")
	}
	if err := st.RecordHarnessParserVersion(ctx, model.Codex, 0); err == nil {
		t.Error("adoption accepted zero parser version")
	}

	if err := st.ResetHarness(ctx, "", 2, store.ResetScope{}); err == nil {
		t.Error("empty harness accepted")
	}
	if err := st.ResetHarness(ctx, model.Codex, 0, store.ResetScope{}); err == nil {
		t.Error("zero parser version accepted")
	}
	if got := storeCount(t, st, `SELECT count(*) FROM event_data`); got != 2 {
		t.Fatalf("events after rejected resets=%d, want 2", got)
	}
}
