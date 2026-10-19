package cursor

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/zzstar101/mytoken/internal/quota/plan"
	"github.com/zzstar101/mytoken/internal/sqlitedsn"
)

const testToken = "test-cursor-token"

// writeStateDB creates a Cursor-like state database containing value under key.
func writeStateDB(t *testing.T, key, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), stateDBFile)
	db, err := sql.Open("sqlite", sqlitedsn.URI(path, ""))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO ItemTable (key, value) VALUES (?, ?)`, key, []byte(value)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	return path
}

func newProvider(t *testing.T, paths []string, baseURL string) *Provider {
	t.Helper()
	return New(Options{StatePaths: paths, BaseURL: baseURL, GOOS: "darwin", Home: t.TempDir(), Now: func() time.Time {
		return time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	}})
}

func TestFetchUsage(t *testing.T) {
	dbPath := writeStateDB(t, tokenKey, testToken)
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("Connect-Protocol-Version"); got != "1" {
			t.Errorf("connect version = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "{}" {
			t.Errorf("body = %q", body)
		}
		switch r.URL.Path {
		case periodUsagePath:
			io.WriteString(w, `{
				"billingCycleStart": "1727740800000",
				"billingCycleEnd": "1729987200000",
				"planUsage": {
					"totalSpend": "500",
					"includedSpend": 800,
					"bonusSpend": 0,
					"remaining": "1500",
					"limit": "2000",
					"totalPercentUsed": 25.5
				}
			}`)
		case planInfoPath:
			io.WriteString(w, `{"planInfo":{"planName":"Pro","includedAmountCents":"2000"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := newProvider(t, []string{dbPath}, srv.URL)
	if !p.Detect(context.Background()) {
		t.Fatal("Detect = false, want true")
	}
	snap, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.Provider != ProviderID || snap.Source != plan.SourceAPI {
		t.Errorf("snapshot identity = %q/%q", snap.Provider, snap.Source)
	}
	if snap.Plan != "Pro" {
		t.Errorf("plan = %q", snap.Plan)
	}
	if len(snap.Windows) != 1 {
		t.Fatalf("windows = %+v", snap.Windows)
	}
	win := snap.Windows[0]
	if win.Name != "month" || win.UsedPercent != 25.5 || win.Used != 5 || win.Limit != 20 || win.Unit != "USD" {
		t.Errorf("window = %+v", win)
	}
	if want := time.UnixMilli(1729987200000).UTC(); !win.ResetsAt.Equal(want) {
		t.Errorf("resets = %v, want %v", win.ResetsAt, want)
	}
	if len(paths) != 2 || paths[0] != periodUsagePath || paths[1] != planInfoPath {
		t.Errorf("requests = %v", paths)
	}
}

func TestFetchUnauthorized(t *testing.T) {
	dbPath := writeStateDB(t, tokenKey, testToken)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := newProvider(t, []string{dbPath}, srv.URL).Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonUnauthorized) {
		t.Fatalf("err = %v, want unauthorized", err)
	}
}

func TestDetectMissingDatabase(t *testing.T) {
	p := newProvider(t, []string{filepath.Join(t.TempDir(), stateDBFile)}, "")
	if p.Detect(context.Background()) {
		t.Fatal("Detect = true for missing database")
	}
	_, err := p.Fetch(context.Background())
	if !plan.IsReason(err, plan.ReasonSourceMissing) {
		t.Fatalf("err = %v, want source_missing", err)
	}
}

func TestTokenMissing(t *testing.T) {
	dbPath := writeStateDB(t, "cursorAuth/refreshToken", "other")
	if _, err := readAccessToken(context.Background(), []string{dbPath}); !plan.IsReason(err, plan.ReasonSourceMissing) {
		t.Fatalf("err = %v, want source_missing", err)
	}
}

func TestDecodeTokenValue(t *testing.T) {
	cases := map[string]string{
		`"abc"`:  "abc",
		`abc`:    "abc",
		`  abc `: "abc",
		`""`:     "",
	}
	for in, want := range cases {
		if got := decodeTokenValue([]byte(in)); got != want {
			t.Errorf("decodeTokenValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWindowsIndividualUsage(t *testing.T) {
	raw := []byte(`{
		"billingCycleEnd": 1729987200,
		"individualUsage": {"plan": {"used": 1000, "limit": 4000, "totalPercentUsed": 25}}
	}`)
	var resp usageResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	windows := resp.windows()
	if len(windows) != 1 {
		t.Fatalf("windows = %+v", windows)
	}
	if windows[0].UsedPercent != 25 || windows[0].Limit != 40 || windows[0].Used != 10 {
		t.Errorf("window = %+v", windows[0])
	}
	if want := time.Unix(1729987200, 0).UTC(); !windows[0].ResetsAt.Equal(want) {
		t.Errorf("resets = %v, want %v", windows[0].ResetsAt, want)
	}
}

func TestWindowsEmpty(t *testing.T) {
	var resp usageResponse
	if got := resp.windows(); got != nil {
		t.Errorf("windows = %+v, want nil", got)
	}
}
