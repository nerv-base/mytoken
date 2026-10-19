// These tests pin the watermark contract of the extracted read-only SQLite
// module directly. The two harness suites exercise the module end to end; what
// they cannot pin cheaply is the single question Reset answers — "is this
// persisted (time_updated, id) resume point still trustworthy?" — and what it
// costs to answer it wrongly: a watermark that survives a pass in which nothing
// could be read can never be lowered again, so every row the table later
// exposes at or before it is skipped for good.
package sqlitecore

import (
	"context"
	"database/sql"
	"math"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/sqlitedsn"
)

// v2DDL is the session_message generation OpenCode added for v2 usage: the row
// kind lives in its own column, which is what Spec.KindExpr selects.
const v2DDL = `CREATE TABLE session_message (
		id            TEXT PRIMARY KEY,
		session_id    TEXT,
		type          TEXT,
		time_updated  INTEGER,
		data          TEXT)`

// v2Payload is a json_valid usage payload the v2 filter accepts.
const v2Payload = `{"tokens":{"input":7,"output":3}}`

func v2Spec() Spec {
	return Spec{
		Table:    "session_message",
		Filter:   "json_valid(data) AND type IN ('assistant','compaction') AND json_extract(data,'$.tokens') IS NOT NULL",
		KindExpr: "type",
	}
}

// fixture is a database file plus the writable staging used to change its
// schema between two read-only scans.
type fixture struct{ path string }

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{path: filepath.Join(t.TempDir(), "opencode.db")}
}

func (f *fixture) exec(t *testing.T, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", sqlitedsn.URI(f.path, url.Values{"mode": {"rwc"}}.Encode()))
	if err != nil {
		t.Fatalf("open %s: %v", f.path, err)
	}
	defer db.Close()
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %s: %v", stmt, err)
		}
	}
}

func (f *fixture) open(t *testing.T) *Snapshot {
	t.Helper()
	snap, err := Open(f.path)
	if err != nil {
		t.Fatalf("Open(%s): %v", f.path, err)
	}
	t.Cleanup(func() { snap.Close() })
	return snap
}

// reset is what a harness persists for this pass: the verdict of Reset alone.
func (f *fixture) reset(t *testing.T, wm Watermark) Watermark {
	t.Helper()
	return f.open(t).Reset(context.Background(), v2Spec(), wm)
}

// resetAndRead runs the pair a harness runs: Reset the persisted watermark
// against this snapshot, then read with whatever Reset returned.
func (f *fixture) resetAndRead(t *testing.T, wm Watermark) ([]Row, Watermark) {
	t.Helper()
	snap := f.open(t)
	ctx := context.Background()
	spec := v2Spec()
	wm = snap.Reset(ctx, spec, wm)
	var rows []Row
	wm = snap.Messages(ctx, spec, wm, func(r Row) { rows = append(rows, r) })
	return rows, wm
}

func wantIDs(t *testing.T, what string, rows []Row, want ...string) {
	t.Helper()
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.ID)
	}
	if len(got) != len(want) {
		t.Fatalf("%s emitted %v, want %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s emitted %v, want %v", what, got, want)
		}
	}
}

// TestResetKeepsOnlyAWatermarkTheTableStillCovers walks the states a persisted
// watermark can be in when the next scan starts: inside the table, ahead of it
// because the database was replaced or rewound, negative, and zero.
func TestResetKeepsOnlyAWatermarkTheTableStillCovers(t *testing.T) {
	seeded := func(t *testing.T) *fixture {
		t.Helper()
		f := newFixture(t)
		f.exec(t, v2DDL,
			`INSERT INTO session_message (id,session_id,type,time_updated,data)
			 VALUES ('a','s1','assistant',1000,'`+v2Payload+`')`,
			`INSERT INTO session_message (id,session_id,type,time_updated,data)
			 VALUES ('b','s1','assistant',2000,'`+v2Payload+`')`)
		return f
	}

	for _, tc := range []struct {
		name string
		wm   Watermark
		want Watermark
	}{
		{"at the newest row", Watermark{Updated: 2000, ID: "b"}, Watermark{Updated: 2000, ID: "b"}},
		{"behind the newest row", Watermark{Updated: 1000, ID: "a"}, Watermark{Updated: 1000, ID: "a"}},
		{"without an id yet", Watermark{Updated: 2000}, Watermark{Updated: 2000}},
		{"newer than the table, a rewound or replaced database", Watermark{Updated: 5000, ID: "z"}, Watermark{}},
		{"negative", Watermark{Updated: -1, ID: "a"}, Watermark{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := seeded(t).reset(t, tc.wm); got != tc.want {
				t.Errorf("Reset(%+v) = %+v, want %+v", tc.wm, got, tc.want)
			}
		})
	}

	t.Run("an emptied table drops every watermark", func(t *testing.T) {
		f := newFixture(t)
		f.exec(t, v2DDL)
		if got := f.reset(t, Watermark{Updated: 2000, ID: "b"}); got != (Watermark{}) {
			t.Errorf("Reset on an empty table = %+v, want zero so the rows are read again", got)
		}
	})

	t.Run("a rewound watermark re-reads every row", func(t *testing.T) {
		rows, wm := seeded(t).resetAndRead(t, Watermark{Updated: 5000, ID: "z"})
		wantIDs(t, "after a rewind", rows, "a", "b")
		if wm != (Watermark{Updated: 2000, ID: "b"}) {
			t.Errorf("watermark after the re-read = %+v, want the table's newest row", wm)
		}
	})
}

// TestResetForgetsAWatermarkWhenTheTableVanishes pins the generation case: the
// message table is named differently by v1 and v2, so whichever one is missing
// this pass must not keep a watermark, because an upgrade can create it later
// with rows dated before the watermark it used to carry.
func TestResetForgetsAWatermarkWhenTheTableVanishes(t *testing.T) {
	f := newFixture(t)
	// The database exists, the generation does not.
	f.exec(t, v2DDL, `DROP TABLE session_message`)
	if got := f.reset(t, Watermark{Updated: 2000, ID: "b"}); got != (Watermark{}) {
		t.Fatalf("Reset on an absent table = %+v, want zero", got)
	}

	f.exec(t, v2DDL,
		`INSERT INTO session_message (id,session_id,type,time_updated,data)
		 VALUES ('a','s1','assistant',1000,'`+v2Payload+`')`)
	rows, wm := f.resetAndRead(t, Watermark{})
	wantIDs(t, "the table reappearing", rows, "a")
	if wm != (Watermark{Updated: 1000, ID: "a"}) {
		t.Errorf("watermark after the table reappeared = %+v", wm)
	}
}

// TestResetDropsAWatermarkForATableMessagesCannotRead pins the predicate the
// two functions must share. Reset used to ask only for time_updated while the
// read needs id, session_id, data, time_updated and the KindExpr column, so a
// table that was present but missing one of the others kept a watermark that
// nothing could lower again; every row the repaired schema then exposes at or
// before it is lost.
func TestResetDropsAWatermarkForATableMessagesCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		ddl  string
	}{
		{"without the data column", `CREATE TABLE session_message (id TEXT, session_id TEXT, time_updated INTEGER)`},
		{"without the id column", `CREATE TABLE session_message (session_id TEXT, time_updated INTEGER, data TEXT)`},
		{"without the kind column", `CREATE TABLE session_message (id TEXT, session_id TEXT, time_updated INTEGER, data TEXT)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			// A row newer than the watermark, so that emptiness is not what
			// drops it: the schema alone has to.
			f.exec(t, tc.ddl, `INSERT INTO session_message (time_updated) VALUES (9000)`)
			if got := f.reset(t, Watermark{Updated: 2000, ID: "b"}); got != (Watermark{}) {
				t.Errorf("Reset = %+v, want zero: the matching Messages call cannot read this table, so the watermark can never be lowered again and a row at or before time_updated=2000 is skipped once the columns return", got)
			}
		})
	}
}

// TestAHalfMigratedTableDoesNotStrandTheRowItHeld is the same defect observed
// end to end. While `type` is still being backfilled the table cannot be read,
// and a row written at that moment with an older timestamp must still be
// emitted once the migration finishes. Carrying the watermark through that pass
// loses it permanently.
func TestAHalfMigratedTableDoesNotStrandTheRowItHeld(t *testing.T) {
	f := newFixture(t)
	f.exec(t, v2DDL,
		`INSERT INTO session_message (id,session_id,type,time_updated,data)
		 VALUES ('a','s1','assistant',1000,'`+v2Payload+`')`,
		`INSERT INTO session_message (id,session_id,type,time_updated,data)
		 VALUES ('b','s1','assistant',2000,'`+v2Payload+`')`)

	rows, wm := f.resetAndRead(t, Watermark{})
	wantIDs(t, "the first scan", rows, "a", "b")

	// Half-way through the migration: the rows are still there, `type` is not,
	// and the application writes one more row while it works.
	f.exec(t,
		`CREATE TABLE migrated (id TEXT, session_id TEXT, time_updated INTEGER, data TEXT)`,
		`INSERT INTO migrated SELECT id, session_id, time_updated, data FROM session_message`,
		`DROP TABLE session_message`,
		`ALTER TABLE migrated RENAME TO session_message`,
		`INSERT INTO session_message (id,session_id,time_updated,data)
		 VALUES ('c','s1',500,'`+v2Payload+`')`)

	// What the harness would persist for this pass.
	carried := f.reset(t, wm)

	// The migration finishes and the kind is backfilled.
	f.exec(t,
		`ALTER TABLE session_message ADD COLUMN type TEXT`,
		`UPDATE session_message SET type = 'assistant'`)

	rows, wm = f.resetAndRead(t, carried)
	wantIDs(t, "the scan after the migration finished", rows, "c", "a", "b")
	if wm != (Watermark{Updated: 2000, ID: "b"}) {
		t.Errorf("watermark after the migration = %+v, want the newest row", wm)
	}
}

// TestMessagesResumesByUpdatedThenID pins the tie-break the watermark depends
// on. Rows come out in (time_updated, id) order, the resume predicate is
// strict, and the row the watermark names is never emitted twice. A row that
// arrives later with an older timestamp is behind the resume point and is not
// revisited — inherent to a (time_updated, id) watermark, and the reason Reset
// must be strict about dropping one.
func TestMessagesResumesByUpdatedThenID(t *testing.T) {
	f := newFixture(t)
	f.exec(t, v2DDL,
		`INSERT INTO session_message (id,session_id,type,time_updated,data)
		 VALUES ('b','s1','assistant',1000,'`+v2Payload+`')`,
		`INSERT INTO session_message (id,session_id,type,time_updated,data)
		 VALUES ('a','s1','assistant',1000,'`+v2Payload+`')`,
		`INSERT INTO session_message (id,session_id,type,time_updated,data)
		 VALUES ('c','s1','assistant',2000,'`+v2Payload+`')`)

	rows, wm := f.resetAndRead(t, Watermark{})
	wantIDs(t, "the first scan", rows, "a", "b", "c")
	if wm != (Watermark{Updated: 2000, ID: "c"}) {
		t.Fatalf("watermark = %+v, want {2000 c}", wm)
	}

	rows, again := f.resetAndRead(t, wm)
	wantIDs(t, "an idle re-scan", rows)
	if again != wm {
		t.Errorf("an idle re-scan moved the watermark from %+v to %+v", wm, again)
	}

	f.exec(t,
		`INSERT INTO session_message (id,session_id,type,time_updated,data)
		 VALUES ('c2','s1','assistant',2000,'`+v2Payload+`')`,
		`INSERT INTO session_message (id,session_id,type,time_updated,data)
		 VALUES ('d','s1','assistant',2000,'`+v2Payload+`')`)
	rows, wm = f.resetAndRead(t, wm)
	wantIDs(t, "the rows added past the resume point", rows, "c2", "d")
	if wm != (Watermark{Updated: 2000, ID: "d"}) {
		t.Errorf("watermark = %+v, want {2000 d}", wm)
	}
}

// TestMessagesKeepsTheCallersWatermarkWhenTheScanIsInterrupted pins what an
// abandoned scan may return. Messages can stop in the middle for reasons that
// have nothing to do with the data: the caller's context is cancelled (app
// shutdown, a scan deadline), the connection dies, the disk returns an error.
// The rows the loop already delivered are real, but the watermark they advanced
// to is not a resume point — nothing proves the table holds no further row at or
// before it, and the harness persists whatever Messages returns as the cursor
// for the next pass. Returning that advanced watermark therefore turns a
// transient failure into permanently lost usage. The pass must hand back the
// watermark it was called with, so the next pass re-reads the range; rows the
// interrupted pass already visited come back too and are merged by DedupKey,
// which is why re-delivery is safe and a silent skip is not.
func TestMessagesKeepsTheCallersWatermarkWhenTheScanIsInterrupted(t *testing.T) {
	f := newFixture(t)
	f.exec(t, v2DDL,
		`INSERT INTO session_message (id,session_id,type,time_updated,data)
		 VALUES ('a','s1','assistant',1000,'`+v2Payload+`')`,
		`INSERT INTO session_message (id,session_id,type,time_updated,data)
		 VALUES ('b','s1','assistant',2000,'`+v2Payload+`')`,
		`INSERT INTO session_message (id,session_id,type,time_updated,data)
		 VALUES ('c','s1','assistant',3000,'`+v2Payload+`')`)
	snap := f.open(t)

	before := Watermark{Updated: 100, ID: "a0"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var seen []string
	got := snap.Messages(ctx, v2Spec(), before, func(r Row) {
		seen = append(seen, r.ID)
		if len(seen) == 1 {
			// Abandon the scan after a real row has been delivered, so the
			// watermark the loop would advance to is distinguishable from the
			// one it was called with. The sleep is the hand-off to
			// database/sql's context watcher: cancelling alone is a race
			// because the driver cursor is only closed once that goroutine
			// runs, and an unclosed cursor simply finishes the read.
			cancel()
			time.Sleep(20 * time.Millisecond)
		}
	})
	if len(seen) == 0 {
		t.Fatalf("the visitor never ran, so the test saw a read that failed before any row; it needs an interruption in the middle of the iteration (rows.Err is invisible from here, so the setup must be checked this way)")
	}
	if len(seen) > 1 {
		t.Fatalf("the visitor ran %d times (%v): the interruption never landed, so this test cannot observe the partial scan it is about", len(seen), seen)
	}
	if got != before {
		t.Errorf("Messages returned %+v after the scan was interrupted; want the caller's %+v so the next pass re-reads the rows this one never reached", got, before)
	}

	// The pass the harness runs next, with the watermark it persisted.
	want := []string{"a", "b", "c"}
	var again []string
	after := snap.Messages(context.Background(), v2Spec(), got, func(r Row) { again = append(again, r.ID) })
	if len(again) != len(want) {
		t.Fatalf("the pass after the interruption re-read %v, want %v: the rows the interrupted pass never reached must still be offered, and the ones it did reach are expected again because the accumulator dedups them", again, want)
	}
	for i := range want {
		if again[i] != want[i] {
			t.Fatalf("the pass after the interruption re-read %v, want %v", again, want)
		}
	}
	if after != (Watermark{Updated: 3000, ID: "c"}) {
		t.Errorf("watermark after the retry = %+v, want {3000 c}", after)
	}
}

// TestMessagesReadsARowWhoseSessionIDIsNull reproduces the reported silent
// undercount. `session_id` is optional metadata: the MessageV2 payload inside
// `data` states the session itself, and MiMo's own parser falls back to it
// (mimocode.go:401 FirstNonEmpty(row.SessionID, payload.SessionID,
// payload.SnakeSessionID)). SQLite, however, lets the column hold NULL, and
// database/sql refuses to scan NULL into a string, so the row failed its scan.
//
// The scan failure was answered with `continue`, which is where the permanent
// loss came from: the next row that scanned advanced the watermark past it, and
// a (time_updated, id) keyset has no way back. In this fixture the NULL-session
// row sits at time_updated=1000 and the readable row at 2000, so the watermark
// returned by the buggy pass names 2000 — the row at 1000 is behind the resume
// point and its tokens are never offered again. A row whose only defect is an
// optional NULL must be read, not skipped, so the fix scans the column into a
// sql.NullString and normalises NULL to the empty string the callers already
// treat as "the message must say which session it belongs to".
func TestMessagesReadsARowWhoseSessionIDIsNull(t *testing.T) {
	f := newFixture(t)
	f.exec(t, v2DDL,
		`INSERT INTO session_message (id,session_id,type,time_updated,data)
		 VALUES ('a',NULL,'assistant',1000,'`+v2Payload+`')`,
		`INSERT INTO session_message (id,session_id,type,time_updated,data)
		 VALUES ('b','s1','assistant',2000,'`+v2Payload+`')`)

	rows, wm := f.resetAndRead(t, Watermark{})
	wantIDs(t, "the scan over a row with no session", rows, "a", "b")
	if rows[0].SessionID != "" {
		t.Errorf("SessionID = %q, want the empty string for a NULL column", rows[0].SessionID)
	}
	if rows[1].SessionID != "s1" {
		t.Errorf("SessionID = %q, want s1: a non-NULL column must still arrive verbatim", rows[1].SessionID)
	}
	if wm != (Watermark{Updated: 2000, ID: "b"}) {
		t.Errorf("watermark = %+v, want {2000 b}", wm)
	}
}

// TestMessagesHoldsTheWatermarkWhenARowCannotBeScanned pins the other half of
// the reported defect: a scan failure that is NOT the optional NULL above.
// SQLite columns carry dynamic types, so `time_updated` — declared INTEGER —
// can hold the TEXT 'not-a-number' when the writing side produced one. That is
// the watermark column itself, so a failed scan leaves the row's position
// unknown, and no later row may be treated as read.
//
// The old loop answered the failure with `continue`, which made the pass look
// successful: the readable rows were delivered, `rows.Err()` stayed nil, and the
// watermark advanced to the newest row it had scanned ({2000 later} below) even
// though a selected row was never read. The row's tokens were then silently
// absent from the ledger for good — nothing re-delivers a row the persisted
// resume point has walked past. The contract Messages documents is that a pass
// either reads the range to the end or hands back the watermark it was called
// with, so the fix returns `before` and the next pass re-reads the range; the
// rows the failed pass already visited come back too and are merged by
// DedupKey, which is why re-delivery is safe and a silent skip is not.
//
// The conservative price is deliberate and recorded here: while a row carries a
// permanently unreadable time_updated the watermark never advances past it, so
// that table's progress stalls instead of its rows being dropped. The retry
// below is the unblocking: repairing the value (upstream's $onUpdate gives any
// really-updated row a new timestamp) lets the whole range through again.
func TestMessagesHoldsTheWatermarkWhenARowCannotBeScanned(t *testing.T) {
	f := newFixture(t)
	f.exec(t, v2DDL,
		`INSERT INTO session_message (id,session_id,type,time_updated,data)
		 VALUES ('ok','s1','assistant',1000,'`+v2Payload+`')`,
		`INSERT INTO session_message (id,session_id,type,time_updated,data)
		 VALUES ('later','s1','assistant',2000,'`+v2Payload+`')`,
		// Dynamic typing: an INTEGER column holding TEXT. TEXT sorts after
		// every number, so this row is read last, after the two readable ones.
		`INSERT INTO session_message (id,session_id,type,time_updated,data)
		 VALUES ('bad','s1','assistant','not-a-number','`+v2Payload+`')`)

	before := Watermark{}
	var seen []string
	got := f.open(t).Messages(context.Background(), v2Spec(), before, func(r Row) { seen = append(seen, r.ID) })
	if len(seen) != 2 || seen[0] != "ok" || seen[1] != "later" {
		t.Fatalf("the failed pass delivered %v, want only the readable rows [ok later]: the malformed row's position is unknown, so nothing at or after it may count as read", seen)
	}
	if got != before {
		t.Errorf("Messages returned %+v after a row failed to scan; want the caller's %+v so the next pass re-reads the range instead of resuming past a row it never read", got, before)
	}

	// Until the value is repaired, every later pass stalls at the same
	// watermark: the table makes no durable progress. That is the conservative
	// cost named above, and it is a stall rather than a loss — the readable rows
	// are delivered again on every pass (the consumers fold them by DedupKey),
	// and the row behind the malformed one is still there to be counted once the
	// value is repaired.
	f.exec(t, `INSERT INTO session_message (id,session_id,type,time_updated,data)
		 VALUES ('behind','s1','assistant',2500,'`+v2Payload+`')`)
	var stalled []string
	stuck := f.open(t).Messages(context.Background(), v2Spec(), got, func(r Row) { stalled = append(stalled, r.ID) })
	if len(stalled) != 3 || stalled[0] != "ok" || stalled[1] != "later" || stalled[2] != "behind" {
		t.Errorf("a pass stalled by the malformed timestamp delivered %v, want the readable rows [ok later behind]: the stall must cost progress, never rows", stalled)
	}
	if stuck != got {
		t.Errorf("the stalled pass returned %+v, want the held %+v: a row it never read is still in the range", stuck, got)
	}

	// The write that repairs the timestamp, and the pass the harness runs next
	// with the watermark the failed pass persisted.
	f.exec(t, `UPDATE session_message SET time_updated = 3000 WHERE id = 'bad'`)
	rows, wm := f.resetAndRead(t, got)
	wantIDs(t, "the pass after the malformed timestamp was repaired", rows, "ok", "later", "behind", "bad")
	if wm != (Watermark{Updated: 3000, ID: "bad"}) {
		t.Errorf("watermark after the retry = %+v, want {3000 bad}", wm)
	}
}

// ---------------------------------------------------------------------------
// tolerant payload projection
// ---------------------------------------------------------------------------

// usageJSON is one completely legible usage block; every case below differs
// from a well-formed row in exactly one optional field, so the classes that come
// back are always legibleTokens.
const usageJSON = `"tokens":{"total":38,"input":12,"output":5,"reasoning":3,"cache":{"read":15,"write":3}}`

func legibleTokens() model.Tokens {
	return model.Tokens{Input: 12, Output: 5, Reasoning: 3, CacheRead: 15, CacheWrite: 3}
}

// TestDecodeKeepsTheRowWhenOptionalMetadataDoesNotFit reproduces the reported
// defect. The `data` column is written by another program, so a single optional
// field can arrive with an unexpected JSON type; the one strict json.Unmarshal
// Decode used to run reported that field and the whole row was thrown away.
// Nothing about that row is recoverable afterwards: the caller has already
// advanced its watermark past it, so the tokens are missing from the ledger for
// good.
//
// The projection is therefore field-wise. A value that does not fit costs only
// itself, and the usage block survives, because the usage block is the reason
// the row was selected in the first place.
func TestDecodeKeepsTheRowWhenOptionalMetadataDoesNotFit(t *testing.T) {
	cost := func(v float64) *float64 { return &v }
	// The baseline is a well-formed row. `extra` is spliced in after it, so a
	// case that malforms a field listed in the baseline does so by repeating
	// the key — JSON objects decode last-one-wins, which keeps every case to
	// exactly one changed field.
	const baseline = `"role":"assistant","modelID":"claude-sonnet-4-5","providerID":"anthropic",` +
		`"path":{"cwd":"/Users/dev/proj","root":"/Users/dev/proj"},"time":{"created":1742300010000},`
	at := time.UnixMilli(1742300010000).UTC()
	cases := []struct {
		name      string
		extra     string
		cost      *float64
		model     string
		provider  string
		project   string
		role      string
		sessionID string
		snakeID   string
		variant   string
		at        time.Time
	}{
		{
			name: "control: a well-formed row", model: "claude-sonnet-4-5", provider: "anthropic",
			project: "/Users/dev/proj", role: "assistant", at: at,
		},
		{
			// The shape from the report: cost written as a quoted number.
			name: "cost is a quoted number", extra: `"cost":"0.0123",`,
			cost: cost(0.0123), model: "claude-sonnet-4-5", provider: "anthropic",
			project: "/Users/dev/proj", role: "assistant", at: at,
		},
		{
			name: "cost is text", extra: `"cost":"free",`,
			model: "claude-sonnet-4-5", provider: "anthropic", project: "/Users/dev/proj",
			role: "assistant", at: at,
		},
		{
			name: "cost is an object", extra: `"cost":{"usd":0.5},`,
			model: "claude-sonnet-4-5", provider: "anthropic", project: "/Users/dev/proj",
			role: "assistant", at: at,
		},
		{
			name: "cost is a bool", extra: `"cost":true,`,
			model: "claude-sonnet-4-5", provider: "anthropic", project: "/Users/dev/proj",
			role: "assistant", at: at,
		},
		{
			name: "path is a string", extra: `"path":"/tmp/x",`,
			model: "claude-sonnet-4-5", provider: "anthropic", role: "assistant", at: at,
		},
		{
			name: "path holds numbers", extra: `"path":{"cwd":7,"root":9},`,
			model: "claude-sonnet-4-5", provider: "anthropic", role: "assistant", at: at,
		},
		{
			name: "time is a number", extra: `"time":1742300010000,`,
			model: "claude-sonnet-4-5", provider: "anthropic", project: "/Users/dev/proj",
			role: "assistant",
		},
		{
			name: "time.created is text", extra: `"time":{"created":"whenever"},`,
			model: "claude-sonnet-4-5", provider: "anthropic", project: "/Users/dev/proj",
			role: "assistant",
		},
		{
			name: "time.created is an object", extra: `"time":{"created":{"ms":1742300010000}},`,
			model: "claude-sonnet-4-5", provider: "anthropic", project: "/Users/dev/proj",
			role: "assistant",
		},
		{
			name: "role is a number", extra: `"role":7,`,
			model: "claude-sonnet-4-5", provider: "anthropic", project: "/Users/dev/proj", at: at,
		},
		{
			name: "modelID is a number", extra: `"modelID":7,`,
			provider: "anthropic", project: "/Users/dev/proj", role: "assistant", at: at,
		},
		{
			name: "providerID is a number", extra: `"providerID":7,`,
			model: "claude-sonnet-4-5", project: "/Users/dev/proj", role: "assistant", at: at,
		},
		{
			name: "model is a scalar", extra: `"model":"claude-sonnet-4-5","modelID":7,`,
			provider: "anthropic", project: "/Users/dev/proj", role: "assistant", at: at,
		},
		{
			name: "model.id is a number", extra: `"modelID":7,"providerID":7,"model":{"id":7,"providerID":"anthropic"},`,
			provider: "anthropic", project: "/Users/dev/proj", role: "assistant", at: at,
		},
		{
			name: "session ids are numbers", extra: `"sessionID":7,"session_id":8,`,
			model: "claude-sonnet-4-5", provider: "anthropic", project: "/Users/dev/proj",
			role: "assistant", at: at,
		},
		{
			name: "mode, agent and variant are scalars", extra: `"mode":7,"agent":9,"variant":true,`,
			model: "claude-sonnet-4-5", provider: "anthropic", project: "/Users/dev/proj",
			role: "assistant", at: at,
		},
		{
			// Fields the projection has no use for must not be able to fail a
			// row either: the schema has more in it than model.Tokens needs.
			name:  "unknown fields carry surprising types",
			extra: `"summary":{"input":1},"structured":[1,2],"snapshot":true,"error":{"name":"x"},`,
			model: "claude-sonnet-4-5", provider: "anthropic", project: "/Users/dev/proj",
			role: "assistant", at: at,
		},
		{
			name: "every optional field at once",
			extra: `"cost":{"usd":1},"path":7,"time":["created"],"role":false,` +
				`"modelID":[],"providerID":{},"model":42,"sessionID":0,"session_id":0,` +
				`"mode":0,"agent":0,"variant":0,`,
			at: time.Time{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := Decode([]byte("{" + baseline + tc.extra + usageJSON + "}"))
			if !ok {
				t.Fatalf("Decode reported nothing to project: one wrong-typed optional field must not cost the row")
			}
			if p.Tokens == nil {
				t.Fatalf("Tokens = nil: the usage block is legible and must survive the projection")
			}
			if got := p.Tokens.Classes(); got != legibleTokens() {
				t.Errorf("tokens = %+v, want %+v (a malformed neighbour must not touch the usage classes)", got, legibleTokens())
			}
			if got := ReportedCost(p.Cost); tc.cost == nil && got != nil || tc.cost != nil && (got == nil || *got != *tc.cost) {
				t.Errorf("cost = %v, want %v", got, tc.cost)
			}
			if got := p.ModelName(); got != tc.model {
				t.Errorf("ModelName() = %q, want %q", got, tc.model)
			}
			if got := p.ProviderName(); got != tc.provider {
				t.Errorf("ProviderName() = %q, want %q", got, tc.provider)
			}
			if got := p.Project(); got != tc.project {
				t.Errorf("Project() = %q, want %q", got, tc.project)
			}
			if p.Role != tc.role {
				t.Errorf("Role = %q, want %q", p.Role, tc.role)
			}
			if p.SessionID != tc.sessionID || p.SnakeSessionID != tc.snakeID {
				t.Errorf("session ids = %q/%q, want %q/%q", p.SessionID, p.SnakeSessionID, tc.sessionID, tc.snakeID)
			}
			if p.Variant != tc.variant {
				t.Errorf("Variant = %q, want %q", p.Variant, tc.variant)
			}
			if !p.Timestamp().Equal(tc.at) {
				t.Errorf("Timestamp() = %v, want %v", p.Timestamp(), tc.at)
			}
		})
	}
}

// TestDecodeRejectsOnlyWhatIsNotAMessageObject draws the boundary the tolerance
// must not cross. A value that is not a JSON object carries no message at all,
// so there is nothing to project and the caller's row is genuinely unusable —
// that is the one answer Decode may give with ok == false.
func TestDecodeRejectsOnlyWhatIsNotAMessageObject(t *testing.T) {
	for _, data := range []string{``, `null`, `[]`, `[{"role":"assistant"}]`, `"assistant"`, `12`, `true`, `{`, `{"role":"assistant"`} {
		if p, ok := Decode([]byte(data)); ok {
			t.Errorf("Decode(%q) = %+v, true; a value that is not a JSON object is not a message", data, p)
		}
	}
	// An object with nothing in it is still an object: it simply has no usage.
	p, ok := Decode([]byte(`{}`))
	if !ok || p.Tokens != nil || p.Role != "" {
		t.Errorf("Decode(`{}`) = %+v, %v; want a zero payload that Decode accepted", p, ok)
	}
}

// TestDecodeTreatsANonObjectUsageBlockAsNoUsage: a `tokens` value that is not
// an object states no usage. The row stays decodable (the callers' SQL filters
// select on json_extract(data,'$.tokens') IS NOT NULL, which any non-null value
// satisfies), but there is nothing to count, which is what nil means.
func TestDecodeTreatsANonObjectUsageBlockAsNoUsage(t *testing.T) {
	for _, tokens := range []string{`"12"`, `12`, `[]`, `["input"]`, `true`, `null`, `"tokens"`} {
		p, ok := Decode([]byte(`{"role":"assistant","tokens":` + tokens + `}`))
		if !ok {
			t.Errorf("Decode with tokens=%s reported nothing to project; the row is still a message", tokens)
			continue
		}
		if p.Tokens != nil {
			t.Errorf("tokens=%s decoded as %+v, want no usage at all", tokens, p.Tokens)
		}
	}
}

// TestDecodeKeepsEveryLegibleTokenClass: within a usage object that *is* an
// object, one unreadable class must not discard its neighbours — the classes are
// independent counters, and a counter that cannot be read reads as zero rather
// than taking the whole request with it.
func TestDecodeKeepsEveryLegibleTokenClass(t *testing.T) {
	p, ok := Decode([]byte(`{"role":"assistant","tokens":{"input":"abc","output":5,"reasoning":"2","cache":{"read":1.9,"write":null}}}`))
	if !ok || p.Tokens == nil {
		t.Fatalf("Decode = %+v, %v; want a usage block", p, ok)
	}
	want := model.Tokens{Output: 5, Reasoning: 2, CacheRead: 1}
	if got := p.Tokens.Classes(); got != want {
		t.Errorf("tokens = %+v, want %+v", got, want)
	}

	// A cache value that is not an object loses only the cache classes.
	p, ok = Decode([]byte(`{"role":"assistant","tokens":{"input":4,"output":1,"cache":"none"}}`))
	if !ok || p.Tokens == nil {
		t.Fatalf("Decode = %+v, %v; want a usage block", p, ok)
	}
	if got := p.Tokens.Classes(); got != (model.Tokens{Input: 4, Output: 1}) {
		t.Errorf("tokens = %+v, want input 4 and output 1", got)
	}

	// A usage block whose every counter is unreadable still decodes, and reads
	// as no usage rather than as an error: Emit's IsZero check skips it.
	p, ok = Decode([]byte(`{"role":"assistant","tokens":{"input":"?","output":"?","reasoning":"?","cache":{"read":"?","write":"?"}}}`))
	if !ok || p.Tokens == nil {
		t.Fatalf("Decode = %+v, %v", p, ok)
	}
	if got := p.Tokens.Classes(); !got.IsZero() {
		t.Errorf("tokens = %+v, want zero", got)
	}
}

// ---------------------------------------------------------------------------
// reported read defects: an id-less row and a saturated counter
// ---------------------------------------------------------------------------

// TestMessagesReadsThroughARowWhoseIDIsNull pins the second reported defect.
// SQLite's TEXT PRIMARY KEY accepts NULL, and scanning it into the plain string
// the loop used failed — so the pass returned the caller's watermark with no
// error and every retry failed on the same row: the rows behind it were never
// delivered, and because the watermark could not move, no later pass reached
// them either. An id is the keyset's stable resume point AND the dedup key, so a
// row without one cannot be counted at all; it must be skipped, never fail the
// pass.
//
// Three passes are run on purpose. The first proves the rows on both sides of
// the id-less row are delivered and that the watermark names the last of them;
// the second proves the skipped row cannot drag the range back; the third
// proves a row appended behind it still arrives.
func TestMessagesReadsThroughARowWhoseIDIsNull(t *testing.T) {
	f := newFixture(t)
	f.exec(t, v2DDL,
		`INSERT INTO session_message (id,session_id,type,time_updated,data) VALUES ('a','s1','assistant',1000,'`+v2Payload+`')`,
		`INSERT INTO session_message (id,session_id,type,time_updated,data) VALUES (NULL,'s1','assistant',1500,'`+v2Payload+`')`,
		`INSERT INTO session_message (id,session_id,type,time_updated,data) VALUES ('c','s1','assistant',2000,'`+v2Payload+`')`)

	var seen []string
	first := f.open(t).Messages(context.Background(), v2Spec(), Watermark{}, func(r Row) {
		if r.ID == "" {
			t.Errorf("the scan delivered %+v, a row with no id: it can neither be deduped nor named as a resume point", r)
		}
		seen = append(seen, r.ID)
	})
	if len(seen) != 2 || seen[0] != "a" || seen[1] != "c" {
		t.Fatalf("pass 1 delivered %v, want [a c]: an id-less row must be skipped, not stall the pass", seen)
	}
	if first != (Watermark{Updated: 2000, ID: "c"}) {
		t.Fatalf("pass 1 returned %+v, want {2000 c}: the rows behind the NULL id were read, so the watermark must name them", first)
	}

	// Pass 2 resumes from the persisted watermark: the skipped row must not be
	// re-read and nothing at or before 'c' may come back.
	seen = nil
	second := f.open(t).Messages(context.Background(), v2Spec(), first, func(r Row) { seen = append(seen, r.ID) })
	if len(seen) != 0 {
		t.Errorf("pass 2 delivered %v, want nothing: the watermark covers every countable row", seen)
	}
	if second != first {
		t.Errorf("pass 2 returned %+v, want the same %+v", second, first)
	}

	// Pass 3: a row appended behind the id-less row still arrives.
	f.exec(t, `INSERT INTO session_message (id,session_id,type,time_updated,data) VALUES ('d','s1','assistant',2500,'`+v2Payload+`')`)
	rows, third := f.resetAndRead(t, first)
	wantIDs(t, "pass 3", rows, "d")
	if third != (Watermark{Updated: 2500, ID: "d"}) {
		t.Errorf("pass 3 returned %+v, want {2500 d}", third)
	}
}

// TestNumDoesNotSaturateAnOutOfRangeValue pins the third reported defect: the
// tolerant decoder read every counter through strconv.ParseFloat and cast the
// float to int64, which on arm64 SATURATES. A counter written as 1e30 or "Inf"
// therefore entered the ledger as math.MaxInt64; two such events made SQLite
// fail their SUM with "integer overflow", so one corrupt counter took a whole
// day's report down. A counter is only useful when it is the int64 the number
// really names: overflow, NaN and infinite values are "unknown" (zero), never a
// saturated maximum. Exact integers keep their value, fractional counters still
// truncate toward zero, and negative classes are still clamped by Int64.
func TestNumDoesNotSaturateAnOutOfRangeValue(t *testing.T) {
	tests := []struct {
		raw   string
		input int64
	}{
		// Legal large integers survive exactly, as json numbers and as quoted
		// numbers.
		{`1e18`, 1000000000000000000},
		{`"1e18"`, 1000000000000000000},
		{`9223372036854775807`, math.MaxInt64},
		{`"9223372036854775807"`, math.MaxInt64},
		// Values an int64 cannot hold are unknown (zero), never MaxInt64.
		{`1e19`, 0},
		{`"1e19"`, 0},
		{`1e30`, 0},
		{`"1e30"`, 0},
		{`1e300`, 0},
		{`9223372036854775808`, 0},
		{`18446744073709551615`, 0},
		{`1.5e19`, 0},
		// A value that is not a finite number reads as unknown.
		{`"Inf"`, 0},
		{`"-Inf"`, 0},
		{`"Infinity"`, 0},
		{`"NaN"`, 0},
		{`"abc"`, 0},
		{`null`, 0},
		// Fractional counters still truncate toward zero.
		{`1.9`, 1},
		{`2.7`, 2},
		// A negative class is clamped to zero rather than counted.
		{`-5`, 0},
		{`"-5"`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			p, ok := Decode([]byte(`{"role":"assistant","tokens":{"input":` + tt.raw + `,"output":1}}`))
			if !ok || p.Tokens == nil {
				t.Fatalf("Decode = %+v, %v; want a usage block", p, ok)
			}
			if got := p.Tokens.Classes().Input; got != tt.input {
				t.Errorf("input %s = %d, want %d", tt.raw, got, tt.input)
			}
		})
	}
}

// TestEpochMSReadsSecondsMillisecondsAndNanoseconds pins what a timestamp does
// after the counter fix above. The same float-to-int64 saturation produced
// times hundreds of millions of years away, so a time is now reported only when
// it lands in a plausible calendar window; the scales the generations in this
// module write — seconds, milliseconds and nanoseconds — all keep working. A
// value outside the window, or before the epoch, is absent, and the caller
// falls back to the row's own time_updated.
func TestEpochMSReadsSecondsMillisecondsAndNanoseconds(t *testing.T) {
	when := time.Date(2025, 3, 18, 11, 23, 30, 0, time.UTC)
	windowStart := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	windowEnd := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		v    int64
		want time.Time
	}{
		{"seconds", when.Unix(), when},
		{"milliseconds", when.UnixMilli(), when},
		{"nanoseconds", when.UnixNano(), when},
		{"absent", 0, time.Time{}},
		{"before the epoch", -1, time.Time{}},
		{"seconds past the window", 999999999999, time.Time{}},
		{"milliseconds past the window", 999999999999999, time.Time{}},
		{"the saturation value", math.MaxInt64, time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EpochMS(tt.v)
			if !got.Equal(tt.want) {
				t.Fatalf("EpochMS(%d) = %v, want %v", tt.v, got, tt.want)
			}
			if !got.IsZero() && (got.Before(windowStart) || !got.Before(windowEnd)) {
				t.Errorf("EpochMS(%d) = %v, outside the plausible window [%v %v)", tt.v, got, windowStart, windowEnd)
			}
		})
	}
}
