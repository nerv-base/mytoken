package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/claudequota"
	"github.com/zzstar101/mytoken/internal/instance"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/snapshot"
	"github.com/zzstar101/mytoken/internal/store"
)

// fakeClaude is the test's source and transport binding for the Claude quota
// CLI. Every read and fetch is counted, so a test can prove that a read-only
// command never touched a credential and that a refresh is the only path that
// reached the transport. No test here touches a real credential or the network.
type fakeClaude struct {
	mu       sync.Mutex
	reads    int
	fetches  int
	instance string
	ref      string
}

func (f *fakeClaude) install(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	if f.instance == "" {
		f.instance = instance.DefaultID
	}
	if f.ref == "" {
		f.ref = "/tmp/fake-home/.claude/.credentials.json"
	}
	instanceID, ref := f.instance, f.ref
	f.mu.Unlock()
	resolved := instance.Resolved{
		ID:      instanceID,
		Label:   "fake claude",
		Harness: model.ClaudeCode,
		Source:  instance.PathDefault,
	}
	src := claudequota.Source{
		Instance: instanceID,
		Label:    "fake claude",
		Kind:     claudequota.SourceFile,
		Ref:      ref,
		Read: func(context.Context) (claudequota.Credential, error) {
			f.mu.Lock()
			f.reads++
			f.mu.Unlock()
			return claudequota.NewCredential("fake-token-not-a-secret"), nil
		},
	}
	prev := claudeQuotaResolveSources
	claudeQuotaResolveSources = func(string, []instance.Fallback) (claudeQuotaBinding, error) {
		return claudeQuotaBinding{
			Sources:   []claudequota.Source{src},
			Instances: []instance.Resolved{resolved},
		}, nil
	}
	t.Cleanup(func() { claudeQuotaResolveSources = prev })
}

// transport installs a fake transport answering every request with body, and
// counts calls.
func (f *fakeClaude) transport(t *testing.T, body string) {
	t.Helper()
	prev := claudeQuotaTransport
	claudeQuotaTransport = func() claudequota.Transport {
		return claudeTransportFunc(func(req *http.Request) (*http.Response, error) {
			f.mu.Lock()
			f.fetches++
			f.mu.Unlock()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    req,
			}, nil
		})
	}
	t.Cleanup(func() { claudeQuotaTransport = prev })
}

func (f *fakeClaude) counters() (reads, fetches int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads, f.fetches
}

type claudeTransportFunc func(*http.Request) (*http.Response, error)

func (f claudeTransportFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

// claudeInfoJSON is the wire shape of `quota claude info --json`.
type claudeInfoJSON struct {
	Target    string               `json:"target"`
	Status    claudeStatusJSON     `json:"status"`
	Instances []claudeInstanceJSON `json:"instances"`
}

type claudeStatusJSON struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
	Detail    string `json:"detail"`
}

type claudeInstanceJSON struct {
	Instance        string `json:"instance"`
	Enabled         bool   `json:"enabled"`
	Reason          string `json:"reason"`
	Target          string `json:"target"`
	SourceKind      string `json:"sourceKind"`
	SourceRef       string `json:"sourceRef"`
	SourceAvailable bool   `json:"sourceAvailable"`
	Candidates      []struct {
		SourceRef string `json:"sourceRef"`
		Selected  bool   `json:"selected"`
	} `json:"candidates"`
	Quota claudeQuotaJSON `json:"quota"`
}

type claudeQuotaJSON struct {
	Instance          string           `json:"instance"`
	Enabled           bool             `json:"enabled"`
	Status            claudeStatusJSON `json:"status"`
	ReadingStatus     string           `json:"readingStatus"`
	ReadingReason     string           `json:"readingReason"`
	FetchedAt         *time.Time       `json:"fetchedAt"`
	AgeSeconds        *int64           `json:"ageSeconds"`
	StaleAfterSeconds int64            `json:"staleAfterSeconds"`
	Windows           []struct {
		Kind        string   `json:"kind"`
		Present     bool     `json:"present"`
		UsedPercent *float64 `json:"used_percent"`
	} `json:"windows"`
	Forecast *struct {
		Available bool   `json:"available"`
		Reason    string `json:"reason"`
		Windows   []struct {
			Window    string `json:"window"`
			Available bool   `json:"available"`
			Source    string `json:"source"`
			Pace      struct {
				Available       bool    `json:"available"`
				PercentPerHour  float64 `json:"percent_per_hour"`
				PercentPerDay   float64 `json:"percent_per_day"`
				UsedDeltaPct    float64 `json:"used_delta_percent"`
				RemainingPct    float64 `json:"remaining_percent"`
				TimeToExhaust   int64   `json:"time_to_exhaustion"`
				ResetsBeforeExh bool    `json:"resets_before_exhaustion"`
			} `json:"pace"`
		} `json:"windows"`
		Limitations []string `json:"limitations"`
	} `json:"forecast"`
}

func decodeClaudeInfo(t *testing.T, out string) claudeInfoJSON {
	t.Helper()
	var data claudeInfoJSON
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		t.Fatalf("bad info JSON: %v\n%s", err, out)
	}
	return data
}

// claudeOne returns the single instance entry, failing when the command did not
// report exactly one.
func claudeOne(t *testing.T, data claudeInfoJSON) claudeInstanceJSON {
	t.Helper()
	if len(data.Instances) != 1 {
		t.Fatalf("want exactly one instance, got %d: %+v", len(data.Instances), data.Instances)
	}
	return data.Instances[0]
}

// TestClaudeQuotaInfoDefaultOff checks the default state with no index at all:
// info is read-only, reports the instance as not enabled, still discloses the
// exact candidate source and the fixed target, reads no credential and creates
// nothing. Missing state is never a zero reading.
func TestClaudeQuotaInfoDefaultOff(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MYTOKEN_HOME", dir)
	fake := &fakeClaude{}
	fake.install(t)

	out, errout, code := runCLI(t, "quota", "claude", "info", "--json")
	if code != 0 {
		t.Fatalf("info exit=%d stderr=%s", code, errout)
	}
	data := decodeClaudeInfo(t, out)
	if data.Target != claudequota.Target {
		t.Fatalf("target = %q, want the fixed HTTPS target %q", data.Target, claudequota.Target)
	}
	one := claudeOne(t, data)
	if one.Enabled {
		t.Fatalf("Claude quota must be off by default: %+v", one)
	}
	if one.Reason == "" {
		t.Fatalf("a disabled instance must name a reason: %+v", one)
	}
	if one.SourceRef != fake.ref || !one.SourceAvailable {
		t.Fatalf("info must disclose the exact candidate source before an enable: %+v", one)
	}
	if len(one.Candidates) != 1 || one.Candidates[0].SourceRef != fake.ref || !one.Candidates[0].Selected {
		t.Fatalf("candidates = %+v", one.Candidates)
	}
	if one.Quota.Status.Available {
		t.Fatalf("nothing is stored, so the cached reading must be unavailable: %+v", one.Quota)
	}
	if one.Quota.Status.Reason != "no_consent" && one.Quota.Status.Reason != "not_enabled" {
		t.Fatalf("cached status reason = %q", one.Quota.Status.Reason)
	}
	if len(one.Quota.Windows) != 0 {
		t.Fatalf("no stored reading may show windows: %+v", one.Quota.Windows)
	}
	// info must not have created an index, read a credential or fetched.
	if _, err := store.OpenReadOnly(t.Context(), dir+"/mytoken.db"); err == nil {
		t.Fatal("info created an index; it must be read-only and create nothing")
	}
	if reads, fetches := fake.counters(); reads != 0 || fetches != 0 {
		t.Fatalf("info read/fetched: reads=%d fetches=%d", reads, fetches)
	}
}

// TestClaudeQuotaEnableNeedsUnambiguousConsent checks that an unattended enable
// cannot be ambiguous: --yes demands both the exact source and the instance.
func TestClaudeQuotaEnableNeedsUnambiguousConsent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MYTOKEN_HOME", dir)
	fake := &fakeClaude{}
	fake.install(t)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing source and instance", []string{"quota", "claude", "enable", "--yes"}, "--instance-id"},
		{"missing source", []string{"quota", "claude", "enable", "--yes", "--instance-id", "default"}, "--source"},
		{"missing instance", []string{"quota", "claude", "enable", "--yes", "--source", fake.ref}, "--instance-id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, errout, code := runCLI(t, tc.args...)
			if code != 2 {
				t.Fatalf("exit=%d stderr=%s", code, errout)
			}
			if !strings.Contains(errout, tc.want) {
				t.Fatalf("stderr %q does not mention %q", errout, tc.want)
			}
		})
	}
	if _, err := store.OpenReadOnly(t.Context(), dir+"/mytoken.db"); err == nil {
		t.Fatal("a rejected enable wrote nothing; the index must not exist")
	}
	if _, fetches := fake.counters(); fetches != 0 {
		t.Fatal("enable must never fetch")
	}
}

// TestClaudeQuotaEnableRecordsConsentWithoutFetching checks that a confirmed
// enable persists exactly one consent for the exact source, reads no credential,
// makes no request, and becomes visible to info.
func TestClaudeQuotaEnableRecordsConsentWithoutFetching(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MYTOKEN_HOME", dir)
	fake := &fakeClaude{}
	fake.install(t)

	out, errout, code := runCLI(t, "quota", "claude", "enable", "--yes",
		"--instance-id", "default", "--source", fake.ref)
	if code != 0 {
		t.Fatalf("enable exit=%d stderr=%s", code, errout)
	}
	if !strings.Contains(out, claudequota.Target) {
		t.Fatalf("enable must disclose the fixed target before enabling: %s", out)
	}
	if !strings.Contains(out, fake.ref) {
		t.Fatalf("enable must disclose the exact source: %s", out)
	}
	if reads, fetches := fake.counters(); reads != 0 || fetches != 0 {
		t.Fatalf("enable must not read a credential or fetch: reads=%d fetches=%d", reads, fetches)
	}
	// The shared record is the service's own settings key, so the GUI service
	// reads the same consent.
	st, err := store.Open(dir + "/mytoken.db")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := st.Setting(t.Context(), claudequota.ConsentKey("default"))
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if raw == "" {
		t.Fatal("enable wrote no consent record")
	}

	out, errout, code = runCLI(t, "quota", "claude", "info", "--json")
	if code != 0 {
		t.Fatalf("info exit=%d stderr=%s", code, errout)
	}
	one := claudeOne(t, decodeClaudeInfo(t, out))
	if !one.Enabled || one.SourceRef != fake.ref || !one.SourceAvailable {
		t.Fatalf("info after enable: %+v", one)
	}
	// Enabled but never fetched: the reading is named, not invented.
	if one.Quota.Status.Available {
		t.Fatalf("no fetch happened, so the reading must be unavailable: %+v", one.Quota)
	}
	if one.Quota.Status.Reason != "no_reading" {
		t.Fatalf("reading reason = %q, want no_reading", one.Quota.Status.Reason)
	}
	if _, fetches := fake.counters(); fetches != 0 {
		t.Fatal("info must not fetch")
	}
}

// TestClaudeQuotaEnableInteractiveRequiresExactSource checks the interactive
// gate: a wrong answer aborts and changes nothing, and the exact reference
// enables.
func TestClaudeQuotaEnableInteractiveRequiresExactSource(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MYTOKEN_HOME", dir)
	fake := &fakeClaude{}
	fake.install(t)
	prevPrompt := claudeQuotaPrompt
	t.Cleanup(func() { claudeQuotaPrompt = prevPrompt })

	claudeQuotaPrompt = strings.NewReader("not-the-source\n")
	_, errout, code := runCLI(t, "quota", "claude", "enable",
		"--instance-id", "default", "--source", fake.ref)
	if code != 1 {
		t.Fatalf("wrong confirmation exit=%d stderr=%s", code, errout)
	}
	if !strings.Contains(errout, "nothing was changed") {
		t.Fatalf("stderr = %q", errout)
	}
	if _, err := store.OpenReadOnly(t.Context(), dir+"/mytoken.db"); err == nil {
		t.Fatal("an aborted enable must not create or write the index")
	}

	claudeQuotaPrompt = strings.NewReader(fake.ref + "\n")
	if _, errout, code = runCLI(t, "quota", "claude", "enable",
		"--instance-id", "default", "--source", fake.ref); code != 0 {
		t.Fatalf("exact confirmation exit=%d stderr=%s", code, errout)
	}
}

// TestClaudeQuotaRefreshFakeOnly checks the explicit refresh: it needs a
// matching consent, uses the fake transport exactly once, publishes the reading
// with its as-of age and additive forecast, and --offline rejects it without a
// request.
func TestClaudeQuotaRefreshFakeOnly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MYTOKEN_HOME", dir)
	fake := &fakeClaude{}
	fake.install(t)
	fake.transport(t, `{"five_hour":{"utilization":10,"resets_at":"2026-01-02T03:04:05Z"}}`)

	// Refresh without any consent must fail before the network and without
	// creating an index.
	_, errout, code := runCLI(t, "quota", "claude", "refresh", "--instance-id", "default")
	if code != 1 {
		t.Fatalf("refresh without consent exit=%d stderr=%s", code, errout)
	}
	if _, fetches := fake.counters(); fetches != 0 {
		t.Fatal("refresh without consent reached the transport")
	}
	if _, err := store.OpenReadOnly(t.Context(), dir+"/mytoken.db"); err == nil {
		t.Fatal("refresh without consent must not create the index")
	}

	if _, errout, code = runCLI(t, "quota", "claude", "enable", "--yes",
		"--instance-id", "default", "--source", fake.ref); code != 0 {
		t.Fatalf("enable exit=%d stderr=%s", code, errout)
	}
	out, errout, code := runCLI(t, "quota", "claude", "refresh", "--instance-id", "default")
	if code != 0 {
		t.Fatalf("refresh exit=%d stderr=%s", code, errout)
	}
	if !strings.Contains(out, claudequota.Target) {
		t.Fatalf("refresh must name the target it used: %s", out)
	}
	if reads, fetches := fake.counters(); reads != 1 || fetches != 1 {
		t.Fatalf("refresh must read once and fetch once: reads=%d fetches=%d", reads, fetches)
	}

	out, errout, code = runCLI(t, "quota", "claude", "info", "--json")
	if code != 0 {
		t.Fatalf("info exit=%d stderr=%s", code, errout)
	}
	one := claudeOne(t, decodeClaudeInfo(t, out))
	if !one.Enabled || !one.Quota.Status.Available {
		t.Fatalf("after a successful refresh the reading must be available: %+v", one.Quota)
	}
	if one.Quota.ReadingStatus != "ok" {
		t.Fatalf("reading status = %q", one.Quota.ReadingStatus)
	}
	if one.Quota.FetchedAt == nil || one.Quota.AgeSeconds == nil {
		t.Fatalf("a reading must carry its as-of timestamp and age: %+v", one.Quota)
	}
	if one.Quota.StaleAfterSeconds <= 0 {
		t.Fatalf("the reading must carry its freshness budget: %+v", one.Quota)
	}
	var used *float64
	for _, w := range one.Quota.Windows {
		if w.Kind == "five_hour" {
			used = w.UsedPercent
		}
	}
	if used == nil || *used != 10 {
		t.Fatalf("five_hour window = %+v", one.Quota.Windows)
	}
	// The forecast is additive: it appears when the reading carries samples and
	// never fabricates a pace. Whatever its verdict, it may not leak the
	// credential source reference.
	if one.Quota.Forecast != nil {
		for _, w := range one.Quota.Forecast.Windows {
			if w.Source != "" {
				t.Fatalf("forecast window leaked the credential source: %+v", w)
			}
		}
	}

	// --offline is rejected before anything network-shaped happens.
	_, errout, code = runCLI(t, "quota", "claude", "refresh", "--offline", "--instance-id", "default")
	if code != 2 {
		t.Fatalf("refresh --offline exit=%d stderr=%s", code, errout)
	}
	if !strings.Contains(errout, "offline") {
		t.Fatalf("stderr = %q", errout)
	}
	if reads, fetches := fake.counters(); reads != 1 || fetches != 1 {
		t.Fatalf("--offline must not read or fetch: reads=%d fetches=%d", reads, fetches)
	}
}

// TestClaudeQuotaDisableClearsAndStopsFetching checks that disable clears the
// consent and the cached reading, so a later refresh cannot fetch and a running
// service re-reads the cleared consent.
func TestClaudeQuotaDisableClearsAndStopsFetching(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MYTOKEN_HOME", dir)
	fake := &fakeClaude{}
	fake.install(t)
	fake.transport(t, `{"five_hour":{"utilization":10}}`)

	if _, errout, code := runCLI(t, "quota", "claude", "enable", "--yes",
		"--instance-id", "default", "--source", fake.ref); code != 0 {
		t.Fatalf("enable exit=%d stderr=%s", code, errout)
	}
	if _, errout, code := runCLI(t, "quota", "claude", "refresh", "--instance-id", "default"); code != 0 {
		t.Fatalf("refresh exit=%d stderr=%s", code, errout)
	}
	_, errout, code := runCLI(t, "quota", "claude", "disable", "--yes", "--instance-id", "default")
	if code != 0 {
		t.Fatalf("disable exit=%d stderr=%s", code, errout)
	}
	if reads, fetches := fake.counters(); reads != 1 || fetches != 1 {
		t.Fatalf("disable must not read or fetch: reads=%d fetches=%d", reads, fetches)
	}

	// Both keys are cleared: an empty row, which a running service sees as
	// revoked rather than as a missing key it could default-enable.
	st, err := store.Open(dir + "/mytoken.db")
	if err != nil {
		t.Fatal(err)
	}
	consent, err := st.Setting(t.Context(), claudequota.ConsentKey("default"))
	if err != nil {
		t.Fatal(err)
	}
	latest, err := st.Setting(t.Context(), claudequota.LatestKey("default"))
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if consent != "" || latest != "" {
		t.Fatalf("disable left state: consent=%q latest=%q", consent, latest)
	}

	out, errout, code := runCLI(t, "quota", "claude", "info", "--json")
	if code != 0 {
		t.Fatalf("info exit=%d stderr=%s", code, errout)
	}
	one := claudeOne(t, decodeClaudeInfo(t, out))
	if one.Enabled {
		t.Fatalf("a cleared consent must not report enabled: %+v", one)
	}
	if one.Quota.Enabled || len(one.Quota.Windows) != 0 {
		t.Fatalf("a cleared reading must not be shown: %+v", one.Quota)
	}

	_, errout, code = runCLI(t, "quota", "claude", "refresh", "--instance-id", "default")
	if code != 1 {
		t.Fatalf("refresh after disable exit=%d stderr=%s", code, errout)
	}
	if reads, fetches := fake.counters(); reads != 1 || fetches != 1 {
		t.Fatalf("a disabled instance must not fetch: reads=%d fetches=%d", reads, fetches)
	}
}

// TestClaudeQuotaInfoLeavesUnrelatedHistoryUntouched proves the fingerprint of
// unrelated stored history survives every read-only command, and that the
// ordinary snapshot path also stays read-only.
func TestClaudeQuotaInfoLeavesUnrelatedHistoryUntouched(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MYTOKEN_HOME", dir)
	fake := &fakeClaude{}
	fake.install(t)
	fake.transport(t, `{"five_hour":{"utilization":10}}`)

	// A stored Codex observation and an unrelated setting stand in for history
	// another worker owns.
	st, err := store.Open(dir + "/mytoken.db")
	if err != nil {
		t.Fatal(err)
	}
	if err = st.SetSetting(t.Context(), "quota.codex.default", `{"harness":"codex","instance":"default","ingested_at":"2026-01-02T03:04:05Z"}`); err != nil {
		t.Fatal(err)
	}
	if err = st.SetSetting(t.Context(), "stats.timezone", "Asia/Shanghai"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	before := claudeFingerprint(t, dir+"/mytoken.db")
	for _, args := range [][]string{
		{"quota", "claude", "info", "--json"},
		{"quota", "claude", "info"},
		{"snapshot", "--once", "--json"},
		{"statusline", "--json"},
	} {
		if _, errout, code := runCLI(t, args...); code != 0 {
			t.Fatalf("%v exit=%d stderr=%s", args, code, errout)
		}
	}
	if after := claudeFingerprint(t, dir+"/mytoken.db"); after != before {
		t.Fatalf("a read-only command changed stored history:\nbefore %s\nafter  %s", before, after)
	}
	if _, fetches := fake.counters(); fetches != 0 {
		t.Fatal("a cached display must not fetch")
	}

	// An enable may write its own consent but must leave the Codex observation
	// and the unrelated setting byte-for-byte alone.
	if _, errout, code := runCLI(t, "quota", "claude", "enable", "--yes",
		"--instance-id", "default", "--source", fake.ref); code != 0 {
		t.Fatalf("enable exit=%d stderr=%s", code, errout)
	}
	after := claudeFingerprint(t, dir+"/mytoken.db")
	if !strings.Contains(after, "quota.codex.default=") || !strings.Contains(after, `stats.timezone=Asia/Shanghai`) {
		t.Fatalf("enable disturbed unrelated history:\n%s", after)
	}
	if !strings.Contains(after, claudequota.ConsentKey("default")+"=") {
		t.Fatalf("enable did not record the consent:\n%s", after)
	}
}

// claudeFingerprint renders every stored setting key and value plus the event
// count, so a test can prove a command left history untouched.
func claudeFingerprint(t *testing.T, path string) string {
	t.Helper()
	ro, err := store.OpenReadOnly(t.Context(), path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()
	rows, err := ro.DB().QueryContext(t.Context(), "SELECT key, value FROM settings ORDER BY key")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var k, v string
		if err = rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(v)
		b.WriteString("\n")
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	var events int64
	if err = ro.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM events").Scan(&events); err != nil {
		t.Fatal(err)
	}
	b.WriteString("events=")
	b.WriteString(strconv.FormatInt(events, 10))
	return b.String()
}

// TestClaudeQuotaCachedDisplayShowsReadingButNoCredentialPath checks the
// snapshot and statusline include the cached Claude reading, its as-of age and
// the additive forecast, and never the credential source path.
func TestClaudeQuotaCachedDisplayShowsReadingButNoCredentialPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MYTOKEN_HOME", dir)
	fake := &fakeClaude{}
	fake.install(t)
	fake.transport(t, `{"five_hour":{"utilization":10},"seven_day":{"utilization":40}}`)

	if _, errout, code := runCLI(t, "quota", "claude", "enable", "--yes",
		"--instance-id", "default", "--source", fake.ref); code != 0 {
		t.Fatalf("enable exit=%d stderr=%s", code, errout)
	}
	if _, errout, code := runCLI(t, "quota", "claude", "refresh", "--instance-id", "default"); code != 0 {
		t.Fatalf("refresh exit=%d stderr=%s", code, errout)
	}

	out, errout, code := runCLI(t, "snapshot", "--once")
	if code != 0 {
		t.Fatalf("snapshot exit=%d stderr=%s", code, errout)
	}
	if !strings.Contains(out, "claudeQuota=") {
		t.Fatalf("snapshot must report the claudeQuota section status:\n%s", out)
	}
	if !strings.Contains(out, "claudeQuota 1 instance(s)") {
		t.Fatalf("snapshot must list the cached Claude instance:\n%s", out)
	}
	if !strings.Contains(out, "five_hour") || !strings.Contains(out, "seven_day") {
		t.Fatalf("snapshot must show the stored windows:\n%s", out)
	}
	if strings.Contains(out, fake.ref) || strings.Contains(out, "/.claude/.credentials.json") {
		t.Fatalf("a plaintext snapshot must not carry a credential path:\n%s", out)
	}

	out, errout, code = runCLI(t, "snapshot", "--once", "--json")
	if code != 0 {
		t.Fatalf("snapshot --json exit=%d stderr=%s", code, errout)
	}
	if strings.Contains(out, fake.ref) {
		t.Fatalf("snapshot JSON must not carry the credential source:\n%s", out)
	}
	var snap struct {
		Sources struct {
			ClaudeQuota struct {
				Available bool `json:"available"`
			} `json:"claudeQuota"`
		} `json:"sources"`
		Data struct {
			ClaudeQuota []struct {
				Instance   string `json:"instance"`
				Enabled    bool   `json:"enabled"`
				AgeSeconds *int64 `json:"ageSeconds"`
				Windows    []struct {
					Kind        string   `json:"kind"`
					UsedPercent *float64 `json:"used_percent"`
				} `json:"windows"`
			} `json:"claudeQuota"`
			TokRate *struct {
				AgeSeconds float64 `json:"ageSeconds"`
			} `json:"tokRate"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatalf("bad snapshot JSON: %v\n%s", err, out)
	}
	if len(snap.Data.ClaudeQuota) != 1 {
		t.Fatalf("snapshot must carry one cached Claude instance: %+v", snap.Data.ClaudeQuota)
	}
	entry := snap.Data.ClaudeQuota[0]
	if !entry.Enabled || entry.AgeSeconds == nil {
		t.Fatalf("cached entry = %+v", entry)
	}

	out, errout, code = runCLI(t, "statusline", "--json")
	if code != 0 {
		t.Fatalf("statusline exit=%d stderr=%s", code, errout)
	}
	if strings.Contains(out, fake.ref) {
		t.Fatalf("statusline JSON must not carry the credential source:\n%s", out)
	}
	out, errout, code = runCLI(t, "statusline")
	if code != 0 {
		t.Fatalf("statusline text exit=%d stderr=%s", code, errout)
	}
	if !strings.Contains(out, "claude") {
		t.Fatalf("statusline must mention the cached Claude quota:\n%s", out)
	}
	if strings.Contains(out, fake.ref) {
		t.Fatalf("statusline must not print the credential path:\n%s", out)
	}
}

// TestClaudeQuotaAsOfRendering checks that the live-ingestion rate is rendered
// as an as-of measurement with its age, never as a live moving-window aggregate
// at the instant of the query, and that the statusline stays compact while
// still saying "as-of" for the cached Claude reading.
func TestClaudeQuotaAsOfRendering(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	st := snapshot.Status{Available: true}
	tr := &snapshot.TokRate{Tokens: 600, Rate: 10, WindowSeconds: 60, At: at, AgeSeconds: 42}

	if got := statusTokRateText(st, tr); !strings.Contains(got, "10.00 tok/s over 60s") || !strings.Contains(got, "as-of 42s") {
		t.Fatalf("statusTokRateText = %q", got)
	}
	var buf bytes.Buffer
	if err := writeTokRate(&buf, &snapshot.Snapshot{
		Sources: snapshot.Sources{TokRate: st},
		Data:    snapshot.Data{TokRate: tr},
	}); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "at=2026-01-02T03:04:05Z") || !strings.Contains(got, "age=42s") {
		t.Fatalf("writeTokRate = %q", got)
	}
	if !strings.Contains(got, "as-of") {
		t.Fatalf("writeTokRate must mark the number as-of: %q", got)
	}

	entry := snapshot.ClaudeQuota{
		Instance:   "default",
		Enabled:    true,
		Status:     snapshot.Status{Available: true},
		FetchedAt:  &at,
		AgeSeconds: ptrInt64(42),
		Windows: []claudequota.Window{{
			Kind: "five_hour", Present: true, UsedPercent: ptrFloat64(10),
		}},
	}
	line := statusClaudeQuotaText(snapshot.Status{Available: true}, []snapshot.ClaudeQuota{entry})
	if !strings.Contains(line, "claude default") || !strings.Contains(line, "five_hour=10.0%") || !strings.Contains(line, "as-of 42s") {
		t.Fatalf("statusClaudeQuotaText = %q", line)
	}
	if line := statusClaudeQuotaText(snapshot.Status{}, nil); line != "" {
		t.Fatalf("no stored Claude state must stay silent, got %q", line)
	}
}

func ptrInt64(v int64) *int64       { return &v }
func ptrFloat64(v float64) *float64 { return &v }

// TestClaudeQuotaHelpDocumentsTheGate checks the help text carries the syntax
// and the consent explanation the CLI promises.
func TestClaudeQuotaHelpDocumentsTheGate(t *testing.T) {
	out, _, code := runCLI(t, "quota", "claude", "help")
	if code != 0 {
		t.Fatalf("help exit=%d", code)
	}
	for _, want := range []string{
		"quota claude info", "quota claude enable", "quota claude disable",
		"quota claude refresh", "--yes", "--source", claudequota.Target,
		"store.OpenReadOnly", "OFF by default",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("help does not document %q:\n%s", want, out)
		}
	}
	// The parent usage points at the subcommand help.
	out, _, code = runCLI(t, "quota", "help")
	if code != 0 || !strings.Contains(out, "quota claude") {
		t.Fatalf("quota help exit=%d:\n%s", code, out)
	}
}
