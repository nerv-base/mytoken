package app

import "context"

// StartBudget is the desktop app's budget entry point: it resolves the stats
// timezone on first use and starts evaluating the configured rules. CLI
// commands never call it, so a `mytoken stats` run neither writes the timezone
// nor delivers a notification.
//
// When no fixed calendar can be resolved (no stable IANA name on this machine,
// or an unusable stored value), the budget loop still starts but evaluates
// nothing: no threshold is reserved and nothing is delivered until the user
// chooses a zone through SetTimezone. A missing calendar must not silently
// become the system zone, because that is what a persisted fixed calendar
// exists to prevent.
func (a *App) StartBudget(ctx context.Context) {
	if a == nil || a.Store == nil {
		return
	}
	// A timezone that cannot be used is reported through Timezone/TimezoneState
	// and the app keeps running unconfigured: a bad calendar setting must not
	// stop the app from starting.
	_ = a.EnsureStatsTimezone(ctx)
	if a.Budget != nil {
		a.Budget.Start(ctx)
	}
}
