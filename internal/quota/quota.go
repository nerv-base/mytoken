package quota

import "time"

// Bounds applied by the offline Codex reader.
const (
	// DefaultMaxLineBytes caps a single JSONL record. Codex rollouts can carry
	// multi-megabyte lines (for example a compacted history payload); records
	// past this size are skipped rather than buffered.
	DefaultMaxLineBytes = 1 << 20

	// DefaultMaxBytes caps the total input consumed from one reader.
	DefaultMaxBytes = 1 << 30
)

// maxLabelLen caps any free-form label copied out of a provider log, so a
// hostile or corrupt record cannot inflate a Report.
const maxLabelLen = 64

// maxSourceLen caps the caller-supplied provenance label. It is longer than a
// label because it is typically a filesystem path that must stay identifiable.
const maxSourceLen = 512

// maxOtherLimitIDs bounds how many other limit identities a Report lists, so a
// stream that names thousands of limits still yields a small Report.
const maxOtherLimitIDs = 8

// trustedEpochMin and trustedEpochMax bound the instants this package is
// willing to treat as a timestamp. Values outside the range are reported as
// out_of_range instead of being reinterpreted (for example as milliseconds),
// because guessing a unit would invent an observation time.
var (
	trustedEpochMin = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	trustedEpochMax = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)

// WindowKind identifies which provider quota window an observation describes.
type WindowKind string

// Known quota window kinds.
const (
	WindowPrimary   WindowKind = "primary"
	WindowSecondary WindowKind = "secondary"
)

// Status describes how much of an observation this package is willing to
// assert.
type Status string

// Observation statuses.
const (
	// StatusOK means every field the caller asked about was present and
	// trustworthy.
	StatusOK Status = "ok"

	// StatusStale means the observation is well formed but is not the current
	// value: it is older than the caller's StaleAfter budget, or a newer
	// window-bearing record was unusable (ReasonNewerInvalid).
	StatusStale Status = "stale"

	// StatusUnknown means the observation is well formed but lacks a
	// trustworthy timestamp, so its age cannot be established.
	StatusUnknown Status = "unknown"

	// StatusMalformed means the log carried a rate_limits payload that could
	// not be understood.
	StatusMalformed Status = "malformed"

	// StatusUnavailable means no usable quota observation was found at all.
	StatusUnavailable Status = "unavailable"
)

// Reason explains a Status, and on a Window explains why a particular field is
// absent. Reason values are stable identifiers suitable for logs and UI.
type Reason string

// Reasons. ReasonNone accompanies StatusOK.
const (
	ReasonNone Reason = ""

	// ReasonMissing: the log did not carry the field. Absent is not zero.
	ReasonMissing Reason = "missing"

	// ReasonMalformed: the field was present but did not have a usable shape.
	ReasonMalformed Reason = "malformed"

	// ReasonStale: the observation is older than the caller's staleness budget.
	ReasonStale Reason = "stale"

	// ReasonNoData: the input contained no rate_limits payload.
	ReasonNoData Reason = "no_data"

	// ReasonNoTimestamp: a trustworthy observation instant is required and
	// absent.
	ReasonNoTimestamp Reason = "no_trustworthy_timestamp"

	// ReasonLineTooLong: a record exceeded MaxLineBytes and was skipped.
	ReasonLineTooLong Reason = "line_too_long"

	// ReasonByteLimit: the input exceeded MaxBytes and was truncated.
	ReasonByteLimit Reason = "byte_limit"

	// ReasonOutOfRange: a numeric value was outside the range this package is
	// willing to accept without guessing a unit.
	ReasonOutOfRange Reason = "out_of_range"

	// ReasonUnsupportedType: the field carried a JSON type the schema does not
	// define for it.
	ReasonUnsupportedType Reason = "unsupported_type"

	// ReasonWindowMismatch: two observations describe different window kinds.
	ReasonWindowMismatch Reason = "window_mismatch"

	// ReasonResetUnknown: two observations cannot be compared because a reset
	// instant is missing.
	ReasonResetUnknown Reason = "reset_unknown"

	// ReasonResetRollover: the earlier reset already elapsed before the later
	// observation, so the two samples belong to different quota periods.
	ReasonResetRollover Reason = "reset_rollover"

	// ReasonResetMismatch: two observations disagree about the reset instant of
	// a window that has not rolled over.
	ReasonResetMismatch Reason = "reset_mismatch"

	// ReasonZeroElapsed: two observations share an instant.
	ReasonZeroElapsed Reason = "zero_elapsed"

	// ReasonDecreasingUsage: the later observation reports less usage than the
	// earlier one without a reset in between.
	ReasonDecreasingUsage Reason = "decreasing_usage"

	// ReasonOutlier: an interval or rate fell outside the plausibility bounds.
	ReasonOutlier Reason = "outlier"

	// ReasonNewerInvalid: a window-bearing record newer, in append order, than
	// the selected observation was unusable. The selected value is retained
	// only as history, never as the current value.
	ReasonNewerInvalid Reason = "newer_invalid"

	// ReasonLimitMismatch: two observations do not name the same rate-limit
	// identity, so they do not describe the same quota bucket.
	ReasonLimitMismatch Reason = "limit_mismatch"

	// ReasonNoObservation: no retained observation exists at all, so there is
	// nothing to forecast from. It is the reason an older stored payload, which
	// predates retention, reports.
	ReasonNoObservation Reason = "no_observation"

	// ReasonInsufficientSamples: fewer than two comparable samples are retained
	// for a window. One observation states a level, never a rate, and a rate is
	// never inferred from anything else.
	ReasonInsufficientSamples Reason = "insufficient_samples"

	// ReasonFutureObservation: the newest retained observation is dated after
	// the instant the question was asked, beyond the tolerated clock skew, so
	// its age is not trustworthy.
	ReasonFutureObservation Reason = "future_observation"

	// ReasonSourceMismatch: the two retained samples for a window did not come
	// from the same source, so they are not known to describe the same account.
	ReasonSourceMismatch Reason = "source_mismatch"
)

// Window is one quota window (primary or secondary) as observed in a log.
//
// Pointer fields distinguish "the log said zero" from "the log did not say".
// A nil UsedPercent with ReasonMissing is a different fact from a
// *UsedPercent of 0.
type Window struct {
	Kind   WindowKind `json:"kind"`
	Status Status     `json:"status"`
	Reason Reason     `json:"reason,omitempty"`

	// UsedPercent is the fraction of the window already consumed, 0..100.
	UsedPercent *float64 `json:"used_percent,omitempty"`

	// RemainingPercent is the fraction still available, 0..100. When the log
	// did not carry it, it is derived from UsedPercent and RemainingDerived is
	// set.
	RemainingPercent *float64 `json:"remaining_percent,omitempty"`
	RemainingDerived bool     `json:"remaining_derived,omitempty"`

	// WindowMinutes is the window length in minutes. The unit comes from the
	// field name; a value in a differently named field is not accepted here.
	WindowMinutes *int64 `json:"window_minutes,omitempty"`

	// ResetsInSeconds is the log's own countdown, in seconds, kept verbatim.
	ResetsInSeconds *int64 `json:"resets_in_seconds,omitempty"`

	// ResetsAt is the reset instant. It is taken from an explicit reset
	// timestamp when the log carries one, otherwise computed from the
	// observation instant plus ResetsInSeconds.
	ResetsAt *time.Time `json:"resets_at,omitempty"`
}

// Credits is the credits block of a Codex rate_limits payload. Every field is
// optional: this package reports credits only when the log states them
// explicitly.
type Credits struct {
	HasCredits *bool `json:"has_credits,omitempty"`
	Unlimited  *bool `json:"unlimited,omitempty"`

	// Balance is the provider's own balance string. Its unit is not stated by
	// the log, so it is preserved verbatim rather than converted.
	Balance *string `json:"balance,omitempty"`
}

// Issue records one class of unsupported or unusable input. Issues are
// aggregated by field and reason and sorted, so a corrupt log produces a short
// stable list rather than one entry per line.
type Issue struct {
	// Field is a static schema path this package generated, such as
	// "rate_limits.primary.used_percent". It never contains log text.
	Field string `json:"field"`

	Reason Reason `json:"reason"`

	// Count is how many times this field/reason pair was seen.
	Count int `json:"count"`

	// Line is the 1-based line of the first occurrence.
	Line int `json:"line"`
}

// Report is the result of reading one quota source.
type Report struct {
	// Source is the caller-supplied provenance label for the reader.
	Source string `json:"source,omitempty"`

	// ObservedAt is the instant of the chosen observation, when the log carried
	// a trustworthy timestamp.
	ObservedAt *time.Time `json:"observed_at,omitempty"`

	// Line is the 1-based line of the chosen observation.
	Line int `json:"line,omitempty"`

	// RecordType is the static record path of the chosen observation, such as
	// "event_msg.token_count".
	RecordType string `json:"record_type,omitempty"`

	// LimitID and LimitName are the labels the selected observation's own
	// record stated for the rate limit. They are never borrowed from another
	// record: LimitID is empty when the selected record did not carry one.
	LimitID   string `json:"limit_id,omitempty"`
	LimitName string `json:"limit_name,omitempty"`

	// OtherLimitIDs lists, sorted and deduplicated, limit IDs that appeared on
	// quota-bearing records other than the selected observation. A single
	// Report describes one selected record, so another record's identity is
	// reported here separately rather than merged into LimitID. At most
	// maxOtherLimitIDs entries are kept.
	OtherLimitIDs []string `json:"other_limit_ids,omitempty"`

	// PlanType is the plan label of the selected observation, present only when
	// that record stated one.
	PlanType *string `json:"plan_type,omitempty"`

	// Credits is present only when the selected observation carried a credits
	// block.
	Credits *Credits `json:"credits,omitempty"`

	// Primary and Secondary are present only when the log carried that window.
	Primary   *Window `json:"primary,omitempty"`
	Secondary *Window `json:"secondary,omitempty"`

	// Status and Reason summarize the Report. A Report can be usable
	// (Available) while still being stale.
	Status Status `json:"status"`
	Reason Reason `json:"reason,omitempty"`

	// SupersededByLine is the 1-based line of the newer window-bearing record
	// that made the selected observation historical (Status stale with
	// ReasonNewerInvalid). Zero when the selected observation is itself the
	// newest window-bearing record.
	SupersededByLine int `json:"superseded_by_line,omitempty"`

	// Issues lists every unsupported or unusable shape encountered.
	Issues []Issue `json:"issues,omitempty"`

	// UnknownFields counts JSON fields this schema does not define. Future
	// fields are tolerated and counted, never interpreted.
	UnknownFields int `json:"unknown_fields,omitempty"`

	// LinesRead and BytesRead describe how much input was consumed.
	LinesRead int   `json:"lines_read"`
	BytesRead int64 `json:"bytes_read"`

	// Truncated reports that the byte limit stopped the read early.
	Truncated bool `json:"truncated,omitempty"`
}

// Window returns the requested window, or nil when the log did not carry it.
func (r *Report) Window(kind WindowKind) *Window {
	if r == nil {
		return nil
	}
	switch kind {
	case WindowPrimary:
		return r.Primary
	case WindowSecondary:
		return r.Secondary
	default:
		return nil
	}
}

// Available reports whether the Report carries at least one usable quota
// observation. A stale observation is still usable data; it is merely old.
func (r *Report) Available() bool {
	if r == nil {
		return false
	}
	return r.Status == StatusOK || r.Status == StatusStale
}

// Sample converts a window observation into the value the pace helper
// consumes. It reports false when the window has no usable used_percent.
//
// The sample carries the limit identity of the Report it came from, so
// EstimatePace can refuse to compare observations of different quota buckets.
func (r *Report) Sample(kind WindowKind) (Sample, bool) {
	w := r.Window(kind)
	if w == nil || w.Status != StatusOK || w.UsedPercent == nil {
		return Sample{}, false
	}
	s := Sample{Kind: kind, UsedPercent: *w.UsedPercent, LimitID: r.LimitID}
	if r.ObservedAt != nil {
		s.ObservedAt = *r.ObservedAt
		s.HasObservedAt = true
	}
	if w.RemainingPercent != nil {
		s.RemainingPercent = *w.RemainingPercent
		s.HasRemainingPercent = true
	}
	if w.ResetsAt != nil {
		s.ResetsAt = *w.ResetsAt
		s.HasResetsAt = true
	}
	return s, true
}
