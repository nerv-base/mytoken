package antigravity

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nerv-base/mytoken/internal/quota/plan"
)

const summaryFixture = `{"groups":[
	{"displayName":"Gemini Models","description":"Gemini 4 Neon, Gemini 4 Carbon","buckets":[
		{"bucketId":"gemini-weekly","displayName":"Weekly Limit Remaining","window":"weekly","resetTime":"2026-12-01T00:33:08Z","remainingFraction":0.5},
		{"bucketId":"gemini-5h","displayName":"Five Hour Limit Remaining","window":"5h","resetTime":"2026-11-24T18:23:03Z","remainingFraction":0.25}
	]},
	{"displayName":"Claude and GPT models","buckets":[
		{"bucketId":"3p-weekly","window":"weekly","remainingFraction":1},
		{"bucketId":"3p-5h","window":"5h","remainingFraction":1}
	]}
]}`

func tokenJSON(token, expiry string) string {
	return fmt.Sprintf(`{"auth_method":"consumer","id_token_email":"user@example.com","token":{"access_token":%q,"expiry":%q,"token_type":"Bearer","expires_in":3598}}`, token, expiry)
}

func writeToken(t *testing.T, root, instance, body string) string {
	t.Helper()
	path := filepath.Join(root, instance, "antigravity-acp", "antigravity-oauth-token")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir token dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	return path
}

func writeDaemon(t *testing.T, dir string, port int, csrf string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir daemon dir: %v", err)
	}
	body := fmt.Sprintf(`{"pid":1234,"httpsPort":%d,"httpPort":%d,"lsVersion":"2.34.0","csrfToken":%q}`, port+1, port, csrf)
	if err := os.WriteFile(filepath.Join(dir, "hub_1234.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write daemon file: %v", err)
	}
}

func serverPort(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	n, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return n
}

func serveFixture(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != summaryPath && r.URL.Path != "/exa.language_server_pb.LanguageServerService/RetrieveUserQuotaSummary" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchFromT3Token(t *testing.T) {
	root := t.TempDir()
	writeToken(t, root, "instance-a", tokenJSON("t3-access-token", "2026-11-25T01:00:00Z"))

	var gotAuth, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		if r.Method != http.MethodPost || r.URL.Path != summaryPath {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, summaryFixture)
	}))
	defer srv.Close()

	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	p := New(Options{
		Home:      t.TempDir(),
		T3Roots:   []string{root},
		Transport: srv.Client(),
		BaseURL:   srv.URL,
		Now:       func() time.Time { return now },
		GOOS:      "linux",
		GOARCH:    "amd64",
	})
	if !p.Detect(context.Background()) {
		t.Fatal("Detect = false, want true")
	}
	snap, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gotAuth != "Bearer t3-access-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if want := "antigravity/hub/2.24.0 (aidev_client; os_type=linux; arch=amd64; cl=963137146)"; gotUA != want {
		t.Errorf("User-Agent = %q, want %q", gotUA, want)
	}
	if snap.Provider != ProviderID || snap.Plan != "Gemini" || snap.Source != plan.SourceAPI {
		t.Errorf("snapshot = %+v", snap)
	}
	if !snap.FetchedAt.Equal(now) {
		t.Errorf("FetchedAt = %v, want %v", snap.FetchedAt, now)
	}
	byName := windowMap(snap.Windows)
	if len(byName) != 2 {
		t.Fatalf("windows = %+v, want 2", snap.Windows)
	}
	if got := byName["5h"].UsedPercent; got != 75 {
		t.Errorf("5h UsedPercent = %v, want 75", got)
	}
	if got := byName["week"].UsedPercent; got != 50 {
		t.Errorf("week UsedPercent = %v, want 50", got)
	}
	wantReset := time.Date(2026, 11, 24, 18, 23, 3, 0, time.UTC)
	if !byName["5h"].ResetsAt.Equal(wantReset) {
		t.Errorf("5h ResetsAt = %v, want %v", byName["5h"].ResetsAt, wantReset)
	}
}

func TestFetchFromDaemon(t *testing.T) {
	home := t.TempDir()
	daemonDir := filepath.Join(home, "daemon")

	var gotCSRF, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCSRF = r.Header.Get("x-codeium-csrf-token")
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/exa.language_server_pb.LanguageServerService/RetrieveUserQuotaSummary" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"response":%s}`, summaryFixture)
	}))
	defer srv.Close()
	writeDaemon(t, daemonDir, serverPort(t, srv.URL), "csrf-value")

	p := New(Options{
		Home:      home,
		DaemonDir: daemonDir,
		T3Roots:   []string{},
		Transport: srv.Client(),
		BaseURL:   "https://example.invalid",
		GOOS:      "linux",
	})
	if !p.Detect(context.Background()) {
		t.Fatal("Detect = false, want true")
	}
	snap, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gotCSRF != "csrf-value" {
		t.Errorf("csrf header = %q", gotCSRF)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty", gotAuth)
	}
	if len(snap.Windows) != 2 {
		t.Fatalf("windows = %+v", snap.Windows)
	}
}

func TestFetchDaemonFallsBackToToken(t *testing.T) {
	root := t.TempDir()
	writeToken(t, root, "instance-a", tokenJSON("fallback-token", "2026-11-25T01:00:00Z"))
	daemonDir := filepath.Join(t.TempDir(), "daemon")

	daemonHits := 0
	daemonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		daemonHits++
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer daemonSrv.Close()
	writeDaemon(t, daemonDir, serverPort(t, daemonSrv.URL), "csrf-value")

	apiHits := 0
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiHits++
		if got := r.Header.Get("Authorization"); got != "Bearer fallback-token" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, summaryFixture)
	}))
	defer apiSrv.Close()

	p := New(Options{
		Home:      t.TempDir(),
		DaemonDir: daemonDir,
		T3Roots:   []string{root},
		Transport: apiSrv.Client(),
		BaseURL:   apiSrv.URL,
		GOOS:      "linux",
	})
	snap, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if daemonHits != 1 || apiHits != 1 {
		t.Errorf("hits daemon=%d api=%d, want 1 and 1", daemonHits, apiHits)
	}
	if len(snap.Windows) != 2 {
		t.Fatalf("windows = %+v", snap.Windows)
	}
}

func TestFetchExpiredToken(t *testing.T) {
	root := t.TempDir()
	writeToken(t, root, "instance-a", tokenJSON("expired", "2026-11-24T00:00:00Z"))
	p := New(Options{
		Home:    t.TempDir(),
		T3Roots: []string{root},
		GOOS:    "linux",
		Now:     func() time.Time { return time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC) },
	})
	_, err := p.Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonTokenExpired) {
		t.Fatalf("err = %v, want token_expired", err)
	}
}

func TestFetchNoCredentials(t *testing.T) {
	p := New(Options{Home: t.TempDir(), T3Roots: []string{}, GOOS: "linux"})
	if p.Detect(context.Background()) {
		t.Fatal("Detect = true, want false")
	}
	_, err := p.Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonNoCredentials) {
		t.Fatalf("err = %v, want no_credentials", err)
	}
}

func TestDetectDoesNotReadKeychain(t *testing.T) {
	p := New(Options{
		Home:    t.TempDir(),
		T3Roots: []string{},
		GOOS:    "darwin",
		KeychainLookup: func(context.Context, string, string) (string, error) {
			t.Fatal("Detect read the Keychain")
			return "", nil
		},
	})
	if p.Detect(context.Background()) {
		t.Fatal("Detect = true, want false")
	}
}

func TestFetchFromKeychain(t *testing.T) {
	raw := tokenJSON("keychain-token", "2026-11-25T01:00:00Z")
	value := keychainPrefix + base64.StdEncoding.EncodeToString([]byte(raw))

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, summaryFixture)
	}))
	defer srv.Close()

	p := New(Options{
		Home:          t.TempDir(),
		T3Roots:       []string{},
		Transport:     srv.Client(),
		BaseURL:       srv.URL,
		GOOS:          "darwin",
		KeychainValue: value,
		Now:           func() time.Time { return time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC) },
	})
	if _, err := p.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gotAuth != "Bearer keychain-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
}

func TestFetchMalformed(t *testing.T) {
	root := t.TempDir()
	writeToken(t, root, "instance-a", tokenJSON("token", "2026-11-25T01:00:00Z"))
	srv := serveFixture(t, `not json`)
	p := New(Options{Home: t.TempDir(), T3Roots: []string{root}, Transport: srv.Client(), BaseURL: srv.URL, GOOS: "linux"})
	_, err := p.Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonMalformed) {
		t.Fatalf("err = %v, want malformed_response", err)
	}
}

func TestFetchNoGeminiGroup(t *testing.T) {
	root := t.TempDir()
	writeToken(t, root, "instance-a", tokenJSON("token", "2026-11-25T01:00:00Z"))
	srv := serveFixture(t, `{"groups":[{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-5h","remainingFraction":1}]}]}`)
	p := New(Options{Home: t.TempDir(), T3Roots: []string{root}, Transport: srv.Client(), BaseURL: srv.URL, GOOS: "linux"})
	_, err := p.Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonNoData) {
		t.Fatalf("err = %v, want no_data", err)
	}
}

func TestUserAgent(t *testing.T) {
	p := New(Options{GOOS: "darwin", GOARCH: "arm64"})
	want := "antigravity/hub/2.24.0 (aidev_client; os_type=darwin; arch=arm64; cl=963137146)"
	if got := p.userAgent(); got != want {
		t.Errorf("userAgent = %q, want %q", got, want)
	}
}

func TestDefaultT3RootsHonorsT3CODEHOME(t *testing.T) {
	p := New(Options{
		Home: "/Users/example",
		Getenv: func(key string) string {
			if key == "T3CODE_HOME" {
				return "/custom/t3"
			}
			return ""
		},
	})
	want := []string{
		filepath.Join("/custom/t3", "userdata", "providers", "antigravity"),
		filepath.Join("/custom/t3", "dev", "providers", "antigravity"),
	}
	if strings.Join(p.t3Roots, "|") != strings.Join(want, "|") {
		t.Errorf("t3Roots = %v, want %v", p.t3Roots, want)
	}
}

func TestBucketWindowName(t *testing.T) {
	tests := []struct {
		b    summaryBucket
		want string
	}{
		{summaryBucket{BucketID: "gemini-weekly"}, "week"},
		{summaryBucket{BucketID: "gemini-5h"}, "5h"},
		{summaryBucket{BucketID: "x", Window: "weekly"}, "week"},
		{summaryBucket{BucketID: "custom"}, "custom"},
	}
	for _, tt := range tests {
		if got := bucketWindowName(tt.b); got != tt.want {
			t.Errorf("bucketWindowName(%+v) = %q, want %q", tt.b, got, tt.want)
		}
	}
}

func windowMap(windows []plan.Window) map[string]plan.Window {
	out := make(map[string]plan.Window, len(windows))
	for _, w := range windows {
		out[w.Name] = w
	}
	return out
}
