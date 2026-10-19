package claudequota

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

const testToken = "sk-ant-oat-fixture-token"

func writeCredential(t *testing.T, dir, token string, expiresAt int64) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":%d,"scopes":["user:profile"]}}`, token, expiresAt)
	path := filepath.Join(dir, CredentialsFileName)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func nowFixture() time.Time {
	return time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
}

func TestProviderFetchFromCredentialFile(t *testing.T) {
	dir := t.TempDir()
	writeCredential(t, dir, testToken, nowFixture().Add(2*time.Hour).UnixMilli())

	var gotAuth, gotBeta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBeta = r.Header.Get("anthropic-beta")
		if r.URL.Path != "/api/oauth/usage" {
			t.Errorf("path = %q", r.URL.Path)
		}
		fmt.Fprint(w, `{
			"five_hour": {"utilization": 12.5, "resets_at": "2026-10-19T14:00:00Z"},
			"seven_day": {"utilization": 40, "resets_at": 1792500000000},
			"seven_day_sonnet": {"utilization": 3}
		}`)
	}))
	defer srv.Close()

	p := New(Options{
		ConfigDirs: []string{dir},
		GOOS:       "linux",
		Transport:  plan.DefaultTransport(),
		Endpoint:   srv.URL + "/api/oauth/usage",
		Now:        nowFixture,
	})
	if !p.Detect(context.Background()) {
		t.Fatal("credential not detected")
	}
	snap, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Provider != ProviderID || snap.Source != plan.SourceAPI || !snap.FetchedAt.Equal(nowFixture()) {
		t.Fatalf("snapshot = %+v", snap)
	}
	if len(snap.Windows) != 3 {
		t.Fatalf("windows = %+v", snap.Windows)
	}
	byName := map[string]plan.Window{}
	for _, w := range snap.Windows {
		byName[w.Name] = w
	}
	if w := byName["5h"]; w.UsedPercent != 12.5 || !w.ResetsAt.Equal(time.Date(2026, 10, 19, 14, 0, 0, 0, time.UTC)) {
		t.Fatalf("5h = %+v", w)
	}
	if w := byName["week"]; w.UsedPercent != 40 || !w.ResetsAt.Equal(time.UnixMilli(1792500000000).UTC()) {
		t.Fatalf("week = %+v", w)
	}
	if gotAuth != "Bearer "+testToken {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if gotBeta != OAuthBeta {
		t.Fatalf("beta = %q", gotBeta)
	}

	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), testToken) || strings.Contains(string(raw), "sk-ant") {
		t.Fatalf("snapshot leaks token: %s", raw)
	}
}

func TestProviderExpiredTokenMakesNoRequest(t *testing.T) {
	dir := t.TempDir()
	writeCredential(t, dir, testToken, nowFixture().Add(-time.Hour).UnixMilli())
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()

	p := New(Options{ConfigDirs: []string{dir}, GOOS: "linux", Endpoint: srv.URL, Now: nowFixture})
	if !p.Detect(context.Background()) {
		t.Fatal("expired credential should still be detected")
	}
	_, err := p.Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonTokenExpired) {
		t.Fatalf("err = %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("expired token was sent")
	}
}

func TestProviderUnauthorized(t *testing.T) {
	dir := t.TempDir()
	writeCredential(t, dir, testToken, 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	p := New(Options{ConfigDirs: []string{dir}, GOOS: "linux", Endpoint: srv.URL, Now: nowFixture})
	_, err := p.Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonUnauthorized) {
		t.Fatalf("err = %v", err)
	}
}

func TestProviderNoCredential(t *testing.T) {
	p := New(Options{ConfigDirs: []string{t.TempDir()}, GOOS: "linux", Now: nowFixture})
	if p.Detect(context.Background()) {
		t.Fatal("empty dir detected")
	}
	_, err := p.Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonNoCredentials) {
		t.Fatalf("err = %v", err)
	}
}

func TestProviderKeychainAndConfigDirEnv(t *testing.T) {
	dir := t.TempDir()
	writeCredential(t, dir, testToken, 0)
	env := map[string]string{"CLAUDE_CONFIG_DIR": dir}
	p := New(Options{
		GOOS: "linux",
		Getenv: func(k string) string {
			return env[k]
		},
		Home:     t.TempDir(),
		Endpoint: "http://127.0.0.1:1/never",
	})
	if len(p.Sources()) != 2 {
		t.Fatalf("sources = %+v", p.Sources())
	}
	if !p.Detect(context.Background()) {
		t.Fatal("CLAUDE_CONFIG_DIR credential not detected")
	}

	// The macOS keychain path is used when no file exists and the platform
	// provides a keychain runner.
	keyring := func(context.Context, string, string) ([]byte, error) {
		return []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q}}`, testToken)), nil
	}
	kp := New(Options{ConfigDirs: []string{t.TempDir()}, GOOS: "darwin", Keychain: keyring, Now: nowFixture})
	if len(kp.Sources()) != 2 {
		t.Fatalf("sources = %+v", kp.Sources())
	}
	if !kp.Detect(context.Background()) {
		t.Fatal("keychain credential not detected")
	}
}

func TestParseUsage(t *testing.T) {
	windows, err := parseUsage([]byte(`{
		"five_hour": 25,
		"seven_day_sonnet": {"percent": 5, "window_minutes": 10080},
		"extra_usage": {"is_enabled": true, "monthly_limit": 100, "used_credits": 10},
		"aaa_limit": {"used_percent": 1, "resets_at": "2026-10-20T00:00:00Z"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 3 {
		t.Fatalf("windows = %+v", windows)
	}
	if windows[0].Name != "5h" || windows[0].UsedPercent != 25 {
		t.Fatalf("first = %+v", windows[0])
	}
	if windows[1].Name != "week-sonnet" || windows[1].UsedPercent != 5 {
		t.Fatalf("second = %+v", windows[1])
	}
	if windows[2].Name != "aaa-limit" {
		t.Fatalf("unknown key = %+v", windows[2])
	}

	for _, body := range []string{`{}`, `{"extra_usage":{"monthly_limit":1}}`, `null`, `[]`} {
		if _, err := parseUsage([]byte(body)); !plan.IsReason(err, plan.ReasonNoWindows) {
			if !plan.IsReason(err, plan.ReasonMalformed) {
				t.Fatalf("%s: err = %v", body, err)
			}
		}
	}
}

func TestDecodeCredential(t *testing.T) {
	cred, err := decodeCredential([]byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":%d}}`, testToken, nowFixture().UnixMilli())))
	if err != nil {
		t.Fatal(err)
	}
	if cred.Reveal() != testToken || !cred.HasExpiry() || !cred.ExpiresAt().Equal(nowFixture()) {
		t.Fatalf("cred = %+v", cred)
	}
	if _, err := decodeCredential([]byte(`{"mcpOAuth":{"x":1}}`)); err == nil {
		t.Fatal("mcpOAuth-only file accepted")
	}
	bare, err := decodeCredential([]byte(testToken))
	if err != nil || bare.Reveal() != testToken {
		t.Fatalf("bare = %v, %v", bare, err)
	}
}

func TestCredentialRedaction(t *testing.T) {
	cred := CredentialExpiring(testToken, nowFixture())
	before := CredentialReveals()
	for _, got := range []string{
		fmt.Sprintf("%v", cred),
		fmt.Sprintf("%#v", cred),
		fmt.Sprintf("%s", cred),
	} {
		if strings.Contains(got, "sk-ant") {
			t.Fatalf("rendered token: %q", got)
		}
	}
	raw, err := json.Marshal(struct {
		Cred Credential `json:"cred"`
	}{cred})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-ant") {
		t.Fatalf("json leaks token: %s", raw)
	}
	if CredentialReveals() != before {
		t.Fatalf("formatting revealed the token")
	}
	if cred.Reveal() != testToken {
		t.Fatal("reveal broken")
	}
	if CredentialReveals() != before+1 {
		t.Fatal("reveal not counted")
	}
}
