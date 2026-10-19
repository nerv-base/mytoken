package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

const testAccessToken = "test-access-token-not-a-secret"

// newProvider builds a provider whose auth file lives in a temp CODEX_HOME and
// whose endpoint is the test server.
func newProvider(t *testing.T, endpoint string, auth any) *Provider {
	t.Helper()
	dir := t.TempDir()
	if auth != nil {
		raw, err := json.Marshal(auth)
		if err != nil {
			t.Fatalf("marshal auth: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), raw, 0o600); err != nil {
			t.Fatalf("write auth: %v", err)
		}
	}
	return New(Options{
		Endpoint: endpoint,
		Getenv:   func(string) string { return dir },
		Now:      func() time.Time { return time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC) },
	})
}

func chatgptAuth(accountID string) map[string]any {
	tokens := map[string]any{"access_token": testAccessToken}
	if accountID != "" {
		tokens["account_id"] = accountID
	}
	return map[string]any{"tokens": tokens, "last_refresh": "2026-10-19T00:00:00Z"}
}

func TestFetchSuccess(t *testing.T) {
	reset := int64(1893456000)
	var gotAuth, gotAccount, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccount = r.Header.Get("ChatGPT-Account-Id")
		gotUA = r.Header.Get("User-Agent")
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/backend-api/wham/usage" {
			t.Errorf("path = %q", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "plus",
			"rate_limit": map[string]any{
				"allowed":       true,
				"limit_reached": false,
				"primary_window": map[string]any{
					"used_percent":         42.5,
					"limit_window_seconds": 18000,
					"reset_after_seconds":  60,
					"reset_at":             reset,
				},
				"secondary_window": map[string]any{
					"used_percent":         12.0,
					"limit_window_seconds": 604800,
					"reset_at":             reset,
				},
			},
		})
	}))
	defer srv.Close()

	p := newProvider(t, srv.URL+"/backend-api/wham/usage", chatgptAuth("acct-123"))
	if !p.Detect(context.Background()) {
		t.Fatal("Detect = false for a ChatGPT login")
	}
	snap, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.Provider != ProviderID || snap.Plan != "plus" || snap.Source != plan.SourceAPI {
		t.Fatalf("snapshot = %+v", snap)
	}
	if want := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC); !snap.FetchedAt.Equal(want) {
		t.Errorf("FetchedAt = %s, want %s", snap.FetchedAt, want)
	}
	if len(snap.Windows) != 2 {
		t.Fatalf("windows = %+v", snap.Windows)
	}
	first, second := snap.Windows[0], snap.Windows[1]
	if first.Name != "5h" || first.UsedPercent != 42.5 {
		t.Errorf("primary = %+v", first)
	}
	if second.Name != "week" || second.UsedPercent != 12 {
		t.Errorf("secondary = %+v", second)
	}
	if want := time.Unix(reset, 0).UTC(); !first.ResetsAt.Equal(want) {
		t.Errorf("primary reset = %s, want %s", first.ResetsAt, want)
	}
	if gotAuth != "Bearer "+testAccessToken {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotAccount != "acct-123" {
		t.Errorf("ChatGPT-Account-Id = %q, want acct-123", gotAccount)
	}
	if gotUA != UserAgent {
		t.Errorf("User-Agent = %q, want %q", gotUA, UserAgent)
	}
}

func TestFetchRejectedTokenAsksToSignIn(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		p := newProvider(t, srv.URL, chatgptAuth("acct-123"))
		_, err := p.Fetch(context.Background())
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: Fetch succeeded", status)
		}
		if !strings.Contains(err.Error(), "sign in to Codex again") {
			t.Errorf("status %d: error = %q, want a sign-in instruction", status, err)
		}
		want := plan.ReasonUnauthorized
		if status == http.StatusForbidden {
			want = plan.ReasonForbidden
		}
		if !plan.IsReason(err, want) {
			t.Errorf("status %d: reason = %v, want %s", status, err, want)
		}
	}
}

func TestFetchMalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"plan_type":`))
	}))
	defer srv.Close()
	p := newProvider(t, srv.URL, chatgptAuth("acct-123"))
	if _, err := p.Fetch(context.Background()); !plan.IsReason(err, plan.ReasonMalformed) {
		t.Fatalf("err = %v, want malformed_response", err)
	}
}

func TestFetchNoWindows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"plan_type":"free","rate_limit":null}`))
	}))
	defer srv.Close()
	p := newProvider(t, srv.URL, chatgptAuth(""))
	if _, err := p.Fetch(context.Background()); !plan.IsReason(err, plan.ReasonNoWindows) {
		t.Fatalf("err = %v, want no_windows", err)
	}
}

func TestAPIKeyLoginIsDetectedButUnsupported(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer srv.Close()
	p := newProvider(t, srv.URL, map[string]any{"OPENAI_API_KEY": "sk-test"})
	if !p.Detect(context.Background()) {
		t.Fatal("Detect = false for an API-key login")
	}
	_, err := p.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "API-key login has no plan quota") {
		t.Fatalf("err = %v, want the API-key explanation", err)
	}
	if requests != 0 {
		t.Errorf("API-key login made %d HTTP requests, want 0", requests)
	}
}

func TestMissingAuthFile(t *testing.T) {
	p := newProvider(t, "http://127.0.0.1:1", nil)
	if p.Detect(context.Background()) {
		t.Fatal("Detect = true without an auth file")
	}
	if _, err := p.Fetch(context.Background()); !plan.IsReason(err, plan.ReasonSourceMissing) {
		t.Fatalf("err = %v, want source_missing", err)
	}
}

func TestAccountIDFromIDTokenFallback(t *testing.T) {
	claims, _ := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-jwt"},
	})
	idToken := "h." + base64.RawURLEncoding.EncodeToString(claims) + ".s"
	auth := map[string]any{"tokens": map[string]any{
		"access_token": testAccessToken,
		"id_token":     idToken,
	}}
	var gotAccount string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccount = r.Header.Get("ChatGPT-Account-Id")
		w.Write([]byte(`{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000}}}`))
	}))
	defer srv.Close()
	p := newProvider(t, srv.URL, auth)
	if _, err := p.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gotAccount != "acct-jwt" {
		t.Errorf("ChatGPT-Account-Id = %q, want acct-jwt", gotAccount)
	}
}

func TestFetchNeverRewritesAuthFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000}}}`))
	}))
	defer srv.Close()
	p := newProvider(t, srv.URL, chatgptAuth("acct-123"))
	path := p.AuthPath()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth: %v", err)
	}
	if _, err := p.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth after: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("Fetch rewrote the Codex auth file")
	}
}

func TestAuthPath(t *testing.T) {
	if got := authPath("/home/u", ""); got != filepath.Join("/home/u", ".codex", "auth.json") {
		t.Errorf("default path = %q", got)
	}
	if got := authPath("/home/u", "/opt/codex"); got != filepath.Join("/opt/codex", "auth.json") {
		t.Errorf("CODEX_HOME path = %q", got)
	}
}

func TestWindowName(t *testing.T) {
	cases := []struct {
		seconds *int64
		want    string
	}{
		{ptr(int64(18000)), "5h"},
		{ptr(int64(604800)), "week"},
		{ptr(int64(86400)), "1d"},
		{ptr(int64(7200)), "2h"},
		{ptr(int64(90)), "1m"},
		{nil, "primary"},
	}
	for _, tc := range cases {
		if got := windowName(tc.seconds, "primary"); got != tc.want {
			t.Errorf("windowName(%v) = %q, want %q", tc.seconds, got, tc.want)
		}
	}
}

func TestTargetURL(t *testing.T) {
	if Target != "https://chatgpt.com/backend-api/wham/usage" {
		t.Errorf("Target = %q", Target)
	}
}

// The backend may send only the relative reset; the window still needs an
// absolute reset instant derived from the fetch time.
func TestWindowResetAfterSeconds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":7,"limit_window_seconds":18000,"reset_after_seconds":3600}}}`))
	}))
	defer srv.Close()
	p := newProvider(t, srv.URL, chatgptAuth("acct-123"))
	snap, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(snap.Windows) != 1 {
		t.Fatalf("windows = %+v", snap.Windows)
	}
	if want := snap.FetchedAt.Add(time.Hour); !snap.Windows[0].ResetsAt.Equal(want) {
		t.Errorf("ResetsAt = %s, want %s", snap.Windows[0].ResetsAt, want)
	}
}

func TestCredentialIsRedacted(t *testing.T) {
	c := credential{accessToken: testAccessToken, accountID: "acct-123"}
	for _, got := range []string{
		fmt.Sprintf("%v", c),
		fmt.Sprintf("%#v", c),
		fmt.Sprintf("%s", c),
	} {
		if strings.Contains(got, testAccessToken) || strings.Contains(got, "acct-123") {
			t.Fatalf("credential rendered a secret: %q", got)
		}
		if got != "[redacted]" {
			t.Errorf("credential rendered as %q, want [redacted]", got)
		}
	}
}

func ptr[T any](v T) *T { return &v }
