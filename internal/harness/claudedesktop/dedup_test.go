package claudedesktop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
)

// A Cowork session stores the same requests twice: the usage ledger beside the
// workspace and the Claude Code transcript inside it. When the metadata links
// the two, the transcript is authoritative and the ledger's turn aggregates are
// dropped. Code-tab turns are the Claude Code CLI's own sessions: their
// transcripts live under the claude-code roots and claude-code counts them.

const dedupSession = "cd-22222222-2222-2222-2222-222222222222"

var dedupBase = time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)

func TestLinkedTranscriptIsAuthoritative(t *testing.T) {
	root, transcript := dedupWorkspace(t, dedupSession, 30)
	if got := parseLedgerKeys(t, root); len(got) != 0 {
		t.Fatalf("linked ledger events = %v, want none (the transcript is authoritative)", got)
	}
	want := []string{transcriptKey("w"), transcriptKey("x"), transcriptKey("y"), transcriptKey("z")}
	if got := parseTranscriptKeys(t, root, transcript); !slices.Equal(got, want) {
		t.Fatalf("linked transcript events = %v, want %v", got, want)
	}
}

// Without the workspace metadata link the two files cannot be related, so the
// ledger rows stay and the transcript is counted too.
func TestUnlinkedLedgerRowsAreKept(t *testing.T) {
	root, _ := dedupWorkspace(t, "cd-33333333-3333-3333-3333-333333333333", 30)
	if got := parseLedgerKeys(t, root); len(got) != 3 {
		t.Fatalf("unlinked ledger events = %v, want 3", got)
	}
}

// A per-request ledger row is the billing record for exactly one transcript
// request, so the transcript copy is dropped rather than counted twice.
func TestPerRequestLedgerRowCoversTranscriptRequest(t *testing.T) {
	root, transcript := dedupWorkspace(t, dedupSession, 30)
	// Rewrite the ledger as one per-request row per transcript request.
	lines := []string{
		dedupLedgerAt(dedupBase.Add(time.Second), "request", 100, 30, 10, 5),
		dedupLedgerAt(dedupBase.Add(2*time.Second), "request", 200, 20, 0, 0),
		dedupLedgerAt(dedupBase.Add(3*time.Second), "request", 300, 30, 0, 0),
		dedupLedgerAt(dedupBase.Add(time.Minute), "request", 400, 40, 0, 0),
	}
	writeFixtureFile(t, filepath.Join(root, "3f9c1d2e", "00000000", "usage-ledger", "2026-10-03.ndjson"), strings.Join(lines, "\n")+"\n")
	if got := parseLedgerKeys(t, root); len(got) != 4 {
		t.Fatalf("per-request ledger events = %v, want 4", got)
	}
	if got := parseTranscriptKeys(t, root, transcript); len(got) != 0 {
		t.Fatalf("covered transcript events = %v, want none", got)
	}
}

// Two turn aggregates in the same second are different requests and must not
// collapse into one event.
func TestLedgerTurnsInSameSecondStayDistinct(t *testing.T) {
	root, _ := dedupWorkspace(t, "cd-33333333-3333-3333-3333-333333333333", 30)
	path := filepath.Join(root, "3f9c1d2e", "00000000", "usage-ledger", "2026-10-03.ndjson")
	lines := []string{
		dedupLedgerAt(dedupBase, "turn", 10, 5, 0, 0),
		dedupLedgerAt(dedupBase, "turn", 20, 6, 0, 0),
	}
	writeFixtureFile(t, path, strings.Join(lines, "\n")+"\n")
	keys := parseLedgerKeys(t, root)
	if len(keys) != 2 || keys[0] == keys[1] {
		t.Fatalf("same-second ledger keys = %v, want two distinct", keys)
	}
}

// A Code-tab ledger row is dropped only when its cliSessionId transcript exists
// under a claude-code root; otherwise it is the only record of that usage.
func TestCodeLedgerDroppedOnlyWithTranscript(t *testing.T) {
	cli := "cd-44444444-4444-4444-4444-444444444444"

	for _, app := range []string{"Claude-3p", "Claude"} {
		t.Run(app, func(t *testing.T) {
			cc := t.TempDir()
			transcript := filepath.Join(cc, "projects", "-Users-demo-proj", cli+".jsonl")
			if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
				t.Fatalf("mkdir claude-code project: %v", err)
			}
			writeFixtureFile(t, transcript, "")
			t.Setenv("CLAUDE_CONFIG_DIR", cc)

			appDir := filepath.Join(t.TempDir(), app)
			ws := filepath.Join(appDir, sessionsDir, "acct", "ws")
			for _, dir := range []string{ws, filepath.Join(ws, "usage-ledger"), filepath.Join(appDir, "claude-code-sessions", "acct-uuid", "ws-uuid")} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", dir, err)
				}
			}
			writeFixtureFile(t, filepath.Join(ws, "local_code.json"), fmt.Sprintf(`{"sessionId":"local_code","cliSessionId":%q}`, cli))
			writeFixtureFile(t, filepath.Join(appDir, "claude-code-sessions", "acct-uuid", "ws-uuid", "local_code.json"), fmt.Sprintf(`{"sessionId":"local_code","cliSessionId":%q}`, cli))
			line := dedupLedgerFor("code", "local_code", dedupBase, "turn", 10, 5, 0, 0)
			writeFixtureFile(t, filepath.Join(ws, "usage-ledger", "2026-10-03.ndjson"), line+"\n")

			if got := parseLedgerKeys(t, appDir); len(got) != 0 {
				t.Fatalf("code ledger with transcript = %v, want none", got)
			}
			if err := os.Remove(transcript); err != nil {
				t.Fatalf("remove transcript: %v", err)
			}
			if got := parseLedgerKeys(t, appDir); len(got) != 1 {
				t.Fatalf("code ledger without transcript = %v, want 1", got)
			}
		})
	}
}

// The 1h cache tier is a subset of cacheWrite, survives the same-key merge and
// never exceeds the write total.
func TestTranscriptCacheWrite1h(t *testing.T) {
	root, transcript := dedupWorkspace(t, dedupSession, 30)
	lines := []string{
		dedupAssistantLine(t, map[string]any{"input_tokens": 10, "output_tokens": 5, "cache_creation_input_tokens": 100, "cache_creation": map[string]any{"ephemeral_1h_input_tokens": 40, "ephemeral_5m_input_tokens": 60}}, 0, "c1"),
		// Same message, reported again with the larger 1h tier: max-merge must
		// keep it instead of resetting to the first line's value.
		dedupAssistantLine(t, map[string]any{"input_tokens": 10, "output_tokens": 5, "cache_creation_input_tokens": 100, "cache_creation": map[string]any{"ephemeral_1h_input_tokens": 70, "ephemeral_5m_input_tokens": 30}}, 0, "c1"),
	}
	writeFixtureFile(t, transcript, strings.Join(lines, "\n")+"\n")
	p := NewWithRoots(root)
	b, err := p.Parse(context.Background(), harness.Source{Path: transcript, Kind: kindJSONL}, harness.Cursor{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(b.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(b.Events))
	}
	tk := b.Events[0].Tokens
	if tk.CacheWrite != 100 || tk.CacheWrite1h != 70 {
		t.Fatalf("tokens = %+v, want CacheWrite 100 and CacheWrite1h 70", tk)
	}
}

func dedupWorkspace(t *testing.T, cliSessionID string, billedOutput int64) (root, transcript string) {
	t.Helper()
	root = t.TempDir()
	workspace := filepath.Join(root, "3f9c1d2e", "00000000")
	if err := os.MkdirAll(filepath.Join(workspace, "usage-ledger"), 0o755); err != nil {
		t.Fatalf("mkdir usage-ledger: %v", err)
	}
	writeFixtureFile(t, filepath.Join(workspace, "local_a.json"),
		fmt.Sprintf(`{"sessionId":"local_a","cliSessionId":%q}`, cliSessionID))

	transcript = filepath.Join(workspace, "short", ".claude", "projects", "proj", dedupSession+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatalf("mkdir transcript: %v", err)
	}
	lines := []string{
		fmt.Sprintf(`{"type":"user","timestamp":%q,"sessionId":%q,"cwd":"/Users/demo/proj","uuid":"u-0","message":{"role":"user","content":"dedup probe"}}`,
			dedupBase.Format(time.RFC3339), dedupSession),
		dedupAssistant(t, time.Second, "x", 100, 50, 10, 5, 20, 30),
		dedupAssistant(t, 2*time.Second, "y", 200, 20, 0, 0, 0, 20),
		dedupAssistant(t, 3*time.Second, "z", 300, 30, 0, 0, 0, 30),
		dedupAssistant(t, time.Minute, "w", 400, 40, 0, 0, 0, 40),
	}
	writeFixtureFile(t, transcript, strings.Join(lines, "\n")+"\n")

	ledger := []string{
		dedupLedgerAt(dedupBase.Add(1400*time.Millisecond), "turn", 100, billedOutput, 10, 5),
		dedupLedgerAt(dedupBase.Add(3*time.Second), "turn", 300, 31, 0, 0),
		dedupLedgerAt(dedupBase.Add(30*time.Second), "turn", 400, 40, 0, 0),
	}
	writeFixtureFile(t, filepath.Join(workspace, "usage-ledger", "2026-10-03.ndjson"), strings.Join(ledger, "\n")+"\n")
	return root, transcript
}

// dedupAssistant renders one transcript line; emitted is the output the parser
// is expected to report after removing thinking tokens.
func dedupAssistant(t *testing.T, at time.Duration, id string, input, output, cacheRead, cacheWrite, thinking, emitted int64) string {
	t.Helper()
	if output-thinking != emitted {
		t.Fatalf("test premise: output %d - thinking %d != emitted %d", output, thinking, emitted)
	}
	return dedupAssistantLine(t, map[string]any{
		"input_tokens":                input,
		"output_tokens":               output,
		"cache_read_input_tokens":     cacheRead,
		"cache_creation_input_tokens": cacheWrite,
		"output_tokens_details":       map[string]any{"thinking_tokens": thinking},
	}, at, id)
}

func dedupAssistantLine(t *testing.T, usage map[string]any, at time.Duration, id string) string {
	t.Helper()
	line := map[string]any{
		"type":      "assistant",
		"timestamp": dedupBase.Add(at).Format(time.RFC3339Nano),
		"sessionId": dedupSession,
		"cwd":       "/Users/demo/proj",
		"uuid":      "u-" + id,
		"requestId": "req-" + id,
		"message": map[string]any{
			"id":      "msg_" + id,
			"model":   "claude-sonnet-4-5",
			"role":    "assistant",
			"usage":   usage,
			"content": []map[string]any{{"type": "text", "text": "reply"}},
		},
	}
	raw, err := json.Marshal(line)
	if err != nil {
		t.Fatalf("marshal transcript line: %v", err)
	}
	return string(raw)
}

func dedupLedgerAt(at time.Time, origin string, input, output, cacheRead, cacheWrite int64) string {
	return dedupLedgerFor("cowork", "local_a", at, origin, input, output, cacheRead, cacheWrite)
}

func dedupLedgerFor(surface, session string, at time.Time, origin string, input, output, cacheRead, cacheWrite int64) string {
	originField := ""
	if origin != "" {
		originField = fmt.Sprintf(`"origin":%q,`, origin)
	}
	return fmt.Sprintf(`{"surface":%q,%s"sessionId":%q,"ts":%d,"models":{"claude-sonnet-4-5":{"inputTokens":%d,"outputTokens":%d,"cacheReadTokens":%d,"cacheWriteTokens":%d,"cost":{"usd":0.01}}}}`,
		surface, originField, session, at.UnixMilli(), input, output, cacheRead, cacheWrite)
}

func transcriptKey(id string) string {
	return transcriptPrefix + dedupSession + ":msg_" + id + ":req-" + id
}

func parseTranscriptKeys(t *testing.T, root, path string) []string {
	t.Helper()
	p := NewWithRoots(root)
	b, err := p.Parse(context.Background(), harness.Source{Path: path, Kind: kindJSONL}, harness.Cursor{})
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	keys := make([]string, 0, len(b.Events))
	for _, e := range b.Events {
		keys = append(keys, e.DedupKey)
	}
	slices.Sort(keys)
	return keys
}

func parseLedgerKeys(t *testing.T, root string) []string {
	t.Helper()
	p := NewWithRoots(root)
	sources, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("discover %s: %v", root, err)
	}
	var keys []string
	for _, src := range sources {
		if filepath.Base(filepath.Dir(src.Path)) != ledgerName {
			continue
		}
		b, err := p.Parse(context.Background(), src, harness.Cursor{})
		if err != nil {
			t.Fatalf("parse %s: %v", src.Path, err)
		}
		for _, e := range b.Events {
			keys = append(keys, e.DedupKey)
		}
	}
	slices.Sort(keys)
	return keys
}
