package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

// snapSeed creates a temp MYTOKEN_HOME, seeds the index through a writable
// store, then seals it (rollback journal, no WAL sidecars) so every command
// under test reads it through store.OpenReadOnly exactly as production does.
func snapSeed(t *testing.T, settings map[string]string, writes ...store.Write) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MYTOKEN_HOME", dir)
	path := filepath.Join(dir, "mytoken.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := st.CommitMany(t.Context(), writes); err != nil {
		t.Fatal(err)
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
	return dir
}

// snapWrite is one instance's event plus its session row, so both the snapshot
// and the statusline can resolve it.
func snapWrite(instanceID, dedup, session string, cost float64) store.Write {
	return snapWriteP(instanceID, dedup, session, "p", cost)
}

// snapWriteP is snapWrite with a chosen pricing provider, for filter
// intersection tests.
func snapWriteP(instanceID, dedup, session, provider string, cost float64) store.Write {
	at := time.Now().Add(-time.Minute)
	e := model.UsageEvent{
		Harness: model.Codex, DedupKey: dedup, SessionID: session, Model: "gpt-5",
		Provider: provider, BaseURL: "https://api.example.com", Timestamp: at,
		Tokens: model.Tokens{Input: 20, Output: 4, CacheRead: 5},
	}
	if cost > 0 {
		v := cost
		e.CostUSD = &v
	}
	return store.Write{
		Harness: model.Codex, InstanceID: instanceID, Path: "fixture/" + instanceID,
		Batch: harness.Batch{
			Events:   []model.UsageEvent{e},
			Sessions: []model.SessionMeta{{Harness: model.Codex, SessionID: session, Title: session, StartedAt: at, UpdatedAt: at}},
			Next:     harness.Cursor{Offset: 1},
		},
	}
}

// snapFingerprint is "name:size:sha256" for every file in dir: the proof that a
// read-only command created, removed or rewrote nothing.
func snapFingerprint(t *testing.T, dir string) string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		out = append(out, fmt.Sprintf("%s:%d:%s", e.Name(), len(raw), hex.EncodeToString(sum[:])))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func snapDecode(t *testing.T, out string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("invalid JSON %q: %v", out, err)
	}
	return doc
}

func snapAt(doc map[string]any, path ...string) any {
	var cur any = doc
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[key]
	}
	return cur
}

// ---------------------------------------------------------------------------
// snapshot
// ---------------------------------------------------------------------------

func TestSnapshotCommandNeedsOnceAndNeverWrites(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MYTOKEN_HOME", dir)

	if _, errout, code := runCLI(t, "snapshot"); code != 2 || !strings.Contains(errout, "--once") {
		t.Fatalf("snapshot without --once: code=%d stderr=%s", code, errout)
	}
	if _, _, code := runCLI(t, "snapshot", "--once", "extra"); code != 2 {
		t.Fatal("snapshot with a positional argument must be a usage error")
	}

	before := snapFingerprint(t, dir)
	out, errout, code := runCLI(t, "snapshot", "--once", "--json")
	if code != 0 {
		t.Fatalf("snapshot --once --json: code=%d stderr=%s", code, errout)
	}
	doc := snapDecode(t, out)
	if doc["snapshotVersion"] != float64(1) {
		t.Fatalf("snapshotVersion = %v, want 1", doc["snapshotVersion"])
	}
	if snapAt(doc, "sources", "index", "available") != false {
		t.Fatalf("sources.index = %v, want unavailable", snapAt(doc, "sources", "index"))
	}
	if got := snapAt(doc, "sources", "index", "reason"); got != "missing_database" {
		t.Fatalf("sources.index.reason = %v, want missing_database", got)
	}
	if snapAt(doc, "data", "totals") != nil || snapAt(doc, "data", "cache") != nil {
		t.Fatalf("data = %v, want null totals/cache for a missing index", snapAt(doc, "data"))
	}
	if got := snapAt(doc, "writes", "performed"); len(got.([]any)) != 0 {
		t.Fatalf("writes.performed = %v, want empty", got)
	}
	if got := snapFingerprint(t, dir); got != before {
		t.Fatalf("snapshot created files:\nbefore %s\nafter  %s", before, got)
	}

	text, _, code := runCLI(t, "snapshot", "--once")
	if code != 0 {
		t.Fatalf("snapshot text: code=%d", code)
	}
	for _, want := range []string{"snapshot v1", "unavailable (missing_database)", "performed=[]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("snapshot text missing %q:\n%s", want, text)
		}
	}

	over, _, code := runCLI(t, "snapshot", "--once", "--json", "--timezone", "UTC")
	if code != 0 {
		t.Fatalf("snapshot --timezone UTC: code=%d", code)
	}
	overDoc := snapDecode(t, over)
	if snapAt(overDoc, "freshness", "timezoneSource") != "override" || snapAt(overDoc, "freshness", "effectiveTimezone") != "UTC" {
		t.Fatalf("freshness timezone = %v/%v", snapAt(overDoc, "freshness", "effectiveTimezone"), snapAt(overDoc, "freshness", "timezoneSource"))
	}
}

func TestSnapshotAndStatuslineHonorInstanceAndProviderFilters(t *testing.T) {
	// Instance b used a different provider, so the two filters can be crossed:
	// only the intersection may count.
	dir := snapSeed(t, nil, snapWriteP("a", "k1", "s1", "p", 0.5), snapWriteP("b", "k2", "s2", "q", 0.25))
	before := snapFingerprint(t, dir)

	for _, tc := range []struct {
		instance, provider string
		want               float64
	}{
		{"a", "p", 1},
		{"b", "p", 0},
		{"b", "q", 1},
	} {
		out, _, code := runCLI(t, "snapshot", "--once", "--json", "--instance-id", tc.instance, "--provider", tc.provider)
		if code != 0 {
			t.Fatalf("snapshot --instance-id %s --provider %s: code=%d", tc.instance, tc.provider, code)
		}
		doc := snapDecode(t, out)
		if snapAt(doc, "sources", "index", "available") != true {
			t.Fatalf("index unavailable for %s/%s: %v", tc.instance, tc.provider, snapAt(doc, "sources", "index"))
		}
		if got := snapAt(doc, "data", "totals", "requests"); got != tc.want {
			t.Fatalf("requests for %s/%s = %v, want %v", tc.instance, tc.provider, got, tc.want)
		}
	}

	out, _, code := runCLI(t, "snapshot", "--once", "--json", "--instance-id", "a")
	if code != 0 {
		t.Fatalf("snapshot --instance-id a: code=%d", code)
	}
	doc := snapDecode(t, out)
	if got := snapAt(doc, "data", "totals", "requests"); got != float64(1) {
		t.Fatalf("requests = %v, want 1", got)
	}
	if snapAt(doc, "sources", "index", "available") != true {
		t.Fatalf("index = %v", snapAt(doc, "sources", "index"))
	}

	// A provider that matches nothing is a real zero, not an unavailable index.
	out, _, code = runCLI(t, "snapshot", "--once", "--json", "--instance-id", "a", "--provider", "nope")
	if code != 0 {
		t.Fatalf("snapshot with an excluding provider: code=%d", code)
	}
	doc = snapDecode(t, out)
	if snapAt(doc, "sources", "index", "available") != true {
		t.Fatal("an excluding filter must not make the index unavailable")
	}
	if got := snapAt(doc, "data", "totals", "requests"); got != float64(0) {
		t.Fatalf("requests = %v, want 0", got)
	}

	// Statusline: the explicit session in instance a resolves; an unknown one is
	// reported unavailable rather than silently swapped for another session.
	out, _, code = runCLI(t, "statusline", "--json", "--harness", "codex", "--instance-id", "a", "--session", "s1")
	if code != 0 {
		t.Fatalf("statusline: code=%d", code)
	}
	doc = snapDecode(t, out)
	if got := snapAt(doc, "session", "sessionId"); got != "s1" {
		t.Fatalf("session.sessionId = %v (%v)", got, snapAt(doc, "sessionStatus"))
	}
	if snapAt(doc, "session", "costSource") != "log" {
		t.Fatalf("costSource = %v, want log", snapAt(doc, "session", "costSource"))
	}
	out, _, code = runCLI(t, "statusline", "--json", "--harness", "codex", "--instance-id", "a", "--session", "nope")
	if code != 0 {
		t.Fatalf("statusline unknown session: code=%d", code)
	}
	doc = snapDecode(t, out)
	if snapAt(doc, "session") != nil || snapAt(doc, "sessionStatus", "reason") != "session_unavailable" {
		t.Fatalf("unknown session = %v/%v", snapAt(doc, "session"), snapAt(doc, "sessionStatus"))
	}
	if got := snapFingerprint(t, dir); got != before {
		t.Fatalf("a read-only command rewrote the index:\nbefore %s\nafter  %s", before, got)
	}
}

func TestStatuslineUnavailableIsNotZero(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MYTOKEN_HOME", dir)

	out, errout, code := runCLI(t, "statusline", "--json")
	if code != 0 {
		t.Fatalf("statusline --json: code=%d stderr=%s", code, errout)
	}
	doc := snapDecode(t, out)
	if snapAt(doc, "index", "available") != false || snapAt(doc, "index", "reason") != "missing_database" {
		t.Fatalf("index = %v", snapAt(doc, "index"))
	}
	if snapAt(doc, "session") != nil {
		t.Fatal("statusline invented a session")
	}
	if snapAt(doc, "balance", "remainingUsd") != nil || snapAt(doc, "balance", "usedUsd") != nil {
		t.Fatalf("balance = %v, want null amounts", snapAt(doc, "balance"))
	}
	text, _, code := runCLI(t, "statusline")
	if code != 0 || !strings.HasPrefix(text, "index unavailable") {
		t.Fatalf("statusline text = %q (code=%d)", text, code)
	}
	if _, _, code := runCLI(t, "statusline", "--harness", "not-a-harness"); code != 2 {
		t.Fatal("an unknown --harness must be a usage error")
	}
}

func TestStatuslineReportsPersistedTimezone(t *testing.T) {
	snapSeed(t, map[string]string{"stats_timezone": "Asia/Tokyo"}, snapWrite("a", "k1", "s1", 0.5))
	out, _, code := runCLI(t, "statusline", "--json")
	if code != 0 {
		t.Fatalf("statusline: code=%d", code)
	}
	doc := snapDecode(t, out)
	if snapAt(doc, "timezone") != "Asia/Tokyo" || snapAt(doc, "timezoneSource") != "stats_timezone" {
		t.Fatalf("timezone = %v/%v", snapAt(doc, "timezone"), snapAt(doc, "timezoneSource"))
	}
	over, _, code := runCLI(t, "statusline", "--json", "--timezone", "UTC")
	if code != 0 {
		t.Fatalf("statusline --timezone: code=%d", code)
	}
	overDoc := snapDecode(t, over)
	if snapAt(overDoc, "timezone") != "UTC" || snapAt(overDoc, "timezoneSource") != "override" {
		t.Fatalf("override timezone = %v/%v", snapAt(overDoc, "timezone"), snapAt(overDoc, "timezoneSource"))
	}

	// The same resolved zone drives snapshot and report, so all three commands
	// agree on the window they measured.
	snapOut, _, code := runCLI(t, "snapshot", "--once", "--json")
	if code != 0 {
		t.Fatalf("snapshot: code=%d", code)
	}
	snapDoc := snapDecode(t, snapOut)
	if snapAt(snapDoc, "freshness", "effectiveTimezone") != "Asia/Tokyo" || snapAt(snapDoc, "freshness", "timezoneSource") != "stats_timezone" {
		t.Fatalf("snapshot zone = %v/%v", snapAt(snapDoc, "freshness", "effectiveTimezone"), snapAt(snapDoc, "freshness", "timezoneSource"))
	}
	repOut, _, code := runCLI(t, "report", "day", "--json")
	if code != 0 {
		t.Fatalf("report: code=%d", code)
	}
	var rep reportCLIJSON
	if err := json.Unmarshal([]byte(repOut), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Timezone != "Asia/Tokyo" || rep.TimezoneSource != "stats_timezone" || rep.Current.Totals.Requests != 1 {
		t.Fatalf("report = %s/%s requests=%d", rep.Timezone, rep.TimezoneSource, rep.Current.Totals.Requests)
	}
}

// TestUnresolvedPersistedTimezoneIsReportedNotAdopted: the legacy "Local" value
// follows the operating system, so it is not a fixed calendar. A read-only
// command must say so instead of adopting the process zone, must leave the
// stored value alone, and must write nothing.
func TestUnresolvedPersistedTimezoneIsReportedNotAdopted(t *testing.T) {
	dir := snapSeed(t, map[string]string{"stats_timezone": "Local"}, snapWrite("a", "k1", "s1", 0.5))
	before := snapFingerprint(t, dir)

	out, _, code := runCLI(t, "snapshot", "--once", "--json")
	if code != 0 {
		t.Fatalf("snapshot: code=%d", code)
	}
	doc := snapDecode(t, out)
	if snapAt(doc, "freshness", "needsTimezoneSelection") != true {
		t.Fatalf("freshness = %v, want needsTimezoneSelection", snapAt(doc, "freshness"))
	}
	if snapAt(doc, "freshness", "effectiveTimezone") != "unconfigured" || snapAt(doc, "freshness", "timezoneSource") != "needs_selection" {
		t.Fatalf("freshness timezone = %v/%v, want unconfigured/needs_selection", snapAt(doc, "freshness", "effectiveTimezone"), snapAt(doc, "freshness", "timezoneSource"))
	}
	if preview := snapAt(doc, "freshness", "systemZonePreview"); preview == nil || preview == "" || preview == "Local" {
		t.Fatalf("systemZonePreview = %v, want the process zone marked as a preview", preview)
	}
	if got := snapAt(doc, "writes", "performed"); len(got.([]any)) != 0 {
		t.Fatalf("writes.performed = %v, want empty", got)
	}

	// The statusline and the report state the same thing rather than naming a
	// calendar the app never persisted.
	slOut, _, code := runCLI(t, "statusline", "--json")
	if code != 0 {
		t.Fatalf("statusline: code=%d", code)
	}
	slDoc := snapDecode(t, slOut)
	if slDoc["timezone"] != "unconfigured" || slDoc["timezoneSource"] != "needs_selection" {
		t.Fatalf("statusline timezone = %v/%v", slDoc["timezone"], slDoc["timezoneSource"])
	}
	_, errOut, code := runCLI(t, "report", "day")
	if code != 0 {
		t.Fatalf("report: code=%d (%s)", code, errOut)
	}
	if !strings.Contains(errOut, "unconfigured") {
		t.Fatalf("report stderr = %q, want the unresolved calendar named", errOut)
	}

	// An explicit override still works on the same database: it is a
	// one-invocation choice and is never persisted.
	over, _, code := runCLI(t, "snapshot", "--once", "--json", "--timezone", "Local")
	if code != 0 {
		t.Fatalf("snapshot --timezone Local: code=%d", code)
	}
	overDoc := snapDecode(t, over)
	if snapAt(overDoc, "freshness", "timezoneSource") != "override" {
		t.Fatalf("override source = %v, want override", snapAt(overDoc, "freshness", "timezoneSource"))
	}
	if z := snapAt(overDoc, "freshness", "effectiveTimezone"); z == nil || z == "" || z == "unconfigured" {
		t.Fatalf("override zone = %v, want the process zone", z)
	}
	if snapAt(overDoc, "freshness", "needsTimezoneSelection") != nil {
		t.Fatalf("override freshness = %v, want no unresolved marker", snapAt(overDoc, "freshness"))
	}

	if got := snapFingerprint(t, dir); got != before {
		t.Fatalf("a read-only command changed the index:\nbefore %s\nafter  %s", before, got)
	}
	ro, err := store.OpenReadOnly(t.Context(), filepath.Join(dir, "mytoken.db"))
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()
	if stored, err := ro.Setting(t.Context(), "stats_timezone"); err != nil || stored != "Local" {
		t.Fatalf("stored stats_timezone = %q (%v), want the value left untouched", stored, err)
	}
}

// ---------------------------------------------------------------------------
// report
// ---------------------------------------------------------------------------

type reportCLIJSON struct {
	Timezone       string `json:"timezone"`
	TimezoneSource string `json:"timezoneSource"`
	Current        struct {
		Kind   string `json:"kind"`
		ID     string `json:"id"`
		Totals struct {
			Requests int64   `json:"requests"`
			CostUSD  float64 `json:"costUsd"`
		} `json:"totals"`
	} `json:"current"`
	Comparison struct {
		Totals struct {
			Requests int64 `json:"requests"`
		} `json:"totals"`
	} `json:"comparison"`
	Change struct {
		CostPct     *float64 `json:"costPct"`
		RequestsPct *float64 `json:"requestsPct"`
	} `json:"change"`
}

func TestReportCommandDispatchAndTempData(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MYTOKEN_HOME", dir)
	if _, _, code := runCLI(t, "report", "bogus"); code != 2 {
		t.Fatal("an unknown period kind must be a usage error")
	}
	_, errout, code := runCLI(t, "report", "day", "--json")
	if code != 1 || !strings.Contains(errout, "not readable read-only") {
		t.Fatalf("report on a missing index: code=%d stderr=%s", code, errout)
	}
	if _, _, code := runCLI(t, "report", "day", "extra"); code != 2 {
		t.Fatal("a second positional argument must be a usage error")
	}

	seeded := snapSeed(t, nil, snapWrite("a", "k1", "s1", 0.5))
	before := snapFingerprint(t, seeded)
	out, errout, code := runCLI(t, "report", "day", "--json")
	if code != 0 {
		t.Fatalf("report day --json: code=%d stderr=%s", code, errout)
	}
	var rep reportCLIJSON
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("invalid JSON %q: %v", out, err)
	}
	if rep.Current.Kind != "day" || rep.Current.Totals.Requests != 1 {
		t.Fatalf("current = %+v, want one request today", rep.Current)
	}
	if rep.Current.Totals.CostUSD != 0.5 {
		t.Fatalf("cost = %v, want 0.5", rep.Current.Totals.CostUSD)
	}
	if rep.Current.ID == "" || rep.Timezone == "" {
		t.Fatalf("report lacks a period id or timezone: %+v", rep)
	}
	if rep.Change.CostPct != nil {
		t.Fatalf("costPct = %v, want null with an empty comparison period", *rep.Change.CostPct)
	}
	if _, _, code := runCLI(t, "report", "week", "--format", "csv"); code != 0 {
		t.Fatalf("report week --format csv: code=%d", code)
	}
	if _, _, code := runCLI(t, "report", "month", "--timezone", "UTC"); code != 0 {
		t.Fatalf("report month --timezone UTC: code=%d", code)
	}
	if got := snapFingerprint(t, seeded); got != before {
		t.Fatalf("report rewrote the index:\nbefore %s\nafter  %s", before, got)
	}
}

// ---------------------------------------------------------------------------
// doctor
// ---------------------------------------------------------------------------

func TestDoctorIsReadOnlyAndReportsMissingIndex(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MYTOKEN_HOME", dir)
	before := snapFingerprint(t, dir)

	out, errout, code := runCLI(t, "doctor", "--json")
	if code != 0 {
		t.Fatalf("doctor on a missing index: code=%d stderr=%s", code, errout)
	}
	doc := snapDecode(t, out)
	if doc["readOnly"] != true {
		t.Fatalf("readOnly = %v, want true", doc["readOnly"])
	}
	if doc["dbUnavailable"] != true || doc["dbReason"] != "missing_database" {
		t.Fatalf("db = %v/%v", doc["dbUnavailable"], doc["dbReason"])
	}
	if snapAt(doc, "schema", "state") != "unreadable" {
		t.Fatalf("schema = %v", snapAt(doc, "schema"))
	}
	// Legacy keys must survive the fix.
	for _, key := range []string{"dataDir", "dbPath", "dbBytes", "pricingSource", "harnesses", "providerSources", "configPath", "configPresent", "instances"} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("doctor JSON lost the legacy key %q", key)
		}
	}
	if got := snapFingerprint(t, dir); got != before {
		t.Fatalf("doctor created the index:\nbefore %s\nafter  %s", before, got)
	}
}

func TestDoctorSeededIndexStaysReadOnly(t *testing.T) {
	dir := snapSeed(t, nil, snapWrite("a", "k1", "s1", 0.5))
	before := snapFingerprint(t, dir)

	out, errout, code := runCLI(t, "doctor", "--json")
	if code != 0 {
		t.Fatalf("doctor: code=%d stderr=%s", code, errout)
	}
	doc := snapDecode(t, out)
	if doc["dbUnavailable"] != false || snapAt(doc, "schema", "state") != "ok" {
		t.Fatalf("db = %v schema = %v", doc["dbUnavailable"], snapAt(doc, "schema"))
	}
	if doc["dbBytes"].(float64) <= 0 {
		t.Fatalf("dbBytes = %v", doc["dbBytes"])
	}
	found := false
	for _, row := range doc["harnesses"].([]any) {
		m := row.(map[string]any)
		if m["harness"] == "codex" {
			found = true
			if m["events"] != float64(1) {
				t.Fatalf("codex events = %v, want 1", m["events"])
			}
		}
	}
	if !found {
		t.Fatal("doctor JSON has no codex harness row")
	}
	if got := snapFingerprint(t, dir); got != before {
		t.Fatalf("doctor rewrote the index:\nbefore %s\nafter  %s", before, got)
	}
}
