package gui

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/reporttime"
	"github.com/zzstar101/mytoken/internal/store"
)

// Span is a range choice of the overview.
type Span int

const (
	SpanToday Span = iota
	Span7
	Span30
	Span90
	SpanAll
)

var spanKeys = []string{"today", "7d", "30d", "90d", "all"}

// statsLoc is the zone calendar boundaries are resolved in. The app passes the
// persisted stats_timezone; a demo or a caller with no settings gets the
// machine's zone, which is what the overview showed before the setting existed.
func statsLoc(loc *time.Location) *time.Location {
	if loc == nil {
		return time.Local
	}
	return loc
}

// dayBoundary is the start (or end) of the calendar day containing t in loc,
// resolved by reporttime so a DST day is 23 or 25 hours rather than a fixed 24.
func dayBoundary(t time.Time, end bool, loc *time.Location) time.Time {
	loc = statsLoc(loc)
	st, err := reporttime.Resolve(t, reporttime.Day, loc)
	if err == nil {
		if end {
			return st.Current.End
		}
		return st.Current.Start
	}
	// reporttime refuses a nil location and nothing else; keep a boundary that
	// still names a real instant if its data ever cannot resolve one.
	l := t.In(loc)
	day := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
	if end {
		return day.AddDate(0, 0, 1)
	}
	return day
}

// weekStart is the Monday 00:00 of the calendar week containing t in loc.
func weekStart(t time.Time, loc *time.Location) time.Time {
	loc = statsLoc(loc)
	if st, err := reporttime.Resolve(t, reporttime.Week, loc); err == nil {
		return st.Current.Start
	}
	l := t.In(loc)
	day := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
	wd := (int(day.Weekday()) + 6) % 7 // Monday = 0
	return day.AddDate(0, 0, -wd)
}

// rangeOf returns the range of a span ending today, with both boundaries on
// calendar days of the stats timezone.
func rangeOf(s Span, now time.Time, loc *time.Location) query.Range {
	loc = statsLoc(loc)
	start := dayBoundary(now, false, loc)
	end := dayBoundary(now, true, loc)
	switch s {
	case SpanToday:
		return query.Range{From: start, To: end}
	case Span7:
		return query.Range{From: dayBoundary(now.AddDate(0, 0, -6), false, loc), To: end}
	case Span30:
		return query.Range{From: dayBoundary(now.AddDate(0, 0, -29), false, loc), To: end}
	case Span90:
		return query.Range{From: dayBoundary(now.AddDate(0, 0, -89), false, loc), To: end}
	}
	return query.Range{}
}

// prevRange is the range of equal length just before r.
func prevRange(r query.Range) query.Range {
	if r.From.IsZero() {
		return query.Range{}
	}
	d := r.To.Sub(r.From)
	return query.Range{From: r.From.Add(-d), To: r.From}
}

// Overview is everything the overview page and the tray panel draw.
type Overview struct {
	Span   Span
	Totals query.Totals
	Prev   query.Totals
	Daily  []query.Point // the span's days (for Today: the last 14 days)
	Hourly []query.Point // last 24h
	Heat   []query.Point // last 26 weeks, daily
	Models []query.Bucket
	// Providers are endpoint groups: usage merged by stable endpoint
	// identity, each keeping the harness/provider members behind it. They
	// are not attribution-name buckets, and their Keys are not billing
	// accounts.
	Providers []query.ProviderGroup
	Harnesses []query.Bucket
	Projects  []query.Bucket
	Active    []query.SessionRow // most recently updated sessions
	Loaded    time.Time
	// ProviderErr is why the endpoint groups are missing. The provider views
	// show it, so a failed query never reads as zero usage.
	ProviderErr error
}

// loadOverview queries everything for span s.
func loadOverview(ctx context.Context, q query.Service, s Span, now time.Time, loc *time.Location) (Overview, error) {
	o, err := loadSpan(ctx, q, s, now, loc)
	if err != nil {
		return o, err
	}
	return o.with(loadShared(ctx, q, now, loc)), nil
}

// loadSpan queries the part of the overview that depends on the span: the
// totals, the trend and the breakdowns. It stops early once ctx is done.
func loadSpan(ctx context.Context, q query.Service, s Span, now time.Time, loc *time.Location) (Overview, error) {
	r := rangeOf(s, now, loc)
	f := query.Filter{Range: r}
	o := Overview{Span: s, Loaded: now}
	var err error
	if o.Totals, err = q.Totals(ctx, f); err != nil {
		return o, err
	}
	if pr := prevRange(r); !pr.From.IsZero() {
		o.Prev, _ = q.Totals(ctx, query.Filter{Range: pr})
	}
	dr := r
	day := rangeOf(SpanToday, now, loc)
	switch s {
	case SpanToday:
		dr = query.Range{From: day.From.AddDate(0, 0, -13), To: day.To}
	case SpanAll:
		dr = query.Range{From: day.From.AddDate(0, 0, -179), To: day.To}
	}
	if o.Daily, err = q.Daily(ctx, query.Filter{Range: dr}); err != nil {
		return o, err
	}
	for _, by := range []struct {
		get func(context.Context, query.Filter) ([]query.Bucket, error)
		out *[]query.Bucket
	}{{q.ByModel, &o.Models}, {q.ByHarness, &o.Harnesses}, {q.ByProject, &o.Projects}} {
		if err = ctx.Err(); err != nil {
			return o, err
		}
		*by.out, _ = by.get(ctx, f)
	}
	// The provider breakdown is identity-shaped, and its error is kept: the
	// provider views show it instead of an empty list that would read as
	// zero usage.
	if o.Providers, o.ProviderErr = q.ProviderGroups(ctx, f); o.ProviderErr != nil {
		if err = ctx.Err(); err != nil {
			return o, err
		}
	}
	return o, ctx.Err()
}

// shared is the part of the overview that is the same for every span: the
// last 24 hours, the year's heatmap and the recent sessions.
type shared struct {
	Hourly []query.Point
	Heat   []query.Point
	Active []query.SessionRow
}

func loadShared(ctx context.Context, q query.Service, now time.Time, loc *time.Location) shared {
	var sh shared
	hr := now.Truncate(time.Hour).Add(time.Hour)
	sh.Hourly, _ = q.Hourly(ctx, query.Filter{Range: query.Range{From: hr.Add(-24 * time.Hour), To: hr}})
	sh.Heat, _ = q.Daily(ctx, query.Filter{Range: query.Range{From: heatStart(now, loc), To: rangeOf(SpanToday, now, loc).To}})
	sh.Active, _, _ = q.Sessions(ctx, query.Filter{}, query.SortRecent, 6, 0)
	return sh
}

// with returns o with the span-independent parts filled in from sh.
func (o Overview) with(sh shared) Overview {
	o.Hourly, o.Heat, o.Active = sh.Hourly, sh.Heat, sh.Active
	return o
}

// heatStart is the Monday 52 weeks before this week's Monday: up to 53 columns;
// the heatmap shows as many recent ones as fit.
func heatStart(now time.Time, loc *time.Location) time.Time {
	loc = statsLoc(loc)
	start := weekStart(now, loc)
	// The earlier Monday is derived from the same weekday, so a week whose
	// Monday was skipped by a zone change still steps back a whole week.
	if st, err := reporttime.Resolve(start.AddDate(0, 0, -7*52), reporttime.Week, loc); err == nil {
		return st.Current.Start
	}
	return start.AddDate(0, 0, -7*52)
}

// SessionPage is the sessions list.
type SessionPage struct {
	Rows  []query.SessionRow
	Total int
}

// SessionDetail is one session opened.
type SessionDetail struct {
	Row      query.SessionRow
	Children []query.SessionRow
	Events   []query.AttributedEvent
	Cache    []query.CacheStep // the session's cache ledger, in time order
}

// sessionRef identifies one session. A session id alone does not: two
// instances can each hold a session with the same id under the same harness,
// so every row key, selection and callback in the GUI carries the instance
// alongside the harness and the id.
type sessionRef struct {
	Harness    model.Harness
	InstanceID string
	SessionID  string
}

// refOf is the reference of a row as the store and query layers report it. An
// empty instance (demo data) is the reserved default instance.
func refOf(r query.SessionRow) sessionRef {
	return sessionRef{Harness: r.Harness, InstanceID: store.NormalizeInstanceID(r.InstanceID), SessionID: r.SessionID}
}

// key is the stable list key of the reference. It includes the instance, so
// two rows with the same harness and session id stay distinct.
func (r sessionRef) key() string {
	return string(r.Harness) + "/" + store.NormalizeInstanceID(r.InstanceID) + "/" + r.SessionID
}

// instanceLabel is the instance to show next to a session, or "" for the
// reserved default instance, which is never ambiguous.
func (r sessionRef) instanceLabel() string {
	inst := store.NormalizeInstanceID(r.InstanceID)
	if inst == store.DefaultInstanceID {
		return ""
	}
	return inst
}

// ambiguousSessions reports the harness/id pairs that more than one instance
// holds. Only those rows need to name their instance: a session id that one
// instance owns is not ambiguous, and inventing a mark for it would be noise.
func ambiguousSessions(rows []query.SessionRow) map[string]bool {
	seen := map[string]map[string]bool{}
	for _, r := range rows {
		k := string(r.Harness) + "/" + r.SessionID
		if seen[k] == nil {
			seen[k] = map[string]bool{}
		}
		seen[k][store.NormalizeInstanceID(r.InstanceID)] = true
	}
	dup := map[string]bool{}
	for k, insts := range seen {
		if len(insts) > 1 {
			dup[k] = true
		}
	}
	return dup
}

// showInstance reports whether a row's session id is ambiguous enough that the
// row has to name the instance it belongs to.
func showInstance(dups map[string]bool, r query.SessionRow) bool {
	return dups[string(r.Harness)+"/"+r.SessionID]
}

// loadSessions lists the sessions matching f.
func loadSessions(ctx context.Context, q query.Service, f query.Filter, sortKey string) (SessionPage, error) {
	rows, total, err := q.Sessions(ctx, f, sortKey, 2000, 0)
	return SessionPage{Rows: rows, Total: total}, err
}

// loadSession opens one session. The instance-aware API is preferred whenever
// the service offers it; the legacy instance-less Session/SessionCache can only
// be used by a service that has no instance-aware one at all, because for a
// known instance they may pick a twin in another instance.
func loadSession(ctx context.Context, q query.Service, ref sessionRef) (SessionDetail, error) {
	row, kids, evs, err := sessionIn(ctx, q, ref)
	if err != nil {
		return SessionDetail{}, err
	}
	steps, err := sessionCacheIn(ctx, q, ref)
	return SessionDetail{Row: row, Children: kids, Events: evs, Cache: steps}, err
}

// sessionRowIn reads one session's row, instance included.
func sessionRowIn(ctx context.Context, q query.Service, ref sessionRef) (query.SessionRow, error) {
	row, _, _, err := sessionIn(ctx, q, ref)
	return row, err
}

// sessionIn reads one session, instance included, through the instance-aware
// API when the service has it. ref.InstanceID is normalized by the callers.
func sessionIn(ctx context.Context, q query.Service, ref sessionRef) (query.SessionRow, []query.SessionRow, []query.AttributedEvent, error) {
	if is, ok := q.(query.InstanceService); ok {
		return is.SessionIn(ctx, ref.Harness, ref.InstanceID, ref.SessionID)
	}
	return q.Session(ctx, ref.Harness, ref.SessionID)
}

// sessionCacheIn reads one session's cache steps, instance included.
func sessionCacheIn(ctx context.Context, q query.Service, ref sessionRef) ([]query.CacheStep, error) {
	if is, ok := q.(query.InstanceService); ok {
		return is.SessionCacheIn(ctx, ref.Harness, ref.InstanceID, ref.SessionID)
	}
	return q.SessionCache(ctx, ref.Harness, ref.SessionID)
}

// filterRows keeps the rows matching every word of needle.
func filterRows(rows []query.SessionRow, needle string) []query.SessionRow {
	needle = strings.TrimSpace(strings.ToLower(needle))
	if needle == "" {
		return rows
	}
	words := strings.Fields(needle)
	out := rows[:0:0]
	for _, r := range rows {
		hay := strings.ToLower(r.Title + " " + r.Project + " " + string(r.Harness) + " " + r.Harness.DisplayName())
		for _, b := range r.Breakdown {
			hay += " " + strings.ToLower(b.Provider+" "+b.Model)
		}
		ok := true
		for _, w := range words {
			if !strings.Contains(hay, w) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, r)
		}
	}
	return out
}

// streak is the number of consecutive active days ending today (or yesterday).
func streak(days []query.Point) int {
	n := 0
	for i := len(days) - 1; i >= 0; i-- {
		if days[i].Tokens.Total() == 0 {
			if i == len(days)-1 {
				continue
			}
			break
		}
		n++
	}
	return n
}

// busiest returns the day with most tokens.
func busiest(days []query.Point) (query.Point, bool) {
	var best query.Point
	ok := false
	for _, d := range days {
		if d.Tokens.Total() > best.Tokens.Total() {
			best, ok = d, true
		}
	}
	return best, ok
}

// topN sorts buckets by tokens and keeps n, folding the rest into "other".
func topN(bs []query.Bucket, n int, other string) []query.Bucket {
	bs = append([]query.Bucket(nil), bs...)
	sort.SliceStable(bs, func(i, j int) bool { return bs[i].Tokens.Total() > bs[j].Tokens.Total() })
	if len(bs) <= n {
		return bs
	}
	rest := query.Bucket{Key: "_other", Label: other}
	for _, b := range bs[n:] {
		rest.Tokens = rest.Tokens.Add(b.Tokens)
		rest.CostUSD += b.CostUSD
		rest.Requests += b.Requests
		rest.Sessions += b.Sessions
	}
	return append(bs[:n], rest)
}
