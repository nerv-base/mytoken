package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The identity migration must not be able to change derived pricing flags. The
// SHA-256 stream covers every request and token class, but it must also cover the
// priced flag: a reprice during migration would silently change reported costs.
func TestProviderMigrationDetectsPricingFlagCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	legacyProviderFixture(t, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TRIGGER flip_priced AFTER INSERT ON settings WHEN NEW.key='provider.identity.v1' BEGIN UPDATE event_data SET priced=1-priced; END;`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err == nil {
		st.Close()
		t.Fatal("pricing flag corruption during migration was committed")
	}
	if !strings.Contains(err.Error(), "invariant") {
		t.Fatalf("unexpected failure, want an invariant error: %v", err)
	}
	raw := openReadOnly(t, path)
	var marker int
	if err = raw.QueryRow("SELECT count(*) FROM settings WHERE key='provider.identity.v1'").Scan(&marker); err != nil || marker != 0 {
		t.Fatalf("marker committed: %d %v", marker, err)
	}
	var priced int
	if err = raw.QueryRow("SELECT sum(priced) FROM events").Scan(&priced); err != nil || priced != 4 {
		t.Fatalf("pricing flags not rolled back: %d %v", priced, err)
	}
}

// Evidence for the digest-coverage review: a dropped row changes the hash stream
// itself, so no separate rowcount is needed to detect it.
func TestProviderMigrationDetectsDroppedRowWithoutRowcount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	legacyProviderFixture(t, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TRIGGER drop_row AFTER INSERT ON settings WHEN NEW.key='provider.identity.v1' BEGIN DELETE FROM event_data WHERE rowid=(SELECT min(rowid) FROM event_data); END;`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err == nil {
		st.Close()
		t.Fatal("dropped row during migration was committed")
	}
	if !strings.Contains(err.Error(), "invariant") {
		t.Fatalf("unexpected failure, want an invariant error: %v", err)
	}
	raw := openReadOnly(t, path)
	var n int
	if err = raw.QueryRow("SELECT count(*) FROM events").Scan(&n); err != nil || n != 4 {
		t.Fatalf("dropped row not rolled back: %d %v", n, err)
	}
}

// A migration marker without its table used to be treated as "already migrated",
// silently skipping the identity step and leaving the index unable to record new
// endpoint identities.
func TestProviderMigrationMarkerWithoutEndpointsFailsActionably(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	legacyProviderFixture(t, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO settings VALUES('provider.identity.v1','1')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err == nil {
		st.Close()
		t.Fatal("inconsistent index (marker without provider_endpoints) was accepted")
	}
	if !strings.Contains(err.Error(), "provider identity index is inconsistent") ||
		!strings.Contains(err.Error(), "provider_endpoints") ||
		!strings.Contains(err.Error(), providerIdentityVersion) {
		t.Fatalf("failure is not actionable: %v", err)
	}
	// No migration ran, so nothing was rewritten and no backup artifact was left
	// behind that would later block the real migration.
	if _, statErr := os.Stat(path + ".pre-provider-v1.db"); statErr == nil {
		t.Fatal("a backup was created for a migration that never ran")
	}
	raw := openReadOnly(t, path)
	var cost float64
	if err = raw.QueryRow("SELECT sum(cost) FROM events").Scan(&cost); err != nil || cost != 5 {
		t.Fatalf("history changed: %g %v", cost, err)
	}
	var marker int
	if err = raw.QueryRow("SELECT count(*) FROM settings WHERE key='provider.identity.v1'").Scan(&marker); err != nil || marker != 1 {
		t.Fatalf("marker should be untouched: %d %v", marker, err)
	}
}

// The reverse inconsistency: the identity table exists but the marker that proves
// the migration completed does not. It used to fail deep inside DDL with a raw
// SQLite error after already writing a backup.
func TestProviderMigrationEndpointsWithoutMarkerFailsActionably(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	legacyProviderFixture(t, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE provider_endpoints(raw_endpoint TEXT PRIMARY KEY,provider_id TEXT NOT NULL,endpoint TEXT NOT NULL)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err == nil {
		st.Close()
		t.Fatal("inconsistent index (provider_endpoints without marker) was accepted")
	}
	if !strings.Contains(err.Error(), "provider identity index is inconsistent") ||
		!strings.Contains(err.Error(), "provider_endpoints") ||
		!strings.Contains(err.Error(), providerIdentityVersion) {
		t.Fatalf("failure is not actionable: %v", err)
	}
	if _, statErr := os.Stat(path + ".pre-provider-v1.db"); statErr == nil {
		t.Fatal("a backup was created for a migration that never ran")
	}
	raw := openReadOnly(t, path)
	var marker int
	if err = raw.QueryRow("SELECT count(*) FROM settings WHERE key='provider.identity.v1'").Scan(&marker); err != nil || marker != 0 {
		t.Fatalf("marker was self-healed: %d %v", marker, err)
	}
	var cost float64
	if err = raw.QueryRow("SELECT sum(cost) FROM events").Scan(&cost); err != nil || cost != 5 {
		t.Fatalf("history changed: %g %v", cost, err)
	}
}

// Any unexpected object under the identity table's name (here a view) is as
// inconsistent as a table without the marker. It used to slip past the preflight
// check because only kind=="table" was rejected: the migration then wrote a
// backup and failed later on a raw DDL conflict.
func TestProviderMigrationNonTableEndpointsObjectFailsBeforeBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	legacyProviderFixture(t, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE VIEW provider_endpoints AS SELECT '' AS raw_endpoint,'' AS provider_id,'' AS endpoint`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err == nil {
		st.Close()
		t.Fatal("inconsistent index (provider_endpoints as a view without marker) was accepted")
	}
	if !strings.Contains(err.Error(), "provider identity index is inconsistent") ||
		!strings.Contains(err.Error(), "provider_endpoints") ||
		!strings.Contains(err.Error(), "view") ||
		!strings.Contains(err.Error(), providerIdentityVersion) {
		t.Fatalf("failure is not actionable: %v", err)
	}
	// The preflight check runs before the backup: no artifact may be left that
	// would later block the real migration.
	if _, statErr := os.Stat(path + ".pre-provider-v1.db"); statErr == nil {
		t.Fatal("a backup was created for a migration that never ran")
	}
	raw := openReadOnly(t, path)
	var marker int
	if err = raw.QueryRow("SELECT count(*) FROM settings WHERE key='provider.identity.v1'").Scan(&marker); err != nil || marker != 0 {
		t.Fatalf("marker was self-healed: %d %v", marker, err)
	}
	var cost float64
	if err = raw.QueryRow("SELECT sum(cost) FROM events").Scan(&cost); err != nil || cost != 5 {
		t.Fatalf("history changed: %g %v", cost, err)
	}
}
