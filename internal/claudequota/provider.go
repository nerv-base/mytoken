package claudequota

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

// ProviderID is the stable id used in settings keys and by the dashboard.
const ProviderID = "claude"

// Provider reports the plan quota of the Claude Code installation on this
// machine. It reads the OAuth credential Claude Code already stored and calls
// Anthropic's usage endpoint; no separate consent or configuration is needed.
type Provider struct {
	sources   []Source
	transport plan.Transport
	endpoint  string
	now       func() time.Time
}

// Options configures a Provider. Every field is optional.
type Options struct {
	// ConfigDirs overrides the directories scanned for .credentials.json.
	// Defaults to $CLAUDE_CONFIG_DIR plus ~/.claude.
	ConfigDirs []string
	// Keychain overrides the macOS keychain reader.
	Keychain KeychainRunner
	// KeychainService overrides the keychain service name.
	KeychainService string
	// KeychainAccount is the optional keychain account name.
	KeychainAccount string
	// Transport performs the HTTP request; defaults to plan.DefaultTransport.
	Transport plan.Transport
	// Endpoint overrides the usage endpoint. Tests point it at a local server.
	Endpoint string
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
	// Getenv, Home and GOOS default to the process environment.
	Getenv func(string) string
	Home   string
	GOOS   string
}

// New returns a Provider for the Claude Code installations found on this
// machine. It performs no I/O beyond the standard environment lookups.
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
	endpoint := opts.Endpoint
	if endpoint == "" {
		endpoint = Target
	}

	dirs := opts.ConfigDirs
	if dirs == nil {
		dirs = configDirs(getenv, home)
	}
	defaultDir := ""
	if home != "" {
		if abs, err := filepath.Abs(DefaultConfigDir(home)); err == nil {
			defaultDir = abs
		}
	}

	p := &Provider{transport: transport, endpoint: endpoint, now: now}
	seen := make(map[string]bool)
	for _, dir := range dirs {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		label := "Claude Code"
		profile := ""
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		if filepath.Clean(dir) != filepath.Clean(defaultDir) {
			profile = filepath.Base(dir)
			label = "Claude Code (" + profile + ")"
		}
		src := FileSource(label, filepath.Join(dir, CredentialsFileName))
		src.Profile = profile
		p.sources = append(p.sources, src)
	}
	run := opts.Keychain
	if run == nil && goos == "darwin" {
		run = DefaultKeychainRunner()
	}
	if run != nil {
		service := opts.KeychainService
		if service == "" {
			service = ClaudeCodeKeychainService
		}
		p.sources = append(p.sources, KeychainSource("Claude Code (keychain)", service, opts.KeychainAccount, run))
	}
	return p
}

// ID implements plan.Provider.
func (p *Provider) ID() string { return ProviderID }

// DisplayName implements plan.Provider.
func (p *Provider) DisplayName() string { return "Claude" }

// Sources returns the credential locations this provider consults.
func (p *Provider) Sources() []Source { return append([]Source(nil), p.sources...) }

// Detect reports whether any known location holds a Claude Code credential. It
// reads the credential but makes no network request.
func (p *Provider) Detect(ctx context.Context) bool {
	for _, src := range p.sources {
		if ctx.Err() != nil {
			return false
		}
		if src.Present(ctx) {
			return true
		}
	}
	return false
}

// Fetch reads the first usable credential and returns the quota it reports.
// An expired token is reported as token_expired rather than refreshed: a
// refresh would rewrite Claude Code's credential file.
func (p *Provider) Fetch(ctx context.Context) (plan.Snapshot, error) {
	now := p.now()
	var firstErr error
	for _, src := range p.sources {
		if err := ctx.Err(); err != nil {
			return plan.Snapshot{}, plan.NewError(plan.ReasonCanceled, err)
		}
		cred, err := src.Read(ctx)
		if err != nil {
			// A missing source is simply an account that is not signed in.
			if !plan.IsReason(err, plan.ReasonSourceMissing) && firstErr == nil {
				firstErr = err
			}
			continue
		}
		if cred.Empty() {
			continue
		}
		if cred.Expired(now) {
			if firstErr == nil {
				firstErr = plan.NewError(plan.ReasonTokenExpired, nil)
			}
			continue
		}
		windows, err := fetchUsage(ctx, p.transport, p.endpoint, cred)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if plan.IsReason(err, plan.ReasonRateLimited) {
				break
			}
			continue
		}
		if len(windows) == 0 {
			if firstErr == nil {
				firstErr = plan.NewError(plan.ReasonNoWindows, nil)
			}
			continue
		}
		return plan.Snapshot{
			Provider:  ProviderID,
			Windows:   windows,
			FetchedAt: now.UTC(),
			Source:    plan.SourceAPI,
		}, nil
	}
	if firstErr != nil {
		return plan.Snapshot{}, firstErr
	}
	return plan.Snapshot{}, plan.NewError(plan.ReasonNoCredentials, nil)
}
