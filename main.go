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
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/plugins/updater"
	"github.com/egoist/mygo/plugins/updater/native"
	"github.com/egoist/mygo/ui"

	"github.com/zzstar101/mytoken/internal/app"
	"github.com/zzstar101/mytoken/internal/cli"
	"github.com/zzstar101/mytoken/internal/gui"
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

	a, err := app.Open()
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
	go func() {
		if err := a.Scanner.Run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("mytoken: scanner: %v", err)
		}
	}()
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
	hooks := gui.Hooks{
		Progress: a.Scanner.Progress,
		Rebuild: func() {
			if err := a.Scanner.Rebuild(ctx); err != nil {
				log.Printf("mytoken: rebuild: %v", err)
			}
		},
		OpenAtLogin:    mygo.App.OpenAtLogin,
		SetOpenAtLogin: func(on bool) { _ = mygo.App.SetOpenAtLogin(on) },
		DataDir:        dataDir,
		Settings:       a.Settings,
		Relays:         a.Relays,
		OpenMain:       func() { mygo.RunOnMain(openMain) },
		Quit:           func() { mygo.App.Quit() },
		Version:        cli.Version,
		CanUpdate:      mygo.Updater.Enabled,
		CheckUpdates:   updater.CheckForUpdates,
		AutoUpdates:    updater.AutomaticChecks,
		SetAutoUpdates: updater.SetAutomaticChecks,
		Visible: func() bool {
			return main != nil && main.IsVisible() || panel != nil && panel.IsVisible()
		},
	}
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
