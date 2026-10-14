package instance_test

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/instance"
	"github.com/zzstar101/mytoken/internal/model"
)

// --- helpers ---------------------------------------------------------------

func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, instance.FileName)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func mustAdd(t *testing.T, cfg *instance.Config, req instance.AddRequest) instance.Entry {
	t.Helper()
	e, err := cfg.Add(req)
	if err != nil {
		t.Fatalf("Add(%+v): %v", req, err)
	}
	return e
}

func newCfg() *instance.Config { return &instance.Config{Version: instance.SchemaVersion} }

func mustDir(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(append([]string{t.TempDir()}, parts...)...)
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
	return p
}

func asConfigError(t *testing.T, err error) *instance.ConfigError {
	t.Helper()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	var ce *instance.ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("want *instance.ConfigError, got %T: %v", err, err)
	}
	return ce
}

// --- loading ---------------------------------------------------------------

func TestLoadMissingConfigIsEmptyAndCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	cfg, err := instance.Load(dir)
	if err != nil {
		t.Fatalf("Load(absent): %v", err)
	}
	if cfg.Version != instance.SchemaVersion {
		t.Fatalf("version = %d, want %d", cfg.Version, instance.SchemaVersion)
	}
	if len(cfg.Entries) != 0 {
		t.Fatalf("entries = %v, want none", cfg.Entries)
	}
	if _, err := os.Stat(filepath.Join(dir, instance.FileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load must not create %s: %v", instance.FileName, err)
	}
}

func TestLoadMalformedJSONIsTypedAndPreservesCause(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"version":1,"entries":[`)
	_, err := instance.Load(dir)
	ce := asConfigError(t, err)
	if ce.Code != instance.CodeInvalidConfig {
		t.Fatalf("code = %q, want %q", ce.Code, instance.CodeInvalidConfig)
	}
	if ce.Unwrap() == nil {
		t.Fatal("underlying JSON error must be preserved through Unwrap")
	}
}

func TestLoadNewerVersionRejected(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"version":99,"entries":[]}`)
	_, err := instance.Load(dir)
	ce := asConfigError(t, err)
	if ce.Code != instance.CodeUnsupportedVersion {
		t.Fatalf("code = %q, want %q", ce.Code, instance.CodeUnsupportedVersion)
	}
}

func TestLoadMissingVersionRejected(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"entries":[]}`)
	_, err := instance.Load(dir)
	ce := asConfigError(t, err)
	if ce.Code != instance.CodeInvalidConfig {
		t.Fatalf("code = %q, want %q", ce.Code, instance.CodeInvalidConfig)
	}
}

// An unknown top-level key (for example the plan's original "instances" name)
// must be an error, never a silent empty config that falls back to defaults.
func TestLoadUnknownFieldRejected(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"version":1,"instances":[]}`)
	_, err := instance.Load(dir)
	ce := asConfigError(t, err)
	if ce.Code != instance.CodeInvalidConfig {
		t.Fatalf("code = %q, want %q", ce.Code, instance.CodeInvalidConfig)
	}
}

func TestLoadDuplicateIDRejected(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"version":1,"entries":[
		{"id":"inst_a","harness":"claude-code","roots":["/tmp/a"],"kind":"instance"},
		{"id":"inst_a","harness":"codex","roots":["/tmp/b"],"kind":"instance"}]}`)
	_, err := instance.Load(dir)
	ce := asConfigError(t, err)
	if ce.Code != instance.CodeDuplicateID {
		t.Fatalf("code = %q, want %q", ce.Code, instance.CodeDuplicateID)
	}
}

func TestLoadReservedDefaultIDRejected(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"version":1,"entries":[
		{"id":"default","harness":"claude-code","roots":["/tmp/a"],"kind":"instance"}]}`)
	_, err := instance.Load(dir)
	ce := asConfigError(t, err)
	if ce.Code != instance.CodeReservedID {
		t.Fatalf("code = %q, want %q", ce.Code, instance.CodeReservedID)
	}
}

func TestLoadUnknownHarnessRejected(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"version":1,"entries":[
		{"id":"inst_a","harness":"not-a-real-harness","roots":["/tmp/a"],"kind":"instance"}]}`)
	_, err := instance.Load(dir)
	ce := asConfigError(t, err)
	if ce.Code != instance.CodeUnknownHarness {
		t.Fatalf("code = %q, want %q", ce.Code, instance.CodeUnknownHarness)
	}
}

func TestLoadInvalidKindRejected(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"version":1,"entries":[
		{"id":"inst_a","harness":"claude-code","roots":["/tmp/a"],"kind":"mirror"}]}`)
	_, err := instance.Load(dir)
	ce := asConfigError(t, err)
	if ce.Code != instance.CodeInvalidKind {
		t.Fatalf("code = %q, want %q", ce.Code, instance.CodeInvalidKind)
	}
}

func TestLoadEnabledInstanceWithoutRootsRejected(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"version":1,"entries":[
		{"id":"inst_a","harness":"claude-code","roots":[],"kind":"instance"}]}`)
	_, err := instance.Load(dir)
	ce := asConfigError(t, err)
	if ce.Code != instance.CodeNoRoots {
		t.Fatalf("code = %q, want %q", ce.Code, instance.CodeNoRoots)
	}
}

// A disabled entry may legitimately have no roots at all.
func TestLoadDisabledEntryWithoutRootsAccepted(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"version":1,"entries":[
		{"id":"inst_a","harness":"claude-code","roots":[],"kind":"instance","disabled":true}]}`)
	cfg, err := instance.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Entries) != 1 || !cfg.Entries[0].Disabled {
		t.Fatalf("entries = %+v", cfg.Entries)
	}
}

// --- saving ----------------------------------------------------------------

func TestSaveRoundTripModeAndNoTempLeftovers(t *testing.T) {
	dir := t.TempDir()
	root := mustDir(t, "工作 笔记本")
	cfg := newCfg()
	mustAdd(t, cfg, instance.AddRequest{
		Label:   "work laptop",
		Harness: model.ClaudeCode,
		Roots:   []string{root},
		Kind:    instance.KindInstance,
	})
	if err := cfg.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, instance.FileName))
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	// Unix permission bits are not representable on Windows: os.Stat reports a
	// synthetic 0666 for regular files, so the 0600 hardening can only be
	// asserted where the platform enforces mode bits. The no-leftover check and
	// the content round trip below still run on Windows.
	if runtime.GOOS != "windows" {
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("config mode = %o, want 600", perm)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != instance.FileName {
			t.Fatalf("unexpected leftover file %q (temp files must be renamed or removed)", e.Name())
		}
	}
	got, err := instance.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Entries) != 1 {
		t.Fatalf("entries = %+v", got.Entries)
	}
	e := got.Entries[0]
	if e.Label != "work laptop" || e.Harness != model.ClaudeCode || e.Kind != instance.KindInstance || e.Disabled {
		t.Fatalf("round trip mismatch: %+v", e)
	}
	if len(e.Roots) != 1 || e.Roots[0] != root {
		t.Fatalf("roots = %v, want [%s]", e.Roots, root)
	}
}

func TestSaveIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfg := newCfg()
	mustAdd(t, cfg, instance.AddRequest{Harness: model.Codex, Roots: []string{mustDir(t)}, Kind: instance.KindInstance})
	if err := cfg.Save(dir); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(filepath.Join(dir, instance.FileName))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := instance.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.Save(dir); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(dir, instance.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("save/load/save is not byte-stable:\n%s\n---\n%s", first, second)
	}
}

func TestSaveRejectsInvalidConfigAndKeepsOldFile(t *testing.T) {
	dir := t.TempDir()
	cfg := newCfg()
	e := mustAdd(t, cfg, instance.AddRequest{Label: "keep me", Harness: model.Codex, Roots: []string{mustDir(t)}, Kind: instance.KindInstance})
	if err := cfg.Save(dir); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, instance.FileName))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Entries = append(cfg.Entries, cfg.Entries[0]) // duplicate id
	if err := cfg.Save(dir); err == nil {
		t.Fatal("Save must reject a duplicate id")
	} else if ce := asConfigError(t, err); ce.Code != instance.CodeDuplicateID {
		t.Fatalf("code = %q, want %q", ce.Code, instance.CodeDuplicateID)
	}
	after, err := os.ReadFile(filepath.Join(dir, instance.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("old config was replaced:\n%s\n---\n%s", before, after)
	}
	_ = e
}

// TestSaveAtomicFailureKeepsOldConfig pins that a Save which fails leaves the
// previously committed config byte-for-byte intact and leaves no temporary file
// behind. The write is blocked in two ways so the guarantee is exercised on
// every platform: a read-only data directory (Unix only, because Windows does
// not enforce directory mode bits) and a data directory path that is already a
// regular file, which MkdirAll refuses everywhere.
func TestSaveAtomicFailureKeepsOldConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := newCfg()
	mustAdd(t, cfg, instance.AddRequest{Label: "old", Harness: model.Codex, Roots: []string{mustDir(t)}, Kind: instance.KindInstance})
	if err := cfg.Save(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, instance.FileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Entries[0].Label = "new"

	// A read-only data directory is the precise Unix lever. It is left out on
	// Windows, where chmod does not stop writes, and when running as root,
	// where mode bits are not enforced; the regular-file case below covers
	// those platforms instead of skipping the guarantee.
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		if err := cfg.Save(dir); err == nil {
			t.Fatal("Save must fail when the data dir is not writable")
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if after, err := os.ReadFile(path); err != nil {
			t.Fatalf("read config after a failed Save: %v", err)
		} else if string(before) != string(after) {
			t.Fatalf("failed Save mutated the old config:\n%s\n---\n%s", before, after)
		}
	}

	// Portable failure: the data directory path is an existing regular file
	// (the config file itself), so MkdirAll refuses it on every platform,
	// Windows included, before Save can touch the old bytes.
	if err := cfg.Save(path); err == nil {
		t.Fatal("Save must fail when the data dir path is a regular file")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config after a failed Save: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("failed Save mutated the old config:\n%s\n---\n%s", before, after)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != instance.FileName {
			t.Fatalf("failed Save left %q behind (temporary files must be removed)", e.Name())
		}
	}
}

func TestSavedFileHasNoSecretFields(t *testing.T) {
	dir := t.TempDir()
	cfg := newCfg()
	mustAdd(t, cfg, instance.AddRequest{Label: "work", Harness: model.ClaudeCode, Roots: []string{mustDir(t)}, Kind: instance.KindInstance})
	if err := cfg.Save(dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, instance.FileName))
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	if len(top) != 2 {
		t.Fatalf("top-level fields = %v, want exactly version and entries", top)
	}
	for _, k := range []string{"version", "entries"} {
		if _, ok := top[k]; !ok {
			t.Fatalf("missing top-level field %q", k)
		}
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(top["entries"], &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %v", entries)
	}
	allowed := map[string]bool{"id": true, "label": true, "harness": true, "roots": true, "kind": true, "disabled": true}
	secretish := []string{"token", "secret", "password", "credential", "apikey", "api_key", "auth"}
	for _, e := range entries {
		for k := range e {
			if !allowed[k] {
				t.Fatalf("unexpected entry field %q", k)
			}
			for _, s := range secretish {
				if strings.Contains(strings.ToLower(k), s) {
					t.Fatalf("secret-shaped entry field %q", k)
				}
			}
		}
		for _, k := range []string{"id", "harness", "roots", "kind"} {
			if _, ok := e[k]; !ok {
				t.Fatalf("missing entry field %q", k)
			}
		}
	}
}

// --- ids -------------------------------------------------------------------

func TestNewIDIsOpaqueAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		id, err := instance.NewID()
		if err != nil {
			t.Fatalf("NewID: %v", err)
		}
		if !strings.HasPrefix(id, "inst_") {
			t.Fatalf("id %q lacks the inst_ prefix", id)
		}
		if len(id) < 24 {
			t.Fatalf("id %q is too short to carry 128 random bits", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestAddIDsAreIndependentOfPathAndContent(t *testing.T) {
	root := mustDir(t, "same-content")
	cfg := newCfg()
	a := mustAdd(t, cfg, instance.AddRequest{Label: "same", Harness: model.ClaudeCode, Roots: []string{root}, Kind: instance.KindInstance})
	b := mustAdd(t, cfg, instance.AddRequest{Label: "same", Harness: model.ClaudeCode, Roots: []string{root}, Kind: instance.KindInstance})
	if a.ID == b.ID {
		t.Fatalf("two adds over identical content share id %q", a.ID)
	}
	base := filepath.Base(root)
	for _, id := range []string{a.ID, b.ID} {
		if strings.Contains(strings.ToLower(id), strings.ToLower(base)) {
			t.Fatalf("id %q looks derived from the path", id)
		}
	}
}

func TestAddRejectsReservedAndDuplicateIDs(t *testing.T) {
	cfg := newCfg()
	root := mustDir(t)
	_, err := cfg.Add(instance.AddRequest{ID: instance.DefaultID, Harness: model.ClaudeCode, Roots: []string{root}, Kind: instance.KindInstance})
	if ce := asConfigError(t, err); ce.Code != instance.CodeReservedID {
		t.Fatalf("reserved id code = %q, want %q", ce.Code, instance.CodeReservedID)
	}
	first := mustAdd(t, cfg, instance.AddRequest{Harness: model.ClaudeCode, Roots: []string{root}, Kind: instance.KindInstance})
	_, err = cfg.Add(instance.AddRequest{ID: first.ID, Harness: model.Codex, Roots: []string{root}, Kind: instance.KindInstance})
	if ce := asConfigError(t, err); ce.Code != instance.CodeDuplicateID {
		t.Fatalf("duplicate id code = %q, want %q", ce.Code, instance.CodeDuplicateID)
	}
	if len(cfg.Entries) != 1 {
		t.Fatalf("a rejected Add mutated the config: %+v", cfg.Entries)
	}
}

func TestAddRejectsUnknownHarness(t *testing.T) {
	cfg := newCfg()
	_, err := cfg.Add(instance.AddRequest{Harness: model.Harness("nope"), Roots: []string{mustDir(t)}, Kind: instance.KindInstance})
	if ce := asConfigError(t, err); ce.Code != instance.CodeUnknownHarness {
		t.Fatalf("code = %q, want %q", ce.Code, instance.CodeUnknownHarness)
	}
}

// --- slice isolation -------------------------------------------------------

func TestAddCopiesInputRootsSlice(t *testing.T) {
	a, b := mustDir(t, "a"), mustDir(t, "b")
	roots := []string{a}
	cfg := newCfg()
	e := mustAdd(t, cfg, instance.AddRequest{Harness: model.ClaudeCode, Roots: roots, Kind: instance.KindInstance})
	roots[0] = b
	if cfg.Entries[0].Roots[0] != a {
		t.Fatalf("Add aliased the caller's slice: %v", cfg.Entries[0].Roots)
	}
	if e.Roots[0] != a {
		t.Fatalf("returned entry aliased the caller's slice: %v", e.Roots)
	}
}

func TestLoadReturnsIndependentConfigs(t *testing.T) {
	dir := t.TempDir()
	cfg := newCfg()
	mustAdd(t, cfg, instance.AddRequest{Harness: model.ClaudeCode, Roots: []string{mustDir(t)}, Kind: instance.KindInstance})
	if err := cfg.Save(dir); err != nil {
		t.Fatal(err)
	}
	first, err := instance.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := instance.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	first.Entries[0].Roots[0] = "/mutated"
	if second.Entries[0].Roots[0] == "/mutated" {
		t.Fatal("two Loads share root slices")
	}
}

// --- move / promote / remove ----------------------------------------------

func TestMoveRetainsIDLabelAndHarness(t *testing.T) {
	oldRoot, newRoot := mustDir(t, "old"), mustDir(t, "new")
	cfg := newCfg()
	e := mustAdd(t, cfg, instance.AddRequest{Label: "laptop", Harness: model.ClaudeCode, Roots: []string{oldRoot}, Kind: instance.KindInstance})
	moved, err := cfg.Move(e.ID, []string{newRoot})
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if moved.ID != e.ID {
		t.Fatalf("Move changed the id: %q -> %q", e.ID, moved.ID)
	}
	if moved.Label != "laptop" || moved.Harness != model.ClaudeCode || moved.Kind != instance.KindInstance {
		t.Fatalf("Move changed immutable attributes: %+v", moved)
	}
	if len(moved.Roots) != 1 || moved.Roots[0] != newRoot {
		t.Fatalf("roots = %v, want [%s]", moved.Roots, newRoot)
	}
	if cfg.Entries[0].Roots[0] != newRoot {
		t.Fatalf("config not updated: %+v", cfg.Entries[0])
	}
}

func TestMoveRejectsUnknownIDAndEmptyRoots(t *testing.T) {
	cfg := newCfg()
	root := mustDir(t)
	e := mustAdd(t, cfg, instance.AddRequest{Harness: model.ClaudeCode, Roots: []string{root}, Kind: instance.KindInstance})
	if _, err := cfg.Move("inst_missing", []string{root}); asConfigError(t, err).Code != instance.CodeNotFound {
		t.Fatalf("unknown id code = %q", asConfigError(t, err).Code)
	}
	if _, err := cfg.Move(e.ID, nil); asConfigError(t, err).Code != instance.CodeNoRoots {
		t.Fatalf("empty roots code = %q, want %q", asConfigError(t, err).Code, instance.CodeNoRoots)
	}
	if cfg.Entries[0].Roots[0] != root {
		t.Fatalf("a rejected Move mutated the config: %+v", cfg.Entries[0])
	}
}

func TestPromoteBackupCopyIsExplicit(t *testing.T) {
	archived := filepath.Join(t.TempDir(), "offline", "archive")
	cfg := newCfg()
	e := mustAdd(t, cfg, instance.AddRequest{Label: "copy", Harness: model.ClaudeCode, Roots: []string{archived}, Kind: instance.KindBackupCopy})
	promoted, err := cfg.Promote(e.ID)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if promoted.Kind != instance.KindInstance || promoted.ID != e.ID {
		t.Fatalf("promote = %+v", promoted)
	}
	if _, err := cfg.Promote(e.ID); asConfigError(t, err).Code != instance.CodeInvalidKind {
		t.Fatalf("promoting a non-backup entry must fail explicitly, got %v", err)
	}
}

func TestRemoveIsConfigOnlyAndKeepsSourceFiles(t *testing.T) {
	root := mustDir(t, "logs")
	source := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(source, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := newCfg()
	e := mustAdd(t, cfg, instance.AddRequest{Harness: model.ClaudeCode, Roots: []string{root}, Kind: instance.KindInstance})
	if err := cfg.Remove(e.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(cfg.Entries) != 0 {
		t.Fatalf("entries = %+v, want none", cfg.Entries)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("Remove touched source data: %v", err)
	}
	if err := cfg.Remove(e.ID); asConfigError(t, err).Code != instance.CodeNotFound {
		t.Fatalf("removing twice = %v, want not-found", err)
	}
}

// --- validate --------------------------------------------------------------

func TestValidateRejectsDuplicateAndReservedIDs(t *testing.T) {
	dup := &instance.Config{Version: instance.SchemaVersion, Entries: []instance.Entry{
		{ID: "inst_a", Harness: model.ClaudeCode, Roots: []string{"/tmp/a"}, Kind: instance.KindInstance},
		{ID: "inst_a", Harness: model.Codex, Roots: []string{"/tmp/b"}, Kind: instance.KindInstance},
	}}
	if ce := asConfigError(t, dup.Validate()); ce.Code != instance.CodeDuplicateID {
		t.Fatalf("code = %q, want %q", ce.Code, instance.CodeDuplicateID)
	}
	reserved := &instance.Config{Version: instance.SchemaVersion, Entries: []instance.Entry{
		{ID: instance.DefaultID, Harness: model.ClaudeCode, Roots: []string{"/tmp/a"}, Kind: instance.KindInstance},
	}}
	if ce := asConfigError(t, reserved.Validate()); ce.Code != instance.CodeReservedID {
		t.Fatalf("code = %q, want %q", ce.Code, instance.CodeReservedID)
	}
}

// Disabled entries are still validated for schema and ids.
func TestValidateChecksDisabledEntries(t *testing.T) {
	cfg := &instance.Config{Version: instance.SchemaVersion, Entries: []instance.Entry{
		{ID: instance.DefaultID, Harness: model.ClaudeCode, Roots: nil, Kind: instance.KindInstance, Disabled: true},
	}}
	if ce := asConfigError(t, cfg.Validate()); ce.Code != instance.CodeReservedID {
		t.Fatalf("code = %q, want %q", ce.Code, instance.CodeReservedID)
	}
}
