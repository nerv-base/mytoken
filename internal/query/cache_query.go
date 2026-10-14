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
const cacheSubColumns = "day,harness,instance_id,session_id,project,model,resolved_provider,cause,requests,cacheable,expected,read,miss,miss_cost"

// providerCacheSubColumns is cacheSubColumns with base_url, in the position the
// endpoint-aware rollup and the endpoint-aware fact scan both emit it, so the
// UNION ALL arms of a single cache query scan into one shape.
const providerCacheSubColumns = "day,harness,instance_id,session_id,project,model,resolved_provider,base_url,cause,requests,cacheable,expected,read,miss,miss_cost"

// cacheRollupFromEvents computes the cache rollup straight from events for a
// (possibly partial-day) filter, grouped by local day and cause.
func cacheRollupFromEvents(f Filter) (string, []any) {
	return cacheRollupFromEventsWhere(f, "", nil)
}

// cacheRollupFromEventsEndpoint is the endpoint-aware variant: it also groups by
// base_url so that the endpoint identity predicate can be evaluated against the
// derivation instead of being applied after a full fact aggregation. Without it
// each distinct endpoint identity would need its own full scan.
func cacheRollupFromEventsEndpoint(f Filter, extra string, extraArgs []any) (string, []any) {
	where, args := filterSQL(f)
	where = strings.ReplaceAll(where, "harness IN", "(harness || '') IN")
	if extra != "" {
		if where == "" {
			where = " WHERE " + extra
		} else {
			where += " AND " + extra
		}
		args = append(args, extraArgs...)
	}
	return `SELECT mytoken_day(timestamp) AS day,harness,instance_id,session_id,project,model,resolved_provider,base_url,miss_cause AS cause,count(*) AS requests,sum(cache_expected>=0) AS cacheable,sum(max(cache_expected,0)) AS expected,sum(CASE WHEN cache_expected>=0 THEN cache_read ELSE 0 END) AS read,sum(cache_miss) AS miss,sum(miss_cost) AS miss_cost FROM events` + where + ` GROUP BY 1,2,3,4,5,6,7,8,9`, args
}

// cacheRollupFromEventsWhere appends an extra predicate (with its bound args) to
// the events scan. Cache identity filters pass the predicate from
// providerIdentityPredicate here because neither cache_daily nor daily_usage
// carry an endpoint column and EndpointID hashes in Go.
func cacheRollupFromEventsWhere(f Filter, extra string, extraArgs []any) (string, []any) {
	where, args := filterSQL(f)
	where = strings.ReplaceAll(where, "harness IN", "(harness || '') IN")
	if extra != "" {
		if where == "" {
			where = " WHERE " + extra
		} else {
			where += " AND " + extra
		}
		args = append(args, extraArgs...)
	}
	// Alias every column to the cache_daily names: this SELECT also feeds
	// UNION ALL arms and outer queries that reference those names.
	return `SELECT mytoken_day(timestamp) AS day,harness,instance_id,session_id,project,model,resolved_provider,miss_cause AS cause,count(*) AS requests,sum(cache_expected>=0) AS cacheable,sum(max(cache_expected,0)) AS expected,sum(CASE WHEN cache_expected>=0 THEN cache_read ELSE 0 END) AS read,sum(cache_miss) AS miss,sum(miss_cost) AS miss_cost FROM events` + where + ` GROUP BY 1,2,3,4,5,6,7,8`, args
}

// cacheRangeSQL uses complete local days from cache_daily and scans only the
// partial boundary days through events. Intervals stay exactly [From,To).
func cacheRangeSQL(f Filter) (string, []any) {
	middle, edges, eventsOnly := daySplit(f)
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

// providerCacheRangeSQL mirrors cacheRangeSQL against the endpoint-aware
// provider_cache_daily rollup, so an endpoint identity filter still gets the
// whole-day rollup path and only the partial boundary days read facts. The extra
// predicate and its args are the identity restriction; they are applied to both
// arms so every returned group is inside the selected identity set.
func providerCacheRangeSQL(f Filter, extra string, extraArgs []any) (string, []any) {
	middle, edges, eventsOnly := daySplit(f)
	if eventsOnly {
		return cacheRollupFromEventsEndpoint(f, extra, extraArgs)
	}
	where, args := filterSQL(middle)
	where = strings.ReplaceAll(where, "timestamp", "day")
	if extra != "" {
		if where == "" {
			where = " WHERE " + extra
		} else {
			where += " AND " + extra
		}
		args = append(args, extraArgs...)
	}
	q := "SELECT " + providerCacheSubColumns + " FROM provider_cache_daily" + where
	for _, edge := range edges {
		eq, ea := cacheRollupFromEventsEndpoint(edge, extra, extraArgs)
		q += " UNION ALL " + eq
		args = append(args, ea...)
	}
	return q, args
}

// cacheSource picks the rollup-backed range SQL, but falls back to a pure events
// scan whenever the stored daily timezone differs from the local zone, because a
// display-timezone override must never read (or rewrite) persistent summaries.
func (s *service) cacheSource(ctx context.Context, tx *sql.Tx, f Filter) (string, []any, error) {
	// filterSQL still applies the ordinary name/model/project/date filters, so
	// identity selection only ever narrows the usual semantics.
	predicate, args, restricted, err := providerIdentityPredicate(ctx, tx, f)
	if err != nil {
		return "", nil, err
	}
	var zone string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='daily_timezone'`).Scan(&zone); err != nil && err != sql.ErrNoRows {
		return "", nil, err
	}
	persistentDays := zone == store.LocalDayZone()
	if restricted {
		// The endpoint-aware cache rollup carries base_url, so the same bounded
		// identity predicate applies to it directly instead of to raw facts. A
		// non-nil empty identity set means no event matched, not "no filter".
		if persistentDays {
			q, a := providerCacheRangeSQL(f, predicate, args)
			return q, a, nil
		}
		q, a := cacheRollupFromEventsEndpoint(f, predicate, args)
		return q, a, nil
	}
	if !persistentDays {
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
	tx, err := s.st.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return CacheTotals{}, err
	}
	defer tx.Rollback()
	out, err := s.cacheTotalsTx(ctx, tx, f)
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

// cacheTotalsTx reads the cache ledger inside a caller-owned transaction so a
// snapshot can pair it with totals from the same read snapshot.
func (s *service) cacheTotalsTx(ctx context.Context, tx *sql.Tx, f Filter) (CacheTotals, error) {
	var out CacheTotals
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
	return out, nil
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
		// The instance is part of a session's identity, so it must be part of the
		// grouping and of the bucket key; otherwise two instances' identical
		// harness/session id would sum into one row.
		query = `SELECT instance_id,harness,session_id,cause,sum(requests),sum(cacheable),sum(expected),sum(read),sum(miss),sum(miss_cost) FROM (` + sub + `) GROUP BY instance_id,harness,session_id,cause`
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
			var instance, h, sid string
			if err = rows.Scan(&instance, &h, &sid, &cause, &req, &cacheable, &expected, &read, &miss, &usd); err != nil {
				rows.Close()
				return nil, err
			}
			key = CacheSessionKeyIn(instance, h, sid)
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
	return s.sessionCache(ctx, h, "", id, false)
}

// SessionCacheIn is SessionCache for one explicit instance, so a session id that
// exists in several data sources can still be read unambiguously.
func (s *service) SessionCacheIn(ctx context.Context, h model.Harness, instance, id string) ([]CacheStep, error) {
	return s.sessionCache(ctx, h, store.NormalizeInstanceID(instance), id, true)
}

// sessionCache reads one session's cache verdicts. Without an explicit instance
// it reports ErrAmbiguousInstance rather than merging two data sources' steps:
// the per-request rows carry no instance column of their own, so mixing them
// would silently interleave two unrelated sessions.
func (s *service) sessionCache(ctx context.Context, h model.Harness, instance, id string, scoped bool) ([]CacheStep, error) {
	tx, err := s.st.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Drive the fact lookup through the dimension index; a session predicate on the
	// view would otherwise tempt SQLite into scanning the dedup index.
	query := `SELECT e.instance_id,e.timestamp,d.model,e.input,e.cache_read,e.cache_write,e.cache_expected,e.cache_miss,e.miss_cause,e.miss_cost,e.boundary FROM event_data e JOIN event_dimensions d ON d.id=e.dimension_id WHERE d.harness=? AND d.session_id=?`
	args := []any{h, id}
	if scoped {
		query += ` AND e.instance_id=?`
		args = append(args, instance)
	}
	query += ` ORDER BY e.timestamp,e.dedup_key`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	instances := map[string]bool{}
	out := []CacheStep{}
	for rows.Next() {
		var instance, mdl, cause, boundary string
		var ts int64
		var input, cacheRead, cacheWrite, expected, miss int64
		var usd float64
		if err = rows.Scan(&instance, &ts, &mdl, &input, &cacheRead, &cacheWrite, &expected, &miss, &cause, &usd, &boundary); err != nil {
			return nil, err
		}
		instances[store.NormalizeInstanceID(instance)] = true
		// Expected passes through verbatim: -1 marks an uncacheable request (see
		// cache.go), letting callers tell it apart from a cacheable zero expectation.
		out = append(out, CacheStep{At: time.Unix(0, ts).UTC(), Model: mdl, Prompt: input, Read: cacheRead, Write: cacheWrite, Expected: expected, Miss: miss, Cause: cause, USD: usd, Boundary: boundary})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if !scoped && len(instances) > 1 {
		return nil, ErrAmbiguousInstance
	}
	return out, tx.Commit()
}
