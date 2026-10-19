package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

// TestScanIndexesSyntheticClaudeLog is the one test that writes through the CLI
// surface: it points every harness override and HOME at empty temp dirs, drops
// a single Claude Code log line in CLAUDE_CONFIG_DIR, and checks that
// `mytoken scan` indexes exactly that event into a fresh MYTOKEN_HOME.
func TestScanIndexesSyntheticClaudeLog(t *testing.T) {
	h := newTestHome(t)
	dbPath := filepath.Join(h.home, "mytoken.db")

	logDir := filepath.Join(h.claudeConfig, "projects", "x")
	mustMkdir(t, logDir)
	logPath := filepath.Join(logDir, "session.jsonl")
	line := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"sessionId":"sess-scan","cwd":"/tmp/proj","uuid":"uuid-1","requestId":"req-1",`+
		`"message":{"id":"msg_1","model":"claude-sonnet-4-5","role":"assistant","content":[{"type":"text","text":"hi"}],`+
		`"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":2,"cache_creation_input_tokens":40,`+
		`"cache_creation":{"ephemeral_1h_input_tokens":30,"ephemeral_5m_input_tokens":10}}}}`,
		time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(logPath, []byte(line+"\n"), 0o600); err != nil {
		t.Fatalf("write synthetic log: %v", err)
	}
	// The scanner waits for a quiet file before ingesting it; backdate the mtime
	// the way a log written a moment ago would look to a real scan.
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(logPath, old, old); err != nil {
		t.Fatalf("backdate synthetic log: %v", err)
	}

	// MYTOKEN_HOME is empty: the scan creates the index itself. newTestHome
	// points every harness override and HOME at empty temp dirs, so the only
	// source it can discover is the synthetic log above.
	out, errOut, code := runCLI(t, "scan")
	if code != 0 {
		t.Fatalf("scan exit = %d, stderr = %s", code, errOut)
	}
	m := regexp.MustCompile(`Scanned (\d+)/(\d+) sources: (\d+) events, (\d+) sessions in `).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("unexpected scan output: %q", out)
	}
	if m[1] != m[2] {
		t.Errorf("scan reports %s of %s sources done", m[1], m[2])
	}
	if m[2] != "1" {
		t.Errorf("scan covered %s sources, want only the one claude-code root: %q", m[2], out)
	}
	if m[3] != "1" {
		t.Errorf("scan indexed %s events, want 1: %q", m[3], out)
	}
	// The printed session count only covers root sessions; this assistant line
	// belongs to a child session, which the table below still records.
	if m[4] != "0" {
		t.Errorf("scan reports %s root sessions, want 0: %q", m[4], out)
	}

	ctx := t.Context()
	ro, err := store.OpenReadOnly(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnly after scan: %v", err)
	}
	defer ro.Close()

	var harnessName, modelName string
	var input, output, cacheRead, cacheWrite, cacheWrite1h int64
	row := ro.DB().QueryRowContext(ctx, `SELECT e.harness, d.model, e.input, e.output, e.cache_read, e.cache_write, e.cache_write_1h
		FROM event_data e JOIN event_dimensions d ON d.id = e.dimension_id`)
	if err := row.Scan(&harnessName, &modelName, &input, &output, &cacheRead, &cacheWrite, &cacheWrite1h); err != nil {
		t.Fatalf("read indexed event: %v", err)
	}
	if harnessName != string(model.ClaudeCode) || modelName != "claude-sonnet-4-5" {
		t.Errorf("indexed %s/%s, want claude-code/claude-sonnet-4-5", harnessName, modelName)
	}
	if input != 10 || output != 5 || cacheRead != 2 || cacheWrite != 40 || cacheWrite1h != 30 {
		t.Errorf("indexed tokens = in %d out %d read %d write %d 1h %d, want 10 5 2 40 30",
			input, output, cacheRead, cacheWrite, cacheWrite1h)
	}
	var events, sessions int
	if err := ro.DB().QueryRowContext(ctx, `SELECT count(*) FROM event_data`).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if err := ro.DB().QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if events != 1 {
		t.Errorf("index has %d events, want 1 (another harness leaked in)", events)
	}
	if sessions < 1 {
		t.Errorf("index has %d sessions, want the scanned session recorded", sessions)
	}
	var scannedSession int
	if err := ro.DB().QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE harness=? AND session_id='sess-scan'`, string(model.ClaudeCode)).Scan(&scannedSession); err != nil {
		t.Fatalf("count scanned session: %v", err)
	}
	if scannedSession != 1 {
		t.Errorf("sessions has %d rows for sess-scan, want 1", scannedSession)
	}

	// The scanned event must be priced from the embedded catalog and visible to
	// the read-only reporting path.
	js, errOut, code := runCLI(t, "summary", "--json", "--timezone", "UTC", "--db", dbPath)
	if code != 0 {
		t.Fatalf("summary exit = %d, stderr = %s", code, errOut)
	}
	var got sumReportJSON
	jsonInto(t, js, &got)
	if got.Today.Requests != 1 {
		t.Errorf("summary today requests = %d, want 1", got.Today.Requests)
	}
	if got.Today.CostUSD <= 0 {
		t.Errorf("summary today cost = %v, want a priced API-equivalent estimate", got.Today.CostUSD)
	}
}
