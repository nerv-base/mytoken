package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/quota"
)

// quotaPaceLatest builds the envelope the Codex writer produces once observation
// retention exists: the report, the two retained samples of the primary window,
// and the pace forecast derived from them at write time. The samples are 20
// minutes apart and consumed 10 points, so the measured pace is 30%/h.
func quotaPaceLatest(instance string, now time.Time) quota.Latest {
	used, windowMinutes := 50.0, int64(300)
	rep := &quota.Report{
		Source:     "fixture/" + instance,
		ObservedAt: &now,
		Line:       1,
		Status:     quota.StatusOK,
		Primary: &quota.Window{
			Kind: quota.WindowPrimary, Status: quota.StatusOK,
			UsedPercent: &used, WindowMinutes: &windowMinutes,
		},
		LinesRead: 1,
	}
	latest := quota.Latest{
		Harness:    quota.HarnessCodex,
		Instance:   instance,
		Source:     rep.Source,
		IngestedAt: now,
		Report:     rep,
		Observations: []quota.Observation{
			{Window: quota.WindowPrimary, Source: rep.Source, ObservedAt: now.Add(-30 * time.Minute), UsedPercent: 40},
			{Window: quota.WindowPrimary, Source: rep.Source, ObservedAt: now.Add(-10 * time.Minute), UsedPercent: 50},
		},
	}
	latest.Forecast = quota.Forecasts(&latest, now, quota.ForecastOptions{})
	return latest
}

func quotaLatestJSON(t *testing.T, latest quota.Latest) string {
	t.Helper()
	raw, err := json.Marshal(latest)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestQuotaPaceFromStoredForecastOnly proves the additive pace surface: a stored
// forecast is exposed in the cached JSON and in the concise table and status
// text with the instant its own observation was taken (as-of), and an envelope
// that carries no forecast shows no pace at all. The reader never derives one:
// a stored single observation whose report does have a window still has no pace.
func TestQuotaPaceFromStoredForecastOnly(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	withPace := quotaPaceLatest("inst-pace", now)
	// The control is the same shape without the retained forecast: a value
	// written before observation retention existed.
	withoutPace := withPace
	withoutPace.Observations = nil
	withoutPace.Forecast = nil

	quotaSeed(t, nil, map[string]string{
		"inst-pace": quotaLatestJSON(t, withPace),
		"inst-old":  quotaLatestJSON(t, withoutPace),
	})

	out, errout, code := runCLI(t, "quota", "list", "--json")
	if code != 0 {
		t.Fatalf("quota list --json: code=%d stderr=%s", code, errout)
	}
	doc := snapDecode(t, out)

	row := quotaInstance(t, doc, "inst-pace")
	pace, ok := row["pace"].(map[string]any)
	if !ok {
		t.Fatalf("inst-pace has no additive pace in the cached JSON: %v", row)
	}
	if pace["available"] != true {
		t.Fatalf("pace.available = %v, want true: %v", pace["available"], pace)
	}
	if _, ok := pace["observed_at"]; !ok {
		t.Fatalf("pace must carry the instant its observation was taken: %v", pace)
	}
	if avail, _ := snapAt(row, "forecast", "available").(bool); !avail {
		t.Fatalf("the verdict must agree with a stored available pace: %v", row["forecast"])
	}
	windows, _ := pace["windows"].([]any)
	if len(windows) == 0 {
		t.Fatalf("pace.windows = %v, want the primary window", pace["windows"])
	}
	window, _ := windows[0].(map[string]any)
	if window["window"] != "primary" {
		t.Fatalf("pace window = %v, want primary", window["window"])
	}
	inner, _ := window["pace"].(map[string]any)
	pph, _ := inner["percent_per_hour"].(float64)
	if pph < 29.9 || pph > 30.1 {
		t.Fatalf("percent_per_hour = %v, want the stored 30%%/h", inner["percent_per_hour"])
	}

	// The control never gets a pace derived at read time, even though its
	// report carries a window a reader could divide by.
	old := quotaInstance(t, doc, "inst-old")
	if old["pace"] != nil {
		t.Fatalf("inst-old has no stored forecast, so it must show no pace: %v", old["pace"])
	}
	if got := snapAt(old, "forecast", "reason"); got != "single_observation" {
		t.Fatalf("inst-old forecast.reason = %v, want the reader's own refusal", got)
	}

	// The concise table carries the rate and marks it as-of.
	text, _, code := runCLI(t, "quota", "list")
	if code != 0 {
		t.Fatalf("quota list: code=%d", code)
	}
	if !strings.Contains(text, "primary=30.00%/h") {
		t.Fatalf("quota list table must show the stored rate:\n%s", text)
	}
	if !strings.Contains(text, "as-of ") {
		t.Fatalf("quota list table must date the stored rate:\n%s", text)
	}

	// The verbose view gets its own pace line.
	status, _, code := runCLI(t, "quota", "status", "inst-pace")
	if code != 0 {
		t.Fatalf("quota status: code=%d", code)
	}
	if !strings.Contains(status, "pace       primary=30.00%/h") {
		t.Fatalf("quota status must show the pace line:\n%s", status)
	}
	if !strings.Contains(status, "as-of ") {
		t.Fatalf("quota status must date the pace:\n%s", status)
	}
	oldStatus, _, code := runCLI(t, "quota", "status", "inst-old")
	if code != 0 {
		t.Fatalf("quota status inst-old: code=%d", code)
	}
	if strings.Contains(oldStatus, "\npace ") {
		t.Fatalf("inst-old must have no pace line:\n%s", oldStatus)
	}

	// The cached snapshot carries the same additive field.
	snapOut, errout, code := runCLI(t, "snapshot", "--once", "--json")
	if code != 0 {
		t.Fatalf("snapshot --once --json: code=%d stderr=%s", code, errout)
	}
	snapDoc := snapDecode(t, snapOut)
	entries, _ := snapAt(snapDoc, "data", "codexQuota").([]any)
	if len(entries) != 2 {
		t.Fatalf("snapshot codexQuota = %v, want two instances", snapAt(snapDoc, "data", "codexQuota"))
	}
	var sawPace, sawNone bool
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		switch entry["instance"] {
		case "inst-pace":
			sawPace = entry["pace"] != nil
		case "inst-old":
			sawNone = entry["pace"] == nil
		}
	}
	if !sawPace || !sawNone {
		t.Fatalf("snapshot pace exposure wrong: pace=%v none=%v", sawPace, sawNone)
	}
}
