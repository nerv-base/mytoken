//go:build !unix && !windows

package instance

import (
	"errors"
	"os"
)

// errLockUnsupported is returned on platforms (for example plan9 or js) that
// have no OS file lock. Refusing loudly is deliberate: falling back to a
// lock-file convention would silently reintroduce the lost update this package
// exists to prevent. syncDir is a no-op because there is no directory fsync to
// offer.
var errLockUnsupported = errors.New("exclusive file locking is not supported on this platform")

func tryLockFile(*os.File) (bool, error) { return false, errLockUnsupported }

func unlockFile(*os.File) error { return nil }

func syncDir(string) error { return nil }
