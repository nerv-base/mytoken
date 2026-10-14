package scan

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness/codex"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/quota"
	"github.com/zzstar101/mytoken/internal/store"
)

// tailOnlyScanner settles every cursor with one full scan, clears the stored
// Codex observations, and returns a fresh scanner. A scan with a fresh scanner
// over settled cursors writes nothing, so the normal publish path is silent and
// anything that ends up stored came from the bounded tail read.
func tailOnlyScanner(t *testing.T, st *store.Store, ids []string, mk func() *Scanner) *Scanner {
	t.Helper()
	ctx := context.Background()
	if err := mk().scan(ctx, false); err != nil {
		t.Fatalf("settling scan: %v", err)
	}
	for _, id := range ids {
		if err := st.SetSetting(ctx, quota.SettingKey(id), ""); err != nil {
			t.Fatalf("clear quota for %q: %v", id, err)
		}
		if _, ok, err := quota.LoadLatest(ctx, st, id); err != nil || ok {
			t.Fatalf("quota for %q after clearing = (found %v, err %v), want none", id, ok, err)
		}
	}
	return mk()
}

// TestQuotaTailPublishesWhenCursorAlreadyAtEOF is the case the user hit: the
// rollout is fully indexed, its cursor is at EOF, so no parse pass ever
// publishes the quota its last lines carry. The tail read must publish it
// without moving the cursor.
func TestQuotaTailPublishesWhenCursorAlreadyAtEOF(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := writeRollout(t, root, quotaRollout,
		usageLineInput("2026-01-02T03:04:05Z", 1000,
			`{"limit_id":"codex","primary":{"used_percent":11.5,"window_minutes":10080,"resets_in_seconds":600},"plan_type":"plus"}`),
		usageLineInput("2026-01-02T03:05:05Z", 2000, ""))
	st := openStore(t)
	sc := tailOnlyScanner(t, st, []string{"default"}, func() *Scanner {
		return New(st, nil, nil, codex.NewWithRoots(root))
	})

	before, err := st.CursorIn(ctx, model.Codex, "default", path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Offset == 0 || before.Offset != before.Size {
		t.Fatalf("cursor = %+v, want the settled EOF position", before)
	}

	if err := sc.scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	after, err := st.CursorIn(ctx, model.Codex, "default", path)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("the tail read moved the cursor: %+v -> %+v", before, after)
	}

	latest := loadQuota(t, st, "default")
	if !latest.Report.Available() {
		t.Fatalf("stored report = %s/%s, want an available reading", latest.Report.Status, latest.Report.Reason)
	}
	if got := percentOf(t, latest.Report.Primary); got != 11.5 {
		t.Fatalf("primary used = %v, want 11.5", got)
	}
	out := sc.QuotaTailOutcomes()
	if len(out) != 1 || !out[0].Published || out[0].Instance != "default" || out[0].Path != path {
		t.Fatalf("tail outcomes = %+v, want one published outcome for %s", out, path)
	}
}

// TestQuotaTailKeepsOlderUsableWhenNewerIsUnusable covers the merge rule: a
// newer record that states no usable window must not replace the older usable
// one. The reading stays available and is marked stale, naming the line that
// superseded it.
func TestQuotaTailKeepsOlderUsableWhenNewerIsUnusable(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeRollout(t, root, quotaRollout,
		quotaLine("2026-01-02T03:04:05Z",
			`{"limit_id":"codex","primary":{"used_percent":30.0,"window_minutes":10080,"resets_in_seconds":600},"plan_type":"plus"}`),
		quotaLine("2026-01-02T03:06:05Z",
			`{"limit_id":"codex","primary":{"used_percent":null,"window_minutes":10080,"resets_in_seconds":600},"plan_type":"plus"}`))
	st := openStore(t)
	sc := tailOnlyScanner(t, st, []string{"default"}, func() *Scanner {
		return New(st, nil, nil, codex.NewWithRoots(root))
	})
	if err := sc.scan(ctx, false); err != nil {
		t.Fatal(err)
	}

	latest := loadQuota(t, st, "default")
	if latest.Report.Status != quota.StatusStale || latest.Report.Reason != quota.ReasonNewerInvalid {
		t.Fatalf("stored report = %s/%s, want %s/%s",
			latest.Report.Status, latest.Report.Reason, quota.StatusStale, quota.ReasonNewerInvalid)
	}
	if latest.Report.SupersededByLine != 2 {
		t.Fatalf("superseded by line %d, want 2", latest.Report.SupersededByLine)
	}
	if got := percentOf(t, latest.Report.Primary); got != 30.0 {
		t.Fatalf("primary used = %v, want the older usable 30.0", got)
	}
}

// TestQuotaTailIsBoundedWithoutAUsableRecord covers a log whose records never
// state a usable window: nothing may be published, the read must stay inside
// its byte bounds, and the outcome must say why.
func TestQuotaTailIsBoundedWithoutAUsableRecord(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	lines := make([]string, 0, 4000)
	for i := 0; i < 4000; i++ {
		lines = append(lines, quotaLine(
			fmt.Sprintf("2026-01-02T%02d:%02d:%02dZ", i/3600, i/60%60, i%60),
			`{"limit_id":"codex","primary":{"used_percent":null,"window_minutes":10080,"resets_in_seconds":600},"plan_type":"plus"}`))
	}
	writeRollout(t, root, quotaRollout, lines...)
	st := openStore(t)
	sc := tailOnlyScanner(t, st, []string{"default"}, func() *Scanner {
		return New(st, nil, nil, codex.NewWithRoots(root))
	})
	if err := sc.scan(ctx, false); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := quota.LoadLatest(ctx, st, "default"); err != nil || ok {
		t.Fatalf("no usable record must publish nothing, got found %v err %v", ok, err)
	}
	out := sc.QuotaTailOutcomes()
	if len(out) != 1 {
		t.Fatalf("tail outcomes = %+v, want one entry", out)
	}
	got := out[0]
	if got.Published {
		t.Fatalf("published a reading with no usable window: %+v", got)
	}
	if got.Reason == "" {
		t.Fatalf("nothing published must carry a reason: %+v", got)
	}
	if got.Files != 1 {
		t.Fatalf("files read = %d, want 1", got.Files)
	}
	if got.Bytes > quotaTailMaxInstanceBytes {
		t.Fatalf("read %d bytes, over the %d byte per-instance budget", got.Bytes, quotaTailMaxInstanceBytes)
	}
	if got.Bytes <= int64(quotaTailFirstWindow) {
		t.Fatalf("read %d bytes, want the window to grow past the first %d", got.Bytes, quotaTailFirstWindow)
	}
}

// TestQuotaTailReachesAnOlderUsableRecordBehindUnusableOnes covers the real log
// shape that defeated the first bound: the newest rollouts of an idle account
// record only all-null rate_limits, and the newest usable observation sits
// several files back. The tail read must keep walking the newest-first list
// until it finds one, instead of stopping at the first few files.
func TestQuotaTailReachesAnOlderUsableRecordBehindUnusableOnes(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	base := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	// write stamps one rollout with an explicit age, so "newest first" is
	// deterministic on filesystems with a coarse modification clock.
	write := func(name string, age time.Duration, lines ...string) string {
		t.Helper()
		p := writeRollout(t, root, name, lines...)
		when := base.Add(-age)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Ten newer rollouts state no usable window at all.
	for i := 0; i < 10; i++ {
		write(fmt.Sprintf("rollout-2026-01-10T12-00-%02d-0199aaaa-bbbb-cccc-dddd-eeeeffff%04d.jsonl", i, i),
			time.Duration(i)*time.Minute,
			quotaLine(fmt.Sprintf("2026-01-10T11:%02d:00Z", i),
				`{"limit_id":"codex","primary":{"used_percent":null,"window_minutes":10080,"resets_in_seconds":600},"plan_type":"plus"}`))
	}
	// The usable observation is the eleventh file, far behind quotaTailMaxFiles
	// of any earlier bound.
	usable := write("rollout-2026-01-10T11-00-00-0199aaaa-bbbb-cccc-dddd-eeeeffff9999.jsonl",
		10*time.Minute,
		quotaLine("2026-01-10T10:00:00Z",
			`{"limit_id":"codex","primary":{"used_percent":7.5,"window_minutes":10080,"resets_in_seconds":600},"plan_type":"plus"}`))

	st := openStore(t)
	sc := tailOnlyScanner(t, st, []string{"default"}, func() *Scanner {
		return New(st, nil, nil, codex.NewWithRoots(root))
	})
	if err := sc.scan(ctx, false); err != nil {
		t.Fatal(err)
	}

	latest := loadQuota(t, st, "default")
	if !latest.Report.Available() {
		t.Fatalf("stored report = %s/%s, want the usable reading eleven files back",
			latest.Report.Status, latest.Report.Reason)
	}
	if got := percentOf(t, latest.Report.Primary); got != 7.5 {
		t.Fatalf("primary used = %v, want 7.5", got)
	}
	out := sc.QuotaTailOutcomes()
	if len(out) != 1 || !out[0].Published || out[0].Path != usable {
		t.Fatalf("tail outcomes = %+v, want one published outcome for %s", out, usable)
	}
	if out[0].Files < 11 {
		t.Fatalf("files read = %d, want the walk to reach the eleventh file", out[0].Files)
	}
	if out[0].Bytes > quotaTailMaxInstanceBytes {
		t.Fatalf("read %d bytes, over the %d byte per-instance budget", out[0].Bytes, quotaTailMaxInstanceBytes)
	}
}

// TestQuotaTailPublishesPerInstance covers isolation: two Codex instances keep
// their own keys and neither leaks into the default one.
func TestQuotaTailPublishesPerInstance(t *testing.T) {
	ctx := context.Background()
	rootA, rootB := t.TempDir(), t.TempDir()
	writeRollout(t, rootA, quotaRollout, quotaLine("2026-01-02T03:04:05Z",
		`{"limit_id":"codex","primary":{"used_percent":11.0,"window_minutes":10080,"resets_in_seconds":600},"plan_type":"plus"}`))
	writeRollout(t, rootB, quotaRollout, quotaLine("2026-01-02T03:04:05Z",
		`{"limit_id":"codex","primary":{"used_percent":22.0,"window_minutes":10080,"resets_in_seconds":600},"plan_type":"plus"}`))
	st := openStore(t)
	mk := func() *Scanner {
		sc, err := NewWithInstances(st, nil, nil,
			Instance{ID: "alpha", Parser: codex.NewWithRoots(rootA)},
			Instance{ID: "beta", Parser: codex.NewWithRoots(rootB)})
		if err != nil {
			t.Fatal(err)
		}
		return sc
	}
	sc := tailOnlyScanner(t, st, []string{"alpha", "beta"}, mk)
	if err := sc.scan(ctx, false); err != nil {
		t.Fatal(err)
	}

	if got := percentOf(t, loadQuota(t, st, "alpha").Report.Primary); got != 11.0 {
		t.Fatalf("alpha primary used = %v, want 11.0", got)
	}
	if got := percentOf(t, loadQuota(t, st, "beta").Report.Primary); got != 22.0 {
		t.Fatalf("beta primary used = %v, want 22.0", got)
	}
	if _, ok, err := quota.LoadLatest(ctx, st, "default"); err != nil || ok {
		t.Fatalf("the default key must stay empty, got found %v err %v", ok, err)
	}
	out := sc.QuotaTailOutcomes()
	if len(out) != 2 {
		t.Fatalf("tail outcomes = %+v, want one per instance", out)
	}
	if out[0].Instance != "alpha" || out[1].Instance != "beta" || !out[0].Published || !out[1].Published {
		t.Fatalf("tail outcomes = %+v, want alpha then beta, both published", out)
	}
}
