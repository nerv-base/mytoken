package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

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
