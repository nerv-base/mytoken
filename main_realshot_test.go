package main

import (
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/zzstar101/mytoken/internal/app"
	"github.com/zzstar101/mytoken/internal/gui"
)

// TestRealShots renders the interface over the real index at MYTOKEN_HOME,
// so the screens can be reviewed on this machine's data without a window:
//
//	MYTOKEN_HOME=/tmp/mt-home MYTOKEN_REALSHOTS=/tmp/mt-real go test -run RealShots .
//
// It also times each page's data load and frame.
func TestRealShots(t *testing.T) {
	dir := os.Getenv("MYTOKEN_REALSHOTS")
	if dir == "" {
		t.Skip("set MYTOKEN_REALSHOTS=dir (and MYTOKEN_HOME to a copy of the index)")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	a, err := app.OpenLocal()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	var mu sync.Mutex
	var queue []func()
	deps := guiDeps(a)
	deps.Post = func(fn func()) { mu.Lock(); queue = append(queue, fn); mu.Unlock() }
	g := gui.New(deps)
	g.Start()
	defer g.Stop()
	g.QuotasChanged()
	drain := func() {
		deadline := time.Now().Add(time.Minute)
		idle := 0
		for idle < 50 && time.Now().Before(deadline) {
			mu.Lock()
			q := queue
			queue = nil
			mu.Unlock()
			for _, fn := range q {
				fn()
			}
			if len(q) == 0 {
				idle++
			} else {
				idle = 0
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	shot := func(tt *ui.Tester, name string) {
		for range 3 {
			tt.Frame()
			drain()
		}
		tt.Advance(3 * time.Second)
		start := time.Now()
		tt.Frame()
		t.Logf("%s: frame %v", name, time.Since(start))
		f, err := os.Create(filepath.Join(dir, name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, tt.Image()); err != nil {
			t.Fatal(err)
		}
	}
	tt := ui.NewTester(g.MainView, 1280, 860)
	tt.SetScale(2)
	for _, page := range []string{"overview", "sessions", "ranking", "projects", "settings"} {
		start := time.Now()
		g.GoTo(page)
		drain()
		t.Logf("%s: load %v", page, time.Since(start))
		shot(tt, page)
	}
	pt := ui.NewTester(g.PanelView, gui.TrayWidth, gui.TrayHeight)
	pt.SetScale(2)
	shot(pt, "tray")
	t.Logf("tray title %q", g.TrayTitle())
}
