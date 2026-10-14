package quota

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// These tests pin down observation provenance: a Report describes exactly one
// selected record, and nothing from another record may be attributed to it.
//
// They were written before the repair and are expected to fail on the original
// selector, which merged ("carried") limit_id/limit_name/plan_type/credits from
// whatever record was seen last and then attached the chosen window's
// Primary/ObservedAt to those borrowed labels.

// TestReadCodexDoesNotRelabelWindowWithOtherLimitID is the concrete
// misattribution: an older usable primary that states no limit ID, followed by
// a newer record that states only a different limit ID. The old window must
// not be reported under the new limit's identity.
func TestReadCodexDoesNotRelabelWindowWithOtherLimitID(t *testing.T) {
	in := jsonl(
		tokenCount("2026-01-02T03:00:00Z", `{"primary":{"used_percent":10}}`),
		tokenCount("2026-01-02T04:00:00Z", `{"limit_id":"limit-B","limit_name":"Beta"}`),
	)
	rep := readCodex(t, in, CodexOptions{})

	if rep.Status != StatusOK {
		t.Fatalf("status = %q/%q, want ok (the newer record stated no quota window)", rep.Status, rep.Reason)
	}
	if rep.Line != 1 {
		t.Errorf("Line = %d, want 1 (the record that carried the window)", rep.Line)
	}
	if rep.ObservedAt == nil || rep.ObservedAt.Format("15:04:05") != "03:00:00" {
		t.Errorf("ObservedAt = %v, want the window record's own 03:00:00 instant", rep.ObservedAt)
	}
	if rep.Primary == nil || rep.Primary.UsedPercent == nil || *rep.Primary.UsedPercent != 10 {
		t.Fatalf("Primary = %+v, want the 10%% window from line 1", rep.Primary)
	}
	if rep.LimitID != "" {
		t.Errorf("LimitID = %q, want empty: line 1 never stated a limit ID and line 2 carried no window", rep.LimitID)
	}
	if rep.LimitName != "" {
		t.Errorf("LimitName = %q, want empty for the same reason", rep.LimitName)
	}
	if len(rep.OtherLimitIDs) != 1 || rep.OtherLimitIDs[0] != "limit-B" {
		t.Errorf("OtherLimitIDs = %v, want [limit-B] listed separately, not attributed", rep.OtherLimitIDs)
	}
	if rep.SupersededByLine != 0 {
		t.Errorf("SupersededByLine = %d, want 0: no newer record attempted a window", rep.SupersededByLine)
	}
}

// TestReadCodexPlanAndCreditsAreNotBorrowedAcrossRecords covers out-of-order
// timestamps: the selected observation is the newest instant (line 1, 06:00),
// but it appears first in the file. plan_type and credits from the other record
// must not be attached to it.
func TestReadCodexPlanAndCreditsAreNotBorrowedAcrossRecords(t *testing.T) {
	in := jsonl(
		tokenCount("2026-01-02T06:00:00Z", `{"limit_id":"limit-A","primary":{"used_percent":10}}`),
		tokenCount("2026-01-02T02:00:00Z", `{"limit_id":"limit-B","primary":{"used_percent":50},"plan_type":"pro","credits":{"has_credits":true,"balance":"9.99"}}`),
	)
	rep := readCodex(t, in, CodexOptions{})

	if rep.Line != 1 || rep.ObservedAt == nil || rep.ObservedAt.Format("15:04:05") != "06:00:00" {
		t.Fatalf("selected line/instant = %d/%v, want line 1 at 06:00:00", rep.Line, rep.ObservedAt)
	}
	if rep.Primary == nil || rep.Primary.UsedPercent == nil || *rep.Primary.UsedPercent != 10 {
		t.Fatalf("Primary = %+v, want the 10%% window from line 1", rep.Primary)
	}
	if rep.LimitID != "limit-A" {
		t.Errorf("LimitID = %q, want limit-A (the selected record's own ID)", rep.LimitID)
	}
	if rep.PlanType != nil {
		t.Errorf("PlanType = %q, want nil: only line 2 (a different limit) stated a plan", *rep.PlanType)
	}
	if rep.Credits != nil {
		t.Errorf("Credits = non-nil, want nil: only line 2 (a different limit) stated credits")
	}
	if len(rep.OtherLimitIDs) != 1 || rep.OtherLimitIDs[0] != "limit-B" {
		t.Errorf("OtherLimitIDs = %v, want [limit-B]", rep.OtherLimitIDs)
	}
}

// TestReadCodexNewerInvalidQuotaIsNotSilentlyCurrent covers conservative
// latest-known semantics. The newest window-bearing record is unusable, so the
// older usable window may be retained only as explicit history.
func TestReadCodexNewerInvalidQuotaIsNotSilentlyCurrent(t *testing.T) {
	t.Run("malformed newer window after a rollover", func(t *testing.T) {
		// Line 1's window resets at 03:30; line 2 is written at 04:00, so the
		// old window has already rolled over by the time the log says its shape
		// is unusable. Reporting line 1's 90% as fresh would be wrong twice.
		in := jsonl(
			tokenCount("2026-01-02T03:00:00Z", `{"limit_id":"limit-A","primary":{"used_percent":90,"resets_in_seconds":1800}}`),
			tokenCount("2026-01-02T04:00:00Z", `{"limit_id":"limit-A","primary":"nope"}`),
		)
		rep := readCodex(t, in, CodexOptions{})

		if rep.Status == StatusOK {
			t.Fatalf("status = %q/%q, want the old window marked not-current", rep.Status, rep.Reason)
		}
		if rep.Status != StatusStale {
			t.Errorf("Status = %q, want stale (historical fallback, not fresh)", rep.Status)
		}
		if rep.Reason != ReasonNewerInvalid {
			t.Errorf("Reason = %q, want %q", rep.Reason, ReasonNewerInvalid)
		}
		if rep.SupersededByLine != 2 {
			t.Errorf("SupersededByLine = %d, want 2", rep.SupersededByLine)
		}
		if !rep.Available() {
			t.Error("Available() = false, want true: the historical window is still readable data")
		}
		if rep.Line != 1 {
			t.Errorf("Line = %d, want 1 (the retained observation's own line)", rep.Line)
		}
		if rep.ObservedAt == nil || rep.ObservedAt.Format("15:04:05") != "03:00:00" {
			t.Errorf("ObservedAt = %v, want the retained window's own exact 03:00:00 instant", rep.ObservedAt)
		}
		if rep.Primary == nil || rep.Primary.UsedPercent == nil || *rep.Primary.UsedPercent != 90 {
			t.Errorf("Primary = %+v, want the retained 90%% window", rep.Primary)
		}
	})

	t.Run("newer window with no usage", func(t *testing.T) {
		in := jsonl(
			tokenCount("2026-01-02T03:00:00Z", `{"primary":{"used_percent":10}}`),
			tokenCount("2026-01-02T04:00:00Z", `{"primary":{"window_minutes":300}}`),
		)
		rep := readCodex(t, in, CodexOptions{})

		if rep.Status == StatusOK {
			t.Fatalf("status = %q/%q, want not ok: the newest window states no usage", rep.Status, rep.Reason)
		}
		if rep.Status != StatusStale {
			t.Errorf("Status = %q, want stale (historical fallback)", rep.Status)
		}
		if rep.Reason != ReasonNewerInvalid || rep.SupersededByLine != 2 {
			t.Errorf("reason/superseded = %q/%d, want %q/2", rep.Reason, rep.SupersededByLine, ReasonNewerInvalid)
		}
		if rep.Primary == nil || rep.Primary.UsedPercent == nil || *rep.Primary.UsedPercent != 10 {
			t.Errorf("Primary = %+v, want the retained 10%% window", rep.Primary)
		}
	})
}

// TestReadCodexMissingTimestampIsNotInventedFromAnotherRecord: an untimestamped
// metadata-only record must not donate its labels, and no ObservedAt may appear
// for it.
func TestReadCodexMissingTimestampIsNotInventedFromAnotherRecord(t *testing.T) {
	in := jsonl(
		tokenCount("2026-01-02T03:00:00Z", `{"primary":{"used_percent":10}}`),
		tokenCount("", `{"limit_id":"limit-B","plan_type":"pro"}`),
	)
	rep := readCodex(t, in, CodexOptions{})

	if rep.ObservedAt == nil || rep.ObservedAt.Format("15:04:05") != "03:00:00" {
		t.Errorf("ObservedAt = %v, want the timestamped window's own 03:00:00", rep.ObservedAt)
	}
	if rep.LimitID != "" || rep.LimitName != "" {
		t.Errorf("limit id/name = %q/%q, want empty: the selected record stated none", rep.LimitID, rep.LimitName)
	}
	if rep.PlanType != nil {
		t.Errorf("PlanType = %q, want nil: the plan was on an untimestamped metadata-only record", *rep.PlanType)
	}
	if len(rep.OtherLimitIDs) != 1 || rep.OtherLimitIDs[0] != "limit-B" {
		t.Errorf("OtherLimitIDs = %v, want [limit-B]", rep.OtherLimitIDs)
	}
	if rep.Primary == nil || rep.Primary.UsedPercent == nil || *rep.Primary.UsedPercent != 10 {
		t.Errorf("Primary = %+v, want the 10%% window", rep.Primary)
	}
}

// TestReadCodexSelectedObservationKeepsItsOwnInstant is the consistency guard:
// with out-of-order timestamps, the window, its instant, and its line all come
// from one record.
func TestReadCodexSelectedObservationKeepsItsOwnInstant(t *testing.T) {
	in := jsonl(
		tokenCount("2026-01-02T10:00:00Z", `{"limit_id":"limit-A","primary":{"used_percent":10}}`),
		tokenCount("2026-01-02T02:00:00Z", `{"limit_id":"limit-B","primary":{"used_percent":50}}`),
		tokenCount("2026-01-02T06:00:00Z", `{"limit_id":"limit-C","primary":{"used_percent":30}}`),
	)
	rep := readCodex(t, in, CodexOptions{})

	if rep.Line != 1 {
		t.Errorf("Line = %d, want 1 (newest instant)", rep.Line)
	}
	if rep.ObservedAt == nil || rep.ObservedAt.Format("15:04:05") != "10:00:00" {
		t.Errorf("ObservedAt = %v, want 10:00:00", rep.ObservedAt)
	}
	if rep.LimitID != "limit-A" {
		t.Errorf("LimitID = %q, want limit-A", rep.LimitID)
	}
	if rep.Primary == nil || rep.Primary.UsedPercent == nil || *rep.Primary.UsedPercent != 10 {
		t.Errorf("Primary = %+v, want the 10%% window from line 1", rep.Primary)
	}
}

// TestReadCodexUnknownFieldsAreNotMetadata: unknown fields stay counted, never
// interpreted as plan or limit identity.
func TestReadCodexUnknownFieldsAreNotMetadata(t *testing.T) {
	in := jsonl(tokenCount("2026-01-02T03:00:00Z",
		`{"primary":{"used_percent":10},"future_plan":"enterprise","future_limit":{"id":"limit-Z"}}`))
	rep := readCodex(t, in, CodexOptions{})

	if rep.UnknownFields != 2 {
		t.Errorf("UnknownFields = %d, want 2", rep.UnknownFields)
	}
	if rep.PlanType != nil {
		t.Errorf("PlanType = %q, want nil for an unknown field", *rep.PlanType)
	}
	if rep.LimitID != "" {
		t.Errorf("LimitID = %q, want empty for an unknown field", rep.LimitID)
	}
}

// TestEstimatePaceRejectsSamplesFromDifferentLimits: two observations of the
// same window kind still describe different quota buckets when they name
// different limit IDs. Report.Sample must carry that identity so EstimatePace
// can refuse the comparison.
func TestEstimatePaceRejectsSamplesFromDifferentLimits(t *testing.T) {
	reset := at(t, "2026-01-02T08:00:00Z").Unix()
	limitsA := fmt.Sprintf(`{"limit_id":"limit-A","primary":{"used_percent":10,"resets_at":%d}}`, reset)
	limitsB := fmt.Sprintf(`{"limit_id":"limit-B","primary":{"used_percent":20,"resets_at":%d}}`, reset)

	repA := readCodex(t, jsonl(tokenCount("2026-01-02T03:00:00Z", limitsA)), CodexOptions{})
	repB := readCodex(t, jsonl(tokenCount("2026-01-02T04:00:00Z", limitsB)), CodexOptions{})

	sa, okA := repA.Sample(WindowPrimary)
	sb, okB := repB.Sample(WindowPrimary)
	if !okA || !okB {
		t.Fatalf("Sample ok = %v/%v, want both usable", okA, okB)
	}

	p := EstimatePace(sa, sb, PaceOptions{})
	if p.Available {
		t.Fatalf("pace = %+v, want unavailable: the samples name different limits", p)
	}
	if p.Reason != ReasonLimitMismatch {
		t.Errorf("Reason = %q, want %q", p.Reason, ReasonLimitMismatch)
	}

	// Positive control: the same limit with the same reset is still measurable.
	sameA, _ := repA.Sample(WindowPrimary)
	sameBRep := readCodex(t, jsonl(tokenCount("2026-01-02T04:00:00Z",
		fmt.Sprintf(`{"limit_id":"limit-A","primary":{"used_percent":20,"resets_at":%d}}`, reset))), CodexOptions{})
	sameB, _ := sameBRep.Sample(WindowPrimary)
	if p := EstimatePace(sameA, sameB, PaceOptions{}); !p.Available {
		t.Errorf("pace for one limit = %+v, want available", p)
	}

	// An identity known on only one side is not proof of a match either.
	unknownB := readCodex(t, jsonl(tokenCount("2026-01-02T04:00:00Z",
		fmt.Sprintf(`{"primary":{"used_percent":20,"resets_at":%d}}`, reset))), CodexOptions{})
	ub, _ := unknownB.Sample(WindowPrimary)
	if p := EstimatePace(sameA, ub, PaceOptions{}); p.Available || p.Reason != ReasonLimitMismatch {
		t.Errorf("pace with one unknown identity = %+v, want unavailable/%s", p, ReasonLimitMismatch)
	}
}

// TestReadCodexReaderErrorTextIsNotEmbedded: a reader error is arbitrary
// caller-supplied text, and this package cannot sanitize it, so it must not be
// reproduced in the returned error's message. The cause stays reachable for
// callers that deliberately unwrap it.
func TestReadCodexReaderErrorTextIsNotEmbedded(t *testing.T) {
	_, err := ReadCodex(context.Background(), secretErrorReader{}, CodexOptions{})
	if err == nil {
		t.Fatal("ReadCodex = nil error, want the reader failure")
	}
	if strings.Contains(err.Error(), secretReaderText) {
		t.Fatalf("error message echoed the reader's own text: %v", err)
	}
	if !errors.Is(err, errSecretReader) {
		t.Fatalf("errors.Is(err, cause) = false; the reader cause must stay reachable for deliberate unwrapping")
	}
}

const secretReaderText = "reader exploded holding " + syntheticSecretValue

var errSecretReader = errors.New(secretReaderText)

type secretErrorReader struct{}

func (secretErrorReader) Read([]byte) (int, error) { return 0, errSecretReader }

// TestReadCodexOtherLimitIDsAreBoundedSortedAndDeduped keeps the separate
// identity list deterministic and small even for a stream that names many
// limits.
func TestReadCodexOtherLimitIDsAreBoundedSortedAndDeduped(t *testing.T) {
	lines := []string{tokenCount("2026-01-02T03:00:00Z", `{"limit_id":"limit-M","primary":{"used_percent":10}}`)}
	ids := []string{"id-03", "id-01", "id-00", "id-02", "id-03", "id-09", "id-08", "id-07", "id-06", "id-05", "id-04"}
	for _, id := range ids {
		lines = append(lines, tokenCount("2026-01-02T03:00:01Z", `{"limit_id":"`+id+`"}`))
	}
	rep := readCodex(t, jsonl(lines...), CodexOptions{})

	if rep.LimitID != "limit-M" {
		t.Fatalf("LimitID = %q, want limit-M", rep.LimitID)
	}
	want := []string{"id-00", "id-01", "id-02", "id-03", "id-04", "id-05", "id-06", "id-07"}
	if len(rep.OtherLimitIDs) != len(want) {
		t.Fatalf("OtherLimitIDs = %v, want %v", rep.OtherLimitIDs, want)
	}
	for i := range want {
		if rep.OtherLimitIDs[i] != want[i] {
			t.Fatalf("OtherLimitIDs = %v, want %v", rep.OtherLimitIDs, want)
		}
	}
}

// TestReadCodexNewerUsableObservationStaysOK is the positive control for the
// supersession rule: a newer usable window is simply the current one.
func TestReadCodexNewerUsableObservationStaysOK(t *testing.T) {
	in := jsonl(
		tokenCount("2026-01-02T03:00:00Z", `{"limit_id":"limit-A","primary":{"used_percent":10}}`),
		tokenCount("2026-01-02T04:00:00Z", `{"limit_id":"limit-A","primary":{"used_percent":20}}`),
	)
	rep := readCodex(t, in, CodexOptions{})

	if rep.Status != StatusOK || rep.Reason != ReasonNone {
		t.Fatalf("status = %q/%q, want ok", rep.Status, rep.Reason)
	}
	if rep.Line != 2 || rep.SupersededByLine != 0 {
		t.Errorf("line/superseded = %d/%d, want 2/0", rep.Line, rep.SupersededByLine)
	}
	if rep.Primary == nil || rep.Primary.UsedPercent == nil || *rep.Primary.UsedPercent != 20 {
		t.Errorf("Primary = %+v, want the newer 20%% window", rep.Primary)
	}
}

// TestReadCodexAppendOrderDecidesCurrency: a newer unusable record supersedes
// an older usable one even when its own timestamp looks older, because a
// rollout is append-ordered and the newest write is the newest knowledge.
func TestReadCodexAppendOrderDecidesCurrency(t *testing.T) {
	in := jsonl(
		tokenCount("2026-01-02T06:00:00Z", `{"limit_id":"limit-A","primary":{"used_percent":10}}`),
		tokenCount("2026-01-02T02:00:00Z", `{"limit_id":"limit-A","primary":"nope"}`),
	)
	rep := readCodex(t, in, CodexOptions{})

	if rep.Status != StatusStale || rep.Reason != ReasonNewerInvalid {
		t.Fatalf("status/reason = %q/%q, want stale/%s", rep.Status, rep.Reason, ReasonNewerInvalid)
	}
	if rep.SupersededByLine != 2 {
		t.Errorf("SupersededByLine = %d, want 2", rep.SupersededByLine)
	}
	if rep.ObservedAt == nil || rep.ObservedAt.Format("15:04:05") != "06:00:00" {
		t.Errorf("ObservedAt = %v, want the retained record's own 06:00:00", rep.ObservedAt)
	}
}

// TestReadCodexReaderErrorIsTyped pins the typed error shape: the cause is
// reachable deliberately, the message is not the cause's text.
func TestReadCodexReaderErrorIsTyped(t *testing.T) {
	_, err := ReadCodex(context.Background(), secretErrorReader{}, CodexOptions{})
	if err == nil {
		t.Fatal("ReadCodex = nil error, want the reader failure")
	}
	var re *ReadError
	if !errors.As(err, &re) {
		t.Fatalf("errors.As(err, *ReadError) = false for %T", err)
	}
	if !errors.Is(re.Cause, errSecretReader) {
		t.Errorf("Cause = %v, want the reader's own error", re.Cause)
	}
	if strings.Contains(re.Error(), secretReaderText) {
		t.Errorf("ReadError.Error() = %q, want a message that does not reproduce the cause", re.Error())
	}
}
