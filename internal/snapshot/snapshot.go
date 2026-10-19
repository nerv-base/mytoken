// Package snapshot builds the versioned read-only snapshot envelope shared by
// the CLI statusline and tray consumers.
//
// The default path is exactly: paths.ResolveDBPath + store.OpenReadOnly. It
// never opens the read-write store, never scans, never touches the network and
// never performs an application write: no directory or database is created, no
// schema is migrated and nothing is repriced.
package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/zzstar101/mytoken/internal/paths"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/quota/plan"
	"github.com/zzstar101/mytoken/internal/scan"
	"github.com/zzstar101/mytoken/internal/store"
)

// Version is the envelope contract version. Consumers switch on it; additive
// fields keep this number, a semantic change bumps it.
const Version = 1

// Reason codes used by the envelope. They are stable strings, not errors.
const (
	ReasonNoLiveSignal     = "no_live_signal"
	ReasonNoPriceCache     = "no_price_cache"
	ReasonUnreadablePrices = "unreadable_price_cache"
	// ReasonIngestRateStale means a live-ingestion signal is stored but its own
	// window has already elapsed, so its rate is expired, not zero.
	ReasonIngestRateStale = "stale_signal"
	// ReasonIngestRateCorrupt means the stored signal is not a valid signal.
	ReasonIngestRateCorrupt       = "corrupt_stored"
	ReasonDiagnosticsNoRecord     = "no_record"
	ReasonDiagnosticsNotCollected = "not_collected"
	ReasonDiagnosticsCorrupt      = "corrupt_record"
	ReasonDiagnosticsUnavailable  = "index_unavailable"
	// ReasonQuotaUnavailable means the plan-quota cache could not be read, so
	// no server-fetched quota is available.
	ReasonQuotaUnavailable = "unavailable"
)

// Status is one availability verdict: a reason is always present when the
// value is unavailable, and never used as a substitute for a real zero.
type Status struct {
	Available bool       `json:"available"`
	Reason    string     `json:"reason,omitempty"`
	Detail    string     `json:"detail,omitempty"`
	FetchedAt *time.Time `json:"fetchedAt,omitempty"`
}

// Schema is the read-only schema verdict, mirroring store.SchemaReport.
type Schema struct {
	State   string            `json:"state"`
	Markers map[string]string `json:"markers,omitempty"`
	Missing []string          `json:"missing,omitempty"`
	Detail  string            `json:"detail,omitempty"`
}

// Sources reports where each part of the envelope came from and whether it was
// readable at all.
type Sources struct {
	Index   Status `json:"index"`
	Pricing Status `json:"pricing"`
	TokRate Status `json:"tokRate"`
	// Quota is the cached plan-quota section: available only when at least one
	// cached snapshot is usable. It never touches the network or a credential.
	Quota Status `json:"quota"`
}

// Freshness is derived from read-only signals. Absent signals stay null.
type Freshness struct {
	EventsThrough    *time.Time `json:"eventsThrough"`
	SessionsThrough  *time.Time `json:"sessionsThrough"`
	SourceModTime    *time.Time `json:"sourceModTime"`
	PricingFetchedAt *time.Time `json:"pricingFetchedAt"`
	RollupZone       string     `json:"rollupZone"`
	LocalZone        string     `json:"localZone"`
	RollupsCurrent   bool       `json:"rollupsCurrent"`
	// StatsTimezone is the persisted stats_timezone setting. It is omitted when
	// the key is absent, and StatsTimezoneRecorded distinguishes "absent" from a
	// recorded empty value.
	StatsTimezone         string `json:"statsTimezone,omitempty"`
	StatsTimezoneRecorded bool   `json:"statsTimezoneRecorded"`
	// EffectiveTimezone is the zone the numbers are reported in, and
	// TimezoneSource says where it came from: "stats_timezone" (persisted),
	// "override" (--timezone) or "system". Absent metadata is never presented as
	// a persisted choice.
	EffectiveTimezone string `json:"effectiveTimezone"`
	TimezoneSource    string `json:"timezoneSource"`
	// NeedsTimezoneSelection reports that no fixed calendar could be resolved.
	NeedsTimezoneSelection bool   `json:"needsTimezoneSelection,omitempty"`
	TimezoneProblem        string `json:"timezoneProblem,omitempty"`
	SystemZonePreview      string `json:"systemZonePreview,omitempty"`
}

// PlanQuota is one cached subscription-plan snapshot.
type PlanQuota struct {
	Provider  string       `json:"provider"`
	Plan      string       `json:"plan,omitempty"`
	Source    string       `json:"source,omitempty"`
	Windows   []PlanWindow `json:"windows"`
	FetchedAt time.Time    `json:"fetchedAt"`
}

// PlanWindow is one usage window of a plan. Used, Limit, Unit, Balance,
// Remaining and ExpiresAt describe a credit balance or a capped allowance; they
// are zero for a percentage-only window.
type PlanWindow struct {
	Name        string    `json:"name"`
	UsedPercent float64   `json:"usedPercent"`
	Used        float64   `json:"used"`
	Limit       float64   `json:"limit"`
	Remaining   float64   `json:"remaining"`
	Unit        string    `json:"unit"`
	Balance     bool      `json:"balance"`
	ResetsAt    time.Time `json:"resetsAt,omitzero"`
	ExpiresAt   time.Time `json:"expiresAt,omitzero"`
}

// TokRate is the live-ingestion signal a scanner persisted.
type TokRate struct {
	Tokens        int64     `json:"tokens"`
	Rate          float64   `json:"ratePerSecond"`
	WindowSeconds float64   `json:"windowSeconds"`
	At            time.Time `json:"at"`
	AgeSeconds    float64   `json:"ageSeconds"`
}

// Data carries the measured values. Totals is null (not zero) when the index
// could not be read.
type Data struct {
	Totals  *query.Totals `json:"totals"`
	TokRate *TokRate      `json:"tokRate"`
	// Plans is one entry per cached plan quota. It is empty, never null.
	Plans []PlanQuota `json:"plans"`
}

// Writes documents what the snapshot did and what an explicit refresh would do.
type Writes struct {
	Performed []string `json:"performed"`
	Potential []string `json:"potential"`
}

// Snapshot is the versioned envelope.
type Snapshot struct {
	SnapshotVersion int       `json:"snapshotVersion"`
	GeneratedAt     time.Time `json:"generatedAt"`
	Schema          Schema    `json:"schema"`
	Sources         Sources   `json:"sources"`
	Freshness       Freshness `json:"freshness"`
	Data            Data      `json:"data"`
	Writes          Writes    `json:"writes"`
}

// Options configure one snapshot read.
type Options struct {
	DBPath             string
	DataDir            string
	Filter             query.Filter
	Now                time.Time
	Performed          []string
	Timezone           string
	Zone               string
	ZoneSource         string
	ZoneNeedsSelection bool
	ZoneProblem        string
	ZoneSystemPreview  string
}

func potentialWrites() []string {
	return []string{"scan", "database_bootstrap", "schema_migration", "rollup_rebuild", "price_reprice"}
}

// Build renders one envelope. It returns an envelope even when the index is
// unavailable.
func Build(ctx context.Context, opts Options) *Snapshot {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	s := &Snapshot{
		SnapshotVersion: Version,
		GeneratedAt:     now.UTC(),
		Data:            Data{Plans: []PlanQuota{}},
		Writes:          Writes{Performed: []string{}, Potential: potentialWrites()},
	}
	if opts.Performed != nil {
		s.Writes.Performed = opts.Performed
	}
	s.Freshness = Freshness{LocalZone: store.LocalDayZone(), RollupsCurrent: true}
	s.Freshness.EffectiveTimezone, s.Freshness.TimezoneSource = reportingZone(opts.Zone, opts.ZoneSource, opts.Timezone)
	if opts.ZoneNeedsSelection && opts.Timezone == "" {
		s.Freshness.NeedsTimezoneSelection = true
		s.Freshness.TimezoneProblem = opts.ZoneProblem
		s.Freshness.SystemZonePreview = opts.ZoneSystemPreview
		s.Freshness.EffectiveTimezone = systemZoneName
		s.Freshness.TimezoneSource = "needs_selection"
	}

	path := opts.DBPath
	if path == "" {
		p, err := paths.ResolveDBPath()
		if err != nil {
			s.Sources.Index = Status{Reason: string(store.ReasonUnavailable), Detail: err.Error()}
			s.Schema = Schema{State: string(store.SchemaUnreadable), Detail: err.Error()}
			s.Sources.Quota = Status{Reason: ReasonQuotaUnavailable, Detail: "the index path could not be resolved"}
			return s
		}
		path = p
	}
	dir := opts.DataDir
	if dir == "" {
		dir, _ = paths.ResolveDataDir()
	}
	if info, err := os.Stat(path); err == nil {
		t := info.ModTime().UTC()
		s.Freshness.SourceModTime = &t
	}
	if dir != "" {
		s.Sources.Pricing, s.Freshness.PricingFetchedAt = readPriceCache(filepath.Join(dir, "prices.json"))
	} else {
		s.Sources.Pricing = Status{Reason: ReasonNoPriceCache, Detail: "no data directory could be resolved"}
	}

	ro, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		reason, _ := store.ReadOnlyReasonOf(err)
		if reason == "" {
			reason = store.ReasonUnavailable
		}
		s.Sources.Index = Status{Reason: string(reason), Detail: err.Error()}
		s.Schema = schemaFromError(err)
		s.Sources.TokRate = Status{Reason: ReasonNoLiveSignal}
		s.Sources.Quota = Status{Reason: string(reason), Detail: err.Error()}
		return s
	}
	defer ro.Close()

	s.Schema = schemaFromReport(ro.ReadOnlySchema())
	env, err := query.ReadEnvelope(ctx, ro, opts.Filter)
	if err != nil {
		s.Sources.Index = Status{Reason: string(store.ReasonUnavailable), Detail: err.Error()}
		s.Sources.TokRate = Status{Reason: ReasonNoLiveSignal}
		s.Sources.Quota = Status{Reason: ReasonQuotaUnavailable, Detail: "the index could not be read"}
		return s
	}
	s.Sources.Index = Status{Available: true}
	totals := env.Totals
	s.Data.Totals = &totals
	s.Freshness.EventsThrough = env.Freshness.EventsThrough
	s.Freshness.SessionsThrough = env.Freshness.SessionsThrough
	s.Freshness.RollupZone = env.Freshness.RollupZone
	s.Freshness.LocalZone = env.Freshness.LocalZone
	s.Freshness.RollupsCurrent = env.Freshness.RollupsCurrent
	s.Freshness.StatsTimezone = env.Freshness.StatsTimezone
	s.Freshness.StatsTimezoneRecorded = env.Freshness.StatsTimezoneRecorded
	s.Sources.TokRate, s.Data.TokRate = ingestRateOf(ctx, ro, now)
	s.Data.Plans, s.Sources.Quota = readPlans(ctx, ro)
	return s
}

// readPlans decodes the plan service's cached snapshots. It uses the service's
// own decoder, so a stored value passes exactly the validation the service
// applies: a snapshot whose source is not "api" (for example a local usage log
// an older build cached) is skipped, and the fetch time is read from the same
// fetched_at field the service wrote. Unreadable rows and the separate
// balance-history rows are skipped too.
func readPlans(ctx context.Context, ro *store.Store) ([]PlanQuota, Status) {
	rows, err := ro.DB().QueryContext(ctx, `SELECT value FROM settings WHERE key LIKE 'quota.plan.%'`)
	if err != nil {
		return []PlanQuota{}, Status{Reason: ReasonQuotaUnavailable, Detail: err.Error()}
	}
	defer rows.Close()
	out := []PlanQuota{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return out, Status{Reason: ReasonQuotaUnavailable, Detail: err.Error()}
		}
		snap, err := plan.DecodeSnapshot([]byte(raw))
		if err != nil {
			continue
		}
		p := PlanQuota{
			Provider:  snap.Provider,
			Plan:      snap.Plan,
			Source:    snap.Source,
			FetchedAt: snap.FetchedAt,
		}
		for _, w := range snap.Windows {
			p.Windows = append(p.Windows, PlanWindow{
				Name:        w.Name,
				UsedPercent: w.UsedPercent,
				Used:        w.Used,
				Limit:       w.Limit,
				Remaining:   w.Remaining,
				Unit:        w.Unit,
				Balance:     w.Balance,
				ResetsAt:    w.ResetsAt,
				ExpiresAt:   w.ExpiresAt,
			})
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return out, Status{Reason: ReasonQuotaUnavailable, Detail: err.Error()}
	}
	if len(out) == 0 {
		return out, Status{Reason: "no_observation"}
	}
	return out, Status{Available: true}
}

func schemaFromReport(r store.SchemaReport) Schema {
	return Schema{State: string(r.State), Markers: r.Markers, Missing: r.Missing, Detail: r.Detail}
}

func schemaFromError(err error) Schema {
	var roErr *store.ReadOnlyError
	if errors.As(err, &roErr) && roErr != nil && roErr.Schema.State != "" {
		return schemaFromReport(roErr.Schema)
	}
	reason, _ := store.ReadOnlyReasonOf(err)
	if reason == "" {
		reason = store.ReasonUnavailable
	}
	return Schema{State: string(store.SchemaUnreadable), Detail: string(reason)}
}

func readPriceCache(path string) (Status, *time.Time) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Status{Reason: ReasonNoPriceCache, Detail: "no price cache at " + path}, nil
	}
	var cache struct {
		Fetched time.Time `json:"fetched"`
	}
	if err := json.Unmarshal(raw, &cache); err != nil || cache.Fetched.IsZero() {
		return Status{Reason: ReasonUnreadablePrices, Detail: "the price cache has no usable fetched timestamp"}, nil
	}
	t := cache.Fetched.UTC()
	return Status{Available: true, FetchedAt: &t}, &t
}

// reportingZone names the zone the caller computed in.
func reportingZone(zone, source, override string) (string, string) {
	switch {
	case zone != "":
		if source == "" {
			source = "caller"
		}
		return zone, source
	case override != "":
		return override, "override"
	}
	return time.Local.String(), "system"
}

// systemZoneName labels an unresolved calendar in read-only metadata.
const systemZoneName = "unconfigured"

// PersistedStatsTimezone reads settings.stats_timezone through the read-only
// path. recorded is false when the key is absent, empty, or the index cannot be
// read at all.
func PersistedStatsTimezone(ctx context.Context, dbPath string) (tz string, recorded bool) {
	path := dbPath
	if path == "" {
		p, err := paths.ResolveDBPath()
		if err != nil {
			return "", false
		}
		path = p
	}
	ro, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		return "", false
	}
	defer ro.Close()
	value, err := ro.Setting(ctx, "stats_timezone")
	if err != nil || value == "" {
		return "", false
	}
	return value, true
}

// ingestRateOf renders the persisted live-ingestion signal.
func ingestRateOf(ctx context.Context, ro *store.Store, now time.Time) (Status, *TokRate) {
	sig, ok, err := scan.LoadIngestRateSignal(ctx, ro)
	if err != nil {
		reason := ReasonNoLiveSignal
		if errors.Is(err, scan.ErrCorruptIngestRateSignal) {
			reason = ReasonIngestRateCorrupt
		}
		return Status{Reason: reason, Detail: err.Error()}, nil
	}
	if !ok {
		return Status{Reason: ReasonNoLiveSignal, Detail: "no live token-rate signal has been published; it comes from a running scan"}, nil
	}
	at := sig.Timestamp
	if !sig.Available {
		reason := sig.Reason
		if reason == "" {
			reason = ReasonNoLiveSignal
		}
		return Status{Reason: reason, Detail: "the scanner's last published signal reported no usable live measurement", FetchedAt: &at}, nil
	}
	window := time.Duration(sig.Window * float64(time.Second))
	if window > 0 && now.Sub(sig.Timestamp) > window {
		return Status{
			Reason:    ReasonIngestRateStale,
			Detail:    "the last published signal is " + now.Sub(sig.Timestamp).Round(time.Second).String() + " old, older than its " + window.String() + " window",
			FetchedAt: &at,
		}, nil
	}
	return Status{Available: true, FetchedAt: &at}, &TokRate{
		Tokens:        sig.Tokens,
		Rate:          sig.Rate,
		WindowSeconds: sig.Window,
		At:            at,
		AgeSeconds:    now.Sub(at).Seconds(),
	}
}
