package gui

import (
	"context"
	"strconv"
	"time"
)

// More preference keys (see data.go for the first ones).
const (
	// PrefTimezone is the IANA zone days and months are counted in; ""
	// follows the system. The adapter applies it to the data layer when it
	// is set, before the GUI reloads.
	PrefTimezone = "ui.timezone"
	// PrefCurrency is the display currency code; "" or "USD" shows prices
	// as they are published. PrefFXRate is units of it per US dollar.
	PrefCurrency = "ui.currency"
	PrefFXRate   = "ui.fxrate"
)

// Login turns launching at login on and off.
type Login interface {
	OpenAtLogin() bool
	SetOpenAtLogin(on bool) error
}

// Updates checks for new versions of the app. Check shows the updater's
// own window; Enabled is false for builds that cannot replace themselves.
type Updates interface {
	Enabled() bool
	Check()
	Automatic() bool
	SetAutomatic(on bool)
}

// Exporter writes every request as CSV to path.
type Exporter interface {
	Export(ctx context.Context, path string) error
}

func (a *App) pref(k string) string {
	if a.d.Prefs == nil {
		return ""
	}
	return a.d.Prefs.Pref(k)
}

// timezones offered besides the system zone.
var timezones = []string{"", "UTC", "Asia/Shanghai", "Asia/Tokyo", "Asia/Singapore", "Europe/London", "Europe/Berlin", "America/New_York", "America/Los_Angeles"}

func zoneLabel(k string) string {
	if k == "" {
		return tr("followSystem")
	}
	return k
}

func systemZone() string {
	name, _ := time.Now().Zone()
	if z := time.Local.String(); z != "" && z != "Local" {
		return z
	}
	return name
}

// The display currency: prices are always computed in US dollars, then
// multiplied by curRate and shown with curSym.
var (
	curSym  = "$"
	curRate = 1.0
)

var currencyCodes = []string{"USD", "CNY", "EUR", "JPY", "GBP", "HKD"}

var currencySyms = map[string]string{"USD": "$", "CNY": "¥", "EUR": "€", "JPY": "JP¥", "GBP": "£", "HKD": "HK$"}

// defaultFX are rough rates used until the user types the one they want.
var defaultFX = map[string]float64{"USD": 1, "CNY": 7.1, "EUR": 0.92, "JPY": 150, "GBP": 0.79, "HKD": 7.8}

func currencyLabel(k string) string { return k + " " + currencySyms[k] }

// applyCurrency reads the currency preferences into curSym and curRate.
func (a *App) applyCurrency() {
	code := a.pref(PrefCurrency)
	if _, ok := currencySyms[code]; !ok {
		code = "USD"
	}
	rate := defaultFX[code]
	if v, err := strconv.ParseFloat(a.pref(PrefFXRate), 64); err == nil && v > 0 && code != "USD" {
		rate = v
	}
	curSym, curRate = currencySyms[code], rate
	a.set.fxRate = rate
	a.updateTray()
}
