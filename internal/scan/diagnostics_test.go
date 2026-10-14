package scan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

// diagParser is a scripted parser: the test decides what is discovered and what
// one parse proves, so diagnostics can be exercised without real harness files.
type diagParser struct {
	harness  model.Harness
	roots    []string
	discover func(ctx context.Context) ([]harness.Source, error)
	parse    func(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error)
}

func (p diagParser) Harness() model.Harness { return p.harness }
func (p diagParser) Roots() []string        { return p.roots }
func (p diagParser) Discover(ctx context.Context) ([]harness.Source, error) {
	if p.discover == nil {
		return nil, nil
	}
	return p.discover(ctx)
}
func (p diagParser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if p.parse == nil {
		return harness.Batch{}, nil
	}
	return p.parse(ctx, src, cur)
}

// oneFileParser discovers exactly path and emits one token-carrying event per
// record it has not read yet, mirroring a parser that advances its cursor.
func oneFileParser(h model.Harness, path string, tokens model.Tokens) diagParser {
	return diagParser{
		harness: h,
		roots:   []string{filepath.Dir(path)},
		discover: func(context.Context) ([]harness.Source, error) {
			return []harness.Source{{Path: path, Kind: "jsonl"}}, nil
		},
		parse: func(_ context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
			raw, err := os.ReadFile(src.Path)
			if err != nil {
				return harness.Batch{}, err
			}
			b := harness.Batch{Next: harness.Cursor{Offset: int64(len(raw)), Size: int64(len(raw))}}
			if cur.Offset < int64(len(raw)) {
				b.Events = append(b.Events, model.UsageEvent{
					DedupKey:  src.Path,
					SessionID: "session",
					Model:     "gpt-5",
					Timestamp: time.Unix(1700000000, 0),
					Tokens:    tokens,
				})
			}
			return b, nil
		},
	}
}

func writeLog(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("record\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadDiag(t *testing.T, st *store.Store, h model.Harness, instance string) (Diagnostics, bool) {
	t.Helper()
	d, ok, err := LoadDiagnostics(context.Background(), st, h, instance)
	if err != nil {
		t.Fatalf("LoadDiagnostics(%s, %s): %v", h, instance, err)
	}
	return d, ok
}

func rawDiag(t *testing.T, st *store.Store, h model.Harness, instance string) string {
	t.Helper()
	raw, err := st.Setting(context.Background(), DiagnosticsSettingKey(h, instance))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDiagnosticsSettingKeySeparatesHarnessAndInstance(t *testing.T) {
	cases := []struct {
		harness  model.Harness
		instance string
		want     string
	}{
		{model.Harness("codex"), "inst_1", "source.diagnostics.v1.codex.inst_1"},
		{model.Harness("codex"), "", "source.diagnostics.v1.codex.default"},
		{model.Harness("a.b"), "c.d", "source.diagnostics.v1.a%2Eb.c%2Ed"},
		{model.Harness("a%2Eb"), "c", "source.diagnostics.v1.a%252Eb.c"},
	}
	for _, c := range cases {
		if got := DiagnosticsSettingKey(c.harness, c.instance); got != c.want {
			t.Errorf("DiagnosticsSettingKey(%q, %q) = %q, want %q", c.harness, c.instance, got, c.want)
		}
	}
}

func TestDiagnosticsRecordsSuccessfulScanThenUnchangedRescan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	writeLog(t, path)
	st := openStore(t)
	defer st.Close()

	t0 := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	sc := New(st, nil, nil, oneFileParser(model.Harness("fixture"), path, model.Tokens{Input: 3}))
	sc.clock = func() time.Time { return t0 }
	ctx := context.Background()
	if err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}

	d, ok := loadDiag(t, st, "fixture", store.DefaultInstanceID)
	if !ok {
		t.Fatal("no diagnostics persisted for a completed scan")
	}
	if d.Version != DiagnosticsVersion || d.Harness != "fixture" || d.Instance != store.DefaultInstanceID {
		t.Fatalf("identity = %+v", d)
	}
	if d.Reason != ReasonOK {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonOK)
	}
	if d.DiscoveredFiles != 1 || d.NewEvents != 1 {
		t.Fatalf("files=%d events=%d, want 1 and 1", d.DiscoveredFiles, d.NewEvents)
	}
	if !d.LastAttemptAt.Equal(t0) || !d.LastSuccessAt.Equal(t0) {
		t.Fatalf("attempt=%v success=%v, want both %v", d.LastAttemptAt, d.LastSuccessAt, t0)
	}
	if d.IndexedSessions == nil || *d.IndexedSessions != 1 {
		t.Fatalf("indexedSessions = %v, want 1", d.IndexedSessions)
	}
	if len(d.Sources) != 1 {
		t.Fatalf("sources = %+v", d.Sources)
	}
	if s := d.Sources[0]; s.Path != path || s.Kind != "jsonl" || s.Reason != ReasonOK || s.Events != 1 || s.Sessions != 0 {
		t.Fatalf("source = %+v", s)
	}

	// An unchanged quick rescan re-measures nothing, so it may not report zero
	// usage, and it must not move the last successful scan.
	t1 := t0.Add(time.Hour)
	sc.clock = func() time.Time { return t1 }
	if err := sc.scan(ctx, true); err != nil {
		t.Fatal(err)
	}
	d2, ok := loadDiag(t, st, "fixture", store.DefaultInstanceID)
	if !ok {
		t.Fatal("record disappeared after an unchanged rescan")
	}
	if d2.Reason != ReasonUnknown {
		t.Fatalf("reason = %q, want %q: an unchanged cursor proves nothing", d2.Reason, ReasonUnknown)
	}
	if d2.NewEvents != 0 {
		t.Fatalf("newEvents = %d, want 0", d2.NewEvents)
	}
	if !d2.LastAttemptAt.Equal(t1) {
		t.Fatalf("attempt = %v, want %v", d2.LastAttemptAt, t1)
	}
	if !d2.LastSuccessAt.Equal(t0) {
		t.Fatalf("success = %v, want the prior %v", d2.LastSuccessAt, t0)
	}
	if d2.Sources[0].Reason != ReasonUnknown {
		t.Fatalf("source reason = %q, want %q", d2.Sources[0].Reason, ReasonUnknown)
	}

	// A third quick rescan skips the file entirely, which is still unknown.
	t2 := t0.Add(2 * time.Hour)
	sc.clock = func() time.Time { return t2 }
	if err := sc.scan(ctx, true); err != nil {
		t.Fatal(err)
	}
	d3, _ := loadDiag(t, st, "fixture", store.DefaultInstanceID)
	if d3.Reason != ReasonUnknown || !d3.LastSuccessAt.Equal(t0) {
		t.Fatalf("skipped rescan record = %+v", d3)
	}
	if !d3.LastAttemptAt.Equal(t2) {
		t.Fatalf("attempt = %v, want %v", d3.LastAttemptAt, t2)
	}
}

func TestDiagnosticsParseFailureDoesNotAdvanceLastSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	writeLog(t, path)
	st := openStore(t)
	defer st.Close()

	var fail error
	p := oneFileParser(model.Harness("fixture"), path, model.Tokens{Input: 3})
	okParse := p.parse
	p.parse = func(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
		if fail != nil {
			return harness.Batch{}, fail
		}
		return okParse(ctx, src, cur)
	}

	t0 := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	sc := New(st, nil, nil, p)
	sc.clock = func() time.Time { return t0 }
	ctx := context.Background()
	if err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	before, _ := loadDiag(t, st, "fixture", store.DefaultInstanceID)

	fail = errors.New("fixture parser: record 7 is malformed")
	t1 := t0.Add(time.Hour)
	sc.clock = func() time.Time { return t1 }
	if err := sc.Scan(ctx); err == nil {
		t.Fatal("a failing scan did not report an error")
	}

	after, ok := loadDiag(t, st, "fixture", store.DefaultInstanceID)
	if !ok {
		t.Fatal("record lost after a failed scan")
	}
	if after.Reason != ReasonUnknown {
		t.Fatalf("reason = %q, want %q", after.Reason, ReasonUnknown)
	}
	if !after.LastSuccessAt.Equal(before.LastSuccessAt) {
		t.Fatalf("success moved from %v to %v across a failed scan", before.LastSuccessAt, after.LastSuccessAt)
	}
	if !after.LastAttemptAt.Equal(t1) {
		t.Fatalf("attempt = %v, want %v", after.LastAttemptAt, t1)
	}
	if after.NewEvents != 0 {
		t.Fatalf("newEvents = %d, want 0", after.NewEvents)
	}
	if len(after.Sources) != 1 || after.Sources[0].Reason != ReasonUnknown {
		t.Fatalf("sources = %+v", after.Sources)
	}
}

func TestDiagnosticsMissingSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gone")
	st := openStore(t)
	defer st.Close()

	sc := New(st, nil, nil, oneFileParser(model.Harness("fixture"), path, model.Tokens{Input: 1}))
	sc.clock = func() time.Time { return time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC) }
	if err := sc.Scan(context.Background()); err == nil {
		t.Fatal("a missing source did not report an error")
	}
	d, ok := loadDiag(t, st, "fixture", store.DefaultInstanceID)
	if !ok {
		t.Fatal("no record for a scan that recognized a missing source")
	}
	if d.Reason != ReasonMissing {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonMissing)
	}
	if !d.LastSuccessAt.IsZero() {
		t.Fatalf("success = %v, want zero: nothing was ever collected", d.LastSuccessAt)
	}
	if d.DiscoveredFiles != 1 || len(d.Sources) != 1 || d.Sources[0].Reason != ReasonMissing {
		t.Fatalf("record = %+v", d)
	}
}

func TestDiagnosticsPermissionDeniedSource(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	writeLog(t, path)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o600) })
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("the platform does not deny the read")
	}

	st := openStore(t)
	defer st.Close()
	sc := New(st, nil, nil, oneFileParser(model.Harness("fixture"), path, model.Tokens{Input: 1}))
	sc.clock = func() time.Time { return time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC) }
	if err := sc.Scan(context.Background()); err == nil {
		t.Fatal("an unreadable source did not report an error")
	}
	d, _ := loadDiag(t, st, "fixture", store.DefaultInstanceID)
	if d.Reason != ReasonPermission {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonPermission)
	}
	if len(d.Sources) != 1 || d.Sources[0].Reason != ReasonPermission {
		t.Fatalf("sources = %+v", d.Sources)
	}
	if !d.LastSuccessAt.IsZero() {
		t.Fatalf("success = %v, want zero", d.LastSuccessAt)
	}
}

func TestDiagnosticsUnsupportedSource(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"sentinel", fmt.Errorf("read shape: %w", ErrUnsupportedSource)},
		{"text", errors.New("unsupported format: not a usage log")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "log")
			writeLog(t, path)
			st := openStore(t)
			defer st.Close()

			p := oneFileParser(model.Harness("fixture"), path, model.Tokens{Input: 1})
			p.parse = func(context.Context, harness.Source, harness.Cursor) (harness.Batch, error) {
				return harness.Batch{}, c.err
			}
			sc := New(st, nil, nil, p)
			sc.clock = func() time.Time { return time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC) }
			if err := sc.Scan(context.Background()); err == nil {
				t.Fatal("an unsupported source did not report an error")
			}
			d, _ := loadDiag(t, st, "fixture", store.DefaultInstanceID)
			if d.Reason != ReasonUnsupported {
				t.Fatalf("reason = %q, want %q", d.Reason, ReasonUnsupported)
			}
		})
	}
}

func TestDiagnosticsZeroUsageIsNotMissingTokenData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	writeLog(t, path)
	st := openStore(t)
	defer st.Close()

	// Records exist and carry all-zero token fields: an observed zero, not an
	// absence of token data.
	sc := New(st, nil, nil, oneFileParser(model.Harness("fixture"), path, model.Tokens{}))
	sc.clock = func() time.Time { return time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC) }
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, _ := loadDiag(t, st, "fixture", store.DefaultInstanceID)
	if d.Reason != ReasonZeroUsage {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonZeroUsage)
	}
	if d.NewEvents != 1 {
		t.Fatalf("newEvents = %d, want 1", d.NewEvents)
	}
}

func TestDiagnosticsNoTokenDataWhenCursorAdvancedWithoutEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	writeLog(t, path)
	st := openStore(t)
	defer st.Close()

	// The cursor advances over new content but the parser produced no usage
	// events for it: no token data, which is not the same as zero usage.
	p := oneFileParser(model.Harness("fixture"), path, model.Tokens{})
	p.parse = func(_ context.Context, src harness.Source, _ harness.Cursor) (harness.Batch, error) {
		raw, err := os.ReadFile(src.Path)
		if err != nil {
			return harness.Batch{}, err
		}
		return harness.Batch{Next: harness.Cursor{Offset: int64(len(raw)), Size: int64(len(raw))}}, nil
	}
	sc := New(st, nil, nil, p)
	sc.clock = func() time.Time { return time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC) }
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, _ := loadDiag(t, st, "fixture", store.DefaultInstanceID)
	if d.Reason != ReasonNoTokenData {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonNoTokenData)
	}
	if d.NewEvents != 0 {
		t.Fatalf("newEvents = %d, want 0", d.NewEvents)
	}
	if d.Sources[0].Reason != ReasonNoTokenData {
		t.Fatalf("source reason = %q, want %q", d.Sources[0].Reason, ReasonNoTokenData)
	}
}

func TestDiagnosticsNoSourcesIsAbsenceNotSuccess(t *testing.T) {
	st := openStore(t)
	defer st.Close()
	p := diagParser{harness: model.Harness("fixture")}
	sc := New(st, nil, nil, p)
	sc.clock = func() time.Time { return time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC) }
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, ok := loadDiag(t, st, "fixture", store.DefaultInstanceID)
	if !ok {
		t.Fatal("no record for a completed scan that recognized nothing")
	}
	if d.Reason != ReasonNoSources {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonNoSources)
	}
	if d.DiscoveredFiles != 0 || d.NewEvents != 0 {
		t.Fatalf("record = %+v", d)
	}
	if !d.LastSuccessAt.IsZero() {
		t.Fatalf("success = %v, want zero: nothing was collected", d.LastSuccessAt)
	}
}

func TestDiagnosticsMultipleInstancesKeepSeparateRecords(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	writeLog(t, a)
	writeLog(t, b)
	st := openStore(t)
	defer st.Close()

	sc, err := NewWithInstances(st, nil, nil,
		Instance{ID: "one", Parser: oneFileParser(model.Harness("fixture"), a, model.Tokens{Input: 1})},
		Instance{ID: "two", Parser: oneFileParser(model.Harness("fixture"), b, model.Tokens{})},
	)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	sc.clock = func() time.Time { return at }
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	one, ok := loadDiag(t, st, "fixture", "one")
	if !ok {
		t.Fatal("no record for instance one")
	}
	two, ok := loadDiag(t, st, "fixture", "two")
	if !ok {
		t.Fatal("no record for instance two")
	}
	if one.Instance != "one" || two.Instance != "two" {
		t.Fatalf("instances = %q and %q", one.Instance, two.Instance)
	}
	if one.Reason != ReasonOK || two.Reason != ReasonZeroUsage {
		t.Fatalf("reasons = %q and %q", one.Reason, two.Reason)
	}
	if one.NewEvents != 1 || two.NewEvents != 1 {
		t.Fatalf("events = %d and %d", one.NewEvents, two.NewEvents)
	}
	if len(one.Sources) != 1 || one.Sources[0].Path != a || len(two.Sources) != 1 || two.Sources[0].Path != b {
		t.Fatalf("sources = %+v and %+v", one.Sources, two.Sources)
	}
	if DiagnosticsSettingKey("fixture", "one") == DiagnosticsSettingKey("fixture", "two") {
		t.Fatal("two instances share one settings key")
	}
}

func TestDiagnosticsCancelledScanKeepsPriorRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	writeLog(t, path)
	st := openStore(t)
	defer st.Close()

	entered := make(chan struct{})
	var block bool
	p := oneFileParser(model.Harness("fixture"), path, model.Tokens{Input: 3})
	okParse := p.parse
	p.parse = func(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
		if block {
			close(entered)
			<-ctx.Done()
			return harness.Batch{}, ctx.Err()
		}
		return okParse(ctx, src, cur)
	}

	t0 := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	sc := New(st, nil, nil, p)
	sc.clock = func() time.Time { return t0 }
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := rawDiag(t, st, "fixture", store.DefaultInstanceID)

	block = true
	t1 := t0.Add(time.Hour)
	sc.clock = func() time.Time { return t1 }
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-entered
		cancel()
	}()
	if err := sc.Scan(ctx); err == nil {
		t.Fatal("a cancelled scan did not report an error")
	}
	if after := rawDiag(t, st, "fixture", store.DefaultInstanceID); after != before {
		t.Fatalf("a cancelled scan rewrote the record:\n before %s\n after  %s", before, after)
	}
}

func TestLoadDiagnosticsReturnsStoredEvidenceUnchanged(t *testing.T) {
	st := openStore(t)
	defer st.Close()

	attempt := time.Date(2024, 4, 1, 8, 30, 0, 0, time.UTC)
	success := time.Date(2024, 3, 20, 7, 0, 0, 0, time.UTC)
	sessions := int64(12)
	stored := Diagnostics{
		Version:         DiagnosticsVersion,
		Harness:         "codex",
		Instance:        "inst_x",
		LastAttemptAt:   attempt,
		LastSuccessAt:   success,
		Reason:          ReasonUnknown,
		DiscoveredFiles: 4,
		NewEvents:       0,
		IndexedSessions: &sessions,
		Sources:         []SourceDiagnostics{{Path: "/tmp/known/session.jsonl", Kind: "jsonl", Reason: ReasonUnknown}},
	}
	blob, err := marshalDiagnostics(&stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(context.Background(), DiagnosticsSettingKey("codex", "inst_x"), blob); err != nil {
		t.Fatal(err)
	}

	got, ok, err := LoadDiagnostics(context.Background(), st, "codex", "inst_x")
	if err != nil || !ok {
		t.Fatalf("LoadDiagnostics = %v, %v", ok, err)
	}
	if !got.LastAttemptAt.Equal(attempt) || !got.LastSuccessAt.Equal(success) {
		t.Fatalf("timestamps = %v, %v", got.LastAttemptAt, got.LastSuccessAt)
	}
	if got.Reason != ReasonUnknown || got.DiscoveredFiles != 4 || got.NewEvents != 0 {
		t.Fatalf("record = %+v", got)
	}
	if got.IndexedSessions == nil || *got.IndexedSessions != 12 {
		t.Fatalf("indexedSessions = %v", got.IndexedSessions)
	}
	if len(got.Sources) != 1 || got.Sources[0].Path != "/tmp/known/session.jsonl" {
		t.Fatalf("sources = %+v", got.Sources)
	}
}

func TestLoadDiagnosticsReportsMissingAndCorruptRecords(t *testing.T) {
	st := openStore(t)
	defer st.Close()
	ctx := context.Background()
	valid := Diagnostics{
		Version:       DiagnosticsVersion,
		Harness:       "fixture",
		Instance:      store.DefaultInstanceID,
		LastAttemptAt: time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC),
		Reason:        ReasonOK,
	}
	set := func(t *testing.T, value string) {
		t.Helper()
		if err := st.SetSetting(ctx, DiagnosticsSettingKey("fixture", store.DefaultInstanceID), value); err != nil {
			t.Fatal(err)
		}
	}

	// Absent and blank values are no record, not an error.
	if _, ok, err := LoadDiagnostics(ctx, st, "fixture", store.DefaultInstanceID); ok || err != nil {
		t.Fatalf("absent record = ok %v err %v", ok, err)
	}
	if _, ok, err := LoadDiagnostics(ctx, nil, "fixture", store.DefaultInstanceID); ok || err != nil {
		t.Fatalf("nil settings = ok %v err %v", ok, err)
	}
	set(t, "   ")
	if _, ok, err := LoadDiagnostics(ctx, st, "fixture", store.DefaultInstanceID); ok || err != nil {
		t.Fatalf("blank record = ok %v err %v", ok, err)
	}
	// Another harness reads its own key, which holds nothing here: absence for
	// that harness, never a record borrowed from this one.
	if _, ok, err := LoadDiagnostics(ctx, st, "codex", store.DefaultInstanceID); ok || err != nil {
		t.Fatalf("other harness = ok %v err %v", ok, err)
	}

	bad := map[string]string{
		"garbage":           "{not json",
		"empty object":      "{}",
		"wrong version":     `{"version":2,"harness":"fixture","instance":"default","lastAttemptAt":"2024-05-01T12:00:00Z","reason":"ok"}`,
		"wrong harness":     `{"version":1,"harness":"codex","instance":"default","lastAttemptAt":"2024-05-01T12:00:00Z","reason":"ok"}`,
		"wrong instance":    `{"version":1,"harness":"fixture","instance":"other","lastAttemptAt":"2024-05-01T12:00:00Z","reason":"ok"}`,
		"unknown reason":    `{"version":1,"harness":"fixture","instance":"default","lastAttemptAt":"2024-05-01T12:00:00Z","reason":"made-up"}`,
		"no attempt time":   `{"version":1,"harness":"fixture","instance":"default","reason":"ok"}`,
		"negative count":    `{"version":1,"harness":"fixture","instance":"default","lastAttemptAt":"2024-05-01T12:00:00Z","reason":"ok","newEvents":-1}`,
		"negative sessions": `{"version":1,"harness":"fixture","instance":"default","lastAttemptAt":"2024-05-01T12:00:00Z","reason":"ok","indexedSessions":-2}`,
		"empty source path": `{"version":1,"harness":"fixture","instance":"default","lastAttemptAt":"2024-05-01T12:00:00Z","reason":"ok","sources":[{"path":"","reason":"ok"}]}`,
		"source bad reason": `{"version":1,"harness":"fixture","instance":"default","lastAttemptAt":"2024-05-01T12:00:00Z","reason":"ok","sources":[{"path":"/x","reason":"nope"}]}`,
		"over bound":        strings.Repeat("x", maxDiagnosticsBytes+1),
	}
	for name, value := range bad {
		t.Run(name, func(t *testing.T) {
			set(t, value)
			if _, ok, err := LoadDiagnostics(ctx, st, "fixture", store.DefaultInstanceID); ok || !errors.Is(err, ErrCorruptDiagnostics) {
				t.Fatalf("ok = %v, err = %v, want ErrCorruptDiagnostics", ok, err)
			}
		})
	}

	// The bound is enforced on the marshalled record too, by dropping detail.
	many := make([]SourceDiagnostics, maxDiagnosticsSources+5)
	for i := range many {
		many[i] = SourceDiagnostics{Path: fmt.Sprintf("/known/location/claude/project/session-%04d.jsonl", i), Kind: "jsonl", Reason: ReasonOK, Events: 1}
	}
	valid.Sources = many
	blob, err := marshalDiagnostics(&valid)
	if err != nil {
		t.Fatal(err)
	}
	set(t, blob)
	got, ok, err := LoadDiagnostics(ctx, st, "fixture", store.DefaultInstanceID)
	if err != nil || !ok {
		t.Fatalf("bounded record = ok %v err %v", ok, err)
	}
	if len(got.Sources) != maxDiagnosticsSources || got.SourcesTruncated != 5 {
		t.Fatalf("sources=%d truncated=%d", len(got.Sources), got.SourcesTruncated)
	}
}

func TestDiagnosticsBoundsPersistedSources(t *testing.T) {
	dir := t.TempDir()
	st := openStore(t)
	defer st.Close()

	const total = 200
	srcs := make([]harness.Source, 0, total)
	for i := 0; i < total; i++ {
		srcs = append(srcs, harness.Source{Path: fmt.Sprintf("/known/location/session-%04d.jsonl", i), Kind: "jsonl"})
	}
	p := diagParser{
		harness: model.Harness("fixture"),
		roots:   []string{dir},
		discover: func(context.Context) ([]harness.Source, error) {
			return srcs, nil
		},
	}
	sc := New(st, nil, nil, p)
	sc.clock = func() time.Time { return time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC) }
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, ok := loadDiag(t, st, "fixture", store.DefaultInstanceID)
	if !ok {
		t.Fatal("no record")
	}
	if d.DiscoveredFiles != total {
		t.Fatalf("discoveredFiles = %d, want %d", d.DiscoveredFiles, total)
	}
	if len(d.Sources) > maxDiagnosticsSources {
		t.Fatalf("sources = %d, over the %d bound", len(d.Sources), maxDiagnosticsSources)
	}
	// Every discovered source is accounted for: either kept as detail or
	// counted as truncated, never silently dropped.
	if len(d.Sources)+d.SourcesTruncated != total {
		t.Fatalf("sources=%d truncated=%d, want %d accounted for", len(d.Sources), d.SourcesTruncated, total)
	}
	if len(d.Sources) != maxDiagnosticsSources || d.SourcesTruncated != total-maxDiagnosticsSources {
		t.Fatalf("sources=%d truncated=%d, want %d and %d", len(d.Sources), d.SourcesTruncated, maxDiagnosticsSources, total-maxDiagnosticsSources)
	}
	if raw := rawDiag(t, st, "fixture", store.DefaultInstanceID); len(raw) > maxDiagnosticsBytes {
		t.Fatalf("persisted %d bytes, over the %d byte bound", len(raw), maxDiagnosticsBytes)
	}
}

func TestDiagnosticsNeverPersistsParserErrorText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	writeLog(t, path)
	st := openStore(t)
	defer st.Close()

	const secret = "SESSION-BODY-SENTINEL-9f3a"
	p := oneFileParser(model.Harness("fixture"), path, model.Tokens{Input: 1})
	p.parse = func(context.Context, harness.Source, harness.Cursor) (harness.Batch, error) {
		return harness.Batch{}, fmt.Errorf("line 12 carries content %q and cannot be decoded", secret)
	}
	sc := New(st, nil, nil, p)
	sc.clock = func() time.Time { return time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC) }
	if err := sc.Scan(context.Background()); err == nil {
		t.Fatal("a failing parse did not report an error")
	}

	raw := rawDiag(t, st, "fixture", store.DefaultInstanceID)
	for _, leak := range []string{secret, "cannot be decoded", "line 12"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("persisted record leaks parser error text %q: %s", leak, raw)
		}
	}
	d, ok := loadDiag(t, st, "fixture", store.DefaultInstanceID)
	if !ok {
		t.Fatal("no record")
	}
	if d.Reason != ReasonUnknown {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonUnknown)
	}
	if d.Sources[0].Reason != ReasonUnknown {
		t.Fatalf("source reason = %q, want %q", d.Sources[0].Reason, ReasonUnknown)
	}
}

func TestDiagnosticsClassifiesFailuresWithoutStoringText(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want DiagnosticsReason
	}{
		{"missing", fmt.Errorf("open: %w", fs.ErrNotExist), ReasonMissing},
		{"permission", fmt.Errorf("open: %w", fs.ErrPermission), ReasonPermission},
		{"unsupported sentinel", ErrUnsupportedSource, ReasonUnsupported},
		{"unsupported text", errors.New("this source kind is Unsupported"), ReasonUnsupported},
		{"cancel", context.Canceled, ReasonUnknown},
		{"unknown", errors.New("checksum mismatch at record 4"), ReasonUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := diagnosticsReasonOf(c.err); got != c.want {
				t.Fatalf("diagnosticsReasonOf(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

func TestDiagnosticsInstanceSummaryPrefersProvenFailure(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok")
	bad := filepath.Join(dir, "bad")
	writeLog(t, ok)
	writeLog(t, bad)
	st := openStore(t)
	defer st.Close()

	// One healthy source and one failing source: the instance reports the
	// failure, never a comfortable average.
	p := diagParser{
		harness: model.Harness("fixture"),
		roots:   []string{dir},
		discover: func(context.Context) ([]harness.Source, error) {
			return []harness.Source{{Path: ok, Kind: "jsonl"}, {Path: bad, Kind: "jsonl"}}, nil
		},
		parse: func(_ context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
			if src.Path == bad {
				return harness.Batch{}, fmt.Errorf("read: %w", fs.ErrPermission)
			}
			raw, err := os.ReadFile(src.Path)
			if err != nil {
				return harness.Batch{}, err
			}
			b := harness.Batch{Next: harness.Cursor{Offset: int64(len(raw)), Size: int64(len(raw))}}
			if cur.Offset < int64(len(raw)) {
				b.Events = append(b.Events, model.UsageEvent{DedupKey: src.Path, SessionID: "s", Model: "m", Timestamp: time.Unix(1700000000, 0), Tokens: model.Tokens{Input: 2}})
			}
			return b, nil
		},
	}
	sc := New(st, nil, nil, p)
	sc.clock = func() time.Time { return time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC) }
	if err := sc.Scan(context.Background()); err == nil {
		t.Fatal("a failing source did not report an error")
	}
	d, _ := loadDiag(t, st, "fixture", store.DefaultInstanceID)
	if d.Reason != ReasonPermission {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonPermission)
	}
	if d.DiscoveredFiles != 2 || d.NewEvents != 1 {
		t.Fatalf("files=%d events=%d, want 2 and 1", d.DiscoveredFiles, d.NewEvents)
	}
	if !d.LastSuccessAt.IsZero() {
		t.Fatalf("success = %v, want zero: the read was not clean", d.LastSuccessAt)
	}
	if len(d.Sources) != 2 || d.Sources[0].Path != bad || d.Sources[0].Reason != ReasonPermission || d.Sources[1].Path != ok {
		t.Fatalf("sources = %+v", d.Sources)
	}
}
