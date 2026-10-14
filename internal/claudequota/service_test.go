package claudequota

// Focused tests for the opt-in Claude quota service. Every credential is a
// synthetic file in a t.TempDir tree and every request goes through a fake
// transport, so no test touches a real home directory, the macOS keychain or
// the network.
//
// The privacy contract is asserted directly: a default-off service reveals no
// credential and sends no request, an explicit enable reads the credential only
// when a fetch runs, and no token ever reaches the settings store, an error
// string or a log-visible formatting path.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "sk-ant-oat01-SECRET-TOKEN-VALUE"

type fakeStore struct {
	mu   sync.Mutex
	vals map[string]string
	gets int
	sets int
}

func newFakeStore() *fakeStore { return &fakeStore{vals: map[string]string{}} }

func (f *fakeStore) Setting(_ context.Context, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	return f.vals[key], nil
}

func (f *fakeStore) SetSetting(_ context.Context, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets++
	f.vals[key] = value
	return nil
}

func (f *fakeStore) get(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vals[key]
}

func (f *fakeStore) all() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for k, v := range f.vals {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(v)
		b.WriteString("\n")
	}
	return b.String()
}

type fakeTransport struct {
	mu    sync.Mutex
	calls int
	reqs  []*http.Request
	fn    func(*http.Request) (*http.Response, error)
}

func (f *fakeTransport) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.calls++
	f.reqs = append(f.reqs, req)
	fn := f.fn
	f.mu.Unlock()
	if fn == nil {
		return nil, errors.New("fake transport: no handler")
	}
	return fn(req)
}

func (f *fakeTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeTransport) last() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		return nil
	}
	return f.reqs[len(f.reqs)-1]
}

// okTransport answers every request with 200 and the given body.
func okTransport(body string) *fakeTransport {
	return &fakeTransport{fn: func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	}}
}

func statusTransport(status int, body string, header http.Header) *fakeTransport {
	if header == nil {
		header = http.Header{}
	}
	header.Set("Content-Type", "application/json")
	return &fakeTransport{fn: func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	}}
}

// writeCredentials writes a Claude CLI credentials file and returns its path.
func writeCredentials(t *testing.T, dir, token string, expires *time.Time) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"claudeAiOauth": map[string]any{
		"accessToken":  token,
		"refreshToken": "REFRESH-MUST-NEVER-BE-READ",
		"scopes":       []string{"user:inference"},
	}}
	if expires != nil {
		payload["claudeAiOauth"].(map[string]any)["expiresAt"] = expires.UnixMilli()
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, CredentialsFileName)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// countedSource wraps a file source and counts credential reads.
func countedSource(t *testing.T, instance, dir, token string, expires *time.Time, reads *atomic.Int64) Source {
	t.Helper()
	path := writeCredentials(t, dir, token, expires)
	src := FileSource(instance, "Claude Code", path)
	inner := src.Read
	src.Read = func(ctx context.Context) (Credential, error) {
		reads.Add(1)
		return inner(ctx)
	}
	return src
}

func windowByKind(windows []Window, kind string) (Window, bool) {
	for _, w := range windows {
		if w.Kind == kind {
			return w, true
		}
	}
	return Window{}, false
}

func reasonIs(t *testing.T, err error, want Reason) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", want)
	}
	if got := reasonOf(err); got != want {
		t.Fatalf("expected reason %s, got %s (%v)", want, got, err)
	}
}

// TestDefaultOffNoReadNoRequest is the core default-off contract: with no stored
// consent, neither the auto loop nor an explicit Refresh reads a credential or
// sends a request, and nothing is reported as 0%.
func TestDefaultOffNoReadNoRequest(t *testing.T) {
	store := newFakeStore()
	var reads atomic.Int64
	src := countedSource(t, "claude", t.TempDir(), testToken, nil, &reads)
	tp := okTransport(`{"five_hour":{"utilization":10}}`)
	svc := New(Options{Store: store, Sources: []Source{src}, Transport: tp})
	ctx := context.Background()

	svc.Start(ctx)
	svc.Wake()
	svc.Close()
	svc.pass(ctx)

	if reads.Load() != 0 {
		t.Fatalf("default off read the credential %d times", reads.Load())
	}
	if tp.count() != 0 {
		t.Fatalf("default off sent %d requests", tp.count())
	}
	info, err := svc.ConsentInfo(ctx, "claude")
	if err != nil {
		t.Fatalf("ConsentInfo: %v", err)
	}
	if info.Enabled {
		t.Fatal("default off reported enabled")
	}
	if info.Target != Target || info.SourceRef != src.Ref {
		t.Fatalf("confirmation text must be available without a secret read: %+v", info)
	}
	snap, err := svc.Snapshot(ctx, "claude")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Status != StatusUnavailable || len(snap.Windows) != 0 {
		t.Fatalf("never-fetched reading must be unavailable and empty, got %+v", snap)
	}
	if _, err := svc.Refresh(ctx, "claude"); !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("refresh without consent: %v", err)
	}
	if reads.Load() != 0 || tp.count() != 0 {
		t.Fatal("refused refresh still touched the credential or the network")
	}
}

// TestEnableThenRefresh proves enable itself is inert, and that exactly one
// credential read and one request happen per authorized fetch.
func TestEnableThenRefresh(t *testing.T) {
	store := newFakeStore()
	var reads atomic.Int64
	src := countedSource(t, "claude", t.TempDir(), testToken, nil, &reads)
	tp := okTransport(`{"five_hour":{"utilization":12.5,"resets_at":"2026-01-02T03:04:05Z"},
		"seven_day":{"utilization":66,"resets_at":1767323045},
		"seven_day_opus":null}`)
	svc := New(Options{Store: store, Sources: []Source{src}, Transport: tp})
	ctx := context.Background()

	info, err := svc.Enable(ctx, "claude", "")
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if !info.Enabled || info.Target != Target || info.SourceRef != src.Ref || info.SourceKind != SourceFile {
		t.Fatalf("enable did not record the exact source and target: %+v", info)
	}
	if reads.Load() != 0 {
		t.Fatalf("enable read the credential %d times", reads.Load())
	}
	if tp.count() != 0 {
		t.Fatalf("enable sent %d requests", tp.count())
	}

	q, err := svc.Refresh(ctx, "claude")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if reads.Load() != 1 || tp.count() != 1 {
		t.Fatalf("want exactly one read and one request, got %d reads / %d requests", reads.Load(), tp.count())
	}
	if q.Status != StatusOK || q.FetchedAt.IsZero() || q.SourceRef != src.Ref {
		t.Fatalf("unexpected reading %+v", q)
	}
	req := tp.last()
	if req.Method != http.MethodGet || req.URL.String() != Target {
		t.Fatalf("request must be a GET to the fixed target, got %s %s", req.Method, req.URL)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+testToken {
		t.Fatalf("authorization header wrong: %q", got)
	}

	five, ok := windowByKind(q.Windows, "five_hour")
	if !ok || !five.Present || five.UsedPercent == nil || *five.UsedPercent != 12.5 {
		t.Fatalf("five_hour window wrong: %+v", five)
	}
	if five.RemainingPercent == nil || *five.RemainingPercent != 87.5 {
		t.Fatalf("five_hour remaining wrong: %+v", five.RemainingPercent)
	}
	if five.WindowMinutes == nil || *five.WindowMinutes != 300 {
		t.Fatalf("five_hour window length wrong: %+v", five.WindowMinutes)
	}
	if five.ResetsAt == nil || !five.ResetsAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("five_hour reset wrong: %+v", five.ResetsAt)
	}
	seven, ok := windowByKind(q.Windows, "seven_day")
	if !ok || seven.ResetsAt == nil || seven.ResetsAt.Unix() != 1767323045 {
		t.Fatalf("seven_day unix reset wrong: %+v", seven)
	}
	// A window the endpoint states as null is absent, never 0%.
	opus, ok := windowByKind(q.Windows, "seven_day_opus")
	if !ok || opus.Present || opus.UsedPercent != nil || opus.RemainingPercent != nil {
		t.Fatalf("null window must stay absent, got %+v", opus)
	}
	// A window the endpoint never mentions is absent, never 0%.
	if _, ok := windowByKind(q.Windows, "seven_day_sonnet"); ok {
		t.Fatal("unstated window must be omitted")
	}

	// A second refresh inside the throttle window is refused without a request.
	if _, err := svc.Refresh(ctx, "claude"); !errors.Is(err, ErrThrottled) {
		t.Fatalf("expected throttle, got %v", err)
	}
	if tp.count() != 1 || reads.Load() != 1 {
		t.Fatalf("throttled refresh must not reach the credential or the network (%d/%d)", tp.count(), reads.Load())
	}
}

// TestMissingIsNotZero covers an empty payload: no window is an error, and the
// cached reading keeps zero windows rather than a 0% window.
func TestMissingIsNotZero(t *testing.T) {
	store := newFakeStore()
	var reads atomic.Int64
	src := countedSource(t, "claude", t.TempDir(), testToken, nil, &reads)
	tp := okTransport(`{}`)
	svc := New(Options{Store: store, Sources: []Source{src}, Transport: tp})
	ctx := context.Background()
	if _, err := svc.Enable(ctx, "claude", ""); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Refresh(ctx, "claude")
	reasonIs(t, err, ReasonNoWindows)
	snap, _ := svc.Snapshot(ctx, "claude")
	if len(snap.Windows) != 0 {
		t.Fatalf("empty payload produced windows: %+v", snap.Windows)
	}
	if snap.Status != StatusUnavailable && snap.Status != StatusStale {
		t.Fatalf("empty payload must not read as fresh 0%%: %+v", snap)
	}
}

// TestPersistenceAndSharedConsent proves the consent survives a restart and is
// read without touching the credential.
func TestPersistenceAndSharedConsent(t *testing.T) {
	store := newFakeStore()
	dir := t.TempDir()
	var reads atomic.Int64
	src := countedSource(t, "claude", dir, testToken, nil, &reads)
	ctx := context.Background()
	first := New(Options{Store: store, Sources: []Source{src}, Transport: okTransport(`{"five_hour":{"utilization":1}}`)})
	if _, err := first.Enable(ctx, "claude", ""); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	first.Close()

	// A second service, as after a restart or in the other front end, sees the
	// same consent from the shared store.
	var reads2 atomic.Int64
	src2 := countedSource(t, "claude", dir, testToken, nil, &reads2)
	second := New(Options{Store: store, Sources: []Source{src2}, Transport: okTransport(`{"five_hour":{"utilization":2}}`)})
	info, err := second.ConsentInfo(ctx, "claude")
	if err != nil {
		t.Fatalf("ConsentInfo: %v", err)
	}
	if !info.Enabled || !info.SourceAvailable || info.ConsentVersion != ConsentVersion {
		t.Fatalf("persisted consent not honored: %+v", info)
	}
	if reads2.Load() != 0 {
		t.Fatal("reading consent state revealed a credential")
	}
	if _, err := second.Refresh(ctx, "claude"); err != nil {
		t.Fatalf("Refresh after restart: %v", err)
	}
	if reads2.Load() != 1 {
		t.Fatalf("want one credential read, got %d", reads2.Load())
	}
}

// TestDisableCancelsInFlight proves disable cancels a request already in flight,
// publishes nothing, and cannot be re-armed by the loop or by the caller.
func TestDisableCancelsInFlight(t *testing.T) {
	store := newFakeStore()
	var reads atomic.Int64
	src := countedSource(t, "claude", t.TempDir(), testToken, nil, &reads)
	started := make(chan struct{})
	tp := &fakeTransport{fn: func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	}}
	svc := New(Options{Store: store, Sources: []Source{src}, Transport: tp})
	ctx := context.Background()
	if _, err := svc.Enable(ctx, "claude", ""); err != nil {
		t.Fatal(err)
	}

	type result struct {
		q   Quota
		err error
	}
	done := make(chan result, 1)
	go func() {
		q, err := svc.Refresh(ctx, "claude")
		done <- result{q, err}
	}()
	<-started

	if err := svc.Disable(ctx, "claude"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	select {
	case r := <-done:
		if r.err == nil {
			t.Fatalf("canceled refresh returned a reading: %+v", r.q)
		}
		if reason := reasonOf(r.err); reason != ReasonCanceled && reason != ReasonDisabled {
			t.Fatalf("expected canceled/disabled, got %s (%v)", reason, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("disable did not cancel the in-flight request")
	}
	if got := store.get(LatestKey("claude")); got != "" {
		t.Fatalf("disabled instance published a reading: %s", got)
	}
	if got := store.get(ConsentKey("claude")); got != "" {
		t.Fatalf("consent not revoked: %s", got)
	}
	calls := tp.count()
	if _, err := svc.Refresh(ctx, "claude"); !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("refresh after disable: %v", err)
	}
	svc.pass(ctx)
	if tp.count() != calls {
		t.Fatal("the auto loop re-armed a disabled instance")
	}
}

// TestRevocationDuringRequestBlocksPublish revokes consent while a request is
// in flight and asserts the fresh reading is discarded.
func TestRevocationDuringRequestBlocksPublish(t *testing.T) {
	store := newFakeStore()
	var reads atomic.Int64
	src := countedSource(t, "claude", t.TempDir(), testToken, nil, &reads)
	var svc *Service
	tp := &fakeTransport{fn: func(req *http.Request) (*http.Response, error) {
		// Revoke from inside the request: the publish must lose the race.
		if err := svc.Disable(req.Context(), "claude"); err != nil {
			t.Errorf("Disable: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{"five_hour":{"utilization":42}}`)),
			Request:    req,
		}, nil
	}}
	svc = New(Options{Store: store, Sources: []Source{src}, Transport: tp})
	ctx := context.Background()
	if _, err := svc.Enable(ctx, "claude", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refresh(ctx, "claude"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("expected disabled after revocation, got %v", err)
	}
	if got := store.get(LatestKey("claude")); got != "" {
		t.Fatalf("revoked instance published a reading: %s", got)
	}
}

// TestSourceAndTargetChangeInvalidatesConsent covers a moved credential file
// and a tampered request target.
func TestSourceAndTargetChangeInvalidatesConsent(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	var reads atomic.Int64
	src := countedSource(t, "claude", t.TempDir(), testToken, nil, &reads)
	tp := okTransport(`{"five_hour":{"utilization":5}}`)
	svc := New(Options{Store: store, Sources: []Source{src}, Transport: tp})
	if _, err := svc.Enable(ctx, "claude", src.Ref); err != nil {
		t.Fatal(err)
	}

	// The configured source moved: the old consent authorizes nothing.
	var reads2 atomic.Int64
	moved := countedSource(t, "claude", t.TempDir(), testToken, nil, &reads2)
	movedSvc := New(Options{Store: store, Sources: []Source{moved}, Transport: tp})
	info, err := movedSvc.ConsentInfo(ctx, "claude")
	if err != nil {
		t.Fatalf("ConsentInfo: %v", err)
	}
	if info.Enabled || info.SourceAvailable {
		t.Fatalf("a consent must not carry over to a different source: %+v", info)
	}
	if _, err := movedSvc.Refresh(ctx, "claude"); err == nil {
		t.Fatal("refresh with a stale source ref must be refused")
	}
	if reads2.Load() != 0 || tp.count() != 0 {
		t.Fatal("a stale source ref still read a credential or sent a request")
	}

	// A tampered target fails closed before any read or request.
	tampered := Consent{Version: ConsentVersion, Instance: "claude", SourceKind: SourceFile,
		SourceRef: src.Ref, Target: "https://example.invalid/usage", GrantedAt: time.Now().UTC()}
	enc, err := encodeConsent(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSetting(ctx, ConsentKey("claude"), enc); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refresh(ctx, "claude"); !errors.Is(err, ErrNotEnabled) && reasonOf(err) != ReasonConsentInvalid {
		t.Fatalf("tampered target must fail closed, got %v", err)
	}
	if reads.Load() != 0 || tp.count() != 0 {
		t.Fatal("tampered consent still read a credential or sent a request")
	}
}

// TestRedaction asserts the token never reaches a formatter, JSON, an error
// string or the settings store.
func TestRedaction(t *testing.T) {
	raw := []byte(`{"claudeAiOauth":{"accessToken":"` + testToken + `","expiresAt":4102444800000}}`)
	cred, err := decodeCredential(raw)
	if err != nil {
		t.Fatalf("decodeCredential: %v", err)
	}
	for _, got := range []string{
		cred.String(),
		cred.GoString(),
		fmt.Sprintf("%v", cred),
		fmt.Sprintf("%s", cred),
		fmt.Sprintf("%#v", cred),
		fmt.Sprintf("%q", cred),
	} {
		if strings.Contains(got, testToken) {
			t.Fatalf("credential leaked through a formatter: %s", got)
		}
		if !strings.Contains(got, redacted) {
			t.Fatalf("credential formatting must be the redaction marker, got %s", got)
		}
	}
	js, err := json.Marshal(cred)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(js), testToken) {
		t.Fatalf("credential leaked through JSON: %s", js)
	}

	// A transport failure whose message contains the token must not surface it.
	store := newFakeStore()
	var reads atomic.Int64
	src := countedSource(t, "claude", t.TempDir(), testToken, nil, &reads)
	tp := &fakeTransport{fn: func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed with Authorization: Bearer " + testToken)
	}}
	svc := New(Options{Store: store, Sources: []Source{src}, Transport: tp})
	ctx := context.Background()
	if _, err := svc.Enable(ctx, "claude", ""); err != nil {
		t.Fatal(err)
	}
	_, ferr := svc.Refresh(ctx, "claude")
	if ferr == nil {
		t.Fatal("expected a transport failure")
	}
	if strings.Contains(fmt.Sprintf("%v", ferr), testToken) || strings.Contains(ferr.Error(), testToken) {
		t.Fatalf("error string leaked the credential: %v", ferr)
	}
	reasonIs(t, ferr, ReasonNetwork)
	if all := store.all(); strings.Contains(all, testToken) {
		t.Fatalf("settings store contains a secret:\n%s", all)
	}
	if !strings.Contains(store.get(ConsentKey("claude")), `"source_ref"`) {
		t.Fatalf("consent record missing: %s", store.get(ConsentKey("claude")))
	}
	// The credential is still revealed exactly once, for the one request.
	if got := CredentialReveals(); got < 1 {
		t.Fatalf("expected the authorized request to reveal the token, got %d", got)
	}
}

// TestSourceFailuresAndHTTPFailures covers missing/expired credentials and the
// endpoint's refusal statuses.
func TestSourceFailuresAndHTTPFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("missing source", func(t *testing.T) {
		store := newFakeStore()
		var reads atomic.Int64
		src := FileSource("claude", "Claude Code", filepath.Join(t.TempDir(), CredentialsFileName))
		inner := src.Read
		src.Read = func(ctx context.Context) (Credential, error) { reads.Add(1); return inner(ctx) }
		tp := okTransport(`{"five_hour":{"utilization":1}}`)
		svc := New(Options{Store: store, Sources: []Source{src}, Transport: tp})
		if _, err := svc.Enable(ctx, "claude", ""); err != nil {
			t.Fatal(err)
		}
		_, err := svc.Refresh(ctx, "claude")
		reasonIs(t, err, ReasonSourceMissing)
		if tp.count() != 0 {
			t.Fatal("a missing credential must not reach the network")
		}
	})

	t.Run("expired token", func(t *testing.T) {
		store := newFakeStore()
		var reads atomic.Int64
		past := time.Now().Add(-2 * time.Hour)
		src := countedSource(t, "claude", t.TempDir(), testToken, &past, &reads)
		tp := okTransport(`{"five_hour":{"utilization":1}}`)
		svc := New(Options{Store: store, Sources: []Source{src}, Transport: tp})
		if _, err := svc.Enable(ctx, "claude", ""); err != nil {
			t.Fatal(err)
		}
		_, err := svc.Refresh(ctx, "claude")
		reasonIs(t, err, ReasonTokenExpired)
		if !strings.Contains(err.Error(), "renew") {
			t.Fatalf("expired token must tell the user to renew it: %v", err)
		}
		if tp.count() != 0 {
			t.Fatal("an expired token must not reach the network")
		}
		if got := store.get(LatestKey("claude")); got == "" || !strings.Contains(got, string(ReasonTokenExpired)) {
			t.Fatalf("expired status not recorded: %s", got)
		}
	})

	t.Run("rate limited then throttled", func(t *testing.T) {
		store := newFakeStore()
		var reads atomic.Int64
		src := countedSource(t, "claude", t.TempDir(), testToken, nil, &reads)
		header := http.Header{}
		header.Set("Retry-After", "120")
		tp := statusTransport(http.StatusTooManyRequests, `{"error":"slow down"}`, header)
		svc := New(Options{Store: store, Sources: []Source{src}, Transport: tp})
		if _, err := svc.Enable(ctx, "claude", ""); err != nil {
			t.Fatal(err)
		}
		_, err := svc.Refresh(ctx, "claude")
		reasonIs(t, err, ReasonRateLimited)
		var e *Error
		if !errors.As(err, &e) || e.RetryAfter != 120*time.Second {
			t.Fatalf("Retry-After not honored: %+v", err)
		}
		if _, err := svc.Refresh(ctx, "claude"); !errors.Is(err, ErrThrottled) {
			t.Fatalf("expected throttle after a 429, got %v", err)
		}
		if tp.count() != 1 {
			t.Fatalf("throttled refresh sent a request: %d", tp.count())
		}
	})

	t.Run("unauthorized", func(t *testing.T) {
		store := newFakeStore()
		var reads atomic.Int64
		src := countedSource(t, "claude", t.TempDir(), testToken, nil, &reads)
		svc := New(Options{Store: store, Sources: []Source{src}, Transport: statusTransport(http.StatusUnauthorized, `{}`, nil)})
		if _, err := svc.Enable(ctx, "claude", ""); err != nil {
			t.Fatal(err)
		}
		_, err := svc.Refresh(ctx, "claude")
		reasonIs(t, err, ReasonUnauthorized)
		if !strings.Contains(err.Error(), "renew") {
			t.Fatalf("401 must point at the Claude CLI: %v", err)
		}
	})

	t.Run("redirect refused", func(t *testing.T) {
		store := newFakeStore()
		var reads atomic.Int64
		src := countedSource(t, "claude", t.TempDir(), testToken, nil, &reads)
		tp := statusTransport(http.StatusFound, "", nil)
		svc := New(Options{Store: store, Sources: []Source{src}, Transport: tp})
		if _, err := svc.Enable(ctx, "claude", ""); err != nil {
			t.Fatal(err)
		}
		_, err := svc.Refresh(ctx, "claude")
		reasonIs(t, err, ReasonRedirectRefused)
	})

	t.Run("malformed body", func(t *testing.T) {
		store := newFakeStore()
		var reads atomic.Int64
		src := countedSource(t, "claude", t.TempDir(), testToken, nil, &reads)
		svc := New(Options{Store: store, Sources: []Source{src}, Transport: okTransport(`not json`)})
		if _, err := svc.Enable(ctx, "claude", ""); err != nil {
			t.Fatal(err)
		}
		_, err := svc.Refresh(ctx, "claude")
		reasonIs(t, err, ReasonMalformed)
	})
}

// TestOfflineNeverNetworks proves offline mode refuses every path.
func TestOfflineNeverNetworks(t *testing.T) {
	store := newFakeStore()
	var reads atomic.Int64
	src := countedSource(t, "claude", t.TempDir(), testToken, nil, &reads)
	tp := okTransport(`{"five_hour":{"utilization":1}}`)
	svc := New(Options{Store: store, Sources: []Source{src}, Transport: tp, Offline: true})
	ctx := context.Background()
	if _, err := svc.Enable(ctx, "claude", ""); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	svc.Start(ctx)
	svc.Wake()
	svc.Close()
	if _, err := svc.Refresh(ctx, "claude"); !errors.Is(err, ErrOffline) {
		t.Fatalf("offline refresh: %v", err)
	}
	svc.pass(ctx)
	if reads.Load() != 0 || tp.count() != 0 {
		t.Fatalf("offline mode read a credential or used the network (%d/%d)", reads.Load(), tp.count())
	}
}

// TestKeychainSourceIsInjectedNeverReal proves the keychain source runs only
// the injected runner and reports unsupported when none exists.
func TestKeychainSourceIsInjectedNeverReal(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int64
	store := newFakeStore()
	run := func(context.Context, string, string) ([]byte, error) {
		calls.Add(1)
		return []byte(`{"claudeAiOauth":{"accessToken":"` + testToken + `"}}`), nil
	}
	src := KeychainSource("claude", "keychain", ClaudeCodeKeychainService, "", run)
	tp := okTransport(`{"five_hour":{"utilization":3}}`)
	svc := New(Options{Store: store, Sources: []Source{src}, Transport: tp})
	if _, err := svc.Enable(ctx, "claude", ""); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("enable touched the keychain runner")
	}
	if _, err := svc.Refresh(ctx, "claude"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("want one injected keychain lookup, got %d", calls.Load())
	}
	if src.Kind != SourceKeychain || !strings.Contains(src.Ref, ClaudeCodeKeychainService) {
		t.Fatalf("keychain ref must be non-secret and stable: %+v", src)
	}

	unsupported := KeychainSource("claude", "keychain", ClaudeCodeKeychainService, "", nil)
	if unsupported.Read == nil {
		t.Fatal("unsupported keychain source must still be readable as an error")
	}
	if _, err := unsupported.Read(ctx); reasonOf(err) != ReasonSourceUnsupported {
		t.Fatalf("want source_unsupported, got %v", err)
	}
}

// TestPerInstanceIsolation proves one instance's consent does not authorize
// another instance or another profile.
func TestPerInstanceIsolation(t *testing.T) {
	store := newFakeStore()
	var aReads, bReads atomic.Int64
	a := countedSource(t, "a", t.TempDir(), testToken, nil, &aReads)
	b := countedSource(t, "b", t.TempDir(), testToken, nil, &bReads)
	tp := okTransport(`{"five_hour":{"utilization":7}}`)
	svc := New(Options{Store: store, Sources: []Source{a, b}, Transport: tp})
	ctx := context.Background()
	if _, err := svc.Enable(ctx, "a", ""); err != nil {
		t.Fatal(err)
	}
	infoB, err := svc.ConsentInfo(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	if infoB.Enabled {
		t.Fatalf("consent for instance a leaked to b: %+v", infoB)
	}
	if _, err := svc.Refresh(ctx, "b"); !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("refresh b: %v", err)
	}
	if bReads.Load() != 0 || tp.count() != 0 {
		t.Fatal("another instance was read without its own consent")
	}
	if _, err := svc.Refresh(ctx, "a"); err != nil {
		t.Fatalf("refresh a: %v", err)
	}
	if aReads.Load() != 1 || len(svc.SourceRefs("a")) != 1 {
		t.Fatal("instance a did not refresh independently")
	}
}

// crossProcessStore models a second process (the CLI) sharing the same settings
// store: it revokes consent exactly while this process is writing a reading, so
// the revocation lands after the publish's pre-write consent recheck.
type crossProcessStore struct {
	*fakeStore
	latestWrites int
}

func (c *crossProcessStore) SetSetting(ctx context.Context, key, value string) error {
	if strings.HasPrefix(key, SettingLatestPrefix) && value != "" && c.latestWrites == 0 {
		c.latestWrites++
		// The other process revoked consent before this reading was stored.
		if err := c.fakeStore.SetSetting(ctx, ConsentKey("claude"), ""); err != nil {
			return err
		}
	}
	return c.fakeStore.SetSetting(ctx, key, value)
}

// TestCrossProcessDisableBlocksPublish pins that a Disable performed by another
// process against the shared store invalidates this process's consent before
// its reading is published: the reading is discarded, never left behind.
func TestCrossProcessDisableBlocksPublish(t *testing.T) {
	base := newFakeStore()
	store := &crossProcessStore{fakeStore: base}
	var reads atomic.Int64
	src := countedSource(t, "claude", t.TempDir(), testToken, nil, &reads)
	tp := okTransport(`{"five_hour":{"utilization":42}}`)
	svc := New(Options{Store: store, Sources: []Source{src}, Transport: tp})
	ctx := context.Background()
	if _, err := svc.Enable(ctx, "claude", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refresh(ctx, "claude"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("a cross-process revoke must block publication, got %v", err)
	}
	if got := base.get(LatestKey("claude")); got != "" {
		t.Fatalf("reading published after a cross-process revoke: %s", got)
	}
	if got := base.get(ConsentKey("claude")); got != "" {
		t.Fatalf("consent should stay revoked: %s", got)
	}
	q, err := svc.Snapshot(ctx, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if q.Status != StatusUnavailable || len(q.Windows) != 0 {
		t.Fatalf("snapshot exposed a reading after a cross-process revoke: %+v", q)
	}
}

// TestConfigDirFromRootsRequiresOneUnambiguousOrigin pins the derivation
// contract: zero or more than one distinct projects root yields no origin, so a
// caller can never bind the first account it happens to see.
func TestConfigDirFromRootsRequiresOneUnambiguousOrigin(t *testing.T) {
	cfg := t.TempDir()
	if dir, ok := ConfigDirFromRoots([]string{filepath.Join(cfg, "projects")}); !ok || dir != cfg {
		t.Fatalf("single projects root = %q,%v; want %q,true", dir, ok, cfg)
	}
	if _, ok := ConfigDirFromRoots([]string{cfg}); ok {
		t.Fatal("a root that is not a projects directory must not yield an origin")
	}
	if _, ok := ConfigDirFromRoots(nil); ok {
		t.Fatal("no roots must not yield an origin")
	}
	a, b := filepath.Join(t.TempDir(), "projects"), filepath.Join(t.TempDir(), "projects")
	if _, ok := ConfigDirFromRoots([]string{a, b}); ok {
		t.Fatal("two distinct projects roots are ambiguous and must yield no origin")
	}
	// A duplicate of the same origin stays unambiguous.
	if dir, ok := ConfigDirFromRoots([]string{a, a}); !ok || dir != filepath.Dir(a) {
		t.Fatalf("the same projects root twice = %q,%v; want %q,true", dir, ok, filepath.Dir(a))
	}
}
