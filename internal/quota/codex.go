package quota

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// CodexOptions configures ReadCodex.
type CodexOptions struct {
	// Source is the provenance label reported back; typically the log path.
	Source string

	// MaxLineBytes caps one JSONL record. Zero or negative uses
	// DefaultMaxLineBytes.
	MaxLineBytes int

	// MaxBytes caps total input. Zero or negative uses DefaultMaxBytes.
	MaxBytes int64

	// Now is the reference instant for staleness. Zero means time.Now.
	Now time.Time

	// StaleAfter marks an otherwise-valid observation stale when it is older
	// than this. Zero disables the check.
	StaleAfter time.Duration
}

// maxResetSeconds bounds a resets_in_seconds countdown at 100 years so an
// absurd value cannot overflow the projection.
const maxResetSeconds = int64(100 * 365 * 24 * 60 * 60)

var nullLiteral = []byte("null")

// ReadCodex reads a Codex rollout JSONL stream and reports the quota
// observation it contains.
//
// It reads only what the log states. Quota is never inferred from token
// counts, context-window sizes, or any other field: a record without a
// rate_limits payload contributes no quota, and every window field stays nil
// unless the log carried it.
func ReadCodex(ctx context.Context, r io.Reader, opts CodexOptions) (*Report, error) {
	if r == nil {
		return nil, errors.New("quota: nil reader")
	}
	maxLine := opts.MaxLineBytes
	if maxLine <= 0 {
		maxLine = DefaultMaxLineBytes
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}

	rep := &Report{Source: sanitize(opts.Source, maxSourceLen)}
	agg := newIssueLog()
	// unknown holds only field *names*, and only for counting; nothing from it
	// is ever copied into the Report, an error, or a log line.
	unknown := make(map[string]struct{})
	sel := &selector{}

	lr := &io.LimitedReader{R: r, N: maxBytes}
	br := bufio.NewReaderSize(lr, maxLine)

	lineNo := 0
	overflow := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunk, rerr := br.ReadSlice('\n')
		rep.BytesRead += int64(len(chunk))

		switch {
		case rerr == bufio.ErrBufferFull:
			// A record larger than one buffer: drain the rest without
			// retaining the body.
			overflow = true
			continue

		case overflow:
			// Tail of a drained oversized record.
			overflow = false
			lineNo++
			agg.add("line", ReasonLineTooLong, lineNo)

		case rerr == nil:
			lineNo++
			sel.observe(decodeLine(trimEOL(chunk), lineNo, agg, unknown))
			continue

		case rerr == io.EOF:
			// A trailing record with no newline is incomplete input and is
			// deliberately not consumed.

		default:
			return nil, readErr(rerr)
		}

		if rerr == io.EOF {
			break
		}
	}

	rep.LinesRead = lineNo
	if lr.N <= 0 {
		rep.Truncated = true
		agg.add("input", ReasonByteLimit, lineNo)
	}
	rep.UnknownFields = len(unknown)

	finalize(rep, sel, agg, opts)
	return rep, nil
}

// ReadError reports that the caller-supplied reader failed. It wraps the
// original cause so callers can still reach it with errors.Is and errors.As,
// but Error deliberately returns a fixed message: a reader error is arbitrary
// caller-supplied text that can embed log content, filesystem paths, or
// credentials, and this package cannot sanitize it. Callers that surface the
// unwrapped cause to users own that text.
type ReadError struct {
	// Cause is the reader's own error. Its message is not sanitized.
	Cause error
}

func (e *ReadError) Error() string {
	return "quota: read codex rollout: reader failed"
}

// Unwrap exposes the unsanitized reader error for deliberate inspection.
func (e *ReadError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func readErr(err error) error {
	return &ReadError{Cause: err}
}

func trimEOL(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte{'\n'})
	return bytes.TrimSuffix(line, []byte{'\r'})
}

// rawLine is the envelope shared by every Codex rollout record.
type rawLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// rawEvent is the payload object. Only the fields this package interprets are
// decoded; everything else is tolerated and counted.
type rawEvent struct {
	Type       string          `json:"type"`
	RateLimits json.RawMessage `json:"rate_limits"`
}

// snapshot is one line's quota content, before selection. The JSON tags are
// the bounded wire form a Collector carries in a parser's cursor, so a live
// incremental parse selects exactly the way ReadCodex selects over a whole
// stream.
type snapshot struct {
	Line      int        `json:"line,omitempty"`
	Record    string     `json:"record,omitempty"`
	At        *time.Time `json:"at,omitempty"`
	LimitID   string     `json:"limit_id,omitempty"`
	LimitName string     `json:"limit_name,omitempty"`
	PlanType  *string    `json:"plan_type,omitempty"`
	Credits   *Credits   `json:"credits,omitempty"`
	Primary   *Window    `json:"primary,omitempty"`
	Secondary *Window    `json:"secondary,omitempty"`
	Bad       bool       `json:"bad,omitempty"`
}

// hasUsage reports whether a window carries an actual usage observation.
func hasUsage(w *Window) bool { return w != nil && w.Status == StatusOK }

// tier ranks snapshots so a usable observation always beats a partial one.
func (s *snapshot) tier() int {
	if s == nil {
		return -1
	}
	if hasUsage(s.Primary) || hasUsage(s.Secondary) {
		return 2
	}
	if s.PlanType != nil || s.Credits != nil || s.LimitID != "" || s.LimitName != "" {
		return 1
	}
	return 0
}

// provenanced reports whether the line said anything about quota at all.
func (s *snapshot) provenanced() bool {
	if s == nil {
		return false
	}
	return s.Primary != nil || s.Secondary != nil || s.PlanType != nil ||
		s.Credits != nil || s.LimitID != "" || s.LimitName != ""
}

// selector picks one observation and, separately, keeps provenance about the
// records it did not pick: the last window-bearing record in append order
// (currency) and every distinct limit ID seen (identity).
//
// Labels are never carried from one record to another. A Report describes one
// selected record, and only that record's own limit/plan/credits labels are
// attributed to it; identities seen elsewhere are listed in OtherLimitIDs
// instead of being merged into it.
type selector struct {
	best     *snapshot
	latest   *snapshot
	limitIDs []string
	seenIDs  map[string]struct{}
	// limitCap bounds how many distinct limit IDs are remembered. Zero means
	// unlimited, which is what ReadCodex uses; a Collector sets it so the state
	// it persists in a cursor can never grow with the length of a log.
	limitCap int
}

// isWindowAttempt reports whether a record tried to state a quota window (or
// carried one it could not decode). A record with only plan/credits/limit
// metadata is not an attempt.
func isWindowAttempt(s *snapshot) bool {
	return s != nil && (s.Bad || s.Primary != nil || s.Secondary != nil)
}

// usableObservation reports whether a record carries a window with an actual
// usage observation.
func usableObservation(s *snapshot) bool {
	return s != nil && (hasUsage(s.Primary) || hasUsage(s.Secondary))
}

func (s *selector) observe(snap *snapshot) {
	if snap == nil {
		return
	}
	if snap.LimitID != "" {
		if s.seenIDs == nil {
			s.seenIDs = make(map[string]struct{})
		}
		_, seen := s.seenIDs[snap.LimitID]
		if !seen && (s.limitCap <= 0 || len(s.seenIDs) < s.limitCap) {
			s.seenIDs[snap.LimitID] = struct{}{}
			s.limitIDs = append(s.limitIDs, snap.LimitID)
		}
	}
	if isWindowAttempt(snap) {
		// Records arrive in append order, so the last one seen is the newest
		// quota knowledge the rollout wrote.
		s.latest = snap
	}
	if s.best == nil || better(s.best, snap) {
		s.best = snap
	}
}

// otherLimitIDs returns every limit ID seen on a record other than best,
// sorted and capped. The selected record's own ID is excluded because that one
// is reported as LimitID.
func (s *selector) otherLimitIDs(best *snapshot) []string {
	self := ""
	if best != nil {
		self = best.LimitID
	}
	out := make([]string, 0, len(s.limitIDs))
	for _, id := range s.limitIDs {
		if id == self {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	if len(out) > maxOtherLimitIDs {
		out = out[:maxOtherLimitIDs]
	}
	return out
}

// better reports whether cand should replace cur: a higher tier wins, then a
// trustworthy timestamp beats none, then the later timestamp wins, then the
// later line.
func better(cur, cand *snapshot) bool {
	if tc, nc := cur.tier(), cand.tier(); tc != nc {
		return nc > tc
	}
	hc, hn := cur.At != nil, cand.At != nil
	if hc != hn {
		return hn
	}
	if hc && hn && !cur.At.Equal(*cand.At) {
		return cand.At.After(*cur.At)
	}
	return cand.Line > cur.Line
}

func finalize(rep *Report, sel *selector, agg *issueLog, opts CodexOptions) {
	best := sel.best
	rep.Issues = agg.list()

	if best != nil {
		rep.Line = best.Line
		rep.RecordType = best.Record
		rep.ObservedAt = best.At
		rep.Primary = best.Primary
		rep.Secondary = best.Secondary
		rep.LimitID = best.LimitID
		rep.LimitName = best.LimitName
		rep.PlanType = best.PlanType
		rep.Credits = best.Credits
	}
	rep.OtherLimitIDs = sel.otherLimitIDs(best)

	switch {
	case best == nil:
		rep.Status, rep.Reason = StatusUnavailable, ReasonNoData
	case best.Bad:
		rep.Status, rep.Reason = StatusUnavailable, ReasonMalformed
	case best.tier() < 2:
		// Plan and credits may be known, but no window was stated, so there is
		// no quota observation to report.
		rep.Status, rep.Reason = StatusUnavailable, ReasonMissing
	case rep.ObservedAt == nil:
		rep.Status, rep.Reason = StatusUnknown, ReasonNoTimestamp
	default:
		rep.Status, rep.Reason = StatusOK, ReasonNone
	}

	// Conservative currency: when the newest window-bearing record was
	// unusable, an older usable observation is retained only as history. It
	// keeps its own line and exact instant and is never reported as fresh.
	if usableObservation(best) && sel.latest != nil && sel.latest.Line != best.Line && !usableObservation(sel.latest) {
		rep.SupersededByLine = sel.latest.Line
		if rep.Status == StatusOK || rep.Status == StatusStale {
			rep.Status, rep.Reason = StatusStale, ReasonNewerInvalid
		}
	}

	if rep.Status == StatusOK && opts.StaleAfter > 0 && rep.ObservedAt != nil {
		now := opts.Now
		if now.IsZero() {
			now = time.Now()
		}
		if now.Sub(*rep.ObservedAt) > opts.StaleAfter {
			rep.Status, rep.Reason = StatusStale, ReasonStale
		}
	}
}

// decodeLine turns one JSONL record into a snapshot, recording every shape it
// could not interpret. It returns nil when the line carries no quota at all.
func decodeLine(line []byte, lineNo int, agg *issueLog, unknown map[string]struct{}) *snapshot {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return nil
	}
	var raw rawLine
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		agg.add("line", ReasonMalformed, lineNo)
		return nil
	}
	if len(raw.Payload) == 0 || bytes.Equal(bytes.TrimSpace(raw.Payload), nullLiteral) {
		return nil
	}
	var env rawEvent
	if err := json.Unmarshal(raw.Payload, &env); err != nil {
		agg.add("payload", ReasonMalformed, lineNo)
		return nil
	}
	if len(env.RateLimits) == 0 || bytes.Equal(bytes.TrimSpace(env.RateLimits), nullLiteral) {
		// No quota payload: token counts and context-window sizes are not
		// quota and are never converted into one.
		return nil
	}

	var at *time.Time
	if raw.Timestamp != "" {
		if ts, reason := decodeTimestamp(raw.Timestamp); reason == ReasonNone {
			at = &ts
		} else {
			agg.add("timestamp", reason, lineNo)
		}
	}
	return decodeRateLimits(env.RateLimits, recordType(raw.Type, env.Type), at, lineNo, agg, unknown)
}

// decodeRateLimits turns one record's rate_limits payload into a snapshot,
// recording every shape it could not interpret. It returns nil when the payload
// says nothing about quota. ReadCodex calls it after decoding a record's
// envelope; a Collector calls it for a record a live parser has already
// decoded, so both select over exactly the same observations.
func decodeRateLimits(rateLimits json.RawMessage, record string, at *time.Time, lineNo int, agg *issueLog, unknown map[string]struct{}) *snapshot {
	trimmed := bytes.TrimSpace(rateLimits)
	if len(trimmed) == 0 || bytes.Equal(trimmed, nullLiteral) {
		return nil
	}

	snap := &snapshot{Line: lineNo, Record: record, At: at}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		agg.add("rate_limits", ReasonMalformed, lineNo)
		return &snapshot{Line: lineNo, Record: record, At: at, Bad: true}
	}
	for key, value := range obj {
		switch key {
		case "primary":
			snap.Primary = decodeWindow(value, WindowPrimary, lineNo, agg, unknown)
		case "secondary":
			snap.Secondary = decodeWindow(value, WindowSecondary, lineNo, agg, unknown)
		case "plan_type":
			snap.PlanType = decodeOptionalString(value, "rate_limits.plan_type", lineNo, agg)
		case "limit_id":
			snap.LimitID = decodeLabel(value, "rate_limits.limit_id", lineNo, agg)
		case "limit_name":
			snap.LimitName = decodeLabel(value, "rate_limits.limit_name", lineNo, agg)
		case "credits":
			snap.Credits = decodeCredits(value, lineNo, agg, unknown)
		default:
			unknown["rate_limits."+key] = struct{}{}
		}
	}

	for _, w := range []*Window{snap.Primary, snap.Secondary} {
		finishWindow(w)
		finishResets(w, snap.At, lineNo, agg)
	}
	snap.Bad = windowBad(snap.Primary) || windowBad(snap.Secondary)
	if !snap.provenanced() && !snap.Bad {
		return nil
	}
	return snap
}

func windowBad(w *Window) bool { return w != nil && w.Status == StatusMalformed }

// recordType renders the static schema path of a record. It never contains log
// text beyond the record type labels themselves.
func recordType(outer, inner string) string {
	switch {
	case outer == "" && inner == "":
		return ""
	case outer == "":
		return sanitize(inner, maxLabelLen)
	case inner == "" || inner == outer:
		return sanitize(outer, maxLabelLen)
	default:
		return sanitize(outer+"."+inner, maxLabelLen)
	}
}

// decodeWindow reads one quota window. It returns nil when the log did not
// carry the window, a malformed Window when the shape was not an object, and
// otherwise a Window whose fields are set only where the log stated them.
func decodeWindow(raw json.RawMessage, kind WindowKind, lineNo int, agg *issueLog, unknown map[string]struct{}) *Window {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, nullLiteral) {
		return nil
	}
	path := "rate_limits." + string(kind)
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		agg.add(path, ReasonMalformed, lineNo)
		return &Window{Kind: kind, Status: StatusMalformed, Reason: ReasonMalformed}
	}

	w := &Window{Kind: kind, Status: StatusOK}
	for key, value := range obj {
		switch key {
		case "used_percent":
			if f, reason := decodePercent(value); reason != ReasonNone {
				agg.add(path+".used_percent", reason, lineNo)
			} else {
				w.UsedPercent = &f
			}
		case "remaining_percent":
			if f, reason := decodePercent(value); reason != ReasonNone {
				agg.add(path+".remaining_percent", reason, lineNo)
			} else {
				w.RemainingPercent = &f
			}
		case "window_minutes":
			if n, reason := decodeInt(value); reason != ReasonNone {
				agg.add(path+".window_minutes", reason, lineNo)
			} else {
				w.WindowMinutes = &n
			}
		case "resets_in_seconds":
			if n, reason := decodeInt(value); reason != ReasonNone {
				agg.add(path+".resets_in_seconds", reason, lineNo)
			} else {
				w.ResetsInSeconds = &n
			}
		case "resets_at":
			if ts, reason := decodeEpochSeconds(value); reason != ReasonNone {
				agg.add(path+".resets_at", reason, lineNo)
			} else {
				w.ResetsAt = &ts
			}
		default:
			unknown["rate_limits."+string(kind)+"."+key] = struct{}{}
		}
	}
	return w
}

// finishWindow derives the remaining percentage when the log stated only the
// used percentage, and marks a window that carried no usage observation.
func finishWindow(w *Window) {
	if w == nil || w.Status != StatusOK {
		return
	}
	if w.UsedPercent == nil && w.RemainingPercent == nil {
		w.Status, w.Reason = StatusUnknown, ReasonMissing
		return
	}
	if w.RemainingPercent == nil && w.UsedPercent != nil {
		r := math.Max(0, math.Min(100, 100-*w.UsedPercent))
		w.RemainingPercent = &r
		w.RemainingDerived = true
	}
}

// finishResets resolves a reset instant. An explicit resets_at wins; otherwise
// the countdown is applied to this record's own observation instant, and only
// when that instant is trustworthy.
func finishResets(w *Window, at *time.Time, lineNo int, agg *issueLog) {
	if w == nil || w.Status != StatusOK || w.ResetsAt != nil {
		return
	}
	path := "rate_limits." + string(w.Kind) + ".resets_at"
	if w.ResetsInSeconds == nil {
		return
	}
	secs := *w.ResetsInSeconds
	if secs < 0 || secs > maxResetSeconds {
		agg.add(path, ReasonOutOfRange, lineNo)
		return
	}
	if at == nil {
		agg.add(path, ReasonNoTimestamp, lineNo)
		return
	}
	reset := at.Add(time.Duration(secs) * time.Second)
	if reset.Before(trustedEpochMin) || !reset.Before(trustedEpochMax) {
		agg.add(path, ReasonOutOfRange, lineNo)
		return
	}
	w.ResetsAt = &reset
}

// decodeCredits reads the credits block. It reports credits only for the
// fields the log stated, and never converts the balance, whose unit the log
// does not define.
func decodeCredits(raw json.RawMessage, lineNo int, agg *issueLog, unknown map[string]struct{}) *Credits {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, nullLiteral) {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		agg.add("rate_limits.credits", ReasonUnsupportedType, lineNo)
		return nil
	}

	c := &Credits{}
	stated := false
	for key, value := range obj {
		switch key {
		case "has_credits":
			if b, ok := decodeBool(value); ok {
				c.HasCredits, stated = &b, true
			} else if !isNull(value) {
				agg.add("rate_limits.credits.has_credits", ReasonUnsupportedType, lineNo)
			}
		case "unlimited":
			if b, ok := decodeBool(value); ok {
				c.Unlimited, stated = &b, true
			} else if !isNull(value) {
				agg.add("rate_limits.credits.unlimited", ReasonUnsupportedType, lineNo)
			}
		case "balance":
			if s, ok := decodeString(value); ok {
				s = sanitize(s, maxLabelLen)
				c.Balance, stated = &s, true
			} else if !isNull(value) {
				agg.add("rate_limits.credits.balance", ReasonUnsupportedType, lineNo)
			}
		default:
			unknown["rate_limits.credits."+key] = struct{}{}
		}
	}
	if !stated {
		return nil
	}
	return c
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), nullLiteral)
}

// decodeNumber reads a JSON number token. A quoted number is a different JSON
// type than the schema defines, so it is reported as unsupported rather than
// coerced; a number that is not representable is malformed.
func decodeNumber(raw json.RawMessage) (json.Number, Reason) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, nullLiteral) {
		return "", ReasonMissing
	}
	if trimmed[0] == '"' {
		return "", ReasonUnsupportedType
	}
	var num json.Number
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&num); err != nil {
		return "", ReasonUnsupportedType
	}
	return num, ReasonNone
}

// decodeFloat reads a JSON number.
func decodeFloat(raw json.RawMessage) (float64, Reason) {
	num, reason := decodeNumber(raw)
	if reason != ReasonNone {
		return 0, reason
	}
	f, err := num.Float64()
	if err != nil {
		return 0, ReasonMalformed
	}
	return f, ReasonNone
}

// decodeInt reads a JSON integer. A number with a fractional part or an
// unrepresentable magnitude is malformed.
func decodeInt(raw json.RawMessage) (int64, Reason) {
	num, reason := decodeNumber(raw)
	if reason != ReasonNone {
		return 0, reason
	}
	n, err := num.Int64()
	if err != nil {
		return 0, ReasonMalformed
	}
	return n, ReasonNone
}

// decodePercent reads a 0..100 percentage.
func decodePercent(raw json.RawMessage) (float64, Reason) {
	f, reason := decodeFloat(raw)
	if reason != ReasonNone {
		return 0, reason
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 100 {
		return 0, ReasonOutOfRange
	}
	return f, ReasonNone
}

// decodeEpochSeconds reads a Unix-seconds instant and refuses to guess a unit
// for a value outside the trusted range.
func decodeEpochSeconds(raw json.RawMessage) (time.Time, Reason) {
	n, reason := decodeInt(raw)
	if reason != ReasonNone {
		return time.Time{}, reason
	}
	if n < trustedEpochMin.Unix() || n >= trustedEpochMax.Unix() {
		return time.Time{}, ReasonOutOfRange
	}
	return time.Unix(n, 0).UTC(), ReasonNone
}

// decodeTimestamp reads an RFC3339 instant, refusing an implausible one.
func decodeTimestamp(s string) (time.Time, Reason) {
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, ReasonMalformed
	}
	ts = ts.UTC()
	if ts.Before(trustedEpochMin) || !ts.Before(trustedEpochMax) {
		return time.Time{}, ReasonOutOfRange
	}
	return ts, ReasonNone
}

func decodeString(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, nullLiteral) {
		return "", false
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return "", false
	}
	return s, true
}

func decodeBool(raw json.RawMessage) (bool, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, nullLiteral) {
		return false, false
	}
	var b bool
	if err := json.Unmarshal(trimmed, &b); err != nil {
		return false, false
	}
	return b, true
}

func decodeLabel(raw json.RawMessage, path string, lineNo int, agg *issueLog) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, nullLiteral) {
		return ""
	}
	s, ok := decodeString(raw)
	if !ok {
		agg.add(path, ReasonUnsupportedType, lineNo)
		return ""
	}
	return sanitize(s, maxLabelLen)
}

func decodeOptionalString(raw json.RawMessage, path string, lineNo int, agg *issueLog) *string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, nullLiteral) {
		return nil
	}
	s, ok := decodeString(raw)
	if !ok {
		agg.add(path, ReasonUnsupportedType, lineNo)
		return nil
	}
	s = sanitize(s, maxLabelLen)
	if s == "" {
		return nil
	}
	return &s
}

// sanitize trims a label, drops control characters, and caps its length on a
// rune boundary. Labels are the only log-derived strings a Report may carry.
func sanitize(s string, limit int) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return s
}

// issueLog aggregates issues by field and reason so a corrupt log yields a
// short, deterministic list.
type issueLog struct {
	order []string
	byKey map[string]*Issue
}

func newIssueLog() *issueLog {
	return &issueLog{byKey: make(map[string]*Issue)}
}

func (l *issueLog) add(field string, reason Reason, line int) {
	key := field + "\x00" + string(reason)
	if iss, ok := l.byKey[key]; ok {
		iss.Count++
		return
	}
	l.byKey[key] = &Issue{Field: field, Reason: reason, Count: 1, Line: line}
	l.order = append(l.order, key)
}

func (l *issueLog) list() []Issue {
	out := make([]Issue, 0, len(l.order))
	for _, key := range l.order {
		out = append(out, *l.byKey[key])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Field != out[j].Field {
			return out[i].Field < out[j].Field
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}
