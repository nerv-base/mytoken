package scan

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

// replayParser serves the same records on every scan whatever its cursor says,
// the way a source that re-reads already-indexed history does: a full rescan,
// a backfill replay, or a parser that ignores its checkpoint. It never reports
// a session, so the tests count only the usage events diagnostics claim.
type replayParser struct {
	harness model.Harness
	path    string
	events  func() []model.UsageEvent
	// before runs inside Parse, so a test can act at the moment a parse has
	// produced its batch but the commit has not started.
	before func()
}

func (p replayParser) Harness() model.Harness { return p.harness }
func (p replayParser) Roots() []string        { return []string{filepath.Dir(p.path)} }
func (p replayParser) Discover(context.Context) ([]harness.Source, error) {
	return []harness.Source{{Path: p.path, Kind: "jsonl"}}, nil
}
func (p replayParser) Parse(context.Context, harness.Source, harness.Cursor) (harness.Batch, error) {
	if p.before != nil {
		p.before()
	}
	return harness.Batch{Next: harness.Cursor{Offset: 1, Size: 1}, Events: p.events()}, nil
}

func fixtureEvent(key string, tokens model.Tokens) model.UsageEvent {
	return model.UsageEvent{
		DedupKey:  key,
		SessionID: "session",
		Model:     "gpt-5",
		Timestamp: time.Unix(1700000000, 0),
		Tokens:    tokens,
	}
}

// TestDiagnosticsReplayedScanCountsNoNewEvents pins the accounting to the commit
// outcome, not to what the parser offered: a second scan that replays a record
// the index already holds collected nothing new.
func TestDiagnosticsReplayedScanCountsNoNewEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	writeLog(t, path)
	st := openStore(t)
	defer st.Close()

	sc := New(st, nil, nil, replayParser{
		harness: model.Harness("fixture"),
		path:    path,
		events: func() []model.UsageEvent {
			return []model.UsageEvent{fixtureEvent("replayed", model.Tokens{Input: 3, Output: 5})}
		},
	})
	at := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	sc.clock = func() time.Time { return at }
	ctx := context.Background()

	if err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	first, ok := loadDiag(t, st, "fixture", store.DefaultInstanceID)
	if !ok {
		t.Fatal("no record after the first scan")
	}
	if first.NewEvents != 1 {
		t.Fatalf("first scan newEvents = %d, want 1: the event was inserted", first.NewEvents)
	}

	at = at.Add(time.Hour)
	sc.clock = func() time.Time { return at }
	if err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	second, ok := loadDiag(t, st, "fixture", store.DefaultInstanceID)
	if !ok {
		t.Fatal("no record after the replay scan")
	}
	if second.NewEvents != 0 {
		t.Fatalf("replayed scan newEvents = %d, want 0: the index already held the event", second.NewEvents)
	}
	if n := countRows(t, st, "SELECT count(*) FROM event_data"); n != 1 {
		t.Fatalf("event_data holds %d rows, want 1: a replay must not add one", n)
	}
}

// TestDiagnosticsMixedBatchCountsOnlyFirstInserts proves one flush containing
// both an already-indexed record and a new one counts the new one alone.
func TestDiagnosticsMixedBatchCountsOnlyFirstInserts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	writeLog(t, path)
	st := openStore(t)
	defer st.Close()

	tokens := model.Tokens{Input: 4, Output: 6}
	var batch []model.UsageEvent
	sc := New(st, nil, nil, replayParser{
		harness: model.Harness("fixture"),
		path:    path,
		events:  func() []model.UsageEvent { return batch },
	})
	at := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	sc.clock = func() time.Time { return at }
	ctx := context.Background()

	batch = []model.UsageEvent{fixtureEvent("seen", tokens)}
	if err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if d, _ := loadDiag(t, st, "fixture", store.DefaultInstanceID); d.NewEvents != 1 {
		t.Fatalf("first scan newEvents = %d, want 1", d.NewEvents)
	}

	// One batch, one replayed key and one genuinely new key.
	at = at.Add(time.Hour)
	sc.clock = func() time.Time { return at }
	batch = []model.UsageEvent{fixtureEvent("seen", tokens), fixtureEvent("fresh", tokens)}
	if err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	d, ok := loadDiag(t, st, "fixture", store.DefaultInstanceID)
	if !ok {
		t.Fatal("no record after the mixed scan")
	}
	if d.NewEvents != 1 {
		t.Fatalf("mixed scan newEvents = %d, want 1: only the unseen key is new", d.NewEvents)
	}
	if n := countRows(t, st, "SELECT count(*) FROM event_data"); n != 2 {
		t.Fatalf("event_data holds %d rows, want 2", n)
	}
}

// TestDiagnosticsFailedCommitCountsNoNewEvents makes the event insert itself
// fail while the store stays writable, so the scan completes with a rolled-back
// commit and must record no new events.
func TestDiagnosticsFailedCommitCountsNoNewEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	writeLog(t, path)
	st := openStore(t)
	defer st.Close()

	if _, err := st.DB().Exec(`CREATE TRIGGER fixture_reject_event BEFORE INSERT ON event_data BEGIN SELECT RAISE(ABORT, 'fixture commit failure'); END`); err != nil {
		t.Fatal(err)
	}

	sc := New(st, nil, nil, replayParser{
		harness: model.Harness("fixture"),
		path:    path,
		events: func() []model.UsageEvent {
			return []model.UsageEvent{fixtureEvent("rejected", model.Tokens{Input: 1})}
		},
	})
	sc.clock = func() time.Time { return time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC) }
	if err := sc.Scan(context.Background()); err == nil {
		t.Fatal("a rejected commit did not report an error")
	}

	d, ok := loadDiag(t, st, "fixture", store.DefaultInstanceID)
	if !ok {
		t.Fatal("no record after a completed scan whose commit failed")
	}
	if d.NewEvents != 0 {
		t.Fatalf("newEvents = %d, want 0: a rolled-back commit inserts nothing", d.NewEvents)
	}
	if !d.LastSuccessAt.IsZero() {
		t.Fatalf("success = %v, want zero: the read was not clean", d.LastSuccessAt)
	}
	if n := countRows(t, st, "SELECT count(*) FROM event_data"); n != 0 {
		t.Fatalf("event_data holds %d rows, want 0", n)
	}
}

// TestDiagnosticsCancelledCommitPersistsNoRecord cancels the scan while its
// commit is in flight: the commit rolls back, its events are not new, and the
// prior record survives untouched.
func TestDiagnosticsCancelledCommitPersistsNoRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	writeLog(t, path)
	st := openStore(t)
	defer st.Close()

	p := replayParser{
		harness: model.Harness("fixture"),
		path:    path,
		events: func() []model.UsageEvent {
			return []model.UsageEvent{fixtureEvent("first", model.Tokens{Input: 2})}
		},
	}
	sc := New(st, nil, nil, p)
	at := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	sc.clock = func() time.Time { return at }
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := rawDiag(t, st, "fixture", store.DefaultInstanceID)

	// The second scan cancels its context from inside Parse, so the event it
	// produced reaches a commit that cannot start.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelSc := New(st, nil, nil, replayParser{
		harness: model.Harness("fixture"),
		path:    path,
		before:  cancel,
		events: func() []model.UsageEvent {
			return []model.UsageEvent{fixtureEvent("second", model.Tokens{Input: 7})}
		},
	})
	at = at.Add(time.Hour)
	cancelSc.clock = func() time.Time { return at }
	if err := cancelSc.Scan(ctx); err == nil {
		t.Fatal("a cancelled scan did not report an error")
	}
	if after := rawDiag(t, st, "fixture", store.DefaultInstanceID); after != before {
		t.Fatalf("a cancelled commit rewrote the record:\n before %s\n after  %s", before, after)
	}
	if n := countRows(t, st, "SELECT count(*) FROM event_data"); n != 1 {
		t.Fatalf("event_data holds %d rows, want 1: the cancelled commit inserted nothing", n)
	}
}
