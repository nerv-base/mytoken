package trae

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

const (
	testToken  = "test-token-not-a-secret"
	testUID    = "123456789012345"
	testDevice = "1234567890123456"
)

// encodeEnvelope is the inverse of clientDecode, for building fixtures.
func encodeEnvelope(t *testing.T, env any) string {
	t.Helper()
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha512.Sum512(body)
	plain := append(sum[:], body...)
	if n := len(plain) % aes.BlockSize; n != 0 {
		pad := make([]byte, aes.BlockSize-n)
		for i := range pad {
			pad[i] = byte(aes.BlockSize - n)
		}
		plain = append(plain, pad...)
	}

	const saltID = "746305100000"
	salt := codecSalts[saltID]
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	inner := sha512.Sum512(random)
	outer := sha512.New()
	outer.Write(inner[:])
	outer.Write(salt[:])
	key := outer.Sum(nil)
	block, err := aes.NewCipher(key[:16])
	if err != nil {
		t.Fatal(err)
	}
	ciphertext := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, key[16:32]).CryptBlocks(ciphertext, plain)
	header, _ := hex.DecodeString(saltID)
	buf := append(header, random...)
	buf = append(buf, ciphertext...)
	return base64.StdEncoding.EncodeToString(buf)
}

func writeStorage(t *testing.T, global bool, expires string) string {
	t.Helper()
	auth := map[string]any{
		"token":        testToken,
		"refreshToken": "test-refresh",
		"userId":       testUID,
		"expiredAt":    expires,
		"host":         "https://api.trae.cn",
	}
	device := map[string]any{"userId": testUID, "privateKeyPEM": "not-a-real-key"}
	doc := map[string]any{
		authKey:                            encodeEnvelope(t, auth),
		deviceKeyPrefix + testDevice:       encodeEnvelope(t, device),
		"iCubeAuthInfo://usertag":          "plain",
		"iCubeServerData://icube.cloudide": "{}",
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "storage.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testProvider(t *testing.T, serverURL string, storage string) *Provider {
	t.Helper()
	return New(Options{
		StoragePaths: []string{storage},
		MagpiePath:   filepath.Join(t.TempDir(), "plugin-auth.json"),
		BaseURL:      serverURL,
		Now:          func() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) },
	})
}

func TestCNFetch(t *testing.T) {
	storage := writeStorage(t, false, "2099-01-01T00:00:00Z")
	fixture := readFixture(t, "trae_usage.json")
	var headers http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		if r.Method != http.MethodPost || r.URL.Path != cnUsagePath {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	p := testProvider(t, srv.URL, storage)
	p.transport = srv.Client()
	if !p.Detect(context.Background()) {
		t.Fatal("Detect = false")
	}
	snap, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	for key, want := range map[string]string{
		"Authorization":      "Cloud-IDE-JWT " + testToken,
		"X-Cloudide-Token":   testToken,
		"x-ide-token":        testToken,
		"x-uid":              testUID,
		"x-app-id":           appID,
		"x-device-id":        testDevice,
		"x-app-version":      "0.1.69",
		"x-app-version-code": "20260917",
		"x-ide-version":      "3.3.65",
		"x-ide-version-code": "20260401",
		"x-ide-version-type": "stable",
		"x-device-type":      "windows",
		"x-system-type":      "Windows",
		"User-Agent":         UserAgent,
	} {
		if got := headers.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	if snap.Provider != ProviderID || snap.Plan != "Credits" || snap.Source != plan.SourceAPI {
		t.Fatalf("snapshot = %+v", snap)
	}
	if len(snap.Windows) != 1 {
		t.Fatalf("windows = %+v", snap.Windows)
	}
	w := snap.Windows[0]
	if w.Name != "Credits" || !w.Balance || w.Unit != "credits" {
		t.Fatalf("window = %+v", w)
	}
	if w.Limit != 4700 || math.Abs(w.Used-20.29) > 1e-9 || math.Abs(w.Remaining-4679.71) > 1e-9 {
		t.Fatalf("window numbers = %+v", w)
	}
	wantExpiry := time.Date(2026, 10, 31, 15, 59, 59, 0, time.UTC)
	if !w.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("expiresAt = %v, want %v", w.ExpiresAt, wantExpiry)
	}
}

func TestCNBusinessErrors(t *testing.T) {
	storage := writeStorage(t, false, "2099-01-01T00:00:00Z")
	cases := []struct {
		name string
		body string
		want plan.Reason
	}{
		{"not logged in", `{"code":1001}`, plan.ReasonUnauthorized},
		{"unknown code", `{"code":4321}`, plan.ReasonHTTPStatus},
		{"no windows", `{"code":0,"user_entitlement_pack_list":[]}`, plan.ReasonNoWindows},
		{"malformed", `{`, plan.ReasonMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			p := testProvider(t, srv.URL, storage)
			p.transport = srv.Client()
			_, err := p.Fetch(context.Background())
			if !plan.IsReason(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestCNUnlimited(t *testing.T) {
	storage := writeStorage(t, false, "2099-01-01T00:00:00Z")
	body := `{"code":0,"is_credits_billing":true,"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":-1}}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	p := testProvider(t, srv.URL, storage)
	p.transport = srv.Client()
	snap, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	w := snap.Windows[0]
	if !w.Balance || w.Limit != 0 || w.Remaining != 0 || w.UsedPercent != 0 {
		t.Fatalf("unlimited window = %+v", w)
	}
}

func TestGlobalFetch(t *testing.T) {
	storage := writeStorage(t, true, "2099-01-01T00:00:00Z")
	fixture := readFixture(t, "trae_global_usage.json")
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	p := NewGlobal(Options{
		StoragePaths: []string{storage},
		MagpiePath:   filepath.Join(t.TempDir(), "plugin-auth.json"),
		BaseURL:      srv.URL,
		Transport:    srv.Client(),
	})
	snap, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if path != globalUsagePath {
		t.Fatalf("path = %q", path)
	}
	if snap.Provider != GlobalProviderID || snap.Plan != "Pro" {
		t.Fatalf("snapshot = %+v", snap)
	}
	w := snap.Windows[0]
	if w.Name != "Dollar Usage" || !w.Balance || w.Unit != "usd" {
		t.Fatalf("window = %+v", w)
	}
	if w.Limit != 25 || w.Used != 4.75 || w.Remaining != 20.25 {
		t.Fatalf("numbers = %+v", w)
	}
}

func TestExpiredTokenDoesNotCallNetwork(t *testing.T) {
	storage := writeStorage(t, false, "2020-01-01T00:00:00Z")
	p := testProvider(t, "https://example.invalid", storage)
	_, err := p.Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonTokenExpired) {
		t.Fatalf("err = %v", err)
	}
}

func TestMagpieFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugin-auth.json")
	doc := map[string]any{
		"trae-cn": map[string]any{
			"access":   testToken,
			"uid":      testUID,
			"deviceId": testDevice,
			"expires":  4102444800000,
		},
	}
	b, _ := json.Marshal(doc)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := readFixture(t, "trae_usage.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()
	p := New(Options{
		StoragePaths: []string{filepath.Join(t.TempDir(), "missing.json")},
		MagpiePath:   path,
		BaseURL:      srv.URL,
		Transport:    srv.Client(),
	})
	if !p.Detect(context.Background()) {
		t.Fatal("Detect = false")
	}
	if _, err := p.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type countingTransport struct{ calls int }

func (c *countingTransport) Do(*http.Request) (*http.Response, error) {
	c.calls++
	return nil, errors.New("network disabled")
}

func TestDetectDoesNotUseNetwork(t *testing.T) {
	storage := writeStorage(t, false, "2099-01-01T00:00:00Z")
	tr := &countingTransport{}
	p := New(Options{
		StoragePaths: []string{storage},
		MagpiePath:   filepath.Join(t.TempDir(), "plugin-auth.json"),
		Transport:    tr,
	})
	if !p.Detect(context.Background()) {
		t.Fatal("Detect = false")
	}
	if tr.calls != 0 {
		t.Fatalf("Detect made %d network calls", tr.calls)
	}
}

func TestDefaultStoragePaths(t *testing.T) {
	home := "/Users/test"
	paths := defaultStoragePaths(false, "darwin", home, func(string) string { return "" })
	want := filepath.Join(home, "Library", "Application Support", "Trae CN", "User", "globalStorage", "storage.json")
	if len(paths) != 1 || paths[0] != want {
		t.Fatalf("CN paths = %v", paths)
	}
	paths = defaultStoragePaths(true, "darwin", home, func(string) string { return "" })
	want = filepath.Join(home, "Library", "Application Support", "Trae", "User", "globalStorage", "storage.json")
	if len(paths) != 1 || paths[0] != want {
		t.Fatalf("global paths = %v", paths)
	}
}
