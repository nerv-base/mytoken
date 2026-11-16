package codebuddycn

import (
	"path/filepath"
	"testing"

	"github.com/nerv-base/mytoken/internal/harness"
	"github.com/nerv-base/mytoken/internal/model"
)

// TestHarnessAndRoots checks that the parser reports the CodeBuddy CN harness id
// and that CODEBUDDYCN_HOME moves the CLI transcript root.
func TestHarnessAndRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEBUDDYCN_HOME", home)
	t.Setenv("CODEBUDDY_CN_HOME", "")
	p := New()
	if got := p.Harness(); got != model.CodeBuddyCN {
		t.Errorf("Harness() = %q, want %q", got, model.CodeBuddyCN)
	}
	want := filepath.Join(home, "projects")
	if got := p.Roots()[0]; got != want {
		t.Fatalf("Roots()[0] = %s, want %s", got, want)
	}
}

// TestAliasEnv checks the fallback spelling, and that it is consulted only when
// the primary variable is absent.
func TestAliasEnv(t *testing.T) {
	home := t.TempDir()
	alias := t.TempDir()
	t.Setenv("CODEBUDDYCN_HOME", "")
	t.Setenv("CODEBUDDY_CN_HOME", alias)
	if got := New().Roots()[0]; got != filepath.Join(alias, "projects") {
		t.Fatalf("Roots()[0] = %s, want %s", got, filepath.Join(alias, "projects"))
	}

	t.Setenv("CODEBUDDYCN_HOME", home)
	if got := New().Roots()[0]; got != filepath.Join(home, "projects") {
		t.Fatalf("primary should win: Roots()[0] = %s, want %s", got, filepath.Join(home, "projects"))
	}
}

// TestMacRoots checks the platform tree that was verified against real files:
// the CLI home, the Electron app log, VS Code's log, and the shared extension
// logs.
func TestMacRoots(t *testing.T) {
	home := t.TempDir()
	base := filepath.Join(home, "Library", "Application Support")
	assertRoots(t, roots(home, noEnv, "darwin"), []string{
		filepath.Join(home, ".codebuddycn", "projects"),
		filepath.Join(base, "CodeBuddy CN", "logs"),
		filepath.Join(base, "Code", "logs"),
		filepath.Join(base, "Code - Insiders", "logs"),
		filepath.Join(base, "CodeBuddyExtension", "Logs", "CodeBuddyIDE"),
		filepath.Join(base, "CodeBuddyExtension", "Logs", "VSCode"),
	})
}

// TestWindowsRoots checks that both the roaming and the local app-data trees are
// searched, because the Windows layout is inferred rather than observed.
func TestWindowsRoots(t *testing.T) {
	home := t.TempDir()
	roaming := filepath.Join(home, "AppData", "Roaming")
	local := filepath.Join(home, "AppData", "Local")
	getenv := func(key string) string {
		switch key {
		case "APPDATA":
			return roaming
		case "LOCALAPPDATA":
			return local
		}
		return ""
	}
	assertRoots(t, roots(home, getenv, "windows"), []string{
		filepath.Join(home, ".codebuddycn", "projects"),
		filepath.Join(roaming, "CodeBuddy CN", "logs"),
		filepath.Join(roaming, "Code", "logs"),
		filepath.Join(roaming, "Code - Insiders", "logs"),
		filepath.Join(local, "CodeBuddy CN", "logs"),
		filepath.Join(local, "Code", "logs"),
		filepath.Join(local, "Code - Insiders", "logs"),
		filepath.Join(roaming, "CodeBuddyExtension", "Logs", "CodeBuddyIDE"),
		filepath.Join(roaming, "CodeBuddyExtension", "Logs", "VSCode"),
		filepath.Join(local, "CodeBuddyExtension", "Logs", "CodeBuddyIDE"),
		filepath.Join(local, "CodeBuddyExtension", "Logs", "VSCode"),
	})
}

// TestLinuxRoots checks that the XDG variables are honoured and that the plain
// defaults are still searched.
func TestLinuxRoots(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, "xdg-config")
	data := filepath.Join(home, "xdg-data")
	getenv := func(key string) string {
		switch key {
		case "XDG_CONFIG_HOME":
			return config
		case "XDG_DATA_HOME":
			return data
		}
		return ""
	}
	assertRoots(t, roots(home, getenv, "linux"), []string{
		filepath.Join(home, ".codebuddycn", "projects"),
		filepath.Join(config, "CodeBuddy CN", "logs"),
		filepath.Join(config, "Code", "logs"),
		filepath.Join(home, ".config", "CodeBuddy CN", "logs"),
		filepath.Join(data, "CodeBuddy CN", "logs"),
		filepath.Join(data, "CodeBuddyExtension", "Logs", "CodeBuddyIDE"),
		filepath.Join(home, ".config", "CodeBuddyExtension", "Logs", "CodeBuddyIDE"),
		filepath.Join(home, ".local", "share", "CodeBuddyExtension", "Logs", "VSCode"),
	})
}

func TestNoDuplicateRoots(t *testing.T) {
	home := t.TempDir()
	getenv := func(string) string { return "" } // XDG unset: defaults must not double up
	seen := map[string]bool{}
	for _, r := range harness.RootPaths(roots(home, getenv, "linux")) {
		if seen[r] {
			t.Errorf("duplicate root %s", r)
		}
		seen[r] = true
	}
}

func assertRoots(t *testing.T, got []harness.Root, want []string) {
	t.Helper()
	paths := harness.RootPaths(got)
	index := make(map[string]bool, len(paths))
	for _, p := range paths {
		index[p] = true
	}
	for _, w := range want {
		if !index[w] {
			t.Errorf("missing root %s in %v", w, paths)
		}
	}
}

func noEnv(string) string { return "" }
