package cli

// Tests for the two behaviours this change is about: a statusline that refuses
// to merge independent relay accounts, and a sources/doctor/snapshot view that
// shows what a scan actually collected instead of what is merely configured.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/instance"
	"github.com/zzstar101/mytoken/internal/relay"
	"github.com/zzstar101/mytoken/internal/scan"
	"github.com/zzstar101/mytoken/internal/store"
)

// relaySeed is one relay site and, when an amount is set, its recorded balance.
type relaySeed struct {
	origin    string
	keyID     string
	at        time.Time
	remaining *float64
	currency  string
	unlimited bool
}

func floatPtr(v float64) *float64 { return &v }

// snapSeedIndex writes the index inside dir (already the process's
// MYTOKEN_HOME) with the given settings and relay readings, then seals it so
// every command under test reads it through store.OpenReadOnly.
func snapSeedIndex(t *testing.T, dir string, settings map[string]string, sites ...relaySeed) {
	t.Helper()
	path := filepath.Join(dir, "mytoken.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	for _, s := range sites {
		if err := st.PutRelaySite(t.Context(), relay.Site{Origin: s.origin, KeyID: s.keyID, DetectedAt: s.at}); err != nil {
			t.Fatal(err)
		}
		if s.remaining == nil && !s.unlimited {
			continue
		}
		if err := st.PutRelayBalance(t.Context(), relay.Balance{
			Origin: s.origin, KeyID: s.keyID, At: s.at,
			RemainingUSD: s.remaining, Unlimited: s.unlimited, Currency: s.currency,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range settings {
		if err := st.SetSetting(t.Context(), k, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB().Exec("PRAGMA journal_mode=DELETE"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

// noFallbackInstances removes the synthetic default roots, so a test sees only
// the instances it configured.
func noFallbackInstances(t *testing.T) {
	t.Helper()
	prev := sourceFallbacks
	sourceFallbacks = func() []instance.Fallback { return nil }
	t.Cleanup(func() { sourceFallbacks = prev })
}

// diagBlob is the stored form of one scan's collection evidence.
func diagBlob(t *testing.T, d scan.Diagnostics) string {
	t.Helper()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// rowByID finds one entry of a report's instances array by id.
func rowByID(t *testing.T, raw any, id string) map[string]any {
	t.Helper()
	entries, _ := raw.([]any)
	for _, e := range entries {
		m, _ := e.(map[string]any)
		if m["id"] == id {
			return m
		}
	}
	t.Fatalf("no row for %q in %v", id, raw)
	return nil
}

// diagByInstance finds one entry of a diagnostics array by instance id.
func diagByInstance(t *testing.T, raw any, instance string) map[string]any {
	t.Helper()
	entries, _ := raw.([]any)
	for _, e := range entries {
		m, _ := e.(map[string]any)
		if m["instance"] == instance {
			return m
		}
	}
	t.Fatalf("no diagnostics entry for instance %q in %v", instance, raw)
	return nil
}

// TestStatuslineNeverMergesRelayAccountsInTextOrJSON pins the plaintext and JSON
// shape for two keys of one origin: two accounts, two amounts, no sum, and no
// timestamp presented as "the" reading time.
func TestStatuslineNeverMergesRelayAccountsInTextOrJSON(t *testing.T) {
	const origin = "https://relay.example.com"
	dir := t.TempDir()
	t.Setenv("MYTOKEN_HOME", dir)
	at1 := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	at2 := at1.Add(45 * time.Minute)
	snapSeedIndex(t, dir, nil,
		relaySeed{origin: origin, keyID: "key-one", at: at1, remaining: floatPtr(10), currency: "USD"},
		relaySeed{origin: origin, keyID: "key-two", at: at2, remaining: floatPtr(20), currency: "USD"},
	)
	before := snapFingerprint(t, dir)

	out, errout, code := runCLI(t, "statusline", "--json")
	if code != 0 {
		t.Fatalf("statusline --json: code=%d stderr=%s", code, errout)
	}
	doc := snapDecode(t, out)
	if snapAt(doc, "balance", "available") != false || snapAt(doc, "balance", "reason") != "multiple_balances" {
		t.Fatalf("balance = %v, want unavailable multiple_balances", snapAt(doc, "balance"))
	}
	if snapAt(doc, "balance", "remainingUsd") != nil || snapAt(doc, "balance", "at") != nil {
		t.Fatalf("JSON exposed a merged scalar: %v", snapAt(doc, "balance"))
	}
	accounts, _ := snapAt(doc, "balance", "accounts").([]any)
	if len(accounts) != 2 {
		t.Fatalf("JSON accounts = %v, want both", snapAt(doc, "balance", "accounts"))
	}
	first, _ := accounts[0].(map[string]any)
	if first["origin"] != origin || first["keyId"] != "key-one" || first["remainingUsd"] != 10.0 {
		t.Fatalf("first account = %v, want key-one's own 10 USD", first)
	}
	second, _ := accounts[1].(map[string]any)
	if second["keyId"] != "key-two" || second["remainingUsd"] != 20.0 {
		t.Fatalf("second account = %v, want key-two's own 20 USD", second)
	}
	if snapAt(doc, "balance", "recorded") != 2.0 || snapAt(doc, "balance", "partial") != false {
		t.Fatalf("recorded/partial = %v/%v", snapAt(doc, "balance", "recorded"), snapAt(doc, "balance", "partial"))
	}

	text, _, code := runCLI(t, "statusline")
	if code != 0 {
		t.Fatalf("statusline: code=%d", code)
	}
	for _, want := range []string{"multiple_balances", "2 account(s)", "relay.example.com/key-one=$10.00", "relay.example.com/key-two=$20.00", at1.Format(time.RFC3339), at2.Format(time.RFC3339)} {
		if !strings.Contains(text, want) {
			t.Fatalf("statusline text %q missing %q", text, want)
		}
	}
	if strings.Contains(text, "30.00") {
		t.Fatalf("statusline summed two keys: %q", text)
	}
	if got := snapFingerprint(t, dir); got != before {
		t.Fatalf("statusline rewrote the data directory:\nbefore %s\nafter  %s", before, got)
	}
}

// TestStatuslinePartialAndUnlimitedAreVisible keeps the partial and unlimited
// cases legible on the command line: a key with no reading is named, never
// filled in or summed, and unlimited is not an amount.
func TestStatuslinePartialAndUnlimitedAreVisible(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MYTOKEN_HOME", dir)
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	snapSeedIndex(t, dir, nil,
		relaySeed{origin: "https://relay.example.com", keyID: "key-one", at: at, remaining: floatPtr(12.5), currency: "USD"},
		relaySeed{origin: "https://relay.example.com", keyID: "key-two", at: at},
	)
	text, _, code := runCLI(t, "statusline")
	if code != 0 {
		t.Fatalf("statusline: code=%d", code)
	}
	for _, want := range []string{"multiple_balances", "partial", "2 account(s)", "key-one=$12.50", "key-two=no amount recorded"} {
		if !strings.Contains(text, want) {
			t.Fatalf("statusline text %q missing %q", text, want)
		}
	}
	if strings.Contains(text, "balance $12.50") {
		t.Fatalf("a second key did not stop the scalar: %q", text)
	}
	// The key with no reading carries no timestamp either: the record never
	// stated one, so none is invented.
	if strings.Contains(text, "key-two=no amount recorded at") {
		t.Fatalf("a key with no reading got an invented timestamp: %q", text)
	}
	out, _, code := runCLI(t, "statusline", "--json")
	if code != 0 {
		t.Fatalf("statusline --json: code=%d", code)
	}
	doc := snapDecode(t, out)
	if snapAt(doc, "balance", "available") != false || snapAt(doc, "balance", "remainingUsd") != nil {
		t.Fatalf("balance = %v, want unavailable with no scalar", snapAt(doc, "balance"))
	}
	if snapAt(doc, "balance", "partial") != true || snapAt(doc, "balance", "recorded") != 1.0 {
		t.Fatalf("partial/recorded = %v/%v", snapAt(doc, "balance", "partial"), snapAt(doc, "balance", "recorded"))
	}

	dh := t.TempDir()
	t.Setenv("MYTOKEN_HOME", dh)
	snapSeedIndex(t, dh, nil, relaySeed{origin: "https://relay.example.com", keyID: "key-one", at: at, unlimited: true, currency: "USD"})
	utext, _, code := runCLI(t, "statusline")
	if code != 0 || !strings.Contains(utext, "balance unlimited") {
		t.Fatalf("statusline = %q (code=%d), want an unlimited balance", utext, code)
	}
}

// TestSourcesAndDoctorReportStoredDiagnostics wires scan.LoadDiagnostics into
// both command views: a stored record shows what the scan did, a missing record
// is unavailable with a reason rather than zero counts, and a disabled or
// backup-copy entry keeps its configuration status with a "not collected"
// explanation.
func TestSourcesAndDoctorReportStoredDiagnostics(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYTOKEN_HOME", home)
	noFallbackInstances(t)
	rootA, rootB, rootHistory := t.TempDir(), t.TempDir(), t.TempDir()
	oldRoot := t.TempDir()
	addInstance(t, "--id", "inst-a", "--root", rootA)
	addInstance(t, "--id", "inst-b", "--root", rootB)
	addInstance(t, "--id", "inst-moved", "--root", rootHistory)
	addInstance(t, "--id", "inst-off", "--root", rootA, "--disabled")
	addInstance(t, "--id", "inst-backup", "--root", rootB, "--kind", "backup-copy")

	attempt := time.Now().Add(-3 * time.Hour).UTC().Truncate(time.Second)
	success := attempt.Add(-30 * time.Minute)
	sessions := int64(3)
	settings := map[string]string{
		scan.DiagnosticsSettingKey("codex", "inst-a"): diagBlob(t, scan.Diagnostics{
			Version: scan.DiagnosticsVersion, Harness: "codex", Instance: "inst-a",
			LastAttemptAt: attempt, LastSuccessAt: success, Reason: scan.ReasonOK,
			DiscoveredFiles: 4, NewEvents: 7, IndexedSessions: &sessions,
			Sources: []scan.SourceDiagnostics{{Path: filepath.Join(rootA, "session.jsonl"), Kind: "jsonl", Reason: scan.ReasonOK, Events: 7, Sessions: 1}},
		}),
		// A record keyed by the same instance id but naming a path outside its
		// current root: last-known evidence about where it used to be.
		scan.DiagnosticsSettingKey("codex", "inst-moved"): diagBlob(t, scan.Diagnostics{
			Version: scan.DiagnosticsVersion, Harness: "codex", Instance: "inst-moved",
			LastAttemptAt: attempt, Reason: scan.ReasonOK,
			Sources: []scan.SourceDiagnostics{{Path: filepath.Join(oldRoot, "session.jsonl"), Kind: "jsonl", Reason: scan.ReasonOK}},
		}),
	}
	snapSeedIndex(t, home, settings)
	before := snapFingerprint(t, home)

	// sources list: the per-instance evidence, in JSON.
	_, out, errout, code := listSources(t)
	if code != 0 {
		t.Fatalf("sources list --json: code=%d stderr=%s", code, errout)
	}
	doc := snapDecode(t, out)
	srcDiag := func(id string) map[string]any {
		d, _ := rowByID(t, snapAt(doc, "instances"), id)["diagnostics"].(map[string]any)
		if d == nil {
			t.Fatalf("no diagnostics for %q in %s", id, out)
		}
		return d
	}
	a := srcDiag("inst-a")
	if a["available"] != true || a["discoveredFiles"] != 4.0 || a["newEvents"] != 7.0 || a["indexedSessions"] != 3.0 || a["collectionReason"] != "ok" {
		t.Fatalf("inst-a diagnostics = %v", a)
	}
	if a["lastAttemptAt"] == nil || a["attemptAgeSeconds"] == nil || a["lastSuccessAt"] == nil {
		t.Fatalf("inst-a lost its timestamps: %v", a)
	}
	if srcs, _ := a["sources"].([]any); len(srcs) != 1 {
		t.Fatalf("inst-a sources = %v", a["sources"])
	}
	missing := srcDiag("inst-b")
	if missing["available"] != false || missing["reason"] != "no_record" {
		t.Fatalf("inst-b diagnostics = %v, want unavailable no_record", missing)
	}
	// A missing record states no counts at all: never a fabricated zero.
	if missing["discoveredFiles"] != nil || missing["newEvents"] != nil || missing["indexedSessions"] != nil || missing["lastAttemptAt"] != nil {
		t.Fatalf("a missing record invented counts: %v", missing)
	}
	if moved := srcDiag("inst-moved"); moved["historical"] != true {
		t.Fatalf("inst-moved diagnostics = %v, want historical", moved)
	}
	if off := srcDiag("inst-off"); off["available"] != false || off["reason"] != "not_collected" {
		t.Fatalf("disabled entry diagnostics = %v, want not_collected", off)
	}
	if back := srcDiag("inst-backup"); back["reason"] != "not_collected" {
		t.Fatalf("backup-copy entry diagnostics = %v, want not_collected", back)
	}
	// The configuration status is kept alongside the explanation.
	if got := rowByID(t, snapAt(doc, "instances"), "inst-off")["status"]; got != "disabled" {
		t.Fatalf("disabled entry status = %v", got)
	}
	if got := rowByID(t, snapAt(doc, "instances"), "inst-backup")["status"]; got != "backup-copy" {
		t.Fatalf("backup entry status = %v", got)
	}

	text, _, code := runCLI(t, "sources", "list")
	if code != 0 {
		t.Fatalf("sources list: code=%d", code)
	}
	for _, want := range []string{"DIAGNOSTICS", "diagnostics codex/inst-a", "no_record", "not_collected", "historical", "files=4", "events=7", "sessions=3"} {
		if !strings.Contains(text, want) {
			t.Fatalf("sources list text missing %q:\n%s", want, text)
		}
	}

	// doctor: the same evidence, joined onto its existing read-only handle.
	docOut, _, code := runCLI(t, "doctor", "--json")
	if code != 0 {
		t.Fatalf("doctor --json: code=%d", code)
	}
	ddoc := snapDecode(t, docOut)
	if snapAt(ddoc, "readOnly") != true {
		t.Fatalf("doctor readOnly = %v", snapAt(ddoc, "readOnly"))
	}
	diagA, _ := rowByID(t, snapAt(ddoc, "instances"), "inst-a")["diagnostics"].(map[string]any)
	if diagA == nil || diagA["available"] != true || diagA["discoveredFiles"] != 4.0 {
		t.Fatalf("doctor inst-a diagnostics = %v", diagA)
	}
	dtext, _, code := runCLI(t, "doctor")
	if code != 0 {
		t.Fatalf("doctor: code=%d", code)
	}
	if !strings.Contains(dtext, "DIAGNOSTICS") || !strings.Contains(dtext, "diagnostics codex/inst-a") {
		t.Fatalf("doctor text missing the diagnostics view:\n%s", dtext)
	}

	// snapshot: the sources section carries the section verdict and the per
	// instance evidence, and reads never write.
	snapOut, _, code := runCLI(t, "snapshot", "--once", "--json")
	if code != 0 {
		t.Fatalf("snapshot: code=%d", code)
	}
	sdoc := snapDecode(t, snapOut)
	if snapAt(sdoc, "sources", "diagnostics", "available") != true {
		t.Fatalf("snapshot sources.diagnostics = %v", snapAt(sdoc, "sources", "diagnostics"))
	}
	sdiagA := diagByInstance(t, snapAt(sdoc, "data", "diagnostics"), "inst-a")
	if sdiagA["available"] != true || sdiagA["newEvents"] != 7.0 || sdiagA["collectionReason"] != "ok" {
		t.Fatalf("snapshot inst-a diagnostics = %v", sdiagA)
	}
	// An unfiltered snapshot reports the records the index holds. It cannot know
	// about a configured instance that has no record; that view belongs to
	// sources/doctor, which hold the configuration.
	if entries, _ := snapAt(sdoc, "data", "diagnostics").([]any); len(entries) != 2 {
		t.Fatalf("snapshot diagnostics = %v, want the two stored records", entries)
	}
	snapText, _, code := runCLI(t, "snapshot", "--once")
	if code != 0 {
		t.Fatalf("snapshot text: code=%d", code)
	}
	if !strings.Contains(snapText, "diagnostics=available") || !strings.Contains(snapText, "instance(s), not combined") {
		t.Fatalf("snapshot text missing the diagnostics section:\n%s", snapText)
	}
	if !strings.Contains(snapText, "codex/inst-a attempt=3h0m") {
		t.Fatalf("snapshot text missing inst-a's attempt age:\n%s", snapText)
	}

	// A filtered snapshot knows the instances it was asked about, so an instance
	// with no stored record is reported as unavailable with a reason, and never
	// as zero counts.
	emptyOut, _, code := runCLI(t, "snapshot", "--once", "--json", "--instance-id", "inst-b")
	if code != 0 {
		t.Fatalf("filtered snapshot: code=%d", code)
	}
	edoc := snapDecode(t, emptyOut)
	if snapAt(edoc, "sources", "diagnostics", "available") != false || snapAt(edoc, "sources", "diagnostics", "reason") != "no_record" {
		t.Fatalf("filtered sources.diagnostics = %v", snapAt(edoc, "sources", "diagnostics"))
	}
	eb := diagByInstance(t, snapAt(edoc, "data", "diagnostics"), "inst-b")
	if eb["available"] != false || eb["reason"] != "no_record" {
		t.Fatalf("filtered inst-b diagnostics = %v", eb)
	}
	if eb["newEvents"] != nil || eb["discoveredFiles"] != nil || eb["indexedSessions"] != nil {
		t.Fatalf("a missing record invented counts: %v", eb)
	}
	emptyText, _, code := runCLI(t, "snapshot", "--once", "--instance-id", "inst-b")
	if code != 0 {
		t.Fatalf("filtered snapshot text: code=%d", code)
	}
	if !strings.Contains(emptyText, "diagnostics=unavailable (no_record)") || !strings.Contains(emptyText, "inst-b no_record:") {
		t.Fatalf("filtered snapshot text missing the no_record reason:\n%s", emptyText)
	}

	if got := snapFingerprint(t, home); got != before {
		t.Fatalf("a read-only command rewrote the data directory:\nbefore %s\nafter  %s", before, got)
	}
}

// TestFlagFirstShorthandIsTheSameSnapshot pins the roadmap shorthand: arguments
// that start with a flag are exactly the snapshot command, never a hidden GUI,
// scan or network call, and an unknown option stays a usage error.
func TestFlagFirstShorthandIsTheSameSnapshot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MYTOKEN_HOME", dir)
	snapSeed(t, nil, snapWrite("a", "k1", "s1", 0.5))
	before := snapFingerprint(t, dir)

	explicit, _, code := runCLI(t, "snapshot", "--once", "--json")
	if code != 0 {
		t.Fatalf("snapshot --once --json: code=%d", code)
	}
	short, _, code := runCLI(t, "--json", "--once")
	if code != 0 {
		t.Fatalf("--json --once: code=%d", code)
	}
	var a, b map[string]any
	if err := json.Unmarshal([]byte(explicit), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(short), &b); err != nil {
		t.Fatal(err)
	}
	delete(a, "generatedAt")
	delete(b, "generatedAt")
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	if string(ra) != string(rb) {
		t.Fatalf("shorthand snapshot differs:\nexplicit %s\nshort    %s", ra, rb)
	}
	if snapAt(b, "snapshotVersion") != 1.0 {
		t.Fatalf("shorthand snapshot version = %v", snapAt(b, "snapshotVersion"))
	}
	if performed, _ := snapAt(b, "writes", "performed").([]any); len(performed) != 0 {
		t.Fatalf("shorthand snapshot wrote: %v", snapAt(b, "writes"))
	}
	text, _, code := runCLI(t, "--once")
	if code != 0 || !strings.HasPrefix(text, "snapshot v1") {
		t.Fatalf("--once text = %q (code=%d)", text, code)
	}
	if _, _, code := runCLI(t, "--bogus"); code != 2 {
		t.Fatalf("--bogus exit = %d, want 2", code)
	}
	if _, _, code := runCLI(t, "--once", "extra"); code != 2 {
		t.Fatalf("--once extra exit = %d, want 2", code)
	}
	if _, _, code := runCLI(t, "--help"); code != 2 {
		t.Fatalf("--help exit = %d, want 2", code)
	}
	if got := snapFingerprint(t, dir); got != before {
		t.Fatalf("the shorthand wrote to the data directory:\nbefore %s\nafter  %s", before, got)
	}
}
