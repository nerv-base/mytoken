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
	"math"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
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
	"github.com/zzstar101/mytoken/internal/paths"
	"github.com/zzstar101/mytoken/internal/quota/plan"
	"github.com/zzstar101/mytoken/internal/sysproxy"
)

// memoryLimit is the GUI's soft Go heap limit. Live data is ~20 MB on a
// 200K-event history; the rest of the process is the window system.
const memoryLimit = 64 << 20

func main() {
	// Finder, the Dock and the login item start the app with no HTTP_PROXY,
	// HTTPS_PROXY or NO_PROXY in its environment, so Go's default transport
	// would ignore the proxy configured in System Settings. Install teaches
	// it the OS proxy before anything can open a socket: the updater, the
	// pricing refresh and every quota provider use http.DefaultTransport,
	// and so does the app the CLI opens.
	sysproxy.Install()

	// `mygo build` cannot pass -ldflags, but it embeds mygo.json's version.
	if cli.Version == "dev" {
		if v := mygo.App.Version(); v != "" {
			cli.Version = v
		}
	}
	// A bare invocation from a terminal is the read-only cost summary; from
	// Finder, Explorer or a desktop launcher (no terminal) it is the app. The
	// GUI also starts with `mytoken gui`, or --hidden from the login item.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--hidden", "gui":
		default:
			os.Exit(cli.Run(os.Args[1:]))
		}
	} else if fromTerminal() {
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
	// A separate MYTOKEN_HOME is a separate index, so a development or
	// test run beside the installed app does not hand itself over to it.
	if os.Getenv("MYTOKEN_HOME") == "" && !mygo.App.RequestSingleInstanceLock() {
		return
	}
	mygo.App.OnSecondInstance(func([]string, string) { openMain() })

	// Checks github.com for a new version once a day; builds that cannot
	// replace themselves, such as development builds and the Debian
	// package, never check.
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
	if err := a.StartScanning(ctx); err != nil {
		log.Printf("mytoken: scan: %v", err)
	}
	setLangFromSystem()

	// Both windows draw the same App; a change redraws both. Updates go
	// through the panel, which outlives the main window.
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
	var tray *mygo.Tray
	deps := guiDeps(a)
	deps.Post = post
	deps.OpenMain = func() { mygo.RunOnMain(openMain) }
	deps.HidePanel = func() {
		if panel != nil {
			panel.Hide()
		}
	}
	deps.Quit = func() { mygo.App.Quit() }
	deps.Reveal = func(path string) { mygo.Shell.ShowItemInFolder(path) }
	deps.PickFolder = func(start string) (string, bool) {
		paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{Parent: main, DefaultPath: start, Directory: true, ShowHiddenFiles: true})
		if err != nil || len(paths) == 0 {
			return "", false
		}
		return paths[0], true
	}
	deps.SaveFile = func(name string) (string, bool) {
		path, err := mygo.Dialog.Save(mygo.SaveDialogOptions{Parent: main, DefaultPath: name, CreateDirectories: true})
		return path, err == nil && path != ""
	}
	deps.Login = login{}
	deps.Updates = updates{enabled: mygo.Updater.Enabled()}
	deps.SetTrayTitle = func(title string) {
		if tray != nil {
			tray.SetTitle(title)
			tip := "MyToken!!!!!"
			if title != "" {
				tip += " · " + title
			}
			tray.SetToolTip(tip)
		}
	}
	ui2 := gui.New(deps)
	if a.Quota != nil {
		a.Quota.SetOnChange(func([]plan.Snapshot) { ui2.QuotasChanged() })
	}

	mygo.App.WhenReady(func() {
		hidden := mygo.App.WasOpenedAtLogin() || len(os.Args) > 1 && os.Args[1] == "--hidden"
		// A quit (the tray menu, Cmd+Q, SIGTERM, logging out) closes every
		// window; only a close by the user leaves the app in the tray.
		quitting := false
		mygo.App.OnBeforeQuit(func(*mygo.QuitEvent) { quitting = true; ui2.Stop() })
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
				// The traffic lights sit inside the sidebar, not on its edge.
				TrafficLightPosition: &mygo.Point{X: gui.TrafficLightX, Y: gui.TrafficLightY},
				StateKey:             "main",
				Content:              ui.View(ui2.MainView),
			})
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
			Content:         ui.View(ui2.PanelView),
		})
		panel.OnBlur(func() { panel.Hide() })
		ready = true
		ui2.Start()

		menu := trayMenu(openMain, func() { logErr("rescan", a.Rescan(context.Background())) }, deps.Updates.Enabled())
		opts := mygo.TrayOptions{Icon: trayIcon(), IconIsTemplate: true, ToolTip: "MyToken!!!!!"}
		if runtime.GOOS != "darwin" {
			// Windows shows the menu on a right click and keeps the left
			// click for the panel; Linux indicators only have a menu.
			opts.Menu = menu
		}
		var err error
		tray, err = mygo.NewTray(opts)
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
		if runtime.GOOS == "darwin" {
			// A status item with a menu opens it on every click, so the menu
			// is lent to the item only while a right click shows it.
			tray.OnRightClick(func() {
				panel.Hide()
				tray.SetMenu(menu)
				tray.PopUpMenu()
				tray.SetMenu(nil)
			})
		}
		ui2.QuotasChanged() // also sets the menu bar text
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

// trayMenu is the icon's context menu.
func trayMenu(open, rescan func(), canUpdate bool) *mygo.Menu {
	l := func(en, zh string) string {
		if gui.Lang() == "zh" {
			return zh
		}
		return en
	}
	items := []*mygo.MenuItem{
		{Label: l("Open MyToken", "打开 MyToken"), Click: func(*mygo.MenuItem, *mygo.Window) { open() }},
		{Label: l("Rescan logs", "重新扫描"), Click: func(*mygo.MenuItem, *mygo.Window) { go rescan() }},
	}
	if canUpdate {
		items = append(items, &mygo.MenuItem{Label: l("Check for Updates…", "检查更新…"),
			Click: func(*mygo.MenuItem, *mygo.Window) { updater.CheckForUpdates() }})
	}
	items = append(items, mygo.Separator(),
		&mygo.MenuItem{Label: l("Quit MyToken", "退出 MyToken"), Click: func(*mygo.MenuItem, *mygo.Window) { mygo.App.Quit() }})
	return mygo.NewMenu(items)
}

// updates is the updater plugin behind the settings' update rows. Whether
// this copy can replace itself is probed once (it writes next to the app).
type updates struct{ enabled bool }

func (u updates) Enabled() bool      { return u.enabled }
func (updates) Check()               { updater.CheckForUpdates() }
func (updates) Automatic() bool      { return updater.AutomaticChecks() }
func (updates) SetAutomatic(on bool) { updater.SetAutomaticChecks(on) }

// login turns the login item on and off.
type login struct{}

func (login) OpenAtLogin() bool            { return mygo.App.OpenAtLogin() }
func (login) SetOpenAtLogin(on bool) error { return mygo.App.SetOpenAtLogin(on) }

func versionLabel() string { return cli.Version }

// setLangFromSystem localizes the GUI from the OS locale, unless MYTOKEN_LANG
// overrides it. Both the normal startup path and the startup-failure window use
// it, so the failure window speaks the user's language too.
func setLangFromSystem() {
	gui.SetSystemLang(mygo.App.Locale())
	gui.SetLang("")
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

// trayIcon draws the template icon: the app icon's mark in one color, a
// quota ring three quarters round about a dot, anti-aliased.
func trayIcon() []byte {
	const n = 36
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	c := float64(n) / 2
	cover := func(d float64) float64 { return math.Max(0, math.Min(1, 0.5-d)) }
	const ringR, ringW, dotR = 14.0, 3.6, 7.0
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			dx, dy := float64(x)+0.5-c, float64(y)+0.5-c
			r := math.Hypot(dx, dy)
			// The ring runs clockwise from the top for 0.72 of a turn,
			// with round ends.
			ang := math.Mod(math.Atan2(dy, dx)+math.Pi/2+2*math.Pi, 2*math.Pi) / (2 * math.Pi)
			ring := 0.0
			if ang <= 0.72 {
				ring = cover(math.Abs(r-ringR) - ringW/2)
			}
			for _, a := range []float64{0, 0.72} {
				th := a*2*math.Pi - math.Pi/2
				ring = math.Max(ring, cover(math.Hypot(dx-math.Cos(th)*ringR, dy-math.Sin(th)*ringR)-ringW/2))
			}
			v := math.Max(ring, cover(r-dotR))
			if v > 0 {
				img.Set(x, y, color.NRGBA{A: uint8(v*255 + 0.5)})
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// fromTerminal reports whether standard output is a terminal, which is how a
// shell user's bare `mytoken` differs from opening the app bundle.
func fromTerminal() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
