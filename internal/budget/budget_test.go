package budget_test

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
	_ "time/tzdata" // pin the IANA database so DST rules are stable

	"github.com/zzstar101/mytoken/internal/budget"
	"github.com/zzstar101/mytoken/internal/reporttime"
)

// testNow is a Monday noon in UTC: one instant that every period kind can
// classify without ambiguity.
var testNow = time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)

// testDayPeriod, testWeekPeriod and testMonthPeriod are the calendar periods of
// testNow in UTC. The tests resolve them through reporttime rather than
// hard-coding a range, exactly as an integration layer must.
var (
	testDayPeriod   = mustPeriod(testNow, reporttime.Day, time.UTC)
	testWeekPeriod  = mustPeriod(testNow, reporttime.Week, time.UTC)
	testMonthPeriod = mustPeriod(testNow, reporttime.Month, time.UTC)
)

// mustPeriod resolves the calendar period of kind containing now in loc. It is a
// test helper: the package itself never guesses or defaults a period.
func mustPeriod(now time.Time, kind reporttime.Kind, loc *time.Location) reporttime.Period {
	state, err := reporttime.Resolve(now, kind, loc)
	if err != nil {
		panic(fmt.Sprintf("reporttime.Resolve(%s, %s, %s): %v", now, kind, loc, err))
	}
	return state.Current
}

func globalScope() budget.Scope { return budget.Scope{Kind: budget.ScopeGlobal} }

func globalRule() budget.Rule {
	return budget.Rule{
		ID:       "rule-1",
		Version:  1,
		Scope:    globalScope(),
		Period:   reporttime.Day,
		Currency: "USD",
		Limit:    10,
		Label:    "daily",
	}
}

func spendOf(scope budget.Scope, currency string, period reporttime.Period, amount float64, unpriced int64) budget.EstimatedSpend {
	return budget.EstimatedSpend{Scope: scope, Period: period, Currency: currency, Amount: amount, UnpricedRequests: unpriced}
}

// globalSpend is the global scope's estimate for testNow's UTC day.
func globalSpend(amount float64, unpriced int64) budget.EstimatedSpend {
	return spendOf(globalScope(), "USD", testDayPeriod, amount, unpriced)
}

func globalSpendIn(period reporttime.Period, amount float64, unpriced int64) budget.EstimatedSpend {
	return spendOf(globalScope(), "USD", period, amount, unpriced)
}

// globalSpendOn is the global scope's estimate for the day containing now in loc.
func globalSpendOn(now time.Time, loc *time.Location, amount float64, unpriced int64) budget.EstimatedSpend {
	return globalSpendIn(mustPeriod(now, reporttime.Day, loc), amount, unpriced)
}

func inputAt(now time.Time, loc *time.Location, rules []budget.Rule, spends []budget.EstimatedSpend) budget.Input {
	return budget.Input{Now: now, Location: loc, Rules: rules, Spends: spends}
}

func input(rules []budget.Rule, spends []budget.EstimatedSpend) budget.Input {
	return inputAt(testNow, time.UTC, rules, spends)
}

func evalOK(t *testing.T, prev budget.State, in budget.Input) budget.Result {
	t.Helper()
	res, err := budget.Evaluate(prev, in)
	if err != nil {
		t.Fatalf("Evaluate() error = %v, want nil", err)
	}
	return res
}

func evalErr(t *testing.T, prev budget.State, in budget.Input, want error) {
	t.Helper()
	res, err := budget.Evaluate(prev, in)
	if !errors.Is(err, want) {
		t.Fatalf("Evaluate() error = %v (result %+v), want %v", err, res, want)
	}
}

func loadLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("time.LoadLocation(%q): %v", name, err)
	}
	return loc
}

func TestRuleValidateRejectsInvalidFields(t *testing.T) {
	valid := globalRule()
	mut := func(f func(*budget.Rule)) budget.Rule {
		r := valid
		f(&r)
		return r
	}
	cases := []struct {
		name string
		rule budget.Rule
		want error
	}{
		{"empty id", mut(func(r *budget.Rule) { r.ID = "" }), budget.ErrInvalidRule},
		{"blank id", mut(func(r *budget.Rule) { r.ID = "   " }), budget.ErrInvalidRule},
		{"zero version", mut(func(r *budget.Rule) { r.Version = 0 }), budget.ErrInvalidRule},
		{"negative version", mut(func(r *budget.Rule) { r.Version = -2 }), budget.ErrInvalidRule},
		{"unknown scope kind", mut(func(r *budget.Rule) { r.Scope = budget.Scope{Kind: "team"} }), budget.ErrInvalidScope},
		{"global with id", mut(func(r *budget.Rule) { r.Scope = budget.Scope{Kind: budget.ScopeGlobal, ID: "x"} }), budget.ErrInvalidScope},
		{"project without id", mut(func(r *budget.Rule) { r.Scope = budget.Scope{Kind: budget.ScopeProject} }), budget.ErrInvalidScope},
		{"blank project id", mut(func(r *budget.Rule) { r.Scope = budget.Scope{Kind: budget.ScopeProject, ID: "  "} }), budget.ErrInvalidScope},
		{"empty period", mut(func(r *budget.Rule) { r.Period = "" }), budget.ErrInvalidPeriod},
		{"uppercase period", mut(func(r *budget.Rule) { r.Period = "DAY" }), budget.ErrInvalidPeriod},
		{"empty currency", mut(func(r *budget.Rule) { r.Currency = "" }), budget.ErrInvalidCurrency},
		{"lowercase currency", mut(func(r *budget.Rule) { r.Currency = "usd" }), budget.ErrInvalidCurrency},
		{"short currency", mut(func(r *budget.Rule) { r.Currency = "US" }), budget.ErrInvalidCurrency},
		{"digit currency", mut(func(r *budget.Rule) { r.Currency = "US1" }), budget.ErrInvalidCurrency},
		{"zero limit", mut(func(r *budget.Rule) { r.Limit = 0 }), budget.ErrInvalidAmount},
		{"negative limit", mut(func(r *budget.Rule) { r.Limit = -1 }), budget.ErrInvalidAmount},
		{"nan limit", mut(func(r *budget.Rule) { r.Limit = math.NaN() }), budget.ErrInvalidAmount},
		{"positive inf limit", mut(func(r *budget.Rule) { r.Limit = math.Inf(1) }), budget.ErrInvalidAmount},
		{"negative inf limit", mut(func(r *budget.Rule) { r.Limit = math.Inf(-1) }), budget.ErrInvalidAmount},
		{"zero threshold", mut(func(r *budget.Rule) { r.Thresholds = []float64{0} }), budget.ErrInvalidThreshold},
		{"negative threshold", mut(func(r *budget.Rule) { r.Thresholds = []float64{-5} }), budget.ErrInvalidThreshold},
		{"nan threshold", mut(func(r *budget.Rule) { r.Thresholds = []float64{math.NaN()} }), budget.ErrInvalidThreshold},
		{"inf threshold", mut(func(r *budget.Rule) { r.Thresholds = []float64{math.Inf(1)} }), budget.ErrInvalidThreshold},
		{"threshold above 100", mut(func(r *budget.Rule) { r.Thresholds = []float64{100.5} }), budget.ErrInvalidThreshold},
		{"duplicate threshold", mut(func(r *budget.Rule) { r.Thresholds = []float64{80, 80} }), budget.ErrInvalidThreshold},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.rule.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}

	accepted := []struct {
		name string
		rule budget.Rule
	}{
		{"defaults", valid},
		{"project scope", mut(func(r *budget.Rule) { r.Scope = budget.Scope{Kind: budget.ScopeProject, ID: "proj-7"} })},
		{"provider scope", mut(func(r *budget.Rule) { r.Scope = budget.Scope{Kind: budget.ScopeProvider, ID: "openai"} })},
		{"week period", mut(func(r *budget.Rule) { r.Period = reporttime.Week })},
		{"month period", mut(func(r *budget.Rule) { r.Period = reporttime.Month })},
		{"disabled", mut(func(r *budget.Rule) { r.Disabled = true })},
		{"unsorted thresholds", mut(func(r *budget.Rule) { r.Thresholds = []float64{90, 50, 75} })},
		{"no label", mut(func(r *budget.Rule) { r.Label = "" })},
	}
	for _, tc := range accepted {
		t.Run("accept "+tc.name, func(t *testing.T) {
			if err := tc.rule.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestEstimatedSpendValidate(t *testing.T) {
	bad := []struct {
		name  string
		spend budget.EstimatedSpend
		want  error
	}{
		{"nan amount", globalSpend(math.NaN(), 0), budget.ErrInvalidAmount},
		{"inf amount", globalSpend(math.Inf(1), 0), budget.ErrInvalidAmount},
		{"negative amount", globalSpend(-0.01, 0), budget.ErrInvalidAmount},
		{"negative unpriced", globalSpend(1, -1), budget.ErrInvalidAmount},
		{"bad currency", spendOf(globalScope(), "usd", testDayPeriod, 1, 0), budget.ErrInvalidCurrency},
		{"bad scope", spendOf(budget.Scope{Kind: "team"}, "USD", testDayPeriod, 1, 0), budget.ErrInvalidScope},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.spend.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
	for _, ok := range []budget.EstimatedSpend{globalSpend(0, 0), globalSpend(1.5, 3)} {
		if err := ok.Validate(); err != nil {
			t.Fatalf("Validate(%+v) = %v, want nil", ok, err)
		}
	}
}

func TestScopeStringAndParseScopeKind(t *testing.T) {
	cases := []struct {
		scope budget.Scope
		want  string
	}{
		{budget.Scope{Kind: budget.ScopeGlobal}, "global"},
		{budget.Scope{Kind: budget.ScopeProject, ID: "proj-7"}, "project:proj-7"},
		{budget.Scope{Kind: budget.ScopeProvider, ID: "openai"}, "provider:openai"},
	}
	for _, tc := range cases {
		if got := tc.scope.String(); got != tc.want {
			t.Errorf("Scope.String() = %q, want %q", got, tc.want)
		}
	}
	if k, err := budget.ParseScopeKind(" Provider "); err != nil || k != budget.ScopeProvider {
		t.Errorf("ParseScopeKind(Provider) = %q, %v; want provider, nil", k, err)
	}
	if _, err := budget.ParseScopeKind("team"); !errors.Is(err, budget.ErrInvalidScope) {
		t.Errorf("ParseScopeKind(team) error = %v, want ErrInvalidScope", err)
	}
}

func TestParseCurrency(t *testing.T) {
	for _, in := range []string{"USD", "cny", " Eur "} {
		got, err := budget.ParseCurrency(in)
		if err != nil {
			t.Fatalf("ParseCurrency(%q) error = %v", in, err)
		}
		if got != strings.ToUpper(strings.TrimSpace(in)) {
			t.Errorf("ParseCurrency(%q) = %q, want canonical uppercase", in, got)
		}
		if !budget.ValidCurrency(got) {
			t.Errorf("ValidCurrency(%q) = false after ParseCurrency accepted it", got)
		}
	}
	for _, in := range []string{"", "US", "US1", "US$", "US D"} {
		if _, err := budget.ParseCurrency(in); !errors.Is(err, budget.ErrInvalidCurrency) {
			t.Errorf("ParseCurrency(%q) error = %v, want ErrInvalidCurrency", in, err)
		}
		if budget.ValidCurrency(in) {
			t.Errorf("ValidCurrency(%q) = true, want false", in)
		}
	}
}

func TestEvaluateRejectsInvalidInput(t *testing.T) {
	r := globalRule()
	t.Run("nil location", func(t *testing.T) {
		evalErr(t, budget.State{}, inputAt(testNow, nil, []budget.Rule{r}, nil), budget.ErrInvalidInput)
	})
	t.Run("zero now", func(t *testing.T) {
		evalErr(t, budget.State{}, inputAt(time.Time{}, time.UTC, []budget.Rule{r}, nil), budget.ErrInvalidInput)
	})
	t.Run("duplicate rule id", func(t *testing.T) {
		r2 := globalRule()
		r2.Version = 2
		evalErr(t, budget.State{}, input([]budget.Rule{r, r2}, nil), budget.ErrDuplicateRule)
	})
	t.Run("duplicate spend scope", func(t *testing.T) {
		evalErr(t, budget.State{}, input([]budget.Rule{r}, []budget.EstimatedSpend{globalSpend(1, 0), globalSpend(2, 0)}), budget.ErrDuplicateSpend)
	})
	t.Run("invalid rule in list", func(t *testing.T) {
		bad := globalRule()
		bad.Currency = "usd"
		evalErr(t, budget.State{}, input([]budget.Rule{bad}, nil), budget.ErrInvalidRule)
	})
	t.Run("invalid spend in list", func(t *testing.T) {
		evalErr(t, budget.State{}, input([]budget.Rule{r}, []budget.EstimatedSpend{globalSpend(math.NaN(), 0)}), budget.ErrInvalidAmount)
	})
	t.Run("future state version", func(t *testing.T) {
		evalErr(t, budget.State{Version: budget.SchemaVersion + 1}, input([]budget.Rule{r}, nil), budget.ErrStateVersion)
	})
}

// A threshold fires exactly once per rule generation, period and threshold:
// crossing 80%, re-reporting 80%, crossing 100%, and reporting above 100% all
// reserve at most one notification each.
func TestEvaluateCrossingFiresOncePerRulePeriodThreshold(t *testing.T) {
	in80 := input([]budget.Rule{globalRule()}, []budget.EstimatedSpend{globalSpend(8.5, 0)})
	first := evalOK(t, budget.State{}, in80)
	if len(first.Notifications) != 1 {
		t.Fatalf("first notifications = %d, want 1 (%+v)", len(first.Notifications), first.Notifications)
	}
	n := first.Notifications[0]
	if n.Threshold != 80 || n.Percent != 85 || n.Estimated != 8.5 || n.Limit != 10 || n.Currency != "USD" {
		t.Errorf("notification = %+v, want threshold 80, percent 85, USD 8.5/10", n)
	}
	if n.RuleID != "rule-1" || n.RuleVersion != 1 || n.PeriodID == "" {
		t.Errorf("notification identity = %+v, want rule-1 v1 with a period id", n)
	}
	if !n.ReservedAt.Equal(testNow) {
		t.Errorf("ReservedAt = %s, want %s", n.ReservedAt, testNow)
	}
	if n.UnknownPricing {
		t.Error("notification UnknownPricing = true, want false when every request is priced")
	}
	ev := first.Evaluations[0]
	if !ev.DataAvailable || ev.Percent != 85 || len(ev.Crossed) != 1 || ev.Crossed[0] != 80 {
		t.Errorf("evaluation = %+v, want available, 85%%, crossed [80]", ev)
	}

	repeat := evalOK(t, first.State, in80)
	if len(repeat.Notifications) != 0 {
		t.Fatalf("repeat notifications = %d, want 0", len(repeat.Notifications))
	}
	if len(repeat.State.Reservations) != 1 {
		t.Fatalf("reservations = %d, want 1", len(repeat.State.Reservations))
	}

	in100 := input([]budget.Rule{globalRule()}, []budget.EstimatedSpend{globalSpend(10.5, 0)})
	second := evalOK(t, repeat.State, in100)
	if len(second.Notifications) != 1 || second.Notifications[0].Threshold != 100 {
		t.Fatalf("100%% notifications = %+v, want exactly the 100 threshold", second.Notifications)
	}
	third := evalOK(t, second.State, in100)
	if len(third.Notifications) != 0 {
		t.Fatalf("repeated 100%% notifications = %d, want 0", len(third.Notifications))
	}

	in150 := input([]budget.Rule{globalRule()}, []budget.EstimatedSpend{globalSpend(15, 0)})
	fourth := evalOK(t, third.State, in150)
	if len(fourth.Notifications) != 0 {
		t.Fatalf("above-100%% notifications = %d, want 0 (both already reserved)", len(fourth.Notifications))
	}
	if got := fourth.Evaluations[0]; got.Percent != 150 || len(got.Crossed) != 2 || got.Crossed[0] != 80 || got.Crossed[1] != 100 {
		t.Errorf("above-100%% evaluation = %+v, want percent 150 and crossed [80 100]", got)
	}
	if len(fourth.State.Reservations) != 2 {
		t.Errorf("reservations = %d, want 2", len(fourth.State.Reservations))
	}
}

func TestEvaluateExactThresholdBoundary(t *testing.T) {
	cases := []struct {
		name     string
		amount   float64
		wantFire []float64
	}{
		{"just below 80", 7.999, nil},
		{"exactly 80", 8, []float64{80}},
		{"just below 100", 9.999, []float64{80}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := evalOK(t, budget.State{}, input([]budget.Rule{globalRule()}, []budget.EstimatedSpend{globalSpend(tc.amount, 0)}))
			if len(res.Notifications) != len(tc.wantFire) {
				t.Fatalf("notifications = %+v, want thresholds %v", res.Notifications, tc.wantFire)
			}
			for i, want := range tc.wantFire {
				if res.Notifications[i].Threshold != want {
					t.Errorf("notification[%d].Threshold = %v, want %v", i, res.Notifications[i].Threshold, want)
				}
			}
		})
	}
	t.Run("exactly 100 crosses both", func(t *testing.T) {
		res := evalOK(t, budget.State{}, input([]budget.Rule{globalRule()}, []budget.EstimatedSpend{globalSpend(10, 0)}))
		if len(res.Notifications) != 2 {
			t.Fatalf("notifications = %+v, want both thresholds", res.Notifications)
		}
		if res.Notifications[0].Threshold != 80 || res.Notifications[1].Threshold != 100 {
			t.Errorf("thresholds = [%v %v], want ascending [80 100]", res.Notifications[0].Threshold, res.Notifications[1].Threshold)
		}
	})
}

func TestEvaluateUnpricedRequestsAreFlagged(t *testing.T) {
	res := evalOK(t, budget.State{}, input([]budget.Rule{globalRule()}, []budget.EstimatedSpend{globalSpend(9, 4)}))
	ev := res.Evaluations[0]
	if !ev.UnknownPricing || ev.UnpricedRequests != 4 {
		t.Errorf("evaluation = %+v, want UnknownPricing with 4 unpriced requests", ev)
	}
	if len(res.Notifications) != 1 || !res.Notifications[0].UnknownPricing {
		t.Errorf("notifications = %+v, want one notification flagged as unknown pricing", res.Notifications)
	}

	// A partial estimate is a lower bound, so an all-unpriced period reports
	// zero percent and still flags the gap instead of pretending the cost is 0.
	res2 := evalOK(t, budget.State{}, input([]budget.Rule{globalRule()}, []budget.EstimatedSpend{globalSpend(0, 5)}))
	ev2 := res2.Evaluations[0]
	if !ev2.DataAvailable || ev2.Percent != 0 || !ev2.UnknownPricing || ev2.UnpricedRequests != 5 {
		t.Errorf("all-unpriced evaluation = %+v, want available, 0%%, flagged, 5 unpriced", ev2)
	}
	if len(res2.Notifications) != 0 {
		t.Errorf("all-unpriced notifications = %+v, want none", res2.Notifications)
	}

	// A genuine zero with full pricing is data, not an unknown.
	res3 := evalOK(t, budget.State{}, input([]budget.Rule{globalRule()}, []budget.EstimatedSpend{globalSpend(0, 0)}))
	if ev3 := res3.Evaluations[0]; !ev3.DataAvailable || ev3.UnknownPricing {
		t.Errorf("priced zero evaluation = %+v, want available and not flagged", ev3)
	}
}

func TestEvaluateMissingSpendIsNotFakeZero(t *testing.T) {
	// The only spend is for a different scope: the global rule has no data and
	// must not report 0%.
	in := input([]budget.Rule{globalRule()}, []budget.EstimatedSpend{
		spendOf(budget.Scope{Kind: budget.ScopeProvider, ID: "openai"}, "USD", testDayPeriod, 9, 0),
	})
	res := evalOK(t, budget.State{}, in)
	ev := res.Evaluations[0]
	if ev.DataAvailable {
		t.Errorf("DataAvailable = true, want false when the rule's scope has no spend")
	}
	if ev.SkippedReason != budget.SkipNoData {
		t.Errorf("SkippedReason = %q, want %q", ev.SkippedReason, budget.SkipNoData)
	}
	if ev.Percent != 0 || len(ev.Crossed) != 0 || len(res.Notifications) != 0 {
		t.Errorf("evaluation = %+v, notifications = %+v; want no crossing and no notification", ev, res.Notifications)
	}
	if _, ok := res.State.Rules["rule-1"]; !ok {
		t.Error("rule generation was not tracked for a rule without spend data")
	}
}

func TestEvaluateDisabledRule(t *testing.T) {
	disabled := globalRule()
	disabled.Disabled = true
	res := evalOK(t, budget.State{}, input([]budget.Rule{disabled}, []budget.EstimatedSpend{globalSpend(9, 0)}))
	if len(res.Notifications) != 0 {
		t.Fatalf("disabled notifications = %+v, want none", res.Notifications)
	}
	if res.Evaluations[0].SkippedReason != budget.SkipDisabled {
		t.Errorf("SkippedReason = %q, want %q", res.Evaluations[0].SkippedReason, budget.SkipDisabled)
	}

	// Disabling after a threshold fired keeps the reservation: re-enabling in
	// the same period must not re-notify.
	fired := evalOK(t, budget.State{}, input([]budget.Rule{globalRule()}, []budget.EstimatedSpend{globalSpend(9, 0)}))
	if len(fired.Notifications) != 1 {
		t.Fatalf("setup notifications = %d, want 1", len(fired.Notifications))
	}
	off := evalOK(t, fired.State, input([]budget.Rule{disabled}, []budget.EstimatedSpend{globalSpend(9, 0)}))
	if len(off.Notifications) != 0 || len(off.State.Reservations) != 1 {
		t.Fatalf("disabled state = %d notifications, %d reservations; want 0 and 1", len(off.Notifications), len(off.State.Reservations))
	}
	on := evalOK(t, off.State, input([]budget.Rule{globalRule()}, []budget.EstimatedSpend{globalSpend(9, 0)}))
	if len(on.Notifications) != 0 {
		t.Fatalf("re-enabled notifications = %+v, want none (reservation survives)", on.Notifications)
	}
}

// A display-label edit is not an identity edit: it must not reset dedup.
func TestLabelEditDoesNotResetDedup(t *testing.T) {
	fired := evalOK(t, budget.State{}, input([]budget.Rule{globalRule()}, []budget.EstimatedSpend{globalSpend(9, 0)}))
	renamed := globalRule()
	renamed.Label = "totally different display name"
	res := evalOK(t, fired.State, input([]budget.Rule{renamed}, []budget.EstimatedSpend{globalSpend(9, 0)}))
	if len(res.Notifications) != 0 {
		t.Fatalf("label edit produced notifications %+v, want none", res.Notifications)
	}
	if len(res.State.Reservations) != 1 {
		t.Fatalf("reservations = %d, want 1", len(res.State.Reservations))
	}
	if budget.NotificationID(globalRule(), "period:v1:day:UTC:2026-03-09T00:00:00Z", 80) !=
		budget.NotificationID(renamed, "period:v1:day:UTC:2026-03-09T00:00:00Z", 80) {
		t.Error("label changed the deterministic notification id")
	}
}

// Editing an identity-bearing field without bumping Version is refused; an
// explicit new Version is a new generation whose old reservations are pruned.
func TestRuleEditRequiresExplicitVersion(t *testing.T) {
	fired := evalOK(t, budget.State{}, input([]budget.Rule{globalRule()}, []budget.EstimatedSpend{globalSpend(8.5, 0)}))
	if len(fired.Notifications) != 1 {
		t.Fatalf("setup notifications = %d, want 1", len(fired.Notifications))
	}

	changed := globalRule()
	changed.Limit = 20 // same Version, different meaning
	evalErr(t, fired.State, input([]budget.Rule{changed}, []budget.EstimatedSpend{globalSpend(8.5, 0)}), budget.ErrRuleVersionConflict)

	changedScope := globalRule()
	changedScope.Currency = "EUR"
	evalErr(t, fired.State, input([]budget.Rule{changedScope}, []budget.EstimatedSpend{spendOf(globalScope(), "EUR", testDayPeriod, 8.5, 0)}), budget.ErrRuleVersionConflict)

	// A later version is accepted, fires again under the new generation, and
	// drops the previous generation's reservations. The spend has to reach the
	// new limit's threshold: 17 of 20 is 85%.
	v2 := globalRule()
	v2.Version = 2
	v2.Limit = 20
	res := evalOK(t, fired.State, input([]budget.Rule{v2}, []budget.EstimatedSpend{globalSpend(17, 0)}))
	if len(res.Notifications) != 1 || res.Notifications[0].RuleVersion != 2 || res.Notifications[0].Limit != 20 {
		t.Fatalf("v2 notifications = %+v, want one v2 notification with the new limit", res.Notifications)
	}
	if len(res.State.Reservations) != 1 {
		t.Fatalf("reservations = %d, want only the v2 generation", len(res.State.Reservations))
	}
	for _, r := range res.State.Reservations {
		if r.RuleVersion != 2 {
			t.Errorf("stale reservation kept: %+v", r)
		}
	}
}

func TestEvaluateNeverCombinesCurrencies(t *testing.T) {
	t.Run("mismatched spend currency is refused", func(t *testing.T) {
		in := input([]budget.Rule{globalRule()}, []budget.EstimatedSpend{spendOf(globalScope(), "EUR", testDayPeriod, 9, 0)})
		evalErr(t, budget.State{}, in, budget.ErrCurrencyMismatch)
	})
	t.Run("separate currencies stay separate", func(t *testing.T) {
		usd := globalRule()
		cny := globalRule()
		cny.ID = "rule-2"
		cny.Currency = "CNY"
		cny.Limit = 100
		cny.Scope = budget.Scope{Kind: budget.ScopeProvider, ID: "deepseek"}
		in := input([]budget.Rule{usd, cny}, []budget.EstimatedSpend{
			globalSpend(9, 0),
			spendOf(cny.Scope, "CNY", testDayPeriod, 90, 0),
		})
		res := evalOK(t, budget.State{}, in)
		if len(res.Notifications) != 2 {
			t.Fatalf("notifications = %+v, want one per rule", res.Notifications)
		}
		byRule := map[string]budget.Notification{}
		for _, n := range res.Notifications {
			byRule[n.RuleID] = n
		}
		if n := byRule["rule-1"]; n.Currency != "USD" || n.Estimated != 9 || n.Limit != 10 {
			t.Errorf("USD notification = %+v", n)
		}
		if n := byRule["rule-2"]; n.Currency != "CNY" || n.Estimated != 90 || n.Limit != 100 {
			t.Errorf("CNY notification = %+v", n)
		}
	})
}

func TestEvaluateDefaultAndCustomThresholds(t *testing.T) {
	if got := budget.DefaultThresholds(); len(got) != 2 || got[0] != 80 || got[1] != 100 {
		t.Fatalf("DefaultThresholds() = %v, want [80 100]", got)
	}
	r := globalRule()
	r.Thresholds = []float64{50}
	res := evalOK(t, budget.State{}, input([]budget.Rule{r}, []budget.EstimatedSpend{globalSpend(6, 0)}))
	if len(res.Notifications) != 1 || res.Notifications[0].Threshold != 50 {
		t.Fatalf("notifications = %+v, want only the configured 50 threshold", res.Notifications)
	}
	// The default 80 threshold is not configured on this rule.
	res2 := evalOK(t, res.State, input([]budget.Rule{r}, []budget.EstimatedSpend{globalSpend(8, 0)}))
	if len(res2.Notifications) != 0 {
		t.Fatalf("notifications = %+v, want none (80 not configured)", res2.Notifications)
	}
}

func TestEvaluatePeriodBoundariesFollowReporttime(t *testing.T) {
	ny := loadLoc(t, "America/New_York")
	cases := []struct {
		name string
		kind reporttime.Kind
		now  time.Time
		loc  *time.Location
	}{
		{"day before boundary", reporttime.Day, time.Date(2026, 3, 8, 23, 59, 59, 0, time.UTC), time.UTC},
		{"day at boundary", reporttime.Day, time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC), time.UTC},
		{"week sunday", reporttime.Week, time.Date(2026, 3, 8, 23, 59, 59, 0, time.UTC), time.UTC},
		{"week monday", reporttime.Week, time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC), time.UTC},
		{"month end", reporttime.Month, time.Date(2026, 2, 28, 23, 59, 59, 0, time.UTC), time.UTC},
		{"month start", reporttime.Month, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.UTC},
		{"ny dst inside day", reporttime.Day, time.Date(2026, 3, 8, 12, 0, 0, 0, ny), ny},
		{"ny dst at boundary", reporttime.Day, time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC), ny},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := globalRule()
			r.Period = tc.kind
			res := evalOK(t, budget.State{}, inputAt(tc.now, tc.loc, []budget.Rule{r}, []budget.EstimatedSpend{globalSpendOn(tc.now, tc.loc, 9, 0)}))
			want, err := reporttime.Resolve(tc.now, tc.kind, tc.loc)
			if err != nil {
				t.Fatalf("reporttime.Resolve: %v", err)
			}
			if got := res.Evaluations[0].PeriodID; got != want.Current.ID() {
				t.Errorf("PeriodID = %q, want reporttime's %q", got, want.Current.ID())
			}
		})
	}

	// Monday weeks: Sunday 2026-03-08 belongs to the week starting 2026-03-02,
	// Monday 2026-03-09 to the week starting 2026-03-09.
	r := globalRule()
	r.Period = reporttime.Week
	sundayAt := time.Date(2026, 3, 8, 23, 59, 59, 0, time.UTC)
	mondayAt := time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)
	sunday := evalOK(t, budget.State{}, inputAt(sundayAt, time.UTC, []budget.Rule{r}, []budget.EstimatedSpend{globalSpendOn(sundayAt, time.UTC, 9, 0)}))
	monday := evalOK(t, budget.State{}, inputAt(mondayAt, time.UTC, []budget.Rule{r}, []budget.EstimatedSpend{globalSpendOn(mondayAt, time.UTC, 9, 0)}))
	if sunday.Evaluations[0].PeriodID == monday.Evaluations[0].PeriodID {
		t.Fatal("Sunday and Monday landed in the same week period")
	}
	wantSun, _ := reporttime.Resolve(sundayAt, reporttime.Week, time.UTC)
	if !wantSun.Current.Start.Equal(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("reporttime week for Sunday starts at %s, want 2026-03-02", wantSun.Current.Start)
	}
}

func TestEvaluateDSTPeriodsUseReporttimeIdentity(t *testing.T) {
	ny := loadLoc(t, "America/New_York")
	noon := time.Date(2026, 3, 8, 12, 0, 0, 0, ny)

	// Sanity: the spring-forward day really is 23 hours, so a fixed 24h window
	// would be wrong and the budget core must key on reporttime instead.
	want, err := reporttime.Resolve(noon, reporttime.Day, ny)
	if err != nil {
		t.Fatalf("reporttime.Resolve: %v", err)
	}
	if got := want.Current.Window().Duration(); got != 23*time.Hour {
		t.Fatalf("DST day duration = %s, want 23h", got)
	}

	r := globalRule()
	res := evalOK(t, budget.State{}, inputAt(noon, ny, []budget.Rule{r}, []budget.EstimatedSpend{globalSpendOn(noon, ny, 9, 0)}))
	if got := res.Evaluations[0].PeriodID; got != want.Current.ID() {
		t.Fatalf("PeriodID = %q, want %q", got, want.Current.ID())
	}

	// The last instant before the local boundary is the previous day; the
	// boundary instant opens the DST day.
	before := time.Date(2026, 3, 8, 4, 59, 59, 0, time.UTC)
	at := time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC)
	prevRes := evalOK(t, budget.State{}, inputAt(before, ny, []budget.Rule{r}, []budget.EstimatedSpend{globalSpendOn(before, ny, 9, 0)}))
	atRes := evalOK(t, prevRes.State, inputAt(at, ny, []budget.Rule{r}, []budget.EstimatedSpend{globalSpendOn(at, ny, 9, 0)}))
	if prevRes.Evaluations[0].PeriodID == atRes.Evaluations[0].PeriodID {
		t.Fatal("instants either side of the DST day boundary share a period")
	}
	if len(atRes.Notifications) != 1 {
		t.Fatalf("notifications after the boundary = %+v, want a new period's notification", atRes.Notifications)
	}
	// The ledger is bounded to the current period of each rule, so opening the
	// DST day prunes the previous day's reservation.
	if len(atRes.State.Reservations) != 1 {
		t.Fatalf("reservations = %d, want only the DST day's reservation", len(atRes.State.Reservations))
	}
	for _, res := range atRes.State.Reservations {
		if res.PeriodID != atRes.Evaluations[0].PeriodID {
			t.Errorf("kept reservation for %q, want %q", res.PeriodID, atRes.Evaluations[0].PeriodID)
		}
	}
}

func TestEvaluateTimezoneIsExplicit(t *testing.T) {
	ny := loadLoc(t, "America/New_York")
	r := globalRule()
	utcSpend := globalSpendOn(testNow, time.UTC, 9, 0)
	nySpend := globalSpendOn(testNow, ny, 9, 0)
	utcRes := evalOK(t, budget.State{}, inputAt(testNow, time.UTC, []budget.Rule{r}, []budget.EstimatedSpend{utcSpend}))
	nyRes := evalOK(t, utcRes.State, inputAt(testNow, ny, []budget.Rule{r}, []budget.EstimatedSpend{utcSpend, nySpend}))
	if utcRes.Evaluations[0].PeriodID == nyRes.Evaluations[0].PeriodID {
		t.Fatal("UTC and New York produced the same period identity")
	}
	// Same instant (12:00Z is 08:00 in New York) so both are inside one local
	// day each; separate identities mean the New York evaluation reserves its
	// own notification instead of reusing the UTC period's reservation. Only
	// the current period's reservation survives, because the ledger is pruned
	// per rule.
	if len(nyRes.Notifications) != 1 {
		t.Fatalf("notifications = %d, want 1 for the New York period", len(nyRes.Notifications))
	}
	if len(nyRes.State.Reservations) != 1 {
		t.Fatalf("reservations = %d, want only the current period's", len(nyRes.State.Reservations))
	}
}

func TestEvaluatePeriodRolloverPrunesOldPeriodReservations(t *testing.T) {
	r := globalRule()
	day1 := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	first := evalOK(t, budget.State{}, inputAt(day1, time.UTC, []budget.Rule{r}, []budget.EstimatedSpend{globalSpendOn(day1, time.UTC, 9, 0)}))
	if len(first.Notifications) != 1 {
		t.Fatalf("day 1 notifications = %+v, want 1", first.Notifications)
	}
	second := evalOK(t, first.State, inputAt(day2, time.UTC, []budget.Rule{r}, []budget.EstimatedSpend{globalSpendOn(day2, time.UTC, 9, 0)}))
	if len(second.Notifications) != 1 {
		t.Fatalf("day 2 notifications = %+v, want 1", second.Notifications)
	}
	if len(second.State.Reservations) != 1 {
		t.Fatalf("reservations = %d, want the old period pruned", len(second.State.Reservations))
	}
	for _, res := range second.State.Reservations {
		if res.PeriodID != second.Evaluations[0].PeriodID {
			t.Errorf("kept reservation for %q, want %q", res.PeriodID, second.Evaluations[0].PeriodID)
		}
	}
}

func TestNotificationIDIsDeterministic(t *testing.T) {
	r := globalRule()
	pid := "period:v1:day:UTC:2026-03-09T00:00:00Z"
	got := budget.NotificationID(r, pid, 80)
	want := "budget:v1:rule-1:1:global:period:v1:day:UTC:2026-03-09T00:00:00Z:80"
	if got != want {
		t.Fatalf("NotificationID() = %q, want %q", got, want)
	}
	if again := budget.NotificationID(r, pid, 80); again != got {
		t.Fatalf("NotificationID() is not deterministic: %q then %q", got, again)
	}
	changed := r
	changed.Version = 2
	if budget.NotificationID(changed, pid, 80) == got {
		t.Error("a new rule version reused the same notification id")
	}
	changed = r
	changed.Scope = budget.Scope{Kind: budget.ScopeProvider, ID: "openai"}
	if budget.NotificationID(changed, pid, 80) == got {
		t.Error("a different scope reused the same notification id")
	}
	if budget.NotificationID(r, "period:v1:week:UTC:2026-03-09T00:00:00Z", 80) == got {
		t.Error("a different period reused the same notification id")
	}
	if budget.NotificationID(r, pid, 100) == got {
		t.Error("a different threshold reused the same notification id")
	}
}

func TestEvaluateOutputIsDeterministicAndSorted(t *testing.T) {
	// Rules and spends deliberately arrive in the wrong order.
	ruleB := globalRule()
	ruleB.ID = "rule-b"
	ruleB.Scope = budget.Scope{Kind: budget.ScopeProvider, ID: "openai"}
	ruleA := globalRule()
	ruleA.ID = "rule-a"

	in := input([]budget.Rule{ruleB, ruleA}, []budget.EstimatedSpend{
		spendOf(ruleB.Scope, "USD", testDayPeriod, 9, 0),
		globalSpend(9, 0),
	})
	res := evalOK(t, budget.State{}, in)
	if len(res.Evaluations) != 2 {
		t.Fatalf("evaluations = %d, want 2", len(res.Evaluations))
	}
	if res.Evaluations[0].RuleID != "rule-a" || res.Evaluations[1].RuleID != "rule-b" {
		t.Errorf("evaluation order = [%s %s], want sorted by rule id", res.Evaluations[0].RuleID, res.Evaluations[1].RuleID)
	}
	for i := 1; i < len(res.Notifications); i++ {
		if res.Notifications[i-1].ID >= res.Notifications[i].ID {
			t.Errorf("notifications not sorted: %q then %q", res.Notifications[i-1].ID, res.Notifications[i].ID)
		}
	}
}

func TestEvaluateDoesNotMutatePreviousState(t *testing.T) {
	fired := evalOK(t, budget.State{}, input([]budget.Rule{globalRule()}, []budget.EstimatedSpend{globalSpend(8.5, 0)}))
	if len(fired.State.Reservations) != 1 {
		t.Fatalf("setup reservations = %d, want 1", len(fired.State.Reservations))
	}

	// A new generation prunes the old ledger in the returned state; the state
	// that was passed in must be untouched.
	v2 := globalRule()
	v2.Version = 2
	res := evalOK(t, fired.State, input([]budget.Rule{v2}, []budget.EstimatedSpend{globalSpend(8.5, 0)}))
	if len(res.State.Reservations) != 1 {
		t.Fatalf("returned reservations = %d, want the v2 generation only", len(res.State.Reservations))
	}
	if len(fired.State.Reservations) != 1 {
		t.Fatalf("previous state reservations = %d, want it unchanged", len(fired.State.Reservations))
	}
	for id, r := range fired.State.Reservations {
		if r.RuleVersion != 1 {
			t.Errorf("previous state reservation %q mutated to %+v", id, r)
		}
	}
}

func TestEvaluateIsRaceFree(t *testing.T) {
	in := input([]budget.Rule{globalRule()}, []budget.EstimatedSpend{globalSpend(9, 0)})
	first := evalOK(t, budget.State{}, in)

	const workers = 32
	results := make([]budget.Result, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// One shared previous state value: Evaluate must copy before mutating.
			results[i], errs[i] = budget.Evaluate(first.State, in)
		}(i)
	}
	wg.Wait()
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d error = %v", i, errs[i])
		}
		if len(results[i].Notifications) != 0 {
			t.Fatalf("worker %d notifications = %+v, want none on replay", i, results[i].Notifications)
		}
	}
	if len(first.State.Reservations) != 1 {
		t.Fatalf("shared previous state reservations = %d, want 1", len(first.State.Reservations))
	}
}
