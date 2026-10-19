package qoder

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

const (
	testPassword = "test-keychain-password"
	testToken    = "test-token-not-a-secret"
)

func encryptAuth(t *testing.T, password, token, expires string) []byte {
	t.Helper()
	plain, err := json.Marshal(map[string]any{
		"schemaVersion": 1,
		"token":         token,
		"refreshToken":  "test-refresh",
		"expiresAt":     expires,
		"user":          map[string]any{"id": "user-id", "name": "Tester"},
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := pbkdf2.Key(sha1.New, password, []byte("saltysalt"), 1003, 16)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	for i := 0; i < pad; i++ {
		plain = append(plain, byte(pad))
	}
	ciphertext := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, []byte("                ")).CryptBlocks(ciphertext, plain)
	return append([]byte("v10"), ciphertext...)
}

func writeAuth(t *testing.T, password, token, expires string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.v1.dat")
	if err := os.WriteFile(path, encryptAuth(t, password, token, expires), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "qoder_usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFetch(t *testing.T) {
	authPath := writeAuth(t, testPassword, testToken, "2099-01-01T00:00:00Z")
	fixture := readFixture(t)
	var headers http.Header
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		headers = r.Header.Clone()
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	p := New(Options{
		AuthPath:  authPath,
		BaseURL:   srv.URL,
		Transport: srv.Client(),
		Password:  testPassword,
		Now:       func() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) },
	})
	if !p.Detect(context.Background()) {
		t.Fatal("Detect = false")
	}
	snap, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet || path != usagePath {
		t.Fatalf("request = %s %s", method, path)
	}
	for key, want := range map[string]string{
		"Authorization":   "Bearer " + testToken,
		"Accept":          "application/json",
		"Cosy-ClientType": "10",
		"User-Agent":      UserAgent,
	} {
		if got := headers.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if snap.Provider != ProviderID || snap.Plan != "personal_professional_trial" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if len(snap.Windows) != 2 {
		t.Fatalf("windows = %+v", snap.Windows)
	}
	wantExpiry := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	for i, w := range snap.Windows {
		if !w.Balance || w.UsedPercent != 0 || !w.ExpiresAt.Equal(wantExpiry) {
			t.Fatalf("window[%d] = %+v", i, w)
		}
	}
	if w := snap.Windows[0]; w.Name != "Credits" || w.Limit != 300 || w.Used != 0 || w.Remaining != 300 {
		t.Fatalf("user quota = %+v", w)
	}
	if w := snap.Windows[1]; w.Name != "Add-on credits" || w.Limit != 100 || w.Remaining != 100 {
		t.Fatalf("add-on quota = %+v", w)
	}
}

func TestWrongPassword(t *testing.T) {
	authPath := writeAuth(t, testPassword, testToken, "2099-01-01T00:00:00Z")
	p := New(Options{AuthPath: authPath, Password: "wrong", BaseURL: "https://example.invalid"})
	_, err := p.Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonSourceUnreadable) {
		t.Fatalf("err = %v", err)
	}
}

func TestMissingCredential(t *testing.T) {
	p := New(Options{AuthPath: filepath.Join(t.TempDir(), "missing.dat")})
	if p.Detect(context.Background()) {
		t.Fatal("Detect = true without a file")
	}
	_, err := p.Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonNoCredentials) {
		t.Fatalf("err = %v", err)
	}
}

func TestEnterpriseHasNoData(t *testing.T) {
	authPath := writeAuth(t, testPassword, testToken, "2099-01-01T00:00:00Z")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"displayMode":"enterprise"}`))
	}))
	defer srv.Close()
	p := New(Options{AuthPath: authPath, BaseURL: srv.URL, Transport: srv.Client(), Password: testPassword})
	_, err := p.Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonNoData) {
		t.Fatalf("err = %v", err)
	}
}

func TestNoWindows(t *testing.T) {
	authPath := writeAuth(t, testPassword, testToken, "2099-01-01T00:00:00Z")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"displayMode":"qoder","qoderUsage":{"userType":"free","userQuota":{"total":0}}}`))
	}))
	defer srv.Close()
	p := New(Options{AuthPath: authPath, BaseURL: srv.URL, Transport: srv.Client(), Password: testPassword})
	_, err := p.Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonNoWindows) {
		t.Fatalf("err = %v", err)
	}
}

func TestUnauthorized(t *testing.T) {
	authPath := writeAuth(t, testPassword, testToken, "2099-01-01T00:00:00Z")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	p := New(Options{AuthPath: authPath, BaseURL: srv.URL, Transport: srv.Client(), Password: testPassword})
	_, err := p.Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonUnauthorized) {
		t.Fatalf("err = %v", err)
	}
}

func TestDetectDoesNotReadKeychain(t *testing.T) {
	authPath := writeAuth(t, testPassword, testToken, "2099-01-01T00:00:00Z")
	// A non-darwin GOOS would make Fetch report unsupported if it tried the
	// Keychain; Detect only stats the file.
	p := New(Options{AuthPath: authPath, GOOS: "linux"})
	if !p.Detect(context.Background()) {
		t.Fatal("Detect = false")
	}
}

func TestDefaultAuthPath(t *testing.T) {
	got := defaultAuthPath("darwin", "/Users/test", func(string) string { return "" })
	want := filepath.Join("/Users/test", "Library", "Application Support", "com.qodercn.app.stable", "auth.v1.dat")
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}
