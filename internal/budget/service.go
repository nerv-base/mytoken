package budget

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/zzstar101/mytoken/internal/reporttime"
	"github.com/zzstar101/mytoken/internal/store"
)

// Delivery outcomes. DeliveryFailed means the operating system refused the
// notification. The reservation is already durable at that point, so the
// service does not deliver it again: delivery is at-most-once, never
// exactly-once, and the caller must say so rather than promise a retry.
const (
	DeliveryDelivered  = "delivered"
	DeliveryFailed     = "failed"
	DeliveryNoNotifier = "no notifier"
)

// SkipForeignCurrency explains a configured rule that is not in the local
// estimate currency. Local estimates are always LocalCurrency, and amounts in
// another currency are never converted or combined, so the rule is not
// evaluated at all.
const SkipForeignCurrency = "no local estimate in a different currency"

// ErrZoneUnconfigured stops an evaluation when no fixed reporting timezone has
// been chosen. Calendar periods cannot be named without one, so nothing is
// estimated, no threshold is reserved and nothing is delivered: falling back to
// an ambient system zone would silently reinterpret every stored period and
// notification. The caller reports it and asks the user to choose a zone.
var ErrZoneUnconfigured = errors.New("budget: no reporting timezone is configured; choose an IANA timezone before budget checks run")

// ZoneReadiness reports whether a fixed reporting timezone is configured. The
// zero value (nil) means ready, so a service built without a gate (tests, the
// CLI) keeps its injected location.
type ZoneReadiness func() bool

// Evaluation cadence. evalSettle lets a burst of store commits become one
// evaluation; evalInterval re-evaluates so a period rollover and a freshly
// crossed threshold are noticed without waiting for a write.
const (
	evalSettle   = 2 * time.Second
	evalInterval = 15 * time.Minute
)

// Notifier delivers one reserved notification. It is called only after the
// reservation is durable.
type Notifier interface {
	// Notify hands n to the operating system. A non-nil error means it was
	// not delivered; the reservation stands, so the notification is not
	// repeated.
	Notify(ctx context.Context, n Notification) error
}

// Delivery records what happened to one reserved notification.
type Delivery struct {
	Notification Notification
	// Outcome is DeliveryDelivered, DeliveryFailed or DeliveryNoNotifier.
	Outcome string
	// Err is the notifier's failure, when Outcome is DeliveryFailed.
	Err error
}

// SkippedRule is a configured rule the service did not evaluate.
type SkippedRule struct {
	Rule   Rule
	Reason string
}

// Status is the last evaluation, for the GUI: what each rule estimated, what
// was delivered, and what went wrong. It is a snapshot, not live state.
type Status struct {
	// At is the instant the evaluation used as "now".
	At time.Time
	// Err is the failure that stopped the last evaluation, if any. A failed
	// evaluation delivers nothing and leaves the ledger untouched.
	Err error
	// Evaluations is one entry per evaluated rule, in rule-id order.
	Evaluations []Evaluation
	// Deliveries is one entry per notification reserved by the last run.
	Deliveries []Delivery
	// Skipped is every configured rule that was not evaluated.
	Skipped []SkippedRule
	// Timezone is the zone name the periods resolved in. It is empty when the
	// last evaluation was refused because no fixed reporting timezone exists.
	Timezone string
	// Notify reports whether a notifier is installed at all.
	Notify bool
	// ZoneUnconfigured is true when the last evaluation was refused because no
	// fixed reporting timezone is configured (ErrZoneUnconfigured). The GUI
	// uses it to point at the timezone setting instead of reporting rules.
	ZoneUnconfigured bool
}

// Service evaluates the persisted rules against local estimated cost and
// delivers notifications through a Notifier.
//
// It owns no clock and no location: both are injected, so a test can pin them
// and the app can follow the persisted stats timezone.
type Service struct {
	store    *store.Store
	costs    SpendSource
	notifier Notifier

	now func() time.Time
	loc func() *time.Location
	// zoneReady gates evaluation on a configured fixed reporting timezone.
	// nil means always ready.
	zoneReady ZoneReadiness

	mu       sync.Mutex
	status   Status
	onUpdate func()

	started bool
	cancel  context.CancelFunc
	workers sync.WaitGroup
	wake    chan struct{}
}

// NewService makes a service over st. costs and notifier may be nil; a nil
// notifier reserves and reports DeliveryNoNotifier instead of delivering.
func NewService(st *store.Store, costs SpendSource, notifier Notifier) *Service {
	return &Service{
		store:    st,
		costs:    costs,
		notifier: notifier,
		now:      time.Now,
		loc:      func() *time.Location { return time.Local },
		wake:     make(chan struct{}, 1),
	}
}

// SetClock overrides the evaluation clock (tests).
func (s *Service) SetClock(now func() time.Time) {
	if now != nil {
		s.mu.Lock()
		s.now = now
		s.mu.Unlock()
	}
}

// SetLocation overrides where periods are resolved (tests, and the app's
// persisted stats timezone).
func (s *Service) SetLocation(loc func() *time.Location) {
	if loc != nil {
		s.mu.Lock()
		s.loc = loc
		s.mu.Unlock()
	}
}

// SetZoneReadiness installs the calendar gate: when ready reports false, every
// evaluation is refused with ErrZoneUnconfigured before any spend is read, so
// no notification is reserved and none is delivered while the reporting
// timezone is unresolved. A nil readiness means always ready.
func (s *Service) SetZoneReadiness(ready ZoneReadiness) {
	s.mu.Lock()
	s.zoneReady = ready
	s.mu.Unlock()
}

// zoneReadyLocked reports whether evaluation may run. The caller holds s.mu.
func (s *Service) zoneReadyLocked() bool {
	if s.zoneReady == nil {
		return true
	}
	return s.zoneReady()
}

// SetOnUpdate installs a callback run after every evaluation, on the
// evaluating goroutine. The GUI uses it to redraw.
func (s *Service) SetOnUpdate(fn func()) {
	s.mu.Lock()
	s.onUpdate = fn
	s.mu.Unlock()
}

// Status returns the last evaluation snapshot.
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// LoadConfig reads the persisted rules.
func (s *Service) LoadConfig(ctx context.Context) (Config, error) { return LoadConfig(ctx, s.store) }

// SaveConfig validates and persists cfg. The caller decides when to evaluate;
// saving alone never triggers a notification.
func (s *Service) SaveConfig(ctx context.Context, cfg Config) error {
	return SaveConfig(ctx, s.store, cfg)
}

// Evaluate runs one full pass: load the rules and the ledger, read local
// estimated cost, reserve every newly crossed threshold, persist the ledger
// and only then deliver. Any failure before the ledger is written delivers
// nothing, and the ledger is left exactly as it was.
func (s *Service) Evaluate(ctx context.Context) error {
	s.mu.Lock()
	st, pending, err := s.evaluateLocked(ctx)
	s.status = st
	update := s.onUpdate
	s.mu.Unlock()

	// Delivery happens outside the lock: handing a notification to a slow
	// notification centre must never block Status, Wake, Close or the GUI.
	if len(pending) > 0 {
		deliveries := make([]Delivery, 0, len(pending))
		for _, n := range pending {
			d := Delivery{Notification: n, Outcome: DeliveryNoNotifier}
			if s.notifier != nil {
				if err := s.notifier.Notify(ctx, n); err != nil {
					d.Outcome, d.Err = DeliveryFailed, err
				} else {
					d.Outcome = DeliveryDelivered
				}
			}
			deliveries = append(deliveries, d)
		}
		s.mu.Lock()
		s.status.Deliveries = deliveries
		s.mu.Unlock()
	}
	if update != nil {
		update()
	}
	return err
}

// evaluateLocked runs one pass and reports what to deliver. The caller holds
// s.mu; deliveries are the caller's job, after the lock is dropped.
func (s *Service) evaluateLocked(ctx context.Context) (Status, []Notification, error) {
	loc := s.locationLocked()
	now := s.nowLocked()
	status := Status{At: now, Timezone: loc.String(), Notify: s.notifier != nil}
	if !s.zoneReadyLocked() {
		// Refuse before the store, the config or any spend is touched: an
		// unresolved calendar must reserve no threshold and deliver nothing.
		status.Timezone = ""
		status.ZoneUnconfigured = true
		status.Err = ErrZoneUnconfigured
		return status, nil, ErrZoneUnconfigured
	}
	if s.store == nil {
		status.Err = errors.New("budget: no store")
		return status, nil, status.Err
	}
	if s.costs == nil {
		status.Err = errors.New("budget: no cost source")
		return status, nil, status.Err
	}
	cfg, err := LoadConfig(ctx, s.store)
	if err != nil {
		status.Err = err
		return status, nil, err
	}
	prev, err := LoadState(ctx, s.store)
	if err != nil {
		status.Err = err
		return status, nil, err
	}
	rules := make([]Rule, 0, len(cfg.Rules))
	for _, r := range cfg.Rules {
		if r.Currency != LocalCurrency {
			status.Skipped = append(status.Skipped, SkippedRule{Rule: r, Reason: SkipForeignCurrency})
			continue
		}
		rules = append(rules, r)
	}
	spends, err := s.spends(ctx, rules, now, loc)
	if err != nil {
		status.Err = err
		return status, nil, err
	}
	res, err := Evaluate(prev, Input{Now: now, Location: loc, Rules: rules, Spends: spends})
	if err != nil {
		status.Err = err
		return status, nil, err
	}
	// The reservation is durable before any delivery: that ordering is what
	// bounds delivery to at most once per rule generation, period and
	// threshold, including across a crash or a restart.
	if err := SaveState(ctx, s.store, res.State); err != nil {
		status.Err = err
		return status, nil, err
	}
	status.Evaluations = res.Evaluations
	return status, res.Notifications, nil
}

// spends reads one estimate per distinct (scope, current period) among the
// rules that are actually evaluated. A scope with no data contributes no
// spend entry at all, which is what makes Evaluate report SkipNoData rather
// than a zero estimate.
func (s *Service) spends(ctx context.Context, rules []Rule, now time.Time, loc *time.Location) ([]EstimatedSpend, error) {
	type key struct{ scope, period string }
	out := make([]EstimatedSpend, 0, len(rules))
	seen := make(map[key]struct{}, len(rules))
	for _, r := range rules {
		if r.Disabled {
			continue
		}
		p, err := reporttime.Resolve(now, r.Period, loc)
		if err != nil {
			return nil, err
		}
		k := key{r.Scope.String(), p.Current.ID()}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		spend, ok, err := s.costs.EstimatedSpend(ctx, r.Scope, p.Current)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		out = append(out, EstimatedSpend{
			Scope:            r.Scope,
			Period:           p.Current,
			Currency:         LocalCurrency,
			Amount:           spend.Amount,
			UnpricedRequests: spend.Unpriced,
		})
	}
	return out, nil
}

// Start begins evaluating: once shortly after start, again whenever the store
// commits new data, on Wake, and every evalInterval so a period rollover or a
// crossed threshold is seen without a write. Close stops it.
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started || s.store == nil {
		s.mu.Unlock()
		return
	}
	s.started = true
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.mu.Unlock()

	ch, unsub := s.store.Subscribe()
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		defer unsub()
		ticker := time.NewTicker(evalInterval)
		defer ticker.Stop()
		settle := time.NewTimer(evalSettle)
		defer settle.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				resetTimer(settle, evalSettle)
				continue
			case <-s.wake:
				resetTimer(settle, evalSettle)
				continue
			case <-settle.C:
			case <-ticker.C:
			}
			_ = s.Evaluate(ctx)
		}
	}()
}

// Wake asks the running loop to evaluate soon. It never blocks and does
// nothing when the loop is not running.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Close stops the loop and waits for it. It is safe to call more than once.
func (s *Service) Close() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.workers.Wait()
}

// locationLocked resolves the injected zone. The caller holds s.mu, which is
// what keeps an evaluation from blocking on its own lock.
func (s *Service) locationLocked() *time.Location {
	loc := s.loc
	if loc == nil {
		return time.Local
	}
	if l := loc(); l != nil {
		return l
	}
	return time.Local
}

// nowLocked reads the injected clock. The caller holds s.mu.
func (s *Service) nowLocked() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

func (s *Service) location() *time.Location {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.locationLocked()
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// Title is the notification headline: the rule's label (or its scope) and the
// threshold that was crossed.
func (n Notification) Title() string {
	label := n.Label
	if label == "" {
		label = scopeText(n.Scope)
	}
	return fmt.Sprintf("Budget %s: %.0f%% reached", label, n.Threshold)
}

// Body is the notification's figures, always naming the currency and saying
// when the estimate is a lower bound because some requests have no known
// price.
func (n Notification) Body() string {
	body := fmt.Sprintf("Estimated %s%.2f of %s%.2f (%.0f%%) this %s.",
		n.Currency, n.Estimated, n.Currency, n.Limit, n.Percent, n.Period)
	if n.UnknownPricing {
		body += " Excludes requests with no known price, so the real cost is higher."
	}
	body += " Local estimate only, not a billed amount."
	return body
}

func scopeText(s Scope) string {
	if s.Kind == ScopeGlobal {
		return "total"
	}
	if s.ID == "" {
		return string(s.Kind)
	}
	return string(s.Kind) + " " + s.ID
}
