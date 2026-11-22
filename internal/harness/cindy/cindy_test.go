package cindy

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nerv-base/mytoken/internal/harness"
)

func TestRootProvenanceEnv(t *testing.T) {
	t.Setenv("CINDY_HOST_DATA_DIR", "")
	t.Setenv("MYTOKEN_CINDY_DIRS", strings.Join([]string{"/tmp/cindy-a", "/tmp/cindy-b"}, string(os.PathListSeparator)))
	roots, err := New().RootProvenance()
	if err != nil {
		t.Fatal(err)
	}
	wantA, wantB := filepath.Clean("/tmp/cindy-a"), filepath.Clean("/tmp/cindy-b")
	if len(roots) != 2 || roots[0].Path != wantA || roots[1].Path != wantB {
		t.Fatalf("roots = %+v, want %q and %q", roots, wantA, wantB)
	}
	if roots[0].Origin != "env" || roots[0].Env != "MYTOKEN_CINDY_DIRS" {
		t.Fatalf("origin = %+v", roots[0])
	}
}

func TestRootProvenanceHostDataDir(t *testing.T) {
	t.Setenv("MYTOKEN_CINDY_DIRS", "")
	t.Setenv("CINDY_HOST_DATA_DIR", "/tmp/cindy-host")
	roots, err := New().RootProvenance()
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 || roots[0].Path != filepath.Clean("/tmp/cindy-host") || roots[0].Env != "CINDY_HOST_DATA_DIR" {
		t.Fatalf("roots = %+v", roots)
	}
}

func TestNormalizeModel(t *testing.T) {
	cases := map[string]string{
		"gpt-6.1-sol#billing=subscription": "gpt-6.1-sol",
		"gpt-6.1-sol#billing=api":          "gpt-6.1-sol",
		"  claude-sonnet-4.5  ":            "claude-sonnet-4.5",
		"":                                 "",
	}
	for in, want := range cases {
		if got := normalizeModel(in); got != want {
			t.Errorf("normalizeModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTitleText(t *testing.T) {
	cases := map[string]string{
		"Fix the login bug": "Fix the login bug",
		"<system-reminder>ignore this</system-reminder>": "",
		"/help": "",
		`[{"type":"text","text":"Explore tests"}]`: "Explore tests",
		`"Quoted title"`: "Quoted title",
	}
	for in, want := range cases {
		if got := titleText(in); got != want {
			t.Errorf("titleText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseDatabase(t *testing.T) {
	dir := t.TempDir()
	db := testDB(t, filepath.Join(dir, "cindy-test.db"))
	mustExec(t, db, `CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT, working_dir TEXT, project_dir TEXT, model TEXT, provider_id TEXT, parent_session_id TEXT, created_at INTEGER, updated_at INTEGER, last_activity_at INTEGER)`)
	mustExec(t, db, `CREATE TABLE messages (id TEXT PRIMARY KEY, session_id TEXT, role TEXT, content TEXT, created_at INTEGER, agent_meta TEXT)`)
	mustExec(t, db, `CREATE TABLE daily_model_usage (day TEXT, agent_kind TEXT, model TEXT, input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER, cache_create_tokens INTEGER, updated_at INTEGER)`)
	mustExec(t, db, `INSERT INTO sessions (id,title,working_dir,model,provider_id,created_at,updated_at,last_activity_at) VALUES ('s1','', '/work/app','gpt-6.1-sol','openai',1794292444986,1794293349257,1794293349257)`)
	mustExec(t, db, `INSERT INTO messages (id,session_id,role,content,created_at,agent_meta) VALUES ('m0','s1','user','Fix the bug',1794292448012,'')`)
	mustExec(t, db, `INSERT INTO messages (id,session_id,role,content,created_at,agent_meta) VALUES ('m1','s1','assistant','Done',1794292471114,'{"turnUsageDetails":{"providerId":"openai","inputTokens":10,"outputTokens":2,"cacheReadTokens":30,"cacheCreateTokens":4,"model":"gpt-6.1-sol#billing=subscription"}}')`)

	p := NewWithRoots(dir)
	sources, err := p.Discover(context.Background())
	if err != nil || len(sources) != 1 {
		t.Fatalf("Discover: %v %v", sources, err)
	}
	batch, err := p.Parse(context.Background(), sources[0], harness.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Events) != 1 {
		t.Fatalf("events = %+v", batch.Events)
	}
	event := batch.Events[0]
	if event.Harness != "cindy" || event.DedupKey != "msg:s1:m1" || event.Model != "gpt-6.1-sol" || event.Provider != "openai" {
		t.Fatalf("event = %+v", event)
	}
	if event.Tokens.Input != 10 || event.Tokens.Output != 2 || event.Tokens.CacheRead != 30 || event.Tokens.CacheWrite != 4 {
		t.Fatalf("tokens = %+v", event.Tokens)
	}
	if event.CostUSD != nil {
		t.Fatalf("cost should be nil: %+v", event.CostUSD)
	}
	if got := event.Timestamp.UTC().Format(time.RFC3339Nano); got != "2026-11-10T06:34:31.114Z" {
		t.Fatalf("timestamp = %s", got)
	}
	if len(batch.Sessions) != 1 || batch.Sessions[0].Title != "Fix the bug" || batch.Sessions[0].Project != "/work/app" {
		t.Fatalf("sessions = %+v", batch.Sessions)
	}
}

func TestDailyResidual(t *testing.T) {
	dir := t.TempDir()
	db := testDB(t, filepath.Join(dir, "cindy-test.db"))
	mustExec(t, db, `CREATE TABLE daily_model_usage (day TEXT, agent_kind TEXT, model TEXT, input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER, cache_create_tokens INTEGER, updated_at INTEGER)`)
	mustExec(t, db, `INSERT INTO daily_model_usage (day,agent_kind,model,input_tokens,output_tokens,cache_read_tokens,cache_create_tokens,updated_at) VALUES ('2026-11-09','codex','gpt-6.1-sol#billing=subscription',500,50,700,80,1794225600000)`)

	p := NewWithRoots(dir)
	sources, err := p.Discover(context.Background())
	if err != nil || len(sources) != 1 {
		t.Fatalf("Discover: %v %v", sources, err)
	}
	batch, err := p.Parse(context.Background(), sources[0], harness.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Events) != 1 {
		t.Fatalf("events = %+v", batch.Events)
	}
	event := batch.Events[0]
	if event.SessionID != dailySessionID || event.Model != "gpt-6.1-sol" || event.DedupKey != "daily:2026-11-09:gpt-6.1-sol" {
		t.Fatalf("event = %+v", event)
	}
	if event.Tokens.Input != 500 || event.Tokens.Output != 50 || event.Tokens.CacheRead != 700 || event.Tokens.CacheWrite != 80 {
		t.Fatalf("tokens = %+v", event.Tokens)
	}
	if len(batch.Sessions) != 1 || batch.Sessions[0].SessionID != dailySessionID || batch.Sessions[0].Title != dailySessionName {
		t.Fatalf("sessions = %+v", batch.Sessions)
	}
}

func testDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustExec(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.Exec(query); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
