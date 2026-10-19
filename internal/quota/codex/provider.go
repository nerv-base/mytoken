// Package codex adapts the Codex plan quota into a plan.Provider.
//
// Codex writes rate_limits events into its local rollout logs; package quota
// parses those logs and the scanner publishes the latest observation under the
// settings key quota.codex.<instance>. This provider reads that persisted
// observation, so the dashboard shows exactly the numbers the rest of the app
// already trusts. It never touches the network.
package codex

import (
	"context"
	"fmt"
	"time"

	"github.com/zzstar101/mytoken/internal/quota"
	"github.com/zzstar101/mytoken/internal/quota/plan"
)

// ProviderID is the stable id used in settings keys and by the dashboard.
const ProviderID = "codex"

// Options configures a Provider.
type Options struct {
	// Store is the settings store that holds the persisted Codex reading.
	Store quota.Setting
	// Instance is the Codex instance id; empty means quota.DefaultInstance.
	Instance string
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
}

// Provider reports the Codex plan quota from the persisted rollout reading.
type Provider struct {
	store    quota.Setting
	instance string
	now      func() time.Time
}

// New returns a Codex provider.
func New(opts Options) *Provider {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	instance := opts.Instance
	if instance == "" {
		instance = quota.DefaultInstance
	}
	return &Provider{store: opts.Store, instance: instance, now: now}
}

// ID implements plan.Provider.
func (p *Provider) ID() string { return ProviderID }

// DisplayName implements plan.Provider.
func (p *Provider) DisplayName() string { return "Codex" }

// Local implements plan.LocalProvider: the reading is already on this machine,
// so an offline app still shows it.
func (p *Provider) Local() bool { return true }

// Detect reports whether a Codex quota reading has been persisted.
func (p *Provider) Detect(ctx context.Context) bool {
	if p.store == nil {
		return false
	}
	latest, ok, err := quota.LoadLatest(ctx, p.store, p.instance)
	return err == nil && ok && latest.Report != nil
}

// Fetch converts the persisted reading into a Snapshot.
func (p *Provider) Fetch(ctx context.Context) (plan.Snapshot, error) {
	if p.store == nil {
		return plan.Snapshot{}, plan.NewError(plan.ReasonNoData, nil)
	}
	latest, ok, err := quota.LoadLatest(ctx, p.store, p.instance)
	if err != nil {
		return plan.Snapshot{}, plan.NewError(plan.ReasonSourceUnreadable, err)
	}
	if !ok || latest.Report == nil {
		return plan.Snapshot{}, plan.NewError(plan.ReasonNoData, nil)
	}
	rep := latest.Report
	var windows []plan.Window
	for _, win := range []*quota.Window{rep.Primary, rep.Secondary} {
		if win == nil {
			continue
		}
		if w, ok := convertWindow(win); ok {
			windows = append(windows, w)
		}
	}
	if len(windows) == 0 {
		return plan.Snapshot{}, plan.NewError(plan.ReasonNoWindows, nil)
	}
	fetched := latest.IngestedAt
	if rep.ObservedAt != nil {
		fetched = *rep.ObservedAt
	}
	if fetched.IsZero() {
		fetched = p.now().UTC()
	}
	snap := plan.Snapshot{
		Provider:  ProviderID,
		Windows:   windows,
		FetchedAt: fetched.UTC(),
		Source:    plan.SourceLocalLog,
	}
	if rep.PlanType != nil {
		snap.Plan = *rep.PlanType
	}
	return snap, nil
}

// convertWindow maps one Codex window. A window with neither a used nor a
// remaining percentage is omitted rather than shown as zero.
func convertWindow(win *quota.Window) (plan.Window, bool) {
	used := 0.0
	haveUsed := false
	if win.UsedPercent != nil {
		used = *win.UsedPercent
		haveUsed = true
	} else if win.RemainingPercent != nil {
		used = 100 - *win.RemainingPercent
		haveUsed = true
	}
	if !haveUsed {
		return plan.Window{}, false
	}
	out := plan.Window{
		Name:        windowName(win.Kind, win.WindowMinutes),
		UsedPercent: used,
	}
	if win.ResetsAt != nil {
		out.ResetsAt = win.ResetsAt.UTC()
	}
	return out, true
}

// windowName labels a window by its length, falling back to its kind.
func windowName(kind quota.WindowKind, minutes *int64) string {
	if minutes != nil && *minutes > 0 {
		m := *minutes
		switch {
		case m == 300:
			return "5h"
		case m == 1440:
			return "day"
		case m == 10080:
			return "week"
		case m%1440 == 0:
			return fmt.Sprintf("%dd", m/1440)
		case m%60 == 0:
			return fmt.Sprintf("%dh", m/60)
		default:
			return fmt.Sprintf("%dm", m)
		}
	}
	switch kind {
	case quota.WindowPrimary:
		return "primary"
	case quota.WindowSecondary:
		return "secondary"
	default:
		return string(kind)
	}
}
