package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

// The two oracles below are the fact-side definition of the endpoint-aware
// rollups: they group the events view by exactly the rollup key and use exactly
// the accounting expressions daily_usage/cache_daily use. A rollup row that is
// not in the oracle (or vice versa) is a divergence between a derived summary and
// the history it claims to summarize.
const providerDailyOracle = `SELECT mytoken_day(timestamp),harness,instance_id,session_id,resolved_provider,model,project,base_url,attrib,sum(input),sum(output),sum(cache_read),sum(cache_write),sum(reasoning),sum(CASE WHEN priced THEN cost ELSE 0 END),sum(CASE WHEN log_cost IS NOT NULL THEN cost ELSE 0 END),sum(CASE WHEN log_cost IS NULL AND priced THEN cost ELSE 0 END),count(*),sum(NOT priced) FROM events GROUP BY 1,2,3,4,5,6,7,8,9`

const providerCacheOracle = `SELECT mytoken_day(timestamp),harness,instance_id,session_id,resolved_provider,model,project,base_url,miss_cause,count(*),sum(cache_expected>=0),sum(max(cache_expected,0)),sum(CASE WHEN cache_expected>=0 THEN cache_read ELSE 0 END),sum(cache_miss),sum(miss_cost) FROM events GROUP BY 1,2,3,4,5,6,7,8,9`

// assertRollupMatchesFacts diffs each rollup against its oracle in both
// directions, so a dropped, duplicated or mislabelled group fails.
func assertRollupMatchesFacts(t *testing.T, st *Store) {
	t.Helper()
	for _, tt := range []struct{ table, columns, oracle string }{
		{"provider_daily", providerDailyDimensions + "," + dailyMetrics, providerDailyOracle},
		{"provider_cache_daily", providerCacheDimensions + "," + cacheMetrics, providerCacheOracle},
	} {
		sel := "SELECT " + tt.columns + " FROM " + tt.table + " "
		for label, q := range map[string]string{
			"rollup has rows the facts do not":  "SELECT count(*) FROM (" + sel + "EXCEPT " + tt.oracle + ")",
			"facts have rows the rollup misses": "SELECT count(*) FROM (" + tt.oracle + " EXCEPT " + sel + ")",
		} {
			var n int
			if err := st.DB().QueryRow(q).Scan(&n); err != nil {
				t.Fatalf("%s: %v", tt.table, err)
			}
			if n != 0 {
				t.Fatalf("%s: %s (%d rows)", tt.table, label, n)
			}
		}
	}
}

type factSnapshot struct {
	rows, input, identities, identityIDs int
	cost                                 float64
}

func readFactSnapshot(t *testing.T, path string) factSnapshot {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var s factSnapshot
	if err = db.QueryRow("SELECT count(*),sum(input),sum(cost) FROM events").Scan(&s.rows, &s.input, &s.cost); err != nil {
		t.Fatal(err)
	}
	return s
}

func readIdentitySnapshot(t *testing.T, path string) factSnapshot {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var s factSnapshot
	if err = db.QueryRow("SELECT count(*),count(DISTINCT provider_id) FROM provider_endpoints").Scan(&s.identities, &s.identityIDs); err != nil {
		t.Fatal(err)
	}
	return s
}

// rollupFixtureEvents crosses pricing names over one endpoint, puts one pricing
// name behind two tenants, and keeps raw URLs that cannot prove endpoint
// equivalence. Costs cover estimate-priced, log-priced and unpriced events, and
// two providers share one harness/session/raw-URL triple without a resolved
// provider in common, so they must stay separate unresolved identities.
func rollupFixtureEvents() []struct {
	event      model.UsageEvent
	resolution Resolution
} {
	day := time.Date(2026, 4, 6, 0, 0, 0, 0, time.Local)
	logCost := 1.25
	specs := []struct {
		key, session, project, provider, base string
		at                                    time.Time
		input                                 int64
		logCost                               *float64
		estimated                             float64
		priced                                bool
	}{
		{"a", "s1", "/p/one", "name-a", "https://api.example.com/v1", day.Add(time.Hour), 10, nil, 0.5, true},
		{"b", "s1", "/p/one", "name-b", "https://api.example.com:443/v1/", day.Add(2 * time.Hour), 20, &logCost, 0, true},
		{"c", "s1", "/p/two", "name-a", "https://api.example.com/v1/alt", day.Add(3 * time.Hour), 30, nil, 0, false},
		{"d", "s2", "/p/one", "name-c", "https://api.example.com/v1?key=x", day.Add(25 * time.Hour), 40, nil, 2, true},
		{"e", "s2", "/p/one", "name-c", "https://api.example.com/v1?key=x", day.Add(26 * time.Hour), 50, &logCost, 0, true},
		{"f", "s2", "/p/one", "name-d", "https://api.example.com/v1?key=x", day.Add(27 * time.Hour), 70, nil, 0, false},
	}
	out := make([]struct {
		event      model.UsageEvent
		resolution Resolution
	}, 0, len(specs))
	for _, s := range specs {
		e := model.UsageEvent{Harness: model.Codex, DedupKey: s.key, SessionID: s.session, Timestamp: s.at, ProjectPath: s.project, Model: "m", Provider: s.provider, BaseURL: s.base, CostUSD: s.logCost, Tokens: model.Tokens{Input: s.input, Output: s.input / 2}}
		out = append(out, struct {
			event      model.UsageEvent
			resolution Resolution
		}{e, Resolution{Provider: s.provider, Attrib: model.AttribLog, Cost: s.estimated, Priced: s.priced}})
	}
	return out
}

func commitRollupFixture(t *testing.T, st *Store, ctx context.Context) {
	t.Helper()
	for i, item := range rollupFixtureEvents() {
		if err := st.Commit(ctx, model.Codex, "fixture", harness.Batch{Events: []model.UsageEvent{item.event}}, []Resolution{item.resolution}); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
}

// rollupFixture leaves a populated index in the pre-rollup shape: neither the
// identity index nor the endpoint-aware rollups exist, so one Open() must run
// both migrations inside a single transaction.
func rollupFixture(t *testing.T, path string) {
	t.Helper()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	commitRollupFixture(t, st, context.Background())
	if err := st.Exec(context.Background(), "DROP TABLE IF EXISTS provider_daily; DROP TABLE IF EXISTS provider_cache_daily; DROP TABLE IF EXISTS provider_endpoints; DELETE FROM settings WHERE key IN ('provider.endpoint-rollup.v1','provider.identity.v1'); INSERT OR REPLACE INTO settings VALUES('pricing_fingerprint','unchanged')"); err != nil {
		t.Fatal(err)
	}
}

func TestProviderRollupMigrationMatchesFacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	rollupFixture(t, path)
	before := readFactSnapshot(t, path)
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	assertRollupMatchesFacts(t, st)
	if got, err := st.Setting(context.Background(), providerRollupVersion); err != nil || got != "1" {
		t.Fatalf("rollup marker: %q %v", got, err)
	}
	// The derived backfill must not touch history, the identity index or prices.
	after := readFactSnapshot(t, path)
	if after != before {
		t.Fatalf("facts changed: %+v → %+v", before, after)
	}
	if after.rows != 6 || after.input != 220 {
		t.Fatalf("unexpected fixture shape: %+v", after)
	}
	if got, err := st.Setting(context.Background(), "pricing_fingerprint"); err != nil || got != "unchanged" {
		t.Fatalf("repricing checkpoint changed: %q %v", got, err)
	}
	// One endpoint behind two pricing names, one tenant behind a second endpoint,
	// and one URL that cannot normalize: three dictionary rows, two providers.
	if got := readIdentitySnapshot(t, path); got.identities != 3 || got.identityIDs != 2 {
		t.Fatalf("identity index: %d/%d", got.identities, got.identityIDs)
	}
	// A second open must be a no-op: the marker means the rollups are complete.
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertRollupMatchesFacts(t, reopened)
}

// TestProviderRollupMigrationOnIdentityMarkedIndex covers the upgrade path where
// provider.identity.v1 is already set, so the rollups cannot ride along in the
// identity transaction and must take their own backup.
func TestProviderRollupMigrationOnIdentityMarkedIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	commitRollupFixture(t, st, context.Background())
	if err := st.Exec(context.Background(), "DROP TABLE IF EXISTS provider_daily; DROP TABLE IF EXISTS provider_cache_daily; DELETE FROM settings WHERE key='provider.endpoint-rollup.v1'"); err != nil {
		t.Fatal(err)
	}
	before := readFactSnapshot(t, path)
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertRollupMatchesFacts(t, reopened)
	if after := readFactSnapshot(t, path); after != before {
		t.Fatalf("facts changed: %+v → %+v", before, after)
	}
	rollupBackup := path + ".pre-provider-endpoint-rollup-v1.db"
	if _, err = os.Stat(rollupBackup); err != nil {
		t.Fatalf("missing endpoint-rollup backup: %v", err)
	}
	// The identity backup was already consumed by the earlier migration, so this
	// run must not claim it, and it must not overwrite or reuse anything.
	if _, err = os.Stat(path + ".pre-provider-v1.db"); !os.IsNotExist(err) {
		t.Fatalf("identity backup written by the rollup migration: %v", err)
	}
	if got, err := reopened.Setting(context.Background(), providerRollupVersion); err != nil || got != "1" {
		t.Fatalf("rollup marker: %q %v", got, err)
	}
}

func TestProviderRollupMaintenanceTracksEvents(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	commitRollupFixture(t, st, ctx)
	assertRollupMatchesFacts(t, st)

	// A new event lands in an existing group.
	day := time.Date(2026, 4, 6, 0, 0, 0, 0, time.Local)
	if err = st.Commit(ctx, model.Codex, "fixture", harness.Batch{Events: []model.UsageEvent{{Harness: model.Codex, DedupKey: "g", SessionID: "s1", Timestamp: day.Add(4 * time.Hour), ProjectPath: "/p/one", Model: "m", Provider: "name-a", BaseURL: "https://api.example.com/v1", Tokens: model.Tokens{Input: 60, Output: 3}}}}, []Resolution{{Provider: "name-a", Cost: 1.5, Priced: true}}); err != nil {
		t.Fatal(err)
	}
	assertRollupMatchesFacts(t, st)

	// Moving an event to another project swaps its dimension row.
	if err = st.Exec(ctx, "UPDATE events SET project='/p/moved' WHERE dedup_key='c'"); err != nil {
		t.Fatal(err)
	}
	assertRollupMatchesFacts(t, st)

	// Repricing rewrites cost/priced in place and must stay reflected.
	if err = st.EnsurePrices(ctx, "rollup-fixture-2", func(model.UsageEvent) (float64, bool) { return 3.5, true }); err != nil {
		t.Fatal(err)
	}
	assertRollupMatchesFacts(t, st)

	// Removing an event drains its group.
	if err = st.Exec(ctx, "DELETE FROM events WHERE dedup_key='e'"); err != nil {
		t.Fatal(err)
	}
	assertRollupMatchesFacts(t, st)
	// A group that lost its last event must not survive as a zero row.
	var leftovers int
	if err = st.DB().QueryRow("SELECT count(*) FROM provider_daily WHERE requests=0").Scan(&leftovers); err != nil || leftovers != 0 {
		t.Fatalf("drained groups survived: %d %v", leftovers, err)
	}
}

func TestProviderRollupInvariantFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	rollupFixture(t, path)
	before := readFactSnapshot(t, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TRIGGER corrupt_rollup_migration AFTER INSERT ON settings WHEN NEW.key='provider.endpoint-rollup.v1' BEGIN UPDATE event_data SET cost=cost+1; END`); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err == nil {
		st.Close()
		t.Fatal("invariant violation was committed")
	}
	if after := readFactSnapshot(t, path); after != before {
		t.Fatalf("cost not rolled back: %+v → %+v", before, after)
	}
	var marker, tables int
	if err = db.QueryRow("SELECT count(*) FROM settings WHERE key IN ('provider.endpoint-rollup.v1','provider.identity.v1')").Scan(&marker); err != nil || marker != 0 {
		t.Fatalf("marker committed: %d %v", marker, err)
	}
	if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('provider_daily','provider_cache_daily')").Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("rollup DDL not rolled back: %d %v", tables, err)
	}
}

func TestProviderRollupPartialStateRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	commitRollupFixture(t, st, context.Background())
	// A rollup table without the marker cannot be trusted to hold a complete
	// backfill, so opening must stop instead of silently using it.
	if err = st.Exec(context.Background(), "DELETE FROM settings WHERE key='provider.endpoint-rollup.v1'"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if st, err = Open(path); err == nil {
		st.Close()
		t.Fatal("incomplete rollup state was accepted")
	} else if !strings.Contains(err.Error(), "inconsistent") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProviderRollupBackupFailurePreservesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	commitRollupFixture(t, st, context.Background())
	if err = st.Exec(context.Background(), "DROP TABLE IF EXISTS provider_daily; DROP TABLE IF EXISTS provider_cache_daily; DELETE FROM settings WHERE key='provider.endpoint-rollup.v1'"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	backup := path + ".pre-provider-endpoint-rollup-v1.db"
	if err = os.WriteFile(backup, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if st, err = Open(path); err == nil {
		st.Close()
		t.Fatal("rollup migration proceeded without a new consistent backup")
	}
	got, err := os.ReadFile(backup)
	if err != nil || string(got) != "existing" {
		t.Fatal("rollup backup overwritten")
	}
}
