package gui

import (
	"context"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/query"
)

// countingSettings counts every pricing load the settings page makes, so a test
// can tell a page entry from a data change.
type countingSettings struct {
	*bigSettings
	priceRules int
	unpriced   int
	providers  int
	aliases    int
}

func (c *countingSettings) PriceRules(ctx context.Context) ([]query.PriceRule, error) {
	c.priceRules++
	return c.bigSettings.PriceRules(ctx)
}

func (c *countingSettings) UnpricedModels(ctx context.Context) ([]query.UnpricedModel, error) {
	c.unpriced++
	return c.bigSettings.UnpricedModels(ctx)
}

func (c *countingSettings) Providers(ctx context.Context) ([]string, error) {
	c.providers++
	return c.bigSettings.Providers(ctx)
}

func (c *countingSettings) ModelAliases(ctx context.Context) ([]query.ModelAlias, error) {
	c.aliases++
	return c.bigSettings.ModelAliases(ctx)
}

func (c *countingSettings) loads() (int, int, int, int) {
	return c.priceRules, c.unpriced, c.providers, c.aliases
}

// TestCommitReloadsDoNotReloadSettingsCards is the regression for the settings
// page stalling while agents write logs: a data change must refresh the cheap
// core, never the pricing catalog or the scope choices. The page still reloads
// them when the user goes there.
func TestCommitReloadsDoNotReloadSettingsCards(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)}
	cs := &countingSettings{bigSettings: makeBigSettings(24, 6, 12)}
	q := &countingQuery{Service: NewDemoService(clock.t), now: clock.now}
	s := bigStateWith(t, q, clock, cs)

	m := &buildMeter{}
	tt := settingsTester(t, s, m)
	s.SetPage("settings")
	tt.Frame()
	if pr, un, pv, al := cs.loads(); pr != 1 || un != 1 || pv != 1 || al != 1 {
		t.Fatalf("entering the settings page loaded pricing %d/%d/%d/%d times, want 1/1/1/1", pr, un, pv, al)
	}

	// Scanner commits: the visible page reloads its core data, at most once per
	// reloadGap, but the settings cards must not be rebuilt.
	for i := 0; i < 6; i++ {
		clock.t = clock.t.Add(3 * time.Second)
		s.changed()
		tt.Frame()
	}
	if pr, un, pv, al := cs.loads(); pr != 1 || un != 1 || pv != 1 || al != 1 {
		t.Fatalf("six commits reloaded the pricing cards %d/%d/%d/%d times, want 1/1/1/1", pr, un, pv, al)
	}
	if q.byProject != 1 || q.providerGroups != 1 {
		t.Fatalf("six commits re-queried the scope choices %d/%d times, want 1/1", q.byProject, q.providerGroups)
	}

	// Leaving and coming back is what reloads them, so the page is never stale
	// for a user who goes there to look.
	s.SetPage("overview")
	tt.Frame()
	s.SetPage("settings")
	tt.Frame()
	if pr, un, pv, al := cs.loads(); pr != 2 || un != 2 || pv != 2 || al != 2 {
		t.Fatalf("re-entering the settings page loaded pricing %d/%d/%d/%d times, want 2/2/2/2", pr, un, pv, al)
	}
}
