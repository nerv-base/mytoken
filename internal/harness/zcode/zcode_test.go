package zcode

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/sqlitedsn"
)

// baseSchema is the documented ZCode 0.4.x ledger layout: the required
// model_usage/session columns plus the optional cache-read, cache-creation and
// provider columns. prompt_text/title are the conversation fields MyToken must
// never materialize (privacy).
const baseSchema = `
CREATE TABLE model_usage (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  started_at INTEGER NOT NULL,
  model_id TEXT NOT NULL,
  status TEXT NOT NULL,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_input_tokens INTEGER NOT NULL DEFAULT 0,
  cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
  provider TEXT,
  prompt_text TEXT
);
CREATE TABLE session (
  id TEXT PRIMARY KEY,
  directory TEXT NOT NULL,
  title TEXT
);`

// minimalSchema drops every optional column: no cache split, no provider.
const minimalSchema = `
CREATE TABLE model_usage (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  started_at INTEGER NOT NULL,
  model_id TEXT NOT NULL,
  status TEXT NOT NULL,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE session (
  id TEXT PRIMARY KEY,
  directory TEXT NOT NULL
);`

const sentinel = "SENTINEL_PRIVATE_TEXT"

// writeDB creates a SQLite database at dir/name from the given statements.
// Removing the sidecar files first keeps every fixture byte-deterministic.
func writeDB(t testing.TB, dir, name string, stmts ...string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	removeSidecars(t, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", firstLine(s), err)
		}
	}
	return path
}

// execDB runs statements against an existing database.
func execDB(t testing.TB, path string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", firstLine(s), err)
		}
	}
}

func removeSidecars(t testing.TB, path string) {
	t.Helper()
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove %s%s: %v", path, suffix, err)
		}
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 60 {
		return s[:60]
	}
	return s
}

type parsed struct {
	Events   []model.UsageEvent
	Sessions []model.SessionMeta
	Cursors  map[string]harness.Cursor
}

// parseRoot discovers and parses every source under root, resuming from
// cursors keyed by absolute source path.
func parseRoot(t *testing.T, root string, cursors map[string]harness.Cursor) parsed {
	t.Helper()
	ctx := context.Background()
	p := NewWithRoots(root)
	srcs, err := p.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover(%s): %v", root, err)
	}
	if cursors == nil {
		cursors = map[string]harness.Cursor{}
	}
	res := parsed{Cursors: map[string]harness.Cursor{}}
	for _, src := range srcs {
		b, err := p.Parse(ctx, src, cursors[src.Path])
		if err != nil {
			t.Fatalf("Parse(%s): %v", src.Path, err)
		}
		res.Events = append(res.Events, b.Events...)
		res.Sessions = append(res.Sessions, b.Sessions...)
		res.Cursors[src.Path] = b.Next
	}
	return res
}

func keys(events []model.UsageEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.DedupKey)
	}
	return out
}

func eventByKey(t *testing.T, events []model.UsageEvent, key string) model.UsageEvent {
	t.Helper()
	for _, e := range events {
		if e.DedupKey == key {
			return e
		}
	}
	t.Fatalf("no event with DedupKey %q in %v", key, keys(events))
	return model.UsageEvent{}
}

func sessionByID(t *testing.T, sessions []model.SessionMeta, id string) model.SessionMeta {
	t.Helper()
	for _, s := range sessions {
		if s.SessionID == id {
			return s
		}
	}
	t.Fatalf("no session %q in %+v", id, sessions)
	return model.SessionMeta{}
}

// ---------------------------------------------------------------------------
// field mapping
// ---------------------------------------------------------------------------

func TestParseMapsRowsToEvents(t *testing.T) {
	dir := t.TempDir()
	path := writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory,title) VALUES
		   ('sess-a','/work/demo-a','first user message `+sentinel+`'),
		   ('sess-b','/work/demo-b',NULL),
		   ('sess-c','/work/demo-c',NULL)`,
		`INSERT INTO model_usage
		   (id,session_id,started_at,model_id,status,input_tokens,output_tokens,
		    cache_read_input_tokens,cache_creation_input_tokens,provider,prompt_text) VALUES
		   ('u1','sess-a',1772359200000,'glm-5.3','completed',1000,200,300,150,'zai','`+sentinel+`'),
		   ('u2','sess-b',1772359260000,'glm-5.3-flash','completed',500,100,0,0,NULL,NULL),
		   ('u3','sess-a',1772359320000,'glm-5.3','running',700,50,0,0,'zai',NULL),
		   ('u4','sess-b',1772359380000,'glm-5.3','failed',400,20,0,0,NULL,NULL),
		   ('u5','sess-c',1772359440000,'glm-5.3','completed',120,10,20,0,'zai',NULL)`,
	)
	res := parseRoot(t, dir, nil)
	if got, want := keys(res.Events), []string{"zcode:u1", "zcode:u2", "zcode:u5"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DedupKeys = %v, want %v", got, want)
	}

	e := eventByKey(t, res.Events, "zcode:u1")
	if e.Harness != model.ZCode {
		t.Errorf("Harness = %q, want %q", e.Harness, model.ZCode)
	}
	if e.SessionID != "sess-a" || e.Model != "glm-5.3" || e.Provider != "zai" {
		t.Errorf("session/model/provider = %q/%q/%q, want sess-a/glm-5.3/zai", e.SessionID, e.Model, e.Provider)
	}
	if e.ProjectPath != "/work/demo-a" {
		t.Errorf("ProjectPath = %q, want /work/demo-a", e.ProjectPath)
	}
	if want := time.UnixMilli(1772359200000).UTC(); !e.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %s, want %s", e.Timestamp, want)
	}
	// input_tokens includes both cache buckets, so fresh input is the remainder.
	want := model.Tokens{Input: 550, Output: 200, CacheRead: 300, CacheWrite: 150}
	if e.Tokens != want {
		t.Errorf("Tokens = %+v, want %+v", e.Tokens, want)
	}
	if e.CostUSD != nil {
		t.Errorf("CostUSD = %v, want nil (ZCode records no per-request cost)", *e.CostUSD)
	}
	if e.BaseURL != "" {
		t.Errorf("BaseURL = %q, want empty", e.BaseURL)
	}

	// sess-c's only row is completed in the fixture, so all three sessions show.
	if len(res.Sessions) != 3 {
		t.Fatalf("Sessions = %+v, want 3", res.Sessions)
	}
	a := sessionByID(t, res.Sessions, "sess-a")
	if a.Harness != model.ZCode || a.Project != "/work/demo-a" || a.Title != "" {
		t.Errorf("sess-a meta = %+v, want zcode harness, project /work/demo-a, empty title", a)
	}
	if want := time.UnixMilli(1772359200000).UTC(); !a.StartedAt.Equal(want) {
		t.Errorf("sess-a StartedAt = %s, want the u1 timestamp %s", a.StartedAt, want)
	}
	// u3 (running) must not widen the session's window.
	if want := time.UnixMilli(1772359200000).UTC(); !a.UpdatedAt.Equal(want) {
		t.Errorf("sess-a UpdatedAt = %s, want %s (running rows do not count)", a.UpdatedAt, want)
	}
	if b := sessionByID(t, res.Sessions, "sess-b"); !b.UpdatedAt.Equal(time.UnixMilli(1772359260000).UTC()) {
		t.Errorf("sess-b UpdatedAt = %s, want the u2 timestamp", b.UpdatedAt)
	}
	if !strings.HasSuffix(path, dbName) || path == "" {
		t.Fatalf("fixture path = %q", path)
	}
}

func TestParseCountsOnlyCompletedStatus(t *testing.T) {
	dir := t.TempDir()
	writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`,
		`INSERT INTO model_usage (id,session_id,started_at,model_id,status,input_tokens,output_tokens) VALUES
		   ('c','s',1772359200000,'m','completed',10,1),
		   ('r','s',1772359200000,'m','running',20,2),
		   ('f','s',1772359200000,'m','failed',30,3),
		   ('x','s',1772359200000,'m','cancelled',40,4),
		   ('u','s',1772359200000,'m','COMPLETED',50,5)`,
	)
	res := parseRoot(t, dir, nil)
	if got, want := keys(res.Events), []string{"zcode:c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DedupKeys = %v, want exactly the completed row %v", got, want)
	}
}

func TestParseWithoutOptionalColumnsKeepsInputWhole(t *testing.T) {
	dir := t.TempDir()
	writeDB(t, dir, dbName, minimalSchema,
		`INSERT INTO session (id,directory) VALUES ('s','/w/no-cache')`,
		`INSERT INTO model_usage (id,session_id,started_at,model_id,status,input_tokens,output_tokens) VALUES
		   ('u1','s',1772359200000,'glm-5.3','completed',900,90)`,
	)
	res := parseRoot(t, dir, nil)
	e := eventByKey(t, res.Events, "zcode:u1")
	if want := (model.Tokens{Input: 900, Output: 90}); e.Tokens != want {
		t.Errorf("Tokens = %+v, want %+v (missing cache columns leave Input whole and caches 0)", e.Tokens, want)
	}
	if e.Provider != "" {
		t.Errorf("Provider = %q, want empty without a provider column", e.Provider)
	}
}

func TestParseBoundsCacheBucketsByInput(t *testing.T) {
	dir := t.TempDir()
	writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`,
		`INSERT INTO model_usage
		   (id,session_id,started_at,model_id,status,input_tokens,output_tokens,
		    cache_read_input_tokens,cache_creation_input_tokens) VALUES
		   ('partial','s',1772359200000,'m','completed',100,1,80,50),
		   ('over','s',1772359201000,'m','completed',100,1,300,70),
		   ('negative','s',1772359202000,'m','completed',100,1,-5,-5)`,
	)
	res := parseRoot(t, dir, nil)
	cases := []struct {
		key  string
		want model.Tokens
	}{
		// 50 of the 50 creation tokens fit after the 80-token read bucket.
		{"zcode:partial", model.Tokens{Input: 0, Output: 1, CacheRead: 80, CacheWrite: 20}},
		// The read bucket alone is clamped to the inclusive input count.
		{"zcode:over", model.Tokens{Input: 0, Output: 1, CacheRead: 100, CacheWrite: 0}},
		{"zcode:negative", model.Tokens{Input: 100, Output: 1}},
	}
	for _, c := range cases {
		if got := eventByKey(t, res.Events, c.key).Tokens; got != c.want {
			t.Errorf("%s Tokens = %+v, want %+v", c.key, got, c.want)
		}
	}
	for _, e := range res.Events {
		if e.Tokens.Input < 0 || e.Tokens.CacheRead < 0 || e.Tokens.CacheWrite < 0 {
			t.Errorf("%s has a negative token class: %+v", e.DedupKey, e.Tokens)
		}
	}
}

func TestParseIncompleteSchemaIsIgnored(t *testing.T) {
	cases := map[string][]string{
		"missing status": {`CREATE TABLE model_usage (
			id TEXT PRIMARY KEY, session_id TEXT, started_at INTEGER, model_id TEXT,
			input_tokens INTEGER, output_tokens INTEGER)`,
			`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT)`},
		"missing session table": {`CREATE TABLE model_usage (
			id TEXT PRIMARY KEY, session_id TEXT, started_at INTEGER, model_id TEXT,
			status TEXT, input_tokens INTEGER, output_tokens INTEGER)`},
		"unrelated database": {`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`,
			`INSERT INTO users (id,name) VALUES (1,'someone')`},
	}
	for name, stmts := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeDB(t, dir, dbName, stmts...)
			res := parseRoot(t, dir, nil)
			if len(res.Events) != 0 || len(res.Sessions) != 0 {
				t.Fatalf("incomplete/unrelated database produced %d events, %d sessions; want none",
					len(res.Events), len(res.Sessions))
			}
		})
	}
}

func TestParseMissingOrMalformedDatabaseIsEmptyNotError(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		dir := t.TempDir()
		src := harness.Source{Path: filepath.Join(dir, dbName), Kind: kindSQLite}
		cur := harness.Cursor{Fingerprint: "old", Size: 7}
		b, err := NewWithRoots(dir).Parse(context.Background(), src, cur)
		if err != nil {
			t.Fatalf("Parse(missing) error = %v, want nil", err)
		}
		if len(b.Events) != 0 || b.Next != cur {
			t.Fatalf("Parse(missing) = %+v, want no events and the unchanged cursor %+v", b, cur)
		}
	})

	t.Run("not a database", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, dbName)
		if err := os.WriteFile(path, []byte("this is not a sqlite database, not even close"), 0o644); err != nil {
			t.Fatal(err)
		}
		res := parseRoot(t, dir, nil)
		if len(res.Events) != 0 || len(res.Sessions) != 0 {
			t.Fatalf("garbage database produced %d events, %d sessions; want none", len(res.Events), len(res.Sessions))
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		dir := t.TempDir()
		path := writeDB(t, dir, dbName, baseSchema)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := NewWithRoots(dir).Parse(ctx, harness.Source{Path: path, Kind: kindSQLite}, harness.Cursor{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Parse(canceled) error = %v, want context.Canceled", err)
		}
	})
}

func TestParseUnknownSessionLeavesProjectEmpty(t *testing.T) {
	dir := t.TempDir()
	writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory) VALUES ('other','/w/other')`,
		`INSERT INTO model_usage (id,session_id,started_at,model_id,status,input_tokens,output_tokens) VALUES
		   ('u1','ghost',1772359200000,'m','completed',10,1)`,
	)
	res := parseRoot(t, dir, nil)
	e := eventByKey(t, res.Events, "zcode:u1")
	if e.ProjectPath != "" {
		t.Errorf("ProjectPath = %q, want empty for a session with no directory row", e.ProjectPath)
	}
	if s := sessionByID(t, res.Sessions, "ghost"); s.Project != "" {
		t.Errorf("ghost session Project = %q, want empty", s.Project)
	}
}

func TestParseScalesSecondTimestampsDefensively(t *testing.T) {
	dir := t.TempDir()
	writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`,
		`INSERT INTO model_usage (id,session_id,started_at,model_id,status,input_tokens,output_tokens) VALUES
		   ('seconds','s',1772359200,'m','completed',10,1),
		   ('unset','s',0,'m','completed',10,1)`,
	)
	res := parseRoot(t, dir, nil)
	if got, want := eventByKey(t, res.Events, "zcode:seconds").Timestamp, time.UnixMilli(1772359200000).UTC(); !got.Equal(want) {
		t.Errorf("seconds timestamp = %s, want %s", got, want)
	}
	if got := eventByKey(t, res.Events, "zcode:unset").Timestamp; !got.IsZero() {
		t.Errorf("unset timestamp = %s, want the zero time (unknown, never invented)", got)
	}
}

// ---------------------------------------------------------------------------
// discovery
// ---------------------------------------------------------------------------

func TestDiscoverFindsLedgerLayouts(t *testing.T) {
	t.Run("db directory", func(t *testing.T) {
		dir := t.TempDir()
		writeDB(t, dir, dbName, baseSchema)
		srcs, err := NewWithRoots(dir).Discover(context.Background())
		if err != nil || len(srcs) != 1 || srcs[0].Kind != kindSQLite {
			t.Fatalf("Discover = %+v, %v; want one sqlite source", srcs, err)
		}
		if want := filepath.Join(dir, dbName); srcs[0].Path != want {
			t.Errorf("source path = %q, want %q", srcs[0].Path, want)
		}
	})

	t.Run("zcode home", func(t *testing.T) {
		home := t.TempDir()
		dbDir := filepath.Join(home, cliDirName, dbDirName)
		if err := os.MkdirAll(dbDir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeDB(t, dbDir, dbName, baseSchema)
		srcs, err := NewWithRoots(home).Discover(context.Background())
		if err != nil || len(srcs) != 1 {
			t.Fatalf("Discover(%s) = %+v, %v; want one source", home, srcs, err)
		}
	})

	t.Run("cli directory", func(t *testing.T) {
		cli := t.TempDir()
		if err := os.MkdirAll(filepath.Join(cli, dbDirName), 0o755); err != nil {
			t.Fatal(err)
		}
		writeDB(t, filepath.Join(cli, dbDirName), dbName, baseSchema)
		srcs, err := NewWithRoots(cli).Discover(context.Background())
		if err != nil || len(srcs) != 1 {
			t.Fatalf("Discover(%s) = %+v, %v; want one source", cli, srcs, err)
		}
	})

	t.Run("database file", func(t *testing.T) {
		dir := t.TempDir()
		path := writeDB(t, dir, dbName, baseSchema)
		srcs, err := NewWithRoots(path).Discover(context.Background())
		if err != nil || len(srcs) != 1 || srcs[0].Path != path {
			t.Fatalf("Discover(%s) = %+v, %v; want the file itself", path, srcs, err)
		}
	})

	t.Run("multiple roots are deduplicated", func(t *testing.T) {
		dir := t.TempDir()
		writeDB(t, dir, dbName, baseSchema)
		srcs, err := NewWithRoots(dir, dir, filepath.Join(dir, ".")).Discover(context.Background())
		if err != nil || len(srcs) != 1 {
			t.Fatalf("Discover = %+v, %v; want one deduplicated source", srcs, err)
		}
	})

	t.Run("missing root", func(t *testing.T) {
		srcs, err := NewWithRoots(filepath.Join(t.TempDir(), "nope")).Discover(context.Background())
		if err != nil || len(srcs) != 0 {
			t.Fatalf("Discover(missing) = %+v, %v; want no sources and no error", srcs, err)
		}
	})

	t.Run("a stray database is not guessed", func(t *testing.T) {
		dir := t.TempDir()
		writeDB(t, dir, dbName, baseSchema)
		writeDB(t, dir, "unrelated.sqlite", baseSchema)
		srcs, err := NewWithRoots(dir).Discover(context.Background())
		if err != nil || len(srcs) != 1 || filepath.Base(srcs[0].Path) != dbName {
			t.Fatalf("Discover = %+v, %v; want only %s", srcs, err, dbName)
		}
	})
}

func TestRootsAreCleanedAndCopied(t *testing.T) {
	base := t.TempDir()
	p := NewWithRoots(base+string(filepath.Separator), base+"/./dup", "  ", base+"/dup")
	want := []string{filepath.Clean(base), filepath.Clean(filepath.Join(base, "dup"))}
	if got := p.Roots(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Roots() = %q, want %q", got, want)
	}
	got := p.Roots()
	got[0] = "/mutated"
	if p.Roots()[0] != want[0] {
		t.Fatal("Roots() must return a defensive copy")
	}
}

// ---------------------------------------------------------------------------
// privacy
// ---------------------------------------------------------------------------

func TestPrivacyNeverSurfacesSecretsOrConversationText(t *testing.T) {
	dir := t.TempDir()
	writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory,title) VALUES ('s','/w/secret','title `+sentinel+`')`,
		`INSERT INTO model_usage
		   (id,session_id,started_at,model_id,status,input_tokens,output_tokens,provider,prompt_text) VALUES
		   ('u1','s',1772359200000,'glm-5.3','completed',10,1,NULL,'prompt `+sentinel+` https://api.z.ai/api/anthropic sk-live-SECRETKEY')`,
	)
	const apiKey = "sk-live-SECRETKEY"
	const baseURL = "https://api.z.ai/api/anthropic"
	for _, name := range []string{"config.json", filepath.Join("v2", "config.json"), filepath.Join("v2", "credentials.json")} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"apiKey":"`+apiKey+`","baseUrl":"`+baseURL+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	srcs, err := NewWithRoots(dir).Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 1 || filepath.Base(srcs[0].Path) != dbName {
		t.Fatalf("Discover = %+v, want only %s (config/credentials are never sources)", srcs, dbName)
	}
	for _, src := range srcs {
		if strings.Contains(src.Path, "config.json") || strings.Contains(src.Path, "credentials.json") {
			t.Fatalf("discovered a sensitive file: %s", src.Path)
		}
	}

	res := parseRoot(t, dir, nil)
	blob, err := json.Marshal(struct {
		Events   []model.UsageEvent  `json:"events"`
		Sessions []model.SessionMeta `json:"sessions"`
	}{res.Events, res.Sessions})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{sentinel, apiKey, "SECRETKEY", baseURL, "api.z.ai", "credentials"} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("parsed output leaked %q:\n%s", secret, blob)
		}
	}
	e := eventByKey(t, res.Events, "zcode:u1")
	if e.BaseURL != "" || e.Provider != "" {
		t.Errorf("BaseURL/Provider = %q/%q, want empty (ZCode records neither here)", e.BaseURL, e.Provider)
	}
}

// ---------------------------------------------------------------------------
// read-only access
// ---------------------------------------------------------------------------

func TestOpenReadOnlyEnablesQueryOnlyAndRejectsWrites(t *testing.T) {
	dir := t.TempDir()
	path := writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`)
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatalf("openReadOnly: %v", err)
	}
	defer db.Close()
	var qo int
	if err := db.QueryRow("PRAGMA query_only").Scan(&qo); err != nil {
		t.Fatalf("PRAGMA query_only: %v", err)
	}
	if qo != 1 {
		t.Errorf("PRAGMA query_only = %d, want 1 (connection-local query-only mode)", qo)
	}
	if _, err := db.Exec(`INSERT INTO session (id,directory) VALUES ('x','/x')`); err == nil {
		t.Error("a write through the read-only connection succeeded, want an error")
	}
}

func TestParseDoesNotTouchTheDatabaseOrItsJournal(t *testing.T) {
	dir := t.TempDir()
	path := writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`,
	)
	// Keep a writer connection in WAL mode open for the whole test: a clean
	// close would checkpoint the log and delete the sidecars, and the point here
	// is that a read-only reader neither creates nor rewrites them.
	keeper := openWALDB(t, path)
	defer keeper.Close()
	if _, err := keeper.Exec(`INSERT INTO model_usage (id,session_id,started_at,model_id,status,input_tokens,output_tokens) VALUES
	   ('u1','s',1772359200000,'m','completed',10,1)`); err != nil {
		t.Fatalf("seed the write-ahead log: %v", err)
	}
	before := map[string][]byte{}
	for _, suffix := range []string{"", "-wal"} {
		raw, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Fatalf("read %s%s: %v", path, suffix, err)
		}
		before[suffix] = raw
	}

	res := parseRoot(t, dir, nil)
	if got, want := keys(res.Events), []string{"zcode:u1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("parse through the write-ahead log = %v, want %v", got, want)
	}

	for suffix, want := range before {
		got, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Fatalf("read %s%s after parse: %v", path, suffix, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s%s changed during parse (%d -> %d bytes)", path, suffix, len(want), len(got))
		}
	}
	if _, err := os.Stat(path + "-journal"); !os.IsNotExist(err) {
		t.Errorf("parse created a rollback journal: err = %v", err)
	}
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Errorf("journal_mode = %q after parse, want wal (the parser must not change it)", mode)
	}
}

// openWALDB opens a read-write connection and puts the database in WAL mode
// without closing it, so the -wal/-shm sidecars stay on disk for the test.
func openWALDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqlitedsn.URI(path, "mode=rw"))
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&mode); err != nil {
		db.Close()
		t.Fatalf("PRAGMA journal_mode=WAL: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		db.Close()
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
	return db
}

// ---------------------------------------------------------------------------
// cursor: incremental, idempotent, watermark
// ---------------------------------------------------------------------------

func TestIncrementalWatermarkEmitsOnlyNewRows(t *testing.T) {
	dir := t.TempDir()
	path := writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`,
		`INSERT INTO model_usage (id,session_id,started_at,model_id,status,input_tokens,output_tokens) VALUES
		   ('u1','s',1772359200000,'m','completed',10,1),
		   ('u2','s',1772359201000,'m','completed',20,2)`,
	)
	first := parseRoot(t, dir, nil)
	if got, want := keys(first.Events), []string{"zcode:u1", "zcode:u2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cold DedupKeys = %v, want %v", got, want)
	}

	execDB(t, path, `INSERT INTO model_usage (id,session_id,started_at,model_id,status,input_tokens,output_tokens) VALUES
	   ('u3','s',1772359202000,'m','completed',30,3)`)
	second := parseRoot(t, dir, first.Cursors)
	if got, want := keys(second.Events), []string{"zcode:u3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("incremental DedupKeys = %v, want only the new row %v", got, want)
	}
	if got := eventByKey(t, second.Events, "zcode:u3"); got.Tokens.Input != 30 || got.Tokens.Output != 3 {
		t.Errorf("u3 Tokens = %+v, want input 30 output 3", got.Tokens)
	}

	third := parseRoot(t, dir, second.Cursors)
	if len(third.Events) != 0 {
		t.Fatalf("unchanged database re-emitted %v", keys(third.Events))
	}
	if !reflect.DeepEqual(third.Cursors, second.Cursors) {
		t.Errorf("cursor moved on an unchanged database:\n got %+v\nwant %+v", third.Cursors, second.Cursors)
	}
}

func TestRowCompletedLaterIsCounted(t *testing.T) {
	dir := t.TempDir()
	path := writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`,
		`INSERT INTO model_usage (id,session_id,started_at,model_id,status,input_tokens,output_tokens) VALUES
		   ('u1','s',1772359200000,'m','running',10,1)`,
	)
	first := parseRoot(t, dir, nil)
	if len(first.Events) != 0 {
		t.Fatalf("a running row produced events: %v", keys(first.Events))
	}

	execDB(t, path, `UPDATE model_usage SET status='completed' WHERE id='u1'`)
	second := parseRoot(t, dir, first.Cursors)
	if got, want := keys(second.Events), []string{"zcode:u1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DedupKeys after completion = %v, want %v (an in-place status update must be seen)", got, want)
	}

	third := parseRoot(t, dir, second.Cursors)
	if len(third.Events) != 0 {
		t.Fatalf("an already counted row was emitted again: %v", keys(third.Events))
	}
}

func TestIdempotentFromZeroCursor(t *testing.T) {
	dir := t.TempDir()
	writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`,
		`INSERT INTO model_usage (id,session_id,started_at,model_id,status,input_tokens,output_tokens) VALUES
		   ('u1','s',1772359200000,'m','completed',10,1),
		   ('u2','s',1772359201000,'m','completed',20,2),
		   ('u3','s',1772359202000,'m','running',30,3)`,
	)
	first := parseRoot(t, dir, nil)
	second := parseRoot(t, dir, nil)
	if !reflect.DeepEqual(keys(first.Events), keys(second.Events)) {
		t.Fatalf("re-parsing from zero changed the key set: %v vs %v", keys(first.Events), keys(second.Events))
	}
	seen := map[string]bool{}
	for _, k := range first.Events {
		if seen[k.DedupKey] {
			t.Fatalf("duplicate DedupKey inside one parse: %q", k.DedupKey)
		}
		seen[k.DedupKey] = true
	}
}

func TestRowidResetRescansWithoutUnstableKeys(t *testing.T) {
	dir := t.TempDir()
	path := writeDB(t, dir, dbName, baseSchema,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`,
		`INSERT INTO model_usage (id,session_id,started_at,model_id,status,input_tokens,output_tokens) VALUES
		   ('u1','s',1772359200000,'m','completed',10,1),
		   ('u2','s',1772359201000,'m','completed',20,2)`,
	)
	first := parseRoot(t, dir, nil)

	// Rebuild the table: rowids restart, so a stale watermark must not hide rows.
	execDB(t, path,
		`DROP TABLE model_usage`,
		`CREATE TABLE model_usage (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, started_at INTEGER NOT NULL,
		   model_id TEXT NOT NULL, status TEXT NOT NULL, input_tokens INTEGER NOT NULL DEFAULT 0,
		   output_tokens INTEGER NOT NULL DEFAULT 0, cache_read_input_tokens INTEGER NOT NULL DEFAULT 0,
		   cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0, provider TEXT, prompt_text TEXT)`,
		`INSERT INTO model_usage (id,session_id,started_at,model_id,status,input_tokens,output_tokens) VALUES
		   ('u3','s',1772359202000,'m','completed',30,3)`,
	)
	second := parseRoot(t, dir, first.Cursors)
	if len(second.Events) == 0 {
		t.Fatal("a rebuilt table produced no events; the watermark must reset when rowids go backwards")
	}
	if got := eventByKey(t, second.Events, "zcode:u3"); got.Tokens.Input != 30 {
		t.Errorf("u3 Tokens = %+v, want input 30", got.Tokens)
	}
	seen := map[string]bool{}
	for _, e := range second.Events {
		if seen[e.DedupKey] {
			t.Fatalf("duplicate DedupKey %q inside one parse", e.DedupKey)
		}
		seen[e.DedupKey] = true
	}
	// The rebuilt row keeps its id-derived key, so a rescan can never double count.
	for _, e := range second.Events {
		if !strings.HasPrefix(e.DedupKey, "zcode:") {
			t.Errorf("unstable DedupKey %q", e.DedupKey)
		}
	}
}

func TestWithoutRowidTableStillParses(t *testing.T) {
	dir := t.TempDir()
	path := writeDB(t, dir, dbName, `CREATE TABLE model_usage (
	   id TEXT PRIMARY KEY, session_id TEXT NOT NULL, started_at INTEGER NOT NULL,
	   model_id TEXT NOT NULL, status TEXT NOT NULL, input_tokens INTEGER NOT NULL DEFAULT 0,
	   output_tokens INTEGER NOT NULL DEFAULT 0) WITHOUT ROWID;
	   CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL)`,
		`INSERT INTO session (id,directory) VALUES ('s','/w')`,
		`INSERT INTO model_usage (id,session_id,started_at,model_id,status,input_tokens,output_tokens) VALUES
		   ('u1','s',1772359200000,'m','completed',10,1)`,
	)
	first := parseRoot(t, dir, nil)
	if got, want := keys(first.Events), []string{"zcode:u1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DedupKeys = %v, want %v", got, want)
	}
	execDB(t, path, `INSERT INTO model_usage (id,session_id,started_at,model_id,status,input_tokens,output_tokens) VALUES
	   ('u2','s',1772359201000,'m','completed',20,2)`)
	second := parseRoot(t, dir, first.Cursors)
	if _, ok := func() (model.UsageEvent, bool) {
		for _, e := range second.Events {
			if e.DedupKey == "zcode:u2" {
				return e, true
			}
		}
		return model.UsageEvent{}, false
	}(); !ok {
		t.Fatalf("the row inserted after the first parse is missing: %v", keys(second.Events))
	}
	seen := map[string]bool{}
	for _, e := range second.Events {
		if seen[e.DedupKey] {
			t.Fatalf("duplicate DedupKey %q in a single parse", e.DedupKey)
		}
		seen[e.DedupKey] = true
	}
}

// ---------------------------------------------------------------------------
// roots and provenance
// ---------------------------------------------------------------------------

func TestRootProvenance(t *testing.T) {
	t.Run("builtin default", func(t *testing.T) {
		t.Setenv("MYTOKEN_ZCODE_DIRS", "")
		roots, err := New().RootProvenance()
		if err != nil {
			t.Fatalf("RootProvenance: %v", err)
		}
		if home := harness.Home(); home != "" {
			want := []harness.Root{{Path: filepath.Join(home, zcodeDirName, cliDirName, dbDirName), Origin: harness.OriginBuiltin}}
			if !reflect.DeepEqual(roots, want) {
				t.Fatalf("RootProvenance = %+v, want %+v", roots, want)
			}
		}
	})

	t.Run("explicit multi-root override", func(t *testing.T) {
		sep := string(filepath.ListSeparator)
		a, b := t.TempDir(), t.TempDir()
		t.Setenv("MYTOKEN_ZCODE_DIRS", a+sep+b)
		roots, err := New().RootProvenance()
		if err != nil {
			t.Fatalf("RootProvenance: %v", err)
		}
		want := harness.EnvRoots("MYTOKEN_ZCODE_DIRS", a, b)
		if !reflect.DeepEqual(roots, want) {
			t.Fatalf("RootProvenance = %+v, want %+v", roots, want)
		}
		if got := New().Roots(); !reflect.DeepEqual(got, []string{a, b}) {
			t.Fatalf("Roots() = %v, want the override roots %v", got, []string{a, b})
		}
		if _, err := os.Stat(filepath.Join(a, dbName)); err == nil {
			t.Fatal("provenance must not touch the filesystem")
		}
	})

	t.Run("malformed override is a hard error", func(t *testing.T) {
		bad := string(filepath.ListSeparator)
		t.Setenv("MYTOKEN_ZCODE_DIRS", bad)
		roots, err := New().RootProvenance()
		if err == nil || len(roots) != 0 {
			t.Fatalf("RootProvenance = %+v, %v; want no roots and an error", roots, err)
		}
		var ele *harness.EnvListError
		if !errors.As(err, &ele) || ele.Env != "MYTOKEN_ZCODE_DIRS" {
			t.Fatalf("error = %v (%T), want *harness.EnvListError for MYTOKEN_ZCODE_DIRS", err, err)
		}
		if got := New().Roots(); len(got) != 0 {
			t.Fatalf("Roots() = %v after a malformed override, want none (never fall back to defaults)", got)
		}
	})

	t.Run("blank override falls back to builtin", func(t *testing.T) {
		t.Setenv("MYTOKEN_ZCODE_DIRS", "   ")
		roots, err := New().RootProvenance()
		if err != nil {
			t.Fatalf("RootProvenance: %v", err)
		}
		for _, r := range roots {
			if r.Origin != harness.OriginBuiltin || r.Env != "" {
				t.Fatalf("blank override produced %+v, want builtin roots", roots)
			}
		}
	})
}

func TestExplicitRootsNeverFallBackToDefaults(t *testing.T) {
	t.Setenv("MYTOKEN_ZCODE_DIRS", t.TempDir())

	empty := NewWithRoots()
	if got := empty.Roots(); len(got) != 0 {
		t.Fatalf("NewWithRoots() roots = %v, want none", got)
	}
	roots, err := empty.RootProvenance()
	if err != nil || len(roots) != 0 {
		t.Fatalf("empty RootProvenance = %+v, %v; want none", roots, err)
	}

	a, b := t.TempDir(), t.TempDir()
	p := NewWithRoots(a, b)
	got, err := p.RootProvenance()
	if err != nil {
		t.Fatalf("RootProvenance: %v", err)
	}
	if want := harness.ExplicitRoots(a, b); !reflect.DeepEqual(got, want) {
		t.Fatalf("RootProvenance = %+v, want %+v", got, want)
	}

	rooted, ok := New().WithRoots(a).(*Parser)
	if !ok {
		t.Fatalf("WithRoots returned %T, want *Parser", New().WithRoots(a))
	}
	if rooted == New() {
		t.Fatal("WithRoots must return a fresh parser")
	}
	if got := rooted.Roots(); !reflect.DeepEqual(got, []string{a}) {
		t.Fatalf("WithRoots roots = %v, want %v", got, []string{a})
	}
	if prov, err := rooted.RootProvenance(); err != nil || !reflect.DeepEqual(prov, harness.ExplicitRoots(a)) {
		t.Fatalf("WithRoots provenance = %+v, %v; want explicit %v", prov, err, []string{a})
	}
}

func TestHarnessIdentity(t *testing.T) {
	p := New()
	if p.Harness() != model.ZCode {
		t.Errorf("Harness() = %q, want %q", p.Harness(), model.ZCode)
	}
	if string(model.ZCode) != "zcode" {
		t.Errorf("model.ZCode = %q, want zcode (the id must be stable)", model.ZCode)
	}
	if got := model.ZCode.DisplayName(); got != "ZCode" {
		t.Errorf("DisplayName() = %q, want ZCode", got)
	}
	if !model.ZCode.Known() {
		t.Error("model.ZCode.Known() = false, want true")
	}
}
