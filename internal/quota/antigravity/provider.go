// Package antigravity reports the Gemini plan quota of Antigravity.
//
// Antigravity serves Gemini through Google Cloud Code and exposes the same
// quota groups its UI shows through retrieveUserQuotaSummary. The endpoint is
// sensitive to the User-Agent: only Antigravity's own hub User-Agent returns
// the "Gemini Models" group.
//
// Credentials are read, never refreshed or persisted, from the first usable
// source:
//
//   - the running Antigravity desktop language server, discovered through
//     ~/.gemini/antigravity/daemon/hub_*.json (no token needed);
//   - the agy CLI sign-in in the login Keychain (macOS);
//   - the token T3 Code saved under providers/antigravity/<instance>/antigravity-acp.
//
// An expired token is reported as token_expired; the owning tool is expected
// to refresh it on its next run.
package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/nerv-base/mytoken/internal/quota/plan"
)

// ProviderID is the stable id used in settings keys and by the dashboard.
const ProviderID = "antigravity"

// BaseURL is the Cloud Code origin Antigravity's consumer accounts use.
const BaseURL = "https://daily-cloudcode-pa.googleapis.com"

const (
	summaryPath  = "/v1internal:retrieveUserQuotaSummary"
	maxBodyBytes = 64 << 10
	// hubUserAgent is the User-Agent Antigravity's own hub sends. The quota
	// endpoint returns the Gemini group only for this client identity.
	hubUserAgent = "antigravity/hub/2.24.0 (aidev_client; os_type=%s; arch=%s; cl=963137146)"
)

// Options configures a Provider. Every field is optional.
type Options struct {
	// Transport performs HTTP requests; defaults to plan.DefaultTransport.
	Transport plan.Transport
	// BaseURL overrides the Cloud Code origin (tests point it at a local server).
	BaseURL string
	// DaemonDir overrides the Antigravity desktop discovery directory.
	DaemonDir string
	// AgyDir overrides the agy CLI data directory.
	AgyDir string
	// T3Roots override the T3 Code provider roots that hold Antigravity
	// instances. The default is ~/.t3/{userdata,dev}/providers/antigravity,
	// or the same below $T3CODE_HOME.
	T3Roots []string
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
	// Getenv, Home and GOOS/GOARCH default to the process environment.
	Getenv func(string) string
	Home   string
	GOOS   string
	GOARCH string
	// KeychainService and KeychainAccount override the Keychain item names.
	KeychainService string
	KeychainAccount string
	// KeychainValue overrides the Keychain lookup with a raw item value
	// (tests and explicit setups only). It is never persisted.
	KeychainValue string
	// KeychainLookup overrides the macOS `security` command (tests only).
	KeychainLookup func(context.Context, string, string) (string, error)
}

// Provider reports Antigravity's Gemini quota.
type Provider struct {
	baseURL         string
	daemonDir       string
	agyDir          string
	t3Roots         []string
	transport       plan.Transport
	now             func() time.Time
	goos            string
	goarch          string
	keychainService string
	keychainAccount string
	keychainValue   string
	keychainLookup  func(context.Context, string, string) (string, error)
}

// New returns the Antigravity provider.
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
	goarch := opts.GOARCH
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	transport := opts.Transport
	if transport == nil {
		transport = plan.DefaultTransport()
	}
	daemonDir := opts.DaemonDir
	if daemonDir == "" {
		daemonDir = defaultDaemonDir(home, getenv)
	}
	agyDir := opts.AgyDir
	if agyDir == "" {
		agyDir = defaultAgyDir(home, getenv)
	}
	roots := opts.T3Roots
	if roots == nil {
		roots = defaultT3Roots(home, getenv)
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
	lookup := opts.KeychainLookup
	if lookup == nil {
		lookup = keychainPassword
	}
	return &Provider{
		baseURL:         strings.TrimRight(base, "/"),
		daemonDir:       daemonDir,
		agyDir:          agyDir,
		t3Roots:         append([]string(nil), roots...),
		transport:       transport,
		now:             now,
		goos:            goos,
		goarch:          goarch,
		keychainService: service,
		keychainAccount: account,
		keychainValue:   opts.KeychainValue,
		keychainLookup:  lookup,
	}
}

// ID implements plan.Provider.
func (p *Provider) ID() string { return ProviderID }

// DisplayName implements plan.Provider.
func (p *Provider) DisplayName() string { return "Antigravity" }

// Detect reports whether any Antigravity usage source is present. It is local
// and never invokes the Keychain.
func (p *Provider) Detect(context.Context) bool {
	if len(p.daemonFiles()) > 0 {
		return true
	}
	if fi, err := os.Stat(p.agyDir); err == nil && fi.IsDir() {
		return true
	}
	return len(p.t3TokenFiles()) > 0
}

// Fetch reads the current Gemini quota. It prefers the running desktop app's
// local language server, then falls back to a stored OAuth token.
func (p *Provider) Fetch(ctx context.Context) (plan.Snapshot, error) {
	var daemonErr error
	for _, file := range p.daemonFiles() {
		snap, err := p.fetchDaemon(ctx, file)
		if err == nil {
			return snap, nil
		}
		daemonErr = err
		break
	}

	cred, credErr := p.credentials(ctx)
	if credErr != nil {
		if plan.IsReason(credErr, plan.ReasonNoCredentials) {
			return plan.Snapshot{}, credErr
		}
		if daemonErr != nil {
			return plan.Snapshot{}, daemonErr
		}
		return plan.Snapshot{}, credErr
	}
	if !cred.expiresAt.IsZero() && !p.now().Before(cred.expiresAt) {
		return plan.Snapshot{}, plan.NewError(plan.ReasonTokenExpired, nil)
	}
	snap, err := p.fetchAPI(ctx, cred.token)
	if err == nil {
		return snap, nil
	}
	if daemonErr != nil && !plan.IsReason(err, plan.ReasonTokenExpired) {
		return plan.Snapshot{}, daemonErr
	}
	return plan.Snapshot{}, err
}

func (p *Provider) userAgent() string {
	return fmt.Sprintf(hubUserAgent, p.goos, p.goarch)
}

// fetchDaemon queries the desktop app's local language server. It needs the
// CSRF token from the hub discovery file but no OAuth token.
func (p *Provider) fetchDaemon(ctx context.Context, file string) (plan.Snapshot, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return plan.Snapshot{}, plan.NewError(plan.ReasonSourceUnreadable, err)
	}
	var info daemonInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return plan.Snapshot{}, plan.NewError(plan.ReasonSourceUnreadable, err)
	}
	if info.HTTPPort <= 0 || strings.TrimSpace(info.CSRF) == "" {
		return plan.Snapshot{}, plan.NewError(plan.ReasonSourceUnreadable, nil)
	}
	target := fmt.Sprintf("http://127.0.0.1:%d/exa.language_server_pb.LanguageServerService/RetrieveUserQuotaSummary", info.HTTPPort)
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("x-codeium-csrf-token", info.CSRF)
	body, err := plan.DoJSON(ctx, p.transport, http.MethodPost, target, h, []byte("{}"), maxBodyBytes)
	if err != nil {
		return plan.Snapshot{}, err
	}
	var envelope struct {
		Response *summaryReply `json:"response"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Response != nil {
		return p.snapshot(envelope.Response)
	}
	var reply summaryReply
	if err := json.Unmarshal(body, &reply); err != nil {
		return plan.Snapshot{}, plan.NewError(plan.ReasonMalformed, err)
	}
	return p.snapshot(&reply)
}

// fetchAPI asks Cloud Code directly with a stored OAuth access token.
func (p *Provider) fetchAPI(ctx context.Context, token string) (plan.Snapshot, error) {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", p.userAgent())
	body, err := plan.DoJSON(ctx, p.transport, http.MethodPost, p.baseURL+summaryPath, h, []byte("{}"), maxBodyBytes)
	if err != nil {
		return plan.Snapshot{}, err
	}
	var reply summaryReply
	if err := json.Unmarshal(body, &reply); err != nil {
		return plan.Snapshot{}, plan.NewError(plan.ReasonMalformed, err)
	}
	return p.snapshot(&reply)
}

// snapshot keeps only the Gemini group; the same response also carries the
// Claude and GPT group, which MyToken does not report under this provider.
func (p *Provider) snapshot(reply *summaryReply) (plan.Snapshot, error) {
	if reply == nil || len(reply.Groups) == 0 {
		return plan.Snapshot{}, plan.NewError(plan.ReasonNoData, nil)
	}
	var windows []plan.Window
	for _, group := range reply.Groups {
		if !strings.Contains(strings.ToLower(group.DisplayName), "gemini") {
			continue
		}
		for _, bucket := range group.Buckets {
			if w, ok := bucket.window(); ok {
				windows = append(windows, w)
			}
		}
	}
	if len(windows) == 0 {
		return plan.Snapshot{}, plan.NewError(plan.ReasonNoData, nil)
	}
	sort.SliceStable(windows, func(i, j int) bool {
		return windowRank(windows[i].Name) < windowRank(windows[j].Name)
	})
	return plan.Snapshot{
		Provider:  ProviderID,
		Plan:      "Gemini",
		Windows:   windows,
		FetchedAt: p.now().UTC(),
		Source:    plan.SourceAPI,
	}, nil
}

func windowRank(name string) int {
	switch name {
	case "5h":
		return 0
	case "week":
		return 1
	default:
		return 2
	}
}

type summaryReply struct {
	Groups []summaryGroup `json:"groups"`
}

type summaryGroup struct {
	DisplayName string          `json:"displayName"`
	Description string          `json:"description"`
	Buckets     []summaryBucket `json:"buckets"`
}

type summaryBucket struct {
	BucketID          string   `json:"bucketId"`
	DisplayName       string   `json:"displayName"`
	Window            string   `json:"window"`
	ResetTime         string   `json:"resetTime"`
	RemainingFraction *float64 `json:"remainingFraction"`
}

// window converts one percentage bucket. Buckets without a remaining fraction
// are skipped rather than shown as zero.
func (b summaryBucket) window() (plan.Window, bool) {
	if b.RemainingFraction == nil {
		return plan.Window{}, false
	}
	remaining := *b.RemainingFraction
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 1 {
		remaining = 1
	}
	name := bucketWindowName(b)
	if name == "" {
		return plan.Window{}, false
	}
	w := plan.Window{
		Name:        name,
		UsedPercent: (1 - remaining) * 100,
	}
	if at, ok := parseInstant(b.ResetTime); ok {
		w.ResetsAt = at
	}
	return w, true
}

func bucketWindowName(b summaryBucket) string {
	id := strings.ToLower(strings.TrimSpace(b.BucketID))
	switch {
	case strings.HasSuffix(id, "-5h"):
		return "5h"
	case strings.HasSuffix(id, "-weekly"), strings.HasSuffix(id, "-week"):
		return "week"
	}
	switch strings.ToLower(strings.TrimSpace(b.Window)) {
	case "5h", "five hour", "five-hour":
		return "5h"
	case "week", "weekly":
		return "week"
	}
	return strings.TrimSpace(b.BucketID)
}

func parseInstant(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

type daemonInfo struct {
	PID       int    `json:"pid"`
	HTTPSPort int    `json:"httpsPort"`
	HTTPPort  int    `json:"httpPort"`
	LSVersion string `json:"lsVersion"`
	CSRF      string `json:"csrfToken"`
}

// daemonFiles returns the live hub discovery files, newest first.
func (p *Provider) daemonFiles() []string {
	matches, err := filepath.Glob(filepath.Join(p.daemonDir, "hub_*.json"))
	if err != nil {
		return nil
	}
	sort.SliceStable(matches, func(i, j int) bool {
		si, errI := os.Stat(matches[i])
		sj, errJ := os.Stat(matches[j])
		if errI != nil || errJ != nil {
			return matches[i] < matches[j]
		}
		return si.ModTime().After(sj.ModTime())
	})
	return matches
}

// t3TokenFiles returns the antigravity-oauth-token files saved by T3 Code.
func (p *Provider) t3TokenFiles() []string {
	var files []string
	for _, root := range p.t3Roots {
		matches, err := filepath.Glob(filepath.Join(root, "*", "antigravity-acp", "antigravity-oauth-token"))
		if err != nil {
			continue
		}
		files = append(files, matches...)
	}
	sort.Strings(files)
	return files
}

func defaultDaemonDir(home string, getenv func(string) string) string {
	return filepath.Join(geminiBase(home, getenv), "antigravity", "daemon")
}

func defaultAgyDir(home string, getenv func(string) string) string {
	return filepath.Join(geminiBase(home, getenv), "antigravity-cli")
}

func geminiBase(home string, getenv func(string) string) string {
	if base := strings.TrimSpace(getenv("GEMINI_CLI_HOME")); base != "" {
		return base
	}
	return filepath.Join(home, ".gemini")
}

func defaultT3Roots(home string, getenv func(string) string) []string {
	base := strings.TrimSpace(getenv("T3CODE_HOME"))
	if base == "" {
		base = filepath.Join(home, ".t3")
	}
	return []string{
		filepath.Join(base, "userdata", "providers", "antigravity"),
		filepath.Join(base, "dev", "providers", "antigravity"),
	}
}
