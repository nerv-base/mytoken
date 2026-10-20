package zcode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
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
	windows, err := envelope.Data.windows()
	if err != nil {
		return plan.Snapshot{}, err
	}
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

// flexFloat accepts a number stated as a JSON number or a JSON string. A value
// that cannot be read at all is remembered instead of being silently zeroed, so
// a broken grant surfaces as a malformed response rather than as a window that
// quietly disappears from the card.
type flexFloat struct {
	Value float64
	Bad   bool
}

func (f *flexFloat) UnmarshalJSON(raw []byte) error {
	f.Value, f.Bad = 0, false
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return nil
	}
	if text[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			f.Bad = true
			return nil
		}
		text = strings.TrimSpace(s)
		if text == "" {
			return nil
		}
	}
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		f.Bad = true
		return nil
	}
	f.Value = v
	return nil
}

type response struct {
	Code flexFloat `json:"code"`
	Msg  string    `json:"msg"`
	Data *data     `json:"data"`
}

type data struct {
	Plans    []planRec `json:"plans"`
	Balances []balance `json:"balances"`
}

type planRec struct {
	Name string `json:"name"`
}

type balance struct {
	ShowName   string    `json:"show_name"`
	UnitType   string    `json:"unit_type"`
	TotalUnits flexFloat `json:"total_units"`
	UsedUnits  flexFloat `json:"used_units"`
	PeriodEnd  flexFloat `json:"period_end"`
	ExpiresAt  flexFloat `json:"expires_at"`
}

// businessError maps a non-zero envelope code onto a plan reason.
func (r response) businessError() error {
	if r.Code.Bad {
		return plan.NewError(plan.ReasonMalformed, fmt.Errorf("zcode: unreadable envelope code %q", r.Msg))
	}
	switch r.Code.Value {
	case 0:
		return nil
	case 401, 403, 1001:
		return plan.NewError(plan.ReasonUnauthorized, nil)
	case 429:
		return plan.NewError(plan.ReasonRateLimited, nil)
	}
	// A business code arrives inside a 200 response, so it is not an HTTP
	// status: the endpoint answered, it just had no plan to report.
	return plan.NewError(plan.ReasonNoData, fmt.Errorf("zcode: business code %v: %s", r.Code.Value, r.Msg))
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

// windows converts the plan's per-model grants into quota windows, soonest to
// reset first.
//
// The payload may carry more than one grant for a model. Grants that reset at
// the same instant are one bucket and are summed; when they reset at different
// instants, the current period is the window the dashboard is about, so the
// later grant is left for its own period rather than being added to today's
// allowance.
func (d *data) windows() ([]plan.Window, error) {
	var out []plan.Window
	index := map[string]int{}
	for _, b := range d.Balances {
		name := strings.TrimSpace(b.ShowName)
		if name == "" {
			continue
		}
		if b.TotalUnits.Bad || b.UsedUnits.Bad || b.PeriodEnd.Bad || b.ExpiresAt.Bad {
			return nil, plan.NewError(plan.ReasonMalformed, fmt.Errorf("zcode: unreadable grant for %s", name))
		}
		total := b.TotalUnits.Value
		if total <= 0 {
			continue
		}
		used := b.UsedUnits.Value
		if used < 0 {
			used = 0
		}
		reset := resetInstant(b)
		if at, ok := index[name]; ok {
			kept := &out[at]
			if kept.ResetsAt.Equal(reset) {
				kept.Limit += total
				kept.Used += used
				kept.UsedPercent = clampPercent(kept.Used / kept.Limit * 100)
				continue
			}
			if !kept.ResetsAt.IsZero() && (reset.IsZero() || kept.ResetsAt.Before(reset)) {
				continue
			}
			kept.Limit, kept.Used, kept.Unit = total, used, unitName(b.UnitType)
			kept.ResetsAt = reset
			kept.UsedPercent = clampPercent(used / total * 100)
			continue
		}
		index[name] = len(out)
		out = append(out, plan.Window{
			Name:        name,
			Limit:       total,
			Used:        used,
			Unit:        unitName(b.UnitType),
			UsedPercent: clampPercent(used / total * 100),
			ResetsAt:    reset,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].ResetsAt, out[j].ResetsAt
		switch {
		case a.Equal(b):
			return out[i].Name < out[j].Name
		case a.IsZero():
			return false
		case b.IsZero():
			return true
		}
		return a.Before(b)
	})
	return out, nil
}

// resetInstant is when the grant's current period ends.
func resetInstant(b balance) time.Time {
	if reset := epochTime(b.PeriodEnd.Value); !reset.IsZero() {
		return reset
	}
	return epochTime(b.ExpiresAt.Value)
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
