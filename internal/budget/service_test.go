package budget_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/budget"
	"github.com/zzstar101/mytoken/internal/reporttime"
	"github.com/zzstar101/mytoken/internal/store"
)

// fakeSpend is an injected cost source: these tests care about the service's
// reservation and delivery behaviour, not about SQL.
type fakeSpend struct {
	mu    sync.Mutex
	byKey map[string]budget.Spend
	calls []string
}

func spendKey(scope budget.Scope, period reporttime.Period) string {
	return scope.String() + "|" + period.ID()
}

func (f *fakeSpend) set(scope budget.Scope, period reporttime.Period, amount float64, unpriced int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byKey == nil {
		f.byKey = map[string]budget.Spend{}
	}
	f.byKey[spendKey(scope, period)] = budget.Spend{Amount: amount, Unpriced: unpriced}
}

func (f *fakeSpend) EstimatedSpend(_ context.Context, scope budget.Scope, period reporttime.Period) (budget.Spend, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := spendKey(scope, period)
	f.calls = append(f.calls, k)
	s, ok := f.byKey[k]
	return s, ok, nil
}

func (f *fakeSpend) askCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeNotifier records deliveries; err makes every delivery fail.
type fakeNotifier struct {
	mu   sync.Mutex
	sent []budget.Notification
	err  error
}

func (f *fakeNotifier) Notify(_ context.Context, n budget.Notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, n)
	return f.err
}

func (f *fakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeNotifier) last() budget.Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent[len(f.sent)-1]
}

func (f *fakeNotifier) all() []budget.Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]budget.Notification(nil), f.sent...)
}

// newTestService pins the clock and the zone so a period is unambiguous.
func newTestService(t *testing.T, st *store.Store, costs budget.SpendSource, n budget.Notifier) *budget.Service {
	t.Helper()
	svc := budget.NewService(st, costs, n)
	svc.SetClock(func() time.Time { return testNow })
	svc.SetLocation(func() *time.Location { return time.UTC })
	t.Cleanup(svc.Close)
	return svc
}

func saveRules(t *testing.T, st *store.Store, rules ...budget.Rule) {
	t.Helper()
	if err := budget.SaveConfig(context.Background(), st, budget.Config{Rules: rules}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
}

func TestServiceDeliversOnceAndKeepsTheReservation(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	saveRules(t, st, globalRule())

	costs := &fakeSpend{}
	costs.set(globalScope(), testDayPeriod, 9, 0)
	n := &fakeNotifier{}
	svc := newTestService(t, st, costs, n)

	if err := svc.Evaluate(ctx); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	status := svc.Status()
	if len(status.Deliveries) != 1 {
		t.Fatalf("Deliveries = %+v, want exactly the 80%% crossing", status.Deliveries)
	}
	d := status.Deliveries[0]
	if d.Outcome != budget.DeliveryDelivered {
		t.Fatalf("Outcome = %q, want %q (err %v)", d.Outcome, budget.DeliveryDelivered, d.Err)
	}
	if d.Notification.Threshold != 80 || d.Notification.Percent != 90 || d.Notification.Estimated != 9 {
		t.Fatalf("notification = %+v, want 80%% of 9/10", d.Notification)
	}
	if got := n.last().Title(); !strings.Contains(got, "80% reached") {
		t.Fatalf("Title = %q, want the crossed threshold", got)
	}

	// Same process, same period: nothing is delivered twice.
	if err := svc.Evaluate(ctx); err != nil {
		t.Fatalf("second Evaluate: %v", err)
	}
	if n.count() != 1 {
		t.Fatalf("second evaluation delivered %d notifications, want 1 total", n.count())
	}
	if len(svc.Status().Deliveries) != 0 {
		t.Fatalf("second evaluation reserved %+v, want nothing new", svc.Status().Deliveries)
	}

	// A restart is a new Service over the same store: the ledger, not process
	// memory, is what stops the repeat.
	n2 := &fakeNotifier{}
	svc2 := newTestService(t, st, costs, n2)
	if err := svc2.Evaluate(ctx); err != nil {
		t.Fatalf("Evaluate after restart: %v", err)
	}
	if n2.count() != 0 {
		t.Fatalf("restart delivered %d notifications, want none", n2.count())
	}
	state, err := budget.LoadState(ctx, st)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Reservations) != 1 {
		t.Fatalf("reservations = %+v, want the durable one", state.Reservations)
	}
}

func TestServiceReportsFailedDeliveryAndDoesNotRetry(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	saveRules(t, st, globalRule())

	costs := &fakeSpend{}
	costs.set(globalScope(), testDayPeriod, 9, 0)
	n := &fakeNotifier{err: errors.New("notifications denied")}
	svc := newTestService(t, st, costs, n)

	if err := svc.Evaluate(ctx); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	d := svc.Status().Deliveries[0]
	if d.Outcome != budget.DeliveryFailed || d.Err == nil {
		t.Fatalf("delivery = %+v, want a reported failure", d)
	}
	if !strings.Contains(d.Err.Error(), "denied") {
		t.Fatalf("Err = %v, want the notifier's reason", d.Err)
	}
	if svc.Status().Err != nil {
		t.Fatalf("Status.Err = %v, want nil: the evaluation itself succeeded", svc.Status().Err)
	}

	// The reservation stands: a failed delivery is reported, never claimed
	// delivered, and never repeated.
	if err := svc.Evaluate(ctx); err != nil {
		t.Fatalf("second Evaluate: %v", err)
	}
	if n.count() != 1 {
		t.Fatalf("notifier called %d times, want 1", n.count())
	}
	if len(svc.Status().Deliveries) != 0 {
		t.Fatalf("second evaluation retried: %+v", svc.Status().Deliveries)
	}
}

func TestServiceWithoutNotifierReservesAndSaysSo(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	saveRules(t, st, globalRule())

	costs := &fakeSpend{}
	costs.set(globalScope(), testDayPeriod, 9, 0)
	svc := newTestService(t, st, costs, nil)

	if err := svc.Evaluate(ctx); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	status := svc.Status()
	if status.Notify {
		t.Fatalf("Status.Notify = true with no notifier installed")
	}
	if len(status.Deliveries) != 1 || status.Deliveries[0].Outcome != budget.DeliveryNoNotifier {
		t.Fatalf("Deliveries = %+v, want %q", status.Deliveries, budget.DeliveryNoNotifier)
	}
	if err := svc.Evaluate(ctx); err != nil {
		t.Fatalf("second Evaluate: %v", err)
	}
	if len(svc.Status().Deliveries) != 0 {
		t.Fatalf("nothing was delivered, so nothing should be reserved twice: %+v", svc.Status().Deliveries)
	}
}

func TestServiceSkipsForeignCurrencyWithoutBlending(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	eur := globalRule()
	eur.ID, eur.Currency, eur.Label = "eur-rule", "EUR", "euro limit"
	saveRules(t, st, globalRule(), eur)

	costs := &fakeSpend{}
	costs.set(globalScope(), testDayPeriod, 9, 0)
	n := &fakeNotifier{}
	svc := newTestService(t, st, costs, n)

	if err := svc.Evaluate(ctx); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	status := svc.Status()
	if len(status.Skipped) != 1 || status.Skipped[0].Rule.ID != "eur-rule" {
		t.Fatalf("Skipped = %+v, want the EUR rule", status.Skipped)
	}
	if status.Skipped[0].Reason != budget.SkipForeignCurrency {
		t.Fatalf("Reason = %q, want %q", status.Skipped[0].Reason, budget.SkipForeignCurrency)
	}
	for _, ev := range status.Evaluations {
		if ev.RuleID == "eur-rule" {
			t.Fatalf("EUR rule was evaluated against a USD estimate: %+v", ev)
		}
	}
	if costs.askCount() != 1 {
		t.Fatalf("cost source asked %d times, want 1: the two rules share a scope and period", costs.askCount())
	}
	for _, d := range status.Deliveries {
		if d.Notification.Currency != "USD" {
			t.Fatalf("delivered %q, want only USD", d.Notification.Currency)
		}
	}
}

func TestServiceFlagsUnknownPrices(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	saveRules(t, st, globalRule())

	costs := &fakeSpend{}
	costs.set(globalScope(), testDayPeriod, 9, 3)
	n := &fakeNotifier{}
	svc := newTestService(t, st, costs, n)

	if err := svc.Evaluate(ctx); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	ev := svc.Status().Evaluations[0]
	if !ev.UnknownPricing || ev.UnpricedRequests != 3 {
		t.Fatalf("evaluation = %+v, want UnknownPricing with 3 unpriced requests", ev)
	}
	got := svc.Status().Deliveries[0].Notification
	if !got.UnknownPricing {
		t.Fatalf("notification = %+v, want the unknown-price flag", got)
	}
	body := got.Body()
	if !strings.Contains(body, "no known price") {
		t.Fatalf("Body = %q, want the unknown-price caveat", body)
	}
	if !strings.Contains(body, "not a billed amount") {
		t.Fatalf("Body = %q, want the local-estimate caveat", body)
	}
}

func TestServiceRoutesEachScopeAndPeriod(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	proj := budget.Rule{ID: "proj", Version: 1, Scope: budget.Scope{Kind: budget.ScopeProject, ID: "/p/one"}, Period: reporttime.Week, Currency: "USD", Limit: 100}
	prov := budget.Rule{ID: "prov", Version: 1, Scope: budget.Scope{Kind: budget.ScopeProvider, ID: "endpoint:v1:abc"}, Period: reporttime.Month, Currency: "USD", Limit: 1000}
	saveRules(t, st, globalRule(), proj, prov)

	costs := &fakeSpend{}
	costs.set(globalScope(), testDayPeriod, 9, 0)
	costs.set(proj.Scope, testWeekPeriod, 50, 0)
	costs.set(prov.Scope, testMonthPeriod, 1000, 0)
	n := &fakeNotifier{}
	svc := newTestService(t, st, costs, n)

	if err := svc.Evaluate(ctx); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	got := map[string]budget.Evaluation{}
	for _, ev := range svc.Status().Evaluations {
		got[ev.RuleID] = ev
	}
	if len(got) != 3 {
		t.Fatalf("evaluations = %+v, want one per rule", svc.Status().Evaluations)
	}
	if ev := got["rule-1"]; ev.Estimated != 9 || ev.Period != reporttime.Day {
		t.Fatalf("global rule saw %+v, want the day estimate", ev)
	}
	if ev := got["proj"]; ev.Estimated != 50 || ev.Period != reporttime.Week || len(ev.Crossed) != 0 {
		t.Fatalf("project rule saw %+v, want the week estimate below every threshold", ev)
	}
	if ev := got["prov"]; ev.Estimated != 1000 || ev.Period != reporttime.Month || len(ev.Crossed) != 2 {
		t.Fatalf("provider rule saw %+v, want the month estimate at 100%%", ev)
	}
	if costs.askCount() != 3 {
		t.Fatalf("cost source asked %d times, want 3 distinct (scope, period) pairs", costs.askCount())
	}
	// The provider-scope id is the same stable key the query layer reports, so
	// the delivered rule names it verbatim.
	var seen bool
	for _, d := range n.all() {
		if d.Scope.Kind == budget.ScopeProvider && d.Scope.ID == "endpoint:v1:abc" {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("notifications = %+v, want the provider stable id preserved", n.all())
	}
}

func TestServiceStartAndCloseStopsCallbacks(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	saveRules(t, st, globalRule())

	costs := &fakeSpend{}
	costs.set(globalScope(), testDayPeriod, 9, 0)
	n := &fakeNotifier{}
	svc := newTestService(t, st, costs, n)

	updates := make(chan struct{}, 8)
	svc.SetOnUpdate(func() {
		select {
		case updates <- struct{}{}:
		default:
		}
	})
	svc.Start(ctx)

	select {
	case <-updates:
	case <-time.After(20 * time.Second):
		t.Fatal("Start never evaluated")
	}
	svc.Close()

drain:
	for {
		select {
		case <-updates:
		default:
			break drain
		}
	}
	before := n.count()
	svc.Wake()
	select {
	case <-updates:
		t.Fatal("callback ran after Close")
	case <-time.After(300 * time.Millisecond):
	}
	if n.count() != before {
		t.Fatalf("notifier called after Close: %d, want %d", n.count(), before)
	}
	svc.Close() // idempotent
}

// TestServiceSendsNothingUntilAZoneIsChosen: with no fixed reporting calendar
// the service must not read a spend, reserve a threshold or deliver a
// notification — a reservation made in an ambient system zone would make the
// budget fire (or not fire) against a calendar the user never chose. The
// crossing is still delivered exactly once after the choice is made.
func TestServiceSendsNothingUntilAZoneIsChosen(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	saveRules(t, st, globalRule())

	costs := &fakeSpend{}
	costs.set(globalScope(), testDayPeriod, 9, 0) // 90%: crosses 80%
	n := &fakeNotifier{}
	svc := newTestService(t, st, costs, n)
	svc.SetZoneReadiness(func() bool { return false })

	if err := svc.Evaluate(ctx); !errors.Is(err, budget.ErrZoneUnconfigured) {
		t.Fatalf("Evaluate = %v, want ErrZoneUnconfigured", err)
	}
	if got := n.count(); got != 0 {
		t.Fatalf("delivered %d notifications with no calendar, want 0", got)
	}
	if got := costs.askCount(); got != 0 {
		t.Fatalf("read spend %d times with no calendar, want 0", got)
	}
	status := svc.Status()
	if !status.ZoneUnconfigured || status.Err == nil || status.Timezone != "" {
		t.Fatalf("Status = %+v, want ZoneUnconfigured with a reason and no zone name", status)
	}
	if len(status.Deliveries) != 0 || len(status.Evaluations) != 0 {
		t.Fatalf("Status = %+v, want no evaluations and no deliveries", status)
	}
	// A reservation must not have been persisted either: after an explicit
	// choice the same crossing is delivered once.
	svc.SetZoneReadiness(func() bool { return true })
	if err := svc.Evaluate(ctx); err != nil {
		t.Fatalf("Evaluate after the choice: %v", err)
	}
	if got := n.count(); got != 1 {
		t.Fatalf("delivered %d notifications after the choice, want 1", got)
	}
}
