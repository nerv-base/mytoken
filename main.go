// Command mytoken is MyToken!!!!!: a tray-resident tracker of the tokens AI
// coding harnesses spend, per session × provider × model.
//
//	mytoken                 print today's and this month's cost
//	mytoken gui             run the app (tray + main window)
//	mytoken daily           print a daily cost table
//	mytoken scan            index the logs once
//	mytoken help            list every command (see docs/CLI.md)
package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
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
	"github.com/zzstar101/mytoken/internal/cli"
	"github.com/zzstar101/mytoken/internal/gui"
	"github.com/zzstar101/mytoken/internal/ingestrate"
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
	// A bare invocation is the read-only cost summary. The GUI starts with
	// `mytoken gui`, or with --hidden when the login item launches it.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--hidden", "gui":
		default:
			os.Exit(cli.Run(os.Args[1:]))
		}
	} else {
		os.Exit(cli.Run(nil))
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

	a, err := app.Open()
	if err != nil {
		// Opening the index failed (a migration refused to run, a leftover
		// backup, a full disk, a locked file). The CLI reports this on stderr
		// and exits non-zero; the tray app must not vanish silently, so it
		// shows the error and the recovery steps in a real window and waits
		// for the user. showStartupFailure runs its own event loop and returns
		// only after that window is closed.
		showStartupFailure(err)
		return
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
	setLangFromSystem()

	mygo.App.WhenReady(func() {
		hidden := mygo.App.WasOpenedAtLogin() || len(os.Args) > 1 && os.Args[1] == "--hidden"
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

// setLangFromSystem localizes the GUI from the OS locale, unless MYTOKEN_LANG
// overrides it. Both the normal startup path and the startup-failure window use
// it, so the failure window speaks the user's language too.
func setLangFromSystem() {
	if l := mygo.App.Locale(); os.Getenv("MYTOKEN_LANG") == "" && l != "" {
		if strings.HasPrefix(strings.ToLower(l), "zh") {
			gui.SetLang("zh")
		} else {
			gui.SetLang("en")
		}
	}
}

// showStartupFailure reports a fatal startup error in a window instead of
// letting the process die with no UI. It shows the cause, the database path,
// the operator recovery guidance, a button that reveals the data directory and
// a Quit button, then runs the app's event loop for as long as that window is
// open. There is no retry loop: the process leaves when the user asks it to.
func showStartupFailure(cause error) {
	log.Printf("mytoken: %v", cause)
	setLangFromSystem()
	dbPath, dbErr := paths.DBPath()
	if dbErr != nil {
		// The path itself is the problem (a missing or unreadable data
		// directory): show why instead of an empty location.
		dbPath = dbErr.Error()
	}
	dataDir, dirErr := paths.ResolveDataDir()
	failure := app.DescribeStartupFailure(gui.Lang(), dbPath, cause)
	view := startupFailureView(failure,
		func() {
			if dirErr != nil {
				log.Printf("mytoken: data directory: %v", dirErr)
				return
			}
			if err := revealDirectory(dataDir); err != nil {
				log.Printf("mytoken: reveal data directory: %v", err)
			}
		},
		func() { mygo.App.Quit() })
	mygo.App.WhenReady(func() {
		// Deliberately not an accessory window: it must appear in the Dock or
		// taskbar and come to the front, unlike the tray's own panel.
		w := mygo.NewWindow(mygo.WindowOptions{
			Title:         failure.Title,
			Width:         760,
			Height:        640,
			MinWidth:      520,
			MinHeight:     420,
			TitleBarStyle: mygo.TitleBarDefault,
			Content:       ui.View(view),
		})
		w.OnClosed(func() { mygo.App.Quit() })
		w.Show()
		w.Focus()
	})
	if err := mygo.App.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "mytoken:", err)
		os.Exit(1)
	}
}

// startupFailureView draws the fatal-startup window. reveal and quit are the
// button actions, kept out of the drawing code so the view stays a function of
// the message alone.
func startupFailureView(f app.StartupFailure, reveal, quit func()) func(*ui.Context) {
	return func(c *ui.Context) {
		ui.Column(c).Fill().Padding(26).Gap(12).Children(func() {
			ui.Text(c, f.Title).FontSize(20).Bold()
			ui.Text(c, f.Summary).FontSize(13).Wrap()
			ui.Text(c, f.DatabaseLabel).FontSize(11.5).Bold()
			ui.Text(c, f.Database).FontSize(12).Wrap().Selectable()
			ui.Text(c, f.ErrorLabel).FontSize(11.5).Bold()
			ui.Scroll(c).Grow(1).Children(func() {
				ui.Text(c, f.Error).FontSize(11.5).Wrap().Selectable()
			})
			ui.Text(c, f.Guidance).FontSize(12).Wrap()
			ui.Row(c).Gap(10).Children(func() {
				if ui.PrimaryButton(c, f.RevealLabel).Clicked() {
					reveal()
				}
				if ui.Button(c, f.QuitLabel).Clicked() {
					quit()
				}
			})
		})
	}
}

// revealCommand is the platform command that shows a directory in the file
// manager. It is pure, so the mapping is testable on every platform.
func revealCommand(goos, dir string) (string, []string) {
	switch goos {
	case "darwin":
		return "open", []string{dir}
	case "windows":
		return "explorer", []string{dir}
	default:
		return "xdg-open", []string{dir}
	}
}

// revealDirectory hands dir to the platform file manager and returns without
// waiting: the file manager owns its own window.
func revealDirectory(dir string) error {
	name, args := revealCommand(runtime.GOOS, dir)
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// guiHooks is the window's view of the app main() has already opened: what the
// GUI may ask of it, never a second app.
func guiHooks(a *app.App, ctx context.Context, dataDir string, openMain func(), visible func() bool) gui.Hooks {
	var setProviderAlias func(ctx context.Context, id, label string) error
	if a.Store != nil {
		setProviderAlias = a.Store.SetProviderAlias
	}
	var ingestRate func(now time.Time) ingestrate.Snapshot
	if a.CurrentScanner() != nil {
		ingestRate = a.ScannerIngestRate
	}
	return gui.Hooks{
		Progress: a.ScannerProgress,
		Rebuild: func() {
			if err := a.Rebuild(ctx); err != nil {
				log.Printf("mytoken: rebuild: %v", err)
			}
		},
		OpenAtLogin:      mygo.App.OpenAtLogin,
		SetOpenAtLogin:   func(on bool) { _ = mygo.App.SetOpenAtLogin(on) },
		DataDir:          dataDir,
		Settings:         a.Settings,
		SetProviderAlias: setProviderAlias,
		Location:         a.Location,
		IngestRate:       ingestRate,
		OpenMain:         openMain,
		Quit:             func() { mygo.App.Quit() },
		Version:          cli.Version,
		CanUpdate:        mygo.Updater.Enabled,
		CheckUpdates:     updater.CheckForUpdates,
		AutoUpdates:      updater.AutomaticChecks,
		SetAutoUpdates:   updater.SetAutomaticChecks,
		Visible:          visible,
	}
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
