package query

import (
	"context"
	"sort"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
)

// UsageRow is one local day's usage of one tool, model and provider.
// HourlyUsage reuses it for one local clock hour, with Day set to the start of
// that hour.
type UsageRow struct {
	Day      time.Time // local midnight (Usage) or local clock-hour start (HourlyUsage)
	Harness  model.Harness
	Model    string // after model aliases
	Provider string // resolved provider
	Tokens   model.Tokens
	CostUSD  float64
	Requests int64
	Unpriced int64
}

// Usage returns the filter's usage grouped by local day, tool, model and
// provider, in no particular order. It is one read for a dashboard that
// derives several totals, series and rankings from the same range, instead
// of paying the fixed cost of a separate query for each.
func (s *service) Usage(ctx context.Context, f Filter) ([]UsageRow, error) {
	d, err := s.load(ctx, f)
	if err != nil {
		return nil, err
	}
	return usageRows(d, false), nil
}

// HourlyUsage returns the filter's usage bucketed by local clock hour, so a
// dashboard can show today broken down within the day without the zero-filled
// points Hourly returns. Only hours that carry usage get a row. It shares
// Usage's pricing, attribution and filters; the hour read goes through the
// fact aggregate because neither rollup is keyed by hour.
func (s *service) HourlyUsage(ctx context.Context, f Filter) ([]UsageRow, error) {
	d, err := s.load(ctx, f, loadMode{hourly: true})
	if err != nil {
		return nil, err
	}
	out := usageRows(d, true)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.Day.Equal(b.Day) {
			return a.Day.Before(b.Day)
		}
		if a.Harness != b.Harness {
			return a.Harness < b.Harness
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.Provider < b.Provider
	})
	return out, nil
}

// usageRows folds loaded events into one row per bucket. hourly selects the
// bucket width; the rest of the aggregation is identical, so Usage and
// HourlyUsage cannot drift apart.
func usageRows(d data, hourly bool) []UsageRow {
	type key struct {
		day             int64
		h               model.Harness
		model, provider string
	}
	index := map[key]int{}
	out := []UsageRow{}
	for _, v := range d.events {
		day := floor(v.Timestamp, hourly)
		k := key{day.UnixNano(), v.Harness, v.Model, v.ResolvedProvider}
		i, ok := index[k]
		if !ok {
			i = len(out)
			index[k] = i
			out = append(out, UsageRow{Day: day, Harness: v.Harness, Model: v.Model, Provider: v.ResolvedProvider})
		}
		r := &out[i]
		r.Tokens = r.Tokens.Add(v.Tokens)
		r.CostUSD += v.Cost
		r.Requests += v.requests
		r.Unpriced += v.unpriced
	}
	return out
}
