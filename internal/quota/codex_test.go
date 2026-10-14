package quota

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// codexInfo mirrors the info object a real Codex rollout attaches to a
// token_count event. Every fixture in this package is synthetic.
const codexInfo = `{"total_token_usage":{"input_tokens":1000,"cached_input_tokens":400,"cache_write_input_tokens":0,"output_tokens":50,"reasoning_output_tokens":20,"total_tokens":1050},"last_token_usage":{"input_tokens":1000,"cached_input_tokens":400,"cache_write_input_tokens":0,"output_tokens":50,"reasoning_output_tokens":20,"total_tokens":1050},"model_context_window":258400}`

// tokenCount builds an event_msg/token_count rollout line. An empty rateLimits
// omits the field; an empty ts omits the timestamp.
func tokenCount(ts, rateLimits string) string {
	p := `{"type":"token_count","info":` + codexInfo
	if rateLimits != "" {
		p += `,"rate_limits":` + rateLimits
	}
	p += `}`
	if ts == "" {
		return `{"type":"event_msg","payload":` + p + `}`
	}
	return `{"timestamp":"` + ts + `","type":"event_msg","payload":` + p + `}`
}

func jsonl(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

// tokenCountSlim is tokenCount without the bulky info object, for tests that
// need a record comfortably under a small MaxLineBytes.
func tokenCountSlim(ts, rateLimits string) string {
	p := `{"type":"token_count"`
	if rateLimits != "" {
		p += `,"rate_limits":` + rateLimits
	}
	p += `}`
	if ts == "" {
		return `{"type":"event_msg","payload":` + p + `}`
	}
	return `{"timestamp":"` + ts + `","type":"event_msg","payload":` + p + `}`
}

func readCodex(t *testing.T, input string, opts CodexOptions) *Report {
	t.Helper()
	rep, err := ReadCodex(context.Background(), strings.NewReader(input), opts)
	if err != nil {
		t.Fatalf("ReadCodex: %v", err)
	}
	if rep == nil {
		t.Fatal("ReadCodex returned a nil report")
	}
	return rep
}

func issueFor(rep *Report, field string) *Issue {
	for i := range rep.Issues {
		if rep.Issues[i].Field == field {
			return &rep.Issues[i]
		}
	}
	return nil
}

func at(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad test timestamp %q: %v", s, err)
	}
	return ts.UTC()
}

func TestReadCodexChoosesLatestTrustworthyObservation(t *testing.T) {
	in := jsonl(
		tokenCount("2026-01-02T03:04:05Z", `{"primary":{"used_percent":10.0},"plan_type":"plus"}`),
		tokenCount("2026-01-02T03:10:05Z", `{"limit_id":"codex","limit_name":null,"primary":{"used_percent":25.0,"window_minutes":300,"resets_in_seconds":7200},"secondary":{"used_percent":3.0,"window_minutes":10080,"resets_at":1767350400},"credits":{"has_credits":true,"unlimited":false,"balance":"9.99"},"plan_type":"plus"}`),
		tokenCount("2026-01-02T03:07:05Z", `{"primary":{"used_percent":15.0},"plan_type":"pro"}`),
	)
	rep := readCodex(t, in, CodexOptions{Source: "rollout.jsonl"})

	if rep.Status != StatusOK || rep.Reason != ReasonNone {
		t.Fatalf("status = %q/%q, want ok", rep.Status, rep.Reason)
	}
	if !rep.Available() {
		t.Fatal("Available() = false, want true")
	}
	if rep.Source != "rollout.jsonl" {
		t.Errorf("Source = %q, want rollout.jsonl", rep.Source)
	}
	if rep.Line != 2 {
		t.Errorf("Line = %d, want 2", rep.Line)
	}
	if rep.RecordType != "event_msg.token_count" {
		t.Errorf("RecordType = %q", rep.RecordType)
	}
	if rep.ObservedAt == nil || !rep.ObservedAt.Equal(at(t, "2026-01-02T03:10:05Z")) {
		t.Errorf("ObservedAt = %v, want 2026-01-02T03:10:05Z", rep.ObservedAt)
	}
	if rep.LimitID != "codex" || rep.LimitName != "" {
		t.Errorf("limit id/name = %q/%q", rep.LimitID, rep.LimitName)
	}
	if rep.PlanType == nil || *rep.PlanType != "plus" {
		t.Errorf("PlanType = %v, want plus", rep.PlanType)
	}
	if rep.Primary == nil || rep.Primary.Status != StatusOK || rep.Primary.Kind != WindowPrimary {
		t.Fatalf("Primary = %+v", rep.Primary)
	}
	if rep.Primary.UsedPercent == nil || *rep.Primary.UsedPercent != 25 {
		t.Errorf("Primary.UsedPercent = %v", rep.Primary.UsedPercent)
	}
	if rep.Primary.RemainingPercent == nil || *rep.Primary.RemainingPercent != 75 || !rep.Primary.RemainingDerived {
		t.Errorf("Primary.RemainingPercent = %v derived=%v", rep.Primary.RemainingPercent, rep.Primary.RemainingDerived)
	}
	if rep.Primary.WindowMinutes == nil || *rep.Primary.WindowMinutes != 300 {
		t.Errorf("Primary.WindowMinutes = %v", rep.Primary.WindowMinutes)
	}
	if rep.Primary.ResetsAt == nil || !rep.Primary.ResetsAt.Equal(at(t, "2026-01-02T05:10:05Z")) {
		t.Errorf("Primary.ResetsAt = %v, want 2026-01-02T05:10:05Z", rep.Primary.ResetsAt)
	}
	if rep.Secondary == nil || rep.Secondary.ResetsAt == nil || !rep.Secondary.ResetsAt.Equal(time.Unix(1767350400, 0).UTC()) {
		t.Errorf("Secondary.ResetsAt = %v", rep.Secondary)
	}
	if rep.Credits == nil || rep.Credits.Balance == nil || *rep.Credits.Balance != "9.99" {
		t.Errorf("Credits = %+v", rep.Credits)
	}
	if rep.Credits.HasCredits == nil || !*rep.Credits.HasCredits {
		t.Errorf("Credits.HasCredits = %v", rep.Credits.HasCredits)
	}
	if len(rep.Issues) != 0 {
		t.Errorf("Issues = %+v, want none", rep.Issues)
	}
	if rep.LinesRead != 3 || rep.Truncated {
		t.Errorf("LinesRead/Truncated = %d/%v", rep.LinesRead, rep.Truncated)
	}
}

func TestReadCodexTieBreaksOnLaterLine(t *testing.T) {
	ts := "2026-01-02T03:04:05Z"
	in := jsonl(
		tokenCount(ts, `{"primary":{"used_percent":10}}`),
		tokenCount(ts, `{"primary":{"used_percent":20}}`),
	)
	rep := readCodex(t, in, CodexOptions{})
	if rep.Line != 2 {
		t.Errorf("Line = %d, want 2", rep.Line)
	}
	if rep.Primary == nil || rep.Primary.UsedPercent == nil || *rep.Primary.UsedPercent != 20 {
		t.Errorf("Primary = %+v", rep.Primary)
	}
}

func TestReadCodexPrefersTimestampedObservation(t *testing.T) {
	in := jsonl(
		tokenCount("2026-01-02T03:04:05Z", `{"primary":{"used_percent":10}}`),
		tokenCount("", `{"primary":{"used_percent":99}}`),
	)
	rep := readCodex(t, in, CodexOptions{})
	if rep.Line != 1 {
		t.Errorf("Line = %d, want 1 (a trustworthy timestamp outranks a later untimestamped record)", rep.Line)
	}
	if rep.Status != StatusOK {
		t.Errorf("Status = %q, want ok", rep.Status)
	}
	if rep.Primary == nil || *rep.Primary.UsedPercent != 10 {
		t.Errorf("Primary = %+v", rep.Primary)
	}
}

func TestReadCodexExplicitZeroIsNotMissing(t *testing.T) {
	in := jsonl(tokenCount("2026-01-02T03:04:05Z",
		`{"primary":{"used_percent":0,"window_minutes":300},"secondary":{"window_minutes":60},"credits":{"has_credits":false,"unlimited":false,"balance":"0"},"plan_type":""}`))
	rep := readCodex(t, in, CodexOptions{})

	if rep.Primary == nil || rep.Primary.UsedPercent == nil || *rep.Primary.UsedPercent != 0 {
		t.Fatalf("Primary.UsedPercent = %v, want an explicit 0", rep.Primary)
	}
	if rep.Primary.RemainingPercent == nil || *rep.Primary.RemainingPercent != 100 || !rep.Primary.RemainingDerived {
		t.Errorf("Primary.RemainingPercent = %v derived=%v", rep.Primary.RemainingPercent, rep.Primary.RemainingDerived)
	}
	if rep.Secondary == nil {
		t.Fatal("Secondary = nil, want the window object")
	}
	if rep.Secondary.UsedPercent != nil {
		t.Errorf("Secondary.UsedPercent = %v, want nil (missing, not 0)", *rep.Secondary.UsedPercent)
	}
	if rep.Secondary.Status != StatusUnknown || rep.Secondary.Reason != ReasonMissing {
		t.Errorf("Secondary status/reason = %q/%q", rep.Secondary.Status, rep.Secondary.Reason)
	}
	if rep.Secondary.WindowMinutes == nil || *rep.Secondary.WindowMinutes != 60 {
		t.Errorf("Secondary.WindowMinutes = %v", rep.Secondary.WindowMinutes)
	}
	if rep.Credits == nil || rep.Credits.HasCredits == nil || *rep.Credits.HasCredits {
		t.Errorf("Credits.HasCredits = %+v, want an explicit false", rep.Credits)
	}
	if rep.Credits.Unlimited == nil || *rep.Credits.Unlimited {
		t.Errorf("Credits.Unlimited = %+v, want an explicit false", rep.Credits)
	}
	if rep.Credits.Balance == nil || *rep.Credits.Balance != "0" {
		t.Errorf("Credits.Balance = %v, want an explicit \"0\"", rep.Credits.Balance)
	}
	if rep.PlanType != nil {
		t.Errorf("PlanType = %q, want nil for an empty plan string", *rep.PlanType)
	}
	if rep.Status != StatusOK {
		t.Errorf("Status = %q, want ok", rep.Status)
	}
}

func TestReadCodexUnsupportedShapesAreExplicit(t *testing.T) {
	in := jsonl(tokenCount("2026-01-02T03:04:05Z",
		`{"primary":"nope","secondary":{"used_percent":"12.5","window_minutes":300.5,"resets_at":1767350400000},"credits":"x","plan_type":7}`))
	rep := readCodex(t, in, CodexOptions{})

	if rep.Status != StatusUnavailable || rep.Reason != ReasonMalformed {
		t.Fatalf("status = %q/%q, want unavailable/malformed", rep.Status, rep.Reason)
	}
	if rep.Primary == nil || rep.Primary.Status != StatusMalformed || rep.Primary.Reason != ReasonMalformed {
		t.Errorf("Primary = %+v, want a malformed window", rep.Primary)
	}
	if rep.Secondary == nil {
		t.Fatal("Secondary = nil, want the window object")
	}
	if rep.Secondary.UsedPercent != nil || rep.Secondary.WindowMinutes != nil || rep.Secondary.ResetsAt != nil {
		t.Errorf("Secondary carried values from unsupported shapes: %+v", rep.Secondary)
	}
	if rep.Secondary.Status != StatusUnknown {
		t.Errorf("Secondary.Status = %q, want unknown", rep.Secondary.Status)
	}
	if rep.Credits != nil || rep.PlanType != nil {
		t.Errorf("Credits/PlanType = %+v/%v, want nil", rep.Credits, rep.PlanType)
	}

	want := []struct {
		field  string
		reason Reason
	}{
		{"rate_limits.credits", ReasonUnsupportedType},
		{"rate_limits.plan_type", ReasonUnsupportedType},
		{"rate_limits.primary", ReasonMalformed},
		{"rate_limits.secondary.resets_at", ReasonOutOfRange},
		{"rate_limits.secondary.used_percent", ReasonUnsupportedType},
		{"rate_limits.secondary.window_minutes", ReasonMalformed},
	}
	for _, w := range want {
		iss := issueFor(rep, w.field)
		if iss == nil {
			t.Errorf("no issue for %s", w.field)
			continue
		}
		if iss.Reason != w.reason {
			t.Errorf("%s reason = %q, want %q", w.field, iss.Reason, w.reason)
		}
		if iss.Count != 1 {
			t.Errorf("%s count = %d, want 1", w.field, iss.Count)
		}
	}
	if rep.UnknownFields != 0 {
		t.Errorf("UnknownFields = %d, want 0", rep.UnknownFields)
	}
}

func TestReadCodexToleratesUnknownFields(t *testing.T) {
	in := jsonl(tokenCount("2026-01-02T03:04:05Z",
		`{"primary":{"used_percent":5,"window_minutes":300,"future_window_field":{"a":1}},"future_top":"x","credits":{"has_credits":true,"future_credit":2}}`))
	rep := readCodex(t, in, CodexOptions{})

	if rep.Status != StatusOK {
		t.Fatalf("status = %q/%q, want ok", rep.Status, rep.Reason)
	}
	if rep.UnknownFields != 3 {
		t.Errorf("UnknownFields = %d, want 3", rep.UnknownFields)
	}
	if len(rep.Issues) != 0 {
		t.Errorf("Issues = %+v, want none for tolerated future fields", rep.Issues)
	}
}

func TestReadCodexSkipsOverlongLines(t *testing.T) {
	pad := strings.Repeat("a", 512)
	long := tokenCount("2026-01-02T03:04:05Z", `{"primary":{"used_percent":1},"pad":"`+pad+`"}`)
	in := jsonl(long, tokenCountSlim("2026-01-02T03:05:05Z", `{"primary":{"used_percent":42}}`))
	rep := readCodex(t, in, CodexOptions{MaxLineBytes: 256})

	if rep.Line != 2 {
		t.Errorf("Line = %d, want 2", rep.Line)
	}
	if rep.Primary == nil || rep.Primary.UsedPercent == nil || *rep.Primary.UsedPercent != 42 {
		t.Errorf("Primary = %+v", rep.Primary)
	}
	iss := issueFor(rep, "line")
	if iss == nil || iss.Reason != ReasonLineTooLong || iss.Count != 1 {
		t.Errorf("line issue = %+v, want one line_too_long", iss)
	}
	if rep.LinesRead != 2 {
		t.Errorf("LinesRead = %d, want 2", rep.LinesRead)
	}
}

func TestReadCodexHonoursByteLimit(t *testing.T) {
	in := jsonl(tokenCount("2026-01-02T03:04:05Z", `{"primary":{"used_percent":10}}`))
	rep := readCodex(t, in, CodexOptions{MaxBytes: 32})

	if !rep.Truncated {
		t.Error("Truncated = false, want true")
	}
	if rep.Status != StatusUnavailable || rep.Reason != ReasonNoData {
		t.Errorf("status = %q/%q, want unavailable/no_data", rep.Status, rep.Reason)
	}
	if rep.Primary != nil {
		t.Errorf("Primary = %+v, want nil", rep.Primary)
	}
}

func TestReadCodexMarksStaleObservation(t *testing.T) {
	in := jsonl(tokenCount("2026-01-02T03:04:05Z", `{"primary":{"used_percent":10}}`))
	rep := readCodex(t, in, CodexOptions{Now: at(t, "2026-01-02T06:00:00Z"), StaleAfter: 2 * time.Hour})

	if rep.Status != StatusStale || rep.Reason != ReasonStale {
		t.Fatalf("status = %q/%q, want stale", rep.Status, rep.Reason)
	}
	if !rep.Available() {
		t.Error("Available() = false, want true for a stale but usable observation")
	}
	if rep.Primary == nil || *rep.Primary.UsedPercent != 10 {
		t.Errorf("Primary = %+v, want the stale values preserved", rep.Primary)
	}
}

func TestReadCodexFreshObservationIsNotStale(t *testing.T) {
	in := jsonl(tokenCount("2026-01-02T03:04:05Z", `{"primary":{"used_percent":10}}`))
	rep := readCodex(t, in, CodexOptions{Now: at(t, "2026-01-02T03:30:00Z"), StaleAfter: 2 * time.Hour})
	if rep.Status != StatusOK || rep.Reason != ReasonNone {
		t.Fatalf("status = %q/%q, want ok", rep.Status, rep.Reason)
	}
}

func TestReadCodexUnknownTimestamp(t *testing.T) {
	in := jsonl(tokenCount("", `{"primary":{"used_percent":7}}`))
	rep := readCodex(t, in, CodexOptions{})

	if rep.Status != StatusUnknown || rep.Reason != ReasonNoTimestamp {
		t.Fatalf("status = %q/%q, want unknown/no_trustworthy_timestamp", rep.Status, rep.Reason)
	}
	if rep.ObservedAt != nil {
		t.Errorf("ObservedAt = %v, want nil", rep.ObservedAt)
	}
	if rep.Primary == nil || rep.Primary.UsedPercent == nil || *rep.Primary.UsedPercent != 7 {
		t.Errorf("Primary = %+v, want the window preserved", rep.Primary)
	}
}

func TestReadCodexImplausibleTimestampIsUntrusted(t *testing.T) {
	in := jsonl(tokenCount("1970-01-01T00:00:00Z", `{"primary":{"used_percent":7}}`))
	rep := readCodex(t, in, CodexOptions{})

	if rep.ObservedAt != nil {
		t.Errorf("ObservedAt = %v, want nil", rep.ObservedAt)
	}
	if rep.Status != StatusUnknown {
		t.Errorf("Status = %q, want unknown", rep.Status)
	}
	iss := issueFor(rep, "timestamp")
	if iss == nil || iss.Reason != ReasonOutOfRange {
		t.Errorf("timestamp issue = %+v, want out_of_range", iss)
	}
}

func TestReadCodexResetsInSeconds(t *testing.T) {
	in := jsonl(tokenCount("2026-01-02T03:00:00Z", `{"primary":{"used_percent":1,"resets_in_seconds":3600}}`))
	rep := readCodex(t, in, CodexOptions{})
	if rep.Primary == nil || rep.Primary.ResetsInSeconds == nil || *rep.Primary.ResetsInSeconds != 3600 {
		t.Fatalf("Primary = %+v", rep.Primary)
	}
	if rep.Primary.ResetsAt == nil || !rep.Primary.ResetsAt.Equal(at(t, "2026-01-02T04:00:00Z")) {
		t.Errorf("ResetsAt = %v, want 2026-01-02T04:00:00Z", rep.Primary.ResetsAt)
	}
}

func TestReadCodexResetsInSecondsNeedsTimestamp(t *testing.T) {
	in := jsonl(tokenCount("", `{"primary":{"used_percent":1,"resets_in_seconds":3600}}`))
	rep := readCodex(t, in, CodexOptions{})
	if rep.Primary == nil || rep.Primary.ResetsAt != nil {
		t.Fatalf("Primary.ResetsAt = %v, want nil without a trustworthy timestamp", rep.Primary)
	}
	iss := issueFor(rep, "rate_limits.primary.resets_at")
	if iss == nil || iss.Reason != ReasonNoTimestamp {
		t.Errorf("issue = %+v, want no_trustworthy_timestamp", iss)
	}
}

func TestReadCodexNilReader(t *testing.T) {
	if _, err := ReadCodex(context.Background(), nil, CodexOptions{}); err == nil {
		t.Fatal("ReadCodex(nil) = nil error, want an error")
	}
}

func TestReadCodexCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	in := jsonl(tokenCount("2026-01-02T03:04:05Z", `{"primary":{"used_percent":1}}`))
	if _, err := ReadCodex(ctx, strings.NewReader(in), CodexOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestReadCodexReaderError(t *testing.T) {
	_, err := ReadCodex(context.Background(), iotest.ErrReader(errors.New("synthetic read failure")), CodexOptions{})
	if err == nil {
		t.Fatal("ReadCodex = nil error, want the reader failure")
	}
}

func TestReportSample(t *testing.T) {
	in := jsonl(tokenCount("2026-01-02T03:00:00Z",
		`{"primary":{"used_percent":10,"resets_in_seconds":3600},"secondary":{"used_percent":2}}`))
	rep := readCodex(t, in, CodexOptions{})

	s, ok := rep.Sample(WindowPrimary)
	if !ok {
		t.Fatal("Sample(primary) = false, want true")
	}
	if s.Kind != WindowPrimary || !s.HasObservedAt || !s.HasResetsAt {
		t.Fatalf("sample = %+v", s)
	}
	if s.UsedPercent != 10 || s.RemainingPercent != 90 {
		t.Errorf("used/remaining = %v/%v", s.UsedPercent, s.RemainingPercent)
	}
	if !s.ResetsAt.Equal(at(t, "2026-01-02T04:00:00Z")) {
		t.Errorf("ResetsAt = %v", s.ResetsAt)
	}

	s2, ok := rep.Sample(WindowSecondary)
	if !ok {
		t.Fatal("Sample(secondary) = false, want true")
	}
	if s2.HasResetsAt {
		t.Errorf("secondary sample claimed a reset: %+v", s2)
	}
}

func TestReportSampleAbsentWindow(t *testing.T) {
	in := jsonl(tokenCount("2026-01-02T03:00:00Z", `{"primary":{"used_percent":10}}`))
	rep := readCodex(t, in, CodexOptions{})
	if _, ok := rep.Sample(WindowSecondary); ok {
		t.Fatal("Sample(secondary) = true for an absent window")
	}
	if rep.Window(WindowSecondary) != nil {
		t.Fatal("Window(secondary) != nil for an absent window")
	}
}

func TestReadCodexDoesNotInferQuotaFromLocalUsage(t *testing.T) {
	in := jsonl(
		`{"timestamp":"2026-01-02T03:00:00Z","type":"session_meta","payload":{"id":"synthetic-session","model_provider":"custom"}}`,
		`{"timestamp":"2026-01-02T03:00:01Z","type":"token_usage_record","payload":{"usage":{"input_tokens":900000,"output_tokens":900000,"total_tokens":1800000},"thread_token_usage":{"input_tokens":900000,"output_tokens":900000,"total_tokens":1800000}}}`,
		tokenCount("2026-01-02T03:00:02Z", ""),
	)
	rep := readCodex(t, in, CodexOptions{})

	if rep.Status != StatusUnavailable || rep.Reason != ReasonNoData {
		t.Fatalf("status = %q/%q, want unavailable/no_data", rep.Status, rep.Reason)
	}
	if rep.Primary != nil || rep.Secondary != nil || rep.Credits != nil || rep.PlanType != nil {
		t.Errorf("quota was invented from local usage: %+v", rep)
	}
}

func TestReadCodexRateLimitsShapeErrors(t *testing.T) {
	rep := readCodex(t, jsonl(tokenCount("2026-01-02T03:04:05Z", `"nope"`)), CodexOptions{})
	if rep.Status != StatusUnavailable || rep.Reason != ReasonMalformed {
		t.Fatalf("status = %q/%q, want unavailable/malformed", rep.Status, rep.Reason)
	}
	if issueFor(rep, "rate_limits") == nil {
		t.Errorf("Issues = %+v, want a rate_limits issue", rep.Issues)
	}

	rep = readCodex(t, jsonl(tokenCount("2026-01-02T03:04:05Z", `null`)), CodexOptions{})
	if rep.Status != StatusUnavailable || rep.Reason != ReasonNoData {
		t.Fatalf("null rate_limits: status = %q/%q, want unavailable/no_data", rep.Status, rep.Reason)
	}
	if len(rep.Issues) != 0 {
		t.Errorf("null rate_limits: Issues = %+v, want none", rep.Issues)
	}
}

func TestReadCodexNullWindowsAreAbsent(t *testing.T) {
	in := jsonl(tokenCount("2026-01-02T03:04:05Z", `{"primary":null,"secondary":null,"plan_type":"plus"}`))
	rep := readCodex(t, in, CodexOptions{})

	if rep.Primary != nil || rep.Secondary != nil {
		t.Errorf("windows = %+v/%+v, want nil", rep.Primary, rep.Secondary)
	}
	if rep.PlanType == nil || *rep.PlanType != "plus" {
		t.Errorf("PlanType = %v, want plus", rep.PlanType)
	}
	if rep.Status != StatusUnavailable || rep.Reason != ReasonMissing {
		t.Errorf("status = %q/%q, want unavailable/missing", rep.Status, rep.Reason)
	}
}

func TestReadCodexToleratesBlankAndCRLFLines(t *testing.T) {
	in := "\r\n" + tokenCount("2026-01-02T03:04:05Z", `{"primary":{"used_percent":5}}`) + "\r\n" + "\n" + "not json\n"
	rep := readCodex(t, in, CodexOptions{})

	if rep.Status != StatusOK {
		t.Fatalf("status = %q/%q, want ok", rep.Status, rep.Reason)
	}
	if rep.LinesRead != 4 {
		t.Errorf("LinesRead = %d, want 4", rep.LinesRead)
	}
	if rep.Primary == nil || *rep.Primary.UsedPercent != 5 {
		t.Errorf("Primary = %+v", rep.Primary)
	}
}

func TestReadCodexDefaultLimits(t *testing.T) {
	if DefaultMaxLineBytes <= 0 || DefaultMaxBytes <= 0 {
		t.Fatalf("defaults = %d/%d, want positive", DefaultMaxLineBytes, DefaultMaxBytes)
	}
	in := jsonl(tokenCount("2026-01-02T03:04:05Z", `{"primary":{"used_percent":1}}`))
	rep := readCodex(t, in, CodexOptions{MaxLineBytes: -1, MaxBytes: -1})
	if rep.Status != StatusOK {
		t.Errorf("negative limits: status = %q/%q, want ok", rep.Status, rep.Reason)
	}
}
