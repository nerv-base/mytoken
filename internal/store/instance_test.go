package store

import (
	"context"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

// commitInstance persists one event for one explicit instance. instance=="" is
// the legacy caller shape and must land on the reserved default instance.
func commitInstance(t *testing.T, st *Store, instance, path, key, session, parent string, input int64) {
	t.Helper()
	e := model.UsageEvent{
		Harness:   model.Codex,
		DedupKey:  key,
		SessionID: session,
		ParentID:  parent,
		Model:     "m",
		Provider:  "p",
		BaseURL:   "https://api.example.com",
		Tokens:    model.Tokens{Input: input},
	}
	if err := st.CommitMany(context.Background(), []Write{{
		Harness:    model.Codex,
		InstanceID: instance,
		Path:       path,
		Batch:      harness.Batch{Events: []model.UsageEvent{e}, Next: harness.Cursor{Offset: input}},
	}}); err != nil {
		t.Fatalf("commit instance=%q path=%q key=%q: %v", instance, path, key, err)
	}
}

func count(t *testing.T, st *Store, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := st.DB().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// The same raw dedup key, session and harness in two explicitly different
// instances must be two stored facts, not one overwritten fact.
func TestInstanceSeparatesEqualDedupKeys(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	commitInstance(t, st, "alpha", "log", "same-key", "s1", "", 10)
	commitInstance(t, st, "beta", "log", "same-key", "s1", "", 10)

	if n := count(t, st, `SELECT count(*) FROM event_data`); n != 2 {
		t.Fatalf("event_data rows = %d, want 2", n)
	}
	if n := count(t, st, `SELECT count(*) FROM event_data WHERE instance_id='alpha'`); n != 1 {
		t.Fatalf("alpha rows = %d, want 1", n)
	}
	if n := count(t, st, `SELECT count(*) FROM event_data WHERE instance_id='beta'`); n != 1 {
		t.Fatalf("beta rows = %d, want 1", n)
	}
	// The stored dedup key stays verbatim: no instance prefix, no hash.
	var key string
	if err := st.DB().QueryRow(`SELECT dedup_key FROM event_data WHERE instance_id='beta'`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if key != "same-key" {
		t.Fatalf("stored dedup key = %q, want verbatim %q", key, "same-key")
	}
	// The compatibility view still exposes both facts to legacy readers.
	if n := count(t, st, `SELECT count(*) FROM events`); n != 2 {
		t.Fatalf("events view rows = %d, want 2", n)
	}
}

// Re-importing the identical fact inside one instance is still idempotent.
func TestInstanceReimportIdempotent(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i := 0; i < 3; i++ {
		commitInstance(t, st, "alpha", "log", "same-key", "s1", "", 10)
	}
	commitInstance(t, st, "beta", "log", "same-key", "s1", "", 10)
	if n := count(t, st, `SELECT count(*) FROM event_data`); n != 2 {
		t.Fatalf("event_data rows = %d, want 2 (one per instance)", n)
	}
}

// Two instances sharing one source path keep independent checkpoints.
func TestInstanceCursorsIsolated(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	commitInstance(t, st, "alpha", "same-path", "k1", "s1", "", 11)
	commitInstance(t, st, "beta", "same-path", "k2", "s2", "", 22)

	a, err := st.CursorIn(ctx, model.Codex, "alpha", "same-path")
	if err != nil || a.Offset != 11 {
		t.Fatalf("alpha cursor = %+v err=%v", a, err)
	}
	b, err := st.CursorIn(ctx, model.Codex, "beta", "same-path")
	if err != nil || b.Offset != 22 {
		t.Fatalf("beta cursor = %+v err=%v", b, err)
	}
	// No default-instance row exists yet, so the legacy accessor stays empty.
	legacy, err := st.Cursor(ctx, model.Codex, "same-path")
	if err != nil || legacy.Offset != 0 {
		t.Fatalf("legacy cursor = %+v err=%v", legacy, err)
	}
	// An empty instance maps to the reserved default for old callers.
	commitInstance(t, st, "", "same-path", "k3", "s3", "", 33)
	legacy, err = st.Cursor(ctx, model.Codex, "same-path")
	if err != nil || legacy.Offset != 33 {
		t.Fatalf("default cursor = %+v err=%v", legacy, err)
	}
	if n := count(t, st, `SELECT count(*) FROM cursors WHERE path='same-path'`); n != 3 {
		t.Fatalf("cursor rows = %d, want 3", n)
	}
}

// Sessions keep their identity per instance: a child in one instance never
// attaches to the same-named root in another.
func TestInstanceSessionsIsolated(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	commitInstance(t, st, "alpha", "log", "a-root", "root", "", 1)
	commitInstance(t, st, "alpha", "log", "a-child", "child", "root", 2)
	commitInstance(t, st, "beta", "log", "b-child", "child", "other-root", 3)

	// Three referenced sessions plus beta's own parent row: the parent id of a
	// child is registered inside the child's instance only.
	if n := count(t, st, `SELECT count(*) FROM sessions`); n != 4 {
		t.Fatalf("sessions = %d, want 4", n)
	}
	if n := count(t, st, `SELECT count(*) FROM sessions WHERE instance_id='alpha' AND parent_id='root'`); n != 1 {
		t.Fatalf("alpha child parent = %d, want 1", n)
	}
	var parent string
	if err := st.DB().QueryRow(`SELECT parent_id FROM sessions WHERE instance_id='beta' AND session_id='child'`).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	if parent != "other-root" {
		t.Fatalf("beta child parent = %q, want other-root", parent)
	}
	if n := count(t, st, `SELECT count(*) FROM sessions WHERE instance_id='beta' AND session_id='other-root' AND parent_id=''`); n != 1 {
		t.Fatalf("beta parent session = %d, want 1", n)
	}
	for _, q := range []string{
		`SELECT count(*) FROM sessions WHERE instance_id='beta' AND parent_id='root'`,
		`SELECT count(*) FROM sessions WHERE instance_id='alpha' AND session_id='other-root'`,
		`SELECT count(*) FROM sessions WHERE instance_id='beta' AND session_id='root'`,
	} {
		if n := count(t, st, q); n != 0 {
			t.Fatalf("cross-instance session leak: %s = %d", q, n)
		}
	}
}

// Every derived rollup carries the instance dimension and stays split.
func TestInstanceRollupsSeparated(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	commitInstance(t, st, "alpha", "log", "k1", "s1", "", 10)
	commitInstance(t, st, "beta", "log", "k2", "s1", "", 20)

	for _, table := range []string{"daily_usage", "cache_daily", "provider_daily", "provider_cache_daily"} {
		rows, err := st.DB().Query(`SELECT instance_id, count(*) FROM ` + table + ` GROUP BY instance_id ORDER BY instance_id`)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		var got []string
		for rows.Next() {
			var id string
			var n int64
			if err := rows.Scan(&id, &n); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			got = append(got, id)
		}
		rows.Close()
		if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
			t.Fatalf("%s instances = %v, want [alpha beta]", table, got)
		}
	}
	var input int64
	if err := st.DB().QueryRow(`SELECT sum(input) FROM daily_usage WHERE instance_id='beta'`).Scan(&input); err != nil || input != 20 {
		t.Fatalf("beta daily input = %d err=%v, want 20", input, err)
	}
}

// The daily maintenance triggers must attribute a delete to the instance that
// owns the fact, never to whichever instance matched first.
func TestInstanceMaintenanceTriggerKeepsInstance(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	commitInstance(t, st, "alpha", "log", "k1", "s1", "", 10)
	commitInstance(t, st, "beta", "log", "k2", "s1", "", 20)
	if _, err := st.DB().Exec(`DELETE FROM event_data WHERE instance_id='beta'`); err != nil {
		t.Fatal(err)
	}
	var alpha, beta int64
	if err := st.DB().QueryRow(`SELECT sum(CASE WHEN instance_id='alpha' THEN input ELSE 0 END), sum(CASE WHEN instance_id='beta' THEN input ELSE 0 END) FROM daily_usage`).Scan(&alpha, &beta); err != nil {
		t.Fatal(err)
	}
	if alpha != 10 || beta != 0 {
		t.Fatalf("daily after delete: alpha=%d beta=%d, want 10/0", alpha, beta)
	}
}

// A source move keeps the raw dedup key and the fact identity; only the cursor
// path is new.
func TestInstanceMoveRetainsDedupKey(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	commitInstance(t, st, "alpha", "old-path", "stable-key", "s1", "", 10)
	commitInstance(t, st, "alpha", "new-path", "stable-key", "s1", "", 10)
	if n := count(t, st, `SELECT count(*) FROM event_data`); n != 1 {
		t.Fatalf("event_data rows = %d, want 1", n)
	}
	var key string
	if err := st.DB().QueryRow(`SELECT dedup_key FROM event_data`).Scan(&key); err != nil || key != "stable-key" {
		t.Fatalf("dedup key = %q err=%v", key, err)
	}
	if n := count(t, st, `SELECT count(*) FROM cursors WHERE path='new-path'`); n != 1 {
		t.Fatalf("new path cursor = %d, want 1", n)
	}
}
