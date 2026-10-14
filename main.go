// Command mytoken is MyToken!!!!!: a tray-resident tracker of the tokens AI
// coding harnesses spend, per session × provider × model.
//
//	mytoken                 run the app (tray + main window)
//	mytoken stats [--json]  print usage without a window
//	mytoken scan            index the logs once
//	mytoken help            list every command (see docs/CLI.md)
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"time"
	// The IANA database travels with the binary: a budget period is resolved
	// in a named zone the user typed, and a shipped build cannot rely on the
	// machine having one installed.
	_ "time/tzdata"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/plugins/updater"
	"github.com/egoist/mygo/plugins/updater/native"
	"github.com/egoist/mygo/ui"

	"github.com/zzstar101/mytoken/internal/app"
	"github.com/zzstar101/mytoken/internal/budget"
	"github.com/zzstar101/mytoken/internal/claudequota"
	"github.com/zzstar101/mytoken/internal/cli"
	"github.com/zzstar101/mytoken/internal/gui"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/ingestrate"
	"github.com/zzstar101/mytoken/internal/instance"
	"github.com/zzstar101/mytoken/internal/paths"
)

// memoryLimit is the GUI's soft Go heap limit. Live data is ~20 MB on a
// 200K-event history; the rest of the process is the window system.
const memoryLimit = 64 << 20

func main() {
	// `mygo build` cannot pass -ldflags, but it embeds mygo.json's version.
	if cli.Version == "dev" {
		if v := mygo.App.Version(); v != "" {
			cli.Version = v
		}
	}
	if len(os.Args) > 1 && os.Args[1] != "--hidden" {
		os.Exit(cli.Run(os.Args[1:]))
	}

	// One instance at a time: a second launch hands its command line over to
	// this process and exits, so the database is only ever opened once.
	// The main window exists only while it is open: closing it destroys it,
	// which gives back its GPU surfaces and glyph atlases (~90 MB), and
	// opening it builds a new one. The tray panel lives all along.
	var main, panel *mygo.Window
	var newMain func(hidden bool) *mygo.Window
	ready := false
	openMain := func() {
		if !ready {
			return // another instance arrived before the windows exist
		}
		if panel != nil {
			panel.Hide()
		}
		if main == nil {
			main = newMain(false)
		}
		main.Show()
		main.Focus()
	}
	if !mygo.App.RequestSingleInstanceLock() {
		return
	}
	mygo.App.OnSecondInstance(func([]string, string) { openMain() })

	// Checks github.com for a new version once a day (Settings turns it
	// off); builds that cannot replace themselves, such as development
	// builds and the Debian package, never check.
	mygo.Use(native.Plugin)

	a, err := app.OpenWith(app.Options{Notify: systemNotifier{}})
	if err != nil {
		log.Fatalf("mytoken: %v", err)
	}
	defer a.Close()

	// MYTOKEN_PPROF=127.0.0.1:6060 serves net/http/pprof, to measure the
	// app against its CPU and memory budgets (docs/ROADMAP.md).
	if addr := os.Getenv("MYTOKEN_PPROF"); addr != "" {
		go func() { log.Printf("mytoken: pprof: %v", http.ListenAndServe(addr, nil)) }()
	}
	// The tray app lives all day while scans churn short-lived buffers; a
	// soft heap limit makes the collector hand that memory back instead of
	// letting the heap float at twice its live size, and collecting at half
	// again the live heap keeps ~8 MB off the tray's footprint for a sliver
	// of CPU while agents write. GOMEMLIMIT and GOGC override.
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(memoryLimit)
	}
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(50)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The app owns the watcher's lifecycle: a source reload cancels and joins
	// this run before starting the replacement, and a runtime failure is
	// recorded on the app instead of falling back to another source set.
	a.StartScanning(ctx)
	// Keeps the relays the user turned on in sync; nothing is sent for a
	// relay that is off, and by default every relay is off.
	a.StartRelaySync()

	dataDir, _ := paths.DataDir()
	var state *gui.State

	// Both windows show the same state; a change redraws both. Updates
	// go through the panel, which outlives the main window.
	post := func(fn func()) {
		w := panel
		if w == nil {
			w = main
		}
		if w == nil {
			return
		}
		w.Update(func() {
			fn()
			// On the main thread, where main is opened and closed.
			if main != nil && main != w {
				main.Update(func() {})
			}
		})
	}
	hooks := guiHooks(a, ctx, dataDir, func() { mygo.RunOnMain(openMain) }, func() bool {
		return main != nil && main.IsVisible() || panel != nil && panel.IsVisible()
	})
	if l := mygo.App.Locale(); os.Getenv("MYTOKEN_LANG") == "" && l != "" {
		if strings.HasPrefix(strings.ToLower(l), "zh") {
			gui.SetLang("zh")
		} else {
			gui.SetLang("en")
		}
	}

	mygo.App.WhenReady(func() {
		hidden := mygo.App.WasOpenedAtLogin() || len(os.Args) > 1
		// The state must exist before the first window: a window that is
		// not hidden builds and draws its content synchronously inside
		// NewWindow, so a nil state panics on that first frame.
		state = gui.NewState(a.Query, hooks, post)
		// A quit (the tray menu, Cmd+Q, SIGTERM, logging out) closes every
		// window; only a close by the user leaves the app in the tray.
		quitting := false
		mygo.App.OnBeforeQuit(func(*mygo.QuitEvent) { quitting = true })
		var trayless bool
		newMain = func(hidden bool) *mygo.Window {
			w := mygo.NewWindow(mygo.WindowOptions{
				Title:         "MyToken!!!!!",
				Width:         1280,
				Height:        860,
				MinWidth:      980,
				MinHeight:     640,
				Hidden:        hidden,
				TitleBarStyle: mygo.TitleBarHidden,
				// The traffic lights sit inside the sidebar card, not on its edge.
				TrafficLightPosition: &mygo.Point{X: gui.TrafficLightX, Y: gui.TrafficLightY},
				StateKey:             "main",
				Content:              ui.View(func(c *ui.Context) { state.MainView(c) }),
			})
			w.OnShow(state.Shown)
			w.OnClosed(func() {
				if main == w {
					main = nil
				}
				switch {
				case quitting:
				case trayless:
					// Without a tray the window is the app.
					mygo.App.Quit()
				default:
					// Hand the window's memory back now, not at the next GC.
					time.AfterFunc(2*time.Second, debug.FreeOSMemory)
				}
			})
			return w
		}
		if !hidden {
			main = newMain(false)
		}

		panel = mygo.NewWindow(mygo.WindowOptions{
			Title:           "MyToken!!!!!",
			Width:           gui.TrayWidth,
			Height:          gui.TrayHeight,
			Hidden:          true,
			Frameless:       true,
			DisableResize:   true,
			DisableMinimize: true,
			DisableMaximize: true,
			AlwaysOnTop:     true,
			SkipTaskbar:     true,
			Transparent:     true,
			Vibrancy:        mygo.VibrancyMenu,
			Content:         ui.View(func(c *ui.Context) { state.TrayView(c) }),
		})
		panel.OnBlur(func() { panel.Hide() })
		// Hidden windows leave changes unloaded: catch up when one shows.
		panel.OnShow(state.Shown)
		ready = true

		state.Start()

		// Budget checks run on the service's own goroutine: it reserves a
		// crossed threshold in the store first, then asks the system to show
		// the notification. It starts only here, in the desktop app, so a CLI
		// command never writes the stats timezone and never notifies.
		a.Budget.SetOnUpdate(func() { post(state.BudgetChanged) })
		a.StartBudget(ctx)
		// A budget notification opens the settings page, where the rules and
		// the delivery report are.
		mygo.App.OnNotificationClick(func(id string) {
			if !strings.HasPrefix(id, "budget:") {
				return
			}
			mygo.RunOnMain(func() {
				state.SetPage("settings")
				openMain()
			})
		})

		tray, err := mygo.NewTray(mygo.TrayOptions{Icon: trayIcon(), IconIsTemplate: true, ToolTip: "MyToken!!!!!"})
		if err != nil {
			// No tray (e.g. Linux without appindicator): the window is the app.
			log.Printf("mytoken: tray: %v", err)
			trayless = true
			openMain()
			return
		}
		mygo.App.SetActivationPolicy(mygo.ActivationPolicyAccessory)
		tray.OnClick(func() {
			if panel.IsVisible() {
				panel.Hide()
				return
			}
			b := tray.Bounds()
			x := b.X + b.Width/2 - gui.TrayWidth/2
			y := b.Y + b.Height + 6
			if b.Y > 300 { // taskbar at the bottom (Windows): open upwards
				y = b.Y - gui.TrayHeight - 6
			}
			panel.SetPosition(x, y)
			panel.Show()
			panel.Focus()
		})
		mygo.App.OnActivate(func(hasVisible bool) {
			if !hasVisible {
				openMain()
			}
		})
		// An accessory app isn't brought forward on launch: do it ourselves.
		if !hidden {
			openMain()
		}
	})
	if err := mygo.App.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "mytoken:", err)
		os.Exit(1)
	}
}

// guiHooks is the window's view of the app main() has already opened: what the
// GUI may ask of it. It is built from that app — never by opening a second one —
// and it hands the pricing settings through untouched: renaming an endpoint for
// display is a store write, not a pricing change.
func guiHooks(a *app.App, ctx context.Context, dataDir string, openMain func(), visible func() bool) gui.Hooks {
	// A method value on a nil receiver is not nil, so a storeless app must be
	// checked here: otherwise the tab would draw its editor and panic on save.
	// No store means no display-name write, which is the hook being nil.
	var setProviderAlias func(ctx context.Context, id, label string) error
	if a.Store != nil {
		setProviderAlias = a.Store.SetProviderAlias
	}
	// The sources editor writes sources.json, so it needs the data directory
	// it lives in; without one there is nothing to administer.
	var sources gui.SourceAdmin
	if dataDir != "" {
		sources = sourceAdmin{dataDir: dataDir, app: a}
	}
	// The quota panels are only meaningful for an app that has a quota
	// service: they read and write the same consent and cache it holds.
	var claudeQuota gui.ClaudeQuota
	if a.ClaudeQuota() != nil {
		claudeQuota = claudeQuotaAdmin{app: a}
	}
	// The offline quota cards read the app's own key/value store. No store
	// means no observation to show, which is the hook being nil.
	var quotaStore gui.QuotaStore
	if a.Store != nil {
		quotaStore = a.Store
	}
	// The collection evidence the scanner persisted for each instance is read
	// from that same store's settings, one read per scanned instance per
	// background reload of the sources card. Without a store there is nothing
	// recorded, which is the hook being nil rather than an empty record.
	var diagnostics gui.DiagnosticsReader
	if a.Store != nil {
		diagnostics = a.SourceDiagnostics
	}
	// The tray's ingest rate is the scanner's own committed-output count.
	// Without a scanner there is nothing to read, which is nil → "—". The
	// bindings go through the app's accessors: a source reload replaces the
	// scanner instance, and a method value captured here would keep reading
	// the one Open built.
	var ingestRate func(now time.Time) ingestrate.Snapshot
	if a.CurrentScanner() != nil {
		ingestRate = a.ScannerIngestRate
	}
	return gui.Hooks{
		Progress: a.ScannerProgress,
		Rebuild: func() {
			if err := a.ScannerRebuild(ctx); err != nil {
				log.Printf("mytoken: rebuild: %v", err)
			}
		},
		OpenAtLogin:    mygo.App.OpenAtLogin,
		SetOpenAtLogin: func(on bool) { _ = mygo.App.SetOpenAtLogin(on) },
		DataDir:        dataDir,
		Settings:       a.Settings,
		// The providers tab renames an endpoint for display only; the write
		// goes to the store this app already opened.
		SetProviderAlias: setProviderAlias,
		Relays:           a.Relays,
		Budget:           a.Budget,
		Calendar:         a,
		// The dashboard and the tray resolve calendar days and weeks in the
		// persisted stats zone, the same one the budget card and the snapshot
		// worker use, so a timezone change moves every period together.
		Location:       a.Location,
		Sources:        sources,
		Diagnostics:    diagnostics,
		ClaudeQuota:    claudeQuota,
		QuotaStore:     quotaStore,
		IngestRate:     ingestRate,
		OpenMain:       openMain,
		Quit:           func() { mygo.App.Quit() },
		Version:        cli.Version,
		CanUpdate:      mygo.Updater.Enabled,
		CheckUpdates:   updater.CheckForUpdates,
		AutoUpdates:    updater.AutomaticChecks,
		SetAutoUpdates: updater.SetAutomaticChecks,
		Visible:        visible,
	}
}

// sourceAdmin implements gui.SourceAdmin over sources.json. It is a thin
// adapter: the resolution rules live in internal/instance, and a write is
// committed only when the configuration it produces still resolves. An
// unusable explicit root is refused instead of being dropped, because
// instance.Resolve refuses it and this refuses to save what Resolve rejects.
type sourceAdmin struct {
	dataDir string
	app     *app.App
}

// Sources is the running resolution: what this process actually scans, which
// is not necessarily what sources.json holds now.
func (s sourceAdmin) Sources() []gui.SourceInfo { return runningSourceInfos(s.app) }

// Problems is the running resolution's diagnostics.
func (s sourceAdmin) Problems() []gui.SourceProblem { return runningSourceProblems(s.app) }

// Config loads sources.json and resolves it, so the editor shows what the
// next start would do.
func (s sourceAdmin) Config(_ context.Context) (gui.SourceView, error) {
	cfg, err := instance.Load(s.dataDir)
	if err != nil {
		return gui.SourceView{}, err
	}
	return s.view(cfg), nil
}

func (s sourceAdmin) Add(ctx context.Context, req gui.SourceAdd) (gui.SourceView, error) {
	return s.update(ctx, func(cfg *instance.Config) error {
		// An empty kind asks for an independent instance; an empty id asks
		// for a generated one, so the identity never comes from the paths.
		_, err := cfg.Add(instance.AddRequest{
			Label:    req.Label,
			Harness:  req.Harness,
			Roots:    req.Roots,
			Kind:     instance.Kind(req.Kind),
			Disabled: req.Disabled,
		})
		return err
	})
}

// Move changes an entry's roots and keeps its id: the instance is the same
// instance at a new location, not a new one.
func (s sourceAdmin) Move(ctx context.Context, id string, roots []string) (gui.SourceView, error) {
	return s.update(ctx, func(cfg *instance.Config) error {
		_, err := cfg.Move(id, roots)
		return err
	})
}

// Remove deletes the entry from sources.json. It never deletes data: the
// directory it pointed at is left exactly as it was.
func (s sourceAdmin) Remove(ctx context.Context, id string) (gui.SourceView, error) {
	return s.update(ctx, func(cfg *instance.Config) error { return cfg.Remove(id) })
}

// Promote turns a backup copy into an independent instance. The id, the
// label and the paths are unchanged; only what gets scanned changes.
func (s sourceAdmin) Promote(ctx context.Context, id string) (gui.SourceView, error) {
	return s.update(ctx, func(cfg *instance.Config) error {
		_, err := cfg.Promote(id)
		return err
	})
}

// update applies one mutation through instance.Update and refuses to save a
// configuration whose resolution gained a new fatal problem. Problems that
// were already there (an unrelated environment default, say) do not block an
// edit; a newly unusable explicit root does.
//
// A save is only half the edit: the running scan keeps the configuration it
// started with until the scanner is replaced. The reload resolves the new
// configuration and builds its scanner before anything is stopped, then
// cancels and joins the old watcher and starts the replacement — so a save
// that cannot be honoured leaves the running scan exactly as it was, and the
// view keeps reporting that a restart is needed instead of pretending the new
// configuration is live.
func (s sourceAdmin) update(ctx context.Context, act func(*instance.Config) error) (gui.SourceView, error) {
	cfg, err := instance.Update(s.dataDir, func(cfg *instance.Config) error {
		before := fatalKeys(cfg)
		if err := act(cfg); err != nil {
			return err
		}
		p, ok := firstNewFatal(before, cfg)
		if !ok {
			return nil
		}
		if p.Path != "" {
			return fmt.Errorf("%s: %s (instance %s, path %s)", p.Code, p.Message, p.ID, p.Path)
		}
		return fmt.Errorf("%s: %s (instance %s)", p.Code, p.Message, p.ID)
	})
	if err != nil {
		return gui.SourceView{}, err
	}
	if err := s.app.ReloadSources(ctx); err != nil {
		// Everything a reload can fail on happens before anything is stopped,
		// so a preflight failure left the previous scan running — unless it
		// had already stopped on its own. Say which of the two is true: a
		// saved-but-unapplied edit is not "scanning stopped".
		if s.app.ScanError() == nil && !errors.Is(err, app.ErrClosed) {
			return s.view(cfg), fmt.Errorf("sources: saved, but the running scan was not reloaded: %w (the previous sources are still being scanned)", err)
		}
		return s.view(cfg), fmt.Errorf("sources: saved, but the running scan was not reloaded: %w", err)
	}
	return s.view(cfg), nil
}

// view maps one resolved configuration into what the editor draws. Scanned
// mirrors internal/app: an instance is scanned only when it is enabled, is an
// instance rather than a backup copy, has roots, and its harness can build a
// rooted parser for them.
func (s sourceAdmin) view(cfg *instance.Config) gui.SourceView {
	resolved, problems := instance.Resolve(cfg, instance.DefaultFallbacks())
	// The editor edits what sources.json declares. A harness that only has a
	// built-in or environment root is a fallback, not an entry: it is shown by
	// the read-only sources summary, never offered for rename or removal.
	declared := make(map[string]bool, len(cfg.Entries))
	for _, e := range cfg.Entries {
		declared[e.ID] = true
	}
	out := gui.SourceView{Sources: make([]gui.SourceInfo, 0, len(cfg.Entries))}
	for _, r := range resolved {
		if !declared[r.ID] {
			continue
		}
		out.Sources = append(out.Sources, gui.SourceInfo{
			ID:       r.ID,
			Label:    r.Label,
			Harness:  r.Harness,
			Roots:    r.Roots,
			Source:   string(r.Source),
			Kind:     string(r.Kind),
			Disabled: r.Disabled,
			Scanned:  scannable(r),
			Reason:   firstReason(problems, r),
		})
	}
	for _, p := range problems {
		// A fatal problem is always worth showing: it is why a source is not
		// being scanned at all. The rest belong to the entries being edited.
		if !p.Fatal && !declared[p.ID] {
			continue
		}
		out.Problems = append(out.Problems, gui.SourceProblem{
			Code:    string(p.Code),
			ID:      p.ID,
			Harness: p.Harness,
			Path:    p.Path,
			Message: p.Message,
			Fatal:   p.Fatal,
		})
	}
	// A save reloads the running scanner, so this is normally false. It stays
	// true when the saved configuration no longer matches what this process
	// resolved — a config edited outside the app, or a reload that could not
	// be honoured — and the card says so instead of implying the change is
	// already live.
	out.Restart = resolutionKey(out.Sources) != resolutionKey(runningSourceInfos(s.app))
	// A watcher that stopped on a runtime failure is reported here, next to
	// the sources it stopped scanning. Nothing falls back to another source
	// set: scanning is stopped until a reload starts a replacement.
	if err := s.app.ScanError(); err != nil {
		out.ScanError = err.Error()
	}
	return out
}

// scannable is internal/app's own rule, restated here because that package
// builds the running app and this only previews the next start.
func scannable(r instance.Resolved) bool {
	if !r.Scannable() || len(r.Roots) == 0 {
		return false
	}
	_, ok := harness.NewRooted(r.Harness, r.Roots...)
	return ok
}

// firstReason is the first non-fatal problem attached to an instance, in
// resolution order: the same "why is this not being scanned" app records.
func firstReason(problems []instance.Problem, r instance.Resolved) string {
	for _, p := range problems {
		if p.Fatal || p.Harness != r.Harness || p.ID != r.ID {
			continue
		}
		return p.Message
	}
	return ""
}

func fatalKeys(cfg *instance.Config) map[string]bool {
	_, problems := instance.Resolve(cfg, instance.DefaultFallbacks())
	out := make(map[string]bool, len(problems))
	for _, p := range problems {
		if p.Fatal {
			out[problemKey(p)] = true
		}
	}
	return out
}

func firstNewFatal(before map[string]bool, cfg *instance.Config) (instance.Problem, bool) {
	_, problems := instance.Resolve(cfg, instance.DefaultFallbacks())
	for _, p := range problems {
		if p.Fatal && !before[problemKey(p)] {
			return p, true
		}
	}
	return instance.Problem{}, false
}

func problemKey(p instance.Problem) string {
	return string(p.Code) + "|" + p.ID + "|" + string(p.Harness) + "|" + p.Path
}

func runningSourceInfos(a *app.App) []gui.SourceInfo {
	if a == nil {
		return nil
	}
	// The live resolution, not the field Open filled: a source reload replaces
	// it, and the editor's "needs a restart" comparison must see the scanner
	// that is actually running.
	sources := a.LiveSources()
	if len(sources) == 0 {
		return nil
	}
	out := make([]gui.SourceInfo, 0, len(sources))
	for _, src := range sources {
		// Only declared entries are compared: the rest are this process's
		// fallbacks, which sources.json cannot change.
		if src.Source != instance.PathUser {
			continue
		}
		out = append(out, gui.SourceInfo{
			ID:       src.ID,
			Label:    src.Label,
			Harness:  src.Harness,
			Roots:    src.Roots,
			Source:   string(src.Source),
			Kind:     string(src.Kind),
			Disabled: src.Disabled,
			Scanned:  src.Scanned,
			Reason:   src.Reason,
		})
	}
	return out
}

func runningSourceProblems(a *app.App) []gui.SourceProblem {
	if a == nil {
		return nil
	}
	sources := a.LiveSources()
	problems := a.LiveSourceProblems()
	if len(problems) == 0 {
		return nil
	}
	declared := make(map[string]bool, len(sources))
	for _, src := range sources {
		if src.Source == instance.PathUser {
			declared[src.ID] = true
		}
	}
	out := make([]gui.SourceProblem, 0, len(problems))
	for _, p := range problems {
		if !p.Fatal && !declared[p.ID] {
			continue
		}
		out = append(out, gui.SourceProblem{
			Code:    string(p.Code),
			ID:      p.ID,
			Harness: p.Harness,
			Path:    p.Path,
			Message: p.Message,
			Fatal:   p.Fatal,
		})
	}
	return out
}

// resolutionKey is the order-independent signature of a resolution: two
// resolutions with the same key scan the same instances at the same roots.
func resolutionKey(list []gui.SourceInfo) string {
	keys := make([]string, 0, len(list))
	for _, in := range list {
		keys = append(keys, fmt.Sprintf("%s|%s|%s|%t|%t|%s",
			in.ID, in.Harness, in.Kind, in.Disabled, in.Scanned, strings.Join(in.Roots, ",")))
	}
	sort.Strings(keys)
	return strings.Join(keys, "\n")
}

// claudeQuotaAdmin implements gui.ClaudeQuota by delegating to the app's one
// quota service: the same consent, cache and throttle the CLI shares.
type claudeQuotaAdmin struct{ app *app.App }

func (c claudeQuotaAdmin) Info(ctx context.Context, instance string) (claudequota.Info, error) {
	return c.app.ClaudeQuotaInfo(ctx, instance)
}

func (c claudeQuotaAdmin) Sources(instance string) []claudequota.Source {
	return c.app.ClaudeQuotaSources(instance)
}

func (c claudeQuotaAdmin) Enable(ctx context.Context, instance, ref string) (claudequota.Info, error) {
	return c.app.EnableClaudeQuota(ctx, instance, ref)
}

func (c claudeQuotaAdmin) Disable(ctx context.Context, instance string) error {
	return c.app.DisableClaudeQuota(ctx, instance)
}

func (c claudeQuotaAdmin) Snapshot(ctx context.Context, instance string) (claudequota.Quota, error) {
	return c.app.ClaudeQuotaSnapshot(ctx, instance)
}

func (c claudeQuotaAdmin) Refresh(ctx context.Context, instance string) (claudequota.Quota, error) {
	return c.app.RefreshClaudeQuota(ctx, instance)
}

// systemNotifier delivers one reserved budget notification through the
// system's notification centre. Show waits for the system to accept or refuse
// it; the caller is the budget service's own goroutine, never the UI thread,
// so a slow notification centre cannot freeze a window. Reusing the
// notification's id replaces an older copy of the same reservation.
type systemNotifier struct{}

func (systemNotifier) Notify(_ context.Context, n budget.Notification) error {
	if !mygo.NotificationsSupported() {
		return errors.New("this system has no notification support")
	}
	return mygo.NewNotification(mygo.NotificationOptions{
		ID:    n.ID,
		Title: n.Title(),
		Body:  n.Body(),
		Group: "budget",
	}).Show()
}

// trayIcon draws the template icon: three rising bars, like a tiny chart.
func trayIcon() []byte {
	const n = 32
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	bar := func(x0, x1, top int) {
		for y := top; y < 28; y++ {
			for x := x0; x < x1; x++ {
				// Round the top corners a little.
				if y == top && (x == x0 || x == x1-1) {
					continue
				}
				img.Set(x, y, color.NRGBA{A: 255})
			}
		}
	}
	bar(4, 10, 17)
	bar(13, 19, 10)
	bar(22, 28, 4)
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
