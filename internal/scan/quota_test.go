package scan

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness/codex"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/quota"
	"github.com/zzstar101/mytoken/internal/store"
)

const quotaRollout = "rollout-2026-01-02T03-04-05-0199aaaa-bbbb-cccc-dddd-eeeeffff0001.jsonl"

// quotaLine is a token_count record that carries rate_limits but no usage at
// all: the quota-only case this ingestion path exists for.
func quotaLine(ts, rateLimits string) string {
	return `{"timestamp":"` + ts + `","type":"event_msg","payload":{"type":"token_count","rate_limits":` + rateLimits + `}}`
}

// usageLine is a token_count record with usage, optionally carrying rate_limits.
func usageLine(ts, rateLimits string) string {
	return usageLineInput(ts, 1000, rateLimits)
}

// usageLineInput is usageLine with an explicit cumulative input count, so two
// records in one rollout are not deduplicated as the same cumulative snapshot.
func usageLineInput(ts string, input int, rateLimits string) string {
	info := `{"total_token_usage":{"input_tokens":` + strconv.Itoa(input) + `,"cached_input_tokens":400,"output_tokens":50,"reasoning_output_tokens":20,"total_tokens":` + strconv.Itoa(input+50) + `},"last_token_usage":{"input_tokens":1000,"cached_input_tokens":400,"output_tokens":50,"reasoning_output_tokens":20,"total_tokens":1050}}`
	p := `{"type":"token_count","info":` + info
	if rateLimits != "" {
		p += `,"rate_limits":` + rateLimits
	}
	p += `}`
	return `{"timestamp":"` + ts + `","type":"event_msg","payload":` + p + `}`
}

func writeRollout(t *testing.T, root, name string, lines ...string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func countRows(t *testing.T, st *store.Store, query string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRowContext(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func loadQuota(t *testing.T, st *store.Store, instance string) *quota.Latest {
	t.Helper()
	latest, ok, err := quota.LoadLatest(context.Background(), st, instance)
	if err != nil {
		t.Fatalf("LoadLatest(%q): %v", instance, err)
	}
	if !ok {
		t.Fatalf("LoadLatest(%q): no observation persisted", instance)
	}
	return latest
}

func percentOf(t *testing.T, w *quota.Window) float64 {
	t.Helper()
	if w == nil || w.UsedPercent == nil {
		t.Fatalf("window = %+v, want a used_percent", w)
	}
	return *w.UsedPercent
}

// TestScanPublishesQuotaOnlyObservation covers the core requirement: a rollout
// whose only quota record carries no usage event still persists a bounded
// latest observation, and its cursor advances normally.
func TestScanPublishesQuotaOnlyObservation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := writeRollout(t, root, quotaRollout, quotaLine("2026-01-02T03:04:05Z",
		`{"limit_id":"codex","limit_name":"Codex","primary":{"used_percent":42.5,"window_minutes":300,"resets_in_seconds":600},"plan_type":"plus"}`))
	st := openStore(t)
	sc := New(st, nil, nil, codex.NewWithRoots(root))
	if err := sc.scan(ctx, false); err != nil {
		t.Fatal(err)
	}

	latest := loadQuota(t, st, store.DefaultInstanceID)
	rep := latest.Report
	if latest.Harness != quota.HarnessCodex || latest.Instance != "default" || latest.Source != path {
		t.Fatalf("latest = %+v", *latest)
	}
	if latest.IngestedAt.IsZero() {
		t.Fatal("ingested_at is zero")
	}
	if rep.Status != quota.StatusOK || rep.Reason != quota.ReasonNone {
		t.Fatalf("status = %q/%q, want ok", rep.Status, rep.Reason)
	}
	if got := percentOf(t, rep.Primary); got != 42.5 {
		t.Fatalf("used_percent = %v, want 42.5", got)
	}
	if rep.Primary.RemainingPercent == nil || *rep.Primary.RemainingPercent != 57.5 || !rep.Primary.RemainingDerived {
		t.Fatalf("remaining = %+v, want derived 57.5", rep.Primary)
	}
	if rep.LimitID != "codex" || rep.PlanType == nil || *rep.PlanType != "plus" {
		t.Fatalf("provenance = %q/%v", rep.LimitID, rep.PlanType)
	}
	if rep.ObservedAt == nil || !rep.ObservedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("observed_at = %v", rep.ObservedAt)
	}
	if n := countRows(t, st, "SELECT count(*) FROM events"); n != 0 {
		t.Fatalf("%d events, want 0: quota must not be inferred from or create usage", n)
	}
	if n := countRows(t, st, "SELECT count(*) FROM settings WHERE key LIKE 'quota.codex.%'"); n != 1 {
		t.Fatalf("%d quota settings, want 1", n)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := st.CursorIn(ctx, model.Codex, store.DefaultInstanceID, path)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Offset != info.Size() || cur.Offset == 0 {
		t.Fatalf("cursor offset = %d, want %d", cur.Offset, info.Size())
	}
}

// TestScanKeepsQuotaFromFastSkippedRecord proves the fast skip path cannot drop
// a quota observation: an event_msg the scanner would otherwise skip still
// contributes when it carries rate_limits.
func TestScanKeepsQuotaFromFastSkippedRecord(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeRollout(t, root, quotaRollout,
		`{"timestamp":"2026-01-02T03:04:05Z","type":"event_msg","payload":{"type":"agent_reasoning","text":"thinking","rate_limits":{"primary":{"used_percent":12.0},"plan_type":"pro"}}}`)
	st := openStore(t)
	sc := New(st, nil, nil, codex.NewWithRoots(root))
	if err := sc.scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	rep := loadQuota(t, st, store.DefaultInstanceID).Report
	if rep.Status != quota.StatusOK || percentOf(t, rep.Primary) != 12.0 {
		t.Fatalf("report = %+v, want an ok 12%% observation", *rep)
	}
	if n := countRows(t, st, "SELECT count(*) FROM events"); n != 0 {
		t.Fatalf("%d events, want 0", n)
	}
}

// TestScanPublishesQuotaPerInstance keeps two Codex instances' observations
// separate, each under its own stable setting key.
func TestScanPublishesQuotaPerInstance(t *testing.T) {
	ctx := context.Background()
	rootA, rootB := t.TempDir(), t.TempDir()
	writeRollout(t, rootA, quotaRollout, quotaLine("2026-01-02T03:04:05Z", `{"primary":{"used_percent":10.0},"plan_type":"plus"}`))
	writeRollout(t, rootB, quotaRollout, quotaLine("2026-01-02T03:04:05Z", `{"primary":{"used_percent":20.0},"plan_type":"pro"}`))
	st := openStore(t)
	sc, err := NewWithInstances(st, nil, nil,
		Instance{ID: "alpha", Parser: codex.NewWithRoots(rootA)},
		Instance{ID: "beta", Parser: codex.NewWithRoots(rootB)})
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	alpha := loadQuota(t, st, "alpha")
	beta := loadQuota(t, st, "beta")
	if alpha.Instance != "alpha" || percentOf(t, alpha.Report.Primary) != 10.0 {
		t.Fatalf("alpha = %+v", *alpha)
	}
	if beta.Instance != "beta" || percentOf(t, beta.Report.Primary) != 20.0 {
		t.Fatalf("beta = %+v", *beta)
	}
	rawA, err := st.Setting(ctx, quota.SettingKey("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	rawB, err := st.Setting(ctx, quota.SettingKey("beta"))
	if err != nil {
		t.Fatal(err)
	}
	if rawA == "" || rawB == "" || rawA == rawB {
		t.Fatalf("stored values are not separate: %q / %q", rawA, rawB)
	}
	if n := countRows(t, st, "SELECT count(*) FROM settings WHERE key LIKE 'quota.codex.%'"); n != 2 {
		t.Fatalf("%d quota settings, want 2", n)
	}
	if n := countRows(t, st, "SELECT count(*) FROM settings WHERE key = '"+quota.SettingKey("default")+"'"); n != 0 {
		t.Fatal("an instance-less key was written for a multi-instance scan")
	}
	if quota.SettingKey("alpha") == quota.SettingKey("beta") {
		t.Fatal("instance keys collide")
	}
}

// TestScanQuotaDoesNotChangeUsageAccounting compares two identical rollouts that
// differ only by rate_limits: the quota field is additive.
func TestScanQuotaDoesNotChangeUsageAccounting(t *testing.T) {
	ctx := context.Background()
	aggregate := func(withQuota bool) (count int, in, out, cost float64) {
		root := t.TempDir()
		rl := ""
		if withQuota {
			rl = `{"primary":{"used_percent":33.0},"plan_type":"plus"}`
		}
		writeRollout(t, root, quotaRollout,
			usageLine("2026-01-02T03:04:05Z", rl),
			usageLine("2026-01-02T03:06:05Z", rl))
		st := openStore(t)
		sc := New(st, nil, nil, codex.NewWithRoots(root))
		if err := sc.scan(ctx, false); err != nil {
			t.Fatal(err)
		}
		if err := st.DB().QueryRowContext(ctx,
			"SELECT count(*), coalesce(sum(input),0), coalesce(sum(output),0), coalesce(sum(cost),0) FROM events").
			Scan(&count, &in, &out, &cost); err != nil {
			t.Fatal(err)
		}
		if withQuota {
			loadQuota(t, st, store.DefaultInstanceID)
		}
		return count, in, out, cost
	}
	c1, i1, o1, cost1 := aggregate(false)
	c2, i2, o2, cost2 := aggregate(true)
	if c1 != c2 || i1 != i2 || o1 != o2 || cost1 != cost2 {
		t.Fatalf("with quota = %d/%v/%v/%v, without = %d/%v/%v/%v", c2, i2, o2, cost2, c1, i1, o1, cost1)
	}
	if c1 == 0 {
		t.Fatal("no usage events were produced; the comparison proves nothing")
	}
}

// TestScanQuotaNewerInvalidDoesNotRefresh keeps an older usable observation from
// looking fresh when a later, unusable record arrives by incremental append.
func TestScanQuotaNewerInvalidDoesNotRefresh(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := writeRollout(t, root, quotaRollout,
		quotaLine("2026-01-02T03:04:05Z", `{"primary":{"used_percent":10.0},"plan_type":"plus"}`))
	st := openStore(t)
	sc := New(st, nil, nil, codex.NewWithRoots(root))
	if err := sc.scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	if rep := loadQuota(t, st, store.DefaultInstanceID).Report; rep.Status != quota.StatusOK {
		t.Fatalf("first scan status = %q/%q", rep.Status, rep.Reason)
	}

	appendLine(t, path, quotaLine("2026-01-02T04:04:05Z", `{"primary":{"used_percent":"ten"}}`)+"\n")
	if err := sc.scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	rep := loadQuota(t, st, store.DefaultInstanceID).Report
	if rep.Status != quota.StatusStale || rep.Reason != quota.ReasonNewerInvalid {
		t.Fatalf("status = %q/%q, want stale/newer_invalid", rep.Status, rep.Reason)
	}
	if rep.SupersededByLine != 2 || rep.Line != 1 {
		t.Fatalf("best line %d superseded by %d, want 1/2", rep.Line, rep.SupersededByLine)
	}
	if got := percentOf(t, rep.Primary); got != 10.0 {
		t.Fatalf("used_percent = %v, want the retained 10", got)
	}
	if rep.ObservedAt == nil || !rep.ObservedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("observed_at = %v, want the retained instant", rep.ObservedAt)
	}
}

// TestScanQuotaSurvivesRestart reads the persisted observation from a fresh
// store handle, without a scan and without touching any log.
func TestScanQuotaSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeRollout(t, root, quotaRollout,
		quotaLine("2026-01-02T03:04:05Z", `{"limit_id":"codex","primary":{"used_percent":7.5},"plan_type":"plus"}`))
	db := filepath.Join(t.TempDir(), "db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	sc := New(st, nil, nil, codex.NewWithRoots(root))
	if err := sc.scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	before := loadQuota(t, st, store.DefaultInstanceID)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after := loadQuota(t, reopened, store.DefaultInstanceID)
	if percentOf(t, after.Report.Primary) != 7.5 || after.Report.Status != quota.StatusOK {
		t.Fatalf("after restart = %+v", *after.Report)
	}
	if !after.IngestedAt.Equal(before.IngestedAt) {
		t.Fatalf("ingested_at moved: %v → %v", before.IngestedAt, after.IngestedAt)
	}
}

// TestScanQuotaNotPublishedWhenCommitFails proves a failed commit neither
// advances the cursor nor publishes fresh quota, and that the next clean scan
// still delivers the observation (nothing was silently discarded).
func TestScanQuotaNotPublishedWhenCommitFails(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := writeRollout(t, root, quotaRollout,
		usageLineInput("2026-01-02T03:04:05Z", 1000, `{"primary":{"used_percent":20.0},"plan_type":"plus"}`))
	st := openStore(t)
	sc := New(st, nil, nil, codex.NewWithRoots(root))
	if err := sc.scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	before := loadQuota(t, st, store.DefaultInstanceID)
	if percentOf(t, before.Report.Primary) != 20.0 {
		t.Fatalf("baseline = %+v", *before.Report)
	}
	cur, err := st.CursorIn(ctx, model.Codex, store.DefaultInstanceID, path)
	if err != nil {
		t.Fatal(err)
	}
	eventsBefore := countRows(t, st, "SELECT count(*) FROM events")

	appendLine(t, path, usageLineInput("2026-01-02T05:04:05Z", 2000, `{"primary":{"used_percent":80.0},"plan_type":"plus"}`)+"\n")
	if _, err := st.DB().ExecContext(ctx, "PRAGMA query_only=1"); err != nil {
		t.Fatal(err)
	}
	// Sanity-check that the connection really is read-only, so a scan failure
	// is attributable to the commit rather than to something else.
	if _, err := st.DB().ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('probe','1')"); err == nil {
		t.Fatal("query_only did not take effect: the store still accepts writes")
	}
	if err := sc.scan(ctx, false); err == nil {
		t.Fatal("scan succeeded against a read-only database")
	}
	after := loadQuota(t, st, store.DefaultInstanceID)
	if got := percentOf(t, after.Report.Primary); got != 20.0 {
		t.Fatalf("failed commit published fresh quota: used_percent = %v, want 20", got)
	}
	if after.Report.Status != quota.StatusOK || !after.IngestedAt.Equal(before.IngestedAt) {
		t.Fatalf("failed commit changed the stored observation: %+v", *after.Report)
	}
	cur2, err := st.CursorIn(ctx, model.Codex, store.DefaultInstanceID, path)
	if err != nil {
		t.Fatal(err)
	}
	if cur2.Offset != cur.Offset {
		t.Fatalf("cursor advanced on a failed commit: %d → %d", cur.Offset, cur2.Offset)
	}
	if n := countRows(t, st, "SELECT count(*) FROM events"); n != eventsBefore {
		t.Fatalf("events = %d, want %d", n, eventsBefore)
	}

	// The next healthy scan must pick the observation up again.
	if _, err := st.DB().ExecContext(ctx, "PRAGMA query_only=0"); err != nil {
		t.Fatal(err)
	}
	if err := sc.scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	recovered := loadQuota(t, st, store.DefaultInstanceID)
	if got := percentOf(t, recovered.Report.Primary); got != 80.0 {
		t.Fatalf("recovered used_percent = %v, want 80", got)
	}
	if n := countRows(t, st, "SELECT count(*) FROM events"); n != eventsBefore+1 {
		t.Fatalf("events = %d, want %d", n, eventsBefore+1)
	}
}
