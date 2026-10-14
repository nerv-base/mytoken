package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/app"
	"github.com/zzstar101/mytoken/internal/gui"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/store"
)

// TestGUIHooksWireTheProviderAliasStore pins the app's wiring of the providers
// tab: the hooks main() builds must carry the already-open app's store write, so
// the real window can rename an endpoint. guiHooks takes the open app, so wiring
// it can never open a second one, and it passes the pricing settings through
// untouched.
func TestGUIHooksWireTheProviderAliasStore(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	const base = "https://api.example.com/v1"
	if err := st.Commit(t.Context(), model.Codex, "main_test", harness.Batch{Events: []model.UsageEvent{{
		DedupKey:  "a",
		SessionID: "s1",
		Model:     "m",
		BaseURL:   base,
		Timestamp: time.Now(),
		Tokens:    model.Tokens{Input: 10},
	}}}, []store.Resolution{{Provider: "alpha", Cost: 1, Priced: true}}); err != nil {
		t.Fatal(err)
	}
	q := query.NewService(st)
	before, err := q.ProviderGroups(t.Context(), query.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || !before[0].Identified {
		t.Fatalf("want one identified endpoint: %+v", before)
	}

	a := &app.App{Store: st}
	hooks := guiHooks(a, context.Background(), t.TempDir(), func() {}, func() bool { return true })
	if hooks.SetProviderAlias == nil {
		t.Fatal("main.go's hooks do not wire a.Store.SetProviderAlias: the providers tab has no display-name store")
	}
	if err := hooks.SetProviderAlias(t.Context(), before[0].Key, "Friendly"); err != nil {
		t.Fatalf("the wired alias write failed: %v", err)
	}
	after, err := q.ProviderGroups(t.Context(), query.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Key != before[0].Key || after[0].Label != "Friendly" {
		t.Fatalf("the wired write did not rename by stable key: %+v", after)
	}

	// An app with no store hides the editor rather than wiring a nil write, and
	// the pricing settings are the app's own, passed through as they are.
	zero := guiHooks(&app.App{}, context.Background(), "", func() {}, func() bool { return true })
	if zero.SetProviderAlias != nil {
		t.Error("a nil store must leave SetProviderAlias nil")
	}
	if zero.Settings != nil {
		t.Error("guiHooks must hand the app's pricing settings through, not substitute its own")
	}
}

// TestGUIHooksWireTheSettingsCards pins the wiring the new settings sections
// need: the sources editor when there is a data directory to read, the offline
// quota store only when a store is open, and the Claude quota panel and ingest
// rate only when the app actually built those services.
func TestGUIHooksWireTheSettingsCards(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	a := &app.App{Store: st}
	hooks := guiHooks(a, context.Background(), t.TempDir(), func() {}, func() bool { return true })
	if hooks.Sources == nil {
		t.Error("a data directory must wire the sources editor")
	}
	if hooks.QuotaStore == nil {
		t.Error("an open store must wire the offline quota reader")
	}
	if hooks.ClaudeQuota != nil {
		t.Error("an app without a quota service must not wire the Claude quota panel")
	}
	if hooks.IngestRate != nil {
		t.Error("an app without a scanner must not wire an ingest rate")
	}
	if _, err := hooks.Sources.Config(context.Background()); err != nil {
		t.Errorf("an empty data directory must read as empty, not fail: %v", err)
	}

	zero := guiHooks(&app.App{}, context.Background(), "", func() {}, func() bool { return true })
	if zero.Sources != nil || zero.ClaudeQuota != nil || zero.QuotaStore != nil || zero.IngestRate != nil {
		t.Error("an app with nothing open must leave every new hook nil")
	}
}

// TestSourceAdminKeepsIdentitiesAndRefusesInvalidRoots drives the real
// SourceAdmin over a temporary data directory: two directories holding the
// same content stay two instances, a move keeps the instance's id, a backup
// copy is never scanned, an unusable root is refused before anything is
// written, and removing an entry never touches the logs it pointed at.
func TestSourceAdminKeepsIdentitiesAndRefusesInvalidRoots(t *testing.T) {
	dataDir := t.TempDir()
	logs := t.TempDir()
	mk := func(name string) string {
		p := filepath.Join(logs, name)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	rootA, rootB, rootC := mk("a"), mk("b"), mk("c")
	// Same content in two directories: an instance is its identity, not its
	// bytes, so these two are never merged.
	for _, r := range []string{rootA, rootB} {
		if err := os.WriteFile(filepath.Join(r, "sessions.jsonl"), []byte("same\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	admin := sourceAdmin{dataDir: dataDir, app: &app.App{}}
	ctx := context.Background()
	byLabel := func(v gui.SourceView, label string) gui.SourceInfo {
		t.Helper()
		for _, in := range v.Sources {
			if in.Label == label {
				return in
			}
		}
		t.Fatalf("no source labelled %q in %+v", label, v.Sources)
		return gui.SourceInfo{}
	}

	view, err := admin.Add(ctx, gui.SourceAdd{Label: "A", Harness: model.Codex, Roots: []string{rootA}, Kind: "instance"})
	if err != nil {
		t.Fatalf("adding an instance with a real root failed: %v", err)
	}
	a := byLabel(view, "A")
	if a.ID == "" || !a.Explicit() || !a.Scanned || a.BackupCopy() {
		t.Fatalf("a scanned independent instance looks wrong: %+v", a)
	}
	if !view.Restart {
		t.Error("a saved change must ask for a restart: the running process still holds the old set")
	}

	view, err = admin.Add(ctx, gui.SourceAdd{Label: "B", Harness: model.Codex, Roots: []string{rootB}, Kind: "instance"})
	if err != nil {
		t.Fatal(err)
	}
	b := byLabel(view, "B")
	if len(view.Sources) != 2 || a.ID == b.ID {
		t.Fatalf("identical content was merged into one instance: %+v", view.Sources)
	}

	view, err = admin.Add(ctx, gui.SourceAdd{Label: "C", Harness: model.Codex, Roots: []string{rootC}, Kind: "backup-copy"})
	if err != nil {
		t.Fatal(err)
	}
	c := byLabel(view, "C")
	if !c.BackupCopy() || c.Scanned {
		t.Fatalf("a backup copy must be declared but never scanned: %+v", c)
	}

	// An unusable explicit root is refused before sources.json is touched.
	sourcesPath := filepath.Join(dataDir, "sources.json")
	before, err := os.ReadFile(sourcesPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Add(ctx, gui.SourceAdd{Label: "Gone", Harness: model.Codex, Roots: []string{filepath.Join(logs, "missing")}, Kind: "instance"}); err == nil {
		t.Fatal("an unusable explicit root was written instead of refused")
	}
	after, err := os.ReadFile(sourcesPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("a refused write changed sources.json")
	}

	// A move is the same instance at another root, not a new one.
	rootD := mk("d")
	view, err = admin.Move(ctx, a.ID, []string{rootD})
	if err != nil {
		t.Fatalf("moving to a real root failed: %v", err)
	}
	moved := byLabel(view, "A")
	if moved.ID != a.ID {
		t.Errorf("a move changed the instance id: %q != %q", moved.ID, a.ID)
	}
	wantD, err := filepath.EvalSymlinks(rootD)
	if err != nil {
		t.Fatal(err)
	}
	if len(moved.Roots) != 1 || moved.Roots[0] != wantD {
		t.Errorf("a move did not adopt the typed roots: %q", moved.Roots)
	}

	// Promoting declares the copy independent; removing drops the entry only.
	view, err = admin.Promote(ctx, c.ID)
	if err != nil {
		t.Fatalf("promoting a backup copy failed: %v", err)
	}
	promoted := byLabel(view, "C")
	if promoted.ID != c.ID || promoted.BackupCopy() || !promoted.Scanned {
		t.Errorf("promotion must keep the id and start scanning: %+v", promoted)
	}
	if _, err := admin.Remove(ctx, b.ID); err != nil {
		t.Fatalf("removing an entry failed: %v", err)
	}
	if _, err := os.Stat(rootB); err != nil {
		t.Errorf("removing a source deleted its logs: %v", err)
	}
	final, err := admin.Config(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range final.Sources {
		if in.ID == b.ID {
			t.Errorf("the removed entry is still listed: %+v", in)
		}
	}
}
