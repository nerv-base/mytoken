package gui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/scan"
)

// SourceInfo is one data source the GUI shows: either an entry written in
// sources.json, or an instance a harness's own defaults resolved to.
type SourceInfo struct {
	// ID is the instance's stable identity. It is never derived from the
	// roots or the label, so a move keeps it, and two instances holding the
	// same content stay two instances with two ids.
	ID       string
	Label    string
	Harness  model.Harness
	Roots    []string
	Source   string // "user" (sources.json), "env" or "default"
	Kind     string // "instance" or "backup-copy"
	Disabled bool
	// Scanned reports whether this instance is handed to a scanner.
	Scanned bool
	// Reason is the first non-fatal resolution problem, "" when there is none.
	Reason string
}

// Explicit reports whether the entry is written in sources.json, as opposed
// to resolved from a harness's own defaults. Only explicit entries can be
// edited: the defaults belong to the harness, not to the user.
func (in SourceInfo) Explicit() bool { return in.Source == "user" }

// BackupCopy reports whether the entry is declared a backup copy, which is
// kept for diagnostics but never scanned and never counted.
func (in SourceInfo) BackupCopy() bool { return in.Kind == "backup-copy" }

// SourceProblem is one resolution problem. Fatal problems stop the whole
// resolution; the rest are diagnostics attached to one instance.
type SourceProblem struct {
	Code    string
	ID      string
	Harness model.Harness
	Path    string
	Message string
	Fatal   bool
}

// SourceAdd is a new sources.json entry. An empty Kind asks for an
// independent instance; an empty Label leaves the entry unnamed.
type SourceAdd struct {
	Label    string
	Harness  model.Harness
	Roots    []string
	Kind     string
	Disabled bool
}

// SourceView is the persisted configuration, resolved the way the next start
// resolves it.
type SourceView struct {
	Sources  []SourceInfo
	Problems []SourceProblem
	// Restart reports that the running app resolved something other than
	// what sources.json holds now: the change is saved, and it has not been
	// applied to the running scan.
	Restart bool
	// ScanError is the runtime failure that stopped the watcher, when there
	// is one. Scanning is stopped until a reload starts a replacement; the
	// card shows this instead of implying the sources are still being read.
	ScanError string
}

// SourceAdmin reads and writes the instance configuration. An implementation
// must validate a change with the same resolver the app starts with and
// refuse to save one that would not resolve: an unusable explicit path is an
// error, never a silent fallback to the harness's defaults.
type SourceAdmin interface {
	// Sources is the running resolution: the instances the scanner is
	// actually reading right now.
	Sources() []SourceInfo
	// Problems is the running resolution's diagnostics.
	Problems() []SourceProblem
	// Config loads sources.json and resolves it.
	Config(ctx context.Context) (SourceView, error)
	// Add, Move, Remove and Promote write sources.json. Move keeps the
	// entry's id; Promote turns a backup copy into an independent instance.
	// Remove deletes the entry only, never the data it pointed at.
	Add(ctx context.Context, req SourceAdd) (SourceView, error)
	Move(ctx context.Context, id string, roots []string) (SourceView, error)
	Remove(ctx context.Context, id string) (SourceView, error)
	Promote(ctx context.Context, id string) (SourceView, error)
}

// sourceState holds the harnesses with indexed requests (all time, most
// requests first) and the instance editor's own state.
type sourceState struct {
	loaded bool
	rows   []query.Bucket
	err    error

	// view is the persisted configuration; cfgErr is why it could not be
	// read or written. seq orders writes so a slow completion cannot put a
	// superseded view back on the card.
	view   SourceView
	cfgErr error
	busy   bool
	msg    string
	seq    int

	// diag is the collection evidence of the view's scanned instances, keyed
	// by instance id, and diagLoaded is whether it was read at all: without
	// it "no record" would be indistinguishable from "not loaded", and the
	// card must never turn a missing record into zeros. loadSeq drops a
	// superseded configuration load, so a slower read cannot put an older
	// view and its evidence back over a newer one.
	diag       map[string]scan.Diagnostics
	diagLoaded bool
	loadSeq    int

	open   bool
	editID string
	edit   string
	draft  sourceDraft
	// confirm is the destructive action awaiting its second click:
	// "remove:<id>" or "promote:<id>".
	confirm string
}

type sourceDraft struct {
	harness string // the harness option text, see harnessOption
	label   string
	roots   string
	backup  bool
}

func (s *State) loadSources() {
	s.run("sources", func(ctx context.Context) func() {
		f := query.Filter{Range: query.Range{From: time.Unix(0, 0), To: s.Hooks.Now().AddDate(0, 0, 1)}}
		buckets, err := s.Q.ByHarness(ctx, f)
		rows := buckets[:0:0]
		for _, b := range buckets {
			if b.Requests > 0 {
				rows = append(rows, b)
			}
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Requests > rows[j].Requests })
		return func() { s.src = sourceState{loaded: true, rows: rows, err: err} }
	})
}

// sourcesCard lists the harnesses with indexed requests, and the roots
// that hold their logs; the supported ones without data are only counted.
func (s *State) sourcesCard(c *ui.Context, pal palette) {
	if !s.src.loaded {
		s.src.loaded = true
		s.loadSources()
	}
	card(c, pal, tr("sources"), nil, func() {
		ui.Text(c, tr("sourcesSub")).FontSize(12).TextColor(pal.muted)
		if s.src.err != nil {
			ui.Text(c, s.src.err.Error()).FontSize(12).TextColor(Soyo)
		}
		for _, b := range s.src.rows {
			h := model.Harness(b.Key)
			ui.Row(c.Key(b.Key)).Gap(10).Padding(6, 0).AlignItems(ui.Start).Children(func() {
				harnessMark(c, h, 16)
				ui.Column(c).Grow(1).Basis(0).Gap(3).Children(func() {
					ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
						ui.Text(c, h.DisplayName()).FontSize(13).FontWeight(650).TextColor(pal.ink).Grow(1)
						ui.Text(c, fmt.Sprintf(tr("sourceRequests"), fmtInt(b.Requests))).FontSize(11.5).TextColor(pal.muted).FontFeatures("tnum")
					})
					if p, ok := harness.Get(h); ok {
						for _, r := range p.Roots() {
							if exists(r) {
								ui.Text(c.Key(r), shortPath(r)).FontSize(11.5).TextColor(pal.muted).SingleLine().Ellipsis("…")
							}
						}
					}
				})
			})
		}
		if idle := len(harness.All()) - len(s.src.rows); s.src.err == nil && idle > 0 {
			ui.Text(c, fmt.Sprintf(tr("sourcesIdle"), idle)).FontSize(11.5).TextColor(pal.muted.Alpha(0.7))
		}
	})
}

// loadSourceConfig reads sources.json and resolves it, so the card shows what
// the next start will do rather than only what is running now. The collection
// evidence is read in the same background pass, one read per scanned instance:
// the card then draws whatever it already holds, so no frame ever queries the
// store. A disabled or backup-copy instance is never attempted by a scan, so
// no evidence is asked of it — it must not be shown as a scan that found
// nothing.
func (s *State) loadSourceConfig() {
	admin := s.Hooks.Sources
	if admin == nil {
		return
	}
	read := s.Hooks.Diagnostics
	s.src.loadSeq++
	seq := s.src.loadSeq
	s.run("source-config", func(ctx context.Context) func() {
		v, err := admin.Config(ctx)
		var diag map[string]scan.Diagnostics
		if read != nil && err == nil {
			diag = make(map[string]scan.Diagnostics, len(v.Sources))
			for _, in := range v.Sources {
				if !in.Scanned {
					continue
				}
				if d, ok := read(ctx, in.Harness, in.ID); ok {
					diag[in.ID] = d
				}
			}
		}
		return func() {
			if s.src.loadSeq != seq {
				return
			}
			s.src.cfgErr = err
			s.src.diag, s.src.diagLoaded = nil, false
			if err == nil {
				s.src.view = v
				s.src.diag, s.src.diagLoaded = diag, read != nil
			}
		}
	})
}

// sourceWrite runs one configuration write and shows its result. The
// completion is scoped to the write that is still the latest: a superseded
// one is dropped instead of overwriting a newer view.
func (s *State) sourceWrite(kind, done string, act func(ctx context.Context) (SourceView, error)) {
	admin := s.Hooks.Sources
	if admin == nil {
		return
	}
	s.src.busy = true
	s.src.msg = ""
	s.src.seq++
	seq := s.src.seq
	s.run(kind, func(ctx context.Context) func() {
		v, err := act(ctx)
		return func() {
			if s.src.seq != seq {
				return
			}
			s.src.busy = false
			if err != nil {
				// A refused write may still have landed on disk — a save
				// whose reload was not honoured is saved but not applied — so
				// re-read the file on the next frame instead of leaving a
				// view that no longer matches it. That reload converges: a
				// successful load does not ask for another.
				s.src.cfgErr = err
				s.src.loaded = false
				return
			}
			s.src.cfgErr = nil
			s.src.view = v
			s.src.msg = tr(done)
			s.src.confirm, s.src.open, s.src.editID = "", false, ""
		}
	})
}

func (s *State) sourceAdd() {
	req := SourceAdd{
		Label:   strings.TrimSpace(s.src.draft.label),
		Harness: harnessFromOption(s.src.draft.harness),
		Roots:   splitRoots(s.src.draft.roots),
		Kind:    "instance",
	}
	if s.src.draft.backup {
		req.Kind = "backup-copy"
	}
	s.sourceWrite("source-add", "srcAdded", func(ctx context.Context) (SourceView, error) {
		return s.Hooks.Sources.Add(ctx, req)
	})
}

func (s *State) sourceMove(id string) {
	roots := splitRoots(s.src.edit)
	s.sourceWrite("source-move", "srcMoved", func(ctx context.Context) (SourceView, error) {
		return s.Hooks.Sources.Move(ctx, id, roots)
	})
}

func (s *State) sourceRemove(id string) {
	s.sourceWrite("source-remove", "srcRemoved", func(ctx context.Context) (SourceView, error) {
		return s.Hooks.Sources.Remove(ctx, id)
	})
}

func (s *State) sourcePromote(id string) {
	s.sourceWrite("source-promote", "srcPromoted", func(ctx context.Context) (SourceView, error) {
		return s.Hooks.Sources.Promote(ctx, id)
	})
}

// sourcesInstancesCard administers sources.json: it lists what the next start
// resolves, diagnoses what does not resolve, and offers add, move, remove and
// promote. A saved change reloads the running scanner — the ids, cursors and
// history live in the store, so the replacement resumes where the old scan
// stopped. "Restart" only appears when the running resolution no longer
// matches what was saved, which is when a change was made outside the app or a
// reload could not be honoured.
func (s *State) sourcesInstancesCard(c *ui.Context, pal palette) {
	admin := s.Hooks.Sources
	if admin == nil {
		return
	}
	if !s.src.loaded {
		s.src.loaded = true
		s.loadSourceConfig()
	}
	card(c, pal, tr("sourcesInstances"), func() {
		if s.src.busy {
			chipOn(c, pal, tr("srcWorking"), pal.muted)
			return
		}
		if ui.Button(c, tr("srcAdd")).Clicked() {
			s.src.open, s.src.editID, s.src.confirm = !s.src.open, "", ""
			if s.src.open {
				s.src.draft = sourceDraft{}
			}
		}
	}, func() {
		ui.Text(c, tr("sourcesIndepSub")).FontSize(12).TextColor(pal.muted)
		if s.src.msg != "" {
			sourceBanner(c, pal, s.src.msg, Anon)
		}
		if s.src.cfgErr != nil {
			sourceBanner(c, pal, s.src.cfgErr.Error(), Taki)
		}
		// A watcher that stopped on a runtime failure: scanning is stopped,
		// the reason is shown, and nothing falls back to another source set.
		if s.src.view.ScanError != "" {
			sourceBanner(c, pal, fmt.Sprintf(tr("srcScanStopped"), s.src.view.ScanError), Taki)
		}
		if s.src.view.Restart {
			sourceBanner(c, pal, tr("srcRestart"), Soyo)
			// The running app still holds the previous resolution: say what
			// it is instead of implying the change is already live.
			running := admin.Sources()
			scanned := 0
			for _, r := range running {
				if r.Scanned {
					scanned++
				}
			}
			ui.Text(c, fmt.Sprintf(tr("srcRunning"), scanned, len(running))).FontSize(11.5).TextColor(pal.muted)
			for _, p := range admin.Problems() {
				s.sourceProblemRow(c, pal, p)
			}
		}
		for _, p := range s.src.view.Problems {
			s.sourceProblemRow(c, pal, p)
		}
		if len(s.src.view.Sources) == 0 {
			ui.Text(c, tr("srcNone")).FontSize(12).TextColor(pal.muted)
		}
		for _, in := range s.src.view.Sources {
			s.sourceRow(c, pal, in)
		}
		switch {
		case s.src.editID != "":
			s.sourceMoveEditor(c, pal)
		case s.src.open:
			s.sourceAddEditor(c, pal)
		}
	})
}

func (s *State) sourceRow(c *ui.Context, pal palette, in SourceInfo) {
	ui.Row(c.Key("src-"+in.ID)).Gap(10).Padding(8, 0).AlignItems(ui.Start).Children(func() {
		harnessMark(c, in.Harness, 18)
		ui.Column(c).Grow(1).Basis(0).Gap(4).Children(func() {
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				name := in.Harness.DisplayName()
				if in.Label != "" {
					name += " · " + in.Label
				}
				ui.Text(c, name).FontSize(13).FontWeight(650).TextColor(pal.ink).SingleLine().Ellipsis("…").Shrink(1)
				ui.Spacer(c)
				switch {
				case in.BackupCopy():
					chipOn(c, pal, tr("srcBackupCopy"), Rana).Tooltip(tr("srcBackupTip"))
				case in.Disabled:
					chipOn(c, pal, tr("srcDisabled"), pal.muted)
				}
				switch {
				case in.Explicit():
					chipOn(c, pal, tr("srcUser"), Anon)
				case in.Source == "env":
					chipOn(c, pal, tr("srcEnv"), Soyo)
				default:
					chipOn(c, pal, tr("srcDefault"), pal.muted)
				}
			})
			ui.Text(c, in.ID).FontSize(11).TextColor(pal.muted.Alpha(0.8)).SingleLine().Ellipsis("…")
			if len(in.Roots) == 0 {
				ui.Text(c, tr("srcNoRoots")).FontSize(11.5).TextColor(pal.muted)
			}
			for _, r := range in.Roots {
				ui.Text(c.Key(r), shortPath(r)).FontSize(11.5).TextColor(pal.muted).SingleLine().Ellipsis("…")
			}
			switch {
			case in.BackupCopy():
				ui.Text(c, tr("srcNotScannedBackup")).FontSize(11.5).TextColor(pal.muted)
			case in.Disabled:
				ui.Text(c, tr("srcNotScannedDisabled")).FontSize(11.5).TextColor(pal.muted)
			case in.Scanned:
				ui.Text(c, tr("srcScanned")).FontSize(11.5).TextColor(pal.muted)
			default:
				ui.Text(c, tr("srcNotScanned")).FontSize(11.5).TextColor(pal.muted)
			}
			if in.Reason != "" {
				ui.Text(c, in.Reason).FontSize(11.5).TextColor(Soyo)
			}
			// The recorded evidence belongs to the instances a scan actually
			// attempts: a disabled or backup-copy entry is never scanned, so
			// it has nothing to report and is never shown as a scan of zero.
			if in.Scanned {
				s.sourceDiagRow(c, pal, in)
			}
			if in.Explicit() && s.src.editID != in.ID {
				s.sourceControls(c, pal, in)
			}
		})
	})
}

// sourceDiagRow shows what the store recorded about this instance's last
// completed scan. Nothing reads the store here: the record was fetched in the
// background with the configuration, and a frame only draws it. A missing
// record says so instead of reading as zero, a session count the record could
// not state reads "—", and a record whose paths predate a move is labelled as
// last known instead of being presented as evidence about the roots now in
// force.
func (s *State) sourceDiagRow(c *ui.Context, pal palette, in SourceInfo) {
	if !s.src.diagLoaded {
		return
	}
	d, ok := s.src.diag[in.ID]
	if !ok {
		ui.Text(c, tr("srcDiagNone")).FontSize(11).TextColor(pal.muted.Alpha(0.8))
		return
	}
	if diagnosticsHistorical(d, in.Roots) {
		ui.Text(c, tr("srcDiagHistorical")).FontSize(11).TextColor(Soyo)
	}
	when, found := diagnosticsLines(s.Hooks.Now(), d)
	ui.Text(c, when).FontSize(11).TextColor(pal.muted).SingleLine().Ellipsis("…")
	ui.Text(c, found).FontSize(11).TextColor(pal.muted).SingleLine().Ellipsis("…")
}

// sourceControls offers the writes one explicit entry allows. Removing and
// promoting ask for a second click: both change what gets scanned, and
// neither can be undone by reloading the page.
func (s *State) sourceControls(c *ui.Context, pal palette, in SourceInfo) {
	ui.Column(c).Gap(4).Padding(2, 0).Children(func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			if ui.Button(c, tr("srcMove")).Clicked() {
				s.src.editID, s.src.edit, s.src.open, s.src.confirm = in.ID, joinRoots(in.Roots), false, ""
			}
			if in.BackupCopy() {
				if s.src.confirm == "promote:"+in.ID {
					if ui.Button(c, tr("srcConfirmPromote")).Clicked() {
						s.sourcePromote(in.ID)
					}
					if ui.Button(c, tr("cancel")).Clicked() {
						s.src.confirm = ""
					}
				} else if ui.Button(c, tr("srcPromote")).Clicked() {
					s.src.confirm = "promote:" + in.ID
				}
			}
			if s.src.confirm == "remove:"+in.ID {
				if ui.Button(c, tr("srcConfirmRemove")).Clicked() {
					s.sourceRemove(in.ID)
				}
				if ui.Button(c, tr("cancel")).Clicked() {
					s.src.confirm = ""
				}
			} else if ui.Button(c, tr("remove")).Clicked() {
				s.src.confirm = "remove:" + in.ID
			}
		})
		switch s.src.confirm {
		case "promote:" + in.ID:
			ui.Text(c, tr("srcPromoteNote")).FontSize(11.5).TextColor(pal.muted)
		case "remove:" + in.ID:
			ui.Text(c, tr("srcRemoveNote")).FontSize(11.5).TextColor(pal.muted)
		}
	})
}

func (s *State) sourceMoveEditor(c *ui.Context, pal palette) {
	ui.Column(c).Gap(6).Padding(8, 10).Radius(12).Background(pal.well).Children(func() {
		ui.Text(c, fmt.Sprintf(tr("srcMoveTitle"), s.src.editID)).FontSize(12).FontWeight(650).TextColor(pal.ink).SingleLine().Ellipsis("…")
		ui.TextInput(c, &s.src.edit)
		ui.Text(c, tr("srcRootsHint")).FontSize(11).TextColor(pal.muted)
		ui.Row(c).Gap(8).Children(func() {
			if ui.Button(c, tr("srcSave")).Clicked() {
				s.sourceMove(s.src.editID)
			}
			if ui.Button(c, tr("cancel")).Clicked() {
				s.src.editID = ""
			}
		})
	})
}

func (s *State) sourceAddEditor(c *ui.Context, pal palette) {
	opts, _ := harnessOptions()
	if s.src.draft.harness == "" && len(opts) > 0 {
		s.src.draft.harness = opts[0]
	}
	ui.Column(c).Gap(7).Padding(8, 10).Radius(12).Background(pal.well).Children(func() {
		ui.Text(c, tr("srcAddTitle")).FontSize(12).FontWeight(650).TextColor(pal.ink)
		fieldLabel(c, pal, tr("srcHarness"))
		if len(opts) > 0 {
			ui.Select(c, &s.src.draft.harness, opts)
		}
		fieldLabel(c, pal, tr("srcLabel"))
		ui.TextInput(c, &s.src.draft.label)
		fieldLabel(c, pal, tr("srcRoots"))
		ui.TextInput(c, &s.src.draft.roots)
		ui.Text(c, tr("srcRootsHint")).FontSize(11).TextColor(pal.muted)
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Switch(c, &s.src.draft.backup)
			ui.Text(c, tr("srcBackupCopy")).FontSize(12).TextColor(pal.ink)
		})
		ui.Text(c, tr("srcAddKindNote")).FontSize(11.5).TextColor(pal.muted)
		ui.Row(c).Gap(8).Children(func() {
			if ui.Button(c, tr("srcAdd")).Clicked() {
				s.sourceAdd()
			}
			if ui.Button(c, tr("cancel")).Clicked() {
				s.src.open = false
			}
		})
	})
}

// sourceProblemRow shows one resolution problem with what to do about it.
func (s *State) sourceProblemRow(c *ui.Context, pal palette, p SourceProblem) {
	col := Soyo
	if p.Fatal {
		col = Taki
	}
	ui.Row(c).Gap(8).Padding(6, 0).AlignItems(ui.Start).Children(func() {
		dot(c, col, 8)
		ui.Column(c).Grow(1).Basis(0).Gap(2).Children(func() {
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, p.Code).FontSize(11.5).FontWeight(650).TextColor(col)
				if p.ID != "" {
					ui.Text(c, p.ID).FontSize(11).TextColor(pal.muted).SingleLine().Ellipsis("…")
				}
				if p.Harness != "" {
					ui.Text(c, string(p.Harness)).FontSize(11).TextColor(pal.muted)
				}
				if p.Fatal {
					chipOn(c, pal, tr("srcFatal"), Taki)
				}
			})
			ui.Text(c, p.Message).FontSize(11.5).TextColor(pal.ink.Alpha(0.85))
			if p.Path != "" {
				ui.Text(c, shortPath(p.Path)).FontSize(11).TextColor(pal.muted).SingleLine().Ellipsis("…")
			}
			ui.Text(c, sourceHint(p.Code)).FontSize(11).TextColor(pal.muted)
		})
	})
}

func sourceBanner(c *ui.Context, pal palette, text string, col ui.Color) {
	ui.Row(c).Gap(8).Padding(8, 10).Radius(10).Background(col.Alpha(0.14)).AlignItems(ui.Center).Children(func() {
		dot(c, col, 8)
		ui.Text(c, text).FontSize(12).TextColor(col.Mix(pal.ink, 0.35)).Grow(1).Basis(0)
	})
}

func fieldLabel(c *ui.Context, pal palette, s string) {
	ui.Text(c, s).FontSize(11).FontWeight(600).TextColor(pal.muted)
}

// sourceHint is the actionable half of a problem: what the code means for
// the user. Unknown codes still say what to do about them.
func sourceHint(code string) string {
	if _, ok := strs["srcHint_"+code]; ok {
		return tr("srcHint_" + code)
	}
	return tr("srcHintOther")
}

// harnessOption is one entry of the harness picker. The id travels with the
// display name so two harnesses can never be confused for one another.
func harnessOption(h model.Harness) string { return h.DisplayName() + " · " + string(h) }

func harnessOptions() (opts []string, hs []model.Harness) {
	for _, p := range harness.All() {
		h := p.Harness()
		opts = append(opts, harnessOption(h))
		hs = append(hs, h)
	}
	return opts, hs
}

func harnessFromOption(opt string) model.Harness {
	opts, hs := harnessOptions()
	for i, o := range opts {
		if o == opt {
			return hs[i]
		}
	}
	if len(hs) > 0 {
		return hs[0]
	}
	return ""
}

// splitRoots turns what the user typed into roots: one per line, and commas
// or semicolons also separate. Empty parts are dropped.
func splitRoots(text string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(text, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r'
	}) {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func joinRoots(roots []string) string { return strings.Join(roots, "\n") }

// aboutVersion is the version shown in the about card.
func aboutVersion(v string) string {
	if v == "" {
		return "dev"
	}
	return "v" + v
}
