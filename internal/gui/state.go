package gui

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/ingestrate"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/store"
)

// Hooks are what the GUI asks of the rest of the app; any may be nil.
type Hooks struct {
	// Progress reports the scan: sources done and total.
	Progress func() (done, total int)
	// Rebuild drops the index and rescans.
	Rebuild func()
	// Version is the app's version. CanUpdate reports whether this build
	// can replace itself; CheckUpdates checks now, showing the update
	// window; AutoUpdates and SetAutoUpdates read and set the daily check.
	Version        string
	CanUpdate      func() bool
	CheckUpdates   func()
	AutoUpdates    func() bool
	SetAutoUpdates func(bool)
	// OpenAtLogin reads and sets launch at login.
	OpenAtLogin    func() bool
	SetOpenAtLogin func(bool)
	// DataDir is shown in the settings.
	DataDir string
	// OpenMain shows the main window; Quit quits (the tray panel's buttons).
	OpenMain func()
	Quit     func()
	// Settings edits pricing; nil hides the pricing cards.
	Settings query.Settings
	// SetProviderAlias sets or clears the display name of an identified
	// endpoint (store.SetProviderAlias). It only changes presentation: the
	// endpoint's identity, attribution and prices stay as they are. nil
	// hides the alias editor.
	SetProviderAlias func(ctx context.Context, id, label string) error
	// Relays reconciles relays; nil hides the page's data.
	Relays Relays
	// Budget edits the persisted budget rules; nil hides the budget card.
	Budget Budget
	// Calendar reads and writes the stats timezone; nil hides that row.
	Calendar Calendar
	// Location is the stats timezone every calendar period is resolved in
	// (app.App.Location). A nil hook, or a nil result, means no fixed calendar
	// is configured: the machine's zone is then a display preview only, never
	// the persisted choice a fixed calendar implies.
	Location func() *time.Location
	// Sources administers the instance configuration (sources.json): the
	// settings page's add, move, remove and promote actions. nil hides the
	// editor and shows the read-only summary only.
	Sources SourceAdmin
	// Diagnostics reads the collection evidence the store recorded for one
	// harness instance (app.SourceDiagnostics). It is read in the background,
	// in the same pass as the configuration, so a frame never touches the
	// store; nil hides the evidence rather than showing zeros.
	Diagnostics DiagnosticsReader
	// ClaudeQuota is the opt-in Claude subscription-quota panel; nil hides
	// the panel. Nothing is requested until the user enables a source.
	ClaudeQuota ClaudeQuota
	// QuotaStore reads the persisted provider-quota observations the offline
	// quota cards show; nil hides those cards. It is a plain key/value read
	// of the app's own store, never a network call.
	QuotaStore QuotaStore
	// IngestRate reports how many output tokens the scanner committed over
	// the trailing ingestrate.Window ending at now (scan.Scanner.IngestRate).
	// This is ingestion throughput, not generation speed; nil shows "—".
	IngestRate func(now time.Time) ingestrate.Snapshot
	// Now is the clock (tests pin it).
	Now func() time.Time
	// Visible reports whether a window shows the state; nil means always.
	// While none does, data changes only mark the views stale, and Shown
	// reloads them: a tray-resident app does no work for nobody to see.
	Visible func() bool
}

// reloadGap is the least time between two reloads for data changes: logs
// written all the time by running agents must not keep the queries busy.
const reloadGap = 2 * time.Second

// State is the GUI's state, shared by the main window and the tray panel.
// Its fields are only touched on the UI thread, through post.
type State struct {
	Q     query.Service
	Hooks Hooks
	// post runs fn on the UI thread and redraws (win.Update); sync loads
	// data inline, for tests and snapshots.
	post func(fn func())
	sync bool

	page string
	span int

	ov       Overview
	ovLoaded bool
	today    Overview // the tray panel's: always SpanToday
	err      error
	// spans caches each span's own part of the overview; shared is the rest.
	// gen counts data changes: a cached span older than gen is shown at
	// once and refreshed behind it.
	spans  [SpanAll + 1]spanEntry
	shared shared
	gen    int

	sessions SessionPage
	sortIdx  int
	search   string
	list     ui.ListState
	// sel is the selected session, instance included: a session id alone
	// cannot name one row when two instances hold the same id.
	sel        sessionRef
	detail     SessionDetail
	openKids   bool
	detailBusy bool
	// provFilter narrows the sessions page to one endpoint's stable provider
	// id. It is a drilldown state, not a pricing choice: the sessions it
	// lists keep the provider they resolved to.
	provFilter providerDrill

	rankTab int
	pv      providerView
	pr      pricingState
	bg      budgetState
	rl      relayState
	ch      cacheState
	src     sourceState
	cq      claudeQuotaState
	oq      offlineQuotaState

	// stale is set by data changes not reloaded yet; lastReload and
	// waiting pace the reloads (see reloadGap).
	stale      bool
	lastReload time.Time
	waiting    bool

	mu     sync.Mutex
	jobs   map[string]*job
	spanAt [SpanAll + 1]context.CancelFunc // in-flight span loads
	cancel func()
}

type spanEntry struct {
	ov  Overview
	err error
	gen int
	ok  bool
}

// job is one kind of load: at most one runs, and at most the latest request
// waits behind it.
type job struct {
	running bool
	next    func(ctx context.Context) func()
}

// NewState makes the GUI state over q. post runs a function on the UI
// thread (mygo's Window.Update); nil means run inline and load synchronously.
func NewState(q query.Service, hooks Hooks, post func(func())) *State {
	s := &State{Q: q, Hooks: hooks, post: post, page: "overview", span: int(Span7), jobs: map[string]*job{}}
	if s.Hooks.Now == nil {
		s.Hooks.Now = time.Now
	}
	if post == nil {
		s.sync = true
		s.post = func(fn func()) { fn() }
	}
	return s
}

// Start loads everything and reloads whenever new data is committed.
func (s *State) Start() {
	s.Reload()
	if s.sync {
		return
	}
	ch, cancel := s.Q.Subscribe()
	s.cancel = cancel
	go func() {
		for range ch {
			s.post(s.changed)
		}
	}()
	// The day turns over, and the tray's last-24h moves on: refresh each minute.
	go func() {
		for range time.Tick(time.Minute) {
			s.post(s.changed)
		}
	}()
}

// changed marks the views stale and reloads them when one is shown, at most
// once per reloadGap.
func (s *State) changed() {
	s.stale = true
	s.refresh()
}

// Shown is called when a window showing the state appears: it shows what
// it has at once, and reloads behind it what changed while hidden.
func (s *State) Shown() { s.refresh() }

func (s *State) refresh() {
	if !s.stale || s.waiting || (s.Hooks.Visible != nil && !s.Hooks.Visible()) {
		return
	}
	if wait := reloadGap - s.Hooks.Now().Sub(s.lastReload); wait > 0 && !s.lastReload.IsZero() {
		s.waiting = true
		time.AfterFunc(wait, func() {
			s.post(func() {
				s.waiting = false
				s.refresh()
			})
		})
		return
	}
	s.Reload()
}

// Stop stops listening for changes.
func (s *State) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
}

// rebuild runs the explicit full rescan and reloads the views when it returns.
// The rescan writes a fresh collection record, so the sources card must show
// the new evidence without anything else being pressed. It reads no credential
// and makes no request: the rescan is the app's own index over the sources the
// user configured.
func (s *State) rebuild() {
	if s.Hooks.Rebuild == nil {
		return
	}
	// Tests load inline: the reload is observable as soon as the rescan
	// returns, with no goroutine and no clock.
	if s.sync {
		s.Hooks.Rebuild()
		s.src.loaded = false
		s.changed()
		return
	}
	// The real rescan walks every source and can take a while: it must not
	// block the frame that started it.
	go func() {
		s.Hooks.Rebuild()
		s.post(func() {
			s.src.loaded = false
			s.changed()
		})
	}()
}

// Reload reloads the data of every view; cached spans count as stale.
func (s *State) Reload() {
	s.gen++
	s.stale = false
	s.lastReload = s.Hooks.Now()
	s.loadSpan(Span(s.span))
	if Span(s.span) != SpanToday {
		s.loadSpan(SpanToday)
	}
	s.loadShared()
	s.loadSessions()
	if s.sel.SessionID != "" {
		s.loadDetail(s.sel)
	}
	// Pricing is only on the settings page: elsewhere, load it when shown.
	if s.Hooks.Settings != nil {
		if s.page == "settings" || s.sync {
			s.loadPricing()
		} else {
			s.pr.loaded = false
		}
	}
	// The relay page loads when drawn; the report shown stays until replaced.
	s.rl.loaded, s.rl.want = false, ""
	s.src.loaded = false
	// The quota cards and the source editor only exist on the settings page:
	// reload them when shown, and drop the picked rows with the data.
	s.cq.loaded = false
	s.oq.loaded = false
	// The budget card only exists on the settings page: reload it when shown.
	s.bg.loaded = false
	// The cache page reloads when drawn; what it shows stays until replaced.
	s.ch.want = ""
}

// location is the stats timezone the dashboard and the tray resolve calendar
// days and weeks in. It is read per load, so a timezone change takes effect on
// the next reload without restarting the app.
func (s *State) location() *time.Location {
	if s.Hooks.Location == nil {
		return time.Local
	}
	if loc := s.Hooks.Location(); loc != nil {
		return loc
	}
	return time.Local
}

// run runs load off the UI thread (or inline) and applies its result. Loads
// of one kind do not overlap: while one runs, the latest request waits
// behind it and runs as soon as it ends; requests in between are dropped.
func (s *State) run(kind string, load func(ctx context.Context) func()) {
	if s.sync {
		load(context.Background())()
		return
	}
	s.mu.Lock()
	j := s.jobs[kind]
	if j == nil {
		j = &job{}
		s.jobs[kind] = j
	}
	if j.running {
		j.next = load
		s.mu.Unlock()
		return
	}
	j.running = true
	s.mu.Unlock()
	go func() {
		for load != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			apply := load(ctx)
			cancel()
			if apply != nil {
				s.post(apply)
			}
			s.mu.Lock()
			load, j.next = j.next, nil
			j.running = load != nil
			s.mu.Unlock()
		}
	}()
}

// setSpan shows the span just chosen: from the cache at once when it has
// it, and loads it when it is missing or stale. Loads of spans no longer
// wanted are cancelled so the chosen one is not queued behind them.
func (s *State) setSpan() {
	s.mu.Lock()
	for i, cancel := range s.spanAt {
		if cancel != nil && i != s.span && Span(i) != SpanToday {
			cancel()
		}
	}
	s.mu.Unlock()
	s.compose()
	if e := s.spans[s.span]; !e.ok || e.gen != s.gen {
		s.loadSpan(Span(s.span))
	}
}

// compose rebuilds the overview and the tray's today from the caches.
func (s *State) compose() {
	if e := s.spans[s.span]; e.ok {
		s.ov, s.err, s.ovLoaded = e.ov.with(s.shared), e.err, true
	}
	if e := s.spans[SpanToday]; e.ok {
		s.today = e.ov.with(s.shared)
	}
}

func (s *State) loadSpan(span Span) {
	now, gen := s.Hooks.Now(), s.gen
	s.run("span:"+spanKeys[span], func(ctx context.Context) func() {
		ctx, cancel := context.WithCancel(ctx)
		s.mu.Lock()
		s.spanAt[span] = cancel
		s.mu.Unlock()
		ov, err := loadSpan(ctx, s.Q, span, now, s.location())
		s.mu.Lock()
		s.spanAt[span] = nil
		s.mu.Unlock()
		cancelled := errors.Is(ctx.Err(), context.Canceled)
		cancel()
		if cancelled {
			return nil
		}
		return func() {
			if s.spans[span].ok && s.spans[span].gen > gen {
				return
			}
			s.spans[span] = spanEntry{ov: ov, err: err, gen: gen, ok: true}
			s.compose()
		}
	})
}

func (s *State) loadShared() {
	now := s.Hooks.Now()
	s.run("shared", func(ctx context.Context) func() {
		sh := loadShared(ctx, s.Q, now, s.location())
		return func() {
			s.shared = sh
			s.compose()
		}
	})
}

var sortKeys = []string{query.SortRecent, query.SortTokens, query.SortCost}

// sessionFilter is the filter the sessions page lists with. A provider
// drilldown narrows by the endpoint's stable id, never by its display name:
// the name is an alias the user may reuse for several endpoints, and the
// sessions themselves must keep the provider they were priced under.
func (s *State) sessionFilter() query.Filter {
	var f query.Filter
	if s.provFilter.Key != "" {
		f.ProviderIDs = []string{s.provFilter.Key}
	}
	return f
}

func (s *State) loadSessions() {
	key, want := sortKeys[s.sortIdx], s.provFilter.Key
	f := s.sessionFilter()
	s.run("sessions", func(ctx context.Context) func() {
		page, err := loadSessions(ctx, s.Q, f, key)
		return func() {
			if err == nil && sortKeys[s.sortIdx] == key && s.provFilter.Key == want {
				s.sessions = page
			}
		}
	})
}

func (s *State) loadDetail(ref sessionRef) {
	ref.InstanceID = store.NormalizeInstanceID(ref.InstanceID)
	s.detailBusy = true
	s.run("detail", func(ctx context.Context) func() {
		d, err := loadSession(ctx, s.Q, ref)
		return func() {
			if s.sel == ref {
				s.detailBusy = false
				if err == nil {
					s.detail = d
				}
			}
		}
	})
}

// SelectRef opens the session a reference names.
func (s *State) SelectRef(ref sessionRef) {
	ref.InstanceID = store.NormalizeInstanceID(ref.InstanceID)
	if ref.SessionID == "" || s.sel == ref {
		return
	}
	s.sel = ref
	s.openKids = false
	s.loadDetail(ref)
}

// SelectRow opens the session a listed row names, instance included.
func (s *State) SelectRow(r query.SessionRow) { s.SelectRef(refOf(r)) }

// Select opens a session by harness and id. Callers that hold a row should
// use SelectRow: an id alone names the reserved default instance, which is
// all this can safely mean.
func (s *State) Select(h model.Harness, id string) {
	s.SelectRef(sessionRef{Harness: h, InstanceID: store.DefaultInstanceID, SessionID: id})
}

// clearSelection drops the open session. The endpoint drilldown changes which
// sessions the list holds, so a session that is no longer listed must not stay
// behind in the detail pane, where it would look like the filter did not apply.
func (s *State) clearSelection() {
	s.sel = sessionRef{}
	s.openKids = false
	s.detailBusy = false
	s.detail = SessionDetail{}
}

// SetPage switches the main window's page ("overview", "sessions", …).
func (s *State) SetPage(p string) { s.page = p }
