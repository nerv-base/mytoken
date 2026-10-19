package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

const (
	testToken = "test-access-token-not-a-secret"
	testUID   = "00000000-0000-0000-0000-000000000000"
)

func writeAuth(t *testing.T, expiresAt int64, encrypted bool) string {
	t.Helper()
	var access any = testToken
	if encrypted {
		access = map[string]any{"$wbEncrypted": 1, "envelope": "sealed"}
	}
	doc := map[string]any{
		"account": map[string]any{"uid": testUID, "nickname": "Tester", "phoneNumber": "000"},
		"auth": map[string]any{
			"accessToken":  access,
			"refreshToken": "test-refresh",
			"expiresAt":    expiresAt,
			"domain":       "www.workbuddy.cn",
			"tokenType":    "Bearer",
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "workbuddy-desktop.info")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "workbuddy_summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func future() int64 { return time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli() }

func TestFetch(t *testing.T) {
	authPath := writeAuth(t, future(), false)
	fixture := readFixture(t)
	var headers http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		if r.Method != http.MethodPost || r.URL.Path != summaryPath {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	p := New(Options{
		AuthPath:  authPath,
		BaseURL:   srv.URL,
		Transport: srv.Client(),
		Now:       func() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) },
	})
	if !p.Detect(context.Background()) {
		t.Fatal("Detect = false")
	}
	snap, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	for key, want := range map[string]string{
		"User-Agent":    UserAgent,
		"Authorization": "Bearer " + testToken,
		"X-User-Id":     testUID,
		"X-Domain":      "www.workbuddy.cn",
		"X-Product":     "SaaS",
		"X-IDE-Type":    "WorkBuddy",
		"Content-Type":  "application/json",
		"Accept":        "application/json",
	} {
		if got := headers.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	if snap.Provider != ProviderID || snap.Plan != "体验版" || snap.Source != plan.SourceAPI {
		t.Fatalf("snapshot = %+v", snap)
	}
	w := snap.Windows[0]
	if w.Name != "Credits" || w.Balance || w.Unit != "credits" {
		t.Fatalf("window = %+v", w)
	}
	if w.Limit != 10177 || w.Used != 5588.27 || w.Remaining != 4588.73 {
		t.Fatalf("numbers = %+v", w)
	}
	if want := 5588.27 / 10177 * 100; w.UsedPercent < want-1e-9 || w.UsedPercent > want+1e-9 {
		t.Fatalf("percent = %v, want %v", w.UsedPercent, want)
	}
}

func TestEnterpriseFallback(t *testing.T) {
	authPath := writeAuth(t, future(), false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case summaryPath:
			_, _ = io.WriteString(w, `{"code":0,"data":{"IsPaidUser":true,"Packages":[]}}`)
		case enterprisePath:
			_, _ = io.WriteString(w, `{"code":0,"data":{"limitNum":"500","credit":"100","cycleStartTime":"2026-10-01 00:00:00","cycleResetTime":"2026-11-01 00:00:00"}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	p := New(Options{AuthPath: authPath, BaseURL: srv.URL, Transport: srv.Client()})
	snap, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	w := snap.Windows[0]
	if w.Limit != 500 || w.Used != 100 || w.Remaining != 400 {
		t.Fatalf("enterprise window = %+v", w)
	}
	if !w.ResetsAt.Equal(time.Date(2026, 10, 31, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("reset = %v", w.ResetsAt)
	}
}

func TestBusinessErrors(t *testing.T) {
	authPath := writeAuth(t, future(), false)
	cases := []struct {
		name string
		body string
		want plan.Reason
	}{
		{"expired token", `{"code":11001}`, plan.ReasonUnauthorized},
		{"bad user agent", `{"code":10085}`, plan.ReasonHTTPStatus},
		{"restricted", `{"code":11140}`, plan.ReasonForbidden},
		{"unknown", `{"code":12345}`, plan.ReasonHTTPStatus},
		{"no data", `{"code":0}`, plan.ReasonNoWindows},
		{"no windows", `{"code":0,"data":{"Packages":[]}}`, plan.ReasonNoWindows},
		{"malformed", `{`, plan.ReasonMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			p := New(Options{AuthPath: authPath, BaseURL: srv.URL, Transport: srv.Client()})
			_, err := p.Fetch(context.Background())
			if !plan.IsReason(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestExpiredTokenDoesNotCallNetwork(t *testing.T) {
	authPath := writeAuth(t, time.Now().Add(-time.Hour).UnixMilli(), false)
	p := New(Options{AuthPath: authPath, BaseURL: "https://example.invalid"})
	_, err := p.Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonTokenExpired) {
		t.Fatalf("err = %v", err)
	}
}

func TestEncryptedTokenIsSignedOut(t *testing.T) {
	authPath := writeAuth(t, future(), true)
	p := New(Options{AuthPath: authPath})
	if p.Detect(context.Background()) {
		t.Fatal("Detect = true for an encrypted token")
	}
	if _, err := p.Fetch(context.Background()); !plan.IsReason(err, plan.ReasonNoCredentials) {
		t.Fatalf("err = %v", err)
	}
}

type countingTransport struct{ calls int }

func (c *countingTransport) Do(*http.Request) (*http.Response, error) {
	c.calls++
	return nil, errors.New("network disabled")
}

func TestDetectDoesNotUseNetwork(t *testing.T) {
	authPath := writeAuth(t, future(), false)
	tr := &countingTransport{}
	p := New(Options{AuthPath: authPath, Transport: tr})
	if !p.Detect(context.Background()) {
		t.Fatal("Detect = false")
	}
	if tr.calls != 0 {
		t.Fatalf("Detect made %d calls", tr.calls)
	}
}

func TestAIDefaults(t *testing.T) {
	path := defaultAuthPath(true, "darwin", "/Users/test", func(string) string { return "" })
	want := filepath.Join("/Users/test", "Library", "Application Support", "CodeBuddyExtension", "Data", "Public", "auth", "workbuddy-desktop-ai.info")
	if path != want {
		t.Fatalf("path = %q", path)
	}
	p := NewAI(Options{AuthPath: path})
	if p.ID() != AIProviderID || p.DisplayName() != "WorkBuddy AI" || p.baseURL != AIBaseURL {
		t.Fatalf("provider = %+v", p)
	}
}
