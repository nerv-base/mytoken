package snapshot

import (
	"context"
	"time"

	"github.com/zzstar101/mytoken/internal/claudequota"
	"github.com/zzstar101/mytoken/internal/paths"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/quota"
	"github.com/zzstar101/mytoken/internal/store"
)

// Claude quota reasons. They sit next to the Codex quota reasons so a consumer
// can tell "nothing was ever consented" (the default state) apart from "consent
// was revoked" and from "consented but nothing fetched yet".
const (
	// ReasonClaudeQuotaNoConsent means no instance has a stored consent, which
	// is the default: Claude quota is off until it is explicitly enabled.
	ReasonClaudeQuotaNoConsent = "no_consent"
	// ReasonClaudeQuotaNotEnabled means a consent row exists but is not usable
	// (cleared, or written by another schema version).
	ReasonClaudeQuotaNotEnabled = "not_enabled"
	// ReasonClaudeQuotaConsentInvalid means a consent row exists but cannot be
	// decoded.
	ReasonClaudeQuotaConsentInvalid = "consent_invalid"
	// ReasonClaudeQuotaNoReading means a usable consent exists but nothing has
	// been fetched yet, so no window may be shown.
	ReasonClaudeQuotaNoReading = "no_reading"
)

// ClaudeQuota is one instance's cached Claude Code quota view, built only from
// the persisted consent and reading. Nothing here reads a credential, opens the
// network, starts a service or writes.
//
// The credential source reference of the consent is deliberately not carried:
// a file source's reference is a path under the user's home, and the snapshot
// is a plaintext envelope. The explicit `quota claude info` command discloses
// it; every cached display shows only the source kind.
type ClaudeQuota struct {
	// Instance is the instance the consent and reading belong to.
	Instance string `json:"instance"`
	// Enabled is true when a usable consent record is stored for this
	// instance. It does not claim the credential source is still readable —
	// that needs the CLI's source resolution and is never assumed here.
	Enabled bool `json:"enabled"`
	// Consent describes the stored grant; nil when nothing is stored.
	Consent *ClaudeConsent `json:"consent,omitempty"`
	// Status is the availability verdict for the cached reading.
	Status Status `json:"status"`
	// FetchedAt is when a fetch last succeeded; nil when none ever did.
	FetchedAt *time.Time `json:"fetchedAt,omitempty"`
	// AttemptedAt is when a fetch was last tried, successful or not.
	AttemptedAt *time.Time `json:"attemptedAt,omitempty"`
	// AgeSeconds is how old FetchedAt was when this view was built. It is the
	// age of the reading, not a claim that the numbers are current.
	AgeSeconds *int64 `json:"ageSeconds,omitempty"`
	// StaleAfterSeconds is the reading's own freshness budget, so a consumer
	// can judge AgeSeconds without hard-coding the package's rule.
	StaleAfterSeconds int64 `json:"staleAfterSeconds,omitempty"`
	// ReadingStatus and ReadingReason are the stored reading's own verdict.
	// Stale is reported as stale and never upgraded to ok.
	ReadingStatus string `json:"readingStatus,omitempty"`
	ReadingReason string `json:"readingReason,omitempty"`
	// Windows are the stated windows. A window the endpoint did not state
	// carries Present=false and no percentage, never 0%.
	Windows []claudequota.Window `json:"windows,omitempty"`
	// Forecast is the pace picture recomputed at GeneratedAt from the reading's
	// own retained samples. It is omitted when the reading carries none; it is
	// never derived from a single percentage.
	Forecast *quota.Forecast `json:"forecast,omitempty"`
}

// ClaudeConsent is the non-secret part of the stored consent.
type ClaudeConsent struct {
	Version    int        `json:"version"`
	GrantedAt  *time.Time `json:"grantedAt,omitempty"`
	SourceKind string     `json:"sourceKind,omitempty"`
	Target     string     `json:"target,omitempty"`
}

// ReadClaudeQuota reads the persisted Claude Code consent and cached readings
// offline and returns one entry per stored (or explicitly requested) instance,
// plus the verdict for the section as a whole.
//
// dbPath overrides the resolved index path; empty means paths.ResolveDBPath.
// instanceIDs is the caller's instance filter; empty means every stored
// instance. The index is opened read-only, nothing is written, no credential is
// read and no network request is made, so this is safe next to a running GUI.
func ReadClaudeQuota(ctx context.Context, dbPath string, instanceIDs []string, now time.Time) ([]ClaudeQuota, Status, error) {
	path := dbPath
	if path == "" {
		p, err := paths.ResolveDBPath()
		if err != nil {
			return nil, Status{}, err
		}
		path = p
	}
	ro, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		entries := BuildClaudeQuota(query.MissingClaudeQuotaObservations(instanceIDs,
			"the index could not be read; nothing was created or opened writable"), now)
		return entries, Status{Reason: quotaIndexReason(err), Detail: err.Error()}, nil
	}
	defer ro.Close()
	obs, err := query.ReadClaudeQuota(ctx, ro, instanceIDs)
	if err != nil {
		return nil, Status{}, err
	}
	entries := BuildClaudeQuota(obs, now)
	return entries, ClaudeQuotaSectionStatus(entries), nil
}

// BuildClaudeQuota renders read-only observations as snapshot entries. It is
// exported so the snapshot, the statusline and the CLI render one shape from
// one place.
func BuildClaudeQuota(obs []query.ClaudeQuotaObservation, now time.Time) []ClaudeQuota {
	out := make([]ClaudeQuota, 0, len(obs))
	for _, o := range obs {
		out = append(out, claudeQuotaEntry(o, now))
	}
	return out
}

// ClaudeQuotaSectionStatus is the whole-section verdict for
// Sources.ClaudeQuota. It is available only when at least one instance has a
// usable consent and a usable reading; the default (nothing consented) is a
// reason, not a zero.
func ClaudeQuotaSectionStatus(entries []ClaudeQuota) Status {
	if len(entries) == 0 {
		return Status{Reason: ReasonClaudeQuotaNoConsent, Detail: "no instance has a stored Claude quota consent"}
	}
	enabled, usable := 0, 0
	for _, e := range entries {
		if e.Enabled {
			enabled++
		}
		if e.Status.Available {
			usable++
		}
	}
	if usable > 0 {
		return Status{Available: true}
	}
	if enabled == 0 {
		return Status{Reason: ReasonClaudeQuotaNotEnabled, Detail: "Claude quota is disabled for every stored instance; it is off by default"}
	}
	// Prefer an enabled instance's own refusal over a disabled sibling's.
	for _, e := range entries {
		if e.Enabled && e.Status.Reason != "" {
			return Status{Reason: e.Status.Reason, Detail: e.Status.Detail, FetchedAt: e.Status.FetchedAt}
		}
	}
	return Status{Reason: ReasonClaudeQuotaNoReading, Detail: "no instance has a usable cached Claude quota reading"}
}

func claudeQuotaEntry(o query.ClaudeQuotaObservation, now time.Time) ClaudeQuota {
	entry := ClaudeQuota{Instance: o.Instance, StaleAfterSeconds: int64(claudequota.StaleAfter / time.Second)}
	if o.ConsentValid {
		entry.Enabled = true
		c := &ClaudeConsent{
			Version:    o.Consent.Version,
			SourceKind: string(o.Consent.SourceKind),
			Target:     o.Consent.Target,
		}
		if !o.Consent.GrantedAt.IsZero() {
			t := o.Consent.GrantedAt.UTC()
			c.GrantedAt = &t
		}
		entry.Consent = c
	}

	switch {
	case !o.ConsentValid:
		if o.ConsentCorrupt {
			entry.Status = Status{Reason: ReasonClaudeQuotaConsentInvalid, Detail: o.ConsentDetail}
			entry.ReadingReason = ReasonClaudeQuotaConsentInvalid
			return entry
		}
		if !o.ConsentPresent {
			entry.Status = Status{Reason: ReasonClaudeQuotaNoConsent, Detail: "Claude quota is not enabled for this instance"}
			entry.ReadingReason = ReasonClaudeQuotaNoConsent
			return entry
		}
		entry.Status = Status{Reason: ReasonClaudeQuotaNotEnabled, Detail: o.ConsentDetail}
		entry.ReadingReason = ReasonClaudeQuotaNotEnabled
		return entry
	case o.QuotaCorrupt:
		entry.Status = Status{Reason: ReasonQuotaCorrupt, Detail: o.QuotaDetail}
		entry.ReadingReason = ReasonQuotaCorrupt
		return entry
	case !o.QuotaPresent:
		entry.Status = Status{Reason: ReasonClaudeQuotaNoReading, Detail: "Claude quota is enabled but nothing has been fetched yet"}
		entry.ReadingReason = ReasonClaudeQuotaNoReading
		return entry
	}

	q := o.Quota
	entry.ReadingStatus = string(q.Status)
	entry.ReadingReason = string(q.Reason)
	if !q.FetchedAt.IsZero() {
		t := q.FetchedAt.UTC()
		entry.FetchedAt = &t
		if age := now.Sub(t); age >= 0 {
			secs := int64(age / time.Second)
			entry.AgeSeconds = &secs
		}
	}
	if !q.AttemptedAt.IsZero() {
		t := q.AttemptedAt.UTC()
		entry.AttemptedAt = &t
	}
	entry.Windows = q.Windows
	// Recompute the pace at this instant from the reading's own retained
	// samples: exporting a stored instant as if it were now would be wrong, and
	// a reading with no samples yields a refusal, not a number.
	entry.Forecast = trimForecast(claudequota.Forecasts(q, now))

	switch q.Status {
	case claudequota.StatusOK:
		entry.Status = Status{Available: true, FetchedAt: entry.FetchedAt}
	case claudequota.StatusStale:
		entry.Status = Status{
			Available: true,
			Reason:    string(claudequota.StatusStale),
			Detail:    "the cached reading is older than its freshness budget; it is shown as stale, not as current",
			FetchedAt: entry.FetchedAt,
		}
	default:
		reason := string(q.Reason)
		if reason == "" {
			reason = string(claudequota.StatusUnavailable)
		}
		detail := "no successful Claude quota fetch is stored for this instance"
		if q.Reason != "" {
			detail = "the last explicit refresh failed; the stored reason is " + string(q.Reason)
		}
		entry.Status = Status{Reason: reason, Detail: detail, FetchedAt: entry.FetchedAt}
	}
	return entry
}

// trimForecast copies a forecast with every provenance label dropped. The
// retained samples' source is the credential source reference, which is a path
// for file sources and must not reach a plaintext envelope.
func trimForecast(f *quota.Forecast) *quota.Forecast {
	if f == nil {
		return nil
	}
	out := *f
	out.Limitations = append([]string(nil), f.Limitations...)
	out.Windows = make([]quota.WindowForecast, 0, len(f.Windows))
	for _, w := range f.Windows {
		w.Source = ""
		out.Windows = append(out.Windows, w)
	}
	return &out
}
