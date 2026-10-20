package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zzstar101/mytoken/internal/app"
	"github.com/zzstar101/mytoken/internal/gui"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/quota/plan"
)

// guiDeps is the interface's view of the opened app: adapters from the app's
// services to the narrow interfaces internal/gui declares.
func guiDeps(a *app.App) gui.Deps {
	p := &prefs{a: a, sys: time.Local}
	p.applyZone(p.Pref(gui.PrefTimezone))
	return gui.Deps{
		Query:   a.Query,
		Quotas:  func() []gui.Quota { return quotas(a.Quota) },
		Sources: sources{a},
		Prices:  prices{a},
		Maint:   maint{a},
		Prefs:   p,
		Version: versionLabel(),
		Export:  exporter{a},
	}
}

// quotas converts the plan snapshots of the tools that are signed in.
func quotas(s *plan.Service) []gui.Quota {
	if s == nil {
		return nil
	}
	problems := map[string]string{}
	signIn := map[string]bool{}
	for _, st := range s.Statuses() {
		switch st.Reason {
		case plan.ReasonNone:
		case plan.ReasonUnauthorized, plan.ReasonForbidden, plan.ReasonTokenExpired:
			// Signed in once, but the service no longer takes the login.
			if st.Detected {
				signIn[st.Provider] = true
			}
		default:
			problems[st.Provider] = st.Reason.Text()
		}
	}
	var out []gui.Quota
	now := time.Now()
	for _, snap := range s.Detected() {
		q := gui.Quota{
			ID: snap.Provider, Name: quotaName(snap.Provider), Plan: plan.PlanLabel(snap.Plan),
			Source: snap.Source, FetchedAt: snap.FetchedAt, Err: problems[snap.Provider],
			SignIn: signIn[snap.Provider],
		}
		delete(signIn, snap.Provider)
		for _, w := range snap.Windows {
			if !w.Valid() {
				continue
			}
			qw := gui.QuotaWindow{
				Name: w.Name, UsedPercent: w.UsedPercent, Used: w.Used, Limit: w.Limit, Unit: w.Unit,
				Balance: w.Balance, Remaining: w.Remaining, ResetsAt: w.ResetsAt, ExpiresAt: w.ExpiresAt,
			}
			if !w.Balance && !w.ResetsAt.IsZero() && w.ResetsAt.Before(now) {
				// The snapshot predates the window's reset: it is fresh again.
				qw.UsedPercent, qw.Used, qw.ResetsAt = 0, 0, time.Time{}
			}
			q.Windows = append(q.Windows, qw)
		}
		for _, d := range snap.Daily {
			q.Daily = append(q.Daily, gui.QuotaDay{Day: d.Day, Used: d.Used, Known: d.Known})
		}
		if len(q.Windows) > 0 || q.SignIn {
			out = append(out, q)
		}
	}
	// Accounts whose login was turned away before any figure came in.
	for _, st := range s.Statuses() {
		if signIn[st.Provider] {
			out = append(out, gui.Quota{ID: st.Provider, Name: quotaName(st.Provider), SignIn: true})
		}
	}
	return out
}

func quotaName(id string) string {
	switch id {
	case "codex":
		return "Codex"
	case "claude":
		return "Claude"
	case "cursor":
		return "Cursor"
	case "copilot":
		return "GitHub Copilot"
	case "glm":
		return "GLM"
	case "deepseek":
		return "DeepSeek"
	case "trae-cn":
		return "Trae"
	case "trae-global":
		return "Trae Global"
	case "workbuddy":
		return "WorkBuddy"
	case "workbuddy-ai":
		return "WorkBuddy AI"
	case "qoder-cn":
		return "Qoder"
	case "zcode":
		return "ZCode"
	}
	if id == "" {
		return id
	}
	return strings.ToUpper(id[:1]) + id[1:]
}

type sources struct{ a *app.App }

func (s sources) List(ctx context.Context) ([]gui.Source, error) {
	list, err := s.a.ListSources(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]gui.Source, len(list))
	for i, v := range list {
		out[i] = gui.Source{
			Harness: v.Harness, Detected: v.Detected, Enabled: v.Enabled, Roots: v.Roots,
			Override: v.Override, Problem: v.Problem, Requests: v.Requests, LastSeen: v.LastSeen,
		}
	}
	return out, nil
}

func (s sources) SetEnabled(ctx context.Context, h model.Harness, on bool) error {
	return s.a.SetSourceEnabled(ctx, h, on)
}

func (s sources) SetRoot(ctx context.Context, h model.Harness, dir string) error {
	return s.a.SetSourceRoot(ctx, h, dir)
}

func (s sources) Rescan(ctx context.Context) error { return s.a.Rescan(ctx) }

type prices struct{ a *app.App }

func (p prices) Unpriced(ctx context.Context) ([]query.UnpricedModel, error) {
	return p.a.Settings.UnpricedModels(ctx)
}

func (p prices) Aliases(ctx context.Context) ([]query.ModelAlias, error) {
	return p.a.Settings.ModelAliases(ctx)
}

func (p prices) SetAliases(ctx context.Context, v []query.ModelAlias) error {
	return p.a.Settings.SetModelAliases(ctx, v)
}

func (p prices) CustomPrices(ctx context.Context) ([]gui.CustomPrice, error) {
	list, err := p.a.CustomPrices(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]gui.CustomPrice, len(list))
	for i, v := range list {
		out[i] = gui.CustomPrice{Model: v.Model, Input: v.Input, Output: v.Output, CacheRead: v.CacheRead, CacheWrite: v.CacheWrite}
	}
	return out, nil
}

func (p prices) SetCustomPrices(ctx context.Context, v []gui.CustomPrice) error {
	in := make([]app.CustomPrice, len(v))
	for i, c := range v {
		in[i] = app.CustomPrice{Model: c.Model, Input: c.Input, Output: c.Output, CacheRead: c.CacheRead, CacheWrite: c.CacheWrite}
	}
	return p.a.SetCustomPrices(ctx, in)
}

func (p prices) SearchCatalog(ctx context.Context, q string, limit int) ([]string, error) {
	return p.a.Settings.SearchCatalog(ctx, q, limit)
}

func (p prices) Catalog() gui.CatalogInfo {
	v, n := p.a.CatalogInfo()
	return gui.CatalogInfo{Version: v, Models: n}
}

type maint struct{ a *app.App }

func (m maint) Info(ctx context.Context) (gui.DBInfo, error) {
	i, err := m.a.Info(ctx)
	return gui.DBInfo{Path: i.Path, Bytes: i.Bytes, Events: i.Events, Sessions: i.Sessions, Oldest: i.Oldest, Newest: i.Newest}, err
}

func (m maint) Rebuild(ctx context.Context) error { return m.a.Rebuild(ctx) }
func (m maint) Compact(ctx context.Context) error { return m.a.Compact(ctx) }

// prefs keeps the interface preferences in the index's settings table, with
// an in-memory copy so drawing never waits on the database.
type prefs struct {
	a   *app.App
	sys *time.Location // the zone the system had at launch

	mu    sync.Mutex
	cache map[string]string
}

const prefPrefix = "gui."

func (p *prefs) Pref(k string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v, ok := p.cache[k]; ok {
		return v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	v, _ := p.a.Store.Setting(ctx, prefPrefix+k)
	if p.cache == nil {
		p.cache = map[string]string{}
	}
	p.cache[k] = v
	return v
}

func (p *prefs) SetPref(k, v string) error {
	if k == gui.PrefTimezone {
		if err := p.applyZone(v); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.a.Store.SetSetting(ctx, prefPrefix+k, v); err != nil {
		return err
	}
	p.mu.Lock()
	if p.cache == nil {
		p.cache = map[string]string{}
	}
	p.cache[k] = v
	p.mu.Unlock()
	return nil
}

// applyZone makes days and months count in the chosen zone: the queries and
// the interface both bucket by time.Local. "" goes back to the system zone.
func (p *prefs) applyZone(name string) error {
	if name == "" {
		time.Local = p.sys
		p.rebucket()
		return nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return fmt.Errorf("unknown timezone %q", name)
	}
	time.Local = loc
	p.rebucket()
	return nil
}

// rebucket rebuilds the daily rollups for the zone now in time.Local, in the
// background: until it lands, queries read the facts directly (correct, only
// slower), and the store's change signal reloads the interface afterwards.
func (p *prefs) rebucket() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := p.a.Store.EnsureLocalDays(ctx); err != nil {
			logErr("timezone rollup", err)
		}
	}()
}

// exporter writes every session as one CSV row.
type exporter struct{ a *app.App }

func (e exporter) Export(ctx context.Context, path string) error {
	rows, _, err := e.a.Query.Sessions(ctx, query.Filter{}, query.SortRecent, 0, 0)
	if err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"tool", "session", "title", "project", "started", "updated", "requests",
		"input", "output", "cache_read", "cache_write", "reasoning", "cost_usd_api_equivalent", "models"})
	for _, r := range rows {
		var models []string
		for _, b := range r.Breakdown {
			models = append(models, b.Model)
		}
		_ = w.Write([]string{
			string(r.Harness), r.SessionID, r.Title, r.Project,
			r.StartedAt.Format(time.RFC3339), r.UpdatedAt.Format(time.RFC3339),
			strconv.FormatInt(r.Requests, 10),
			strconv.FormatInt(r.Tokens.Input, 10), strconv.FormatInt(r.Tokens.Output, 10),
			strconv.FormatInt(r.Tokens.CacheRead, 10), strconv.FormatInt(r.Tokens.CacheWrite, 10),
			strconv.FormatInt(r.Tokens.Reasoning, 10),
			strconv.FormatFloat(r.CostUSD, 'f', 6, 64), strings.Join(models, " "),
		})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func logErr(what string, err error) {
	if err != nil {
		log.Printf("mytoken: %s: %v", what, err)
	}
}
