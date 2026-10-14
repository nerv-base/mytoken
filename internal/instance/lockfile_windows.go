//go:build windows

package instance

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockRange is the byte range LockFileEx excludes on. A single byte is enough:
// the lock's purpose is mutual exclusion, not range protection.
const (
	lockRangeLow  = 1
	lockRangeHigh = 0
)

// tryLockFile attempts a non-blocking exclusive LockFileEx on f. It reports
// (false, nil) when another handle already holds the range. Windows releases
// the lock when the handle closes, which happens on process exit, so a crashed
// writer cannot wedge later ones.
func tryLockFile(f *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, lockRangeLow, lockRangeHigh, new(windows.Overlapped))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION),
		errors.Is(err, windows.ERROR_IO_PENDING),
		errors.Is(err, windows.ERROR_IO_INCOMPLETE):
		return false, nil
	default:
		return false, err
	}
}

// unlockFile releases the range held through f.
func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, lockRangeLow, lockRangeHigh, new(windows.Overlapped))
}

// syncDir is a no-op on Windows: FlushFileBuffers is not supported on a
// directory handle, so the platform cannot make the rename durable beyond the
// config file's own flush. Reporting success here is honest about what the
// platform offers, and callers still get the flushed file contents.
func syncDir(string) error { return nil }
