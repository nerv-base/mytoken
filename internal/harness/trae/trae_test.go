package trae

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
	"github.com/zzstar101/mytoken/internal/model"
)

// writeDB lays down a file at dir/<name> holding body.
func writeDB(t *testing.T, dir, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDiscoverRootShapes(t *testing.T) {
	// Every root shape Trae's own layout can hand over must find the same file.
	body := append([]byte(sqliteMagic), make([]byte, cipherPageSize)...)
	cases := map[string]func(dir string) (root, want string){
		"ai-agent directory": func(dir string) (string, string) {
			return filepath.Join(dir, agentDir), writeDB(t, filepath.Join(dir, agentDir), dbName, body)
		},
		"modular directory": func(dir string) (string, string) {
			return filepath.Join(dir, modularDir), writeDB(t, filepath.Join(dir, modularDir, agentDir), dbName, body)
		},
		"product data directory": func(dir string) (string, string) {
			product := filepath.Join(dir, "Trae CN")
			return product, writeDB(t, filepath.Join(product, modularDir, agentDir), dbName, body)
		},
		"database file": func(dir string) (string, string) {
			return writeDB(t, filepath.Join(dir, "somewhere"), dbName, body), ""
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			root, want := build(t.TempDir())
			if want == "" {
				want = root
			}
			srcs, err := NewWithRoots(root).Discover(context.Background())
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if len(srcs) != 1 || srcs[0].Path != want {
				t.Fatalf("sources = %+v, want %s", srcs, want)
			}
			if srcs[0].Kind != kindSQLite {
				t.Fatalf("kind = %q, want %q", srcs[0].Kind, kindSQLite)
			}
		})
	}
}

func TestDiscoverMissingRoot(t *testing.T) {
	srcs, err := NewWithRoots(filepath.Join(t.TempDir(), "nope")).Discover(context.Background())
	if err != nil {
		t.Fatalf("a missing root is not an error: %v", err)
	}
	if len(srcs) != 0 {
		t.Fatalf("sources = %+v, want none", srcs)
	}
}

func TestDiscoverDeduplicates(t *testing.T) {
	dir := t.TempDir()
	path := writeDB(t, filepath.Join(dir, agentDir), dbName, append([]byte(sqliteMagic), make([]byte, cipherPageSize)...))
	// Two roots, the second reached through the first: one source, not two.
	p := NewWithRoots(filepath.Join(dir, agentDir), filepath.Join(dir, agentDir, "..", agentDir))
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 1 || srcs[0].Path != path {
		t.Fatalf("sources = %+v, want one %s", srcs, path)
	}
}

func TestNewWithRootsDoesNotFallBack(t *testing.T) {
	if got := NewWithRoots().Roots(); len(got) != 0 {
		t.Fatalf("roots = %v, want none", got)
	}
}

func TestIsEncrypted(t *testing.T) {
	dir := t.TempDir()
	plain := writeDB(t, dir, "plain.db", append([]byte(sqliteMagic), make([]byte, cipherPageSize)...))
	enc := writeDB(t, dir, "enc.db", make([]byte, cipherPageSize))
	for path, want := range map[string]bool{plain: false, enc: true} {
		got, err := isEncrypted(path)
		if err != nil {
			t.Fatalf("isEncrypted(%s): %v", path, err)
		}
		if got != want {
			t.Fatalf("isEncrypted(%s) = %v, want %v", filepath.Base(path), got, want)
		}
	}
}

func TestEncryptedDatabaseNeedsKey(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, agentDir)
	writeDB(t, root, dbName, make([]byte, cipherPageSize))

	p := NewWithRoots(root)
	if reason := p.NotReady(); reason == "" {
		t.Fatal("an encrypted database without a key must report a readiness problem")
	}
	if _, err := p.Discover(context.Background()); !errors.Is(err, ErrKeyNeeded) {
		t.Fatalf("Discover = %v, want ErrKeyNeeded", err)
	}

	keyed, err := p.WithKey("x'" + "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff" + "'")
	if err != nil {
		t.Fatalf("WithKey: %v", err)
	}
	if reason := keyed.(*Parser).NotReady(); reason != "" {
		t.Fatalf("a parser with a key must be ready, got %q", reason)
	}
	if _, err := keyed.Discover(context.Background()); err != nil {
		t.Fatalf("Discover with a key: %v", err)
	}
}

func TestWithKey(t *testing.T) {
	p := NewWithRoots("/a", "/b")
	tests := []struct {
		name  string
		key   string
		fails bool
		empty bool
	}{
		{name: "empty clears", key: "  ", empty: true},
		{name: "hex", key: "3605f6691095a993f03d5009c918352ef5be31ae31e8f000212b81ff058da773"},
		{name: "sqlcipher literal with salt", key: "x'3605f6691095a993f03d5009c918352ef5be31ae31e8f000212b81ff058da773fa8183e03e3f2442643c87a4e8d06419'"},
		{name: "not hex", key: "nope", fails: true},
		{name: "too short", key: "abcd", fails: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.WithKey(tc.key)
			if tc.fails {
				if err == nil {
					t.Fatal("want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("WithKey: %v", err)
			}
			next := got.(*Parser)
			if got := len(next.key) == 0; got != tc.empty {
				t.Fatalf("key is empty = %v, want %v", got, tc.empty)
			}
			if len(next.Roots()) != 2 {
				t.Fatalf("roots = %v, want the receiver's two", next.Roots())
			}
			if len(p.key) != 0 {
				t.Fatal("WithKey must not mutate the receiver")
			}
		})
	}
}

func TestKeyFromEnv(t *testing.T) {
	t.Setenv(envKey, "x'3605f6691095a993f03d5009c918352ef5be31ae31e8f000212b81ff058da773'")
	p := New()
	if reason := p.NotReady(); reason != "" {
		t.Fatalf("a key from the environment must make the parser ready, got %q", reason)
	}
	t.Setenv(envKey, "not-a-key")
	if reason := New().NotReady(); reason == "" {
		t.Fatal("an invalid key in the environment must be reported")
	}
}

func TestTurnTime(t *testing.T) {
	want := time.UnixMilli(1792114011935)
	if got := turnTime(1792114011935, 0); !got.Equal(want) {
		t.Fatalf("milliseconds = %v, want %v", got, want)
	}
	if got := turnTime(0, 1792114011); !got.Equal(time.Unix(1792114011, 0).UTC()) {
		t.Fatalf("seconds = %v, want %v", got, time.Unix(1792114011, 0).UTC())
	}
	if got := turnTime(0, 0); !got.IsZero() {
		t.Fatalf("no timestamp = %v, want the zero time rather than 1970", got)
	}
}

// materialize builds the conformance fixture database in a fresh directory.
func materialize(t *testing.T) string {
	t.Helper()
	return harnesstest.Materialize(t, conformanceCase())
}

// TestParseTokenMath pins the inclusive-token arithmetic on the fixture turn
// whose numbers come from a real Trae turn: prompt_tokens already counts the
// cache reads, and completion_tokens already counts the reasoning tokens, so the
// reader must subtract them to keep MyToken's exclusive columns honest.
func TestParseTokenMath(t *testing.T) {
	root := materialize(t)
	snap, _ := harnesstest.Parse(t, conformanceCase(), root, nil)
	byKey := map[string]model.UsageEvent{}
	for _, ev := range snap.Events {
		byKey[ev.DedupKey] = ev
	}
	got, ok := byKey["trae:turn-1"]
	if !ok {
		t.Fatalf("turn-1 missing from %v", keys(byKey))
	}
	want := model.Tokens{Input: 4911, Output: 7091, CacheRead: 118784, CacheWrite: 0, Reasoning: 65}
	if got.Tokens != want {
		t.Fatalf("tokens = %+v, want %+v", got.Tokens, want)
	}
	if got.Model != "deepseek-v4.1-flash" {
		t.Fatalf("model = %q, want the billed config_name", got.Model)
	}
	if got.SessionID != "sess-alpha" {
		t.Fatalf("session = %q, want sess-alpha", got.SessionID)
	}
	if got.ProjectPath != "/work/alpha" {
		t.Fatalf("project = %q, want /work/alpha", got.ProjectPath)
	}
	if got.Timestamp.IsZero() {
		t.Fatal("timestamp must come from the turn context")
	}
}

func keys(m map[string]model.UsageEvent) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestParseUnchangedSourceKeepsTheCursor is the property the scanner relies on:
// a second scan of the same bytes must not re-emit anything.
func TestParseUnchangedSourceKeepsTheCursor(t *testing.T) {
	root := materialize(t)
	p := NewWithRoots(root)
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	first, err := p.Parse(context.Background(), srcs[0], harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(first.Events) == 0 {
		t.Fatal("the fixture must yield events")
	}
	second, err := p.Parse(context.Background(), srcs[0], first.Next)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(second.Events) != 0 || len(second.Sessions) != 0 {
		t.Fatalf("an unchanged source re-emitted %d events and %d sessions", len(second.Events), len(second.Sessions))
	}
	if second.Next != first.Next {
		t.Fatalf("cursor moved: %+v then %+v", first.Next, second.Next)
	}
}

func TestSourceSessions(t *testing.T) {
	root := materialize(t)
	p := NewWithRoots(root)
	srcs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	got, err := p.SourceSessions(context.Background(), srcs[0])
	if err != nil {
		t.Fatalf("SourceSessions: %v", err)
	}
	want := []string{"sess-alpha", "sess-beta"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("sessions = %v, want %v", got, want)
	}
}

func TestHarnessAndVersion(t *testing.T) {
	p := NewWithRoots()
	if p.Harness() != model.Trae {
		t.Fatalf("harness = %q, want %q", p.Harness(), model.Trae)
	}
	if p.ParserVersion() < 1 {
		t.Fatalf("parser version = %d", p.ParserVersion())
	}
	if _, ok := harness.Get(model.Trae); !ok {
		t.Fatal("the parser must register itself")
	}
}
