package quota

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// observeLine feeds one fixture line to a Collector exactly the way the Codex
// parser does: the record's type path, its timestamp text, its 1-based line,
// and the raw rate_limits value.
func observeLine(t *testing.T, c *Collector, line string, lineNo int) {
	t.Helper()
	var env struct {
		Timestamp string `json:"timestamp"`
		Type      string `json:"type"`
		Payload   struct {
			Type       string          `json:"type"`
			RateLimits json.RawMessage `json:"rate_limits"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("bad fixture line: %v", err)
	}
	record := env.Type
	if env.Payload.Type != "" {
		record += "." + env.Payload.Type
	}
	c.ObserveRateLimits(record, env.Timestamp, lineNo, env.Payload.RateLimits)
}

// collected reads lines through a Collector, re-encoding and reloading its
// state at split (0 disables the split), so a test can prove the selection
// survives an incremental batch boundary.
func collected(t *testing.T, lines []string, split int, opts CodexOptions) *Report {
	t.Helper()
	c := NewCollector()
	for i, line := range lines {
		if split > 0 && i == split {
			c = LoadCollector(c.EncodeState())
		}
		observeLine(t, c, line, i+1)
	}
	return c.Report(opts)
}

// TestCollectorMatchesReadCodex proves the incremental path and the offline
// reader agree record for record, including across a state reload.
func TestCollectorMatchesReadCodex(t *testing.T) {
	lines := []string{
		tokenCount("2026-01-02T03:04:05Z", `{"primary":{"used_percent":10.0},"plan_type":"plus"}`),
		`{"timestamp":"2026-01-02T03:05:05Z","type":"response_item","payload":{"type":"function_call","name":"shell"}}`,
		tokenCountSlim("2026-01-02T03:06:05Z", `{"limit_id":"codex","primary":{"used_percent":30.0,"window_minutes":300,"resets_in_seconds":600}}`),
		tokenCount("2026-01-02T03:07:05Z", `{"primary":{"used_percent":15.0},"plan_type":"pro"}`),
		tokenCount("2026-01-02T03:10:05Z", `{"limit_id":"codex","limit_name":null,"primary":{"used_percent":25.0,"window_minutes":300,"resets_in_seconds":7200},"secondary":{"used_percent":3.0,"window_minutes":10080,"resets_at":1767350400},"credits":{"has_credits":true,"unlimited":false,"balance":"9.99"},"plan_type":"plus"}`),
	}
	opts := CodexOptions{Source: "rollout.jsonl"}
	want := readCodex(t, jsonl(lines...), opts)
	want.LinesRead, want.BytesRead, want.Truncated = 0, 0, false

	for _, split := range []int{0, 1, 3, 5} {
		got := collected(t, lines, split, opts)
		if got == nil {
			t.Fatalf("split %d: no report", split)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("split %d:\n got %+v\nwant %+v", split, *got, *want)
		}
	}
}

// TestCollectorWithoutRateLimitsStaysEmpty keeps a cursor free of quota
// bookkeeping for a rollout that never mentions quota.
func TestCollectorWithoutRateLimitsStaysEmpty(t *testing.T) {
	c := NewCollector()
	observeLine(t, c, tokenCount("2026-01-02T03:04:05Z", ""), 1)
	observeLine(t, c, tokenCount("2026-01-02T03:05:05Z", "null"), 2)
	if got := c.EncodeState(); got != "" {
		t.Fatalf("state = %q, want empty", got)
	}
	if rep := c.Report(CodexOptions{Source: "x"}); rep != nil {
		t.Fatalf("report = %+v, want nil", *rep)
	}
}

// TestCollectorKeepsQuotaOnlyRecord is the ingestion case this work exists for:
// a record that carries rate_limits but no usage at all still contributes an
// observation.
func TestCollectorKeepsQuotaOnlyRecord(t *testing.T) {
	lines := []string{
		tokenCountSlim("2026-01-02T03:04:05Z", `{"limit_id":"codex","primary":{"used_percent":42.5,"window_minutes":300},"plan_type":"plus"}`),
	}
	got := collected(t, lines, 0, CodexOptions{Source: "rollout.jsonl"})
	if got == nil || got.Status != StatusOK {
		t.Fatalf("report = %+v, want ok", got)
	}
	if got.Primary == nil || got.Primary.UsedPercent == nil || *got.Primary.UsedPercent != 42.5 {
		t.Fatalf("primary = %+v, want used_percent 42.5", got.Primary)
	}
	if got.Primary.RemainingPercent == nil || *got.Primary.RemainingPercent != 57.5 || !got.Primary.RemainingDerived {
		t.Fatalf("remaining = %+v, want derived 57.5", got.Primary)
	}
	if got.LimitID != "codex" || got.PlanType == nil || *got.PlanType != "plus" {
		t.Fatalf("provenance = %q/%v", got.LimitID, got.PlanType)
	}
	if got.RecordType != "event_msg.token_count" {
		t.Fatalf("record type = %q", got.RecordType)
	}
	if got.ObservedAt == nil || !got.ObservedAt.Equal(at(t, "2026-01-02T03:04:05Z")) {
		t.Fatalf("observed at = %v", got.ObservedAt)
	}
}

// TestCollectorNewerInvalidAcrossBatches keeps an older usable observation from
// looking fresh after a later, unusable one arrives in a later batch.
func TestCollectorNewerInvalidAcrossBatches(t *testing.T) {
	first := []string{tokenCount("2026-01-02T03:04:05Z", `{"primary":{"used_percent":10.0},"plan_type":"plus"}`)}
	c := NewCollector()
	observeLine(t, c, first[0], 1)
	if rep := c.Report(CodexOptions{Source: "x"}); rep.Status != StatusOK {
		t.Fatalf("first batch status = %q/%q, want ok", rep.Status, rep.Reason)
	}
	c = LoadCollector(c.EncodeState())

	observeLine(t, c, tokenCount("2026-01-02T04:04:05Z", `{"primary":{"used_percent":"ten"}}`), 2)
	rep := c.Report(CodexOptions{Source: "x"})
	if rep.Status != StatusStale || rep.Reason != ReasonNewerInvalid {
		t.Fatalf("status = %q/%q, want stale/newer_invalid", rep.Status, rep.Reason)
	}
	if rep.SupersededByLine != 2 || rep.Line != 1 {
		t.Fatalf("lines = best %d superseded %d, want 1/2", rep.Line, rep.SupersededByLine)
	}
	if rep.Primary == nil || rep.Primary.UsedPercent == nil || *rep.Primary.UsedPercent != 10.0 {
		t.Fatalf("primary = %+v, want the retained 10%% window", rep.Primary)
	}
	if rep.ObservedAt == nil || !rep.ObservedAt.Equal(at(t, "2026-01-02T03:04:05Z")) {
		t.Fatalf("observed at = %v, want the retained instant", rep.ObservedAt)
	}
}

// TestCollectorStateStaysBounded proves a hostile or long log cannot grow the
// state a cursor carries.
func TestCollectorStateStaysBounded(t *testing.T) {
	c := NewCollector()
	for i := 0; i < 500; i++ {
		rl := `{"limit_id":"id-` + strings.Repeat("x", 40) + `-` + string(rune('a'+i%26)) + `","primary":{"used_percent":1.0,"unknown_` + strings.Repeat("y", 60) + `":1},"plan_type":"plus"}`
		observeLine(t, c, tokenCount("2026-01-02T03:04:05Z", rl), i+1)
	}
	state := c.EncodeState()
	if state == "" {
		t.Fatal("state is empty, want a bounded state")
	}
	if len(state) > maxCollectorStateBytes {
		t.Fatalf("state = %d bytes, want <= %d", len(state), maxCollectorStateBytes)
	}
	reloaded := LoadCollector(state)
	rep := reloaded.Report(CodexOptions{Source: "x"})
	if rep == nil || rep.Status != StatusOK {
		t.Fatalf("report = %+v, want ok", rep)
	}
	if len(rep.OtherLimitIDs) > maxOtherLimitIDs {
		t.Fatalf("other limit ids = %d, want <= %d", len(rep.OtherLimitIDs), maxOtherLimitIDs)
	}
}

func TestCollectorDropsUnreadableState(t *testing.T) {
	c := LoadCollector("{not json")
	if c.EncodeState() != "" {
		t.Fatal("unreadable state was kept")
	}
	if rep := c.Report(CodexOptions{Source: "x"}); rep != nil {
		t.Fatalf("report = %+v, want nil", *rep)
	}
}

func TestSettingKeyIsStable(t *testing.T) {
	if SettingPrefix != "quota.codex." {
		t.Fatalf("prefix = %q", SettingPrefix)
	}
	if got := SettingKey(""); got != "quota.codex.default" {
		t.Fatalf("SettingKey(\"\") = %q", got)
	}
	if got := SettingKey("alpha"); got != "quota.codex.alpha" {
		t.Fatalf("SettingKey(alpha) = %q", got)
	}
}

type memSettings map[string]string

func (m memSettings) Setting(_ context.Context, key string) (string, error) { return m[key], nil }
func (m memSettings) SetSetting(_ context.Context, key, value string) error {
	m[key] = value
	return nil
}

func TestLoadLatestRejectsCorruptValueWithoutEchoingIt(t *testing.T) {
	m := memSettings{SettingKey("alpha"): `{"harness":"codex","instance":"alpha","report":{"source":"SYNTHETIC-SECRET-4a71"`}
	latest, ok, err := LoadLatest(context.Background(), m, "alpha")
	if ok || latest != nil || err != ErrCorruptStored {
		t.Fatalf("LoadLatest = %v/%v/%v, want false nil ErrCorruptStored", latest, ok, err)
	}
	if strings.Contains(err.Error(), "SYNTHETIC-SECRET-4a71") {
		t.Fatalf("error echoed stored content: %v", err)
	}
}

func TestLoadLatestMissingKeyIsNotAnError(t *testing.T) {
	latest, ok, err := LoadLatest(context.Background(), memSettings{}, "alpha")
	if err != nil || ok || latest != nil {
		t.Fatalf("LoadLatest = %v/%v/%v, want nil/false/nil", latest, ok, err)
	}
}

// TestPublishLatestNeverRefreshesAnOldQuota proves the stored value keeps the
// stale status a newer invalid record produced, and that republishing it is
// idempotent.
func TestPublishLatestNeverRefreshesAnOldQuota(t *testing.T) {
	ctx := context.Background()
	m := memSettings{}
	valid := &Report{
		Source:     "rollout.jsonl",
		ObservedAt: ptrTime(at(t, "2026-01-02T03:04:05Z")),
		Line:       1,
		Primary:    &Window{Kind: WindowPrimary, Status: StatusOK, UsedPercent: ptrFloat(10)},
		Status:     StatusOK,
		Reason:     ReasonNone,
	}
	if err := PublishLatest(ctx, m, "alpha", valid, at(t, "2026-01-02T05:00:00Z")); err != nil {
		t.Fatal(err)
	}
	latest, ok, err := LoadLatest(ctx, m, "alpha")
	if err != nil || !ok {
		t.Fatalf("LoadLatest = %v/%v", ok, err)
	}
	if latest.Report.Status != StatusOK || latest.Harness != HarnessCodex || latest.Instance != "alpha" {
		t.Fatalf("latest = %+v", *latest)
	}
	if latest.IngestedAt.IsZero() || !latest.IngestedAt.Equal(at(t, "2026-01-02T05:00:00Z")) {
		t.Fatalf("ingested at = %v", latest.IngestedAt)
	}

	invalid := &Report{
		Source:     "rollout.jsonl",
		ObservedAt: ptrTime(at(t, "2026-01-02T04:04:05Z")),
		Line:       2,
		Primary:    &Window{Kind: WindowPrimary, Status: StatusUnknown, Reason: ReasonMissing},
		Status:     StatusUnavailable,
		Reason:     ReasonMissing,
	}
	if err := PublishLatest(ctx, m, "alpha", invalid, at(t, "2026-01-02T05:01:00Z")); err != nil {
		t.Fatal(err)
	}
	latest, _, _ = LoadLatest(ctx, m, "alpha")
	if latest.Report.Status != StatusStale || latest.Report.Reason != ReasonNewerInvalid {
		t.Fatalf("after invalid newer: %q/%q, want stale/newer_invalid", latest.Report.Status, latest.Report.Reason)
	}
	if latest.Report.SupersededByLine != 2 || latest.Report.Primary == nil || *latest.Report.Primary.UsedPercent != 10 {
		t.Fatalf("retained = %+v", *latest.Report)
	}
	// Republishing the same invalid observation must not upgrade it.
	if err := PublishLatest(ctx, m, "alpha", invalid, at(t, "2026-01-02T05:02:00Z")); err != nil {
		t.Fatal(err)
	}
	latest, _, _ = LoadLatest(ctx, m, "alpha")
	if latest.Report.Status != StatusStale || latest.Report.Reason != ReasonNewerInvalid {
		t.Fatalf("after republish: %q/%q, want stale/newer_invalid", latest.Report.Status, latest.Report.Reason)
	}
}

func TestPublishLatestRejectsOversizedValue(t *testing.T) {
	m := memSettings{}
	rep := &Report{Source: strings.Repeat("s", maxLatestBytes*2), Status: StatusOK}
	if err := PublishLatest(context.Background(), m, "alpha", rep, time.Now()); err == nil {
		t.Fatal("oversized observation was stored")
	}
	if len(m) != 0 {
		t.Fatalf("stored %d keys, want none", len(m))
	}
}

func TestMergeCurrency(t *testing.T) {
	ok10 := func(ts string) *Report {
		return &Report{
			ObservedAt: ptrTime(at(t, ts)),
			Line:       1,
			Primary:    &Window{Kind: WindowPrimary, Status: StatusOK, UsedPercent: ptrFloat(10)},
			Status:     StatusOK,
			Reason:     ReasonNone,
		}
	}
	usable := ok10("2026-01-02T03:04:05Z")
	invalidNewer := &Report{
		ObservedAt: ptrTime(at(t, "2026-01-02T04:04:05Z")),
		Line:       2,
		Primary:    &Window{Kind: WindowPrimary, Status: StatusUnknown, Reason: ReasonMissing},
		Status:     StatusUnavailable,
		Reason:     ReasonMissing,
	}
	planOnlyNewer := &Report{
		ObservedAt: ptrTime(at(t, "2026-01-02T04:04:05Z")),
		Line:       2,
		PlanType:   ptrString("plus"),
		Status:     StatusUnavailable,
		Reason:     ReasonMissing,
	}
	stale := *usable
	stale.Status, stale.Reason, stale.SupersededByLine = StatusStale, ReasonNewerInvalid, 9

	if got := Merge(nil, usable); got != usable {
		t.Fatalf("nil current: got %+v", *got)
	}
	if got := Merge(usable, nil); got != usable {
		t.Fatalf("nil next: got %+v", *got)
	}
	if got := Merge(usable, invalidNewer); got.Status != StatusStale || got.Reason != ReasonNewerInvalid || got.SupersededByLine != 2 {
		t.Fatalf("invalid newer: %q/%q line %d", got.Status, got.Reason, got.SupersededByLine)
	}
	if got := Merge(invalidNewer, usable); got.Status != StatusStale || got.SupersededByLine != 2 {
		t.Fatalf("invalid newer first: %q/%q line %d", got.Status, got.Reason, got.SupersededByLine)
	}
	if got := Merge(usable, planOnlyNewer); got != usable {
		t.Fatalf("plan-only newer changed the observation: %+v", *got)
	}
	// A stale observation must never come back fresh.
	if got := Merge(&stale, planOnlyNewer); got != &stale || got.Status != StatusStale {
		t.Fatalf("stale was refreshed: %+v", *got)
	}
	newer := ok10("2026-01-02T05:04:05Z")
	if got := Merge(usable, newer); got != newer {
		t.Fatalf("newer usable did not win: %+v", *got)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
func ptrFloat(f float64) *float64    { return &f }
func ptrString(s string) *string     { return &s }
