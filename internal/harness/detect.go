package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzstar101/mytoken/internal/model"
)

// Rooter names the root-override capability. It is an alias for Rooted: the
// capability predates this name and every parser implements the variadic form.
type Rooter = Rooted

// Status is the cheap detection result for one harness, as shown in the app.
type Status struct {
	Harness  model.Harness `json:"harness"`
	Detected bool          `json:"detected"`
	Roots    []string      `json:"roots"`
	Problem  string        `json:"problem,omitempty"`
}

// ValidateRoot failures. Callers use errors.Is to tell them apart.
var (
	ErrRootMissing = errors.New("harness: root does not exist")
	ErrRootNotDir  = errors.New("harness: root is not a directory")
	ErrRootNoData  = errors.New("harness: root contains no data this harness can read")
)

// Detect reports whether p looks installed on this machine. It only stats and
// shallow-reads p's roots, so it is safe to call for every harness at startup;
// it never walks a log tree.
//
// Detected is true when at least one root exists, is a directory, and holds
// something other than hidden or empty entries. That is the cheap proxy for
// "Discover would find a source"; a tool whose marker file or database sits at
// the root is detected as soon as the root is populated. Problem is set only
// when Detected is false.
func Detect(p Parser) Status {
	st := Status{Harness: p.Harness(), Roots: append([]string(nil), p.Roots()...)}
	if len(st.Roots) == 0 {
		st.Problem = "no data directory configured"
		return st
	}
	found := false
	var unreadable error
	for _, root := range st.Roots {
		info, err := os.Stat(root)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) && unreadable == nil {
				unreadable = err
			}
			continue
		}
		if !info.IsDir() {
			continue
		}
		found = true
		if dirHasData(root) {
			st.Detected = true
			return st
		}
	}
	switch {
	case unreadable != nil:
		st.Problem = "cannot read data directory: " + unreadable.Error()
	case found:
		st.Problem = "data directory is empty"
	default:
		st.Problem = "data directory not found"
	}
	return st
}

// dirHasData reports whether dir holds a plausible data entry. It reads at most
// the first directory block and ignores dotfiles and empty subdirectories, so a
// root that merely exists (a stale empty config dir) is not called detected.
func dirHasData(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return false
	}
	defer f.Close()
	entries, err := f.ReadDir(32)
	if err != nil && len(entries) == 0 {
		return false
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if !e.IsDir() {
			return true
		}
		if sub, err := os.ReadDir(filepath.Join(dir, e.Name())); err == nil && len(sub) > 0 {
			return true
		}
	}
	return false
}

// ValidateRoot reports whether dir can serve as an explicit data directory for
// p. It is meant for the directory the user picks in settings: nil means the
// directory holds at least one readable source; otherwise the error wraps
// ErrRootMissing, ErrRootNotDir or ErrRootNoData and is descriptive in English.
//
// The check runs p's own Discover against dir, so a directory for a different
// harness (or a parent such as ~/.claude when the parser expects
// ~/.claude/projects) is rejected rather than silently producing no usage.
func ValidateRoot(p Parser, dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return fmt.Errorf("%w: no directory given", ErrRootMissing)
	}
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrRootMissing, dir)
		}
		return fmt.Errorf("%w: %s: %v", ErrRootMissing, dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s", ErrRootNotDir, dir)
	}
	rooted, ok := p.(Rooted)
	if !ok {
		return fmt.Errorf("harness: %s does not support an explicit data directory", p.Harness())
	}
	sources, err := rooted.WithRoots(dir).Discover(context.Background())
	if err != nil {
		return fmt.Errorf("harness: cannot read %s: %w", dir, err)
	}
	if len(sources) == 0 {
		return fmt.Errorf("%w: %s holds no %s logs", ErrRootNoData, dir, p.Harness().DisplayName())
	}
	return nil
}
