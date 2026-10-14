package cli

import (
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/quota"
)

// quotaObservation is the exact JSON shape scan persists: one bounded
// quota.Latest envelope per instance under quota.codex.<instance>.
func quotaObservation(t *testing.T, instance string, rep *quota.Report) string {
	t.Helper()
	raw, err := json.Marshal(quota.Latest{
		Harness:    quota.HarnessCodex,
		Instance:   instance,
		Source:     "fixture/" + instance,
		IngestedAt: time.Now().Add(-time.Minute).UTC(),
		Report:     rep,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// quotaSeed writes one stored observation per instance through a writable store
// and then seals it, so the command reads it through store.OpenReadOnly exactly
// as it does in production. raw overrides a key's value verbatim, for corrupt
// and report-less stored rows.
func quotaSeed(t *testing.T, reports map[string]*quota.Report, raw map[string]string) string {
	t.Helper()
	settings := map[string]string{}
	for instance, rep := range reports {
		settings[quota.SettingKey(instance)] = quotaObservation(t, instance, rep)
	}
	for instance, value := range raw {
		settings[quota.SettingKey(instance)] = value
	}
	return snapSeed(t, settings)
}

// quotaReportFixture is a usable observation with both windows, credits, plan
// and an explicit reset instant.
func quotaReportFixture() *quota.Report {
	observed := time.Now().Add(-3 * time.Minute).UTC()
	reset := observed.Add(90 * time.Minute)
	used, remaining := 42.5, 57.5
	windowMinutes, resetsIn := int64(300), int64(5400)
	plan, balance := "plus", "12.50"
	hasCredits, unlimited := true, false
	secondaryReset := observed.Add(20 * time.Minute)
	secondaryUsed := 7.5
	return &quota.Report{
		Source:        "fixture/inst-a",
		ObservedAt:    &observed,
		Line:          7,
		RecordType:    "rate_limits",
		LimitID:       "codex",
		LimitName:     "Codex",
		OtherLimitIDs: []string{"codex_other"},
		PlanType:      &plan,
		Credits:       &quota.Credits{HasCredits: &hasCredits, Unlimited: &unlimited, Balance: &balance},
		Primary: &quota.Window{
			Kind: quota.WindowPrimary, Status: quota.StatusOK,
			UsedPercent: &used, RemainingPercent: &remaining,
			WindowMinutes: &windowMinutes, ResetsInSeconds: &resetsIn, ResetsAt: &reset,
		},
		Secondary: &quota.Window{
			Kind: quota.WindowSecondary, Status: quota.StatusOK,
			UsedPercent: &secondaryUsed, ResetsAt: &secondaryReset,
		},
		Status:    quota.StatusOK,
		LinesRead: 3,
		BytesRead: 2048,
	}
}

func quotaInstance(t *testing.T, doc map[string]any, instance string) map[string]any {
	t.Helper()
	rows, ok := doc["instances"].([]any)
	if !ok {
		t.Fatalf("instances = %v, want an array", doc["instances"])
	}
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if row["instance"] == instance {
			return row
		}
	}
	t.Fatalf("instance %q not reported; instances = %v", instance, doc["instances"])
	return nil
}

// quotaAssertNoTokenDerived rejects any token-usage field in a quota payload:
// a quota number comes from the stored observation alone. The refusal text may
// mention tokens, but no number may be derived from them.
func quotaAssertNoTokenDerived(t *testing.T, out, label string) {
	t.Helper()
	for _, key := range []string{`"tokens"`, `"input"`, `"output"`, `"cacheRead"`, `"requests"`} {
		if strings.Contains(out, key) {
			t.Fatalf("%s emitted the token-derived field %s:\n%s", label, key, out)
		}
	}
}

// TestQuotaListReadsStoredObservationsOffline proves the whole offline path: the
// stored keys are enumerated from the index, every field comes from the stored
// observation, instances stay separate, and the read creates and rewrites
// nothing.
func TestQuotaListReadsStoredObservationsOffline(t *testing.T) {
	staleUsed := 80.0
	stale := &quota.Report{
		Source:           "fixture/inst-b",
		Status:           quota.StatusStale,
		Reason:           quota.ReasonNewerInvalid,
		Line:             9,
		Primary:          &quota.Window{Kind: quota.WindowPrimary, Status: quota.StatusStale, Reason: quota.ReasonNewerInvalid, UsedPercent: &staleUsed},
		LinesRead:        2,
		SupersededByLine: 12,
	}
	dir := quotaSeed(t, map[string]*quota.Report{"inst-a": quotaReportFixture(), "inst-b": stale}, nil)
	before := snapFingerprint(t, dir)

	out, errout, code := runCLI(t, "quota", "list", "--json")
	if code != 0 {
		t.Fatalf("quota list --json: code=%d stderr=%s", code, errout)
	}
	// Nothing may be inferred from token counts: the observation is the only
	// source of a quota number.
	quotaAssertNoTokenDerived(t, out, "quota list")
	doc := snapDecode(t, out)
	if avail, _ := snapAt(doc, "status", "available").(bool); !avail {
		t.Fatalf("section = %v, want available", doc["status"])
	}
	if rows, _ := doc["instances"].([]any); len(rows) != 2 {
		t.Fatalf("instances = %v, want the two stored instances and nothing combined", doc["instances"])
	}

	a := quotaInstance(t, doc, "inst-a")
	if a["available"] != true || a["reportStatus"] != "ok" {
		t.Fatalf("inst-a = %v, want available ok", a)
	}
	if a["key"] != quota.SettingKey("inst-a") {
		t.Fatalf("inst-a key = %v, want %q", a["key"], quota.SettingKey("inst-a"))
	}
	if a["observedAt"] == nil || a["ingestedAt"] == nil {
		t.Fatalf("inst-a = %v, want observedAt and ingestedAt", a)
	}
	if got := snapAt(a, "primary", "usedPercent"); got != 42.5 {
		t.Fatalf("primary.usedPercent = %v, want 42.5", got)
	}
	if got := snapAt(a, "primary", "remainingPercent"); got != 57.5 {
		t.Fatalf("primary.remainingPercent = %v, want 57.5", got)
	}
	if got := snapAt(a, "primary", "windowMinutes"); got != float64(300) {
		t.Fatalf("primary.windowMinutes = %v, want 300", got)
	}
	if got := snapAt(a, "primary", "resetsInSeconds"); got != float64(5400) {
		t.Fatalf("primary.resetsInSeconds = %v, want 5400", got)
	}
	if snapAt(a, "primary", "resetsAt") == nil {
		t.Fatalf("primary = %v, want the stored reset instant", a["primary"])
	}
	if got := snapAt(a, "secondary", "usedPercent"); got != 7.5 {
		t.Fatalf("secondary.usedPercent = %v, want 7.5", got)
	}
	if got := a["planType"]; got != "plus" {
		t.Fatalf("planType = %v, want plus", got)
	}
	// The balance keeps the provider's own unit and is never converted or summed.
	if got := snapAt(a, "credits", "balance"); got != "12.50" {
		t.Fatalf("credits.balance = %v, want the stored string 12.50", got)
	}
	if got := snapAt(a, "credits", "unlimited"); got != false {
		t.Fatalf("credits.unlimited = %v, want the stored false", got)
	}
	if got := snapAt(a, "forecast", "reason"); got != "single_observation" {
		t.Fatalf("forecast.reason = %v, want single_observation", got)
	}

	b := quotaInstance(t, doc, "inst-b")
	// A stale observation stays stale: it is usable, never upgraded to ok, and
	// its own reason travels verbatim.
	if b["available"] != true || b["reportStatus"] != "stale" || b["reportReason"] != "newer_invalid" {
		t.Fatalf("inst-b = %v, want available stale newer_invalid", b)
	}
	if got := snapAt(b, "primary", "status"); got != "stale" {
		t.Fatalf("inst-b primary.status = %v, want stale", got)
	}
	if b["credits"] != nil || b["secondary"] != nil {
		t.Fatalf("inst-b = %v, want no credits and no secondary window", b)
	}

	if got := snapFingerprint(t, dir); got != before {
		t.Fatalf("quota list rewrote the data directory:\nbefore %s\nafter  %s", before, got)
	}

	// Text and CSV render the same one-observation facts.
	text, _, code := runCLI(t, "quota", "list")
	if code != 0 {
		t.Fatalf("quota list text: code=%d", code)
	}
	for _, want := range []string{"QUOTA", "inst-a", "inst-b", "never combined", "not_reported"} {
		if !strings.Contains(text, want) {
			t.Fatalf("quota list text missing %q:\n%s", want, text)
		}
	}
	status, _, code := runCLI(t, "quota", "status", "inst-a")
	if code != 0 {
		t.Fatalf("quota status inst-a: code=%d", code)
	}
	for _, want := range []string{"forecast   unavailable (single_observation)", "credits", "balance=\"12.50\"", "resetsAt="} {
		if !strings.Contains(status, want) {
			t.Fatalf("quota status text missing %q:\n%s", want, status)
		}
	}

	csvOut, _, code := runCLI(t, "quota", "list", "--format", "csv")
	if code != 0 {
		t.Fatalf("quota list csv: code=%d", code)
	}
	rows, err := csv.NewReader(strings.NewReader(csvOut)).ReadAll()
	if err != nil {
		t.Fatalf("quota CSV: %v\n%s", err, csvOut)
	}
	if len(rows) != 3 || len(rows[0]) != 26 {
		t.Fatalf("quota CSV = %d rows of %d columns, want 3 rows of 26", len(rows), len(rows[0]))
	}
	idx := map[string]int{}
	for i, name := range rows[0] {
		idx[name] = i
	}
	if got := rows[1][idx["primary_used_percent"]]; got != "42.5" {
		t.Fatalf("primary_used_percent = %q, want 42.5", got)
	}
	// An unstated secondary window is empty, never 0 and never not_reported.
	if got := rows[2][idx["secondary_status"]]; got != "" {
		t.Fatalf("inst-b secondary_status = %q, want empty", got)
	}
}

// TestQuotaListKeepsEveryInstanceVisible proves that enumeration reports each
// stored key on its own and that an explicit filter reports a missing instance
// instead of dropping it.
func TestQuotaListKeepsEveryInstanceVisible(t *testing.T) {
	quotaSeed(t, map[string]*quota.Report{"inst-ok": quotaReportFixture()}, map[string]string{
		"inst-json": "{not json",
		"inst-nil":  `{"harness":"codex","instance":"inst-nil"}`,
	})

	out, errout, code := runCLI(t, "quota", "list", "--json")
	if code != 0 {
		t.Fatalf("quota list --json: code=%d stderr=%s", code, errout)
	}
	doc := snapDecode(t, out)
	if rows, _ := doc["instances"].([]any); len(rows) != 3 {
		t.Fatalf("instances = %v, want all three stored keys", doc["instances"])
	}
	// One bad key cannot hide the good instance: each is reported on its own.
	ok := quotaInstance(t, doc, "inst-ok")
	if ok["available"] != true {
		t.Fatalf("inst-ok = %v, want available despite the corrupt neighbours", ok)
	}
	for _, instance := range []string{"inst-json", "inst-nil"} {
		row := quotaInstance(t, doc, instance)
		if row["available"] != false || snapAt(row, "status", "reason") != "corrupt_stored" {
			t.Fatalf("%s = %v, want unavailable corrupt_stored", instance, row)
		}
		if detail, _ := snapAt(row, "status", "detail").(string); detail == "" {
			t.Fatalf("%s = %v, want a decode detail", instance, row)
		}
	}

	// An explicitly requested instance with nothing stored is still reported.
	out, errout, code = runCLI(t, "quota", "list", "--json", "--instance-id", "inst-missing")
	if code != 0 {
		t.Fatalf("quota list --instance-id: code=%d stderr=%s", code, errout)
	}
	doc = snapDecode(t, out)
	if rows, _ := doc["instances"].([]any); len(rows) != 1 {
		t.Fatalf("filtered instances = %v, want exactly the requested instance", doc["instances"])
	}
	missing := quotaInstance(t, doc, "inst-missing")
	if missing["available"] != false || snapAt(missing, "status", "reason") != "no_observation" {
		t.Fatalf("inst-missing = %v, want unavailable no_observation", missing)
	}
	if detail, _ := snapAt(missing, "status", "detail").(string); !strings.Contains(detail, "no stored Codex quota observation") {
		t.Fatalf("inst-missing detail = %q", detail)
	}
	// Nothing is combined across instances, so the unusable-only section says so.
	if avail, _ := snapAt(doc, "status", "available").(bool); avail {
		t.Fatalf("section = %v, want unavailable when no requested instance is usable", doc["status"])
	}
}

// TestQuotaForecastNeverInventsAPace proves the pace refusal: one stored
// observation per instance cannot yield a two-point pace, a window-less
// observation has nothing to pace, and an unusable report inherits its reason.
func TestQuotaForecastNeverInventsAPace(t *testing.T) {
	quotaSeed(t, map[string]*quota.Report{"inst-a": quotaReportFixture()}, nil)
	out, errout, code := runCLI(t, "quota", "status", "inst-a", "--json")
	if code != 0 {
		t.Fatalf("quota status: code=%d stderr=%s", code, errout)
	}
	row := quotaInstance(t, snapDecode(t, out), "inst-a")
	if avail, _ := snapAt(row, "forecast", "available").(bool); avail {
		t.Fatalf("forecast = %v, want unavailable: one observation is not a pace", row["forecast"])
	}
	if got := snapAt(row, "forecast", "reason"); got != "single_observation" {
		t.Fatalf("forecast.reason = %v, want single_observation", got)
	}
	detail, _ := snapAt(row, "forecast", "detail").(string)
	if !strings.Contains(detail, "never derived from token counts") {
		t.Fatalf("forecast.detail = %q, want the explicit refusal", detail)
	}

	// A usable observation with no window has nothing to pace.
	plan := "plus"
	quotaSeed(t, map[string]*quota.Report{
		"inst-plan": {Source: "fixture/inst-plan", Status: quota.StatusOK, PlanType: &plan},
	}, nil)
	out, _, code = runCLI(t, "quota", "status", "inst-plan", "--json")
	if code != 0 {
		t.Fatalf("quota status inst-plan: code=%d", code)
	}
	row = quotaInstance(t, snapDecode(t, out), "inst-plan")
	if got := snapAt(row, "forecast", "reason"); got != "no_usable_window" {
		t.Fatalf("window-less forecast.reason = %v, want no_usable_window", got)
	}
	if row["primary"] != nil || row["secondary"] != nil {
		t.Fatalf("window-less row = %v, want no window", row)
	}

	// An unusable stored report reports its own reason, and nothing is invented.
	quotaSeed(t, map[string]*quota.Report{
		"inst-none": {Source: "fixture/inst-none", Status: quota.StatusUnavailable, Reason: quota.ReasonNoData},
	}, nil)
	out, _, code = runCLI(t, "quota", "status", "inst-none", "--json")
	if code != 0 {
		t.Fatalf("quota status inst-none: code=%d", code)
	}
	row = quotaInstance(t, snapDecode(t, out), "inst-none")
	if row["available"] != false || snapAt(row, "status", "reason") != "unavailable" {
		t.Fatalf("unusable row = %v, want unavailable", row)
	}
	if got := snapAt(row, "status", "detail"); got != "no_data" {
		t.Fatalf("unusable detail = %v, want the stored reason no_data", got)
	}
	if got := snapAt(row, "forecast", "reason"); got != "unavailable" {
		t.Fatalf("unusable forecast.reason = %v, want unavailable", got)
	}
}

// TestQuotaWithoutIndexStaysReadOnly proves that a fresh machine (no index) is
// reported as unavailable rather than treated as an empty index, that an
// explicit instance stays visible, and that nothing is created.
func TestQuotaWithoutIndexStaysReadOnly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MYTOKEN_HOME", dir)
	before := snapFingerprint(t, dir)

	out, errout, code := runCLI(t, "quota", "list", "--json")
	if code != 0 {
		t.Fatalf("quota list without an index: code=%d stderr=%s", code, errout)
	}
	doc := snapDecode(t, out)
	if avail, _ := snapAt(doc, "status", "available").(bool); avail {
		t.Fatalf("status = %v, want unavailable", doc["status"])
	}
	if got := snapAt(doc, "status", "reason"); got != "missing_database" {
		t.Fatalf("section reason = %v, want missing_database", got)
	}
	if rows, _ := doc["instances"].([]any); len(rows) != 0 {
		t.Fatalf("instances = %v, want none without an index", doc["instances"])
	}

	out, _, code = runCLI(t, "quota", "list", "--json", "--instance-id", "inst-a")
	if code != 0 {
		t.Fatalf("filtered quota list without an index: code=%d", code)
	}
	rows, _ := snapDecode(t, out)["instances"].([]any)
	if len(rows) != 1 || snapAt(rows[0].(map[string]any), "status", "reason") != "no_observation" {
		t.Fatalf("filtered instances = %v, want the requested instance as no_observation", snapDecode(t, out)["instances"])
	}

	// The snapshot reports the same reason for its quota section.
	snapOut, snapperr, code := runCLI(t, "snapshot", "--once", "--json")
	if code != 0 {
		t.Fatalf("snapshot without an index: code=%d stderr=%s", code, snapperr)
	}
	if got := snapAt(snapDecode(t, snapOut), "sources", "quota", "reason"); got != "missing_database" {
		t.Fatalf("snapshot sources.quota.reason = %v, want missing_database", got)
	}

	if got := snapFingerprint(t, dir); got != before {
		t.Fatalf("quota created files in the data directory:\nbefore %q\nafter  %q", before, got)
	}
}

func TestQuotaCommandUsageErrors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MYTOKEN_HOME", dir)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no subcommand", []string{"quota"}, "Usage: mytoken quota"},
		{"unknown subcommand", []string{"quota", "nope"}, "unknown quota command: nope"},
		{"list takes no positional", []string{"quota", "list", "extra"}, "Usage: mytoken quota"},
		{"invalid format", []string{"quota", "list", "--format", "yaml"}, "invalid --format"},
		{"status takes one positional", []string{"quota", "status", "a", "b"}, "Usage: mytoken quota"},
	}
	for _, tc := range cases {
		_, errout, code := runCLI(t, tc.args...)
		if code != 2 {
			t.Errorf("%s: code=%d stderr=%s, want 2", tc.name, code, errout)
			continue
		}
		if !strings.Contains(errout, tc.want) {
			t.Errorf("%s: stderr %q missing %q", tc.name, errout, tc.want)
		}
	}
	if _, _, code := runCLI(t, "quota", "help"); code != 0 {
		t.Errorf("quota help: code=%d, want 0", code)
	}
	// The top-level help must advertise the command it dispatches.
	help, _, code := runCLI(t, "help")
	if code != 0 || !strings.Contains(help, "quota list") || !strings.Contains(help, "quota status") {
		t.Errorf("help does not document quota list/status:\n%s", help)
	}
}
