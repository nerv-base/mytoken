package gui

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

// overview is what the Overview page and the tray show.
type overview struct {
	ready bool
	err   string
	day   time.Time // the local day "today" was computed for

	today, yesterday, month, lastMonth query.Totals
	// forecast is this month's cost so far plus the last seven days'
	// daily average for each day left.
	forecast float64

	// days holds the last 90 days, oldest first; tools the top tools of
	// those days, whose costs days[i].tools follows, the rest in other.
	days  []dayCost
	tools []model.Harness

	// per chart range (7, 30, 90 days)
	totals  [3]query.Totals
	byTool  [3][]query.Bucket
	byModel [3][]query.Bucket

	quotas []Quota
	active []activeSession
}

type dayCost struct {
	day   time.Time
	cost  float64
	tools []float64 // aligned with overview.tools
	other float64
}

// activeSession is a session that made a request in the last minutes.
type activeSession struct {
	row query.SessionRow
	// rate is the output speed in tokens per second, estimated from the
	// gaps between requests (logs carry no durations); 0 when unknown.
	rate     float64
	perHour  float64 // cost of the last hour
	hit      float64
	last     time.Time
	model    string
	requests int // in the last ten minutes
}

var chartDays = [3]int{7, 30, 90}

const topTools = 5

func (a *App) loadOverview() {
	q := a.d.Query
	if q == nil {
		return
	}
	now := a.now()
	quotas := a.d.Quotas
	a.run(&a.ovJob, func(ctx context.Context) func() {
		ov, err := computeOverview(ctx, q, now, &a.recent)
		if quotas != nil {
			ov.quotas = quotas()
		}
		return func() {
			if err != nil {
				a.ov.err = err.Error()
				return
			}
			a.ov = ov
			a.updateTray()
		}
	})
}

func computeOverview(ctx context.Context, q Query, now time.Time, recent *recentEvents) (overview, error) {
	ov := overview{ready: true}
	today := dayStart(now)
	ov.day = today
	tomorrow := today.AddDate(0, 0, 1)
	mon := monthStart(now)
	lastMon := mon.AddDate(0, -1, 0)
	from90 := today.AddDate(0, 0, -89)
	from := from90
	if lastMon.Before(from) {
		from = lastMon
	}

	// One grouped read feeds every total, series and ranking on the page;
	// today's session count (which needs session identity) and the running
	// sessions are read alongside it.
	var (
		wg               sync.WaitGroup
		rows             []query.UsageRow
		errU, errT, errA error
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		rows, errU = q.Usage(ctx, query.Filter{Range: query.Range{From: from, To: tomorrow}})
	}()
	go func() {
		defer wg.Done()
		ov.today, errT = q.Totals(ctx, query.Filter{Range: query.Range{From: today, To: tomorrow}})
	}()
	go func() {
		defer wg.Done()
		ov.active, errA = activeSessions(ctx, q, now, recent)
	}()
	wg.Wait()
	if err := errors.Join(errU, errT, errA); err != nil {
		return ov, err
	}

	sum := func(from, to time.Time) query.Totals {
		var t query.Totals
		for _, r := range rows {
			if !r.Day.Before(from) && r.Day.Before(to) {
				t.Tokens = t.Tokens.Add(r.Tokens)
				t.CostUSD += r.CostUSD
				t.Requests += r.Requests
				t.Unpriced += r.Unpriced
			}
		}
		t.CacheHit = t.Tokens.HitRate()
		return t
	}
	ov.yesterday = sum(today.AddDate(0, 0, -1), today)
	ov.month = sum(mon, mon.AddDate(0, 1, 0))
	ov.lastMonth = sum(lastMon, mon)

	// The top tools of the last 90 days by cost.
	toolCost := map[model.Harness]float64{}
	for _, r := range rows {
		if !r.Day.Before(from90) {
			toolCost[r.Harness] += r.CostUSD
		}
	}
	for h, c := range toolCost {
		if c > 0 {
			ov.tools = append(ov.tools, h)
		}
	}
	sort.Slice(ov.tools, func(i, j int) bool {
		a, b := toolCost[ov.tools[i]], toolCost[ov.tools[j]]
		if a != b {
			return a > b
		}
		return ov.tools[i] < ov.tools[j]
	})
	if len(ov.tools) > topTools {
		ov.tools = ov.tools[:topTools]
	}
	slot := map[model.Harness]int{}
	for i, h := range ov.tools {
		slot[h] = i
	}

	// 90 dense days, oldest first.
	index := map[int64]int{}
	for d := from90; d.Before(tomorrow); d = d.AddDate(0, 0, 1) {
		index[d.UnixNano()] = len(ov.days)
		ov.days = append(ov.days, dayCost{day: d, tools: make([]float64, len(ov.tools))})
	}
	for _, r := range rows {
		i, ok := index[r.Day.UnixNano()]
		if !ok {
			continue
		}
		d := &ov.days[i]
		d.cost += r.CostUSD
		if k, ok := slot[r.Harness]; ok {
			d.tools[k] += r.CostUSD
		} else {
			d.other += r.CostUSD
		}
	}

	// The forecast: a week's average for each day left after today.
	week := sum(today.AddDate(0, 0, -7), today).CostUSD
	left := mon.AddDate(0, 1, 0).Sub(tomorrow).Hours() / 24
	ov.forecast = ov.month.CostUSD + week/7*max(left, 0)

	for k, n := range chartDays {
		start := today.AddDate(0, 0, 1-n)
		ov.totals[k] = sum(start, tomorrow)
		tools := map[string]*query.Bucket{}
		models := map[string]*query.Bucket{}
		add := func(m map[string]*query.Bucket, key, label string, r query.UsageRow) {
			b := m[key]
			if b == nil {
				b = &query.Bucket{Key: key, Label: label}
				m[key] = b
			}
			b.Tokens = b.Tokens.Add(r.Tokens)
			b.CostUSD += r.CostUSD
			b.Requests += r.Requests
			b.Unpriced += r.Unpriced
		}
		for _, r := range rows {
			if r.Day.Before(start) || !r.Day.Before(tomorrow) {
				continue
			}
			add(tools, string(r.Harness), r.Harness.DisplayName(), r)
			add(models, r.Model, r.Model, r)
		}
		ov.byTool[k] = flatten(tools)
		ov.byModel[k] = flatten(models)
		byCost(ov.byTool[k])
		byCost(ov.byModel[k])
	}
	return ov, nil
}

func flatten(m map[string]*query.Bucket) []query.Bucket {
	out := make([]query.Bucket, 0, len(m))
	for _, b := range m {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func byCost(b []query.Bucket) {
	sort.SliceStable(b, func(i, j int) bool {
		if b[i].CostUSD != b[j].CostUSD {
			return b[i].CostUSD > b[j].CostUSD
		}
		return b[i].Tokens.Total() > b[j].Tokens.Total()
	})
}

// activeWindow is how recent a session's last request must be for the
// tray to count it as running.
const activeWindow = 15 * time.Minute

// recentEvents keeps the last hour of each running session's requests, so
// the refresh that runs every minute (and after every commit) reads a
// session's whole history again only when it actually got a new request.
type recentEvents struct {
	mu sync.Mutex
	m  map[string]recentEntry
}

type recentEntry struct {
	updated time.Time
	events  []query.AttributedEvent
}

func (c *recentEvents) get(ctx context.Context, q Query, r query.SessionRow, now time.Time) ([]query.AttributedEvent, error) {
	key := string(r.Harness) + "\x00" + r.SessionID
	if c != nil {
		c.mu.Lock()
		e, ok := c.m[key]
		c.mu.Unlock()
		if ok && e.updated.Equal(r.UpdatedAt) {
			return e.events, nil
		}
	}
	_, _, events, err := q.Session(ctx, r.Harness, r.SessionID)
	if err != nil {
		return nil, err
	}
	// Only the last hour is ever looked at (rates and $/h).
	cut := len(events)
	for cut > 0 && now.Sub(events[cut-1].Timestamp) <= time.Hour+activeWindow {
		cut--
	}
	if cut > 0 {
		cut-- // keep one earlier request: the first gap needs it
	}
	events = append([]query.AttributedEvent(nil), events[cut:]...)
	if c != nil {
		c.mu.Lock()
		if c.m == nil {
			c.m = map[string]recentEntry{}
		}
		c.m[key] = recentEntry{updated: r.UpdatedAt, events: events}
		c.mu.Unlock()
	}
	return events, nil
}

// keep drops every cached session not in live.
func (c *recentEvents) keep(live map[string]bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	for k := range c.m {
		if !live[k] {
			delete(c.m, k)
		}
	}
	c.mu.Unlock()
}

func activeSessions(ctx context.Context, q Query, now time.Time, recent *recentEvents) ([]activeSession, error) {
	rows, _, err := q.Sessions(ctx, query.Filter{Range: query.Range{From: now.Add(-activeWindow)}}, query.SortRecent, 4, 0)
	if err != nil {
		return nil, err
	}
	var out []activeSession
	live := map[string]bool{}
	defer func() { recent.keep(live) }()
	for _, r := range rows {
		if now.Sub(r.UpdatedAt) > activeWindow {
			continue
		}
		live[string(r.Harness)+"\x00"+r.SessionID] = true
		events, err := recent.get(ctx, q, r, now)
		if err != nil {
			return nil, err
		}
		s := activeSession{row: r, hit: r.Tokens.HitRate(), last: r.UpdatedAt}
		s.rate, s.requests = outputRate(events, now)
		for i := len(events) - 1; i >= 0; i-- {
			e := events[i]
			if now.Sub(e.Timestamp) > time.Hour {
				break
			}
			s.perHour += e.Cost
			if s.model == "" {
				s.model = e.Model
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// outputRate estimates output tokens per second over the last ten minutes:
// each request's output over the time since the request before it, capped
// so that a pause for the user or a tool does not count as generation.
func outputRate(events []query.AttributedEvent, now time.Time) (float64, int) {
	const window, gapCap = 10 * time.Minute, 90 * time.Second
	var out int64
	var secs float64
	n := 0
	for i := len(events) - 1; i > 0; i-- {
		e, prev := events[i], events[i-1]
		if now.Sub(e.Timestamp) > window {
			break
		}
		gap := e.Timestamp.Sub(prev.Timestamp)
		if gap <= 0 {
			continue
		}
		gap = min(gap, gapCap)
		out += e.Tokens.Output + e.Tokens.Reasoning
		secs += gap.Seconds()
		n++
	}
	if secs < 1 || out == 0 {
		return 0, n
	}
	return float64(out) / secs, n
}

// rangeFilter is the filter of the shared range choice i, ending today.
func (a *App) rangeFilter(i int) query.Filter {
	n := rangeDays[i]
	if n == 0 {
		return query.Filter{}
	}
	today := dayStart(a.now())
	return query.Filter{Range: query.Range{From: today.AddDate(0, 0, 1-n), To: today.AddDate(0, 0, 1)}}
}

// ranking is the Ranking page's data.
type ranking struct {
	ready     bool
	err       string
	forRange  int
	dims      [3][]query.Bucket // tool, model, provider
	totalCost float64
	// year holds a year of days, oldest first, for the heat map.
	year         []query.Point
	streak, best int
	topDay       query.Point
	activeDays   int
}

func (a *App) loadRanking() {
	q := a.d.Query
	if q == nil {
		return
	}
	r, f, now := a.rangeSel, a.rangeFilter(a.rangeSel), a.now()
	a.run(&a.rkJob, func(ctx context.Context) func() {
		rk, err := computeRanking(ctx, q, f, now)
		rk.forRange = r
		return func() {
			if err != nil {
				a.rk.err = err.Error()
				return
			}
			a.rk = rk
		}
	})
}

// computeRanking reads the chosen range and a year of whole weeks in one
// grouped read when the range fits inside that year (all ranges but "all").
func computeRanking(ctx context.Context, q Query, f query.Filter, now time.Time) (ranking, error) {
	rk := ranking{ready: true}
	today := dayStart(now)
	tomorrow := today.AddDate(0, 0, 1)
	// A year of whole weeks, so that the heat map starts on a Monday.
	from := today.AddDate(0, 0, -364)
	for from.Weekday() != time.Monday {
		from = from.AddDate(0, 0, -1)
	}
	read := query.Filter{Range: query.Range{From: from, To: tomorrow}}
	if f.Range.From.IsZero() || f.Range.From.Before(from) {
		read.Range.From = f.Range.From
	}
	rows, err := q.Usage(ctx, read)
	if err != nil {
		return rk, err
	}
	var dims [3]map[string]*query.Bucket
	for k := range dims {
		dims[k] = map[string]*query.Bucket{}
	}
	add := func(m map[string]*query.Bucket, key, label string, r query.UsageRow) {
		b := m[key]
		if b == nil {
			b = &query.Bucket{Key: key, Label: label}
			m[key] = b
		}
		b.Tokens = b.Tokens.Add(r.Tokens)
		b.CostUSD += r.CostUSD
		b.Requests += r.Requests
		b.Unpriced += r.Unpriced
	}
	var pts []query.Point
	byDay := map[int64]int{}
	for _, r := range rows {
		inRange := (f.Range.From.IsZero() || !r.Day.Before(f.Range.From)) && (f.Range.To.IsZero() || r.Day.Before(f.Range.To))
		if inRange {
			add(dims[0], string(r.Harness), r.Harness.DisplayName(), r)
			add(dims[1], r.Model, r.Model, r)
			add(dims[2], r.Provider, r.Provider, r)
			rk.totalCost += r.CostUSD
		}
		if r.Day.Before(from) {
			continue
		}
		i, ok := byDay[r.Day.UnixNano()]
		if !ok {
			i = len(pts)
			byDay[r.Day.UnixNano()] = i
			pts = append(pts, query.Point{Day: r.Day})
		}
		pts[i].Tokens = pts[i].Tokens.Add(r.Tokens)
		pts[i].CostUSD += r.CostUSD
	}
	for k := range dims {
		rk.dims[k] = flatten(dims[k])
		byCost(rk.dims[k])
	}
	rk.year = dense(from, today, pts)
	rk.streak, rk.best, rk.activeDays, rk.topDay = streaks(rk.year)
	return rk, nil
}

// streaks returns the current run of days with usage (today may still be
// empty without breaking it), the longest run, the days with usage and the
// most expensive day.
// dense returns one point per day from first to last inclusive, taking
// each day's figures from pts, which may skip idle days.
func dense(first, last time.Time, pts []query.Point) []query.Point {
	type ymd struct {
		y int
		m time.Month
		d int
	}
	by := make(map[ymd]query.Point, len(pts))
	for _, p := range pts {
		y, m, d := p.Day.Date()
		by[ymd{y, m, d}] = p
	}
	var out []query.Point
	for day := first; !day.After(last); day = day.AddDate(0, 0, 1) {
		y, m, d := day.Date()
		p := by[ymd{y, m, d}]
		p.Day = day
		out = append(out, p)
	}
	return out
}

func streaks(days []query.Point) (current, best, active int, top query.Point) {
	run := 0
	for _, p := range days {
		if p.Tokens.Total() > 0 {
			run++
			active++
			best = max(best, run)
		} else {
			run = 0
		}
		if p.CostUSD > top.CostUSD {
			top = p
		}
	}
	current = run
	if n := len(days); n > 1 && days[n-1].Tokens.Total() == 0 {
		run = 0
		for i := n - 2; i >= 0 && days[i].Tokens.Total() > 0; i-- {
			run++
		}
		current = run
	}
	return
}

// projects is the Projects page's list.
type projects struct {
	ready    bool
	err      string
	forRange int
	rows     []query.Bucket
	maxCost  float64
}

type projectDetail struct {
	key      string
	ready    bool
	err      string
	days     []query.Point
	byModel  []query.Bucket
	byTool   []query.Bucket
	sessions []query.SessionRow
	total    query.Totals
}

func (a *App) loadProjects() {
	q := a.d.Query
	if q == nil {
		return
	}
	r, f := a.rangeSel, a.rangeFilter(a.rangeSel)
	a.run(&a.pjJob, func(ctx context.Context) func() {
		rows, err := q.ByProject(ctx, f)
		byCost(rows)
		pj := projects{ready: true, forRange: r, rows: rows}
		for _, b := range rows {
			pj.maxCost = max(pj.maxCost, b.CostUSD)
		}
		return func() {
			if err != nil {
				a.pj.err = err.Error()
				return
			}
			a.pj = pj
		}
	})
}

func (a *App) loadProjectDetail() {
	q := a.d.Query
	key := a.pjSel
	if q == nil || key == "" {
		return
	}
	f := a.rangeFilter(a.rangeSel)
	if f.Range.From.IsZero() {
		// All time: the chart still needs bounds, a year back.
		today := dayStart(a.now())
		f.Range = query.Range{From: today.AddDate(0, 0, -364), To: today.AddDate(0, 0, 1)}
	}
	f.Project = key
	all := a.rangeFilter(a.rangeSel)
	all.Project = key
	a.run(&a.pjdJob, func(ctx context.Context) func() {
		d := projectDetail{key: key, ready: true}
		// Independent reads; the read pool runs them side by side.
		errs := make([]error, 5)
		var wg sync.WaitGroup
		wg.Add(5)
		go func() { defer wg.Done(); d.days, errs[0] = q.Daily(ctx, f) }()
		go func() { defer wg.Done(); d.total, errs[1] = q.Totals(ctx, all) }()
		go func() { defer wg.Done(); d.byModel, errs[2] = q.ByModel(ctx, all) }()
		go func() { defer wg.Done(); d.byTool, errs[3] = q.ByHarness(ctx, all) }()
		go func() { defer wg.Done(); d.sessions, _, errs[4] = q.Sessions(ctx, all, query.SortCost, 50, 0) }()
		wg.Wait()
		byCost(d.byModel)
		byCost(d.byTool)
		if err := errors.Join(errs...); err != nil {
			d.err = err.Error()
		}
		return func() {
			if a.pjSel == key {
				a.pjd = d
			}
		}
	})
}

// sessions is the Sessions page's list.
type sessions struct {
	ready    bool
	err      string
	forRange int
	rows     []query.SessionRow
	total    int
	// shown indexes rows by the search, as typed when it was computed.
	shown []int
	query string
	// tools lists the tools present in rows, first seen first.
	tools []model.Harness
}

// sessionLimit bounds the rows the list holds; the newest come first.
const sessionLimit = 5000

func (a *App) loadSessions() {
	q := a.d.Query
	if q == nil {
		return
	}
	r, f := a.rangeSel, a.rangeFilter(a.rangeSel)
	order := query.SortRecent
	if a.ssSort == 1 {
		order = query.SortCost
	}
	a.run(&a.ssJob, func(ctx context.Context) func() {
		rows, total, err := q.Sessions(ctx, f, order, sessionLimit, 0)
		return func() {
			if err != nil {
				a.ss.err = err.Error()
				return
			}
			a.ss = sessions{ready: true, forRange: r, rows: rows, total: total}
			a.filterSessions()
		}
	})
}

// filterSessions recomputes the rows that match the search box. It runs
// when the search or the rows change, never per frame.
func (a *App) filterSessions() {
	s := &a.ss
	s.query = a.ssQuery
	s.shown = s.shown[:0]
	terms := strings.Fields(strings.ToLower(a.ssQuery))
	s.tools = s.tools[:0]
	seen := map[model.Harness]bool{}
	for i, r := range s.rows {
		if !seen[r.Harness] {
			seen[r.Harness] = true
			s.tools = append(s.tools, r.Harness)
		}
		if a.ssTool != "" && string(r.Harness) != a.ssTool {
			continue
		}
		if len(terms) == 0 || matches(r, terms) {
			s.shown = append(s.shown, i)
		}
	}
}

func matches(r query.SessionRow, terms []string) bool {
	hay := strings.ToLower(r.Title + " " + r.Project + " " + r.Harness.DisplayName() + " " + r.SessionID)
	for _, b := range r.Breakdown {
		hay += " " + strings.ToLower(b.Model)
	}
	for _, t := range terms {
		if !strings.Contains(hay, t) {
			return false
		}
	}
	return true
}

// sessionDetail is one session's requests.
type sessionDetail struct {
	key      string
	ready    bool
	err      string
	row      query.SessionRow
	children []query.SessionRow
	events   []query.AttributedEvent
	// cum is the running cost after each event; span the time from the
	// first request to the last.
	cum  []float64
	span time.Duration
}

func sessionKey(h model.Harness, id string) string { return string(h) + "\x00" + id }

func (a *App) openSession(r query.SessionRow) {
	key := sessionKey(r.Harness, r.SessionID)
	if a.sd.key == key {
		return
	}
	a.sd = sessionDetail{key: key, row: r}
	a.sdEvents = ui.ListState{}
	a.loadSessionDetail()
}

func (a *App) loadSessionDetail() {
	q := a.d.Query
	if q == nil || a.sd.key == "" {
		return
	}
	key, h, id := a.sd.key, a.sd.row.Harness, a.sd.row.SessionID
	a.run(&a.sdJob, func(ctx context.Context) func() {
		row, kids, events, err := q.Session(ctx, h, id)
		d := sessionDetail{key: key, ready: true, row: row, children: kids, events: events}
		if err != nil {
			d.err = err.Error()
		}
		var c float64
		d.cum = make([]float64, len(events))
		for i, e := range events {
			c += e.Cost
			d.cum[i] = c
		}
		if n := len(events); n > 1 {
			d.span = events[n-1].Timestamp.Sub(events[0].Timestamp)
		}
		return func() {
			if a.sd.key == key {
				a.sd = d
			}
		}
	})
}
