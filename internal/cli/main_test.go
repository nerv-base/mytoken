package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

// TestMain lets every CLI invocation run in its own process. Harness parsers
// capture their default roots at package init, so an in-process run would keep
// the roots of the machine this test runs on; a child process instead starts
// with the isolated env the test sets. The child runs exactly one command and
// exits with its status.
func TestMain(m *testing.M) {
	if os.Getenv("MYTOKEN_CLI_TEST_CHILD") == "1" {
		var args []string
		if raw := os.Getenv("MYTOKEN_CLI_TEST_ARGS"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				os.Stderr.WriteString("bad MYTOKEN_CLI_TEST_ARGS: " + err.Error() + "\n")
				os.Exit(3)
			}
		}
		os.Exit(Run(args))
	}
	os.Exit(m.Run())
}

// harnessEnvDirs is every directory-shaped harness override this build reads.
// Pointing all of them at fresh temp dirs keeps a scan away from the real
// machine logs; CLAUDE_CONFIG_DIR is set separately to a per-test config dir.
var harnessEnvDirs = []string{
	"CLAUDE_CONFIG_DIR",
	"CODEX_HOME",
	"DSH_HOME",
	"PI_HOME",
	"PI_CONFIG_DIR",
	"PI_CODING_AGENT_DIR",
	"GEMINI_CLI_HOME",
	"QWEN_DATA_DIR",
	"GROK_HOME",
	"MIMOCODE_HOME",
	"MAVIS_DATA_DIR",
	"MINIMAX_DATA_DIR",
	"MUX_ROOT",
	"CODEBURN_MUX_DIR",
	"CODEBURN_OPENCLAUDEDIR",
	"FACTORY_DIR",
	"CLINE_DATA_DIR",
	"CLINE_DIR",
	"CLINE_SESSION_DATA_DIR",
	"STEP_CODING_AGENT_SESSION_DIR",
	"STEP_CODING_AGENT_DIR",
	"CRUSH_GLOBAL_DATA",
	"GOOSE_PATH_ROOT",
	"KIMI_SHARE_DIR",
	"KIMI_CODE_HOME",
	"CODEBUDDY_HOME",
	"CODEBUFF_DATA_DIR",
	"HERMES_HOME",
	"AMP_DATA_DIR",
	"KIRO_HOME",
	"OPENCODE_DATA_HOME",
	"MYTOKEN_AMP_DIRS",
	"MYTOKEN_ANTIGRAVITY_DIRS",
	"MYTOKEN_CLAUDEDESKTOP_DIRS",
	"MYTOKEN_CLAUDE_DIRS",
	"MYTOKEN_CLINE_DIRS",
	"MYTOKEN_CODEBUFF_DIRS",
	"MYTOKEN_CRUSH_DIRS",
	"MYTOKEN_DROID_DIRS",
	"MYTOKEN_FORGE_DIRS",
	"MYTOKEN_GEMINI_DIRS",
	"MYTOKEN_GOOSE_DIRS",
	"MYTOKEN_GROK_DIRS",
	"MYTOKEN_KILO_DIRS",
	"MYTOKEN_KIMI_DIRS",
	"MYTOKEN_KIRO_DIRS",
	"MYTOKEN_MIMO_CODE_DIRS",
	"MYTOKEN_MINIMAX_DIRS",
	"MYTOKEN_MUX_DIRS",
	"MYTOKEN_OPENCLAUDE_DIRS",
	"MYTOKEN_OPENCLAW_DIRS",
	"MYTOKEN_OPENCODE_DIRS",
	"MYTOKEN_PI_DIRS",
	"MYTOKEN_QWEN_DIRS",
	"MYTOKEN_ROO_DIRS",
	"MYTOKEN_STEP_CODE_DIRS",
	"MYTOKEN_ZCODE_DIRS",
	"MYTOKEN_ZED_DIRS",
}

// testHome is one isolated machine: a MYTOKEN_HOME, a HOME and a CLAUDE_CONFIG_DIR.
type testHome struct {
	root         string
	home         string
	claudeConfig string
}

// newTestHome points every path this process could discover at temp dirs and
// makes MYTOKEN_HOME the index under test.
func newTestHome(t *testing.T) *testHome {
	t.Helper()
	root := t.TempDir()
	h := &testHome{
		root:         root,
		home:         filepath.Join(root, "mytoken-home"),
		claudeConfig: filepath.Join(root, "claude-config"),
	}
	mustMkdir(t, h.home)
	mustMkdir(t, filepath.Join(h.claudeConfig, "projects"))

	envRoot := filepath.Join(root, "env")
	for _, key := range append([]string{
		"HOME", "APPDATA", "LOCALAPPDATA",
		"XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME",
	}, harnessEnvDirs...) {
		dir := filepath.Join(envRoot, key)
		mustMkdir(t, dir)
		t.Setenv(key, dir)
	}
	// These name a file rather than a directory.
	for _, key := range []string{"MIMOCODE_DB"} {
		dir := filepath.Join(envRoot, key+"-dir")
		mustMkdir(t, dir)
		t.Setenv(key, filepath.Join(dir, "data.db"))
	}
	t.Setenv("CLAUDE_CONFIG_DIR", h.claudeConfig)
	t.Setenv("MYTOKEN_HOME", h.home)
	// A non-UTC process zone proves --timezone really drives the day buckets.
	t.Setenv("TZ", "Asia/Shanghai")
	return h
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

// runCLI runs one command in a fresh process and returns its streams and exit
// status. The child inherits the whole isolated env set by the test.
func runCLI(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), exe)
	cmd.Env = append(os.Environ(),
		"MYTOKEN_CLI_TEST_CHILD=1",
		"MYTOKEN_CLI_TEST_ARGS="+string(argsJSON),
	)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err = cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		code = 0
	case errors.As(err, &exit):
		code = exit.ExitCode()
	default:
		t.Fatalf("run %v: %v (stderr: %s)", args, err, errOut.String())
	}
	return out.String(), errOut.String(), code
}

// fixture is a populated index: two harnesses, three UTC days, priced and
// unpriced facts, and one 1-hour cache write.
type fixture struct {
	*testHome
	dbPath       string
	now          time.Time
	day0         time.Time
	today        time.Time
	yesterday    time.Time
	twoDaysAgo   time.Time
	todaySession string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	h := newTestHome(t)
	now := time.Now().UTC()
	day0 := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	today := day0.Add(now.Sub(day0) / 2)
	yesterday := day0.AddDate(0, 0, -1)
	twoDaysAgo := day0.AddDate(0, 0, -2)
	f := &fixture{
		testHome:     h,
		dbPath:       filepath.Join(h.home, "mytoken.db"),
		now:          now,
		day0:         day0,
		today:        today,
		yesterday:    yesterday,
		twoDaysAgo:   twoDaysAgo,
		todaySession: "sess-today-claude",
	}

	claudeEvents := []model.UsageEvent{
		{
			Harness: model.ClaudeCode, DedupKey: "claude:today", SessionID: f.todaySession,
			ProjectPath: "/tmp/proj", Timestamp: today,
			Model: "claude-sonnet-4-5", Provider: "anthropic",
			Tokens: model.Tokens{Input: 100, Output: 20, CacheRead: 300, CacheWrite: 40, CacheWrite1h: 40},
		},
		{
			Harness: model.ClaudeCode, DedupKey: "claude:yesterday", SessionID: "sess-yesterday-claude",
			ProjectPath: "/tmp/proj", Timestamp: yesterday.Add(23 * time.Hour),
			Model: "claude-sonnet-4-5", Provider: "anthropic",
			Tokens: model.Tokens{Input: 10, Output: 5, CacheRead: 100},
		},
		{
			Harness: model.ClaudeCode, DedupKey: "claude:older", SessionID: "sess-older-claude",
			ProjectPath: "/tmp/proj", Timestamp: twoDaysAgo.Add(time.Hour),
			Model: "claude-sonnet-4-5", Provider: "anthropic",
			Tokens: model.Tokens{Input: 7, Output: 3},
		},
	}
	claudeResolutions := []store.Resolution{
		{Provider: "anthropic", Attrib: model.AttribLog, Cost: 0.50, Priced: true},
		{Provider: "anthropic", Attrib: model.AttribLog, Cost: 0.25, Priced: true},
		{Provider: "anthropic", Attrib: model.AttribLog, Cost: 0.10, Priced: true},
	}
	codexEvents := []model.UsageEvent{
		{
			Harness: model.Codex, DedupKey: "codex:today", SessionID: "sess-today-codex",
			ProjectPath: "/tmp/proj", Timestamp: today,
			Model: "gpt-5-codex", Provider: "openai",
			Tokens: model.Tokens{Input: 50, Output: 10},
		},
		{
			Harness: model.Codex, DedupKey: "codex:yesterday", SessionID: "sess-yesterday-codex",
			ProjectPath: "/tmp/proj", Timestamp: yesterday.Add(23*time.Hour + 30*time.Minute),
			Model: "gpt-5-codex", Provider: "openai",
			Tokens: model.Tokens{Input: 20, Output: 8},
		},
	}
	codexResolutions := []store.Resolution{
		{Provider: "openai", Attrib: model.AttribLog},
		{Provider: "openai", Attrib: model.AttribLog},
	}

	st, err := store.Open(f.dbPath)
	if err != nil {
		t.Fatalf("store.Open(%s): %v", f.dbPath, err)
	}
	ctx := t.Context()
	if err := st.Commit(ctx, model.ClaudeCode, "fixture", harness.Batch{Events: claudeEvents}, claudeResolutions); err != nil {
		st.Close()
		t.Fatalf("commit claude-code: %v", err)
	}
	if err := st.Commit(ctx, model.Codex, "fixture", harness.Batch{Events: codexEvents}, codexResolutions); err != nil {
		st.Close()
		t.Fatalf("commit codex: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close fixture store: %v", err)
	}
	return f
}

func mustContain(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("output does not contain %q:\n%s", want, got)
		}
	}
}

func jsonInto(t *testing.T, out string, dst any) {
	t.Helper()
	if err := json.Unmarshal([]byte(out), dst); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, out)
	}
}
