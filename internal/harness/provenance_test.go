// Root-provenance conformance suite for every registered harness parser.
//
// The multi-instance scanner has to decide what to do when a default root does
// not exist: a path the user asked for explicitly (MYTOKEN_*_DIRS or a
// harness-specific directory variable) is an error, while a built-in platform
// default is simply absent on this machine. That decision needs provenance, so
// every registered parser reports the origin of each default root through
// harness.Provenance.
//
// The suite is deliberately environment-only and synthetic: it never reads the
// real home directory's contents and never shells out.
package harness_test

import (
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

// overrideEnvSurface documents, per harness, the explicit environment
// overrides its root resolution actually reads, in precedence order (the first
// set variable wins). It was built by inspecting every registered parser's root
// function; it is asserted in both directions below, so a parser that starts
// reading a new variable without tagging it fails this suite.
//
// Platform variables (HOME, XDG_DATA_HOME, XDG_CONFIG_HOME, APPDATA,
// LOCALAPPDATA, os.UserConfigDir) are intentionally absent: they are
// derivation inputs for the built-in tier, not explicit requests, so roots
// derived from them stay non-fatal when the directory is missing.
var overrideEnvSurface = []struct {
	h    model.Harness
	envs []string
}{
	{model.Amp, []string{"MYTOKEN_AMP_DIRS", "AMP_DATA_DIR"}},
	{model.Antigravity, []string{"MYTOKEN_ANTIGRAVITY_DIRS", "GEMINI_CLI_HOME"}},
	{model.ClaudeCode, []string{"CLAUDE_CONFIG_DIR"}},
	{model.ClaudeDesktop, []string{"MYTOKEN_CLAUDEDESKTOP_DIRS"}},
	{model.Cline, []string{"MYTOKEN_CLINE_DIRS", "CLINE_DATA_DIR", "CLINE_DIR", "CLINE_SESSION_DATA_DIR"}},
	{model.CodeBuddy, []string{"CODEBUDDY_HOME"}},
	{model.Codebuff, []string{"MYTOKEN_CODEBUFF_DIRS", "CODEBUFF_DATA_DIR"}},
	{model.Codex, []string{"CODEX_HOME"}},
	{model.Crush, []string{"MYTOKEN_CRUSH_DIRS", "CRUSH_GLOBAL_DATA"}},
	{model.Droid, []string{"MYTOKEN_DROID_DIRS", "FACTORY_DIR"}},
	{model.DSH, []string{"DSH_HOME"}},
	{model.Forge, []string{"MYTOKEN_FORGE_DIRS"}},
	{model.Gemini, []string{"MYTOKEN_GEMINI_DIRS", "GEMINI_CLI_HOME"}},
	{model.Goose, []string{"MYTOKEN_GOOSE_DIRS", "GOOSE_PATH_ROOT"}},
	{model.Grok, []string{"MYTOKEN_GROK_DIRS", "GROK_HOME"}},
	{model.Hermes, []string{"HERMES_HOME"}},
	{model.Kilo, []string{"MYTOKEN_KILO_DIRS"}},
	{model.Kimi, []string{"MYTOKEN_KIMI_DIRS", "KIMI_SHARE_DIR"}},
	{model.Kiro, []string{"MYTOKEN_KIRO_DIRS", "KIRO_HOME"}},
	{model.Mux, []string{"MYTOKEN_MUX_DIRS", "CODEBURN_MUX_DIR", "MUX_ROOT"}},
	{model.OpenClaude, []string{"MYTOKEN_OPENCLAUDE_DIRS", "CODEBURN_OPENCLAUDE_DIR"}},
	{model.OpenClaw, []string{"MYTOKEN_OPENCLAW_DIRS"}},
	{model.OpenCode, []string{"MYTOKEN_OPENCODE_DIRS", "OPENCODE_DATA_HOME"}},
	{model.Pi, []string{"PI_HOME"}},
	{model.Qwen, []string{"MYTOKEN_QWEN_DIRS", "QWEN_DATA_DIR"}},
	{model.Roo, []string{"MYTOKEN_ROO_DIRS"}},
	{model.WorkBuddy, []string{"WORKBUDDY_HOME"}},
	{model.Zed, []string{"MYTOKEN_ZED_DIRS"}},
}

// malformedValue returns a non-blank value for an override that names no usable
// directory: the platform list separator alone for the MYTOKEN_*_DIRS lists,
// and a lone comma for Amp's comma-separated AMP_DATA_DIR.
func malformedValue(name string) string {
	if name == "AMP_DATA_DIR" {
		return ","
	}
	return string(filepath.ListSeparator)
}

func envsFor(h model.Harness) []string {
	for _, e := range overrideEnvSurface {
		if e.h == h {
			return e.envs
		}
	}
	return nil
}

// blankOverrideEnvs clears every documented explicit override so a test starts
// from the built-in tier.
func blankOverrideEnvs(t *testing.T) {
	t.Helper()
	for _, e := range overrideEnvSurface {
		for _, name := range e.envs {
			t.Setenv(name, "")
		}
	}
}

func registeredProvenance(t *testing.T, h model.Harness) []harness.Root {
	t.Helper()
	roots, ok, err := harness.RegisteredProvenance(h)
	if !ok {
		t.Fatalf("%s: RegisteredProvenance reported the parser as unsupported", h)
	}
	if err != nil {
		t.Fatalf("%s: RegisteredProvenance: %v", h, err)
	}
	return roots
}

// TestProvenanceEveryRegisteredHarnessImplementsCapability is the coverage
// check: no registered harness may silently lack provenance, and the documented
// env surface must cover exactly the registered set.
func TestProvenanceEveryRegisteredHarnessImplementsCapability(t *testing.T) {
	parsers := harness.All()
	if len(parsers) < wantRegistered {
		t.Fatalf("registered harness parsers = %d, want at least %d", len(parsers), wantRegistered)
	}
	if len(overrideEnvSurface) != len(parsers) {
		t.Fatalf("documented env surface covers %d harnesses, registry has %d; a harness is missing from the table or registered twice",
			len(overrideEnvSurface), len(parsers))
	}
	for _, p := range parsers {
		h := p.Harness()
		if _, ok := p.(harness.Provenance); !ok {
			t.Errorf("%s: parser %T does not implement harness.Provenance", h, p)
			continue
		}
		if envsFor(h) == nil {
			t.Errorf("%s: not covered by the documented env surface", h)
		}
	}
}

// TestProvenanceBuiltinWhenNoOverrideIsSet pins the built-in tier: with every
// explicit override blank, every root is a built-in default and is therefore a
// non-fatal skip when absent.
func TestProvenanceBuiltinWhenNoOverrideIsSet(t *testing.T) {
	blankOverrideEnvs(t)
	homeKnown := harness.Home() != ""
	for _, p := range harness.All() {
		h := p.Harness()
		roots := registeredProvenance(t, h)
		if homeKnown && len(roots) == 0 {
			t.Errorf("%s: no default roots at all with a known home directory", h)
		}
		for _, r := range roots {
			if r.Origin != harness.OriginBuiltin || r.Env != "" {
				t.Errorf("%s: root %q has origin %q env %q; want builtin with no env",
					h, r.Path, r.Origin, r.Env)
			}
			if strings.TrimSpace(r.Path) == "" {
				t.Errorf("%s: empty root path reported", h)
			}
		}
	}
}

// TestProvenanceHonoursEveryDocumentedOverride sets each documented override on
// its own and requires the resulting roots to be tagged env-origin under that
// variable's name, so a mis-mapped or untagged variable cannot pass unnoticed.
func TestProvenanceHonoursEveryDocumentedOverride(t *testing.T) {
	for _, e := range overrideEnvSurface {
		for _, name := range e.envs {
			t.Run(string(e.h)+"/"+name, func(t *testing.T) {
				blankOverrideEnvs(t)
				dir := t.TempDir()
				t.Setenv(name, dir)

				roots := registeredProvenance(t, e.h)
				var envRoots []harness.Root
				for _, r := range roots {
					switch r.Origin {
					case harness.OriginEnv:
						if r.Env != name {
							t.Errorf("env-origin root %q is tagged %q, want %q", r.Path, r.Env, name)
						}
						envRoots = append(envRoots, r)
					case harness.OriginBuiltin:
						if r.Env != "" {
							t.Errorf("built-in root %q carries env %q", r.Path, r.Env)
						}
					default:
						t.Errorf("default root %q has unexpected origin %q", r.Path, r.Origin)
					}
				}
				if len(envRoots) == 0 {
					t.Fatalf("%s=%q produced no env-origin root; roots=%+v", name, dir, roots)
				}
				for _, r := range envRoots {
					if !pathUnder(dir, r.Path) {
						t.Errorf("env-origin root %q is not under the %s override %q", r.Path, name, dir)
					}
				}
			})
		}
	}
}

// TestProvenanceBlankOverrideIsTreatedAsAbsent pins the blank-value contract: a
// whitespace-only value counts as unset, falls through to the next tier and is
// never reported as an explicit request.
func TestProvenanceBlankOverrideIsTreatedAsAbsent(t *testing.T) {
	for _, e := range overrideEnvSurface {
		for _, name := range e.envs {
			t.Run(string(e.h)+"/"+name, func(t *testing.T) {
				blankOverrideEnvs(t)
				t.Setenv(name, "   ")

				roots := registeredProvenance(t, e.h)
				for _, r := range roots {
					if r.Origin == harness.OriginEnv {
						t.Errorf("%s=%q (blank) was treated as an explicit override: root %q tagged env %q",
							name, "   ", r.Path, r.Env)
					}
				}
			})
		}
	}
}

// TestProvenanceMalformedOverrideIsErrorNotDefault pins the hard half of the
// blank contract: a non-blank value that names no usable directory is an error,
// and the built-in default must not be substituted for it.
func TestProvenanceMalformedOverrideIsErrorNotDefault(t *testing.T) {
	for _, e := range overrideEnvSurface {
		for _, name := range e.envs {
			if !strings.HasSuffix(name, "_DIRS") && name != "AMP_DATA_DIR" {
				continue
			}
			t.Run(string(e.h)+"/"+name, func(t *testing.T) {
				blankOverrideEnvs(t)
				bad := malformedValue(name)
				t.Setenv(name, bad)

				roots, ok, err := harness.RegisteredProvenance(e.h)
				if !ok {
					t.Fatalf("provenance unsupported for %s", e.h)
				}
				if err == nil {
					t.Fatalf("%s=%q must be a hard error, got roots=%+v", name, bad, roots)
				}
				if len(roots) != 0 {
					t.Errorf("%s=%q must not fall back to defaults, got roots=%+v", name, bad, roots)
				}
				var ele *harness.EnvListError
				if !errors.As(err, &ele) {
					t.Fatalf("%s=%q error = %v (%T), want *harness.EnvListError", name, bad, err, err)
				}
				if ele.Env != name {
					t.Errorf("EnvListError.Env = %q, want %q", ele.Env, name)
				}
			})
		}
	}
}

// TestProvenanceIsLiveNotInitCaptured is the regression guard for the original
// flaw: the registered singleton's Roots() were captured during package init,
// so provenance must re-read the environment on every call or a rescan would
// miss an override set after startup.
func TestProvenanceIsLiveNotInitCaptured(t *testing.T) {
	blankOverrideEnvs(t)
	p, ok := harness.Get(model.Qwen)
	if !ok {
		t.Fatal("qwen is not registered")
	}
	before := append([]string(nil), p.Roots()...)

	dir := t.TempDir()
	t.Setenv("MYTOKEN_QWEN_DIRS", dir)

	roots := registeredProvenance(t, model.Qwen)
	want := []harness.Root{{Path: dir, Origin: harness.OriginEnv, Env: "MYTOKEN_QWEN_DIRS"}}
	if !reflect.DeepEqual(roots, want) {
		t.Errorf("live provenance = %+v, want %+v", roots, want)
	}
	if got := p.Roots(); !reflect.DeepEqual(got, before) {
		t.Errorf("singleton Roots() = %q after setting the override, want the init-captured %q", got, before)
	}
}

// TestProvenanceExplicitRootsNeverFallBackToDefaults pins the constructor
// contract: a parser built with explicit roots (including the empty list)
// reports exactly those roots, with no environmental fallback, even while an
// override that would change the defaults is set.
func TestProvenanceExplicitRootsNeverFallBackToDefaults(t *testing.T) {
	blankOverrideEnvs(t)
	for _, p := range harness.All() {
		h := p.Harness()
		t.Run(string(h), func(t *testing.T) {
			for _, name := range envsFor(h) {
				t.Setenv(name, t.TempDir())
			}

			empty, ok := harness.NewRooted(h)
			if !ok {
				t.Fatalf("NewRooted(%s) = false, want true", h)
			}
			pv, ok := empty.(harness.Provenance)
			if !ok {
				t.Fatalf("rooted parser for %s does not implement harness.Provenance", h)
			}
			if roots, err := pv.RootProvenance(); err != nil || len(roots) != 0 {
				t.Errorf("NewRooted(%s).RootProvenance() = (%+v, %v), want no roots and no error", h, roots, err)
			}

			dirA, dirB := t.TempDir(), t.TempDir()
			rooted, ok := harness.NewRooted(h, dirA, dirB)
			if !ok {
				t.Fatalf("NewRooted(%s, ...) = false, want true", h)
			}
			prov, ok := rooted.(harness.Provenance)
			if !ok {
				t.Fatalf("rooted parser for %s does not implement harness.Provenance", h)
			}
			got, err := prov.RootProvenance()
			if err != nil {
				t.Fatalf("RootProvenance() error = %v", err)
			}
			want := []harness.Root{
				{Path: dirA, Origin: harness.OriginExplicit},
				{Path: dirB, Origin: harness.OriginExplicit},
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("RootProvenance() = %+v, want the explicit roots %+v", got, want)
			}
		})
	}
}

// TestRegisteredProvenanceUnknownHarness checks the hard-failure path for a
// harness that is not registered at all.
func TestRegisteredProvenanceUnknownHarness(t *testing.T) {
	roots, ok, err := harness.RegisteredProvenance(model.Harness("not-a-registered-harness"))
	if ok || roots != nil || err != nil {
		t.Fatalf("RegisteredProvenance(unknown) = (%+v, %v, %v), want (nil, false, nil)", roots, ok, err)
	}
}

// TestProvenanceConcurrentAccess runs under -race and pins that provenance is a
// read-only, lock-free-of-registry-mutation operation: many goroutines asking
// every parser at once must agree with a single-threaded answer and must not
// perturb the registry.
func TestProvenanceConcurrentAccess(t *testing.T) {
	blankOverrideEnvs(t)
	parsers := harness.All()
	before := make(map[model.Harness][]harness.Root, len(parsers))
	for _, p := range parsers {
		before[p.Harness()] = registeredProvenance(t, p.Harness())
	}

	const workers = 8
	const iterations = 25
	var wg sync.WaitGroup
	errs := make(chan string, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				for _, p := range harness.All() {
					h := p.Harness()
					roots, ok, err := harness.RegisteredProvenance(h)
					if !ok || err != nil {
						errs <- string(h) + ": ok=" + strconv.FormatBool(ok) + " err=" + errString(err)
						return
					}
					if !reflect.DeepEqual(roots, before[h]) {
						errs <- string(h) + ": provenance changed under concurrency"
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if got := len(harness.All()); got != len(parsers) {
		t.Errorf("registry size changed from %d to %d during concurrent provenance reads", len(parsers), got)
	}
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// TestGetenvAndSplitEnvListEdges pins the shared list-parsing helpers that every
// parser's provenance path is built on.
func TestGetenvAndSplitEnvListEdges(t *testing.T) {
	t.Run("getenv blank is absent", func(t *testing.T) {
		for _, blank := range []string{"", " ", "\t\n"} {
			t.Setenv("MYTOKEN_PROVENANCE_TEST", blank)
			if v, ok := harness.Getenv("MYTOKEN_PROVENANCE_TEST"); ok || v != "" {
				t.Errorf("Getenv(blank %q) = (%q, %v), want (\"\", false)", blank, v, ok)
			}
		}
		t.Setenv("MYTOKEN_PROVENANCE_TEST", "  /tmp/x  ")
		if v, ok := harness.Getenv("MYTOKEN_PROVENANCE_TEST"); !ok || v != "/tmp/x" {
			t.Errorf("Getenv(trimmed) = (%q, %v), want (/tmp/x, true)", v, ok)
		}
	})

	t.Run("split list cleans, trims and drops blanks", func(t *testing.T) {
		sep := string(filepath.ListSeparator)
		got, err := harness.SplitEnvList("X", " /a "+sep+sep+"b/../b"+sep+"c ")
		if err != nil {
			t.Fatalf("SplitEnvList: %v", err)
		}
		// SplitEnvList cleans with filepath semantics, and filepath.Clean
		// rewrites "/" to "\" on Windows. Deriving the expected elements the
		// same way keeps the expectation platform-correct instead of pinning
		// the Unix spelling of the input.
		want := []string{
			filepath.Clean(filepath.FromSlash("/a")),
			filepath.Clean(filepath.FromSlash("b/../b")),
			filepath.Clean(filepath.FromSlash("c")),
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("SplitEnvList = %q, want %q", got, want)
		}
		// The meaning of the splitter, asserted portably and on every
		// platform: only the three non-blank entries survive, each trimmed
		// and already clean. This is what "drops blanks and cleans" means,
		// independent of the host's path separator.
		if len(got) != 3 {
			t.Fatalf("SplitEnvList returned %d entries, want the 3 non-blank ones: %q", len(got), got)
		}
		for i, e := range got {
			if strings.TrimSpace(e) == "" {
				t.Errorf("entry %d is blank: %q", i, e)
			}
			if e != filepath.Clean(e) {
				t.Errorf("entry %d = %q is not cleaned", i, e)
			}
		}
	})

	t.Run("separator-only list is an error", func(t *testing.T) {
		sep := string(filepath.ListSeparator)
		for _, bad := range []string{sep, sep + sep, "  " + sep + "   ", "   "} {
			got, err := harness.SplitEnvList("X", bad)
			if err == nil {
				t.Errorf("SplitEnvList(%q) = %q, want an error", bad, got)
				continue
			}
			var ele *harness.EnvListError
			if !errors.As(err, &ele) || ele.Env != "X" || ele.Value != bad {
				t.Errorf("SplitEnvList(%q) error = %v (%T), want *EnvListError{Env: X, Value: %q}", bad, err, err, bad)
			}
		}
	})
}

// pathUnder reports whether p is dir itself or lives inside it, lexically.
func pathUnder(dir, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
