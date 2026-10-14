package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

// DataDir returns the platform data directory and creates it with private permissions.
func DataDir() (string, error) {
	dir, err := ResolveDataDir()
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return dir, nil
}

// ResolveDataDir returns the platform data directory without creating it.
// A read-only caller — a snapshot in particular — must be able to learn where
// the index lives without bringing a directory into existence, because a
// missing database has to stay missing.
func ResolveDataDir() (string, error) {
	dir := os.Getenv("MYTOKEN_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		switch runtime.GOOS {
		case "darwin":
			dir = filepath.Join(home, "Library", "Application Support", "MyToken")
		case "windows":
			base := os.Getenv("APPDATA")
			if base == "" {
				base = filepath.Join(home, "AppData", "Roaming")
			}
			dir = filepath.Join(base, "MyToken")
		default:
			base := os.Getenv("XDG_DATA_HOME")
			if base == "" {
				base = filepath.Join(home, ".local", "share")
			}
			dir = filepath.Join(base, "mytoken")
		}
	}
	return filepath.Abs(dir)
}

// DBPath returns the database path, creating the data directory first.
func DBPath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "mytoken.db"), nil
}

// ResolveDBPath returns the database path without creating the data directory.
func ResolveDBPath() (string, error) {
	dir, err := ResolveDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "mytoken.db"), nil
}
