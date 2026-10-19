// Package harness_test holds the registry-level rooted-parser conformance
// suite.
//
// Every registered harness parser must be constructible for an explicit root
// list through harness.NewRooted without touching the singleton that
// harness.Register installed. The suite runs with HOME redirected to a temp
// directory and uses synthetic paths only, so it never discovers anything under
// the real home.
package harness_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	_ "github.com/zzstar101/mytoken/internal/harness/amp"
	_ "github.com/zzstar101/mytoken/internal/harness/antigravity"
	_ "github.com/zzstar101/mytoken/internal/harness/claude"
	_ "github.com/zzstar101/mytoken/internal/harness/claudedesktop"
	_ "github.com/zzstar101/mytoken/internal/harness/cline"
	_ "github.com/zzstar101/mytoken/internal/harness/codebuddy"
	_ "github.com/zzstar101/mytoken/internal/harness/codebuff"
	_ "github.com/zzstar101/mytoken/internal/harness/codex"
	_ "github.com/zzstar101/mytoken/internal/harness/crush"
	_ "github.com/zzstar101/mytoken/internal/harness/droid"
	_ "github.com/zzstar101/mytoken/internal/harness/dsh"
	_ "github.com/zzstar101/mytoken/internal/harness/forge"
	_ "github.com/zzstar101/mytoken/internal/harness/gemini"
	_ "github.com/zzstar101/mytoken/internal/harness/goose"
	_ "github.com/zzstar101/mytoken/internal/harness/grok"
	_ "github.com/zzstar101/mytoken/internal/harness/hermes"
	_ "github.com/zzstar101/mytoken/internal/harness/kilo"
	_ "github.com/zzstar101/mytoken/internal/harness/kimi"
	_ "github.com/zzstar101/mytoken/internal/harness/kiro"
	_ "github.com/zzstar101/mytoken/internal/harness/mimocode"
	_ "github.com/zzstar101/mytoken/internal/harness/minimax"
	_ "github.com/zzstar101/mytoken/internal/harness/mux"
	_ "github.com/zzstar101/mytoken/internal/harness/openclaude"
	_ "github.com/zzstar101/mytoken/internal/harness/openclaw"
	_ "github.com/zzstar101/mytoken/internal/harness/opencode"
	_ "github.com/zzstar101/mytoken/internal/harness/pi"
	_ "github.com/zzstar101/mytoken/internal/harness/qwen"
	_ "github.com/zzstar101/mytoken/internal/harness/roo"
	_ "github.com/zzstar101/mytoken/internal/harness/stepcode"
	_ "github.com/zzstar101/mytoken/internal/harness/workbuddy"
	_ "github.com/zzstar101/mytoken/internal/harness/zcode"
	_ "github.com/zzstar101/mytoken/internal/harness/zed"
	"github.com/zzstar101/mytoken/internal/model"
)

// wantRegistered is the number of harness parsers this version registers
// (internal/app/app.go imports exactly these). The suite stops only if fewer
// are present, so a newly registered harness is covered automatically while a
// dropped registration cannot make the suite vacuous.
const wantRegistered = 32

// TestNewRootedSupportsEveryRegisteredHarness is the coverage check: every
// registered parser must implement harness.Rooted, and NewRooted must build a
// distinct parser whose Harness and Roots match the request exactly.
func TestNewRootedSupportsEveryRegisteredHarness(t *testing.T) {
	parsers := harness.All()
	if len(parsers) < wantRegistered {
		t.Fatalf("registered harness parsers = %d, want at least %d (did a package lose its registration?)", len(parsers), wantRegistered)
	}
	t.Logf("registered harness parsers: %d", len(parsers))

	dir := t.TempDir()
	for _, p := range parsers {
		h := p.Harness()
		t.Run(string(h), func(t *testing.T) {
			if _, ok := p.(harness.Rooted); !ok {
				t.Fatalf("%s parser %T does not implement harness.Rooted", h, p)
			}
			want := []string{filepath.Join(dir, string(h), "a"), filepath.Join(dir, string(h), "b")}
			got, ok := harness.NewRooted(h, want...)
			if !ok {
				t.Fatalf("NewRooted(%s, ...) = false, want true", h)
			}
			if got == nil {
				t.Fatalf("NewRooted(%s, ...) returned a nil parser with ok=true", h)
			}
			if got == p {
				t.Fatalf("NewRooted(%s, ...) returned the registered singleton", h)
			}
			if gotHarness := got.Harness(); gotHarness != h {
				t.Errorf("NewRooted(%s, ...).Harness() = %q, want %q", h, gotHarness, h)
			}
			if gotRoots := got.Roots(); !reflect.DeepEqual(gotRoots, want) {
				t.Errorf("NewRooted(%s, %q).Roots() = %q, want the given roots", h, want, gotRoots)
			}
		})
	}
}

// TestNewRootedExplicitEmptyRootsStayEmpty pins the contract that an explicit
// empty root list means "no roots", never "use the defaults".
func TestNewRootedExplicitEmptyRootsStayEmpty(t *testing.T) {
	parsers := harness.All()
	if len(parsers) < wantRegistered {
		t.Fatalf("registered harness parsers = %d, want at least %d", len(parsers), wantRegistered)
	}
	for _, p := range parsers {
		h := p.Harness()
		t.Run(string(h), func(t *testing.T) {
			got, ok := harness.NewRooted(h)
			if !ok {
				t.Fatalf("NewRooted(%s) = false, want true", h)
			}
			if roots := got.Roots(); len(roots) != 0 {
				t.Errorf("NewRooted(%s) with no roots produced %d roots %q; explicit empty roots must not fall back to defaults", h, len(roots), roots)
			}
			allEmpty, ok := harness.NewRooted(h, "", "  ")
			if !ok {
				t.Fatalf("NewRooted(%s, \"\", \"  \") = false, want true", h)
			}
			if roots := allEmpty.Roots(); len(roots) != 0 {
				t.Errorf("NewRooted(%s, empty entries) produced %d roots %q; explicit empty roots must not fall back to defaults", h, len(roots), roots)
			}
		})
	}
}

// TestNewRootedUnknownHarness checks the hard-failure path for a harness that
// is not registered at all.
func TestNewRootedUnknownHarness(t *testing.T) {
	p, ok := harness.NewRooted(model.Harness("not-a-registered-harness"), t.TempDir())
	if ok || p != nil {
		t.Fatalf("NewRooted(unknown) = (%v, %v), want (nil, false)", p, ok)
	}
}

// TestNewRootedDoesNotMutateSingleton proves the registered singleton keeps its
// own roots and that a rooted parser shares no state with it.
func TestNewRootedDoesNotMutateSingleton(t *testing.T) {
	parsers := harness.All()
	dir := t.TempDir()
	for _, p := range parsers {
		h := p.Harness()
		t.Run(string(h), func(t *testing.T) {
			before := append([]string(nil), p.Roots()...)
			first, ok := harness.NewRooted(h, filepath.Join(dir, string(h), "one"))
			if !ok {
				t.Fatalf("NewRooted(%s, ...) = false, want true", h)
			}
			second, ok := harness.NewRooted(h, filepath.Join(dir, string(h), "two"))
			if !ok {
				t.Fatalf("NewRooted(%s, ...) = false, want true", h)
			}
			if first == second {
				t.Errorf("two NewRooted(%s) calls returned the same parser", h)
			}
			if got := p.Roots(); !reflect.DeepEqual(got, before) {
				t.Errorf("NewRooted(%s, ...) changed the registered singleton roots: %q -> %q", h, before, got)
			}
			if len(before) > 0 && reflect.DeepEqual(second.Roots(), before) {
				t.Errorf("NewRooted(%s, ...) roots equal the singleton's roots; the explicit roots were ignored", h)
			}
		})
	}
}

// TestNewRootedParsersAreIsolated constructs two parsers for every harness and
// checks that neither the caller's slice nor a returned Roots() slice aliases
// the parser, and that the two parsers never touch each other.
func TestNewRootedParsersAreIsolated(t *testing.T) {
	parsers := harness.All()
	for _, p := range parsers {
		h := p.Harness()
		t.Run(string(h), func(t *testing.T) {
			dir := t.TempDir()
			wantA := []string{filepath.Join(dir, "a")}
			wantB := []string{filepath.Join(dir, "b")}
			rootsA := append([]string(nil), wantA...)
			rootsB := append([]string(nil), wantB...)

			pa, okA := harness.NewRooted(h, rootsA...)
			pb, okB := harness.NewRooted(h, rootsB...)
			if !okA || !okB {
				t.Fatalf("NewRooted(%s, ...) = (%v, %v), want (true, true)", h, okA, okB)
			}

			rootsA[0] = filepath.Join(dir, "mutated-caller-slice")
			if got := pa.Roots(); !reflect.DeepEqual(got, wantA) {
				t.Errorf("parser aliases the caller's roots slice: Roots() = %q, want %q", got, wantA)
			}

			out := pa.Roots()
			if len(out) > 0 {
				out[0] = filepath.Join(dir, "mutated-roots-result")
			}
			if got := pa.Roots(); !reflect.DeepEqual(got, wantA) {
				t.Errorf("Roots() exposes the parser's internal slice: after mutating the result, Roots() = %q, want %q", got, wantA)
			}

			if got := pb.Roots(); !reflect.DeepEqual(got, wantB) {
				t.Errorf("sibling parser roots changed: Roots() = %q, want %q", got, wantB)
			}
			if _, ok := p.(harness.Rooted); !ok {
				t.Errorf("%s parser %T does not implement harness.Rooted", h, p)
			}
		})
	}
}

// TestNewRootedIgnoresEnvironmentAndDefaults redirects HOME and the documented
// override variables to a temp directory, then proves an explicit root list is
// used verbatim, an empty list stays empty, and construction performs no IO in
// that directory.
func TestNewRootedIgnoresEnvironmentAndDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MYTOKEN_CLAUDE_DIRS", filepath.Join(home, "env-claude"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "env-config"))
	t.Setenv("CODEX_HOME", filepath.Join(home, "env-codex"))
	t.Setenv("GEMINI_CLI_HOME", filepath.Join(home, "env-gemini"))
	t.Setenv("MYTOKEN_KILO_DIRS", filepath.Join(home, "env-kilo"))
	t.Setenv("MYTOKEN_ROO_DIRS", filepath.Join(home, "env-roo"))

	before := dirEntries(t, home)
	for _, p := range harness.All() {
		h := p.Harness()
		got, ok := harness.NewRooted(h)
		if !ok {
			t.Fatalf("NewRooted(%s) = false, want true", h)
		}
		if roots := got.Roots(); len(roots) != 0 {
			t.Errorf("NewRooted(%s) fell back to %q despite an explicit empty root list", h, roots)
		}
		want := filepath.Join(home, "synthetic", string(h))
		explicit, _ := harness.NewRooted(h, want)
		if roots := explicit.Roots(); !reflect.DeepEqual(roots, []string{want}) {
			t.Errorf("NewRooted(%s, %q).Roots() = %q, want the explicit root (env/default fallback?)", h, want, roots)
		}
	}
	if after := dirEntries(t, home); !reflect.DeepEqual(after, before) {
		t.Errorf("constructing rooted parsers performed IO under HOME: %q -> %q", before, after)
	}
}

// TestNewRootedConcurrentAccess exercises the registry under -race: many
// goroutines build rooted parsers (including for every harness) while reading
// the registry, and the singleton set must be unchanged at the end.
func TestNewRootedConcurrentAccess(t *testing.T) {
	parsers := harness.All()
	const workers = 8
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				root := filepath.Join(os.TempDir(), "mytoken-rooted-test", fmt.Sprintf("w%d-i%d", w, i))
				for _, p := range parsers {
					h := p.Harness()
					got, ok := harness.NewRooted(h, root)
					if !ok {
						t.Errorf("NewRooted(%s, ...) = false, want true", h)
						return
					}
					if got.Harness() != h {
						t.Errorf("NewRooted(%s, ...).Harness() = %q, want %q", h, got.Harness(), h)
						return
					}
				}
				harness.All()
				harness.Get(model.ClaudeCode)
			}
		}(w)
	}
	wg.Wait()

	after := harness.All()
	if len(after) != len(parsers) {
		t.Fatalf("registry size changed under concurrent NewRooted: %d -> %d", len(parsers), len(after))
	}
	for i := range parsers {
		if after[i] != parsers[i] {
			t.Errorf("registered singleton for %s changed under concurrent NewRooted", parsers[i].Harness())
		}
	}
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
