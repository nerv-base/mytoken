// Package snapshot builds the versioned read-only snapshot envelope shared by
// the CLI statusline, the tray and future MCP consumers.
//
// The default path is exactly: paths.ResolveDBPath + store.OpenReadOnly. It
// never opens the read-write store, never scans, never touches the network and
// never performs an application write: no directory or database is created, no
// schema is migrated and nothing is repriced. SQLite's own read coordination may
// create the -wal/-shm sidecars of a WAL database (or record a read mark in an
// existing -shm); that is transient bookkeeping, not index data, and it never
// modifies the database file or committed WAL frames. When the index cannot be
// read, the envelope still renders and the reason travels in
// sources.index.reason; an unavailable value is never reported as zero.
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
	ReasonNoRelaySite      = "no_relay_site"
	ReasonBalanceNotReport = "balance_not_reported"
	// ReasonIngestRateStale means a live-ingestion signal is stored but its own
	// window has already elapsed, so its rate is expired, not zero.
	ReasonIngestRateStale = "stale_signal"
	// ReasonIngestRateCorrupt means the stored signal is not a valid signal.
	ReasonIngestRateCorrupt = "corrupt_stored"
	// ReasonMultipleBalances means more than one relay key recorded a balance.
	// Keys may belong to the same upstream account or to different ones, so the
	// amounts are reported per key and no scalar total is offered.
	ReasonMultipleBalances = "multiple_balances"
	// ReasonMixedCurrency means the recorded balances do not share one currency,
	// so they could not be added even for a single account.
	ReasonMixedCurrency = "mixed_currency"
	// ReasonDiagnosticsNoRecord means no collection diagnostics were ever
	// stored for the instance: it is unavailable, never a zero count.
	ReasonDiagnosticsNoRecord = "no_record"
	// ReasonDiagnosticsNotCollected means the source is configured but not
	// collected (disabled or a backup-copy), so no record is expected.
	ReasonDiagnosticsNotCollected = "not_collected"
	// ReasonDiagnosticsCorrupt means a stored record exists but is not a valid
	// record for its key.
	ReasonDiagnosticsCorrupt = "corrupt_record"
	// ReasonDiagnosticsUnavailable means the diagnostics could not be read from
	// the index at all (the settings table or the index was unavailable).
	ReasonDiagnosticsUnavailable = "index_unavailable"
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
	Balance Status `json:"balance"`
	TokRate Status `json:"tokRate"`
	// Quota is the persisted Codex quota section: available only when at least
	// one stored observation is usable, otherwise a reason such as
	// no_observation. It never touches the network or a credential.
	Quota Status `json:"quota"`
	// ClaudeQuota is the cached Claude Code consent and reading section. It is
	// read from the settings table only: no credential is read and no request
	// is made, which is why an ordinary snapshot never needs the network.
	ClaudeQuota Status `json:"claudeQuota"`
	// Diagnostics is the collection-evidence section the scanner persisted per
	// harness instance: when it last attempted and last succeeded, what it
	// found and why. It is available only when at least one stored record is
	// usable, and an absent record is a reason, never a zero count.
	Diagnostics Status `json:"diagnostics"`
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
	// NeedsTimezoneSelection reports that no fixed calendar could be resolved,
	// so nothing here may be presented as a persisted choice. The numbers are
	// then a system-zone preview only, named by SystemZonePreview.
	NeedsTimezoneSelection bool   `json:"needsTimezoneSelection,omitempty"`
	TimezoneProblem        string `json:"timezoneProblem,omitempty"`
	// SystemZonePreview names the process zone as a preview. It is set only
	// when NeedsTimezoneSelection is true and never presented as the fixed
	// calendar.
	SystemZonePreview string `json:"systemZonePreview,omitempty"`
}

// Balance is one relay key's quota as the index last recorded it.
type Balance struct {
	Origin       string    `json:"origin"`
	KeyID        string    `json:"keyId"`
	At           time.Time `json:"at"`
	RemainingUSD *float64  `json:"remainingUsd"`
	UsedUSD      *float64  `json:"usedUsd"`
	Unlimited    bool      `json:"unlimited"`
	Currency     string    `json:"currency,omitempty"`
}

// Data carries the measured values. Totals and Cache are null (not zero) when
// the index could not be read.
type Data struct {
	Totals   *query.Totals      `json:"totals"`
	Cache    *query.CacheTotals `json:"cache"`
	Balances []Balance          `json:"balances"`
	// CodexQuota is one entry per stored (or explicitly filtered) Codex quota
	// instance. It is empty, never null, when nothing is stored; instances are
	// never combined.
	CodexQuota []CodexQuota `json:"codexQuota"`
	// ClaudeQuota is one entry per stored (or explicitly filtered) Claude Code
	// instance: its consent state and its cached reading. It is empty, never
	// null, when nothing is stored; instances are never combined, and the
	// credential source path of a consent is never included.
	ClaudeQuota []ClaudeQuota `json:"claudeQuota"`
	// TokRate is the live-ingestion rate the running scanner published under
	// ingest_rate.latest.v1, or null with Sources.TokRate explaining why. A
	// one-shot process cannot measure this itself, so it is never derived from
	// stored token counts.
	TokRate *TokRate `json:"tokRate"`
	// Diagnostics is one entry per stored (or explicitly filtered) harness
	// instance, carrying that instance's own attempt and success times, counts
	// and reasons. It is empty, never null, when nothing is stored; instances
	// are never combined, and a missing record is unavailable rather than zero.
	Diagnostics []InstanceDiagnostics `json:"diagnostics"`
}

// TokRate is the live-ingestion signal a scanner persisted: output tokens that
// entered the index inside its trailing window, and the rate over that window.
//
// The rate is an as-of measurement, not a value at the query instant: a
// one-shot reader reports the instant the signal was published (At) and its age
// so a consumer can see the number is not live.
type TokRate struct {
	Tokens        int64     `json:"tokens"`
	Rate          float64   `json:"ratePerSecond"`
	WindowSeconds float64   `json:"windowSeconds"`
	At            time.Time `json:"at"`
	// AgeSeconds is how old At was when this envelope was built.
	AgeSeconds float64 `json:"ageSeconds"`
}

// Writes documents what the snapshot did and what an explicit refresh would do.
// A default snapshot performs nothing.
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
	// DBPath overrides the resolved index path. Empty means paths.ResolveDBPath.
	DBPath string
	// DataDir overrides the resolved data directory (prices.json lookup).
	DataDir string
	Filter  query.Filter
	Now     time.Time
	// Performed lists the write operations this run already did (only an
	// explicit --refresh ever fills it).
	Performed []string
	// Timezone is an explicit --timezone override. It is reported as the
	// effective zone and never written back as persisted metadata.
	Timezone string
	// Zone and ZoneSource are the reporting zone the caller already resolved
	// and computed in (see the CLI's reportingZone). When Zone is set it is
	// reported verbatim, so the envelope never names a zone the queries did not
	// run in.
	Zone       string
	ZoneSource string
	// ZoneNeedsSelection reports that no fixed calendar zone exists (the key is
	// absent, or holds a value the caller could not use). The envelope then
	// says so instead of presenting the system zone as the calendar.
	ZoneNeedsSelection bool
	// ZoneProblem is the human-readable reason, surfaced in read-only metadata.
	ZoneProblem string
	// ZoneSystemPreview is the system zone the caller fell back to for display.
	ZoneSystemPreview string
}

// potentialWrites is the honest list of writes an explicit refresh may cause,
// so a consumer never has to guess why the read-only path is preferred.
func potentialWrites() []string {
	return []string{"scan", "database_bootstrap", "schema_migration", "rollup_rebuild", "price_reprice"}
}

// Build renders one envelope. It returns an envelope even when the index is
// unavailable; only a broken data-dir resolution can make it fail, and that is
// still reported inside the envelope.
func Build(ctx context.Context, opts Options) *Snapshot {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	s := &Snapshot{
		SnapshotVersion: Version,
		GeneratedAt:     now.UTC(),
		Data:            Data{Balances: []Balance{}, CodexQuota: []CodexQuota{}, ClaudeQuota: []ClaudeQuota{}, Diagnostics: []InstanceDiagnostics{}},
		Writes:          Writes{Performed: []string{}, Potential: potentialWrites()},
	}
	if opts.Performed != nil {
		s.Writes.Performed = opts.Performed
	}
	s.Freshness = Freshness{LocalZone: store.LocalDayZone(), RollupsCurrent: true}
	s.Freshness.EffectiveTimezone, s.Freshness.TimezoneSource = reportingZone(opts.Zone, opts.ZoneSource, opts.Timezone)
	// An explicit override is a deliberate one-run choice, so it stands even
	// when no fixed calendar is persisted. Without one, an unresolved calendar
	// is reported as unresolved: the process zone is a preview, never the
	// persisted choice.
	if opts.ZoneNeedsSelection && opts.Timezone == "" {
		s.Freshness.NeedsTimezoneSelection = true
		s.Freshness.TimezoneProblem = opts.ZoneProblem
		s.Freshness.SystemZonePreview = opts.ZoneSystemPreview
		s.Freshness.EffectiveTimezone = systemZoneName
		s.Freshness.TimezoneSource = "needs_selection"
	}

	path := opts.DBPath
	if path == "" {
		if p, err := paths.ResolveDBPath(); err == nil {
			path = p
		} else {
			s.Sources.Index = Status{Reason: string(store.ReasonUnavailable), Detail: err.Error()}
			s.Schema = Schema{State: string(store.SchemaUnreadable), Detail: err.Error()}
			s.Sources.Quota = Status{Reason: ReasonQuotaUnavailable, Detail: "the index path could not be resolved"}
			s.Sources.ClaudeQuota = Status{Reason: ReasonClaudeQuotaNoConsent, Detail: "the index path could not be resolved"}
			s.Data.Diagnostics, s.Sources.Diagnostics = readDiagnosticsSection(ctx, nil, opts.Filter.InstanceIDs, now)
			return s
		}
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
		s.Sources.Balance = Status{Reason: ReasonNoRelaySite, Detail: "the index is not readable"}
		s.Sources.TokRate = Status{Reason: ReasonNoLiveSignal}
		s.Sources.Quota = Status{Reason: string(reason), Detail: err.Error()}
		s.Sources.ClaudeQuota = Status{Reason: string(reason), Detail: err.Error()}
		s.Data.ClaudeQuota = BuildClaudeQuota(query.MissingClaudeQuotaObservations(opts.Filter.InstanceIDs,
			"the index could not be read; nothing was created or opened writable"), now)
		s.Data.Diagnostics, s.Sources.Diagnostics = readDiagnosticsSection(ctx, nil, opts.Filter.InstanceIDs, now)
		return s
	}
	defer ro.Close()

	s.Schema = schemaFromReport(ro.ReadOnlySchema())
	env, err := query.ReadEnvelope(ctx, ro, opts.Filter, query.EnvelopeOptions{Quota: true})
	if err != nil {
		s.Sources.Index = Status{Reason: string(store.ReasonUnavailable), Detail: err.Error()}
		s.Sources.Balance = Status{Reason: ReasonNoRelaySite, Detail: "the index is not readable"}
		s.Sources.TokRate = Status{Reason: ReasonNoLiveSignal}
		s.Sources.Quota = Status{Reason: ReasonQuotaUnavailable, Detail: "the index could not be read"}
		s.Sources.ClaudeQuota = Status{Reason: ReasonClaudeQuotaNoConsent, Detail: "the index could not be read"}
		s.Data.ClaudeQuota = BuildClaudeQuota(query.MissingClaudeQuotaObservations(opts.Filter.InstanceIDs,
			"the index could not be read"), now)
		s.Data.Diagnostics, s.Sources.Diagnostics = readDiagnosticsSection(ctx, nil, opts.Filter.InstanceIDs, now)
		return s
	}
	s.Sources.Index = Status{Available: true}
	totals, cache := env.Totals, env.Cache
	s.Data.Totals = &totals
	s.Data.Cache = &cache
	s.Freshness.EventsThrough = env.Freshness.EventsThrough
	s.Freshness.SessionsThrough = env.Freshness.SessionsThrough
	s.Freshness.RollupZone = env.Freshness.RollupZone
	s.Freshness.LocalZone = env.Freshness.LocalZone
	s.Freshness.RollupsCurrent = env.Freshness.RollupsCurrent
	s.Freshness.StatsTimezone = env.Freshness.StatsTimezone
	s.Freshness.StatsTimezoneRecorded = env.Freshness.StatsTimezoneRecorded
	s.Data.Balances, s.Sources.Balance = readBalances(ctx, ro)
	s.Data.CodexQuota = BuildCodexQuota(env.Quota)
	s.Sources.Quota = QuotaSectionStatus(s.Data.CodexQuota)
	s.Sources.TokRate, s.Data.TokRate = ingestRateOf(ctx, ro, now)
	s.Data.Diagnostics, s.Sources.Diagnostics = readDiagnosticsSection(ctx, ro, opts.Filter.InstanceIDs, now)
	claudeObs, err := query.ReadClaudeQuota(ctx, ro, opts.Filter.InstanceIDs)
	if err != nil {
		s.Sources.ClaudeQuota = Status{Reason: ReasonClaudeQuotaNoConsent, Detail: "the Claude quota settings could not be read: " + err.Error()}
		return s
	}
	s.Data.ClaudeQuota = BuildClaudeQuota(claudeObs, now)
	s.Sources.ClaudeQuota = ClaudeQuotaSectionStatus(s.Data.ClaudeQuota)
	return s
}

// ingestRateOf renders the persisted live-ingestion signal through the
// producer's own loader, so no field is guessed. A one-shot process reports what
// a running scanner published, or an explicit reason why there is none; it never
// extrapolates a rate from stored token counts.
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
	// The producer's own freshness rule: a signal older than its window is
	// stale, and an expired rate is not a current rate.
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

func schemaFromReport(r store.SchemaReport) Schema {
	return Schema{State: string(r.State), Markers: r.Markers, Missing: r.Missing, Detail: r.Detail}
}

// schemaFromError keeps the classification the read-only open reached. A
// refusal that never classified the schema reports unreadable, never "ok".
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

// readBalances reads the persisted relay sites and their last recorded balance.
// It is the offline source: no credential lookup, no network, no write. A site
// that never recorded a balance stays visible with null amounts, and the status
// reports unavailable rather than zero.
func readBalances(ctx context.Context, ro *store.Store) ([]Balance, Status) {
	sites, err := ro.RelaySites(ctx)
	if err != nil {
		return []Balance{}, Status{Reason: string(store.ReasonUnavailable), Detail: err.Error()}
	}
	if len(sites) == 0 {
		return []Balance{}, Status{Reason: ReasonNoRelaySite}
	}
	out := make([]Balance, 0, len(sites))
	reported := false
	for _, site := range sites {
		b, err := ro.LatestRelayBalance(ctx, site.Origin, site.KeyID)
		if err != nil {
			return []Balance{}, Status{Reason: string(store.ReasonUnavailable), Detail: err.Error()}
		}
		row := Balance{Origin: site.Origin, KeyID: site.KeyID}
		if b != nil {
			row.At = b.At.UTC()
			row.RemainingUSD = b.RemainingUSD
			row.UsedUSD = b.UsedUSD
			row.Unlimited = b.Unlimited
			row.Currency = b.Currency
			if b.RemainingUSD != nil || b.UsedUSD != nil || b.Unlimited {
				reported = true
			}
		}
		out = append(out, row)
	}
	if !reported {
		return out, Status{Reason: ReasonBalanceNotReport, Detail: "no relay key has a recorded balance yet"}
	}
	return out, Status{Available: true}
}

// BalanceSites is exposed for the statusline, which needs the same read-only
// balance source as the envelope.
func BalanceSites(ctx context.Context, ro *store.Store) ([]Balance, Status) {
	return readBalances(ctx, ro)
}

// reportingZone names the zone the caller computed in. An explicitly resolved
// zone wins, then an explicit override, then the process zone. The pair is
// always what the queries actually used: a persisted stats_timezone is only
// reported this way when the caller resolved and ran in it (see
// PersistedStatsTimezone), never inferred from metadata alone. An unresolved
// calendar is handled by the caller, which marks the pair "needs_selection".
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

// systemZoneName labels an unresolved calendar in read-only metadata. It is
// deliberately not a zone name: nothing here is a fixed calendar.
const systemZoneName = "unconfigured"

// PersistedStatsTimezone reads settings.stats_timezone through the read-only
// path. recorded is false when the key is absent, empty, or the index cannot be
// read at all — a missing key is never presented as a persisted timezone. It
// performs no application write (the package comment describes the SQLite read
// coordination it inherits).
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
