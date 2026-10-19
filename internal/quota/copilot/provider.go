// Package copilot reports GitHub Copilot's monthly request quota.
//
// The legacy GitHub Copilot extensions store the signed-in user's OAuth token
// in apps.json or hosts.json under the platform config directory. With that
// token this package calls the same endpoint the editor extension uses:
//
//	GET https://api.github.com/copilot_internal/user
//	Authorization: token <oauth_token>
//
// The response's quota_snapshots (premium_interactions, chat) and the older
// monthly_quotas/limited_user_quotas pair are mapped onto a monthly window.
// Payload shape per https://github.com/steipete/CodexBar
// (docs/copilot.md and Sources/CodexBarCore/CopilotUsageModels.swift).
package copilot

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

// ProviderID is the stable id used in settings keys and by the dashboard.
const ProviderID = "copilot"

// BaseURL is the GitHub API origin.
const BaseURL = "https://api.github.com"

const (
	usagePath     = "/copilot_internal/user"
	maxBodyBytes  = 64 << 10
	editorVersion = "vscode/1.96.2"
	pluginVersion = "copilot-chat/0.26.7"
	chatUserAgent = "GitHubCopilotChat/0.26.7"
	githubAPIDate = "2025-04-01"
	windowMonth   = "month"
	unitRequests  = "requests"
	fallbackPlan  = "unknown"
)

// goosDefault is the platform this binary was built for.
const goosDefault = runtime.GOOS

// Options configures a Provider. Every field is optional.
type Options struct {
	// CredentialPaths overrides the credential file locations.
	CredentialPaths []string
	// Transport performs HTTP requests; defaults to plan.DefaultTransport.
	Transport plan.Transport
	// BaseURL overrides the API origin (tests point it at a local server).
	BaseURL string
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
	// Getenv, Home and GOOS default to the process environment.
	Getenv func(string) string
	Home   string
	GOOS   string
}

// Provider reports GitHub Copilot plan usage.
type Provider struct {
	paths     []string
	transport plan.Transport
	baseURL   string
	now       func() time.Time
}

// New returns a Copilot provider.
func New(opts Options) *Provider {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	home := opts.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	goos := opts.GOOS
	if goos == "" {
		goos = goosDefault
	}
	transport := opts.Transport
	if transport == nil {
		transport = plan.DefaultTransport()
	}
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = BaseURL
	}
	paths := opts.CredentialPaths
	if paths == nil {
		paths = credentialPaths(getenv, home, goos)
	}
	return &Provider{paths: paths, transport: transport, baseURL: strings.TrimRight(baseURL, "/"), now: now}
}

// ID implements plan.Provider.
func (p *Provider) ID() string { return ProviderID }

// DisplayName implements plan.Provider.
func (p *Provider) DisplayName() string { return "GitHub Copilot" }

// Detect reports whether a Copilot token is stored locally.
func (p *Provider) Detect(ctx context.Context) bool {
	token, err := readOAuthToken(ctx, p.paths)
	return err == nil && token != ""
}

// Fetch returns the monthly request quota.
func (p *Provider) Fetch(ctx context.Context) (plan.Snapshot, error) {
	token, err := readOAuthToken(ctx, p.paths)
	if err != nil {
		return plan.Snapshot{}, err
	}
	header := http.Header{}
	header.Set("Authorization", "token "+token)
	header.Set("Accept", "application/json")
	header.Set("Editor-Version", editorVersion)
	header.Set("Editor-Plugin-Version", pluginVersion)
	header.Set("User-Agent", chatUserAgent)
	header.Set("X-GitHub-Api-Version", githubAPIDate)
	raw, err := plan.DoJSON(ctx, p.transport, http.MethodGet, p.baseURL+usagePath, header, nil, maxBodyBytes)
	if err != nil {
		return plan.Snapshot{}, err
	}
	var usage userResponse
	if err := json.Unmarshal(raw, &usage); err != nil {
		return plan.Snapshot{}, plan.NewError(plan.ReasonMalformed, err)
	}
	windows := usage.windows()
	if len(windows) == 0 {
		return plan.Snapshot{}, plan.NewError(plan.ReasonNoWindows, nil)
	}
	planName := strings.TrimSpace(usage.CopilotPlan)
	if planName == "" {
		planName = fallbackPlan
	}
	return plan.Snapshot{
		Provider:  ProviderID,
		Plan:      planName,
		Windows:   windows,
		FetchedAt: p.now().UTC(),
		Source:    plan.SourceAPI,
	}, nil
}

// flexFloat accepts a number stated as a JSON number or a JSON string.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(raw []byte) error {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		*f = 0
		return nil
	}
	if text[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			*f = 0
			return nil
		}
		text = strings.TrimSpace(s)
	}
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		*f = 0
		return nil
	}
	*f = flexFloat(v)
	return nil
}

type quotaSnapshot struct {
	Entitlement      flexFloat `json:"entitlement"`
	Remaining        flexFloat `json:"remaining"`
	CreditsUsed      flexFloat `json:"credits_used"`
	PercentRemaining *float64  `json:"percent_remaining"`
	Unlimited        bool      `json:"unlimited"`
}

type userResponse struct {
	CopilotPlan       string                   `json:"copilot_plan"`
	TokenBasedBilling bool                     `json:"token_based_billing"`
	QuotaResetDate    flexDate                 `json:"quota_reset_date"`
	QuotaSnapshots    map[string]quotaSnapshot `json:"quota_snapshots"`
	MonthlyQuotas     map[string]flexFloat     `json:"monthly_quotas"`
	LimitedUserQuotas map[string]flexFloat     `json:"limited_user_quotas"`
}

// windows returns the premium interaction and chat windows, in that order.
func (r userResponse) windows() []plan.Window {
	reset := time.Time(r.QuotaResetDate)
	var windows []plan.Window
	for _, kind := range sortedSnapshotKinds(r.QuotaSnapshots) {
		snapshot := r.QuotaSnapshots[kind]
		if snapshot.Unlimited {
			continue
		}
		window, ok := snapshotWindow(kind, snapshot, reset)
		if ok {
			windows = append(windows, window)
		}
	}
	if len(windows) > 0 {
		return windows
	}
	for _, kind := range []string{"premium_interactions", "completions", "chat"} {
		limit := float64(r.MonthlyQuotas[kind])
		remaining, hasRemaining := r.LimitedUserQuotas[kind]
		if limit <= 0 || !hasRemaining {
			continue
		}
		used := limit - float64(remaining)
		if used < 0 {
			used = 0
		}
		windows = append(windows, plan.Window{
			Name:        windowName(kind),
			UsedPercent: used / limit * 100,
			Used:        used,
			Limit:       limit,
			Unit:        unitRequests,
			ResetsAt:    reset,
		})
	}
	return windows
}

func sortedSnapshotKinds(snapshots map[string]quotaSnapshot) []string {
	keys := make([]string, 0, len(snapshots))
	for key := range snapshots {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return snapshotRank(keys[i]) < snapshotRank(keys[j])
	})
	return keys
}

func snapshotRank(kind string) int {
	switch kind {
	case "premium_interactions":
		return 0
	case "chat":
		return 1
	default:
		return 2
	}
}

// snapshotWindow converts one quota snapshot. Entries that state no usage at
// all are placeholders and produce no window.
func snapshotWindow(kind string, snapshot quotaSnapshot, reset time.Time) (plan.Window, bool) {
	limit := float64(snapshot.Entitlement)
	remaining := float64(snapshot.Remaining)
	hasPercent := snapshot.PercentRemaining != nil
	if limit <= 0 && remaining <= 0 && !hasPercent {
		return plan.Window{}, false
	}
	used := 0.0
	percent := 0.0
	switch {
	case hasPercent:
		percent = 100 - *snapshot.PercentRemaining
		if limit > 0 {
			used = limit * percent / 100
		}
	case limit > 0:
		used = limit - remaining
		percent = used / limit * 100
	default:
		return plan.Window{}, false
	}
	if used < 0 {
		used = 0
	}
	if limit > 0 && used > limit {
		used = limit
	}
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	window := plan.Window{
		Name:        windowName(kind),
		UsedPercent: percent,
		ResetsAt:    reset,
	}
	if limit > 0 {
		window.Used = used
		window.Limit = limit
		window.Unit = unitRequests
	}
	return window, true
}

func windowName(kind string) string {
	switch kind {
	case "premium_interactions", "completions":
		return windowMonth
	case "chat":
		return "month-chat"
	default:
		return "month-" + strings.ReplaceAll(kind, "_", "-")
	}
}

// flexDate accepts RFC 3339 timestamps and bare calendar dates.
type flexDate time.Time

func (d *flexDate) UnmarshalJSON(raw []byte) error {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return nil
	}
	if text[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil
		}
		text = strings.TrimSpace(s)
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if parsed, err := time.Parse(layout, text); err == nil {
			*d = flexDate(parsed.UTC())
			return nil
		}
	}
	return nil
}
