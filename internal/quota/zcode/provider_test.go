package zcode

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

const testSecret = "zcode-credential-fallback:win32:C:\\Users\\tester:tester"

// seal builds an enc:v1 value exactly as the ZCode app does.
func seal(t *testing.T, secret, plain string) string {
	t.Helper()
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("cipher.NewGCM: %v", err)
	}
	nonce := make([]byte, nonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	sealed := gcm.Seal(nil, nonce, []byte(plain), nil)
	text, tag := sealed[:len(sealed)-tagBytes], sealed[len(sealed)-tagBytes:]
	enc := base64.RawURLEncoding
	return cipherPrefix + enc.EncodeToString(nonce) + "." + enc.EncodeToString(tag) + "." + enc.EncodeToString(text)
}

// writeCredentials stores a sealed token and a device id under dir.
func writeCredentials(t *testing.T, dir, secret, token, deviceMid string) {
	t.Helper()
	doc := map[string]string{
		tokenKey:                seal(t, secret, token),
		"oauth:active_provider": seal(t, secret, "zai"),
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal credentials: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, credentialFileName), raw, 0o600); err != nil {
		t.Fatalf("WriteFile credentials: %v", err)
	}
	if deviceMid != "" {
		raw, err := json.Marshal(map[string]any{"version": 2, "deviceMid": deviceMid})
		if err != nil {
			t.Fatalf("Marshal onboarding: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, deviceMidFile), raw, 0o600); err != nil {
			t.Fatalf("WriteFile onboarding: %v", err)
		}
	}
}

// testProvider points a Provider at a temp config dir and a test server.
func testProvider(t *testing.T, baseURL, dir string) *Provider {
	t.Helper()
	return New(Options{
		BaseURL:   baseURL,
		ConfigDir: dir,
		Now:       func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) },
		Getenv:    func(string) string { return "" },
		Home:      `C:\Users\tester`,
		GOOS:      "windows",
	})
}

const balanceBody = `{
  "code": 0,
  "msg": "",
  "data": {
    "server_time": 1792469265,
    "plans": [
      {"user_plan_id": "upl_1", "plan_id": "zcode-v3-start-plan-0817", "name": "ZCode Start Plan", "status": "active"}
    ],
    "balances": [
      {"entitlement_id": "ent_2_0817_glm_5p3", "show_name": "GLM-5.3", "unit_type": "token",
       "meter": "model_usage", "total_units": 3000000, "used_units": 120000,
       "remaining_units": 2880000, "period_start": 1792425600, "period_end": 1792511999,
       "expires_at": 1792511999},
      {"entitlement_id": "ent_2_0817_glm_5p3f", "show_name": "GLM-5.3-Flash", "unit_type": "token",
       "total_units": 5000000, "used_units": 0, "remaining_units": 5000000,
       "period_end": 1792511999, "expires_at": 1792511999}
    ]
  }
}`

func TestDetectAndFetch(t *testing.T) {
	dir := t.TempDir()
	writeCredentials(t, dir, testSecret, "test-jwt", "1a4666b6-608a-42e8-8f35-3afdde6d64b3")

	var gotAuth, gotDevice, gotAgent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotDevice = r.Header.Get("X-Device-Mid")
		gotAgent = r.Header.Get("User-Agent")
		if r.URL.Path != balancePath {
			t.Errorf("path = %q, want %q", r.URL.Path, balancePath)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(balanceBody))
	}))
	defer server.Close()

	p := testProvider(t, server.URL, dir)
	ctx := context.Background()
	if !p.Detect(ctx) {
		t.Fatal("Detect = false, want true")
	}
	if got := p.ID(); got != ProviderID {
		t.Errorf("ID = %q, want %q", got, ProviderID)
	}
	if got := p.DisplayName(); got != "ZCode" {
		t.Errorf("DisplayName = %q, want %q", got, "ZCode")
	}
	snap, err := p.Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gotAuth != "Bearer test-jwt" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer test-jwt")
	}
	if gotDevice != "1a4666b6-608a-42e8-8f35-3afdde6d64b3" {
		t.Errorf("X-Device-Mid = %q", gotDevice)
	}
	if gotAgent != userAgent {
		t.Errorf("User-Agent = %q, want %q", gotAgent, userAgent)
	}
	if snap.Provider != ProviderID {
		t.Errorf("Provider = %q, want %q", snap.Provider, ProviderID)
	}
	if snap.Source != plan.SourceAPI {
		t.Errorf("Source = %q, want %q", snap.Source, plan.SourceAPI)
	}
	if snap.Plan != "ZCode Start Plan" {
		t.Errorf("Plan = %q", snap.Plan)
	}
	if len(snap.Windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(snap.Windows))
	}
	first := snap.Windows[0]
	if first.Name != "GLM-5.3" || first.Unit != "tokens" {
		t.Errorf("window[0] = %+v", first)
	}
	if first.Limit != 3000000 || first.Used != 120000 {
		t.Errorf("window[0] limit/used = %v/%v", first.Limit, first.Used)
	}
	if first.Balance {
		t.Error("window[0].Balance = true, want a capped percentage window")
	}
	if first.UsedPercent < 3.9 || first.UsedPercent > 4.1 {
		t.Errorf("window[0].UsedPercent = %v, want ~4", first.UsedPercent)
	}
	if want := time.Unix(1792511999, 0).UTC(); !first.ResetsAt.Equal(want) {
		t.Errorf("window[0].ResetsAt = %v, want %v", first.ResetsAt, want)
	}
	if err := snap.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestDetectWithoutCredentials(t *testing.T) {
	p := testProvider(t, "https://example.invalid", t.TempDir())
	if p.Detect(context.Background()) {
		t.Error("Detect = true, want false")
	}
	_, err := p.Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonSourceMissing) {
		t.Errorf("Fetch error = %v, want reason %q", err, plan.ReasonSourceMissing)
	}
}

func TestFetchWrongSecret(t *testing.T) {
	dir := t.TempDir()
	writeCredentials(t, dir, "some-other-secret", "test-jwt", "")
	p := testProvider(t, "https://example.invalid", dir)
	if p.Detect(context.Background()) {
		t.Error("Detect = true, want false for an unreadable token")
	}
	_, err := p.Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonNoCredentials) {
		t.Errorf("Fetch error = %v, want reason %q", err, plan.ReasonNoCredentials)
	}
}

func TestFetchStatusErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		reason plan.Reason
	}{
		{"unauthorized", http.StatusUnauthorized, plan.ReasonUnauthorized},
		{"forbidden", http.StatusForbidden, plan.ReasonForbidden},
		{"rate limited", http.StatusTooManyRequests, plan.ReasonRateLimited},
		{"server error", http.StatusInternalServerError, plan.ReasonHTTPStatus},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeCredentials(t, dir, testSecret, "test-jwt", "")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			p := testProvider(t, server.URL, dir)
			_, err := p.Fetch(context.Background())
			if !plan.IsReason(err, tc.reason) {
				t.Errorf("error = %v, want reason %q", err, tc.reason)
			}
		})
	}
}

func TestFetchPayloadErrors(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		reason plan.Reason
	}{
		{"malformed", `{"code":0,"data":`, plan.ReasonMalformed},
		{"no data", `{"code":0,"msg":"","data":null}`, plan.ReasonNoData},
		{"no windows", `{"code":0,"data":{"balances":[]}}`, plan.ReasonNoWindows},
		{"zero grant", `{"code":0,"data":{"balances":[{"show_name":"GLM-5.3","total_units":0}]}}`, plan.ReasonNoWindows},
		{"business", `{"code":3001,"msg":"parameter error"}`, plan.ReasonHTTPStatus},
		{"business unauthorized", `{"code":401,"msg":"token expired"}`, plan.ReasonUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeCredentials(t, dir, testSecret, "test-jwt", "")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			p := testProvider(t, server.URL, dir)
			_, err := p.Fetch(context.Background())
			if !plan.IsReason(err, tc.reason) {
				t.Errorf("error = %v, want reason %q", err, tc.reason)
			}
		})
	}
}

func TestDeviceMidFallback(t *testing.T) {
	dir := t.TempDir()
	writeCredentials(t, dir, testSecret, "test-jwt", "")
	seen := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("X-Device-Mid")
		_, _ = w.Write([]byte(balanceBody))
	}))
	defer server.Close()
	p := testProvider(t, server.URL, dir)
	if _, err := p.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := <-seen; got != zeroDeviceMid {
		t.Errorf("X-Device-Mid = %q, want %q", got, zeroDeviceMid)
	}
}

func TestAppVersionQuery(t *testing.T) {
	dir := t.TempDir()
	writeCredentials(t, dir, testSecret, "test-jwt", "")
	var gotVersion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.URL.Query().Get("app_version")
		_, _ = w.Write([]byte(balanceBody))
	}))
	defer server.Close()
	p := testProvider(t, server.URL, dir)
	p.version = "3.15.3"
	if _, err := p.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gotVersion != "3.15.3" {
		t.Errorf("app_version = %q, want %q", gotVersion, "3.15.3")
	}
}

func TestCandidateSecrets(t *testing.T) {
	env := map[string]string{"USERNAME": `ASUKA\tester`}
	getenv := func(k string) string { return env[k] }
	got := candidateSecrets(`C:\Users\tester`, "windows", getenv)
	if len(got) == 0 {
		t.Fatal("candidateSecrets returned nothing")
	}
	if want := testSecret; got[0] != want {
		t.Errorf("first secret = %q, want %q", got[0], want)
	}
	env = map[string]string{secretEnv: "override"}
	got = candidateSecrets(`C:\Users\tester`, "windows", getenv)
	if len(got) != 1 || got[0] != "override" {
		t.Errorf("override = %v, want [override]", got)
	}
}

func TestNodePlatform(t *testing.T) {
	cases := map[string]string{"windows": "win32", "": "win32", "darwin": "darwin", "linux": "linux"}
	for goos, want := range cases {
		if got := nodePlatform(goos); got != want {
			t.Errorf("nodePlatform(%q) = %q, want %q", goos, got, want)
		}
	}
}

func TestDefaultConfigDir(t *testing.T) {
	getenv := func(k string) string {
		if k == "USERPROFILE" {
			return `C:\Users\tester`
		}
		return ""
	}
	got := defaultConfigDir("", getenv)
	if want := filepath.Join(`C:\Users\tester`, dataRootName, configDirName); got != want {
		t.Errorf("defaultConfigDir = %q, want %q", got, want)
	}
	if !strings.HasSuffix(got, filepath.Join(dataRootName, configDirName)) {
		t.Errorf("defaultConfigDir = %q, want a %s suffix", got, filepath.Join(dataRootName, configDirName))
	}
}
