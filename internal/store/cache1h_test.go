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

// The 1-hour cache-write tier is a subset of CacheWrite: it must round-trip
// through ingestion without changing the 5-minute remainder.
func TestCacheWrite1hRoundTrips(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC().Truncate(time.Second)
	batch := harness.Batch{
		Events: []model.UsageEvent{{
			Harness: model.ClaudeCode, DedupKey: "req-1", SessionID: "s1",
			Timestamp: now, Model: "claude-opus-5-5", Provider: "anthropic",
			Tokens: model.Tokens{Input: 10, Output: 20, CacheRead: 30, CacheWrite: 100, CacheWrite1h: 40},
		}},
		Sessions: []model.SessionMeta{{Harness: model.ClaudeCode, SessionID: "s1", StartedAt: now, UpdatedAt: now}},
	}
	if err = st.Commit(context.Background(), model.ClaudeCode, "test.jsonl", batch, nil); err != nil {
		t.Fatal(err)
	}
	var cacheWrite, cacheWrite1h int64
	if err = st.DB().QueryRow(`SELECT cache_write,cache_write_1h FROM events`).Scan(&cacheWrite, &cacheWrite1h); err != nil {
		t.Fatal(err)
	}
	if cacheWrite != 100 || cacheWrite1h != 40 {
		t.Fatalf("cache write = %d/%d, want 100/40", cacheWrite, cacheWrite1h)
	}
}
