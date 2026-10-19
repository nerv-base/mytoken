// These tests run the reported read defects through the whole ingest path they
// were observed on: the parser, the scanner and the real store. The package's
// other tests pin the parser and the shared SQLite module in isolation; what
// only this level shows is that one corrupt row no longer costs the ledger the
// rows beside it, and that one corrupt counter no longer makes a day's sums
// unreadable.
package opencode

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/zzstar101/mytoken/internal/scan"
	"github.com/zzstar101/mytoken/internal/store"
)

// ingestFixture writes one OpenCode database and opens a fresh store over it.
func ingestFixture(t *testing.T, ddl string) (dir string, st *store.Store) {
	t.Helper()
	dir = t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "opencode.db"))
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("seed fixture db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture db: %v", err)
	}
	st, err = store.Open(filepath.Join(t.TempDir(), "idx"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return dir, st
}

// scanPasses runs n scans, stopping at the first failure: the store rejects a
// whole batch for one event without a session, so a single bad row used to make
// every pass fail the same way and no cursor ever advanced.
func scanPasses(t *testing.T, st *store.Store, dir string, n int) {
	t.Helper()
	sc := scan.New(st, nil, nil, NewWithRoots(dir))
	for pass := 1; pass <= n; pass++ {
		if err := sc.Scan(context.Background()); err != nil {
			t.Fatalf("pass %d: Scan: %v", pass, err)
		}
	}
}

// ledgerSessions maps every stored dedup key to the session the ledger kept.
// The session lives on the dimension, so this reads the same join view the
// reports do.
func ledgerSessions(t *testing.T, st *store.Store) map[string]string {
	t.Helper()
	rows, err := st.DB().Query("SELECT dedup_key, session_id FROM events ORDER BY dedup_key")
	if err != nil {
		t.Fatalf("ledger query: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, session string
		if err := rows.Scan(&key, &session); err != nil {
			t.Fatalf("ledger scan: %v", err)
		}
		out[key] = session
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("ledger rows: %v", err)
	}
	return out
}

// TestIngestSurvivesANULLSessionIDColumn is the reported failure end to end: a
// v2 row whose session_id column is NULL while its payload states the session.
// The batch used to be rejected ("invalid event: harness/session/dedup key"),
// so neither row was ever stored and the cursor never moved.
func TestIngestSurvivesANULLSessionIDColumn(t *testing.T) {
	const ddl = `
CREATE TABLE session_v2 (id TEXT PRIMARY KEY, parent_id TEXT, directory TEXT, title TEXT, time_created INTEGER, time_updated INTEGER);
CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, seq INTEGER, time_created INTEGER, time_updated INTEGER, data TEXT);
INSERT INTO session_v2 VALUES ('ses_clean','','/Users/dev/clean','Clean',1742305000000,1742305000000);
INSERT INTO session_v2 VALUES ('ses_null','','/Users/dev/null','Null',1742305000000,1742305000000);
INSERT INTO session_message VALUES ('msg_clean','ses_clean','assistant',0,1742305010000,1742305010000,
 '{"role":"assistant","cost":0,"tokens":{"total":120,"input":100,"output":20},"modelID":"m","providerID":"p","time":{"created":1742305010000}}');
INSERT INTO session_message VALUES ('msg_null',NULL,'assistant',1,1742305020000,1742305020000,
 '{"role":"assistant","sessionID":"ses_null","cost":0,"tokens":{"total":70,"input":60,"output":10},"modelID":"m","providerID":"p","time":{"created":1742305020000}}');
`
	dir, st := ingestFixture(t, ddl)
	scanPasses(t, st, dir, 2)

	want := map[string]string{"opencode:msg_clean": "ses_clean", "opencode:msg_null": "ses_null"}
	got := ledgerSessions(t, st)
	if len(got) != len(want) {
		t.Fatalf("ledger holds %v, want %v: the NULL column must cost the row nothing and the clean row beside it even less", got, want)
	}
	for key, session := range want {
		if got[key] != session {
			t.Errorf("ledger %s session = %q, want %q (the payload states it)", key, got[key], session)
		}
	}
}

// TestIngestSurvivesAnEmptySessionIDColumn is the same failure written as an
// empty string, which is legal under `session_id NOT NULL`.
func TestIngestSurvivesAnEmptySessionIDColumn(t *testing.T) {
	const ddl = `
CREATE TABLE session (id TEXT PRIMARY KEY, parent_id TEXT, directory TEXT, title TEXT, time_created INTEGER, time_updated INTEGER);
CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER, time_updated INTEGER, data TEXT NOT NULL);
INSERT INTO session VALUES ('ses_e','','/Users/dev/e','E',1742308000000,1742308000000);
INSERT INTO message VALUES ('msg_good','ses_e',1742308010000,1742308010000,
 '{"role":"assistant","sessionID":"ses_e","cost":0,"tokens":{"input":100,"output":20},"modelID":"m","providerID":"p","time":{"created":1742308010000}}');
INSERT INTO message VALUES ('msg_empty','',1742308020000,1742308020000,
 '{"role":"assistant","sessionID":"ses_e","cost":0,"tokens":{"input":60,"output":10},"modelID":"m","providerID":"p","time":{"created":1742308020000}}');
`
	dir, st := ingestFixture(t, ddl)
	scanPasses(t, st, dir, 3)

	want := map[string]string{"opencode:msg_good": "ses_e", "opencode:msg_empty": "ses_e"}
	got := ledgerSessions(t, st)
	if len(got) != len(want) {
		t.Fatalf("ledger holds %v, want %v: an empty session_id column must not stop the batch", got, want)
	}
	for key, session := range want {
		if got[key] != session {
			t.Errorf("ledger %s session = %q, want %q", key, got[key], session)
		}
	}
}

// TestIngestSurvivesANULLMessageID is the second reported defect end to end: a
// NULL message id inside the v1 table used to fail the row scan, which returned
// the old watermark with no error, so every pass re-read the same row and the
// legal row behind it never reached the ledger.
func TestIngestSurvivesANULLMessageID(t *testing.T) {
	const ddl = `
CREATE TABLE session (id TEXT PRIMARY KEY, parent_id TEXT, directory TEXT, title TEXT, time_created INTEGER, time_updated INTEGER);
CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER, time_updated INTEGER, data TEXT NOT NULL);
INSERT INTO session VALUES ('ses_n','','/Users/dev/n','N',1742290000000,1742290000000);
INSERT INTO message VALUES ('msg_a','ses_n',1742300010000,1742300010000,
 '{"role":"assistant","sessionID":"ses_n","cost":0,"tokens":{"input":100,"output":10},"modelID":"m","providerID":"p","time":{"created":1742300010000}}');
INSERT INTO message VALUES (NULL,'ses_n',1742300020000,1742300020000,
 '{"role":"assistant","sessionID":"ses_n","cost":0,"tokens":{"input":200,"output":20},"modelID":"m","providerID":"p","time":{"created":1742300020000}}');
INSERT INTO message VALUES ('msg_c','ses_n',1742300030000,1742300030000,
 '{"role":"assistant","sessionID":"ses_n","cost":0,"tokens":{"input":300,"output":30},"modelID":"m","providerID":"p","time":{"created":1742300030000}}');
`
	dir, st := ingestFixture(t, ddl)
	scanPasses(t, st, dir, 3)

	got := ledgerSessions(t, st)
	if len(got) != 2 || got["opencode:msg_a"] != "ses_n" || got["opencode:msg_c"] != "ses_n" {
		t.Fatalf("ledger holds %v, want both msg_a and msg_c: the id-less row cannot be counted, but it must not hide the tail", got)
	}
}

// TestIngestKeepsOneEventWhenTheDatabaseAndLegacyJSONCollide pins the guarantee
// that holds for the review's other finding. OpenCode's generations overlap: a
// message can exist both in the sqlite table and in a leftover
// storage/message JSON file, and those are two separate SOURCES, which the
// scanner parses on different workers. Both emit the same key,
// "opencode:<message id>", so the store's upsert keeps exactly one event — but
// WHICH source's counters survive depends on which parse committed last, and a
// scan therefore does not promise a byte-reproducible row for that key. That
// ordering is not decidable inside this package, so the test asserts what the
// ledger does promise instead: one event per key, and numbers that come from a
// single source (never a blend of the two).
func TestIngestKeepsOneEventWhenTheDatabaseAndLegacyJSONCollide(t *testing.T) {
	const (
		ddl = `
CREATE TABLE session (id TEXT PRIMARY KEY, parent_id TEXT, directory TEXT, title TEXT, time_created INTEGER, time_updated INTEGER);
CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER, time_updated INTEGER, data TEXT);
INSERT INTO session VALUES ('ses_dup','','/Users/dev/dup','Dup',1742307000000,1742307000000);
INSERT INTO message VALUES ('msg_dup','ses_dup',1742307010000,1742307010000,
 '{"role":"assistant","sessionID":"ses_dup","cost":0,"tokens":{"total":600,"input":500,"output":100},"modelID":"m","providerID":"p","time":{"created":1742307010000}}');
`
		legacy = `{"id":"msg_dup","sessionID":"ses_dup","role":"assistant","modelID":"m","providerID":"p","cost":0,
 "tokens":{"input":70,"output":9,"reasoning":0,"cache":{"read":0,"write":0}},
 "time":{"created":1742307010000,"completed":1742307010000}}`
	)
	type pair struct{ input, output int64 }
	dbPair, jsonPair := pair{500, 100}, pair{70, 9}
	seen := map[pair]int{}
	for run := 0; run < 20; run++ {
		dir, st := ingestFixture(t, ddl)
		msgDir := filepath.Join(dir, "storage", "message", "ses_dup")
		if err := os.MkdirAll(msgDir, 0o755); err != nil {
			t.Fatalf("run %d: create legacy dir: %v", run, err)
		}
		if err := os.WriteFile(filepath.Join(msgDir, "msg_dup.json"), []byte(legacy), 0o644); err != nil {
			t.Fatalf("run %d: write legacy file: %v", run, err)
		}
		scanPasses(t, st, dir, 1)

		rows, err := st.DB().Query("SELECT dedup_key, input, output FROM event_data")
		if err != nil {
			t.Fatalf("run %d: ledger query: %v", run, err)
		}
		var got []pair
		var keys []string
		for rows.Next() {
			var key string
			var p pair
			if err := rows.Scan(&key, &p.input, &p.output); err != nil {
				t.Fatalf("run %d: ledger scan: %v", run, err)
			}
			keys = append(keys, key)
			got = append(got, p)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("run %d: ledger rows: %v", run, err)
		}
		rows.Close()
		if len(got) != 1 || keys[0] != "opencode:msg_dup" {
			t.Fatalf("run %d: ledger holds %v (%v), want exactly one opencode:msg_dup", run, keys, got)
		}
		if got[0] != dbPair && got[0] != jsonPair {
			t.Fatalf("run %d: stored %+v, want the database's %+v or the legacy file's %+v; a stored value blending the two would silently double-count or lose usage", run, got[0], dbPair, jsonPair)
		}
		seen[got[0]]++
	}
	t.Logf("stored counters over 20 scans: database %+v x%d, legacy %+v x%d (the winner is completion-order dependent)", dbPair, seen[dbPair], jsonPair, seen[jsonPair])
}

// TestIngestDoesNotStoreASaturatedCounter is the third reported defect end to
// end. A counter written as 1e30 saturated to math.MaxInt64 on the way into the
// ledger, and two such rows made SQLite fail their SUM with "integer overflow";
// the same row's time.created became a date four hundred million years away.
// Both halves are asserted here: the sum is computable and zero, and every
// stored timestamp is an instant this harness could have recorded (the row's
// own time_updated), not a saturated int64.
func TestIngestDoesNotStoreASaturatedCounter(t *testing.T) {
	const ddl = `
CREATE TABLE session (id TEXT PRIMARY KEY, parent_id TEXT, directory TEXT, title TEXT, time_created INTEGER, time_updated INTEGER);
CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, seq INTEGER, time_created INTEGER, time_updated INTEGER, data TEXT);
INSERT INTO session VALUES ('ses_ov','','/Users/dev/ov','Ov',1742309000000,1742309000000);
INSERT INTO session_message VALUES ('msg_ov0','ses_ov','assistant',0,1742309010000,1742309010000,
 '{"role":"assistant","sessionID":"ses_ov","cost":0,"tokens":{"input":1e30,"output":5},"modelID":"m","providerID":"p","time":{"created":1e30}}');
INSERT INTO session_message VALUES ('msg_ov1','ses_ov','assistant',1,1742309010001,1742309010001,
 '{"role":"assistant","sessionID":"ses_ov","cost":0,"tokens":{"input":1e30,"output":5},"modelID":"m","providerID":"p","time":{"created":1e30}}');
`
	dir, st := ingestFixture(t, ddl)
	scanPasses(t, st, dir, 1)

	if got := ledgerSessions(t, st); len(got) != 2 {
		t.Fatalf("ledger holds %v, want the two rows: a counter that is unknown must not cost the row its output", got)
	}

	var sum sql.NullInt64
	if err := st.DB().QueryRow("SELECT SUM(input) FROM event_data").Scan(&sum); err != nil {
		t.Fatalf("SUM(input) over the ledger: %v", err)
	}
	if sum.Int64 != 0 {
		t.Errorf("SUM(input) = %d, want 0: an unreadable counter is unknown, never a saturated maximum", sum.Int64)
	}

	var oldest, newest int64
	if err := st.DB().QueryRow("SELECT MIN(timestamp), MAX(timestamp) FROM event_data").Scan(&oldest, &newest); err != nil {
		t.Fatalf("timestamp range: %v", err)
	}
	lo := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	hi := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	if oldest < lo || newest > hi {
		t.Errorf("ledger timestamps = [%d %d], want inside [%d %d]: a time this module cannot read falls back to the row's time_updated, not to a saturated int64", oldest, newest, lo, hi)
	}
}
