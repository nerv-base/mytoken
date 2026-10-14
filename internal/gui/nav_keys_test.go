package gui

import (
	"image/color"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

// This file covers the sidebar's keyboard navigation: the native focus
// group on the left navigation (arrows, Home and End, a labelled focusable
// container, the page following the focused row), the sessions list's rows,
// and the visuals the app owns and keeps: the per-page accent, the pick
// decoration and the gliding pill.

// navState builds the demo state the way the running app does.
func navState(t *testing.T) *State {
	t.Helper()
	now := time.Date(2026, 10, 9, 15, 30, 0, 0, time.Local)
	s := NewState(NewDemoService(now), Hooks{Settings: NewDemoSettings(), Now: func() time.Time { return now },
		Progress: func() (int, int) { return 10, 10 }}, nil)
	s.Start()
	return s
}

// navTester lays the whole window out on the dark theme and settles it.
func navTester(t *testing.T, reduceMotion bool) (*ui.Tester, *State) {
	t.Helper()
	s := navState(t)
	tt := ui.NewTester(s.MainView, 1280, 860)
	tt.SetScale(1)
	tt.SetDark(true)
	tt.SetPreferences(ui.Preferences{ReduceMotion: reduceMotion})
	for i := 0; i < 4; i++ {
		tt.Frame()
	}
	return tt, s
}

// navLabel is the sidebar row's label for page i.
func navLabel(i int) string { return tr(navItems[i].id) }

// navRowBox is the laid-out box of the sidebar row for page i.
func navRowBox(t *testing.T, tt *ui.Tester, i int) ui.Rect {
	t.Helper()
	r, ok := tt.Find(navLabel(i))
	if !ok {
		t.Fatalf("the %q row is not laid out", navLabel(i))
	}
	return r
}

// navPillPixel samples the left padding of a row: inside the gliding pill
// when that row is the chosen one, and left of its icon in any case.
func navPillPixel(t *testing.T, tt *ui.Tester, i int) color.RGBA {
	t.Helper()
	r := navRowBox(t, tt, i)
	return tt.Image().RGBAAt(int(r.X)+4, int(r.Y+r.H/2))
}

// navPickPixel samples the pick decoration at the end of a row, which the
// chosen row alone paints, in that page's own color.
func navPickPixel(t *testing.T, tt *ui.Tester, i int) color.RGBA {
	t.Helper()
	r := navRowBox(t, tt, i)
	return tt.Image().RGBAAt(int(r.X+r.W)-17, int(r.Y+r.H/2))
}

// nearColor reports whether a rendered pixel is c, within a tolerance that
// leaves room for antialiasing and color conversion.
func nearColor(p color.RGBA, c ui.Color, tol int) bool {
	diff := func(a, b uint8) int {
		if a > b {
			return int(a - b)
		}
		return int(b - a)
	}
	return diff(p.R, c.R) <= tol && diff(p.G, c.G) <= tol && diff(p.B, c.B) <= tol
}

// nearPixel reports whether two rendered pixels are the same color, within
// a tolerance that leaves room for the glass behind them changing by a
// value from row to row.
func nearPixel(a, b color.RGBA, tol int) bool {
	diff := func(x, y uint8) int {
		if x > y {
			return int(x - y)
		}
		return int(y - x)
	}
	return diff(a.R, b.R) <= tol && diff(a.G, b.G) <= tol && diff(a.B, b.B) <= tol
}

// TestNavigationIsALabelledContainer asserts the navigation is named and
// that its box holds every row: the container semantics assistive tools
// and the focus group read.
func TestNavigationIsALabelledContainer(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	tt, _ := navTester(t, true)
	box, ok := tt.Find(tr("navigation"))
	if !ok {
		t.Fatalf("the navigation container %q is not laid out", tr("navigation"))
	}
	first := navRowBox(t, tt, 0)
	last := navRowBox(t, tt, len(navItems)-1)
	if first.X < box.X-1 || first.X+first.W > box.X+box.W+1 {
		t.Errorf("row 0 %v is not inside the navigation %v", first, box)
	}
	if last.Y < box.Y-1 || last.Y+last.H > box.Y+box.H+1 {
		t.Errorf("the last row %v is not inside the navigation %v", last, box)
	}
	if box.H <= first.H {
		t.Errorf("the navigation %v is no taller than one row %v", box, first)
	}
}

// TestNavIsReachableByTab walks the focus order with Tab until the
// navigation takes the focus, and the chosen page comes along.
func TestNavIsReachableByTab(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	tt, s := navTester(t, true)
	reached := -1
	for i := 0; i < 12 && reached < 0; i++ {
		tt.Key(0, ui.KeyTab)
		tt.Frame()
		for j := range navItems {
			if tt.Focused(navLabel(j)) {
				reached = j
				break
			}
		}
	}
	if reached < 0 {
		t.Fatal("Tab never reached a row of the navigation")
	}
	if reached != 0 {
		t.Errorf("Tab entered the navigation at %q, want the chosen page %q", navLabel(reached), navLabel(0))
	}
	if s.page != navItems[reached].id {
		t.Errorf("the page is %q while %q has the focus", s.page, navLabel(reached))
	}
}

// TestNavArrowKeysMoveTheSelectedPage walks the whole navigation with the
// arrow keys, wraps at both ends, and the page follows the focused row.
func TestNavArrowKeysMoveTheSelectedPage(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	tt, s := navTester(t, true)
	if err := tt.Click(navLabel(0)); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if s.page != navItems[0].id {
		t.Fatalf("after the click the page is %q, want %q", s.page, navItems[0].id)
	}
	for i := 1; i < len(navItems); i++ {
		tt.Key(0, ui.KeyDown)
		tt.Frame()
		if s.page != navItems[i].id {
			t.Fatalf("after %d downs the page is %q, want %q", i, s.page, navItems[i].id)
		}
		if !tt.Focused(navLabel(i)) {
			t.Fatalf("after %d downs %q does not have the focus", i, navLabel(i))
		}
	}
	tt.Key(0, ui.KeyDown)
	tt.Frame()
	if s.page != navItems[0].id {
		t.Errorf("down from the last row went to %q, want %q", s.page, navItems[0].id)
	}
	tt.Key(0, ui.KeyUp)
	tt.Frame()
	if s.page != navItems[len(navItems)-1].id {
		t.Errorf("up from the first row went to %q, want %q", s.page, navItems[len(navItems)-1].id)
	}
}

// TestNavHomeAndEndJumpToTheEnds takes the focus to the middle and jumps
// home and to the end.
func TestNavHomeAndEndJumpToTheEnds(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	tt, s := navTester(t, true)
	middle := len(navItems) / 2
	if err := tt.Click(navLabel(middle)); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	tt.Key(0, ui.KeyHome)
	tt.Frame()
	if s.page != navItems[0].id || !tt.Focused(navLabel(0)) {
		t.Errorf("Home went to %q (focus on page 0: %v), want %q", s.page, tt.Focused(navLabel(0)), navItems[0].id)
	}
	tt.Key(0, ui.KeyEnd)
	tt.Frame()
	last := len(navItems) - 1
	if s.page != navItems[last].id || !tt.Focused(navLabel(last)) {
		t.Errorf("End went to %q (focus on the last page: %v), want %q", s.page, tt.Focused(navLabel(last)), navItems[last].id)
	}
}

// TestNavRowKeepsItsPageAccent asserts each chosen row still paints its own
// page's color, and that two pages do not share it.
func TestNavRowKeepsItsPageAccent(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	tt, s := navTester(t, true)
	const rank, proj = 2, 3 // ranking is Rana green, projects Soyo honey
	if navItems[rank].col == navItems[proj].col {
		t.Fatalf("%q and %q share one accent", navItems[rank].id, navItems[proj].id)
	}
	if err := tt.Click(navLabel(rank)); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if s.page != navItems[rank].id {
		t.Fatalf("the click chose %q, want %q", s.page, navItems[rank].id)
	}
	gotRank := navPickPixel(t, tt, rank)
	if !nearColor(gotRank, navItems[rank].col, 24) {
		t.Errorf("the chosen %q row shows %v, want its own accent %v", navItems[rank].id, gotRank, navItems[rank].col)
	}
	if err := tt.Click(navLabel(proj)); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	gotProj := navPickPixel(t, tt, proj)
	if !nearColor(gotProj, navItems[proj].col, 24) {
		t.Errorf("the chosen %q row shows %v, want its own accent %v", navItems[proj].id, gotProj, navItems[proj].col)
	}
	if gotRank == gotProj {
		t.Errorf("both pages show the same color %v", gotRank)
	}
}

// TestSessionsListArrowKeysMoveTheSelection: the sessions list keeps the
// keyboard: the arrows move the focus among its rows and the selection
// follows, as a click does.
func TestSessionsListArrowKeysMoveTheSelection(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	tt, s := navTester(t, true)
	s.page = "sessions"
	tt.Frame()
	rows := filterRows(s.sessions.Rows, "")
	if len(rows) < 2 {
		t.Fatalf("the demo has %d sessions, want at least 2", len(rows))
	}
	title := func(i int) string {
		if rows[i].Title == "" {
			return tr("untitled")
		}
		return rows[i].Title
	}
	if err := tt.Click(title(0)); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if s.sel != refOf(rows[0]) {
		t.Fatalf("the click chose %+v, want %+v", s.sel, refOf(rows[0]))
	}
	tt.Key(0, ui.KeyDown)
	tt.Frame()
	if !tt.Focused(title(1)) {
		t.Errorf("after a down the focus is not on %q", title(1))
	}
	if s.sel != refOf(rows[1]) {
		t.Errorf("after a down the selection is %+v, want %+v", s.sel, refOf(rows[1]))
	}
	tt.Key(0, ui.KeyUp)
	tt.Frame()
	if s.sel != refOf(rows[0]) {
		t.Errorf("after an up the selection is %+v, want %+v", s.sel, refOf(rows[0]))
	}
}

// TestNavGliderSettlesAtOnceWithReducedMotion: the pill that glides between
// the sidebar rows is a transition now, so reduced motion puts it on the new
// row at once.
func TestNavGliderSettlesAtOnceWithReducedMotion(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	tt, s := navTester(t, true)
	raised, plain := navPillPixel(t, tt, 0), navPillPixel(t, tt, 1)
	if nearPixel(raised, plain, 6) {
		t.Fatalf("the pill is not visible at the sample point: %v on both rows", raised)
	}
	if err := tt.Click(navLabel(1)); err != nil {
		t.Fatal(err)
	}
	if s.page != navItems[1].id {
		t.Fatalf("the click chose %q, want %q", s.page, navItems[1].id)
	}
	if got := navPillPixel(t, tt, 1); !nearPixel(got, raised, 3) {
		t.Errorf("reduced motion: the pill is not on row 1: %v, want %v", got, raised)
	}
	if got := navPillPixel(t, tt, 0); !nearPixel(got, plain, 3) {
		t.Errorf("reduced motion: the pill is still on row 0: %v, want %v", got, plain)
	}
}

// seatLabels names the boxes of glideView.
var seatLabels = [...]string{"seat0", "seat1", "seat2"}

// glideView is the smallest control that uses the glider: a padded row of
// three seats with the pill behind the chosen one, built as the segmented
// control builds it (the navigation's own row has no padding).
func glideView(sel *int) func(*ui.Context) {
	return func(c *ui.Context) {
		pal := applyTheme(c)
		ui.Column(c).Padding(14).Children(func() {
			box := ui.Row(c).Key("track").Label("track").Padding(3).Gap(2)
			box.Children(func() {
				items := make([]ui.Element, 0, len(seatLabels))
				for _, name := range seatLabels {
					items = append(items, ui.Box(c).Key(name).Label(name).Size(60, 24))
				}
				glider(c, box, items, *sel, 3, 3, 6, pal.raised, ui.Transparent, ui.RGBA(0, 0, 0, 0.12))
			})
		})
	}
}

// seatPixel samples the left padding of a seat: inside the pill when the
// pill is over it.
func seatPixel(t *testing.T, tt *ui.Tester, i int) color.RGBA {
	t.Helper()
	r, ok := tt.Find(seatLabels[i])
	if !ok {
		t.Fatalf("seat %d is not laid out", i)
	}
	return tt.Image().RGBAAt(int(r.X)+4, int(r.Y+r.H/2))
}

// trackPixel samples the row's own padding, left of where the pill's place
// begins: the pill starts at the first seat only when the parent's padding
// is accounted for.
func trackPixel(t *testing.T, tt *ui.Tester) color.RGBA {
	t.Helper()
	r, ok := tt.Find("track")
	if !ok {
		t.Fatal("the track is not laid out")
	}
	return tt.Image().RGBAAt(int(r.X)+1, int(r.Y+r.H/2))
}

// glideTester settles glideView with the pill on seat 0 and then chooses
// seat to, so that a test sees the frame right after the choice.
func glideTester(t *testing.T, reduceMotion bool, to int) *ui.Tester {
	t.Helper()
	sel := 0
	tt := ui.NewTester(glideView(&sel), 240, 80)
	tt.SetScale(1)
	tt.SetDark(true)
	tt.SetPreferences(ui.Preferences{ReduceMotion: reduceMotion})
	for i := 0; i < 4; i++ {
		tt.Frame()
	}
	sel = to
	tt.Frame()
	return tt
}

// TestSegmentedThumbStaysUnderItsChoice: the glider is what draws the
// segmented control's raised thumb, so the padding it is handed has to keep
// that thumb on the chosen segment's own box.
func TestSegmentedThumbStaysUnderItsChoice(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	sel := 0
	tt := ui.NewTester(func(c *ui.Context) {
		pal := applyTheme(c)
		ui.Column(c).Padding(14).Children(func() {
			segmented(c, pal, &sel, "one", "two")
		})
	}, 240, 80)
	tt.SetScale(1)
	tt.SetDark(true)
	tt.SetPreferences(ui.Preferences{ReduceMotion: true})
	for i := 0; i < 4; i++ {
		tt.Frame()
	}
	thumb := func(label string) color.RGBA {
		t.Helper()
		r, ok := tt.Find(label)
		if !ok {
			t.Fatalf("segment %q is not laid out", label)
		}
		return tt.Image().RGBAAt(int(r.X)+4, int(r.Y+r.H/2))
	}
	first, second := thumb("one"), thumb("two")
	if nearPixel(first, second, 6) {
		t.Fatalf("the thumb is not visible at the sample point: %v on both segments", first)
	}
	sel = 1
	tt.Frame()
	if got := thumb("two"); !nearPixel(got, first, 3) {
		t.Errorf("the thumb is not under the chosen segment: %v, want %v", got, first)
	}
	if got := thumb("one"); !nearPixel(got, second, 3) {
		t.Errorf("the thumb stayed on the first segment: %v, want %v", got, second)
	}
}

// TestGliderPillGlidesAndReducedMotionSettlesIt checks the glider's own
// contract on a control small enough to watch frame by frame: with motion
// the pill is still on its way right after the choice, and reduced motion
// settles it on the new seat at once.
func TestGliderPillGlidesAndReducedMotionSettlesIt(t *testing.T) {
	base := glideTester(t, false, 0)
	pill, plain := seatPixel(t, base, 0), seatPixel(t, base, 1)
	if nearPixel(pill, plain, 6) {
		t.Fatalf("the pill is not visible at the sample point: %v on both seats", pill)
	}
	if got := trackPixel(t, base); nearPixel(got, pill, 3) {
		t.Errorf("the pill starts in the track's own padding: %v, want %v", got, plain)
	}
	moving := glideTester(t, false, 2)
	if got := seatPixel(t, moving, 0); !nearPixel(got, pill, 3) {
		t.Errorf("the pill left seat 0 at once: %v, want %v", got, pill)
	}
	if got := seatPixel(t, moving, 2); nearPixel(got, pill, 3) {
		t.Errorf("the pill arrived on seat 2 at once: %v, want %v", got, plain)
	}
	still := glideTester(t, true, 2)
	if got := seatPixel(t, still, 2); !nearPixel(got, pill, 3) {
		t.Errorf("reduced motion: the pill is not on seat 2: %v, want %v", got, pill)
	}
	if got := seatPixel(t, still, 0); !nearPixel(got, plain, 3) {
		t.Errorf("reduced motion: the pill is still on seat 0: %v, want %v", got, plain)
	}
	if got := trackPixel(t, still); !nearPixel(got, plain, 3) {
		t.Errorf("reduced motion: the pill is still in the track's padding: %v, want %v", got, plain)
	}
}
