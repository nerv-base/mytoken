package gui

import (
	"context"
	"time"

	"github.com/egoist/mygo/ui"
)

// App is the interface's state. Every field is read and written on the UI
// thread only: data is computed by background jobs whose results are
// applied through Deps.Post, so a view never waits on the index.
type App struct {
	d Deps

	page string // "overview", "sessions", "ranking", "projects", "settings"

	ov    overview
	ovJob job
	// trayTitle is the menu-bar text last set.
	trayTitle    string
	trayTitleSet bool
	// recent caches running sessions' last hour of requests.
	recent recentEvents
	// chartRange is the overview chart's range: 0 = 7, 1 = 30, 2 = 90 days.
	chartRange int
	// spendTab is the spend card's view: 0 = this month, 1 = today's clock.
	spendTab int
	// tokRange is the token card's range (index of chartDays).
	tokRange int
	// quotaAll shows every quota account instead of the first rows.
	quotaAll bool
	// deck is the running-sessions card deck: which session is in front,
	// which way the last switch went, and its auto-advance.
	deck deckState
	// mo is the motion state: which cards revealed, where markers were.
	mo motion

	rk    ranking
	rkJob job
	// rkRange and the other ranges index rangeDays.
	rkRange, rkDim int

	pj     projects
	pjJob  job
	pjSel  string
	pjd    projectDetail
	pjdJob job
	pjList ui.ListState

	ss      sessions
	ssJob   job
	ssQuery string
	// ssSort picks the list order (0 newest, 1 costliest); ssTool keeps
	// one tool's sessions ("" all).
	ssSort   int
	ssTool   string
	ssSel    int
	ssList   ui.ListState
	sd       sessionDetail
	sdJob    job
	sdEvents ui.ListState

	rangeSel int // shared by Sessions, Ranking and Projects

	set settingsState

	unsub   func()
	stopped bool
	ctx     context.Context
	cancel  context.CancelFunc
}

// rangeDays are the lengths of the shared range choices; 0 is all time.
var rangeDays = []int{7, 30, 90, 0}

// New returns the interface over d. Call Start to begin loading.
func New(d Deps) *App {
	a := &App{d: d, page: "overview", ssSel: -1, rangeSel: 1, chartRange: 1, tokRange: 1}
	a.ctx, a.cancel = context.WithCancel(context.Background())
	if d.Prefs != nil {
		setLang(d.Prefs.Pref(PrefLang))
		a.applyCurrency()
	}
	return a
}

// Start loads the first data and follows the index. It must be called on
// the UI thread.
func (a *App) Start() {
	if a.d.SetLocale != nil {
		a.d.SetLocale(a.pref(PrefLang))
	}
	a.reloadAll()
	if a.d.Query == nil {
		return
	}
	ch, unsub := a.d.Query.Subscribe()
	a.unsub = unsub
	go func() {
		for range ch {
			a.post(a.dataChanged)
		}
	}()
	// The clock moves the "today" boundary and relative times; once a
	// minute is enough, and costs one cheap reload.
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-a.ctx.Done():
				return
			case <-t.C:
				a.post(a.minuteTick)
			}
		}
	}()
}

// Stop ends the background work.
func (a *App) Stop() {
	a.stopped = true
	a.cancel()
	if a.unsub != nil {
		a.unsub()
	}
}

// QuotasChanged reloads the quota cards; call it from any goroutine.
func (a *App) QuotasChanged() {
	a.post(func() {
		if a.d.Quotas != nil {
			a.ov.quotas = a.d.Quotas()
			a.updateTray()
		}
	})
}

func (a *App) now() time.Time {
	if a.d.Now != nil {
		return a.d.Now()
	}
	return time.Now()
}

func (a *App) post(fn func()) {
	if a.d.Post != nil {
		a.d.Post(fn)
		return
	}
	fn()
}

// dataChanged reloads the overview, which the tray shows too, and the
// page in view; the other pages reload when they are entered.
func (a *App) dataChanged() {
	a.loadOverview()
	a.reloadPage()
	if a.page == "settings" {
		a.loadSettings()
	}
}

func (a *App) minuteTick() {
	day := dayStart(a.now())
	if !day.Equal(a.ov.day) {
		a.dataChanged()
		return
	}
	// Active sessions age out with the clock alone.
	if len(a.ov.active) > 0 {
		a.loadOverview()
	}
}

func (a *App) reloadAll() {
	a.loadOverview()
	a.reloadPage()
}

func (a *App) reloadPage() {
	switch a.page {
	case "ranking":
		a.loadRanking()
	case "projects":
		a.loadProjects()
		if a.pjSel != "" {
			a.loadProjectDetail()
		}
	case "sessions":
		a.loadSessions()
		if a.sd.key != "" {
			a.loadSessionDetail()
		}
	case "settings":
		a.loadSettings()
	}
}

// goTo switches pages and loads what the new page shows.
func (a *App) goTo(page string) {
	if a.page == page {
		return
	}
	a.page = page
	a.mo.reset()
	a.reloadPage()
}

// job is a background computation whose newest request wins: a request
// while one is running waits for it and replaces any earlier waiting one.
type job struct {
	running bool
	pending func(context.Context) func()
}

// run starts work in the background; the function it returns is applied
// on the UI thread.
func (a *App) run(j *job, work func(ctx context.Context) func()) {
	if a.stopped {
		return
	}
	if j.running {
		j.pending = work
		return
	}
	j.running = true
	go func() {
		ctx, cancel := context.WithTimeout(a.ctx, time.Minute)
		apply := work(ctx)
		cancel()
		a.post(func() {
			j.running = false
			if apply != nil && !a.stopped {
				apply()
			}
			if next := j.pending; next != nil {
				j.pending = nil
				a.run(j, next)
			}
		})
	}()
}

// busy reports whether any job is running or waiting, for tests.
func (a *App) busy() bool {
	for _, j := range []*job{&a.ovJob, &a.rkJob, &a.pjJob, &a.pjdJob, &a.ssJob, &a.sdJob, &a.set.job, &a.set.act} {
		if j.running || j.pending != nil {
			return true
		}
	}
	return false
}

func dayStart(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func monthStart(t time.Time) time.Time {
	y, m, _ := t.Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, t.Location())
}

// GoTo shows a page: "overview", "sessions", "ranking", "projects" or
// "settings".
func (a *App) GoTo(page string) { a.goTo(page) }
