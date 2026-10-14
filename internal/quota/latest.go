package quota

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// SettingPrefix is the stable, documented prefix of every setting key this
// package persists. The full key is the prefix plus the instance id:
//
//	quota.codex.<instance>
//
// The value is a bounded Latest JSON object: one selected observation, its
// source, its ingest instant, its status, and the bounded samples a pace
// forecast is derived from (at most two per window). No raw rollout text, no
// credentials, and no per-record history are stored, so a key's size is bounded
// by the Report schema and the retention bound rather than by how long the log
// has been running.
const SettingPrefix = "quota.codex."

// DefaultInstance mirrors store.DefaultInstanceID for callers that only hold a
// possibly-empty instance id. It is duplicated rather than imported because the
// quota package must not depend on any other package in this module.
const DefaultInstance = "default"

// HarnessCodex is the harness label a persisted Codex observation carries.
const HarnessCodex = "codex"

// maxLatestBytes bounds one persisted Latest value. The caps a Report already
// carries keep it far below this; the check only guarantees that a stored value
// can never become an unbounded history.
const maxLatestBytes = 64 << 10

// ErrCorruptStored reports a settings value this package cannot decode. The
// error carries no part of the stored value, so a corrupt row cannot leak log
// text through an error path.
var ErrCorruptStored = errors.New("quota: stored latest observation is corrupt")

// Setting is the minimal read side of the settings table the loader needs.
// *store.Store satisfies it. It keeps this package free of a store dependency,
// so a CLI or GUI can load quota from the index without a scan or a network.
type Setting interface {
	Setting(ctx context.Context, key string) (string, error)
}

// Settings adds the write side used to publish an observation.
type Settings interface {
	Setting
	SetSetting(ctx context.Context, key, value string) error
}

// SettingKey returns the stable key one instance's latest Codex quota
// observation is stored under. An empty instance maps to DefaultInstance, the
// same reserved id the store uses.
func SettingKey(instance string) string {
	return SettingPrefix + normalizeInstance(instance)
}

func normalizeInstance(instance string) string {
	if strings.TrimSpace(instance) == "" {
		return DefaultInstance
	}
	return instance
}

// Latest is the bounded, most recent quota observation persisted for one Codex
// instance. A stored value always carries a Report; LoadLatest rejects a value
// without one rather than hand a consumer a nil report to dereference.
type Latest struct {
	Harness    string    `json:"harness"`
	Instance   string    `json:"instance"`
	Source     string    `json:"source,omitempty"`
	IngestedAt time.Time `json:"ingested_at"`
	Report     *Report   `json:"report,omitempty"`

	// Observations are the retained samples a pace forecast is derived from:
	// at most two per window, each carrying the instant its own source stated.
	// A value stored before retention existed simply has none, so this field is
	// additive and a reader must treat its absence as "no forecast yet".
	Observations []Observation `json:"observations,omitempty"`

	// Forecast is the pace picture derived from Observations at IngestedAt. It
	// is present only once at least one observation has been retained; a
	// consumer can show it directly, and can recompute it at a later instant
	// with Forecasts.
	Forecast *Forecast `json:"forecast,omitempty"`
}

// LoadLatest reads the persisted latest observation for one instance. It
// reports ok=false when nothing has been persisted yet (a fresh index, or an
// instance that never yielded a rate_limits payload) and an error only when the
// store failed or the stored value is unreadable.
func LoadLatest(ctx context.Context, s Setting, instance string) (*Latest, bool, error) {
	if s == nil {
		return nil, false, nil
	}
	raw, err := s.Setting(ctx, SettingKey(instance))
	if err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(raw) == "" {
		return nil, false, nil
	}
	var latest Latest
	if err := json.Unmarshal([]byte(raw), &latest); err != nil {
		return nil, false, ErrCorruptStored
	}
	if latest.Report == nil {
		return nil, false, ErrCorruptStored
	}
	if latest.Harness == "" {
		latest.Harness = HarnessCodex
	}
	if latest.Instance == "" {
		latest.Instance = normalizeInstance(instance)
	}
	return &latest, true, nil
}

// PublishLatest merges rep into the instance's persisted latest observation and
// writes it back with an ingest instant of now. It also folds rep's trustworthy
// samples into the instance's retained observations and recomputes the pace
// forecast, so a stored value carries the forecast its own samples support.
//
// The caller must only call this after the batch that produced rep committed:
// publishing is what makes a quota observation visible, so it must never
// outlive a failed commit.
func PublishLatest(ctx context.Context, s Settings, instance string, rep *Report, now time.Time) error {
	if s == nil || rep == nil {
		return nil
	}
	instance = normalizeInstance(instance)
	if now.IsZero() {
		now = time.Now()
	}
	cur, ok, err := LoadLatest(ctx, s, instance)
	if err != nil {
		return err
	}
	merged := rep
	var retained []Observation
	if ok && cur.Report != nil {
		merged = Merge(cur.Report, rep)
		retained = cur.Observations
	}
	// Retention only ever adds the observations of a report that is itself ok;
	// a merged stale or superseded report contributes nothing, so a historical
	// value can never enter the sample pair as if it were a fresh measurement.
	retained = RetainObservations(retained, reportObservations(merged)...)
	latest := &Latest{
		Harness:      HarnessCodex,
		Instance:     instance,
		Source:       merged.Source,
		IngestedAt:   now.UTC(),
		Report:       merged,
		Observations: retained,
	}
	latest.Forecast = Forecasts(latest, now, ForecastOptions{})
	raw, err := json.Marshal(latest)
	if err != nil {
		return errors.New("quota: encode latest observation")
	}
	if len(raw) > maxLatestBytes {
		return errors.New("quota: latest observation exceeds its bound")
	}
	return s.SetSetting(ctx, SettingKey(instance), string(raw))
}

// Merge returns the instance-wide currency of two observations of the same
// quota. Each argument already carries the currency of its own append-ordered
// source, so this only decides which source's knowledge is newer:
//
//   - the later ObservedAt wins; an observation with no trustworthy instant is
//     the oldest, and a tie keeps the incoming one;
//   - a winner that carries a usage observation keeps its own status verbatim,
//     so an observation already marked stale (ReasonNewerInvalid) can never be
//     upgraded to fresh by a second source;
//   - a winner that tried to state a window and failed does not displace an
//     older usable observation as fresh; the retained observation is returned
//     as stale/newer_invalid instead;
//   - a winner that stated no window at all (plan or credits only) leaves the
//     usable observation in place unchanged, because such a record is not a
//     window-bearing observation.
func Merge(cur, next *Report) *Report {
	switch {
	case cur == nil:
		return next
	case next == nil:
		return cur
	}
	older, newer := cur, next
	if reportNewer(cur, next) {
		older, newer = next, cur
	}
	if usableReport(newer) {
		return newer
	}
	if !usableReport(older) {
		return newer
	}
	if !windowAttemptReport(newer) {
		return older
	}
	retained := *older
	retained.Status, retained.Reason = StatusStale, ReasonNewerInvalid
	retained.SupersededByLine = newer.Line
	return &retained
}

// reportNewer reports whether a is strictly newer than b by observation
// instant. A report with no trustworthy instant is older than any report that
// has one.
func reportNewer(a, b *Report) bool {
	switch {
	case a.ObservedAt == nil:
		return false
	case b.ObservedAt == nil:
		return true
	default:
		return a.ObservedAt.After(*b.ObservedAt)
	}
}

// usableReport reports whether a Report retains a window with an actual usage
// observation.
func usableReport(r *Report) bool {
	return r != nil && (hasUsage(r.Primary) || hasUsage(r.Secondary))
}

// windowAttemptReport reports whether a Report tried to state a window,
// including a window it could not decode.
func windowAttemptReport(r *Report) bool {
	return r != nil && (r.Primary != nil || r.Secondary != nil)
}
