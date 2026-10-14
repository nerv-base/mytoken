package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/instance"
	"github.com/zzstar101/mytoken/internal/model"
)

// tempFallback is a synthetic parser default so these tests never read the real
// home directory.
type tempFallback struct {
	harness model.Harness
	roots   []string
}

func (f tempFallback) Harness() model.Harness { return f.harness }
func (f tempFallback) Roots() []string        { return f.roots }

// useTempFallback replaces the production default roots with one temp root for
// the duration of the test.
func useTempFallback(t *testing.T, roots ...string) {
	t.Helper()
	prev := sourceFallbacks
	sourceFallbacks = func() []instance.Fallback {
		return []instance.Fallback{tempFallback{harness: model.Codex, roots: roots}}
	}
	t.Cleanup(func() { sourceFallbacks = prev })
}

type srcRootJSON struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
}

type srcEntryJSON struct {
	ID        string        `json:"id"`
	Label     string        `json:"label"`
	Harness   string        `json:"harness"`
	Kind      string        `json:"kind"`
	Origin    string        `json:"origin"`
	Status    string        `json:"status"`
	Scannable bool          `json:"scannable"`
	Reason    string        `json:"reason"`
	Roots     []srcRootJSON `json:"roots"`
}

type srcProblemJSON struct {
	Code    string `json:"code"`
	ID      string `json:"id"`
	Harness string `json:"harness"`
	Path    string `json:"path"`
	Message string `json:"message"`
	Fatal   bool   `json:"fatal"`
}

type srcReportJSON struct {
	ConfigPath    string           `json:"configPath"`
	ConfigPresent bool             `json:"configPresent"`
	ConfigError   string           `json:"configError"`
	Instances     []srcEntryJSON   `json:"instances"`
	Problems      []srcProblemJSON `json:"problems"`
}

func listSources(t *testing.T) (srcReportJSON, string, string, int) {
	t.Helper()
	out, errout, code := runCLI(t, "sources", "list", "--json")
	var rep srcReportJSON
	if e := json.Unmarshal([]byte(out), &rep); e != nil {
		t.Fatalf("sources list --json: %v\nout=%s\nerr=%s", e, out, errout)
	}
	return rep, out, errout, code
}

func entryByID(rep srcReportJSON, id string) (srcEntryJSON, bool) {
	for _, e := range rep.Instances {
		if e.ID == id {
			return e, true
		}
	}
	return srcEntryJSON{}, false
}

func addInstance(t *testing.T, extra ...string) instance.Entry {
	t.Helper()
	args := append([]string{"sources", "add", "--harness", "codex", "--json"}, extra...)
	out, errout, code := runCLI(t, args...)
	if code != 0 {
		t.Fatalf("%v: code=%d stderr=%s", args, code, errout)
	}
	var e instance.Entry
	if err := json.Unmarshal([]byte(out), &e); err != nil {
		t.Fatalf("add json %s: %v", out, err)
	}
	return e
}

func TestSourcesUsageAndDispatch(t *testing.T) {
	t.Setenv("MYTOKEN_HOME", t.TempDir())
	useTempFallback(t, t.TempDir())

	for _, args := range [][]string{
		{"sources"},
		{"sources", "bogus"},
		{"sources", "list", "extra"},
		{"sources", "add"},
		{"sources", "move"},
		{"sources", "remove"},
		{"sources", "promote"},
	} {
		_, errout, code := runCLI(t, args...)
		if code != 2 {
			t.Errorf("%v: code=%d stderr=%s", args, code, errout)
		}
		if !strings.Contains(errout, "Usage: mytoken sources") {
			t.Errorf("%v: stderr missing usage: %s", args, errout)
		}
	}
}

func TestSourcesAddListRemoveRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYTOKEN_HOME", home)
	rootA := t.TempDir()
	useTempFallback(t, t.TempDir())

	e := addInstance(t, "--root", rootA, "--label", "work")
	if !strings.HasPrefix(e.ID, "inst_") {
		t.Fatalf("id %q is not an opaque instance id", e.ID)
	}
	if e.Harness != model.Codex || e.Kind != instance.KindInstance || e.Label != "work" {
		t.Fatalf("entry %+v", e)
	}

	rep, _, _, code := listSources(t)
	if code != 0 {
		t.Fatalf("list code=%d", code)
	}
	if !rep.ConfigPresent {
		t.Fatal("configPresent = false after add")
	}
	got, ok := entryByID(rep, e.ID)
	if !ok {
		t.Fatalf("added id %s missing from list: %+v", e.ID, rep.Instances)
	}
	if got.Origin != string(instance.PathUser) || got.Kind != string(instance.KindInstance) {
		t.Fatalf("origin/kind = %s/%s", got.Origin, got.Kind)
	}
	if got.Status != "enabled" || !got.Scannable {
		t.Fatalf("status/scannable = %s/%t", got.Status, got.Scannable)
	}
	if len(got.Roots) != 1 || !got.Roots[0].Exists {
		t.Fatalf("roots = %+v", got.Roots)
	}

	// The text view must expose the same id/origin/status.
	text, _, code := runCLI(t, "sources", "list")
	if code != 0 {
		t.Fatalf("text list code=%d", code)
	}
	for _, want := range []string{"Config:", e.ID, "user", "enabled"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text list missing %q:\n%s", want, text)
		}
	}

	// The config is written 0600 and is the shared sources.json.
	info, err := os.Stat(filepath.Join(home, instance.FileName))
	if err != nil {
		t.Fatalf("sources.json: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("sources.json mode = %o", info.Mode().Perm())
	}

	if _, errout, code := runCLI(t, "sources", "remove", e.ID); code != 0 {
		t.Fatalf("remove code=%d stderr=%s", code, errout)
	}
	rep, _, _, _ = listSources(t)
	if _, ok := entryByID(rep, e.ID); ok {
		t.Fatalf("removed id still listed: %+v", rep.Instances)
	}
}

func TestSourcesIDsAreOpaqueAndDistinct(t *testing.T) {
	t.Setenv("MYTOKEN_HOME", t.TempDir())
	rootA, rootB := t.TempDir(), t.TempDir()
	useTempFallback(t, t.TempDir())

	a := addInstance(t, "--root", rootA)
	b := addInstance(t, "--root", rootB)
	if a.ID == b.ID {
		t.Fatal("add reused an id")
	}
	for _, e := range []instance.Entry{a, b} {
		if strings.Contains(e.ID, filepath.Base(rootA)) || strings.Contains(e.ID, filepath.Base(rootB)) {
			t.Fatalf("id %q looks derived from the root path", e.ID)
		}
	}
	// An explicit --id is honored, so automation can name an instance.
	c := addInstance(t, "--root", rootA, "--id", "team-a")
	if c.ID != "team-a" {
		t.Fatalf("explicit id = %q", c.ID)
	}
}

func TestSourcesAddRejectsInvalidArguments(t *testing.T) {
	t.Setenv("MYTOKEN_HOME", t.TempDir())
	root := t.TempDir()
	missing := filepath.Join(t.TempDir(), "nope")
	useTempFallback(t, t.TempDir())

	cases := []struct {
		args []string
		want string
	}{
		{[]string{"sources", "add", "--root", root}, "unknown harness"},
		{[]string{"sources", "add", "--harness", "nope", "--root", root}, "unknown harness"},
		{[]string{"sources", "add", "--harness", "codex"}, "at least one --root"},
		{[]string{"sources", "add", "--harness", "codex", "--root", missing}, "does not exist"},
		{[]string{"sources", "add", "--harness", "codex", "--root", root, "--kind", "bogus"}, "invalid --kind"},
		{[]string{"sources", "add", "--harness", "codex", "--root", root, "--root", root}, "duplicate --root"},
		{[]string{"sources", "move", "--root", root}, "Usage: mytoken sources move"},
		{[]string{"sources", "move", "inst_x"}, "at least one --root"},
		{[]string{"sources", "remove"}, "Usage: mytoken sources remove"},
		{[]string{"sources", "promote"}, "Usage: mytoken sources promote"},
		{[]string{"sources", "remove", "a", "b"}, "Usage: mytoken sources remove"},
	}
	for _, c := range cases {
		_, errout, code := runCLI(t, c.args...)
		if code != 2 {
			t.Errorf("%v: code=%d stderr=%s", c.args, code, errout)
		}
		if !strings.Contains(errout, c.want) {
			t.Errorf("%v: stderr %q missing %q", c.args, errout, c.want)
		}
	}
	// Nothing above may have written a config.
	if _, err := os.Stat(filepath.Join(os.Getenv("MYTOKEN_HOME"), instance.FileName)); err == nil {
		t.Fatal("a rejected command wrote sources.json")
	}
}

func TestSourcesBackupCopyPromoteKeepsIdentity(t *testing.T) {
	t.Setenv("MYTOKEN_HOME", t.TempDir())
	missing := filepath.Join(t.TempDir(), "archived")
	useTempFallback(t, t.TempDir())

	// A backup-copy root must not have to exist: it is archived data.
	e := addInstance(t, "--root", missing, "--kind", "backup-copy")
	rep, _, _, code := listSources(t)
	if code != 0 {
		t.Fatalf("list code=%d", code)
	}
	got, ok := entryByID(rep, e.ID)
	if !ok {
		t.Fatalf("backup copy %s not listed", e.ID)
	}
	if got.Status != "backup-copy" || got.Scannable {
		t.Fatalf("status/scannable = %s/%t", got.Status, got.Scannable)
	}
	if len(got.Roots) != 1 || got.Roots[0].Exists {
		t.Fatalf("backup-copy roots should be reported verbatim: %+v", got.Roots)
	}

	// A disabled instance is also stored without stat'ing its roots.
	d := addInstance(t, "--root", missing, "--disabled")
	if rep, _, _, _ = listSources(t); rep.ConfigError != "" {
		t.Fatalf("disabled entry broke the config: %s", rep.ConfigError)
	}
	dg, _ := entryByID(rep, d.ID)
	if dg.Status != "disabled" || dg.Scannable {
		t.Fatalf("disabled status/scannable = %s/%t", dg.Status, dg.Scannable)
	}

	// move keeps the id and the kind.
	rootB := t.TempDir()
	out, errout, code := runCLI(t, "sources", "move", e.ID, "--root", rootB, "--json")
	if code != 0 {
		t.Fatalf("move code=%d stderr=%s", code, errout)
	}
	var moved instance.Entry
	if err := json.Unmarshal([]byte(out), &moved); err != nil {
		t.Fatalf("move json %s: %v", out, err)
	}
	if moved.ID != e.ID || moved.Kind != instance.KindBackupCopy {
		t.Fatalf("move changed identity: %+v", moved)
	}
	if len(moved.Roots) != 1 || moved.Roots[0] != rootB {
		t.Fatalf("move roots = %v", moved.Roots)
	}

	// promote is explicit and also keeps the id.
	out, errout, code = runCLI(t, "sources", "promote", e.ID, "--json")
	if code != 0 {
		t.Fatalf("promote code=%d stderr=%s", code, errout)
	}
	var promoted instance.Entry
	if err := json.Unmarshal([]byte(out), &promoted); err != nil {
		t.Fatalf("promote json %s: %v", out, err)
	}
	if promoted.ID != e.ID || promoted.Kind != instance.KindInstance {
		t.Fatalf("promoted = %+v", promoted)
	}
	// Promoting an instance is not a no-op: it must say so.
	_, errout, code = runCLI(t, "sources", "promote", e.ID)
	if code == 0 || !strings.Contains(errout, "backup-copy") {
		t.Fatalf("second promote code=%d stderr=%s", code, errout)
	}
}

func TestSourcesUnknownIDIsOperationalError(t *testing.T) {
	t.Setenv("MYTOKEN_HOME", t.TempDir())
	useTempFallback(t, t.TempDir())
	for _, args := range [][]string{
		{"sources", "remove", "inst_missing"},
		{"sources", "promote", "inst_missing"},
		{"sources", "move", "inst_missing", "--root", t.TempDir()},
	} {
		_, errout, code := runCLI(t, args...)
		if code != 1 {
			t.Errorf("%v: code=%d stderr=%s", args, code, errout)
		}
		if !strings.Contains(errout, "unknown instance id") {
			t.Errorf("%v: stderr %q", args, errout)
		}
	}
}

func TestSourcesNeverOverwritesMalformedConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYTOKEN_HOME", home)
	useTempFallback(t, t.TempDir())
	bad := []byte("{ not json at all\n")
	path := filepath.Join(home, instance.FileName)
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()

	_, errout, code := runCLI(t, "sources", "add", "--harness", "codex", "--root", root)
	if code != 1 {
		t.Fatalf("add on a malformed config: code=%d stderr=%s", code, errout)
	}
	if !strings.Contains(errout, "invalid-config") {
		t.Fatalf("stderr %q does not name the config error", errout)
	}

	out, errout, code := runCLI(t, "sources", "list", "--json")
	if code != 1 {
		t.Fatalf("list on a malformed config: code=%d", code)
	}
	var rep srcReportJSON
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("malformed-config list JSON %s: %v", out, err)
	}
	if rep.ConfigError == "" || rep.ConfigPresent != true {
		t.Fatalf("report = %+v", rep)
	}
	if len(rep.Instances) != 0 {
		t.Fatalf("instances from a malformed config: %+v", rep.Instances)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, bad) {
		t.Fatalf("malformed config was rewritten: %q", after)
	}
	if _, err := os.Stat(filepath.Join(home, instance.LockFileName)); err == nil {
		t.Fatal("leftover lock file")
	}
}

func TestSourcesListDoesNotCreateDataDir(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "absent", "data")
	t.Setenv("MYTOKEN_HOME", home)
	useTempFallback(t, t.TempDir())

	out, errout, code := runCLI(t, "sources", "list", "--json")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("sources list created the data directory: %v", err)
	}
	var rep srcReportJSON
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.ConfigPresent || rep.ConfigPath == "" {
		t.Fatalf("report = %+v", rep)
	}
	// The synthetic default instance is still described.
	e, ok := entryByID(rep, instance.DefaultID)
	if !ok || e.Origin != string(instance.PathDefault) || !e.Scannable {
		t.Fatalf("default instance = %+v (found=%t)", e, ok)
	}
}

func TestSourcesFatalRootIsReportedNonZero(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYTOKEN_HOME", home)
	useTempFallback(t, t.TempDir())
	missing := filepath.Join(t.TempDir(), "gone")
	body := `{"version":1,"entries":[{"id":"inst_broken","harness":"codex","roots":["` + missing + `"],"kind":"instance"}]}`
	if err := os.WriteFile(filepath.Join(home, instance.FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errout, code := runCLI(t, "sources", "list", "--json")
	if code != 1 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	var rep srcReportJSON
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	got, ok := entryByID(rep, "inst_broken")
	if !ok {
		t.Fatalf("broken instance missing: %+v", rep.Instances)
	}
	if got.Status != "error" || got.Scannable {
		t.Fatalf("broken status/scannable = %s/%t", got.Status, got.Scannable)
	}
	if !strings.Contains(got.Reason, "does not exist") {
		t.Fatalf("reason = %q", got.Reason)
	}
	fatal := false
	for _, p := range rep.Problems {
		if p.Fatal && p.ID == "inst_broken" && p.Code == "missing-dir" {
			fatal = true
		}
	}
	if !fatal {
		t.Fatalf("no fatal missing-dir problem: %+v", rep.Problems)
	}
}

func TestSourcesRemoveDeletesOnlyTheConfigEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYTOKEN_HOME", home)
	root := t.TempDir()
	// A file inside the root stands in for logs/database content.
	sentinel := filepath.Join(root, "keep.log")
	if err := os.WriteFile(sentinel, []byte("data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	useTempFallback(t, t.TempDir())

	e := addInstance(t, "--root", root)
	if _, errout, code := runCLI(t, "sources", "remove", e.ID); code != 0 {
		t.Fatalf("remove code=%d stderr=%s", code, errout)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("remove touched the source directory: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("remove deleted the source directory: %v", err)
	}
}
