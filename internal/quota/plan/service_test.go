package plan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeProvider struct {
	id       string
	detected bool

	mu    sync.Mutex
	fetch func(ctx context.Context) (Snapshot, error)
	calls int
	det   int
}

func (p *fakeProvider) ID() string          { return p.id }
func (p *fakeProvider) DisplayName() string { return p.id }

func (p *fakeProvider) Detect(context.Context) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.det++
	return p.detected
}

func (p *fakeProvider) Fetch(ctx context.Context) (Snapshot, error) {
	p.mu.Lock()
	p.calls++
	fn := p.fetch
	p.mu.Unlock()
	if fn == nil {
		return Snapshot{Provider: p.id, Windows: []Window{{Name: "week", UsedPercent: 10}}}, nil
	}
	return fn(ctx)
}

func (p *fakeProvider) stats() (calls, det int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.det
}

type fakeStore struct {
	mu   sync.Mutex
	vals map[string]string
	err  error
}

func newFakeStore() *fakeStore { return &fakeStore{vals: map[string]string{}} }

func (s *fakeStore) Setting(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.vals[key]; ok {
		return v, nil
	}
	return "", fmt.Errorf("no setting %q", key)
}

func (s *fakeStore) SetSetting(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.vals[key] = value
	return nil
}

func (s *fakeStore) get(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vals[key]
	return v, ok
}

func TestRegistryValidation(t *testing.T) {
	if _, err := NewRegistry(&fakeProvider{id: "Upper"}); err == nil {
		t.Fatal("uppercase id accepted")
	}
	if _, err := NewRegistry(&fakeProvider{id: ""}); err == nil {
		t.Fatal("empty id accepted")
	}
	if _, err := NewRegistry(&fakeProvider{id: "a"}, &fakeProvider{id: "a"}); err == nil {
		t.Fatal("duplicate id accepted")
	}
	if _, err := NewRegistry(nil); err == nil {
		t.Fatal("nil provider accepted")
	}
	a := &fakeProvider{id: "a", detected: true}
	b := &fakeProvider{id: "b"}
	r, err := NewRegistry(a, b)
	if err != nil {
		t.Fatal(err)
	}
	got := r.Detected(context.Background())
	if len(got) != 1 || got[0].ID() != "a" {
		t.Fatalf("detected = %v", len(got))
	}
	if p, ok := r.Lookup("b"); !ok || p.ID() != "b" {
		t.Fatal("lookup failed")
	}
}

func TestSnapshotValidate(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		in   Snapshot
		ok   bool
	}{
		{"empty", Snapshot{}, false},
		{"no windows", Snapshot{Provider: "a"}, false},
		{"blank name dropped", Snapshot{Provider: "a", Windows: []Window{{Name: " "}}}, false},
		{"ok", Snapshot{Provider: " a ", Windows: []Window{{Name: "week"}}}, true},
	}
	for _, tc := range cases {
		s := tc.in
		err := s.validate(now)
		if (err == nil) != tc.ok {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
		if err == nil && (s.Provider != "a" || s.Source != SourceAPI || s.FetchedAt != now) {
			t.Fatalf("%s: normalized = %+v", tc.name, s)
		}
	}
}

func TestDecodeSnapshotRoundTripsFetchedAt(t *testing.T) {
	fetched := time.Date(2026, 10, 19, 16, 44, 4, 123456789, time.UTC)
	expires := time.Date(2026, 10, 31, 23, 59, 59, 0, time.FixedZone("CST", 8*60*60))
	in := Snapshot{
		Provider:  "trae-cn",
		Plan:      "credits",
		Source:    SourceAPI,
		FetchedAt: fetched,
		Windows: []Window{{
			Name:      "Credits",
			Used:      20.29,
			Limit:     4700,
			Remaining: 4679.71,
			Unit:      "credits",
			Balance:   true,
			ExpiresAt: expires,
		}},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"fetched_at"`) {
		t.Fatalf("persisted encoding has no fetched_at: %s", raw)
	}
	out, err := DecodeSnapshot(raw)
	if err != nil {
		t.Fatalf("DecodeSnapshot: %v", err)
	}
	if !out.FetchedAt.Equal(fetched) {
		t.Fatalf("FetchedAt = %v, want %v", out.FetchedAt, fetched)
	}
	if len(out.Windows) != 1 {
		t.Fatalf("windows = %+v", out.Windows)
	}
	w := out.Windows[0]
	if w.Remaining != 4679.71 || w.Limit != 4700 || w.Used != 20.29 || w.Unit != "credits" ||
		!w.Balance || !w.ExpiresAt.Equal(expires) {
		t.Fatalf("window changed in round trip: %+v", w)
	}

	// A row written before fetched_at existed still decodes; the missing time
	// stays zero instead of being invented.
	old := []byte(`{"provider":"codex","source":"api","windows":[{"name":"week","used_percent":9}]}`)
	got, err := DecodeSnapshot(old)
	if err != nil {
		t.Fatalf("old row: %v", err)
	}
	if !got.FetchedAt.IsZero() {
		t.Fatalf("old row FetchedAt = %v, want zero", got.FetchedAt)
	}

	// The plan service rejects a cached snapshot that did not come from a server.
	bad := []byte(`{"provider":"codex","source":"local-log","windows":[{"name":"week","used_percent":9}]}`)
	if _, err := DecodeSnapshot(bad); err == nil {
		t.Fatal("local-log snapshot accepted")
	}
}

func TestDoJSONStatusMapping(t *testing.T) {
	statuses := []struct {
		code int
		head map[string]string
		want Reason
	}{
		{401, nil, ReasonUnauthorized},
		{403, nil, ReasonForbidden},
		{429, map[string]string{"Retry-After": "42"}, ReasonRateLimited},
		{500, nil, ReasonHTTPStatus},
		{302, map[string]string{"Location": "/elsewhere"}, ReasonRedirectRefused},
	}
	for _, tc := range statuses {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for k, v := range tc.head {
				w.Header().Set(k, v)
			}
			w.WriteHeader(tc.code)
		}))
		_, err := DoJSON(context.Background(), DefaultTransport(), http.MethodGet, srv.URL, nil, nil, 1024)
		srv.Close()
		if !IsReason(err, tc.want) {
			t.Fatalf("status %d: err = %v, want %s", tc.code, err, tc.want)
		}
		if tc.want == ReasonRateLimited {
			var e *Error
			if errors.As(err, &e) && e.RetryAfter != 42*time.Second {
				t.Fatalf("retry after = %v", e.RetryAfter)
			}
		}
	}
}

func TestDoJSONBoundsAndTargets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer t" {
			t.Errorf("authorization = %q", got)
		}
		fmt.Fprint(w, "0123456789")
	}))
	defer srv.Close()

	if _, err := DoJSON(context.Background(), DefaultTransport(), http.MethodGet, srv.URL, http.Header{"Authorization": {"Bearer t"}}, nil, 4); !IsReason(err, ReasonMalformed) {
		t.Fatalf("oversize err = %v", err)
	}
	if _, err := DoJSON(context.Background(), DefaultTransport(), http.MethodGet, "http://example.com/x", nil, nil, 1024); !IsReason(err, ReasonUnsupported) {
		t.Fatalf("plain http err = %v", err)
	}
	bad := transportFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("boom") })
	if _, err := DoJSON(context.Background(), bad, http.MethodGet, srv.URL, nil, nil, 1024); !IsReason(err, ReasonNetwork) {
		t.Fatalf("network err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DoJSON(ctx, DefaultTransport(), http.MethodGet, srv.URL, nil, nil, 1024); !IsReason(err, ReasonCanceled) {
		t.Fatalf("canceled err = %v", err)
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestServiceFetchPersistsAndNotifies(t *testing.T) {
	store := newFakeStore()
	p := &fakeProvider{id: "alpha", detected: true}
	var changes int
	svc, err := NewService(Options{
		Providers: []Provider{p},
		Store:     store,
		Now:       func() time.Time { return time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC) },
		OnChange:  func([]Snapshot) { changes++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	svc.Refresh(context.Background())

	got := svc.Latest()
	if len(got) != 1 || got[0].Provider != "alpha" || got[0].Source != SourceAPI {
		t.Fatalf("latest = %+v", got)
	}
	raw, ok := store.get(SnapshotKey("alpha"))
	if !ok {
		t.Fatal("snapshot not persisted")
	}
	var decoded Snapshot
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil || decoded.Provider != "alpha" {
		t.Fatalf("persisted = %q, %v", raw, err)
	}
	if changes != 1 {
		t.Fatalf("changes = %d", changes)
	}

	// A new Service restores the snapshot without a fetch.
	p2 := &fakeProvider{id: "alpha", detected: true}
	svc2, err := NewService(Options{Providers: []Provider{p2}, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	restored, ok := svc2.Snapshot("alpha")
	if !ok || restored.Provider != "alpha" {
		t.Fatalf("restored = %+v, %v", restored, ok)
	}
	wantFetched := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	if !restored.FetchedAt.Equal(wantFetched) {
		t.Fatalf("restored FetchedAt = %v, want %v", restored.FetchedAt, wantFetched)
	}
}

func TestServiceKeepsLastGoodSnapshotOnFailure(t *testing.T) {
	store := newFakeStore()
	now := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	p := &fakeProvider{id: "alpha", detected: true}
	p.fetch = func(context.Context) (Snapshot, error) {
		return Snapshot{}, &Error{Reason: ReasonHTTPStatus, StatusCode: 500}
	}
	svc, err := NewService(Options{
		Providers: []Provider{p},
		Store:     store,
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	// Seed a good reading, then make the provider fail.
	svc.snaps["alpha"] = Snapshot{Provider: "alpha", Windows: []Window{{Name: "5h", UsedPercent: 20}}, FetchedAt: now}
	svc.Refresh(context.Background())
	got := svc.Latest()
	if len(got) != 1 || got[0].Windows[0].UsedPercent != 20 {
		t.Fatalf("latest after failure = %+v", got)
	}
	st := svc.Statuses()[0]
	if st.Reason != ReasonHTTPStatus || st.Attempts != 1 {
		t.Fatalf("status = %+v", st)
	}
	if !st.NextAttempt.After(now) {
		t.Fatalf("no backoff: %+v", st)
	}
}

func TestServiceRetryAfterAndDetection(t *testing.T) {
	store := newFakeStore()
	now := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	p := &fakeProvider{id: "alpha", detected: true}
	p.fetch = func(context.Context) (Snapshot, error) {
		return Snapshot{}, &Error{Reason: ReasonRateLimited, RetryAfter: 10 * time.Minute}
	}
	svc, err := NewService(Options{Providers: []Provider{p}, Store: store, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	svc.Refresh(context.Background())
	if st := svc.Statuses()[0]; st.NextAttempt.Sub(now) < 10*time.Minute {
		t.Fatalf("retry-after ignored: %+v", st)
	}

	// A provider without a credential is not fetched.
	q := &fakeProvider{id: "beta", detected: false}
	svc2, err := NewService(Options{Providers: []Provider{q}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	svc2.Refresh(context.Background())
	if calls, det := q.stats(); calls != 0 || det == 0 {
		t.Fatalf("calls=%d det=%d", calls, det)
	}
	if st := svc2.Statuses()[0]; st.Detected || st.Reason != ReasonNone {
		t.Fatalf("status = %+v", st)
	}
}

func TestServiceOfflineSkipsEveryProvider(t *testing.T) {
	// Every quota comes from a provider's server now, so offline mode skips
	// all of them and only the cached snapshots are shown.
	now := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	first := &fakeProvider{id: "first", detected: true}
	second := &fakeProvider{id: "second", detected: true}
	svc, err := NewService(Options{
		Providers: []Provider{first, second},
		Offline:   true,
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	svc.Refresh(context.Background())
	if calls, _ := first.stats(); calls != 0 {
		t.Fatalf("first fetched %d times", calls)
	}
	if calls, _ := second.stats(); calls != 0 {
		t.Fatalf("second fetched %d times", calls)
	}
	for _, st := range svc.Statuses() {
		if !st.Offline {
			t.Fatalf("status = %+v, want Offline", st)
		}
	}
}

func TestServiceLoopPollsAndCloses(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	done := make(chan struct{}, 1)
	p := &fakeProvider{id: "alpha", detected: true}
	p.fetch = func(context.Context) (Snapshot, error) {
		select {
		case done <- struct{}{}:
		default:
		}
		return Snapshot{Provider: "alpha", Windows: []Window{{Name: "week", UsedPercent: 1}}}, nil
	}
	svc, err := NewService(Options{
		Providers: []Provider{p},
		Now:       func() time.Time { return now },
		Tick:      5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.Start()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not fetch")
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := svc.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if len(svc.Latest()) != 1 {
		t.Fatal("snapshot missing after close")
	}
}

func TestServiceDetectedFiltersSignedOutProviders(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	store := newFakeStore()
	p := &fakeProvider{id: "alpha", detected: true}
	svc, err := NewService(Options{Providers: []Provider{p}, Store: store, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	svc.Refresh(context.Background())
	if got := svc.Detected(); len(got) != 1 {
		t.Fatalf("detected = %+v", got)
	}

	// Signing out keeps the last good snapshot but hides it from Detected.
	p.detected = false
	svc.Refresh(context.Background())
	if got := svc.Detected(); len(got) != 0 {
		t.Fatalf("detected after sign-out = %+v", got)
	}
	if got := svc.Latest(); len(got) != 1 {
		t.Fatalf("latest after sign-out = %+v", got)
	}
}

func TestSortSnapshots(t *testing.T) {
	snaps := []Snapshot{{Provider: "z"}, {Provider: "b"}, {Provider: "a"}}
	sortSnapshots([]string{"b", "a"}, snaps)
	if snaps[0].Provider != "b" || snaps[1].Provider != "a" || snaps[2].Provider != "z" {
		t.Fatalf("order = %v", snaps)
	}
}

func TestErrorSanitized(t *testing.T) {
	err := &Error{Reason: ReasonUnauthorized, StatusCode: 401, cause: errors.New("secret token abc")}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error leaks cause: %v", err)
	}
	if !IsReason(err, ReasonUnauthorized) {
		t.Fatal("reason lost")
	}
	if got := err.QuotaReason(); got != string(ReasonUnauthorized) {
		t.Fatalf("reason = %q", got)
	}
	if got := err.RetryDelay(); got != 0 {
		t.Fatalf("retry delay = %v", got)
	}
}
