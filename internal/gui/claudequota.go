package gui

import (
	"context"
	"fmt"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/claudequota"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

// ClaudeQuota is the app's opt-in Claude subscription-quota service. Every
// call names one instance. Enable is the only call that records consent and
// the only one that can lead to a request; Refresh is the only other request,
// and it happens when the user asks for it. Info and Snapshot never leave the
// machine: they read what was already stored.
type ClaudeQuota interface {
	Info(ctx context.Context, instance string) (claudequota.Info, error)
	Sources(instance string) []claudequota.Source
	Enable(ctx context.Context, instance, ref string) (claudequota.Info, error)
	Disable(ctx context.Context, instance string) error
	Snapshot(ctx context.Context, instance string) (claudequota.Quota, error)
	Refresh(ctx context.Context, instance string) (claudequota.Quota, error)
}

// claudeQuotaState is the panel's own state: which instance it is scoped to,
// which credential source the user picked ("" until they pick one, so nothing
// is ever enabled by default), and the last reading.
type claudeQuotaState struct {
	loaded bool
	insts  []SourceInfo
	inst   string
	refs   []string
	sel    string
	info   claudequota.Info
	quota  claudequota.Quota
	haveQ  bool
	err    error
	busy   bool
	msg    string
	seq    int
}

// claudeInstances lists the Claude Code instances the panel can be scoped to.
// With no source hook the app's own default instance is the only one there is.
func (s *State) claudeInstances() []SourceInfo {
	var out []SourceInfo
	if s.Hooks.Sources != nil {
		for _, in := range s.Hooks.Sources.Sources() {
			if in.Harness == model.ClaudeCode {
				out = append(out, in)
			}
		}
	}
	if len(out) == 0 {
		out = []SourceInfo{{ID: store.DefaultInstanceID, Harness: model.ClaudeCode}}
	}
	return out
}

func (s *State) loadClaudeQuota() {
	q := s.Hooks.ClaudeQuota
	if q == nil {
		return
	}
	s.cq.insts = s.claudeInstances()
	if !s.claudeInstanceKnown(s.cq.inst) {
		s.cq.inst = s.cq.insts[0].ID
		s.cq.sel = ""
	}
	inst := s.cq.inst
	s.run("claude-quota", func(ctx context.Context) func() {
		info, err := q.Info(ctx, inst)
		if err != nil {
			return func() {
				if s.cq.inst == inst {
					s.cq.err = err
				}
			}
		}
		quota, qerr := q.Snapshot(ctx, inst)
		srcs := q.Sources(inst)
		refs := make([]string, 0, len(srcs))
		for _, src := range srcs {
			refs = append(refs, src.Ref)
		}
		return func() {
			// Scope the completion to the instance the panel still shows: a
			// reading for another one must not land here.
			if s.cq.inst != inst {
				return
			}
			s.cq.err = nil
			s.cq.info = info
			s.cq.refs = refs
			s.cq.haveQ = qerr == nil
			if qerr == nil {
				s.cq.quota = quota
			} else {
				s.cq.quota = info.Quota
			}
			if !containsString(refs, s.cq.sel) {
				s.cq.sel = ""
			}
		}
	})
}

func (s *State) claudeInstanceKnown(id string) bool {
	for _, in := range s.cq.insts {
		if in.ID == id {
			return true
		}
	}
	return false
}

// claudeQuotaWrite runs one consent-changing call and re-reads the cached
// state. The completion is dropped when the panel has moved on.
func (s *State) claudeQuotaWrite(kind, done string, act func(ctx context.Context) (claudequota.Info, error)) {
	q := s.Hooks.ClaudeQuota
	if q == nil {
		return
	}
	inst := s.cq.inst
	s.cq.busy = true
	s.cq.msg = ""
	s.cq.seq++
	seq := s.cq.seq
	s.run(kind, func(ctx context.Context) func() {
		info, err := act(ctx)
		var quota claudequota.Quota
		var qerr error
		if err == nil {
			quota, qerr = q.Snapshot(ctx, inst)
		}
		return func() {
			if s.cq.seq != seq || s.cq.inst != inst {
				return
			}
			s.cq.busy = false
			if err != nil {
				s.cq.err = err
				return
			}
			s.cq.err = nil
			s.cq.info = info
			if qerr == nil {
				s.cq.quota, s.cq.haveQ = quota, true
			} else {
				s.cq.quota, s.cq.haveQ = info.Quota, false
			}
			s.cq.msg = tr(done)
		}
	})
}

func (s *State) claudeQuotaEnable() {
	inst, ref := s.cq.inst, s.cq.sel
	s.claudeQuotaWrite("claude-enable", "claudeQuotaEnabledMsg", func(ctx context.Context) (claudequota.Info, error) {
		return s.Hooks.ClaudeQuota.Enable(ctx, inst, ref)
	})
}

// claudeQuotaDisable revokes consent and stops the automatic requests at
// once; the reading it had cached is cleared with it.
func (s *State) claudeQuotaDisable() {
	inst := s.cq.inst
	s.claudeQuotaWrite("claude-disable", "claudeQuotaDisabledMsg", func(ctx context.Context) (claudequota.Info, error) {
		if err := s.Hooks.ClaudeQuota.Disable(ctx, inst); err != nil {
			return claudequota.Info{}, err
		}
		return s.Hooks.ClaudeQuota.Info(ctx, inst)
	})
}

// claudeQuotaRefresh asks the endpoint now. Only the user starts this: the
// card never refreshes on its own.
func (s *State) claudeQuotaRefresh() {
	inst := s.cq.inst
	s.claudeQuotaWrite("claude-refresh", "claudeQuotaRefreshed", func(ctx context.Context) (claudequota.Info, error) {
		if _, err := s.Hooks.ClaudeQuota.Refresh(ctx, inst); err != nil {
			return claudequota.Info{}, err
		}
		return s.Hooks.ClaudeQuota.Info(ctx, inst)
	})
}

// claudeQuotaCard is the opt-in panel. Nothing is enabled until the user has
// read the disclosure, picked a credential source and pressed Enable; the
// card never picks a source for them.
func (s *State) claudeQuotaCard(c *ui.Context, pal palette) {
	if s.Hooks.ClaudeQuota == nil {
		return
	}
	if !s.cq.loaded {
		s.cq.loaded = true
		s.loadClaudeQuota()
	}
	card(c, pal, tr("claudeQuota"), func() {
		if s.cq.busy {
			chipOn(c, pal, tr("srcWorking"), pal.muted)
			return
		}
		if s.cq.info.Enabled {
			chipOn(c, pal, tr("claudeQuotaEnabled"), Anon)
		} else {
			chipOn(c, pal, tr("claudeQuotaDisabled"), pal.muted)
		}
	}, func() {
		ui.Text(c, tr("claudeQuotaSub")).FontSize(12).TextColor(pal.muted)
		if len(s.cq.insts) > 1 {
			sel := 0
			for i, in := range s.cq.insts {
				if in.ID == s.cq.inst {
					sel = i
				}
			}
			if segmentedGroup(c, pal, &sel, tr("instance"), instanceLabels(s.cq.insts)...) {
				s.cq.inst, s.cq.sel, s.cq.msg, s.cq.err = s.cq.insts[sel].ID, "", "", nil
				s.loadClaudeQuota()
			}
		}
		if s.cq.err != nil {
			sourceBanner(c, pal, s.cq.err.Error(), Taki)
		}
		if s.cq.msg != "" {
			sourceBanner(c, pal, s.cq.msg, Anon)
		}
		if !s.cq.info.Enabled {
			s.claudeQuotaConsent(c, pal)
			return
		}
		s.claudeQuotaReading(c, pal)
		// What the consent actually covers, in the same words it was granted
		// with: the exact source and the exact target.
		sourceDisclosureRow(c, pal, tr("claudeQuotaRef"), s.cq.info.SourceRef)
		sourceDisclosureRow(c, pal, tr("claudeQuotaTarget"), s.cq.info.Target)
		if !s.cq.info.GrantedAt.IsZero() {
			sourceDisclosureRow(c, pal, tr("claudeQuotaGranted"), fmtWhen(s.cq.info.GrantedAt))
		}
		ui.Text(c, tr("claudeQuotaCached")).FontSize(11).TextColor(pal.muted)
		ui.Row(c).Gap(8).Children(func() {
			if ui.Button(c, tr("claudeQuotaRefresh")).Clicked() {
				s.claudeQuotaRefresh()
			}
			if ui.Button(c, tr("claudeQuotaDisable")).Clicked() {
				s.claudeQuotaDisable()
			}
		})
	})
}

// claudeQuotaConsent is the disclosure shown before anything is enabled: the
// exact credential sources, the exact target, and what consent means.
func (s *State) claudeQuotaConsent(c *ui.Context, pal palette) {
	ui.Text(c, tr("claudeQuotaDisclose")).FontSize(11.5).FontWeight(650).TextColor(pal.ink)
	sourceDisclosureRow(c, pal, tr("claudeQuotaTarget"), claudequota.Target)
	ui.Text(c, tr("claudeQuotaScope")).FontSize(11.5).TextColor(pal.muted)
	if len(s.cq.refs) == 0 {
		ui.Text(c, tr("claudeQuotaNoSources")).FontSize(12).TextColor(Soyo)
		return
	}
	ui.Text(c, tr("claudeQuotaPickHint")).FontSize(11.5).TextColor(pal.muted)
	for _, src := range s.Hooks.ClaudeQuota.Sources(s.cq.inst) {
		s.claudeQuotaSourceRow(c, pal, src)
	}
	if ui.Button(c, tr("claudeQuotaEnable")).Clicked() {
		if s.cq.sel == "" {
			s.cq.msg = tr("claudeQuotaPick")
			return
		}
		s.claudeQuotaEnable()
	}
}

// claudeQuotaSourceRow is one credential source. The row's accessible name is
// the exact reference, so a screen reader reads the path or keychain item that
// enabling would use.
func (s *State) claudeQuotaSourceRow(c *ui.Context, pal palette, src claudequota.Source) {
	on := s.cq.sel == src.Ref
	usable := src.Read != nil
	row := ui.Row(c.Key("cq-"+src.Ref)).Gap(8).Padding(6, 8).Radius(9).AlignItems(ui.Center).Label(src.Ref).Transition(hoverFade)
	if usable {
		row.Cursor(ui.CursorPointer).Role(ui.RoleButton)
	}
	if on {
		row.Background(pal.hover).Border(1, pal.edge)
	} else if usable && row.Hovered() {
		row.Background(pal.hover)
	}
	row.Children(func() {
		dot(c, claudeKindColor(src.Kind), 8)
		ui.Column(c).Grow(1).Basis(0).Gap(2).Children(func() {
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, claudeSourceTitle(src)).FontSize(12).FontWeight(600).TextColor(pal.ink).SingleLine().Ellipsis("…").Shrink(1)
				if !usable {
					chipOn(c, pal, tr("claudeQuotaUnsupported"), Soyo)
				}
				if on {
					chipOn(c, pal, tr("claudeQuotaChosen"), Anon)
				}
			})
			ui.Text(c, src.Ref).FontSize(11).TextColor(pal.muted).SingleLine().Ellipsis("…")
		})
	})
	if usable && row.Clicked() {
		s.cq.sel = src.Ref
		s.cq.msg = ""
	}
}

// claudeQuotaReading shows the cached reading. A window the payload did not
// carry stays "—": absent is not zero, and a stale reading says so.
func (s *State) claudeQuotaReading(c *ui.Context, pal palette) {
	q := s.cq.quota
	status := q.Status
	if status == "" {
		status = claudequota.StatusUnavailable
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		chipOn(c, pal, claudeStatusText(status), claudeStatusColor(pal, status))
		if q.FetchedAt.IsZero() {
			ui.Text(c, tr("claudeQuotaNever")).FontSize(11.5).TextColor(pal.muted)
		} else {
			ui.Text(c, fmt.Sprintf(tr("claudeQuotaFetched"), fmtAgo(s.Hooks.Now(), q.FetchedAt))).FontSize(11.5).TextColor(pal.muted)
		}
	})
	if q.Reason != "" {
		ui.Text(c, tr("claudeQuotaReason")+": "+claudeReasonText(q.Reason)).FontSize(11.5).TextColor(pal.muted)
	}
	if len(q.Windows) == 0 {
		ui.Text(c, tr("claudeQuotaNoWindows")).FontSize(11.5).TextColor(pal.muted)
	} else {
		for _, w := range q.Windows {
			s.claudeQuotaWindow(c, pal, w)
		}
	}
	// The reading's retained samples carry a two-point pace forecast. It is
	// recomputed at this card's clock rather than read from the stored
	// snapshot, so the rate and the reset countdown describe the same instant.
	now := s.Hooks.Now()
	quotaForecastBlock(c, pal, claudequota.Forecasts(q, now), now)
}

func (s *State) claudeQuotaWindow(c *ui.Context, pal palette, w claudequota.Window) {
	ui.Column(c).Gap(3).Padding(4, 0).Children(func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, claudeWindowText(w)).FontSize(12).FontWeight(600).TextColor(pal.ink)
			ui.Spacer(c)
			ui.Text(c, claudeUsedText(w)).FontSize(12).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum")
		})
		ui.Row(c).Gap(8).Children(func() {
			if w.WindowMinutes != nil {
				ui.Text(c, fmt.Sprintf(tr("claudeQuotaMinutes"), *w.WindowMinutes)).FontSize(11).TextColor(pal.muted)
			}
			if w.ResetsAt != nil {
				ui.Text(c, fmt.Sprintf(tr("claudeQuotaResets"), fmtWhen(*w.ResetsAt))).FontSize(11).TextColor(pal.muted)
			}
			if w.RemainingPercent != nil {
				ui.Text(c, fmt.Sprintf(tr("claudeQuotaRemaining"), trimNum(*w.RemainingPercent, 1)+"%")).FontSize(11).TextColor(pal.muted)
			}
		})
	})
}

// claudeUsedText is "—" whenever the payload did not state a utilization:
// a missing window must never read as 0% used.
func claudeUsedText(w claudequota.Window) string {
	if !w.Present || w.UsedPercent == nil {
		return "—"
	}
	return fmt.Sprintf(tr("claudeQuotaUsed"), trimNum(*w.UsedPercent, 1)+"%")
}

func claudeWindowText(w claudequota.Window) string {
	switch w.Kind {
	case "five_hour":
		return tr("claudeQuotaFiveHour")
	case "seven_day":
		return tr("claudeQuotaSevenDay")
	case "seven_day_opus":
		return tr("claudeQuotaSevenDayOpus")
	case "seven_day_sonnet":
		return tr("claudeQuotaSevenDaySonnet")
	}
	return w.Kind
}

func claudeSourceTitle(src claudequota.Source) string {
	name := src.Label
	if name == "" {
		name = src.Instance
	}
	if src.Kind == claudequota.SourceKeychain {
		return name + " · " + tr("claudeQuotaKeychain")
	}
	return name + " · " + tr("claudeQuotaFile")
}

func claudeKindColor(k claudequota.SourceKind) ui.Color {
	if k == claudequota.SourceKeychain {
		return Rana
	}
	return Anon
}

func claudeStatusText(st claudequota.Status) string {
	switch st {
	case claudequota.StatusOK:
		return tr("claudeQuotaOK")
	case claudequota.StatusStale:
		return tr("claudeQuotaStale")
	default:
		return tr("claudeQuotaUnavailable")
	}
}

func claudeStatusColor(pal palette, st claudequota.Status) ui.Color {
	switch st {
	case claudequota.StatusOK:
		return Anon
	case claudequota.StatusStale:
		return Soyo
	default:
		return pal.muted
	}
}

// claudeReasonText names a reason. An unmapped one still says what it is
// rather than hiding it, and never carries a credential: reasons are fixed
// tokens, not server text.
func claudeReasonText(r claudequota.Reason) string {
	if r == "" {
		return ""
	}
	if _, ok := strs["claudeQuotaReason_"+string(r)]; ok {
		return tr("claudeQuotaReason_" + string(r))
	}
	return fmt.Sprintf("%s (%s)", tr("claudeQuotaReasonOther"), string(r))
}

// sourceDisclosureRow is one label/value line of a disclosure.
func sourceDisclosureRow(c *ui.Context, pal palette, label, value string) {
	if value == "" {
		value = "—"
	}
	ui.Row(c).Gap(8).AlignItems(ui.Start).Children(func() {
		ui.Text(c, label).FontSize(11.5).TextColor(pal.muted).Width(88).Shrink(0)
		ui.Text(c, value).FontSize(11.5).TextColor(pal.ink.Alpha(0.9)).Grow(1).Basis(0).SingleLine().Ellipsis("…")
	})
}

func instanceLabels(insts []SourceInfo) []string {
	out := make([]string, 0, len(insts))
	for _, in := range insts {
		name := in.Label
		if name == "" {
			name = string(in.Harness)
		}
		out = append(out, name)
	}
	return out
}

func containsString(list []string, want string) bool {
	if want == "" {
		return false
	}
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
