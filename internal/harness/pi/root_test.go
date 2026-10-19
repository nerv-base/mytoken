package pi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

// This file pins Oh My Pi (OMP) root compatibility: the registry has no OMP
// parser of its own, so the Pi parser has to find an OMP install's sessions
// next to its own. Only the variables the official OMP path resolver reads are
// supported (packages/utils/src/dirs.ts in can1357/oh-my-pi):
//
//	PI_HOME                 MyToken's own explicit base (unchanged)
//	PI_CODING_AGENT_DIR     the agent directory, default profile only
//	OMP_PROFILE, PI_PROFILE named profile, OMP_PROFILE wins
//	XDG_DATA_HOME           redirects to $XDG_DATA_HOME/omp, gated on that
//	                        directory existing
//
// The tests never touch the real home: every case redirects HOME into a temp
// tree and blanks the variables the parser may read.
//
// Test conventions: the harness package registers parsers during init, so
// nothing here registers a parser or depends on package state.

// isolateHome points HOME/USERPROFILE at a fresh temp directory and clears every
// variable the Pi/OMP root resolver may read, so a developer's own environment
// cannot leak into a case.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, name := range []string{
		"PI_HOME", "PI_CODING_AGENT_DIR", "PI_CONFIG_DIR", "OMP_PROFILE", "PI_PROFILE", "XDG_DATA_HOME", "MYTOKEN_PI_DIRS",
		"OMO_CODING_AGENT_DIR", "SENPI_CODING_AGENT_DIR",
	} {
		t.Setenv(name, "")
	}
	return home
}

// piRootProvenance answers the Pi parser's live default-root provenance. It
// asserts the registry shape every case depends on: pi is registered and
// implements harness.Provenance.
func piRootProvenance(t *testing.T) []harness.Root {
	t.Helper()
	p, ok := harness.Get(model.Pi)
	if !ok {
		t.Fatal("pi is not registered")
	}
	pv, ok := p.(harness.Provenance)
	if !ok {
		t.Fatalf("pi parser %T does not implement harness.Provenance", p)
	}
	roots, err := pv.RootProvenance()
	if err != nil {
		t.Fatalf("RootProvenance: %v", err)
	}
	return roots
}

// rootWithPath returns the root whose path is exactly path.
func rootWithPath(roots []harness.Root, path string) (harness.Root, bool) {
	for _, r := range roots {
		if r.Path == path {
			return r, true
		}
	}
	return harness.Root{}, false
}

// requireRoot fails unless roots contains path exactly once.
func requireRoot(t *testing.T, roots []harness.Root, path string) harness.Root {
	t.Helper()
	found, ok := rootWithPath(roots, path)
	if !ok {
		t.Fatalf("roots lack %q: %+v", path, roots)
	}
	n := 0
	for _, r := range roots {
		if r.Path == path {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("roots contain %q %d times, want once: %+v", path, n, roots)
	}
	return found
}

// requireNoRoot fails when roots contains path.
func requireNoRoot(t *testing.T, roots []harness.Root, path string) {
	t.Helper()
	if r, ok := rootWithPath(roots, path); ok {
		t.Fatalf("roots must not contain %q, got %+v", path, r)
	}
}

// requireAbsent fails when any root names path.
func requireAbsent(t *testing.T, roots []harness.Root, path string) {
	t.Helper()
	for _, r := range roots {
		if r.Path == path {
			t.Fatalf("root %+v must be left out when the directory is absent", r)
		}
	}
}

// mkdirAll creates dir and its parents.
func mkdirAll(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	return dir
}

// TestRootsOMPBuiltinIsDiscoveredBeyondPiDefault pins the offline default: with
// no override set, an OMP install under the platform default
// ~/.omp/agent/sessions is watched alongside the unchanged ~/.pi/agent/sessions
// root, and the pid file a running OMP leaves in that tree is not a root.
func TestRootsOMPBuiltinIsDiscoveredBeyondPiDefault(t *testing.T) {
	home := isolateHome(t)

	roots := piRootProvenance(t)

	wantPi := filepath.Join(home, ".pi", "agent", "sessions")
	if r := requireRoot(t, roots, wantPi); r.Origin != harness.OriginBuiltin || r.Env != "" {
		t.Errorf("pi default root %+v, want builtin origin with no env", r)
	}
	wantOMP := filepath.Join(home, ".omp", "agent", "sessions")
	if r := requireRoot(t, roots, wantOMP); r.Origin != harness.OriginBuiltin || r.Env != "" {
		t.Errorf("omp default root %+v, want builtin origin with no env", r)
	}
	// The pid file next to the sessions directory is not a log directory.
	requireAbsent(t, roots, filepath.Join(home, ".omp", "agent"))
	requireAbsent(t, roots, home)
}

// TestRootsAgentDirOverride pins PI_CODING_AGENT_DIR: the official agent-
// directory override, which takes effect for the default profile (no
// OMP_PROFILE/PI_PROFILE) and points at <dir>/sessions.
func TestRootsAgentDirOverride(t *testing.T) {
	isolateHome(t)
	agent := mkdirAll(t, filepath.Join(t.TempDir(), "custom-agent"))
	t.Setenv("PI_CODING_AGENT_DIR", agent)

	want := filepath.Join(agent, "sessions")
	r := requireRoot(t, piRootProvenance(t), want)
	if r.Origin != harness.OriginEnv || r.Env != "PI_CODING_AGENT_DIR" {
		t.Errorf("root %+v, want env origin tagged PI_CODING_AGENT_DIR", r)
	}
}

// TestRootsAgentDirOverrideIsDirectoryExistenceAgnostic pins that a
// PI_CODING_AGENT_DIR which does not exist yet is still reported: OMP creates
// the agent directory lazily, so an override is a user request, not a probe.
func TestRootsAgentDirOverrideIsDirectoryExistenceAgnostic(t *testing.T) {
	isolateHome(t)
	agent := filepath.Join(t.TempDir(), "not-created-yet")
	t.Setenv("PI_CODING_AGENT_DIR", agent)

	requireRoot(t, piRootProvenance(t), filepath.Join(agent, "sessions"))
}

// TestRootsXDGDataHomeGatedOnOMPDirectory pins the XDG redirect and its gate:
// the official resolver uses $XDG_DATA_HOME/omp only when that directory
// exists, and then sessions sit directly under it (the agent/ level is
// flattened away).
func TestRootsXDGDataHomeGatedOnOMPDirectory(t *testing.T) {
	t.Run("existing omp directory redirects", func(t *testing.T) {
		home := isolateHome(t)
		xdg := mkdirAll(t, filepath.Join(t.TempDir(), "xdg-data"))
		// The XDG app root is $XDG_DATA_HOME/omp — upstream's APP_NAME ("omp"),
		// NOT the dot-prefixed config directory name (".omp").
		mkdirAll(t, filepath.Join(xdg, "omp"))
		t.Setenv("XDG_DATA_HOME", xdg)

		roots := piRootProvenance(t)
		want := filepath.Join(xdg, "omp", "sessions")
		if r := requireRoot(t, roots, want); r.Origin != harness.OriginBuiltin || r.Env != "" {
			t.Errorf("xdg root %+v, want builtin origin with no env", r)
		}
		// The redirect replaces the default-profile agent root.
		requireNoRoot(t, roots, filepath.Join(home, ".omp", "agent", "sessions"))
		// Unrelated directories are never scanned.
		requireAbsent(t, roots, filepath.Join(xdg, "omp"))
		requireAbsent(t, roots, home)
	})

	t.Run("absent omp directory does not redirect", func(t *testing.T) {
		home := isolateHome(t)
		xdg := filepath.Join(t.TempDir(), "xdg-data")
		t.Setenv("XDG_DATA_HOME", xdg)

		roots := piRootProvenance(t)
		requireNoRoot(t, roots, filepath.Join(xdg, "omp", "sessions"))
		requireRoot(t, roots, filepath.Join(home, ".omp", "agent", "sessions"))
	})
}

// TestRootsXDGDoesNotReplaceExplicitAgentDir pins the documented precedence:
// OMP's own resolver consults PI_CODING_AGENT_DIR in default mode, so an
// explicit agent directory is honoured even while an XDG redirect is available.
func TestRootsXDGDoesNotReplaceExplicitAgentDir(t *testing.T) {
	isolateHome(t)
	agent := mkdirAll(t, filepath.Join(t.TempDir(), "custom-agent"))
	t.Setenv("PI_CODING_AGENT_DIR", agent)
	xdg := mkdirAll(t, filepath.Join(t.TempDir(), "xdg-data"))
	mkdirAll(t, filepath.Join(xdg, "omp"))
	t.Setenv("XDG_DATA_HOME", xdg)

	roots := piRootProvenance(t)
	if r := requireRoot(t, roots, filepath.Join(agent, "sessions")); r.Env != "PI_CODING_AGENT_DIR" {
		t.Errorf("root %+v, want the PI_CODING_AGENT_DIR root", r)
	}
	requireNoRoot(t, roots, filepath.Join(xdg, "omp", "sessions"))
}

// TestRootsProfileAgentDir pins the named-profile layout:
// ~/.omp/profiles/<name>/agent/sessions, selected by OMP_PROFILE and by the
// legacy PI_PROFILE fallback.
func TestRootsProfileAgentDir(t *testing.T) {
	t.Run("omp profile", func(t *testing.T) {
		home := isolateHome(t)
		t.Setenv("OMP_PROFILE", "work")

		want := filepath.Join(home, ".omp", "profiles", "work", "agent", "sessions")
		r := requireRoot(t, piRootProvenance(t), want)
		if r.Origin != harness.OriginEnv || r.Env != "OMP_PROFILE" {
			t.Errorf("profile root %+v, want env origin tagged OMP_PROFILE", r)
		}
	})

	t.Run("pi profile fallback", func(t *testing.T) {
		home := isolateHome(t)
		t.Setenv("PI_PROFILE", "work")

		want := filepath.Join(home, ".omp", "profiles", "work", "agent", "sessions")
		r := requireRoot(t, piRootProvenance(t), want)
		if r.Origin != harness.OriginEnv || r.Env != "PI_PROFILE" {
			t.Errorf("profile root %+v, want env origin tagged PI_PROFILE", r)
		}
	})
}

// TestRootsProfilePrecedence pins OMP_PROFILE over PI_PROFILE and the official
// rule that "default" is the default profile, not a directory name.
func TestRootsProfilePrecedence(t *testing.T) {
	t.Run("omp profile wins", func(t *testing.T) {
		home := isolateHome(t)
		t.Setenv("OMP_PROFILE", "alpha")
		t.Setenv("PI_PROFILE", "beta")

		roots := piRootProvenance(t)
		requireRoot(t, roots, filepath.Join(home, ".omp", "profiles", "alpha", "agent", "sessions"))
		requireAbsent(t, roots, filepath.Join(home, ".omp", "profiles", "beta", "agent", "sessions"))
	})

	t.Run("default sentinel is not a profile", func(t *testing.T) {
		home := isolateHome(t)
		t.Setenv("OMP_PROFILE", "default")

		requireRoot(t, piRootProvenance(t), filepath.Join(home, ".omp", "agent", "sessions"))
	})

	t.Run("blank omp profile falls back to pi profile", func(t *testing.T) {
		home := isolateHome(t)
		t.Setenv("OMP_PROFILE", "   ")
		t.Setenv("PI_PROFILE", "legacy")

		r := requireRoot(t, piRootProvenance(t), filepath.Join(home, ".omp", "profiles", "legacy", "agent", "sessions"))
		if r.Env != "PI_PROFILE" {
			t.Errorf("profile root %+v, want it tagged PI_PROFILE", r)
		}
	})
}

// TestRootsProfileRejectsPathTraversal pins that a profile name is never
// trusted as a path: OMP validates it against ^[a-z0-9][a-z0-9._-]{0,63}$, so
// separators, "..", a trailing dot and an over-long name cannot escape the
// profiles directory.
func TestRootsProfileRejectsPathTraversal(t *testing.T) {
	for _, tc := range []struct{ name, profile string }{
		{"parent", ".."},
		{"parent segment", "../../evil"},
		{"absolute", "/tmp/evil"},
		{"separator", "a/b"},
		{"backslash", `a\b`},
		{"leading dot", ".hidden"},
		{"trailing dot", "work."},
		{"empty segment", "..evil"},
		{"too long", strings.Repeat("a", 200)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := isolateHome(t)
			t.Setenv("OMP_PROFILE", tc.profile)

			roots := piRootProvenance(t)
			// A rejected profile must not be silently turned into a directory
			// named after the raw value: no root may sit under profiles/<raw>.
			// (The rejected name falls back to the default profile, so the
			// plain ~/.omp/agent/sessions root does appear.)
			escaped := filepath.Join(home, ".omp", "profiles") + string(filepath.Separator)
			for _, r := range roots {
				if strings.HasPrefix(r.Path, escaped) {
					t.Fatalf("profile %q escaped into %q; roots=%+v", tc.profile, r.Path, roots)
				}
			}
			// ... and no root may escape HOME.
			for _, r := range roots {
				if outsideHome(home, r.Path) {
					t.Fatalf("profile %q produced root %q outside HOME %q; roots=%+v",
						tc.profile, r.Path, home, roots)
				}
			}
		})
	}
}

// outsideHome reports whether an absolute path escapes home.
func outsideHome(home, p string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(home), filepath.Clean(p))
	if err != nil {
		return true
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// TestRootsMyTokenPiDirsOverride pins the MyToken-side explicit root list: like
// every other parser it wins over every OMP-derived base and names the roots
// verbatim.
func TestRootsMyTokenPiDirsOverride(t *testing.T) {
	isolateHome(t)
	t.Setenv("OMP_PROFILE", "work")
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(t.TempDir(), "agent"))
	a, b := t.TempDir(), t.TempDir()
	t.Setenv("MYTOKEN_PI_DIRS", a+string(filepath.ListSeparator)+b)

	roots := piRootProvenance(t)
	if len(roots) != 2 {
		t.Fatalf("MYTOKEN_PI_DIRS roots = %+v, want exactly the two requested", roots)
	}
	if r := requireRoot(t, roots, a); r.Origin != harness.OriginEnv || r.Env != "MYTOKEN_PI_DIRS" {
		t.Errorf("root %+v, want env origin tagged MYTOKEN_PI_DIRS", r)
	}
	requireRoot(t, roots, b)
}

// TestRootsConfigDirOverride pins PI_CONFIG_DIR, the config directory name the
// official resolver reads instead of its ".omp" default.
func TestRootsConfigDirOverride(t *testing.T) {
	home := isolateHome(t)
	t.Setenv("PI_CONFIG_DIR", ".omp-dev")

	roots := piRootProvenance(t)
	requireRoot(t, roots, filepath.Join(home, ".omp-dev", "agent", "sessions"))
	requireNoRoot(t, roots, filepath.Join(home, ".omp", "agent", "sessions"))
}

// TestRootsBlankConfigDirIsUnset pins that a blank PI_CONFIG_DIR counts as
// unset: it must not collapse the config root onto HOME itself.
func TestRootsBlankConfigDirIsUnset(t *testing.T) {
	home := isolateHome(t)
	t.Setenv("PI_CONFIG_DIR", "   ")

	roots := piRootProvenance(t)
	requireRoot(t, roots, filepath.Join(home, ".omp", "agent", "sessions"))
	requireNoRoot(t, roots, filepath.Join(home, "agent", "sessions"))
}

// TestRootsDedupesOverlappingRoots pins the dedup contract: a PI_CODING_AGENT_DIR
// that resolves to the same sessions directory as Pi's own default yields one
// root, keeping the first occurrence's provenance.
func TestRootsDedupesOverlappingRoots(t *testing.T) {
	home := isolateHome(t)
	// PI_HOME's base is the Pi install itself, so its sessions directory is
	// exactly the built-in ~/.pi/agent/sessions root when the environment is relative.
	t.Setenv("PI_HOME", filepath.Join(home, ".pi", "agent"))

	roots := piRootProvenance(t)
	want := filepath.Join(home, ".pi", "agent", "sessions")
	if r := requireRoot(t, roots, want); r.Origin != harness.OriginEnv || r.Env != "PI_HOME" {
		t.Errorf("root %+v, want the PI_HOME occurrence kept", r)
	}
	if len(roots) != 1 {
		t.Fatalf("roots = %+v, want exactly one after dedup", roots)
	}
}

// TestDedupeRoots pins the helper's contract directly: an empty path is
// dropped, duplicates collapse onto their first occurrence, and order is kept.
func TestDedupeRoots(t *testing.T) {
	in := []harness.Root{
		{Path: "", Origin: harness.OriginBuiltin},
		{Path: "/a", Origin: harness.OriginEnv, Env: "PI_HOME"},
		{Path: "/b", Origin: harness.OriginBuiltin},
		{Path: "/a", Origin: harness.OriginBuiltin},
	}
	got := dedupeRoots(in)
	want := []string{"/a", "/b"}
	if len(got) != len(want) {
		t.Fatalf("dedupeRoots = %+v, want %v", got, want)
	}
	for i, r := range got {
		if r.Path != want[i] {
			t.Fatalf("dedupeRoots[%d] = %q, want %q", i, r.Path, want[i])
		}
	}
	if got[0].Env != "PI_HOME" {
		t.Errorf("dedupeRoots kept %+v, want the first occurrence's provenance", got[0])
	}
}

// TestRootsMyTokenPiDirsRejectsBlankList pins that an explicitly set but
// unusable MYTOKEN_PI_DIRS is a hard error rather than a silent fallback.
func TestRootsMyTokenPiDirsRejectsBlankList(t *testing.T) {
	isolateHome(t)
	t.Setenv("MYTOKEN_PI_DIRS", string(filepath.ListSeparator))

	p, ok := harness.Get(model.Pi)
	if !ok {
		t.Fatal("pi is not registered")
	}
	pv, ok := p.(harness.Provenance)
	if !ok {
		t.Fatalf("pi parser %T does not implement harness.Provenance", p)
	}
	var listErr *harness.EnvListError
	if _, err := pv.RootProvenance(); !errors.As(err, &listErr) {
		t.Fatalf("RootProvenance err = %v, want *harness.EnvListError", err)
	}
}

// TestDefaultsDiscoverOMPAndPiRootsTogether pins discovery through the public
// API: a freshly built default parser, in a frozen temp HOME with only an OMP
// install, finds the OMP session log and parses its usage; nothing outside a
// sessions directory becomes a source.
func TestDefaultsDiscoverOMPAndPiRootsTogether(t *testing.T) {
	home := isolateHome(t)
	ompSession := writeSessionLog(t, filepath.Join(home, ".omp", "agent", "sessions", "--tmp-proj--"),
		"01a00000-0000-7000-8000-0000000000f1", "/tmp/proj", "hello from omp", 100, 20)
	// Files that live in the same tree but are not session logs.
	writeRaw(t, filepath.Join(home, ".omp", "agent", "sessions", "--tmp-proj--", "credentials.jsonl"), "{not json}\n")
	writeRaw(t, filepath.Join(home, ".omp", "agent", "sessions", "--tmp-proj--", "notes.txt"), "not a log\n")
	// Credentials and databases sit in the agent tree but are never sources.
	writeRaw(t, filepath.Join(home, ".omp", "agent", "credentials.json"), "{}")
	writeRaw(t, filepath.Join(home, ".omp", "agent", "agent.db"), "sqlite")
	writeRaw(t, filepath.Join(home, ".omp", "agent", "stats.db"), "sqlite")
	writeRaw(t, filepath.Join(home, ".omp", "agent", "omp.pid"), "12345")

	// The Pi install is present too, with one log of its own.
	piSession := writeSessionLog(t, filepath.Join(home, ".pi", "agent", "sessions", "--tmp-other--"),
		"01a00000-0000-7000-8000-0000000000f3", "/tmp/other", "hello from pi", 5, 7)

	// New() must be called here, not via the registered singleton: the registry
	// builds its parsers during package init, so its roots were captured from
	// the real environment before HOME was redirected.
	p := New()
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	got := map[string]bool{}
	for _, s := range srcs {
		got[s.Path] = true
	}
	if !got[ompSession] {
		t.Errorf("OMP session %q was not discovered; got %v", ompSession, srcs)
	}
	if !got[piSession] {
		t.Errorf("Pi session %q was not discovered; got %v", piSession, srcs)
	}
	if len(srcs) != 2 {
		t.Errorf("Discover returned %d sources, want the 2 session logs: %+v", len(srcs), srcs)
	}

	for _, src := range srcs {
		b, err := p.Parse(context.Background(), src, harness.Cursor{})
		if err != nil {
			t.Fatalf("Parse(%s): %v", src.Path, err)
		}
		if len(b.Events) != 1 {
			t.Fatalf("Parse(%s) events = %d, want 1", src.Path, len(b.Events))
		}
	}
}

// TestDefaultsSkipAbsentOMPRoots pins the built-in tier's contract: an OMP
// directory that is absent is a non-fatal miss, never an error, and the Pi
// root is still reported.
func TestDefaultsSkipAbsentOMPRoots(t *testing.T) {
	home := isolateHome(t)

	// Fresh parser: see TestDefaultsDiscoverOMPAndPiRootsTogether for why the
	// registered singleton cannot be used once HOME is redirected.
	p := New()
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover with no roots present: %v", err)
	}
	if len(srcs) != 0 {
		t.Fatalf("Discover found %+v, want nothing in a bare HOME", srcs)
	}

	roots := piRootProvenance(t)
	if _, ok := rootWithPath(roots, filepath.Join(home, ".omp", "agent", "sessions")); !ok {
		t.Errorf("absent OMP root must still be reported as a builtin default: %+v", roots)
	}
}

// writeSessionLog writes a Pi-shaped session log: a session header, a user
// message for the title and one assistant message carrying usage.
func writeSessionLog(t *testing.T, dir, id, cwd, prompt string, in, out int) string {
	t.Helper()
	mkdirAll(t, dir)
	path := filepath.Join(dir, "2026-01-01T00-00-00-000Z_"+id+".jsonl")
	body := `{"type":"session","version":3,"id":"` + id + `","timestamp":"2026-01-01T00:00:00Z","cwd":"` + cwd + `"}` + "\n" +
		`{"type":"message","id":"m1","parentId":null,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"` + prompt + `"}]}}` + "\n" +
		`{"type":"message","id":"m2","parentId":"m1","timestamp":"2026-01-01T00:00:02Z","message":{"role":"assistant","model":"test-model","provider":"test","responseId":"resp_` + id + `","usage":{"input":` + strconv.Itoa(in) + `,"output":` + strconv.Itoa(out) + `,"cacheRead":0,"cacheWrite":0,"totalTokens":` + strconv.Itoa(in+out) + `,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0.5}}}}` + "\n"
	writeRaw(t, path, body)
	return path
}

// writeRaw writes a fixture file, creating its directory.
func writeRaw(t *testing.T, path, body string) {
	t.Helper()
	mkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ---------------------------------------------------------------------------
// OMO / senpi root compatibility
// ---------------------------------------------------------------------------
//
// Oh My OpenAgent (OMO) brands the senpi engine, and the two share one resolver
// (packages/omo-senpi/src/components/agent-home/resolve-agent-home.ts in the
// oh-my-openagent source tree). It picks exactly one agent home:
//
//	$OMO_CODING_AGENT_DIR, else $SENPI_CODING_AGENT_DIR, else $PI_CODING_AGENT_DIR
//	~/.omo/agent   when ~/.omo/agent/settings.json exists   (canonical)
//	~/.omo         when ~/.omo/settings.json exists         (legacy flat)
//	~/.senpi/agent                                          (standalone)
//
// ... and sessions always live in <agent-home>/sessions. The Pi parser covers
// it because senpi writes the same type=session JSONL and has no parser of its
// own. tests here never touch the real home: isolateHome redirects HOME and
// blanks every variable the resolver reads.

// TestRootsOMOSenpiDefaultIsAddedOnceBeyondPiAndOMP pins the offline default:
// with no override and no sentinel, Pi gets exactly one further root, the
// standalone ~/.senpi/agent/sessions fallback, reported after its own and OMP's.
func TestRootsOMOSenpiDefaultIsAddedOnceBeyondPiAndOMP(t *testing.T) {
	home := isolateHome(t)

	roots := piRootProvenance(t)

	want := filepath.Join(home, ".senpi", "agent", "sessions")
	if r := requireRoot(t, roots, want); r.Origin != harness.OriginBuiltin || r.Env != "" {
		t.Errorf("senpi fallback root %+v, want builtin origin with no env", r)
	}
	// Pi, OMP and OMO/senpi: exactly one root each.
	if len(roots) != 3 {
		t.Fatalf("default roots = %+v, want exactly Pi + OMP + OMO/senpi", roots)
	}
	if roots[len(roots)-1].Path != want {
		t.Errorf("last default root = %q, want the OMO/senpi root %q", roots[len(roots)-1].Path, want)
	}
	// Only the sessions directory is a root, never the agent tree around it.
	requireAbsent(t, roots, filepath.Join(home, ".senpi", "agent"))
	requireAbsent(t, roots, filepath.Join(home, ".senpi"))
}

// TestRootsOMOSenpiSentinelDetection pins the sentinel rule: only a
// settings.json marker makes ~/.omo an engine home, the canonical ~/.omo/agent
// layout wins over the legacy flat one, and no sentinel means the standalone
// ~/.senpi/agent fallback.
func TestRootsOMOSenpiSentinelDetection(t *testing.T) {
	t.Run("canonical omo agent home", func(t *testing.T) {
		home := isolateHome(t)
		writeRaw(t, filepath.Join(home, ".omo", "agent", "settings.json"), "{}")

		roots := piRootProvenance(t)
		want := filepath.Join(home, ".omo", "agent", "sessions")
		if r := requireRoot(t, roots, want); r.Origin != harness.OriginBuiltin || r.Env != "" {
			t.Errorf("canonical root %+v, want builtin origin with no env", r)
		}
		requireNoRoot(t, roots, filepath.Join(home, ".senpi", "agent", "sessions"))
		requireNoRoot(t, roots, filepath.Join(home, ".omo", "sessions"))
	})

	t.Run("legacy flat omo home", func(t *testing.T) {
		home := isolateHome(t)
		writeRaw(t, filepath.Join(home, ".omo", "settings.json"), "{}")

		roots := piRootProvenance(t)
		want := filepath.Join(home, ".omo", "sessions")
		if r := requireRoot(t, roots, want); r.Origin != harness.OriginBuiltin || r.Env != "" {
			t.Errorf("legacy root %+v, want builtin origin with no env", r)
		}
		requireNoRoot(t, roots, filepath.Join(home, ".omo", "agent", "sessions"))
		requireNoRoot(t, roots, filepath.Join(home, ".senpi", "agent", "sessions"))
	})

	t.Run("canonical beats legacy", func(t *testing.T) {
		home := isolateHome(t)
		writeRaw(t, filepath.Join(home, ".omo", "agent", "settings.json"), "{}")
		writeRaw(t, filepath.Join(home, ".omo", "settings.json"), "{}")

		roots := piRootProvenance(t)
		requireRoot(t, roots, filepath.Join(home, ".omo", "agent", "sessions"))
		requireNoRoot(t, roots, filepath.Join(home, ".omo", "sessions"))
	})

	t.Run("directory without sentinel is not engine state", func(t *testing.T) {
		home := isolateHome(t)
		mkdirAll(t, filepath.Join(home, ".omo", "agent"))
		writeRaw(t, filepath.Join(home, ".omo", "agent", "credentials.json"), "{}")

		roots := piRootProvenance(t)
		requireRoot(t, roots, filepath.Join(home, ".senpi", "agent", "sessions"))
		requireNoRoot(t, roots, filepath.Join(home, ".omo", "agent", "sessions"))
		requireNoRoot(t, roots, filepath.Join(home, ".omo", "sessions"))
	})
}

// TestRootsOMOSenpiEnvPriority pins the official environment order
// OMO_CODING_AGENT_DIR > SENPI_CODING_AGENT_DIR > PI_CODING_AGENT_DIR, the
// env-origin tagging of each, and the stable dedupe against the OMP tier.
func TestRootsOMOSenpiEnvPriority(t *testing.T) {
	t.Run("omo wins over senpi and pi", func(t *testing.T) {
		isolateHome(t)
		omo, senpi, piDir := t.TempDir(), t.TempDir(), t.TempDir()
		t.Setenv("OMO_CODING_AGENT_DIR", omo)
		t.Setenv("SENPI_CODING_AGENT_DIR", senpi)
		t.Setenv("PI_CODING_AGENT_DIR", piDir)

		roots := piRootProvenance(t)
		if r := requireRoot(t, roots, filepath.Join(omo, "sessions")); r.Origin != harness.OriginEnv || r.Env != "OMO_CODING_AGENT_DIR" {
			t.Errorf("root %+v, want env origin tagged OMO_CODING_AGENT_DIR", r)
		}
		requireNoRoot(t, roots, filepath.Join(senpi, "sessions"))
		// The OMP tier still honours PI_CODING_AGENT_DIR, so its root stays a
		// separate one; the OMO tier resolved to OMO_CODING_AGENT_DIR instead.
		if r := requireRoot(t, roots, filepath.Join(piDir, "sessions")); r.Origin != harness.OriginEnv || r.Env != "PI_CODING_AGENT_DIR" {
			t.Errorf("root %+v, want the OMP PI_CODING_AGENT_DIR root", r)
		}
		if len(roots) != 3 {
			t.Errorf("roots = %+v, want Pi + OMP + OMO", roots)
		}
	})

	t.Run("senpi wins over pi when omo is unset", func(t *testing.T) {
		isolateHome(t)
		senpi, piDir := t.TempDir(), t.TempDir()
		t.Setenv("SENPI_CODING_AGENT_DIR", senpi)
		t.Setenv("PI_CODING_AGENT_DIR", piDir)

		roots := piRootProvenance(t)
		if r := requireRoot(t, roots, filepath.Join(senpi, "sessions")); r.Origin != harness.OriginEnv || r.Env != "SENPI_CODING_AGENT_DIR" {
			t.Errorf("root %+v, want env origin tagged SENPI_CODING_AGENT_DIR", r)
		}
		requireNoRoot(t, roots, filepath.Join(piDir, "sessions", "x"))
		requireRoot(t, roots, filepath.Join(piDir, "sessions"))
	})

	t.Run("pi coding agent dir dedupes against the OMP root", func(t *testing.T) {
		home := isolateHome(t)
		dir := t.TempDir()
		t.Setenv("PI_CODING_AGENT_DIR", dir)

		roots := piRootProvenance(t)
		// OMP already reported <dir>/sessions, and the OMO tier resolves the same
		// variable to the same home, so stable dedupe keeps the first occurrence
		// and the root set gains nothing.
		r := requireRoot(t, roots, filepath.Join(dir, "sessions"))
		if r.Origin != harness.OriginEnv || r.Env != "PI_CODING_AGENT_DIR" {
			t.Errorf("root %+v, want env origin tagged PI_CODING_AGENT_DIR", r)
		}
		if len(roots) != 2 {
			t.Fatalf("roots = %+v, want Pi default + the shared agent dir once", roots)
		}
		requireNoRoot(t, roots, filepath.Join(home, ".senpi", "agent", "sessions"))
		requireNoRoot(t, roots, filepath.Join(home, ".omo", "agent", "sessions"))
	})

	t.Run("same path as the OMP override dedupes", func(t *testing.T) {
		isolateHome(t)
		dir := t.TempDir()
		t.Setenv("OMO_CODING_AGENT_DIR", dir)
		t.Setenv("PI_CODING_AGENT_DIR", dir)

		roots := piRootProvenance(t)
		// OMO_CODING_AGENT_DIR names the same directory as OMP's override, so
		// the OMO tier resolves to a home OMP already reported. Stable dedupe
		// keeps the first occurrence's provenance and the root set stays at two.
		r := requireRoot(t, roots, filepath.Join(dir, "sessions"))
		if r.Origin != harness.OriginEnv || r.Env != "PI_CODING_AGENT_DIR" {
			t.Errorf("root %+v, want the first (OMP) occurrence kept", r)
		}
		if len(roots) != 2 {
			t.Fatalf("roots = %+v, want Pi default + the shared agent dir once", roots)
		}
	})

	t.Run("blank and whitespace values count as unset", func(t *testing.T) {
		home := isolateHome(t)
		t.Setenv("OMO_CODING_AGENT_DIR", "   ")
		t.Setenv("SENPI_CODING_AGENT_DIR", "")

		roots := piRootProvenance(t)
		requireRoot(t, roots, filepath.Join(home, ".senpi", "agent", "sessions"))
		for _, r := range roots {
			if r.Origin == harness.OriginEnv && (r.Env == "OMO_CODING_AGENT_DIR" || r.Env == "SENPI_CODING_AGENT_DIR") {
				t.Errorf("blank value produced env root %+v", r)
			}
		}
	})

	t.Run("values are trimmed and resolved", func(t *testing.T) {
		isolateHome(t)
		dir := t.TempDir()
		t.Setenv("OMO_CODING_AGENT_DIR", "  "+dir+"  ")

		if r := requireRoot(t, piRootProvenance(t), filepath.Join(dir, "sessions")); r.Env != "OMO_CODING_AGENT_DIR" {
			t.Errorf("root %+v, want the trimmed OMO_CODING_AGENT_DIR root", r)
		}
	})

	t.Run("relative values resolve against the working directory", func(t *testing.T) {
		isolateHome(t)
		wd, err := os.Getwd()
		if err != nil {
			t.Fatalf("Getwd: %v", err)
		}
		t.Setenv("SENPI_CODING_AGENT_DIR", "senpi-relative-agent")

		want := filepath.Join(wd, "senpi-relative-agent", "sessions")
		if r := requireRoot(t, piRootProvenance(t), want); r.Env != "SENPI_CODING_AGENT_DIR" {
			t.Errorf("root %+v, want the resolved relative SENPI_CODING_AGENT_DIR root", r)
		}
	})
}

// TestRootsOMOSenpiDoesNotChangeReplacementSemantics pins that the new tier is
// strictly additive inside the built-in tier: PI_HOME and MYTOKEN_PI_DIRS still
// replace the whole default set and never drag the OMO/senpi root in.
func TestRootsOMOSenpiDoesNotChangeReplacementSemantics(t *testing.T) {
	t.Run("PI_HOME replaces the whole default set", func(t *testing.T) {
		home := isolateHome(t)
		base := filepath.Join(home, "pi-base")
		t.Setenv("PI_HOME", base)

		roots := piRootProvenance(t)
		if len(roots) != 1 {
			t.Fatalf("PI_HOME roots = %+v, want exactly the one replacement root", roots)
		}
		if r := requireRoot(t, roots, filepath.Join(base, "sessions")); r.Origin != harness.OriginEnv || r.Env != "PI_HOME" {
			t.Errorf("root %+v, want env origin tagged PI_HOME", r)
		}
		requireNoRoot(t, roots, filepath.Join(home, ".senpi", "agent", "sessions"))
		requireNoRoot(t, roots, filepath.Join(home, ".omo", "agent", "sessions"))
		requireNoRoot(t, roots, filepath.Join(home, ".omp", "agent", "sessions"))
	})

	t.Run("MYTOKEN_PI_DIRS names roots verbatim", func(t *testing.T) {
		home := isolateHome(t)
		a, b := t.TempDir(), t.TempDir()
		t.Setenv("MYTOKEN_PI_DIRS", a+string(filepath.ListSeparator)+b)

		roots := piRootProvenance(t)
		if len(roots) != 2 {
			t.Fatalf("MYTOKEN_PI_DIRS roots = %+v, want exactly the two requested", roots)
		}
		requireNoRoot(t, roots, filepath.Join(home, ".senpi", "agent", "sessions"))
	})
}

// TestDefaultsDiscoverOMOSenpiRoots pins discovery through the public API: a
// default parser finds the OMO/senpi session log in the resolved home and
// parses its usage, while credentials, databases, settings and non-session
// JSONL in the same tree are never sources. The canonical home also suppresses
// the standalone fallback, so its logs are not double counted.
func TestDefaultsDiscoverOMOSenpiRoots(t *testing.T) {
	t.Run("canonical omo home", func(t *testing.T) {
		home := isolateHome(t)
		writeRaw(t, filepath.Join(home, ".omo", "agent", "settings.json"), "{}")
		session := writeSessionLog(t, filepath.Join(home, ".omo", "agent", "sessions", "--tmp-senpi--"),
			"01a00000-0000-7000-8000-0000000000e1", "/tmp/senpi", "hello from senpi", 100, 20)
		// A standalone senpi install is not scanned once the canonical sentinel
		// wins: the resolver picks exactly one home.
		writeSessionLog(t, filepath.Join(home, ".senpi", "agent", "sessions", "--tmp-other--"),
			"01a00000-0000-7000-8000-0000000000e2", "/tmp/other", "must not be scanned", 1, 1)
		// Files that live in the same tree but are not session logs.
		writeRaw(t, filepath.Join(home, ".omo", "agent", "sessions", "--tmp-senpi--", "credentials.json"), "{}")
		writeRaw(t, filepath.Join(home, ".omo", "agent", "sessions", "--tmp-senpi--", "agent.db"), "sqlite")
		writeRaw(t, filepath.Join(home, ".omo", "agent", "sessions", "--tmp-senpi--", "events.jsonl"),
			`{"type":"event","id":"e1"}`+"\n")
		writeRaw(t, filepath.Join(home, ".omo", "agent", "sessions", "--tmp-senpi--", "notes.txt"), "not a log\n")

		p := New()
		srcs, err := p.Discover(context.Background())
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if len(srcs) != 1 || srcs[0].Path != session {
			t.Fatalf("Discover = %+v, want only the OMO session %q", srcs, session)
		}
		b, err := p.Parse(context.Background(), srcs[0], harness.Cursor{})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if len(b.Events) != 1 {
			t.Fatalf("Parse events = %d, want 1", len(b.Events))
		}
	})

	t.Run("standalone senpi fallback", func(t *testing.T) {
		home := isolateHome(t)
		session := writeSessionLog(t, filepath.Join(home, ".senpi", "agent", "sessions", "--tmp-senpi--"),
			"01a00000-0000-7000-8000-0000000000e3", "/tmp/senpi", "standalone", 7, 3)

		p := New()
		srcs, err := p.Discover(context.Background())
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if len(srcs) != 1 || srcs[0].Path != session {
			t.Fatalf("Discover = %+v, want only %q", srcs, session)
		}
	})
}
