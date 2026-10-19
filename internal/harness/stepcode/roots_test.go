package stepcode

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
)

// clearStepEnv removes every variable this parser reads so a test starts from
// the built-in defaults.
func clearStepEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{stepDirsEnv, stepSessionDirEnv, stepAgentDirEnv} {
		t.Setenv(k, "")
	}
}

// TestDefaultRoot checks the built-in default ~/.stepcode/agent/sessions and its
// provenance.
func TestDefaultRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	clearStepEnv(t)

	roots, err := New().RootProvenance()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".stepcode", "agent", "sessions")
	if len(roots) != 1 || roots[0].Path != want {
		t.Fatalf("roots = %+v, want the single default %s", roots, want)
	}
	if roots[0].Origin != harness.OriginBuiltin {
		t.Errorf("origin = %q, want builtin", roots[0].Origin)
	}
	if got := New().Roots(); len(got) != 1 || got[0] != want {
		t.Errorf("Roots() = %v", got)
	}
}

// TestEnvRootPriority walks the documented priority: the MyToken list override
// replaces the whole set, then STEP_CODING_AGENT_SESSION_DIR (which names the
// session root itself), then STEP_CODING_AGENT_DIR (which names the agent root).
func TestEnvRootPriority(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Run("session dir wins over agent dir", func(t *testing.T) {
		session := filepath.Join(t.TempDir(), "sessions-here")
		agent := t.TempDir()
		t.Setenv(stepSessionDirEnv, session)
		t.Setenv(stepAgentDirEnv, agent)
		t.Setenv(stepDirsEnv, "")

		roots, err := New().RootProvenance()
		if err != nil {
			t.Fatal(err)
		}
		if len(roots) != 1 || roots[0].Path != session {
			t.Fatalf("roots = %+v, want %s", roots, session)
		}
		if roots[0].Origin != harness.OriginEnv || roots[0].Env != stepSessionDirEnv {
			t.Errorf("provenance = %+v, want env %s", roots[0], stepSessionDirEnv)
		}
	})

	t.Run("agent dir names the sessions child", func(t *testing.T) {
		agent := t.TempDir()
		t.Setenv(stepSessionDirEnv, "")
		t.Setenv(stepAgentDirEnv, agent)
		t.Setenv(stepDirsEnv, "")

		roots, err := New().RootProvenance()
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(agent, "sessions")
		if len(roots) != 1 || roots[0].Path != want {
			t.Fatalf("roots = %+v, want %s", roots, want)
		}
		if roots[0].Env != stepAgentDirEnv {
			t.Errorf("env = %q, want %q", roots[0].Env, stepAgentDirEnv)
		}
	})

	t.Run("dirs list replaces everything", func(t *testing.T) {
		a, b := t.TempDir(), t.TempDir()
		t.Setenv(stepDirsEnv, a+string(filepath.ListSeparator)+b)
		t.Setenv(stepSessionDirEnv, filepath.Join(t.TempDir(), "ignored"))
		t.Setenv(stepAgentDirEnv, t.TempDir())

		roots, err := New().RootProvenance()
		if err != nil {
			t.Fatal(err)
		}
		if len(roots) != 2 || roots[0].Path != a || roots[1].Path != b {
			t.Fatalf("roots = %+v, want exactly %s and %s", roots, a, b)
		}
		for _, r := range roots {
			if r.Origin != harness.OriginEnv || r.Env != stepDirsEnv {
				t.Errorf("provenance = %+v", r)
			}
		}
	})

	t.Run("tilde is expanded", func(t *testing.T) {
		t.Setenv(stepDirsEnv, "")
		t.Setenv(stepAgentDirEnv, "")
		t.Setenv(stepSessionDirEnv, "~/.stepcode/custom-sessions")

		roots, err := New().RootProvenance()
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(home, ".stepcode", "custom-sessions")
		if len(roots) != 1 || roots[0].Path != want {
			t.Fatalf("roots = %+v, want %s", roots, want)
		}
	})
}

// TestBadDirsListIsHardError: a non-blank explicit override that names no
// directory must fail loudly and must never fall back to the defaults.
func TestBadDirsListIsHardError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(stepSessionDirEnv, "")
	t.Setenv(stepAgentDirEnv, "")

	sep := string(filepath.ListSeparator)
	for _, bad := range []string{sep, sep + sep} {
		t.Setenv(stepDirsEnv, bad)
		roots, err := New().RootProvenance()
		var listErr *harness.EnvListError
		if !errors.As(err, &listErr) {
			t.Fatalf("%q: err = %v, want *harness.EnvListError", bad, err)
		}
		if len(roots) != 0 {
			t.Errorf("%q: roots = %+v, want none (no silent fallback)", bad, roots)
		}
		if got := New().Roots(); len(got) != 0 {
			t.Errorf("%q: Roots() = %v, want none", bad, got)
		}
	}
}

// TestExplicitRoots proves a parser built for explicit roots reports them, and
// that the registered parser implements harness.Rooted.
func TestExplicitRoots(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	p := NewWithRoots(a, b)
	roots, err := p.RootProvenance()
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 || roots[0].Origin != harness.OriginExplicit || roots[1].Path != b {
		t.Fatalf("roots = %+v", roots)
	}

	reg, ok := harness.Get(HarnessID)
	if !ok {
		t.Fatal("step-code parser is not registered")
	}
	rooted, ok := reg.(harness.Rooted)
	if !ok {
		t.Fatal("registered parser does not implement harness.Rooted")
	}
	got := rooted.WithRoots(a)
	if len(got.Roots()) != 1 || got.Roots()[0] != a {
		t.Errorf("WithRoots roots = %v", got.Roots())
	}
	if got.Harness() != HarnessID {
		t.Errorf("WithRoots harness = %q", got.Harness())
	}
}

// TestHarnesstestRootFor guards the entry point the conformance suite uses: an
// explicit root must be the only thing watched, regardless of the environment.
func TestHarnesstestRootFor(t *testing.T) {
	t.Setenv(stepDirsEnv, t.TempDir())
	root := t.TempDir()
	if got := NewWithRoot(root).Roots(); len(got) != 1 || got[0] != root {
		t.Errorf("Roots() = %v, want only %s even with %s set", got, root, stepDirsEnv)
	}
}
