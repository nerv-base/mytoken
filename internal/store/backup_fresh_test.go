package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assertSQLiteIntegrity opens path read-only and requires PRAGMA quick_check to
// report a healthy database, so a "fresh backup" is verified, not merely present.
func assertSQLiteIntegrity(t *testing.T, path string) {
	t.Helper()
	var check string
	if err := openReadOnly(t, path).QueryRow("PRAGMA quick_check").Scan(&check); err != nil {
		t.Fatalf("quick_check %s: %v", path, err)
	}
	if check != "ok" {
		t.Fatalf("quick_check %s: %q", path, check)
	}
}

// freshMigrationBackups returns the timestamped siblings of the canonical
// pre-migration artifact for suffix, i.e. the files a fallback write produced.
// The canonical name itself is never returned.
func freshMigrationBackups(t *testing.T, dbPath, suffix string) []string {
	t.Helper()
	dir := filepath.Dir(dbPath)
	canonicalName := filepath.Base(dbPath) + suffix
	prefix := strings.TrimSuffix(canonicalName, ".db") + "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.Name() == canonicalName || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out
}

// backupCandidateName must keep the canonical name first, then produce names
// that sort by time, carry a counter when one is taken, and stay legal on
// Windows (no colon, slash or other reserved character).
func TestBackupCandidateName(t *testing.T) {
	canonical := filepath.Join(t.TempDir(), "mytoken.db.pre-provider-v1.db")
	if got := backupCandidateName(canonical, 0, "20261014-181500"); got != canonical {
		t.Fatalf("attempt 0 = %q, want the canonical %q", got, canonical)
	}
	first := backupCandidateName(canonical, 1, "20261014-181500")
	if want := canonical[:len(canonical)-len(".db")] + ".20261014-181500.db"; first != want {
		t.Fatalf("first fallback = %q, want %q", first, want)
	}
	second := backupCandidateName(canonical, 2, "20261014-181500")
	if want := canonical[:len(canonical)-len(".db")] + ".20261014-181500.2.db"; second != want {
		t.Fatalf("second fallback = %q, want %q", second, want)
	}
	// A later stamp sorts after an earlier one, so the directory lists them in
	// the order the attempts happened.
	if later := backupCandidateName(canonical, 1, "20261014-181501"); later <= first {
		t.Fatalf("stamped names are not orderable: %q <= %q", later, first)
	}
	for _, attempt := range []int{1, 2, 11, 99} {
		name := filepath.Base(backupCandidateName(canonical, attempt, "20261014-181500"))
		if strings.ContainsAny(name, `:*?"<>|/\`) {
			t.Fatalf("attempt %d produced a name Windows cannot store: %q", attempt, name)
		}
	}
}

// The canonical name is still used whenever it is free, for both migrations.
func TestMigrationBackupUsesCanonicalNameWhenFree(t *testing.T) {
	t.Run("provider", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "history.db")
		legacyProviderFixture(t, path)
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		canonical := path + ".pre-provider-v1.db"
		if _, err = os.Stat(canonical); err != nil {
			t.Fatalf("canonical backup missing: %v", err)
		}
		if extra := freshMigrationBackups(t, path, ".pre-provider-v1.db"); len(extra) != 0 {
			t.Fatalf("a free canonical name must still be used; siblings: %v", extra)
		}
		assertSQLiteIntegrity(t, canonical)
	})
	t.Run("instance", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "instance.db")
		writeLegacyPhysicalDatabase(t, path)
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		canonical := path + ".pre-instance-v1.db"
		if _, err = os.Stat(canonical); err != nil {
			t.Fatalf("canonical backup missing: %v", err)
		}
		if extra := freshMigrationBackups(t, path, ".pre-instance-v1.db"); len(extra) != 0 {
			t.Fatalf("a free canonical name must still be used; siblings: %v", extra)
		}
		assertSQLiteIntegrity(t, canonical)
	})
}

// Claiming a backup name must never reuse, overwrite or delete an existing file:
// each taken candidate is stepped over, in order, until one is free.
func TestFreshBackupReservationNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	canonical := filepath.Join(dir, "mytoken.db.pre-provider-v1.db")
	const stamp = "20261014-181500"
	taken := map[string]string{
		backupCandidateName(canonical, 0, stamp): "canonical leftover",
		backupCandidateName(canonical, 1, stamp): "stamped leftover",
		backupCandidateName(canonical, 2, stamp): "counted leftover",
	}
	for path, content := range taken {
		if err = os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}

	written, err := st.backupFreshDestination(context.Background(), canonical, stamp)
	if err != nil {
		t.Fatalf("no free name found although attempt 3 is free: %v", err)
	}
	if want := backupCandidateName(canonical, 3, stamp); written != want {
		t.Fatalf("wrote to %q, want the next free name %q", written, want)
	}
	for path, content := range taken {
		got, readErr := os.ReadFile(path)
		if readErr != nil || string(got) != content {
			t.Fatalf("taken name %s was modified: %q %v", path, got, readErr)
		}
	}
	assertSQLiteIntegrity(t, written)

	// A second run must not reuse the artifact the first run just wrote.
	again, err := st.backupFreshDestination(context.Background(), canonical, stamp)
	if err != nil {
		t.Fatal(err)
	}
	if again != backupCandidateName(canonical, 4, stamp) {
		t.Fatalf("second run wrote %q, want %q", again, backupCandidateName(canonical, 4, stamp))
	}
}

// When every candidate name is taken the helper stops with a clear error after a
// bounded number of attempts instead of looping, and touches nothing.
func TestFreshBackupReservationExhaustion(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	canonical := filepath.Join(dir, "mytoken.db.pre-instance-v1.db")
	const stamp = "20261014-181500"
	for attempt := 0; attempt < maxBackupAttempts; attempt++ {
		if err = os.WriteFile(backupCandidateName(canonical, attempt, stamp), []byte("taken"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	written, err := st.backupFreshDestination(context.Background(), canonical, stamp)
	if err == nil {
		t.Fatalf("wrote %q although every candidate name is taken", written)
	}
	if written != "" {
		t.Fatalf("reported path %q on exhaustion, want none", written)
	}
	if !strings.Contains(err.Error(), "after 100 attempts") {
		t.Fatalf("exhaustion error is not actionable: %v", err)
	}
	for attempt := 0; attempt < maxBackupAttempts; attempt++ {
		path := backupCandidateName(canonical, attempt, stamp)
		got, readErr := os.ReadFile(path)
		if readErr != nil || string(got) != "taken" {
			t.Fatalf("candidate %s was modified: %q %v", path, got, readErr)
		}
	}
}
