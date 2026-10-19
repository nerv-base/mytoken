package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Source labels a Snapshot's origin. Every provider fetches its quota from the
// vendor's server, so a snapshot is always SourceAPI: a value read from a local
// usage log is never a quota.
const (
	// SourceAPI is a reading fetched from the vendor's usage endpoint.
	SourceAPI = "api"
)

// Provider is one plan-quota source.
//
// Detect must be cheap: it reports whether the tool looks signed in (a
// credential is present and readable) and performs no network request. Fetch
// performs the actual read; it is called only after Detect returned true, on
// the Service's schedule, and must return a sanitized error (see Error).
type Provider interface {
	// ID is a stable, lowercase identifier used in settings keys and by the UI.
	ID() string
	// DisplayName is the human-readable tool name.
	DisplayName() string
	// Detect reports whether the tool's credential is present.
	Detect(ctx context.Context) bool
	// Fetch reads the current quota and returns it as a Snapshot.
	Fetch(ctx context.Context) (Snapshot, error)
}

// Reasoner is an error that names a stable, sanitized failure cause. The
// Service stores the code instead of the error text, so a failure is shown as
// one of a fixed set of reasons.
type Reasoner interface {
	QuotaReason() string
}

// RetryDelayer is an error that asks the Service to wait before the next
// attempt (for example the Retry-After of a 429 response).
type RetryDelayer interface {
	RetryDelay() time.Duration
}

// Window is one quota window of a plan.
type Window struct {
	// Name is the window's label: "5h", "week", "month", or a provider-specific
	// suffix such as "week-opus".
	Name string `json:"name"`
	// UsedPercent is how much of the window is consumed, 0-100 (values above
	// 100 mean the account is over its allowance).
	UsedPercent float64 `json:"used_percent"`
	// Used and Limit are absolute amounts when the provider states them; both
	// are zero for a percentage-only window.
	Used  float64 `json:"used,omitempty"`
	Limit float64 `json:"limit,omitempty"`
	// Unit is the unit of Used and Limit ("USD", "requests", "%", ...). It is
	// empty for a percentage-only window.
	Unit string `json:"unit,omitempty"`
	// ResetsAt is the stated reset instant; the zero value means unstated.
	ResetsAt time.Time `json:"resets_at,omitzero"`
	// Balance marks a credit balance rather than a fixed periodic allowance.
	// Remaining is then authoritative, Limit may be zero, and UsedPercent must
	// not be shown as a percentage.
	Balance bool `json:"balance,omitempty"`
	// Remaining is what is left, in Unit. It is authoritative for a Balance
	// window and is filled in for a capped window when Limit is known
	// (Limit-Used, clamped at zero).
	Remaining float64 `json:"remaining,omitempty"`
	// ExpiresAt is the soonest expiry of the remaining credits, when the
	// provider states one. The zero value means unstated.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// DayUse is the balance consumed on one local calendar day.
type DayUse struct {
	// Day is the start of the local calendar day.
	Day time.Time `json:"day"`
	// Used is the amount consumed that day, in the balance window's unit.
	Used float64 `json:"used,omitempty"`
	// Known reports whether a reading exists for the day. A false value means
	// the day had no data and Used is not meaningful (and is zero).
	Known bool `json:"known,omitempty"`
}

// Valid reports whether the window carries a usable label.
func (w Window) Valid() bool { return strings.TrimSpace(w.Name) != "" }

// DayStart returns the start of t's local calendar day.
func DayStart(t time.Time) time.Time {
	y, m, d := t.In(time.Local).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
}

// Snapshot is one provider's quota at one instant. It holds quota numbers and
// instants only — never a credential.
type Snapshot struct {
	// Provider is the Provider.ID that produced the snapshot.
	Provider string `json:"provider"`
	// Plan is the subscription tier when the provider states one.
	Plan string `json:"plan,omitempty"`
	// Windows are the quota windows, in display order.
	Windows []Window `json:"windows,omitempty"`
	// Daily is the per-day consumption of the balance window for the last 14
	// local days, oldest first. A provider may supply it; otherwise the Service
	// derives it from the credit-balance history. Known is false for a day
	// without data.
	Daily []DayUse `json:"daily,omitempty"`
	// FetchedAt is when the request that produced the reading was made.
	FetchedAt time.Time `json:"fetched_at"`
	// Source is SourceAPI.
	Source string `json:"source,omitempty"`
}

// Clone returns a deep copy, so a caller that mutates the windows cannot
// change the Service's cached snapshot.
func (s Snapshot) Clone() Snapshot {
	out := s
	if s.Windows != nil {
		out.Windows = append([]Window(nil), s.Windows...)
	}
	if s.Daily != nil {
		out.Daily = append([]DayUse(nil), s.Daily...)
	}
	return out
}

// normalize applies the invariants of the window: a balance window is never a
// percentage, and a capped window with a stated limit exposes what is left.
func (w *Window) normalize() {
	if w.Balance {
		w.UsedPercent = 0
		return
	}
	if w.Limit > 0 {
		remaining := w.Limit - w.Used
		if remaining < 0 {
			remaining = 0
		}
		w.Remaining = remaining
	}
}

// normalizeDaily puts provider-supplied day usage in ascending local-day order,
// drops duplicates and keeps the most recent 14 days.
func normalizeDaily(in []DayUse) []DayUse {
	if len(in) == 0 {
		return nil
	}
	byDay := make(map[int64]DayUse, len(in))
	for _, d := range in {
		d.Day = DayStart(d.Day)
		if !d.Known {
			d.Used = 0
		}
		byDay[d.Day.Unix()] = d
	}
	out := make([]DayUse, 0, len(byDay))
	for _, d := range byDay {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day.Before(out[j].Day) })
	if len(out) > 14 {
		out = out[len(out)-14:]
	}
	return out
}

// Validate normalizes a snapshot and checks its invariants. It is what a
// snapshot read back from the settings store must pass before it is treated as
// a quota. A missing fetch time is preserved: reading a stored snapshot must
// not invent a time it did not record.
func (s *Snapshot) Validate() error {
	s.Provider = strings.TrimSpace(s.Provider)
	if s.Provider == "" {
		return fmt.Errorf("plan: provider returned a snapshot without an id")
	}
	if len(s.Windows) == 0 {
		return &Error{Reason: ReasonNoWindows}
	}
	kept := s.Windows[:0]
	for _, w := range s.Windows {
		if !w.Valid() {
			continue
		}
		w.normalize()
		kept = append(kept, w)
	}
	s.Windows = kept
	if len(s.Windows) == 0 {
		return &Error{Reason: ReasonNoWindows}
	}
	s.Daily = normalizeDaily(s.Daily)
	if s.Source != "" && s.Source != SourceAPI {
		// A snapshot that names any other origin (a local usage log, a cache)
		// is not a quota: the numbers must have come from the provider.
		return fmt.Errorf("plan: snapshot source %q is not %q", s.Source, SourceAPI)
	}
	s.Source = SourceAPI
	return nil
}

// validate normalizes and checks a snapshot a provider just returned, stamping
// the fetch time when the provider did not state one.
func (s *Snapshot) validate(now time.Time) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if s.FetchedAt.IsZero() {
		s.FetchedAt = now.UTC()
	}
	return nil
}

// DecodeSnapshot decodes a snapshot persisted under SnapshotKey and applies the
// same validation as a snapshot returned by a provider. The stored encoding is
// Snapshot's own JSON (snake_case fields, including fetched_at), so a value the
// Service wrote is read back with its fetch time intact. A snapshot that names
// a non-API source is rejected instead of being shown as a quota.
func DecodeSnapshot(raw []byte) (Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return Snapshot{}, err
	}
	if err := s.Validate(); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Registry is an ordered set of providers with unique ids.
type Registry struct {
	list []Provider
	byID map[string]Provider
}

// NewRegistry validates and indexes providers. Ids must be non-empty, at most
// 32 lowercase letters, digits or dashes, and unique.
func NewRegistry(providers ...Provider) (*Registry, error) {
	r := &Registry{byID: make(map[string]Provider, len(providers))}
	for _, p := range providers {
		if p == nil {
			return nil, fmt.Errorf("plan: nil provider")
		}
		id := p.ID()
		if !idPattern.MatchString(id) {
			return nil, fmt.Errorf("plan: invalid provider id %q", id)
		}
		if _, ok := r.byID[id]; ok {
			return nil, fmt.Errorf("plan: duplicate provider id %q", id)
		}
		r.byID[id] = p
		r.list = append(r.list, p)
	}
	return r, nil
}

// Providers returns the providers in registration order.
func (r *Registry) Providers() []Provider {
	if r == nil {
		return nil
	}
	return append([]Provider(nil), r.list...)
}

// Lookup returns the provider with id.
func (r *Registry) Lookup(id string) (Provider, bool) {
	if r == nil {
		return nil, false
	}
	p, ok := r.byID[id]
	return p, ok
}

// Detected returns the providers whose credential is present.
func (r *Registry) Detected(ctx context.Context) []Provider {
	if r == nil {
		return nil
	}
	out := make([]Provider, 0, len(r.list))
	for _, p := range r.list {
		if p.Detect(ctx) {
			out = append(out, p)
		}
	}
	return out
}

// sortSnapshots orders snapshots by the provider order of the registry that
// produced them; ids that are not registered sort last by id.
func sortSnapshots(order []string, snaps []Snapshot) {
	rank := make(map[string]int, len(order))
	for i, id := range order {
		rank[id] = i
	}
	sort.SliceStable(snaps, func(i, j int) bool {
		ri, okI := rank[snaps[i].Provider]
		rj, okJ := rank[snaps[j].Provider]
		switch {
		case okI && okJ:
			return ri < rj
		case okI != okJ:
			return okI
		default:
			return snaps[i].Provider < snaps[j].Provider
		}
	})
}
