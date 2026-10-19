package plan

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Source labels a Snapshot's origin.
const (
	// SourceLocalLog is a reading taken from a log the tool wrote itself.
	SourceLocalLog = "local-log"
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

// LocalProvider is a Provider that reads only state already on this machine.
// A Service in offline mode still polls it.
type LocalProvider interface {
	Local() bool
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
}

// Valid reports whether the window carries a usable label.
func (w Window) Valid() bool { return strings.TrimSpace(w.Name) != "" }

// Snapshot is one provider's quota at one instant. It holds quota numbers and
// instants only — never a credential.
type Snapshot struct {
	// Provider is the Provider.ID that produced the snapshot.
	Provider string `json:"provider"`
	// Plan is the subscription tier when the provider states one.
	Plan string `json:"plan,omitempty"`
	// Windows are the quota windows, in display order.
	Windows []Window `json:"windows,omitempty"`
	// FetchedAt is when the reading was taken (provider-stated for a local log,
	// the request time for an API call).
	FetchedAt time.Time `json:"fetched_at"`
	// Source is SourceLocalLog or SourceAPI.
	Source string `json:"source,omitempty"`
}

// Clone returns a deep copy, so a caller that mutates the windows cannot
// change the Service's cached snapshot.
func (s Snapshot) Clone() Snapshot {
	out := s
	if s.Windows != nil {
		out.Windows = append([]Window(nil), s.Windows...)
	}
	return out
}

// validate normalizes and checks a snapshot a provider returned.
func (s *Snapshot) validate(now time.Time) error {
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
		kept = append(kept, w)
	}
	s.Windows = kept
	if len(s.Windows) == 0 {
		return &Error{Reason: ReasonNoWindows}
	}
	if s.FetchedAt.IsZero() {
		s.FetchedAt = now.UTC()
	}
	if s.Source == "" {
		s.Source = SourceAPI
	}
	return nil
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
