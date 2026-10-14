package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

func legacyProviderFixture(t *testing.T, path string) {
	t.Helper()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for i, base := range []string{"https://API.example.com:443/v1/", "https://api.example.com/v1", "https://api.example.com/tenant/B", "http://localhost:15721/v1"} {
		e := model.UsageEvent{DedupKey: base, SessionID: "s", Model: "m", BaseURL: base, Tokens: model.Tokens{Input: int64(i + 1), Output: 2}}
		if err = st.Commit(ctx, model.Codex, "fixture", harness.Batch{Events: []model.UsageEvent{e}}, []Resolution{{Provider: "old", Cost: 1.25, Priced: true}}); err != nil {
			t.Fatal(err)
		}
	}
	if err = st.Exec(ctx, "DROP TABLE IF EXISTS provider_endpoints; DELETE FROM settings WHERE key='provider.identity.v1'; INSERT OR REPLACE INTO settings VALUES('pricing_fingerprint','unchanged')"); err != nil {
		t.Fatal(err)
	}
}

func TestProviderMigrationBacksUpAndPreservesFacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	legacyProviderFixture(t, path)
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	backup := path + ".pre-provider-v1.db"
	if _, err = os.Stat(backup); err != nil {
		t.Fatalf("missing pre-migration backup: %v", err)
	}
	var count, ids int
	if err = st.DB().QueryRow("SELECT count(*),count(DISTINCT provider_id) FROM provider_endpoints").Scan(&count, &ids); err != nil || count != 3 || ids != 2 {
		t.Fatalf("identities: %d/%d %v", count, ids, err)
	}
	var n, in, out int
	var cost float64
	if err = st.DB().QueryRow("SELECT count(*),sum(input),sum(output),sum(cost) FROM events").Scan(&n, &in, &out, &cost); err != nil || n != 4 || in != 10 || out != 8 || cost != 5 {
		t.Fatalf("facts changed: %d %d %d %g %v", n, in, out, cost, err)
	}
	if got, err := st.Setting(context.Background(), "pricing_fingerprint"); err != nil || got != "unchanged" {
		t.Fatalf("repricing checkpoint changed: %q %v", got, err)
	}
	// Reopening a completed migration must not attempt to overwrite its backup.
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
}

func TestProviderMigrationInvariantFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	legacyProviderFixture(t, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TRIGGER corrupt_identity_migration AFTER INSERT ON settings WHEN NEW.key='provider.identity.v1' BEGIN UPDATE event_data SET cost=cost+1; END`); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err == nil {
		st.Close()
		t.Fatal("invariant violation was committed")
	}
	var cost float64
	var marker, tables int
	if err = db.QueryRow("SELECT sum(cost) FROM events").Scan(&cost); err != nil || cost != 5 {
		t.Fatalf("cost not rolled back: %g %v", cost, err)
	}
	if err = db.QueryRow("SELECT count(*) FROM settings WHERE key='provider.identity.v1'").Scan(&marker); err != nil || marker != 0 {
		t.Fatalf("marker committed: %d %v", marker, err)
	}
	if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='provider_endpoints'").Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("identity DDL not rolled back: %d %v", tables, err)
	}
}

func TestProviderMigrationBackupFailureStopsOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	legacyProviderFixture(t, path)
	backup := path + ".pre-provider-v1.db"
	if err := os.WriteFile(backup, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err == nil {
		st.Close()
		t.Fatal("migration proceeded without a new consistent backup")
	}
	got, err := os.ReadFile(backup)
	if err != nil || string(got) != "existing" {
		t.Fatal("backup overwritten")
	}
}
