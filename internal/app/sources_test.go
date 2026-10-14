package app

// Tests for the multi-instance app wiring: sources.json -> instance.Resolve ->
// rooted parsers -> scan.NewWithInstances, with the resolved diagnostics that
// the forthcoming GUI/CLI read.
//
// Every test runs inside a throwaway HOME/MYTOKEN_HOME tree and feeds the
// scanner only synthetic Claude JSONL it writes itself, so no real log,
// database or credential is opened. The only network call in the app is the
// price refresh inside open(true); tests therefore use OpenLocal (offline)
// except the one test that asserts open(true) returns before that worker
// exists.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/instance"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

// sandbox isolates a test from the machine: HOME, USERPROFILE and MYTOKEN_HOME
// point into a fresh tree, and every harness home/config override is blanked so
// no parser can resolve a real directory. Blank counts as absent
// (harness.Getenv).
//
// os.UserHomeDir reads HOME on Unix but USERPROFILE on Windows (HOMEDRIVE +
// HOMEPATH are only its fallback), so setting HOME alone let a Windows run
// resolve the runner's real home - which is what broke
// TestClaudeQuotaSourcesKeychainOnlyForActualDefaultProfile. Every variable the
// product can consult is pinned.
func sandbox(t *testing.T) (home, data string) {
	t.Helper()
	root := t.TempDir()
	home = filepath.Join(root, "home")
	data = filepath.Join(root, "data")
	for _, dir := range []string{home, data} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasSuffix(key, "_HOME") || strings.HasSuffix(key, "_CONFIG_DIR") || strings.HasSuffix(key, "_DIR") {
			t.Setenv(key, "")
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	t.Setenv("MYTOKEN_HOME", data)
	return home, data
}

// writeClaudeSession writes the minimal two-line JSONL the claude parser needs:
// one user line for the session, one assistant line carrying token usage.
func writeClaudeSession(t *testing.T, projects, project, session string, in, out int) string {
	t.Helper()
	dir := filepath.Join(projects, project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, session+".jsonl")
	body := strings.Join([]string{
		`{"type":"user","timestamp":"2026-01-02T03:04:05Z","sessionId":"` + session + `","cwd":"/tmp/` + project + `","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","timestamp":"2026-01-02T03:04:06Z","sessionId":"` + session + `","requestId":"req-` + session + `","message":{"id":"msg-` + session + `","model":"claude-sonnet-4-20250514","usage":{"input_tokens":` + strconv.Itoa(in) + `,"output_tokens":` + strconv.Itoa(out) + `}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func saveConfig(t *testing.T, data string, cfg *instance.Config) {
	t.Helper()
	if cfg.Version == 0 {
		cfg.Version = instance.SchemaVersion
	}
	if err := cfg.Save(data); err != nil {
		t.Fatal(err)
	}
}

// openScanned opens offline and runs one full scan, closing the app when the
// test ends.
func openScanned(t *testing.T) *App {
	t.Helper()
	a, err := OpenLocal()
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if err := a.Scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return a
}

func totals(t *testing.T, a *App, filter query.Filter) query.Totals {
	t.Helper()
	v, err := a.Query.Totals(context.Background(), filter)
	if err != nil {
		t.Fatalf("Totals(%+v): %v", filter, err)
	}
	return v
}

func assertNoFile(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Errorf("%s exists: %s", path, why)
	}
}

// TestRegisteredParsersAreProvenanced pins the two registry invariants the
// wiring relies on: every registered parser can report where its roots came
// from (so env/HOME is read live, not frozen at init) and every one can be
// re-rooted (so an explicit config root is wireable).
func TestRegisteredParsersAreProvenanced(t *testing.T) {
	all := harness.All()
	if len(all) == 0 {
		t.Fatal("harness.All() is empty: app.go's blank imports must register the parsers")
	}
	for _, p := range all {
		if _, ok := p.(instance.Provenanced); !ok {
			t.Errorf("harness %s: %T does not implement instance.Provenanced", p.Harness(), p)
		}
		if _, ok := harness.NewRooted(p.Harness(), t.TempDir()); !ok {
			t.Errorf("harness %s is not rooted: explicit roots cannot be wired", p.Harness())
		}
	}
}

// TestSourcesDefaultDiscoveryWhenNoConfig is the legacy-compatibility guard:
// with no sources.json the app must behave exactly as before, discovering the
// env-declared claude root under the "default" instance id.
func TestSourcesDefaultDiscoveryWhenNoConfig(t *testing.T) {
	_, data := sandbox(t)
	cfgDir := t.TempDir()
	projects := filepath.Join(cfgDir, "projects")
	writeClaudeSession(t, projects, "proj", "s1", 100, 50)
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	assertNoFile(t, filepath.Join(data, instance.FileName), "fixture must not create sources.json")

	a, err := OpenLocal()
	if err != nil {
		t.Fatalf("OpenLocal with no sources.json: %v", err)
	}
	defer a.Close()

	var claude []Source
	for _, s := range a.Sources {
		if s.Harness == model.ClaudeCode {
			claude = append(claude, s)
		}
	}
	if len(claude) != 1 {
		t.Fatalf("claude sources = %d (%+v), want 1", len(claude), a.Sources)
	}
	got := claude[0]
	if got.ID != instance.DefaultID {
		t.Errorf("ID = %q, want %q", got.ID, instance.DefaultID)
	}
	if got.Source != instance.PathEnv {
		t.Errorf("Source = %q, want %q (env-declared root)", got.Source, instance.PathEnv)
	}
	if !got.Scannable() || !got.Scanned {
		t.Errorf("Scannable/Scanned = %v/%v, want true/true", got.Scannable(), got.Scanned)
	}
	// Every other harness is simply not installed under the throwaway HOME, so
	// its missing built-in roots are reported non-fatally and nothing else is
	// scanned.
	if scanned := a.ScannedSources(); len(scanned) != 1 || scanned[0].Harness != model.ClaudeCode {
		t.Errorf("scanned sources = %+v, want only the env-declared claude root", scanned)
	}
	for _, p := range a.SourceProblems {
		if p.Fatal {
			t.Errorf("a built App carries a fatal problem: %+v", p)
		}
	}

	if err := a.Scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	v := totals(t, a, query.Filter{Harnesses: []model.Harness{model.ClaudeCode}})
	if v.Requests != 1 {
		t.Errorf("claude requests = %d, want 1", v.Requests)
	}
	if v.Tokens.Input != 100 || v.Tokens.Output != 50 {
		t.Errorf("claude tokens = %+v, want input 100 output 50", v.Tokens)
	}
}

// TestSourcesExplicitInstancesScanIndependently is the core wiring proof:
// two explicit instances with byte-identical logs are scanned separately, the
// env root is replaced rather than added, and cursors stay per instance.
func TestSourcesExplicitInstancesScanIndependently(t *testing.T) {
	_, data := sandbox(t)
	rootA, rootB := t.TempDir(), t.TempDir()
	writeClaudeSession(t, rootA, "proj", "s1", 100, 50)
	writeClaudeSession(t, rootB, "proj", "s1", 100, 50) // identical content, distinct root
	cfgDir := t.TempDir()
	writeClaudeSession(t, filepath.Join(cfgDir, "projects"), "proj", "s1", 100, 50)
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)

	saveConfig(t, data, &instance.Config{Entries: []instance.Entry{
		{ID: "inst_a", Label: "A", Harness: model.ClaudeCode, Roots: []string{rootA}, Kind: instance.KindInstance},
		{ID: "inst_b", Label: "B", Harness: model.ClaudeCode, Roots: []string{rootB}, Kind: instance.KindInstance},
	}})

	a := openScanned(t)
	if n := len(a.Scanner.Instances()); n != 2 {
		t.Fatalf("scanner instances = %d, want 2", n)
	}
	for _, id := range []string{"inst_a", "inst_b"} {
		if v := totals(t, a, query.Filter{InstanceIDs: []string{id}}); v.Requests != 1 {
			t.Errorf("instance %s requests = %d, want 1 (equal content must not dedupe across instances)", id, v.Requests)
		}
	}
	if v := totals(t, a, query.Filter{InstanceIDs: []string{instance.DefaultID}}); v.Requests != 0 {
		t.Errorf("default requests = %d, want 0: an explicit config replaces the env default", v.Requests)
	}
	if v := totals(t, a, query.Filter{}); v.Requests != 2 {
		t.Errorf("all-instance requests = %d, want 2", v.Requests)
	}

	for _, id := range []string{"inst_a", "inst_b"} {
		cursors, err := a.Store.CursorsIn(context.Background(), model.ClaudeCode, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(cursors) != 1 {
			t.Errorf("instance %s cursors = %d, want 1", id, len(cursors))
		}
	}
	def, err := a.Store.CursorsIn(context.Background(), model.ClaudeCode, instance.DefaultID)
	if err != nil {
		t.Fatal(err)
	}
	if len(def) != 0 {
		t.Errorf("default cursors = %d, want 0", len(def))
	}
}

// TestSourcesBackupCopyAndDisabledAreNotScanned: a backup-copy or disabled
// entry claims its harness (so no default fallback), but contributes no scan
// target and no events, even though it has a valid root.
func TestSourcesBackupCopyAndDisabledAreNotScanned(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry instance.Entry
	}{
		{"backup-copy", instance.Entry{ID: "inst_bk", Label: "bk", Harness: model.ClaudeCode, Kind: instance.KindBackupCopy}},
		{"disabled", instance.Entry{ID: "inst_off", Label: "off", Harness: model.ClaudeCode, Kind: instance.KindInstance, Disabled: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, data := sandbox(t)
			root := t.TempDir()
			writeClaudeSession(t, root, "proj", "s1", 100, 50)
			cfgDir := t.TempDir()
			writeClaudeSession(t, filepath.Join(cfgDir, "projects"), "proj", "s1", 100, 50)
			t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)

			entry := tc.entry
			entry.Roots = []string{root}
			saveConfig(t, data, &instance.Config{Entries: []instance.Entry{entry}})

			a := openScanned(t)
			if n := len(a.Scanner.Instances()); n != 0 {
				t.Fatalf("scanner instances = %d, want 0 (no scan, no fallback)", n)
			}
			if v := totals(t, a, query.Filter{}); v.Requests != 0 {
				t.Errorf("requests = %d, want 0", v.Requests)
			}
			var configured []Source
			for _, s := range a.Sources {
				if s.Source == instance.PathUser {
					configured = append(configured, s)
				}
			}
			if len(configured) != 1 {
				t.Fatalf("configured sources = %+v, want exactly the one entry", configured)
			}
			s := configured[0]
			if s.ID != entry.ID || s.Scannable() || s.Scanned {
				t.Errorf("source = %+v, want id %q with Scannable=false Scanned=false", s, entry.ID)
			}
		})
	}
}

// TestSourcesMalformedConfigStopsBeforeStore: a malformed sources.json is a
// hard failure, never a silent fallback to discovery, and it fails before the
// store exists.
func TestSourcesMalformedConfigStopsBeforeStore(t *testing.T) {
	_, data := sandbox(t)
	if err := os.WriteFile(filepath.Join(data, instance.FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	a, err := OpenLocal()
	if err == nil {
		a.Close()
		t.Fatal("OpenLocal succeeded with a malformed sources.json, want error")
	}
	if a != nil {
		t.Errorf("app = %v, want nil", a)
	}
	var cfgErr *instance.ConfigError
	if !errors.As(err, &cfgErr) {
		t.Errorf("error = %T (%v), want *instance.ConfigError", err, err)
	}
	assertNoFile(t, filepath.Join(data, "mytoken.db"), "a malformed config must fail before store.Open")
}

// TestSourcesInvalidExplicitRootStopsBeforeStore: a configured root that does
// not exist is fatal and names the instance, so the GUI/CLI can point at it.
func TestSourcesInvalidExplicitRootStopsBeforeStore(t *testing.T) {
	_, data := sandbox(t)
	saveConfig(t, data, &instance.Config{Entries: []instance.Entry{{
		ID: "inst_x", Label: "x", Harness: model.ClaudeCode,
		Roots: []string{filepath.Join(data, "does-not-exist")}, Kind: instance.KindInstance,
	}}})

	a, err := OpenLocal()
	if err == nil {
		a.Close()
		t.Fatal("OpenLocal succeeded with a missing explicit root, want error")
	}
	if a != nil {
		t.Errorf("app = %v, want nil", a)
	}
	var srcErr *SourceError
	if !errors.As(err, &srcErr) {
		t.Fatalf("error = %T (%v), want *SourceError", err, err)
	}
	if srcErr.Code != string(instance.CodeMissingDir) {
		t.Errorf("code = %q, want %q", srcErr.Code, instance.CodeMissingDir)
	}
	if srcErr.ID != "inst_x" || srcErr.Harness != model.ClaudeCode {
		t.Errorf("error = %+v, want id inst_x harness %s", srcErr, model.ClaudeCode)
	}
	assertNoFile(t, filepath.Join(data, "mytoken.db"), "a fatal resolution problem must fail before store.Open")
}

// TestFatalConfigStopsOpenBeforeRefreshWorker checks the refresh entry point:
// with a fatal config, Open must not even reach the price worker (whose first
// act is writing prices-attempt.json before any HTTP).
func TestFatalConfigStopsOpenBeforeRefreshWorker(t *testing.T) {
	_, data := sandbox(t)
	if err := os.WriteFile(filepath.Join(data, instance.FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Open()
	if err == nil {
		a.Close()
		t.Fatal("Open succeeded with a malformed sources.json, want error")
	}
	if a != nil {
		t.Errorf("app = %v, want nil", a)
	}
	for _, name := range []string{"mytoken.db", "prices-attempt.json", "prices.json"} {
		assertNoFile(t, filepath.Join(data, name), "a fatal config must stop before the store and the refresh worker")
	}
}

// TestResolveSourcesNoFactoryIsFatal: a scannable entry whose parser factory is
// missing is a hard error, not a silently dropped root.
func TestResolveSourcesNoFactoryIsFatal(t *testing.T) {
	cfg := &instance.Config{Version: instance.SchemaVersion, Entries: []instance.Entry{{
		ID: "inst_a", Label: "a", Harness: model.ClaudeCode, Roots: []string{t.TempDir()}, Kind: instance.KindInstance,
	}}}
	fail := func(model.Harness, ...string) (harness.Parser, bool) { return nil, false }

	rs, err := resolveSources(cfg, nil, fail)
	if err == nil {
		t.Fatalf("resolveSources accepted a missing parser factory: root would be silently dropped (sources=%+v)", rs.sources)
	}
	var srcErr *SourceError
	if !errors.As(err, &srcErr) {
		t.Fatalf("error = %T (%v), want *SourceError", err, err)
	}
	if srcErr.Code != string(CodeNoFactory) {
		t.Errorf("code = %q, want %q", srcErr.Code, CodeNoFactory)
	}
	if len(rs.targets) != 0 {
		t.Errorf("targets = %+v, want none", rs.targets)
	}

	rs, err = resolveSources(cfg, nil, harness.NewRooted)
	if err != nil {
		t.Fatalf("resolveSources with harness.NewRooted: %v", err)
	}
	if len(rs.targets) != 1 || rs.targets[0].ID != "inst_a" {
		t.Fatalf("targets = %+v, want one inst_a", rs.targets)
	}
	if !rs.sources[0].Scanned {
		t.Errorf("source = %+v, want Scanned=true", rs.sources[0])
	}
}

type fakeFallback struct {
	h    model.Harness
	prov func() ([]harness.Root, error)
}

func (f fakeFallback) Harness() model.Harness                  { return f.h }
func (f fakeFallback) Roots() []string                         { roots, _ := f.prov(); return harness.RootPaths(roots) }
func (f fakeFallback) RootProvenance() ([]harness.Root, error) { return f.prov() }

// TestResolveSourcesSurfacesNonFatalProblems: a deduplicated nested default
// root is reported, not swallowed, and still yields a scannable instance.
func TestResolveSourcesSurfacesNonFatalProblems(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "sub")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	fb := fakeFallback{h: model.ClaudeCode, prov: func() ([]harness.Root, error) {
		return append(harness.BuiltinRoots(parent), harness.BuiltinRoots(child)...), nil
	}}

	rs, err := resolveSources(&instance.Config{Version: instance.SchemaVersion}, []instance.Fallback{fb}, harness.NewRooted)
	if err != nil {
		t.Fatalf("resolveSources: %v", err)
	}
	if len(rs.targets) != 1 {
		t.Fatalf("targets = %+v, want the broad root only", rs.targets)
	}
	var overlap bool
	for _, p := range rs.problems {
		if p.Code == instance.CodeOverlap && !p.Fatal {
			overlap = true
		}
	}
	if !overlap {
		t.Errorf("problems = %+v, want a non-fatal overlap", rs.problems)
	}
	if rs.sources[0].Reason == "" {
		t.Errorf("source = %+v, want Reason describing the dropped nested root", rs.sources[0])
	}
}

// TestResolveSourcesEnvOriginMissingRootIsFatal: a missing env-declared default
// root refuses that harness's whole default instance rather than scanning a
// built-in subset.
func TestResolveSourcesEnvOriginMissingRootIsFatal(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	fb := fakeFallback{h: model.ClaudeCode, prov: func() ([]harness.Root, error) {
		return harness.EnvRoots("CLAUDE_CONFIG_DIR", missing), nil
	}}

	rs, err := resolveSources(&instance.Config{Version: instance.SchemaVersion}, []instance.Fallback{fb}, harness.NewRooted)
	if err == nil {
		t.Fatalf("resolveSources accepted a missing env root (sources=%+v)", rs.sources)
	}
	var srcErr *SourceError
	if !errors.As(err, &srcErr) || srcErr.Code == "" {
		t.Fatalf("error = %T (%v), want a coded *SourceError", err, err)
	}
	if len(rs.targets) != 0 {
		t.Errorf("targets = %+v, want none", rs.targets)
	}
}

// TestSourcesMoveKeepsInstanceID: moving an instance to a new root keeps its
// id and therefore its accumulated totals; no new identity appears.
func TestSourcesMoveKeepsInstanceID(t *testing.T) {
	_, data := sandbox(t)
	rootA, rootB := t.TempDir(), t.TempDir()
	writeClaudeSession(t, rootA, "proj", "s1", 100, 50)
	writeClaudeSession(t, rootB, "proj", "s2", 10, 5)
	saveConfig(t, data, &instance.Config{Entries: []instance.Entry{{
		ID: "inst_move", Label: "m", Harness: model.ClaudeCode, Roots: []string{rootA}, Kind: instance.KindInstance,
	}}})

	first, err := OpenLocal()
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := totals(t, first, query.Filter{InstanceIDs: []string{"inst_move"}}); v.Requests != 1 {
		t.Fatalf("before move requests = %d, want 1", v.Requests)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	cfg, err := instance.Load(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Move("inst_move", []string{rootB}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(data); err != nil {
		t.Fatal(err)
	}

	second, err := OpenLocal()
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := second.Scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	var configured []Source
	for _, s := range second.Sources {
		if s.Source == instance.PathUser {
			configured = append(configured, s)
		}
	}
	if len(configured) != 1 || configured[0].ID != "inst_move" {
		t.Fatalf("configured sources = %+v, want only inst_move", configured)
	}
	if !configured[0].Scanned {
		t.Errorf("after move source = %+v, want Scanned=true at the new root", configured[0])
	}
	if v := totals(t, second, query.Filter{InstanceIDs: []string{"inst_move"}}); v.Requests != 2 {
		t.Errorf("after move requests = %d, want 2 (both roots accumulated under one id)", v.Requests)
	}
	if v := totals(t, second, query.Filter{InstanceIDs: []string{instance.DefaultID}}); v.Requests != 0 {
		t.Errorf("default requests = %d, want 0: a move must not invent a new identity", v.Requests)
	}
}
