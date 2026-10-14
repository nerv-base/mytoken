package paths

import (
	"os"
	"path/filepath"
	"testing"
)

// ResolveDataDir is the non-creating resolver: a read-only snapshot path
// resolution must never bring the data directory into existence.
func TestResolveDataDirDoesNotCreate(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "nested", "mytoken")
	t.Setenv("MYTOKEN_HOME", target)

	dir, err := ResolveDataDir()
	if err != nil {
		t.Fatalf("ResolveDataDir: %v", err)
	}
	if dir != target {
		t.Fatalf("ResolveDataDir = %q, want %q", dir, target)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("ResolveDataDir created %q (stat err = %v)", target, err)
	}
	if _, err := os.Stat(filepath.Join(base, "nested")); !os.IsNotExist(err) {
		t.Fatalf("ResolveDataDir created the parent directory (stat err = %v)", err)
	}
}

func TestResolveDBPathDoesNotCreate(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "nested", "mytoken")
	t.Setenv("MYTOKEN_HOME", target)

	db, err := ResolveDBPath()
	if err != nil {
		t.Fatalf("ResolveDBPath: %v", err)
	}
	if want := filepath.Join(target, "mytoken.db"); db != want {
		t.Fatalf("ResolveDBPath = %q, want %q", db, want)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("ResolveDBPath created %q (stat err = %v)", target, err)
	}
}

// DataDir keeps its historical creating behaviour: the read-write app still
// needs the directory to exist before it opens the store.
func TestDataDirStillCreates(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "nested", "mytoken")
	t.Setenv("MYTOKEN_HOME", target)

	dir, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("DataDir did not create %q: %v", dir, err)
	}
	if !info.IsDir() {
		t.Fatalf("%q is not a directory", dir)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("%q mode = %o, want 700", dir, perm)
	}
}

// The creating and non-creating resolvers must agree on the location, so a
// read-only caller resolves exactly the path the app would have created.
func TestResolveAndCreateAgree(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "nested", "mytoken")
	t.Setenv("MYTOKEN_HOME", target)

	resolved, err := ResolveDataDir()
	if err != nil {
		t.Fatal(err)
	}
	created, err := DataDir()
	if err != nil {
		t.Fatal(err)
	}
	if resolved != created {
		t.Fatalf("ResolveDataDir = %q, DataDir = %q", resolved, created)
	}
	resolvedDB, err := ResolveDBPath()
	if err != nil {
		t.Fatal(err)
	}
	createdDB, err := DBPath()
	if err != nil {
		t.Fatal(err)
	}
	if resolvedDB != createdDB {
		t.Fatalf("ResolveDBPath = %q, DBPath = %q", resolvedDB, createdDB)
	}
}

// A relative MYTOKEN_HOME resolves to an absolute path without creating it.
func TestResolveDataDirIsAbsoluteWithoutCreating(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	t.Setenv("MYTOKEN_HOME", "relative-home")

	dir, err := ResolveDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(dir) {
		t.Fatalf("ResolveDataDir = %q, want an absolute path", dir)
	}
	if want := filepath.Join(base, "relative-home"); dir != want {
		t.Fatalf("ResolveDataDir = %q, want %q", dir, want)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("ResolveDataDir created %q (stat err = %v)", dir, err)
	}
}
