package query

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

// Cache reads aggregate the cache_daily rollup for whole local days and scan only
// the partial boundary days through events, exactly as aggregateSQL does for
// usage. The rollup carries the same metrics per group, so a single UNION ALL
// feeds every cache query. The columns, in order, are:
//
//	day,harness,session_id,project,model,resolved_provider,cause,
//	requests,cacheable,expected,read,miss,miss_cost
const cacheSubColumns = "day,harness,session_id,project,model,resolved_provider,cause,requests,cacheable,expected,read,miss,miss_cost"

// cacheRollupFromEvents computes the cache rollup straight from events for a
// (possibly partial-day) filter, grouped by local day and cause.
func cacheRollupFromEvents(f Filter) (string, []any) {
	where, args := filterSQL(f)
	where = strings.ReplaceAll(where, "harness IN", "(harness || '') IN")
	return `SELECT mytoken_day(timestamp),harness,session_id,project,model,resolved_provider,miss_cause,count(*),sum(cache_expected>=0),sum(max(cache_expected,0)),sum(CASE WHEN cache_expected>=0 THEN cache_read ELSE 0 END),sum(cache_miss),sum(miss_cost) FROM events` + where + ` GROUP BY 1,2,3,4,5,6,7`, args
}

// cacheRangeSQL uses complete local days from cache_daily and scans only the
// partial boundary days through events. Intervals stay exactly [From,To).
func cacheRangeSQL(f Filter) (string, []any) {
	middle := f
	var edges []Filter
	eventsOnly := false
	if !f.Range.From.IsZero() && !f.Range.From.Equal(floor(f.Range.From, false)) {
		end := step(floor(f.Range.From, false), false)
		if !f.Range.To.IsZero() && !end.Before(f.Range.To) {
			eventsOnly = true
		} else {
			edge := f
			edge.Range.To = end
			edges = append(edges, edge)
			middle.Range.From = end
		}
	}
	if !eventsOnly && !f.Range.To.IsZero() && !f.Range.To.Equal(floor(f.Range.To, false)) {
		start := floor(f.Range.To, false)
		if !middle.Range.From.IsZero() && !start.After(middle.Range.From) {
			edge := middle
			edge.Range.To = f.Range.To
			if len(edges) == 0 {
				eventsOnly = true
			} else {
				edges = append(edges, edge)
				middle.Range.To = middle.Range.From
			}
		} else {
			edge := f
			edge.Range.From = start
			edges = append(edges, edge)
			middle.Range.To = start
		}
	}
	if eventsOnly {
		return cacheRollupFromEvents(f)
	}
	where, args := filterSQL(middle)
	where = strings.ReplaceAll(where, "timestamp", "day")
	q := "SELECT " + cacheSubColumns + " FROM cache_daily" + where
	for _, edge := range edges {
		eq, ea := cacheRollupFromEvents(edge)
		q += " UNION ALL " + eq
		args = append(args, ea...)
	}
	return q, args
}

// cacheSource picks the rollup-backed range SQL, but falls back to a pure events
// scan whenever the stored daily timezone differs from the local zone, because a
// display-timezone override must never read (or rewrite) persistent summaries.
func (s *service) cacheSource(ctx context.Context, tx *sql.Tx, f Filter) (string, []any, error) {
	var zone string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='daily_timezone'`).Scan(&zone); err != nil && err != sql.ErrNoRows {
		return "", nil, err
	}
	if zone != store.LocalDayZone() {
		q, a := cacheRollupFromEvents(f)
		return q, a, nil
	}
	q, a := cacheRangeSQL(f)
	return q, a, nil
}

// sortCauses returns the per-cause breakdown with the empty "no miss cause" bucket
// (cold start, fully cached, or uncacheable) last and the named causes sorted.
func sortCauses(m map[string]*CacheCause) []CacheCause {
	out := make([]CacheCause, 0, len(m))
	for _, c := range m {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Cause == "") != (out[j].Cause == "") {
			return out[j].Cause == ""
		}
		return out[i].Cause < out[j].Cause
	})
	return out
}

func (s *service) CacheTotals(ctx context.Context, f Filter) (CacheTotals, error) {
	var out CacheTotals
	tx, err := s.st.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	sub, args, err := s.cacheSource(ctx, tx, f)
	if err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT cause,sum(requests),sum(cacheable),sum(expected),sum(read),sum(miss),sum(miss_cost) FROM (`+sub+`) GROUP BY cause ORDER BY cause`, args...)
	if err != nil {
		return out, err
	}
	causes := map[string]*CacheCause{}
	for rows.Next() {
		var cause string
		var req, cacheable, expected, read, miss int64
		var usd float64
		if err = rows.Scan(&cause, &req, &cacheable, &expected, &read, &miss, &usd); err != nil {
			rows.Close()
			return out, err
		}
		out.Requests += req
		out.Cacheable += cacheable
		out.Expected += expected
		out.Read += read
		out.Miss += miss
		out.USD += usd
		causes[cause] = &CacheCause{Cause: cause, Requests: req, Miss: miss, USD: usd}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	out.ByCause = sortCauses(causes)
	return out, tx.Commit()
}

func (s *service) CacheDaily(ctx context.Context, f Filter) ([]CachePoint, error) {
	tx, err := s.st.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	sub, args, err := s.cacheSource(ctx, tx, f)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT day,cause,sum(requests),sum(cacheable),sum(expected),sum(read),sum(miss),sum(miss_cost) FROM (`+sub+`) GROUP BY day,cause ORDER BY day,cause`, args...)
	if err != nil {
		return nil, err
	}
	days := map[int64]*CachePoint{}
	causes := map[int64]map[string]*CacheCause{}
	for rows.Next() {
		var day int64
		var cause string
		var req, cacheable, expected, read, miss int64
		var usd float64
		if err = rows.Scan(&day, &cause, &req, &cacheable, &expected, &read, &miss, &usd); err != nil {
			rows.Close()
			return nil, err
		}
		p := days[day]
		if p == nil {
			p = &CachePoint{Day: time.Unix(0, day).Local()}
			days[day] = p
			causes[day] = map[string]*CacheCause{}
		}
		p.Requests += req
		p.Cacheable += cacheable
		p.Expected += expected
		p.Read += read
		p.Miss += miss
		p.USD += usd
		causes[day][cause] = &CacheCause{Cause: cause, Requests: req, Miss: miss, USD: usd}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(days) == 0 {
		return []CachePoint{}, tx.Commit()
	}
	// Zero-fill the gaps between the earliest and latest day so charts stay continuous.
	var lo, hi int64
	first := true
	for day := range days {
		if first || day < lo {
			lo = day
		}
		if first || day > hi {
			hi = day
		}
		first = false
	}
	out := []CachePoint{}
	for at := lo; !time.Unix(0, at).After(time.Unix(0, hi)); at = step(time.Unix(0, at), false).UnixNano() {
		if p, ok := days[at]; ok {
			p.ByCause = sortCauses(causes[at])
			out = append(out, *p)
		} else {
			out = append(out, CachePoint{Day: time.Unix(0, at).Local()})
		}
	}
	return out, tx.Commit()
}

func (s *service) CacheBy(ctx context.Context, f Filter, dim CacheDim) ([]CacheBucket, error) {
	tx, err := s.st.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	sub, args, err := s.cacheSource(ctx, tx, f)
	if err != nil {
		return nil, err
	}
	group := "resolved_provider"
	switch dim {
	case CacheByModel:
		group = "model"
	case CacheByHarness:
		group = "harness"
	case CacheByProject:
		group = "project"
	}
	var query string
	if dim == CacheBySession {
		query = `SELECT harness,session_id,cause,sum(requests),sum(cacheable),sum(expected),sum(read),sum(miss),sum(miss_cost) FROM (` + sub + `) GROUP BY harness,session_id,cause`
	} else {
		query = `SELECT ` + group + `,cause,sum(requests),sum(cacheable),sum(expected),sum(read),sum(miss),sum(miss_cost) FROM (` + sub + `) GROUP BY ` + group + `,cause`
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	buckets := map[string]*CacheBucket{}
	causes := map[string]map[string]*CacheCause{}
	for rows.Next() {
		var key, cause string
		var req, cacheable, expected, read, miss int64
		var usd float64
		if dim == CacheBySession {
			var h, sid string
			if err = rows.Scan(&h, &sid, &cause, &req, &cacheable, &expected, &read, &miss, &usd); err != nil {
				rows.Close()
				return nil, err
			}
			key = CacheSessionKey(h, sid)
		} else {
			if err = rows.Scan(&key, &cause, &req, &cacheable, &expected, &read, &miss, &usd); err != nil {
				rows.Close()
				return nil, err
			}
		}
		b := buckets[key]
		if b == nil {
			b = &CacheBucket{Key: key}
			buckets[key] = b
			causes[key] = map[string]*CacheCause{}
		}
		b.Requests += req
		b.Cacheable += cacheable
		b.Expected += expected
		b.Read += read
		b.Miss += miss
		b.USD += usd
		causes[key][cause] = &CacheCause{Cause: cause, Requests: req, Miss: miss, USD: usd}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	out := make([]CacheBucket, 0, len(buckets))
	for k, b := range buckets {
		b.ByCause = sortCauses(causes[k])
		out = append(out, *b)
	}
	// Worst cache offenders first: most misses, then most expected volume.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Miss != out[j].Miss {
			return out[i].Miss > out[j].Miss
		}
		if out[i].Expected != out[j].Expected {
			return out[i].Expected > out[j].Expected
		}
		return out[i].Key < out[j].Key
	})
	return out, tx.Commit()
}

func (s *service) SessionCache(ctx context.Context, h model.Harness, id string) ([]CacheStep, error) {
	tx, err := s.st.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Drive the fact lookup through the dimension index; a session predicate on the
	// view would otherwise tempt SQLite into scanning the dedup index.
	rows, err := tx.QueryContext(ctx, `SELECT e.timestamp,d.model,e.input,e.cache_read,e.cache_write,e.cache_expected,e.cache_miss,e.miss_cause,e.miss_cost,e.boundary FROM event_data e JOIN event_dimensions d ON d.id=e.dimension_id WHERE d.harness=? AND d.session_id=? ORDER BY e.timestamp,e.dedup_key`, h, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CacheStep{}
	for rows.Next() {
		var ts int64
		var mdl, cause, boundary string
		var input, cacheRead, cacheWrite, expected, miss int64
		var usd float64
		if err = rows.Scan(&ts, &mdl, &input, &cacheRead, &cacheWrite, &expected, &miss, &cause, &usd, &boundary); err != nil {
			return nil, err
		}
		// Expected passes through verbatim: -1 marks an uncacheable request (see
		// cache.go), letting callers tell it apart from a cacheable zero expectation.
		out = append(out, CacheStep{At: time.Unix(0, ts).UTC(), Model: mdl, Prompt: input, Read: cacheRead, Write: cacheWrite, Expected: expected, Miss: miss, Cause: cause, USD: usd, Boundary: boundary})
	}
	return out, rows.Err()
}
