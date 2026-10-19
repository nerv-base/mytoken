// Package glm reports the z.ai / BigModel GLM Coding Plan quota.
//
// The Coding Plan is API-key based: the key comes from Z_AI_API_KEY for the
// international region, or from BIGMODEL_API_KEY (and documented aliases and
// one-line key files) for the mainland-China region. With a key this package
// calls the same endpoint as the plan dashboard:
//
//	GET https://api.z.ai/api/monitor/usage/quota/limit
//	GET https://open.bigmodel.cn/api/monitor/usage/quota/limit
//	authorization: Bearer <key>
//
// data.limits[] entries of type TOKENS_LIMIT or CREDIT_LIMIT are the Coding
// Plan windows; unit/number state their duration (1=day, 3=hour, 5=month,
// 6=week), and percentage or the usage/currentValue/remaining counts state the
// used share. TIME_LIMIT describes the separate MCP tool lane and is not a
// token window, so it is ignored here. Payload shape per
// https://github.com/steipete/CodexBar (docs/zai.md).
package glm

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

// ProviderID is the stable id used in settings keys and by the dashboard.
const ProviderID = "glm"

const (
	quotaPath    = "/api/monitor/usage/quota/limit"
	maxBodyBytes = 64 << 10
	userAgent    = "mytoken-quota/1"

	limitTokens = "TOKENS_LIMIT"
	limitCredit = "CREDIT_LIMIT"
)

// Options configures a Provider. Every field is optional.
type Options struct {
	// Region forces "global" or "bigmodel-cn"; empty probes both.
	Region string
	// Transport performs HTTP requests; defaults to plan.DefaultTransport.
	Transport plan.Transport
	// BaseURL overrides the API origin (tests point it at a local server).
	BaseURL string
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
	// Getenv and Home default to the process environment.
	Getenv func(string) string
	Home   string
	GOOS   string
}

// Provider reports GLM Coding Plan usage.
type Provider struct {
	region    string
	transport plan.Transport
	baseURL   string
	now       func() time.Time
	getenv    func(string) string
	home      string
}

// New returns a GLM provider.
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
	transport := opts.Transport
	if transport == nil {
		transport = plan.DefaultTransport()
	}
	return &Provider{
		region:    normalizeRegion(opts.Region),
		transport: transport,
		baseURL:   strings.TrimRight(opts.BaseURL, "/"),
		now:       now,
		getenv:    getenv,
		home:      home,
	}
}

func normalizeRegion(region string) string {
	switch strings.ToLower(strings.TrimSpace(region)) {
	case RegionGlobal, "z.ai", "intl":
		return RegionGlobal
	case RegionBigModel, "bigmodel", "cn", "china":
		return RegionBigModel
	default:
		return ""
	}
}

// ID implements plan.Provider.
func (p *Provider) ID() string { return ProviderID }

// DisplayName implements plan.Provider.
func (p *Provider) DisplayName() string { return "z.ai / GLM" }

// Detect reports whether an API key is stored locally.
func (p *Provider) Detect(ctx context.Context) bool {
	token, _, err := p.credentials()
	return err == nil && token != ""
}

// Fetch returns the Coding Plan windows.
func (p *Provider) Fetch(ctx context.Context) (plan.Snapshot, error) {
	token, region, err := p.credentials()
	if err != nil {
		return plan.Snapshot{}, err
	}
	baseURL := p.baseURL
	if baseURL == "" {
		if region == RegionBigModel {
			baseURL = bigModelBaseURL
		} else {
			baseURL = globalBaseURL
		}
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	header.Set("Accept", "application/json")
	header.Set("User-Agent", userAgent)
	raw, err := plan.DoJSON(ctx, p.transport, http.MethodGet, baseURL+quotaPath, header, nil, maxBodyBytes)
	if err != nil {
		return plan.Snapshot{}, err
	}
	var envelope response
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return plan.Snapshot{}, plan.NewError(plan.ReasonMalformed, err)
	}
	if envelope.Data == nil {
		return plan.Snapshot{}, plan.NewError(plan.ReasonNoData, nil)
	}
	windows := envelope.Data.windows()
	if len(windows) == 0 {
		return plan.Snapshot{}, plan.NewError(plan.ReasonNoWindows, nil)
	}
	return plan.Snapshot{
		Provider:  ProviderID,
		Plan:      envelope.Data.planName(),
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

type response struct {
	Code    flexFloat `json:"code"`
	Success bool      `json:"success"`
	Data    *data     `json:"data"`
}

type data struct {
	PlanName    string  `json:"planName"`
	Plan        string  `json:"plan"`
	PlanType    string  `json:"plan_type"`
	PackageName string  `json:"packageName"`
	Level       string  `json:"level"`
	Limits      []limit `json:"limits"`
}

type limit struct {
	Type         string     `json:"type"`
	Unit         flexFloat  `json:"unit"`
	Number       flexFloat  `json:"number"`
	Percentage   *float64   `json:"percentage"`
	Usage        flexFloat  `json:"usage"`
	CurrentValue flexFloat  `json:"currentValue"`
	Remaining    *flexFloat `json:"remaining"`
	NextReset    flexFloat  `json:"nextResetTime"`
}

func (d *data) planName() string {
	for _, name := range []string{d.PlanName, d.Plan, d.PlanType, d.PackageName, d.Level} {
		if name = strings.TrimSpace(name); name != "" {
			return name
		}
	}
	return ""
}

// windows returns the Coding Plan token and credit windows, shortest first.
func (d *data) windows() []plan.Window {
	type entry struct {
		minutes int
		window  plan.Window
	}
	var entries []entry
	for _, item := range d.Limits {
		window, minutes, ok := item.window()
		if !ok {
			continue
		}
		entries = append(entries, entry{minutes: minutes, window: window})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].minutes < entries[j].minutes })
	windows := make([]plan.Window, 0, len(entries))
	for _, item := range entries {
		windows = append(windows, item.window)
	}
	return windows
}

// window converts one limit entry. TIME_LIMIT and unknown entries are skipped.
func (l limit) window() (plan.Window, int, bool) {
	unit := ""
	switch l.Type {
	case limitTokens:
		unit = "tokens"
	case limitCredit:
		unit = "credits"
	default:
		return plan.Window{}, 0, false
	}
	minutes := limitMinutes(int(l.Unit), float64(l.Number))
	percent, ok := l.usedPercent()
	if !ok {
		return plan.Window{}, 0, false
	}
	window := plan.Window{Name: windowName(minutes), UsedPercent: percent}
	if float64(l.Usage) > 0 {
		window.Limit = float64(l.Usage)
		window.Used = l.usedCount()
		window.Unit = unit
	}
	if reset := epochTime(float64(l.NextReset)); !reset.IsZero() {
		window.ResetsAt = reset
	}
	return window, minutes, true
}

// usedPercent prefers the usage/remaining counts when they are stated and
// falls back to the percentage field.
func (l limit) usedPercent() (float64, bool) {
	limit := float64(l.Usage)
	if limit > 0 {
		used := l.usedCount()
		if used >= 0 && (l.Remaining != nil || float64(l.CurrentValue) > 0) {
			return clampPercent(used / limit * 100), true
		}
	}
	if l.Percentage != nil {
		return clampPercent(*l.Percentage), true
	}
	return 0, false
}

// usedCount returns the consumed amount from remaining or currentValue.
func (l limit) usedCount() float64 {
	limit := float64(l.Usage)
	if l.Remaining != nil {
		if used := limit - float64(*l.Remaining); used >= 0 {
			return used
		}
	}
	return float64(l.CurrentValue)
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// limitMinutes converts unit/number to a window length in minutes. Units are
// 1=day, 3=hour, 5=month (30 days), 6=week; other units are unknown.
func limitMinutes(unit int, number float64) int {
	var per float64
	switch unit {
	case 1:
		per = 24 * 60
	case 3:
		per = 60
	case 5:
		per = 30 * 24 * 60
	case 6:
		per = 7 * 24 * 60
	default:
		return 0
	}
	if number <= 0 {
		return 0
	}
	return int(per * number)
}

func windowName(minutes int) string {
	switch minutes {
	case 5 * 60:
		return "5h"
	case 24 * 60:
		return "day"
	case 7 * 24 * 60:
		return "week"
	case 30 * 24 * 60:
		return "month"
	case 0:
		return "plan"
	}
	if minutes%(24*60) == 0 {
		return strconv.Itoa(minutes/(24*60)) + "d"
	}
	if minutes%60 == 0 {
		return strconv.Itoa(minutes/60) + "h"
	}
	return strconv.Itoa(minutes) + "m"
}

// epochTime reads epoch milliseconds or seconds.
func epochTime(v float64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	if v > 1e12 {
		return time.UnixMilli(int64(v)).UTC()
	}
	return time.Unix(int64(v), 0).UTC()
}
