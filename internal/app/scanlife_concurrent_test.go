package app

// Tests for the scan lifecycle when its operations overlap: two source reloads
// at once, and a reload racing Close. The helpers these reuse (sandbox,
// claudeRoot, addClaudeEntry, updateEntry, liveSource, scannedIDs, totals) live
// in scanlife_test.go and sources_test.go and build the same throwaway
// HOME/MYTOKEN_HOME sandbox with synthetic Claude JSONL: no real log, database,
// credential or network is touched.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/instance"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

// TestConcurrentReloadsKeepOneWatcher: overlapping source reloads must be
// serialized. Each reload resolves, builds its replacement, swaps it in and
// then cancels and joins the watcher it found before starting one for its own
// scanner; if two ran through that sequence at once, one could cancel the
// other's watcher, install a scanner nothing scans, or wait for a watcher that
// has already been replaced. Afterwards exactly one watcher runs, the live
// resolution is the one the running scanner was built from, and the store's
// history, cursors and credential consent are untouched.
func TestConcurrentReloadsKeepOneWatcher(t *testing.T) {
	_, data := sandbox(t)
	rootA := claudeRoot(t, "cfg-a")
	rootB := claudeRoot(t, "cfg-b")
	writeClaudeSession(t, rootA, "proj", "s1", 100, 50)
	writeClaudeSession(t, rootB, "proj", "s2", 7, 3)
	id := addClaudeEntry(t, data, "cfg-a", rootA, "")

	a, err := OpenLocal()
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	defer a.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Seed history from the old path before the watcher starts, so what the
	// reloads keep is deterministic.
	if err := a.CurrentScanner().Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	before := totals(t, a, query.Filter{}).Requests
	if before != 1 {
		t.Fatalf("requests before the reloads = %d, want 1", before)
	}

	a.StartScanning(ctx)
	a.scanMu.Lock()
	first := a.scanDone
	a.scanMu.Unlock()
	if first == nil {
		t.Fatal("StartScanning did not start a watcher")
	}

	updateEntry(t, data, id, func(e *instance.Entry) { e.Roots = []string{rootB} })

	// Fire the reloads together: the point is the interleaving, not the order.
	const reloads = 4
	errs := make([]error, reloads)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < reloads; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = a.ReloadSources(ctx)
		}(i)
	}
	close(start)
	returned := make(chan struct{})
	go func() { wg.Wait(); close(returned) }()
	select {
	case <-returned:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent reloads deadlocked")
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("reload %d: %v", i, err)
		}
	}

	a.scanMu.Lock()
	running, second, failed := a.scanRunning, a.scanDone, a.scanFailed
	a.scanMu.Unlock()
	if !running || second == nil {
		t.Fatalf("no watcher is running after the reloads: running=%v done=%v", running, second)
	}
	if failed != nil {
		t.Errorf("a watcher failed during the reloads: %v", failed)
	}
	if second == first {
		t.Error("the watcher registration was reused instead of replaced")
	}
	select {
	case <-first:
	default:
		t.Error("the reloads did not join the watcher they replaced")
	}

	// The installed scanner and the live resolution go together.
	if live := len(a.CurrentScanner().Instances()); live != len(scannedIDs(a)) {
		t.Errorf("scanner instances = %d, live scanned = %v", live, scannedIDs(a))
	}
	if got := scannedIDs(a); len(got) != 1 || got[0] != id {
		t.Errorf("scanned ids = %v, want [%s]", got, id)
	}
	src := liveSource(t, a, id)
	want := canonical(t, rootB)
	if len(src.Roots) != 1 || src.Roots[0] != want {
		t.Errorf("live roots = %v, want [%s]", src.Roots, want)
	}
	if !src.Scanned {
		t.Error("the moved instance is not scanned")
	}

	// The replacement watcher scans its new root once before it starts watching
	// (Scanner.Run scans, then watches), so history legitimately gains exactly
	// one row: the session under the new path. Wait for that scan to land
	// instead of racing it, then pin that the reload itself re-dated or
	// double-counted nothing.
	waitForRequests(t, a, before+1)
	if got := totals(t, a, query.Filter{}).Requests; got != before+1 {
		t.Errorf("requests after the reloads = %d, want %d", got, before+1)
	}
	if _, err := a.Store.CursorIn(ctx, model.ClaudeCode, id, want); err != nil {
		t.Errorf("CursorIn after the reloads: %v", err)
	}

	// Replacing the credential service is part of the same sequence: it must
	// still be consistent, and readable, after the reloads.
	if refs := a.ClaudeQuotaSources(id); len(refs) != 1 {
		t.Errorf("credential sources after the reloads = %+v, want exactly one", refs)
	}
	if _, err := a.ClaudeQuotaInfo(ctx, id); err != nil {
		t.Errorf("ClaudeQuotaInfo after the reloads: %v", err)
	}

	// An explicit scan of the replacement path adds nothing: the watcher's own
	// initial scan already committed that session exactly once.
	if err := a.CurrentScanner().Scan(ctx); err != nil {
		t.Fatalf("Scan after the reloads: %v", err)
	}
	if got := totals(t, a, query.Filter{}).Requests; got != before+1 {
		t.Errorf("requests after scanning the new path = %d, want %d", got, before+1)
	}
}

// waitForRequests blocks until the store reports at least want requests, so an
// assertion about what a background watcher committed does not depend on how
// quickly its goroutine got scheduled. It fails loudly rather than hanging when
// the scan never lands.
func waitForRequests(t *testing.T, a *App, want int64) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		got := totals(t, a, query.Filter{}).Requests
		if got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("requests never reached %d (still %d): the replacement watcher did not scan its new path", want, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCloseDuringAReloadInstallsNothing: Close and an in-flight reload must not
// interleave. Close waits for the reload's whole sequence, stops the watcher it
// started, and closes the store; a reload that arrives after that refuses to
// build anything, so no watcher is ever started against a closed store.
func TestCloseDuringAReloadInstallsNothing(t *testing.T) {
	for i := 0; i < 5; i++ {
		_, data := sandbox(t)
		rootA := claudeRoot(t, "cfg-a")
		rootB := claudeRoot(t, "cfg-b")
		writeClaudeSession(t, rootA, "proj", "s1", 100, 50)
		writeClaudeSession(t, rootB, "proj", "s2", 7, 3)
		id := addClaudeEntry(t, data, "cfg-a", rootA, "")

		a, err := OpenLocal()
		if err != nil {
			t.Fatalf("iteration %d: OpenLocal: %v", i, err)
		}
		ctx := context.Background()
		// A completed scan leaves the collection evidence this instance is
		// about to lose along with its store.
		if err := a.CurrentScanner().Scan(ctx); err != nil {
			t.Fatalf("iteration %d: Scan: %v", i, err)
		}
		if _, ok := a.SourceDiagnostics(ctx, model.ClaudeCode, id); !ok {
			t.Fatalf("iteration %d: a completed scan recorded no diagnostics", i)
		}
		a.StartScanning(ctx)
		updateEntry(t, data, id, func(e *instance.Entry) { e.Roots = []string{rootB} })

		reloaded := make(chan error, 1)
		closed := make(chan error, 1)
		go func() { reloaded <- a.ReloadSources(ctx) }()
		go func() { closed <- a.Close() }()

		select {
		case err := <-closed:
			if err != nil {
				t.Fatalf("iteration %d: Close: %v", i, err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("iteration %d: Close deadlocked against a reload", i)
		}
		select {
		case err := <-reloaded:
			// Either the reload finished before Close took over, or Close was
			// first and the reload refused to touch a closed app.
			if err != nil && !errors.Is(err, ErrClosed) {
				t.Errorf("iteration %d: reload racing Close: %v", i, err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("iteration %d: a reload deadlocked against Close", i)
		}

		a.scanMu.Lock()
		running, rest, wanted := a.scanRunning, a.scanDone, a.scanWanted
		a.scanMu.Unlock()
		if running || rest != nil || wanted {
			t.Fatalf("iteration %d: Close left a watcher: running=%v done=%v wanted=%v", i, running, rest, wanted)
		}

		// Nothing may start a watcher on a closed app, and the closed store
		// reports no evidence rather than zeros.
		a.StartScanning(ctx)
		if err := a.ReloadSources(ctx); !errors.Is(err, ErrClosed) {
			t.Errorf("iteration %d: ReloadSources on a closed app = %v, want ErrClosed", i, err)
		}
		a.scanMu.Lock()
		again := a.scanRunning
		a.scanMu.Unlock()
		if again {
			t.Fatalf("iteration %d: a closed app started a watcher", i)
		}
		if _, ok := a.SourceDiagnostics(ctx, model.ClaudeCode, id); ok {
			t.Errorf("iteration %d: a closed app still reports collection evidence", i)
		}
	}
}

// TestReloadStartAndCloseShareOneLock: the three operations that own the
// watcher slot must take the same lock, and the test proves it directly rather
// than hoping a race occurs. While this test holds the lock, a reload, a start
// and Close all have to wait; once it is released each finishes. A reload that
// ran through while another owned the slot could cancel the other's watcher and
// start a replacement no one joins, and a Close that ran through could tear the
// store down under a reload that is about to install a scanner.
func TestReloadStartAndCloseShareOneLock(t *testing.T) {
	_, data := sandbox(t)
	root := claudeRoot(t, "cfg-a")
	writeClaudeSession(t, root, "proj", "s1", 100, 50)
	id := addClaudeEntry(t, data, "cfg-a", root, "")

	a, err := OpenLocal()
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	ctx := context.Background()
	a.StartScanning(ctx)

	if !a.reloadMu.TryLock() {
		t.Fatal("the reload lock was already held")
	}
	reloadDone := make(chan error, 1)
	go func() { reloadDone <- a.ReloadSources(ctx) }()
	startDone := make(chan struct{})
	go func() {
		a.StartScanning(ctx)
		close(startDone)
	}()

	// Both must wait for the slot rather than run through it.
	select {
	case err := <-reloadDone:
		a.reloadMu.Unlock()
		t.Fatalf("ReloadSources returned %v while another operation owned the slot", err)
	case <-startDone:
		a.reloadMu.Unlock()
		t.Fatal("StartScanning returned while another operation owned the slot")
	case <-time.After(300 * time.Millisecond):
	}
	a.reloadMu.Unlock()

	select {
	case err := <-reloadDone:
		if err != nil {
			t.Errorf("the reload that waited: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the reload that waited never finished")
	}
	select {
	case <-startDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the start that waited never finished")
	}
	// The waiting start found the reload's watcher already running, so the
	// live scanner still scans the entry and holds its history.
	if got := scannedIDs(a); len(got) != 1 || got[0] != id {
		t.Fatalf("scanned instances after the overlap = %v, want [%s]", got, id)
	}

	// Close owns the same slot.
	if !a.reloadMu.TryLock() {
		t.Fatal("the reload lock was already held before Close")
	}
	closed := make(chan struct{})
	go func() {
		_ = a.Close()
		close(closed)
	}()
	select {
	case <-closed:
		a.reloadMu.Unlock()
		t.Fatal("Close returned while another operation owned the slot")
	case <-time.After(300 * time.Millisecond):
	}
	a.reloadMu.Unlock()

	select {
	case <-closed:
	case <-time.After(30 * time.Second):
		t.Fatal("Close never finished once it had the slot")
	}
	if !a.closed {
		t.Error("Close did not mark the app closed")
	}
	if err := a.ReloadSources(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("reload after Close = %v, want ErrClosed", err)
	}
}
