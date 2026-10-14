package gui

// Tests for the sources card's collection evidence: what the store recorded is
// what the card shows, a record that is missing is missing rather than zero, a
// record written before a move is labelled last known, and the explicit rebuild
// action still works and refreshes the evidence. Everything is synthetic — a
// fake SourceAdmin, a fake record reader, demo data — so no store, keychain,
// network or window is touched.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/scan"

	// The app registers every parser; the sources card marks each row with its
	// harness, so the unit test registers the one harness it names.
	_ "github.com/zzstar101/mytoken/internal/harness/codex"
)

// diagReader is a DiagnosticsReader the tests own: it records every read and
// hands back the record set for an instance id, or none when there is none.
type diagReader struct {
	reads   []string
	records map[string]scan.Diagnostics
}

func (r *diagReader) read(_ context.Context, h model.Harness, instance string) (scan.Diagnostics, bool) {
	r.reads = append(r.reads, string(h)+"/"+instance)
	d, ok := r.records[instance]
	return d, ok
}

// countText is how many rendered elements show exactly s.
func countText(tt *ui.Tester, s string) int {
	n := 0
	for _, text := range tt.Texts() {
		if text == s {
			n++
		}
	}
	return n
}

// TestSourceEvidenceIsRecordedNotZero pins what the card says about each
// scanned instance: the recorded attempt, success, recognized files, new
// events, indexed sessions and reason are shown as they were written, a
// session count the record could not state reads as unknown and never as 0, an
// instance with no record says so, and an instance no scan attempts (a backup
// copy, a disabled entry) is never given evidence at all.
func TestSourceEvidenceIsRecordedNotZero(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)

	live := scan.Diagnostics{
		Version:         scan.DiagnosticsVersion,
		Harness:         string(model.Codex),
		Instance:        "inst_live",
		LastAttemptAt:   now.Add(-4 * time.Minute),
		LastSuccessAt:   now.Add(-2 * time.Hour),
		Reason:          scan.ReasonOK,
		DiscoveredFiles: 12,
		NewEvents:       3204,
		IndexedSessions: ptrTo(int64(88)),
		Sources:         []scan.SourceDiagnostics{{Path: "/tmp/a/one.jsonl", Kind: "jsonl", Reason: scan.ReasonOK, Events: 3204, Sessions: 88}},
	}
	// A record the scanner wrote without a session count: the count is unknown,
	// which is not the same claim as zero.
	fuzzy := scan.Diagnostics{
		Version:         scan.DiagnosticsVersion,
		Harness:         string(model.Codex),
		Instance:        "inst_fuzzy",
		LastAttemptAt:   now.Add(-90 * time.Minute),
		Reason:          scan.ReasonNoTokenData,
		DiscoveredFiles: 3,
		Sources:         []scan.SourceDiagnostics{{Path: "/tmp/c/odd.jsonl", Reason: scan.ReasonNoTokenData}},
	}

	f := &fakeSources{view: SourceView{Sources: []SourceInfo{
		sourceInfoFor("inst_live", model.Codex, "instance", []string{"/tmp/a"}, true),
		sourceInfoFor("inst_fuzzy", model.Codex, "instance", []string{"/tmp/c"}, true),
		sourceInfoFor("inst_none", model.Codex, "instance", []string{"/tmp/e"}, true),
		sourceInfoFor("inst_backup", model.Codex, "backup-copy", []string{"/tmp/b"}, false),
		{ID: "inst_off", Harness: model.Codex, Roots: []string{"/tmp/d"}, Source: "user", Kind: "instance", Disabled: true},
	}}}
	read := &diagReader{records: map[string]scan.Diagnostics{"inst_live": live, "inst_fuzzy": fuzzy}}
	s := NewState(NewDemoService(now), Hooks{Sources: f, Diagnostics: read.read, Now: func() time.Time { return now }}, nil)
	tt := renderPanel(t, 900, 900, func(c *ui.Context, pal palette) { s.sourcesInstancesCard(c, pal) })

	when, found := diagnosticsLines(now, live)
	if !tt.HasText(when) || !tt.HasText(found) {
		t.Errorf("the card does not show the recorded evidence:\n want %q\n want %q", when, found)
	}
	if !tt.HasText(fmt.Sprintf(tr("srcDiagFiles"), fmtInt(12))) {
		t.Error("the recognized file count is not shown")
	}
	_, fuzzyFound := diagnosticsLines(now, fuzzy)
	if !tt.HasText(fuzzyFound) {
		t.Errorf("the second record is not shown: %q", fuzzyFound)
	}
	if !strings.Contains(fuzzyFound, "—") {
		t.Errorf("an unknown session count must read as unknown: %q", fuzzyFound)
	}
	if tt.HasText(fmt.Sprintf(tr("srcDiagSessions"), "0")) {
		t.Error("an instance with no session count was shown as zero sessions")
	}
	if n := countText(tt, tr("srcDiagNone")); n != 1 {
		t.Errorf("the scanned instance with no record must say so exactly once, got %d", n)
	}
	if n := countText(tt, when); n != 1 {
		t.Errorf("evidence was drawn %d times, want once: no scan attempts the backup copy or the disabled entry", n)
	}
	if tt.HasText(tr("srcDiagHistorical")) {
		t.Error("a record whose paths are under the current roots was called historical")
	}
	if !tt.HasText(tr("srcNotScannedBackup")) || !tt.HasText(tr("srcNotScannedDisabled")) {
		t.Error("the backup copy and the disabled entry must still say they are not scanned")
	}
	// One background read per scanned instance; the two entries no scan
	// attempts are never asked about.
	if len(read.reads) != 3 {
		t.Errorf("diagnostics reads = %v, want one per scanned instance (3)", read.reads)
	}
	if f.configs != 1 {
		t.Errorf("configuration reads = %d, want one background load", f.configs)
	}
	// The record names the files a scan read; the card shows counts, never
	// those paths, so evidence cannot leak a location the row does not already
	// declare as a root.
	for _, text := range tt.Texts() {
		if strings.Contains(text, "one.jsonl") || strings.Contains(text, "odd.jsonl") {
			t.Errorf("the card rendered a recorded file path: %q", text)
		}
	}
}

// TestSourceEvidenceAfterAMoveIsLastKnown: the record is keyed by instance id,
// so it survives a move and still names the old location. Until a scan of the
// new roots completes, the card must present it as last known — never as a
// successful scan of the source now in force — and drop the label as soon as a
// fresh record exists.
func TestSourceEvidenceAfterAMoveIsLastKnown(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)
	stale := scan.Diagnostics{
		LastAttemptAt: now.Add(-2 * time.Hour), LastSuccessAt: now.Add(-2 * time.Hour),
		Reason: scan.ReasonOK, DiscoveredFiles: 5, NewEvents: 40, IndexedSessions: ptrTo(int64(3)),
		Sources: []scan.SourceDiagnostics{{Path: "/old/root/one.jsonl", Reason: scan.ReasonOK}},
	}
	fresh := scan.Diagnostics{
		LastAttemptAt: now.Add(-time.Minute), Reason: scan.ReasonOK, DiscoveredFiles: 1, NewEvents: 2,
		IndexedSessions: ptrTo(int64(1)),
		Sources:         []scan.SourceDiagnostics{{Path: "/new/root/two.jsonl", Reason: scan.ReasonOK}},
	}

	f := &fakeSources{view: SourceView{Sources: []SourceInfo{
		sourceInfoFor("inst_moved", model.Codex, "instance", []string{"/new/root"}, true),
	}}}
	read := &diagReader{records: map[string]scan.Diagnostics{"inst_moved": stale}}
	s := NewState(NewDemoService(now), Hooks{Sources: f, Diagnostics: read.read, Now: func() time.Time { return now }}, nil)
	tt := renderPanel(t, 900, 300, func(c *ui.Context, pal palette) { s.sourcesInstancesCard(c, pal) })

	if !tt.HasText(tr("srcDiagHistorical")) {
		t.Error("a record from before a move was shown without saying it is last known")
	}
	if _, found := diagnosticsLines(now, stale); !tt.HasText(found) {
		t.Error("the last known evidence was hidden instead of labelled")
	}

	// A scan of the new roots writes a fresh record: the label goes away and
	// the new evidence takes its place.
	read.records["inst_moved"] = fresh
	s.src.loaded = false
	for i := 0; i < 4; i++ {
		tt.Frame()
	}
	if tt.HasText(tr("srcDiagHistorical")) {
		t.Error("a fresh record for the current roots was still called historical")
	}
	if _, found := diagnosticsLines(now, fresh); !tt.HasText(found) {
		t.Error("the fresh record was not shown")
	}
}

// TestSourceEvidenceNeverReadsFresherThanItIs pins the age wording: a zero
// timestamp is "never", a timestamp ahead of the clock is shown as itself
// instead of being rounded into "just now", and an old record keeps its real
// age.
func TestSourceEvidenceNeverReadsFresherThanItIs(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)

	if got := diagnosticsAge(now, time.Time{}); got != tr("srcDiagNever") {
		t.Errorf("a zero timestamp = %q, want %q", got, tr("srcDiagNever"))
	}
	ahead := now.Add(time.Hour)
	if got := diagnosticsAge(now, ahead); got != fmtWhen(ahead) {
		t.Errorf("a timestamp after now = %q, want %q", got, fmtWhen(ahead))
	}
	if got := diagnosticsAge(now, now.Add(-30*time.Second)); got != fmtAgo(now, now.Add(-30*time.Second)) {
		t.Errorf("a fresh record = %q, want its real age", got)
	}
	old := now.AddDate(0, 0, -40)
	if got := diagnosticsAge(now, old); got != old.Format("2006-01-02") {
		t.Errorf("a 40-day-old record = %q, want %q", got, old.Format("2006-01-02"))
	}
}

// TestRebuildActionRefreshesEvidence pins the explicit re-trigger: nothing
// rescans on its own, drawing frames never re-reads the store, the button is
// still usable, and the record the rescan wrote is shown afterwards without
// anything else being pressed. The page opens no URL and the rescan is the
// app's own index, so no credential is read and no request is made.
func TestRebuildActionRefreshesEvidence(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)
	before := scan.Diagnostics{
		LastAttemptAt: now.Add(-2 * time.Hour), Reason: scan.ReasonOK, DiscoveredFiles: 4,
		NewEvents: 9, IndexedSessions: ptrTo(int64(2)),
		Sources: []scan.SourceDiagnostics{{Path: "/tmp/a/one.jsonl", Reason: scan.ReasonOK}},
	}
	after := scan.Diagnostics{
		LastAttemptAt: now, Reason: scan.ReasonOK, DiscoveredFiles: 5,
		NewEvents: 11, IndexedSessions: ptrTo(int64(3)),
		Sources: []scan.SourceDiagnostics{{Path: "/tmp/a/two.jsonl", Reason: scan.ReasonOK}},
	}

	f := &fakeSources{view: SourceView{Sources: []SourceInfo{
		sourceInfoFor("inst_live", model.Codex, "instance", []string{"/tmp/a"}, true),
	}}}
	read := &diagReader{records: map[string]scan.Diagnostics{"inst_live": before}}
	rebuilds := 0
	s := NewState(NewDemoService(now), Hooks{
		Sources:     f,
		Diagnostics: read.read,
		Now:         func() time.Time { return now },
		Rebuild: func() {
			rebuilds++
			// The rescan writes a fresh record for the same instance.
			read.records["inst_live"] = after
		},
	}, nil)
	tt := renderPanel(t, 900, 1100, func(c *ui.Context, pal palette) { s.settingsPage(c, pal) })

	if rebuilds != 0 {
		t.Fatalf("the rescan ran without being asked: %d rebuilds", rebuilds)
	}
	if f.configs != 1 {
		t.Errorf("the sources card loaded %d times while drawing; frames must not re-read", f.configs)
	}
	if len(read.reads) != 1 {
		t.Errorf("diagnostics were read %v; want once per background load, never per frame", read.reads)
	}

	// Find returns the first label carrying the text — the setting row's own
	// title — so the button is the control just to its right.
	rect, ok := tt.Find(tr("rebuild"))
	if !ok {
		t.Fatal("the settings page does not offer the rebuild action")
	}
	tt.ClickAt(rect.X+rect.W+20, rect.Y+rect.H/2)
	tt.Frame()
	if rebuilds != 1 {
		t.Fatalf("the rebuild action is not usable: %d rebuilds", rebuilds)
	}
	for i := 0; i < 4; i++ {
		tt.Frame()
	}
	if _, found := diagnosticsLines(now, after); !tt.HasText(found) {
		t.Error("the evidence the rescan wrote was not shown after the rebuild")
	}
	if _, gone := diagnosticsLines(now, before); tt.HasText(gone) {
		t.Error("the superseded evidence is still shown after the rebuild")
	}
	if urls := tt.OpenedURLs(); len(urls) != 0 {
		t.Errorf("the settings page opened %v; nothing may be fetched", urls)
	}
}

// TestSourceEvidenceIsHiddenWithoutAReader: with no Diagnostics hook the card
// shows no evidence at all — not zeros, and not an "unavailable" line per row.
func TestSourceEvidenceIsHiddenWithoutAReader(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)
	f := &fakeSources{view: SourceView{Sources: []SourceInfo{
		sourceInfoFor("inst_live", model.Codex, "instance", []string{"/tmp/a"}, true),
	}}}
	s := NewState(NewDemoService(now), Hooks{Sources: f, Now: func() time.Time { return now }}, nil)
	tt := renderPanel(t, 900, 300, func(c *ui.Context, pal palette) { s.sourcesInstancesCard(c, pal) })

	for _, text := range []string{tr("srcDiagNone"), tr("srcDiagHistorical")} {
		if tt.HasText(text) {
			t.Errorf("without a reader the card must not show %q", text)
		}
	}
	if !tt.HasText(tr("srcScanned")) {
		t.Error("the rest of the row must still be drawn")
	}
	if f.configs != 1 {
		t.Errorf("configuration reads = %d, want one", f.configs)
	}
}
