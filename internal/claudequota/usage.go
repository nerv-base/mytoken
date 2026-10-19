package claudequota

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

// Target is Anthropic's OAuth usage endpoint, the one Claude Code's own /usage
// view is backed by.
const Target = "https://api.anthropic.com/api/oauth/usage"

// OAuthBeta is the beta header the usage endpoint requires.
const OAuthBeta = "oauth-2025-04-20"

// UserAgent identifies this client.
const UserAgent = "mytoken-quota/1"

const maxQuotaBytes = 64 << 10

// knownWindow maps the endpoint's window keys onto display labels.
var knownWindows = []struct{ key, name string }{
	{"five_hour", "5h"},
	{"seven_day", "week"},
	{"seven_day_opus", "week-opus"},
	{"seven_day_sonnet", "week-sonnet"},
}

func fetchUsage(ctx context.Context, tp plan.Transport, endpoint string, cred Credential) ([]plan.Window, error) {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+cred.Reveal())
	header.Set("Accept", "application/json")
	header.Set("User-Agent", UserAgent)
	header.Set("anthropic-beta", OAuthBeta)
	raw, err := plan.DoJSON(ctx, tp, http.MethodGet, endpoint, header, nil, maxQuotaBytes)
	if err != nil {
		return nil, err
	}
	return parseUsage(raw)
}

// windowPayload is one quota window object. Utilization is a percentage
// (0-100); the alternative field names appear in some responses.
type windowPayload struct {
	Utilization *float64        `json:"utilization"`
	UsedPercent *float64        `json:"used_percent"`
	Percent     *float64        `json:"percent"`
	ResetsAt    json.RawMessage `json:"resets_at"`
}

// parseUsage maps the usage payload onto ordered quota windows. Keys are
// processed in a fixed order; unrecognized keys that decode as a window follow
// sorted by key. A payload with no usable window is an error, not 0%.
func parseUsage(raw []byte) ([]plan.Window, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, plan.NewError(plan.ReasonMalformed, err)
	}
	if fields == nil {
		return nil, plan.NewError(plan.ReasonMalformed, nil)
	}
	var out []plan.Window
	known := make(map[string]bool, len(knownWindows))
	for _, w := range knownWindows {
		known[w.key] = true
		raw, ok := fields[w.key]
		if !ok {
			continue
		}
		if win, ok := decodeWindow(w.name, raw); ok {
			out = append(out, win)
		}
	}
	var rest []string
	for key := range fields {
		if !known[key] {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)
	for _, key := range rest {
		if win, ok := decodeWindow(windowName(key), fields[key]); ok {
			out = append(out, win)
		}
	}
	if len(out) == 0 {
		return nil, plan.NewError(plan.ReasonNoWindows, nil)
	}
	return out, nil
}

// windowName turns an unknown payload key into a display label.
func windowName(key string) string {
	if strings.HasPrefix(key, "seven_day_") {
		return "week-" + strings.ReplaceAll(strings.TrimPrefix(key, "seven_day_"), "_", "-")
	}
	return strings.ReplaceAll(key, "_", "-")
}

// decodeWindow accepts either a bare percentage or a window object. It reports
// false when the payload states neither a percentage nor a reset instant, so a
// window the provider did not mention is omitted rather than shown as zero.
func decodeWindow(name string, raw json.RawMessage) (plan.Window, bool) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return plan.Window{}, false
	}
	win := plan.Window{Name: name}
	if raw[0] == '{' {
		var p windowPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return plan.Window{}, false
		}
		pct := firstPercent(p.Utilization, p.UsedPercent, p.Percent)
		if pct != nil {
			win.UsedPercent = *pct
		}
		if at, ok := decodeInstant(p.ResetsAt); ok {
			win.ResetsAt = at
		}
		if pct == nil && win.ResetsAt.IsZero() {
			return plan.Window{}, false
		}
		return win, true
	}
	var pct float64
	if err := json.Unmarshal(raw, &pct); err != nil || math.IsNaN(pct) || math.IsInf(pct, 0) {
		return plan.Window{}, false
	}
	win.UsedPercent = pct
	return win, true
}

func firstPercent(vals ...*float64) *float64 {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

// decodeInstant reads a reset instant stated as RFC 3339 text, an epoch in
// seconds or an epoch in milliseconds.
func decodeInstant(raw json.RawMessage) (time.Time, bool) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, false
	}
	switch raw[0] {
	case '"':
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return time.Time{}, false
		}
		text = strings.TrimSpace(text)
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999Z0700"} {
			if at, err := time.Parse(layout, text); err == nil {
				return at.UTC(), true
			}
		}
		return time.Time{}, false
	default:
		var n float64
		if err := json.Unmarshal(raw, &n); err != nil || n <= 0 {
			return time.Time{}, false
		}
		if n > 1e12 {
			return time.UnixMilli(int64(n)).UTC(), true
		}
		return time.Unix(int64(n), 0).UTC(), true
	}
}
