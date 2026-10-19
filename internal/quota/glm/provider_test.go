package glm

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

const testKey = "test-glm-key"

func newProvider(t *testing.T, env map[string]string, home, region, baseURL string) *Provider {
	t.Helper()
	return New(Options{
		Region: region,
		Getenv: func(key string) string { return env[key] },
		Home:   home,
		Now: func() time.Time {
			return time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
		},
		BaseURL: baseURL,
	})
}

func TestFetchUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != quotaPath {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testKey {
			t.Errorf("authorization = %q", got)
		}
		io.WriteString(w, `{"code":200,"success":true,"data":{"planName":"Pro","limits":[
			{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":25,"nextResetTime":1785816000000},
			{"type":"TOKENS_LIMIT","unit":6,"number":1,"percentage":9,"nextResetTime":1786291200000},
			{"type":"TIME_LIMIT","unit":5,"number":1,"percentage":22}
		]}}`)
	}))
	defer srv.Close()

	p := newProvider(t, map[string]string{"Z_AI_API_KEY": testKey}, t.TempDir(), "", srv.URL)
	if !p.Detect(context.Background()) {
		t.Fatal("Detect = false, want true")
	}
	snap, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.Provider != ProviderID || snap.Plan != "Pro" || snap.Source != plan.SourceAPI {
		t.Errorf("snapshot = %+v", snap)
	}
	if len(snap.Windows) != 2 {
		t.Fatalf("windows = %+v", snap.Windows)
	}
	if got := snap.Windows[0]; got.Name != "5h" || got.UsedPercent != 25 {
		t.Errorf("5h window = %+v", got)
	}
	if got := snap.Windows[1]; got.Name != "week" || got.UsedPercent != 9 {
		t.Errorf("week window = %+v", got)
	}
	if want := time.UnixMilli(1785816000000).UTC(); !snap.Windows[0].ResetsAt.Equal(want) {
		t.Errorf("resets = %v, want %v", snap.Windows[0].ResetsAt, want)
	}
}

func TestFetchBigModelKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testKey {
			t.Errorf("authorization = %q", got)
		}
		io.WriteString(w, `{"code":200,"success":true,"data":{"level":"lite","limits":[
			{"type":"CREDIT_LIMIT","unit":3,"number":5,"usage":2000,"currentValue":100,"remaining":1900,"percentage":5}
		]}}`)
	}))
	defer srv.Close()

	p := newProvider(t, map[string]string{"BIGMODEL_API_KEY": testKey}, t.TempDir(), "", srv.URL)
	snap, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.Plan != "lite" || len(snap.Windows) != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	win := snap.Windows[0]
	if win.Name != "5h" || win.Unit != "credits" || win.Limit != 2000 || win.Used != 100 || win.UsedPercent != 5 {
		t.Errorf("window = %+v", win)
	}
}

func TestFetchKeyFile(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "zhipu")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "api_key"), []byte(testKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":{"planName":"Pro","limits":[{"type":"TOKENS_LIMIT","unit":1,"number":30,"percentage":50}]}}`)
	}))
	defer srv.Close()

	snap, err := newProvider(t, nil, home, RegionBigModel, srv.URL).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := snap.Windows[0]; got.Name != "month" || got.UsedPercent != 50 {
		t.Errorf("window = %+v", got)
	}
}

func TestFetchUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := newProvider(t, map[string]string{"Z_AI_API_KEY": testKey}, t.TempDir(), RegionGlobal, srv.URL).
		Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonUnauthorized) {
		t.Fatalf("err = %v, want unauthorized", err)
	}
}

func TestDetectWithoutKey(t *testing.T) {
	p := newProvider(t, nil, t.TempDir(), "", "")
	if p.Detect(context.Background()) {
		t.Fatal("Detect = true without key")
	}
	_, err := p.Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonNoCredentials) {
		t.Fatalf("err = %v, want no_credentials", err)
	}
}

func TestNoCodingPlanLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":{"limits":[{"type":"TIME_LIMIT","unit":5,"number":1,"percentage":22}]}}`)
	}))
	defer srv.Close()
	_, err := newProvider(t, map[string]string{"Z_AI_API_KEY": testKey}, t.TempDir(), "", srv.URL).
		Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonNoWindows) {
		t.Fatalf("err = %v, want no_windows", err)
	}
}

func TestCleanKey(t *testing.T) {
	cases := map[string]string{
		"  key  ":      "key",
		"Bearer key":   "key",
		"Bearer  key ": "key",
		`"key"`:        "key",
		"\nkey\n":      "key",
	}
	for in, want := range cases {
		if got := cleanKey(in); got != want {
			t.Errorf("cleanKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWindowName(t *testing.T) {
	cases := map[int]string{
		300:   "5h",
		1440:  "day",
		10080: "week",
		43200: "month",
		120:   "2h",
		2880:  "2d",
		90:    "90m",
		0:     "plan",
	}
	for minutes, want := range cases {
		if got := windowName(minutes); got != want {
			t.Errorf("windowName(%d) = %q, want %q", minutes, got, want)
		}
	}
}
