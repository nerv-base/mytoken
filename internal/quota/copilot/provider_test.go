package copilot

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

const testToken = "ghu_test_token"

func writeCredentials(t *testing.T, name, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), credentialDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(dir)
}

func newProvider(t *testing.T, configDir, baseURL string) *Provider {
	t.Helper()
	getenv := func(key string) string {
		if key == "XDG_CONFIG_HOME" {
			return configDir
		}
		return ""
	}
	return New(Options{
		BaseURL: baseURL,
		Getenv:  getenv,
		GOOS:    "linux",
		Now: func() time.Time {
			return time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
		},
	})
}

func TestFetchUsage(t *testing.T) {
	configDir := writeCredentials(t, "hosts.json", `{"github.com":{"user":"octocat","oauth_token":"`+testToken+`"}}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != usagePath {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "token "+testToken {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got != githubAPIDate {
			t.Errorf("api version = %q", got)
		}
		if got := r.Header.Get("Editor-Plugin-Version"); got != pluginVersion {
			t.Errorf("plugin version = %q", got)
		}
		io.WriteString(w, `{
			"copilot_plan": "business",
			"quota_reset_date": "2026-11-01",
			"quota_snapshots": {
				"premium_interactions": {"entitlement": 300, "remaining": 210, "percent_remaining": 70},
				"chat": {"entitlement": 0, "remaining": 0}
			}
		}`)
	}))
	defer srv.Close()

	p := newProvider(t, configDir, srv.URL)
	if !p.Detect(context.Background()) {
		t.Fatal("Detect = false, want true")
	}
	snap, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.Provider != ProviderID || snap.Plan != "business" || snap.Source != plan.SourceAPI {
		t.Errorf("snapshot = %+v", snap)
	}
	if len(snap.Windows) != 1 {
		t.Fatalf("windows = %+v", snap.Windows)
	}
	win := snap.Windows[0]
	if win.Name != "month" || win.UsedPercent != 30 || win.Used != 90 || win.Limit != 300 || win.Unit != "requests" {
		t.Errorf("window = %+v", win)
	}
	if want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC); !win.ResetsAt.Equal(want) {
		t.Errorf("resets = %v, want %v", win.ResetsAt, want)
	}
}

func TestFetchMonthlyQuotasFallback(t *testing.T) {
	configDir := writeCredentials(t, "apps.json", `{"github.com":{"oauth_token":"`+testToken+`"}}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{
			"copilot_plan": "individual",
			"monthly_quotas": {"chat": "50", "completions": 100},
			"limited_user_quotas": {"chat": "40", "completions": 100}
		}`)
	}))
	defer srv.Close()

	snap, err := newProvider(t, configDir, srv.URL).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(snap.Windows) != 2 {
		t.Fatalf("windows = %+v", snap.Windows)
	}
	if got := snap.Windows[0]; got.Name != "month" || got.UsedPercent != 0 || got.Limit != 100 {
		t.Errorf("premium window = %+v", got)
	}
	if got := snap.Windows[1]; got.Name != "month-chat" || got.UsedPercent != 20 || got.Used != 10 || got.Limit != 50 {
		t.Errorf("chat window = %+v", got)
	}
}

func TestFetchUnauthorized(t *testing.T) {
	configDir := writeCredentials(t, "hosts.json", `{"github.com":{"oauth_token":"`+testToken+`"}}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := newProvider(t, configDir, srv.URL).Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonUnauthorized) {
		t.Fatalf("err = %v, want unauthorized", err)
	}
}

func TestDetectWithoutCredentials(t *testing.T) {
	p := newProvider(t, t.TempDir(), "")
	if p.Detect(context.Background()) {
		t.Fatal("Detect = true without credentials")
	}
	_, err := p.Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonNoCredentials) {
		t.Fatalf("err = %v, want no_credentials", err)
	}
}

func TestPlaceholderSnapshots(t *testing.T) {
	configDir := writeCredentials(t, "hosts.json", `{"github.com":{"oauth_token":"`+testToken+`"}}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"quota_snapshots":{"premium_interactions":{"entitlement":0,"remaining":0}}}`)
	}))
	defer srv.Close()
	_, err := newProvider(t, configDir, srv.URL).Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonNoWindows) {
		t.Fatalf("err = %v, want no_windows", err)
	}
}

func TestTokenFromCredentials(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"hosts", `{"github.com":{"oauth_token":"abc"}}`, "abc"},
		{"other host", `{"example.com":{"oauth_token":"abc"}}`, ""},
		{"nested", `{"github.com":{"apps":{"oauth_token":"abc"}}}`, ""},
		{"invalid", `not json`, ""},
	}
	for _, c := range cases {
		if got := tokenFromCredentials([]byte(c.body)); got != c.want {
			t.Errorf("%s: token = %q, want %q", c.name, got, c.want)
		}
	}
}
