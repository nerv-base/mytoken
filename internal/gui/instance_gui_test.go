package gui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/provider"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/store"
)

// The instance problem in one fixture: one session id, "s-dup", in two
// instances, each instance behind its own endpoint.
const (
	instAlpha  = "alpha"
	instBeta   = "beta"
	dupSession = "s-dup"
	kidSession = "s-kid"
	endpointA  = "https://alpha.example.test/v1"
	endpointB  = "https://beta.example.test/v1"
	provShared = "shared-provider"
)

// commitInstance records one usage event in a named instance, with the title
// and project of the session it belongs to.
func (f *providerFixture) commitInstance(t *testing.T, instance string, e model.UsageEvent, r store.Resolution, title, project string) {
	t.Helper()
	if e.Timestamp.IsZero() {
		e.Timestamp = providerTestNow()
	}
	b := harness.Batch{Events: []model.UsageEvent{e}}
	if title != "" {
		b.Sessions = []model.SessionMeta{{
			Harness: e.Harness, SessionID: e.SessionID, ParentID: e.ParentID,
			Title: title, Project: project, StartedAt: e.Timestamp, UpdatedAt: e.Timestamp,
		}}
	}
	err := f.st.CommitMany(t.Context(), []store.Write{{
		Harness: e.Harness, InstanceID: instance, Path: "fixture",
		Batch: b, Resolutions: []store.Resolution{r},
	}})
	if err != nil {
		t.Fatal(err)
	}
}

// instanceFixture is a real store and query where "s-dup" exists in two
// instances: alpha has two requests plus a subagent, beta has one request.
func instanceFixture(t *testing.T) *providerFixture {
	t.Helper()
	f := newProviderFixture(t)
	event := func(session, dedup, parent, base string, input int64) model.UsageEvent {
		return model.UsageEvent{
			Harness: model.Codex, DedupKey: dedup, SessionID: session, ParentID: parent,
			Model: "gpt-5.1", Provider: provShared, BaseURL: base,
			Tokens: model.Tokens{Input: input},
		}
	}
	res := func(cost float64) store.Resolution {
		return store.Resolution{Provider: provShared, Attrib: model.AttribLog, Cost: cost, Priced: true}
	}
	f.commitInstance(t, instAlpha, event(dupSession, "a-1", "", endpointA, 1000), res(1), "Alpha work", "/work/alpha-app")
	f.commitInstance(t, instAlpha, event(dupSession, "a-2", "", endpointA, 2000), res(2), "", "")
	f.commitInstance(t, instAlpha, event(kidSession, "a-k", dupSession, endpointA, 300), res(0.3), "Alpha child", "/work/alpha-app")
	f.commitInstance(t, instBeta, event(dupSession, "b-1", "", endpointB, 7000), res(7), "Beta work", "/work/beta-app")
	return f
}

// legacyBanService fails the instance-less session lookups. A caller that
// resolves a selected, known instance through them cannot pass here.
type legacyBanService struct {
	query.Service
	mu     sync.Mutex
	legacy int
}

func (l *legacyBanService) banned() {
	l.mu.Lock()
	l.legacy++
	l.mu.Unlock()
}

// LegacyCalls counts the ambiguous lookups the GUI made.
func (l *legacyBanService) LegacyCalls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.legacy
}

func (l *legacyBanService) Session(context.Context, model.Harness, string) (query.SessionRow, []query.SessionRow, []query.AttributedEvent, error) {
	l.banned()
	return query.SessionRow{}, nil, nil, errors.New("legacy Session(h, id) cannot tell two instances apart")
}

func (l *legacyBanService) SessionCache(context.Context, model.Harness, string) ([]query.CacheStep, error) {
	l.banned()
	return nil, errors.New("legacy SessionCache(h, id) cannot tell two instances apart")
}

func (l *legacyBanService) SessionIn(ctx context.Context, h model.Harness, instance, id string) (query.SessionRow, []query.SessionRow, []query.AttributedEvent, error) {
	return query.SessionIn(ctx, l.Service, h, instance, id)
}

func (l *legacyBanService) SessionCacheIn(ctx context.Context, h model.Harness, instance, session string) ([]query.CacheStep, error) {
	return query.SessionCacheIn(ctx, l.Service, h, instance, session)
}

// recordingService records the filters the sessions list is loaded with.
type recordingService struct {
	query.Service
	mu      sync.Mutex
	filters []query.Filter
}

func (r *recordingService) Sessions(ctx context.Context, f query.Filter, sortKey string, limit, offset int) ([]query.SessionRow, int, error) {
	r.mu.Lock()
	r.filters = append(r.filters, f)
	r.mu.Unlock()
	return r.Service.Sessions(ctx, f, sortKey, limit, offset)
}

func (r *recordingService) SessionIn(ctx context.Context, h model.Harness, instance, id string) (query.SessionRow, []query.SessionRow, []query.AttributedEvent, error) {
	return query.SessionIn(ctx, r.Service, h, instance, id)
}

func (r *recordingService) SessionCacheIn(ctx context.Context, h model.Harness, instance, session string) ([]query.CacheStep, error) {
	return query.SessionCacheIn(ctx, r.Service, h, instance, session)
}

// providerIDFilters are the loads that asked for a stable provider id.
func (r *recordingService) providerIDFilters() []query.Filter {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []query.Filter
	for _, f := range r.filters {
		if len(f.ProviderIDs) > 0 {
			out = append(out, f)
		}
	}
	return out
}

// TestSessionListKeepsEqualIDsInDifferentInstancesApart is the regression this
// work exists for: one session id in two instances is two rows, two
// selections and two details, never one merged row.
func TestSessionListKeepsEqualIDsInDifferentInstancesApart(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	f := instanceFixture(t)

	rows, _, err := f.q.Sessions(t.Context(), query.Filter{}, query.SortRecent, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	var twins []query.SessionRow
	for _, r := range rows {
		if r.SessionID == dupSession {
			twins = append(twins, r)
		}
	}
	if len(twins) != 2 {
		t.Fatalf("the fixture must list %s once per instance, got %d rows: %+v", dupSession, len(twins), rows)
	}
	keys := map[string]string{}
	for _, r := range twins {
		k := refOf(r).key()
		if other, ok := keys[k]; ok {
			t.Fatalf("two instances share the list key %q (%s and %s)", k, other, r.InstanceID)
		}
		keys[k] = r.InstanceID
		if refOf(r).instanceLabel() != r.InstanceID {
			t.Errorf("row %+v does not carry its instance", r)
		}
	}

	s := providerGUI(t, f.q, Hooks{})
	s.SetPage("sessions")
	tt := ui.NewTester(s.MainView, 1400, 1000)
	// Rendering both rows at all is part of the assertion: the tester panics
	// on duplicate list keys, and the old key was harness/id.
	tt.Frame()
	if err := tt.Click("Beta work"); err != nil {
		t.Fatalf("the beta row is not clickable: %v", err)
	}
	if s.sel != (sessionRef{Harness: model.Codex, InstanceID: instBeta, SessionID: dupSession}) {
		t.Fatalf("clicking the beta row selected %+v", s.sel)
	}
	if s.detail.Row.InstanceID != instBeta || s.detail.Row.Title != "Beta work" {
		t.Fatalf("the beta detail is %+v", s.detail.Row)
	}
	if len(s.detail.Events) != 1 || len(s.detail.Children) != 0 {
		t.Fatalf("the beta detail has %d events and %d children, want 1 and 0", len(s.detail.Events), len(s.detail.Children))
	}
	if err := tt.Click("Alpha work"); err != nil {
		t.Fatalf("the alpha row is not clickable: %v", err)
	}
	if s.sel.InstanceID != instAlpha {
		t.Fatalf("clicking the alpha row selected %+v", s.sel)
	}
	if s.detail.Row.InstanceID != instAlpha || len(s.detail.Events) != 2 || len(s.detail.Children) != 1 {
		t.Fatalf("the alpha detail is %+v (%d events, %d children)", s.detail.Row, len(s.detail.Events), len(s.detail.Children))
	}
}

// TestSelectedInstanceNeverUsesTheLegacyLookup pins that a selected row whose
// instance is known is resolved through the instance-aware API even when the
// service still offers the ambiguous one.
func TestSelectedInstanceNeverUsesTheLegacyLookup(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	f := instanceFixture(t)
	ban := &legacyBanService{Service: f.q}
	s := providerGUI(t, ban, Hooks{})
	s.SetPage("sessions")
	tt := ui.NewTester(s.MainView, 1400, 1000)
	tt.Frame()
	if err := tt.Click("Alpha work"); err != nil {
		t.Fatalf("the alpha row is not clickable: %v", err)
	}
	if got := ban.LegacyCalls(); got != 0 {
		t.Fatalf("the GUI resolved a known instance with the ambiguous lookup %d times", got)
	}
	if s.detail.Row.InstanceID != instAlpha || s.sel.InstanceID != instAlpha {
		t.Fatalf("detail = %+v, selection = %+v", s.detail.Row, s.sel)
	}
	if len(s.detail.Events) != 2 || len(s.detail.Cache) != 2 {
		t.Fatalf("the alpha detail has %d events and %d cache steps, want 2 and 2", len(s.detail.Events), len(s.detail.Cache))
	}
}

// TestCachePageResolvesTheRightInstance pins that a cache row carries the
// instance of its bucket key, so clicking it opens the twin it names.
func TestCachePageResolvesTheRightInstance(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	f := instanceFixture(t)
	// The keys the store actually reports must read back to the instance and
	// session they name. (Their miss cost is zero in this fixture, so the
	// listing policy below is exercised with the costs a miss would carry.)
	real, err := f.q.CacheBy(t.Context(), query.Filter{Range: rangeOf(Span7, providerTestNow(), time.Local)}, query.CacheBySession)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range real {
		ref, ok := parseCacheSessionKey(b.Key)
		if !ok {
			t.Errorf("the store's cache key %q does not parse", b.Key)
			continue
		}
		if got := query.CacheSessionKeyIn(ref.InstanceID, string(ref.Harness), ref.SessionID); got != b.Key {
			t.Errorf("cache key %q parsed to %+v, which rebuilds as %q", b.Key, ref, got)
		}
	}

	// Two instances, one session id: two rows, each resolved in its own
	// instance, so each shows its own session's title.
	rows := cacheSessionsFrom(t.Context(), f.q, []query.CacheBucket{
		{Key: query.CacheSessionKeyIn(instAlpha, string(model.Codex), dupSession), CacheTotals: query.CacheTotals{USD: 3}},
		{Key: query.CacheSessionKeyIn(instBeta, string(model.Codex), dupSession), CacheTotals: query.CacheTotals{USD: 7}},
	})
	byInstance := map[string]cacheSession{}
	for _, r := range rows {
		if r.ref.SessionID == dupSession {
			byInstance[r.ref.InstanceID] = r
		}
	}
	if len(byInstance) != 2 {
		t.Fatalf("cache rows for %s = %d, want one per instance: %+v", dupSession, len(byInstance), rows)
	}
	if got := byInstance[instAlpha].row.Title; got != "Alpha work" {
		t.Errorf("the alpha cache row shows %q, want the alpha session", got)
	}
	if got := byInstance[instBeta].row.Title; got != "Beta work" {
		t.Errorf("the beta cache row shows %q, want the beta session", got)
	}

	s := providerGUI(t, f.q, Hooks{})
	tt := ui.NewTester(func(c *ui.Context) {
		pal := applyTheme(c)
		ui.Column(c).Padding(14).Children(func() { s.cacheSessions(c, pal, rows) })
	}, 760, 420)
	tt.Frame()
	if err := tt.Click("Beta work"); err != nil {
		t.Fatalf("the beta cache row is not clickable: %v", err)
	}
	if s.sel != (sessionRef{Harness: model.Codex, InstanceID: instBeta, SessionID: dupSession}) {
		t.Fatalf("clicking the beta cache row selected %+v", s.sel)
	}
	if s.detail.Row.InstanceID != instBeta || s.detail.Row.Title != "Beta work" {
		t.Fatalf("the cache click opened %+v", s.detail.Row)
	}
}

// TestCacheSessionKeysParseBackToTheirInstance pins the key format the cache
// page reads against the one the query builds.
func TestCacheSessionKeysParseBackToTheirInstance(t *testing.T) {
	for _, want := range []sessionRef{
		{Harness: model.Codex, InstanceID: store.DefaultInstanceID, SessionID: "s1"},
		{Harness: model.Codex, InstanceID: instAlpha, SessionID: "s1"},
		{Harness: model.ClaudeCode, InstanceID: instBeta, SessionID: dupSession},
	} {
		key := query.CacheSessionKeyIn(want.InstanceID, string(want.Harness), want.SessionID)
		got, ok := parseCacheSessionKey(key)
		if !ok {
			t.Errorf("parseCacheSessionKey(%q) did not parse", key)
			continue
		}
		if got != want {
			t.Errorf("parseCacheSessionKey(%q) = %+v, want %+v", key, got, want)
		}
	}
	if key := query.CacheSessionKeyIn("", string(model.Codex), "s1"); key != "codex/s1" {
		t.Fatalf("the default instance key changed shape: %q", key)
	}
	if _, ok := parseCacheSessionKey("codex"); ok {
		t.Error("a key without a session parsed")
	}
	if _, ok := parseCacheSessionKey("instance:alpha"); ok {
		t.Error("a key without a session parsed")
	}
}

// TestSessionRowShowsInstanceOnlyWhenAmbiguous pins that the instance mark
// appears where a session id alone is ambiguous, and nowhere else: the id is
// never invented, and a unique session is not dressed up with one.
func TestSessionRowShowsInstanceOnlyWhenAmbiguous(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	twins := providerGUI(t, instanceFixture(t).q, Hooks{})
	twins.SetPage("sessions")
	tt := ui.NewTester(twins.MainView, 1400, 1000)
	tt.Frame()
	for _, inst := range []string{instAlpha, instBeta} {
		if _, ok := tt.Find(inst); !ok {
			t.Errorf("the %s twin does not show its instance", inst)
		}
	}

	only := newProviderFixture(t)
	only.commitInstance(t, instAlpha, model.UsageEvent{
		Harness: model.Codex, DedupKey: "only", SessionID: "s-only",
		Model: "gpt-5.1", Provider: provShared, BaseURL: endpointA,
		Tokens: model.Tokens{Input: 10},
	}, store.Resolution{Provider: provShared, Attrib: model.AttribLog, Cost: 1, Priced: true}, "Only work", "/work/alpha-app")
	s := providerGUI(t, only.q, Hooks{})
	s.SetPage("sessions")
	tt2 := ui.NewTester(s.MainView, 1400, 1000)
	tt2.Frame()
	if !tt2.HasText("Only work") {
		t.Fatal("the only session is not listed")
	}
	if _, ok := tt2.Find(instAlpha); ok {
		t.Error("a session that is not ambiguous shows an instance mark")
	}
}

// TestProviderDrilldownFiltersByStableIDNotAlias pins that the provider
// drilldown narrows the sessions by the stable endpoint id, so two endpoints
// sharing one display name stay apart, and that the pricing identity behind
// the sessions is left alone.
func TestProviderDrilldownFiltersByStableIDNotAlias(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	f := instanceFixture(t)
	for _, base := range []string{endpointA, endpointB} {
		id, ok := provider.EndpointID(base)
		if !ok {
			t.Fatalf("%s has no endpoint id", base)
		}
		if err := f.st.SetProviderAlias(t.Context(), id, "Shared"); err != nil {
			t.Fatal(err)
		}
	}
	rec := &recordingService{Service: f.q}
	s := providerGUI(t, rec, Hooks{})
	groups := s.ov.Providers
	if len(groups) != 2 {
		t.Fatalf("endpoint groups = %d, want 2: %+v", len(groups), groups)
	}
	for _, g := range groups {
		if g.Label != "Shared" {
			t.Fatalf("alias not applied: %q", g.Label)
		}
	}
	idA, _ := provider.EndpointID(endpointA)
	var want query.ProviderGroup
	for _, g := range groups {
		if g.Key == idA {
			want = g
		}
	}
	if want.Key == "" {
		t.Fatalf("the alpha endpoint %s is not among the groups: %+v", idA, groups)
	}

	// The row selection itself (by stable key, with a duplicate alias) is
	// pinned by TestProviderTabKeepsDuplicateAliasesApart; what this test adds
	// is the drilldown action and where its filter comes from.
	s.selectProvider(want.Key)
	s.SetPage("ranking")
	s.rankTab = 1
	// Open the session the drilldown is about to filter out: it must not stay
	// in the detail pane, where it would read as if the filter had failed.
	s.SelectRef(sessionRef{Harness: model.Codex, InstanceID: instBeta, SessionID: dupSession})
	tt := ui.NewTester(s.MainView, 1400, 1000)
	tt.Frame()
	if err := tt.Click(tr("providerSessions")); err != nil {
		t.Fatalf("the provider sessions action is missing: %v", err)
	}
	if s.page != "sessions" {
		t.Fatalf("the action left the page at %q", s.page)
	}
	if s.provFilter.Key != want.Key {
		t.Fatalf("drilldown filter = %q, want the stable id %q", s.provFilter.Key, want.Key)
	}
	if s.sel.SessionID != "" {
		t.Fatalf("the drilldown left the filtered-out session %+v open", s.sel)
	}
	fs := rec.providerIDFilters()
	if len(fs) != 1 || len(fs[0].ProviderIDs) != 1 || fs[0].ProviderIDs[0] != want.Key {
		t.Fatalf("session filters = %+v, want exactly the stable id %q", fs, want.Key)
	}
	if len(fs[0].Providers) != 0 {
		t.Fatalf("the drilldown filtered by a display name: %+v", fs[0].Providers)
	}
	if len(s.sessions.Rows) != 1 || s.sessions.Rows[0].InstanceID != instAlpha {
		t.Fatalf("drilled sessions = %+v, want the alpha session only", s.sessions.Rows)
	}
	if b := s.sessions.Rows[0].Breakdown; len(b) != 1 || b[0].Provider != provShared {
		t.Fatalf("the drilldown rewrote the resolved provider: %+v", b)
	}
	drilled := ui.NewTester(s.MainView, 1400, 1000)
	drilled.Frame()
	if !drilled.HasText("Alpha work") {
		t.Error("the drilled list does not show the alpha session")
	}
	if drilled.HasText("Beta work") {
		t.Error("the drilled list still shows the other endpoint's session")
	}
	// The filter is visible and can be dropped.
	if err := drilled.Click(tr("providerSessionsClear")); err != nil {
		t.Fatalf("the drilldown cannot be cleared: %v", err)
	}
	if s.provFilter.Key != "" {
		t.Fatalf("clearing left the filter at %q", s.provFilter.Key)
	}
}

// TestDemoServiceAnswersInstanceAwareLookups pins that the demo keeps working
// through the same additive API, so demo mode does not fork the GUI.
func TestDemoServiceAnswersInstanceAwareLookups(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	now := providerTestNow()
	q := NewDemoService(now)
	page, _, err := q.Sessions(t.Context(), query.Filter{}, query.SortRecent, 2000, 0)
	if err != nil || len(page) == 0 {
		t.Fatalf("demo sessions: %v, %d rows", err, len(page))
	}
	want := page[0]
	row, _, events, err := query.SessionIn(t.Context(), q, want.Harness, store.DefaultInstanceID, want.SessionID)
	if err != nil {
		t.Fatalf("the demo service cannot answer an instance-aware lookup: %v", err)
	}
	if row.SessionID != want.SessionID || row.InstanceID != store.DefaultInstanceID {
		t.Fatalf("demo SessionIn returned %+v", row)
	}
	if len(events) == 0 {
		t.Error("demo SessionIn returned no events")
	}
	if _, _, _, err := query.SessionIn(t.Context(), q, want.Harness, "no-such-instance", want.SessionID); err == nil {
		t.Error("the demo service answered for an instance it does not have")
	}
	if steps, err := query.SessionCacheIn(t.Context(), q, want.Harness, store.DefaultInstanceID, want.SessionID); err != nil || len(steps) == 0 {
		t.Errorf("demo SessionCacheIn: %v, %d steps", err, len(steps))
	}

	// A demo row opens through the instance-aware path, and the legacy
	// instance-less Select still means the default instance.
	s := providerGUI(t, q, Hooks{})
	s.SetPage("sessions")
	s.SelectRow(s.sessions.Rows[0])
	if s.sel.InstanceID != store.DefaultInstanceID || s.detail.Row.SessionID != s.sessions.Rows[0].SessionID {
		t.Fatalf("opening a demo row selected %+v, detail %+v", s.sel, s.detail.Row)
	}
	s2 := providerGUI(t, q, Hooks{})
	s2.Select(want.Harness, want.SessionID)
	if s2.sel.InstanceID != store.DefaultInstanceID || s2.detail.Row.SessionID != want.SessionID {
		t.Fatalf("Select without a row gave %+v, detail %+v", s2.sel, s2.detail.Row)
	}

	// The demo provider drilldown must filter by the stable id too.
	groups, err := q.ProviderGroups(t.Context(), query.Filter{})
	if err != nil || len(groups) == 0 {
		t.Fatalf("demo provider groups: %v, %d", err, len(groups))
	}
	got, _, err := q.Sessions(t.Context(), query.Filter{ProviderIDs: []string{groups[0].Key}}, query.SortRecent, 2000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || len(got) >= len(page) {
		t.Errorf("the demo provider filter returned %d of %d sessions", len(got), len(page))
	}
	if none, _, _ := q.Sessions(t.Context(), query.Filter{ProviderIDs: []string{"demo:provider:nobody"}}, query.SortRecent, 10, 0); len(none) != 0 {
		t.Errorf("the demo returned %d sessions for an unknown provider id", len(none))
	}
}

// TestInstanceSnapshots renders the instance-ambiguous states for review:
// set MYTOKEN_INSTANCE_SNAPSHOTS=dir to write them. The directory is the
// caller's; the upgrade baseline under build/verification/v040/before|after is
// never touched.
func TestInstanceSnapshots(t *testing.T) {
	dir := os.Getenv("MYTOKEN_INSTANCE_SNAPSHOTS")
	if dir == "" {
		t.Skip("set MYTOKEN_INSTANCE_SNAPSHOTS=dir to render")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	defer SetLang(Lang())
	f := instanceFixture(t)
	for _, base := range []string{endpointA, endpointB} {
		id, ok := provider.EndpointID(base)
		if !ok {
			t.Fatalf("%s has no endpoint id", base)
		}
		if err := f.st.SetProviderAlias(t.Context(), id, "Shared"); err != nil {
			t.Fatal(err)
		}
	}
	keys := make([]string, 0, 2)
	for _, g := range mustProviders(t, f) {
		keys = append(keys, g.Key)
	}
	for _, lang := range []string{"zh", "en"} {
		SetLang(lang)
		s := providerGUI(t, f.q, Hooks{SetProviderAlias: f.st.SetProviderAlias})
		// One session id, two instances: the list marks which is which.
		s.SetPage("sessions")
		s.SelectRef(sessionRef{Harness: model.Codex, InstanceID: instAlpha, SessionID: dupSession})
		shot(t, filepath.Join(dir, lang+"-sessions-twins.png"), s.MainView, 1280, 860, false)

		// The provider detail, with the drilldown action.
		s.SetPage("ranking")
		s.rankTab = 1
		s.selectProvider(keys[0])
		shot(t, filepath.Join(dir, lang+"-provider-detail.png"), s.MainView, 1280, 860, false)

		// The drilldown: the sessions behind that one endpoint.
		s.showProviderSessions(query.ProviderGroup{Bucket: query.Bucket{Key: keys[0], Label: "Shared"}})
		shot(t, filepath.Join(dir, lang+"-provider-drilldown.png"), s.MainView, 1280, 860, false)
		shot(t, filepath.Join(dir, lang+"-provider-drilldown-dark.png"), s.MainView, 1280, 860, true)

		// The cache page's session list, one id in two instances.
		rows := cacheSessionsFrom(t.Context(), f.q, []query.CacheBucket{
			{Key: query.CacheSessionKeyIn(instAlpha, string(model.Codex), dupSession), CacheTotals: query.CacheTotals{USD: 3, Miss: 1200, Expected: 3000}},
			{Key: query.CacheSessionKeyIn(instBeta, string(model.Codex), dupSession), CacheTotals: query.CacheTotals{USD: 7, Miss: 7000, Expected: 7000}},
		})
		shot(t, filepath.Join(dir, lang+"-cache-twins.png"), func(c *ui.Context) {
			pal := applyTheme(c)
			ui.Column(c).Padding(14).Gap(8).Children(func() {
				kicker(c, pal, tr("cacheSessions"))
				s.cacheSessions(c, pal, rows)
			})
		}, 760, 320, false)
	}
}

func mustProviders(t *testing.T, f *providerFixture) []query.ProviderGroup {
	t.Helper()
	gs, err := f.q.ProviderGroups(t.Context(), query.Filter{Range: rangeOf(Span7, providerTestNow(), time.Local)})
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 2 {
		t.Fatalf("endpoint groups = %d, want 2", len(gs))
	}
	return gs
}

// TestSegmentedGroupsAreNamedAtEveryCallSite pins the accessible group name of
// the segmented controls the GUI actually builds.
func TestSegmentedGroupsAreNamedAtEveryCallSite(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	s := providerGUI(t, NewDemoService(providerTestNow()), Hooks{})
	overview := ui.NewTester(s.MainView, 1400, 1000)
	overview.Frame()
	if _, ok := overview.Find(tr("span")); !ok {
		t.Error("the span switch has no accessible group name")
	}
	s.SetPage("sessions")
	tt := ui.NewTester(s.MainView, 1400, 1000)
	tt.Frame()
	if _, ok := tt.Find(tr("sortBy")); !ok {
		t.Error("the session sort control has no accessible group name")
	}
	s.SetPage("ranking")
	rt := ui.NewTester(s.MainView, 1400, 1000)
	rt.Frame()
	if _, ok := rt.Find(tr("rankingBy")); !ok {
		t.Error("the ranking control has no accessible group name")
	}
}

// TestSegmentedGroupNameDoesNotChangePainting pins that naming the group is
// accessibility only: the pixels and the behavior are the ones the unnamed
// helper always had.
func TestSegmentedGroupNameDoesNotChangePainting(t *testing.T) {
	defer SetLang(Lang())
	SetLang("en")
	labels := []string{tr("recent"), tr("mostTokens"), tr("mostCost")}
	plainSel, namedSel := 1, 1
	plain := func(c *ui.Context) {
		pal := applyTheme(c)
		ui.Column(c).Padding(14).Children(func() { segmented(c, pal, &plainSel, labels...) })
	}
	named := func(c *ui.Context) {
		pal := applyTheme(c)
		ui.Column(c).Padding(14).Children(func() { segmentedGroup(c, pal, &namedSel, tr("sortBy"), labels...) })
	}
	a, b := ui.NewTester(plain, 420, 64), ui.NewTester(named, 420, 64)
	a.Frame()
	b.Frame()
	if !bytes.Equal(a.Image().Pix, b.Image().Pix) {
		t.Error("naming the segmented group changed how it paints")
	}
	if _, ok := b.Find(tr("sortBy")); !ok {
		t.Error("the named group is not exposed to assistive technology")
	}
	if _, ok := a.Find(tr("sortBy")); ok {
		t.Error("the unnamed helper grew a name")
	}
	if err := b.Click(labels[2]); err != nil {
		t.Fatalf("the named control is not clickable: %v", err)
	}
	if namedSel != 2 {
		t.Errorf("the named control reported selection %d, want 2", namedSel)
	}
	if err := a.Click(labels[2]); err != nil {
		t.Fatalf("the plain control is not clickable: %v", err)
	}
	if plainSel != 2 {
		t.Errorf("the plain control reported selection %d, want 2", plainSel)
	}
}
