package instance

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LockFileName is the lock file that serializes sources.json updates. It lives
// next to the config so two processes pointing at the same data directory
// contend on the same inode. The file is created on first use and deliberately
// never removed: once an OS lock is in play, unlinking the file would let a
// third writer lock a freshly created inode while a second writer still holds
// the old one, which is exactly the lost update this lock exists to prevent.
const LockFileName = ".sources.lock"

// Lock tuning. lockTimeout bounds how long Update waits for a live writer and
// lockRetryEvery is the poll interval. There is deliberately no age-based
// staleness window: a lock is abandoned only when its owning process exits, at
// which point the OS drops the lock. Tests shorten the timeout.
var (
	lockTimeout    = 5 * time.Second
	lockRetryEvery = 5 * time.Millisecond
)

// dirLocks serializes callers inside one process so they do not spin on the
// lock file (and so a long mutation cannot interleave with a short one).
var dirLocks sync.Map // string -> *sync.Mutex

func dirLock(dataDir string) *sync.Mutex {
	key := filepath.Clean(dataDir)
	actual, _ := dirLocks.LoadOrStore(key, &sync.Mutex{})
	return actual.(*sync.Mutex)
}

// Update performs a serialized read-modify-write of the sources.json config in
// dataDir: it loads the config, applies mutate, then saves it atomically. A
// malformed or newer config fails at Load, before mutate runs, and is never
// overwritten; a mutation error or a failed Save leaves the previous file
// byte-identical (a Save that fails only on the post-rename directory flush
// reports that the rename already committed instead). The returned config is
// the value that was written.
//
// The whole sequence is guarded by an in-process mutex plus a cross-process
// exclusive OS lock (flock on Unix, LockFileEx on Windows) on LockFileName, so
// cooperating writers cannot lose each other's entries and a writer that
// crashes or is killed releases the lock automatically. Acquisition is bounded:
// a lock held by another live writer fails with a *ConfigError after
// lockTimeout instead of blocking forever. Age is never a reason to steal a
// lock, because a slow writer (for example one validating a directory on a slow
// mount) is still alive; mutate must not call Update for the same dataDir.
//
// Update is additive for the config; Load, Save and Validate are unchanged. The
// lock is advisory, so it only serializes writers that go through Update:
// os.Rename alone is not a compare-and-swap, and a process that edits
// sources.json directly is outside this contract.
func Update(dataDir string, mutate func(*Config) error) (*Config, error) {
	path := filepath.Join(dataDir, FileName)
	if mutate == nil {
		return nil, &ConfigError{Code: CodeInvalidConfig, Path: path, Message: "nil update function"}
	}
	// Read once before locking: a config this call could never rewrite is
	// refused without creating the lock file, so a rejected update leaves no
	// artifact behind. The authoritative read happens again under the lock, so
	// the read-modify-write itself stays serialized.
	if _, err := Load(dataDir); err != nil {
		return nil, err
	}
	mu := dirLock(dataDir)
	mu.Lock()
	defer mu.Unlock()

	unlock, err := acquireLock(dataDir)
	if err != nil {
		return nil, err
	}
	defer unlock()

	cfg, err := Load(dataDir)
	if err != nil {
		return nil, err
	}
	if err := mutate(cfg); err != nil {
		return nil, err
	}
	if err := cfg.Save(dataDir); err != nil {
		return nil, err
	}
	return cfg, nil
}

// acquireLock takes the cross-process exclusive lock for dataDir and returns
// the release function. It creates dataDir and the lock file when missing, so a
// first `sources add` works. The lock is a real OS lock held until the returned
// function runs or the process exits: a crashed owner cannot wedge later
// writers, and a live owner is never overruled because its lock looks old.
// Acquisition is bounded by lockTimeout and returns a *ConfigError instead of
// blocking forever. The lock file itself is never unlinked.
func acquireLock(dataDir string) (func(), error) {
	path := filepath.Join(dataDir, LockFileName)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, &ConfigError{Code: CodeIOError, Path: path, Message: "create data directory", Err: err}
	}
	deadline := time.Now().Add(lockTimeout)
	for {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, &ConfigError{Code: CodeIOError, Path: path, Message: "open lock file", Err: err}
		}
		locked, err := tryLockFile(f)
		if err != nil {
			_ = f.Close()
			return nil, &ConfigError{Code: CodeIOError, Path: path, Message: "lock config file", Err: err}
		}
		if locked {
			// The owner stamp is diagnostic only: the OS lock, not this text,
			// is what excludes other writers, so failing to record it must not
			// fail an update that already holds the lock.
			_, _ = f.Seek(0, io.SeekStart)
			_ = f.Truncate(0)
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			return func() { releaseLock(f) }, nil
		}
		_ = f.Close()

		wait := time.Until(deadline)
		if wait <= 0 {
			return nil, &ConfigError{
				Code:    CodeIOError,
				Path:    path,
				Message: "config lock is held by another writer",
				Err:     fs.ErrExist,
			}
		}
		if wait > lockRetryEvery {
			wait = lockRetryEvery
		}
		time.Sleep(wait)
	}
}

// releaseLock drops the OS lock and closes the descriptor. The lock file stays
// on disk: removing it here would race a writer that already opened the same
// inode, letting a third writer lock a new inode concurrently.
func releaseLock(f *os.File) {
	_ = unlockFile(f)
	_ = f.Close()
}

// lockIsHeld reports whether another process currently holds the lock for
// dataDir. It exists for diagnostics and tests; it takes no lock of its own.
func lockIsHeld(dataDir string) (bool, error) {
	f, err := os.OpenFile(filepath.Join(dataDir, LockFileName), os.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	locked, err := tryLockFile(f)
	if err != nil {
		return false, err
	}
	if locked {
		_ = unlockFile(f)
		return false, nil
	}
	return true, nil
}
