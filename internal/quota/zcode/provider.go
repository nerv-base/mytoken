package zcode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

// ProviderID is the stable id used in settings keys and by the dashboard.
const ProviderID = "zcode"

const (
	// balancePath is the ZCode plan balance endpoint.
	balancePath = "/api/v1/zcode-plan/billing/balance"
	// defaultBaseURL is the ZCode gateway that serves the Start Plan.
	defaultBaseURL = "https://zcode.z.ai"
	// maxBodyBytes caps the response we read.
	maxBodyBytes = 64 << 10
	// userAgent identifies MyToken to the gateway.
	userAgent = "mytoken-quota/1"
	// defaultPlanName labels the snapshot when the payload names no plan.
	defaultPlanName = "ZCode Start Plan"
)

// Options configures a Provider. Every field is optional.
type Options struct {
	// Transport performs HTTP requests; defaults to plan.DefaultTransport.
	Transport plan.Transport
	// BaseURL overrides the API origin (tests point it at a local server).
	BaseURL string
	// ConfigDir overrides the directory holding credentials.json.
	ConfigDir string
	// DeviceMid overrides the stored device id (tests only).
	DeviceMid string
	// AppVersion, when set, is sent as the app_version query parameter.
	AppVersion string
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
	// Getenv and Home default to the process environment.
	Getenv func(string) string
	Home   string
	GOOS   string
}

// Provider reports the ZCode Start Plan quota.
type Provider struct {
	transport plan.Transport
	baseURL   string
	configDir string
	deviceMid string
	version   string
	now       func() time.Time
	getenv    func(string) string
	home      string
	goos      string
}

// New returns a ZCode provider.
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
		goos = runtime.GOOS
	}
	transport := opts.Transport
	if transport == nil {
		transport = plan.DefaultTransport()
	}
	return &Provider{
		transport: transport,
		baseURL:   strings.TrimRight(opts.BaseURL, "/"),
		configDir: strings.TrimSpace(opts.ConfigDir),
		deviceMid: strings.TrimSpace(opts.DeviceMid),
		version:   strings.TrimSpace(opts.AppVersion),
		now:       now,
		getenv:    getenv,
		home:      home,
		goos:      goos,
	}
}

// ID implements plan.Provider.
func (p *Provider) ID() string { return ProviderID }

// DisplayName implements plan.Provider.
func (p *Provider) DisplayName() string { return "ZCode" }

// Detect reports whether a ZCode sign-in is stored and readable locally.
func (p *Provider) Detect(ctx context.Context) bool {
	c, err := p.credentials()
	return err == nil && c.token != ""
}

// Fetch returns the Start Plan windows.
func (p *Provider) Fetch(ctx context.Context) (plan.Snapshot, error) {
	c, err := p.credentials()
	if err != nil {
		return plan.Snapshot{}, err
	}
	baseURL := p.baseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	target := baseURL + balancePath
	if p.version != "" {
		q := url.Values{}
		q.Set("app_version", p.version)
		target += "?" + q.Encode()
	}
	deviceMid := p.deviceMid
	if deviceMid == "" {
		deviceMid = c.deviceMid
	}
	if deviceMid == "" {
		deviceMid = zeroDeviceMid
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.token)
	header.Set("Accept", "application/json")
	header.Set("User-Agent", userAgent)
	header.Set("X-Device-Mid", deviceMid)
	raw, err := plan.DoJSON(ctx, p.transport, http.MethodGet, target, header, nil, maxBodyBytes)
	if err != nil {
		return plan.Snapshot{}, err
	}
	var envelope response
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return plan.Snapshot{}, plan.NewError(plan.ReasonMalformed, err)
	}
	if err := envelope.businessError(); err != nil {
		return plan.Snapshot{}, err
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
	Code flexFloat `json:"code"`
	Msg  string    `json:"msg"`
	Data *data     `json:"data"`
}

type data struct {
	ServerTime flexFloat `json:"server_time"`
	Plans      []planRec `json:"plans"`
	Balances   []balance `json:"balances"`
}

type planRec struct {
	Name   string `json:"name"`
	PlanID string `json:"plan_id"`
	Status string `json:"status"`
}

type balance struct {
	EntitlementID  string    `json:"entitlement_id"`
	ShowName       string    `json:"show_name"`
	UnitType       string    `json:"unit_type"`
	Meter          string    `json:"meter"`
	TotalUnits     flexFloat `json:"total_units"`
	UsedUnits      flexFloat `json:"used_units"`
	RemainingUnits flexFloat `json:"remaining_units"`
	PeriodStart    flexFloat `json:"period_start"`
	PeriodEnd      flexFloat `json:"period_end"`
	ExpiresAt      flexFloat `json:"expires_at"`
}

// businessError maps a non-zero envelope code onto a plan reason.
func (r response) businessError() error {
	switch float64(r.Code) {
	case 0:
		return nil
	case 401, 403, 1001:
		return plan.NewError(plan.ReasonUnauthorized, nil)
	case 429:
		return plan.NewError(plan.ReasonRateLimited, nil)
	}
	return plan.NewError(plan.ReasonHTTPStatus, nil)
}

// planName is the plan's display name, falling back to a stable label.
func (d *data) planName() string {
	for _, item := range d.Plans {
		if name := strings.TrimSpace(item.Name); name != "" {
			return name
		}
	}
	return defaultPlanName
}

// windows converts the plan's per-model grants into quota windows, the
// soonest to expire first.
func (d *data) windows() []plan.Window {
	seen := map[string]bool{}
	var out []plan.Window
	for _, b := range d.Balances {
		name := strings.TrimSpace(b.ShowName)
		if name == "" {
			continue
		}
		if seen[name] {
			continue
		}
		total := float64(b.TotalUnits)
		if total <= 0 {
			continue
		}
		seen[name] = true
		used := float64(b.UsedUnits)
		if used < 0 {
			used = 0
		}
		w := plan.Window{
			Name:        name,
			Limit:       total,
			Used:        used,
			Unit:        unitName(b.UnitType),
			UsedPercent: clampPercent(used / total * 100),
		}
		if reset := epochTime(float64(b.PeriodEnd)); !reset.IsZero() {
			w.ResetsAt = reset
		} else if reset := epochTime(float64(b.ExpiresAt)); !reset.IsZero() {
			w.ResetsAt = reset
		}
		out = append(out, w)
	}
	return out
}

// unitName is the unit as the dashboard shows it.
func unitName(unitType string) string {
	switch strings.ToLower(strings.TrimSpace(unitType)) {
	case "token", "tokens":
		return "tokens"
	case "credit", "credits":
		return "credits"
	case "request", "requests":
		return "requests"
	}
	return strings.TrimSpace(unitType)
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

// epochTime reads epoch seconds or milliseconds.
func epochTime(v float64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	if v > 1e12 {
		return time.UnixMilli(int64(v)).UTC()
	}
	return time.Unix(int64(v), 0).UTC()
}
