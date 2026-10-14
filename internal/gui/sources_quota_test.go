package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/claudequota"
	"github.com/zzstar101/mytoken/internal/ingestrate"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/quota"

	// The app registers every parser; the harness picker lists what is
	// registered, so the unit test registers the one harness it names.
	_ "github.com/zzstar101/mytoken/internal/harness/codex"
)

// The tests here drive the three new settings cards and the tray's ingest
// rate with synthetic hooks: no store, no keychain, no network and no window.

// fakeSources is a gui.SourceAdmin the tests own: it records every write so a
// test can pin the exact id and roots a control passed.
type fakeSources struct {
	view     SourceView
	info     []SourceInfo
	problems []SourceProblem
	cfgErr   error
	writeErr error
	configs  int

	adds     []SourceAdd
	moves    []moveCall
	removes  []string
	promotes []string
}

type moveCall struct {
	id    string
	roots []string
}

func (f *fakeSources) Sources() []SourceInfo     { return f.info }
func (f *fakeSources) Problems() []SourceProblem { return f.problems }
func (f *fakeSources) Config(context.Context) (SourceView, error) {
	f.configs++
	return f.view, f.cfgErr
}
func (f *fakeSources) Add(_ context.Context, req SourceAdd) (SourceView, error) {
	f.adds = append(f.adds, req)
	return f.view, f.writeErr
}
func (f *fakeSources) Move(_ context.Context, id string, roots []string) (SourceView, error) {
	f.moves = append(f.moves, moveCall{id: id, roots: roots})
	return f.view, f.writeErr
}
func (f *fakeSources) Remove(_ context.Context, id string) (SourceView, error) {
	f.removes = append(f.removes, id)
	return f.view, f.writeErr
}
func (f *fakeSources) Promote(_ context.Context, id string) (SourceView, error) {
	f.promotes = append(f.promotes, id)
	return f.view, f.writeErr
}

// fakeClaudeQuota is a gui.ClaudeQuota that starts disabled, as the real one
// does, and flips only when the panel asks it to.
type fakeClaudeQuota struct {
	info      claudequota.Info
	quota     claudequota.Quota
	sources   []claudequota.Source
	enables   []string
	disables  int
	refreshes int
}

func (f *fakeClaudeQuota) Info(context.Context, string) (claudequota.Info, error) {
	return f.info, nil
}
func (f *fakeClaudeQuota) Sources(string) []claudequota.Source { return f.sources }
func (f *fakeClaudeQuota) Enable(_ context.Context, _, ref string) (claudequota.Info, error) {
	f.enables = append(f.enables, ref)
	f.info.Enabled = true
	f.info.SourceRef = ref
	return f.info, nil
}
func (f *fakeClaudeQuota) Disable(context.Context, string) error {
	f.disables++
	f.info.Enabled = false
	f.info.Quota = claudequota.Quota{}
	return nil
}
func (f *fakeClaudeQuota) Snapshot(context.Context, string) (claudequota.Quota, error) {
	return f.quota, nil
}
func (f *fakeClaudeQuota) Refresh(context.Context, string) (claudequota.Quota, error) {
	f.refreshes++
	return f.quota, nil
}

// fakeQuotaStore is the app's key/value store with one recording in it.
type fakeQuotaStore struct{ raw string }

func (f fakeQuotaStore) Setting(context.Context, string) (string, error) { return f.raw, nil }

func ptrTo[T any](v T) *T { return &v }

// renderPanel renders a snippet of the settings page in memory.
func renderPanel(t *testing.T, w, h int, body func(c *ui.Context, pal palette)) *ui.Tester {
	t.Helper()
	tt := ui.NewTester(func(c *ui.Context) {
		pal := applyTheme(c)
		ui.Column(c).Padding(12).Gap(12).Children(func() { body(c, pal) })
	}, w, h)
	tt.SetScale(1)
	tt.SetPreferences(ui.Preferences{ReduceMotion: true})
	for i := 0; i < 4; i++ {
		tt.Frame()
	}
	return tt
}

func sourceInfoFor(id string, h model.Harness, kind string, roots []string, scanned bool) SourceInfo {
	return SourceInfo{ID: id, Harness: h, Roots: roots, Source: "user", Kind: kind, Scanned: scanned}
}

// TestSourcesInstancesCardShowsInstancesAndDiagnostics pins what the editor
// says about each instance: an independent one is scanned, a backup copy is
// not, the ids are shown as they are, and a saved change that does not match
// the running process says a restart is needed instead of pretending to
// reload.
func TestSourcesInstancesCardShowsInstancesAndDiagnostics(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)
	f := &fakeSources{
		view: SourceView{
			Sources: []SourceInfo{
				{ID: "inst_main", Label: "Main", Harness: model.Codex, Roots: []string{"/tmp/a"}, Source: "user", Kind: "instance", Scanned: true},
				sourceInfoFor("inst_backup", model.Codex, "backup-copy", []string{"/tmp/b"}, false),
				{ID: "default", Harness: model.ClaudeCode, Roots: []string{"/home/u/.claude/projects"}, Source: "default", Kind: "instance", Scanned: true},
			},
			Problems: []SourceProblem{{Code: "missing-dir", ID: "inst_gone", Harness: model.Codex, Path: "/tmp/gone", Message: "root does not exist", Fatal: true}},
			Restart:  true,
		},
		info:     []SourceInfo{{ID: "inst_main", Harness: model.Codex, Scanned: true}},
		problems: []SourceProblem{{Code: "no-factory", ID: "inst_odd", Harness: model.Codex, Message: "no parser"}},
	}
	s := NewState(NewDemoService(now), Hooks{Sources: f, Now: func() time.Time { return now }}, nil)
	tt := renderPanel(t, 900, 700, func(c *ui.Context, pal palette) { s.sourcesInstancesCard(c, pal) })

	for _, want := range []string{
		"inst_main", "Main", "inst_backup", "inst_gone", "default",
		tr("srcScanned"), tr("srcNotScannedBackup"), tr("srcBackupCopy"), tr("srcUser"), tr("srcDefault"),
		tr("srcRestart"), trf("srcRunning", 1, 1),
		"missing-dir", tr("srcFatal"), tr("srcHint_missing-dir"),
		"no-factory", tr("srcHint_no-factory"),
	} {
		if !tt.HasText(want) {
			t.Errorf("the sources editor does not show %q", want)
		}
	}
	// The backup copy is declared, never scanned: it must not claim to scan.
	if tt.HasText(tr("srcNotScannedDisabled")) {
		t.Error("an enabled backup copy was shown as disabled")
	}
}

// TestSourceWritesCarryExactIDsAndRefuseFailures pins the write path: each
// control passes the entry's own id (a move is the same instance), and a
// refused write reports the error instead of a success message.
func TestSourceWritesCarryExactIDsAndRefuseFailures(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)
	f := &fakeSources{}
	s := NewState(NewDemoService(now), Hooks{Sources: f, Now: func() time.Time { return now }}, nil)

	s.src.edit = "/tmp/one, /tmp/two"
	s.sourceMove("inst_keep")
	if len(f.moves) != 1 || f.moves[0].id != "inst_keep" {
		t.Fatalf("a move must carry the entry's own id: %+v", f.moves)
	}
	if got := f.moves[0].roots; len(got) != 2 || got[0] != "/tmp/one" || got[1] != "/tmp/two" {
		t.Fatalf("move roots = %q, want the two typed roots", got)
	}
	if s.src.msg != tr("srcMoved") {
		t.Errorf("a saved move did not report success: msg = %q", s.src.msg)
	}

	s.src.draft = sourceDraft{harness: harnessOption(model.Codex), label: "Archive", roots: "/tmp/backup", backup: true}
	s.sourceAdd()
	if len(f.adds) != 1 {
		t.Fatalf("the add form did not write: %+v", f.adds)
	}
	if f.adds[0].Kind != "backup-copy" || f.adds[0].Label != "Archive" || f.adds[0].Harness != model.Codex {
		t.Errorf("add request = %+v", f.adds[0])
	}
	if got := f.adds[0].Roots; len(got) != 1 || got[0] != "/tmp/backup" {
		t.Errorf("add roots = %q", got)
	}

	s.sourceRemove("inst_keep")
	s.sourcePromote("inst_backup")
	if len(f.removes) != 1 || f.removes[0] != "inst_keep" {
		t.Errorf("remove carried %q", f.removes)
	}
	if len(f.promotes) != 1 || f.promotes[0] != "inst_backup" {
		t.Errorf("promote carried %q", f.promotes)
	}

	// A refused write must not report success.
	f.writeErr = errors.New("missing-dir: root does not exist (instance inst_x, path /tmp/gone)")
	s.src.msg = ""
	s.sourceMove("inst_x")
	if s.src.msg != "" {
		t.Errorf("a refused write reported success: msg = %q", s.src.msg)
	}
	if s.src.cfgErr == nil {
		t.Error("a refused write must surface the error")
	}
}

// TestClaudeQuotaNeedsAnIntentionalChoice pins the opt-in rule: nothing is
// enabled by itself, pressing Enable with no source selected does nothing,
// and only then does the panel show a reading.
func TestClaudeQuotaNeedsAnIntentionalChoice(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)
	const ref = "/home/u/.claude/.credentials.json"
	f := &fakeClaudeQuota{
		info: claudequota.Info{Instance: "default", Target: claudequota.Target},
		sources: []claudequota.Source{
			{Instance: "default", Label: "Claude", Kind: claudequota.SourceFile, Ref: ref, Profile: "/home/u/.claude",
				Read: func(context.Context) (claudequota.Credential, error) { return claudequota.Credential{}, nil }},
			{Instance: "default", Label: "Claude", Kind: claudequota.SourceKeychain, Ref: "Claude Code-credentials"},
		},
	}
	s := NewState(NewDemoService(now), Hooks{ClaudeQuota: f, Now: func() time.Time { return now }}, nil)
	tt := renderPanel(t, 900, 700, func(c *ui.Context, pal palette) { s.claudeQuotaCard(c, pal) })

	// Before anything is enabled: what would be read, and where it would go.
	for _, want := range []string{tr("claudeQuotaDisclose"), tr("claudeQuotaTarget"), claudequota.Target, ref} {
		if !tt.HasText(want) {
			t.Errorf("the disclosure does not show %q", want)
		}
	}
	if f.info.Enabled {
		t.Fatal("the panel enabled itself")
	}
	if tt.HasText(tr("claudeQuotaRefresh")) || tt.HasText(tr("claudeQuotaCached")) {
		t.Error("a disabled panel shows a reading")
	}
	if err := tt.Click(tr("claudeQuotaEnable")); err != nil {
		t.Fatalf("the Enable button is not clickable: %v", err)
	}
	if len(f.enables) != 0 {
		t.Fatalf("Enable with no source selected still enabled %q", f.enables)
	}
	if s.cq.msg != tr("claudeQuotaPick") {
		t.Errorf("no source selected: msg = %q, want the pick hint", s.cq.msg)
	}

	// Choosing a source explicitly and enabling asks for exactly that ref.
	s.cq.sel = ref
	s.claudeQuotaEnable()
	if len(f.enables) != 1 || f.enables[0] != ref {
		t.Fatalf("enable carried %q, want %q", f.enables, ref)
	}
	if !s.cq.info.Enabled {
		t.Error("enabling did not refresh the panel's state")
	}
	after := renderPanel(t, 900, 700, func(c *ui.Context, pal palette) { s.claudeQuotaCard(c, pal) })
	if !after.HasText(tr("claudeQuotaCached")) || !after.HasText(ref) {
		t.Error("an enabled panel must show the cached reading and the exact source it uses")
	}
	if !after.HasText(claudequota.Target) {
		t.Error("an enabled panel must keep disclosing the request target")
	}

	// Disabling asks the service at once and shows the panel off again.
	s.claudeQuotaDisable()
	if f.disables != 1 {
		t.Errorf("disable called the service %d times, want 1", f.disables)
	}
	if s.cq.info.Enabled || s.cq.msg != tr("claudeQuotaDisabledMsg") {
		t.Errorf("disable left enabled=%v msg=%q", s.cq.info.Enabled, s.cq.msg)
	}
	off := renderPanel(t, 900, 700, func(c *ui.Context, pal palette) { s.claudeQuotaCard(c, pal) })
	if off.HasText(tr("claudeQuotaRefresh")) {
		t.Error("the disabled panel still offers Refresh")
	}
	if !off.HasText(claudequota.Target) {
		t.Error("the disabled panel must still disclose what enabling would request")
	}
}

// TestClaudeQuotaMissingUsageIsNotZero pins the reading rules: a window the
// payload did not carry reads "—", never 0%.
func TestClaudeQuotaMissingUsageIsNotZero(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	if got := claudeUsedText(claudequota.Window{Kind: "five_hour"}); got != "—" {
		t.Errorf("an absent window reads %q, want —", got)
	}
	if got := claudeUsedText(claudequota.Window{Kind: "five_hour", Present: true}); got != "—" {
		t.Errorf("a present window with no percentage reads %q, want —", got)
	}
	used := claudeUsedText(claudequota.Window{Kind: "five_hour", Present: true, UsedPercent: ptrTo(12.5)})
	if used == "—" {
		t.Fatal("a stated utilization was dropped")
	}
	if got := claudeStatusText(claudequota.StatusUnavailable); got != tr("claudeQuotaUnavailable") {
		t.Errorf("unavailable status reads %q", got)
	}
}

// TestOfflineQuotaCardShowsOnlyWhatWasRecorded pins the offline card: no
// record is said out loud, a corrupt record is not a zero, and the credits
// are shown verbatim with their unit unstated.
func TestOfflineQuotaCardShowsOnlyWhatWasRecorded(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)
	newState := func(raw string) *State {
		s := NewState(NewDemoService(now), Hooks{
			QuotaStore: fakeQuotaStore{raw: raw},
			Now:        func() time.Time { return now },
		}, nil)
		s.oq.inst = "default"
		return s
	}

	empty := renderPanel(t, 900, 700, func(c *ui.Context, pal palette) { newState("").offlineQuotaCard(c, pal) })
	if !empty.HasText(tr("quotaNoData")) || !empty.HasText(tr("quotaNoDataHint")) {
		t.Error("a missing record must say there is no record")
	}
	if empty.HasText(tr("quotaObserved")) {
		t.Error("a missing record was rendered as an observation")
	}

	corrupt := renderPanel(t, 900, 700, func(c *ui.Context, pal palette) { newState("{not json").offlineQuotaCard(c, pal) })
	if !corrupt.HasText(tr("quotaCorrupt")) {
		t.Error("a corrupt record must be reported as corrupt")
	}

	rep := &quota.Report{
		Source:     "codex",
		ObservedAt: &now,
		Status:     quota.StatusOK,
		Primary: &quota.Window{
			Kind:             quota.WindowPrimary,
			Status:           quota.StatusOK,
			WindowMinutes:    ptrTo(int64(300)),
			RemainingDerived: true,
			RemainingPercent: ptrTo(60.0),
		},
		Credits: &quota.Credits{HasCredits: ptrTo(true), Balance: ptrTo("12.5")},
	}
	raw, err := json.Marshal(quota.Latest{Harness: "codex", Instance: "default", Source: "log", IngestedAt: now, Report: rep})
	if err != nil {
		t.Fatal(err)
	}
	full := renderPanel(t, 900, 800, func(c *ui.Context, pal palette) { newState(string(raw)).offlineQuotaCard(c, pal) })
	justNow := fmtAgo(now, now)
	for _, want := range []string{
		fmt.Sprintf(tr("quotaObserved"), justNow), fmt.Sprintf(tr("quotaIngested"), justNow),
		tr("quotaSource"), tr("quotaPrimary"), tr("quotaSecondary"),
		tr("quotaCredits"), tr("quotaCreditsHas"), "12.5", tr("quotaUnitsUnstated"), tr("quotaNoTotals"),
	} {
		if !full.HasText(want) {
			t.Errorf("the offline card does not show %q", want)
		}
	}
	// The empty primary window carries no percentage: it reads "—", so a
	// missing value can never be mistaken for 0%.
	if !full.HasText("—") {
		t.Error("a window with no recorded utilization must read —")
	}
	// No pace is invented from a single observation.
	for _, unwanted := range []string{"per day", "daily pace", "tok/s"} {
		if full.HasText(unwanted) {
			t.Errorf("the offline card invented %q", unwanted)
		}
	}
}

// TestTrayIngestRateIsCommittedOutput pins the tray line: it is the scanner's
// ingest-rate snapshot, and anything the scanner cannot vouch for reads "—".
func TestTrayIngestRateIsCommittedOutput(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)
	render := func(hooks Hooks) *ui.Tester {
		s := NewState(NewDemoService(now), hooks, nil)
		s.Start()
		tt := ui.NewTester(s.TrayView, TrayWidth, TrayHeight)
		tt.SetScale(1)
		tt.SetPreferences(ui.Preferences{ReduceMotion: true})
		for i := 0; i < 6; i++ {
			tt.Frame()
		}
		return tt
	}
	base := func() Hooks {
		return Hooks{Relays: NewDemoRelays(now), Now: func() time.Time { return now }}
	}

	// No hook at all: unknown, not zero.
	none := render(base())
	rate := strings.ToUpper(tr("ingestRate"))
	if !none.HasText(rate) || !none.HasText(tr("ingestRateUnknown")) || !none.HasText("—") {
		t.Error("without a scanner the tray must show an unknown ingest rate")
	}

	// A scanner that cannot vouch for the number.
	h := base()
	h.IngestRate = func(time.Time) ingestrate.Snapshot {
		return ingestrate.Snapshot{Reason: ingestrate.ReasonNoLiveSignal, Window: ingestrate.Window}
	}
	unknown := render(h)
	if !unknown.HasText(tr("ingestRateUnknown")) || unknown.HasText("42.5") {
		t.Error("an unavailable snapshot must read unknown")
	}

	// A committed rate is shown with its window.
	h.IngestRate = func(time.Time) ingestrate.Snapshot {
		return ingestrate.Snapshot{Available: true, Tokens: 2550, Window: ingestrate.Window, Rate: 42.5}
	}
	live := render(h)
	if !live.HasText("42.5") {
		t.Error("a committed ingest rate was not shown")
	}
	if !live.HasText(trf("ingestRateWindow", 60)) {
		t.Error("the ingest rate was shown without its window")
	}
	// It is a rate of tokens per second, never a generation speed.
	if live.HasText("tok/s generated") {
		t.Error("the tray claims a generation speed")
	}
}
