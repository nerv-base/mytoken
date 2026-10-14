package budget_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/budget"
	"github.com/zzstar101/mytoken/internal/reporttime"
)

// The ledger is pruned to each rule's current period on every evaluation, so the
// claim that an earlier period "can never be crossed again" only holds while the
// evaluation clock moves forward. A wall-clock rollback must therefore be
// refused, with the ledger left byte-for-byte untouched.
func TestEvaluateRefusesRegressedEvaluationAfterRollover(t *testing.T) {
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
		t.Fatalf("day 2 reservations = %d, want day 1 pruned", len(second.State.Reservations))
	}

	before, err := budget.EncodeState(second.State)
	if err != nil {
		t.Fatalf("EncodeState: %v", err)
	}
	res, err := budget.Evaluate(second.State, inputAt(day1, time.UTC, []budget.Rule{r}, []budget.EstimatedSpend{globalSpendOn(day1, time.UTC, 9, 0)}))
	if !errors.Is(err, budget.ErrEvaluationRegressed) {
		t.Fatalf("rolled-back Evaluate() error = %v, want ErrEvaluationRegressed", err)
	}
	if res.State.Reservations != nil || len(res.Evaluations) != 0 || len(res.Notifications) != 0 {
		t.Fatalf("refused evaluation returned %+v, want the zero Result", res)
	}
	after, err := budget.EncodeState(second.State)
	if err != nil {
		t.Fatalf("EncodeState: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("refused evaluation mutated the ledger:\nbefore %s\nafter  %s", before, after)
	}
	if len(second.State.Reservations) != 1 {
		t.Fatalf("ledger reservations = %d, want day 2's kept", len(second.State.Reservations))
	}

	// A refusal must not wedge the ledger: forward progress still works.
	day3 := time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC)
	third := evalOK(t, second.State, inputAt(day3, time.UTC, []budget.Rule{r}, []budget.EstimatedSpend{globalSpendOn(day3, time.UTC, 9, 0)}))
	if len(third.Notifications) != 1 {
		t.Fatalf("day 3 notifications = %+v, want forward progress to keep notifying", third.Notifications)
	}
}

// The watermark is persisted, so a restart cannot re-open an earlier period:
// replaying the current period stays idempotent, but once a later period has
// been evaluated the earlier one is refused for good.
func TestEvaluationWatermarkSurvivesSerializedRestart(t *testing.T) {
	r := globalRule()
	day1 := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	day1In := inputAt(day1, time.UTC, []budget.Rule{r}, []budget.EstimatedSpend{globalSpendOn(day1, time.UTC, 9, 0)})

	first := evalOK(t, budget.State{}, day1In)
	raw, err := budget.EncodeState(first.State)
	if err != nil {
		t.Fatalf("EncodeState: %v", err)
	}
	restored, err := budget.DecodeState(raw)
	if err != nil {
		t.Fatalf("DecodeState: %v", err)
	}
	rs, ok := restored.Rules[r.ID]
	if !ok {
		t.Fatalf("restored rules = %+v, want %s", restored.Rules, r.ID)
	}
	if want := mustPeriod(day1, reporttime.Day, time.UTC).Start; !rs.Watermark.Equal(want) {
		t.Fatalf("restored watermark = %s, want %s", rs.Watermark, want)
	}

	// Replaying the current period after the restart is idempotent, not a
	// regression: its reservation came back with the document.
	replay := evalOK(t, restored, day1In)
	if len(replay.Notifications) != 0 {
		t.Fatalf("post-restart replay notifications = %+v, want none", replay.Notifications)
	}

	// Crossing into the next period after the restart advances the watermark.
	rolled := evalOK(t, restored, inputAt(day2, time.UTC, []budget.Rule{r}, []budget.EstimatedSpend{globalSpendOn(day2, time.UTC, 9, 0)}))
	if len(rolled.Notifications) != 1 {
		t.Fatalf("day 2 notifications = %+v, want 1", rolled.Notifications)
	}
	if want := mustPeriod(day2, reporttime.Day, time.UTC).Start; !rolled.State.Rules[r.ID].Watermark.Equal(want) {
		t.Fatalf("advanced watermark = %s, want %s", rolled.State.Rules[r.ID].Watermark, want)
	}

	// Now the rollback across the restart must be refused.
	if _, err := budget.Evaluate(rolled.State, day1In); !errors.Is(err, budget.ErrEvaluationRegressed) {
		t.Fatalf("post-restart rollback error = %v, want ErrEvaluationRegressed", err)
	}
}

// Changing the reporting timezone opens a genuinely new period, but it must not
// silently reset the once-per-period contract: the period the watermark has
// moved past can never be re-entered.
func TestEvaluateTimezoneChangeDoesNotSilentlyResetDedup(t *testing.T) {
	ny := loadLoc(t, "America/New_York")
	r := globalRule()
	utcSpend := globalSpendOn(testNow, time.UTC, 9, 0)
	nySpend := globalSpendOn(testNow, ny, 9, 0)

	utcRes := evalOK(t, budget.State{}, inputAt(testNow, time.UTC, []budget.Rule{r}, []budget.EstimatedSpend{utcSpend}))
	if len(utcRes.Notifications) != 1 {
		t.Fatalf("UTC notifications = %+v, want 1", utcRes.Notifications)
	}
	nyRes := evalOK(t, utcRes.State, inputAt(testNow, ny, []budget.Rule{r}, []budget.EstimatedSpend{utcSpend, nySpend}))
	if len(nyRes.Notifications) != 1 {
		t.Fatalf("New York notifications = %+v, want 1 for the new period", nyRes.Notifications)
	}

	res, err := budget.Evaluate(nyRes.State, inputAt(testNow, time.UTC, []budget.Rule{r}, []budget.EstimatedSpend{utcSpend, nySpend}))
	if !errors.Is(err, budget.ErrEvaluationRegressed) {
		t.Fatalf("switch-back error = %v, want ErrEvaluationRegressed instead of a silent re-notification", err)
	}
	if len(res.Notifications) != 0 {
		t.Fatalf("switch-back notifications = %+v, want none", res.Notifications)
	}
}

// A timezone change that maps the current instant into an *earlier* calendar
// period is the same hazard in the other direction, and it is refused the same
// way: switching to a zone behind the watermark must not silently reset the
// contract. Handling this as an error, an alarm or a manual confirmation is the
// access layer's decision; swallowing it would silently stop evaluating.
func TestEvaluateTimezoneChangeBackwardsIsRefused(t *testing.T) {
	honolulu := loadLoc(t, "Pacific/Honolulu")
	r := globalRule()
	early := time.Date(2026, 3, 9, 2, 0, 0, 0, time.UTC) // 2026-03-08 16:00 in Hawaii

	fired := evalOK(t, budget.State{}, inputAt(early, time.UTC, []budget.Rule{r}, []budget.EstimatedSpend{globalSpendOn(early, time.UTC, 9, 0)}))
	if len(fired.Notifications) != 1 {
		t.Fatalf("setup notifications = %+v, want 1", fired.Notifications)
	}
	spend := globalSpendOn(early, honolulu, 9, 0)
	res, err := budget.Evaluate(fired.State, inputAt(early, honolulu, []budget.Rule{r}, []budget.EstimatedSpend{spend}))
	if !errors.Is(err, budget.ErrEvaluationRegressed) {
		t.Fatalf("timezone step back error = %v, want ErrEvaluationRegressed", err)
	}
	if len(res.Notifications) != 0 {
		t.Fatalf("timezone step back notifications = %+v, want none", res.Notifications)
	}
}

// The watermark is the period start, not the wall instant: a backwards clock
// move inside the current period cannot duplicate anything, because the period's
// reservation is still in the ledger.
func TestWatermarkAllowsBackwardsClockInsideTheSamePeriod(t *testing.T) {
	r := globalRule()
	noon := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)
	morning := time.Date(2026, 3, 9, 6, 0, 0, 0, time.UTC)

	fired := evalOK(t, budget.State{}, inputAt(noon, time.UTC, []budget.Rule{r}, []budget.EstimatedSpend{globalSpend(9, 0)}))
	if len(fired.Notifications) != 1 {
		t.Fatalf("setup notifications = %+v, want 1", fired.Notifications)
	}
	again := evalOK(t, fired.State, inputAt(morning, time.UTC, []budget.Rule{r}, []budget.EstimatedSpend{globalSpend(9, 0)}))
	if len(again.Notifications) != 0 {
		t.Fatalf("intra-period notifications = %+v, want none", again.Notifications)
	}
}

// The ledger is pruned on every evaluation, including disabled ones, so the
// watermark has to advance for disabled rules too. Otherwise disabling a rule,
// crossing a period boundary and rolling the clock back would re-notify.
func TestWatermarkAdvancesForDisabledRules(t *testing.T) {
	r := globalRule()
	disabled := globalRule()
	disabled.Disabled = true
	day1 := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

	fired := evalOK(t, budget.State{}, inputAt(day1, time.UTC, []budget.Rule{r}, []budget.EstimatedSpend{globalSpendOn(day1, time.UTC, 9, 0)}))
	if len(fired.Notifications) != 1 {
		t.Fatalf("setup notifications = %+v, want 1", fired.Notifications)
	}
	off := evalOK(t, fired.State, inputAt(day2, time.UTC, []budget.Rule{disabled}, []budget.EstimatedSpend{globalSpendOn(day2, time.UTC, 9, 0)}))
	if len(off.State.Reservations) != 0 {
		t.Fatalf("disabled evaluation reservations = %+v, want the old period pruned", off.State.Reservations)
	}
	if _, err := budget.Evaluate(off.State, inputAt(day1, time.UTC, []budget.Rule{r}, []budget.EstimatedSpend{globalSpendOn(day1, time.UTC, 9, 0)})); !errors.Is(err, budget.ErrEvaluationRegressed) {
		t.Fatalf("rolled-back Evaluate() error = %v, want ErrEvaluationRegressed", err)
	}
}

// The rule's own kind decides what "rollover" means: the guard must hold for
// week and month rules too, not only for days.
func TestWatermarkRefusesRollbackAcrossEveryPeriodKind(t *testing.T) {
	for _, kind := range []reporttime.Kind{reporttime.Day, reporttime.Week, reporttime.Month} {
		t.Run(string(kind), func(t *testing.T) {
			r := globalRule()
			r.Period = kind
			period := mustPeriod(testNow, kind, time.UTC)
			next := period.End.Add(time.Hour)

			fired := evalOK(t, budget.State{}, inputAt(testNow, time.UTC, []budget.Rule{r}, []budget.EstimatedSpend{globalSpendIn(period, 9, 0)}))
			if len(fired.Notifications) != 1 {
				t.Fatalf("setup notifications = %+v, want 1", fired.Notifications)
			}
			nextPeriod := mustPeriod(next, kind, time.UTC)
			rolled := evalOK(t, fired.State, inputAt(next, time.UTC, []budget.Rule{r}, []budget.EstimatedSpend{globalSpendIn(nextPeriod, 9, 0)}))
			if len(rolled.Notifications) != 1 {
				t.Fatalf("next period notifications = %+v, want 1", rolled.Notifications)
			}
			if got := rolled.State.Rules[r.ID].Watermark; !got.Equal(nextPeriod.Start) {
				t.Fatalf("watermark = %s, want %s", got, nextPeriod.Start)
			}
			if _, err := budget.Evaluate(rolled.State, inputAt(testNow, time.UTC, []budget.Rule{r}, []budget.EstimatedSpend{globalSpendIn(period, 9, 0)})); !errors.Is(err, budget.ErrEvaluationRegressed) {
				t.Fatalf("rollback error = %v, want ErrEvaluationRegressed", err)
			}
		})
	}
}

// A lower Version is a stale or rolled-back configuration, never a new
// generation: it must be refused before the fingerprint conflict is considered.
func TestEvaluateRejectsRuleVersionDecrease(t *testing.T) {
	v2 := globalRule()
	v2.Version = 2
	v2.Limit = 20

	fired := evalOK(t, budget.State{}, input([]budget.Rule{v2}, []budget.EstimatedSpend{globalSpend(17, 0)}))
	if len(fired.Notifications) != 1 || fired.Notifications[0].RuleVersion != 2 {
		t.Fatalf("setup notifications = %+v, want one v2 notification", fired.Notifications)
	}
	before, err := budget.EncodeState(fired.State)
	if err != nil {
		t.Fatalf("EncodeState: %v", err)
	}

	// v1 differs from v2 in identity (limit 10 vs 20), so this would also trip
	// the fingerprint conflict. The decrease must win, and the v2 ledger must
	// survive.
	res, err := budget.Evaluate(fired.State, input([]budget.Rule{globalRule()}, []budget.EstimatedSpend{globalSpend(17, 0)}))
	if !errors.Is(err, budget.ErrRuleVersionDecrease) {
		t.Fatalf("version decrease error = %v, want ErrRuleVersionDecrease", err)
	}
	if errors.Is(err, budget.ErrRuleVersionConflict) {
		t.Fatalf("version decrease reported as a fingerprint conflict: %v", err)
	}
	if res.State.Reservations != nil {
		t.Fatalf("refused evaluation returned a state: %+v", res.State)
	}
	after, err := budget.EncodeState(fired.State)
	if err != nil {
		t.Fatalf("EncodeState: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("version decrease mutated the ledger:\nbefore %s\nafter  %s", before, after)
	}
	for _, reservation := range fired.State.Reservations {
		if reservation.RuleVersion != 2 {
			t.Errorf("stale reservation kept: %+v", reservation)
		}
	}
}
