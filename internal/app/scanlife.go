package app

// This file owns the scan lifecycle: which scanner instance is current, how a
// sources.json edit swaps it for one over the new instances, and how the
// watcher is stopped. The public Scanner field is the instance Open built and
// never changes, so a CLI command that reads it directly stays valid; the GUI
// reads the accessors here, which follow a reload.
//
// A reload never restarts the process: the running watcher is cancelled and
// joined, and a scanner over the new instances takes its place. Per-source
// state (cursors, dedup keys, history) lives in the store keyed by harness,
// instance id and path, so the replacement resumes exactly where the old
// scanner stopped instead of re-reading history.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/ingestrate"
	"github.com/zzstar101/mytoken/internal/instance"
	"github.com/zzstar101/mytoken/internal/paths"
	"github.com/zzstar101/mytoken/internal/scan"
)

// ErrNoScanner reports an app with no scanner at all. A successfully opened
// app always has one.
var ErrNoScanner = errors.New("app: no scanner is available")

// ErrClosed reports an app that has been closed. Its store, resolver and
// scanner are gone, so there is nothing left to replace: a reload after Close
// must not build a scanner over a closed store, install it, or start a watcher
// on it.
var ErrClosed = errors.New("app: closed")

// CurrentScanner returns the scanner the app is scanning with right now: the
// one Open built, or the one a successful ReloadSources installed. It is nil
// only for a zero App.
func (a *App) CurrentScanner() *scan.Scanner {
	if a == nil {
		return nil
	}
	a.scanMu.Lock()
	defer a.scanMu.Unlock()
	return a.scanner
}

// LiveSources returns the resolution the current scanner was built from, as a
// copy: Open's resolution until a source reload succeeds, the reload's
// afterwards. It is what this process actually scans, which is not necessarily
// what sources.json holds now.
func (a *App) LiveSources() []Source {
	if a == nil {
		return nil
	}
	a.scanMu.Lock()
	defer a.scanMu.Unlock()
	out := make([]Source, len(a.sources))
	copy(out, a.sources)
	return out
}

// LiveSourceProblems returns the non-fatal problems of the current
// resolution, as a copy.
func (a *App) LiveSourceProblems() []instance.Problem {
	if a == nil {
		return nil
	}
	a.scanMu.Lock()
	defer a.scanMu.Unlock()
	out := make([]instance.Problem, len(a.sourceProbs))
	copy(out, a.sourceProbs)
	return out
}

// ScanError returns the runtime failure that stopped the watcher, if any. It
// is cleared when a new watcher starts. A failure is never papered over by
// falling back to another source set: scanning stops and the reason is
// reported.
func (a *App) ScanError() error {
	if a == nil {
		return nil
	}
	a.scanMu.Lock()
	defer a.scanMu.Unlock()
	return a.scanFailed
}

// StartScanning starts the current scanner's watcher and returns immediately.
// The watcher runs until ctx is cancelled, Close is called, or a source reload
// replaces it. A runtime failure is recorded (ScanError) instead of discarded.
//
// It shares reloadMu with ReloadSources and Close, so a start can never
// interleave with a swap: whichever runs first completes, and the next one
// observes its result instead of racing it.
func (a *App) StartScanning(ctx context.Context) {
	if a == nil {
		return
	}
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	if a.closed {
		return
	}
	a.scanMu.Lock()
	defer a.scanMu.Unlock()
	a.scanWanted = true
	a.startWatcherLocked(ctx)
}

// startWatcherLocked launches the watcher goroutine unless one is already
// alive. Callers hold scanMu.
func (a *App) startWatcherLocked(ctx context.Context) {
	if a.closed || a.scanner == nil || a.scanRunning {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	sc := a.scanner
	a.scanRunning, a.scanDone, a.scanCancel, a.scanFailed = true, done, cancel, nil
	go func() {
		err := sc.Run(runCtx)
		a.scanMu.Lock()
		// The goroutine is gone: a later reload may start a replacement, and
		// stopScan must not wait on a channel nobody will close.
		a.scanRunning, a.scanDone, a.scanCancel = false, nil, nil
		if err != nil && runCtx.Err() == nil {
			// A real runtime failure (a watcher that cannot be created, a
			// store that went away). The watcher has stopped; record why, and
			// never fall back to scanning something else.
			a.scanFailed = fmt.Errorf("scan: %w", err)
		}
		a.scanMu.Unlock()
		close(done)
	}()
}

// stopScan stops the watcher and waits for it to return. It is idempotent, and
// it is the only way to make Close wait for the scanner before the store it
// scans through is closed.
func (a *App) stopScan() {
	if a == nil {
		return
	}
	a.scanMu.Lock()
	a.scanWanted = false
	cancel, done := a.scanCancel, a.scanDone
	a.scanMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// ReloadSources re-resolves sources.json and, when the new configuration
// resolves, replaces the running scanner with one over the new instances.
//
// Everything that can fail happens before anything is stopped: the
// configuration is loaded and resolved, and the replacement scanner is built,
// while the old watcher keeps running. Only then is the old watcher cancelled
// and joined, and the replacement started — so two watchers never scan the
// same sources concurrently, and a configuration that cannot be honoured
// leaves the running scan exactly as it was (never a silent fallback to the
// previous or to default discovery).
//
// Disabled and backup-copy entries resolve but are never scanned, so they
// claim their harness without being handed to the scanner.
//
// An app that was never opened has no scanner, store or resolver and therefore
// nothing running to replace: the edit simply applies to the next Open, and
// that is not an error.
//
// One reload runs at a time, for its whole resolve-swap-join-start sequence
// (reloadMu). Two overlapping reloads would each resolve and build their own
// replacement, then race over the single watcher slot: one could cancel the
// other's watcher, and one could install a scanner that the other's
// already-running watcher never scans. Serializing them means the second
// reload sees the first one's result and replaces it in turn — exactly as if
// the two edits had been applied one after the other.
func (a *App) ReloadSources(ctx context.Context) error {
	if a == nil {
		return ErrNoScanner
	}
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	if a.closed {
		// Close owns the scanner, the store and the watcher now; building a
		// replacement here would resurrect a scan the closed store cannot
		// serve.
		return ErrClosed
	}
	if a.Store == nil || a.Resolver == nil || a.CurrentScanner() == nil {
		return nil
	}
	dir, err := paths.DataDir()
	if err != nil {
		return err
	}
	cfg, err := instance.Load(dir)
	if err != nil {
		return err
	}
	resolved, err := resolveSources(cfg, instance.DefaultFallbacks(), harness.NewRooted)
	if err != nil {
		return err
	}
	// Build the replacement first: a nil parser or a duplicate instance id
	// must fail here, with the old scanner still running.
	sc, err := scan.NewWithInstances(a.Store, a.Resolver, a.Pricing, resolved.targets...)
	if err != nil {
		return err
	}

	a.scanMu.Lock()
	cancel, done, wanted := a.scanCancel, a.scanDone, a.scanWanted
	a.scanner, a.sources, a.sourceProbs = sc, resolved.sources, resolved.problems
	a.scanMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}

	// The credential sources follow the instances they belong to: a consent
	// is bound to one exact source reference, so a path that moved stops
	// resolving and that instance fails closed instead of reading the new
	// path under the old grant.
	a.reloadClaudeQuotaSources(resolved.sources)

	if !wanted {
		return nil
	}
	a.scanMu.Lock()
	a.startWatcherLocked(ctx)
	a.scanMu.Unlock()
	return nil
}

// ScannerProgress reports the current scanner's progress, so a GUI binding
// reads the instance that is running rather than the one Open built.
func (a *App) ScannerProgress() (done, total int) {
	if sc := a.CurrentScanner(); sc != nil {
		return sc.Progress()
	}
	return 0, 0
}

// ScannerIngestRate reports the current scanner's live ingest telemetry.
func (a *App) ScannerIngestRate(now time.Time) ingestrate.Snapshot {
	if sc := a.CurrentScanner(); sc != nil {
		return sc.IngestRate(now)
	}
	return ingestrate.Snapshot{}
}

// ScannerRebuild rebuilds the current scanner's derived index and rescans.
func (a *App) ScannerRebuild(ctx context.Context) error {
	sc := a.CurrentScanner()
	if sc == nil {
		return ErrNoScanner
	}
	return sc.Rebuild(ctx)
}
