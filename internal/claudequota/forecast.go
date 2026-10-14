package claudequota

import (
	"time"

	"github.com/zzstar101/mytoken/internal/quota"
)

// forecastOptions returns the bounds a Claude reading's forecast uses. The
// staleness budget is the reading's own StaleAfter, so a forecast can never
// claim to be fresher than the reading it was derived from. The pace defaults
// apply otherwise: the rate is a percentage of the window per unit time, never
// a token or cost count, which this package does not observe.
func forecastOptions() quota.ForecastOptions {
	return quota.ForecastOptions{MaxAge: StaleAfter}
}

// Forecasts derives the pace forecast for one cached reading.
//
// It is the entry point a CLI or GUI should call when it holds a reading and
// wants the pace recomputed at its own instant: it reads only the reading's
// retained observations, never a credential, the store or the network. The
// same forecast is already stored on the reading when it was published, so a
// consumer that only wants to render can use Quota.Forecast directly.
//
// It never returns nil: a reading with no retained samples yields a forecast
// that is unavailable with a reason.
func Forecasts(q Quota, now time.Time) *quota.Forecast {
	return quota.ForecastObservations(q.Observations, now, forecastOptions())
}

// observationsFrom returns the samples a successful reading contributes.
//
// Only a reading that succeeded contributes: a failure repeats windows it did
// not re-measure, and repeating them must never look like a second sample. A
// window the endpoint did not state carries no percentage and is skipped, so
// "absent" never becomes 0%.
func observationsFrom(q Quota, observedAt time.Time) []quota.Observation {
	if q.Status != StatusOK || observedAt.IsZero() {
		return nil
	}
	at := observedAt.UTC()
	out := make([]quota.Observation, 0, len(q.Windows))
	for _, w := range q.Windows {
		if w.Kind == "" || !w.Present || w.UsedPercent == nil {
			continue
		}
		o := quota.Observation{
			Window:      quota.WindowKind(w.Kind),
			Source:      q.SourceRef,
			ObservedAt:  at,
			UsedPercent: *w.UsedPercent,
		}
		if w.RemainingPercent != nil {
			remaining := *w.RemainingPercent
			o.RemainingPercent = &remaining
		}
		if w.ResetsAt != nil {
			reset := w.ResetsAt.UTC()
			o.ResetsAt = &reset
		}
		out = append(out, o)
	}
	return out
}

// withForecast folds a reading's samples into the retained set and recomputes
// the forecast. prior is the reading that was cached before q; the caller must
// only pass it once the consent that authorizes this publish has been checked,
// so retention never reads quota state the attempt may not write.
//
// A reading that already carries its own retention (the failure path keeps the
// previous samples) is authoritative for its own set, and a reading that did not
// succeed adds nothing.
func withForecast(prior, q Quota, now time.Time) Quota {
	obs := q.Observations
	if len(obs) == 0 {
		obs = prior.Observations
	}
	if q.Status == StatusOK {
		obs = quota.RetainObservations(obs, observationsFrom(q, q.FetchedAt)...)
	}
	q.Observations = obs
	q.Forecast = quota.ForecastObservations(obs, now, forecastOptions())
	return q
}
