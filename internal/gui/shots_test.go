package gui

import (
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

// harness wires an App to the demo data with a Post queue the test drains.
type harness struct {
	app *App
	mu  sync.Mutex
	q   []func()
	now time.Time
}

func newHarness(t testing.TB) *harness {
	h := &harness{now: time.Date(2026, 10, 19, 15, 30, 0, 0, time.Local)}
	lang := os.Getenv("GUI_SHOTS_LANG")
	if lang == "" {
		lang = "zh"
	}
	prefs := memPrefs{PrefLang: lang}
	h.app = New(Deps{
		Query:   newDemo(h.now),
		Quotas:  func() []Quota { return demoQuotas(h.now) },
		Sources: demoSources{},
		Prices:  demoPrices{},
		Maint:   demoMaint{},
		Prefs:   prefs,
		Version: "0.5.0-dev",
		Now:     func() time.Time { return h.now },
		Post: func(fn func()) {
			h.mu.Lock()
			h.q = append(h.q, fn)
			h.mu.Unlock()
		},
		SetTrayTitle: func(string) {},
		Reveal:       func(string) {},
		PickFolder:   func(string) (string, bool) { return "", false },
	})
	h.app.Start()
	h.app.QuotasChanged()
	h.drain(t)
	return h
}

// drain runs posted work until every job settled.
func (h *harness) drain(t testing.TB) {
	deadline := time.Now().Add(20 * time.Second)
	for {
		h.mu.Lock()
		q := h.q
		h.q = nil
		h.mu.Unlock()
		for _, fn := range q {
			fn()
		}
		if len(q) == 0 && !h.app.busy() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("jobs did not settle")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func shotDir(t testing.TB) string {
	dir := os.Getenv("GUI_SHOTS")
	if dir == "" {
		t.Skip("set GUI_SHOTS=<dir> to write screenshots")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func save(t testing.TB, tt *ui.Tester, path string) {
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, tt.Image()); err != nil {
		t.Fatal(err)
	}
}

// settle renders, lets posted loads land and animations finish.
func settle(t testing.TB, h *harness, tt *ui.Tester) {
	for range 3 {
		tt.Frame()
		h.drain(t)
	}
	tt.Advance(3 * time.Second)
	tt.Frame()
}

func TestScreenshots(t *testing.T) {
	dir := shotDir(t)
	for _, dark := range []bool{false, true} {
		suffix := "light"
		if dark {
			suffix = "dark"
		}
		h := newHarness(t)
		tt := ui.NewTester(h.app.MainView, 1280, 860)
		tt.SetDark(dark)
		tt.SetScale(2)
		for _, page := range []string{"overview", "sessions", "ranking", "projects", "settings"} {
			h.app.goTo(page)
			settle(t, h, tt)
			if page == "sessions" && len(h.app.ss.shown) > 0 {
				h.app.ssSel = 0
				h.app.openSession(h.app.ss.rows[h.app.ss.shown[0]])
				settle(t, h, tt)
			}
			if page == "projects" && len(h.app.pj.rows) > 0 {
				h.app.pjSel = h.app.pj.rows[0].Key
				h.app.reloadPage()
				settle(t, h, tt)
			}
			save(t, tt, filepath.Join(dir, page+"-"+suffix+".png"))
		}
		for _, g := range []string{"sources", "prices", "data"} {
			h.app.set.group = g
			settle(t, h, tt)
			save(t, tt, filepath.Join(dir, "settings-"+g+"-"+suffix+".png"))
		}
		for _, page := range []string{"overview", "ranking"} {
			h.app.goTo(page)
			tt.SetSize(1280, 1640)
			settle(t, h, tt)
			save(t, tt, filepath.Join(dir, page+"-full-"+suffix+".png"))
		}
		tt.SetSize(1280, 860)
		pt := ui.NewTester(h.app.PanelView, TrayWidth, TrayHeight)
		pt.SetDark(dark)
		pt.SetScale(2)
		settle(t, h, pt)
		save(t, pt, filepath.Join(dir, "tray-"+suffix+".png"))
		t.Logf("tray title %q", h.app.TrayTitle())
	}
}

// BenchmarkFrame times one full build+layout+paint of each page.
func BenchmarkFrame(b *testing.B) {
	for _, page := range []string{"overview", "sessions", "ranking", "projects", "settings"} {
		b.Run(page, func(b *testing.B) {
			h := newHarness(b)
			tt := ui.NewTester(h.app.MainView, 1280, 860)
			h.app.goTo(page)
			settle(b, h, tt)
			if page == "sessions" && len(h.app.ss.shown) > 0 {
				h.app.openSession(h.app.ss.rows[h.app.ss.shown[0]])
				settle(b, h, tt)
			}
			b.ResetTimer()
			for range b.N {
				tt.Frame()
			}
		})
	}
}
