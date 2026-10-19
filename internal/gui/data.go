package gui

import (
	"context"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

// Query is the read API the interface needs: a subset of query.Service, so
// that the demo data and tests can stand in for the index.
type Query interface {
	Totals(ctx context.Context, f query.Filter) (query.Totals, error)
	Daily(ctx context.Context, f query.Filter) ([]query.Point, error)
	ByHarness(ctx context.Context, f query.Filter) ([]query.Bucket, error)
	ByModel(ctx context.Context, f query.Filter) ([]query.Bucket, error)
	ByProvider(ctx context.Context, f query.Filter) ([]query.Bucket, error)
	ByProject(ctx context.Context, f query.Filter) ([]query.Bucket, error)
	Usage(ctx context.Context, f query.Filter) ([]query.UsageRow, error)
	// HourlyUsage is Usage by local clock hour.
	HourlyUsage(ctx context.Context, f query.Filter) ([]query.UsageRow, error)
	Sessions(ctx context.Context, f query.Filter, sort string, limit, offset int) ([]query.SessionRow, int, error)
	Session(ctx context.Context, h model.Harness, id string) (query.SessionRow, []query.SessionRow, []query.AttributedEvent, error)
	// Subscribe notifies (throttled) whenever new data is committed.
	Subscribe() (<-chan struct{}, func())
}

// Quota is one plan quota a tool reports, such as Claude's 5-hour window.
type Quota struct {
	ID, Name string // provider id ("claude") and display name ("Claude")
	Plan     string // "Max 20x", "Pro", "" when unknown
	Windows  []QuotaWindow
	// Source is "api": figures are always asked of the service, with the
	// tool's stored login.
	Source    string
	FetchedAt time.Time
	Err       string // last fetch error, shown beside a stale snapshot
	// SignIn is set when the service turned the stored login away: the
	// tool needs signing in again (MyToken never refreshes a login).
	SignIn bool
	// Daily is what a credit balance lost each day, oldest first, for the
	// last two weeks; empty when unknown.
	Daily []QuotaDay
}

// QuotaWindow is one window of a plan quota, or a credit balance.
type QuotaWindow struct {
	Name        string // "5h", "week", "month", "credits", or a tool's own label
	UsedPercent float64
	// Used and Limit are absolute amounts in Unit when the tool states
	// them ("usd", "requests", "credits"); 0 when it only gives a percent.
	Used, Limit float64
	Unit        string
	// Balance marks credits without a fixed cap (top-ups, gift packs):
	// Remaining is what is left and there is no percent.
	Balance   bool
	Remaining float64
	ResetsAt  time.Time
	ExpiresAt time.Time // when the soonest of the remaining credits expire
}

// QuotaDay is one day of credit use.
type QuotaDay struct {
	Day   time.Time
	Used  float64
	Known bool
}

// Source is one tool MyToken can read and how it is set up.
type Source struct {
	Harness  model.Harness
	Detected bool // the tool's data was found where it keeps it
	Enabled  bool
	Roots    []string // the directories read
	Override string   // a directory the user chose instead; "" = automatic
	Problem  string   // why the source cannot be read, in plain words
	Requests int64
	LastSeen time.Time
}

// Sources manages the tools that are read.
type Sources interface {
	List(ctx context.Context) ([]Source, error)
	SetEnabled(ctx context.Context, h model.Harness, on bool) error
	// SetRoot checks that dir holds the tool's data and reads it from there;
	// "" goes back to the automatic location.
	SetRoot(ctx context.Context, h model.Harness, dir string) error
	Rescan(ctx context.Context) error
}

// CustomPrice is a user price for a model, USD per million tokens.
type CustomPrice struct {
	Model                                string
	Input, Output, CacheRead, CacheWrite float64
}

// CatalogInfo describes the bundled official price list.
type CatalogInfo struct {
	Version string
	Models  int
}

// Prices is the pricing side of the settings.
type Prices interface {
	Unpriced(ctx context.Context) ([]query.UnpricedModel, error)
	Aliases(ctx context.Context) ([]query.ModelAlias, error)
	SetAliases(ctx context.Context, aliases []query.ModelAlias) error
	CustomPrices(ctx context.Context) ([]CustomPrice, error)
	SetCustomPrices(ctx context.Context, prices []CustomPrice) error
	SearchCatalog(ctx context.Context, q string, limit int) ([]string, error)
	Catalog() CatalogInfo
}

// DBInfo describes the local index.
type DBInfo struct {
	Path           string
	Bytes          int64
	Events         int64
	Sessions       int64
	Oldest, Newest time.Time
}

// Maintenance acts on the local index.
type Maintenance interface {
	Info(ctx context.Context) (DBInfo, error)
	// Rebuild empties the index and reads every source again.
	Rebuild(ctx context.Context) error
	// Compact gives free pages back to the file system.
	Compact(ctx context.Context) error
}

// Prefs stores the interface's preferences.
type Prefs interface {
	Pref(key string) string
	SetPref(key, value string) error
}

// Preference keys.
const (
	PrefLang       = "ui.lang"       // "", "zh", "en"
	PrefAppearance = "ui.appearance" // "", "light", "dark"
	PrefTrayTitle  = "tray.title"    // "cost" (default), "rate", "hit", "quota", "none"
)

// Deps is everything the interface talks to. Any field may be nil except
// Query; the matching controls then hide.
type Deps struct {
	Query   Query
	Quotas  func() []Quota
	Sources Sources
	Prices  Prices
	Maint   Maintenance
	Prefs   Prefs
	Version string

	// Post runs fn on the UI thread and redraws the windows.
	Post func(fn func())
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	OpenMain     func()
	HidePanel    func()
	Quit         func()
	Reveal       func(path string)
	PickFolder   func(start string) (string, bool)
	SetTrayTitle func(title string)
	// Login, Export and SaveFile are optional; their settings hide
	// without them. SaveFile asks where to save a file named name.
	Login Login
	// SetLocale tells the toolkit the language picked ("zh", "en", "").
	SetLocale func(lang string)
	Updates   Updates
	Export    Exporter
	SaveFile  func(name string) (string, bool)
}
