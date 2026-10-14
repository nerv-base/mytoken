package budget

import (
	"context"
	"errors"
	"fmt"

	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/reporttime"
)

// LocalCurrency is the currency every local estimate is denominated in. The
// store prices usage in US dollars and no other currency is ever produced:
// amounts in a different currency are never converted, combined or compared.
const LocalCurrency = "USD"

// Spend is the local estimated cost of one scope over one exact calendar
// period, plus the number of requests whose price is unknown. A spend with
// Unpriced > 0 is a lower bound, not the whole cost, and callers must say so.
type Spend struct {
	Amount   float64
	Unpriced int64
}

// SpendSource answers what one scope cost, in local estimated currency, over
// one exact calendar period.
//
// Implementations must use estimated costs only. Log-billed and relay-billed
// amounts are a different quantity (what a provider charged, not what the local
// price table estimates) and must never be summed into an estimate. They must
// answer for exactly the period they are handed, and they must report ok=false
// when the scope has no data in that period: no data is not a zero estimate.
type SpendSource interface {
	EstimatedSpend(ctx context.Context, scope Scope, period reporttime.Period) (spend Spend, ok bool, err error)
}

// QueryCosts reads local estimated cost from the existing query service. It
// adds no provenance of its own: CostEstimateUSD is the store's cost_estimate
// column, the sum of priced events that carry no log cost.
type QueryCosts struct {
	Q query.Service
}

// EstimatedSpend implements SpendSource over query.Service. Global scope uses
// Totals, project scope ByProject's stable project key and provider scope
// ProviderGroups' stable display identity key (the same key the providers page
// drills into, so a rule cannot drift onto a display label).
func (q QueryCosts) EstimatedSpend(ctx context.Context, scope Scope, period reporttime.Period) (Spend, bool, error) {
	if q.Q == nil {
		return Spend{}, false, errors.New("budget: no query service")
	}
	if err := scope.Validate(); err != nil {
		return Spend{}, false, err
	}
	if err := validateSpendPeriod(period); err != nil {
		return Spend{}, false, err
	}
	f := query.Filter{Range: query.Range{From: period.Start, To: period.End}}
	switch scope.Kind {
	case ScopeGlobal:
		t, err := q.Q.Totals(ctx, f)
		if err != nil {
			return Spend{}, false, err
		}
		if t.Requests == 0 {
			return Spend{}, false, nil
		}
		return Spend{Amount: t.CostEstimateUSD, Unpriced: t.Unpriced}, true, nil
	case ScopeProject:
		buckets, err := q.Q.ByProject(ctx, f)
		if err != nil {
			return Spend{}, false, err
		}
		for _, b := range buckets {
			if b.Key != scope.ID {
				continue
			}
			if b.Requests == 0 {
				return Spend{}, false, nil
			}
			return Spend{Amount: b.CostEstimateUSD, Unpriced: b.Unpriced}, true, nil
		}
		return Spend{}, false, nil
	case ScopeProvider:
		groups, err := q.Q.ProviderGroups(ctx, f)
		if err != nil {
			return Spend{}, false, err
		}
		for _, g := range groups {
			if g.Key != scope.ID {
				continue
			}
			if g.Requests == 0 {
				return Spend{}, false, nil
			}
			return Spend{Amount: g.CostEstimateUSD, Unpriced: g.Unpriced}, true, nil
		}
		return Spend{}, false, nil
	default:
		return Spend{}, false, fmt.Errorf("%w: unknown scope kind %q", ErrInvalidScope, scope.Kind)
	}
}
