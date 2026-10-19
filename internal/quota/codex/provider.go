// Package codex reads OpenAI Codex plan usage from the ChatGPT backend.
//
// It is the same read the codex CLI and CodexBar perform: the OAuth tokens the
// Codex CLI stored in $CODEX_HOME/auth.json (default ~/.codex/auth.json) are
// sent to GET https://chatgpt.com/backend-api/wham/usage, and the 5-hour and
// weekly rate-limit windows it returns are mapped onto plan windows. Nothing
// here reads a local usage log: the numbers always come from the provider.
//
// The credential is only ever read. MyToken never refreshes or rewrites
// auth.json, because rotating the token would sign the user out of Codex
// itself; an expired or rejected token is reported as an error asking the user
// to sign in to Codex again.
package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

// ProviderID is the stable id used in settings keys and by the dashboard.
const ProviderID = "codex"

// Target is the ChatGPT backend usage endpoint. The codex CLI reaches the same
// payload through its backend client (PathStyle::ChatGptApi + "/wham/usage").
const Target = "https://chatgpt.com/backend-api/wham/usage"

// UserAgent is the header the codex CLI sends by default.
const UserAgent = "codex-cli"

// maxQuotaBytes bounds one usage response body.
const maxQuotaBytes = 64 << 10

// maxCredentialBytes bounds the auth file read.
const maxCredentialBytes = 64 << 10

// Options configures a Provider. Every field has a usable default.
type Options struct {
	// Transport performs the HTTP request; defaults to the shared client that
	// respects the OS proxy (plan.DefaultTransport).
	Transport plan.Transport
	// Endpoint overrides Target; tests point it at a loopback server.
	Endpoint string
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
	// Getenv reads CODEX_HOME; defaults to os.Getenv.
	Getenv func(string) string
	// Home overrides the home directory; defaults to os.UserHomeDir.
	Home string
}

// Provider fetches the Codex plan quota from the ChatGPT backend.
type Provider struct {
	transport plan.Transport
	endpoint  string
	now       func() time.Time
	getenv    func(string) string
	home      string
}

// New returns a Codex provider.
func New(opts Options) *Provider {
	p := &Provider{
		transport: opts.Transport,
		endpoint:  opts.Endpoint,
		now:       opts.Now,
		getenv:    opts.Getenv,
		home:      opts.Home,
	}
	if p.transport == nil {
		p.transport = plan.DefaultTransport()
	}
	if p.endpoint == "" {
		p.endpoint = Target
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.getenv == nil {
		p.getenv = os.Getenv
	}
	if p.home == "" {
		p.home, _ = os.UserHomeDir()
	}
	return p
}

// ID implements plan.Provider.
func (p *Provider) ID() string { return ProviderID }

// DisplayName implements plan.Provider.
func (p *Provider) DisplayName() string { return "Codex" }

// AuthPath returns the Codex CLI auth file this provider reads:
// $CODEX_HOME/auth.json, or ~/.codex/auth.json when CODEX_HOME is unset.
func (p *Provider) AuthPath() string { return authPath(p.home, p.getenv("CODEX_HOME")) }

func authPath(home, codexHome string) string {
	if dir := strings.TrimSpace(codexHome); dir != "" {
		return filepath.Join(dir, "auth.json")
	}
	return filepath.Join(home, ".codex", "auth.json")
}

// Detect reports whether the Codex CLI stored a credential. It reads only the
// auth file, never the network.
func (p *Provider) Detect(context.Context) bool {
	_, err := p.readCredential()
	return err == nil
}

// Fetch requests the current quota from the ChatGPT backend.
func (p *Provider) Fetch(ctx context.Context) (plan.Snapshot, error) {
	cred, err := p.readCredential()
	if err != nil {
		return plan.Snapshot{}, err
	}
	if cred.accessToken == "" {
		// An API-key-only login authenticates requests but has no plan quota:
		// the account is billed per token rather than through a subscription.
		return plan.Snapshot{}, fmt.Errorf(
			"%w (API-key login has no plan quota; sign in to Codex with a ChatGPT account to see plan usage)",
			plan.NewError(plan.ReasonUnsupported, nil))
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+cred.accessToken)
	header.Set("User-Agent", UserAgent)
	header.Set("Accept", "application/json")
	if cred.accountID != "" {
		header.Set("ChatGPT-Account-Id", cred.accountID)
	}
	raw, err := plan.DoJSON(ctx, p.transport, http.MethodGet, p.endpoint, header, nil, maxQuotaBytes)
	if err != nil {
		if plan.IsReason(err, plan.ReasonUnauthorized) || plan.IsReason(err, plan.ReasonForbidden) {
			// Never refresh or rewrite the token: doing so would sign the user
			// out of Codex. Ask them to sign in there instead.
			return plan.Snapshot{}, fmt.Errorf(
				"%w; sign in to Codex again (the stored token was rejected and MyToken never refreshes it)", err)
		}
		return plan.Snapshot{}, err
	}
	return parseUsage(raw, p.now())
}

// credential is the part of the Codex CLI auth file this provider needs. It is
// deliberately unexported and never rendered: only Fetch reads it, and it is
// never logged.
type credential struct {
	accessToken string
	accountID   string
	// apiKeyOnly marks an auth file whose only credential is OPENAI_API_KEY.
	apiKeyOnly bool
}

// String keeps the credential out of logs and error messages.
func (credential) String() string { return "[redacted]" }

// GoString keeps the credential out of %#v output.
func (credential) GoString() string { return "[redacted]" }

// authFile mirrors $CODEX_HOME/auth.json. Unknown fields are ignored so a
// newer Codex CLI does not break the read.
type authFile struct {
	OpenAIAPIKey *string `json:"OPENAI_API_KEY"`
	AuthMode     string  `json:"auth_mode"`
	Tokens       *struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
		IDToken     string `json:"id_token"`
	} `json:"tokens"`
}

func (p *Provider) readCredential() (credential, error) {
	path := p.AuthPath()
	if path == "" {
		return credential{}, plan.NewError(plan.ReasonNoCredentials, nil)
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return credential{}, plan.NewError(plan.ReasonSourceMissing, err)
		}
		return credential{}, plan.NewError(plan.ReasonSourceUnreadable, err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxCredentialBytes+1))
	if err != nil {
		return credential{}, plan.NewError(plan.ReasonSourceUnreadable, err)
	}
	if len(raw) > maxCredentialBytes {
		return credential{}, plan.NewError(plan.ReasonSourceUnreadable, nil)
	}
	var af authFile
	if err := json.Unmarshal(raw, &af); err != nil {
		return credential{}, plan.NewError(plan.ReasonSourceUnreadable, err)
	}
	var cred credential
	if af.Tokens != nil {
		cred.accessToken = strings.TrimSpace(af.Tokens.AccessToken)
		cred.accountID = strings.TrimSpace(af.Tokens.AccountID)
		if cred.accountID == "" {
			// Older auth files predate tokens.account_id; the account id is a
			// claim in the id token (or the access token).
			cred.accountID = accountIDFromJWT(af.Tokens.IDToken)
			if cred.accountID == "" {
				cred.accountID = accountIDFromJWT(af.Tokens.AccessToken)
			}
		}
	}
	if cred.accessToken != "" {
		return cred, nil
	}
	if af.OpenAIAPIKey != nil && strings.TrimSpace(*af.OpenAIAPIKey) != "" {
		cred.apiKeyOnly = true
		return cred, nil
	}
	return credential{}, plan.NewError(plan.ReasonNoCredentials, nil)
}

// accountIDFromJWT reads the chatgpt_account_id claim out of an unverified JWT
// payload. The token is only decoded, never validated or printed: the backend
// remains the authority on whether it is still good.
func accountIDFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if raw, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return ""
		}
	}
	var claims struct {
		ChatGPTAccountID string `json:"chatgpt_account_id"`
		Auth             *struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return ""
	}
	if claims.Auth != nil && strings.TrimSpace(claims.Auth.ChatGPTAccountID) != "" {
		return strings.TrimSpace(claims.Auth.ChatGPTAccountID)
	}
	return strings.TrimSpace(claims.ChatGPTAccountID)
}

// usagePayload is the ChatGPT backend usage response. rate_limit is nullable
// (and so are its windows), because an account without an active window still
// has a plan_type.
type usagePayload struct {
	PlanType  *string         `json:"plan_type"`
	RateLimit json.RawMessage `json:"rate_limit"`
}

type rateLimitDetails struct {
	PrimaryWindow   json.RawMessage `json:"primary_window"`
	SecondaryWindow json.RawMessage `json:"secondary_window"`
}

// windowSnapshot is one rate-limit window. used_percent is a percentage
// (0-100), limit_window_seconds is the window length, and reset_at is an epoch
// in seconds (reset_after_seconds is the relative fallback).
type windowSnapshot struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds *int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  *int64   `json:"reset_after_seconds"`
	ResetAt            *int64   `json:"reset_at"`
}

// parseUsage maps the backend payload onto ordered quota windows. A payload
// with no usable window is an error, not 0%.
func parseUsage(raw []byte, now time.Time) (plan.Snapshot, error) {
	var payload usagePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return plan.Snapshot{}, plan.NewError(plan.ReasonMalformed, err)
	}
	snap := plan.Snapshot{
		Provider:  ProviderID,
		FetchedAt: now.UTC(),
		Source:    plan.SourceAPI,
	}
	if payload.PlanType != nil {
		snap.Plan = strings.TrimSpace(*payload.PlanType)
	}
	if rl := decodeJSONObject(payload.RateLimit); rl != nil {
		var details rateLimitDetails
		if err := json.Unmarshal(rl, &details); err != nil {
			return plan.Snapshot{}, plan.NewError(plan.ReasonMalformed, err)
		}
		for _, win := range []struct {
			fallback string
			raw      json.RawMessage
		}{
			{"primary", details.PrimaryWindow},
			{"secondary", details.SecondaryWindow},
		} {
			if w, ok := decodeWindow(win.fallback, win.raw, now); ok {
				snap.Windows = append(snap.Windows, w)
			}
		}
	}
	if len(snap.Windows) == 0 {
		return plan.Snapshot{}, plan.NewError(plan.ReasonNoWindows, nil)
	}
	return snap, nil
}

// decodeJSONObject returns raw when it is a JSON object and nil for the null or
// absent forms the backend uses for "not reported".
func decodeJSONObject(raw json.RawMessage) json.RawMessage {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" || raw[0] != '{' {
		return nil
	}
	return raw
}

// decodeWindow maps one window. It reports false when the window states
// neither a percentage nor a reset instant, so a window the backend did not
// report is omitted rather than shown as zero.
func decodeWindow(fallback string, raw json.RawMessage, now time.Time) (plan.Window, bool) {
	raw = decodeJSONObject(raw)
	if raw == nil {
		return plan.Window{}, false
	}
	var ws windowSnapshot
	if err := json.Unmarshal(raw, &ws); err != nil {
		return plan.Window{}, false
	}
	win := plan.Window{Name: windowName(ws.LimitWindowSeconds, fallback)}
	if ws.UsedPercent != nil {
		win.UsedPercent = *ws.UsedPercent
	}
	if ws.ResetAt != nil && *ws.ResetAt > 0 {
		win.ResetsAt = time.Unix(*ws.ResetAt, 0).UTC()
	} else if ws.ResetAfterSeconds != nil && *ws.ResetAfterSeconds > 0 {
		win.ResetsAt = now.Add(time.Duration(*ws.ResetAfterSeconds) * time.Second).UTC()
	}
	if ws.UsedPercent == nil && win.ResetsAt.IsZero() {
		return plan.Window{}, false
	}
	return win, true
}

// windowName labels a window by its length: the 5-hour and weekly windows the
// Codex plan is metered by, or a length-derived label for anything else.
func windowName(seconds *int64, fallback string) string {
	if seconds == nil || *seconds <= 0 {
		return fallback
	}
	minutes := *seconds / 60
	switch {
	case minutes == 300:
		return "5h"
	case minutes == 10080:
		return "week"
	case minutes > 0 && minutes%1440 == 0:
		return fmt.Sprintf("%dd", minutes/1440)
	case minutes > 0 && minutes%60 == 0:
		return fmt.Sprintf("%dh", minutes/60)
	case minutes > 0:
		return fmt.Sprintf("%dm", minutes)
	default:
		return fallback
	}
}
