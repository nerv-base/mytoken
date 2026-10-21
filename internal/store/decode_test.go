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

// The measured decode window is a fact like any other: it round-trips, and a
// log that rewrites a message while it streams ends with the last copy's
// window, which is the one covering the whole generation.
func TestDecodeMsRoundTrips(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	session := model.SessionMeta{Harness: model.DSH, SessionID: "s1", StartedAt: now, UpdatedAt: now}
	event := func(decodeMS int64) model.UsageEvent {
		return model.UsageEvent{
			Harness: model.DSH, DedupKey: "msg-1", SessionID: "s1",
			Timestamp: now, Model: "deepseek-flash", Provider: "deepseek-official",
			Tokens: model.Tokens{Input: 100, Output: 50}, DecodeMS: decodeMS,
		}
	}

	// A message rewritten mid-stream: the window grows with the stream.
	for _, window := range []int64{1000, 3990} {
		batch := harness.Batch{
			Events:   []model.UsageEvent{event(window)},
			Sessions: []model.SessionMeta{session},
		}
		if err = st.Commit(ctx, model.DSH, "test.jsonl", batch, nil); err != nil {
			t.Fatal(err)
		}
		var decodeMS int64
		if err = st.DB().QueryRow(`SELECT decode_ms FROM events`).Scan(&decodeMS); err != nil {
			t.Fatal(err)
		}
		if decodeMS != window {
			t.Fatalf("decode_ms = %d after committing %d", decodeMS, window)
		}
	}

	// A log that does not time its requests keeps the zero default, which
	// readers report as "no window" rather than as a measurement.
	batch := harness.Batch{
		Events: []model.UsageEvent{{
			Harness: model.DSH, DedupKey: "msg-2", SessionID: "s1",
			Timestamp: now.Add(time.Second), Model: "deepseek-flash", Provider: "deepseek-official",
			Tokens: model.Tokens{Input: 10, Output: 5},
		}},
		Sessions: []model.SessionMeta{session},
	}
	if err = st.Commit(ctx, model.DSH, "test.jsonl", batch, nil); err != nil {
		t.Fatal(err)
	}
	var decodeMS int64
	if err = st.DB().QueryRow(`SELECT decode_ms FROM events WHERE dedup_key='msg-2'`).Scan(&decodeMS); err != nil {
		t.Fatal(err)
	}
	if decodeMS != 0 {
		t.Fatalf("decode_ms = %d, want 0 for a log that carries no window", decodeMS)
	}
	// Token accounting is untouched by the column.
	var in, out, reasoning int64
	if err = st.DB().QueryRow(`SELECT sum(input),sum(output),sum(reasoning) FROM events`).Scan(&in, &out, &reasoning); err != nil {
		t.Fatal(err)
	}
	if in != 110 || out != 55 || reasoning != 0 {
		t.Fatalf("totals = %d/%d/%d, want 110/55/0", in, out, reasoning)
	}
}
