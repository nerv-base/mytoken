package app

// Tests for the scan lifecycle: a sources.json edit replaces the running
// scanner without a process restart, keeping the resolution's instance ids and
// the per-source cursors that live in the store.
//
// Every test runs inside the throwaway HOME/MYTOKEN_HOME sandbox and scans only
// synthetic Claude JSONL written into temp directories, so no real log,
// database, credential or network is touched.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/instance"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/store"
)

// claudeRoot makes a Claude Code instance root: a directory named "projects",
// which is the shape the credential-source mapping keys on.
func claudeRoot(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name, "projects")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// canonical resolves the symlinks t.TempDir can carry (macOS /var is a symlink
// to /private/var) so a path compares equal to the canonical roots
// instance.Resolve reports and the scanner records.
func canonical(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", path, err)
	}
	return resolved
}

// addClaudeEntry appends one explicit claude-code entry and returns its id.
func addClaudeEntry(t *testing.T, data, label, root string, kind instance.Kind) string {
	t.Helper()
	var id string
	if _, err := instance.Update(data, func(cfg *instance.Config) error {
		e, err := cfg.Add(instance.AddRequest{Label: label, Harness: model.ClaudeCode, Roots: []string{root}, Kind: kind})
		id = e.ID
		return err
	}); err != nil {
		t.Fatalf("add entry %s: %v", label, err)
	}
	if id == "" {
		t.Fatal("Add generated no id")
	}
	return id
}

// updateEntry edits one entry in place, keeping its id: what the source
// editor's Move, disable and enable all do.
func updateEntry(t *testing.T, data, id string, mutate func(*instance.Entry)) {
	t.Helper()
	if _, err := instance.Update(data, func(cfg *instance.Config) error {
		for i := range cfg.Entries {
			if cfg.Entries[i].ID == id {
				mutate(&cfg.Entries[i])
				return nil
			}
		}
		return errors.New("entry not found: " + id)
	}); err != nil {
		t.Fatalf("update entry %s: %v", id, err)
	}
}

// liveSource returns the current resolution's entry for id.
func liveSource(t *testing.T, a *App, id string) Source {
	t.Helper()
	for _, s := range a.LiveSources() {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no live source with id %q in %+v", id, a.LiveSources())
	return Source{}
}

func scannedIDs(a *App) []string {
	var out []string
	for _, s := range a.LiveSources() {
		if s.Scanned {
			out = append(out, s.ID)
		}
	}
	return out
}

// TestReloadSourcesMovesARunningScan: moving an instance while its watcher runs
// cancels and joins the old watcher before the replacement starts, keeps the
// instance id and the cursors/history the store already holds, and picks up the
// new path.
func TestReloadSourcesMovesARunningScan(t *testing.T) {
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

	// Seed history from the old path deterministically, before the watcher is
	// running.
	if err := a.CurrentScanner().Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got := totals(t, a, query.Filter{}).Requests; got != 1 {
		t.Fatalf("requests before the move = %d, want 1", got)
	}
	if got := scannedIDs(a); len(got) != 1 || got[0] != id {
		t.Fatalf("scanned ids before the move = %v, want [%s]", got, id)
	}
	old := a.CurrentScanner()

	a.StartScanning(ctx)
	a.scanMu.Lock()
	oldDone, oldRunning := a.scanDone, a.scanRunning
	a.scanMu.Unlock()
	if !oldRunning || oldDone == nil {
		t.Fatal("StartScanning did not start a watcher")
	}

	moveEntry := func() {
		updateEntry(t, data, id, func(e *instance.Entry) { e.Roots = []string{rootB} })
	}
	moveEntry()
	if err := a.ReloadSources(ctx); err != nil {
		t.Fatalf("ReloadSources: %v", err)
	}

	// The old watcher was cancelled and joined before ReloadSources returned,
	// so two watchers never read the same sources at once.
	select {
	case <-oldDone:
	default:
		t.Error("ReloadSources returned before the previous watcher stopped")
	}
	if a.CurrentScanner() == old {
		t.Error("the scanner was not replaced")
	}
	a.scanMu.Lock()
	running, done := a.scanRunning, a.scanDone
	a.scanMu.Unlock()
	if !running || done == nil {
		t.Error("the replacement watcher is not running")
	}
	if done == oldDone {
		t.Error("the replacement reused the old watcher's registration")
	}

	// The id survived the path change, the new root is the scannable one, and
	// the history read before the move is still there.
	src := liveSource(t, a, id)
	if want := canonical(t, rootB); len(src.Roots) != 1 || src.Roots[0] != want {
		t.Errorf("live roots = %v, want [%s]", src.Roots, want)
	}
	if !src.Scanned {
		t.Error("the moved instance is not scanned")
	}
	if got := scannedIDs(a); len(got) != 1 || got[0] != id {
		t.Errorf("scanned ids after the move = %v, want [%s]", got, id)
	}
	// The replacement watcher scans its new root once before it starts watching
	// (Scanner.Run scans, then watches), so history legitimately gains exactly
	// one row: the session under the new path, on top of the one read before
	// the move. Wait for that scan rather than racing it, then pin that the
	// move re-dated or double-counted nothing.
	waitForRequests(t, a, 2)
	if got := totals(t, a, query.Filter{}).Requests; got != 2 {
		t.Errorf("requests after the move = %d, want the 1 read before it plus the new path's 1", got)
	}

	// An explicit scan of the replacement path adds nothing: the watcher's own
	// initial scan already committed that session exactly once.
	if err := a.CurrentScanner().Scan(ctx); err != nil {
		t.Fatalf("Scan after reload: %v", err)
	}
	if got := totals(t, a, query.Filter{}).Requests; got != 2 {
		t.Errorf("requests after scanning the new path = %d, want 2", got)
	}
}

// TestReloadSourcesExcludesDisabledAndBackupCopies: a reload honours the
// disabled flag, never promotes a backup-copy implicitly, and can bring an
// instance back under the same id.
func TestReloadSourcesExcludesDisabledAndBackupCopies(t *testing.T) {
	_, data := sandbox(t)
	rootA := claudeRoot(t, "cfg-a")
	rootB := claudeRoot(t, "cfg-b")
	writeClaudeSession(t, rootA, "proj", "s1", 100, 50)
	writeClaudeSession(t, rootB, "proj", "s1", 100, 50)
	idA := addClaudeEntry(t, data, "cfg-a", rootA, "")
	idB := addClaudeEntry(t, data, "backup", rootB, instance.KindBackupCopy)

	a, err := OpenLocal()
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	defer a.Close()
	ctx := context.Background()

	if got := scannedIDs(a); len(got) != 1 || got[0] != idA {
		t.Fatalf("scanned ids = %v, want only %s", got, idA)
	}
	if bk := liveSource(t, a, idB); bk.Scanned || bk.Kind != instance.KindBackupCopy {
		t.Fatalf("backup copy = %+v, want an unscanned backup-copy", bk)
	}

	// Disabling the only scannable instance stops scanning it and starts
	// nothing in its place: the backup copy is not promoted by a reload.
	updateEntry(t, data, idA, func(e *instance.Entry) { e.Disabled = true })
	if err := a.ReloadSources(ctx); err != nil {
		t.Fatalf("ReloadSources: %v", err)
	}
	if got := scannedIDs(a); len(got) != 0 {
		t.Errorf("scanned ids after disabling = %v, want none", got)
	}
	if off := liveSource(t, a, idA); off.Scanned || !off.Disabled {
		t.Errorf("disabled instance = %+v, want Disabled and not scanned", off)
	}
	if bk := liveSource(t, a, idB); bk.Scanned {
		t.Error("the backup copy was promoted to a scanned instance by a reload")
	}
	if n := len(a.CurrentScanner().Instances()); n != 0 {
		t.Errorf("scanner instances = %d, want 0", n)
	}

	// Re-enabling scans it again under the same id.
	updateEntry(t, data, idA, func(e *instance.Entry) { e.Disabled = false })
	if err := a.ReloadSources(ctx); err != nil {
		t.Fatalf("ReloadSources: %v", err)
	}
	if got := scannedIDs(a); len(got) != 1 || got[0] != idA {
		t.Errorf("scanned ids after re-enabling = %v, want [%s]", got, idA)
	}
	if n := len(a.CurrentScanner().Instances()); n != 1 {
		t.Errorf("scanner instances = %d, want 1", n)
	}
}

// TestReloadSourcesKeepsCursorsAndNeverFallsBack: the cursor row a source
// resumes from is keyed by harness, instance id and path, so a reload keeps it;
// and a configuration that cannot be honoured leaves the running scan exactly
// as it was instead of falling back to another source set.
func TestReloadSourcesKeepsCursorsAndNeverFallsBack(t *testing.T) {
	_, data := sandbox(t)
	rootA := claudeRoot(t, "cfg-a")
	rootB := claudeRoot(t, "cfg-b")
	writeClaudeSession(t, rootA, "proj", "s1", 100, 50)
	id := addClaudeEntry(t, data, "cfg-a", rootA, "")
	oldRoot := canonical(t, rootA)

	a, err := OpenLocal()
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	defer a.Close()
	ctx := context.Background()

	seed := harness.Cursor{Offset: 4096, Size: 4096, Fingerprint: "seed"}
	if err := a.Store.CommitMany(ctx, []store.Write{{
		Harness: model.ClaudeCode, InstanceID: id, Path: oldRoot, Batch: harness.Batch{Next: seed},
	}}); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}
	before, err := a.Store.CursorIn(ctx, model.ClaudeCode, id, oldRoot)
	if err != nil {
		t.Fatalf("CursorIn: %v", err)
	}
	if before.Fingerprint != seed.Fingerprint {
		t.Fatalf("cursor not seeded: %+v", before)
	}

	// A reload that resolves keeps the id and the cursor row for the old path:
	// the replacement resumes from the same store row rather than re-reading.
	updateEntry(t, data, id, func(e *instance.Entry) { e.Roots = []string{rootB} })
	if err := a.ReloadSources(ctx); err != nil {
		t.Fatalf("ReloadSources: %v", err)
	}
	if got := scannedIDs(a); len(got) != 1 || got[0] != id {
		t.Fatalf("scanned ids after the move = %v, want [%s]", got, id)
	}
	after, err := a.Store.CursorIn(ctx, model.ClaudeCode, id, oldRoot)
	if err != nil {
		t.Fatalf("CursorIn after reload: %v", err)
	}
	if after != before {
		t.Errorf("cursor after reload = %+v, want %+v", after, before)
	}

	// A configuration that cannot be honoured must not be applied and must not
	// fall back: the running scanner, its resolution and its scan all stay.
	old := a.CurrentScanner()
	oldSources := a.LiveSources()
	gone := filepath.Join(t.TempDir(), "gone", "projects")
	if _, err := instance.Update(data, func(cfg *instance.Config) error {
		_, err := cfg.Add(instance.AddRequest{Label: "gone", Harness: model.ClaudeCode, Roots: []string{gone}})
		return err
	}); err != nil {
		t.Fatalf("add unresolvable entry: %v", err)
	}
	if err := a.ReloadSources(ctx); err == nil {
		t.Fatal("ReloadSources accepted a configuration whose root does not exist")
	}
	if a.CurrentScanner() != old {
		t.Error("a failed reload replaced the scanner")
	}
	if got := a.LiveSources(); !reflect.DeepEqual(got, oldSources) {
		t.Errorf("live sources after a failed reload = %+v, want %+v", got, oldSources)
	}
	if got := scannedIDs(a); len(got) != 1 || got[0] != id {
		t.Errorf("scanned ids after a failed reload = %v, want [%s]", got, id)
	}

	// A malformed sources.json must be refused the same way.
	if err := os.WriteFile(filepath.Join(data, instance.FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.ReloadSources(ctx); err == nil {
		t.Fatal("ReloadSources accepted a malformed sources.json")
	}
	if a.CurrentScanner() != old {
		t.Error("a malformed config replaced the scanner")
	}
}

// TestCloseStopsTheWatcher: Close must wait for the scanner's watcher before it
// closes the store the scanner writes through.
func TestCloseStopsTheWatcher(t *testing.T) {
	_, data := sandbox(t)
	root := claudeRoot(t, "cfg-a")
	writeClaudeSession(t, root, "proj", "s1", 100, 50)
	addClaudeEntry(t, data, "cfg-a", root, "")

	a, err := OpenLocal()
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	a.StartScanning(context.Background())
	a.scanMu.Lock()
	done, running := a.scanDone, a.scanRunning
	a.scanMu.Unlock()
	if !running || done == nil {
		t.Fatal("StartScanning did not start a watcher")
	}

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-done:
	default:
		t.Error("Close returned while the watcher was still running")
	}
	a.scanMu.Lock()
	running, rest := a.scanRunning, a.scanDone
	a.scanMu.Unlock()
	if running || rest != nil {
		t.Error("Close left the watcher registered")
	}
}

// TestReloadSourcesRebindsClaudeConsentToTheMovedPath: a consent authorizes one
// exact credential source. When its path moves, the consent must stop
// authorizing the instance rather than silently reading the new path.
func TestReloadSourcesRebindsClaudeConsentToTheMovedPath(t *testing.T) {
	_, data := sandbox(t)
	rootA := claudeRoot(t, "cfg-a")
	rootB := claudeRoot(t, "cfg-b")
	id := addClaudeEntry(t, data, "cfg-a", rootA, "")

	a, err := OpenLocal()
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	defer a.Close()
	ctx := context.Background()

	refs := a.ClaudeQuotaSources(id)
	if len(refs) != 1 {
		t.Fatalf("credential sources = %+v, want exactly the configured file", refs)
	}
	granted := refs[0].Ref
	if _, err := a.EnableClaudeQuota(ctx, id, ""); err != nil {
		t.Fatalf("EnableClaudeQuota: %v", err)
	}
	if info, err := a.ClaudeQuotaInfo(ctx, id); err != nil || !info.Enabled {
		t.Fatalf("consent = %+v (err %v), want enabled", info, err)
	}

	updateEntry(t, data, id, func(e *instance.Entry) { e.Roots = []string{rootB} })
	if err := a.ReloadSources(ctx); err != nil {
		t.Fatalf("ReloadSources: %v", err)
	}

	refs = a.ClaudeQuotaSources(id)
	if len(refs) != 1 || refs[0].Ref == granted {
		t.Fatalf("credential sources after the move = %+v, want a ref that followed the path", refs)
	}
	info, err := a.ClaudeQuotaInfo(ctx, id)
	if err != nil {
		t.Fatalf("ClaudeQuotaInfo: %v", err)
	}
	if info.Enabled {
		t.Error("the old consent still authorizes the moved instance")
	}
	if info.SourceRef != granted {
		t.Errorf("consent source ref = %q, want the granted %q", info.SourceRef, granted)
	}
	if info.SourceAvailable {
		t.Error("the granted source is reported available after the path moved")
	}
}
