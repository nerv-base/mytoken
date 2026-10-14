package gui

import (
	"fmt"
	"strconv"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/quota"
)

// quotaForecastBlock shows the two-point pace forecast of a stored quota
// reading: the measured rate, the even-use reference it can be compared with,
// the projected exhaustion, and the model's own limitations. A window whose
// pair was not comparable shows the reason instead, so "no rate" is always
// explained rather than asserted.
//
// now is the instant the forecast was recomputed at; the caller passes its own
// clock so the card and the CLI agree.
func quotaForecastBlock(c *ui.Context, pal palette, f *quota.Forecast, now time.Time) {
	if f == nil {
		return
	}
	if len(f.Windows) == 0 {
		// Nothing was retained at all: say so once rather than leaving the
		// card silent about the pace it cannot show.
		if f.Reason != "" {
			ui.Text(c, fmt.Sprintf(tr("quotaPaceUnavailable"), quotaReasonText(f.Reason))).
				FontSize(11).TextColor(pal.muted)
		}
		return
	}
	ui.Column(c).Gap(3).Children(func() {
		ui.Text(c, tr("quotaPaceTitle")).FontSize(12).FontWeight(600).TextColor(pal.ink)
		if f.ObservedAt != nil {
			ui.Text(c, fmt.Sprintf(tr("quotaPaceFrom"), fmtAgo(now, *f.ObservedAt), observationsText(f))).
				FontSize(11).TextColor(pal.muted)
		}
		for _, w := range f.Windows {
			quotaPaceWindow(c, pal, w, len(f.Windows) > 1)
		}
		// The caveats are data from the calculation, not prose: a new one
		// shows up here without this card having to know it.
		for _, l := range f.Limitations {
			ui.Text(c, "· "+quotaLimitationText(l)).FontSize(10.5).TextColor(pal.muted)
		}
	})
}

// quotaPaceWindow draws one window's forecast.
func quotaPaceWindow(c *ui.Context, pal palette, w quota.WindowForecast, label bool) {
	head := func() {
		if label {
			ui.Text(c, quotaWindowKindText(w.Window)).FontSize(11.5).FontWeight(600).TextColor(pal.ink)
		}
	}
	if !w.Pace.Available {
		ui.Row(c).Gap(6).Children(func() {
			head()
			ui.Text(c, fmt.Sprintf(tr("quotaPaceUnavailable"), quotaReasonText(w.Reason))).
				FontSize(11).TextColor(pal.muted)
		})
		return
	}
	ui.Column(c).Gap(2).Children(func() {
		ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
			head()
			ui.Text(c, fmt.Sprintf(tr("quotaPaceRate"), fmtRate(w.Pace.PercentPerHour), fmtShortDur(w.Pace.Elapsed))).
				FontSize(11.5).TextColor(pal.ink).FontFeatures("tnum")
		})
		switch {
		case w.Pace.PercentPerHour == 0:
			ui.Text(c, tr("quotaPaceIdle")).FontSize(11).TextColor(pal.muted)
		case w.Pace.ResetsBeforeExhaustion:
			ui.Text(c, tr("quotaPaceResetsFirst")).FontSize(11).TextColor(pal.muted)
		case w.Pace.ExhaustsAt != nil:
			ui.Text(c, fmt.Sprintf(tr("quotaPaceExhausts"), fmtShortDur(w.Pace.TimeToExhaustion), fmtWhen(*w.Pace.ExhaustsAt))).
				FontSize(11).TextColor(pal.ink.Alpha(0.9)).FontFeatures("tnum")
		}
		if w.Pace.EvenPercentPerHour > 0 {
			ui.Text(c, fmt.Sprintf(tr("quotaPaceEven"), fmtRate(w.Pace.EvenPercentPerHour))).
				FontSize(11).TextColor(pal.muted)
		}
		// A pair that is stale or dated in the future still carries its rate,
		// but is not actionable: say why next to the number.
		if !w.Available {
			ui.Text(c, fmt.Sprintf(tr("quotaPaceNotActionable"), quotaReasonText(w.Reason))).
				FontSize(11).TextColor(Soyo)
		}
	})
}

// observationsText is "2 samples" spelled out, so "one observation is not a
// rate" is checkable by the reader.
func observationsText(f *quota.Forecast) string {
	n := 0
	for _, w := range f.Windows {
		if w.Observations > n {
			n = w.Observations
		}
	}
	return fmt.Sprintf(tr("quotaPaceSamples"), n)
}

// quotaWindowKindText names a forecast window. An unknown kind keeps its token.
func quotaWindowKindText(k quota.WindowKind) string {
	switch k {
	case quota.WindowPrimary:
		return tr("quotaPrimary")
	case quota.WindowSecondary:
		return tr("quotaSecondary")
	}
	if k == "" {
		return tr("quotaUnknown")
	}
	return string(k)
}

// quotaLimitationText translates the fixed caveats the calculation publishes.
// Matching is by content, so a caveat this build does not know is shown as the
// calculation wrote it instead of being dropped or mislabelled.
func quotaLimitationText(l string) string {
	switch l {
	case "pace is a straight line through the two most recent samples, so bursty usage is not modelled":
		return tr("quotaLimitBursty")
	case "a window reset discards the sample pair, so no forecast is stated until a sample from the new period arrives":
		return tr("quotaLimitReset")
	case "exhaustion is projected only while usage rises and the projection lands before the reset; otherwise the reset is reported instead":
		return tr("quotaLimitExhaust")
	}
	return l
}

// fmtRate renders a percent-per-hour rate. A very small rate keeps enough
// digits to stay non-zero, so "0%/h" never stands in for "barely moving".
func fmtRate(pct float64) string {
	switch {
	case pct == 0:
		return "0%/h"
	case pct < 0:
		pct = 0
		return "0%/h"
	case pct < 0.01:
		return strconv.FormatFloat(pct, 'f', -1, 64) + "%/h"
	}
	return trimNum(pct, 2) + "%/h"
}
