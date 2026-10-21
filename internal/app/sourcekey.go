package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/paths"
	"github.com/zzstar101/mytoken/internal/traekey"
)

// Some harnesses encrypt their local database and keep the key nowhere on
// disk — Trae derives it per installation and only ever holds it in memory, so
// the user has to supply it once. That key is a credential, and the index is a
// usage ledger that can be copied or exported, so it lives in its own 0600 file
// under the data directory instead of the settings table.

// keyDirName is the subdirectory of the data directory that holds harness keys.
const keyDirName = "keys"

// sourceKeyPath returns the file that holds h's key. It uses the resolving
// variant so listing sources never creates a directory.
func sourceKeyPath(h model.Harness) (string, error) {
	dir, err := paths.ResolveDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, keyDirName, string(h)+".key"), nil
}

// readSourceKey returns the stored key for h, or "" when none is stored.
// A key that cannot be read is reported as no key: the parser then explains
// what is missing instead of the scan failing for an opaque reason.
func readSourceKey(h model.Harness) string {
	path, err := sourceKeyPath(h)
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// writeSourceKey stores key for h, or removes the file when key is empty.
func writeSourceKey(h model.Harness, key string) error {
	path, err := sourceKeyPath(h)
	if err != nil {
		return err
	}
	if key == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// O_EXCL is wrong here — replacing a key is expected — but the file must be
	// readable only by its owner from the moment it exists.
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// SourceKeySet reports whether a harness that needs a key has one stored.
func (a *App) SourceKeySet(h model.Harness) bool { return readSourceKey(h) != "" }

// SourceKeyStatus reports whether a harness takes a database key at all and
// whether one is stored. It touches no index, so a read-only caller can print
// the answer.
func SourceKeyStatus(h model.Harness) (takesKey, stored bool) {
	p, ok := harness.Get(h)
	if !ok {
		return false, false
	}
	if _, ok := p.(harness.Keyed); !ok {
		return false, false
	}
	return true, readSourceKey(h) != ""
}

// SourceKey returns the stored key for h, or "" when none is stored. It exists
// for the command line, which can reveal what it stored.
func SourceKey(h model.Harness) string { return readSourceKey(h) }

// parserWithKey resolves h the way a scan will — honouring the root override
// the user set — and applies key to it. Probing the default roots instead of the
// configured one would check data the next scan never reads.
func (a *App) parserWithKey(ctx context.Context, h model.Harness, key string) (harness.Parser, error) {
	p, ok := harness.Get(h)
	if !ok {
		return nil, fmt.Errorf("unknown harness %q", h)
	}
	if _, ok := p.(harness.Keyed); !ok {
		return nil, fmt.Errorf("%s does not take a database key", h.DisplayName())
	}
	root, _ := readSetting(ctx, a.Store, rootSettingKey(h))
	if root = strings.TrimSpace(root); root != "" {
		if err := harness.ValidateRoot(p, root); err != nil {
			return nil, err
		}
		if r, ok := p.(harness.Rooted); ok {
			p = r.WithRoots(root)
		}
	}
	if key == "" {
		return p, nil
	}
	return p.(harness.Keyed).WithKey(key)
}

// SetSourceKey stores the key a harness needs to open its sources and rebuilds
// the scanner. An empty key forgets it. The key is proved against the data on
// disk before it is stored, so a wrong key is refused here rather than turning
// into an empty scan later.
func (a *App) SetSourceKey(ctx context.Context, h model.Harness, key string) error {
	key = strings.TrimSpace(key)
	p, err := a.parserWithKey(ctx, h, key)
	if err != nil {
		return err
	}
	if key != "" {
		if err := probeKey(ctx, p); err != nil {
			return err
		}
	}
	if err := writeSourceKey(h, key); err != nil {
		return err
	}
	return a.Rescan(ctx)
}

// probeKey parses every source the parser sees so a wrong key is rejected
// before it is stored. A harness with no data on this machine has nothing to
// prove, so the key is accepted.
func probeKey(ctx context.Context, p harness.Parser) error {
	srcs, err := p.Discover(ctx)
	if err != nil {
		return err
	}
	for _, src := range srcs {
		if _, err := p.Parse(ctx, src, harness.Cursor{}); err != nil {
			return err
		}
	}
	return nil
}

// CaptureSourceKey reads h's key out of its own application and stores it.
//
// A key that only ever exists in memory cannot be read from disk, and asking the
// user to hunt for it in a debugger is not a workflow. This asks the running
// application instead: capture opens the application once, takes the key from
// its memory, proves it against the database and stores it. progress may be nil
// and receives a line per step for a UI that has somewhere to say it.
func (a *App) CaptureSourceKey(ctx context.Context, h model.Harness, progress func(string)) error {
	if progress == nil {
		progress = func(string) {}
	}
	p, err := a.parserWithKey(ctx, h, "")
	if err != nil {
		return err
	}
	lister, ok := p.(harness.DatabaseLister)
	if !ok {
		return fmt.Errorf("%s does not keep an encrypted database", h.DisplayName())
	}
	// Capture works against the database the next scan would read, configured
	// root and all, so it proves the key on the very file that will use it.
	dbs, err := lister.DatabaseFiles(ctx)
	if err != nil {
		return err
	}
	if len(dbs) == 0 {
		return fmt.Errorf("no %s database found on this machine", h.DisplayName())
	}
	key, err := traekey.Capture(ctx, traekey.Options{DB: dbs[0], Log: progress})
	if err != nil {
		return err
	}
	if err := a.SetSourceKey(ctx, h, key); err != nil {
		return err
	}
	progress("done")
	return nil
}
