package gui

import (
	"context"
	"sort"
	"strings"
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
		ov, err := computeOverview(ctx, q, now)
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

func computeOverview(ctx context.Context, q Query, now time.Time) (overview, error) {
	ov := overview{ready: true}
	today := dayStart(now)
	ov.day = today
	tomorrow := today.AddDate(0, 0, 1)
	mon := monthStart(now)
	var err error
	tot := func(from, to time.Time) query.Totals {
		if err != nil {
			return query.Totals{}
		}
		var t query.Totals
		t, err = q.Totals(ctx, query.Filter{Range: query.Range{From: from, To: to}})
		return t
	}
	ov.today = tot(today, tomorrow)
	ov.yesterday = tot(today.AddDate(0, 0, -1), today)
	ov.month = tot(mon, mon.AddDate(0, 1, 0))
	ov.lastMonth = tot(mon.AddDate(0, -1, 0), mon)
	if err != nil {
		return ov, err
	}

	from := today.AddDate(0, 0, -89)
	r90 := query.Filter{Range: query.Range{From: from, To: tomorrow}}
	total, err := q.Daily(ctx, r90)
	if err != nil {
		return ov, err
	}
	tools, err := q.ByHarness(ctx, r90)
	if err != nil {
		return ov, err
	}
	sort.SliceStable(tools, func(i, j int) bool { return tools[i].CostUSD > tools[j].CostUSD })
	for _, b := range tools {
		if len(ov.tools) == topTools {
			break
		}
		if b.CostUSD > 0 {
			ov.tools = append(ov.tools, model.Harness(b.Key))
		}
	}
	index := map[time.Time]int{}
	for i, p := range total {
		ov.days = append(ov.days, dayCost{day: p.Day, cost: p.CostUSD, tools: make([]float64, len(ov.tools)), other: p.CostUSD})
		index[p.Day] = i
	}
	for k, h := range ov.tools {
		f := r90
		f.Harnesses = []model.Harness{h}
		pts, err := q.Daily(ctx, f)
		if err != nil {
			return ov, err
		}
		for _, p := range pts {
			if i, ok := index[p.Day]; ok {
				ov.days[i].tools[k] = p.CostUSD
				ov.days[i].other -= p.CostUSD
			}
		}
	}
	for i := range ov.days {
		ov.days[i].other = max(ov.days[i].other, 0)
	}

	// The forecast: a week's average for each day left after today.
	var week float64
	for _, d := range ov.days {
		if !d.day.Before(today.AddDate(0, 0, -7)) && d.day.Before(today) {
			week += d.cost
		}
	}
	left := mon.AddDate(0, 1, 0).Sub(tomorrow).Hours() / 24
	ov.forecast = ov.month.CostUSD + week/7*max(left, 0)

	for k, n := range chartDays {
		f := query.Filter{Range: query.Range{From: today.AddDate(0, 0, 1-n), To: tomorrow}}
		if ov.totals[k], err = q.Totals(ctx, f); err != nil {
			return ov, err
		}
		if ov.byTool[k], err = q.ByHarness(ctx, f); err != nil {
			return ov, err
		}
		if ov.byModel[k], err = q.ByModel(ctx, f); err != nil {
			return ov, err
		}
		byCost(ov.byTool[k])
		byCost(ov.byModel[k])
	}

	ov.active, err = activeSessions(ctx, q, now)
	return ov, err
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

func activeSessions(ctx context.Context, q Query, now time.Time) ([]activeSession, error) {
	rows, _, err := q.Sessions(ctx, query.Filter{Range: query.Range{From: now.Add(-activeWindow)}}, query.SortRecent, 4, 0)
	if err != nil {
		return nil, err
	}
	var out []activeSession
	for _, r := range rows {
		if now.Sub(r.UpdatedAt) > activeWindow {
			continue
		}
		_, _, events, err := q.Session(ctx, r.Harness, r.SessionID)
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
		rk := ranking{ready: true, forRange: r}
		var err error
		for k, by := range []func(context.Context, query.Filter) ([]query.Bucket, error){q.ByHarness, q.ByModel, q.ByProvider} {
			if rk.dims[k], err = by(ctx, f); err != nil {
				break
			}
			byCost(rk.dims[k])
		}
		for _, b := range rk.dims[0] {
			rk.totalCost += b.CostUSD
		}
		if err == nil {
			today := dayStart(now)
			// A year of whole weeks, so that the heat map starts on a Monday.
			from := today.AddDate(0, 0, -364)
			for from.Weekday() != time.Monday {
				from = from.AddDate(0, 0, -1)
			}
			var pts []query.Point
			pts, err = q.Daily(ctx, query.Filter{Range: query.Range{From: from, To: today.AddDate(0, 0, 1)}})
			rk.year = dense(from, today, pts)
			rk.streak, rk.best, rk.activeDays, rk.topDay = streaks(rk.year)
		}
		return func() {
			if err != nil {
				a.rk.err = err.Error()
				return
			}
			a.rk = rk
		}
	})
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
		var err error
		d.days, err = q.Daily(ctx, f)
		if err == nil {
			d.total, err = q.Totals(ctx, all)
		}
		if err == nil {
			d.byModel, err = q.ByModel(ctx, all)
			byCost(d.byModel)
		}
		if err == nil {
			d.byTool, err = q.ByHarness(ctx, all)
			byCost(d.byTool)
		}
		if err == nil {
			d.sessions, _, err = q.Sessions(ctx, all, query.SortCost, 50, 0)
		}
		if err != nil {
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
