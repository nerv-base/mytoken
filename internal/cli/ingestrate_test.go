package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/scan"
)

// ingestSignal renders a stored ingest-rate signal through the producer's own
// type, so the test cannot drift from the persisted field names.
func ingestSignal(at time.Time, available bool, reason string, window float64, tokens int64, rate float64) string {
	raw, err := json.Marshal(scan.IngestRateSignal{
		Timestamp: at,
		Available: available,
		Reason:    reason,
		Window:    window,
		Tokens:    tokens,
		Rate:      rate,
	})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// TestPublishedIngestRateReachesSnapshotAndStatusline proves the wiring for
// ingest_rate.latest.v1: a one-shot process reports the rate a running scan
// published, in both the envelope and the statusline, without measuring or
// inventing anything itself.
func TestPublishedIngestRateReachesSnapshotAndStatusline(t *testing.T) {
	dir := snapSeed(t, map[string]string{
		scan.IngestRateSettingKey: ingestSignal(time.Now().UTC().Add(-5*time.Second), true, "", 60, 600, 10),
	})
	before := snapFingerprint(t, dir)

	out, errout, code := runCLI(t, "snapshot", "--once", "--json")
	if code != 0 {
		t.Fatalf("snapshot --json: code=%d stderr=%s", code, errout)
	}
	doc := snapDecode(t, out)
	if avail, _ := snapAt(doc, "sources", "tokRate", "available").(bool); !avail {
		t.Fatalf("sources.tokRate = %v, want available", snapAt(doc, "sources", "tokRate"))
	}
	if snapAt(doc, "sources", "tokRate", "fetchedAt") == nil {
		t.Fatalf("sources.tokRate = %v, want the signal timestamp", snapAt(doc, "sources", "tokRate"))
	}
	if got := snapAt(doc, "data", "tokRate", "ratePerSecond"); got != 10.0 {
		t.Fatalf("data.tokRate.ratePerSecond = %v, want 10", got)
	}
	if got := snapAt(doc, "data", "tokRate", "tokens"); got != float64(600) {
		t.Fatalf("data.tokRate.tokens = %v, want 600", got)
	}
	if got := snapAt(doc, "data", "tokRate", "windowSeconds"); got != float64(60) {
		t.Fatalf("data.tokRate.windowSeconds = %v, want 60", got)
	}

	text, _, code := runCLI(t, "snapshot", "--once")
	if code != 0 {
		t.Fatalf("snapshot text: code=%d", code)
	}
	if !strings.Contains(text, "tokRate    10.00 tok/s over 60s tokens=600") {
		t.Fatalf("snapshot text missing the published rate:\n%s", text)
	}

	slOut, errout, code := runCLI(t, "statusline", "--json")
	if code != 0 {
		t.Fatalf("statusline --json: code=%d stderr=%s", code, errout)
	}
	sl := snapDecode(t, slOut)
	if avail, _ := snapAt(sl, "tokRateStatus", "available").(bool); !avail {
		t.Fatalf("statusline tokRateStatus = %v, want available", snapAt(sl, "tokRateStatus"))
	}
	if got := snapAt(sl, "tokRate", "ratePerSecond"); got != 10.0 {
		t.Fatalf("statusline tokRate.ratePerSecond = %v, want 10", got)
	}
	slText, _, code := runCLI(t, "statusline")
	if code != 0 {
		t.Fatalf("statusline text: code=%d", code)
	}
	if !strings.Contains(slText, "ingest 10.00 tok/s over 60s") {
		t.Fatalf("statusline text missing the published rate:\n%s", slText)
	}

	if got := snapFingerprint(t, dir); got != before {
		t.Fatalf("reporting the ingest rate rewrote the index:\nbefore %s\nafter  %s", before, got)
	}
}

// TestIngestRateIsNeverFabricated proves the refusals: an expired signal is not
// a current rate, an unusable signal keeps its own reason, a corrupt value is
// named as such, and an absent one stays no_live_signal. In every case the
// number is null rather than derived from stored token counts.
func TestIngestRateIsNeverFabricated(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name       string
		settings   map[string]string
		wantReason string
	}{
		{"expired", map[string]string{scan.IngestRateSettingKey: ingestSignal(now.Add(-10*time.Minute), true, "", 60, 600, 10)}, "stale_signal"},
		{"unusable", map[string]string{scan.IngestRateSettingKey: ingestSignal(now.Add(-time.Second), false, "no_live_signal", 60, 0, 0)}, "no_live_signal"},
		{"corrupt", map[string]string{scan.IngestRateSettingKey: `{"timestamp":`}, "corrupt_stored"},
		{"absent", map[string]string{}, "no_live_signal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := snapSeed(t, tc.settings)
			before := snapFingerprint(t, dir)

			out, errout, code := runCLI(t, "snapshot", "--once", "--json")
			if code != 0 {
				t.Fatalf("snapshot --json: code=%d stderr=%s", code, errout)
			}
			doc := snapDecode(t, out)
			if avail, _ := snapAt(doc, "sources", "tokRate", "available").(bool); avail {
				t.Fatalf("sources.tokRate = %v, want unavailable", snapAt(doc, "sources", "tokRate"))
			}
			if got := snapAt(doc, "sources", "tokRate", "reason"); got != tc.wantReason {
				t.Fatalf("sources.tokRate.reason = %v, want %s", got, tc.wantReason)
			}
			if got := snapAt(doc, "data", "tokRate"); got != nil {
				t.Fatalf("data.tokRate = %v, want null: no rate may be derived from stored tokens", got)
			}

			slOut, errout, code := runCLI(t, "statusline", "--json")
			if code != 0 {
				t.Fatalf("statusline --json: code=%d stderr=%s", code, errout)
			}
			sl := snapDecode(t, slOut)
			if got := snapAt(sl, "tokRateStatus", "reason"); got != tc.wantReason {
				t.Fatalf("statusline tokRateStatus.reason = %v, want %s", got, tc.wantReason)
			}
			if got := sl["tokRate"]; got != nil {
				t.Fatalf("statusline tokRate = %v, want null", got)
			}
			slText, _, code := runCLI(t, "statusline")
			if code != 0 {
				t.Fatalf("statusline text: code=%d", code)
			}
			if want := "ingest unavailable (" + tc.wantReason + ")"; !strings.Contains(slText, want) {
				t.Fatalf("statusline text %q missing %q", slText, want)
			}

			if got := snapFingerprint(t, dir); got != before {
				t.Fatalf("the read rewrote the index:\nbefore %s\nafter  %s", before, got)
			}
		})
	}
}
