// Root-provenance integration tests for instance.Resolve.
//
// They pin the resolution rule the multi-instance scanner depends on:
//
//	configured sources  >  explicit environment / harness override  >  built-ins
//
// A missing or unusable root in the middle tier is a hard error and is never
// replaced by a built-in default, a missing built-in default stays a non-fatal
// skip, and a configured entry suppresses the environment tier entirely.
package instance_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	// The registry-scale test below needs every parser registered, and parsers
	// register themselves from their own package's init.
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
	_ "github.com/zzstar101/mytoken/internal/harness/mux"
	_ "github.com/zzstar101/mytoken/internal/harness/openclaude"
	_ "github.com/zzstar101/mytoken/internal/harness/openclaw"
	_ "github.com/zzstar101/mytoken/internal/harness/opencode"
	_ "github.com/zzstar101/mytoken/internal/harness/pi"
	_ "github.com/zzstar101/mytoken/internal/harness/qwen"
	_ "github.com/zzstar101/mytoken/internal/harness/roo"
	_ "github.com/zzstar101/mytoken/internal/harness/workbuddy"
	_ "github.com/zzstar101/mytoken/internal/harness/zed"
	"github.com/zzstar101/mytoken/internal/instance"
	"github.com/zzstar101/mytoken/internal/model"
)

// fakeProvenanced stands in for a registered, provenance-aware harness parser.
// The registry is never touched: Resolve only needs the optional capability.
type fakeProvenanced struct {
	h     model.Harness
	roots []harness.Root
	err   error
	// reads counts RootProvenance calls so a test can prove the environment
	// tier is never consulted when it must be suppressed.
	reads *int
	// rootsReads counts Roots calls (legacy fallbacks).
	rootsReads *int
}

func (f fakeProvenanced) Harness() model.Harness { return f.h }

func (f fakeProvenanced) Roots() []string {
	if f.rootsReads != nil {
		*f.rootsReads++
	}
	return harness.RootPaths(f.roots)
}

func (f fakeProvenanced) RootProvenance() ([]harness.Root, error) {
	if f.reads != nil {
		*f.reads++
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.roots, nil
}

var _ instance.Provenanced = fakeProvenanced{}

func resolvedFor(rs []instance.Resolved, h model.Harness) (instance.Resolved, bool) {
	for _, r := range rs {
		if r.Harness == h {
			return r, true
		}
	}
	return instance.Resolved{}, false
}

// TestResolveMissingExplicitEnvRootIsFatalAndNotReplaced pins the first half of
// the middle tier: an override the user set names a directory that does not
// exist, so the whole default instance is refused instead of silently scanning
// the built-in dataset.
func TestResolveMissingExplicitEnvRootIsFatalAndNotReplaced(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	cfg := newCfg()
	rs, ps := instance.Resolve(cfg, []instance.Fallback{fakeProvenanced{
		h:     model.ClaudeCode,
		roots: []harness.Root{{Path: missing, Origin: harness.OriginEnv, Env: "CLAUDE_CONFIG_DIR"}},
	}})
	p, ok := findProblem(ps, instance.CodeMissingDir)
	if !ok {
		t.Fatalf("want a missing-dir problem, got %+v", ps)
	}
	if !p.Fatal {
		t.Fatalf("an explicit env root that does not exist must be fatal: %+v", p)
	}
	if p.ID != instance.DefaultID || p.Harness != model.ClaudeCode {
		t.Fatalf("problem = %+v, want the default id for %s", p, model.ClaudeCode)
	}
	if !strings.Contains(p.Message, "CLAUDE_CONFIG_DIR") {
		t.Errorf("problem message %q should name the environment variable", p.Message)
	}
	if r, ok := resolvedFor(rs, model.ClaudeCode); ok {
		t.Fatalf("an unusable explicit env root must not be replaced by a default instance: %+v", r)
	}
}

// TestResolveUnusableExplicitEnvRootVariantsIsFatal covers the other ways an
// explicit root can be wrong: a regular file and an unreadable directory. The
// file case runs on every platform; unreadable means "mode bits deny reading",
// a concept Windows does not have, so that case is skipped there.
func TestResolveUnusableExplicitEnvRootVariantsIsFatal(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	unreadable := t.TempDir()
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o700) })

	for _, tc := range []struct {
		name string
		path string
		code instance.Code
		// unixModeBits marks a case whose setup needs POSIX permission bits.
		unixModeBits bool
	}{
		{"file", file, instance.CodeNotDir, false},
		{"unreadable", unreadable, instance.CodePermission, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.unixModeBits && runtime.GOOS == "windows" {
				// Windows ignores os.Chmod on directories, so an existing but
				// unreadable directory cannot be produced portably. The "file"
				// case above still asserts the fatal refusal on Windows.
				t.Skip("windows: os.Chmod does not deny directory reads; an unreadable directory requires an ACL")
			}
			if os.Geteuid() == 0 {
				t.Skip("running as root: permission bits are not enforced")
			}
			cfg := newCfg()
			rs, ps := instance.Resolve(cfg, []instance.Fallback{fakeProvenanced{
				h:     model.Codex,
				roots: []harness.Root{{Path: tc.path, Origin: harness.OriginEnv, Env: "CODEX_HOME"}},
			}})
			p, ok := findProblem(ps, tc.code)
			if !ok {
				t.Fatalf("want a %s problem, got %+v", tc.code, ps)
			}
			if !p.Fatal {
				t.Fatalf("problem must be fatal: %+v", p)
			}
			if r, ok := resolvedFor(rs, model.Codex); ok {
				t.Fatalf("default instance must be refused: %+v", r)
			}
		})
	}
}

// TestResolveMissingBuiltinRootIsNonFatal pins the second half of the middle
// tier: a built-in platform default that is absent on this machine is a normal
// skip, diagnosed but not fatal, and the harness keeps its default instance.
func TestResolveMissingBuiltinRootIsNonFatal(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-installed")
	cfg := newCfg()
	rs, ps := instance.Resolve(cfg, []instance.Fallback{fakeProvenanced{
		h:     model.ClaudeCode,
		roots: []harness.Root{{Path: missing, Origin: harness.OriginBuiltin}},
	}})
	p, ok := findProblem(ps, instance.CodeMissingDir)
	if !ok {
		t.Fatalf("want a missing-dir problem, got %+v", ps)
	}
	if p.Fatal {
		t.Fatalf("a missing built-in default must stay a non-fatal skip: %+v", p)
	}
	if strings.Contains(p.Message, "explicit root from") {
		t.Errorf("built-in problem message must not claim an explicit root: %q", p.Message)
	}
	r, ok := resolvedFor(rs, model.ClaudeCode)
	if !ok {
		t.Fatalf("the built-in default instance must survive: %+v", rs)
	}
	if r.ID != instance.DefaultID || r.Source != instance.PathDefault || !r.Scannable() {
		t.Fatalf("default instance = %+v", r)
	}
}

// TestResolveValidExplicitEnvRootsAreKeptAndTaggedEnv pins the happy path of
// the middle tier: two distinct env-derived roots are both kept and the
// instance is marked as environment-sourced, not as a platform default.
func TestResolveValidExplicitEnvRootsAreKeptAndTaggedEnv(t *testing.T) {
	one := mustDir(t, "env-one")
	two := mustDir(t, "env-two")
	cfg := newCfg()
	rs, ps := instance.Resolve(cfg, []instance.Fallback{fakeProvenanced{
		h: model.Codex,
		roots: []harness.Root{
			{Path: one, Origin: harness.OriginEnv, Env: "CODEX_HOME"},
			{Path: two, Origin: harness.OriginEnv, Env: "CODEX_HOME"},
		},
	}})
	if len(ps) != 0 {
		t.Fatalf("problems = %+v", ps)
	}
	if len(rs) != 1 {
		t.Fatalf("resolved = %+v", rs)
	}
	if rs[0].ID != instance.DefaultID || rs[0].Harness != model.Codex || rs[0].Source != instance.PathEnv {
		t.Fatalf("resolved = %+v, want the default id for codex sourced from the environment", rs[0])
	}
	want := []string{canonicalRoot(t, one), canonicalRoot(t, two)}
	if len(rs[0].Roots) != 2 || rs[0].Roots[0] != want[0] || rs[0].Roots[1] != want[1] {
		t.Fatalf("roots = %q, want both env roots %q", rs[0].Roots, want)
	}
}

// TestResolveMixedEnvThenBuiltinRootsKeepsBothTags pins that a harness whose
// default set is partly an explicit override and partly a platform default is
// reported as environment-sourced, because one unusable root changes the
// decision for the whole instance.
func TestResolveMixedEnvThenBuiltinRootsKeepsBothTags(t *testing.T) {
	envRoot := mustDir(t, "cline-data")
	builtinRoot := mustDir(t, "vscode-tasks")
	cfg := newCfg()
	rs, _ := instance.Resolve(cfg, []instance.Fallback{fakeProvenanced{
		h: model.Cline,
		roots: []harness.Root{
			{Path: builtinRoot, Origin: harness.OriginBuiltin},
			{Path: envRoot, Origin: harness.OriginEnv, Env: "CLINE_DATA_DIR"},
		},
	}})
	if len(rs) != 1 || rs[0].Source != instance.PathEnv {
		t.Fatalf("resolved = %+v, want a single env-sourced instance", rs)
	}
	if len(rs[0].Roots) != 2 {
		t.Fatalf("roots = %q, want both", rs[0].Roots)
	}
}

// TestResolveMalformedExplicitEnvListIsFatalAndNotDefaulted pins the malformed
// list contract: a non-blank override that names no usable directory is a hard
// error, never a quiet conversion into the built-in defaults.
func TestResolveMalformedExplicitEnvListIsFatalAndNotDefaulted(t *testing.T) {
	cfg := newCfg()
	bad := &harness.EnvListError{Env: "MYTOKEN_QWEN_DIRS", Value: string(filepath.ListSeparator)}
	rs, ps := instance.Resolve(cfg, []instance.Fallback{fakeProvenanced{h: model.Qwen, err: bad}})
	p, ok := findProblem(ps, instance.CodeInvalidRoot)
	if !ok {
		t.Fatalf("want an invalid-root problem, got %+v", ps)
	}
	if !p.Fatal {
		t.Fatalf("a malformed explicit list must be fatal: %+v", p)
	}
	if !strings.Contains(p.Message, "MYTOKEN_QWEN_DIRS") {
		t.Errorf("problem message %q should name the environment variable", p.Message)
	}
	if r, ok := resolvedFor(rs, model.Qwen); ok {
		t.Fatalf("a malformed explicit list must not fall back to defaults: %+v", r)
	}
}

// TestResolveConfiguredEntrySuppressesProvenanceReads pins the precedence
// contract at the read level: once a harness has a configured source the
// environment tier is not consulted at all, so an invalid override for that
// harness can never affect resolution.
func TestResolveConfiguredEntrySuppressesProvenanceReads(t *testing.T) {
	root := mustDir(t, "configured")
	cfg := newCfg()
	e := mustAdd(t, cfg, instance.AddRequest{
		Harness: model.ClaudeCode,
		Roots:   []string{root},
		Kind:    instance.KindInstance,
	})
	reads := 0
	rootsReads := 0
	rs, ps := instance.Resolve(cfg, []instance.Fallback{fakeProvenanced{
		h:          model.ClaudeCode,
		roots:      []harness.Root{{Path: filepath.Join(root, "gone"), Origin: harness.OriginEnv, Env: "CLAUDE_CONFIG_DIR"}},
		reads:      &reads,
		rootsReads: &rootsReads,
	}})
	if reads != 0 || rootsReads != 0 {
		t.Fatalf("environment tier read %d times and legacy Roots %d times, want 0 and 0", reads, rootsReads)
	}
	if len(ps) != 0 {
		t.Fatalf("problems = %+v, want none", ps)
	}
	if len(rs) != 1 || rs[0].ID != e.ID || rs[0].Source != instance.PathUser {
		t.Fatalf("resolved = %+v, want only the configured entry %s", rs, e.ID)
	}
}

// TestResolveDisabledAndBackupEntriesSuppressProvenance pins that a harness
// claimed by a disabled or backup-copy entry never contributes an environment
// default (so a disabled dataset is not resurrected by the env tier).
func TestResolveDisabledAndBackupEntriesSuppressProvenance(t *testing.T) {
	claudeRoot := mustDir(t, "claude-backup")
	codexRoot := mustDir(t, "codex-disabled")
	cfg := newCfg()
	backup := mustAdd(t, cfg, instance.AddRequest{
		Harness: model.ClaudeCode, Roots: []string{claudeRoot}, Kind: instance.KindBackupCopy,
	})
	disabled := mustAdd(t, cfg, instance.AddRequest{
		Harness: model.Codex, Roots: []string{codexRoot}, Kind: instance.KindInstance, Disabled: true,
	})

	reads := 0
	rs, ps := instance.Resolve(cfg, []instance.Fallback{
		fakeProvenanced{h: model.ClaudeCode, roots: []harness.Root{{Path: filepath.Join(claudeRoot, "gone"), Origin: harness.OriginEnv, Env: "CLAUDE_CONFIG_DIR"}}, reads: &reads},
		fakeProvenanced{h: model.Codex, roots: []harness.Root{{Path: filepath.Join(codexRoot, "gone"), Origin: harness.OriginEnv, Env: "CODEX_HOME"}}, reads: &reads},
	})
	if reads != 0 {
		t.Fatalf("environment tier was read %d times for a disabled/backup harness, want 0", reads)
	}
	if len(ps) != 0 {
		t.Fatalf("problems = %+v, want none", ps)
	}
	if len(rs) != 2 {
		t.Fatalf("resolved = %+v, want the two configured entries only", rs)
	}
	for _, r := range rs {
		if r.ID == instance.DefaultID {
			t.Fatalf("a disabled/backup entry must not produce a default instance: %+v", r)
		}
	}
	got, ok := resolvedByID(rs, backup.ID)
	if !ok || got.Kind != instance.KindBackupCopy || got.Scannable() {
		t.Fatalf("backup entry = %+v, ok=%v", got, ok)
	}
	got, ok = resolvedByID(rs, disabled.ID)
	if !ok || !got.Disabled || got.Scannable() {
		t.Fatalf("disabled entry = %+v, ok=%v", got, ok)
	}
}

// TestFallbackWithoutProvenanceIsTreatedAsPlatformDefault keeps the adapter
// contract: a plain Fallback (today's parser shape, or a third-party parser)
// declares platform defaults, so its invalid roots stay non-fatal.
func TestFallbackWithoutProvenanceIsTreatedAsPlatformDefault(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	cfg := newCfg()
	rs, ps := instance.Resolve(cfg, []instance.Fallback{fakeFallback{model.ClaudeCode, []string{missing}}})
	p, ok := findProblem(ps, instance.CodeMissingDir)
	if !ok || p.Fatal {
		t.Fatalf("want a non-fatal missing-dir problem, got %+v", ps)
	}
	if _, ok := resolvedFor(rs, model.ClaudeCode); !ok {
		t.Fatalf("a plain fallback keeps its default instance: %+v", rs)
	}
}

// TestDefaultFallbacksAreProvenanceAware pins the runtime assembly API the
// scanner and app will use: every registered harness arrives as a Provenanced
// fallback, and a config that claims every harness suppresses them all.
func TestDefaultFallbacksAreProvenanceAware(t *testing.T) {
	fbs := instance.DefaultFallbacks()
	if len(fbs) < 28 {
		t.Fatalf("DefaultFallbacks = %d entries, want at least 28", len(fbs))
	}
	for _, fb := range fbs {
		if _, ok := fb.(instance.Provenanced); !ok {
			t.Fatalf("fallback for %s (%T) does not implement instance.Provenanced", fb.Harness(), fb)
		}
	}

	root := mustDir(t, "claimed")
	cfg := newCfg()
	for _, fb := range fbs {
		mustAdd(t, cfg, instance.AddRequest{Harness: fb.Harness(), Roots: []string{root}, Kind: instance.KindInstance})
	}
	rs, ps := instance.Resolve(cfg, fbs)
	if len(ps) != 0 {
		t.Fatalf("problems = %+v, want none", ps)
	}
	if len(rs) != len(fbs) {
		t.Fatalf("resolved = %d, want exactly the %d configured entries", len(rs), len(fbs))
	}
	for _, r := range rs {
		if r.ID == instance.DefaultID || r.Source != instance.PathUser {
			t.Fatalf("configured entries must suppress every default: %+v", r)
		}
	}
}
