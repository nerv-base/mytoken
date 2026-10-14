package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/relay"
	"github.com/zzstar101/mytoken/internal/store"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// openIndex creates a writable index for seeding. The test seals it before any
// read, so the read-only path is the only path exercised.
func openIndex(t *testing.T) (string, string, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mytoken.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open(%s): %v", path, err)
	}
	return dir, path, st
}

// seal closes the store and converts the journal back to DELETE, then removes
// the WAL sidecars. These tests exercise the read-only path on a sealed
// rollback-journal index, where the reader needs no sidecars at all and the byte
// fingerprint is therefore the strongest available statement.
func seal(t *testing.T, st *store.Store, path string) {
	t.Helper()
	if _, err := st.DB().Exec("PRAGMA journal_mode=DELETE"); err != nil {
		t.Fatalf("journal_mode=DELETE: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove %s%s: %v", path, suffix, err)
		}
	}
}

// fingerprint is "name:size:sha256" for every file in dir. Because these indices
// are sealed to rollback-journal format, a read creates nothing, removes nothing
// and rewrites nothing - not even a sidecar.
func fingerprint(t *testing.T, dir string) string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", e.Name(), err)
		}
		sum := sha256.Sum256(raw)
		out = append(out, fmt.Sprintf("%s:%d:%s", e.Name(), len(raw), hex.EncodeToString(sum[:])))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func usageEvent(session, key string, tokens model.Tokens, cost float64, at time.Time) model.UsageEvent {
	e := model.UsageEvent{
		Harness:   model.Codex,
		DedupKey:  key,
		SessionID: session,
		Model:     "gpt-5",
		Provider:  "p",
		BaseURL:   "https://api.example.com",
		Timestamp: at,
		Tokens:    tokens,
	}
	if cost > 0 {
		v := cost
		e.CostUSD = &v
	}
	return e
}

func seed(t *testing.T, st *store.Store, writes ...store.Write) {
	t.Helper()
	if err := st.CommitMany(context.Background(), writes); err != nil {
		t.Fatalf("CommitMany: %v", err)
	}
}

func writePriceCache(t *testing.T, dir string, fetched time.Time) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"fetched": fetched})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prices.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// unavailable index
// ---------------------------------------------------------------------------

func TestSnapshotMissingIndexIsTypedAndNeverZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mytoken.db")
	before := fingerprint(t, dir)

	s := Build(context.Background(), Options{DBPath: path, DataDir: dir})

	if s.SnapshotVersion != Version {
		t.Fatalf("snapshotVersion = %d, want %d", s.SnapshotVersion, Version)
	}
	if s.Sources.Index.Available {
		t.Fatal("index reported available for a missing database")
	}
	if s.Sources.Index.Reason != string(store.ReasonMissingDatabase) {
		t.Fatalf("index reason = %q, want %q", s.Sources.Index.Reason, store.ReasonMissingDatabase)
	}
	if s.Schema.State != string(store.SchemaUnreadable) {
		t.Fatalf("schema state = %q, want %q", s.Schema.State, store.SchemaUnreadable)
	}
	// An unavailable value is not a zero: totals/cache stay null.
	if s.Data.Totals != nil || s.Data.Cache != nil {
		t.Fatalf("data totals/cache = %v/%v, want nil/nil", s.Data.Totals, s.Data.Cache)
	}
	if s.Freshness.EventsThrough != nil || s.Freshness.SessionsThrough != nil {
		t.Fatal("freshness invented timestamps for an unreadable index")
	}
	if s.Sources.Pricing.Available {
		t.Fatal("pricing reported available without a price cache")
	}
	if s.Sources.Balance.Available || s.Sources.Balance.Reason == "" {
		t.Fatalf("balance = %+v, want an unavailable reason", s.Sources.Balance)
	}
	if s.Sources.TokRate.Reason != ReasonNoLiveSignal {
		t.Fatalf("tokRate reason = %q, want %q", s.Sources.TokRate.Reason, ReasonNoLiveSignal)
	}
	if len(s.Writes.Performed) != 0 {
		t.Fatalf("writes performed = %v, want none", s.Writes.Performed)
	}
	if len(s.Writes.Potential) == 0 {
		t.Fatal("writes.potential is empty; an explicit refresh must be explainable")
	}
	if got := fingerprint(t, dir); got != before {
		t.Fatalf("snapshot touched the data directory:\nbefore %s\nafter  %s", before, got)
	}

	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["snapshotVersion"] != float64(Version) {
		t.Fatalf("JSON snapshotVersion = %v", doc["snapshotVersion"])
	}
	data, _ := doc["data"].(map[string]any)
	if data["totals"] != nil || data["cache"] != nil {
		t.Fatalf("JSON data = %v, want null totals/cache", data)
	}
	sources, _ := doc["sources"].(map[string]any)
	index, _ := sources["index"].(map[string]any)
	if index["available"] != false || index["reason"] != string(store.ReasonMissingDatabase) {
		t.Fatalf("JSON sources.index = %v", index)
	}
}

// ---------------------------------------------------------------------------
// readable index
// ---------------------------------------------------------------------------

func TestSnapshotReadsOneEnvelopeAndLeavesStalePricingUntouched(t *testing.T) {
	ctx := context.Background()
	dir, path, st := openIndex(t)
	base := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	seed(t, st,
		store.Write{
			Harness: model.Codex, InstanceID: "a", Path: "log-a",
			Batch: harness.Batch{
				Events:   []model.UsageEvent{usageEvent("s1", "k1", model.Tokens{Input: 100, Output: 10, CacheRead: 300, CacheWrite: 100}, 0.5, base)},
				Sessions: []model.SessionMeta{{Harness: model.Codex, SessionID: "s1", Title: "one", StartedAt: base, UpdatedAt: base}},
				Next:     harness.Cursor{Offset: 1},
			},
		},
		store.Write{
			Harness: model.Codex, InstanceID: "b", Path: "log-b",
			Batch: harness.Batch{
				Events:   []model.UsageEvent{usageEvent("s2", "k2", model.Tokens{Input: 20, Output: 5}, 0.25, base.Add(time.Minute))},
				Sessions: []model.SessionMeta{{Harness: model.Codex, SessionID: "s2", Title: "two", StartedAt: base, UpdatedAt: base.Add(time.Minute)}},
				Next:     harness.Cursor{Offset: 1},
			},
		})
	if err := st.SetSetting(ctx, "daily_timezone", store.LocalDayZone()); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, "stats_timezone", "Asia/Tokyo"); err != nil {
		t.Fatal(err)
	}
	seal(t, st, path)
	stale := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	writePriceCache(t, dir, stale)
	before := fingerprint(t, dir)

	s := Build(ctx, Options{DBPath: path, DataDir: dir})

	if !s.Sources.Index.Available {
		t.Fatalf("index unavailable: %+v", s.Sources.Index)
	}
	if s.Schema.State != string(store.SchemaOK) {
		t.Fatalf("schema state = %q, want %q", s.Schema.State, store.SchemaOK)
	}
	if s.Data.Totals == nil || s.Data.Cache == nil {
		t.Fatal("a readable index must produce totals and cache from the same read")
	}
	if got := s.Data.Totals.Requests; got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
	if got := s.Data.Totals.Tokens.Total(); got != 535 {
		t.Fatalf("tokens = %d, want 535", got)
	}
	if got := s.Data.Totals.CostUSD; got != 0.75 {
		t.Fatalf("cost = %v, want 0.75", got)
	}
	if !s.Sources.Pricing.Available || s.Sources.Pricing.FetchedAt == nil || !s.Sources.Pricing.FetchedAt.Equal(stale) {
		t.Fatalf("pricing = %+v, want the stale cache's timestamp", s.Sources.Pricing)
	}
	if s.Freshness.PricingFetchedAt == nil || !s.Freshness.PricingFetchedAt.Equal(stale) {
		t.Fatalf("freshness.pricingFetchedAt = %v, want %v", s.Freshness.PricingFetchedAt, stale)
	}
	if s.Freshness.SourceModTime == nil {
		t.Fatal("freshness.sourceModTime is nil for an existing index")
	}
	if s.Freshness.EventsThrough == nil || !s.Freshness.EventsThrough.Equal(base.Add(time.Minute)) {
		t.Fatalf("freshness.eventsThrough = %v, want %v", s.Freshness.EventsThrough, base.Add(time.Minute))
	}
	if s.Freshness.SessionsThrough == nil {
		t.Fatal("freshness.sessionsThrough is nil although a session was seeded")
	}
	if s.Freshness.RollupZone != store.LocalDayZone() || !s.Freshness.RollupsCurrent {
		t.Fatalf("rollup freshness = %q/%v, want the local zone and current", s.Freshness.RollupZone, s.Freshness.RollupsCurrent)
	}
	if !s.Freshness.StatsTimezoneRecorded || s.Freshness.StatsTimezone != "Asia/Tokyo" {
		t.Fatalf("statsTimezone = %q recorded=%v, want Asia/Tokyo/true", s.Freshness.StatsTimezone, s.Freshness.StatsTimezoneRecorded)
	}
	// The metadata is always reported, but the effective zone is only the
	// persisted one when the caller resolved it (the CLI does this through
	// PersistedStatsTimezone); the library never claims a zone its caller did
	// not compute in.
	if s.Freshness.EffectiveTimezone != time.Local.String() || s.Freshness.TimezoneSource != "system" {
		t.Fatalf("effective zone = %q (%s), want the process zone", s.Freshness.EffectiveTimezone, s.Freshness.TimezoneSource)
	}
	if tz, recorded := PersistedStatsTimezone(ctx, path); !recorded || tz != "Asia/Tokyo" {
		t.Fatalf("PersistedStatsTimezone = %q/%v, want Asia/Tokyo/true", tz, recorded)
	}
	resolved := Build(ctx, Options{DBPath: path, DataDir: dir, Zone: "Asia/Tokyo", ZoneSource: "stats_timezone"})
	if resolved.Freshness.EffectiveTimezone != "Asia/Tokyo" || resolved.Freshness.TimezoneSource != "stats_timezone" {
		t.Fatalf("resolved zone = %q (%s), want the persisted stats_timezone", resolved.Freshness.EffectiveTimezone, resolved.Freshness.TimezoneSource)
	}
	// No relay site: the balance is unavailable, not $0.
	if s.Sources.Balance.Available || s.Sources.Balance.Reason != ReasonNoRelaySite {
		t.Fatalf("balance = %+v, want %q", s.Sources.Balance, ReasonNoRelaySite)
	}
	// The stale pricing cache and the database bytes are untouched.
	if got := fingerprint(t, dir); got != before {
		t.Fatalf("snapshot rewrote the data directory:\nbefore %s\nafter  %s", before, got)
	}

	// An explicit --timezone is reported as the effective zone and does not
	// rewrite the persisted metadata.
	over := Build(ctx, Options{DBPath: path, DataDir: dir, Timezone: "UTC"})
	if over.Freshness.EffectiveTimezone != "UTC" || over.Freshness.TimezoneSource != "override" {
		t.Fatalf("override zone = %q (%s), want UTC/override", over.Freshness.EffectiveTimezone, over.Freshness.TimezoneSource)
	}
	if over.Freshness.StatsTimezone != "Asia/Tokyo" {
		t.Fatalf("override changed the persisted statsTimezone to %q", over.Freshness.StatsTimezone)
	}
	if got := fingerprint(t, dir); got != before {
		t.Fatal("the timezone override wrote to the data directory")
	}
}

func TestSnapshotAbsentStatsTimezoneIsHonest(t *testing.T) {
	dir, path, st := openIndex(t)
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	seed(t, st, store.Write{
		Harness: model.Codex, InstanceID: "a", Path: "log",
		Batch: harness.Batch{
			Events:   []model.UsageEvent{usageEvent("s1", "k1", model.Tokens{Input: 1}, 0.1, base)},
			Sessions: []model.SessionMeta{{Harness: model.Codex, SessionID: "s1", StartedAt: base, UpdatedAt: base}},
			Next:     harness.Cursor{Offset: 1},
		},
	})
	seal(t, st, path)

	s := Build(context.Background(), Options{DBPath: path, DataDir: dir})
	if s.Freshness.StatsTimezoneRecorded {
		t.Fatal("statsTimezoneRecorded is true without a stats_timezone key")
	}
	if s.Freshness.StatsTimezone != "" {
		t.Fatalf("statsTimezone = %q, want empty", s.Freshness.StatsTimezone)
	}
	if s.Freshness.TimezoneSource != "system" || s.Freshness.EffectiveTimezone != time.Local.String() {
		t.Fatalf("effective zone = %q (%s), want the system zone", s.Freshness.EffectiveTimezone, s.Freshness.TimezoneSource)
	}
	if tz, recorded := PersistedStatsTimezone(context.Background(), path); recorded || tz != "" {
		t.Fatalf("PersistedStatsTimezone = %q/%v, want absent", tz, recorded)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "statsTimezone\"") {
		t.Fatalf("absent stats_timezone must be omitted from JSON: %s", raw)
	}
	if !strings.Contains(string(raw), `"statsTimezoneRecorded":false`) {
		t.Fatalf("JSON must state that the metadata is absent: %s", raw)
	}
}

func TestSnapshotUnresolvedCalendarIsNotAFixedZone(t *testing.T) {
	dir, path, st := openIndex(t)
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	seed(t, st, store.Write{
		Harness: model.Codex, InstanceID: "a", Path: "log",
		Batch: harness.Batch{
			Events:   []model.UsageEvent{usageEvent("s1", "k1", model.Tokens{Input: 1}, 0.1, base)},
			Sessions: []model.SessionMeta{{Harness: model.Codex, SessionID: "s1", StartedAt: base, UpdatedAt: base}},
			Next:     harness.Cursor{Offset: 1},
		},
	})
	seal(t, st, path)
	before := fingerprint(t, dir)

	// The caller could not resolve a fixed calendar (the persisted value is the
	// legacy "Local" or unusable): the envelope must say so, name the process
	// zone as a preview only, and claim no persisted choice.
	s := Build(context.Background(), Options{
		DBPath: path, DataDir: dir,
		ZoneNeedsSelection: true,
		ZoneProblem:        `stats_timezone "Local" is not a usable IANA zone; choose one`,
		ZoneSystemPreview:  "Europe/Berlin",
	})
	if !s.Freshness.NeedsTimezoneSelection {
		t.Fatal("NeedsTimezoneSelection = false, want the unresolved calendar reported")
	}
	if s.Freshness.EffectiveTimezone != "unconfigured" || s.Freshness.TimezoneSource != "needs_selection" {
		t.Fatalf("effective zone = %q (%s), want unconfigured/needs_selection", s.Freshness.EffectiveTimezone, s.Freshness.TimezoneSource)
	}
	if s.Freshness.SystemZonePreview != "Europe/Berlin" {
		t.Fatalf("SystemZonePreview = %q, want the preview only", s.Freshness.SystemZonePreview)
	}
	if !strings.Contains(s.Freshness.TimezoneProblem, "Local") {
		t.Fatalf("TimezoneProblem = %q, want the unusable value named", s.Freshness.TimezoneProblem)
	}

	// An explicit override is a deliberate one-run choice, so it stands on its
	// own even with no fixed calendar persisted.
	over := Build(context.Background(), Options{
		DBPath: path, DataDir: dir,
		Timezone: "UTC", ZoneNeedsSelection: true, ZoneProblem: "unusable",
	})
	if over.Freshness.EffectiveTimezone != "UTC" || over.Freshness.TimezoneSource != "override" {
		t.Fatalf("override zone = %q (%s), want UTC/override", over.Freshness.EffectiveTimezone, over.Freshness.TimezoneSource)
	}
	if over.Freshness.NeedsTimezoneSelection {
		t.Fatal("an explicit override is not an unresolved calendar")
	}
	if got := fingerprint(t, dir); got != before {
		t.Fatal("reporting an unresolved calendar wrote to the data directory")
	}
}

func TestSnapshotFiltersIntersect(t *testing.T) {
	ctx := context.Background()
	dir, path, st := openIndex(t)
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	seed(t, st,
		store.Write{Harness: model.Codex, InstanceID: "a", Path: "a", Batch: harness.Batch{Events: []model.UsageEvent{usageEvent("s1", "k1", model.Tokens{Input: 1}, 0.1, base)}, Next: harness.Cursor{Offset: 1}}},
		store.Write{Harness: model.Codex, InstanceID: "b", Path: "b", Batch: harness.Batch{Events: []model.UsageEvent{usageEvent("s2", "k2", model.Tokens{Input: 1}, 0.1, base)}, Next: harness.Cursor{Offset: 1}}},
	)
	seal(t, st, path)

	cases := []struct {
		name string
		f    query.Filter
		want int64
	}{
		{"instance a", query.Filter{InstanceIDs: []string{"a"}}, 1},
		{"instance b", query.Filter{InstanceIDs: []string{"b"}}, 1},
		{"unknown instance", query.Filter{InstanceIDs: []string{"zzz"}}, 0},
		{"provider and instance intersect", query.Filter{Providers: []string{"p"}, InstanceIDs: []string{"a"}}, 1},
		{"provider and instance exclude", query.Filter{Providers: []string{"p"}, InstanceIDs: []string{"b"}}, 1},
		{"provider mismatch", query.Filter{Providers: []string{"nope"}, InstanceIDs: []string{"a"}}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Build(ctx, Options{DBPath: path, DataDir: dir, Filter: tc.f})
			if !s.Sources.Index.Available {
				t.Fatalf("index unavailable: %+v", s.Sources.Index)
			}
			if s.Data.Totals == nil {
				t.Fatal("totals are null for a readable index")
			}
			if got := s.Data.Totals.Requests; got != tc.want {
				t.Fatalf("requests = %d, want %d", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// statusline
// ---------------------------------------------------------------------------

func TestStatuslineMissingIndexIsUnavailableNotZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mytoken.db")

	sl := BuildStatusline(context.Background(), StatuslineOptions{DBPath: path})
	if sl.Index.Available {
		t.Fatal("statusline reported an index it could not read")
	}
	if sl.Index.Reason != string(store.ReasonMissingDatabase) {
		t.Fatalf("index reason = %q", sl.Index.Reason)
	}
	if sl.Session != nil {
		t.Fatal("statusline invented a session")
	}
	if sl.Balance.Available || sl.Balance.RemainingUSD != nil || sl.Balance.UsedUSD != nil {
		t.Fatalf("balance = %+v, want unavailable with null amounts", sl.Balance)
	}
	raw, err := json.Marshal(sl)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["session"] != nil {
		t.Fatalf("JSON session = %v, want null", doc["session"])
	}
	balance, _ := doc["balance"].(map[string]any)
	if balance["remainingUsd"] != nil || balance["usedUsd"] != nil {
		t.Fatalf("JSON balance = %v, want null amounts", balance)
	}
}

func TestStatuslineReadsSessionCostCacheAndBalance(t *testing.T) {
	ctx := context.Background()
	dir, path, st := openIndex(t)
	base := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)
	seed(t, st, store.Write{
		Harness: model.Codex, InstanceID: "a", Path: "log",
		Batch: harness.Batch{
			Events: []model.UsageEvent{
				usageEvent("s1", "k1", model.Tokens{Input: 100, Output: 10, CacheRead: 300, CacheWrite: 100}, 0.5, base),
				usageEvent("s1", "k2", model.Tokens{Input: 50, Output: 5}, 0.25, base.Add(time.Minute)),
			},
			Sessions: []model.SessionMeta{{Harness: model.Codex, SessionID: "s1", Title: "work", StartedAt: base, UpdatedAt: base.Add(time.Minute)}},
			Next:     harness.Cursor{Offset: 2},
		},
	})
	remaining := 12.5
	site := relay.Site{Origin: "https://relay.example.com", KeyID: "k1", DetectedAt: base}
	if err := st.PutRelaySite(ctx, site); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRelayBalance(ctx, relay.Balance{Origin: site.Origin, KeyID: site.KeyID, At: base, RemainingUSD: &remaining, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, "stats_timezone", "Asia/Tokyo"); err != nil {
		t.Fatal(err)
	}
	seal(t, st, path)
	before := fingerprint(t, dir)

	// The CLI resolves the persisted zone and passes it in; the statusline
	// then reports the zone it actually computed the window in.
	sl := BuildStatusline(ctx, StatuslineOptions{DBPath: path, Zone: "Asia/Tokyo", ZoneSource: "stats_timezone"})
	if !sl.Index.Available {
		t.Fatalf("index unavailable: %+v", sl.Index)
	}
	if sl.Session == nil {
		t.Fatalf("session = nil (%+v)", sl.SessionStatus)
	}
	if sl.Session.SessionID != "s1" || sl.Session.CostSource != "log" {
		t.Fatalf("session = %+v, want s1 with log cost", sl.Session)
	}
	if sl.Session.CostUSD != 0.75 {
		t.Fatalf("session cost = %v, want 0.75", sl.Session.CostUSD)
	}
	// cacheRead 300 over input+cacheRead+cacheWrite = 150+300+100.
	if want := 300.0 / 550.0; sl.Session.CacheHit < want-0.001 || sl.Session.CacheHit > want+0.001 {
		t.Fatalf("cache hit = %v, want %v", sl.Session.CacheHit, want)
	}
	if !sl.Balance.Available || sl.Balance.RemainingUSD == nil || *sl.Balance.RemainingUSD != 12.5 || sl.Balance.Sites != 1 {
		t.Fatalf("balance = %+v, want 12.50 over 1 site", sl.Balance)
	}
	if sl.Timezone != "Asia/Tokyo" || sl.TimezoneSource != "stats_timezone" {
		t.Fatalf("timezone = %q (%s), want the persisted stats_timezone", sl.Timezone, sl.TimezoneSource)
	}
	if got := fingerprint(t, dir); got != before {
		t.Fatalf("statusline rewrote the data directory:\nbefore %s\nafter  %s", before, got)
	}

	// A site that never recorded a balance stays unavailable, not zero.
	_, path2, st2 := openIndex(t)
	seed(t, st2, store.Write{Harness: model.Codex, InstanceID: "a", Path: "log", Batch: harness.Batch{Events: []model.UsageEvent{usageEvent("s1", "k1", model.Tokens{Input: 1}, 0.1, base)}, Sessions: []model.SessionMeta{{Harness: model.Codex, SessionID: "s1", StartedAt: base, UpdatedAt: base}}, Next: harness.Cursor{Offset: 1}}})
	if err := st2.PutRelaySite(ctx, relay.Site{Origin: "https://relay.example.com", KeyID: "k2", DetectedAt: base}); err != nil {
		t.Fatal(err)
	}
	seal(t, st2, path2)
	sl2 := BuildStatusline(ctx, StatuslineOptions{DBPath: path2})
	if sl2.Balance.Available || sl2.Balance.Reason != ReasonBalanceNotReport {
		t.Fatalf("balance = %+v, want %q", sl2.Balance, ReasonBalanceNotReport)
	}
	if sl2.Balance.RemainingUSD != nil {
		t.Fatal("an unreported balance must not be rendered as a zero")
	}
}

func TestStatuslineExplicitSessionWithoutInstanceIsTyped(t *testing.T) {
	ctx := context.Background()
	_, path, st := openIndex(t)
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	seed(t, st, store.Write{
		Harness: model.Codex, InstanceID: "a", Path: "log",
		Batch: harness.Batch{
			Events:   []model.UsageEvent{usageEvent("s1", "k1", model.Tokens{Input: 1}, 0.1, base)},
			Sessions: []model.SessionMeta{{Harness: model.Codex, SessionID: "s1", StartedAt: base, UpdatedAt: base}},
			Next:     harness.Cursor{Offset: 1},
		},
	})
	seal(t, st, path)

	missing := BuildStatusline(ctx, StatuslineOptions{DBPath: path, SessionID: "nope"})
	if missing.Session != nil {
		t.Fatal("statusline returned a session that does not exist")
	}
	if missing.SessionStatus.Reason != "session_unavailable" {
		t.Fatalf("session status = %+v, want session_unavailable", missing.SessionStatus)
	}
	found := BuildStatusline(ctx, StatuslineOptions{DBPath: path, SessionID: "s1"})
	if found.Session == nil || found.Session.SessionID != "s1" {
		t.Fatalf("explicit session = %+v (%+v)", found.Session, found.SessionStatus)
	}
}

// putBalance seeds one relay site and, unless every reading is absent, its last
// recorded balance. A site with no balance row is a key a scan saw that never
// reported an amount: it must stay visible without becoming $0.
func putBalance(t *testing.T, st *store.Store, origin, keyID string, at time.Time, remaining, used *float64, unlimited bool, currency string) {
	t.Helper()
	if err := st.PutRelaySite(context.Background(), relay.Site{Origin: origin, KeyID: keyID, DetectedAt: at}); err != nil {
		t.Fatal(err)
	}
	if remaining == nil && used == nil && !unlimited {
		return
	}
	if err := st.PutRelayBalance(context.Background(), relay.Balance{
		Origin: origin, KeyID: keyID, At: at,
		RemainingUSD: remaining, UsedUSD: used, Unlimited: unlimited, Currency: currency,
	}); err != nil {
		t.Fatal(err)
	}
}

// balanceByKey finds one relay account by the key id the index recorded.
func balanceByKey(t *testing.T, b BalanceLine, keyID string) Balance {
	t.Helper()
	for _, a := range b.Accounts {
		if a.KeyID == keyID {
			return a
		}
	}
	t.Fatalf("no account for key %q in %+v", keyID, b.Accounts)
	return Balance{}
}

// TestStatuslineNeverMergesRelayAccounts is the core of the no-blind-summing
// rule: several relay keys may belong to one upstream account, so their
// balances are never added, their currencies never mixed and no account's
// timestamp is presented as the freshest reading of them all.
func TestStatuslineNeverMergesRelayAccounts(t *testing.T) {
	ctx := context.Background()
	const origin = "https://relay.example.com"
	base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	firstAt, secondAt := base, base.Add(30*time.Minute)

	line := func(t *testing.T, seedSites func(st *store.Store)) BalanceLine {
		t.Helper()
		_, path, st := openIndex(t)
		seed(t, st, store.Write{Harness: model.Codex, InstanceID: "a", Path: "log", Batch: harness.Batch{
			Events:   []model.UsageEvent{usageEvent("s1", "e1", model.Tokens{Input: 1}, 0.1, base)},
			Sessions: []model.SessionMeta{{Harness: model.Codex, SessionID: "s1", StartedAt: base, UpdatedAt: base}},
			Next:     harness.Cursor{Offset: 1},
		}})
		seedSites(st)
		seal(t, st, path)
		sl := BuildStatusline(ctx, StatuslineOptions{DBPath: path})
		if !sl.Index.Available {
			t.Fatalf("index unavailable: %+v", sl.Index)
		}
		return sl.Balance
	}

	t.Run("two keys of one origin are two accounts, never a sum", func(t *testing.T) {
		v10, v20 := 10.0, 20.0
		b := line(t, func(st *store.Store) {
			putBalance(t, st, origin, "key-one", firstAt, &v10, nil, false, "USD")
			putBalance(t, st, origin, "key-two", secondAt, &v20, nil, false, "USD")
		})
		if b.Available {
			t.Fatalf("balance available over two keys: %+v", b)
		}
		if b.Reason != ReasonMultipleBalances {
			t.Fatalf("reason = %q, want %q", b.Reason, ReasonMultipleBalances)
		}
		if b.RemainingUSD != nil || b.UsedUSD != nil || b.Unlimited || b.Currency != "" || b.At != nil {
			t.Fatalf("a scalar survived over two keys: %+v", b)
		}
		if b.Sites != 2 || b.Recorded != 2 || b.Partial {
			t.Fatalf("sites=%d recorded=%d partial=%t, want 2/2/false", b.Sites, b.Recorded, b.Partial)
		}
		if len(b.Accounts) != 2 {
			t.Fatalf("accounts = %+v, want both rows", b.Accounts)
		}
		one := balanceByKey(t, b, "key-one")
		two := balanceByKey(t, b, "key-two")
		if one.Origin != origin || two.Origin != origin {
			t.Fatalf("origins = %q/%q, want the original %q", one.Origin, two.Origin, origin)
		}
		if one.RemainingUSD == nil || *one.RemainingUSD != 10 || two.RemainingUSD == nil || *two.RemainingUSD != 20 {
			t.Fatalf("accounts lost their own amounts: %+v", b.Accounts)
		}
		// Each account keeps its own timestamp; the line keeps none.
		if !one.At.Equal(firstAt) || !two.At.Equal(secondAt) {
			t.Fatalf("account timestamps = %v/%v, want %v/%v", one.At, two.At, firstAt, secondAt)
		}

		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		if doc["remainingUsd"] != nil || doc["usedUsd"] != nil || doc["at"] != nil || doc["available"] != false {
			t.Fatalf("JSON exposes a merged scalar: %v", doc)
		}
		accounts, _ := doc["accounts"].([]any)
		if len(accounts) != 2 {
			t.Fatalf("JSON accounts = %v", doc["accounts"])
		}
		first, _ := accounts[0].(map[string]any)
		if first["origin"] != origin || first["keyId"] != "key-one" || first["remainingUsd"] != 10.0 {
			t.Fatalf("JSON account = %v", first)
		}
	})

	t.Run("different currencies are never combined", func(t *testing.T) {
		usd, cny := 10.0, 70.0
		b := line(t, func(st *store.Store) {
			putBalance(t, st, origin, "key-one", firstAt, &usd, nil, false, "USD")
			putBalance(t, st, origin, "key-two", secondAt, &cny, nil, false, "CNY")
		})
		if b.Available || b.Reason != ReasonMixedCurrency {
			t.Fatalf("balance = %+v, want %q", b, ReasonMixedCurrency)
		}
		if b.RemainingUSD != nil || b.At != nil {
			t.Fatalf("mixed currencies produced a scalar: %+v", b)
		}
		if !strings.Contains(b.Detail, "CNY") {
			t.Fatalf("detail = %q, want the disagreeing currency named", b.Detail)
		}
	})

	t.Run("a second silent key still blocks the scalar and marks it partial", func(t *testing.T) {
		v10 := 10.0
		b := line(t, func(st *store.Store) {
			putBalance(t, st, origin, "key-one", firstAt, &v10, nil, false, "USD")
			putBalance(t, st, origin, "key-two", secondAt, nil, nil, false, "")
		})
		// Two keys are two possibly-shared accounts: even though only one
		// recorded an amount, that amount is not reported as the account balance.
		if b.Available || b.Reason != ReasonMultipleBalances {
			t.Fatalf("balance = %+v, want %q", b, ReasonMultipleBalances)
		}
		if b.RemainingUSD != nil || b.UsedUSD != nil || b.At != nil {
			t.Fatalf("a scalar survived a second key: %+v", b)
		}
		if !b.Partial || b.Sites != 2 || b.Recorded != 1 {
			t.Fatalf("sites=%d recorded=%d partial=%t, want 2/1/true", b.Sites, b.Recorded, b.Partial)
		}
		one := balanceByKey(t, b, "key-one")
		if one.RemainingUSD == nil || *one.RemainingUSD != 10 || one.Currency != "USD" || !one.At.Equal(firstAt) {
			t.Fatalf("key-one lost its own reading: %+v", one)
		}
		silent := balanceByKey(t, b, "key-two")
		if silent.RemainingUSD != nil || silent.UsedUSD != nil || silent.Unlimited {
			t.Fatalf("a key without a reading reported one: %+v", silent)
		}
	})

	t.Run("one key alone is the only case with a scalar", func(t *testing.T) {
		v12 := 12.5
		b := line(t, func(st *store.Store) {
			putBalance(t, st, origin, "key-one", firstAt, &v12, nil, false, "USD")
		})
		if !b.Available || b.RemainingUSD == nil || *b.RemainingUSD != 12.5 || b.Currency != "USD" {
			t.Fatalf("balance = %+v, want key-one's own 12.50", b)
		}
		if b.Recorded != 1 || b.Partial || len(b.Accounts) != 1 {
			t.Fatalf("recorded=%d partial=%t accounts=%d, want 1/false/1", b.Recorded, b.Partial, len(b.Accounts))
		}
		if b.At == nil || !b.At.Equal(firstAt) {
			t.Fatalf("at = %v, want the account's own %v", b.At, firstAt)
		}
	})

	t.Run("unlimited is a recorded reading, not an amount", func(t *testing.T) {
		b := line(t, func(st *store.Store) {
			putBalance(t, st, origin, "key-one", firstAt, nil, nil, true, "USD")
		})
		if !b.Available || !b.Unlimited || b.RemainingUSD != nil || b.UsedUSD != nil {
			t.Fatalf("balance = %+v, want unlimited with null amounts", b)
		}
		if b.Recorded != 1 || b.Partial {
			t.Fatalf("recorded=%d partial=%t, want 1/false", b.Recorded, b.Partial)
		}
	})

	t.Run("a key without a recorded balance is unavailable, never zero", func(t *testing.T) {
		b := line(t, func(st *store.Store) {
			putBalance(t, st, origin, "key-one", firstAt, nil, nil, false, "")
		})
		if b.Available || b.Reason != ReasonBalanceNotReport {
			t.Fatalf("balance = %+v, want %q", b, ReasonBalanceNotReport)
		}
		if b.RemainingUSD != nil || b.UsedUSD != nil || b.At != nil || b.Recorded != 0 {
			t.Fatalf("an unrecorded balance became a value: %+v", b)
		}
		if b.Sites != 1 || len(b.Accounts) != 1 || b.Accounts[0].KeyID != "key-one" {
			t.Fatalf("the known key vanished: %+v", b)
		}
	})
}
