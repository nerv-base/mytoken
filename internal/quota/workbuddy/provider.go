// Package workbuddy reports the plan quota of WorkBuddy and WorkBuddy AI.
//
// WorkBuddy is Tencent's CodeBuddy desktop app. Its sign-in is a plaintext JSON
// file under CodeBuddyExtension/Data/Public/auth/workbuddy-desktop.info (the
// international build uses workbuddy-desktop-ai.info); the token is read from
// there on every fetch and is never logged or persisted. WorkBuddy 5.6 and
// later may seal the token as a {$wbEncrypted: ...} object — such an install is
// treated as signed out.
//
// The billing endpoint is the one the app's own usage page calls. It has no
// /v2 prefix:
//
//	POST https://copilot.tencent.com/billing/meter/get-user-resource-summary
//	POST https://copilot.tencent.com/billing/meter/get-enterprise-user-usage
//
// The User-Agent must look like WorkBuddy/<version>; the gateway answers an
// unknown client with code 10085. Tokens are deliberately not refreshed: the
// desktop app and this process would otherwise race for the same refresh token
// and sign each other out.
package workbuddy

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

// Provider ids.
const (
	ProviderID   = "workbuddy"
	AIProviderID = "workbuddy-ai"
)

// Endpoints and client identity.
const (
	BaseURL      = "https://copilot.tencent.com"
	AIBaseURL    = "https://www.workbuddy.ai"
	UserAgent    = "WorkBuddy/5.5.6"
	maxBodyBytes = 64 << 10

	summaryPath    = "/billing/meter/get-user-resource-summary"
	enterprisePath = "/billing/meter/get-enterprise-user-usage"
)

var errNoCredentials = plan.NewError(plan.ReasonNoCredentials, nil)

// Options configures a Provider. Every field is optional.
type Options struct {
	// AuthPath overrides the desktop sign-in file.
	AuthPath string
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

// Provider reports one WorkBuddy deployment's quota.
type Provider struct {
	id        string
	name      string
	baseURL   string
	authPath  string
	transport plan.Transport
	now       func() time.Time
}

// New returns the WorkBuddy (China) provider.
func New(opts Options) *Provider { return newProvider(false, opts) }

// NewAI returns the WorkBuddy AI (international) provider.
func NewAI(opts Options) *Provider { return newProvider(true, opts) }

func newProvider(ai bool, opts Options) *Provider {
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
	authPath := opts.AuthPath
	if authPath == "" {
		authPath = defaultAuthPath(ai, goos, home, getenv)
	}
	base := opts.BaseURL
	if base == "" {
		if ai {
			base = AIBaseURL
		} else {
			base = BaseURL
		}
	}
	p := &Provider{
		id:        ProviderID,
		name:      "WorkBuddy",
		baseURL:   strings.TrimRight(base, "/"),
		authPath:  authPath,
		transport: transport,
		now:       now,
	}
	if ai {
		p.id = AIProviderID
		p.name = "WorkBuddy AI"
	}
	return p
}

// ID implements plan.Provider.
func (p *Provider) ID() string { return p.id }

// DisplayName implements plan.Provider.
func (p *Provider) DisplayName() string { return p.name }

// Detect reports whether the desktop app holds a readable (unencrypted)
// sign-in. It reads only the local file.
func (p *Provider) Detect(context.Context) bool {
	s, err := readStored(p.authPath)
	if err != nil || strings.TrimSpace(s.Account.UID) == "" {
		return false
	}
	_, ok := tokenString(s.Auth.AccessToken)
	return ok
}

// Fetch reads the stored sign-in and asks the billing API.
func (p *Provider) Fetch(ctx context.Context) (plan.Snapshot, error) {
	c, err := p.credentials()
	if err != nil {
		return plan.Snapshot{}, err
	}
	if !c.expiresAt.IsZero() && !p.now().Before(c.expiresAt) {
		return plan.Snapshot{}, plan.NewError(plan.ReasonTokenExpired, nil)
	}

	h := p.headers(c)
	raw, err := plan.DoJSON(ctx, p.transport, http.MethodPost, p.baseURL+summaryPath, h, []byte(`{}`), maxBodyBytes)
	if err != nil {
		return plan.Snapshot{}, err
	}
	var r summaryReply
	if err := json.Unmarshal(raw, &r); err != nil {
		return plan.Snapshot{}, plan.NewError(plan.ReasonMalformed, err)
	}
	if err := businessError(r.Code); err != nil {
		return plan.Snapshot{}, err
	}
	if r.Data == nil {
		return plan.Snapshot{}, plan.NewError(plan.ReasonNoWindows, nil)
	}

	var total, used float64
	for _, pkg := range r.Data.Packages {
		total += float64(pkg.CycleTotalCapacity)
		used += float64(pkg.CycleUsedCapacity)
	}
	var reset time.Time
	if total <= 0 {
		// Enterprise (team) accounts answer the summary with no packages at
		// all; their cycle is on the enterprise meter instead.
		if entTotal, entUsed, entReset, err := p.enterprise(ctx, h); err == nil {
			total, used, reset = entTotal, entUsed, entReset
		}
	}
	if total <= 0 {
		return plan.Snapshot{}, plan.NewError(plan.ReasonNoWindows, nil)
	}

	percent := math.Min(100, math.Max(0, used/total*100))
	planName := "Free"
	if r.Data.IsPaidUser {
		planName = "Pro"
	}
	if name := strings.TrimSpace(r.Data.SubscriptionPackageName); name != "" {
		planName = name
	}
	return plan.Snapshot{
		Provider: p.id,
		Plan:     planName,
		Windows: []plan.Window{{
			Name:        "Credits",
			UsedPercent: percent,
			Used:        used,
			Limit:       total,
			Remaining:   math.Max(0, total-used),
			Unit:        "credits",
			ResetsAt:    reset,
		}},
		FetchedAt: p.now().UTC(),
		Source:    plan.SourceAPI,
	}, nil
}

func (p *Provider) enterprise(ctx context.Context, h http.Header) (total, used float64, reset time.Time, err error) {
	raw, err := plan.DoJSON(ctx, p.transport, http.MethodPost, p.baseURL+enterprisePath, h, []byte(`{}`), maxBodyBytes)
	if err != nil {
		return 0, 0, time.Time{}, err
	}
	var r enterpriseReply
	if err := json.Unmarshal(raw, &r); err != nil {
		return 0, 0, time.Time{}, plan.NewError(plan.ReasonMalformed, err)
	}
	if err := businessError(r.Code); err != nil {
		return 0, 0, time.Time{}, err
	}
	if r.Data == nil {
		return 0, 0, time.Time{}, plan.NewError(plan.ReasonNoWindows, nil)
	}
	total = float64(r.Data.LimitNum)
	used = float64(r.Data.Credit)
	if total <= 0 {
		return 0, 0, time.Time{}, plan.NewError(plan.ReasonNoWindows, nil)
	}
	reset = parseBeijing(r.Data.CycleResetTime)
	return total, used, reset, nil
}

func (p *Provider) headers(c credential) http.Header {
	domain := c.domain
	if strings.Contains(domain, "://") {
		if u, err := url.Parse(domain); err == nil {
			domain = u.Host
		}
	}
	if domain == "" {
		if u, err := url.Parse(p.baseURL); err == nil {
			domain = u.Host
		}
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("User-Agent", UserAgent)
	h.Set("Authorization", "Bearer "+c.access)
	h.Set("X-User-Id", c.uid)
	h.Set("X-Domain", domain)
	h.Set("X-Product", "SaaS")
	h.Set("X-IDE-Type", "WorkBuddy")
	if c.enterpriseID != "" {
		h.Set("X-Enterprise-Id", c.enterpriseID)
		h.Set("X-Tenant-Id", c.enterpriseID)
	}
	return h
}

type summaryReply struct {
	Code int          `json:"code"`
	Msg  string       `json:"msg"`
	Data *summaryData `json:"data"`
}

type summaryData struct {
	IsPaidUser              bool      `json:"IsPaidUser"`
	TotalCredits            flexFloat `json:"TotalCredits"`
	SubscriptionPackageName string    `json:"SubscriptionPackageName"`
	Packages                []struct {
		PackageCode         string    `json:"PackageCode"`
		CycleTotalCapacity  flexFloat `json:"CycleTotalCapacity"`
		CycleUsedCapacity   flexFloat `json:"CycleUsedCapacity"`
		CycleRemainCapacity flexFloat `json:"CycleRemainCapacity"`
		CapacityUnit        string    `json:"CapacityUnit"`
	} `json:"Packages"`
}

type enterpriseReply struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data *enterpriseData `json:"data"`
}

type enterpriseData struct {
	LimitNum       flexFloat `json:"limitNum"`
	Credit         flexFloat `json:"credit"`
	CycleStartTime string    `json:"cycleStartTime"`
	CycleResetTime string    `json:"cycleResetTime"`
}

// businessError maps WorkBuddy's 200-with-code failures.
func businessError(code int) error {
	switch code {
	case 0:
		return nil
	case 11001:
		return plan.NewError(plan.ReasonUnauthorized, nil)
	case 10085:
		return plan.NewError(plan.ReasonHTTPStatus, nil)
	case 11140:
		return plan.NewError(plan.ReasonForbidden, nil)
	default:
		return plan.NewError(plan.ReasonHTTPStatus, nil)
	}
}

// parseBeijing parses WorkBuddy's "YYYY-MM-DD HH:MM:SS" cycle instants, which
// are stated in UTC+8.
func parseBeijing(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.FixedZone("UTC+8", 8*3600))
	if err != nil {
		return time.Time{}
	}
	return t
}

// flexFloat accepts a JSON number or numeric string.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*f = flexFloat(v)
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
