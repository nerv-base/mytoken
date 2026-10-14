package instance_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/zzstar101/mytoken/internal/instance"
	"github.com/zzstar101/mytoken/internal/model"
)

// --- helpers ---------------------------------------------------------------

// fakeFallback stands in for a registered harness parser: harness.Parser
// satisfies instance.Fallback structurally, and tests never touch the global
// registry.
type fakeFallback struct {
	h     model.Harness
	roots []string
}

func (f fakeFallback) Harness() model.Harness { return f.h }
func (f fakeFallback) Roots() []string        { return f.roots }

func findProblem(ps []instance.Problem, code instance.Code) (instance.Problem, bool) {
	for _, p := range ps {
		if p.Code == code {
			return p, true
		}
	}
	return instance.Problem{}, false
}

func canonicalRoot(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", p, err)
	}
	return r
}

func cfgWithEntry(t *testing.T, e instance.Entry) *instance.Config {
	t.Helper()
	cfg := &instance.Config{Version: instance.SchemaVersion, Entries: []instance.Entry{e}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config is invalid: %v", err)
	}
	return cfg
}

func resolvedByID(rs []instance.Resolved, id string) (instance.Resolved, bool) {
	for _, r := range rs {
		if r.ID == id {
			return r, true
		}
	}
	return instance.Resolved{}, false
}

// --- explicit root validation ---------------------------------------------

func TestResolveMissingRootIsFatalAndNeverFallsBack(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	fallbackRoot := mustDir(t, "fallback")
	cfg := newCfg()
	e := mustAdd(t, cfg, instance.AddRequest{Harness: model.ClaudeCode, Roots: []string{missing}, Kind: instance.KindInstance})

	rs, ps := instance.Resolve(cfg, []instance.Fallback{fakeFallback{model.ClaudeCode, []string{fallbackRoot}}})
	p, ok := findProblem(ps, instance.CodeMissingDir)
	if !ok {
		t.Fatalf("want missing-dir problem, got %+v", ps)
	}
	if !p.Fatal {
		t.Fatalf("explicit missing root must be fatal: %+v", p)
	}
	if p.ID != e.ID {
		t.Fatalf("problem id = %q, want %q", p.ID, e.ID)
	}
	for _, r := range rs {
		if r.Source == instance.PathDefault || r.ID == instance.DefaultID {
			t.Fatalf("invalid explicit root fell back to defaults: %+v", r)
		}
	}
	got, ok := resolvedByID(rs, e.ID)
	if !ok {
		t.Fatalf("explicit entry missing from resolution: %+v", rs)
	}
	if len(got.Roots) != 0 {
		t.Fatalf("failed root must not be scanned: %v", got.Roots)
	}
}

func TestResolveFileRootRejected(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := cfgWithEntry(t, instance.Entry{ID: "inst_a", Harness: model.ClaudeCode, Roots: []string{file}, Kind: instance.KindInstance})
	rs, ps := instance.Resolve(cfg, nil)
	if p, ok := findProblem(ps, instance.CodeNotDir); !ok || !p.Fatal {
		t.Fatalf("want fatal not-dir, got %+v", ps)
	}
	if len(rs) != 1 || len(rs[0].Roots) != 0 {
		t.Fatalf("resolved = %+v", rs)
	}
}

func TestResolveUnreadableRootRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The "unreadable directory" concept is expressed here with Unix mode
		// bits (os.Chmod 0o000). Windows ignores chmod and cannot express a
		// directory that exists but cannot be read without an ACL, which a
		// portable test cannot install. The other unusable-root refusals still
		// run on Windows: TestResolveFileRootRejected (a root that is a file)
		// and TestResolveMissingRootIsFatalAndNeverFallsBack (a missing root).
		t.Skip("windows: os.Chmod does not deny directory reads; an unreadable directory requires an ACL")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions are not enforced")
	}
	root := mustDir(t, "locked")
	if err := os.Chmod(root, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(root, 0o700)
	cfg := cfgWithEntry(t, instance.Entry{ID: "inst_a", Harness: model.ClaudeCode, Roots: []string{root}, Kind: instance.KindInstance})
	rs, ps := instance.Resolve(cfg, nil)
	if p, ok := findProblem(ps, instance.CodePermission); !ok || !p.Fatal {
		t.Fatalf("want fatal permission problem, got %+v", ps)
	}
	if len(rs) != 1 || len(rs[0].Roots) != 0 {
		t.Fatalf("resolved = %+v", rs)
	}
}

func TestResolveSymlinkedRootCanonicalizedAndDuplicateRejected(t *testing.T) {
	real := mustDir(t, "real")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfg := cfgWithEntry(t, instance.Entry{ID: "inst_a", Harness: model.ClaudeCode, Roots: []string{real, link}, Kind: instance.KindInstance})
	rs, ps := instance.Resolve(cfg, nil)
	if p, ok := findProblem(ps, instance.CodeDuplicateRoot); !ok || !p.Fatal {
		t.Fatalf("want fatal duplicate-root for symlink to the same dir, got %+v", ps)
	}
	if len(rs) != 1 {
		t.Fatalf("resolved = %+v", rs)
	}
	want := canonicalRoot(t, real)
	if len(rs[0].Roots) != 1 || rs[0].Roots[0] != want {
		t.Fatalf("roots = %v, want [%s]", rs[0].Roots, want)
	}
}

func TestResolveOverlappingRootsWithinOneEntryRejected(t *testing.T) {
	outer := mustDir(t, "outer")
	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := cfgWithEntry(t, instance.Entry{ID: "inst_a", Harness: model.ClaudeCode, Roots: []string{outer, inner}, Kind: instance.KindInstance})
	rs, ps := instance.Resolve(cfg, nil)
	if p, ok := findProblem(ps, instance.CodeOverlap); !ok || !p.Fatal {
		t.Fatalf("want fatal overlap, got %+v", ps)
	}
	if len(rs[0].Roots) != 1 || rs[0].Roots[0] != canonicalRoot(t, outer) {
		t.Fatalf("roots = %v, want only the outer root", rs[0].Roots)
	}
}

func TestResolveOverlapAcrossEntriesOfSameHarness(t *testing.T) {
	outer := mustDir(t, "outer")
	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &instance.Config{Version: instance.SchemaVersion, Entries: []instance.Entry{
		{ID: "inst_a", Harness: model.ClaudeCode, Roots: []string{outer}, Kind: instance.KindInstance},
		{ID: "inst_b", Harness: model.ClaudeCode, Roots: []string{inner}, Kind: instance.KindInstance},
	}}
	rs, ps := instance.Resolve(cfg, nil)
	p, ok := findProblem(ps, instance.CodeOverlap)
	if !ok || !p.Fatal {
		t.Fatalf("want fatal cross-entry overlap, got %+v", ps)
	}
	if p.ID != "inst_b" {
		t.Fatalf("overlap reported for %q, want the later entry", p.ID)
	}
	got, _ := resolvedByID(rs, "inst_b")
	if len(got.Roots) != 0 {
		t.Fatalf("overlapping root must not be scanned twice: %v", got.Roots)
	}
}

// Two different harnesses may legitimately read a shared directory; only
// same-harness overlap double counts, so cross-harness overlap is not fatal.
func TestResolveSharedRootAcrossDifferentHarnessesIsNotFatal(t *testing.T) {
	root := mustDir(t, "shared")
	cfg := &instance.Config{Version: instance.SchemaVersion, Entries: []instance.Entry{
		{ID: "inst_a", Harness: model.ClaudeCode, Roots: []string{root}, Kind: instance.KindInstance},
		{ID: "inst_b", Harness: model.Codex, Roots: []string{root}, Kind: instance.KindInstance},
	}}
	rs, ps := instance.Resolve(cfg, nil)
	for _, p := range ps {
		if p.Fatal {
			t.Fatalf("unexpected fatal problem %+v", p)
		}
	}
	if len(rs) != 2 || len(rs[0].Roots) != 1 || len(rs[1].Roots) != 1 {
		t.Fatalf("resolved = %+v", rs)
	}
}

func TestResolvePreservesRootCase(t *testing.T) {
	root := mustDir(t, "MixedCaseRoot")
	cfg := cfgWithEntry(t, instance.Entry{ID: "inst_a", Harness: model.ClaudeCode, Roots: []string{root}, Kind: instance.KindInstance})
	rs, ps := instance.Resolve(cfg, nil)
	if len(ps) != 0 {
		t.Fatalf("problems = %+v", ps)
	}
	if got := filepath.Base(rs[0].Roots[0]); got != "MixedCaseRoot" {
		t.Fatalf("root case was folded: %q", rs[0].Roots[0])
	}
}

func TestResolveCaseSensitiveDistinctPathsStayDistinct(t *testing.T) {
	base := t.TempDir()
	upper := filepath.Join(base, "Alpha")
	lower := filepath.Join(base, "alpha")
	if err := os.Mkdir(upper, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lower, 0o700); err != nil {
		if fiU, e1 := os.Stat(upper); e1 == nil {
			if fiL, e2 := os.Stat(lower); e2 == nil && os.SameFile(fiU, fiL) {
				t.Skip("case-insensitive filesystem: Alpha and alpha are one directory")
			}
		}
		t.Fatalf("mkdir %s: %v", lower, err)
	}
	cfg := cfgWithEntry(t, instance.Entry{ID: "inst_a", Harness: model.ClaudeCode, Roots: []string{upper, lower}, Kind: instance.KindInstance})
	rs, ps := instance.Resolve(cfg, nil)
	if _, ok := findProblem(ps, instance.CodeDuplicateRoot); ok {
		t.Fatalf("case-variant paths must not be folded into one root: %+v", ps)
	}
	if len(rs[0].Roots) != 2 {
		t.Fatalf("roots = %v, want two distinct paths", rs[0].Roots)
	}
}

// --- case aliases and directory identity -----------------------------------

// requireCaseInsensitiveVolume probes the filesystem at runtime: it creates a
// directory under one spelling and stats it under another. On a case-folding
// volume (macOS APFS/HFS+ by default, Windows NTFS) the differently cased name
// resolves to the same directory; on a case-sensitive one (ext4, APFS case
// sensitive) os.Stat fails with ENOENT.
//
// Tests that simulate a case alias (two spellings of one directory) must call
// this first: on a case-sensitive volume "Alpha" and "alpha" are two unrelated
// directories, so the alias they exercise does not exist and there is no
// behaviour to pin. On a case-folding volume they run in full, and the distinct
// case-sensitive behaviour is covered by
// TestResolveCaseSensitiveDistinctPathsStayDistinct.
func requireCaseInsensitiveVolume(t *testing.T) {
	t.Helper()
	probeDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(probeDir, "case-probe-a"), 0o700); err != nil {
		t.Fatalf("create case probe: %v", err)
	}
	if _, err := os.Stat(filepath.Join(probeDir, "CASE-PROBE-A")); err != nil {
		t.Skip("case-sensitive filesystem: a differently cased name is a distinct path, so case aliases cannot exist")
	}
}

// requireCaseAliasing skips unless alias and existing name the same directory
// because the volume folds case. On a genuinely case-sensitive volume they are
// two distinct directories, and the distinct-paths behaviour is covered by
// TestResolveCaseSensitiveDistinctPathsStayDistinct instead.
func requireCaseAliasing(t *testing.T, existing, alias string) {
	t.Helper()
	fe, err := os.Stat(existing)
	if err != nil {
		t.Fatalf("stat %s: %v", existing, err)
	}
	fa, err := os.Stat(alias)
	if err != nil {
		t.Skipf("case-sensitive volume: %s does not alias %s", alias, existing)
	}
	if !os.SameFile(fe, fa) {
		t.Skipf("case-sensitive volume: %s and %s are distinct directories", existing, alias)
	}
}

func anyFatal(ps []instance.Problem) bool {
	for _, p := range ps {
		if p.Fatal {
			return true
		}
	}
	return false
}

// Two entries of one harness whose roots are the same directory only because
// the volume folds case must not both be scanned: EvalSymlinks preserves the
// spelling it was given, so a lexical comparison alone cannot see that "Alpha"
// and "alpha" are one directory. The rejected entry must be reported as a
// fatal overlap, never dropped silently.
func TestResolveCrossEntryCaseAliasEqualRootsRejected(t *testing.T) {
	requireCaseInsensitiveVolume(t)
	base := t.TempDir()
	upper := filepath.Join(base, "Alpha")
	if err := os.Mkdir(upper, 0o700); err != nil {
		t.Fatal(err)
	}
	lower := filepath.Join(base, "alpha")
	requireCaseAliasing(t, upper, lower)

	cfg := &instance.Config{Version: instance.SchemaVersion, Entries: []instance.Entry{
		{ID: "inst_a", Harness: model.ClaudeCode, Roots: []string{upper}, Kind: instance.KindInstance},
		{ID: "inst_b", Harness: model.ClaudeCode, Roots: []string{lower}, Kind: instance.KindInstance},
	}}
	rs, ps := instance.Resolve(cfg, nil)
	p, ok := findProblem(ps, instance.CodeOverlap)
	if !ok || !p.Fatal {
		t.Fatalf("want fatal overlap for a cross-entry case alias, got %+v", ps)
	}
	if p.ID != "inst_b" {
		t.Fatalf("overlap reported for %q, want the later entry", p.ID)
	}
	if p.Path != lower {
		t.Fatalf("overlap path = %q, want the rejected configured path %q (explicit roots must never be dropped silently)", p.Path, lower)
	}
	first, _ := resolvedByID(rs, "inst_a")
	if len(first.Roots) != 1 || first.Roots[0] != canonicalRoot(t, upper) {
		t.Fatalf("first entry roots = %v, want [%s]", first.Roots, canonicalRoot(t, upper))
	}
	second, _ := resolvedByID(rs, "inst_b")
	if len(second.Roots) != 0 {
		t.Fatalf("a case-alias root must not be scanned a second time: %v", second.Roots)
	}
}

// The same holds when one entry names a directory and another names a subtree
// of it, spelled with a different case on a case-insensitive volume: the
// ancestor relationship is invisible to a lexical path-prefix test.
func TestResolveCrossEntryCaseAliasAncestorRootsRejected(t *testing.T) {
	requireCaseInsensitiveVolume(t)
	base := t.TempDir()
	upper := filepath.Join(base, "Alpha")
	innerAlias := filepath.Join(base, "alpha", "sub")
	if err := os.MkdirAll(innerAlias, 0o700); err != nil {
		t.Fatal(err)
	}
	requireCaseAliasing(t, filepath.Join(upper, "sub"), innerAlias)

	cfg := &instance.Config{Version: instance.SchemaVersion, Entries: []instance.Entry{
		{ID: "inst_a", Harness: model.ClaudeCode, Roots: []string{upper}, Kind: instance.KindInstance},
		{ID: "inst_b", Harness: model.ClaudeCode, Roots: []string{innerAlias}, Kind: instance.KindInstance},
	}}
	rs, ps := instance.Resolve(cfg, nil)
	p, ok := findProblem(ps, instance.CodeOverlap)
	if !ok || !p.Fatal {
		t.Fatalf("want fatal overlap for a cross-entry case-alias subtree, got %+v", ps)
	}
	if p.ID != "inst_b" || p.Path != innerAlias {
		t.Fatalf("overlap problem = %+v, want the later entry and its configured path %q", p, innerAlias)
	}
	second, _ := resolvedByID(rs, "inst_b")
	if len(second.Roots) != 0 {
		t.Fatalf("an ancestor-aliased root must not be scanned twice: %v", second.Roots)
	}
}

// A single entry listing a directory and its case-aliased subtree has the same
// double-counting hazard and must be rejected too.
func TestResolveSameEntryCaseAliasAncestorRootsRejected(t *testing.T) {
	requireCaseInsensitiveVolume(t)
	base := t.TempDir()
	upper := filepath.Join(base, "Alpha")
	innerAlias := filepath.Join(base, "alpha", "sub")
	if err := os.MkdirAll(innerAlias, 0o700); err != nil {
		t.Fatal(err)
	}
	requireCaseAliasing(t, filepath.Join(upper, "sub"), innerAlias)

	cfg := cfgWithEntry(t, instance.Entry{
		ID: "inst_a", Harness: model.ClaudeCode, Roots: []string{upper, innerAlias}, Kind: instance.KindInstance,
	})
	rs, ps := instance.Resolve(cfg, nil)
	if p, ok := findProblem(ps, instance.CodeOverlap); !ok || !p.Fatal {
		t.Fatalf("want fatal overlap for a case-aliased subtree, got %+v", ps)
	}
	want := canonicalRoot(t, upper)
	if len(rs[0].Roots) != 1 || rs[0].Roots[0] != want {
		t.Fatalf("roots = %v, want only [%s]", rs[0].Roots, want)
	}
}

// Default roots that overlap are deduplicated instead of scanning a subtree
// twice, but the drop is diagnosed (non-fatally, like every default-root
// problem) rather than being silent.
func TestResolveFallbackOverlappingRootsDeduplicated(t *testing.T) {
	outer := mustDir(t, "outer")
	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &instance.Config{Version: instance.SchemaVersion}
	rs, ps := instance.Resolve(cfg, []instance.Fallback{fakeFallback{model.ClaudeCode, []string{outer, inner}}})
	if anyFatal(ps) {
		t.Fatalf("a default-root overlap must stay non-fatal: %+v", ps)
	}
	if len(rs) != 1 {
		t.Fatalf("resolved = %+v", rs)
	}
	want := canonicalRoot(t, outer)
	if len(rs[0].Roots) != 1 || rs[0].Roots[0] != want {
		t.Fatalf("fallback roots = %v, want only [%s] so the subtree is not scanned twice", rs[0].Roots, want)
	}
	if p, ok := findProblem(ps, instance.CodeOverlap); !ok || p.Fatal {
		t.Fatalf("want a non-fatal overlap problem for the dropped default root, got %+v", ps)
	}
}

// The broadest default root must survive whichever order the parser declared
// its roots in: keeping the narrower root when it came first would silently stop
// watching every sibling subtree of the ancestor.
func TestResolveFallbackBroadRootKeptRegardlessOfDeclaredOrder(t *testing.T) {
	outer := mustDir(t, "outer")
	inner := filepath.Join(outer, "inner")
	sibling := filepath.Join(outer, "sibling")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &instance.Config{Version: instance.SchemaVersion}
	rs, ps := instance.Resolve(cfg, []instance.Fallback{fakeFallback{model.ClaudeCode, []string{inner, outer}}})
	if anyFatal(ps) {
		t.Fatalf("default-root dedupe must stay non-fatal: %+v", ps)
	}
	want := canonicalRoot(t, outer)
	if len(rs) != 1 || len(rs[0].Roots) != 1 || rs[0].Roots[0] != want {
		t.Fatalf("fallback roots = %+v, want only [%s] so sibling subtrees stay watched", rs, want)
	}
	if p, ok := findProblem(ps, instance.CodeOverlap); !ok || p.Fatal {
		t.Fatalf("want a non-fatal overlap problem for the dropped narrower root, got %+v", ps)
	}
}

// The same dedupe must see through a case alias on a case-insensitive volume:
// an equal alias is caught by the inode check, but an aliased subtree is only
// caught by walking ancestor identity, because the retained spelling of the
// ancestor and the declared spelling of the subtree differ in case.
func TestResolveFallbackCaseAliasRootsDeduplicated(t *testing.T) {
	requireCaseInsensitiveVolume(t)
	base := t.TempDir()
	upper := filepath.Join(base, "Alpha")
	if err := os.Mkdir(upper, 0o700); err != nil {
		t.Fatal(err)
	}
	lower := filepath.Join(base, "alpha")
	innerAlias := filepath.Join(base, "alpha", "sub")
	if err := os.MkdirAll(innerAlias, 0o700); err != nil {
		t.Fatal(err)
	}
	requireCaseAliasing(t, upper, lower)

	t.Run("equal alias", func(t *testing.T) {
		cfg := &instance.Config{Version: instance.SchemaVersion}
		rs, ps := instance.Resolve(cfg, []instance.Fallback{fakeFallback{model.ClaudeCode, []string{upper, lower}}})
		if anyFatal(ps) {
			t.Fatalf("default-root dedupe must stay non-fatal: %+v", ps)
		}
		want := canonicalRoot(t, upper)
		if len(rs) != 1 || len(rs[0].Roots) != 1 || rs[0].Roots[0] != want {
			t.Fatalf("fallback roots = %+v, want only [%s]", rs, want)
		}
	})

	t.Run("aliased subtree", func(t *testing.T) {
		// No root is declared with the "alpha" spelling, so a lexical prefix
		// test cannot see that innerAlias lives under upper.
		cfg := &instance.Config{Version: instance.SchemaVersion}
		rs, ps := instance.Resolve(cfg, []instance.Fallback{fakeFallback{model.ClaudeCode, []string{upper, innerAlias}}})
		if anyFatal(ps) {
			t.Fatalf("default-root dedupe must stay non-fatal: %+v", ps)
		}
		want := canonicalRoot(t, upper)
		if len(rs) != 1 || len(rs[0].Roots) != 1 || rs[0].Roots[0] != want {
			t.Fatalf("fallback roots = %+v, want only [%s] so the aliased subtree is not scanned twice", rs, want)
		}
		if p, ok := findProblem(ps, instance.CodeOverlap); !ok || p.Fatal {
			t.Fatalf("want a non-fatal overlap problem for the dropped aliased subtree, got %+v", ps)
		}
	})
}

// Dedupe is per harness: two harnesses may legitimately read the same default
// directory, so a shared root is never removed from the second harness.
func TestResolveFallbackOverlapDedupedPerHarnessOnly(t *testing.T) {
	outer := mustDir(t, "outer")
	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &instance.Config{Version: instance.SchemaVersion}
	rs, _ := instance.Resolve(cfg, []instance.Fallback{
		fakeFallback{model.ClaudeCode, []string{outer, inner}},
		fakeFallback{model.Codex, []string{outer}},
	})
	want := canonicalRoot(t, outer)
	for _, r := range rs {
		if len(r.Roots) != 1 || r.Roots[0] != want {
			t.Fatalf("harness %s roots = %v, want [%s]", r.Harness, r.Roots, want)
		}
	}
	if len(rs) != 2 {
		t.Fatalf("resolved = %+v, want one instance per harness", rs)
	}
}

// Fatal problems stay fatal: a configured root that is invalid or overlaps
// another of the same harness must make the caller refuse the scan, while a
// missing tier-3 default stays a skip. Resolve still returns the entries that
// did validate so diagnostics can name them.
func TestResolveFatalContractForConfiguredVersusDefaultRoots(t *testing.T) {
	t.Run("configured invalid root is fatal", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "gone")
		cfg := cfgWithEntry(t, instance.Entry{ID: "inst_a", Harness: model.ClaudeCode, Roots: []string{missing}, Kind: instance.KindInstance})
		_, ps := instance.Resolve(cfg, nil)
		if !anyFatal(ps) {
			t.Fatalf("an invalid explicit root must force the caller to refuse the scan: %+v", ps)
		}
	})
	t.Run("missing default root is not fatal", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "gone")
		cfg := &instance.Config{Version: instance.SchemaVersion}
		_, ps := instance.Resolve(cfg, []instance.Fallback{fakeFallback{model.ClaudeCode, []string{missing}}})
		if anyFatal(ps) {
			t.Fatalf("a missing tier-3 default stays a skip: %+v", ps)
		}
	})
	t.Run("partial results accompany a fatal problem", func(t *testing.T) {
		ok := mustDir(t, "ok")
		missing := filepath.Join(t.TempDir(), "gone")
		cfg := &instance.Config{Version: instance.SchemaVersion, Entries: []instance.Entry{
			{ID: "inst_ok", Harness: model.ClaudeCode, Roots: []string{ok}, Kind: instance.KindInstance},
			{ID: "inst_bad", Harness: model.ClaudeCode, Roots: []string{missing}, Kind: instance.KindInstance},
		}}
		rs, ps := instance.Resolve(cfg, nil)
		if !anyFatal(ps) {
			t.Fatalf("want a fatal problem: %+v", ps)
		}
		got, found := resolvedByID(rs, "inst_ok")
		if !found || len(got.Roots) != 1 {
			t.Fatalf("the valid entry must still be returned for diagnostics: %+v", rs)
		}
	})
}

// --- disabled and backup-copy entries -------------------------------------

func TestResolveDisabledEntryNeedsNoRootsAndSuppressesDefaults(t *testing.T) {
	activeRoot := mustDir(t, "active")
	offline := filepath.Join(t.TempDir(), "offline-archive")
	fallbackRoot := mustDir(t, "fallback")
	cfg := &instance.Config{Version: instance.SchemaVersion, Entries: []instance.Entry{
		{ID: "inst_active", Harness: model.ClaudeCode, Roots: []string{activeRoot}, Kind: instance.KindInstance},
		{ID: "inst_offline", Harness: model.ClaudeCode, Roots: []string{offline}, Kind: instance.KindInstance, Disabled: true},
	}}
	rs, ps := instance.Resolve(cfg, []instance.Fallback{fakeFallback{model.ClaudeCode, []string{fallbackRoot}}})
	if len(ps) != 0 {
		t.Fatalf("disabled entries must not require their roots to exist: %+v", ps)
	}
	if _, ok := resolvedByID(rs, instance.DefaultID); ok {
		t.Fatalf("an entry naming the harness must suppress its default chain: %+v", rs)
	}
	active, _ := resolvedByID(rs, "inst_active")
	if !active.Scannable() || len(active.Roots) != 1 || active.Roots[0] != canonicalRoot(t, activeRoot) {
		t.Fatalf("active entry = %+v", active)
	}
	off, ok := resolvedByID(rs, "inst_offline")
	if !ok {
		t.Fatalf("disabled entry must still be reported for diagnostics: %+v", rs)
	}
	if off.Scannable() {
		t.Fatalf("disabled entry must not be scannable: %+v", off)
	}
	if !off.Disabled {
		t.Fatalf("disabled flag lost in resolution: %+v", off)
	}
	if len(off.Roots) != 1 || off.Roots[0] != offline {
		t.Fatalf("offline roots must be reported verbatim, not stat'ed: %v", off.Roots)
	}
}

func TestResolveBackupCopyStoredNotScannable(t *testing.T) {
	archived := filepath.Join(t.TempDir(), "archived", "gone")
	cfg := cfgWithEntry(t, instance.Entry{ID: "inst_copy", Label: "copy", Harness: model.ClaudeCode, Roots: []string{archived}, Kind: instance.KindBackupCopy})
	rs, ps := instance.Resolve(cfg, nil)
	if len(ps) != 0 {
		t.Fatalf("backup-copy roots must not have to exist: %+v", ps)
	}
	if len(rs) != 1 {
		t.Fatalf("resolved = %+v", rs)
	}
	if rs[0].Scannable() {
		t.Fatalf("backup-copy must never be scanned or counted: %+v", rs[0])
	}
	if rs[0].Kind != instance.KindBackupCopy {
		t.Fatalf("kind = %q", rs[0].Kind)
	}
	if len(rs[0].Roots) != 1 || rs[0].Roots[0] != archived {
		t.Fatalf("roots = %v", rs[0].Roots)
	}
}

// --- fallback injection ----------------------------------------------------

func TestResolveInjectsFallbackDefaultsWhenConfigIsAbsent(t *testing.T) {
	present := mustDir(t, "codex-default")
	missing := filepath.Join(t.TempDir(), "claude-default-gone")
	cfg := &instance.Config{Version: instance.SchemaVersion}
	rs, ps := instance.Resolve(cfg, []instance.Fallback{
		fakeFallback{model.Codex, []string{present}},
		fakeFallback{model.ClaudeCode, []string{missing}},
	})
	if len(rs) != 2 {
		t.Fatalf("resolved = %+v", rs)
	}
	for _, r := range rs {
		if r.ID != instance.DefaultID || r.Source != instance.PathDefault || r.Kind != instance.KindInstance || r.Disabled {
			t.Fatalf("default instance = %+v", r)
		}
		if !r.Scannable() {
			t.Fatalf("default instance must stay scannable: %+v", r)
		}
	}
	var codex instance.Resolved
	for _, r := range rs {
		if r.Harness == model.Codex {
			codex = r
		}
	}
	if codex.ID != instance.DefaultID || len(codex.Roots) != 1 || codex.Roots[0] != canonicalRoot(t, present) {
		t.Fatalf("codex default = %+v", codex)
	}
	p, ok := findProblem(ps, instance.CodeMissingDir)
	if !ok {
		t.Fatalf("missing default root must be diagnosed: %+v", ps)
	}
	if p.Fatal {
		t.Fatalf("a missing tier-3 default is today's silent skip, not fatal: %+v", p)
	}
}

func TestResolveExplicitEntrySuppressesFallbackChain(t *testing.T) {
	explicit := mustDir(t, "explicit")
	fallbackRoot := mustDir(t, "fallback")
	cfg := newCfg()
	mustAdd(t, cfg, instance.AddRequest{Harness: model.ClaudeCode, Roots: []string{explicit}, Kind: instance.KindInstance})
	rs, ps := instance.Resolve(cfg, []instance.Fallback{fakeFallback{model.ClaudeCode, []string{fallbackRoot}}})
	if len(ps) != 0 {
		t.Fatalf("problems = %+v", ps)
	}
	if len(rs) != 1 {
		t.Fatalf("explicit entries replace the default chain, got %+v", rs)
	}
	if rs[0].Source != instance.PathUser || rs[0].ID == instance.DefaultID {
		t.Fatalf("resolved = %+v", rs)
	}
	canon := canonicalRoot(t, explicit)
	for _, r := range rs {
		for _, root := range r.Roots {
			if root == canonicalRoot(t, fallbackRoot) {
				t.Fatalf("fallback root leaked into an explicit configuration: %+v", r)
			}
		}
	}
	if len(rs[0].Roots) != 1 || rs[0].Roots[0] != canon {
		t.Fatalf("roots = %v, want [%s]", rs[0].Roots, canon)
	}
}

func TestResolveOrderIsDeterministic(t *testing.T) {
	dirA := mustDir(t, "a")
	dirB := mustDir(t, "b")
	cfg := &instance.Config{Version: instance.SchemaVersion, Entries: []instance.Entry{
		{ID: "inst_z", Harness: model.Codex, Roots: []string{dirB}, Kind: instance.KindInstance},
		{ID: "inst_a", Harness: model.ClaudeCode, Roots: []string{dirA}, Kind: instance.KindInstance},
	}}
	fbs := []instance.Fallback{
		fakeFallback{model.Gemini, []string{dirA}},
		fakeFallback{model.ClaudeCode, []string{dirB}},
	}
	var first []string
	for i := 0; i < 3; i++ {
		rs, _ := instance.Resolve(cfg, fbs)
		var ids []string
		for _, r := range rs {
			ids = append(ids, string(r.Harness)+"/"+r.ID)
		}
		if i == 0 {
			first = ids
			continue
		}
		if len(ids) != len(first) {
			t.Fatalf("unstable length: %v vs %v", first, ids)
		}
		for j := range ids {
			if ids[j] != first[j] {
				t.Fatalf("unstable order: %v vs %v", first, ids)
			}
		}
	}
	// Explicit entries keep the order they have in the config file; injected
	// defaults follow, sorted by harness id so the output never depends on map
	// iteration order.
	want := []string{"codex/inst_z", "claude-code/inst_a", "gemini/" + instance.DefaultID}
	for i := range want {
		if first[i] != want[i] {
			t.Fatalf("order = %v, want entries in config order then defaults sorted by harness (%v)", first, want)
		}
	}
}

// --- identity, labels, isolation, invalid input ---------------------------

func TestResolveLabelsAreNotIdentity(t *testing.T) {
	a, b := mustDir(t, "a"), mustDir(t, "b")
	cfg := newCfg()
	ea := mustAdd(t, cfg, instance.AddRequest{Label: "same label", Harness: model.ClaudeCode, Roots: []string{a}, Kind: instance.KindInstance})
	eb := mustAdd(t, cfg, instance.AddRequest{Label: "same label", Harness: model.ClaudeCode, Roots: []string{b}, Kind: instance.KindInstance})
	rs, ps := instance.Resolve(cfg, nil)
	if len(ps) != 0 {
		t.Fatalf("problems = %+v", ps)
	}
	if ea.ID == eb.ID {
		t.Fatal("labels must not be identity")
	}
	ra, _ := resolvedByID(rs, ea.ID)
	rb, _ := resolvedByID(rs, eb.ID)
	if ra.Label != "same label" || rb.Label != "same label" {
		t.Fatalf("labels = %q, %q", ra.Label, rb.Label)
	}
	if ra.Roots[0] == rb.Roots[0] {
		t.Fatalf("distinct roots collapsed: %v", ra.Roots)
	}
}

func TestResolveCopiesOutputRootsSlice(t *testing.T) {
	root := mustDir(t, "root")
	cfg := cfgWithEntry(t, instance.Entry{ID: "inst_a", Harness: model.ClaudeCode, Roots: []string{root}, Kind: instance.KindInstance})
	first, _ := instance.Resolve(cfg, nil)
	first[0].Roots[0] = "/mutated"
	if cfg.Entries[0].Roots[0] != root {
		t.Fatalf("Resolve aliased config roots: %v", cfg.Entries[0].Roots)
	}
	second, _ := instance.Resolve(cfg, nil)
	if second[0].Roots[0] != canonicalRoot(t, root) {
		t.Fatalf("Resolve results share memory: %v", second[0].Roots)
	}
}

func TestResolveInvalidConfigReturnsFatalProblem(t *testing.T) {
	cfg := &instance.Config{Version: instance.SchemaVersion, Entries: []instance.Entry{
		{ID: instance.DefaultID, Harness: model.ClaudeCode, Roots: []string{"/tmp/a"}, Kind: instance.KindInstance},
	}}
	rs, ps := instance.Resolve(cfg, nil)
	if len(rs) != 0 {
		t.Fatalf("invalid config must not resolve: %+v", rs)
	}
	p, ok := findProblem(ps, instance.CodeReservedID)
	if !ok || !p.Fatal {
		t.Fatalf("want fatal reserved-id problem, got %+v", ps)
	}
}

// harness.Parser satisfies instance.Fallback structurally; this asserts the
// adapter the downstream wiring will use compiles and behaves.
func TestFallbackInterfaceIsSatisfiedByParserShapedType(t *testing.T) {
	var _ instance.Fallback = fakeFallback{}
	cfg := &instance.Config{Version: instance.SchemaVersion}
	rs, _ := instance.Resolve(cfg, []instance.Fallback{fakeFallback{model.Zed, nil}})
	if len(rs) != 0 {
		t.Fatalf("a fallback with no roots contributes no instance: %+v", rs)
	}
}
