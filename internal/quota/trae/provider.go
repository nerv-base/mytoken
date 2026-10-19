// Package trae reports the plan quota of Trae CN and Trae Global.
//
// Trae is a VS Code fork; its sign-in lives in the IDE's globalStorage
// (storage.json) under iCubeAuthInfo://icube.cloudide, wrapped in the client's
// own AES codec (see codec.go). When the IDE is not signed in, the package
// falls back to magpie's sign-in for the same site (~/.config/magpie/
// plugin-auth.json). Tokens are read from disk on every fetch and are never
// logged or persisted.
//
// The CN endpoint is the one the IDE's own usage page calls:
//
//	POST https://api.trae.cn/trae/api/v2/pay/ide_user_ent_usage
//
// and the global endpoint is the dollar-billing entitlement list:
//
//	POST https://api-sg-central.trae.ai/trae/api/v1/pay/user_current_entitlement_list
//
// Trae tokens last about eight days and are deliberately not refreshed:
// renewing through the IDE's exchange endpoint rebinds the token to a device
// and would disturb the desktop client, so an expired token is reported as
// token_expired instead.
package trae

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

// Provider ids.
const (
	ProviderID       = "trae-cn"
	GlobalProviderID = "trae-global"
)

// Endpoints and client identity.
const (
	BaseURL         = "https://api.trae.cn"
	GlobalBaseURL   = "https://api-sg-central.trae.ai"
	GlobalUSBaseURL = "https://api-us-east.trae.ai"

	cnUsagePath     = "/trae/api/v2/pay/ide_user_ent_usage"
	globalUsagePath = "/trae/api/v1/pay/user_current_entitlement_list"

	appID             = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	clientVersion     = "0.1.69"
	clientVersionCode = "20260917"
	ideVersionCode    = "20260401"
	cnIDEVersion      = "3.3.65"
	globalIDEVersion  = "3.5.51"

	maxBodyBytes = 64 << 10
)

// UserAgent is the Electron shell User-Agent Trae CN's pages expect.
const UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) TRAE-SOLO-CN/" + clientVersion + " Chrome/120.0.6099.291 Electron/28.2.10 Safari/537.36"

// GlobalUserAgent identifies the international shell.
const GlobalUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) TRAE-SOLO/" + clientVersion + " Chrome/120.0.6099.291 Electron/28.2.10 Safari/537.36"

// Options configures a Provider. Every field is optional.
type Options struct {
	// StoragePaths overrides the storage.json locations.
	StoragePaths []string
	// MagpiePath overrides ~/.config/magpie/plugin-auth.json.
	MagpiePath string
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

// Provider reports one Trae deployment's quota.
type Provider struct {
	id             string
	name           string
	global         bool
	baseURL        string
	baseOverridden bool
	usagePath      string
	body           []byte
	userAgent      string
	ideVersion     string
	storagePaths   []string
	magpiePath     string
	magpieKey      string
	transport      plan.Transport
	now            func() time.Time
}

// New returns the Trae CN provider.
func New(opts Options) *Provider { return newProvider(false, opts) }

// NewGlobal returns the Trae Global provider.
func NewGlobal(opts Options) *Provider { return newProvider(true, opts) }

func newProvider(global bool, opts Options) *Provider {
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
	paths := opts.StoragePaths
	if paths == nil {
		paths = defaultStoragePaths(global, goos, home, getenv)
	}
	magpie := opts.MagpiePath
	if magpie == "" {
		magpie = defaultMagpiePath(home, getenv)
	}
	base := opts.BaseURL
	if base == "" {
		if global {
			base = GlobalBaseURL
		} else {
			base = BaseURL
		}
	}
	base = strings.TrimRight(base, "/")

	p := &Provider{
		id:             ProviderID,
		name:           "Trae CN",
		global:         global,
		baseURL:        base,
		baseOverridden: opts.BaseURL != "",
		usagePath:      cnUsagePath,
		body:           []byte(`{"require_usage":true,"req_source":0}`),
		userAgent:      UserAgent,
		ideVersion:     cnIDEVersion,
		storagePaths:   paths,
		magpiePath:     magpie,
		magpieKey:      "trae-cn",
		transport:      transport,
		now:            now,
	}
	if global {
		p.id = GlobalProviderID
		p.name = "Trae Global"
		p.usagePath = globalUsagePath
		p.body = []byte(`{}`)
		p.userAgent = GlobalUserAgent
		p.ideVersion = globalIDEVersion
		p.magpieKey = "trae-global"
	}
	return p
}

// ID implements plan.Provider.
func (p *Provider) ID() string { return p.id }

// DisplayName implements plan.Provider.
func (p *Provider) DisplayName() string { return p.name }

// Detect reports whether the IDE's storage.json exists, or magpie holds a
// sign-in for this site. It reads only local files.
func (p *Provider) Detect(context.Context) bool {
	for _, path := range p.storagePaths {
		if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
			return true
		}
	}
	_, ok := p.magpieCredentials()
	return ok
}

// Fetch reads the stored sign-in and asks the deployment's usage endpoint.
func (p *Provider) Fetch(ctx context.Context) (plan.Snapshot, error) {
	c, err := p.credentials()
	if err != nil {
		return plan.Snapshot{}, err
	}
	if !c.expiresAt.IsZero() && !p.now().Before(c.expiresAt) {
		return plan.Snapshot{}, plan.NewError(plan.ReasonTokenExpired, nil)
	}

	base := p.baseURL
	if p.global && !p.baseOverridden {
		region := strings.ToUpper(strings.TrimSpace(c.region))
		if strings.HasPrefix(region, "US") || strings.Contains(strings.ToLower(c.host), "api-us") {
			base = GlobalUSBaseURL
		}
	}

	raw, err := plan.DoJSON(ctx, p.transport, http.MethodPost, base+p.usagePath, p.headers(c), p.body, maxBodyBytes)
	if err != nil {
		return plan.Snapshot{}, err
	}
	if p.global {
		return p.globalSnapshot(raw)
	}
	return p.cnSnapshot(raw)
}

func (p *Provider) headers(c credential) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("Authorization", "Cloud-IDE-JWT "+c.token)
	h.Set("X-Cloudide-Token", c.token)
	h.Set("x-ide-token", c.token)
	h.Set("x-uid", c.uid)
	h.Set("x-app-id", appID)
	if c.deviceID != "" {
		h.Set("x-device-id", c.deviceID)
	}
	if c.machineID != "" {
		h.Set("x-machine-id", c.machineID)
	}
	h.Set("x-app-version", clientVersion)
	h.Set("x-app-version-code", clientVersionCode)
	h.Set("x-ide-version", p.ideVersion)
	h.Set("x-ide-version-code", ideVersionCode)
	h.Set("x-ide-version-type", "stable")
	h.Set("x-device-type", "windows")
	h.Set("x-os-version", "Windows 10")
	h.Set("x-system-type", "Windows")
	h.Set("User-Agent", p.userAgent)
	return h
}

// usageReply is Trae CN's credits reply.
type usageReply struct {
	Code             int  `json:"code"`
	IsCreditsBilling bool `json:"is_credits_billing"`
	IsDollarBilling  bool `json:"is_dollar_usage_billing"`
	UsageSummary     struct {
		Consumed flexFloat `json:"consumed_amount"`
		Ratio    flexFloat `json:"consumption_ratio"`
		Total    flexFloat `json:"total_amount"`
	} `json:"usage_summary"`
	Packs []pack `json:"user_entitlement_pack_list"`
}

type pack struct {
	DisplayDesc string `json:"display_desc"`
	GroupName   string `json:"group_name"`
	EndTime     int64  `json:"end_time"`
	ExpireTime  int64  `json:"expire_time"`
	Base        struct {
		Quota struct {
			CreditsLimit *flexFloat `json:"credits_limit"`
		} `json:"quota"`
		EndTime    int64 `json:"end_time"`
		ExpireTime int64 `json:"expire_time"`
	} `json:"entitlement_base_info"`
	Usage struct {
		CreditsAmount flexFloat `json:"credits_amount"`
	} `json:"usage"`
}

func (p *Provider) cnSnapshot(raw []byte) (plan.Snapshot, error) {
	var r usageReply
	if err := json.Unmarshal(raw, &r); err != nil {
		return plan.Snapshot{}, plan.NewError(plan.ReasonMalformed, err)
	}
	if err := p.businessError(r.Code); err != nil {
		return plan.Snapshot{}, err
	}
	w, err := r.window()
	if err != nil {
		return plan.Snapshot{}, err
	}
	planName := "Free"
	if r.IsCreditsBilling {
		planName = "Credits"
	}
	return plan.Snapshot{
		Provider:  p.id,
		Plan:      planName,
		Windows:   []plan.Window{w},
		FetchedAt: p.now().UTC(),
		Source:    plan.SourceAPI,
	}, nil
}

func (r usageReply) window() (plan.Window, error) {
	var limit, used float64
	unlimited := false
	var soonest time.Time
	for _, p := range r.Packs {
		l := p.Base.Quota.CreditsLimit
		if l == nil {
			continue
		}
		value := float64(*l)
		if value < 0 {
			unlimited = true
		} else {
			limit += value
		}
		if u := float64(p.Usage.CreditsAmount); u > 0 {
			used += u
		}
		end := unixValue(firstNonZero64(p.Base.EndTime, p.Base.ExpireTime, p.EndTime, p.ExpireTime))
		if !end.IsZero() && (soonest.IsZero() || end.Before(soonest)) {
			soonest = end
		}
	}
	if r.UsageSummary.Total > 0 {
		limit = float64(r.UsageSummary.Total)
	}
	if r.UsageSummary.Consumed > 0 {
		used = float64(r.UsageSummary.Consumed)
	}

	w := plan.Window{
		Name:      "Credits",
		Balance:   true,
		Unit:      "credits",
		Limit:     limit,
		Used:      used,
		ExpiresAt: soonest,
	}
	if limit <= 0 {
		if unlimited {
			// An unlimited balance has no number to show; Limit 0 and
			// Remaining 0 mean "unstated", not "empty".
			return w, nil
		}
		return plan.Window{}, plan.NewError(plan.ReasonNoWindows, nil)
	}
	w.Remaining = math.Max(0, limit-used)
	return w, nil
}

// globalReply is Trae Global's dollar-billing entitlement list. The pack list
// has appeared under three names across deployments.
type globalReply struct {
	Code                       int          `json:"code"`
	UserCurrentEntitlementList []dollarPack `json:"user_current_entitlement_list"`
	EntitlementPackList        []dollarPack `json:"entitlement_pack_list"`
	UserEntitlementPackList    []dollarPack `json:"user_entitlement_pack_list"`
}

type dollarPack struct {
	DisplayDesc string `json:"display_desc"`
	Base        struct {
		EndTime int64 `json:"end_time"`
		Quota   struct {
			Basic flexFloat `json:"basic_usage_limit"`
			Bonus flexFloat `json:"bonus_usage_limit"`
		} `json:"quota"`
	} `json:"entitlement_base_info"`
	Usage struct {
		Basic flexFloat `json:"basic_usage_amount"`
		Bonus flexFloat `json:"bonus_usage_amount"`
	} `json:"usage"`
}

func (r globalReply) packs() []dollarPack {
	switch {
	case len(r.UserCurrentEntitlementList) > 0:
		return r.UserCurrentEntitlementList
	case len(r.EntitlementPackList) > 0:
		return r.EntitlementPackList
	default:
		return r.UserEntitlementPackList
	}
}

func (p *Provider) globalSnapshot(raw []byte) (plan.Snapshot, error) {
	var r globalReply
	if err := json.Unmarshal(raw, &r); err != nil {
		return plan.Snapshot{}, plan.NewError(plan.ReasonMalformed, err)
	}
	if err := p.businessError(r.Code); err != nil {
		return plan.Snapshot{}, err
	}
	var w plan.Window
	var planName string
	for _, entry := range r.packs() {
		limit := float64(entry.Base.Quota.Basic + entry.Base.Quota.Bonus)
		if limit <= 0 {
			continue
		}
		used := float64(entry.Usage.Basic + entry.Usage.Bonus)
		w = plan.Window{
			Name:      "Dollar Usage",
			Balance:   true,
			Unit:      "usd",
			Limit:     limit,
			Used:      used,
			Remaining: math.Max(0, limit-used),
			ExpiresAt: unixValue(entry.Base.EndTime),
		}
		planName = entry.DisplayDesc
		break
	}
	if w.Name == "" {
		return plan.Snapshot{}, plan.NewError(plan.ReasonNoWindows, nil)
	}
	return plan.Snapshot{
		Provider:  p.id,
		Plan:      planName,
		Windows:   []plan.Window{w},
		FetchedAt: p.now().UTC(),
		Source:    plan.SourceAPI,
	}, nil
}

// businessError maps Trae's 200-with-code failures.
func (p *Provider) businessError(code int) error {
	switch code {
	case 0:
		return nil
	case 1001:
		return plan.NewError(plan.ReasonUnauthorized, nil)
	}
	if p.global {
		switch code {
		case 20101, 401:
			return plan.NewError(plan.ReasonUnauthorized, nil)
		case 4008, 1005, 4011:
			return plan.NewError(plan.ReasonNoWindows, nil)
		}
	}
	return plan.NewError(plan.ReasonHTTPStatus, nil)
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

func firstNonZero64(values ...int64) int64 {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}
