package gui

import (
	"context"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

// demo is an in-memory Query over synthetic, deterministic usage, so pages
// can be rendered and timed without a database.
type demo struct {
	now      time.Time
	sessions []demoSession
}

type demoSession struct {
	meta   model.SessionMeta
	events []query.AttributedEvent
}

type demoTool struct {
	h      model.Harness
	models []string
	weight float64
}

var demoTools = []demoTool{
	{model.Codex, []string{"gpt-5.5", "gpt-5.5-mini"}, 5},
	{model.ClaudeCode, []string{"claude-opus-4-6", "claude-sonnet-4-6"}, 3},
	{model.DSH, []string{"deepseek-v4", "claude-opus-4-6"}, 2},
	{model.OpenCode, []string{"kimi-k2.5"}, 0.7},
	{model.Pi, []string{"gemini-3-pro"}, 0.5},
	{model.Gemini, []string{"gemini-3-flash"}, 0.3},
}

// demoPrice is USD per 1M input tokens; output is 5x, cache read 0.1x.
var demoPrice = map[string]float64{
	"gpt-5.5": 1.25, "gpt-5.5-mini": 0.25, "claude-opus-4-6": 5, "claude-sonnet-4-6": 3,
	"deepseek-v4": 0.3, "kimi-k2.5": 0.6, "gemini-3-pro": 2, "gemini-3-flash": 0.3,
}

var demoProjects = []string{
	"/Users/me/code/mytoken", "/Users/me/code/website", "/Users/me/code/infra",
	"/Users/me/code/mobile-app", "/Users/me/work/billing-service", "/Users/me/notes",
}

var demoTitles = []string{
	"Fix flaky scanner test", "Redesign the overview page", "Migrate to new chart API",
	"Investigate memory growth", "Add Cursor quota provider", "Write release notes",
	"Refactor pricing catalog", "Speed up session queries", "Tray panel polish",
	"Explain the cache hit rate", "Port settings to sidebar", "Review dependency updates",
}

func newDemo(now time.Time) *demo {
	r := rand.New(rand.NewPCG(7, 11))
	d := &demo{now: now}
	day0 := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	n := 0
	for back := 400; back >= 0; back-- {
		day := day0.AddDate(0, 0, -back)
		// Busier on weekdays, quieter long ago, with idle days.
		activity := 0.4 + 0.6*float64(400-back)/400
		if wd := day.Weekday(); wd == time.Saturday || wd == time.Sunday {
			activity *= 0.35
		}
		if r.Float64() < 0.12 {
			continue
		}
		count := int(math.Round(activity * (1 + r.Float64()*4)))
		for range count {
			tool := pickTool(r)
			start := day.Add(time.Duration(8+r.IntN(13))*time.Hour + time.Duration(r.IntN(60))*time.Minute)
			if back == 0 && start.After(now) {
				start = now.Add(-time.Duration(5+r.IntN(90)) * time.Minute)
			}
			d.sessions = append(d.sessions, makeSession(r, n, tool, start, now))
			n++
		}
	}
	// Two sessions running right now, for the tray.
	d.sessions = append(d.sessions, makeSession(r, n, demoTools[0], now.Add(-40*time.Minute), now), makeSession(r, n+1, demoTools[1], now.Add(-12*time.Minute), now))
	return d
}

func pickTool(r *rand.Rand) demoTool {
	var sum float64
	for _, t := range demoTools {
		sum += t.weight
	}
	x := r.Float64() * sum
	for _, t := range demoTools {
		if x -= t.weight; x <= 0 {
			return t
		}
	}
	return demoTools[0]
}

func makeSession(r *rand.Rand, n int, tool demoTool, start, now time.Time) demoSession {
	id := strings.Repeat("0", 4) + string(rune('a'+n%26)) + time.Duration(n).String()
	meta := model.SessionMeta{
		Harness: tool.h, SessionID: id, Title: demoTitles[n%len(demoTitles)],
		Project: demoProjects[r.IntN(len(demoProjects))], StartedAt: start,
	}
	s := demoSession{meta: meta}
	t := start
	reqs := 4 + r.IntN(60)
	ctxTokens := int64(8000 + r.IntN(20000))
	for i := range reqs {
		t = t.Add(time.Duration(4+r.IntN(40)) * time.Second)
		if t.After(now) {
			break
		}
		m := tool.models[0]
		if len(tool.models) > 1 && r.Float64() < 0.25 {
			m = tool.models[1]
		}
		in := int64(200 + r.IntN(3000))
		out := int64(100 + r.IntN(1800))
		read := ctxTokens
		ctxTokens += in + out
		write := int64(0)
		if i%9 == 0 {
			write = in * 3
		}
		tk := model.Tokens{Input: in, Output: out, CacheRead: read, CacheWrite: write}
		p := demoPrice[m] / 1e6
		cost := float64(in)*p + float64(out)*p*5 + float64(read)*p*0.1 + float64(write)*p*1.25
		s.events = append(s.events, query.AttributedEvent{
			UsageEvent: model.UsageEvent{Harness: tool.h, SessionID: id, Timestamp: t, Model: m, Tokens: tk, ProjectPath: meta.Project},
			Cost:       cost, Priced: true, ResolvedProvider: providerOf(m),
		})
	}
	if len(s.events) > 0 {
		s.meta.UpdatedAt = s.events[len(s.events)-1].Timestamp
	} else {
		s.meta.UpdatedAt = start
	}
	return s
}

func providerOf(m string) string {
	switch {
	case strings.HasPrefix(m, "gpt"):
		return "openai"
	case strings.HasPrefix(m, "claude"):
		return "anthropic"
	case strings.HasPrefix(m, "gemini"):
		return "google"
	case strings.HasPrefix(m, "kimi"):
		return "moonshot"
	}
	return "deepseek"
}

func keep(f query.Filter, e query.AttributedEvent) bool {
	if !f.Range.From.IsZero() && e.Timestamp.Before(f.Range.From) {
		return false
	}
	if !f.Range.To.IsZero() && !e.Timestamp.Before(f.Range.To) {
		return false
	}
	if len(f.Harnesses) > 0 && !containsH(f.Harnesses, e.Harness) {
		return false
	}
	if len(f.Models) > 0 && !containsS(f.Models, e.Model) {
		return false
	}
	if len(f.Providers) > 0 && !containsS(f.Providers, e.ResolvedProvider) {
		return false
	}
	return f.Project == "" || e.ProjectPath == f.Project
}

func containsH(l []model.Harness, h model.Harness) bool {
	for _, x := range l {
		if x == h {
			return true
		}
	}
	return false
}

func containsS(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func (d *demo) each(f query.Filter, fn func(s *demoSession, e query.AttributedEvent)) {
	for i := range d.sessions {
		s := &d.sessions[i]
		for _, e := range s.events {
			if keep(f, e) {
				fn(s, e)
			}
		}
	}
}

func (d *demo) Totals(_ context.Context, f query.Filter) (query.Totals, error) {
	var t query.Totals
	seen := map[string]bool{}
	d.each(f, func(s *demoSession, e query.AttributedEvent) {
		t.Tokens = t.Tokens.Add(e.Tokens)
		t.CostUSD += e.Cost
		t.Requests++
		seen[s.meta.SessionID] = true
	})
	t.Sessions = int64(len(seen))
	return t, nil
}

func (d *demo) Daily(_ context.Context, f query.Filter) ([]query.Point, error) {
	m := map[time.Time]*query.Point{}
	d.each(f, func(_ *demoSession, e query.AttributedEvent) {
		ts := e.Timestamp.In(time.Local)
		day := time.Date(ts.Year(), ts.Month(), ts.Day(), 0, 0, 0, 0, time.Local)
		p := m[day]
		if p == nil {
			p = &query.Point{Day: day}
			m[day] = p
		}
		p.Tokens = p.Tokens.Add(e.Tokens)
		p.CostUSD += e.Cost
	})
	out := make([]query.Point, 0, len(m))
	for _, p := range m {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day.Before(out[j].Day) })
	return out, nil
}

func (d *demo) group(f query.Filter, key func(s *demoSession, e query.AttributedEvent) (string, string)) []query.Bucket {
	m := map[string]*query.Bucket{}
	ss := map[string]map[string]bool{}
	d.each(f, func(s *demoSession, e query.AttributedEvent) {
		k, label := key(s, e)
		b := m[k]
		if b == nil {
			b = &query.Bucket{Key: k, Label: label}
			m[k] = b
			ss[k] = map[string]bool{}
		}
		b.Tokens = b.Tokens.Add(e.Tokens)
		b.CostUSD += e.Cost
		b.Requests++
		ss[k][s.meta.SessionID] = true
	})
	out := make([]query.Bucket, 0, len(m))
	for k, b := range m {
		b.Sessions = int64(len(ss[k]))
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CostUSD > out[j].CostUSD })
	return out
}

func (d *demo) ByHarness(_ context.Context, f query.Filter) ([]query.Bucket, error) {
	return d.group(f, func(_ *demoSession, e query.AttributedEvent) (string, string) {
		return string(e.Harness), e.Harness.DisplayName()
	}), nil
}

func (d *demo) ByModel(_ context.Context, f query.Filter) ([]query.Bucket, error) {
	return d.group(f, func(_ *demoSession, e query.AttributedEvent) (string, string) { return e.Model, e.Model }), nil
}

func (d *demo) ByProvider(_ context.Context, f query.Filter) ([]query.Bucket, error) {
	return d.group(f, func(_ *demoSession, e query.AttributedEvent) (string, string) {
		return e.ResolvedProvider, e.ResolvedProvider
	}), nil
}

func (d *demo) ByProject(_ context.Context, f query.Filter) ([]query.Bucket, error) {
	return d.group(f, func(s *demoSession, _ query.AttributedEvent) (string, string) {
		return s.meta.Project, s.meta.Project
	}), nil
}

func (d *demo) row(s *demoSession, f query.Filter) (query.SessionRow, bool) {
	r := query.SessionRow{SessionMeta: s.meta}
	pm := map[string]*query.ProviderModel{}
	for _, e := range s.events {
		if !keep(f, e) {
			continue
		}
		r.Tokens = r.Tokens.Add(e.Tokens)
		r.CostUSD += e.Cost
		r.Requests++
		b := pm[e.Model]
		if b == nil {
			b = &query.ProviderModel{Provider: e.ResolvedProvider, Model: e.Model}
			pm[e.Model] = b
		}
		b.Tokens = b.Tokens.Add(e.Tokens)
		b.CostUSD += e.Cost
		b.Requests++
	}
	for _, b := range pm {
		r.Breakdown = append(r.Breakdown, *b)
	}
	sort.Slice(r.Breakdown, func(i, j int) bool { return r.Breakdown[i].CostUSD > r.Breakdown[j].CostUSD })
	return r, r.Requests > 0
}

func (d *demo) Sessions(_ context.Context, f query.Filter, order string, limit, offset int) ([]query.SessionRow, int, error) {
	var rows []query.SessionRow
	for i := range d.sessions {
		if r, ok := d.row(&d.sessions[i], f); ok {
			rows = append(rows, r)
		}
	}
	switch order {
	case query.SortCost:
		sort.Slice(rows, func(i, j int) bool { return rows[i].CostUSD > rows[j].CostUSD })
	case query.SortTokens:
		sort.Slice(rows, func(i, j int) bool { return rows[i].Tokens.Total() > rows[j].Tokens.Total() })
	default:
		sort.Slice(rows, func(i, j int) bool { return rows[i].UpdatedAt.After(rows[j].UpdatedAt) })
	}
	total := len(rows)
	if offset > len(rows) {
		offset = len(rows)
	}
	rows = rows[offset:]
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, total, nil
}

func (d *demo) Session(_ context.Context, h model.Harness, id string) (query.SessionRow, []query.SessionRow, []query.AttributedEvent, error) {
	for i := range d.sessions {
		s := &d.sessions[i]
		if s.meta.Harness == h && s.meta.SessionID == id {
			r, _ := d.row(s, query.Filter{})
			return r, nil, s.events, nil
		}
	}
	return query.SessionRow{}, nil, nil, nil
}

func (d *demo) Subscribe() (<-chan struct{}, func()) { return make(chan struct{}), func() {} }

func demoQuotas(now time.Time) []Quota {
	return []Quota{
		{ID: "codex", Name: "Codex", Plan: "Pro", Source: "local-log", FetchedAt: now, Windows: []QuotaWindow{
			{Name: "5h", UsedPercent: 42, ResetsAt: now.Add(2*time.Hour + 14*time.Minute)},
			{Name: "week", UsedPercent: 63, ResetsAt: now.Add(3*24*time.Hour + 5*time.Hour)},
		}},
		{ID: "claude", Name: "Claude", Plan: "Max 5x", Source: "api", FetchedAt: now, Windows: []QuotaWindow{
			{Name: "5h", UsedPercent: 88, ResetsAt: now.Add(47 * time.Minute)},
			{Name: "week", UsedPercent: 31, ResetsAt: now.Add(5 * 24 * time.Hour)},
		}},
	}
}

type demoSources struct{}

func (demoSources) List(context.Context) ([]Source, error) {
	now := time.Now()
	return []Source{
		{Harness: model.Codex, Detected: true, Enabled: true, Roots: []string{"~/.codex/sessions"}, Requests: 127034, LastSeen: now.Add(-time.Minute)},
		{Harness: model.ClaudeCode, Detected: true, Enabled: true, Roots: []string{"~/.claude/projects"}, Requests: 443, LastSeen: now.Add(-3 * time.Hour)},
		{Harness: model.DSH, Detected: true, Enabled: true, Roots: []string{"~/.dsh/sessions"}, Requests: 61210, LastSeen: now},
		{Harness: model.OpenCode, Detected: true, Enabled: false, Roots: []string{"~/.local/share/opencode"}, Requests: 210},
		{Harness: model.Pi, Override: "/Volumes/work/pi", Detected: true, Enabled: true, Roots: []string{"/Volumes/work/pi"}, Requests: 3120},
		{Harness: model.Gemini, Problem: "folder has no Gemini CLI logs", Enabled: true},
		{Harness: model.Kimi},
		{Harness: model.Crush},
	}, nil
}
func (demoSources) SetEnabled(context.Context, model.Harness, bool) error { return nil }
func (demoSources) SetRoot(context.Context, model.Harness, string) error  { return nil }
func (demoSources) Rescan(context.Context) error                          { return nil }

type demoPrices struct{}

func (demoPrices) Unpriced(context.Context) ([]query.UnpricedModel, error) {
	return []query.UnpricedModel{{Model: "my-gateway/opus", Provider: "custom", Requests: 312, Tokens: 4_200_000, Suggestions: []string{"claude-opus-4-6"}}}, nil
}
func (demoPrices) Aliases(context.Context) ([]query.ModelAlias, error) {
	return []query.ModelAlias{{From: "glm-4.6-air", To: "glm-4.6"}}, nil
}
func (demoPrices) SetAliases(context.Context, []query.ModelAlias) error { return nil }
func (demoPrices) CustomPrices(context.Context) ([]CustomPrice, error) {
	return []CustomPrice{{Model: "deepseek-v4", Input: 0.27, Output: 1.1, CacheRead: 0.07, CacheWrite: 0.27}}, nil
}
func (demoPrices) SetCustomPrices(context.Context, []CustomPrice) error { return nil }
func (demoPrices) SearchCatalog(_ context.Context, q string, _ int) ([]string, error) {
	return []string{q + "-latest"}, nil
}
func (demoPrices) Catalog() CatalogInfo { return CatalogInfo{Version: "2026-10-15", Models: 1873} }

type demoMaint struct{}

func (demoMaint) Info(context.Context) (DBInfo, error) {
	return DBInfo{Path: "~/Library/Application Support/MyToken/mytoken.db", Bytes: 61 << 20, Events: 255204, Sessions: 4180,
		Oldest: time.Date(2025, 9, 2, 0, 0, 0, 0, time.Local), Newest: time.Now()}, nil
}
func (demoMaint) Rebuild(context.Context) error { return nil }
func (demoMaint) Compact(context.Context) error { return nil }

type memPrefs map[string]string

func (p memPrefs) Pref(k string) string      { return p[k] }
func (p memPrefs) SetPref(k, v string) error { p[k] = v; return nil }
