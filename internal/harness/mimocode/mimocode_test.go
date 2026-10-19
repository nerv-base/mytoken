package mimocode

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
	"github.com/zzstar101/mytoken/internal/harness/opencode"
	"github.com/zzstar101/mytoken/internal/harness/opencode/sqlitecore"
	"github.com/zzstar101/mytoken/internal/model"
)

// ---------------------------------------------------------------------------
// fixture helpers
// ---------------------------------------------------------------------------

// mimoDDL is the MiMo Code schema the parser reads: the tables of
// packages/cli/src/session/session.sql.ts (session, message, part) with the
// columns the offline parser is allowed to look at. `part` holds conversation
// content and must stay unread; the tests below plant a sentinel in it.
const mimoDDL = `
CREATE TABLE session (
  id text PRIMARY KEY, project_id text NOT NULL DEFAULT 'prj', parent_id text,
  directory text NOT NULL, title text NOT NULL,
  time_created integer NOT NULL, time_updated integer NOT NULL);
CREATE TABLE message (
  id text PRIMARY KEY, session_id text NOT NULL, agent_id text NOT NULL DEFAULT 'main',
  time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
CREATE TABLE part (
  id text PRIMARY KEY, message_id text NOT NULL, session_id text NOT NULL,
  time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
`

// openTestDB opens path and installs the MiMo Code schema, leaving the
// connection open (so a WAL stays live) until the test ends.
func openTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(mimoDDL); err != nil {
		t.Fatalf("schema %s: %v", path, err)
	}
	return db
}

func execSQL(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for i, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("statement %d (%s): %v", i, stmt, err)
		}
	}
}

func seedSession(t *testing.T, db *sql.DB, id, parent, directory, title string, created, updated int64) {
	t.Helper()
	execSQL(t, db, fmt.Sprintf(
		`INSERT INTO session (id,project_id,parent_id,directory,title,time_created,time_updated) VALUES (%s,%s,%s,%s,%s,%d,%d)`,
		quote(id), quote("prj_"+id), nullable(parent), quote(directory), quote(title), created, updated))
}

func seedMessage(t *testing.T, db *sql.DB, id, sessionID string, updated int64, data string) {
	t.Helper()
	execSQL(t, db, fmt.Sprintf(
		`INSERT INTO message (id,session_id,agent_id,time_created,time_updated,data) VALUES (%s,%s,'main',%d,%d,%s)`,
		quote(id), quote(sessionID), updated, updated, quote(data)))
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func nullable(s string) string {
	if s == "" {
		return "NULL"
	}
	return quote(s)
}

// assistantJSON is a MessageV2.Info assistant row body with the exact usage
// shape MiMo Code writes (packages/cli/src/session/message-v2.ts). A blank cost
// omits the field entirely.
func assistantJSON(model, provider string, created int64, in, out, reasoning, cacheRead, cacheWrite int64, cost string) string {
	if cost == "" {
		cost = "null"
	}
	total := in + out + reasoning + cacheRead + cacheWrite
	return fmt.Sprintf(
		`{"role":"assistant","agent":"build","path":{"cwd":"/Users/dev/mimoproj","root":"/Users/dev/mimoproj"},`+
			`"cost":%s,"tokens":{"total":%d,"input":%d,"output":%d,"reasoning":%d,"cache":{"write":%d,"read":%d}},`+
			`"modelID":%q,"providerID":%q,"time":{"created":%d,"completed":%d}}`,
		cost, total, in, out, reasoning, cacheWrite, cacheRead, model, provider, created, created+500)
}

// ---------------------------------------------------------------------------
// parse helpers
// ---------------------------------------------------------------------------

type parsed struct {
	Sources  []harness.Source
	Events   []model.UsageEvent
	Sessions []model.SessionMeta
	Cursors  map[string]harness.Cursor
}

// parseFrom discovers and parses every source, resuming from cur keyed by source
// path — the same loop the shared conformance suite runs.
func parseFrom(t *testing.T, p harness.Parser, cur map[string]harness.Cursor) parsed {
	t.Helper()
	ctx := context.Background()
	srcs, err := p.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	out := parsed{Sources: srcs, Cursors: map[string]harness.Cursor{}}
	for _, src := range srcs {
		batch, err := p.Parse(ctx, src, cur[src.Path])
		if err != nil {
			t.Fatalf("Parse(%s): %v", src.Path, err)
		}
		out.Events = append(out.Events, batch.Events...)
		out.Sessions = append(out.Sessions, batch.Sessions...)
		out.Cursors[src.Path] = batch.Next
	}
	return out
}

func keys(events []model.UsageEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.DedupKey)
	}
	slices.Sort(out)
	return out
}

// eventByID finds the event whose harness key ends in the message id, so the
// same lookup works for events keyed by either harness namespace.
func eventByID(t *testing.T, events []model.UsageEvent, id string) model.UsageEvent {
	t.Helper()
	for _, ev := range events {
		if strings.HasSuffix(ev.DedupKey, ":"+id) {
			return ev
		}
	}
	t.Fatalf("no event for %q in %v", id, keys(events))
	return model.UsageEvent{}
}

// ---------------------------------------------------------------------------
// identity and registration
// ---------------------------------------------------------------------------

// TestIdentityKeepsMiMoCodeSeparate pins the independent source identity: its
// own harness id, its own registered parser and its own dedup key prefix, none
// of which may collide with OpenCode's.
func TestIdentityKeepsMiMoCodeSeparate(t *testing.T) {
	if got, want := string(HarnessID), "mimo-code"; got != want {
		t.Fatalf("HarnessID = %q, want %q", got, want)
	}
	if HarnessID == model.OpenCode {
		t.Fatal("MiMo Code must not reuse the OpenCode harness identity")
	}
	if got := New().Harness(); got != HarnessID {
		t.Fatalf("New().Harness() = %q, want %q", got, HarnessID)
	}
	p, ok := harness.Get(HarnessID)
	if !ok {
		t.Fatalf("harness.Get(%q): not registered", HarnessID)
	}
	if p.Harness() != HarnessID {
		t.Fatalf("registered parser harness = %q, want %q", p.Harness(), HarnessID)
	}
	// Importing the OpenCode package registers it: the two identities coexist.
	if _, ok := harness.Get(model.OpenCode); !ok {
		t.Fatalf("harness.Get(%q): OpenCode parser must stay registered", model.OpenCode)
	}
}

// TestDedupKeyPrefixIsIndependent guards the event key namespace: every key this
// parser emits is "mimo-code:<id>", never an OpenCode key.
func TestDedupKeyPrefixIsIndependent(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, filepath.Join(dir, "mimocode.db"))
	seedSession(t, db, "ses_p", "", "/Users/dev/mimoproj", "t", 1000, 9000)
	seedMessage(t, db, "msg_1", "ses_p", 2000, assistantJSON("claude-sonnet-4-5", "anthropic", 2000, 10, 2, 0, 0, 0, "0.01"))

	got := keys(parseFrom(t, NewWithRoots(dir), nil).Events)
	want := []string{"mimo-code:msg_1"}
	if !slices.Equal(got, want) {
		t.Fatalf("dedup keys = %v, want %v", got, want)
	}
	if strings.HasPrefix(got[0], "opencode:") {
		t.Fatalf("dedup key %q collides with the OpenCode namespace", got[0])
	}
}

// ---------------------------------------------------------------------------
// roots and environment
// ---------------------------------------------------------------------------

func requireRoots(t *testing.T, p *Parser, want []string, origin harness.RootOrigin, env string) {
	t.Helper()
	roots, err := p.RootProvenance()
	if err != nil {
		t.Fatalf("RootProvenance: %v", err)
	}
	if got := harness.RootPaths(roots); !slices.Equal(got, want) {
		t.Fatalf("RootProvenance paths = %v, want %v", got, want)
	}
	if got := p.Roots(); !slices.Equal(got, want) {
		t.Fatalf("Roots() = %v, want %v", got, want)
	}
	for _, r := range roots {
		if r.Origin != origin {
			t.Fatalf("root %q origin = %q, want %q", r.Path, r.Origin, origin)
		}
		if r.Env != env {
			t.Fatalf("root %q env = %q, want %q", r.Path, r.Env, env)
		}
	}
}

func requireBadRoots(t *testing.T, p *Parser, what string) {
	t.Helper()
	roots, err := p.RootProvenance()
	if err == nil {
		t.Fatalf("%s: RootProvenance accepted a bad value: %v", what, roots)
	}
	if got := p.Roots(); len(got) != 0 {
		t.Fatalf("%s: Roots() = %v, want none (a bad explicit value must not fall back)", what, got)
	}
}

// platformData is the data directory MiMo Code's own code resolves, restated
// here independently of the parser instead of calling into it. Upstream is
//
//	data: path.join(xdgData!, "mimocode")        // packages/shared/src/global.ts
//	xdgData = env.XDG_DATA_HOME ||
//	          path.join(os.homedir(), ".local", "share")   // xdg-basedir@5.1.0
//
// xdg-basedir@5.1.0 branches on nothing but the environment: there is no
// darwin/windows case, so a `~/Library/Application Support/mimocode` or
// `%LOCALAPPDATA%/mimocode` default would be a directory MiMo Code never
// writes. The upstream README advertises those two paths, which is document
// drift, not behaviour.
func platformData(t *testing.T, sub string) string {
	t.Helper()
	if v := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); v != "" {
		return filepath.Join(v, sub)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	return filepath.Join(home, ".local", "share", sub)
}

// TestDefaultDataDirectoryFollowsXdgBasedir pins the upstream default rule
// against the README's per-OS story: MiMo Code joins `mimocode` onto
// xdg-basedir's data directory, and xdg-basedir knows exactly one fallback for
// every platform.
func TestDefaultDataDirectoryFollowsXdgBasedir(t *testing.T) {
	clear := func(t *testing.T) {
		t.Helper()
		t.Setenv(envDirs, "")
		t.Setenv(envDB, "")
		t.Setenv(envHome, "")
		t.Setenv("XDG_DATA_HOME", "")
	}

	t.Run("without XDG_DATA_HOME it is <home>/.local/share/mimocode", func(t *testing.T) {
		clear(t)
		requireRoots(t, New(), []string{platformData(t, dataDirName)}, harness.OriginBuiltin, "")
	})

	t.Run("an empty XDG_DATA_HOME counts as unset, like xdg-basedir's ||", func(t *testing.T) {
		clear(t)
		t.Setenv("XDG_DATA_HOME", "")
		requireRoots(t, New(), []string{filepath.Join(mustHome(t), ".local", "share", dataDirName)}, harness.OriginBuiltin, "")
	})

	t.Run("MIMOCODE_HOME beats it", func(t *testing.T) {
		clear(t)
		limit := t.TempDir()
		t.Setenv(envHome, limit)
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		requireRoots(t, New(), []string{filepath.Join(limit, "data")}, harness.OriginEnv, envHome)
	})

	t.Run("the macOS and Windows defaults are never used", func(t *testing.T) {
		clear(t)
		home := mustHome(t)
		roots, err := New().RootProvenance()
		if err != nil {
			t.Fatalf("RootProvenance: %v", err)
		}
		if len(roots) != 1 {
			t.Fatalf("roots = %v, want exactly the default data directory", roots)
		}
		got := roots[0].Path
		for _, legacy := range []string{
			filepath.Join(home, "Library", "Application Support", dataDirName),
			filepath.Join(home, "AppData", "Local", dataDirName),
			filepath.Join(os.Getenv("LOCALAPPDATA"), dataDirName),
		} {
			if got == legacy {
				t.Errorf("default data directory = %q, want the xdg-basedir path %q", got, platformData(t, dataDirName))
			}
		}
		if want := platformData(t, dataDirName); got != want {
			t.Fatalf("default data directory = %q, want %q", got, want)
		}
	})
}

func mustHome(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	return home
}

// TestRootsEnvironmentPriority covers the documented precedence:
// MYTOKEN_MIMO_CODE_DIRS > MIMOCODE_DB (absolute, or relative to the data dir)
// > MIMOCODE_HOME/data > the platform data directory. Every explicit value is
// taken literally: a malformed one yields no roots and a reported error instead
// of a silent fallback.
func TestRootsEnvironmentPriority(t *testing.T) {
	home := t.TempDir()
	limit := t.TempDir()

	clearEnv := func(t *testing.T) {
		t.Helper()
		t.Setenv(envDirs, "")
		t.Setenv(envDB, "")
		t.Setenv(envHome, "")
		t.Setenv("XDG_DATA_HOME", "")
		t.Setenv("HOME", home)
	}

	t.Run("platform default", func(t *testing.T) {
		clearEnv(t)
		requireRoots(t, New(), []string{platformData(t, "mimocode")}, harness.OriginBuiltin, "")
	})

	t.Run("XDG_DATA_HOME", func(t *testing.T) {
		clearEnv(t)
		xdg := t.TempDir()
		t.Setenv("XDG_DATA_HOME", xdg)
		requireRoots(t, New(), []string{filepath.Join(xdg, "mimocode")}, harness.OriginBuiltin, "")
	})

	t.Run("MIMOCODE_HOME", func(t *testing.T) {
		clearEnv(t)
		t.Setenv(envHome, limit)
		requireRoots(t, New(), []string{filepath.Join(limit, "data")}, harness.OriginEnv, envHome)
	})

	t.Run("MIMOCODE_HOME beats the platform default", func(t *testing.T) {
		clearEnv(t)
		other := t.TempDir()
		t.Setenv(envHome, other)
		t.Setenv("XDG_DATA_HOME", limit)
		requireRoots(t, New(), []string{filepath.Join(other, "data")}, harness.OriginEnv, envHome)
	})

	t.Run("MIMOCODE_DB absolute", func(t *testing.T) {
		clearEnv(t)
		dbPath := filepath.Join(limit, "custom-name.sqlite")
		t.Setenv(envDB, dbPath)
		t.Setenv(envHome, t.TempDir())
		requireRoots(t, New(), []string{dbPath}, harness.OriginEnv, envDB)
	})

	t.Run("MIMOCODE_DB relative to MIMOCODE_HOME data", func(t *testing.T) {
		clearEnv(t)
		t.Setenv(envHome, limit)
		t.Setenv(envDB, "mimocode-beta.db")
		requireRoots(t, New(), []string{filepath.Join(limit, "data", "mimocode-beta.db")}, harness.OriginEnv, envDB)
	})

	t.Run("MIMOCODE_DB relative to the platform data dir", func(t *testing.T) {
		clearEnv(t)
		xdg := t.TempDir()
		t.Setenv("XDG_DATA_HOME", xdg)
		t.Setenv(envDB, "mimocode-canary.db")
		requireRoots(t, New(), []string{filepath.Join(xdg, "mimocode", "mimocode-canary.db")}, harness.OriginEnv, envDB)
	})

	t.Run("MYTOKEN_MIMO_CODE_DIRS wins", func(t *testing.T) {
		clearEnv(t)
		a, b := t.TempDir(), t.TempDir()
		t.Setenv(envDirs, strings.Join([]string{a, b}, string(filepath.ListSeparator)))
		t.Setenv(envDB, filepath.Join(limit, "custom.sqlite"))
		t.Setenv(envHome, limit)
		requireRoots(t, New(), []string{a, b}, harness.OriginEnv, envDirs)
	})

	t.Run("a bad MYTOKEN_MIMO_CODE_DIRS is not replaced", func(t *testing.T) {
		clearEnv(t)
		t.Setenv(envDirs, string(filepath.ListSeparator))
		requireBadRoots(t, New(), "MYTOKEN_MIMO_CODE_DIRS without a directory")
	})

	t.Run("a relative MIMOCODE_HOME is rejected", func(t *testing.T) {
		clearEnv(t)
		t.Setenv(envHome, filepath.Join("relative", "home"))
		requireBadRoots(t, New(), "relative MIMOCODE_HOME")
	})

	t.Run("an in-memory MIMOCODE_DB is rejected", func(t *testing.T) {
		clearEnv(t)
		t.Setenv(envDB, ":memory:")
		requireBadRoots(t, New(), "in-memory MIMOCODE_DB")
	})

	t.Run("a bad MIMOCODE_DB does not fall back to the data dir", func(t *testing.T) {
		clearEnv(t)
		t.Setenv(envDB, ":memory:")
		t.Setenv("XDG_DATA_HOME", limit)
		requireBadRoots(t, New(), "in-memory MIMOCODE_DB with a valid data dir")
	})
}

// TestExplicitRootsOverrideEnvironment pins that a parser built for explicit
// roots ignores the environment entirely.
func TestExplicitRootsOverrideEnvironment(t *testing.T) {
	t.Setenv(envDirs, t.TempDir())
	t.Setenv(envHome, t.TempDir())
	a, b := t.TempDir(), t.TempDir()
	requireRoots(t, NewWithRoots(a, b), []string{a, b}, harness.OriginExplicit, "")
	if got := harness.RootPaths(harness.ExplicitRoots(a)); !slices.Equal(got, []string{a}) {
		t.Fatalf("ExplicitRoots = %v", got)
	}
}

// TestWithRootsBuildsAFreshParser covers the harness.Rooted capability: the
// registered singleton must not be mutated.
func TestWithRootsBuildsAFreshParser(t *testing.T) {
	t.Setenv(envDirs, "")
	t.Setenv(envDB, "")
	t.Setenv(envHome, "")
	singleton := New()
	before := singleton.Roots()
	dir := t.TempDir()
	child, ok := singleton.WithRoots(dir).(*Parser)
	if !ok {
		t.Fatalf("WithRoots returned %T", singleton.WithRoots(dir))
	}
	if child == singleton {
		t.Fatal("WithRoots returned the receiver")
	}
	if got := child.Roots(); !slices.Equal(got, []string{dir}) {
		t.Fatalf("child roots = %v, want [%s]", got, dir)
	}
	if got := singleton.Roots(); !slices.Equal(got, before) {
		t.Fatalf("WithRoots mutated the receiver: %v -> %v", before, got)
	}
	if child.Harness() != HarnessID {
		t.Fatalf("child harness = %q", child.Harness())
	}
}

// ---------------------------------------------------------------------------
// discovery
// ---------------------------------------------------------------------------

// TestDiscoverOnlyMiMoDatabases is the discovery contract: exactly the main
// channel database and the channel databases directly below a root, nothing
// else. Credentials, config and log files that sit next to them are never
// reported, and nested directories are not walked.
func TestDiscoverOnlyMiMoDatabases(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("mimocode.db", "db")
	write("mimocode-canary.db", "db")
	write("mimocode.db-wal", "wal")
	write("mimocode.db-shm", "shm")
	write("mimocode.log", harnesstest.SentinelText)
	write("auth.json", harnesstest.SentinelText)
	write("config.json", harnesstest.SentinelText)
	write(".credentials.json", harnesstest.SentinelText)
	write("opencode.db", "db")
	write("notes.txt", harnesstest.SentinelText)
	write(filepath.Join("nested", "mimocode.db"), "db")

	got := parseFrom(t, NewWithRoots(root), nil)
	want := []harness.Source{
		{Path: filepath.Join(root, "mimocode-canary.db"), Kind: "sqlite"},
		{Path: filepath.Join(root, "mimocode.db"), Kind: "sqlite"},
	}
	if !reflect.DeepEqual(got.Sources, want) {
		t.Fatalf("Discover = %v, want %v", got.Sources, want)
	}
	if len(got.Events) != 0 {
		t.Fatalf("non-database decoys produced events: %v", keys(got.Events))
	}
}

// TestDiscoverExplicitDatabaseFile: an explicitly configured file root is taken
// literally, even under a name the directory scan would never match.
func TestDiscoverExplicitDatabaseFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom-name.sqlite")
	db := openTestDB(t, path)
	seedSession(t, db, "ses_x", "", "/Users/dev/mimoproj", "t", 1000, 5000)
	seedMessage(t, db, "msg_x", "ses_x", 2000, assistantJSON("gpt-5", "openai", 2000, 5, 1, 0, 0, 0, ""))

	got := parseFrom(t, NewWithRoots(path), nil)
	want := []harness.Source{{Path: path, Kind: "sqlite"}}
	if !reflect.DeepEqual(got.Sources, want) {
		t.Fatalf("Discover = %v, want %v", got.Sources, want)
	}
	if len(got.Events) != 1 {
		t.Fatalf("events = %v, want 1", keys(got.Events))
	}
}

// TestDiscoverMissingRootIsEmpty: a root that does not exist is not an error and
// yields no sources (the shared discover-missing check).
func TestDiscoverMissingRootIsEmpty(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing-root")
	srcs, err := NewWithRoots(root).Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 0 {
		t.Fatalf("Discover = %v, want none", srcs)
	}
}

// TestMultipleDatabasesAndChannelDatabase: the main database and a channel
// database below one root are both parsed, each event keeping the identity of
// the row it came from.
func TestMultipleDatabasesAndChannelDatabase(t *testing.T) {
	dir := t.TempDir()
	main := openTestDB(t, filepath.Join(dir, "mimocode.db"))
	seedSession(t, main, "ses_main", "", "/Users/dev/mimoproj", "t", 1000, 9000)
	seedMessage(t, main, "msg_main", "ses_main", 2000, assistantJSON("claude-sonnet-4-5", "anthropic", 2000, 10, 2, 0, 0, 0, "0.01"))

	channel := openTestDB(t, filepath.Join(dir, "mimocode-canary.db"))
	seedSession(t, channel, "ses_canary", "", "/Users/dev/otherproj", "t", 1000, 9000)
	seedMessage(t, channel, "msg_canary", "ses_canary", 3000, assistantJSON("kimi-k2", "moonshot", 3000, 20, 4, 0, 0, 0, "0.02"))

	got := parseFrom(t, NewWithRoots(dir), nil)
	if want := []string{"mimo-code:msg_canary", "mimo-code:msg_main"}; !slices.Equal(keys(got.Events), want) {
		t.Fatalf("events = %v, want %v", keys(got.Events), want)
	}
	projects := map[string]string{}
	for _, ev := range got.Events {
		projects[strings.TrimPrefix(ev.DedupKey, "mimo-code:")] = ev.ProjectPath
	}
	want := map[string]string{"msg_main": "mimoproj", "msg_canary": "otherproj"}
	if !reflect.DeepEqual(projects, want) {
		t.Fatalf("project paths = %v, want %v", projects, want)
	}
	if len(got.Sessions) != 2 {
		t.Fatalf("sessions = %v, want 2", got.Sessions)
	}
}

// ---------------------------------------------------------------------------
// usage mapping
// ---------------------------------------------------------------------------

// TestUsageMappingMatchesOpenCodeV1 parses one and the same row set through the
// OpenCode v1 parser and through this one: the usage mapping must be identical
// field by field. Only the parts the two harnesses deliberately differ on do
// differ — the harness id, the dedup key namespace, the stored project path
// (MiMo Code keeps the directory basename) and the stored title (MiMo Code
// keeps none).
func TestUsageMappingMatchesOpenCodeV1(t *testing.T) {
	dir := t.TempDir()
	rows := []struct {
		id       string
		session  string
		parent   string
		updated  int64
		data     string
		expected model.Tokens
	}{
		{
			id: "msg_a", session: "ses_p", updated: 2000,
			data:     assistantJSON("claude-sonnet-4-5", "anthropic", 2000, 1000, 200, 50, 400, 100, "0.0123"),
			expected: model.Tokens{Input: 1000, Output: 200, Reasoning: 50, CacheRead: 400, CacheWrite: 100},
		},
		{
			id: "msg_b", session: "ses_c", parent: "ses_p", updated: 3000,
			data:     assistantJSON("gpt-5", "openai", 3000, 300, 40, 10, 100, 0, "0.004"),
			expected: model.Tokens{Input: 300, Output: 40, Reasoning: 10, CacheRead: 100},
		},
	}
	for _, name := range []string{"opencode.db", "mimocode.db"} {
		db := openTestDB(t, filepath.Join(dir, name))
		seedSession(t, db, "ses_p", "", "/Users/dev/mimoproj", "Fix the thing", 1000, 9000)
		seedSession(t, db, "ses_c", "ses_p", "/Users/dev/mimoproj", "Explore", 1000, 9000)
		for _, row := range rows {
			seedMessage(t, db, row.id, row.session, row.updated, row.data)
		}
		db.Close()
	}

	oc := parseFrom(t, opencode.NewWithRoots(dir), nil)
	mc := parseFrom(t, NewWithRoots(dir), nil)
	if len(oc.Events) != 2 || len(mc.Events) != 2 {
		t.Fatalf("opencode events = %v, mimo events = %v", keys(oc.Events), keys(mc.Events))
	}
	for _, row := range rows {
		a := eventByID(t, oc.Events, row.id)
		b := eventByID(t, mc.Events, row.id)
		if b.Model != a.Model || b.Provider != a.Provider {
			t.Errorf("%s: model/provider = %q/%q, opencode has %q/%q", row.id, b.Model, b.Provider, a.Model, a.Provider)
		}
		if b.Tokens != row.expected || a.Tokens != row.expected {
			t.Errorf("%s: tokens = %+v (opencode %+v), want %+v", row.id, b.Tokens, a.Tokens, row.expected)
		}
		if !reflect.DeepEqual(b.CostUSD, a.CostUSD) {
			t.Errorf("%s: cost = %v, opencode has %v", row.id, b.CostUSD, a.CostUSD)
		}
		if !b.Timestamp.Equal(a.Timestamp) {
			t.Errorf("%s: timestamp = %v, opencode has %v", row.id, b.Timestamp, a.Timestamp)
		}
		if b.SessionID != a.SessionID || b.ParentID != a.ParentID {
			t.Errorf("%s: session/parent = %q/%q, opencode has %q/%q", row.id, b.SessionID, b.ParentID, a.SessionID, a.ParentID)
		}
		if b.Harness != HarnessID || b.Harness == a.Harness {
			t.Errorf("%s: harness = %q, opencode has %q", row.id, b.Harness, a.Harness)
		}
	}

	// Deliberate divergences.
	if oc.Events[0].ProjectPath != "/Users/dev/mimoproj" {
		t.Errorf("opencode project path = %q", oc.Events[0].ProjectPath)
	}
	for _, ev := range mc.Events {
		if ev.ProjectPath != "mimoproj" {
			t.Errorf("MiMo Code project path = %q, want the directory basename", ev.ProjectPath)
		}
	}

	// A row without a `cost` field still counts its usage, and reports no cost.
	db := openTestDB(t, filepath.Join(dir, "mimocode-nocost.db"))
	seedSession(t, db, "ses_n", "", "/Users/dev/mimoproj", "t", 1000, 9000)
	seedMessage(t, db, "msg_n", "ses_n", 4000, assistantJSON("claude-sonnet-4-5", "anthropic", 4000, 120, 12, 0, 0, 0, ""))
	db.Close()
	withNoCost := eventByID(t, parseFrom(t, NewWithRoots(filepath.Join(dir, "mimocode-nocost.db")), nil).Events, "msg_n")
	if withNoCost.CostUSD != nil {
		t.Errorf("cost = %v, want nil when the row has none", *withNoCost.CostUSD)
	}
	if withNoCost.Tokens != (model.Tokens{Input: 120, Output: 12}) {
		t.Errorf("tokens = %+v", withNoCost.Tokens)
	}
}

// TestParentChildSessionsCountOnce: session.parent_id only establishes the
// parent link. Every message is counted exactly once, and a child's messages
// are reported under the child with the parent attached — the parent's own
// messages keep no parent id of their own.
func TestParentChildSessionsCountOnce(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, filepath.Join(dir, "mimocode.db"))
	seedSession(t, db, "ses_parent", "", "/Users/dev/mimoproj", "parent", 1000, 9000)
	seedSession(t, db, "ses_child", "ses_parent", "/Users/dev/mimoproj", "child", 2000, 9500)
	seedMessage(t, db, "msg_p1", "ses_parent", 3000, assistantJSON("claude-sonnet-4-5", "anthropic", 3000, 10, 1, 0, 0, 0, "0.01"))
	seedMessage(t, db, "msg_c1", "ses_child", 4000, assistantJSON("claude-sonnet-4-5", "anthropic", 4000, 20, 2, 0, 0, 0, "0.02"))
	seedMessage(t, db, "msg_c2", "ses_child", 5000, assistantJSON("claude-sonnet-4-5", "anthropic", 5000, 30, 3, 0, 0, 0, "0.03"))

	got := parseFrom(t, NewWithRoots(dir), nil)
	want := []string{"mimo-code:msg_c1", "mimo-code:msg_c2", "mimo-code:msg_p1"}
	if !slices.Equal(keys(got.Events), want) {
		t.Fatalf("events = %v, want %v", keys(got.Events), want)
	}
	if p := eventByID(t, got.Events, "msg_p1"); p.SessionID != "ses_parent" || p.ParentID != "" {
		t.Errorf("parent message session/parent = %q/%q", p.SessionID, p.ParentID)
	}
	for _, id := range []string{"msg_c1", "msg_c2"} {
		c := eventByID(t, got.Events, id)
		if c.SessionID != "ses_child" || c.ParentID != "ses_parent" {
			t.Errorf("child message %s session/parent = %q/%q", id, c.SessionID, c.ParentID)
		}
	}
	seen := map[string]int{}
	for _, ev := range got.Events {
		seen[ev.DedupKey]++
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("event %s counted %d times", k, n)
		}
	}
	if len(got.Sessions) != 2 {
		t.Fatalf("sessions = %v", got.Sessions)
	}
	for _, s := range got.Sessions {
		switch s.SessionID {
		case "ses_child":
			if s.ParentID != "ses_parent" {
				t.Errorf("child session meta parent = %q", s.ParentID)
			}
		case "ses_parent":
			if s.ParentID != "" {
				t.Errorf("parent session meta parent = %q", s.ParentID)
			}
		default:
			t.Errorf("unexpected session %q", s.SessionID)
		}
	}
}

// TestNoUsageRowsEmitNoEvent covers the rows MiMo Code writes without usage: an
// assistant message without a tokens object, and all-zero tokens with zero cost.
// Neither is a request that cost anything.
func TestNoUsageRowsEmitNoEvent(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, filepath.Join(dir, "mimocode.db"))
	seedSession(t, db, "ses_p", "", "/Users/dev/mimoproj", "t", 1000, 9000)
	seedMessage(t, db, "msg_user", "ses_p", 1500, `{"role":"user","time":{"created":1500}}`)
	seedMessage(t, db, "msg_notokens", "ses_p", 2000, `{"role":"assistant","modelID":"gpt-5","providerID":"openai","time":{"created":2000}}`)
	seedMessage(t, db, "msg_zero", "ses_p", 2500, assistantJSON("gpt-5", "openai", 2500, 0, 0, 0, 0, 0, "0"))
	seedMessage(t, db, "msg_ok", "ses_p", 3000, assistantJSON("gpt-5", "openai", 3000, 7, 1, 0, 0, 0, ""))

	got := parseFrom(t, NewWithRoots(dir), nil)
	if want := []string{"mimo-code:msg_ok"}; !slices.Equal(keys(got.Events), want) {
		t.Fatalf("events = %v, want %v", keys(got.Events), want)
	}
}

// TestMalformedOptionalMetadataStillCountsTheRow is the MiMo Code half of the
// reported defect: one optional field with an unexpected JSON type used to fail
// sqlitecore.Decode, and because the `message` watermark advances with the scan
// the tokens were lost for good rather than re-read. The MiMo schema makes this
// harder to spot than OpenCode's: the same usage block is also copied into the
// step-finish `part` row, so a reader that went looking in `part` for the rows
// this one dropped would count a repaired request twice.
//
// The last row is the boundary: a `tokens` value that is not an object states no
// usage, so it is skipped on purpose — and the second pass pins that the
// watermark moved past it rather than stalling on it.
func TestMalformedOptionalMetadataStillCountsTheRow(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, filepath.Join(dir, "mimocode.db"))
	seedSession(t, db, "ses_p", "", "/Users/dev/mimoproj", "Fix the thing", 1000, 9000)
	// The reported shape, via assistantJSON's raw cost slot: cost written as a
	// quoted number. MiMo Code writes it as a JSON number.
	seedMessage(t, db, "msg_quoted_cost", "ses_p", 1742302000000,
		assistantJSON("claude-sonnet-4-5", "anthropic", 1742302000000, 10, 2, 0, 0, 0, `"0.0123"`))
	seedMessage(t, db, "msg_path_string", "ses_p", 1742303000000,
		`{"role":"assistant","path":"/tmp/x","tokens":{"input":20,"output":3},"modelID":"gpt-5","providerID":"openai","time":{"created":1742303000000}}`)
	seedMessage(t, db, "msg_time_text", "ses_p", 1742305000000,
		`{"role":"assistant","time":{"created":"whenever"},"tokens":{"input":30,"output":4},"modelID":"gpt-5","providerID":"openai"}`)
	seedMessage(t, db, "msg_tokens_scalar", "ses_p", 1742306000000,
		`{"role":"assistant","tokens":5,"time":{"created":1742306000000}}`)
	db.Close()

	p := NewWithRoots(dir)
	first := parseFrom(t, p, nil)
	want := []string{"mimo-code:msg_path_string", "mimo-code:msg_quoted_cost", "mimo-code:msg_time_text"}
	if !slices.Equal(keys(first.Events), want) {
		t.Fatalf("events = %v, want %v", keys(first.Events), want)
	}
	for id, tokens := range map[string]model.Tokens{
		"msg_quoted_cost": {Input: 10, Output: 2},
		"msg_path_string": {Input: 20, Output: 3},
		"msg_time_text":   {Input: 30, Output: 4},
	} {
		if got := eventByID(t, first.Events, id).Tokens; got != tokens {
			t.Errorf("%s tokens = %+v, want %+v", id, got, tokens)
		}
	}
	if ev := eventByID(t, first.Events, "msg_quoted_cost"); ev.CostUSD == nil || *ev.CostUSD != 0.0123 {
		t.Errorf("quoted cost = %v, want 0.0123", ev.CostUSD)
	}
	if ev := eventByID(t, first.Events, "msg_path_string"); ev.ProjectPath != "mimoproj" {
		t.Errorf("row with an unreadable path: ProjectPath = %q, want the session directory basename", ev.ProjectPath)
	}
	if ev := eventByID(t, first.Events, "msg_time_text"); !ev.Timestamp.Equal(time.UnixMilli(1742305000000).UTC()) {
		t.Errorf("row with an unreadable time: Timestamp = %v, want the row's time_updated", ev.Timestamp)
	}

	// The unusable row does not hold the watermark: the next pass has no work.
	second := parseFrom(t, p, first.Cursors)
	if len(second.Events) != 0 {
		t.Fatalf("second pass re-emitted %v: the watermark must not stall on the unusable row", keys(second.Events))
	}
}

// ---------------------------------------------------------------------------
// part is never read
// ---------------------------------------------------------------------------

// TestStepFinishPartIsNeverCountedTwice pins why the parser reads only the
// `message` table. Both upstream schemas write the same usage twice: once on the
// assistant message, which is the authoritative record, and once on the
// `step-finish` part that reports how the step ended (MiMo:
// packages/cli/src/session/message-v2.ts:373-390 vs :579-637, both carrying
// `cost` and the identical `tokens` struct). A reader that summed the two would
// double every request it touched, so this plants the very same usage in both
// places and insists on exactly one event carrying exactly one copy of it.
func TestStepFinishPartIsNeverCountedTwice(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, filepath.Join(dir, "mimocode.db"))
	seedSession(t, db, "ses_p", "", "/Users/dev/proj", "Fix the thing", 1000, 9000)
	seedMessage(t, db, "msg_1", "ses_p", 1742302000000,
		assistantJSON("claude-sonnet-4-5", "anthropic", 1742302000000, 10, 2, 0, 4, 1, "0.0123"))
	execSQL(t, db, fmt.Sprintf(
		`INSERT INTO part (id,message_id,session_id,time_created,time_updated,data) VALUES ('prt_1','msg_1','ses_p',1742302000000,1742302000000,%s)`,
		quote(`{"type":"step-finish","reason":"stop","cost":0.0123,"tokens":{"input":10,"output":2,"reasoning":0,"cache":{"read":4,"write":1}}}`)))
	db.Close()

	got := parseFrom(t, NewWithRoots(dir), nil)
	if len(got.Events) != 1 {
		t.Fatalf("events = %v, want only the assistant message", keys(got.Events))
	}
	ev := eventByID(t, got.Events, "msg_1")
	if want := (model.Tokens{Input: 10, Output: 2, CacheRead: 4, CacheWrite: 1}); ev.Tokens != want {
		t.Errorf("tokens = %+v, want %+v: the step-finish part repeats the message's usage", ev.Tokens, want)
	}
	if ev.CostUSD == nil || *ev.CostUSD != 0.0123 {
		t.Errorf("cost = %v, want 0.0123 counted once", ev.CostUSD)
	}
}

// ---------------------------------------------------------------------------
// privacy
// ---------------------------------------------------------------------------

// TestPrivacyKeepsOnlyTheDirectoryBasename plants conversation text in every
// place MiMo Code stores it — the session title, the assistant text and the
// `part` table — plus an absolute directory. None of it may reach the parsed
// output; the project path is reduced to the directory basename.
func TestPrivacyKeepsOnlyTheDirectoryBasename(t *testing.T) {
	dir := t.TempDir()
	const (
		private = harnesstest.SentinelText
		project = "/Users/private-user/work/secret-project-worktree"
	)
	db := openTestDB(t, filepath.Join(dir, "mimocode.db"))
	seedSession(t, db, "ses_p", "", project, private, 1000, 9000)
	// The assistant row carries the same text MiMo Code streams into the
	// message and its parts.
	seedMessage(t, db, "msg_1", "ses_p", 2000,
		strings.TrimSuffix(assistantJSON("claude-sonnet-4-5", "anthropic", 2000, 10, 2, 0, 0, 0, "0.01"), "}")+
			fmt.Sprintf(`,"text":%q}`, private))
	execSQL(t, db, fmt.Sprintf(
		`INSERT INTO part (id,message_id,session_id,time_created,time_updated,data) VALUES ('prt_1','msg_1','ses_p',2000,2000,%s)`,
		quote(`{"type":"text","text":"`+private+`"}`)))

	got := parseFrom(t, NewWithRoots(dir), nil)
	if len(got.Events) != 1 {
		t.Fatalf("events = %v", keys(got.Events))
	}
	blob, err := json.Marshal(harnesstest.Snapshot{
		Harness:  string(HarnessID),
		Events:   got.Events,
		Sessions: got.Sessions,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{private, "private-user", project, "/Users"} {
		if strings.Contains(string(blob), forbidden) {
			t.Errorf("parsed output contains %q:\n%s", forbidden, blob)
		}
	}
	if strings.ContainsAny(string(blob), `/\`) {
		t.Errorf("parsed output leaks a path separator:\n%s", blob)
	}
	if got := got.Events[0].ProjectPath; got != "secret-project-worktree" {
		t.Errorf("project path = %q, want the basename", got)
	}
	if len(got.Sessions) != 1 || got.Sessions[0].Title != "" {
		t.Errorf("sessions = %+v, want no stored title", got.Sessions)
	}
	if got := got.Sessions[0].Project; got != "secret-project-worktree" {
		t.Errorf("session project = %q, want the basename", got)
	}
}

// ---------------------------------------------------------------------------
// read-only access
// ---------------------------------------------------------------------------

// TestSourceIsOpenedReadOnly parses the database and then asks the same shared
// module to write to it: the open must be query_only and every write must fail.
func TestSourceIsOpenedReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mimocode.db")
	db := openTestDB(t, path)
	seedSession(t, db, "ses_p", "", "/Users/dev/mimoproj", "t", 1000, 9000)
	seedMessage(t, db, "msg_1", "ses_p", 2000, assistantJSON("gpt-5", "openai", 2000, 10, 2, 0, 0, 0, "0.01"))
	db.Close()

	before := fileDigest(t, path)
	if got := keys(parseFrom(t, NewWithRoots(dir), nil).Events); !slices.Equal(got, []string{"mimo-code:msg_1"}) {
		t.Fatalf("events = %v", got)
	}
	if after := fileDigest(t, path); after != before {
		t.Fatalf("parsing rewrote the source database: %s -> %s", before, after)
	}

	ro, err := sqlitecore.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ro.Close()
	var mode int
	if err := ro.DB().QueryRow("PRAGMA query_only").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA query_only: %v", err)
	}
	if mode != 1 {
		t.Errorf("PRAGMA query_only = %d, want 1", mode)
	}
	if _, err := ro.DB().Exec(`INSERT INTO session (id,directory,title,time_created,time_updated) VALUES ('ses_write','/tmp','x',1,1)`); err == nil {
		t.Fatal("INSERT succeeded on a read-only snapshot")
	}
	if _, err := ro.DB().Exec(`DELETE FROM message`); err == nil {
		t.Fatal("DELETE succeeded on a read-only snapshot")
	}
	if after := fileDigest(t, path); after != before {
		t.Fatalf("read-only access rewrote the source database: %s -> %s", before, after)
	}
}

// TestActiveWALDatabaseIsReadFromASafeSnapshot keeps a writer connection open so
// the committed rows live in the -wal file, then parses. The snapshot must see
// them, and must not touch the database or its log.
func TestActiveWALDatabaseIsReadFromASafeSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mimocode.db")
	db := openTestDB(t, path)
	execSQL(t, db, `PRAGMA journal_mode=WAL`)
	seedSession(t, db, "ses_p", "", "/Users/dev/mimoproj", "t", 1000, 9000)
	seedMessage(t, db, "msg_1", "ses_p", 2000, assistantJSON("claude-sonnet-4-5", "anthropic", 2000, 10, 2, 0, 0, 0, "0.01"))

	wal := path + "-wal"
	walBefore, err := os.Stat(wal)
	if err != nil {
		t.Fatalf("no write-ahead log next to the database: %v", err)
	}
	dbBefore := fileDigest(t, path)

	// db stays open: the WAL is live and has not been checkpointed away.
	got := parseFrom(t, NewWithRoots(dir), nil)
	if want := []string{"mimo-code:msg_1"}; !slices.Equal(keys(got.Events), want) {
		t.Fatalf("events from a live WAL = %v, want %v", keys(got.Events), want)
	}
	if got.Cursors[path].Extra == "" {
		t.Error("cursor carries no watermark")
	}
	walAfter, err := os.Stat(wal)
	if err != nil {
		t.Fatalf("the parse removed the write-ahead log: %v", err)
	}
	if walAfter.Size() != walBefore.Size() {
		t.Errorf("write-ahead log changed size: %d -> %d", walBefore.Size(), walAfter.Size())
	}
	if after := fileDigest(t, path); after != dbBefore {
		t.Errorf("the parse rewrote the main database: %s -> %s", dbBefore, after)
	}
}

// ---------------------------------------------------------------------------
// incremental reads
// ---------------------------------------------------------------------------

// TestIncrementalResumesFromTheWatermark: a row appended after a parse is
// reported exactly once, and an idle re-parse reports nothing.
func TestIncrementalResumesFromTheWatermark(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mimocode.db")
	db := openTestDB(t, path)
	seedSession(t, db, "ses_p", "", "/Users/dev/mimoproj", "t", 1000, 9000)
	seedMessage(t, db, "msg_1", "ses_p", 2000, assistantJSON("gpt-5", "openai", 2000, 10, 2, 0, 0, 0, "0.01"))

	p := NewWithRoots(dir)
	first := parseFrom(t, p, nil)
	if want := []string{"mimo-code:msg_1"}; !slices.Equal(keys(first.Events), want) {
		t.Fatalf("cold parse = %v, want %v", keys(first.Events), want)
	}
	if idle := parseFrom(t, p, first.Cursors); len(idle.Events) != 0 {
		t.Fatalf("idle re-parse emitted %v", keys(idle.Events))
	}

	seedMessage(t, db, "msg_2", "ses_p", 3000, assistantJSON("gpt-5", "openai", 3000, 20, 4, 0, 0, 0, "0.02"))
	second := parseFrom(t, p, first.Cursors)
	if want := []string{"mimo-code:msg_2"}; !slices.Equal(keys(second.Events), want) {
		t.Fatalf("incremental parse = %v, want %v", keys(second.Events), want)
	}
	if third := parseFrom(t, p, second.Cursors); len(third.Events) != 0 {
		t.Fatalf("third parse emitted %v", keys(third.Events))
	}
}

// TestForeignFileAndUnknownSchemaAreNotFatal: neither a file that is not a
// database nor a database without the MiMo Code tables may fail a scan.
func TestForeignFileAndUnknownSchemaAreNotFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mimocode.db"), []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := parseFrom(t, NewWithRoots(dir), nil); len(got.Events) != 0 || len(got.Sessions) != 0 {
		t.Fatalf("garbage database produced %v / %v", keys(got.Events), got.Sessions)
	}

	other := t.TempDir()
	db := openTestDB(t, filepath.Join(other, "mimocode.db"))
	execSQL(t, db, `DROP TABLE message`, `DROP TABLE session`, `DROP TABLE part`, `CREATE TABLE unrelated (id text PRIMARY KEY)`)
	db.Close()
	if got := parseFrom(t, NewWithRoots(other), nil); len(got.Events) != 0 {
		t.Fatalf("database without MiMo Code tables produced %v", keys(got.Events))
	}
}

// fileDigest is the sha256 of a file, used to prove a source was not touched.
func fileDigest(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
