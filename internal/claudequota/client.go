package claudequota

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/quota"
)

// Status is the freshness of a cached quota reading.
type Status string

// Statuses.
const (
	// StatusOK means the cached reading is fresh.
	StatusOK Status = "ok"
	// StatusStale means a reading exists but is older than the stale window.
	StatusStale Status = "stale"
	// StatusUnavailable means no usable reading exists.
	StatusUnavailable Status = "unavailable"
)

// StaleAfter is how old a successful reading may be before it reports stale.
const StaleAfter = 30 * time.Minute

// Window is one rolling quota window. Absent data is represented by omission or
// by Present=false, never by a zero percentage: a window the endpoint did not
// state must not read as 0% used.
type Window struct {
	// Kind is the endpoint's window key (five_hour, seven_day, ...).
	Kind string `json:"kind"`
	// Present is false when the endpoint stated the window but carried no
	// usage for it.
	Present bool `json:"present"`
	// UsedPercent is the stated utilization, when stated.
	UsedPercent *float64 `json:"used_percent,omitempty"`
	// RemainingPercent is 100-UsedPercent, present only when UsedPercent is.
	RemainingPercent *float64 `json:"remaining_percent,omitempty"`
	// ResetsAt is the stated reset instant, when stated.
	ResetsAt *time.Time `json:"resets_at,omitempty"`
	// WindowMinutes is the nominal length of the window, when known.
	WindowMinutes *int `json:"window_minutes,omitempty"`
}

// Quota is the cached reading for one instance. It holds quota windows and
// freshness only, never a credential.
type Quota struct {
	// Instance is the instance this reading belongs to.
	Instance string `json:"instance"`
	// Target is the request target the reading came from.
	Target string `json:"target"`
	// SourceKind and SourceRef identify the credential source authorized.
	SourceKind SourceKind `json:"source_kind"`
	SourceRef  string     `json:"source_ref"`
	// FetchedAt is when a fetch last succeeded (zero when none ever did).
	FetchedAt time.Time `json:"fetched_at,omitempty"`
	// AttemptedAt is when a fetch was last tried, successful or not.
	AttemptedAt time.Time `json:"attempted_at,omitempty"`
	// Status is the freshness of FetchedAt's reading.
	Status Status `json:"status"`
	// Reason is the last failure reason, empty after a success.
	Reason Reason `json:"reason,omitempty"`
	// Windows are the stated quota windows.
	Windows []Window `json:"windows,omitempty"`

	// Observations are the retained samples a pace forecast is derived from:
	// at most two per window, each with the instant it was fetched at and the
	// source reference it came from. A reading stored before retention existed
	// simply has none, so this field is additive.
	Observations []quota.Observation `json:"observations,omitempty"`

	// Forecast is the pace picture derived from Observations. It is recomputed
	// whenever the reading is published and refreshed against the current clock
	// whenever the reading is read, so a consumer can show it directly. Like
	// Observations it is additive and may be absent in an older payload.
	Forecast *quota.Forecast `json:"forecast,omitempty"`
}

// Transport performs one HTTP request. It is injected so tests never use the
// network.
type Transport interface {
	Do(req *http.Request) (*http.Response, error)
}

const (
	requestTimeout = 15 * time.Second
	// UserAgent identifies this client to the endpoint.
	UserAgent = "mytoken-claudequota/1"
)

// DefaultTransport is the real transport: a bounded client that refuses every
// redirect, so a cross-origin redirect can never receive the credential.
func DefaultTransport() Transport {
	return &http.Client{
		Timeout: requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// fetchUsage performs the single authorized request and decodes the windows.
// It is called only after the consent check and only with a credential that
// has not expired.
func fetchUsage(ctx context.Context, tp Transport, cred Credential) ([]Window, error) {
	if tp == nil {
		return nil, errReason(ReasonNetwork)
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, Target, nil)
	if err != nil {
		return nil, errReason(ReasonNetwork)
	}
	req.Header.Set("Authorization", "Bearer "+cred.Reveal())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)

	resp, err := tp.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &Error{Reason: ReasonCanceled, cause: err}
		}
		return nil, &Error{Reason: ReasonNetwork, cause: err}
	}
	defer resp.Body.Close()

	// A redirect (or any answer from another origin) is refused outright.
	if resp.Request != nil && resp.Request.URL != nil {
		if u := resp.Request.URL; !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Host, "api.anthropic.com") {
			return nil, &Error{Reason: ReasonRedirectRefused, StatusCode: resp.StatusCode}
		}
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, &Error{Reason: ReasonRedirectRefused, StatusCode: resp.StatusCode}
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, &Error{Reason: ReasonUnauthorized, StatusCode: resp.StatusCode}
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, &Error{Reason: ReasonRateLimited, StatusCode: resp.StatusCode, RetryAfter: retryAfter(resp)}
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, &Error{Reason: ReasonHTTPStatus, StatusCode: resp.StatusCode}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxQuotaBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, &Error{Reason: ReasonCanceled, cause: err}
		}
		return nil, &Error{Reason: ReasonNetwork, cause: err}
	}
	if len(body) > maxQuotaBytes {
		return nil, errReason(ReasonMalformed)
	}
	return parseUsage(body)
}

func retryAfter(resp *http.Response) time.Duration {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// windowMinutes is the nominal length of each known window key.
var windowMinutes = map[string]int{
	"five_hour":        5 * 60,
	"seven_day":        7 * 24 * 60,
	"seven_day_opus":   7 * 24 * 60,
	"seven_day_sonnet": 7 * 24 * 60,
}

// knownWindows fixes the display order of the known window keys.
var knownWindows = []string{"five_hour", "seven_day", "seven_day_opus", "seven_day_sonnet"}

// parseUsage decodes the usage payload. Unknown window keys are kept (sorted
// after the known ones) so schema drift degrades to extra rows rather than to a
// failure; a payload with no stated window is an error, never 0%.
func parseUsage(body []byte) ([]Window, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, errReason(ReasonMalformed)
	}
	var out []Window
	seen := map[string]bool{}
	for _, key := range knownWindows {
		raw, ok := top[key]
		if !ok {
			continue
		}
		seen[key] = true
		if w, ok := decodeWindow(key, raw); ok {
			out = append(out, w)
		}
	}
	rest := make([]string, 0, len(top))
	for key := range top {
		if seen[key] {
			continue
		}
		rest = append(rest, key)
	}
	sort.Strings(rest)
	for _, key := range rest {
		if w, ok := decodeWindow(key, top[key]); ok {
			out = append(out, w)
		}
	}
	if len(out) == 0 {
		return nil, errReason(ReasonNoWindows)
	}
	return out, nil
}

// windowPayload is the per-window object shape.
type windowPayload struct {
	Utilization *float64        `json:"utilization"`
	UsedPercent *float64        `json:"used_percent"`
	Percent     *float64        `json:"percent"`
	ResetsAt    json.RawMessage `json:"resets_at"`
	WindowMins  *int            `json:"window_minutes"`
}

// decodeWindow decodes one window. It returns false when the key carries no
// usage at all (null, or an empty value), which is how "missing" is preserved.
func decodeWindow(kind string, raw json.RawMessage) (Window, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return Window{Kind: kind, Present: false, WindowMinutes: minutesFor(kind)}, true
	}
	// A window stated as a bare number, as some payload revisions do.
	if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
		used := f
		remaining := 100 - f
		return Window{
			Kind:             kind,
			Present:          true,
			UsedPercent:      &used,
			RemainingPercent: &remaining,
			WindowMinutes:    minutesFor(kind),
		}, true
	}
	var p windowPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return Window{}, false
	}
	w := Window{Kind: kind, WindowMinutes: minutesFor(kind)}
	switch {
	case p.Utilization != nil:
		w.UsedPercent = p.Utilization
	case p.UsedPercent != nil:
		w.UsedPercent = p.UsedPercent
	case p.Percent != nil:
		w.UsedPercent = p.Percent
	}
	if p.WindowMins != nil && *p.WindowMins > 0 {
		w.WindowMinutes = p.WindowMins
	}
	if w.UsedPercent != nil {
		remaining := 100 - *w.UsedPercent
		w.RemainingPercent = &remaining
		w.Present = true
	}
	if t, ok := decodeInstant(p.ResetsAt); ok {
		w.ResetsAt = &t
		w.Present = true
	}
	if !w.Present {
		return w, true // explicitly stated with no usage: absent, not zero
	}
	return w, true
}

func minutesFor(kind string) *int {
	if m, ok := windowMinutes[kind]; ok {
		return &m
	}
	return nil
}

// decodeInstant accepts an RFC3339 string or a Unix seconds/milliseconds
// number, the shapes the endpoint has been observed to use.
func decodeInstant(raw json.RawMessage) (time.Time, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return time.Time{}, false
	}
	if strings.HasPrefix(s, `"`) {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return time.Time{}, false
		}
		str = strings.TrimSpace(str)
		if str == "" {
			return time.Time{}, false
		}
		if t, err := time.Parse(time.RFC3339, str); err == nil {
			return t.UTC(), true
		}
		if t, err := time.Parse("2006-01-02T15:04:05.999999999Z0700", str); err == nil {
			return t.UTC(), true
		}
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	if n > 1e12 {
		return time.UnixMilli(n).UTC(), true
	}
	return time.Unix(n, 0).UTC(), true
}

// decodeQuota decodes a persisted reading, bounded and non-fatal.
func decodeQuota(raw string) (Quota, bool) {
	if strings.TrimSpace(raw) == "" || len(raw) > maxQuotaBytes {
		return Quota{}, false
	}
	var q Quota
	if err := json.Unmarshal([]byte(raw), &q); err != nil {
		return Quota{}, false
	}
	return q, true
}

func encodeQuota(q Quota) (string, error) {
	b, err := json.Marshal(q)
	if err != nil {
		return "", errReason(ReasonStoreUnavailable)
	}
	if len(b) > maxQuotaBytes {
		return "", errReason(ReasonStoreUnavailable)
	}
	return string(b), nil
}
