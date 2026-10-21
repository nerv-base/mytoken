package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/trae"
	"github.com/zzstar101/mytoken/internal/model"
)

// useTraeRoots swaps the registered Trae reader for one rooted at the given
// directories, so these tests never look at the real Trae data on the machine.
func useTraeRoots(t *testing.T, roots ...string) {
	t.Helper()
	prev, _ := harness.Get(model.Trae)
	harness.Register(trae.NewWithRoots(roots...))
	t.Cleanup(func() { harness.Register(prev) })
}

// TestSourceKeyLivesOutsideTheIndex checks the key goes to a 0600 file under the
// data directory and never into the settings table: the index is a usage ledger
// that can be copied or shipped in a bug report.
func TestSourceKeyLivesOutsideTheIndex(t *testing.T) {
	useTraeRoots(t) // no roots: nothing to prove the key against, nothing to read
	a := openTestApp(t)
	ctx := context.Background()

	if takes, stored := SourceKeyStatus(model.Trae); !takes || stored {
		t.Fatalf("trae status = takes %v stored %v, want takes and not stored", takes, stored)
	}
	sources, err := a.ListSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s := findSource(t, sources, model.Trae); !s.NeedsKey || s.HasKey {
		t.Fatalf("source = NeedsKey %v HasKey %v, want true false", s.NeedsKey, s.HasKey)
	}

	key := strings.Repeat("ab", 32)
	if err := a.SetSourceKey(ctx, model.Trae, key); err != nil {
		t.Fatal(err)
	}
	if got := SourceKey(model.Trae); got != key {
		t.Fatalf("stored key = %q, want %q", got, key)
	}
	path, err := sourceKeyPath(model.Trae)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode = %o, want 600", perm)
	}
	var n int
	if err := a.Store.ReadDB().QueryRowContext(ctx, `SELECT count(*) FROM settings WHERE value LIKE ?`, "%"+key+"%").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("the key leaked into the settings table")
	}
	sources, err = a.ListSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s := findSource(t, sources, model.Trae); !s.HasKey {
		t.Fatal("source does not report the stored key")
	}

	if err := a.SetSourceKey(ctx, model.Trae, ""); err != nil {
		t.Fatal(err)
	}
	if got := SourceKey(model.Trae); got != "" {
		t.Fatalf("key after clearing = %q, want empty", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("key file still there: %v", err)
	}
}

// TestSourceKeyRefusedWithoutNeed: a harness with a plain database must not
// accept one, so a typo in the command cannot create a phantom setting.
func TestSourceKeyRefusedWithoutNeed(t *testing.T) {
	a := openTestApp(t)
	if err := a.SetSourceKey(context.Background(), model.ClaudeCode, "abcd"); err == nil {
		t.Fatal("claude-code accepted a database key")
	}
}

// TestSourceKeyRejectsWrongKey proves a bad key is refused before it is stored:
// the reader has to open the database with it first.
func TestSourceKeyRejectsWrongKey(t *testing.T) {
	dir := t.TempDir()
	agentDir := dir + "/ai-agent"
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentDir+"/database.db", []byte("not a database at all, just bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	useTraeRoots(t, dir)
	a := openTestApp(t)
	ctx := context.Background()

	if err := a.SetSourceKey(ctx, model.Trae, strings.Repeat("cd", 32)); err == nil {
		t.Fatal("a key was accepted for a database that cannot be read")
	}
	if got := SourceKey(model.Trae); got != "" {
		t.Fatalf("key %q was stored despite the failure", got)
	}
	if takes, stored := SourceKeyStatus(model.Trae); !takes || stored {
		t.Fatal("a refused key left a key behind")
	}
}
