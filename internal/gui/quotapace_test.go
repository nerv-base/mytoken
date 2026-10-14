package gui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/claudequota"
	"github.com/zzstar101/mytoken/internal/quota"
)

// paceUnavailablePrefix is the "no pace" message without its reason suffix, so
// a test can assert on it without restating how the reason is formatted.
func paceUnavailablePrefix() string {
	msg := tr("quotaPaceUnavailable")
	if i := strings.Index(msg, "%"); i >= 0 {
		return msg[:i]
	}
	return msg
}

// Two observations of one window two hours apart are a real rate; the block
// must show the measured pace, the even-use reference, the exhaustion
// projection and the model's limitations instead of withholding a number.
func TestQuotaForecastBlockShowsTwoPointPace(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.UTC)
	reset := now.Add(24 * time.Hour)
	obs := []quota.Observation{
		{Window: quota.WindowPrimary, ObservedAt: now.Add(-2 * time.Hour), UsedPercent: 10, RemainingPercent: ptrTo(90.0), ResetsAt: &reset},
		{Window: quota.WindowPrimary, ObservedAt: now, UsedPercent: 30, RemainingPercent: ptrTo(70.0), ResetsAt: &reset},
	}
	f := quota.ForecastObservations(obs, now, quota.ForecastOptions{})
	if !f.Available || len(f.Windows) != 1 || !f.Windows[0].Pace.Available {
		t.Fatalf("two samples two hours apart must be forecastable: %+v", f)
	}
	p := f.Windows[0].Pace
	if p.PercentPerHour != 10 {
		t.Fatalf("rate = %v%%/h, want 10%%/h", p.PercentPerHour)
	}
	if p.ExhaustsAt == nil || !p.ExhaustsAt.Equal(now.Add(7*time.Hour)) {
		t.Fatalf("exhaustion = %v, want %s (70%% left at 10%%/h)", p.ExhaustsAt, now.Add(7*time.Hour))
	}
	if p.ResetsBeforeExhaustion {
		t.Fatal("a reset 24h away does not preempt exhaustion in 7h")
	}

	tt := renderPanel(t, 900, 700, func(c *ui.Context, pal palette) { quotaForecastBlock(c, pal, f, now) })
	for _, want := range []string{
		tr("quotaPaceTitle"),
		"10%/h",
		"7h",
		fmtRate(p.EvenPercentPerHour),
		tr("quotaLimitBursty"), tr("quotaLimitReset"), tr("quotaLimitExhaust"),
	} {
		if !tt.HasText(want) {
			t.Errorf("the forecast block does not show %q", want)
		}
	}
	if tt.HasText(paceUnavailablePrefix()) {
		t.Error("an available forecast must not read as unavailable")
	}
}

// One observation is not a rate: the block says why no pace is stated instead
// of leaving the reader to guess, and the same holds for a pair that went down.
func TestQuotaForecastBlockExplainsUnavailableSamples(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.UTC)

	one := quota.ForecastObservations([]quota.Observation{
		{Window: quota.WindowPrimary, ObservedAt: now, UsedPercent: 10},
	}, now, quota.ForecastOptions{})
	if one.Available || one.Reason != quota.ReasonInsufficientSamples {
		t.Fatalf("one sample = %+v, want insufficient samples", one)
	}
	tt := renderPanel(t, 900, 700, func(c *ui.Context, pal palette) { quotaForecastBlock(c, pal, one, now) })
	if !tt.HasText(tr("quotaReasonInsufficient")) {
		t.Errorf("a one-sample window must explain itself: want %q", tr("quotaReasonInsufficient"))
	}

	down := quota.ForecastObservations([]quota.Observation{
		{Window: quota.WindowPrimary, ObservedAt: now.Add(-time.Hour), UsedPercent: 30},
		{Window: quota.WindowPrimary, ObservedAt: now, UsedPercent: 20},
	}, now, quota.ForecastOptions{})
	if down.Available {
		t.Fatalf("a decreasing pair must not read as a pace: %+v", down)
	}
	text := renderPanel(t, 900, 700, func(c *ui.Context, pal palette) { quotaForecastBlock(c, pal, down, now) })
	if !text.HasText(paceUnavailablePrefix()) {
		t.Error("an unavailable forecast must say so")
	}
}

// The offline card derives its pace from the reading's retained observations:
// the blanket "no pace is inferred" claim is gone and the rate is shown.
func TestOfflineQuotaCardShowsThePaceItCanDerive(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.UTC)
	reset := now.Add(24 * time.Hour)
	obs := []quota.Observation{
		{Window: quota.WindowPrimary, ObservedAt: now.Add(-2 * time.Hour), UsedPercent: 10, RemainingPercent: ptrTo(90.0), ResetsAt: &reset},
		{Window: quota.WindowPrimary, ObservedAt: now, UsedPercent: 30, RemainingPercent: ptrTo(70.0), ResetsAt: &reset},
	}
	rep := &quota.Report{
		Source:     "codex",
		ObservedAt: &now,
		Status:     quota.StatusOK,
		Primary:    &quota.Window{Kind: quota.WindowPrimary, Status: quota.StatusOK, UsedPercent: ptrTo(30.0), RemainingPercent: ptrTo(70.0)},
	}
	raw, err := json.Marshal(quota.Latest{Harness: "codex", Instance: "default", Source: "log", IngestedAt: now, Report: rep, Observations: obs})
	if err != nil {
		t.Fatal(err)
	}
	s := NewState(NewDemoService(now), Hooks{
		QuotaStore: fakeQuotaStore{raw: string(raw)},
		Now:        func() time.Time { return now },
	}, nil)
	s.oq.inst = "default"
	tt := renderPanel(t, 900, 900, func(c *ui.Context, pal palette) { s.offlineQuotaCard(c, pal) })

	for _, want := range []string{"10%/h", tr("quotaPaceTitle"), tr("quotaLimitBursty"), tr("quotaNoTotals")} {
		if !tt.HasText(want) {
			t.Errorf("the offline card does not show %q", want)
		}
	}

	// One sample: the card must state the reason rather than a rate.
	one, err := json.Marshal(quota.Latest{
		Harness: "codex", Instance: "default", IngestedAt: now, Report: rep,
		Observations: []quota.Observation{{Window: quota.WindowPrimary, ObservedAt: now, UsedPercent: 30, RemainingPercent: ptrTo(70.0)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s2 := NewState(NewDemoService(now), Hooks{
		QuotaStore: fakeQuotaStore{raw: string(one)},
		Now:        func() time.Time { return now },
	}, nil)
	s2.oq.inst = "default"
	thin := renderPanel(t, 900, 900, func(c *ui.Context, pal palette) { s2.offlineQuotaCard(c, pal) })
	if !thin.HasText(tr("quotaReasonInsufficient")) {
		t.Errorf("a one-sample record must explain the missing rate: want %q", tr("quotaReasonInsufficient"))
	}
}

// The Claude panel recomputes the reading's pace at its own clock, so a reading
// with two retained samples shows the same measured rate.
func TestClaudeQuotaReadingShowsThePaceItCanDerive(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.UTC)
	reset := now.Add(5 * time.Hour)
	obs := []quota.Observation{
		{Window: quota.WindowPrimary, ObservedAt: now.Add(-time.Hour), UsedPercent: 20, RemainingPercent: ptrTo(80.0), ResetsAt: &reset},
		{Window: quota.WindowPrimary, ObservedAt: now, UsedPercent: 40, RemainingPercent: ptrTo(60.0), ResetsAt: &reset},
	}
	q := claudequota.Quota{
		Status:       claudequota.StatusOK,
		FetchedAt:    now,
		SourceRef:    "keychain",
		Observations: obs,
	}
	s := NewState(NewDemoService(now), Hooks{Now: func() time.Time { return now }}, nil)
	s.cq.quota = q
	s.cq.haveQ = true
	tt := renderPanel(t, 900, 700, func(c *ui.Context, pal palette) { s.claudeQuotaReading(c, pal) })

	if !tt.HasText("20%/h") {
		t.Errorf("the Claude panel does not show the measured rate; texts: %v", tt.Texts())
	}
	if !tt.HasText(tr("quotaPaceTitle")) {
		t.Errorf("the Claude panel does not show the pace block: want %q", tr("quotaPaceTitle"))
	}
}
