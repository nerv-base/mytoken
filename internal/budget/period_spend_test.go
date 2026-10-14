package budget_test

import (
	"errors"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/budget"
	"github.com/zzstar101/mytoken/internal/reporttime"
)

// A scope can carry a day, a week and a month estimate at the same instant, and
// every rule must see only its own period's amount. Keying spends by scope alone
// made the three estimates collide and made each rule read one shared amount.
func TestEvaluateRoutesSpendByScopeAndPeriod(t *testing.T) {
	dayRule := globalRule() // rule-1: global, day, USD, limit 10
	weekRule := globalRule()
	weekRule.ID = "rule-2"
	weekRule.Period = reporttime.Week
	weekRule.Limit = 100
	monthRule := globalRule()
	monthRule.ID = "rule-3"
	monthRule.Period = reporttime.Month
	monthRule.Limit = 1000

	in := input([]budget.Rule{monthRule, weekRule, dayRule}, []budget.EstimatedSpend{
		globalSpendIn(testMonthPeriod, 1050, 0), // 105%
		globalSpendIn(testWeekPeriod, 50, 0),    // 50%
		globalSpendIn(testDayPeriod, 9, 0),      // 90%
	})
	res := evalOK(t, budget.State{}, in)

	if len(res.Evaluations) != 3 {
		t.Fatalf("evaluations = %d, want 3", len(res.Evaluations))
	}
	want := map[string]struct {
		period    reporttime.Period
		estimated float64
		percent   float64
		crossed   []float64
	}{
		"rule-1": {testDayPeriod, 9, 90, []float64{80}},
		"rule-2": {testWeekPeriod, 50, 50, nil},
		"rule-3": {testMonthPeriod, 1050, 105, []float64{80, 100}},
	}
	for _, ev := range res.Evaluations {
		w := want[ev.RuleID]
		if !ev.DataAvailable || ev.Estimated != w.estimated || ev.Percent != w.percent || ev.PeriodID != w.period.ID() {
			t.Errorf("evaluation %s = available %v period %q estimated %v percent %v; want %v/%q/%v/%v",
				ev.RuleID, ev.DataAvailable, ev.PeriodID, ev.Estimated, ev.Percent, true, w.period.ID(), w.estimated, w.percent)
		}
		if len(ev.Crossed) != len(w.crossed) {
			t.Fatalf("rule %s crossed = %v, want %v", ev.RuleID, ev.Crossed, w.crossed)
		}
		for i, threshold := range w.crossed {
			if ev.Crossed[i] != threshold {
				t.Errorf("rule %s crossed = %v, want %v", ev.RuleID, ev.Crossed, w.crossed)
			}
		}
	}

	// Only the day and month rules cross: the week estimate is its own amount,
	// not the day's. Every notification carries its own period's amount.
	if len(res.Notifications) != 3 {
		t.Fatalf("notifications = %+v, want 3", res.Notifications)
	}
	byRule := map[string][]budget.Notification{}
	for _, n := range res.Notifications {
		byRule[n.RuleID] = append(byRule[n.RuleID], n)
	}
	if got := byRule["rule-2"]; len(got) != 0 {
		t.Errorf("week notifications = %+v, want none at 50%%", got)
	}
	for _, n := range byRule["rule-1"] {
		if n.Estimated != 9 || n.PeriodID != testDayPeriod.ID() {
			t.Errorf("day notification = %+v, want the day estimate 9", n)
		}
	}
	for _, n := range byRule["rule-3"] {
		if n.Estimated != 1050 || n.PeriodID != testMonthPeriod.ID() {
			t.Errorf("month notification = %+v, want the month estimate 1050", n)
		}
	}
}

// A spend for the same scope and currency but another period is not this rule's
// data: it is reported as missing, never reused as this period's amount and
// never downgraded to a priced zero.
func TestEvaluateOtherPeriodIsMissingNotZero(t *testing.T) {
	yesterday := mustPeriod(time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC), reporttime.Day, time.UTC)
	r := globalRule()

	res := evalOK(t, budget.State{}, input([]budget.Rule{r}, []budget.EstimatedSpend{
		globalSpendIn(yesterday, 9, 0),
		globalSpendIn(testWeekPeriod, 9, 0),
	}))
	ev := res.Evaluations[0]
	if ev.DataAvailable {
		t.Fatalf("DataAvailable = true: another period's spend was reused as the day rule's data (%+v)", ev)
	}
	if ev.SkippedReason != budget.SkipNoData || ev.Estimated != 0 || ev.Percent != 0 || ev.UnknownPricing {
		t.Fatalf("evaluation = %+v, want SkipNoData with no estimate and no unknown-pricing flag", ev)
	}
	if len(ev.Crossed) != 0 || len(res.Notifications) != 0 {
		t.Fatalf("crossed = %v, notifications = %+v; want none", ev.Crossed, res.Notifications)
	}
	if _, ok := res.State.Rules["rule-1"]; !ok {
		t.Error("rule generation was not tracked for a rule without data in this period")
	}

	// The rule's own period with a genuine priced zero is data, not missing.
	zero := evalOK(t, budget.State{}, input([]budget.Rule{r}, []budget.EstimatedSpend{globalSpendIn(testDayPeriod, 0, 0)}))
	ev0 := zero.Evaluations[0]
	if !ev0.DataAvailable || ev0.UnknownPricing || ev0.Estimated != 0 || ev0.Percent != 0 || ev0.SkippedReason != "" {
		t.Fatalf("priced zero evaluation = %+v, want available, not flagged, 0", ev0)
	}
	if len(zero.Notifications) != 0 {
		t.Fatalf("priced zero notifications = %+v, want none", zero.Notifications)
	}
}

// A rule matches only its own currency, and only for its own period. Currencies
// are never converted, summed or compared.
func TestEvaluatePeriodScopedCurrencyMatching(t *testing.T) {
	r := globalRule() // global, day, USD, limit 10

	t.Run("another currency in the rule's period is refused", func(t *testing.T) {
		evalErr(t, budget.State{}, input([]budget.Rule{r}, []budget.EstimatedSpend{
			spendOf(globalScope(), "EUR", testDayPeriod, 9, 0),
		}), budget.ErrCurrencyMismatch)
	})

	t.Run("the rule's own currency is used and the other is never mixed in", func(t *testing.T) {
		res := evalOK(t, budget.State{}, input([]budget.Rule{r}, []budget.EstimatedSpend{
			spendOf(globalScope(), "EUR", testDayPeriod, 1000, 0),
			globalSpendIn(testDayPeriod, 9, 0),
		}))
		ev := res.Evaluations[0]
		if ev.Currency != "USD" || ev.Estimated != 9 || ev.Percent != 90 {
			t.Fatalf("evaluation = %+v, want the USD 9 estimate", ev)
		}
		if len(res.Notifications) != 1 || res.Notifications[0].Estimated != 9 || res.Notifications[0].Currency != "USD" {
			t.Fatalf("notifications = %+v, want one USD 9 notification", res.Notifications)
		}
	})

	t.Run("another currency in another period is not this period's data", func(t *testing.T) {
		res := evalOK(t, budget.State{}, input([]budget.Rule{r}, []budget.EstimatedSpend{
			spendOf(globalScope(), "EUR", testWeekPeriod, 9, 0),
		}))
		ev := res.Evaluations[0]
		if ev.DataAvailable || ev.SkippedReason != budget.SkipNoData {
			t.Fatalf("evaluation = %+v, want SkipNoData for a wrong-period spend", ev)
		}
	})

	t.Run("the same scope, period and currency twice is a duplicate", func(t *testing.T) {
		evalErr(t, budget.State{}, input([]budget.Rule{r}, []budget.EstimatedSpend{
			globalSpendIn(testDayPeriod, 1, 0),
			globalSpendIn(testDayPeriod, 2, 0),
		}), budget.ErrDuplicateSpend)
	})

	t.Run("the same scope and period in two currencies is not a duplicate", func(t *testing.T) {
		res := evalOK(t, budget.State{}, input([]budget.Rule{r}, []budget.EstimatedSpend{
			spendOf(globalScope(), "USD", testDayPeriod, 1, 0),
			spendOf(globalScope(), "EUR", testDayPeriod, 2, 0),
		}))
		if len(res.Evaluations) != 1 || res.Evaluations[0].Estimated != 1 {
			t.Fatalf("evaluations = %+v, want the USD rule's own estimate", res.Evaluations)
		}
	})
}

// The spend carries the whole range, and that range must be a real calendar
// period of its kind: a forged, truncated, overlong or DST-ignoring window is
// refused rather than accepted as some period's data.
func TestEstimatedSpendRequiresACanonicalPeriod(t *testing.T) {
	ny := loadLoc(t, "America/New_York")
	dstDay := mustPeriod(time.Date(2026, 3, 8, 12, 0, 0, 0, ny), reporttime.Day, ny) // 23 hours

	bad := []struct {
		name   string
		period reporttime.Period
	}{
		{"zero period", reporttime.Period{}},
		{"zero start", reporttime.Period{Kind: reporttime.Day, End: testDayPeriod.End}},
		{"unknown kind", reporttime.Period{Kind: reporttime.Kind("year"), Start: testDayPeriod.Start, End: testDayPeriod.End}},
		{"reversed range", reporttime.Period{Kind: reporttime.Day, Start: testDayPeriod.End, End: testDayPeriod.Start}},
		{"start not a boundary", reporttime.Period{Kind: reporttime.Day, Start: testDayPeriod.Start.Add(12 * time.Hour), End: testDayPeriod.Start.Add(36 * time.Hour)}},
		{"truncated range", reporttime.Period{Kind: reporttime.Day, Start: testDayPeriod.Start, End: testDayPeriod.Start.Add(12 * time.Hour)}},
		{"overlong range", reporttime.Period{Kind: reporttime.Day, Start: testDayPeriod.Start, End: testDayPeriod.Start.Add(25 * time.Hour)}},
		{"day range labelled a week", reporttime.Period{Kind: reporttime.Week, Start: testDayPeriod.Start, End: testDayPeriod.End}},
		{"dst day forged as 24h", reporttime.Period{Kind: reporttime.Day, Start: dstDay.Start, End: dstDay.Start.Add(24 * time.Hour)}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			s := spendOf(globalScope(), "USD", tc.period, 1, 0)
			if err := s.Validate(); !errors.Is(err, budget.ErrInvalidPeriod) {
				t.Fatalf("Validate(%+v) = %v, want ErrInvalidPeriod", tc.period, err)
			}
		})
	}

	for _, period := range []reporttime.Period{testDayPeriod, testWeekPeriod, testMonthPeriod, dstDay} {
		s := spendOf(globalScope(), "USD", period, 1, 0)
		if err := s.Validate(); err != nil {
			t.Fatalf("Validate(%+v) = %v, want nil", period, err)
		}
		if got := s.PeriodID(); got != period.ID() {
			t.Fatalf("PeriodID() = %q, want reporttime's %q", got, period.ID())
		}
	}
}

// The same rejection must abort a whole evaluation, leaving the ledger untouched.
func TestEvaluateRejectsSpendWithForgedPeriod(t *testing.T) {
	forged := reporttime.Period{Kind: reporttime.Day, Start: testDayPeriod.Start, End: testDayPeriod.Start.Add(12 * time.Hour)}
	res, err := budget.Evaluate(budget.State{}, input([]budget.Rule{globalRule()}, []budget.EstimatedSpend{
		spendOf(globalScope(), "USD", forged, 9, 0),
	}))
	if !errors.Is(err, budget.ErrInvalidPeriod) {
		t.Fatalf("Evaluate() error = %v, want ErrInvalidPeriod", err)
	}
	if res.State.Reservations != nil || len(res.Evaluations) != 0 || len(res.Notifications) != 0 {
		t.Fatalf("rejected evaluation returned %+v, want the zero Result", res)
	}
}
