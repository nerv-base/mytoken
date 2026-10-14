package snapshot

import (
	"context"
	"time"

	"github.com/zzstar101/mytoken/internal/paths"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/quota"
	"github.com/zzstar101/mytoken/internal/store"
)

// Extra quota reasons. They sit next to the existing Reason* constants so a
// consumer can tell "nothing was ever stored" apart from "stored but unusable"
// and from "a pace needs a second observation".
const (
	// ReasonQuotaNoObservation means no quota.codex.<instance> key exists.
	ReasonQuotaNoObservation = "no_observation"
	// ReasonQuotaCorrupt means a key exists but is not a usable observation.
	ReasonQuotaCorrupt = "corrupt_stored"
	// ReasonQuotaUnavailable means a stored observation exists but reports no
	// usable window; the report's own reason travels in Detail.
	ReasonQuotaUnavailable = "unavailable"
	// ReasonQuotaNoWindow means the observation is usable but carries no
	// window, so no window or reset may be shown.
	ReasonQuotaNoWindow = "no_usable_window"
	// ReasonQuotaSingleObservation means exactly one observation was stored, so
	// a pace is not available. It is never filled in from token counts.
	ReasonQuotaSingleObservation = "single_observation"
)

// CodexQuota is one instance's offline Codex quota view, built only from the
// persisted observation. Nothing here reads a credential, opens the network,
// starts a scan or writes: an instance with no observation is reported
// unavailable instead of being dropped or filled in from another instance.
//
// Instances are never combined. Two instances are two accounts with their own
// credit units, so no field here is a sum or an average across instances.
type CodexQuota struct {
	// Instance is the instance the observation was stored under.
	Instance string `json:"instance"`
	// Harness is the harness the observation belongs to.
	Harness string `json:"harness"`
	// Key is the stored settings key this entry came from.
	Key string `json:"key,omitempty"`
	// Available is true when the stored observation is usable, i.e. its report
	// is ok or stale. An unavailable entry still carries its reason and detail.
	Available bool   `json:"available"`
	Status    Status `json:"status"`
	// Source is the provenance label the writer recorded (a rollout path).
	Source string `json:"source,omitempty"`
	// ObservedAt is the provider instant the numbers describe, nil when the log
	// carried no trustworthy timestamp: a reset is then never invented from it.
	ObservedAt *time.Time `json:"observedAt,omitempty"`
	// IngestedAt is when the observation was persisted.
	IngestedAt *time.Time `json:"ingestedAt,omitempty"`
	// ReportStatus/ReportReason are the stored report's own verdict. Stale is
	// reported as stale and never upgraded to ok.
	ReportStatus string `json:"reportStatus,omitempty"`
	ReportReason string `json:"reportReason,omitempty"`
	LimitID      string `json:"limitId,omitempty"`
	LimitName    string `json:"limitName,omitempty"`
	// OtherLimitIDs are limit IDs seen on other records; they are never merged
	// into LimitID.
	OtherLimitIDs []string `json:"otherLimitIds,omitempty"`
	PlanType      *string  `json:"planType,omitempty"`
	// Credits is the observation's own credits block, per instance. Its balance
	// string keeps the provider's unit because the log states none.
	Credits *QuotaCredits `json:"credits,omitempty"`
	// Primary and Secondary are nil unless the stored observation carried that
	// window, which is what "show windows only when supported" means here.
	Primary   *QuotaWindow `json:"primary,omitempty"`
	Secondary *QuotaWindow `json:"secondary,omitempty"`
	Issues    []QuotaIssue `json:"issues,omitempty"`
	// Forecast is the pace verdict; see QuotaForecast.
	Forecast QuotaForecast `json:"forecast"`
	// Pace is the pace forecast the writer retained in the stored envelope,
	// exposed additively and verbatim: its per-window rates, the instant its
	// own observation was taken, and its own refusal reasons. It is nil when
	// the envelope carried none (a value written before observation retention
	// existed), and a reader must show that as absent rather than compute one.
	Pace *quota.Forecast `json:"pace,omitempty"`
}

// QuotaWindow is one stored quota window, rendered with the snapshot's naming.
// Every optional value stays a pointer: an unknown percentage is nil, never 0.
type QuotaWindow struct {
	Kind   string `json:"kind"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	// UsedPercent is 0..100 as reported.
	UsedPercent *float64 `json:"usedPercent,omitempty"`
	// RemainingPercent is 0..100; RemainingDerived marks a value computed from
	// UsedPercent rather than stated by the log.
	RemainingPercent *float64 `json:"remainingPercent,omitempty"`
	RemainingDerived bool     `json:"remainingDerived,omitempty"`
	// WindowMinutes is the window length in minutes when stated.
	WindowMinutes *int64 `json:"windowMinutes,omitempty"`
	// ResetsInSeconds is the log's own countdown, kept verbatim.
	ResetsInSeconds *int64 `json:"resetsInSeconds,omitempty"`
	// ResetsAt is the reset instant, explicit or derived from ObservedAt +
	// ResetsInSeconds. nil when neither existed.
	ResetsAt *time.Time `json:"resetsAt,omitempty"`
}

// QuotaCredits mirrors the observation's credits block verbatim.
type QuotaCredits struct {
	HasCredits *bool `json:"hasCredits,omitempty"`
	Unlimited  *bool `json:"unlimited,omitempty"`
	// Balance is the provider's own string. Its unit is unstated, so it is
	// never converted and never summed across instances.
	Balance *string `json:"balance,omitempty"`
}

// QuotaIssue is one class of unusable input the reader recorded.
type QuotaIssue struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
	Count  int    `json:"count"`
	Line   int    `json:"line,omitempty"`
}

// QuotaForecast is the pace verdict for one instance.
//
// A pace needs two comparable observations with a resettable, non-decreasing
// window between them. The index persists exactly one observation per instance,
// so this is normally unavailable with Reason single_observation. It is never
// derived from token counts, from a single usage percentage, or from another
// instance's observation.
type QuotaForecast struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// ReadCodexQuota reads the persisted Codex quota observations offline and
// returns one entry per stored (or explicitly requested) instance, plus the
// verdict for the quota section as a whole.
//
// dbPath overrides the resolved index path; empty means paths.ResolveDBPath.
// instanceIDs is the caller's instance filter; empty means every stored
// instance. The index is opened read-only and nothing is written, so this is
// safe while another process holds the index.
func ReadCodexQuota(ctx context.Context, dbPath string, instanceIDs []string) ([]CodexQuota, Status, error) {
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
		// An index that cannot be read is not an operational error for an
		// offline view: the store's own refusal reason becomes the section
		// verdict, an explicitly requested instance stays visible as missing,
		// and nothing is created, migrated or opened writable.
		entries := BuildCodexQuota(query.MissingQuotaObservations(instanceIDs,
			"the index could not be read; nothing was created or opened writable"))
		return entries, Status{Reason: quotaIndexReason(err), Detail: err.Error()}, nil
	}
	defer ro.Close()
	obs, err := query.ReadQuota(ctx, ro, instanceIDs)
	if err != nil {
		return nil, Status{}, err
	}
	entries := BuildCodexQuota(obs)
	return entries, QuotaSectionStatus(entries), nil
}

// quotaIndexReason names why the index could not be read, reusing the store's
// stable refusal codes so the CLI and the snapshot report one vocabulary.
func quotaIndexReason(err error) string {
	if reason, ok := store.ReadOnlyReasonOf(err); ok {
		return string(reason)
	}
	return string(store.ReasonUnavailable)
}

// BuildCodexQuota renders read-only observations as snapshot entries. It is
// exported so the snapshot and the CLI render one shape from one place.
func BuildCodexQuota(obs []query.QuotaObservation) []CodexQuota {
	out := make([]CodexQuota, 0, len(obs))
	for _, o := range obs {
		out = append(out, codexQuotaEntry(o))
	}
	return out
}

// QuotaSectionStatus is the whole-section verdict for Sources.Quota.
func QuotaSectionStatus(entries []CodexQuota) Status {
	if len(entries) == 0 {
		return Status{Reason: ReasonQuotaNoObservation, Detail: "no instance has a persisted Codex quota observation"}
	}
	usable := 0
	for _, e := range entries {
		if e.Available {
			usable++
		}
	}
	if usable == 0 {
		return Status{Reason: ReasonQuotaNoObservation, Detail: "stored observations exist but none is usable"}
	}
	return Status{Available: true}
}

func codexQuotaEntry(o query.QuotaObservation) CodexQuota {
	q := CodexQuota{Instance: o.Instance, Harness: quota.HarnessCodex, Key: o.Key}
	switch {
	case o.Missing:
		q.Status = Status{Reason: ReasonQuotaNoObservation, Detail: o.Detail}
		q.Forecast = QuotaForecast{Reason: ReasonQuotaNoObservation, Detail: "no stored observation to forecast from"}
		return q
	case o.Corrupt:
		q.Status = Status{Reason: ReasonQuotaCorrupt, Detail: o.Detail}
		q.Forecast = QuotaForecast{Reason: ReasonQuotaCorrupt, Detail: "the stored observation cannot be decoded"}
		return q
	}
	if o.IngestedKnown && !o.IngestedAt.IsZero() {
		t := o.IngestedAt.UTC()
		q.IngestedAt = &t
	}
	rep := o.Report
	q.Source = o.Source
	if rep.Source != "" {
		q.Source = rep.Source
	}
	if rep.ObservedAt != nil {
		t := rep.ObservedAt.UTC()
		q.ObservedAt = &t
	}
	q.ReportStatus = string(rep.Status)
	q.ReportReason = string(rep.Reason)
	q.LimitID = rep.LimitID
	q.LimitName = rep.LimitName
	q.OtherLimitIDs = rep.OtherLimitIDs
	q.PlanType = rep.PlanType
	if rep.Credits != nil {
		if c, ok := creditsOf(*rep.Credits); ok {
			q.Credits = &c
		}
	}
	q.Primary = quotaWindowOf(rep.Primary)
	q.Secondary = quotaWindowOf(rep.Secondary)
	q.Issues = issuesOf(rep.Issues)
	q.Available = rep.Available()
	if q.Available {
		q.Status = Status{Available: true}
	} else {
		detail := string(rep.Reason)
		if detail == "" {
			detail = "the stored observation reports no usable window"
		}
		q.Status = Status{Reason: ReasonQuotaUnavailable, Detail: detail}
	}
	q.Forecast = forecastOf(q)
	// A stored pace forecast is authoritative when the envelope carries one:
	// it was derived from retained comparable observations, which this reader
	// cannot reconstruct from a single report. Without one, the refusal above
	// stands and nothing is invented.
	if o.Forecast != nil {
		q.Pace = o.Forecast
		if o.Forecast.Available {
			q.Forecast = QuotaForecast{Available: true}
		} else if o.Forecast.Reason != "" {
			q.Forecast = QuotaForecast{
				Reason: string(o.Forecast.Reason),
				Detail: "the stored pace forecast is unavailable",
			}
		}
	}
	return q
}

// forecastOf decides the pace verdict. A pace is offered only when two
// comparable observations exist; the persisted envelope holds one, so the
// answer is a precise refusal rather than a number invented from usage counts.
func forecastOf(q CodexQuota) QuotaForecast {
	if !q.Available {
		return QuotaForecast{Reason: q.Status.Reason, Detail: q.Status.Detail}
	}
	if q.Primary == nil && q.Secondary == nil {
		return QuotaForecast{
			Reason: ReasonQuotaNoWindow,
			Detail: "the stored observation carries no window, so there is nothing to pace",
		}
	}
	return QuotaForecast{
		Reason: ReasonQuotaSingleObservation,
		Detail: "the index stores one observation per instance; a pace needs two comparable observations and is never derived from token counts",
	}
}

func quotaWindowOf(w *quota.Window) *QuotaWindow {
	if w == nil {
		return nil
	}
	out := &QuotaWindow{
		Kind:             string(w.Kind),
		Status:           string(w.Status),
		Reason:           string(w.Reason),
		UsedPercent:      w.UsedPercent,
		RemainingPercent: w.RemainingPercent,
		RemainingDerived: w.RemainingDerived,
		WindowMinutes:    w.WindowMinutes,
		ResetsInSeconds:  w.ResetsInSeconds,
	}
	if w.ResetsAt != nil {
		t := w.ResetsAt.UTC()
		out.ResetsAt = &t
	}
	return out
}

func creditsOf(c quota.Credits) (QuotaCredits, bool) {
	if c.HasCredits == nil && c.Unlimited == nil && c.Balance == nil {
		return QuotaCredits{}, false
	}
	return QuotaCredits{HasCredits: c.HasCredits, Unlimited: c.Unlimited, Balance: c.Balance}, true
}

func issuesOf(in []quota.Issue) []QuotaIssue {
	if len(in) == 0 {
		return nil
	}
	out := make([]QuotaIssue, 0, len(in))
	for _, i := range in {
		out = append(out, QuotaIssue{Field: i.Field, Reason: string(i.Reason), Count: i.Count, Line: i.Line})
	}
	return out
}
