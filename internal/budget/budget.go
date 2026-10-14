// Package budget is the pure, notify-only budget validation and evaluation
// core for MyToken 0.4.0.
//
// It answers one question: given the local estimated cost already attributed to
// a scope in one calendar period, has a configured threshold of a rule's limit
// been reached for the first time in that period? Nothing else. It never blocks,
// never talks to a provider and never fetches a balance: the caller supplies the
// estimate, the evaluation instant and the reporting timezone.
//
// # Contract
//
//   - notify-only: the package produces notifications. It never enforces a
//     limit and never changes collection or attribution behaviour.
//   - local estimated costs only: the input is an EstimatedSpend built from
//     locally computed costs. Actual billing is a different subsystem and is
//     never mixed in here.
//   - explicit currency: every rule and every spend carries a three-letter code.
//     A rule whose spend is reported in another currency is rejected with
//     ErrCurrencyMismatch; currencies are never converted, summed or compared.
//   - explicit period provenance: every spend carries the full calendar period
//     it was aggregated over, and a rule sees only the spend for its own scope,
//     its own current period identity and its own currency. A spend for another
//     period is not this rule's data (SkipNoData), never a zero estimate, and no
//     default period is ever assumed on the caller's behalf.
//   - unknown pricing is flagged, never guessed: EstimatedSpend counts requests
//     whose price is unknown separately, and any evaluation or notification
//     built from such a spend sets UnknownPricing. Such an estimate is a lower
//     bound, not a complete cost.
//   - no data is not zero: a rule whose scope has no spend in the input is
//     reported with DataAvailable false and SkipNoData, never as 0%.
//   - explicit identity: a rule is identified by a stable caller-assigned ID, an
//     explicit Version, its scope, period, currency, limit and thresholds. The
//     display Label and the Disabled flag are deliberately outside that
//     identity, so renaming or toggling a rule never resets its dedup ledger.
//   - once per rule generation, period and threshold: a notification is reserved
//     in the returned State the first time the threshold is crossed, and the
//     same period, threshold and generation never reserves it again. A durable
//     monotonic watermark per rule generation makes that hold even when the
//     clock is rolled back or the reporting timezone is changed: an evaluation
//     whose period begins before the recorded watermark is refused with
//     ErrEvaluationRegressed instead of silently re-notifying.
//
// # Time
//
// now and *time.Location are injected; the package never reads time.Local and
// holds no package state. Period boundaries, Monday weeks and DST handling come
// from internal/reporttime, so a day across a spring-forward transition is 23h
// and a week containing one is 167h. Period identity is reporttime's own
// identity (kind, zone name and the actual start instant), so the same wall
// clock date in two zones is two different periods.
//
// # Persistence and delivery
//
// Evaluate is pure: it takes the previous State and returns the next State
// together with the notifications reserved during this call. The caller owns
// persistence (EncodeState/DecodeState cover the document format). The State is
// a reserve/dedup ledger, not a delivery record:
//
//   - Save before deliver gives at-most-once notification per period and may
//     lose one if the process dies after saving but before delivery.
//   - Deliver before save gives at-least-once and may duplicate after a crash.
//
// This package makes no exactly-once claim across crashes, and no claim about
// OS-level delivery at all. Delivery, retries and user-visible de-duplication
// beyond this ledger belong to the integration layer.
package budget

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/reporttime"
)

// SchemaVersion is the version of the persisted reserve/dedup state document
// written by EncodeState and accepted by DecodeState and Evaluate.
const SchemaVersion = 2

// Notification identity and state schema tags. They are part of the stable,
// persisted format.
const (
	notificationIDTag  = "budget:v1"
	ruleFingerprintTag = "budget-rule:v1"
)

// SkippedReason values explain why a rule produced no evaluation of spend.
const (
	// SkipDisabled marks a rule the caller disabled. Its ledger entry is still
	// tracked so re-enabling it inside the same period does not re-notify.
	SkipDisabled = "rule disabled"
	// SkipNoData marks a rule whose scope has no spend for the rule's current
	// period identity. This is not a zero estimate, and a spend for another
	// period of the same scope is not a substitute for it.
	SkipNoData = "no spend data for scope"
)

// Sentinel errors. Every validation and evaluation failure wraps one of these,
// so callers can classify without matching message text.
var (
	// ErrInvalidRule marks a rule whose identity or limit fields are unusable.
	ErrInvalidRule = errors.New("budget: invalid rule")
	// ErrInvalidScope marks an unknown scope kind or an invalid stable scope id.
	ErrInvalidScope = errors.New("budget: invalid scope")
	// ErrInvalidPeriod marks a period kind that is not day, week or month.
	ErrInvalidPeriod = errors.New("budget: invalid period")
	// ErrInvalidCurrency marks a currency that is not a canonical three-letter
	// uppercase code.
	ErrInvalidCurrency = errors.New("budget: invalid currency")
	// ErrInvalidAmount marks a NaN, infinite, negative or otherwise unusable
	// amount, limit or request count.
	ErrInvalidAmount = errors.New("budget: invalid amount")
	// ErrInvalidThreshold marks a threshold that is NaN, infinite, outside
	// (0, 100] or duplicated inside one rule.
	ErrInvalidThreshold = errors.New("budget: invalid threshold")
	// ErrDuplicateRule marks two rules sharing one stable rule ID.
	ErrDuplicateRule = errors.New("budget: duplicate rule id")
	// ErrDuplicateSpend marks two spends for one scope.
	ErrDuplicateSpend = errors.New("budget: duplicate spend scope")
	// ErrRuleVersionConflict marks a rule that changed identity-bearing fields
	// without bumping Version. The caller must bump Version explicitly.
	ErrRuleVersionConflict = errors.New("budget: rule changed without a new version")
	// ErrRuleVersionDecrease marks a rule whose Version is lower than the
	// generation already recorded in the ledger. A lower version is a stale or
	// rolled-back configuration, never a new generation, so it is refused
	// instead of re-arming the rule's notifications.
	ErrRuleVersionDecrease = errors.New("budget: rule version decreased below the recorded generation")
	// ErrEvaluationRegressed marks an evaluation whose resolved period starts
	// before the rule's persisted evaluation watermark: a wall-clock rollback or
	// a timezone change that moves the period backwards. It is refused, and the
	// ledger is left untouched, so an already-notified period is never crossed
	// again.
	ErrEvaluationRegressed = errors.New("budget: evaluation regressed behind the persisted watermark")
	// ErrCurrencyMismatch marks a rule and a spend that disagree on currency.
	// Currencies are never converted or combined.
	ErrCurrencyMismatch = errors.New("budget: currency mismatch")
	// ErrInvalidInput marks a missing location or evaluation instant.
	ErrInvalidInput = errors.New("budget: invalid input")
	// ErrStateVersion marks a persisted state document from an unsupported
	// schema version.
	ErrStateVersion = errors.New("budget: unsupported state version")
	// ErrInvalidState marks an undecodable or malformed state document.
	ErrInvalidState = errors.New("budget: invalid state")
)

// DefaultThresholds returns the default warn and limit percentages, 80% and
// 100%. The result is a fresh slice; callers may modify it.
func DefaultThresholds() []float64 { return []float64{80, 100} }

// ScopeKind is the axis a rule's limit applies to. Only these three values are
// valid; ParseScopeKind normalizes input, Validate and Scope.String never guess.
type ScopeKind string

const (
	// ScopeGlobal is the whole installation.
	ScopeGlobal ScopeKind = "global"
	// ScopeProject is one stable project id.
	ScopeProject ScopeKind = "project"
	// ScopeProvider is one stable provider id.
	ScopeProvider ScopeKind = "provider"
)

// Valid reports whether k is one of the canonical scope kinds. It is exact.
func (k ScopeKind) Valid() bool {
	return k == ScopeGlobal || k == ScopeProject || k == ScopeProvider
}

// ParseScopeKind normalizes user input (surrounding space and case) into a
// canonical ScopeKind. Anything else is an error; nothing defaults to a scope.
func ParseScopeKind(s string) (ScopeKind, error) {
	k := ScopeKind(strings.ToLower(strings.TrimSpace(s)))
	if !k.Valid() {
		return "", fmt.Errorf("%w: unknown scope kind %q (want global, project or provider)", ErrInvalidScope, s)
	}
	return k, nil
}

// Scope is one budget target: the whole installation, one project or one
// provider. ID is the stable identity of the project or provider, never a
// display label, and must be empty for ScopeGlobal.
type Scope struct {
	Kind ScopeKind
	ID   string
}

// IsGlobal reports whether the scope is the whole installation.
func (s Scope) IsGlobal() bool { return s.Kind == ScopeGlobal }

// Validate checks the scope kind and its stable id shape. A global scope must
// not carry an id, and a project or provider scope must carry one.
func (s Scope) Validate() error {
	switch s.Kind {
	case ScopeGlobal:
		if s.ID != "" {
			return fmt.Errorf("%w: global scope must not carry a stable id (got %q)", ErrInvalidScope, s.ID)
		}
	case ScopeProject, ScopeProvider:
		if err := validStableID(s.ID); err != nil {
			return fmt.Errorf("%w: %s scope: %v", ErrInvalidScope, s.Kind, err)
		}
	default:
		return fmt.Errorf("%w: unknown scope kind %q", ErrInvalidScope, s.Kind)
	}
	return nil
}

// String is the canonical scope key, used to match a rule to its spend and as
// part of the deterministic notification id: "global", "project:<id>" or
// "provider:<id>". An unknown kind yields "invalid:<kind>" rather than
// panicking; such a scope is rejected by Validate.
func (s Scope) String() string {
	switch s.Kind {
	case ScopeGlobal:
		return "global"
	case ScopeProject:
		return "project:" + s.ID
	case ScopeProvider:
		return "provider:" + s.ID
	}
	return "invalid:" + string(s.Kind)
}

// validStableID accepts a non-empty identity with no surrounding whitespace and
// no control characters. The id is opaque: it is never parsed, only compared.
func validStableID(id string) error {
	if id == "" {
		return errors.New("empty stable id")
	}
	if strings.TrimSpace(id) != id {
		return fmt.Errorf("stable id %q has surrounding whitespace", id)
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("stable id %q contains a control character", id)
		}
	}
	return nil
}

// ValidCurrency reports whether s is a canonical three-letter uppercase
// currency code. It is exact: "usd" and "US" are not valid. The package does
// not maintain a code list, so a well-formed but unknown code is accepted and
// simply never matches a different code.
func ValidCurrency(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}

// ParseCurrency normalizes user input (surrounding space and case) into a
// canonical uppercase currency code. Anything else is an error.
func ParseCurrency(s string) (string, error) {
	c := strings.ToUpper(strings.TrimSpace(s))
	if !ValidCurrency(c) {
		return "", fmt.Errorf("%w: %q (want a canonical three-letter code such as USD)", ErrInvalidCurrency, s)
	}
	return c, nil
}

// Rule is one configured budget rule.
//
// ID and Version form the rule generation: ID is the stable identity and
// Version must be bumped for every edit of Scope, Period, Currency, Limit or
// Thresholds. Label is display text only and Disabled only pauses evaluation;
// neither is part of the identity, so neither resets the dedup ledger.
type Rule struct {
	// ID is the stable caller-assigned rule identity. Required and non-empty.
	ID string
	// Version is the explicit rule generation. Required and at least 1.
	Version int
	// Scope is the budget target.
	Scope Scope
	// Period is the calendar period the limit applies to.
	Period reporttime.Kind
	// Currency is the currency of Limit, in canonical uppercase.
	Currency string
	// Limit is the period limit in Currency. Finite and greater than zero.
	Limit float64
	// Thresholds are percentages of Limit. Nil or empty means DefaultThresholds.
	Thresholds []float64
	// Disabled pauses evaluation while keeping the ledger entry.
	Disabled bool
	// Label is display text only. It never affects identity or dedup.
	Label string
}

// Validate rejects a rule whose identity, scope, period, currency, limit or
// thresholds are unusable. It never normalizes or repairs the rule.
func (r Rule) Validate() error {
	if err := validStableID(r.ID); err != nil {
		return fmt.Errorf("%w: id: %v", ErrInvalidRule, err)
	}
	if r.Version < 1 {
		return fmt.Errorf("%w: version %d must be at least 1", ErrInvalidRule, r.Version)
	}
	if err := r.Scope.Validate(); err != nil {
		// Both classifications are useful: the rule is unusable and the scope
		// is the reason.
		return fmt.Errorf("%w: %w", ErrInvalidRule, err)
	}
	if !r.Period.Valid() {
		return fmt.Errorf("%w: %q (want day, week or month)", ErrInvalidPeriod, r.Period)
	}
	if !ValidCurrency(r.Currency) {
		return fmt.Errorf("%w: %q must be a canonical three-letter uppercase code", ErrInvalidCurrency, r.Currency)
	}
	if invalidAmount(r.Limit) || r.Limit <= 0 {
		return fmt.Errorf("%w: limit %v must be finite and greater than zero", ErrInvalidAmount, r.Limit)
	}
	seen := make(map[float64]bool, len(r.Thresholds))
	for _, t := range r.Thresholds {
		if invalidThreshold(t) {
			return fmt.Errorf("%w: %v (want a finite percentage in (0, 100])", ErrInvalidThreshold, t)
		}
		if seen[t] {
			return fmt.Errorf("%w: duplicate threshold %v%%", ErrInvalidThreshold, t)
		}
		seen[t] = true
	}
	return nil
}

// EffectiveThresholds returns the rule's thresholds in ascending order, or the
// default 80% and 100% when none are configured. The result is a fresh slice.
func (r Rule) EffectiveThresholds() []float64 {
	src := r.Thresholds
	if len(src) == 0 {
		src = DefaultThresholds()
	}
	out := append([]float64(nil), src...)
	sort.Float64s(out)
	return out
}

// Fingerprint is the deterministic hash of the rule's identity-bearing fields:
// ID, Version, Scope, Period, Currency, Limit and effective thresholds. Label
// and Disabled are excluded on purpose, so a rename or a pause does not look
// like a rule change. Evaluate compares it to detect an edit that did not bump
// Version.
func (r Rule) Fingerprint() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%d\x00%s\x00%s\x00%s\x00%s\x00",
		ruleFingerprintTag, r.ID, r.Version, r.Scope.String(), r.Period, r.Currency,
		strconv.FormatFloat(r.Limit, 'g', -1, 64))
	for _, t := range r.EffectiveThresholds() {
		fmt.Fprintf(h, "%s\x00", strconv.FormatFloat(t, 'g', -1, 64))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// EstimatedSpend is one scope's locally estimated cost for one calendar period,
// in one explicit currency. It is never an actual provider balance or invoice.
//
// Period is mandatory and carries the whole provenance of the estimate: the
// period kind, the timezone and the exact half-open [Start, End) range it was
// aggregated over. A rule matches a spend only when the spend's period identity
// is exactly the rule's current period, so the same scope can carry separate
// day, week and month estimates without them ever being confused.
type EstimatedSpend struct {
	// Scope is the target this estimate is attributed to.
	Scope Scope
	// Period is the exact calendar period this estimate covers. It must be a
	// real period of its kind in its own location: Start is the local calendar
	// boundary, End is the next one, and both are validated with
	// internal/reporttime. There is no default period.
	Period reporttime.Period
	// Currency is the canonical currency of Amount.
	Currency string
	// Amount is the estimated cost of requests whose price is known. Finite
	// and non-negative.
	Amount float64
	// UnpricedRequests counts requests in the period whose price is unknown.
	// They are excluded from Amount and flagged on every evaluation that uses
	// this spend, so Amount is a lower bound.
	UnpricedRequests int64
}

// PeriodID is the spend's period identity, reporttime's own Period.ID(). It is
// derived, never stored separately, so a caller cannot claim one identity while
// carrying another range.
func (e EstimatedSpend) PeriodID() string { return e.Period.ID() }

// Validate rejects an unusable estimate. It never rounds, converts or repairs.
func (e EstimatedSpend) Validate() error {
	if err := e.Scope.Validate(); err != nil {
		return err
	}
	if err := validateSpendPeriod(e.Period); err != nil {
		return err
	}
	if !ValidCurrency(e.Currency) {
		return fmt.Errorf("%w: %q must be a canonical three-letter uppercase code", ErrInvalidCurrency, e.Currency)
	}
	if invalidAmount(e.Amount) || e.Amount < 0 {
		return fmt.Errorf("%w: amount %v must be finite and non-negative", ErrInvalidAmount, e.Amount)
	}
	if e.UnpricedRequests < 0 {
		return fmt.Errorf("%w: unpriced request count %d must be non-negative", ErrInvalidAmount, e.UnpricedRequests)
	}
	return nil
}

// validateSpendPeriod proves that p is a real calendar period of its own kind in
// its own location: reporttime must reproduce exactly this half-open range when
// asked for the period that contains p.Start. A zero, forged, truncated,
// overlong or mislabelled range is refused, so a caller can never present spend
// for one period under another period's identity.
//
// The package never defaults, infers or widens a period. If a caller cannot say
// which period an estimate covers, it has no usable estimate.
func validateSpendPeriod(p reporttime.Period) error {
	if p.Start.IsZero() || p.End.IsZero() {
		return fmt.Errorf("%w: period %+v has a zero bound; the covered range is required", ErrInvalidPeriod, p)
	}
	if !p.Kind.Valid() {
		return fmt.Errorf("%w: period kind %q is not day, week or month", ErrInvalidPeriod, p.Kind)
	}
	if !p.End.After(p.Start) {
		return fmt.Errorf("%w: period %s ends at %s, which is not after its start", ErrInvalidPeriod, p.ID(), p.End)
	}
	loc := p.Start.Location()
	resolved, err := reporttime.Resolve(p.Start, p.Kind, loc)
	if err != nil {
		return fmt.Errorf("%w: period %s cannot be resolved in %s: %v", ErrInvalidPeriod, p.ID(), loc, err)
	}
	current := resolved.Current
	if current.Kind != p.Kind || !current.Start.Equal(p.Start) || !current.End.Equal(p.End) {
		return fmt.Errorf("%w: %s is labelled %s but reporttime resolves its start to the %s period %s..%s in %s",
			ErrInvalidPeriod, p.ID(), p.Kind, current.Kind, current.Start, current.End, loc)
	}
	return nil
}

// Input is one evaluation request. Now and Location are injected: the package
// never reads the clock or time.Local. Location is the persisted statistics
// timezone, and period identity depends on it.
type Input struct {
	// Now is the evaluation instant. Must not be the zero time.
	Now time.Time
	// Location is the reporting timezone. Must not be nil.
	Location *time.Location
	// Rules are all currently configured rules, in any order.
	Rules []Rule
	// Spends are the local estimates, at most one per scope, period identity
	// and currency. The same scope may carry separate day, week and month
	// estimates, and separate currencies, which are never combined.
	Spends []EstimatedSpend
}

// Evaluation is the per-rule outcome of one evaluation. It is reported for
// every rule, including disabled rules and rules without spend data, so callers
// can show why nothing fired.
type Evaluation struct {
	RuleID      string
	RuleVersion int
	Label       string
	Scope       Scope
	Period      reporttime.Kind
	PeriodID    string
	Currency    string
	Limit       float64
	// Estimated is the estimated cost of priced requests in the period. It is
	// a lower bound when UnknownPricing is true.
	Estimated float64
	// UnpricedRequests counts the period's requests with no known price.
	UnpricedRequests int64
	// UnknownPricing is true when some requests in the period had no known
	// price, so Estimated and Percent are lower bounds.
	UnknownPricing bool
	// Percent is Estimated as a percentage of Limit. It is 0 when no spend data
	// is available and is never capped, so 150 means 150%.
	Percent float64
	// DataAvailable is false when the input had no spend for this rule's scope
	// and this exact period identity. Spend for the same scope in another
	// period, or in another currency, does not count: it is missing data, not a
	// zero estimate.
	DataAvailable bool
	// SkippedReason is SkipDisabled or SkipNoData when the rule was not
	// evaluated against spend, and empty otherwise.
	SkippedReason string
	// Crossed lists the effective thresholds reached in this period, ascending,
	// whether or not they were already notified.
	Crossed []float64
}

// Notification is one threshold notification reserved by Evaluate. The caller
// persists State and then delivers it; see the package documentation for the
// save/deliver ordering tradeoff.
type Notification struct {
	// ID is the deterministic reservation id. It is stable for one rule
	// generation, period and threshold.
	ID             string
	RuleID         string
	RuleVersion    int
	Label          string
	Scope          Scope
	Period         reporttime.Kind
	PeriodID       string
	Threshold      float64
	Currency       string
	Limit          float64
	Estimated      float64
	Percent        float64
	UnknownPricing bool
	// ReservedAt is the evaluation instant rendered in the reporting timezone.
	ReservedAt time.Time
}

// RuleState is one rule generation's persisted ledger entry.
type RuleState struct {
	// Version is the rule generation last evaluated.
	Version int
	// Fingerprint is the identity hash of that generation.
	Fingerprint string
	// LastPeriodID is the period of the most recent evaluation.
	LastPeriodID string
	// Watermark is the start instant of the most recent period evaluated for
	// this generation. It is a durable, monotonic high-water mark: an
	// evaluation whose resolved period starts before it is refused with
	// ErrEvaluationRegressed. It is reset only by an explicit version bump,
	// which is a new generation by definition.
	Watermark time.Time
}

// Reservation is one reserved notification id in the dedup ledger.
type Reservation struct {
	ID          string
	RuleID      string
	RuleVersion int
	PeriodID    string
	Threshold   float64
	ReservedAt  time.Time
}

// State is the persisted reserve/dedup ledger. Evaluate never mutates the State
// it is given; it returns a deep copy.
type State struct {
	// Version is the state schema version, always SchemaVersion when written.
	Version int
	// Rules tracks the last evaluated generation and period per rule ID.
	Rules map[string]RuleState
	// Reservations is keyed by deterministic notification id.
	Reservations map[string]Reservation
}

// Result is one evaluation's next state and outputs.
type Result struct {
	// State is the next ledger, safe to persist. The input State is untouched.
	State State
	// Evaluations has one entry per rule, sorted by rule ID.
	Evaluations []Evaluation
	// Notifications holds only the notifications newly reserved by this call,
	// sorted by rule ID and then threshold. Delivery is the caller's job.
	Notifications []Notification
}

// NotificationID is the deterministic identity of one notification: the rule
// generation, the scope, the period identity and the threshold. Label is not
// part of it, so renaming a rule cannot re-notify, and PeriodID already carries
// the kind, the timezone and the actual start instant.
func NotificationID(r Rule, periodID string, threshold float64) string {
	return fmt.Sprintf("%s:%s:%d:%s:%s:%s", notificationIDTag, r.ID, r.Version, r.Scope.String(), periodID, strconv.FormatFloat(threshold, 'g', -1, 64))
}

// invalidAmount reports whether a finite, usable money value is impossible.
func invalidAmount(v float64) bool {
	return math.IsNaN(v) || math.IsInf(v, 0)
}

// invalidThreshold reports whether a threshold percentage is unusable.
func invalidThreshold(v float64) bool {
	return math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > 100
}
