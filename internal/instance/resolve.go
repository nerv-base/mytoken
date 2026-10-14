package instance

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

// Fallback is the slice of a harness parser that resolution needs: which
// harness it serves and which roots it would use by default. harness.Parser
// satisfies this interface structurally, so resolution stays testable with
// plain fakes.
type Fallback interface {
	Harness() model.Harness
	Roots() []string
}

// Provenanced is the optional extension of Fallback implemented by every real
// harness parser. It reports each default root together with where it came
// from, which is what lets resolution treat an explicit environment override
// like a configured path (fatal when it does not exist) while a missing
// built-in platform default stays a non-fatal skip. The provenance is asked for
// at call time, so an override set after startup is honoured; a Fallback that
// does not implement it is treated as declaring platform defaults.
type Provenanced interface {
	Fallback
	RootProvenance() ([]harness.Root, error)
}

// DefaultFallbacks returns every registered harness parser as a Fallback, ready
// to pass to Resolve. Each real parser also implements Provenanced, so
// resolution sees the origin of every default root.
func DefaultFallbacks() []Fallback {
	parsers := harness.All()
	out := make([]Fallback, 0, len(parsers))
	for _, p := range parsers {
		out = append(out, p)
	}
	return out
}

// Resolved is one instance after resolution. Roots are canonical absolute paths
// ready to hand to a parser; for disabled and backup-copy entries they are
// reported verbatim because those roots are not required to exist.
type Resolved struct {
	ID       string
	Label    string
	Harness  model.Harness
	Roots    []string
	Source   PathSource
	Kind     Kind
	Disabled bool
}

// Scannable reports whether this instance may be scanned and counted. Disabled
// entries and backup-copies never are.
func (r Resolved) Scannable() bool { return !r.Disabled && r.Kind == KindInstance }

// Problem is one diagnostic reason. Fatal marks a condition that makes a
// configured instance unusable: an explicit path that is missing, a file, or
// unreadable, a duplicated root, overlapping roots for one harness, or a root
// that came from an explicit environment override and does not exist. A
// non-fatal problem is today's silent-skip case (a built-in platform default
// root that does not exist, or one that overlaps another default root and is
// therefore deduplicated) reported with a reason instead of being ignored.
type Problem struct {
	Code    Code
	ID      string
	Harness model.Harness
	Path    string
	Message string
	Fatal   bool
}

// Resolve turns a config plus the parsers' own default roots into the set of
// instances to collect. Explicit entries come first, in config order; injected
// defaults follow, sorted by harness id.
//
// Rules that must not drift:
//   - An explicit entry for a harness replaces that harness's default chain
//     entirely, even when the entry is disabled or is only a backup-copy, so
//     suppression is deterministic.
//   - An invalid explicit path is fatal and never falls back to another dataset.
//   - A default root declared by a provenance-aware parser is judged by its
//     origin. A root that came from an explicit environment override
//     (MYTOKEN_*_DIRS or a harness-specific directory variable) is a deliberate
//     choice: when it is missing, a file, or unreadable the problem is fatal,
//     exactly like an invalid configured path, and it is never replaced by the
//     built-in default. The whole default instance for that harness is refused
//     rather than scanning a partial built-in set, so the error is never hidden.
//     A root that came from a built-in or generic platform
//     default stays a non-fatal skip, because the harness may simply not be
//     installed on this machine. A Fallback with no provenance capability is
//     treated as declaring platform defaults.
//   - A non-blank environment override that lists no usable directory (an empty
//     or separator-only list) is a fatal invalid-root problem. Only a blank
//     (unset or whitespace-only) value counts as absent, in which case the next
//     tier is consulted. A malformed explicit list is never silently converted
//     into the built-in defaults.
//   - Disabled and backup-copy entries are never stat'ed: their archived or
//     offline roots must not have to exist, and must not break unrelated active
//     instances.
//   - Overlap is only fatal within one harness; two harnesses may legitimately
//     read the same directory.
//   - Root paths keep their case; nothing here lowercases them. Sameness and
//     containment are decided by directory identity as well as spelling, so a
//     case alias of the same directory, or of an ancestor, is still caught on a
//     case-insensitive volume while genuinely distinct paths on a case-sensitive
//     volume stay distinct.
//   - Overlapping default roots are deduplicated instead of fatal: the broadest
//     root survives so no subtree is scanned twice and no sibling subtree is
//     lost, and each dropped root is reported non-fatally. Explicit roots are
//     never dropped silently.
//   - Fatal problems are not partial skips: if any returned Problem is fatal the
//     caller must refuse to scan entirely, and the returned Resolved values are
//     diagnostics only.
func Resolve(cfg *Config, fallbacks []Fallback) ([]Resolved, []Problem) {
	if cfg == nil {
		return nil, []Problem{{Code: CodeInvalidConfig, Message: "nil config", Fatal: true}}
	}
	if err := cfg.Validate(); err != nil {
		var ce *ConfigError
		if errors.As(err, &ce) {
			return nil, []Problem{{
				Code: ce.Code, ID: ce.ID, Harness: ce.Harness, Path: ce.Path,
				Message: ce.Message, Fatal: true,
			}}
		}
		return nil, []Problem{{Code: CodeInvalidConfig, Message: err.Error(), Fatal: true}}
	}

	var resolved []Resolved
	var problems []Problem

	claimed := make(map[model.Harness]bool, len(cfg.Entries))
	claimedRoots := make(map[model.Harness][]string, len(cfg.Entries))

	for i := range cfg.Entries {
		e := &cfg.Entries[i]
		claimed[e.Harness] = true

		out := Resolved{
			ID: e.ID, Label: e.Label, Harness: e.Harness,
			Roots: copyStrings(e.Roots), Source: PathUser,
			Kind: e.Kind, Disabled: e.Disabled,
		}
		// Disabled and backup-copy entries are recorded for diagnostics exactly
		// as written: their roots are neither required nor touched.
		if e.Disabled || e.Kind != KindInstance {
			resolved = append(resolved, out)
			continue
		}

		var kept []string
		for _, root := range e.Roots {
			real, prob := canonicalRoot(root)
			if prob != nil {
				prob.ID, prob.Harness = e.ID, e.Harness
				problems = append(problems, *prob)
				continue
			}
			if duplicateRoot(real, kept) {
				problems = append(problems, Problem{
					Code: CodeDuplicateRoot, ID: e.ID, Harness: e.Harness, Path: root,
					Message: "root already listed by this instance", Fatal: true,
				})
				continue
			}
			if overlappingRoot(real, kept) || overlappingRoot(real, claimedRoots[e.Harness]) {
				problems = append(problems, Problem{
					Code: CodeOverlap, ID: e.ID, Harness: e.Harness, Path: root,
					Message: "root overlaps another root of the same harness", Fatal: true,
				})
				continue
			}
			kept = append(kept, real)
		}
		out.Roots = kept
		claimedRoots[e.Harness] = append(claimedRoots[e.Harness], kept...)
		resolved = append(resolved, out)
	}

	for _, fb := range sortedFallbacks(fallbacks, claimed, &problems) {
		resolved = append(resolved, Resolved{
			ID: DefaultID, Harness: fb.harness, Roots: fb.roots,
			Source: fb.source, Kind: KindInstance,
		})
	}

	return resolved, problems
}

type fallbackRoots struct {
	harness model.Harness
	roots   []string
	// source is PathEnv when any surviving root was derived from an explicit
	// environment override, and PathDefault otherwise.
	source PathSource
}

// declaredProvenance reports the default roots a fallback declares, each tagged
// with its origin. Provenance-aware parsers answer live from the current
// environment; a plain Fallback is treated as declaring platform defaults.
func declaredProvenance(fb Fallback, h model.Harness) ([]harness.Root, *Problem) {
	if pv, ok := fb.(Provenanced); ok {
		roots, err := pv.RootProvenance()
		if err != nil {
			return nil, &Problem{
				Code:    CodeInvalidRoot,
				ID:      DefaultID,
				Harness: h,
				Message: err.Error(),
				Fatal:   true,
			}
		}
		return roots, nil
	}
	paths := fb.Roots()
	roots := make([]harness.Root, 0, len(paths))
	for _, p := range paths {
		roots = append(roots, harness.Root{Path: p, Origin: harness.OriginBuiltin})
	}
	return roots, nil
}

// sortedFallbacks keeps the first provider per harness, skips providers whose
// harness already has explicit entries, canonicalises their roots, deduplicates
// roots that would make one subtree count twice and sorts the survivors by
// harness id so output order never depends on caller order.
func sortedFallbacks(fallbacks []Fallback, claimed map[model.Harness]bool, problems *[]Problem) []fallbackRoots {
	seen := make(map[model.Harness]bool, len(fallbacks))
	var out []fallbackRoots
	for _, fb := range fallbacks {
		if fb == nil {
			continue
		}
		h := fb.Harness()
		if h == "" || !h.Known() || claimed[h] || seen[h] {
			continue
		}
		seen[h] = true
		declared, prob := declaredProvenance(fb, h)
		if prob != nil {
			*problems = append(*problems, *prob)
			continue
		}
		if len(declared) == 0 {
			// A parser with no default root contributes no instance at all.
			continue
		}
		origins := make(map[string]harness.RootOrigin, len(declared))
		var canon []string
		envFailed := false
		for _, root := range declared {
			real, prob := canonicalRoot(root.Path)
			if prob != nil {
				fatal := root.Origin == harness.OriginEnv
				message := prob.Message
				if fatal && root.Env != "" {
					message = "explicit root from " + root.Env + ": " + prob.Message
				}
				*problems = append(*problems, Problem{
					Code: prob.Code, ID: DefaultID, Harness: h, Path: prob.Path,
					Message: message, Fatal: fatal,
				})
				if fatal {
					envFailed = true
				}
				continue
			}
			canon = append(canon, real)
			origins[real] = root.Origin
		}
		if envFailed {
			// The user asked for an explicit dataset and it is unusable. Do not
			// materialise a built-in default instance instead: that would scan a
			// different dataset while hiding the error.
			continue
		}
		// A default parser can declare both a directory and one of its
		// ancestors (for example a tool config dir plus a nested projects dir).
		// Scanning both walks the nested subtree twice, so keep only the
		// broadest root: the union of two overlapping directories is the
		// broadest one, so no sibling subtree is lost, and diagnose each drop.
		kept, dropped := dedupeDefaults(canon)
		for _, root := range dropped {
			*problems = append(*problems, Problem{
				Code: CodeOverlap, ID: DefaultID, Harness: h, Path: root,
				Message: "default root overlaps another root of the same harness",
				Fatal:   false,
			})
		}
		source := PathDefault
		for _, root := range kept {
			if origins[root] == harness.OriginEnv {
				source = PathEnv
			}
		}
		out = append(out, fallbackRoots{harness: h, roots: kept, source: source})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].harness < out[j].harness })
	return out
}

// canonicalRoot validates one root an enabled instance wants to scan: it must
// exist, be a directory and be readable. The canonical form resolves symlinks
// without changing letter case.
func canonicalRoot(root string) (string, *Problem) {
	if strings.TrimSpace(root) == "" {
		return "", &Problem{Code: CodeInvalidRoot, Path: root, Message: "empty root path", Fatal: true}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", &Problem{Code: CodeInvalidRoot, Path: root, Message: "resolve absolute path", Fatal: true}
	}
	fi, err := os.Stat(abs)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return "", &Problem{Code: CodeMissingDir, Path: abs, Message: "root does not exist", Fatal: true}
		case errors.Is(err, fs.ErrPermission):
			return "", &Problem{Code: CodePermission, Path: abs, Message: "root is not readable", Fatal: true}
		default:
			return "", &Problem{Code: CodeInvalidRoot, Path: abs, Message: err.Error(), Fatal: true}
		}
	}
	if !fi.IsDir() {
		return "", &Problem{Code: CodeNotDir, Path: abs, Message: "root is not a directory", Fatal: true}
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", &Problem{Code: CodeInvalidRoot, Path: abs, Message: err.Error(), Fatal: true}
	}
	if err := readableDir(real); err != nil {
		return "", &Problem{Code: CodePermission, Path: real, Message: "root is not readable", Fatal: true}
	}
	return real, nil
}

func readableDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// duplicateRoot reports whether real names the same directory as one already
// kept in the same list. Comparison falls back to file identity, so a case
// alias of the same directory is recognised even though EvalSymlinks preserves
// the spelling it was given.
func duplicateRoot(real string, kept []string) bool {
	for _, prev := range kept {
		if sameDir(prev, real) {
			return true
		}
	}
	return false
}

// overlappingRoot reports whether real contains, or is contained by, any
// already-kept root. Containment is decided on path components so "/a/bc" is
// not treated as living inside "/a/b", and then by directory identity so a case
// alias of a directory or of an ancestor is caught on a case-insensitive volume
// without folding case.
func overlappingRoot(real string, kept []string) bool {
	for _, prev := range kept {
		if containsDir(prev, real) || containsDir(real, prev) {
			return true
		}
	}
	return false
}

// dedupeDefaults collapses one fallback's canonical roots so no subtree is
// scanned twice. An exact duplicate, including a case alias of the same
// directory, is dropped silently, matching the historical skip. When one root
// strictly contains another only the broadest survives, and the dropped root is
// returned so the caller can report it non-fatally. Order is preserved.
func dedupeDefaults(roots []string) (kept, dropped []string) {
	for i, r := range roots {
		if duplicateRoot(r, roots[:i]) {
			continue
		}
		contained := false
		for j, other := range roots {
			if i == j || sameDir(other, r) {
				continue
			}
			if containsDir(other, r) {
				contained = true
				break
			}
		}
		if contained {
			dropped = append(dropped, r)
		} else {
			kept = append(kept, r)
		}
	}
	return kept, dropped
}

// sameDir reports whether a and b name the same directory, comparing file
// identity (device and inode) rather than spelling. On a case-insensitive
// volume "Alpha" and "alpha" are the same directory; on a case-sensitive one
// they are not, so nothing here folds case.
func sameDir(a, b string) bool {
	if a == b {
		return true
	}
	afi, err := os.Stat(a)
	if err != nil {
		return false
	}
	bfi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(afi, bfi)
}

// containsDir reports whether outer is inner itself or an ancestor of inner.
// The lexical test is the fast path and is exact on a case-sensitive volume;
// when it misses, inner and each of its ancestors are compared to outer by
// directory identity, which catches a case alias. The walk is bounded by the
// depth of inner and never enumerates a directory.
func containsDir(outer, inner string) bool {
	if pathContains(outer, inner) {
		return true
	}
	for cur := inner; ; {
		if sameDir(cur, outer) {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
}

func pathContains(outer, inner string) bool {
	if outer == inner {
		return true
	}
	if outer == string(filepath.Separator) {
		return true
	}
	return strings.HasPrefix(inner, outer+string(filepath.Separator))
}
