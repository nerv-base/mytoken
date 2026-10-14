// Package attrib resolves providers without reading conversation content.
package attrib

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/source"
	"github.com/zzstar101/mytoken/internal/store"
)

type config struct {
	from           int64
	base, provider string
}
type Rule struct {
	Harness  model.Harness `json:"harness"`
	Model    string        `json:"model"`
	From, To time.Time
	Provider string `json:"provider"`
	// Instance scopes the rule to one scanner instance. Empty is the historical
	// and default scope: the rule applies to every instance, default included.
	// A rule naming an instance applies to that instance only, so an operator
	// can pin a provider to one instance without matching another's events.
	Instance string `json:"instance,omitempty"`
}

// scopedTo reports whether a rule may decide attribution for an instance whose
// id is already normalized. An instance-less rule is deliberately global; a
// named rule never leaks to a differently named instance.
func (v Rule) scopedTo(instance string) bool {
	return v.Instance == "" || store.NormalizeInstanceID(v.Instance) == instance
}

// Resolver attributes an event to a provider. Provider data comes from
// optional sources (cc-switch, local harness configs) consulted in priority
// order; with no source at all it still resolves from user rules, the log
// itself and the model name.
type Resolver struct {
	st      *store.Store
	sources []source.Source
	mu      sync.RWMutex
	refresh sync.Mutex
	configs map[model.Harness][]config
	rules   []Rule
	hosts   map[string]string
}

func New(st *store.Store) (*Resolver, error) {
	home, e := os.UserHomeDir()
	if e != nil {
		return nil, e
	}
	return NewWithPaths(st, home, filepath.Join(home, ".cc-switch", "cc-switch.db"))
}
func NewWithPaths(st *store.Store, home, ccPath string) (*Resolver, error) {
	return NewWithSources(st, source.NewCCSwitch(ccPath), source.NewHarnessConfig(home))
}

// NewWithSources builds a resolver over explicit provider sources, in priority
// order. Tests and future embedders use it to inject paths.
func NewWithSources(st *store.Store, sources ...source.Source) (*Resolver, error) {
	r := &Resolver{st: st, sources: sources, configs: map[model.Harness][]config{}, hosts: map[string]string{}}
	if e := r.Refresh(context.Background()); e != nil {
		return nil, e
	}
	// Historical attribution is a pricing boundary, not a display label. The
	// endpoint identity migration handles display grouping without rewriting
	// old providers or invalidating their pricing checkpoint. Resolve below
	// still applies current attribution rules to newly ingested requests.
	return r, nil
}

// migrateBareGeneric re-resolves events an older version attributed to a
// generic log name ("custom") that had no base URL to name it by, and events
// attributed to cc-switch's local proxy: Resolve now looks past both. Costs
// depend on the provider, so the price checkpoint is cleared.
func (r *Resolver) migrateBareGeneric(ctx context.Context) error {
	const key = "attribution.bare-generic.v1"
	done, err := r.st.Setting(ctx, key)
	if err != nil || done == "1" {
		return err
	}
	rows, err := r.st.DB().QueryContext(ctx, `SELECT rowid,harness,timestamp,model,provider,base_url,input,output,cache_read,cache_write,reasoning,resolved_provider,attrib FROM events
		WHERE attrib!=? AND ((attrib=? AND lower(trim(resolved_provider)) IN ('','custom','openai-compatible','default','proxy')) OR (attrib=? AND resolved_provider IN ('127.0.0.1','localhost')))`,
		model.AttribUserRule, model.AttribLog, model.AttribConfig)
	if err != nil {
		return err
	}
	type change struct {
		id               int64
		provider, attrib string
	}
	var changes []change
	for rows.Next() {
		var id, at int64
		var e model.UsageEvent
		var old, oldAttrib string
		if err = rows.Scan(&id, &e.Harness, &at, &e.Model, &e.Provider, &e.BaseURL, &e.Tokens.Input, &e.Tokens.Output, &e.Tokens.CacheRead, &e.Tokens.CacheWrite, &e.Tokens.Reasoning, &old, &oldAttrib); err != nil {
			rows.Close()
			return err
		}
		e.Timestamp = time.Unix(0, at).UTC()
		if p, a := r.Resolve(ctx, e); p != old || string(a) != oldAttrib {
			changes = append(changes, change{id, p, string(a)})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	tx, err := r.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if len(changes) > 0 {
		stmt, err := tx.PrepareContext(ctx, "UPDATE events SET resolved_provider=?, attrib=? WHERE rowid=?")
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, c := range changes {
			if _, err = stmt.ExecContext(ctx, c.provider, c.attrib, c.id); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO settings VALUES('pricing_fingerprint','') ON CONFLICT(key) DO UPDATE SET value=excluded.value"); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO settings VALUES(?,'1') ON CONFLICT(key) DO UPDATE SET value=excluded.value", key); err != nil {
		return err
	}
	return tx.Commit()
}
func (r *Resolver) migrateGenericProviders(ctx context.Context) error {
	done, err := r.st.Setting(ctx, "attribution.generic-host.v1")
	if err != nil || done == "1" {
		return err
	}
	rows, err := r.st.DB().QueryContext(ctx, `SELECT DISTINCT resolved_provider,base_url FROM events WHERE lower(trim(resolved_provider)) IN ('','custom','openai-compatible','default','proxy') AND base_url!='' AND attrib!=?`, model.AttribUserRule)
	if err != nil {
		return err
	}
	type change struct{ old, base, next string }
	var changes []change
	for rows.Next() {
		var old, base string
		if err = rows.Scan(&old, &base); err != nil {
			rows.Close()
			return err
		}
		if next := displayProvider(old, base); next != old {
			changes = append(changes, change{old, base, next})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range changes {
		if err = r.st.Exec(ctx, "UPDATE events SET resolved_provider=? WHERE resolved_provider=? AND base_url=? AND attrib!=?", c.next, c.old, c.base, model.AttribUserRule); err != nil {
			return err
		}
	}
	return r.st.SetSetting(ctx, "attribution.generic-host.v1", "1")
}

// Sources returns the configured provider sources in priority order.
func (r *Resolver) Sources() []source.Source {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]source.Source{}, r.sources...)
}

func (r *Resolver) Close() error {
	r.refresh.Lock()
	defer r.refresh.Unlock()
	var first error
	for _, s := range r.sources {
		if e := s.Close(); e != nil && first == nil {
			first = e
		}
	}
	return first
}

// Resolve attributes an event recorded under the store's default instance. It
// is the legacy entry point and keeps its historical resolution: instance-less
// user rules, the log's own provider, the request matchers and the global
// provider_configs timeline, in that order.
func (r *Resolver) Resolve(ctx context.Context, e model.UsageEvent) (string, model.AttribSource) {
	return r.ResolveInstance(ctx, store.DefaultInstanceID, e)
}

// ResolveInstance is the additive, instance-aware form of Resolve. instance is
// the scanner instance the event was ingested under; the empty string and
// store.DefaultInstanceID both mean the default instance and preserve Resolve
// exactly.
//
// Global provenance is not instance-bound. The request matchers (cc-switch's
// proxy log) and the provider_configs timeline are keyed by harness alone, so
// they cannot say which instance a request or session id belonged to. A named
// instance therefore never borrows them, however exactly its request or session
// ids collide with the default instance's. Without that binding, claiming a
// configured upstream would be a guess: the instance resolves from its own
// recorded log endpoint, from user rules (an instance-less rule is deliberately
// global; a rule may also name this instance), and otherwise falls back to the
// explicit inferred/unknown model name.
func (r *Resolver) ResolveInstance(_ context.Context, instance string, e model.UsageEvent) (string, model.AttribSource) {
	inst := store.NormalizeInstanceID(instance)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, v := range r.rules {
		if (v.Harness == "" || v.Harness == e.Harness) && (v.Model == "" || v.Model == e.Model) && v.scopedTo(inst) && (v.From.IsZero() || !e.Timestamp.Before(v.From)) && (v.To.IsZero() || e.Timestamp.Before(v.To)) {
			return v.Provider, model.AttribUserRule
		}
	}
	// A generic name ("custom") without a base URL says nothing: look further.
	// A recorded endpoint is exact and belongs to whichever instance wrote it.
	if provider := displayProvider(e.Provider, e.BaseURL); provider != "" && !generic(provider) {
		return provider, model.AttribLog
	}
	if inst != store.DefaultInstanceID {
		// The matchers and the config timeline below are global, so a named
		// instance stops here rather than inherit another profile's upstream.
		return Infer(e.Model), model.AttribInferred
	}
	// Request-level sources (cc-switch's proxy log) come next, in priority order.
	for _, s := range r.sources {
		m, ok := s.(source.Matcher)
		if !ok {
			continue
		}
		if provider, ok := m.Match(e); ok {
			return displayProvider(provider, e.BaseURL), model.AttribCCSwitch
		}
	}
	configs := r.configs[e.Harness]
	at := e.Timestamp.UnixNano()
	for i := len(configs) - 1; i >= 0; i-- {
		// While a harness pointed at cc-switch's proxy, the provider behind it
		// is the one cc-switch had selected, recorded before the switch.
		if configs[i].from <= at && configs[i].provider != "" && !source.IsLocalProxy(configs[i].base) {
			return displayProvider(configs[i].provider, e.BaseURL), model.AttribConfig
		}
	}
	return Infer(e.Model), model.AttribInferred
}

// generic reports whether a provider name only says "some gateway".
func generic(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", "custom", "openai-compatible", "default", "proxy":
		return true
	}
	return false
}

// displayProvider preserves named providers and identifies generic relays by host.
func displayProvider(provider, base string) string {
	if generic(provider) && !source.IsLocalProxy(base) {
		u, err := url.Parse(base)
		if err == nil && u.Hostname() != "" && (u.Scheme == "https" || u.Scheme == "http") {
			host := strings.ToLower(u.Hostname())
			if strings.Contains(host, ":") {
				host = "[" + host + "]"
			}
			port := u.Port()
			if port != "" && !(u.Scheme == "https" && port == "443") && !(u.Scheme == "http" && port == "80") {
				host += ":" + port
			}
			return host
		}
	}
	return provider
}

func Infer(name string) string {
	name = strings.ToLower(name)
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	for _, v := range []struct{ prefix, provider string }{{"claude-", "anthropic"}, {"gpt-", "openai"}, {"chatgpt-", "openai"}, {"o1", "openai"}, {"o3", "openai"}, {"o4", "openai"}, {"gemini-", "google"}, {"deepseek-", "deepseek"}, {"kimi-", "moonshot"}, {"moonshot-", "moonshot"}, {"glm-", "zhipu"}, {"qwen", "alibaba"}, {"mistral", "mistral"}, {"codestral", "mistral"}} {
		if strings.HasPrefix(name, v.prefix) {
			return v.provider
		}
	}
	return "unknown"
}

// ProviderForURL maps a host to a provider; unknown gateways retain their hostname.
func ProviderForURL(base string) string {
	u, e := url.Parse(base)
	if e != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	for _, v := range []struct{ domain, provider string }{{"anthropic.com", "anthropic"}, {"openai.com", "openai"}, {"googleapis.com", "google"}, {"deepseek.com", "deepseek"}, {"moonshot.cn", "moonshot"}, {"moonshot.ai", "moonshot"}, {"bigmodel.cn", "zhipu"}, {"aliyuncs.com", "alibaba"}, {"openrouter.ai", "openrouter"}, {"siliconflow.cn", "siliconflow"}, {"siliconflow.com", "siliconflow"}, {"volces.com", "bytedance"}, {"mistral.ai", "mistral"}} {
		if host == v.domain || strings.HasSuffix(host, "."+v.domain) {
			return v.provider
		}
	}
	return host
}
func sanitizedURL(base string) string {
	u, e := url.Parse(base)
	if e != nil || u.Hostname() == "" {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
func (r *Resolver) Refresh(ctx context.Context) error {
	r.refresh.Lock()
	defer r.refresh.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if raw, e := r.st.Setting(ctx, "attribution.rules"); e == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &r.rules)
	}
	if raw, e := r.st.Setting(ctx, "attribution.hosts"); e == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &r.hosts)
	}
	for _, s := range r.sources {
		if e := s.Load(ctx); e != nil {
			return e
		}
		if c, ok := s.(source.Configs); ok {
			if e := r.backfillConfigs(ctx, c.CurrentConfigs()); e != nil {
				return e
			}
		}
	}
	rows, e := r.st.DB().QueryContext(ctx, "SELECT harness,from_time,base_url,provider FROM provider_configs ORDER BY from_time")
	if e != nil {
		return e
	}
	defer rows.Close()
	configs := map[model.Harness][]config{}
	for rows.Next() {
		var h model.Harness
		var c config
		if e = rows.Scan(&h, &c.from, &c.base, &c.provider); e != nil {
			return e
		}
		configs[h] = append(configs[h], c)
	}
	if e = rows.Err(); e != nil {
		return e
	}
	r.configs = configs
	return nil
}

// backfillConfigs appends a source's current selections to the provider_configs
// timeline: a selection that predates the timeline anchors it at time 0, a live
// one only when it differs from the latest entry.
func (r *Resolver) backfillConfigs(ctx context.Context, list []source.CurrentConfig) error {
	for _, v := range list {
		base, provider := v.Base, v.Provider
		if base != "" {
			base = sanitizedURL(base)
			if base != "" {
				host := ProviderForURL(base)
				if custom, ok := r.hosts[host]; ok {
					host = custom
				}
				provider = host
			}
		}
		if provider == "" {
			continue
		}
		if v.SeedAtZero {
			if e := r.st.Exec(ctx, `INSERT INTO provider_configs(harness,from_time,base_url,provider) SELECT ?,0,'',? WHERE NOT EXISTS(SELECT 1 FROM provider_configs WHERE harness=?)`, v.Harness, provider, v.Harness); e != nil {
				return e
			}
			continue
		}
		var oldBase, oldProvider string
		e := r.st.DB().QueryRowContext(ctx, "SELECT base_url,provider FROM provider_configs WHERE harness=? ORDER BY from_time DESC LIMIT 1", v.Harness).Scan(&oldBase, &oldProvider)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if oldBase != base || oldProvider != provider {
			if e = r.st.Exec(ctx, "INSERT INTO provider_configs VALUES(?,?,?,?)", v.Harness, time.Now().UnixNano(), base, provider); e != nil {
				return e
			}
		}
	}
	return nil
}
func (r *Resolver) String() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return fmt.Sprintf("attribution: %d sources, %d rules", len(r.sources), len(r.rules))
}
