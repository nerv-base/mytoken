package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

func openTestApp(t *testing.T) *App {
	t.Helper()
	t.Setenv("MYTOKEN_HOME", t.TempDir())
	a, err := OpenLocal()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func findSource(t *testing.T, sources []Source, h model.Harness) Source {
	t.Helper()
	for _, s := range sources {
		if s.Harness == h {
			return s
		}
	}
	t.Fatalf("harness %s missing from sources", h)
	return Source{}
}

func TestSourceSettingsApplyWithoutRestart(t *testing.T) {
	a := openTestApp(t)
	ctx := context.Background()
	sources, err := a.ListSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) == 0 {
		t.Fatal("no harness sources")
	}
	before := findSource(t, sources, model.ClaudeCode)
	if !before.Enabled {
		t.Fatal("claude-code should default to enabled")
	}
	if err = a.SetSourceEnabled(ctx, model.ClaudeCode, false); err != nil {
		t.Fatal(err)
	}
	sources, err = a.ListSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if findSource(t, sources, model.ClaudeCode).Enabled {
		t.Fatal("disabled harness still reports enabled")
	}
	if v, err := a.Store.Setting(ctx, "harness.claude-code.enabled"); err != nil || v != "false" {
		t.Fatalf("setting = %q, %v", v, err)
	}
	if err = a.SetSourceEnabled(ctx, model.ClaudeCode, true); err != nil {
		t.Fatal(err)
	}

	// An invalid override is refused before it is saved.
	missing := filepath.Join(t.TempDir(), "missing")
	if err = a.SetSourceRoot(ctx, model.ClaudeCode, missing); err == nil {
		t.Fatal("missing root was accepted")
	} else if !errors.Is(err, harness.ErrRootMissing) {
		t.Fatalf("error = %v, want ErrRootMissing", err)
	}
	if v, _ := a.Store.Setting(ctx, "harness.claude-code.root"); v != "" {
		t.Fatalf("invalid root was persisted: %q", v)
	}

	// A directory with one session file is a valid claude-code root.
	root := t.TempDir()
	project := filepath.Join(root, "p")
	if err = os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(project, "s.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = a.SetSourceRoot(ctx, model.ClaudeCode, root); err != nil {
		t.Fatalf("valid root refused: %v", err)
	}
	if v, _ := a.Store.Setting(ctx, "harness.claude-code.root"); v != root {
		t.Fatalf("root setting = %q, want %q", v, root)
	}
	sources, err = a.ListSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := findSource(t, sources, model.ClaudeCode); got.Override != root {
		t.Fatalf("override = %q, want %q", got.Override, root)
	}
	// "" restores automatic discovery.
	if err = a.SetSourceRoot(ctx, model.ClaudeCode, ""); err != nil {
		t.Fatal(err)
	}
	if v, _ := a.Store.Setting(ctx, "harness.claude-code.root"); v != "" {
		t.Fatalf("auto root left %q behind", v)
	}
}

func TestCustomPricesRoundTrip(t *testing.T) {
	a := openTestApp(t)
	ctx := context.Background()
	if _, err := a.CustomPrices(ctx); err != nil {
		t.Fatal(err)
	}
	prices := []CustomPrice{{Model: "claude-opus-5-5", Input: 1.5, Output: 9, CacheRead: 0.15, CacheWrite: 1.875}}
	if err := a.SetCustomPrices(ctx, prices); err != nil {
		t.Fatal(err)
	}
	got, err := a.CustomPrices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Model != "claude-opus-5-5" || got[0].Input != 1.5 {
		t.Fatalf("custom prices = %+v", got)
	}
	if info, models := a.CatalogInfo(); info == "" || models == 0 {
		t.Fatalf("catalog = %q/%d", info, models)
	}
}
