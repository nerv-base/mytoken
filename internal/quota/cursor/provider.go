// Package cursor reports the Cursor plan quota.
//
// Cursor keeps the signed-in user's token in its global state database
// (state.vscdb, table ItemTable, key cursorAuth/accessToken). When the token is
// present this package copies the database aside, reads the token, and asks
// Cursor's DashboardService for the current billing period. The endpoint is the
// Connect RPC the Cursor dashboard itself calls:
//
//	POST https://api2.cursor.sh/aiserver.v1.DashboardService/GetCurrentPeriodUsage
//	Authorization: Bearer <token>
//	Connect-Protocol-Version: 1
//
// The response shape (planUsage in cents plus totalPercentUsed, and
// billingCycleEnd in epoch milliseconds) is documented by
// https://github.com/PowerUserZ/OpenTokenUsage (docs/providers/cursor.md) and
// matches Cursor's own dashboard payloads.
//
// The token is short-lived and deliberately not refreshed: refreshing would
// rewrite Cursor's stored credential, so an expired token is reported as an
// authorization failure instead.
package cursor

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

// ProviderID is the stable id used in settings keys and by the dashboard.
const ProviderID = "cursor"

// BaseURL is Cursor's API origin.
const BaseURL = "https://api2.cursor.sh"

// UserAgent identifies this client.
const UserAgent = "mytoken-quota/1"

const (
	periodUsagePath = "/aiserver.v1.DashboardService/GetCurrentPeriodUsage"
	planInfoPath    = "/aiserver.v1.DashboardService/GetPlanInfo"
	maxBodyBytes    = 64 << 10
)

// Options configures a Provider. Every field is optional.
type Options struct {
	// StatePaths overrides the state database locations.
	StatePaths []string
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

// Provider reports Cursor's monthly included usage.
type Provider struct {
	paths     []string
	transport plan.Transport
	baseURL   string
	now       func() time.Time
}

// New returns a Cursor provider.
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
	paths := opts.StatePaths
	if paths == nil {
		paths = stateDBPaths(getenv, home, goos)
	}
	return &Provider{paths: paths, transport: transport, baseURL: strings.TrimRight(baseURL, "/"), now: now}
}

// ID implements plan.Provider.
func (p *Provider) ID() string { return ProviderID }

// DisplayName implements plan.Provider.
func (p *Provider) DisplayName() string { return "Cursor" }

// Detect reports whether Cursor's state database holds an access token.
func (p *Provider) Detect(ctx context.Context) bool {
	token, err := readAccessToken(ctx, p.paths)
	return err == nil && token != ""
}

// Fetch returns the current billing period's included usage.
func (p *Provider) Fetch(ctx context.Context) (plan.Snapshot, error) {
	token, err := readAccessToken(ctx, p.paths)
	if err != nil {
		return plan.Snapshot{}, err
	}
	if token == "" {
		return plan.Snapshot{}, plan.NewError(plan.ReasonNoCredentials, nil)
	}
	raw, err := p.post(ctx, periodUsagePath, token)
	if err != nil {
		return plan.Snapshot{}, err
	}
	var usage usageResponse
	if err := json.Unmarshal(raw, &usage); err != nil {
		return plan.Snapshot{}, plan.NewError(plan.ReasonMalformed, err)
	}
	windows := usage.windows()
	if len(windows) == 0 {
		return plan.Snapshot{}, plan.NewError(plan.ReasonNoWindows, nil)
	}
	planName := strings.TrimSpace(usage.MembershipType)
	if planName == "" {
		planName = p.planName(ctx, token)
	}
	return plan.Snapshot{
		Provider:  ProviderID,
		Plan:      planName,
		Windows:   windows,
		FetchedAt: p.now().UTC(),
		Source:    plan.SourceAPI,
	}, nil
}

// planName asks for the plan label; failure is not fatal because the quota is
// still usable without it.
func (p *Provider) planName(ctx context.Context, token string) string {
	raw, err := p.post(ctx, planInfoPath, token)
	if err != nil {
		return ""
	}
	var info struct {
		PlanInfo *struct {
			PlanName string `json:"planName"`
		} `json:"planInfo"`
	}
	if err := json.Unmarshal(raw, &info); err != nil || info.PlanInfo == nil {
		return ""
	}
	return strings.TrimSpace(info.PlanInfo.PlanName)
}

func (p *Provider) post(ctx context.Context, path, token string) ([]byte, error) {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	header.Set("Content-Type", "application/json")
	header.Set("Connect-Protocol-Version", "1")
	header.Set("User-Agent", UserAgent)
	return plan.DoJSON(ctx, p.transport, http.MethodPost, p.baseURL+path, header, []byte("{}"), maxBodyBytes)
}

// flexAmount accepts a number stated as a JSON number or a JSON string.
type flexAmount float64

func (f *flexAmount) UnmarshalJSON(raw []byte) error {
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
	*f = flexAmount(v)
	return nil
}

type planUsage struct {
	Limit            flexAmount `json:"limit"`
	TotalSpend       flexAmount `json:"totalSpend"`
	IncludedSpend    flexAmount `json:"includedSpend"`
	BonusSpend       flexAmount `json:"bonusSpend"`
	Remaining        flexAmount `json:"remaining"`
	TotalPercentUsed *float64   `json:"totalPercentUsed"`
	AutoPercentUsed  *float64   `json:"autoPercentUsed"`
	APIPercentUsed   *float64   `json:"apiPercentUsed"`
}

type individualPlan struct {
	Used             flexAmount `json:"used"`
	Limit            flexAmount `json:"limit"`
	Remaining        flexAmount `json:"remaining"`
	TotalPercentUsed *float64   `json:"totalPercentUsed"`
	Breakdown        *struct {
		Included flexAmount `json:"included"`
		Bonus    flexAmount `json:"bonus"`
		Total    flexAmount `json:"total"`
	} `json:"breakdown"`
}

type usageResponse struct {
	BillingCycleEnd   flexAmount `json:"billingCycleEnd"`
	BillingCycleStart flexAmount `json:"billingCycleStart"`
	PlanUsage         *planUsage `json:"planUsage"`
	IndividualUsage   *struct {
		Plan *individualPlan `json:"plan"`
	} `json:"individualUsage"`
	MembershipType string `json:"membershipType"`
}

// windows maps the payload onto a single "month" window. Amounts are cents in
// the payload and US dollars in the window. A payload that states no usage at
// all yields no window rather than a zero one.
func (r usageResponse) windows() []plan.Window {
	limit, used, pct := 0.0, 0.0, (*float64)(nil)
	if pu := r.PlanUsage; pu != nil {
		limit = float64(pu.Limit)
		used = float64(pu.TotalSpend)
		if float64(pu.Remaining) > 0 && limit > 0 {
			used = limit - float64(pu.Remaining)
		}
		pct = pu.TotalPercentUsed
	} else if iu := r.IndividualUsage; iu != nil && iu.Plan != nil {
		limit = float64(iu.Plan.Limit)
		used = float64(iu.Plan.Used)
		if used == 0 && float64(iu.Plan.Remaining) > 0 && limit > 0 {
			used = limit - float64(iu.Plan.Remaining)
		}
		pct = iu.Plan.TotalPercentUsed
	}
	if pct == nil && limit > 0 {
		v := used / limit * 100
		pct = &v
	}
	if pct == nil || (limit <= 0 && *pct == 0) {
		return nil
	}
	win := plan.Window{Name: "month", UsedPercent: *pct}
	if limit > 0 {
		win.Limit = limit / 100
		win.Used = used / 100
		win.Unit = "USD"
	}
	if end := epochTime(float64(r.BillingCycleEnd)); !end.IsZero() {
		win.ResetsAt = end
	}
	return []plan.Window{win}
}

// epochTime reads epoch milliseconds (the DashboardService unit) or seconds.
func epochTime(v float64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	if v > 1e12 {
		return time.UnixMilli(int64(v)).UTC()
	}
	return time.Unix(int64(v), 0).UTC()
}
