package gui

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/model"
)

// This file covers the first native-control migration batch: the segmented
// switcher on ui.SegmentedBase and the token ring on ui/chart. The tests
// are about the contract the app needs (selection, key mapping, empty and
// unknown data, accessibility semantics, reduced motion), not about the
// controls' pixels; the focused snapshots at the end are for review.

// nativeTester lays out a view of one control and settles it, so that the
// elements have bounds and the entrance animations have a second frame.
func nativeTester(t *testing.T, w, h int, reduceMotion bool, view func(*ui.Context)) *ui.Tester {
	t.Helper()
	tt := ui.NewTester(view, w, h)
	tt.SetScale(1)
	tt.SetPreferences(ui.Preferences{ReduceMotion: reduceMotion})
	for i := 0; i < 4; i++ {
		tt.Frame()
	}
	return tt
}

// segmentedView builds a segmented control on a small canvas.
func segmentedView(sel *int, labels []string) func(*ui.Context) {
	return func(c *ui.Context) {
		pal := applyTheme(c)
		ui.Column(c).Padding(14).Children(func() {
			segmented(c, pal, sel, labels...)
		})
	}
}

// sampledSegment returns the color at the left padding of the segment with
// the given label: inside the pill, left of its text, so it shows the
// raised thumb when that segment is chosen and the track otherwise.
func sampledSegment(t *testing.T, tt *ui.Tester, label string) color.RGBA {
	t.Helper()
	r, ok := tt.Find(label)
	if !ok {
		t.Fatalf("segment %q is not laid out", label)
	}
	return tt.Image().RGBAAt(int(r.X)+6, int(r.Y+r.H/2))
}

func TestSegmentedClickSelectsAndReportsChange(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	sel := 0
	changes := 0
	tt := nativeTester(t, 420, 64, true, func(c *ui.Context) {
		pal := applyTheme(c)
		ui.Column(c).Padding(14).Children(func() {
			if segmented(c, pal, &sel, "Alpha", "Beta", "Gamma") {
				changes++
			}
		})
	})
	changes = 0
	if err := tt.Click("Gamma"); err != nil {
		t.Fatalf("click Gamma: %v", err)
	}
	tt.Frame()
	if sel != 2 {
		t.Errorf("after clicking Gamma the choice is %d, want 2", sel)
	}
	if changes != 1 {
		t.Errorf("one click reported changed %d times, want 1", changes)
	}
	// Clicking the choice already made changes nothing.
	changes = 0
	if err := tt.Click("Gamma"); err != nil {
		t.Fatalf("click Gamma again: %v", err)
	}
	tt.Frame()
	if sel != 2 || changes != 0 {
		t.Errorf("re-clicking the chosen segment: choice %d, changed %d times, want 2 and 0", sel, changes)
	}
}

// The switcher is a radio group: the arrows move the keyboard focus and
// the choice with it, so it is reachable without a pointer.
func TestSegmentedArrowKeysMoveChoiceAndFocus(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	sel := 0
	tt := nativeTester(t, 420, 64, true, segmentedView(&sel, []string{"Alpha", "Beta", "Gamma"}))
	if err := tt.Click("Alpha"); err != nil {
		t.Fatalf("click Alpha: %v", err)
	}
	tt.Frame()
	tt.Key(0, ui.KeyRight)
	tt.Frame()
	if sel != 1 {
		t.Errorf("Right from the first segment chose %d, want 1", sel)
	}
	if !tt.Focused("Beta") {
		t.Error("Right did not move the keyboard focus to the second segment")
	}
	// A radio group wraps at both ends.
	tt.Key(0, ui.KeyRight)
	tt.Frame()
	if sel != 2 {
		t.Errorf("Right from the second segment chose %d, want 2", sel)
	}
	tt.Key(0, ui.KeyRight)
	tt.Frame()
	if sel != 0 {
		t.Errorf("Right past the last segment chose %d, want it to wrap to 0", sel)
	}
	tt.Key(0, ui.KeyLeft)
	tt.Frame()
	if sel != 2 {
		t.Errorf("Left from the first segment chose %d, want it to wrap to 2", sel)
	}
}

// A choice read from stored state that no longer matches the labels must
// not leave the control with nothing chosen and a bad index to dereference
// later (loadSessions indexes sortKeys by sortIdx).
func TestSegmentedClampsAChoiceOutOfRange(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	for _, tc := range []struct{ in, want int }{{99, 2}, {-4, 0}, {1, 1}} {
		sel := tc.in
		nativeTester(t, 420, 64, true, segmentedView(&sel, []string{"Alpha", "Beta", "Gamma"}))
		if sel != tc.want {
			t.Errorf("choice %d settled at %d, want %d", tc.in, sel, tc.want)
		}
	}
}

func TestSegmentedWithoutLabelsIsInert(t *testing.T) {
	sel := 1
	tt := nativeTester(t, 200, 64, true, segmentedView(&sel, nil))
	if sel != 1 {
		t.Errorf("a control without labels moved the choice to %d, want it left at 1", sel)
	}
	if _, ok := tt.Find("Alpha"); ok {
		t.Error("a control without labels laid out a segment")
	}
}

func TestSegmentedSingleChoiceReportsNoChange(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	sel := 0
	changes := 0
	tt := nativeTester(t, 200, 64, true, func(c *ui.Context) {
		pal := applyTheme(c)
		ui.Column(c).Padding(14).Children(func() {
			if segmented(c, pal, &sel, "Only") {
				changes++
			}
		})
	})
	changes = 0
	if err := tt.Click("Only"); err != nil {
		t.Fatalf("click the only segment: %v", err)
	}
	tt.Frame()
	if sel != 0 || changes != 0 {
		t.Errorf("clicking the only segment: choice %d, changed %d times, want 0 and 0", sel, changes)
	}
}

// The choice is bound to the caller's index, not to an element, so it
// survives the elements being rebuilt on a resize.
func TestSegmentedChoiceSurvivesResize(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	sel := 1
	tt := nativeTester(t, 420, 64, true, segmentedView(&sel, []string{"Alpha", "Beta", "Gamma"}))
	if err := tt.Click("Gamma"); err != nil {
		t.Fatalf("click Gamma: %v", err)
	}
	tt.Frame()
	if sel != 2 {
		t.Fatalf("after clicking Gamma the choice is %d, want 2", sel)
	}
	tt.SetSize(300, 90)
	for i := 0; i < 3; i++ {
		tt.Frame()
	}
	if sel != 2 {
		t.Errorf("the choice is %d after a resize, want 2", sel)
	}
	if !tt.HasText("Gamma") {
		t.Error("the segments are gone after a resize")
	}
}

// Reduce Motion means no thumb to slide: it is on the new choice in the
// frame the click is handled, not at the end of a 320ms ease.
func TestSegmentedThumbSettlesAtOnceWithReducedMotion(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	sel := 0
	tt := nativeTester(t, 420, 64, true, segmentedView(&sel, []string{"Alpha", "Beta", "Gamma"}))
	chosen, plain := sampledSegment(t, tt, "Alpha"), sampledSegment(t, tt, "Gamma")
	if chosen == plain {
		t.Fatalf("no thumb is drawn: the chosen and the plain segment both sample %v", chosen)
	}
	if err := tt.Click("Gamma"); err != nil {
		t.Fatalf("click Gamma: %v", err)
	}
	tt.Frame()
	if got := sampledSegment(t, tt, "Gamma"); got != chosen {
		t.Errorf("with reduced motion Gamma samples %v one frame after the click, want the thumb %v", got, chosen)
	}
	if got := sampledSegment(t, tt, "Alpha"); got != plain {
		t.Errorf("with reduced motion Alpha still samples the thumb %v, want %v", got, plain)
	}
}

// donutSlices must map the token classes the app draws to the slices the
// chart package hands back, dropping what takes no room.
func TestDonutSlicesDropZeroAndUnknownClasses(t *testing.T) {
	for _, tc := range []struct {
		name string
		tok  model.Tokens
		want []int
	}{
		{"empty", model.Tokens{}, nil},
		{"unknown", model.Tokens{Input: -5, Output: -1}, nil},
		{"one", model.Tokens{Input: 10}, []int{0}},
		{"gaps", model.Tokens{Input: 3, CacheRead: 1}, []int{0, 2}},
		{"all", model.Tokens{Input: 1, Output: 1, CacheRead: 1, CacheWrite: 1, Reasoning: 1}, []int{0, 1, 2, 3, 4}},
	} {
		slices, idx := donutSlices(tc.tok)
		if len(slices) != len(tc.want) {
			t.Errorf("%s: %d slices, want %d", tc.name, len(slices), len(tc.want))
			continue
		}
		for i, want := range tc.want {
			if idx[i] != want {
				t.Errorf("%s: slice %d is class %d, want %d", tc.name, i, idx[i], want)
			}
		}
	}
	// A class with no tokens takes no room: the drawn slices still start at
	// twelve o'clock and end back there.
	slices, _ := donutSlices(model.Tokens{Input: 3, CacheRead: 1})
	if slices[0].Start != 0 {
		t.Errorf("the first slice starts at %v, want 0", slices[0].Start)
	}
	if math.Abs(slices[1].End-2*math.Pi) > 1e-9 {
		t.Errorf("the last slice ends at %v, want 2π", slices[1].End)
	}
	// Room follows the share: three times the tokens, three times the angle
	// past the pad.
	a := slices[0].End - slices[0].Start - donutGap
	b := slices[1].End - slices[1].Start - donutGap
	if math.Abs(a/b-3) > 1e-6 {
		t.Errorf("the slices are %v and %v wide past the pad, want a ratio of 3", a, b)
	}
}

// donutHover is the pointer's slice, through the chart package's own hit
// test: the angle names the class, and the class index is the one in
// classes, past the ones that draw nothing.
func TestDonutHoverMapsTheAngleToTheClass(t *testing.T) {
	const inner, outer float32 = 51, 75
	mid := (inner + outer) / 2
	at := func(a float64) (float32, float32) {
		return mid * float32(math.Sin(a)), -mid * float32(math.Cos(a))
	}
	// Two equal classes: the first fills the right half from twelve o'clock.
	tok := model.Tokens{Input: 50, Output: 50}
	if x, y := at(math.Pi / 4); donutHover(tok, x, y, inner, outer) != 0 {
		t.Errorf("a quarter turn in the hover is class %d, want 0 (input)", donutHover(tok, x, y, inner, outer))
	}
	if x, y := at(5 * math.Pi / 4); donutHover(tok, x, y, inner, outer) != 1 {
		t.Errorf("five quarters turn in the hover is class %d, want 1 (output)", donutHover(tok, x, y, inner, outer))
	}
	// Off the ring, and with nothing to show, there is no class.
	if got := donutHover(tok, 0, inner/2, inner, outer); got != -1 {
		t.Errorf("a hover in the hole is class %d, want none", got)
	}
	if got := donutHover(tok, 0, outer+5, inner, outer); got != -1 {
		t.Errorf("a hover outside the ring is class %d, want none", got)
	}
	if got := donutHover(model.Tokens{}, 0, -mid, inner, outer); got != -1 {
		t.Errorf("a hover on an empty ring is class %d, want none", got)
	}
	// Input then reasoning: the second slice drawn is class 4.
	tok = model.Tokens{Input: 50, Reasoning: 50}
	if x, y := at(5 * math.Pi / 4); donutHover(tok, x, y, inner, outer) != 4 {
		t.Errorf("the second slice drawn is class %d, want 4 (reasoning)", donutHover(tok, x, y, inner, outer))
	}
}

// donutImage renders one donut and returns the tester and its frame, so
// that samples can be taken from the ring the layout gave it.
func donutImage(t *testing.T, tok model.Tokens, frames int, reduceMotion bool) (*ui.Tester, *image.RGBA) {
	t.Helper()
	tt := ui.NewTester(func(c *ui.Context) {
		pal := applyTheme(c)
		ui.Column(c).Padding(20).Children(func() {
			donut(c, pal, tok, 150).Label("donut")
		})
	}, 200, 200)
	tt.SetScale(1)
	tt.SetPreferences(ui.Preferences{ReduceMotion: reduceMotion})
	for i := 0; i < frames; i++ {
		tt.Frame()
	}
	return tt, tt.Image()
}

// ringSample returns the pixel at the given angle, halfway across the ring.
func ringSample(t *testing.T, tt *ui.Tester, img *image.RGBA, angle float64) color.RGBA {
	t.Helper()
	r, ok := tt.Find("donut")
	if !ok {
		t.Fatal("the donut is not laid out")
	}
	ro := r.W / 2
	ri := ro * 0.68
	rad := (ri + ro) / 2
	x := int(r.X + r.W/2 + rad*float32(math.Sin(angle)))
	y := int(r.Y + r.H/2 - rad*float32(math.Cos(angle)))
	return img.RGBAAt(x, y)
}

func TestDonutRingDrawsEachClassAndAnEmptyMix(t *testing.T) {
	emptyTester, empty := donutImage(t, model.Tokens{}, 4, true)
	// Nothing to show: the ring is one flat well, the same all the way round.
	if a, b := ringSample(t, emptyTester, empty, math.Pi/4), ringSample(t, emptyTester, empty, 5*math.Pi/4); a != b {
		t.Errorf("an empty mix draws %v at one diagonal and %v at the other, want the plain ring", a, b)
	}
	halfTester, half := donutImage(t, model.Tokens{Input: 50, Output: 50}, 4, true)
	a, b := ringSample(t, halfTester, half, math.Pi/4), ringSample(t, halfTester, half, 5*math.Pi/4)
	if a == b {
		t.Errorf("two equal classes draw %v in both halves, want the two class colors", a)
	}
	if a == ringSample(t, emptyTester, empty, math.Pi/4) || b == ringSample(t, emptyTester, empty, 5*math.Pi/4) {
		t.Error("a half with tokens still shows the empty ring there")
	}
}

// Reduce Motion settles the entrance sweep at once; without it the ring is
// still growing a frame after the first build.
func TestDonutSweepSettlesAtOnceWithReducedMotion(t *testing.T) {
	tok := model.Tokens{Input: 50, Output: 50}
	stillTester, still := donutImage(t, tok, 2, true)
	emptyTester, empty := donutImage(t, model.Tokens{}, 2, true)
	for _, angle := range []float64{math.Pi / 4, 5 * math.Pi / 4} {
		if ringSample(t, stillTester, still, angle) == ringSample(t, emptyTester, empty, angle) {
			t.Errorf("with reduced motion the ring at %v is still the empty ring after the second frame", angle)
		}
	}
	movingTester, moving := donutImage(t, tok, 2, false)
	if ringSample(t, movingTester, moving, 5*math.Pi/4) != ringSample(t, emptyTester, empty, 5*math.Pi/4) {
		t.Error("without reduced motion the ring is already drawn at the far diagonal after the second frame")
	}
}

// The switcher's callers act on the new choice in the same frame it is
// made (spanSwitch calls setSpan right after), while the framework's own
// binding only lands after the build. This drives the real span switch and
// checks that the click really loaded the span it chose.
func TestSpanSwitchClickLoadsTheNewSpan(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	now := time.Date(2026, 5, 20, 15, 0, 0, 0, time.Local)
	q := &countingService{Service: NewDemoService(now)}
	l := newLoop()
	s := NewState(q, Hooks{Now: func() time.Time { return now }}, l.post)
	l.do(s.Reload)
	waitFor(t, func() bool {
		var ok bool
		l.do(func() { ok = s.spans[Span7].ok && s.spans[SpanToday].ok })
		return ok
	})
	tt := ui.NewTester(func(c *ui.Context) {
		l.do(func() { s.spanSwitch(c, applyTheme(c)) })
	}, 700, 64)
	tt.SetScale(1)
	tt.SetPreferences(ui.Preferences{ReduceMotion: true})
	for i := 0; i < 3; i++ {
		tt.Frame()
	}
	if err := tt.Click(tr(spanKeys[Span30])); err != nil {
		t.Fatalf("click %q: %v", tr(spanKeys[Span30]), err)
	}
	tt.Frame()
	if got := func() int { var v int; l.do(func() { v = s.span }); return v }(); got != int(Span30) {
		t.Fatalf("the switch chose index %d, want %d", got, int(Span30))
	}
	waitFor(t, func() bool {
		var ok bool
		l.do(func() { ok = s.ov.Span == Span30 })
		return ok
	})
}

// TestNativeControlSnapshots renders the migrated controls alone, for
// review next to the same views before the migration: set
// MYTOKEN_NATIVE_SNAPSHOTS=dir to write them.
func TestNativeControlSnapshots(t *testing.T) {
	dir := os.Getenv("MYTOKEN_NATIVE_SNAPSHOTS")
	if dir == "" {
		t.Skip("set MYTOKEN_NATIVE_SNAPSHOTS=dir to render")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	defer SetLang(Lang())
	for _, lang := range []string{"zh", "en"} {
		SetLang(lang)
		spans := make([]string, len(spanKeys))
		for i, k := range spanKeys {
			spans[i] = tr(k)
		}
		segViews := []struct {
			name   string
			sel    int
			labels []string
		}{
			{"segmented-rank", 1, []string{tr("byModel"), tr("byProvider"), tr("byHarness")}},
			{"segmented-span", 3, spans},
			{"segmented-sort", 2, []string{tr("recent"), tr("mostTokens"), tr("mostCost")}},
			{"segmented-one", 0, []string{tr("recent")}},
			{"segmented-none", 0, nil},
		}
		for _, v := range segViews {
			sel := v.sel
			view := segmentedView(&sel, v.labels)
			for _, dark := range []bool{false, true} {
				name := fmt.Sprintf("%s-%s", v.name, lang)
				if dark {
					name += "-dark"
				}
				shot(t, filepath.Join(dir, name+".png"), view, 420, 64, dark)
			}
		}
		donViews := []struct {
			name string
			tok  model.Tokens
		}{
			{"donut-mixed", model.Tokens{Input: 412000, Output: 88400, CacheRead: 1250000, CacheWrite: 96000, Reasoning: 31200}},
			{"donut-single", model.Tokens{Input: 100000}},
			{"donut-two", model.Tokens{Input: 50000, Output: 50000}},
			{"donut-uneven", model.Tokens{Input: 1, Reasoning: 999999}},
			{"donut-zero", model.Tokens{}},
		}
		for _, v := range donViews {
			view := func(c *ui.Context) {
				pal := applyTheme(c)
				ui.Column(c).Padding(20).Children(func() {
					donut(c, pal, v.tok, 150).Label("donut")
				})
			}
			for _, dark := range []bool{false, true} {
				name := fmt.Sprintf("%s-%s", v.name, lang)
				if dark {
					name += "-dark"
				}
				shot(t, filepath.Join(dir, name+".png"), view, 200, 200, dark)
			}
		}
	}
}
