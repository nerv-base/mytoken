// Package app wires the backend and registered parsers.
package app

import (
	"context"
	"github.com/zzstar101/mytoken/internal/attrib"
	"github.com/zzstar101/mytoken/internal/budget"
	"github.com/zzstar101/mytoken/internal/claudequota"
	"github.com/zzstar101/mytoken/internal/harness"
	_ "github.com/zzstar101/mytoken/internal/harness/amp"
	_ "github.com/zzstar101/mytoken/internal/harness/antigravity"
	_ "github.com/zzstar101/mytoken/internal/harness/claude"
	_ "github.com/zzstar101/mytoken/internal/harness/claudedesktop"
	_ "github.com/zzstar101/mytoken/internal/harness/cline"
	_ "github.com/zzstar101/mytoken/internal/harness/codebuddy"
	_ "github.com/zzstar101/mytoken/internal/harness/codebuff"
	_ "github.com/zzstar101/mytoken/internal/harness/codex"
	_ "github.com/zzstar101/mytoken/internal/harness/crush"
	_ "github.com/zzstar101/mytoken/internal/harness/droid"
	_ "github.com/zzstar101/mytoken/internal/harness/dsh"
	_ "github.com/zzstar101/mytoken/internal/harness/forge"
	_ "github.com/zzstar101/mytoken/internal/harness/gemini"
	_ "github.com/zzstar101/mytoken/internal/harness/goose"
	_ "github.com/zzstar101/mytoken/internal/harness/grok"
	_ "github.com/zzstar101/mytoken/internal/harness/hermes"
	_ "github.com/zzstar101/mytoken/internal/harness/kilo"
	_ "github.com/zzstar101/mytoken/internal/harness/kimi"
	_ "github.com/zzstar101/mytoken/internal/harness/kiro"
	_ "github.com/zzstar101/mytoken/internal/harness/mimocode"
	_ "github.com/zzstar101/mytoken/internal/harness/minimax"
	_ "github.com/zzstar101/mytoken/internal/harness/mux"
	_ "github.com/zzstar101/mytoken/internal/harness/openclaude"
	_ "github.com/zzstar101/mytoken/internal/harness/openclaw"
	_ "github.com/zzstar101/mytoken/internal/harness/opencode"
	_ "github.com/zzstar101/mytoken/internal/harness/pi"
	_ "github.com/zzstar101/mytoken/internal/harness/qwen"
	_ "github.com/zzstar101/mytoken/internal/harness/roo"
	_ "github.com/zzstar101/mytoken/internal/harness/stepcode"
	_ "github.com/zzstar101/mytoken/internal/harness/workbuddy"
	_ "github.com/zzstar101/mytoken/internal/harness/zcode"
	_ "github.com/zzstar101/mytoken/internal/harness/zed"
	"github.com/zzstar101/mytoken/internal/instance"
	"github.com/zzstar101/mytoken/internal/paths"
	"github.com/zzstar101/mytoken/internal/pricing"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/reconcile"
	"github.com/zzstar101/mytoken/internal/scan"
	"github.com/zzstar101/mytoken/internal/store"
	"sync"
	"time"
)

type App struct {
	cancel   context.CancelFunc
	workers  sync.WaitGroup
	Store    *store.Store
	Scanner  *scan.Scanner
	Settings query.Settings
	Query    query.Service
	Resolver *attrib.Resolver
	Pricing  *pricing.Pricer
	// Sources is every instance the loaded sources.json resolved to, in
	// resolution order, including the disabled and backup-copy entries the
	// scanner ignores. It is the diagnostics surface for the GUI/CLI; see
	// Source.
	Sources []Source
	// SourceProblems is every non-fatal resolution problem (a missing built-in
	// default root, a deduplicated default root). Fatal problems never reach a
	// built App: Open and OpenLocal return them as *SourceError instead.
	SourceProblems []instance.Problem
	// Relays reconciles relays the user turned on (see StartRelaySync).
	Relays      *reconcile.Service
	relayCancel context.CancelFunc
	// Budget evaluates the persisted budget rules against local estimated cost
	// and delivers notifications. It exists even without a notifier; only
	// StartBudget starts its loop.
	Budget *budget.Service

	// claudeQuota is the opt-in Claude Code subscription-quota service. It is
	// always constructed (the GUI needs it to read consent state) but it stays
	// inert until a persisted consent exists; offline apps never auto refresh.
	// Use ClaudeQuotaInfo/EnableClaudeQuota/DisableClaudeQuota/
	// RefreshClaudeQuota/SnapshotClaudeQuota, not the field directly.
	claudeQuota *claudequota.Service

	// zoneMu guards the stats timezone adopted from the store (see
	// EnsureStatsTimezone and Location). zoneConfigured is false while the
	// calendar is unresolved: zoneLoc is then nil and ZoneName is empty, so no
	// caller can mistake the system zone for a fixed persisted choice.
	zoneMu            sync.Mutex
	zoneName          string
	zoneLoc           *time.Location
	zoneErr           error
	zoneConfigured    bool
	zoneSystemPreview string

	// scanMu guards the live scan lifecycle: the scanner a source reload last
	// installed, the resolution it was built from, and the watcher goroutine.
	//
	// Scanner, Sources and SourceProblems above are written once by open and
	// never again, so a CLI command that reads them directly can never race a
	// reload; the GUI reads the accessors in scanlife.go and sources.go, which
	// follow a reload.
	scanMu      sync.Mutex
	scanner     *scan.Scanner
	sources     []Source
	sourceProbs []instance.Problem
	scanWanted  bool
	scanRunning bool
	scanCancel  context.CancelFunc
	scanDone    chan struct{}
	scanFailed  error
	// reloadMu serializes whole source reloads against each other, against
	// StartScanning and against Close. scanMu alone cannot: a reload must wait
	// for the watcher it cancelled, and that watcher takes scanMu itself, so a
	// reload cannot hold scanMu across the wait. Without reloadMu two
	// overlapping reloads can each install a scanner and then race to start
	// one watcher, leaving the other scanner with nothing running it — or
	// start two watchers over the same sources — and a reload can start a
	// watcher after Close has already stopped the scan. It is always taken
	// before scanMu, never the other way round.
	reloadMu sync.Mutex
	// closed is set by Close under reloadMu. A reload or a start that runs
	// after it owns nothing: the store is closing, so it must not install a
	// scanner or start a watcher over it (see ErrClosed).
	closed bool

	// claudeMu guards the Claude quota service, which a successful source
	// reload replaces so a stored consent is re-checked against the sources
	// that exist now. claudeRefresh records whether this app may read a
	// credential at all (an OpenLocal app never may).
	claudeMu      sync.Mutex
	claudeRefresh bool
}

// Options configure the desktop app. The zero value is the CLI's: no budget
// notifier, so nothing is delivered even if a rule is configured.
type Options struct {
	// Notify delivers budget notifications. nil means the service reserves a
	// crossed threshold and reports that no notifier is installed instead of
	// delivering it.
	Notify budget.Notifier
}

func Open() (*App, error) { return open(true, Options{}) }

// OpenWith opens the desktop app with its delivery hooks installed.
func OpenWith(o Options) (*App, error) { return open(true, o) }

// OpenLocal uses the loaded offline prices without starting a network worker.
func OpenLocal() (*App, error) { return open(false, Options{}) }

func open(refresh bool, o Options) (*App, error) {
	dir, e := paths.DataDir()
	if e != nil {
		return nil, e
	}
	// Resolve the configured sources before anything else. A malformed
	// sources.json, an unusable explicit root or an unavailable parser factory
	// must stop here, before a store, a scan or a background refresh exists.
	cfg, e := instance.Load(dir)
	if e != nil {
		return nil, e
	}
	sources, e := resolveSources(cfg, instance.DefaultFallbacks(), harness.NewRooted)
	if e != nil {
		return nil, e
	}
	db, e := paths.DBPath()
	if e != nil {
		return nil, e
	}
	st, e := store.Open(db)
	if e != nil {
		return nil, e
	}
	resolver, e := attrib.New(st)
	if e != nil {
		st.Close()
		return nil, e
	}
	prices := pricing.New(dir)
	a := &App{Store: st, Resolver: resolver, Pricing: prices, Sources: sources.sources, SourceProblems: sources.problems}
	// The live resolution starts as the one just loaded; only a successful
	// ReloadSources replaces it.
	a.sources, a.sourceProbs = sources.sources, sources.problems
	a.Settings = query.NewSettings(st, prices)
	if e = query.LoadPricingSettings(context.Background(), st, prices); e != nil {
		a.Close()
		return nil, e
	}
	// An empty target list scans nothing: a configuration whose entries are all
	// disabled or backup-copies must not fall back to default discovery.
	a.Scanner, e = scan.NewWithInstances(st, resolver, prices, sources.targets...)
	if e != nil {
		a.Close()
		return nil, e
	}
	// The scanner the public field holds is the live one until a reload
	// replaces it; a CLI command reads the field and never reloads.
	a.scanner = a.Scanner
	a.Query = query.NewService(st)
	a.Relays = newRelays(a)
	// The budget service always exists so the GUI can edit rules; its loop only
	// runs after StartBudget (desktop startup), never for a CLI command.
	a.Budget = budget.NewService(st, budget.QueryCosts{Q: a.Query}, o.Notify)
	a.Budget.SetLocation(func() *time.Location { return a.Location() })
	// No threshold is reserved and no notification is delivered while the
	// reporting calendar is unresolved: an ambient system zone must never stand
	// in for a fixed persisted choice (see StartBudget and EnsureStatsTimezone).
	a.Budget.SetZoneReadiness(func() bool { return a.TimezoneState().Configured })
	if e = st.EnsurePrices(context.Background(), prices.Fingerprint(), prices.Evaluate); e != nil {
		a.Close()
		return nil, e
	}
	// The Claude quota service is constructed for every app so consent can be
	// read and edited, but only an online Open starts its refresh loop: an
	// OpenLocal app never reads a credential automatically and never requests.
	a.claudeQuota = newClaudeQuota(st, sources.sources, refresh)
	a.claudeRefresh = refresh
	a.claudeQuota.Start(context.Background())
	if !refresh {
		return a, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.workers.Add(1)
	go func() {
		defer a.workers.Done()
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			// Refresh failure leaves the loaded offline table available.
			_ = prices.Refresh(ctx)
			if ctx.Err() != nil {
				return
			}
			_ = st.EnsurePrices(ctx, prices.Fingerprint(), prices.Evaluate)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return a, nil
}
func (a *App) Close() error {
	if a.relayCancel != nil {
		a.relayCancel()
	}
	if a.cancel != nil {
		a.cancel()
	}
	a.workers.Wait()
	// Stop the watcher and join it before the store it scans through goes
	// away. A reload holds reloadMu for its whole resolve-swap-start sequence,
	// so holding it here means no replacement watcher can be started between
	// the stop below and the store closing — and the watcher a reload just
	// started is stopped here too, instead of outliving the store.
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	a.closed = true
	a.stopScan()
	// Close cancels any in-flight Claude quota request and joins its loop
	// before the store it writes through goes away.
	if q := a.ClaudeQuota(); q != nil {
		q.Close()
	}
	if a.Budget != nil {
		a.Budget.Close()
	}
	if a.Resolver != nil {
		_ = a.Resolver.Close()
	}
	if a.Store != nil {
		return a.Store.Close()
	}
	return nil
}
