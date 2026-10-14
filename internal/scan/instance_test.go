package scan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

// openInstanceStore opens a scratch on-disk store so the cursor table, whose
// primary key now carries the instance id, is exercised the way the app uses it.
func openInstanceStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// recordingParser is a fake harness parser over a temporary root. It records
// every cursor its Parse calls receive, which is how these tests prove the
// scanner checkpointed each instance independently.
type recordingParser struct {
	id        model.Harness
	roots     []string
	sources   []harness.Source
	mu        sync.Mutex
	offsets   []int64
	discovers int
}

func newRecordingParser(root string) *recordingParser {
	return &recordingParser{
		id:      model.Harness("fixture"),
		roots:   []string{root},
		sources: []harness.Source{{Path: filepath.Join(root, "log"), Kind: "jsonl"}},
	}
}

func (p *recordingParser) Harness() model.Harness { return p.id }
func (p *recordingParser) Roots() []string        { return append([]string(nil), p.roots...) }
func (p *recordingParser) Discover(context.Context) ([]harness.Source, error) {
	p.mu.Lock()
	p.discovers++
	p.mu.Unlock()
	return append([]harness.Source(nil), p.sources...), nil
}
func (p *recordingParser) Parse(_ context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	p.mu.Lock()
	p.offsets = append(p.offsets, cur.Offset)
	p.mu.Unlock()
	raw, err := os.ReadFile(src.Path)
	if err != nil {
		return harness.Batch{}, err
	}
	b := harness.Batch{Next: harness.Cursor{Offset: int64(len(raw)), Size: int64(len(raw))}}
	for i := cur.Offset; i < int64(len(raw)); i++ {
		if raw[i] == '\n' {
			b.Events = append(b.Events, model.UsageEvent{DedupKey: string(raw[i-1]), SessionID: "s", Model: "gpt-5", Timestamp: time.Now(), Tokens: model.Tokens{Input: 1}})
		}
	}
	return b, nil
}
func (p *recordingParser) seenOffsets() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int64(nil), p.offsets...)
}
func (p *recordingParser) discoverCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.discovers
}

func instanceEvents(t *testing.T, st *store.Store) map[string]int {
	t.Helper()
	rows, err := st.DB().Query("SELECT instance_id, count(*) FROM events GROUP BY instance_id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			t.Fatal(err)
		}
		out[id] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func totalEvents(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow("SELECT count(*) FROM events").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func cursorRows(t *testing.T, st *store.Store, h model.Harness) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow("SELECT count(*) FROM cursors WHERE harness=?", h).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// --- constructor contract -------------------------------------------------

func TestNewWithInstancesRejectsDuplicateIDPerHarness(t *testing.T) {
	st := openInstanceStore(t)
	dir := t.TempDir()
	_, err := NewWithInstances(st, nil, nil,
		Instance{ID: "dup", Parser: testParser{dir}},
		Instance{ID: "dup", Parser: testParser{dir}},
	)
	if !errors.Is(err, ErrDuplicateInstance) {
		t.Fatalf("err=%v, want ErrDuplicateInstance", err)
	}
}

// The reserved "default" id is minted once per harness by the legacy fallback
// path, so the same id on two different harnesses is not a duplicate.
func TestNewWithInstancesAllowsSameIDAcrossHarnesses(t *testing.T) {
	st := openInstanceStore(t)
	a := newRecordingParser(t.TempDir())
	a.id = model.ClaudeCode
	b := newRecordingParser(t.TempDir())
	b.id = model.Codex
	sc, err := NewWithInstances(st, nil, nil,
		Instance{ID: store.DefaultInstanceID, Parser: a},
		Instance{ID: store.DefaultInstanceID, Parser: b},
	)
	if err != nil {
		t.Fatalf("same id on two harnesses must be allowed: %v", err)
	}
	if got := len(sc.Instances()); got != 2 {
		t.Fatalf("instances=%d, want 2", got)
	}
}

func TestNewWithInstancesNormalizesEmptyIDToDefault(t *testing.T) {
	st := openInstanceStore(t)
	dir := t.TempDir()
	sc, err := NewWithInstances(st, nil, nil, Instance{Parser: testParser{dir}})
	if err != nil {
		t.Fatal(err)
	}
	got := sc.Instances()
	if len(got) != 1 || got[0].ID != store.DefaultInstanceID {
		t.Fatalf("instances=%+v, want one %q instance", got, store.DefaultInstanceID)
	}
	_, err = NewWithInstances(st, nil, nil,
		Instance{Parser: testParser{dir}},
		Instance{ID: store.DefaultInstanceID, Parser: testParser{dir}},
	)
	if !errors.Is(err, ErrDuplicateInstance) {
		t.Fatalf("empty id must collide with the explicit default: err=%v", err)
	}
}

func TestNewWithInstancesRejectsNilParser(t *testing.T) {
	st := openInstanceStore(t)
	if _, err := NewWithInstances(st, nil, nil, Instance{ID: "x"}); !errors.Is(err, ErrNilParser) {
		t.Fatalf("err=%v, want ErrNilParser", err)
	}
}

// An explicit empty target list means no targets: unlike legacy New it must not
// fall back to every registered harness.
func TestNewWithInstancesEmptyDoesNotFallBackToRegistry(t *testing.T) {
	st := openInstanceStore(t)
	sc, err := NewWithInstances(st, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := sc.Instances(); len(got) != 0 {
		t.Fatalf("instances=%+v, want none", got)
	}
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if done, total := sc.Progress(); done != 0 || total != 0 {
		t.Fatalf("progress %d/%d, want 0/0", done, total)
	}
	if n := totalEvents(t, st); n != 0 {
		t.Fatalf("events=%d, want 0", n)
	}
}

// A parser with no roots is used exactly as given: the scanner never
// substitutes a registered parser for it.
func TestInstanceWithNoRootsNeverFallsBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	if err := os.WriteFile(path, []byte("a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	st := openInstanceStore(t)
	p := &recordingParser{id: model.Harness("fixture-rootless"), sources: []harness.Source{{Path: path, Kind: "jsonl"}}}
	sc, err := NewWithInstances(st, nil, nil, Instance{ID: "only", Parser: p})
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := p.discoverCount(); got != 1 {
		t.Fatalf("given parser's Discover ran %d times, want 1 (no registry fallback)", got)
	}
	if got := instanceEvents(t, st); !reflect.DeepEqual(got, map[string]int{"only": 1}) {
		t.Fatalf("events=%v, want only=1", got)
	}
}

func TestLegacyNewMapsParsersToDefaultInstance(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "log"), []byte("a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	st := openInstanceStore(t)
	sc := New(st, nil, nil, testParser{dir})
	got := sc.Instances()
	if len(got) != 1 || got[0].ID != store.DefaultInstanceID {
		t.Fatalf("instances=%+v, want one %q instance", got, store.DefaultInstanceID)
	}
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{store.DefaultInstanceID: 1}; !reflect.DeepEqual(instanceEvents(t, st), want) {
		t.Fatalf("events=%v, want %v", instanceEvents(t, st), want)
	}
}

// --- same path, same harness, two instance ids ----------------------------

func TestInstanceScanSamePathSameHarnessStaysIndependent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "log"), []byte("a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	st := openInstanceStore(t)
	sc, err := NewWithInstances(st, nil, nil,
		Instance{ID: "one", Parser: testParser{dir}},
		Instance{ID: "two", Parser: testParser{dir}},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"one": 1, "two": 1}
	if got := instanceEvents(t, st); !reflect.DeepEqual(got, want) {
		t.Fatalf("events=%v, want %v (identical content under two ids counts twice)", got, want)
	}
	if got := cursorRows(t, st, model.Harness("fixture")); got != 2 {
		t.Fatalf("cursor rows=%d, want 2 (one per instance)", got)
	}
	if done, total := sc.Progress(); done != 2 || total != 2 {
		t.Fatalf("progress %d/%d, want 2/2", done, total)
	}

	// Re-reading the same source for the same instance is idempotent.
	if err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if got := instanceEvents(t, st); !reflect.DeepEqual(got, want) {
		t.Fatalf("rescan events=%v, want %v", got, want)
	}
	if err := sc.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	if got := instanceEvents(t, st); !reflect.DeepEqual(got, want) {
		t.Fatalf("rebuild events=%v, want %v", got, want)
	}
}

// One instance's checkpoint must never be handed to another instance that
// reads the same path.
func TestInstanceCursorIsNotShared(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "log"), []byte("a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	st := openInstanceStore(t)
	ctx := context.Background()

	first := newRecordingParser(dir)
	sc1, err := NewWithInstances(st, nil, nil, Instance{ID: "A", Parser: first})
	if err != nil {
		t.Fatal(err)
	}
	if err := sc1.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if got := first.seenOffsets(); !reflect.DeepEqual(got, []int64{0}) {
		t.Fatalf("A's first parse offsets=%v, want [0]", got)
	}

	a := newRecordingParser(dir)
	b := newRecordingParser(dir)
	sc2, err := NewWithInstances(st, nil, nil,
		Instance{ID: "A", Parser: a},
		Instance{ID: "B", Parser: b},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := sc2.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if got := a.seenOffsets(); !reflect.DeepEqual(got, []int64{2}) {
		t.Fatalf("A resumed at %v, want [2]", got)
	}
	if got := b.seenOffsets(); !reflect.DeepEqual(got, []int64{0}) {
		t.Fatalf("B started at %v, want [0] (no cross-instance cursor reuse)", got)
	}
	if want := map[string]int{"A": 1, "B": 1}; !reflect.DeepEqual(instanceEvents(t, st), want) {
		t.Fatalf("events=%v, want %v", instanceEvents(t, st), want)
	}
}

// A move keeps the instance id but changes its path: the new path must start
// from scratch, the old path's cursor must not leak to the next reader, and the
// moved instance's historical events stay.
func TestMovedInstanceDoesNotReuseOldPathCursor(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(dirA, "log"), []byte("a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "log"), []byte("bb\n"), 0600); err != nil {
		t.Fatal(err)
	}
	st := openInstanceStore(t)
	ctx := context.Background()

	first := newRecordingParser(dirA)
	sc1, err := NewWithInstances(st, nil, nil, Instance{ID: "A", Parser: first})
	if err != nil {
		t.Fatal(err)
	}
	if err := sc1.Scan(ctx); err != nil {
		t.Fatal(err)
	}

	moved := newRecordingParser(dirB)
	sc2, err := NewWithInstances(st, nil, nil, Instance{ID: "A", Parser: moved})
	if err != nil {
		t.Fatal(err)
	}
	if err := sc2.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if got := moved.seenOffsets(); !reflect.DeepEqual(got, []int64{0}) {
		t.Fatalf("moved A started at %v, want [0] (old path cursor must not carry over)", got)
	}

	// A different instance taking the old path starts from scratch too.
	other := newRecordingParser(dirA)
	sc3, err := NewWithInstances(st, nil, nil, Instance{ID: "B", Parser: other})
	if err != nil {
		t.Fatal(err)
	}
	if err := sc3.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if got := other.seenOffsets(); !reflect.DeepEqual(got, []int64{0}) {
		t.Fatalf("B on the old path started at %v, want [0]", got)
	}
	if want := map[string]int{"A": 2, "B": 1}; !reflect.DeepEqual(instanceEvents(t, st), want) {
		t.Fatalf("events=%v, want %v", instanceEvents(t, st), want)
	}
}

// The watched-change "seen" map is keyed by harness+instance+path, so two
// instances over one path skip and parse independently.
func TestQuickScanSeenStateIsPerInstance(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "log"), []byte("a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	st := openInstanceStore(t)
	sc, err := NewWithInstances(st, nil, nil,
		Instance{ID: "one", Parser: testParser{dir}},
		Instance{ID: "two", Parser: testParser{dir}},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	parses := func() int { sc.mu.Lock(); defer sc.mu.Unlock(); return sc.parses }
	step := func(quick bool, want int, why string) {
		t.Helper()
		before := parses()
		if err := sc.scan(ctx, quick); err != nil {
			t.Fatal(err)
		}
		if got := parses() - before; got != want {
			t.Fatalf("%s: %d parses, want %d", why, got, want)
		}
	}
	step(false, 2, "first scan parses both instances")
	step(true, 2, "both remember nothing new")
	step(true, 0, "both skipped while unchanged")

	sc.mu.Lock()
	keys := make([]string, 0, len(sc.seen))
	for key := range sc.seen {
		keys = append(keys, key)
	}
	sc.mu.Unlock()
	if len(keys) != 2 {
		t.Fatalf("seen keys=%v, want one per instance", keys)
	}
	for _, want := range []string{"\x00one\x00", "\x00two\x00"} {
		found := false
		for _, key := range keys {
			if strings.Contains(key, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("seen keys=%v, missing instance segment %q", keys, want)
		}
	}
}

// --- watching -------------------------------------------------------------

// waitWatching starts Run and blocks until the scanner has watched directories.
func waitWatching(t *testing.T, sc *Scanner) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		sc.mu.Lock()
		watching := sc.watching
		ready := sc.dirs != nil
		sc.mu.Unlock()
		if ready && watching > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Run did not start watching")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// runWatching starts Run, waits for watches, then cancels and waits for return.
func runWatching(t *testing.T, sc *Scanner) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sc.Run(ctx) }()
	waitWatching(t, sc)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run=%v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func watchedCount(t *testing.T, sc *Scanner) int {
	t.Helper()
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.watching
}

// Two instances over one directory share one OS watch; distinct roots get one
// watch each.
func TestWatchDirsSharedAcrossInstances(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "log"), []byte("a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "log"), []byte("a\n"), 0600); err != nil {
		t.Fatal(err)
	}

	one := New(openInstanceStore(t), nil, nil, testParser{root})
	one.interval = time.Hour
	runWatching(t, one)
	if got := watchedCount(t, one); got != 1 {
		t.Fatalf("one instance watches %d dirs, want 1", got)
	}

	two, err := NewWithInstances(openInstanceStore(t), nil, nil,
		Instance{ID: "one", Parser: testParser{root}},
		Instance{ID: "two", Parser: testParser{root}},
	)
	if err != nil {
		t.Fatal(err)
	}
	two.interval = time.Hour
	runWatching(t, two)
	if got := watchedCount(t, two); got != 1 {
		t.Fatalf("two instances over one root watch %d dirs, want 1", got)
	}

	split, err := NewWithInstances(openInstanceStore(t), nil, nil,
		Instance{ID: "one", Parser: testParser{root}},
		Instance{ID: "two", Parser: testParser{other}},
	)
	if err != nil {
		t.Fatal(err)
	}
	split.interval = time.Hour
	runWatching(t, split)
	if got := watchedCount(t, split); got != 2 {
		t.Fatalf("two distinct roots watch %d dirs, want 2", got)
	}
}

// A change under a shared watch is dispatched to every instance, each advancing
// its own cursor.
func TestWatchDispatchScansEveryInstance(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	if err := os.WriteFile(path, []byte("a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	st := openInstanceStore(t)
	sc, err := NewWithInstances(st, nil, nil,
		Instance{ID: "one", Parser: testParser{dir}},
		Instance{ID: "two", Parser: testParser{dir}},
	)
	if err != nil {
		t.Fatal(err)
	}
	sc.interval = time.Hour
	sc.debounce = 20 * time.Millisecond
	sc.minGap = 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sc.Run(ctx) }()
	waitWatching(t, sc)
	waitForEvents(t, st, 2)

	appendLine(t, path, "b\n")
	waitForEvents(t, st, 4)

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run=%v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if want := map[string]int{"one": 2, "two": 2}; !reflect.DeepEqual(instanceEvents(t, st), want) {
		t.Fatalf("events=%v, want %v", instanceEvents(t, st), want)
	}
}

func waitForEvents(t *testing.T, st *store.Store, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := totalEvents(t, st); got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("events=%d, want %d", totalEvents(t, st), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Cancellation returns promptly, and a restarted Run re-establishes its watches
// on the same Scanner (Run closes its watcher on every return).
func TestRunCancellationAndRestartWithInstances(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "log"), []byte("a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sc, err := NewWithInstances(openInstanceStore(t), nil, nil,
		Instance{ID: "one", Parser: testParser{dir}},
		Instance{ID: "two", Parser: testParser{dir}},
	)
	if err != nil {
		t.Fatal(err)
	}
	sc.interval = time.Hour
	runWatching(t, sc)
	if got := watchedCount(t, sc); got != 1 {
		t.Fatalf("watches=%d, want 1", got)
	}

	sc.mu.Lock()
	sc.watching = 0
	sc.mu.Unlock()
	runWatching(t, sc)
	if got := watchedCount(t, sc); got != 1 {
		t.Fatalf("restarted Run watches=%d, want 1", got)
	}
}
