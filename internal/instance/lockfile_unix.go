//go:build unix

package instance

import (
	"errors"
	"os"
	"syscall"
)

// tryLockFile attempts a non-blocking exclusive flock on f. It reports
// (false, nil) when another open file description already holds the lock, and
// (false, err) only for a genuine failure. The lock lasts as long as the file
// descriptor is open, so process exit (including SIGKILL) releases it.
func tryLockFile(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EAGAIN), errors.Is(err, syscall.EINTR):
		return false, nil
	default:
		return false, err
	}
}

// unlockFile releases the lock held through f. Closing the descriptor releases
// it as well; unlocking first keeps the intent explicit.
func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// syncDir flushes a directory (the parent of the renamed config, so the new
// name itself survives a crash) to stable storage. Filesystems that do not
// support directory sync report success: the rename already committed, and
// claiming a durability failure there would be misleading.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.ENOSYS) {
			return nil
		}
		return err
	}
	return nil
}
