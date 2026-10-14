package store

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A zero-byte artifact is not a usable snapshot, yet SQLite reports a clean bill
// of health for one, so PRAGMA quick_check alone would accept it. This test pins
// that evidence and the explicit size/type validation.
func TestVerifyBackupArtifactRejectsEmptyArtifact(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "interrupted.db")
	f, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	empty := openReadOnly(t, destination)
	var check string
	if err = empty.QueryRow("PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
		t.Fatalf("expected sqlite to accept a zero-byte file, got %q %v", check, err)
	}

	size, err := verifyBackupArtifact(f)
	if err == nil {
		t.Fatalf("empty artifact accepted with size %d", size)
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Fatalf("unhelpful empty-artifact error: %v", err)
	}

	if _, err = f.WriteString("x"); err != nil {
		t.Fatal(err)
	}
	if size, err = verifyBackupArtifact(f); err != nil || size != 1 {
		t.Fatalf("non-empty regular file rejected: %d %v", size, err)
	}
	if _, err = verifyBackupArtifact(mustOpenDir(t, dir)); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory accepted as a backup artifact: %v", err)
	}
}

func mustOpenDir(t *testing.T, dir string) *os.File {
	t.Helper()
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// Windows cannot open a directory for fsync, so that platform is an explicit
// no-op; it is not claimed to be tested or durable.
func TestSyncDirectoryPlatformBehaviour(t *testing.T) {
	if err := syncDirectory(filepath.Join(t.TempDir(), "missing"), "windows"); err != nil {
		t.Fatalf("windows branch must stay an explicit no-op: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if err := syncDirectory(missing, "darwin"); err == nil {
		t.Fatal("a missing directory must be reported, not ignored")
	}
	if runtime.GOOS != "windows" {
		if err := syncDirectory(t.TempDir(), runtime.GOOS); err != nil {
			t.Fatalf("syncing a real directory failed on %s: %v", runtime.GOOS, err)
		}
	}
}

// The snapshot written by a real backup must be a nonempty regular file whose
// name is durable, and a backup failure must not leave the live index changed.
func TestBackupArtifactIsNonEmptyAndNamed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err = st.Exec(context.Background(), "INSERT INTO settings(key,value) VALUES('keep','1') ON CONFLICT(key) DO UPDATE SET value=excluded.value"); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "snapshot.db")
	if err = st.Backup(context.Background(), destination); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Fatal("backup artifact is empty")
	}
	if err = syncParentDirectory(destination); err != nil {
		t.Fatalf("backup name is not durable: %v", err)
	}
}

// A refused backup must explain the recovery path: stop the app, preserve the
// database with its WAL, verify both independently, and decide explicitly. A
// write that fails for a reason other than "that name is taken" is reported at
// once instead of being retried under a fresh name.
func TestMigrationBackupFailureErrorIsActionable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.db")
	legacyProviderFixture(t, path)
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	canonical := path + ".pre-provider-v1.db"
	// The parent directory does not exist, so the reservation is refused for a
	// reason that is not fs.ErrExist: no other candidate name can help.
	attempted := filepath.Join(dir, "missing", filepath.Base(canonical))
	written, err := st.BackupFreshDestination(context.Background(), attempted)
	if err == nil {
		t.Fatal("backup into a missing directory succeeded")
	}
	if written != attempted {
		t.Fatalf("attempted %q, want %q", written, attempted)
	}
	message := migrationBackupFailure("provider", "No event, session or price row was changed and the identity marker was not set", canonical, written, err).Error()
	for _, want := range []string{
		"provider migration stopped before any history was migrated",
		"the migration transaction never started",
		attempted,
		canonical,
		"does not promise a byte-identical file",
		"never overwritten, reused or deleted",
		"Stop every MyToken process",
		"-wal and -shm",
		"verify both independently",
		"Do not restart while this state is unverified",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("failure message is missing %q:\n%s", want, message)
		}
	}
}
