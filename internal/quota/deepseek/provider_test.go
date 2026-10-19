package deepseek

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

const testKey = "sk-test-deepseek-key"

var testNow = time.Date(2026, 10, 20, 0, 30, 0, 0, time.UTC)

func testEnv(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func newTestProvider(env map[string]string, home, credentialPath, baseURL string) *Provider {
	return New(Options{
		BaseURL:        baseURL,
		CredentialPath: credentialPath,
		Getenv:         testEnv(env),
		Home:           home,
		Now:            func() time.Time { return testNow },
	})
}

func balanceServer(t *testing.T, wantKey, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q", r.Method)
		}
		if r.URL.Path != balancePath {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantKey {
			t.Errorf("authorization = %q", got)
		}
		io.WriteString(w, body)
	}))
}

func writeCredentials(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProviderIdentity(t *testing.T) {
	p := New(Options{})
	if p.ID() != "deepseek" || p.DisplayName() != "DeepSeek" {
		t.Errorf("id = %q, name = %q", p.ID(), p.DisplayName())
	}
}

func TestFetchBalanceCNY(t *testing.T) {
	srv := balanceServer(t, testKey, `{"is_available":true,"balance_infos":[
		{"currency":"CNY","total_balance":"36.57","granted_balance":"1.25","topped_up_balance":"35.31"}
	]}`)
	defer srv.Close()

	p := newTestProvider(map[string]string{apiKeyEnv: testKey}, t.TempDir(), "", srv.URL)
	if !p.Detect(context.Background()) {
		t.Fatal("Detect = false, want true")
	}
	snap, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.Provider != ProviderID || snap.Source != plan.SourceAPI || !snap.FetchedAt.Equal(testNow) {
		t.Errorf("snapshot = %+v", snap)
	}
	if len(snap.Windows) != 1 {
		t.Fatalf("windows = %+v", snap.Windows)
	}
	got := snap.Windows[0]
	if got.Name != "balance" || got.Unit != "cny" || !got.Balance || got.Limit != 0 {
		t.Errorf("window = %+v", got)
	}
	if got.Remaining != 36.57 || got.UsedPercent != 0 {
		t.Errorf("remaining = %v, used percent = %v", got.Remaining, got.UsedPercent)
	}
}

func TestFetchBalanceTwoCurrencies(t *testing.T) {
	srv := balanceServer(t, testKey, `{"is_available":true,"balance_infos":[
		{"currency":"CNY","total_balance":"10.50","granted_balance":"0","topped_up_balance":"10.50"},
		{"currency":"USD","total_balance":"2.25","granted_balance":"0.25","topped_up_balance":"2.00"}
	]}`)
	defer srv.Close()

	snap, err := newTestProvider(map[string]string{apiKeyEnv: testKey}, t.TempDir(), "", srv.URL).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(snap.Windows) != 2 {
		t.Fatalf("windows = %+v", snap.Windows)
	}
	if got := snap.Windows[0]; got.Name != "Balance (CNY)" || got.Unit != "cny" || got.Remaining != 10.5 {
		t.Errorf("CNY window = %+v", got)
	}
	if got := snap.Windows[1]; got.Name != "Balance (USD)" || got.Unit != "usd" || got.Remaining != 2.25 {
		t.Errorf("USD window = %+v", got)
	}
}

func TestFetchUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := newTestProvider(map[string]string{apiKeyEnv: testKey}, t.TempDir(), "", srv.URL).Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonUnauthorized) {
		t.Fatalf("Fetch error = %v, want unauthorized", err)
	}
}

func TestFetchBalanceExhausted(t *testing.T) {
	srv := balanceServer(t, testKey, `{"is_available":false,"balance_infos":[
		{"currency":"CNY","total_balance":"0.00","granted_balance":"0","topped_up_balance":"0"}
	]}`)
	defer srv.Close()

	_, err := newTestProvider(map[string]string{apiKeyEnv: testKey}, t.TempDir(), "", srv.URL).Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonBalanceExhausted) {
		t.Fatalf("Fetch error = %v, want balance exhausted", err)
	}
	if !strings.Contains(err.Error(), "balance is exhausted") {
		t.Errorf("error text = %q", err.Error())
	}
}

func TestFetchMalformed(t *testing.T) {
	for name, body := range map[string]string{
		"missing availability": `{"balance_infos":[]}`,
		"bad total":            `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"nope"}]}`,
		"missing currency":     `{"is_available":true,"balance_infos":[{"total_balance":"1.00"}]}`,
		"no windows":           `{"is_available":true,"balance_infos":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := balanceServer(t, testKey, body)
			defer srv.Close()
			_, err := newTestProvider(map[string]string{apiKeyEnv: testKey}, t.TempDir(), "", srv.URL).Fetch(context.Background())
			if name == "no windows" {
				if !plan.IsReason(err, plan.ReasonNoWindows) {
					t.Fatalf("Fetch error = %v, want no windows", err)
				}
				return
			}
			if !plan.IsReason(err, plan.ReasonMalformed) {
				t.Fatalf("Fetch error = %v, want malformed", err)
			}
		})
	}
}

func TestStoredKeyPrecedesEnvironment(t *testing.T) {
	home := t.TempDir()
	writeCredentials(t, filepath.Join(home, ".dsh", ".credentials.yaml"), `version: 1
refs:
  DEEPSEEK_API_KEY: sk-stored-key
records:
  deepseek-account-platform/default:
    token: account-grant-token
    issuer: https://platform.deepseek.com
`)
	srv := balanceServer(t, "sk-stored-key", `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"1.00"}]}`)
	defer srv.Close()

	p := newTestProvider(map[string]string{apiKeyEnv: "sk-env-key"}, home, "", srv.URL)
	if !p.Detect(context.Background()) {
		t.Fatal("Detect = false, want true")
	}
	if _, err := p.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
}

func TestDSHHomeOverride(t *testing.T) {
	home := t.TempDir()
	dsh := t.TempDir()
	writeCredentials(t, filepath.Join(dsh, ".credentials.yaml"), "refs:\n  DEEPSEEK_API_KEY: sk-dsh-home\n")
	srv := balanceServer(t, "sk-dsh-home", `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"2.00"}]}`)
	defer srv.Close()

	p := newTestProvider(map[string]string{"DSH_HOME": dsh}, home, "", srv.URL)
	if _, err := p.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
}

func TestCredentialPathOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom-credentials.yaml")
	writeCredentials(t, path, "refs:\n  DEEPSEEK_API_KEY: \"sk-custom\" # comment\n")
	srv := balanceServer(t, "sk-custom", `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"3.00"}]}`)
	defer srv.Close()

	p := newTestProvider(map[string]string{"DSH_HOME": t.TempDir()}, t.TempDir(), path, srv.URL)
	if _, err := p.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
}

func TestEnvironmentFallback(t *testing.T) {
	srv := balanceServer(t, "sk-env-key", `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"4.00"}]}`)
	defer srv.Close()

	p := newTestProvider(map[string]string{apiKeyEnv: "sk-env-key"}, t.TempDir(), "", srv.URL)
	if !p.Detect(context.Background()) {
		t.Fatal("Detect = false, want true")
	}
	if _, err := p.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
}

func TestNoCredentials(t *testing.T) {
	p := newTestProvider(nil, t.TempDir(), "", "http://127.0.0.1:1")
	if p.Detect(context.Background()) {
		t.Fatal("Detect = true, want false")
	}
	if _, err := p.Fetch(context.Background()); !plan.IsReason(err, plan.ReasonNoCredentials) {
		t.Fatalf("Fetch error = %v, want no credentials", err)
	}
}

func TestRefsFromCredentials(t *testing.T) {
	raw := []byte(`version: 1
refs:
  DEEPSEEK_API_KEY: "sk-quoted" # comment
  OTHER_KEY: plain
records:
  deepseek-account-platform/default:
    token: not-a-ref
    issuer: https://platform.deepseek.com
`)
	refs := refsFromCredentials(raw)
	if got := refs[apiKeyRef]; got != "sk-quoted" {
		t.Errorf("stored key = %q", got)
	}
	if got := refs["OTHER_KEY"]; got != "plain" {
		t.Errorf("other key = %q", got)
	}
	if _, ok := refs["token"]; ok {
		t.Errorf("record field leaked into refs: %+v", refs)
	}
}

func TestCleanKey(t *testing.T) {
	for in, want := range map[string]string{
		"  sk-a  ":          "sk-a",
		`"sk-a"`:            "sk-a",
		"'sk-a'":            "sk-a",
		"Bearer sk-a":       "sk-a",
		"sk-a sk-b":         "",
		"sk-a\nsecond-line": "",
		"":                  "",
	} {
		if got := cleanKey(in); got != want {
			t.Errorf("cleanKey(%q) = %q, want %q", in, got, want)
		}
	}
}
