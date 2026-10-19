// Package qoder reports the plan quota of Qoder CN.
//
// Qoder's desktop app keeps its sign-in in auth.v1.dat beside its Chromium
// profile. The file is a macOS safeStorage "v10" envelope (AES-128-CBC,
// PBKDF2-HMAC-SHA1, key in the login Keychain); the AES key is read via the
// `security` command only in Fetch, never in Detect. Tokens are never logged
// or persisted, and are deliberately not refreshed: the desktop app's refresh
// semantics for this token are unverified, so an expired token is reported as
// token_expired.
//
// The usage endpoint is the one Qoder's own usage page calls:
//
//	GET https://openapi.qoder.com.cn/sash/api/v2/me/usage
//
// with Authorization: Bearer <token>, Cosy-ClientType: 10 and User-Agent:
// Qoder.
package qoder

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

// ProviderID is the stable id used in settings keys and by the dashboard.
const ProviderID = "qoder-cn"

// BaseURL is Qoder CN's OpenAPI origin.
const BaseURL = "https://openapi.qoder.com.cn"

// UserAgent identifies this client to Qoder.
const UserAgent = "Qoder"

const (
	usagePath    = "/sash/api/v2/me/usage"
	maxBodyBytes = 64 << 10
)

// Options configures a Provider. Every field is optional.
type Options struct {
	// AuthPath overrides the auth.v1.dat location.
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
	// KeychainService and KeychainAccount override the Keychain item names.
	KeychainService string
	KeychainAccount string
	// Password overrides the Keychain lookup (tests and explicit setups only).
	// It is never persisted.
	Password string
}

// Provider reports Qoder's credit balances.
type Provider struct {
	baseURL         string
	authPath        string
	transport       plan.Transport
	now             func() time.Time
	goos            string
	keychainService string
	keychainAccount string
	password        string
}

// New returns the Qoder CN provider.
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
	authPath := opts.AuthPath
	if authPath == "" {
		authPath = defaultAuthPath(goos, home, getenv)
	}
	base := opts.BaseURL
	if base == "" {
		base = BaseURL
	}
	service := opts.KeychainService
	if service == "" {
		service = keychainService
	}
	account := opts.KeychainAccount
	if account == "" {
		account = keychainAccount
	}
	return &Provider{
		baseURL:         strings.TrimRight(base, "/"),
		authPath:        authPath,
		transport:       transport,
		now:             now,
		goos:            goos,
		keychainService: service,
		keychainAccount: account,
		password:        opts.Password,
	}
}

// ID implements plan.Provider.
func (p *Provider) ID() string { return ProviderID }

// DisplayName implements plan.Provider.
func (p *Provider) DisplayName() string { return "Qoder CN" }

// Detect reports whether auth.v1.dat exists. It never touches the Keychain.
func (p *Provider) Detect(context.Context) bool {
	fi, err := os.Stat(p.authPath)
	return err == nil && !fi.IsDir()
}

// Fetch decrypts the stored sign-in and asks the usage endpoint.
func (p *Provider) Fetch(ctx context.Context) (plan.Snapshot, error) {
	c, err := p.credentials(ctx)
	if err != nil {
		return plan.Snapshot{}, err
	}
	if !c.expiresAt.IsZero() && !p.now().Before(c.expiresAt) {
		return plan.Snapshot{}, plan.NewError(plan.ReasonTokenExpired, nil)
	}

	h := http.Header{}
	h.Set("Accept", "application/json")
	h.Set("Authorization", "Bearer "+c.token)
	h.Set("Cosy-ClientType", "10")
	h.Set("User-Agent", UserAgent)

	raw, err := plan.DoJSON(ctx, p.transport, http.MethodGet, p.baseURL+usagePath, h, nil, maxBodyBytes)
	if err != nil {
		return plan.Snapshot{}, err
	}
	var r usageReply
	if err := json.Unmarshal(raw, &r); err != nil {
		return plan.Snapshot{}, plan.NewError(plan.ReasonMalformed, err)
	}
	if r.DisplayMode == "enterprise" {
		// Enterprise usage is not exposed by this endpoint.
		return plan.Snapshot{}, plan.NewError(plan.ReasonNoData, nil)
	}
	if r.QoderUsage == nil {
		return plan.Snapshot{}, plan.NewError(plan.ReasonNoData, nil)
	}

	expires := unixMilli(int64(r.QoderUsage.ExpiresAt))
	var windows []plan.Window
	add := func(def string, b *bucket) {
		if w, ok := b.window(def, expires); ok {
			windows = append(windows, w)
		}
	}
	add("Credits", r.QoderUsage.UserQuota)
	add("Add-on credits", r.QoderUsage.AddOnQuota)
	add("Shared credits", r.QoderUsage.OrgResourcePackage)
	for i := range r.QoderUsage.DedicatedResourcePackages {
		add("Dedicated credits", &r.QoderUsage.DedicatedResourcePackages[i])
	}
	if len(windows) == 0 {
		return plan.Snapshot{}, plan.NewError(plan.ReasonNoWindows, nil)
	}
	planName := strings.TrimSpace(r.QoderUsage.UserType)
	if planName == "" {
		planName = "Qoder"
	}
	return plan.Snapshot{
		Provider:  ProviderID,
		Plan:      planName,
		Windows:   windows,
		FetchedAt: p.now().UTC(),
		Source:    plan.SourceAPI,
	}, nil
}

type usageReply struct {
	DisplayMode string      `json:"displayMode"`
	QoderUsage  *qoderUsage `json:"qoderUsage"`
}

type qoderUsage struct {
	UserType                  string    `json:"userType"`
	UsageType                 string    `json:"usageType"`
	ExpiresAt                 flexInt64 `json:"expiresAt"`
	UserQuota                 *bucket   `json:"userQuota"`
	AddOnQuota                *bucket   `json:"addOnQuota"`
	OrgResourcePackage        *bucket   `json:"orgResourcePackage"`
	DedicatedResourcePackages []bucket  `json:"dedicatedResourcePackages"`
}

type bucket struct {
	Name      string     `json:"name"`
	Unit      string     `json:"unit"`
	Total     *flexFloat `json:"total"`
	Cap       *flexFloat `json:"cap"`
	Used      *flexFloat `json:"used"`
	Remaining *flexFloat `json:"remaining"`
}

// window maps one bucket to a Balance window. A bucket without a positive cap
// or without a used/remaining reading is skipped.
func (b *bucket) window(def string, expires time.Time) (plan.Window, bool) {
	if b == nil {
		return plan.Window{}, false
	}
	var total float64
	if b.Total != nil && float64(*b.Total) > 0 {
		total = float64(*b.Total)
	} else if b.Cap != nil {
		total = float64(*b.Cap)
	} else {
		return plan.Window{}, false
	}
	if total <= 0 {
		return plan.Window{}, false
	}
	var used, remaining float64
	switch {
	case b.Used != nil:
		used = float64(*b.Used)
	case b.Remaining != nil:
		used = total - float64(*b.Remaining)
	default:
		return plan.Window{}, false
	}
	if used < 0 {
		return plan.Window{}, false
	}
	if b.Remaining != nil {
		remaining = float64(*b.Remaining)
	} else {
		remaining = total - used
	}
	if remaining < 0 {
		remaining = 0
	}
	unit := strings.TrimSpace(b.Unit)
	if unit == "" {
		unit = "credits"
	}
	name := strings.TrimSpace(b.Name)
	if name == "" {
		name = def
	}
	return plan.Window{
		Name:      name,
		Balance:   true,
		Used:      used,
		Limit:     total,
		Remaining: remaining,
		Unit:      unit,
		ExpiresAt: expires,
	}, true
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

// flexInt64 accepts a JSON number or numeric string.
type flexInt64 int64

func (n *flexInt64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return err
		}
		v = int64(f)
	}
	*n = flexInt64(v)
	return nil
}

func unixMilli(n int64) time.Time {
	if n <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(n)
}
