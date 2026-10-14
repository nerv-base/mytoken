package scan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

// Collection diagnostics record what the last scan of one harness instance
// actually did: when it ran, when it last read the instance cleanly, how many
// files the parser recognized, how many usage events it committed, how many
// sessions the index holds, and why a source yielded nothing. One sanitized
// record per instance is persisted once per completed scan under
// DiagnosticsSettingPrefix, and GUI and CLI read it back through
// LoadDiagnostics so both show the same evidence.
//
// A record never carries file contents, session text, credentials or raw parser
// errors. A source that fails is reduced to a reason code, and a scan that
// proved nothing about usage (an unchanged cursor, for instance) is unknown
// rather than an invented zero.
//
// An instance absent from the settings is an instance the scanner never
// scanned: disabled and backup-copy sources are configured, not collected, and
// stay absent here. Configuration diagnostics belong to the app that resolves
// the sources, not to this record.

// DiagnosticsSettingPrefix starts every per-instance diagnostics key; see
// DiagnosticsSettingKey for the full shape.
const DiagnosticsSettingPrefix = "source.diagnostics.v1."

// DiagnosticsVersion is the schema version written into every record and
// required by LoadDiagnostics.
const DiagnosticsVersion = 1

// maxDiagnosticsBytes bounds a persisted record. A record over the bound loses
// its per-file detail rather than being truncated mid-field, so a reader always
// parses a whole record.
const maxDiagnosticsBytes = 16384

// maxDiagnosticsSources bounds the per-file detail in a record. DiscoveredFiles
// and the instance reason still describe every source.
const maxDiagnosticsSources = 128

// ErrCorruptDiagnostics reports a stored record that is not a valid record for
// the requested harness and instance: unparsable JSON, another version, another
// instance, an unknown reason code, a negative count or an over-bound value.
// Callers treat it as no record.
var ErrCorruptDiagnostics = errors.New("scan: stored diagnostics are corrupt")

// ErrUnsupportedSource marks a source whose format or kind the parser cannot
// read, so diagnostics report "unsupported" instead of an opaque failure.
var ErrUnsupportedSource = errors.New("scan: unsupported source")

// DiagnosticsReason classifies why a scan or a source produced what it did. It
// is a closed set; a stored value outside it makes the record corrupt.
type DiagnosticsReason string

const (
	// ReasonOK is a source that produced usage events carrying token counts.
	ReasonOK DiagnosticsReason = "ok"
	// ReasonZeroUsage is a source that produced usage records whose token
	// fields are all zero: an observed zero, not missing data.
	ReasonZeroUsage DiagnosticsReason = "zero-usage"
	// ReasonNoTokenData is new content (the cursor advanced) that held no usage
	// events, so no token data is available for it.
	ReasonNoTokenData DiagnosticsReason = "no-token-data"
	// ReasonMissing is a recognized path that does not exist.
	ReasonMissing DiagnosticsReason = "missing"
	// ReasonPermission is a recognized path that exists but cannot be read.
	ReasonPermission DiagnosticsReason = "permission"
	// ReasonUnsupported is a source whose format or kind cannot be parsed.
	ReasonUnsupported DiagnosticsReason = "unsupported"
	// ReasonUnknown is an attempt that proved nothing. An unchanged cursor or
	// file is unknown, never zero usage: the parser did not re-measure usage,
	// so no claim about tokens may be made.
	ReasonUnknown DiagnosticsReason = "unknown"
	// ReasonNoSources is a completed discovery that recognized no source.
	ReasonNoSources DiagnosticsReason = "no-sources"
)

// diagnosticsReasons is the closed set of persisted reason codes.
var diagnosticsReasons = map[DiagnosticsReason]bool{
	ReasonOK:          true,
	ReasonZeroUsage:   true,
	ReasonNoTokenData: true,
	ReasonMissing:     true,
	ReasonPermission:  true,
	ReasonUnsupported: true,
	ReasonUnknown:     true,
	ReasonNoSources:   true,
}

// diagReasonPriority orders the instance-level reason: the most specific
// actionable finding wins, and unknown (nothing proven) ranks last so a proven
// finding is never hidden by a source that merely had no new data.
var diagReasonPriority = [...]DiagnosticsReason{
	ReasonMissing,
	ReasonPermission,
	ReasonUnsupported,
	ReasonNoTokenData,
	ReasonZeroUsage,
	ReasonOK,
	ReasonUnknown,
}

// Diagnostics is the persisted, sanitized collection state of one harness
// instance. Every field is exported so the JSON contract is the struct itself.
type Diagnostics struct {
	// Version is the schema version, DiagnosticsVersion for a record this
	// build wrote.
	Version int `json:"version"`
	// Harness is the parser id this record belongs to.
	Harness string `json:"harness"`
	// Instance is the instance id this record belongs to.
	Instance string `json:"instance"`
	// LastAttemptAt is when the most recent completed scan of this instance
	// finished, whether or not it succeeded. A cancelled scan writes nothing.
	LastAttemptAt time.Time `json:"lastAttemptAt"`
	// LastSuccessAt is when a completed scan last read the instance cleanly:
	// discovery succeeded, at least one source was recognized, no source
	// failed, and at least one source's outcome was proven rather than
	// unknown. A failed or inconclusive scan never advances it, so it stays
	// the prior successful evidence, and it is zero when nothing was ever
	// collected.
	LastSuccessAt time.Time `json:"lastSuccessAt,omitzero"`
	// Reason classifies the latest completed attempt at the instance level.
	Reason DiagnosticsReason `json:"reason"`
	// DiscoveredFiles is how many regular files the parser recognized, whether
	// or not they could be read.
	DiscoveredFiles int `json:"discoveredFiles"`
	// NewEvents is how many usage events the latest completed scan committed
	// for the first time. Events the index already held, including a replayed
	// or duplicated source, are not new, and a commit that failed contributes
	// nothing.
	NewEvents int64 `json:"newEvents"`
	// IndexedSessions is how many sessions the index holds for this instance.
	// It is omitted when the count cannot be read reliably, which is unknown,
	// not zero.
	IndexedSessions *int64 `json:"indexedSessions,omitempty"`
	// Sources is the per-source classification, sorted by path and bounded by
	// maxDiagnosticsSources.
	Sources []SourceDiagnostics `json:"sources,omitempty"`
	// SourcesTruncated counts sources left out of Sources.
	SourcesTruncated int `json:"sourcesTruncated,omitempty"`
}

// SourceDiagnostics is one discovered source's latest classification.
type SourceDiagnostics struct {
	// Path is the source path the parser discovered: a known scanned location,
	// never a path this package walked on its own.
	Path string `json:"path"`
	// Kind is the parser's source kind, such as "jsonl" or "sqlite".
	Kind string `json:"kind,omitempty"`
	// Reason classifies the latest attempt at this source.
	Reason DiagnosticsReason `json:"reason"`
	// Events is how many usage events the source produced in the latest
	// completed scan.
	Events int64 `json:"events,omitempty"`
	// Sessions is how many session records the source produced.
	Sessions int64 `json:"sessions,omitempty"`
}

// DiagnosticsSettings is the read side LoadDiagnostics needs. *store.Store
// satisfies it, and a read-only consumer only needs Setting.
type DiagnosticsSettings interface {
	Setting(ctx context.Context, key string) (string, error)
}

// DiagnosticsSettingKey returns the settings key holding one harness instance's
// diagnostics: "source.diagnostics.v1.<harness>.<instance>". Harness and
// instance are escaped separately, so a dot inside either can never be read as
// the separator between them, and an empty instance means the store's default
// instance.
func DiagnosticsSettingKey(h model.Harness, instance string) string {
	return DiagnosticsSettingPrefix + diagnosticsKeyPart(string(h)) + "." + diagnosticsKeyPart(store.NormalizeInstanceID(instance))
}

// diagnosticsKeyPart escapes "%" and "." so one key component cannot bleed into
// the next. Every real harness id and generated instance id is unaffected.
func diagnosticsKeyPart(s string) string {
	s = strings.ReplaceAll(s, "%", "%25")
	return strings.ReplaceAll(s, ".", "%2E")
}

// LoadDiagnostics returns the diagnostics persisted for one harness instance,
// with ok reporting whether a record exists. It reads and validates only; it
// never writes and never re-dates evidence, so a stale record comes back with
// the timestamps the scan that wrote it recorded. A nil settings interface
// reports no record. A stored value that is not a valid record for this harness
// and instance returns ErrCorruptDiagnostics, which callers treat as absent.
func LoadDiagnostics(ctx context.Context, settings DiagnosticsSettings, h model.Harness, instance string) (Diagnostics, bool, error) {
	if settings == nil {
		return Diagnostics{}, false, nil
	}
	raw, err := settings.Setting(ctx, DiagnosticsSettingKey(h, instance))
	if err != nil {
		return Diagnostics{}, false, err
	}
	return decodeDiagnostics(raw, h, instance)
}

func decodeDiagnostics(raw string, h model.Harness, instance string) (Diagnostics, bool, error) {
	if strings.TrimSpace(raw) == "" {
		return Diagnostics{}, false, nil
	}
	if len(raw) > maxDiagnosticsBytes {
		return Diagnostics{}, false, fmt.Errorf("%w: value holds %d bytes, over the %d byte bound", ErrCorruptDiagnostics, len(raw), maxDiagnosticsBytes)
	}
	var d Diagnostics
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return Diagnostics{}, false, fmt.Errorf("%w: %v", ErrCorruptDiagnostics, err)
	}
	if err := validateDiagnostics(d, h, instance); err != nil {
		return Diagnostics{}, false, err
	}
	return d, true, nil
}

func validateDiagnostics(d Diagnostics, h model.Harness, instance string) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrCorruptDiagnostics, fmt.Sprintf(format, args...))
	}
	if d.Version != DiagnosticsVersion {
		return bad("version %d, want %d", d.Version, DiagnosticsVersion)
	}
	if d.Harness != string(h) {
		return bad("harness %q, want %q", d.Harness, string(h))
	}
	if d.Instance != store.NormalizeInstanceID(instance) {
		return bad("instance %q, want %q", d.Instance, store.NormalizeInstanceID(instance))
	}
	if d.LastAttemptAt.IsZero() {
		return bad("no last attempt time")
	}
	if !diagnosticsReasons[d.Reason] {
		return bad("unknown reason %q", d.Reason)
	}
	if d.DiscoveredFiles < 0 || d.NewEvents < 0 || d.SourcesTruncated < 0 {
		return bad("negative count")
	}
	if d.IndexedSessions != nil && *d.IndexedSessions < 0 {
		return bad("negative indexed session count")
	}
	if len(d.Sources) > maxDiagnosticsSources {
		return bad("%d sources, over the %d bound", len(d.Sources), maxDiagnosticsSources)
	}
	for i := range d.Sources {
		if d.Sources[i].Path == "" {
			return bad("source %d has an empty path", i)
		}
		if !diagnosticsReasons[d.Sources[i].Reason] {
			return bad("source %d has unknown reason %q", i, d.Sources[i].Reason)
		}
		if d.Sources[i].Events < 0 || d.Sources[i].Sessions < 0 {
			return bad("source %d has a negative count", i)
		}
	}
	return nil
}

// diagnosticsReasonOf reduces a failure to a reason code. The raw error text is
// never stored, so an unrecognized failure is unknown rather than echoed.
func diagnosticsReasonOf(err error) DiagnosticsReason {
	switch {
	case err == nil:
		return ReasonOK
	case errors.Is(err, fs.ErrNotExist):
		return ReasonMissing
	case errors.Is(err, fs.ErrPermission):
		return ReasonPermission
	case errors.Is(err, ErrUnsupportedSource):
		return ReasonUnsupported
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return ReasonUnknown
	case strings.Contains(strings.ToLower(err.Error()), "unsupported"):
		// A parser that rejects a format it cannot read reports it in words;
		// only the classification survives, never the message.
		return ReasonUnsupported
	default:
		return ReasonUnknown
	}
}

// parseOutcome is what one Parse call proved about a source. It carries counts
// and flags, never content.
type parseOutcome struct {
	events     int64
	sessions   int64
	zeroTokens bool // events > 0 and every one carries zero tokens
	advanced   bool // the parser's next cursor differs from the one it was given
}

// diagSource accumulates one discovered source's evidence for the scan in
// flight.
type diagSource struct {
	path     string
	kind     string
	reason   DiagnosticsReason
	events   int64
	sessions int64
	failed   bool
}

// diagInstance accumulates one instance's evidence for the scan in flight.
type diagInstance struct {
	harness    model.Harness
	instance   string
	discovered int
	files      int
	failed     bool
	newEvents  int64
	sessions   int64
	sources    map[string]*diagSource
}

// diagKey identifies one instance's accumulator, keeping harness and instance
// separate the way every other per-source key in this package does.
func diagKey(h model.Harness, instance string) string {
	return string(h) + "\x00" + store.NormalizeInstanceID(instance)
}

// diagBegin resets the evidence for a new scan. It runs once per scan, under
// scanMu, before any source is discovered.
func (s *Scanner) diagBegin() {
	s.diagMu.Lock()
	s.diag = map[string]*diagInstance{}
	s.diagMu.Unlock()
}

// diagEntry returns the instance's accumulator, creating it. The caller holds
// diagMu.
func (s *Scanner) diagEntry(h model.Harness, instance string) *diagInstance {
	if s.diag == nil {
		return nil
	}
	k := diagKey(h, instance)
	ev := s.diag[k]
	if ev == nil {
		ev = &diagInstance{harness: h, instance: store.NormalizeInstanceID(instance), sources: map[string]*diagSource{}}
		s.diag[k] = ev
	}
	return ev
}

// diagSource returns the source's evidence, creating it. The caller holds
// diagMu.
func (s *Scanner) diagSource(h model.Harness, instance, path string) *diagSource {
	ev := s.diagEntry(h, instance)
	if ev == nil {
		return nil
	}
	src := ev.sources[path]
	if src == nil {
		src = &diagSource{path: path}
		ev.sources[path] = src
	}
	return src
}

// diagAttempt records that the scan attempted one instance, before anything is
// discovered. An instance the scanner scans and recognizes nothing in still
// gets a record; an instance the scanner never scans, such as a disabled or
// backup-copy source, gets none.
func (s *Scanner) diagAttempt(h model.Harness, instance string) {
	s.diagMu.Lock()
	defer s.diagMu.Unlock()
	s.diagEntry(h, instance)
}

// diagDiscover records a source the parser recognized. isDir reports a
// directory source, which is not counted as a recognized file; statErr is the
// scanner's own stat of the path, classified into the reason.
func (s *Scanner) diagDiscover(h model.Harness, instance string, src harness.Source, isDir bool, statErr error) {
	s.diagMu.Lock()
	defer s.diagMu.Unlock()
	ev := s.diagEntry(h, instance)
	if ev == nil {
		return
	}
	d, seen := ev.sources[src.Path], true
	if d == nil {
		seen = false
		d = &diagSource{path: src.Path}
		ev.sources[src.Path] = d
		ev.discovered++
		if !isDir {
			ev.files++
		}
	}
	if seen {
		// Discovery should not repeat a path; if it does, keep one entry and
		// do not count it twice.
		if src.Kind != "" {
			d.kind = src.Kind
		}
		return
	}
	d.kind = src.Kind
	switch {
	case statErr != nil:
		// A path that cannot even be stat'ed is still a recognized file, and
		// the failure is the evidence until a parse says otherwise.
		d.reason = diagnosticsReasonOf(statErr)
		d.failed = true
	default:
		// Nothing is known yet; the parse outcome decides.
		d.reason = ReasonUnknown
	}
}

// diagSkipped records a source a quick scan skipped because its stat had not
// moved. That is no proof about usage, so it is unknown, not zero.
func (s *Scanner) diagSkipped(h model.Harness, instance, path string) {
	s.diagMu.Lock()
	defer s.diagMu.Unlock()
	if d := s.diagSource(h, instance, path); d != nil {
		d.reason = ReasonUnknown
		d.failed = false
	}
}

// diagFailure records that discovery or the cursor read of an instance failed,
// so the instance cannot count as a clean read.
func (s *Scanner) diagFailure(h model.Harness, instance string) {
	s.diagMu.Lock()
	defer s.diagMu.Unlock()
	if ev := s.diagEntry(h, instance); ev != nil {
		ev.failed = true
	}
}

// diagParse records what one Parse call proved about a source.
func (s *Scanner) diagParse(h model.Harness, instance string, src harness.Source, out parseOutcome, parseErr error) {
	s.diagMu.Lock()
	defer s.diagMu.Unlock()
	d := s.diagSource(h, instance, src.Path)
	if d == nil {
		return
	}
	if d.kind == "" {
		d.kind = src.Kind
	}
	d.events, d.sessions = out.events, out.sessions
	switch {
	case parseErr != nil:
		d.reason = diagnosticsReasonOf(parseErr)
		d.failed = true
	case out.events == 0 && out.sessions == 0 && !out.advanced:
		// An empty batch with an unchanged cursor proves nothing about usage:
		// the parser did not re-measure it, so neither zero usage nor
		// no-token-data may be claimed.
		d.reason = ReasonUnknown
		d.failed = false
	case out.events == 0:
		// New content was read and held no usage events.
		d.reason = ReasonNoTokenData
		d.failed = false
	case out.zeroTokens:
		d.reason = ReasonZeroUsage
		d.failed = false
	default:
		d.reason = ReasonOK
		d.failed = false
	}
}

// diagCommitted records the events a successful commit actually made visible.
// It runs once per flush, never per event, and it reads the commit's own
// outcome: only rows the index did not already hold count, so a replayed,
// duplicated or re-parsed source cannot be recorded as new collection. A
// commit that failed reaches diagCommitFailed instead and contributes nothing.
func (s *Scanner) diagCommitted(writes []store.Write, outcome store.CommitOutcome) {
	s.diagMu.Lock()
	defer s.diagMu.Unlock()
	for i := range writes {
		ev := s.diagEntry(writes[i].Harness, writes[i].InstanceID)
		if ev == nil {
			continue
		}
		// The outcome is index-aligned with writes; a shorter one (no entry
		// point produces it) contributes no events rather than an invention.
		if i < len(outcome.Writes) {
			ev.newEvents += outcome.Writes[i].NewEvents
		}
		ev.sessions += int64(len(writes[i].Batch.Sessions))
	}
}

// diagCommitFailed records that a commit failed, so its instance is not a clean
// read and its events were not made visible.
func (s *Scanner) diagCommitFailed(writes []store.Write) {
	s.diagMu.Lock()
	defer s.diagMu.Unlock()
	for i := range writes {
		if ev := s.diagEntry(writes[i].Harness, writes[i].InstanceID); ev != nil {
			ev.failed = true
		}
	}
}

// diagSnapshot is a lock-free copy of one instance's scan evidence.
type diagSnapshot struct {
	harness    model.Harness
	instance   string
	discovered int
	files      int
	failed     bool
	newEvents  int64
	sessions   int64
	sources    []diagSourceSnapshot
}

type diagSourceSnapshot struct {
	path     string
	kind     string
	reason   DiagnosticsReason
	events   int64
	sessions int64
	failed   bool
}

// diagTake copies the scan's evidence and sorts it for a stable record.
func (s *Scanner) diagTake() []diagSnapshot {
	s.diagMu.Lock()
	defer s.diagMu.Unlock()
	out := make([]diagSnapshot, 0, len(s.diag))
	for _, ev := range s.diag {
		snap := diagSnapshot{harness: ev.harness, instance: ev.instance, discovered: ev.discovered, files: ev.files, failed: ev.failed, newEvents: ev.newEvents, sessions: ev.sessions}
		snap.sources = make([]diagSourceSnapshot, 0, len(ev.sources))
		for _, src := range ev.sources {
			snap.sources = append(snap.sources, diagSourceSnapshot{path: src.path, kind: src.kind, reason: src.reason, events: src.events, sessions: src.sessions, failed: src.failed})
		}
		sort.Slice(snap.sources, func(i, j int) bool { return snap.sources[i].path < snap.sources[j].path })
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].harness != out[j].harness {
			return out[i].harness < out[j].harness
		}
		return out[i].instance < out[j].instance
	})
	return out
}

// clean reports a completed instance read: discovery recognized at least one
// source, nothing failed, and at least one source's outcome was proven. A scan
// whose sources were all skipped or inconclusive measured nothing, so it
// advances no success time and leaves the prior evidence standing.
func (ev diagSnapshot) clean() bool {
	if ev.failed || ev.discovered == 0 {
		return false
	}
	proven := false
	for i := range ev.sources {
		if ev.sources[i].failed {
			return false
		}
		switch ev.sources[i].reason {
		case ReasonOK, ReasonZeroUsage, ReasonNoTokenData:
			proven = true
		}
	}
	return proven
}

// summaryReason is the instance-level reason: the most specific finding wins,
// and unknown ranks last so a proven finding is never hidden.
func summaryReason(ev diagSnapshot) DiagnosticsReason {
	if len(ev.sources) == 0 {
		if ev.failed {
			return ReasonUnknown
		}
		return ReasonNoSources
	}
	for _, want := range diagReasonPriority {
		for i := range ev.sources {
			if ev.sources[i].reason == want {
				return want
			}
		}
	}
	return ReasonUnknown
}

// boundedSources is the persisted per-source detail, capped and counted.
func (ev diagSnapshot) boundedSources() ([]SourceDiagnostics, int) {
	total := len(ev.sources)
	kept := ev.sources
	truncated := 0
	if total > maxDiagnosticsSources {
		kept = ev.sources[:maxDiagnosticsSources]
		truncated = total - maxDiagnosticsSources
	}
	out := make([]SourceDiagnostics, 0, len(kept))
	for i := range kept {
		out = append(out, SourceDiagnostics{Path: kept[i].path, Kind: kept[i].kind, Reason: kept[i].reason, Events: kept[i].events, Sessions: kept[i].sessions})
	}
	return out, truncated
}

// buildDiagnostics turns the scan's evidence and the prior record into the next
// record. Prior success is carried forward unless this scan read the instance
// cleanly.
func buildDiagnostics(ev diagSnapshot, prev Diagnostics, prevOK bool, at time.Time, sessions *int64) Diagnostics {
	d := Diagnostics{
		Version:         DiagnosticsVersion,
		Harness:         string(ev.harness),
		Instance:        store.NormalizeInstanceID(ev.instance),
		LastAttemptAt:   at.UTC(),
		Reason:          summaryReason(ev),
		DiscoveredFiles: ev.files,
		NewEvents:       ev.newEvents,
		IndexedSessions: sessions,
	}
	switch {
	case ev.clean():
		d.LastSuccessAt = at.UTC()
	case prevOK:
		d.LastSuccessAt = prev.LastSuccessAt
	}
	d.Sources, d.SourcesTruncated = ev.boundedSources()
	return d
}

// marshalDiagnostics renders a record within maxDiagnosticsBytes. Per-file
// detail is dropped, and counted, before the bound can truncate a record: a
// reader always parses a whole record, and every source stays accounted for.
func marshalDiagnostics(d *Diagnostics) (string, error) {
	// A record this package writes always loads back: the per-source cap is
	// applied here as well as while building, so a caller cannot persist a
	// record its own reader would reject as corrupt.
	if len(d.Sources) > maxDiagnosticsSources {
		d.SourcesTruncated += len(d.Sources) - maxDiagnosticsSources
		d.Sources = d.Sources[:maxDiagnosticsSources]
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	for len(raw) > maxDiagnosticsBytes && len(d.Sources) > 0 {
		keep := len(d.Sources) / 2
		d.SourcesTruncated += len(d.Sources) - keep
		d.Sources = d.Sources[:keep]
		if raw, err = json.Marshal(d); err != nil {
			return "", err
		}
	}
	if len(raw) > maxDiagnosticsBytes {
		return "", fmt.Errorf("scan: diagnostics record is %d bytes, over the %d byte bound", len(raw), maxDiagnosticsBytes)
	}
	return string(raw), nil
}

// indexedSessionsQuery counts the sessions the index holds for one instance.
const indexedSessionsQuery = `SELECT count(*) FROM sessions WHERE harness = ? AND instance_id = ?`

// indexedSessions counts the indexed sessions for one instance. A count that
// cannot be read reliably, including an index that predates the instance
// dimension, stays absent: unknown, never zero.
func (s *Scanner) indexedSessions(ctx context.Context, h model.Harness, instance string) *int64 {
	if s.st == nil {
		return nil
	}
	var n int64
	if err := s.st.DB().QueryRowContext(ctx, indexedSessionsQuery, string(h), store.NormalizeInstanceID(instance)).Scan(&n); err != nil || n < 0 {
		return nil
	}
	return &n
}

// persistDiagnostics writes one bounded record per instance this scan touched.
// It runs once per completed scan, after every source and commit is accounted
// for, and never notifies or re-scans. A corrupt prior record is replaced, not
// trusted and not echoed.
func (s *Scanner) persistDiagnostics(ctx context.Context, at time.Time) error {
	if s.st == nil {
		return nil
	}
	var errs []error
	for _, ev := range s.diagTake() {
		key := DiagnosticsSettingKey(ev.harness, ev.instance)
		prev, prevOK, err := LoadDiagnostics(ctx, s.st, ev.harness, ev.instance)
		if err != nil {
			if !errors.Is(err, ErrCorruptDiagnostics) {
				errs = append(errs, fmt.Errorf("read diagnostics %s: %w", key, err))
				continue
			}
			prevOK = false
		}
		d := buildDiagnostics(ev, prev, prevOK, at, s.indexedSessions(ctx, ev.harness, ev.instance))
		blob, err := marshalDiagnostics(&d)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := s.st.SetSetting(ctx, key, blob); err != nil {
			errs = append(errs, fmt.Errorf("persist diagnostics %s: %w", key, err))
		}
	}
	return errors.Join(errs...)
}
