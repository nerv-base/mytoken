package gui

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/quota"
	"github.com/zzstar101/mytoken/internal/store"
)

// QuotaStore is the app's own key/value store, read for the quota
// observations the scan already published. It is a local read of what this
// machine recorded: no network call and no scan.
type QuotaStore interface {
	Setting(ctx context.Context, key string) (string, error)
}

// offlineQuotaState is the offline card's state: which instance it is scoped
// to and the last observation stored for it.
type offlineQuotaState struct {
	loaded bool
	insts  []SourceInfo
	inst   string
	latest *quota.Latest
	found  bool
	err    error
	busy   bool
}

// codexInstances lists the Codex instances the card can be scoped to. With no
// source hook the app's own default instance is the only one there is.
func (s *State) codexInstances() []SourceInfo {
	var out []SourceInfo
	if s.Hooks.Sources != nil {
		for _, in := range s.Hooks.Sources.Sources() {
			if in.Harness == model.Codex {
				out = append(out, in)
			}
		}
	}
	if len(out) == 0 {
		out = []SourceInfo{{ID: store.DefaultInstanceID, Harness: model.Codex}}
	}
	return out
}

// loadOfflineQuota reads the stored observation of one instance. A missing
// key is "no data", not zero: the card shows that nothing was recorded
// instead of inventing a number.
func (s *State) loadOfflineQuota() {
	qs := s.Hooks.QuotaStore
	if qs == nil {
		return
	}
	s.oq.insts = s.codexInstances()
	known := false
	for _, in := range s.oq.insts {
		if in.ID == s.oq.inst {
			known = true
		}
	}
	if !known {
		s.oq.inst = s.oq.insts[0].ID
	}
	inst := s.oq.inst
	s.oq.busy = true
	s.run("offline-quota", func(ctx context.Context) func() {
		latest, found, err := quota.LoadLatest(ctx, qs, inst)
		return func() {
			// Scope the completion to the instance the card still shows.
			if s.oq.inst != inst {
				return
			}
			s.oq.busy = false
			s.oq.err, s.oq.latest, s.oq.found = err, latest, found
		}
	})
}

// offlineQuotaCard shows the Codex subscription limits as the local logs
// recorded them, per instance: freshness, the reason when there is none, the
// windows and the credits. It shows no account total and derives no pace.
func (s *State) offlineQuotaCard(c *ui.Context, pal palette) {
	if s.Hooks.QuotaStore == nil {
		return
	}
	if !s.oq.loaded {
		s.oq.loaded = true
		s.loadOfflineQuota()
	}
	card(c, pal, tr("quotaOffline"), func() {
		switch {
		case s.oq.busy:
			chipOn(c, pal, tr("srcWorking"), pal.muted)
		case s.oq.err != nil:
			chipOn(c, pal, tr("quotaCorruptShort"), Soyo)
		case !s.oq.found || s.oq.latest == nil:
			chipOn(c, pal, tr("quotaNoDataShort"), pal.muted)
		default:
			rep := s.oq.latest.Report
			chipOn(c, pal, quotaStatusText(rep.Status), quotaStatusColor(pal, rep.Status))
		}
	}, func() {
		ui.Text(c, tr("quotaOfflineSub")).FontSize(12).TextColor(pal.muted)
		if len(s.oq.insts) > 1 {
			sel := 0
			for i, in := range s.oq.insts {
				if in.ID == s.oq.inst {
					sel = i
				}
			}
			if segmentedGroup(c, pal, &sel, tr("instance"), instanceLabels(s.oq.insts)...) {
				s.oq.inst = s.oq.insts[sel].ID
				s.loadOfflineQuota()
			}
		}
		if s.oq.err != nil {
			if errors.Is(s.oq.err, quota.ErrCorruptStored) {
				sourceBanner(c, pal, tr("quotaCorrupt"), Taki)
			} else {
				sourceBanner(c, pal, s.oq.err.Error(), Taki)
			}
			return
		}
		if !s.oq.found || s.oq.latest == nil {
			ui.Text(c, tr("quotaNoData")).FontSize(12).TextColor(pal.muted)
			ui.Text(c, tr("quotaNoDataHint")).FontSize(11).TextColor(pal.muted)
			return
		}
		rep := s.oq.latest.Report
		ui.Column(c).Gap(3).Children(func() {
			if rep.ObservedAt != nil {
				ui.Text(c, fmt.Sprintf(tr("quotaObserved"), fmtAgo(s.Hooks.Now(), *rep.ObservedAt))).FontSize(11.5).TextColor(pal.muted)
			} else {
				ui.Text(c, tr("quotaNoTimestamp")).FontSize(11.5).TextColor(Soyo)
			}
			if !s.oq.latest.IngestedAt.IsZero() {
				ui.Text(c, fmt.Sprintf(tr("quotaIngested"), fmtAgo(s.Hooks.Now(), s.oq.latest.IngestedAt))).FontSize(11.5).TextColor(pal.muted)
			}
			if s.oq.latest.Source != "" {
				sourceDisclosureRow(c, pal, tr("quotaSource"), s.oq.latest.Source)
			}
			if rep.Reason != "" {
				ui.Text(c, tr("quotaReason")+": "+quotaReasonText(rep.Reason)).FontSize(11.5).TextColor(pal.muted)
			}
		})
		s.offlineQuotaWindow(c, pal, tr("quotaPrimary"), rep.Primary)
		s.offlineQuotaWindow(c, pal, tr("quotaSecondary"), rep.Secondary)
		s.offlineQuotaCredits(c, pal, rep.Credits)
		if len(rep.Issues) > 0 {
			ui.Text(c, fmt.Sprintf(tr("quotaIssues"), len(rep.Issues))).FontSize(11).TextColor(pal.muted)
		}
		if rep.Truncated {
			ui.Text(c, tr("quotaTruncated")).FontSize(11).TextColor(Soyo)
		}
		// The two-point pace forecast of the same retained observations,
		// recomputed at this card's clock so "resets in" stays true.
		now := s.Hooks.Now()
		quotaForecastBlock(c, pal, quota.Forecasts(s.oq.latest, now, quota.ForecastOptions{}), now)
		ui.Text(c, tr("quotaNoTotals")).FontSize(11).TextColor(pal.muted)
	})
}

// offlineQuotaWindow shows one window as recorded. Every missing value stays
// "—"; nothing is interpolated from the other window.
func (s *State) offlineQuotaWindow(c *ui.Context, pal palette, label string, w *quota.Window) {
	if w == nil {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, label).FontSize(12).FontWeight(600).TextColor(pal.ink)
			ui.Spacer(c)
			ui.Text(c, "—").FontSize(12).FontWeight(700).TextColor(pal.muted)
		})
		return
	}
	ui.Column(c).Gap(3).Padding(4, 0).Children(func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, label).FontSize(12).FontWeight(600).TextColor(pal.ink)
			if w.Status != quota.StatusOK {
				chipOn(c, pal, quotaStatusText(w.Status), quotaStatusColor(pal, w.Status))
			}
			ui.Spacer(c)
			ui.Text(c, quotaUsedText(w)).FontSize(12).FontWeight(700).TextColor(pal.ink).FontFeatures("tnum")
		})
		ui.Row(c).Gap(8).Children(func() {
			if w.WindowMinutes != nil {
				ui.Text(c, fmt.Sprintf(tr("quotaWindowMinutes"), *w.WindowMinutes)).FontSize(11).TextColor(pal.muted)
			}
			if w.ResetsAt != nil {
				ui.Text(c, fmt.Sprintf(tr("quotaResetsAt"), fmtWhen(*w.ResetsAt))).FontSize(11).TextColor(pal.muted)
			} else if w.ResetsInSeconds != nil {
				ui.Text(c, fmt.Sprintf(tr("quotaResetsIn"), fmtShortDur(time.Duration(*w.ResetsInSeconds)*time.Second))).FontSize(11).TextColor(pal.muted)
			}
			if w.RemainingPercent != nil && w.RemainingDerived {
				ui.Text(c, tr("quotaRemainingDerived")).FontSize(11).TextColor(pal.muted)
			}
		})
		if w.Reason != "" {
			ui.Text(c, quotaReasonText(w.Reason)).FontSize(11).TextColor(pal.muted)
		}
	})
}

// quotaUsedText is "—" when the record stated no utilization.
func quotaUsedText(w *quota.Window) string {
	if w.UsedPercent == nil {
		return "—"
	}
	return fmt.Sprintf(tr("quotaUsedPct"), trimNum(*w.UsedPercent, 1)+"%")
}

// offlineQuotaCredits shows what the record said about credits. The balance
// is printed exactly as recorded: its unit is not stated by the log, and this
// card does not guess one.
func (s *State) offlineQuotaCredits(c *ui.Context, pal palette, cr *quota.Credits) {
	if cr == nil {
		return
	}
	ui.Column(c).Gap(3).Children(func() {
		ui.Text(c, tr("quotaCredits")).FontSize(12).FontWeight(600).TextColor(pal.ink)
		if cr.HasCredits != nil {
			if *cr.HasCredits {
				ui.Text(c, tr("quotaCreditsHas")).FontSize(11.5).TextColor(pal.muted)
			} else {
				ui.Text(c, tr("quotaCreditsNone")).FontSize(11.5).TextColor(pal.muted)
			}
		}
		if cr.Unlimited != nil && *cr.Unlimited {
			ui.Text(c, tr("quotaCreditsUnlimited")).FontSize(11.5).TextColor(pal.muted)
		}
		if cr.Balance != nil {
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, fmt.Sprintf(tr("quotaBalance"), *cr.Balance)).FontSize(11.5).TextColor(pal.ink.Alpha(0.9)).FontFeatures("tnum")
				chipOn(c, pal, tr("quotaUnitsUnstated"), pal.muted)
			})
		}
	})
}

func quotaStatusText(st quota.Status) string {
	switch st {
	case quota.StatusOK:
		return tr("quotaOK")
	case quota.StatusStale:
		return tr("quotaStale")
	case quota.StatusUnknown:
		return tr("quotaUnknown")
	case quota.StatusMalformed:
		return tr("quotaMalformed")
	case quota.StatusUnavailable:
		return tr("quotaUnavailable")
	}
	if st == "" {
		return tr("quotaUnknown")
	}
	return string(st)
}

func quotaStatusColor(pal palette, st quota.Status) ui.Color {
	switch st {
	case quota.StatusOK:
		return Anon
	case quota.StatusStale:
		return Soyo
	case quota.StatusMalformed, quota.StatusUnavailable:
		return pal.muted
	}
	return pal.muted
}

// quotaReasonText names a recorded reason. Unmapped reasons keep their token
// so a new one is visible rather than silently dropped.
func quotaReasonText(r quota.Reason) string {
	switch r {
	case quota.ReasonNoData:
		return tr("quotaReasonNoData")
	case quota.ReasonMissing:
		return tr("quotaReasonMissing")
	case quota.ReasonMalformed:
		return tr("quotaReasonMalformed")
	case quota.ReasonStale:
		return tr("quotaReasonStale")
	case quota.ReasonNoTimestamp:
		return tr("quotaReasonNoTimestamp")
	case quota.ReasonNewerInvalid:
		return tr("quotaReasonNewerInvalid")
	case quota.ReasonResetUnknown, quota.ReasonResetRollover, quota.ReasonResetMismatch:
		return tr("quotaReasonReset")
	case quota.ReasonWindowMismatch:
		return tr("quotaReasonWindowMismatch")
	case quota.ReasonLimitMismatch:
		return tr("quotaReasonLimitMismatch")
	case quota.ReasonZeroElapsed:
		return tr("quotaReasonZeroElapsed")
	case quota.ReasonDecreasingUsage:
		return tr("quotaReasonDecreasing")
	case quota.ReasonOutlier:
		return tr("quotaReasonOutlier")
	case quota.ReasonNoObservation:
		return tr("quotaReasonNoObservation")
	case quota.ReasonInsufficientSamples:
		return tr("quotaReasonInsufficient")
	case quota.ReasonFutureObservation:
		return tr("quotaReasonFuture")
	case quota.ReasonSourceMismatch:
		return tr("quotaReasonSourceMismatch")
	}
	if r == "" {
		return ""
	}
	return fmt.Sprintf("%s (%s)", tr("quotaReasonOther"), string(r))
}

// fmtShortDur is a coarse "how long from now" for a recorded countdown.
func fmtShortDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return trimNum(d.Hours(), 1) + "h"
	default:
		return trimNum(d.Hours()/24, 1) + "d"
	}
}
