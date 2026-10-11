package zed

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
)

// writeThreads builds a threads.db from raw thread documents (plain JSON rows).
func writeThreads(t *testing.T, docs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "threads.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(threadsDDL); err != nil {
		t.Fatalf("ddl: %v", err)
	}
	for id, doc := range docs {
		if _, err := db.Exec(
			`INSERT INTO threads (id, summary, updated_at, data_type, data, parent_id, folder_paths, created_at)
			 VALUES (?, ?, ?, 'json', ?, '', '', ?)`,
			id, "summary of "+id, "2025-03-19T16:00:00Z", []byte(doc), "2025-03-19T16:00:00Z",
		); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	return dir
}

func parseAll(t *testing.T, dir string) (harness.Batch, map[string]harness.Cursor) {
	t.Helper()
	p := NewWithRoots(dir)
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var out harness.Batch
	next := map[string]harness.Cursor{}
	for _, src := range srcs {
		b, err := p.Parse(context.Background(), src, harness.Cursor{})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		out.Events = append(out.Events, b.Events...)
		out.Sessions = append(out.Sessions, b.Sessions...)
		next[src.Path] = b.Next
	}
	return out, next
}

// A thread whose per-request map is empty reports its whole cumulative usage once,
// under the fixed remainder key.
func TestCumulativeRemainderTopUp(t *testing.T) {
	dir := writeThreads(t, map[string]string{
		"t-rem": `{
			"model": {"provider": "anthropic", "model": "claude-sonnet-4-6"},
			"request_token_usage": {"req-1": {"input_tokens": 0, "output_tokens": 0}},
			"cumulative_token_usage": {"input_tokens": 5400, "output_tokens": 900, "cache_read_input_tokens": 20000}
		}`,
	})
	batch, _ := parseAll(t, dir)
	if len(batch.Events) != 1 {
		t.Fatalf("events = %d, want 1: %+v", len(batch.Events), batch.Events)
	}
	ev := batch.Events[0]
	if want := "zed:t-rem:" + remainderKey; ev.DedupKey != want {
		t.Errorf("DedupKey = %q, want %q", ev.DedupKey, want)
	}
	if want := (struct{ i, o, cr, cw, r int64 }{5400, 900, 20000, 0, 0}); (struct{ i, o, cr, cw, r int64 }{
		ev.Tokens.Input, ev.Tokens.Output, ev.Tokens.CacheRead, ev.Tokens.CacheWrite, ev.Tokens.Reasoning,
	}) != want {
		t.Errorf("tokens = %+v, want input 5400 output 900 cacheRead 20000", ev.Tokens)
	}
}

// The per-request keys are user-message ids: they must not appear verbatim in a
// dedup key, and the key must be stable across parses.
func TestRequestKeysAreHashed(t *testing.T) {
	const secret = "user message id that must not leak"
	dir := writeThreads(t, map[string]string{
		"t-hash": `{
			"model": {"model": "gpt-5"},
			"request_token_usage": {"` + secret + `": {"input_tokens": 10, "output_tokens": 1}}
		}`,
	})
	first, _ := parseAll(t, dir)
	if len(first.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(first.Events))
	}
	key := first.Events[0].DedupKey
	if strings.Contains(key, secret) {
		t.Fatalf("DedupKey %q leaks the request key", key)
	}
	second, _ := parseAll(t, dir)
	if len(second.Events) != 1 || second.Events[0].DedupKey != key {
		t.Errorf("DedupKey is not stable: %q vs %q", key, second.Events[0].DedupKey)
	}
}

// Rows with an unknown data_type are skipped without disturbing healthy rows, and
// a second parse from the returned cursor is quiet.
func TestSkipsUnknownEncodingAndStaysQuiet(t *testing.T) {
	dir := writeThreads(t, map[string]string{
		"t-ok":  `{"model": {"model": "gpt-5"}, "request_token_usage": {"a": {"input_tokens": 5, "output_tokens": 2}}}`,
		"t-bad": `not json at all`,
	})
	db, err := sql.Open("sqlite", filepath.Join(dir, "threads.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`UPDATE threads SET data_type = 'protobuf' WHERE id = 't-bad'`); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := db.Exec(`UPDATE threads SET data = ? WHERE id = 't-ok'`, []byte(`{`)); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	batch, _ := parseAll(t, dir)
	if len(batch.Events) != 0 {
		t.Fatalf("events = %d, want 0 (broken rows are skipped): %+v", len(batch.Events), batch.Events)
	}
}

// discover-missing: a root that does not exist yields no sources and no error.
func TestDiscoverMissingRoot(t *testing.T) {
	p := NewWithRoots(filepath.Join(t.TempDir(), "nope"))
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 0 {
		t.Fatalf("sources = %+v, want none", srcs)
	}
}

// The cursor handed back on an unchanged database is returned untouched.
func TestUnchangedSourceKeepsCursor(t *testing.T) {
	dir := writeThreads(t, map[string]string{
		"t-cursor": `{"model": {"model": "gpt-5"}, "request_token_usage": {"a": {"input_tokens": 5, "output_tokens": 2}}}`,
	})
	p := NewWithRoots(dir)
	srcs, err := p.Discover(context.Background())
	if err != nil || len(srcs) != 1 {
		t.Fatalf("Discover: %v (%d sources)", err, len(srcs))
	}
	first, err := p.Parse(context.Background(), srcs[0], harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	second, err := p.Parse(context.Background(), srcs[0], first.Next)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(second.Events) != 0 || len(second.Sessions) != 0 {
		t.Fatalf("unchanged source re-emitted %d events / %d sessions", len(second.Events), len(second.Sessions))
	}
	if second.Next != first.Next {
		t.Errorf("cursor moved on an unchanged source:\n was %+v\n got %+v", first.Next, second.Next)
	}
}
