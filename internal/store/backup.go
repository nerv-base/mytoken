package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/sqlitedsn"
)

// verifyBackupArtifact rejects artifacts that cannot be a usable snapshot even
// though SQLite still reports a clean quick_check for them: a zero-byte file is
// reported as "ok", and a non-regular file is not a database at all.
func verifyBackupArtifact(f *os.File) (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("inspect migration backup: %w", err)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("migration backup is not a regular file: mode %s", info.Mode())
	}
	if info.Size() == 0 {
		return 0, fmt.Errorf("migration backup is empty (0 bytes): the snapshot was not written")
	}
	return info.Size(), nil
}

// syncParentDirectory makes the backup's directory entry durable, so a crash
// cannot leave a flushed file whose name is lost. Windows cannot open a
// directory for fsync, so that platform is an explicit no-op: it is neither
// tested here nor claimed to be durable.
func syncParentDirectory(path string) error {
	return syncDirectory(filepath.Dir(path), runtime.GOOS)
}

func syncDirectory(dir, goos string) error {
	if goos == "windows" {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open migration backup directory: %w", err)
	}
	defer f.Close()
	if err = f.Sync(); err != nil {
		return fmt.Errorf("sync migration backup directory: %w", err)
	}
	return nil
}

// Backup creates an independent, consistent SQLite snapshot including committed
// WAL data. The caller chooses a new destination in a private directory; existing
// files are never replaced. An error forbids proceeding with a migration. Failed
// partial files are retained for diagnosis, never mistaken for a completed copy.
func (s *Store) Backup(ctx context.Context, destination string) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Reserve exclusively, with private permissions, before SQLite opens it.
	// VACUUM INTO accepts an empty file but refuses nonempty destinations.
	f, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("reserve migration backup: %w", err)
	}
	defer f.Close()
	if _, err = s.db.ExecContext(ctx, "VACUUM INTO ?", destination); err != nil {
		return fmt.Errorf("write migration backup: %w", err)
	}
	// VACUUM INTO does not itself guarantee fsync of the output file.
	if err = f.Sync(); err != nil {
		return fmt.Errorf("sync migration backup: %w", err)
	}
	if _, err = verifyBackupArtifact(f); err != nil {
		return err
	}
	if err = syncParentDirectory(destination); err != nil {
		return err
	}
	backup, err := sql.Open("sqlite", sqlitedsn.URI(destination, "mode=ro"))
	if err != nil {
		return fmt.Errorf("open migration backup: %w", err)
	}
	defer backup.Close()
	var check string
	if err = backup.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil {
		return fmt.Errorf("validate migration backup: %w", err)
	}
	if check != "ok" {
		return fmt.Errorf("migration backup integrity check failed")
	}
	return nil
}

// backupStampLayout is the timestamp form used by a fallback backup name. It is
// lexicographically orderable and contains no character Windows forbids in a
// filename (notably no colon), so the same name works on every platform.
const backupStampLayout = "20060102-150405"

// maxBackupAttempts bounds how many backup names are tried before a migration
// gives up with a clear error, so a directory full of leftovers cannot make the
// open path loop forever.
const maxBackupAttempts = 100

// backupCandidateName is the pure naming rule for a pre-migration backup.
// Attempt 0 keeps the canonical name, so a free canonical path is still used and
// the on-disk contract is unchanged. Later attempts insert an orderable stamp,
// then a counter, before the extension:
//
//	mytoken.db.pre-provider-v1.db
//	mytoken.db.pre-provider-v1.20261014-181500.db
//	mytoken.db.pre-provider-v1.20261014-181500.2.db
func backupCandidateName(canonical string, attempt int, stamp string) string {
	if attempt <= 0 {
		return canonical
	}
	ext := filepath.Ext(canonical)
	base := strings.TrimSuffix(canonical, ext)
	if attempt == 1 {
		return base + "." + stamp + ext
	}
	return base + "." + stamp + "." + strconv.Itoa(attempt) + ext
}

// BackupFreshDestination writes a consistent pre-migration backup to a name
// that is still free and returns the path it actually used, so the caller can
// report and verify the real artifact. The canonical name is preferred; when it
// already exists — typically a leftover from an interrupted attempt — an
// orderable, timestamped sibling is used instead, and a counter once that is
// taken too. Nothing existing is ever overwritten, reused or deleted: every
// attempt still claims its own name with O_CREATE|O_EXCL, so two processes
// cannot both win.
//
// On failure the returned path is where the write was attempted, or "" when no
// free name was found within maxBackupAttempts tries.
func (s *Store) BackupFreshDestination(ctx context.Context, canonical string) (string, error) {
	return s.backupFreshDestination(ctx, canonical, time.Now().Format(backupStampLayout))
}

// backupFreshDestination is BackupFreshDestination with the timestamp passed in,
// so tests can pin the exact candidate names.
func (s *Store) backupFreshDestination(ctx context.Context, canonical, stamp string) (string, error) {
	for attempt := 0; attempt < maxBackupAttempts; attempt++ {
		candidate := backupCandidateName(canonical, attempt, stamp)
		err := s.Backup(ctx, candidate)
		if err == nil {
			return candidate, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return candidate, err
		}
		// The candidate is taken: take the next free name. Never reuse, rename or
		// delete what is already there — it may be a partial artifact of an
		// interrupted attempt, and a second process may have created it.
	}
	return "", fmt.Errorf("no free pre-migration backup name for %s after %d attempts; move the leftover backups aside and retry", canonical, maxBackupAttempts)
}

// migrationBackupFailure is the operator-facing error for a migration that could
// not write its pre-migration backup. canonical is the artifact name the
// migration normally uses and attempted is the path the failed write went to
// (the canonical path, or a fresh sibling when the canonical name was taken).
func migrationBackupFailure(migration, untouched, canonical, attempted string, cause error) error {
	if attempted == "" {
		attempted = canonical
	}
	return fmt.Errorf(`%s migration stopped before any history was migrated: could not write a consistent pre-migration backup to %s: %w

%s; the migration transaction never started. The preflight check does not promise a byte-identical file: opening the index has already applied MyToken's bootstrap DDL (journal mode, and any base table or index created if absent), so only the history is guaranteed untouched. The canonical backup %s is never overwritten, reused or deleted automatically, because it may be a partial artifact of an interrupted attempt; a leftover there is stepped over by writing to the next free, timestamped name, and this message names the artifact the write actually used.

Recovery requires an explicit operator decision. Stop every MyToken process first, then preserve the database together with its -wal and -shm files and the backup, and verify both independently (open the database and run PRAGMA quick_check; open the backup read-only and check it holds the expected history). Only then choose deliberately between repairing the cause of the failed backup, freeing disk space, or restoring the backup. Do not restart while this state is unverified.`,
		migration, attempted, cause, untouched, canonical)
}
