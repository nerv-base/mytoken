package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/quota"
	"github.com/zzstar101/mytoken/internal/store"
)

// fixtureWriterDBEnv carries the index path to the fixture writer process.
const fixtureWriterDBEnv = "MYTOKEN_CLI_FIXTURE_WRITER_DB"

// staleFingerprint is a deliberately outdated pricing fingerprint; a read-only
// command must report it as it is, never refresh or recompute it.
const staleFingerprint = "stale-fixture-fingerprint"

// TestFixtureWALWriterProcess is not a behavioural test of its own: it is the
// writer process behind TestReadOnlyCommandsCrossProcessWALSmoke. It commits two
// priced events into a WAL index, publishes one Codex quota observation through
// the same producer path the scanner uses, records the stale pricing
// fingerprint, prints "ready" and then holds the writable connection open until
// its stdin closes. That is the state every read-only command must survive: a WAL
// index whose live sidecars belong to another process.
func TestFixtureWALWriterProcess(t *testing.T) {
	dbPath := os.Getenv(fixtureWriterDBEnv)
	if dbPath == "" {
		t.Skip("fixture writer process for TestReadOnlyCommandsCrossProcessWALSmoke")
	}
	ctx := context.Background()
	st, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixture writer: store.Open:", err)
		os.Exit(2)
	}
	if err := st.CommitMany(ctx, []store.Write{
		snapWriteP("inst-a", "dedup-a", "sess-a", "p", 1.25),
		snapWriteP("inst-b", "dedup-b", "sess-b", "p", 0.75),
	}); err != nil {
		fmt.Fprintln(os.Stderr, "fixture writer: CommitMany:", err)
		os.Exit(2)
	}
	if err := st.SetSetting(ctx, "pricing_fingerprint", staleFingerprint); err != nil {
		fmt.Fprintln(os.Stderr, "fixture writer: SetSetting:", err)
		os.Exit(2)
	}
	// Publish a quota observation exactly as the scanner does: one setting key
	// per instance, offline, no credentials and no network.
	observed := time.Now().Add(-2 * time.Minute).UTC()
	used, windowMinutes := 61.0, int64(300)
	if err := quota.PublishLatest(ctx, st, "inst-a", &quota.Report{
		Source:     "fixture/inst-a",
		ObservedAt: &observed,
		Line:       1,
		Status:     quota.StatusOK,
		Primary: &quota.Window{
			Kind: quota.WindowPrimary, Status: quota.StatusOK,
			UsedPercent: &used, WindowMinutes: &windowMinutes,
		},
		LinesRead: 1,
	}, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "fixture writer: PublishLatest:", err)
		os.Exit(2)
	}
	fmt.Println("ready")
	_ = os.Stdout.Sync()
	// Hold the writable WAL connection (and its -wal/-shm) open until released.
	_, _ = io.Copy(io.Discard, os.Stdin)
	if err := st.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "fixture writer: Close:", err)
		os.Exit(2)
	}
}

// TestReadOnlyCommandsCrossProcessWALSmoke is the end-to-end proof that the
// read-only surface is usable against an index another process is writing. A
// fixture writer process holds a WAL index open; snapshot, statusline, report,
// doctor and quota each read it through store.OpenReadOnly and must report real
// numbers instead of an unavailable index. The committed cost and the stale
// pricing fingerprint must be untouched, the database image and committed WAL
// frames must be unchanged, the same reads must still work after the writer has
// closed and checkpointed, and a missing index must be reported without creating
// anything.
func TestReadOnlyCommandsCrossProcessWALSmoke(t *testing.T) {
	if os.Getenv(fixtureWriterDBEnv) != "" {
		t.Skip("fixture writer process; the parent runs the smoke assertions")
	}
	dir := t.TempDir()
	t.Setenv("MYTOKEN_HOME", dir)
	dbPath := filepath.Join(dir, "mytoken.db")

	release := startFixtureWALWriter(t, dir, dbPath)
	dbBefore := roSmokeDigest(t, dbPath)
	walBefore := roSmokeDigest(t, dbPath+"-wal")

	// While the foreign writer is alive: every read-only command reports the two
	// committed events instead of "unavailable".
	assertSnapshotSmokeTotals(t, "live writer")
	assertStatuslineSmokeReadsLiveIndex(t)
	assertReportSmokeTotals(t)
	assertDoctorSmokeReadsLiveIndex(t)
	assertQuotaSmokeReadsLiveIndex(t, "live writer")

	// The reader may write SQLite's own -shm read mark; the database file and
	// the committed WAL frames are exactly as the writer left them.
	roSmokeEqual(t, "db bytes after the reads", roSmokeDigest(t, dbPath), dbBefore)
	roSmokeEqual(t, "committed wal bytes after the reads", roSmokeDigest(t, dbPath+"-wal"), walBefore)

	// Closing the writer checkpoints the WAL and unlinks the sidecars; the same
	// reads must still return the real totals.
	release()
	dbAfter := roSmokeDigest(t, dbPath)
	assertSnapshotSmokeTotals(t, "closed writer")
	assertQuotaSmokeReadsLiveIndex(t, "closed writer")
	roSmokeEqual(t, "db bytes after a post-checkpoint read", roSmokeDigest(t, dbPath), dbAfter)

	// The stale cost and pricing fingerprint the writer committed are unchanged:
	// a read never reprices and never rewrites metadata.
	assertStaleCostAndFingerprint(t, dbPath)

	// A missing index is reported without creating the file.
	empty := t.TempDir()
	t.Setenv("MYTOKEN_HOME", empty)
	missing := filepath.Join(empty, "mytoken.db")
	before := snapFingerprint(t, empty)
	out, errout, code := runCLI(t, "snapshot", "--once", "--json")
	if code != 0 {
		t.Fatalf("snapshot on a missing index: code=%d stderr=%s", code, errout)
	}
	doc := snapDecode(t, out)
	if got := snapAt(doc, "sources", "index", "reason"); got != string(store.ReasonMissingDatabase) {
		t.Fatalf("missing index reason = %v, want %v", got, store.ReasonMissingDatabase)
	}
	// quota reads the same missing index as an unavailable verdict and creates
	// nothing: no index, no schema bootstrap, no writable open.
	quotaOut, quotaErr, quotaCode := runCLI(t, "quota", "list", "--json")
	if quotaCode != 0 {
		t.Fatalf("quota list on a missing index: code=%d stderr=%s", quotaCode, quotaErr)
	}
	quotaDoc := snapDecode(t, quotaOut)
	if got := snapAt(quotaDoc, "status", "reason"); got != string(store.ReasonMissingDatabase) {
		t.Fatalf("quota section reason = %v, want %v", got, store.ReasonMissingDatabase)
	}
	if rows, _ := quotaDoc["instances"].([]any); len(rows) != 0 {
		t.Fatalf("quota instances on a missing index = %v, want none", quotaDoc["instances"])
	}
	// The snapshot reports the same reason for its quota section.
	if got := snapAt(doc, "sources", "quota", "reason"); got != string(store.ReasonMissingDatabase) {
		t.Fatalf("snapshot sources.quota.reason = %v, want %v", got, store.ReasonMissingDatabase)
	}
	roSmokeEqual(t, "directory contents", snapFingerprint(t, empty), before)
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("read-only snapshot created %s (stat err = %v)", missing, err)
	}
}

// startFixtureWALWriter starts the fixture writer process, waits for it to hold
// a live WAL index and returns a release function that closes its stdin (so it
// checkpoints and exits) exactly once.
func startFixtureWALWriter(t *testing.T, dir, dbPath string) func() {
	t.Helper()
	cmd := exec.Command(os.Args[0],
		"-test.run=^TestFixtureWALWriterProcess$", "-test.count=1", "-test.timeout=120s")
	cmd.Env = append(os.Environ(), fixtureWriterDBEnv+"="+dbPath, "MYTOKEN_HOME="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fixture writer: %v", err)
	}
	ready := make(chan struct{})
	gone := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == "ready" {
				close(ready)
				// Keep draining so the child never blocks on a full pipe.
				for sc.Scan() {
				}
				return
			}
		}
		close(gone)
	}()

	select {
	case <-ready:
	case <-gone:
		_ = cmd.Wait()
		t.Fatal("fixture writer exited before signalling readiness")
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("timed out waiting for the fixture writer to signal readiness")
	}

	var once sync.Once
	release := func() {
		once.Do(func() {
			_ = stdin.Close()
			if err := cmd.Wait(); err != nil {
				t.Errorf("fixture writer exit: %v", err)
			}
		})
	}
	t.Cleanup(release)

	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dbPath + suffix); err != nil {
			t.Fatalf("fixture writer should hold %s: %v", dbPath+suffix, err)
		}
	}
	return release
}

// assertSnapshotSmokeTotals runs the default read-only snapshot and requires the
// two committed events (tokens 20+4+5 each, $1.25 + $0.75) to come back as real
// values.
func assertSnapshotSmokeTotals(t *testing.T, when string) {
	t.Helper()
	out, errout, code := runCLI(t, "snapshot", "--once", "--json")
	if code != 0 {
		t.Fatalf("snapshot [%s]: code=%d stderr=%s", when, code, errout)
	}
	doc := snapDecode(t, out)
	if avail, _ := snapAt(doc, "sources", "index", "available").(bool); !avail {
		t.Fatalf("snapshot [%s]: index unavailable: reason=%v detail=%v",
			when, snapAt(doc, "sources", "index", "reason"), snapAt(doc, "sources", "index", "detail"))
	}
	if state := snapAt(doc, "schema", "state"); state != string(store.SchemaOK) {
		t.Fatalf("snapshot [%s]: schema = %v", when, state)
	}
	totals, ok := snapAt(doc, "data", "totals").(map[string]any)
	if !ok {
		t.Fatalf("snapshot [%s]: totals = %v, want real values", when, snapAt(doc, "data", "totals"))
	}
	if req, _ := totals["requests"].(float64); int64(req) != 2 {
		t.Fatalf("snapshot [%s]: requests = %v, want 2", when, totals["requests"])
	}
	if tokens, _ := snapAt(totals, "tokens").(map[string]any); smokeTokenTotal(tokens) != 58 {
		t.Fatalf("snapshot [%s]: tokens = %v, want 58", when, totals["tokens"])
	}
	if cost, _ := totals["costUsd"].(float64); cost != 2.0 {
		t.Fatalf("snapshot [%s]: costUsd = %v, want 2", when, totals["costUsd"])
	}
}

// assertStatuslineSmokeReadsLiveIndex requires the statusline to resolve the
// most recent session (one request, $1.25) instead of an unavailable index.
func assertStatuslineSmokeReadsLiveIndex(t *testing.T) {
	t.Helper()
	out, errout, code := runCLI(t, "statusline", "--json")
	if code != 0 {
		t.Fatalf("statusline: code=%d stderr=%s", code, errout)
	}
	doc := snapDecode(t, out)
	if avail, _ := snapAt(doc, "index", "available").(bool); !avail {
		t.Fatalf("statusline: index unavailable: reason=%v", snapAt(doc, "index", "reason"))
	}
	session, ok := snapAt(doc, "session").(map[string]any)
	if !ok {
		t.Fatalf("statusline: session = %v, want the most recent committed session", snapAt(doc, "session"))
	}
	if req, _ := session["requests"].(float64); int64(req) != 1 {
		t.Fatalf("statusline: session requests = %v, want 1", session["requests"])
	}
	// Which of the two committed sessions is "most recent" depends on the
	// fixture clock; both are priced, so either real cost is correct here.
	if cost, _ := session["costUsd"].(float64); cost != 1.25 && cost != 0.75 {
		t.Fatalf("statusline: session costUsd = %v, want a committed 1.25 or 0.75", session["costUsd"])
	}
}

// assertReportSmokeTotals requires the day report to read both committed events.
func assertReportSmokeTotals(t *testing.T) {
	t.Helper()
	out, errout, code := runCLI(t, "report", "--json")
	if code != 0 {
		t.Fatalf("report: code=%d stderr=%s", code, errout)
	}
	doc := snapDecode(t, out)
	totals, ok := snapAt(doc, "current", "totals").(map[string]any)
	if !ok {
		t.Fatalf("report: current totals = %v", snapAt(doc, "current"))
	}
	if req, _ := totals["requests"].(float64); int64(req) != 2 {
		t.Fatalf("report: requests = %v, want 2", totals["requests"])
	}
}

// assertDoctorSmokeReadsLiveIndex requires doctor to open the foreign writer's
// index read-only instead of refusing it (the pre-fix behaviour reported
// dbUnavailable with reason wal_writer_required).
func assertDoctorSmokeReadsLiveIndex(t *testing.T) {
	t.Helper()
	out, errout, code := runCLI(t, "doctor", "--json")
	if code != 0 {
		t.Fatalf("doctor: code=%d stderr=%s", code, errout)
	}
	doc := snapDecode(t, out)
	if doc["readOnly"] != true {
		t.Fatalf("doctor: readOnly = %v, want true", doc["readOnly"])
	}
	if doc["dbUnavailable"] != false {
		t.Fatalf("doctor: dbUnavailable = %v (reason %v, detail %v), want false",
			doc["dbUnavailable"], doc["dbReason"], doc["dbDetail"])
	}
	if state := snapAt(doc, "schema", "state"); state != string(store.SchemaOK) {
		t.Fatalf("doctor: schema = %v, want %v", state, store.SchemaOK)
	}
}

// assertQuotaSmokeReadsLiveIndex requires `quota list` to read the observation
// the foreign writer published instead of an unavailable section, to keep the
// instances apart, and to refuse a pace: two priced events in the same index
// must never be turned into a quota pace.
func assertQuotaSmokeReadsLiveIndex(t *testing.T, when string) {
	t.Helper()
	out, errout, code := runCLI(t, "quota", "list", "--json")
	if code != 0 {
		t.Fatalf("quota list [%s]: code=%d stderr=%s", when, code, errout)
	}
	quotaAssertNoTokenDerived(t, out, "quota list ["+when+"]")
	doc := snapDecode(t, out)
	if avail, _ := snapAt(doc, "status", "available").(bool); !avail {
		t.Fatalf("quota list [%s]: section unavailable: reason=%v detail=%v",
			when, snapAt(doc, "status", "reason"), snapAt(doc, "status", "detail"))
	}
	rows, ok := doc["instances"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("quota list [%s]: instances = %v, want exactly the one published instance", when, doc["instances"])
	}
	entry, _ := rows[0].(map[string]any)
	if entry["instance"] != "inst-a" || entry["available"] != true {
		t.Fatalf("quota list [%s]: entry = %v, want the published inst-a observation", when, entry)
	}
	if got := snapAt(entry, "primary", "usedPercent"); got != float64(61) {
		t.Fatalf("quota list [%s]: primary.usedPercent = %v, want the stored 61", when, got)
	}
	// One stored observation is not a pace. The verdict must stay unavailable:
	// when the envelope carries the pace worker's own retained forecast it
	// names its reason (insufficient_samples), and an envelope that predates
	// retention falls back to the reader's own refusal (single_observation).
	// Either way no rate is invented here.
	if avail, _ := snapAt(entry, "forecast", "available").(bool); avail {
		t.Fatalf("quota list [%s]: forecast.available = true, want an honest refusal", when)
	}
	if got := snapAt(entry, "forecast", "reason"); got != "insufficient_samples" && got != "single_observation" {
		t.Fatalf("quota list [%s]: forecast.reason = %v, want an honest refusal", when, got)
	}
	if pace, ok := entry["pace"].(map[string]any); ok {
		if paceAvail, _ := pace["available"].(bool); paceAvail {
			t.Fatalf("quota list [%s]: stored pace = %v claims a pace from one observation", when, pace)
		}
	}

	// An instance with nothing stored stays visible when it is asked for.
	out, errout, code = runCLI(t, "quota", "list", "--json", "--instance-id", "inst-b")
	if code != 0 {
		t.Fatalf("quota list --instance-id inst-b [%s]: code=%d stderr=%s", when, code, errout)
	}
	filtered := snapDecode(t, out)
	rows, _ = filtered["instances"].([]any)
	if len(rows) != 1 {
		t.Fatalf("quota list --instance-id inst-b [%s]: instances = %v, want one entry", when, filtered["instances"])
	}
	if row, _ := rows[0].(map[string]any); snapAt(row, "status", "reason") != "no_observation" {
		t.Fatalf("quota list --instance-id inst-b [%s]: %v, want no_observation", when, rows[0])
	}
}

// assertStaleCostAndFingerprint re-reads the index read-only and requires the
// committed cost and the stale pricing fingerprint to be exactly as written.
func assertStaleCostAndFingerprint(t *testing.T, dbPath string) {
	t.Helper()
	ctx := context.Background()
	ro, err := store.OpenReadOnly(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnly for the stale-metadata check: %v", err)
	}
	defer ro.Close()

	var fp string
	if err := ro.DB().QueryRowContext(ctx, "SELECT value FROM settings WHERE key = 'pricing_fingerprint'").Scan(&fp); err != nil {
		t.Fatalf("read pricing_fingerprint: %v", err)
	}
	roSmokeEqual(t, "pricing_fingerprint", fp, staleFingerprint)

	var cost float64
	if err := ro.DB().QueryRowContext(ctx, "SELECT cost FROM events WHERE dedup_key = 'dedup-a'").Scan(&cost); err != nil {
		t.Fatalf("read committed cost: %v", err)
	}
	if cost != 1.25 {
		t.Fatalf("committed cost = %v, want 1.25", cost)
	}
}

// roSmokeDigest is the sha256 of a file's bytes.
func roSmokeDigest(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func roSmokeEqual(t *testing.T, label, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %q, want %q", label, got, want)
	}
}

func smokeTokenTotal(tokens map[string]any) int64 {
	var sum int64
	for _, key := range []string{"input", "output", "cacheRead", "cacheWrite", "reasoning"} {
		v, _ := tokens[key].(float64)
		sum += int64(v)
	}
	return sum
}
