package budget_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/budget"
	"github.com/zzstar101/mytoken/internal/reporttime"
)

// A restart is simulated by encoding the reserve/dedup state and decoding it in
// a fresh process image: the same period must not notify twice.
func TestStatePersistenceRestartSimulation(t *testing.T) {
	in := input([]budget.Rule{globalRule()}, []budget.EstimatedSpend{globalSpend(8.5, 0)})
	first := evalOK(t, budget.State{}, in)
	if len(first.Notifications) != 1 {
		t.Fatalf("first run notifications = %d, want 1", len(first.Notifications))
	}

	raw, err := budget.EncodeState(first.State)
	if err != nil {
		t.Fatalf("EncodeState: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("EncodeState produced no bytes")
	}
	restored, err := budget.DecodeState(raw)
	if err != nil {
		t.Fatalf("DecodeState: %v", err)
	}
	if restored.Version != budget.SchemaVersion {
		t.Fatalf("restored version = %d, want %d", restored.Version, budget.SchemaVersion)
	}
	if len(restored.Reservations) != 1 {
		t.Fatalf("restored reservations = %d, want 1", len(restored.Reservations))
	}

	afterRestart := evalOK(t, restored, in)
	if len(afterRestart.Notifications) != 0 {
		t.Fatalf("post-restart notifications = %+v, want none", afterRestart.Notifications)
	}
	if len(afterRestart.State.Reservations) != 1 {
		t.Fatalf("post-restart reservations = %d, want 1", len(afterRestart.State.Reservations))
	}

	// The next period notifies again after the restart, so the ledger is not a
	// permanent mute.
	next := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	nextIn := inputAt(next, time.UTC, []budget.Rule{globalRule()}, []budget.EstimatedSpend{globalSpendOn(next, time.UTC, 8.5, 0)})
	rolled := evalOK(t, afterRestart.State, nextIn)
	if len(rolled.Notifications) != 1 {
		t.Fatalf("next period notifications = %+v, want 1", rolled.Notifications)
	}

	// The watermark is part of the persisted document, not process memory.
	rs, ok := restored.Rules["rule-1"]
	if !ok {
		t.Fatalf("restored rules = %+v, want rule-1", restored.Rules)
	}
	if want := mustPeriod(testNow, reporttime.Day, time.UTC).Start; !rs.Watermark.Equal(want) {
		t.Fatalf("restored watermark = %s, want %s", rs.Watermark, want)
	}
}

func TestEncodeStateRoundTripsEmptyState(t *testing.T) {
	raw, err := budget.EncodeState(budget.State{})
	if err != nil {
		t.Fatalf("EncodeState: %v", err)
	}
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		t.Fatalf("encoded state is not JSON: %v", err)
	}
	if head.Version != budget.SchemaVersion {
		t.Fatalf("encoded version = %d, want %d", head.Version, budget.SchemaVersion)
	}
	if _, err := budget.DecodeState(raw); err != nil {
		t.Fatalf("DecodeState(round trip): %v", err)
	}
}

func TestDecodeStateRejectsBadDocuments(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want error
	}{
		{"not json", "not json at all", budget.ErrInvalidState},
		{"empty object", "{}", budget.ErrStateVersion},
		{"zero version", `{"version":0}`, budget.ErrStateVersion},
		{"previous version", `{"version":1}`, budget.ErrStateVersion},
		{"future version", `{"version":99}`, budget.ErrStateVersion},
		{"unknown field", fmt.Sprintf(`{"version":%d,"bogus":true}`, budget.SchemaVersion), budget.ErrInvalidState},
		{"trailing data", fmt.Sprintf(`{"version":%d}{"version":%d}`, budget.SchemaVersion, budget.SchemaVersion), budget.ErrInvalidState},
		{"rule entry without a watermark", fmt.Sprintf(`{"version":%d,"rules":{"rule-1":{"version":1,"fingerprint":"fp","last_period_id":"period:v1:day:UTC:2026-03-09T00:00:00Z"}},"reservations":{}}`, budget.SchemaVersion), budget.ErrInvalidState},
		{"rule entry with a zero watermark", fmt.Sprintf(`{"version":%d,"rules":{"rule-1":{"version":1,"fingerprint":"fp","last_period_id":"period:v1:day:UTC:2026-03-09T00:00:00Z","watermark":"0001-01-01T00:00:00Z"}},"reservations":{}}`, budget.SchemaVersion), budget.ErrInvalidState},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := budget.DecodeState([]byte(tc.raw)); !errors.Is(err, tc.want) {
				t.Fatalf("DecodeState(%s) error = %v, want %v", tc.raw, err, tc.want)
			}
		})
	}
}

// The evaluation watermark is part of the persisted document, so a restart can
// never re-open a period that was already crossed.
func TestEncodeStateCarriesTheEvaluationWatermark(t *testing.T) {
	r := globalRule()
	day1 := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)
	res := evalOK(t, budget.State{}, inputAt(day1, time.UTC, []budget.Rule{r}, []budget.EstimatedSpend{globalSpendOn(day1, time.UTC, 9, 0)}))

	raw, err := budget.EncodeState(res.State)
	if err != nil {
		t.Fatalf("EncodeState: %v", err)
	}
	var doc struct {
		Rules map[string]struct {
			Watermark *time.Time `json:"watermark"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("encoded state is not JSON: %v", err)
	}
	entry, ok := doc.Rules[r.ID]
	if !ok {
		t.Fatalf("encoded rules = %+v, want %s", doc.Rules, r.ID)
	}
	if entry.Watermark == nil {
		t.Fatalf("encoded rule entry omits the watermark: %s", raw)
	}
	if want := mustPeriod(day1, reporttime.Day, time.UTC).Start; !entry.Watermark.Equal(want) {
		t.Fatalf("encoded watermark = %s, want %s", entry.Watermark, want)
	}

	restored, err := budget.DecodeState(raw)
	if err != nil {
		t.Fatalf("DecodeState: %v", err)
	}
	if got := restored.Rules[r.ID].Watermark; !got.Equal(*entry.Watermark) {
		t.Fatalf("round-tripped watermark = %s, want %s", got, *entry.Watermark)
	}
}

// A rule entry without a watermark cannot be trusted: it would silently disable
// the rollback guard, so encoding must refuse it rather than emit it.
func TestEncodeStateRejectsRuleEntryWithoutWatermark(t *testing.T) {
	_, err := budget.EncodeState(budget.State{
		Rules: map[string]budget.RuleState{
			"rule-1": {Version: 1, Fingerprint: "fp", LastPeriodID: "period:v1:day:UTC:2026-03-09T00:00:00Z"},
		},
	})
	if !errors.Is(err, budget.ErrInvalidState) {
		t.Fatalf("EncodeState error = %v, want ErrInvalidState", err)
	}
}

// The whole run must work for week and month rules too, not only days.
func TestEvaluateAllPeriodKinds(t *testing.T) {
	for _, kind := range []reporttime.Kind{reporttime.Day, reporttime.Week, reporttime.Month} {
		t.Run(string(kind), func(t *testing.T) {
			r := globalRule()
			r.Period = kind
			res := evalOK(t, budget.State{}, input([]budget.Rule{r}, []budget.EstimatedSpend{globalSpendIn(mustPeriod(testNow, kind, time.UTC), 9, 0)}))
			if got := res.Evaluations[0].Period; got != kind {
				t.Fatalf("evaluation period = %q, want %q", got, kind)
			}
			want, err := reporttime.Resolve(testNow, kind, time.UTC)
			if err != nil {
				t.Fatalf("reporttime.Resolve: %v", err)
			}
			if res.Evaluations[0].PeriodID != want.Current.ID() {
				t.Fatalf("PeriodID = %q, want %q", res.Evaluations[0].PeriodID, want.Current.ID())
			}
			if len(res.Notifications) != 1 || res.Notifications[0].Period != kind {
				t.Fatalf("notifications = %+v, want one %s notification", res.Notifications, kind)
			}
		})
	}
}
