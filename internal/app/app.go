// Package app wires the backend and registered parsers.
package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zzstar101/mytoken/internal/attrib"
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
	"github.com/zzstar101/mytoken/internal/ingestrate"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/paths"
	"github.com/zzstar101/mytoken/internal/pricing"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/quota/plan"
	"github.com/zzstar101/mytoken/internal/quota/providers"
	"github.com/zzstar101/mytoken/internal/scan"
	"github.com/zzstar101/mytoken/internal/store"
)

var (
	// ErrClosed reports a scan operation on a closed app.
	ErrClosed = errors.New("app: closed")
	// ErrNoScanner reports an app with no live scanner.
	ErrNoScanner = errors.New("app: no scanner is available")
)

const (
	// EnabledKey is the settings key that toggles one harness. A missing row
	// means enabled; the literal value "false" disables it.
	EnabledKey = "harness.%s.enabled"
	// RootKey is the settings key that overrides one harness's scan roots.
	RootKey = "harness.%s.root"
)

// DBInfo describes the index file and the history it holds.
type DBInfo struct {
	Path     string    `json:"path"`
	Bytes    int64     `json:"bytes"`
	Events   int64     `json:"events"`
	Sessions int64     `json:"sessions"`
	Oldest   time.Time `json:"oldest"`
	Newest   time.Time `json:"newest"`
}

// Source is one harness row in the settings' data-source panel.
type Source struct {
	Harness  model.Harness `json:"harness"`
	Detected bool          `json:"detected"`
	Enabled  bool          `json:"enabled"`
	Roots    []string      `json:"roots"`
	Override string        `json:"override"`
	Problem  string        `json:"problem"`
	Requests int64         `json:"requests"`
	LastSeen time.Time     `json:"lastSeen"`
}

// CustomPrice is a per-model override in USD per 1M tokens.
type CustomPrice struct {
	Model      string  `json:"model"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

// App owns the index, the scan lifecycle and the plan-quota service.
type App struct {
	Path     string
	Store    *store.Store
	Scanner  *scan.Scanner
	Settings query.Settings
	Query    query.Service
	Resolver *attrib.Resolver
	Pricing  *pricing.Pricer
	Quota    *plan.Service

	zoneMu            sync.Mutex
	zoneName          string
	zoneLoc           *time.Location
	zoneErr           error
	zoneConfigured    bool
	zoneSystemPreview string

	cancel  context.CancelFunc
	workers sync.WaitGroup

	scanMu     sync.Mutex
	scanner    *scan.Scanner
	scanWanted bool
	scanCancel context.CancelFunc
	scanDone   chan struct{}
	scanFailed error

	reloadMu sync.Mutex
	closed   bool
}

// Open opens the writable app and refreshes prices in the background.
func Open() (*App, error) { return open(true) }

// OpenLocal behaves like Open but never uses the network.
func OpenLocal() (*App, error) { return open(false) }

func open(refresh bool) (*App, error) {
	dir, e := paths.DataDir()
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
	a := &App{Path: db, Store: st, Resolver: resolver, Pricing: prices}
	a.Settings = query.NewSettings(st, prices)
	if e = query.LoadPricingSettings(context.Background(), st, prices); e != nil {
		a.Close()
		return nil, e
	}
	a.Query = query.NewService(st)
	parsers, _ := a.parserSet(context.Background())
	a.Scanner = scan.New(st, resolver, prices, parsers...)
	a.scanner = a.Scanner
	svc, e := plan.NewService(plan.Options{
		Providers: providers.All(providers.Options{Store: st}),
		Store:     st,
		Offline:   !refresh,
	})
	if e != nil {
		a.Close()
		return nil, e
	}
	a.Quota = svc
	svc.Start()
	if e = st.EnsurePrices(context.Background(), prices.Fingerprint(), prices.Evaluate); e != nil {
		a.Close()
		return nil, e
	}
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

// Close stops the scanner, quota service and workers, then closes the store.
func (a *App) Close() error {
	if a.cancel != nil {
		a.cancel()
	}
	a.workers.Wait()
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	a.scanMu.Lock()
	cancel, done := a.scanCancel, a.scanDone
	a.scanWanted = false
	a.scanMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	if a.Quota != nil {
		_ = a.Quota.Close()
	}
	if a.Resolver != nil {
		_ = a.Resolver.Close()
	}
	if a.Store != nil {
		return a.Store.Close()
	}
	return nil
}

func enabledSettingKey(h model.Harness) string { return fmt.Sprintf(EnabledKey, h) }
func rootSettingKey(h model.Harness) string    { return fmt.Sprintf(RootKey, h) }

func readSetting(ctx context.Context, st *store.Store, key string) (string, error) {
	v, err := st.Setting(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// parserSet returns the parsers enabled by settings, applying root overrides.
// The second return value carries per-harness problems in harness order.
func (a *App) parserSet(ctx context.Context) ([]harness.Parser, map[model.Harness]string) {
	problems := map[model.Harness]string{}
	var out []harness.Parser
	for _, p := range harness.All() {
		h := p.Harness()
		enabled, _ := readSetting(ctx, a.Store, enabledSettingKey(h))
		if strings.EqualFold(strings.TrimSpace(enabled), "false") {
			continue
		}
		root, _ := readSetting(ctx, a.Store, rootSettingKey(h))
		root = strings.TrimSpace(root)
		if root != "" {
			if err := harness.ValidateRoot(p, root); err != nil {
				problems[h] = err.Error()
				continue
			}
			if r, ok := p.(harness.Rooted); ok {
				p = r.WithRoots(root)
			}
		}
		out = append(out, p)
	}
	return out, problems
}

// ListSources reports every compiled harness with its settings and usage.
func (a *App) ListSources(ctx context.Context) ([]Source, error) {
	rows, err := a.Store.ReadDB().QueryContext(ctx, `SELECT harness, count(*), max(timestamp) FROM event_data GROUP BY harness`)
	if err != nil {
		return nil, err
	}
	type usage struct {
		requests int64
		last     int64
	}
	counts := map[model.Harness]usage{}
	for rows.Next() {
		var h string
		var u usage
		if err := rows.Scan(&h, &u.requests, &u.last); err != nil {
			rows.Close()
			return nil, err
		}
		counts[model.Harness(h)] = u
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	parsers, problems := a.parserSet(ctx)
	active := map[model.Harness]harness.Parser{}
	for _, p := range parsers {
		active[p.Harness()] = p
	}
	all := harness.All()
	sort.Slice(all, func(i, j int) bool { return all[i].Harness() < all[j].Harness() })
	out := make([]Source, 0, len(all))
	for _, p := range all {
		h := p.Harness()
		enabled, _ := readSetting(ctx, a.Store, enabledSettingKey(h))
		root, _ := readSetting(ctx, a.Store, rootSettingKey(h))
		src := Source{Harness: h, Enabled: !strings.EqualFold(strings.TrimSpace(enabled), "false"), Override: strings.TrimSpace(root)}
		probe := p
		if src.Override != "" {
			if r, ok := p.(harness.Rooted); ok {
				probe = r.WithRoots(src.Override)
			}
		}
		status := harness.Detect(probe)
		src.Detected = status.Detected
		src.Roots = status.Roots
		src.Problem = status.Problem
		if problems[h] != "" {
			src.Problem = problems[h]
			src.Detected = false
		}
		if u := counts[h]; u.requests > 0 {
			src.Requests = u.requests
			src.LastSeen = time.Unix(0, u.last).UTC()
		}
		out = append(out, src)
	}
	return out, nil
}

// SetSourceEnabled toggles one harness and rebuilds the scanner.
func (a *App) SetSourceEnabled(ctx context.Context, h model.Harness, on bool) error {
	if _, ok := harness.Get(h); !ok {
		return fmt.Errorf("unknown harness %q", h)
	}
	value := "false"
	if on {
		value = "true"
	}
	if err := a.Store.SetSetting(ctx, enabledSettingKey(h), value); err != nil {
		return err
	}
	return a.Rescan(ctx)
}

// SetSourceRoot saves a root override ("" = automatic roots) after validating
// that the directory really holds data for this harness.
func (a *App) SetSourceRoot(ctx context.Context, h model.Harness, dir string) error {
	p, ok := harness.Get(h)
	if !ok {
		return fmt.Errorf("unknown harness %q", h)
	}
	dir = strings.TrimSpace(dir)
	if dir != "" {
		if err := harness.ValidateRoot(p, dir); err != nil {
			return err
		}
	}
	if err := a.Store.SetSetting(ctx, rootSettingKey(h), dir); err != nil {
		return err
	}
	return a.Rescan(ctx)
}

// Rescan rebuilds the parser set from settings and replaces the live scanner.
// An already-running watcher is restarted on the new set.
func (a *App) Rescan(ctx context.Context) error {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	if a.closed {
		return ErrClosed
	}
	parsers, _ := a.parserSet(ctx)
	next := scan.New(a.Store, a.Resolver, a.Pricing, parsers...)
	a.scanMu.Lock()
	cancel, done := a.scanCancel, a.scanDone
	a.scanCancel, a.scanDone = nil, nil
	a.scanMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	a.scanMu.Lock()
	a.scanner = next
	a.Scanner = next
	wanted := a.scanWanted
	if wanted {
		scanCtx, cancel := context.WithCancel(context.Background())
		a.scanCancel = cancel
		done := make(chan struct{})
		a.scanDone = done
		go a.runScanner(next, scanCtx, done)
	}
	a.scanMu.Unlock()
	return nil
}

func (a *App) runScanner(sc *scan.Scanner, ctx context.Context, done chan struct{}) {
	defer close(done)
	if err := sc.Run(ctx); err != nil {
		a.scanMu.Lock()
		a.scanFailed = err
		a.scanMu.Unlock()
	}
}

// StartScanning starts the watcher over the current scanner once.
func (a *App) StartScanning(ctx context.Context) error {
	a.scanMu.Lock()
	defer a.scanMu.Unlock()
	if a.closed {
		return ErrClosed
	}
	if a.scanWanted {
		return nil
	}
	a.scanWanted = true
	scanCtx, cancel := context.WithCancel(ctx)
	a.scanCancel = cancel
	done := make(chan struct{})
	a.scanDone = done
	go a.runScanner(a.scanner, scanCtx, done)
	return nil
}

// ScannerProgress reports the current scan's completed and total sources.
func (a *App) ScannerProgress() (done, total int) {
	if sc := a.CurrentScanner(); sc != nil {
		return sc.Progress()
	}
	return 0, 0
}

// ScannerIngestRate reports the committed output-token rate of the live scan.
func (a *App) ScannerIngestRate(now time.Time) ingestrate.Snapshot {
	if sc := a.CurrentScanner(); sc != nil {
		return sc.IngestRate(now)
	}
	return ingestrate.Snapshot{}
}

// CurrentScanner returns the live scanner.
func (a *App) CurrentScanner() *scan.Scanner {
	a.scanMu.Lock()
	defer a.scanMu.Unlock()
	return a.scanner
}

// ScanError reports why the watcher stopped on its own.
func (a *App) ScanError() error {
	a.scanMu.Lock()
	defer a.scanMu.Unlock()
	return a.scanFailed
}

// CustomPrices lists the stored per-model overrides.
func (a *App) CustomPrices(ctx context.Context) ([]CustomPrice, error) {
	rules, err := a.Settings.PriceRules(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]CustomPrice, 0, len(rules))
	for _, r := range rules {
		if r.Model == "" {
			continue // provider-wide rules are not per-model overrides
		}
		p := CustomPrice{Model: r.Model}
		if r.Input != nil {
			p.Input = *r.Input
		}
		if r.Output != nil {
			p.Output = *r.Output
		}
		if r.CacheRead != nil {
			p.CacheRead = *r.CacheRead
		}
		if r.CacheWrite != nil {
			p.CacheWrite = *r.CacheWrite
		}
		out = append(out, p)
	}
	return out, nil
}

// SetCustomPrices replaces the per-model overrides and reprices the index.
func (a *App) SetCustomPrices(ctx context.Context, prices []CustomPrice) error {
	rules := make([]query.PriceRule, 0, len(prices))
	for _, p := range prices {
		model := strings.TrimSpace(p.Model)
		if model == "" {
			return errors.New("custom price requires a model")
		}
		in, out, cr, cw := p.Input, p.Output, p.CacheRead, p.CacheWrite
		rules = append(rules, query.PriceRule{
			Model:      model,
			Input:      &in,
			Output:     &out,
			CacheRead:  &cr,
			CacheWrite: &cw,
			Source:     "user",
		})
	}
	return a.Settings.SetPriceRules(ctx, rules)
}

// CatalogInfo reports the loaded price catalog version and model count.
func (a *App) CatalogInfo() (string, int) { return a.Pricing.Catalog() }

// QuotaService exposes the plan-quota service (nil-safe for callers).
func (a *App) QuotaService() *plan.Service { return a.Quota }

// RefreshQuotas fetches every detected plan now.
func (a *App) RefreshQuotas(ctx context.Context) {
	if a.Quota != nil {
		a.Quota.Refresh(ctx)
	}
}

// Info describes the index file.
func (a *App) Info(ctx context.Context) (DBInfo, error) {
	out := DBInfo{Path: a.Path}
	if fi, err := osStat(a.Path); err == nil {
		out.Bytes = fi
	}
	row := a.Store.ReadDB().QueryRowContext(ctx, `SELECT count(*), coalesce(min(timestamp),0), coalesce(max(timestamp),0) FROM event_data`)
	var oldest, newest int64
	if err := row.Scan(&out.Events, &oldest, &newest); err != nil {
		return out, err
	}
	if err := a.Store.ReadDB().QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&out.Sessions); err != nil {
		return out, err
	}
	if oldest > 0 {
		out.Oldest = time.Unix(0, oldest).UTC()
	}
	if newest > 0 {
		out.Newest = time.Unix(0, newest).UTC()
	}
	return out, nil
}

// Compact rewrites the database file, reclaiming free pages.
func (a *App) Compact(ctx context.Context) error {
	return a.Store.Exec(ctx, "VACUUM")
}

// Rebuild empties the index and scans every enabled harness again.
func (a *App) Rebuild(ctx context.Context) error {
	if err := a.Store.Exec(ctx, `DELETE FROM event_data; DELETE FROM event_dimensions; DELETE FROM sessions; DELETE FROM cursors`); err != nil {
		return err
	}
	sc := a.CurrentScanner()
	if sc == nil {
		return ErrNoScanner
	}
	return sc.Rebuild(ctx)
}

// osStat reports the file size of path.
func osStat(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}
