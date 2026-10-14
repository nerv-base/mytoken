package query

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/store"
)

const usageColumns = `harness,instance_id,session_id,project,day,model,resolved_provider,attrib,input,output,cache_read,cache_write,reasoning,cost,cost_log,cost_estimate,requests,unpriced`

// providerUsageColumns mirrors usageColumns with base_url in the position the
// endpoint-aware fact aggregate emits it, so a provider_daily rollup row and a
// partial boundary-day fact row scan into the same destination.
const providerUsageColumns = `harness,instance_id,session_id,project,day,model,base_url,resolved_provider,attrib,input,output,cache_read,cache_write,reasoning,cost,cost_log,cost_estimate,requests,unpriced`

// daySplit separates a filter into the complete local days a daily rollup can
// answer and the partial boundary days that have to come from facts. eventsOnly
// reports that the whole interval lies inside a single partial day, in which case
// the caller must read the filter straight from facts. The interval is always
// preserved exactly as [From,To).
func daySplit(f Filter) (middle Filter, edges []Filter, eventsOnly bool) {
	middle = f
	if !f.Range.From.IsZero() && !f.Range.From.Equal(floor(f.Range.From, false)) {
		end := step(floor(f.Range.From, false), false)
		if !f.Range.To.IsZero() && !end.Before(f.Range.To) {
			return f, nil, true
		}
		edge := f
		edge.Range.To = end
		edges = append(edges, edge)
		middle.Range.From = end
	}
	if !f.Range.To.IsZero() && !f.Range.To.Equal(floor(f.Range.To, false)) {
		start := floor(f.Range.To, false)
		if !middle.Range.From.IsZero() && !start.After(middle.Range.From) {
			edge := middle
			edge.Range.To = f.Range.To
			if len(edges) == 0 {
				// The whole interval is the tail of one local day, so it is
				// already covered by the unmodified filter.
				return f, nil, true
			}
			edges = append(edges, edge)
			middle.Range.To = middle.Range.From
		} else {
			edge := f
			edge.Range.From = start
			edges = append(edges, edge)
			middle.Range.To = start
		}
	}
	return middle, edges, false
}

// aggregateSQL uses complete local days from the rollup, and scans only the
// partial boundary days through events_time. Intervals remain exactly [From,To).
func aggregateSQL(f Filter, hourly bool) (string, []any) {
	if hourly {
		return eventAggregateSQL(f, true)
	}
	middle, edges, eventsOnly := daySplit(f)
	if eventsOnly {
		return eventAggregateSQL(f, false)
	}
	where, args := filterSQL(middle)
	where = strings.ReplaceAll(where, "timestamp", "day")
	q := "SELECT " + usageColumns + " FROM daily_usage" + where
	for _, edge := range edges {
		eq, ea := eventAggregateSQL(edge, false)
		q += " UNION ALL " + eq
		args = append(args, ea...)
	}
	return q, args
}

// providerAggregateSQL answers an endpoint-aware aggregate from the derived
// provider_daily rollup for complete local days and from facts for the partial
// boundary days. daily_usage drops base_url, so without this rollup every
// identity query had to aggregate the whole fact table.
func providerAggregateSQL(f Filter) (string, []any) {
	middle, edges, eventsOnly := daySplit(f)
	if eventsOnly {
		return eventAggregateSQLMode(f, false, true)
	}
	where, args := filterSQL(middle)
	where = strings.ReplaceAll(where, "timestamp", "day")
	q := "SELECT " + providerUsageColumns + " FROM provider_daily" + where
	for _, edge := range edges {
		eq, ea := eventAggregateSQLMode(edge, false, true)
		q += " UNION ALL " + eq
		args = append(args, ea...)
	}
	return q, args
}

func eventAggregateSQL(f Filter, hourly bool) (string, []any) {
	return eventAggregateSQLMode(f, hourly, false)
}

func eventAggregateSQLMode(f Filter, hourly, withEndpoint bool) (string, []any) {
	where, args := filterSQL(f)
	if !f.Range.From.IsZero() || !f.Range.To.IsZero() {
		// Keep a harness filter from selecting the (harness,dedup_key) unique
		// index and scanning that harness's entire history. The text expression
		// preserves equality semantics but lets events_time bound the fact scan;
		// unlike INDEXED BY it also works while initial bulk indexes are deferred.
		where = strings.ReplaceAll(where, "harness IN", "(harness || '') IN")
	}
	bucket := "mytoken_day(timestamp)"
	if hourly {
		// The Go-backed SQL function uses the same local/DST rules as series().
		bucket = "mytoken_hour(timestamp)"
	}
	dimensions := "model,resolved_provider,attrib"
	if withEndpoint {
		dimensions = "model,base_url,resolved_provider,attrib"
	}
	return `SELECT harness,instance_id,session_id,project,` + bucket + ` AS day,` + dimensions + `,sum(input),sum(output),sum(cache_read),sum(cache_write),sum(reasoning),sum(CASE WHEN priced THEN cost ELSE 0 END),sum(CASE WHEN log_cost IS NOT NULL THEN cost ELSE 0 END),sum(CASE WHEN log_cost IS NULL AND priced THEN cost ELSE 0 END),count(*),sum(NOT priced) FROM events` + where + ` GROUP BY harness,instance_id,session_id,project,day,` + dimensions, args
}

// branchAggregateSQL reads the per-request branch dimension. A branch is not a
// usage dimension, so it lives in its own metadata table and is never carried by
// a rollup: this is always a fact scan. The metadata is joined from the facts by
// the same stable identity the fact row uses, as a LEFT JOIN, so a request whose
// log stated no branch stays in the result with an empty branch instead of being
// dropped. When the index predates the table the join is skipped entirely and
// every request reads as unknown.
func branchAggregateSQL(f Filter, hasBranch bool) (string, []any) {
	where, args := filterSQL(f)
	if !f.Range.From.IsZero() || !f.Range.To.IsZero() {
		where = strings.ReplaceAll(where, "harness IN", "(harness || '') IN")
	}
	branch := "''"
	from := " FROM events"
	if hasBranch {
		// USING keeps the three key columns unambiguous, and every other column
		// the filter and the projection name exists only in the facts.
		branch = "coalesce(b.branch,'')"
		from = " FROM events LEFT JOIN event_branches b USING(harness,instance_id,dedup_key)"
	}
	return `SELECT harness,instance_id,session_id,project,` + branch + `,model,base_url,resolved_provider,attrib,sum(input),sum(output),sum(cache_read),sum(cache_write),sum(reasoning),sum(CASE WHEN priced THEN cost ELSE 0 END),sum(CASE WHEN log_cost IS NOT NULL THEN cost ELSE 0 END),sum(CASE WHEN log_cost IS NULL AND priced THEN cost ELSE 0 END),count(*),sum(NOT priced)` + from + where + ` GROUP BY harness,instance_id,session_id,project,` + branch + `,model,base_url,resolved_provider,attrib`, args
}

// scanUsageMode reads usage facts. branch selects the per-request branch
// dimension, which exists only in the fact table, so it always reads facts and
// never a rollup — the costs it sums are the same expressions the rollups were
// built from, so a branch grouping still adds up to the totals.
func scanUsageMode(ctx context.Context, tx *sql.Tx, f Filter, hourly, withEndpoint, branch bool) ([]AttributedEvent, error) {
	var zone string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='daily_timezone'`).Scan(&zone); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	// CLI display-timezone overrides must never read (or rewrite) persistent
	// summaries, so they always fall back to facts. Hourly buckets only exist in
	// the fact aggregate, because both rollups are keyed by local day.
	persistentDays := zone == store.LocalDayZone()
	var query string
	var args []any
	switch {
	case branch:
		hasBranch, err := store.EventBranchesPresent(ctx, tx)
		if err != nil {
			return nil, err
		}
		query, args = branchAggregateSQL(f, hasBranch)
	case hourly:
		query, args = eventAggregateSQLMode(f, true, withEndpoint)
	case withEndpoint && persistentDays:
		// provider_daily carries base_url, so an endpoint identity filter reads
		// the endpoint-aware rollup instead of aggregating every fact row.
		query, args = providerAggregateSQL(f)
	case withEndpoint:
		query, args = eventAggregateSQLMode(f, false, true)
	case persistentDays:
		query, args = aggregateSQL(f, false)
	default:
		query, args = eventAggregateSQL(f, false)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AttributedEvent{}
	for rows.Next() {
		var v AttributedEvent
		if branch {
			dest := []any{&v.Harness, &v.InstanceID, &v.SessionID, &v.ProjectPath, &v.GitBranch, &v.Model, &v.BaseURL, &v.ResolvedProvider, &v.Attrib, &v.Tokens.Input, &v.Tokens.Output, &v.Tokens.CacheRead, &v.Tokens.CacheWrite, &v.Tokens.Reasoning, &v.Cost, &v.costLog, &v.costEstimate, &v.requests, &v.unpriced}
			if err = rows.Scan(dest...); err != nil {
				return nil, err
			}
			v.Priced = v.unpriced == 0
			out = append(out, v)
			continue
		}
		var stamp int64
		dest := []any{&v.Harness, &v.InstanceID, &v.SessionID, &v.ProjectPath, &stamp, &v.Model}
		if withEndpoint {
			dest = append(dest, &v.BaseURL)
		}
		dest = append(dest, &v.ResolvedProvider, &v.Attrib, &v.Tokens.Input, &v.Tokens.Output, &v.Tokens.CacheRead, &v.Tokens.CacheWrite, &v.Tokens.Reasoning, &v.Cost, &v.costLog, &v.costEstimate, &v.requests, &v.unpriced)
		if err = rows.Scan(dest...); err != nil {
			return nil, err
		}
		v.Timestamp = time.Unix(0, stamp).UTC()
		v.Priced = v.unpriced == 0
		out = append(out, v)
	}
	return out, rows.Err()
}

func eventSource(v *AttributedEvent) {
	v.requests = 1
	if v.CostUSD != nil {
		v.CostSource = CostSourceLog
		v.costLog = v.Cost
	} else if v.Priced {
		v.CostSource = CostSourceEstimate
		v.costEstimate = v.Cost
	} else {
		v.CostSource = CostSourceUnpriced
		v.unpriced = 1
		v.Cost = 0
	}
}
