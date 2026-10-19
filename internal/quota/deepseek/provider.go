// Package deepseek reports the credit balance of a DeepSeek platform account.
//
// The platform API key is read from the DeepSeek Harness (DSH) credential
// store at $DSH_HOME/.credentials.yaml, defaulting to
// ~/.dsh/.credentials.yaml, from the refs entry DEEPSEEK_API_KEY. When DSH
// stores no key, DEEPSEEK_API_KEY from the environment is used. The key
// authenticates the official balance endpoint:
//
//	GET https://api.deepseek.com/user/balance
//	Authorization: Bearer <key>
//
// Each balance_infos[] entry carries a currency and a decimal total_balance;
// every currency becomes one balance window. is_available=false means the
// account cannot make API calls and is reported as an exhausted balance.
//
// DSH also stores a signed-in account grant in records
// (deepseek-account-platform/default). That grant is a session token for
// platform.deepseek.com, not a platform API key, so it is not used here.
//
// API documentation: https://api-docs.deepseek.com/api/get-user-balance
package deepseek

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

// ProviderID is the stable id used in settings keys and by the UI.
const ProviderID = "deepseek"

const (
	// defaultBaseURL is the DeepSeek platform API origin.
	defaultBaseURL = "https://api.deepseek.com"
	balancePath    = "/user/balance"
	maxBodyBytes   = 64 << 10
	userAgent      = "mytoken-quota/1"
)

// Options configures a Provider. Every field is optional.
type Options struct {
	// BaseURL overrides the API origin (tests point it at a local server).
	BaseURL string
	// CredentialPath overrides the DSH credential store path.
	CredentialPath string
	// Transport performs HTTP requests; defaults to plan.DefaultTransport.
	Transport plan.Transport
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
	// Getenv and Home default to the process environment.
	Getenv func(string) string
	Home   string
}

// Provider reports DeepSeek's platform credit balance.
type Provider struct {
	baseURL      string
	pathOverride string
	transport    plan.Transport
	now          func() time.Time
	getenv       func(string) string
	home         string
}

// New returns a DeepSeek provider.
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
		baseURL:      strings.TrimRight(opts.BaseURL, "/"),
		pathOverride: opts.CredentialPath,
		transport:    transport,
		now:          now,
		getenv:       getenv,
		home:         home,
	}
}

// ID implements plan.Provider.
func (p *Provider) ID() string { return ProviderID }

// DisplayName implements plan.Provider.
func (p *Provider) DisplayName() string { return "DeepSeek" }

// Detect reports whether a DeepSeek platform API key is stored.
func (p *Provider) Detect(context.Context) bool {
	_, err := p.credentials()
	return err == nil
}

// Fetch asks the balance endpoint for the account's credit balance.
func (p *Provider) Fetch(ctx context.Context) (plan.Snapshot, error) {
	key, err := p.credentials()
	if err != nil {
		return plan.Snapshot{}, err
	}
	base := p.baseURL
	if base == "" {
		base = defaultBaseURL
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+key)
	header.Set("Accept", "application/json")
	header.Set("User-Agent", userAgent)
	raw, err := plan.DoJSON(ctx, p.transport, http.MethodGet, base+balancePath, header, nil, maxBodyBytes)
	if err != nil {
		return plan.Snapshot{}, err
	}
	var reply balanceReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return plan.Snapshot{}, plan.NewError(plan.ReasonMalformed, err)
	}
	if reply.IsAvailable == nil {
		return plan.Snapshot{}, plan.NewError(plan.ReasonMalformed, nil)
	}
	if !*reply.IsAvailable {
		return plan.Snapshot{}, plan.NewError(plan.ReasonBalanceExhausted, nil)
	}
	windows, err := balanceWindows(reply.BalanceInfos)
	if err != nil {
		return plan.Snapshot{}, err
	}
	if len(windows) == 0 {
		return plan.Snapshot{}, plan.NewError(plan.ReasonNoWindows, nil)
	}
	return plan.Snapshot{
		Provider:  ProviderID,
		Windows:   windows,
		FetchedAt: p.now().UTC(),
		Source:    plan.SourceAPI,
	}, nil
}

// balanceReply is the /user/balance payload. Balances are decimal strings.
type balanceReply struct {
	IsAvailable  *bool         `json:"is_available"`
	BalanceInfos []balanceInfo `json:"balance_infos"`
}

type balanceInfo struct {
	Currency        string `json:"currency"`
	TotalBalance    string `json:"total_balance"`
	GrantedBalance  string `json:"granted_balance"`
	ToppedUpBalance string `json:"topped_up_balance"`
}

// balanceWindows turns one currency's total balance into one balance window.
// The granted/topped-up split is deliberately not shown.
func balanceWindows(infos []balanceInfo) ([]plan.Window, error) {
	windows := make([]plan.Window, 0, len(infos))
	for _, info := range infos {
		currency := strings.TrimSpace(info.Currency)
		if currency == "" {
			return nil, plan.NewError(plan.ReasonMalformed, nil)
		}
		total, err := strconv.ParseFloat(strings.TrimSpace(info.TotalBalance), 64)
		if err != nil || math.IsNaN(total) || math.IsInf(total, 0) {
			return nil, plan.NewError(plan.ReasonMalformed, err)
		}
		name := "balance"
		if len(infos) > 1 {
			name = "Balance (" + strings.ToUpper(currency) + ")"
		}
		windows = append(windows, plan.Window{
			Name:      name,
			Balance:   true,
			Remaining: total,
			Unit:      strings.ToLower(currency),
		})
	}
	return windows, nil
}
