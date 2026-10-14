package budget

import (
	"fmt"
	"sort"
	"time"

	"github.com/zzstar101/mytoken/internal/reporttime"
)

// Evaluate validates the input, resolves each rule's current period in the
// reporting timezone, compares the rule's estimated spend against its
// thresholds and reserves one notification per newly crossed threshold.
//
// It is pure: it reads no clock, no globals and no files. The previous State is
// never mutated; the next State is returned inside Result and is safe to
// persist with EncodeState. Notifications in Result are only the ones newly
// reserved by this call, so a caller that persists the returned state before
// delivering gets at-most-once notification per period; see the package
// documentation for the crash tradeoff.
//
// Spend is matched on scope, period identity and currency together: a global
// day rule, a global week rule and a global month rule evaluated in one call
// each see only their own period's estimate, and a same-scope estimate for a
// different period or a different currency is never substituted, never
// defaulted and never summed. A spend whose period is not a real calendar
// period of its kind is refused with ErrInvalidPeriod rather than guessed.
//
// The once-per-period guarantee is anchored in a durable monotonic watermark per
// rule generation, so an evaluation that regresses behind the persisted
// watermark (wall-clock rollback, a timezone switch back to an earlier period,
// or a replay after a restart) is refused with ErrEvaluationRegressed and
// mutates nothing. A Version below the recorded generation is refused with
// ErrRuleVersionDecrease: it is a stale configuration, never a new generation.
//
// Errors are sentinel-wrapped and abort the whole evaluation: a single invalid
// rule, a duplicated spend, a rule edited without a version bump, a currency
// mismatch or a regressed evaluation leaves the ledger untouched. Rules are
// reported in ID order and notifications in (rule ID, threshold) order, so the
// output is deterministic for a given input.
func Evaluate(prev State, in Input) (Result, error) {
	if in.Location == nil {
		return Result{}, fmt.Errorf("%w: a reporting timezone is required", ErrInvalidInput)
	}
	if in.Now.IsZero() {
		return Result{}, fmt.Errorf("%w: a non-zero evaluation instant is required", ErrInvalidInput)
	}
	if prev.Version != 0 && prev.Version != SchemaVersion {
		return Result{}, fmt.Errorf("%w: previous state is version %d (want %d)", ErrStateVersion, prev.Version, SchemaVersion)
	}

	rules := append([]Rule(nil), in.Rules...)
	sort.Slice(rules, func(i, j int) bool { return rules[i].ID < rules[j].ID })
	for i, r := range rules {
		if err := r.Validate(); err != nil {
			return Result{}, fmt.Errorf("%w: rule %q: %w", ErrInvalidRule, r.ID, err)
		}
		if i > 0 && rules[i-1].ID == r.ID {
			return Result{}, fmt.Errorf("%w: %q", ErrDuplicateRule, r.ID)
		}
	}

	// Index the estimates by the full identity a rule can match on: scope,
	// period identity and currency. The same scope may carry separate day, week
	// and month estimates and separate currencies; only an exact triple is a
	// duplicate, because only an exact triple is the same claim twice.
	spends := make(map[string]map[string]EstimatedSpend, len(in.Spends))
	for _, s := range in.Spends {
		if err := s.Validate(); err != nil {
			return Result{}, fmt.Errorf("spend for %s: %w", s.Scope, err)
		}
		key := scopePeriodKey(s.Scope, s.PeriodID())
		byCurrency := spends[key]
		if byCurrency == nil {
			byCurrency = make(map[string]EstimatedSpend, 1)
			spends[key] = byCurrency
		}
		if _, dup := byCurrency[s.Currency]; dup {
			return Result{}, fmt.Errorf("%w: %s in %s for %s", ErrDuplicateSpend, s.Scope, s.Currency, s.PeriodID())
		}
		byCurrency[s.Currency] = s
	}

	next := cloneState(prev)
	evals := make([]Evaluation, 0, len(rules))
	var notifs []Notification

	for _, r := range rules {
		period, err := reporttime.Resolve(in.Now, r.Period, in.Location)
		if err != nil {
			return Result{}, fmt.Errorf("%w: rule %q: %w", ErrInvalidPeriod, r.ID, err)
		}
		periodID := period.Current.ID()
		fingerprint := r.Fingerprint()

		// Decide this generation's identity and the watermark it inherits. A
		// new generation has no history, and a stale generation is refused
		// before its fingerprint is even considered.
		known, seen := next.Rules[r.ID]
		var watermark time.Time
		switch {
		case !seen:
			// Nothing can legitimately be reserved for an unknown generation,
			// but drop any stray entries rather than trust them.
			dropRuleReservations(next, r.ID)
		case r.Version < known.Version:
			return Result{}, fmt.Errorf("%w: rule %q is version %d but version %d is already recorded; a version decrease is a stale configuration, not a new generation",
				ErrRuleVersionDecrease, r.ID, r.Version, known.Version)
		case r.Version > known.Version:
			// An explicit new generation. Its identity changed by design, so
			// the previous generation's reservations and watermark are dropped
			// rather than silently reused.
			dropRuleReservations(next, r.ID)
		case known.Fingerprint != fingerprint:
			return Result{}, fmt.Errorf("%w: rule %q is still version %d but its scope, period, currency, limit or thresholds changed; bump Version to start a new generation", ErrRuleVersionConflict, r.ID, r.Version)
		default:
			watermark = known.Watermark
		}

		// The watermark is the period start, not the wall instant, so moving
		// the clock backwards inside the current period is harmless: that
		// period's reservation is still in the ledger. Starting before the
		// watermark means the period was already left behind, and re-entering it
		// could re-notify, so it is refused with nothing mutated.
		if !watermark.IsZero() && period.Current.Start.Before(watermark) {
			return Result{}, fmt.Errorf("%w: rule %q resolves to period %s starting %s, before the recorded watermark %s; refusing instead of re-notifying an earlier period",
				ErrEvaluationRegressed, r.ID, periodID, period.Current.Start.Format(time.RFC3339Nano), watermark.Format(time.RFC3339Nano))
		}
		next.Rules[r.ID] = RuleState{Version: r.Version, Fingerprint: fingerprint, LastPeriodID: periodID, Watermark: period.Current.Start}

		// Bound the ledger to this rule's current period. Reservations from an
		// earlier period can no longer be crossed at or after the watermark, and
		// dropping them keeps the persisted state from growing without limit:
		// at most one watermark plus one reservation per effective threshold per
		// rule generation.
		dropOtherPeriodReservations(next, r.ID, periodID)

		ev := Evaluation{
			RuleID:      r.ID,
			RuleVersion: r.Version,
			Label:       r.Label,
			Scope:       r.Scope,
			Period:      r.Period,
			PeriodID:    periodID,
			Currency:    r.Currency,
			Limit:       r.Limit,
		}
		byCurrency := spends[scopePeriodKey(r.Scope, periodID)]
		spend, hasSpend := byCurrency[r.Currency]
		switch {
		case r.Disabled:
			// A disabled rule is not evaluated, but its ledger entry and its
			// reservations are kept, so re-enabling it in the same period does
			// not re-notify. The watermark still advanced above, because the
			// ledger was pruned for this period.
			ev.SkippedReason = SkipDisabled
		case !hasSpend && len(byCurrency) > 0:
			return Result{}, fmt.Errorf("%w: rule %q is in %s but the spend for %s in %s is in %s; currencies are never converted or combined",
				ErrCurrencyMismatch, r.ID, r.Currency, r.Scope, periodID, firstCurrency(byCurrency))
		case !hasSpend:
			// No estimate for this period is not a zero estimate.
			ev.SkippedReason = SkipNoData
		default:
			ev.DataAvailable = true
			ev.Estimated = spend.Amount
			ev.UnpricedRequests = spend.UnpricedRequests
			ev.UnknownPricing = spend.UnpricedRequests > 0
			ev.Percent = spend.Amount / r.Limit * 100
			for _, threshold := range r.EffectiveThresholds() {
				if crosses(spend.Amount, r.Limit, threshold) {
					ev.Crossed = append(ev.Crossed, threshold)
				}
			}
			for _, threshold := range ev.Crossed {
				id := NotificationID(r, periodID, threshold)
				if _, reserved := next.Reservations[id]; reserved {
					continue
				}
				next.Reservations[id] = Reservation{
					ID:          id,
					RuleID:      r.ID,
					RuleVersion: r.Version,
					PeriodID:    periodID,
					Threshold:   threshold,
					ReservedAt:  in.Now,
				}
				notifs = append(notifs, Notification{
					ID:             id,
					RuleID:         r.ID,
					RuleVersion:    r.Version,
					Label:          r.Label,
					Scope:          r.Scope,
					Period:         r.Period,
					PeriodID:       periodID,
					Threshold:      threshold,
					Currency:       r.Currency,
					Limit:          r.Limit,
					Estimated:      spend.Amount,
					Percent:        ev.Percent,
					UnknownPricing: ev.UnknownPricing,
					ReservedAt:     in.Now,
				})
			}
		}
		evals = append(evals, ev)
	}

	sort.SliceStable(notifs, func(i, j int) bool {
		if notifs[i].RuleID != notifs[j].RuleID {
			return notifs[i].RuleID < notifs[j].RuleID
		}
		if notifs[i].RuleVersion != notifs[j].RuleVersion {
			return notifs[i].RuleVersion < notifs[j].RuleVersion
		}
		if notifs[i].PeriodID != notifs[j].PeriodID {
			return notifs[i].PeriodID < notifs[j].PeriodID
		}
		return notifs[i].Threshold < notifs[j].Threshold
	})

	return Result{State: next, Evaluations: evals, Notifications: notifs}, nil
}

// crosses reports whether amount has reached threshold percent of limit.
//
// It compares by cross-multiplication (amount*100 >= threshold*limit) instead of
// dividing, so the exact 80% and 100% boundaries are not moved by rounding in
// amount/limit.
func crosses(amount, limit, threshold float64) bool {
	return amount*100 >= threshold*limit
}

// scopePeriodKey is the lookup key for "this scope's estimate for this exact
// period". The period id already carries the kind, the timezone and the actual
// start instant, so two periods that merely look alike are still distinct.
func scopePeriodKey(scope Scope, periodID string) string {
	return scope.String() + "\x00" + periodID
}

// dropRuleReservations forgets one rule generation's reservations.
func dropRuleReservations(s State, ruleID string) {
	for id, res := range s.Reservations {
		if res.RuleID == ruleID {
			delete(s.Reservations, id)
		}
	}
}

// dropOtherPeriodReservations bounds a rule's ledger to its current period. It
// is only ever called once the period is known not to precede the rule's
// watermark, so the dropped reservations can no longer be crossed.
func dropOtherPeriodReservations(s State, ruleID, periodID string) {
	for id, res := range s.Reservations {
		if res.RuleID == ruleID && res.PeriodID != periodID {
			delete(s.Reservations, id)
		}
	}
}

// firstCurrency picks the deterministically smallest currency in a period's
// estimates, so a currency-mismatch error does not depend on map order.
func firstCurrency(byCurrency map[string]EstimatedSpend) string {
	currencies := make([]string, 0, len(byCurrency))
	for currency := range byCurrency {
		currencies = append(currencies, currency)
	}
	sort.Strings(currencies)
	return currencies[0]
}

// cloneState deep-copies a state so Evaluate can never mutate its input, and
// normalizes the version so a zero value and a freshly decoded state behave
// identically.
func cloneState(s State) State {
	c := State{
		Version:      SchemaVersion,
		Rules:        make(map[string]RuleState, len(s.Rules)),
		Reservations: make(map[string]Reservation, len(s.Reservations)),
	}
	for id, rs := range s.Rules {
		c.Rules[id] = rs
	}
	for id, res := range s.Reservations {
		c.Reservations[id] = res
	}
	return c
}
