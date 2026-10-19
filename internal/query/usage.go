package query

import (
	"context"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
)

// UsageRow is one local day's usage of one tool, model and provider.
type UsageRow struct {
	Day      time.Time // local midnight
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
	type key struct {
		day             int64
		h               model.Harness
		model, provider string
	}
	index := map[key]int{}
	out := []UsageRow{}
	for _, v := range d.events {
		day := floor(v.Timestamp, false)
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
	return out, nil
}
