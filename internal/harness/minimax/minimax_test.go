package minimax

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

// ledgerSchema is the official table (packages/local-runtime-v2/src/infra/db/
// schema/usage.ts), used to build the small databases these tests parse.
const ledgerSchema = `
CREATE TABLE local_runtime_token_usage (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  agent_name TEXT NOT NULL,
  framework_type TEXT NOT NULL,
  turn_id TEXT,
  model TEXT,
  ts INTEGER NOT NULL,
  input_tokens INTEGER NOT NULL,
  output_tokens INTEGER NOT NULL,
  reasoning_tokens INTEGER NOT NULL,
  cache_read_tokens INTEGER NOT NULL,
  cache_write_tokens INTEGER NOT NULL,
  cost_usd REAL,
  raw TEXT
);`

const (
	rowColumns = `(session_id, agent_name, framework_type, turn_id, model, ts,
   input_tokens, output_tokens, reasoning_tokens, cache_read_tokens,
   cache_write_tokens, cost_usd, raw)`
)

// insert builds one INSERT statement from literal values.
func insert(values string) string {
	return "INSERT INTO local_runtime_token_usage " + rowColumns + " VALUES " + values + ";"
}

func seedLedger(t testing.TB, root, script string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(dbRelPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	execSQL(t, path, script)
	return path
}

func execSQL(t testing.TB, path, script string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	if _, err := db.Exec(script); err != nil {
		t.Fatalf("exec on %s: %v", path, err)
	}
}

// parseRoot discovers every source under root and parses each from a cold
// cursor.
func parseRoot(t testing.TB, root string) harness.Batch {
	t.Helper()
	ctx := context.Background()
	p := NewWithRoots(root)
	srcs, err := p.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover(%s): %v", root, err)
	}
	var out harness.Batch
	for _, src := range srcs {
		b, err := p.Parse(ctx, src, harness.Cursor{})
		if err != nil {
			t.Fatalf("Parse(%s): %v", src.Path, err)
		}
		out.Events = append(out.Events, b.Events...)
		out.Sessions = append(out.Sessions, b.Sessions...)
		out.Next = b.Next
	}
	return out
}

func oneSource(t testing.TB, root string) harness.Source {
	t.Helper()
	srcs, err := NewWithRoots(root).Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 1 {
		t.Fatalf("Discover returned %d sources, want 1: %+v", len(srcs), srcs)
	}
	return srcs[0]
}

func TestHarnessIdentity(t *testing.T) {
	if got, want := string(Harness), "minimax-code"; got != want {
		t.Errorf("Harness = %q, want %q", got, want)
	}
	if got := New().Harness(); got != Harness {
		t.Errorf("Parser.Harness() = %q, want %q", got, Harness)
	}
	if !Harness.Known() {
		t.Log("model.Harness has no MiniMax Code constant yet; the registry wiring adds it (see docs/verification/minimax-code.md)")
	}
}

// TestParseMapsLedgerRows pins the row-to-event mapping, including the
// documented answers for values the ledger does not carry.
func TestParseMapsLedgerRows(t *testing.T) {
	root := t.TempDir()
	seedLedger(t, root, ledgerSchema+insert(`
  ('s1','build','pi-agent','turn-1','MiniMax-M2.7',1772445600000,
   100,20,5,7,3,0.5,'{"text":"SENTINEL_PRIVATE_TEXT"}'),
  ('s1','build','pi-agent','turn-2',NULL,1772445900000,
   0,0,0,0,0,NULL,'{"text":"SENTINEL_PRIVATE_TEXT"}'),
  ('s1','build','pi-agent','turn-3','',1772446200000,
   0,0,0,0,0,0,'{"text":"SENTINEL_PRIVATE_TEXT"}')`))

	batch := parseRoot(t, root)
	if len(batch.Events) != 3 {
		t.Fatalf("parsed %d events, want 3: %+v", len(batch.Events), batch.Events)
	}
	first := batch.Events[0]
	if first.Harness != Harness {
		t.Errorf("Harness = %q, want %q", first.Harness, Harness)
	}
	if want := "minimax-code:s1:1"; first.DedupKey != want {
		t.Errorf("DedupKey = %q, want %q", first.DedupKey, want)
	}
	if want := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC); !first.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %v, want %v", first.Timestamp, want)
	}
	if first.Model != "MiniMax-M2.7" {
		t.Errorf("Model = %q, want MiniMax-M2.7", first.Model)
	}
	if want := (model.Tokens{Input: 100, Output: 20, CacheRead: 7, CacheWrite: 3, Reasoning: 5}); first.Tokens != want {
		t.Errorf("Tokens = %+v, want %+v", first.Tokens, want)
	}
	if first.CostUSD == nil || *first.CostUSD != 0.5 {
		t.Errorf("CostUSD = %v, want 0.5", first.CostUSD)
	}
	// The ledger has no provider, request id, project path or compaction marker:
	// they stay empty rather than being guessed.
	if first.Provider != "" || first.RequestID != "" || first.ProjectPath != "" || first.BaseURL != "" || first.Boundary != "" {
		t.Errorf("event carries guessed metadata: %+v", first)
	}
	// A null model is not a model name; a null cost is unknown, but an explicit
	// zero is the provider's own "this turn was free" and is kept as $0.
	nullRow, zeroRow := batch.Events[1], batch.Events[2]
	if nullRow.Model != unknownModel || zeroRow.Model != unknownModel {
		t.Errorf("null/empty model = %q/%q, want %q", nullRow.Model, zeroRow.Model, unknownModel)
	}
	if nullRow.CostUSD != nil {
		t.Errorf("event 2 CostUSD = %v, want nil for a NULL cost", *nullRow.CostUSD)
	}
	if zeroRow.CostUSD == nil || *zeroRow.CostUSD != 0 {
		t.Errorf("event 3 CostUSD = %v, want a pointer to 0 for an explicit zero cost", zeroRow.CostUSD)
	}
	if len(batch.Sessions) != 1 {
		t.Fatalf("parsed %d sessions, want 1: %+v", len(batch.Sessions), batch.Sessions)
	}
	sess := batch.Sessions[0]
	if sess.SessionID != "s1" || sess.Harness != Harness {
		t.Errorf("session = %+v, want session s1 of %q", sess, Harness)
	}
	if want := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC); !sess.StartedAt.Equal(want) {
		t.Errorf("StartedAt = %v, want %v", sess.StartedAt, want)
	}
	if want := time.Date(2026, 3, 2, 10, 10, 0, 0, time.UTC); !sess.UpdatedAt.Equal(want) {
		t.Errorf("UpdatedAt = %v, want %v", sess.UpdatedAt, want)
	}
	if sess.Title != "" {
		t.Errorf("Title = %q, want empty: the ledger has no message text", sess.Title)
	}
}

// TestParseSkipsRowsWithoutSession keeps unattributable rows out of the output
// while still advancing the cursor past them, so they are not retried forever.
func TestParseSkipsRowsWithoutSession(t *testing.T) {
	root := t.TempDir()
	seedLedger(t, root, ledgerSchema+insert(`
  ('','build','pi-agent','turn-1','MiniMax-M2.7',1772445600000,1,1,1,1,1,NULL,'{}'),
  ('   ','build','pi-agent','turn-2','MiniMax-M2.7',1772445600001,2,2,2,2,2,NULL,'{}'),
  ('s1','build','pi-agent','turn-3','MiniMax-M2.7',1772445600002,3,3,3,3,3,NULL,'{}')`))

	p := NewWithRoots(root)
	src := oneSource(t, root)
	ctx := context.Background()
	batch, err := p.Parse(ctx, src, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(batch.Events) != 1 || batch.Events[0].DedupKey != "minimax-code:s1:3" {
		t.Fatalf("events = %+v, want only the row with a session id", batch.Events)
	}
	if batch.Next.Extra == "" {
		t.Fatal("Next.Extra is empty: the cursor did not advance past the skipped rows")
	}
	again, err := p.Parse(ctx, src, batch.Next)
	if err != nil {
		t.Fatalf("Parse from Next: %v", err)
	}
	if len(again.Events) != 0 {
		t.Errorf("re-parsing from Next re-emitted %d events: %+v", len(again.Events), again.Events)
	}
}

// TestParseIncompatibleSchema ignores a database that is not MiniMax Code's
// ledger (or is a generation this parser does not know) instead of guessing at
// its columns.
func TestParseIncompatibleSchema(t *testing.T) {
	cases := []struct {
		name   string
		script string
	}{
		{
			name: "no ledger table",
			script: `CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT);
				INSERT INTO settings VALUES ('api_key','sk-minimax-DO-NOT-LEAK');`,
		},
		{
			name: "missing required columns",
			script: `CREATE TABLE local_runtime_token_usage (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					session_id TEXT NOT NULL);
				INSERT INTO local_runtime_token_usage (session_id) VALUES ('s1');`,
		},
		{
			name: "ts is not an integer",
			script: `CREATE TABLE local_runtime_token_usage (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					session_id TEXT NOT NULL,
					ts TEXT NOT NULL,
					input_tokens INTEGER NOT NULL,
					output_tokens INTEGER NOT NULL);
				INSERT INTO local_runtime_token_usage (session_id, ts, input_tokens, output_tokens)
					VALUES ('s1','2026-03-02T10:00:00Z',1,1);`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			seedLedger(t, root, tc.script)
			src := oneSource(t, root)
			batch, err := NewWithRoots(root).Parse(context.Background(), src, harness.Cursor{})
			if err != nil {
				t.Fatalf("Parse on an incompatible database returned an error: %v", err)
			}
			if len(batch.Events) != 0 || len(batch.Sessions) != 0 {
				t.Fatalf("an incompatible schema produced %d events / %d sessions", len(batch.Events), len(batch.Sessions))
			}
			// The file is still remembered, so the scanner does not reopen it on
			// every pass; nothing about it is claimed as supported.
			if batch.Next.Fingerprint == "" && batch.Next.Size == 0 {
				t.Errorf("Next cursor was not recorded: %+v", batch.Next)
			}
		})
	}
}

// TestParseOptionalColumnsAbsent accepts a ledger generation without the
// optional classes: they become the documented zero values, never guesses.
func TestParseOptionalColumnsAbsent(t *testing.T) {
	root := t.TempDir()
	seedLedger(t, root, `
CREATE TABLE local_runtime_token_usage (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  ts INTEGER NOT NULL,
  input_tokens INTEGER NOT NULL,
  output_tokens INTEGER NOT NULL);
INSERT INTO local_runtime_token_usage (session_id, ts, input_tokens, output_tokens)
  VALUES ('s1', 1772445600000, 10, 4);`)

	batch := parseRoot(t, root)
	if len(batch.Events) != 1 {
		t.Fatalf("parsed %d events, want 1", len(batch.Events))
	}
	ev := batch.Events[0]
	if want := (model.Tokens{Input: 10, Output: 4}); ev.Tokens != want {
		t.Errorf("Tokens = %+v, want %+v", ev.Tokens, want)
	}
	if ev.Model != unknownModel || ev.CostUSD != nil {
		t.Errorf("missing model/cost should be unknown, got %q / %v", ev.Model, ev.CostUSD)
	}
}

// TestParseIncompatibleOptionalColumnTypes degrades a column whose declared
// type is not the one the schema promises instead of reading it blindly.
func TestParseIncompatibleOptionalColumnTypes(t *testing.T) {
	root := t.TempDir()
	seedLedger(t, root, `
CREATE TABLE local_runtime_token_usage (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  ts INTEGER NOT NULL,
  input_tokens INTEGER NOT NULL,
  output_tokens INTEGER NOT NULL,
  model BLOB,
  reasoning_tokens TEXT,
  cache_read_tokens INTEGER,
  cache_write_tokens INTEGER,
  cost_usd TEXT);
INSERT INTO local_runtime_token_usage
  (session_id, ts, input_tokens, output_tokens, model, reasoning_tokens, cache_read_tokens, cache_write_tokens, cost_usd)
  VALUES ('s1', 1772445600000, 10, 4, x'0102', 'lots', 6, 2, '1.25');`)

	batch := parseRoot(t, root)
	if len(batch.Events) != 1 {
		t.Fatalf("parsed %d events, want 1", len(batch.Events))
	}
	ev := batch.Events[0]
	if ev.Model != unknownModel {
		t.Errorf("Model = %q, want %q", ev.Model, unknownModel)
	}
	if ev.Tokens.Reasoning != 0 {
		t.Errorf("Reasoning = %d, want 0 for a non-integer column", ev.Tokens.Reasoning)
	}
	if ev.Tokens.CacheRead != 6 || ev.Tokens.CacheWrite != 2 {
		t.Errorf("Tokens = %+v, want cache read/write 6/2", ev.Tokens)
	}
	if ev.CostUSD != nil {
		t.Errorf("CostUSD = %v, want nil for a non-real column", *ev.CostUSD)
	}
}

// TestParseClampsUnusableNumbers keeps a negative or null counter from becoming
// a negative event, and separates the three cost cases the official writer can
// store: a finite zero (a trustworthy free turn), a negative amount, and a
// non-finite REAL.
func TestParseClampsUnusableNumbers(t *testing.T) {
	root := t.TempDir()
	seedLedger(t, root, `
CREATE TABLE local_runtime_token_usage (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  ts INTEGER NOT NULL,
  input_tokens INTEGER NOT NULL,
  output_tokens INTEGER NOT NULL,
  reasoning_tokens INTEGER,
  cache_read_tokens INTEGER,
  cache_write_tokens INTEGER,
  cost_usd REAL);
INSERT INTO local_runtime_token_usage
  (session_id, ts, input_tokens, output_tokens, reasoning_tokens, cache_read_tokens, cache_write_tokens, cost_usd)
  VALUES ('s1', 1772445600000, -5, 4, NULL, NULL, NULL, -0.5),
         ('s1', 1772445900000, 7, 4, NULL, NULL, NULL, 0),
         ('s1', 1772446200000, 1, 1, NULL, NULL, NULL, 9e999),
         ('s1', 1772446500000, 1, 1, NULL, NULL, NULL, -9e999);`)

	batch := parseRoot(t, root)
	if len(batch.Events) != 4 {
		t.Fatalf("parsed %d events, want 4: %+v", len(batch.Events), batch.Events)
	}
	if want := (model.Tokens{Output: 4}); batch.Events[0].Tokens != want {
		t.Errorf("Tokens = %+v, want %+v", batch.Events[0].Tokens, want)
	}
	if got := batch.Events[0].CostUSD; got != nil {
		t.Errorf("CostUSD = %v, want nil for a negative cost", *got)
	}
	if got := batch.Events[1].CostUSD; got == nil || *got != 0 {
		t.Errorf("CostUSD = %v, want a pointer to 0 for an explicit zero cost", got)
	}
	for _, i := range []int{2, 3} {
		if got := batch.Events[i].CostUSD; got != nil {
			t.Errorf("event %d CostUSD = %v, want nil for a non-finite cost", i+1, *got)
		}
	}
}

// TestParseTimestampStaysUnknown pins that a zero/absent timestamp is reported
// as the zero time (the store treats it as unknown) and never invented.
func TestParseTimestampStaysUnknown(t *testing.T) {
	root := t.TempDir()
	seedLedger(t, root, ledgerSchema+insert(`
  ('s1','build','pi-agent','turn-1','MiniMax-M2.7',0,1,1,0,0,0,NULL,'{}')`))

	batch := parseRoot(t, root)
	if len(batch.Events) != 1 {
		t.Fatalf("parsed %d events, want 1", len(batch.Events))
	}
	if !batch.Events[0].Timestamp.IsZero() {
		t.Errorf("Timestamp = %v, want the zero time", batch.Events[0].Timestamp)
	}
	if !batch.Sessions[0].StartedAt.IsZero() || !batch.Sessions[0].UpdatedAt.IsZero() {
		t.Errorf("session times = %v/%v, want zero", batch.Sessions[0].StartedAt, batch.Sessions[0].UpdatedAt)
	}
}

// TestParseIncrementalByRowID appends to the same database and expects only the
// new rows, with a cursor that does not move when nothing changed.
func TestParseIncrementalByRowID(t *testing.T) {
	root := t.TempDir()
	path := seedLedger(t, root, ledgerSchema+insert(`
  ('s1','build','pi-agent','turn-1','MiniMax-M2.7',1772445600000,1,1,1,1,1,0.1,'{}'),
  ('s1','build','pi-agent','turn-2','MiniMax-M2.7',1772445601000,2,2,2,2,2,0.2,'{}')`))

	p := NewWithRoots(root)
	src := oneSource(t, root)
	ctx := context.Background()

	first, err := p.Parse(ctx, src, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(first.Events) != 2 {
		t.Fatalf("cold parse produced %d events, want 2", len(first.Events))
	}
	stable, err := p.Parse(ctx, src, first.Next)
	if err != nil {
		t.Fatalf("Parse on an unchanged ledger: %v", err)
	}
	if len(stable.Events) != 0 {
		t.Errorf("unchanged ledger re-emitted %d events", len(stable.Events))
	}
	if stable.Next != first.Next {
		t.Errorf("cursor moved on an unchanged ledger:\n was %+v\n got %+v", first.Next, stable.Next)
	}

	execSQL(t, path, insert(`
  ('s2','build','pi-agent','turn-1','MiniMax-M3',1772445602000,3,3,3,3,3,0.3,'{}')`))
	grown, err := p.Parse(ctx, src, stable.Next)
	if err != nil {
		t.Fatalf("Parse after growth: %v", err)
	}
	if len(grown.Events) != 1 || grown.Events[0].SessionID != "s2" {
		t.Fatalf("incremental parse produced %+v, want only the appended row", grown.Events)
	}
	if want := time.Date(2026, 3, 2, 10, 0, 2, 0, time.UTC); !grown.Events[0].Timestamp.Equal(want) {
		t.Errorf("Timestamp = %v, want %v", grown.Events[0].Timestamp, want)
	}
	if len(grown.Sessions) != 1 || grown.Sessions[0].SessionID != "s2" {
		t.Errorf("incremental sessions = %+v, want only s2", grown.Sessions)
	}
	last, err := p.Parse(ctx, src, grown.Next)
	if err != nil {
		t.Fatalf("Parse at the end: %v", err)
	}
	if len(last.Events) != 0 || last.Next != grown.Next {
		t.Errorf("cursor/events moved with no new rows: %+v / %+v", last.Events, last.Next)
	}
}

// TestParseLedgerResetRescans replaces the whole database with an unrelated one,
// including one whose row ids overlap the previous watermark: the watermark row
// no longer matches, so the parser re-reads from the start and the stable
// DedupKey keeps the result idempotent.
func TestParseLedgerResetRescans(t *testing.T) {
	root := t.TempDir()
	path := seedLedger(t, root, ledgerSchema+insert(`
  ('s1','build','pi-agent','turn-1','MiniMax-M2.7',1772445600000,1,1,1,1,1,0.1,'{}'),
  ('s1','build','pi-agent','turn-2','MiniMax-M2.7',1772445601000,2,2,2,2,2,0.2,'{}')`))

	p := NewWithRoots(root)
	src := oneSource(t, root)
	ctx := context.Background()
	first, err := p.Parse(ctx, src, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(first.Events) != 2 {
		t.Fatalf("cold parse produced %d events, want 2", len(first.Events))
	}

	// A different ledger: same file location, same row ids, different contents.
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove: %v", err)
		}
	}
	execSQL(t, path, ledgerSchema+insert(`
  ('s9','build','pi-agent','turn-1','MiniMax-M3',1772452800000,9,9,9,9,9,0.9,'{}'),
  ('s9','build','pi-agent','turn-2','MiniMax-M3',1772452801000,8,8,8,8,8,0.8,'{}')`))

	reset, err := p.Parse(ctx, src, first.Next)
	if err != nil {
		t.Fatalf("Parse after replacement: %v", err)
	}
	if len(reset.Events) != 2 {
		t.Fatalf("replaced ledger produced %d events, want a full re-read (2)", len(reset.Events))
	}
	for _, ev := range reset.Events {
		if !strings.HasPrefix(ev.DedupKey, "minimax-code:s9:") {
			t.Errorf("DedupKey = %q, want the replaced ledger's own keys", ev.DedupKey)
		}
	}
}

// TestParseIsIdempotent re-reads the same database from a cold cursor and gets
// exactly the same dedup keys.
func TestParseIsIdempotent(t *testing.T) {
	root := t.TempDir()
	seedLedger(t, root, ledgerSchema+insert(`
  ('s1','build','pi-agent','turn-1','MiniMax-M2.7',1772445600000,1,1,1,1,1,0.1,'{}'),
  ('s2','build','pi-agent','turn-1','MiniMax-M3',1772445601000,2,2,2,2,2,0.2,'{}')`))
	first := parseRoot(t, root)
	second := parseRoot(t, root)
	if len(first.Events) != len(second.Events) {
		t.Fatalf("event counts differ: %d vs %d", len(first.Events), len(second.Events))
	}
	// Compare the serialized events: CostUSD is a pointer, so struct identity
	// would compare allocations rather than the values they carry.
	a, err := json.Marshal(first.Events)
	if err != nil {
		t.Fatalf("marshal first: %v", err)
	}
	b, err := json.Marshal(second.Events)
	if err != nil {
		t.Fatalf("marshal second: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("re-parsing from a cold cursor changed the events:\n %s\n %s", a, b)
	}
	for i := range first.Events {
		if first.Events[i].DedupKey != second.Events[i].DedupKey {
			t.Errorf("event %d key moved: %q vs %q", i, first.Events[i].DedupKey, second.Events[i].DedupKey)
		}
	}
}

// TestParseNeverReadsOtherTables feeds private conversation text and an API key
// through unrelated tables and the usage row's own raw column, then proves none
// of it reached the parsed output, and that the source database was not touched.
func TestParseNeverReadsOtherTables(t *testing.T) {
	root := t.TempDir()
	path := seedLedger(t, root, ledgerSchema+`
CREATE TABLE runtime_message_text (id INTEGER PRIMARY KEY AUTOINCREMENT, body TEXT NOT NULL);
INSERT INTO runtime_message_text (body) VALUES ('SENTINEL_PRIVATE_TEXT sk-minimax-DO-NOT-LEAK');
`+insert(`
  ('s1','build','pi-agent','turn-1','MiniMax-M2.7',1772445600000,1,1,1,1,1,0.1,'{"text":"SENTINEL_PRIVATE_TEXT sk-minimax-DO-NOT-LEAK"}')`))

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read db: %v", err)
	}
	batch := parseRoot(t, root)
	blob, err := json.Marshal(batch)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, secret := range []string{"SENTINEL_PRIVATE_TEXT", "sk-minimax-DO-NOT-LEAK"} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("private text %q reached the parsed output: %s", secret, blob)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read db: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("parsing modified the source database")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(path + suffix); err == nil {
			t.Errorf("parsing left a %s sidecar behind", suffix)
		}
	}
}

// TestOpenReadOnlyRejectsWrites pins the connection the parser uses: read-only
// and query_only, so no code path here can write to a user's ledger.
func TestOpenReadOnlyRejectsWrites(t *testing.T) {
	root := t.TempDir()
	path := seedLedger(t, root, ledgerSchema+insert(`
  ('s1','build','pi-agent','turn-1','MiniMax-M2.7',1772445600000,1,1,1,1,1,0.1,'{}')`))

	db, err := openReadOnly(path)
	if err != nil {
		t.Fatalf("openReadOnly: %v", err)
	}
	defer db.Close()
	var queryOnly int
	if err := db.QueryRow("PRAGMA query_only").Scan(&queryOnly); err != nil {
		t.Fatalf("PRAGMA query_only: %v", err)
	}
	if queryOnly != 1 {
		t.Errorf("PRAGMA query_only = %d, want 1", queryOnly)
	}
	if _, err := db.Exec(insert(`
  ('s2','build','pi-agent','turn-1','MiniMax-M3',1772445601000,2,2,2,2,2,0.2,'{}')`)); err == nil {
		t.Error("a write through the parser's connection succeeded")
	}
	var rows int
	if err := db.QueryRow("SELECT COUNT(*) FROM local_runtime_token_usage").Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Errorf("row count = %d, want 1", rows)
	}
}

// TestTableColumnsAndSchemaProbe covers the probe directly: declared types are
// reported as written, and a ledger without the required integer columns is not
// claimed as supported.
func TestTableColumnsAndSchemaProbe(t *testing.T) {
	root := t.TempDir()
	path := seedLedger(t, root, ledgerSchema)
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatalf("openReadOnly: %v", err)
	}
	defer db.Close()
	cols, err := tableColumns(context.Background(), db, usageTable)
	if err != nil {
		t.Fatalf("tableColumns: %v", err)
	}
	if got := cols["ts"]; !strings.EqualFold(got, "INTEGER") {
		t.Errorf("ts declared type = %q, want INTEGER", got)
	}
	if got := cols["cost_usd"]; !strings.EqualFold(got, "REAL") {
		t.Errorf("cost_usd declared type = %q, want REAL", got)
	}
	if !supportedSchema(cols) {
		t.Error("the official schema should be supported")
	}
	// A table that does not exist is an empty schema, not a failed probe.
	missing, err := tableColumns(context.Background(), db, "definitely_not_a_ledger")
	if err != nil || len(missing) != 0 {
		t.Errorf("tableColumns(missing) = %v, %v; want an empty map and no error", missing, err)
	}
	for _, drop := range []string{"id", "session_id", "ts", "input_tokens", "output_tokens"} {
		mutated := map[string]string{}
		for k, v := range cols {
			if k != drop {
				mutated[k] = v
			}
		}
		if supportedSchema(mutated) {
			t.Errorf("schema without %s was accepted", drop)
		}
	}
	if supportedSchema(nil) {
		t.Error("an empty schema was accepted")
	}
}

// TestDiscoverLayouts covers the documented layout, a root that names the
// database itself, a missing root, and the legacy compatibility symlink.
func TestDiscoverLayouts(t *testing.T) {
	ctx := context.Background()

	t.Run("root directory", func(t *testing.T) {
		root := t.TempDir()
		path := seedLedger(t, root, ledgerSchema)
		srcs, err := NewWithRoots(root).Discover(ctx)
		if err != nil || len(srcs) != 1 {
			t.Fatalf("Discover = %+v, %v; want one source", srcs, err)
		}
		if srcs[0].Path != path {
			t.Errorf("source path = %q, want %q", srcs[0].Path, path)
		}
		if srcs[0].Kind != kindSQLite {
			t.Errorf("source kind = %q, want %q", srcs[0].Kind, kindSQLite)
		}
	})

	t.Run("root names the database", func(t *testing.T) {
		root := t.TempDir()
		path := seedLedger(t, root, ledgerSchema)
		srcs, err := NewWithRoots(path).Discover(ctx)
		if err != nil || len(srcs) != 1 || srcs[0].Path != path {
			t.Fatalf("Discover = %+v, %v; want the database itself", srcs, err)
		}
	})

	t.Run("legacy symlink is the same file", func(t *testing.T) {
		home := t.TempDir()
		primary := filepath.Join(home, ".minimax")
		seedLedger(t, primary, ledgerSchema)
		legacy := filepath.Join(home, ".mavis")
		if err := os.Symlink(primary, legacy); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		srcs, err := NewWithRoots(primary, legacy).Discover(ctx)
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if len(srcs) != 1 {
			t.Fatalf("Discover returned %d sources for a symlinked pair, want 1: %+v", len(srcs), srcs)
		}
	})

	t.Run("missing root", func(t *testing.T) {
		srcs, err := NewWithRoots(filepath.Join(t.TempDir(), "absent")).Discover(ctx)
		if err != nil || len(srcs) != 0 {
			t.Fatalf("Discover = %+v, %v; want no sources and no error", srcs, err)
		}
	})

	t.Run("blank root", func(t *testing.T) {
		srcs, err := NewWithRoots("", "   ").Discover(ctx)
		if err != nil || len(srcs) != 0 {
			t.Fatalf("Discover = %+v, %v; want no sources and no error", srcs, err)
		}
	})
}

// TestRootProvenance pins the root resolution order: the mytoken override, then
// the runtime's own public data-dir variables, then the built-in pair.
func TestRootProvenance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MYTOKEN_MINIMAX_DIRS", "")
	t.Setenv("MINIMAX_DATA_DIR", "")
	t.Setenv("MAVIS_DATA_DIR", "")

	roots := provenance(t)
	if len(roots) != 2 {
		t.Fatalf("builtin roots = %+v, want .minimax and .mavis", roots)
	}
	if want := filepath.Join(home, ".minimax"); roots[0].Path != want || roots[0].Origin != harness.OriginBuiltin {
		t.Errorf("root[0] = %+v, want builtin %s", roots[0], want)
	}
	if want := filepath.Join(home, ".mavis"); roots[1].Path != want || roots[1].Origin != harness.OriginBuiltin {
		t.Errorf("root[1] = %+v, want builtin %s", roots[1], want)
	}
	if got := DefaultRoots(); len(got) != 2 || got[0] != roots[0].Path {
		t.Errorf("DefaultRoots() = %+v, want the provenance paths", got)
	}

	legacy := filepath.Join(t.TempDir(), "legacy")
	t.Setenv("MAVIS_DATA_DIR", legacy)
	roots = provenance(t)
	if len(roots) != 1 || roots[0].Path != legacy || roots[0].Origin != harness.OriginEnv || roots[0].Env != "MAVIS_DATA_DIR" {
		t.Errorf("MAVIS_DATA_DIR provenance = %+v", roots)
	}

	primary := filepath.Join(t.TempDir(), "primary")
	t.Setenv("MINIMAX_DATA_DIR", primary)
	roots = provenance(t)
	if len(roots) != 1 || roots[0].Path != primary || roots[0].Env != "MINIMAX_DATA_DIR" {
		t.Errorf("MINIMAX_DATA_DIR should win over MAVIS_DATA_DIR: %+v", roots)
	}

	a, b := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	t.Setenv("MYTOKEN_MINIMAX_DIRS", a+string(os.PathListSeparator)+b)
	roots = provenance(t)
	if len(roots) != 2 || roots[0].Path != a || roots[1].Path != b || roots[0].Env != "MYTOKEN_MINIMAX_DIRS" {
		t.Errorf("MYTOKEN_MINIMAX_DIRS provenance = %+v", roots)
	}

	t.Setenv("MYTOKEN_MINIMAX_DIRS", string(os.PathListSeparator))
	if _, err := New().RootProvenance(); err == nil {
		t.Error("a malformed MYTOKEN_MINIMAX_DIRS list was accepted")
	}
}

// provenance reports the registered parser's default roots.
func provenance(t testing.TB) []harness.Root {
	t.Helper()
	roots, err := New().RootProvenance()
	if err != nil {
		t.Fatalf("RootProvenance: %v", err)
	}
	return roots
}

// TestWithRootsKeepsExplicitRoots pins the Rooted contract: an explicit list
// replaces the defaults and an explicit empty list stays empty.
func TestWithRootsKeepsExplicitRoots(t *testing.T) {
	t.Setenv("MYTOKEN_MINIMAX_DIRS", "")
	t.Setenv("MINIMAX_DATA_DIR", "")
	t.Setenv("MAVIS_DATA_DIR", "")

	base := New()
	if len(base.Roots()) == 0 {
		t.Fatal("the default parser has no roots to replace")
	}
	got := base.WithRoots("/tmp/explicit")
	if roots := got.Roots(); len(roots) != 1 || roots[0] != "/tmp/explicit" {
		t.Fatalf("WithRoots roots = %+v", roots)
	}
	prov, err := got.(*Parser).RootProvenance()
	if err != nil || len(prov) != 1 || prov[0].Origin != harness.OriginExplicit || prov[0].Path != "/tmp/explicit" {
		t.Fatalf("WithRoots provenance = %+v, %v", prov, err)
	}
	sum, err := got.(*Parser).Discover(context.Background())
	if err != nil || len(sum) != 0 {
		t.Errorf("Discover on an absent explicit root = %+v, %v", sum, err)
	}
	if len(base.Roots()) == 0 {
		t.Error("WithRoots mutated the registry parser")
	}

	empty := base.WithRoots()
	if roots := empty.Roots(); len(roots) != 0 {
		t.Errorf("WithRoots() fell back to defaults: %+v", roots)
	}
	blank := base.WithRoots("  ")
	if roots := blank.Roots(); len(roots) != 0 {
		t.Errorf("WithRoots(blank) kept %+v, want no roots", roots)
	}
}

// TestParseReadsWAL pins that rows still living in an un-checkpointed WAL are
// read, which is the state a running MiniMax Code runtime leaves behind.
func TestParseReadsWAL(t *testing.T) {
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "live.sqlite")
	db, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if _, err := db.Exec(ledgerSchema + insert(`
  ('s1','build','pi-agent','turn-1','MiniMax-M2.7',1772445600000,1,2,3,4,5,0.1,'{}')`)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	root := t.TempDir()
	dst := filepath.Join(root, filepath.FromSlash(dbRelPath))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(srcPath + suffix)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("read %s: %v", suffix, err)
		}
		if err := os.WriteFile(dst+suffix, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", suffix, err)
		}
	}
	db.Close()

	batch := parseRoot(t, root)
	if len(batch.Events) != 1 {
		t.Fatalf("parsed %d events from a WAL database, want 1", len(batch.Events))
	}
	if want := (model.Tokens{Input: 1, Output: 2, CacheRead: 4, CacheWrite: 5, Reasoning: 3}); batch.Events[0].Tokens != want {
		t.Errorf("Tokens = %+v, want %+v", batch.Events[0].Tokens, want)
	}
}

// TestParseIsConcurrencySafe runs independent parses at the same time; -race
// turns any shared mutable state into a failure.
func TestParseIsConcurrencySafe(t *testing.T) {
	roots := []string{t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()}
	for i, root := range roots {
		seedLedger(t, root, ledgerSchema+insert(`
  ('s`+strconv.Itoa(i)+`','build','pi-agent','turn-1','MiniMax-M2.7',1772445600000,1,1,1,1,1,0.1,'{}')`))
	}
	type result struct {
		events int
		err    error
	}
	done := make(chan result, len(roots))
	for _, root := range roots {
		go func(root string) {
			ctx := context.Background()
			p := NewWithRoots(root)
			srcs, err := p.Discover(ctx)
			if err != nil {
				done <- result{err: err}
				return
			}
			if len(srcs) == 0 {
				done <- result{err: errors.New("Discover returned no sources")}
				return
			}
			batch, err := p.Parse(ctx, srcs[0], harness.Cursor{})
			done <- result{events: len(batch.Events), err: err}
		}(root)
	}
	for range roots {
		got := <-done
		if got.err != nil {
			t.Errorf("concurrent parse: %v", got.err)
			continue
		}
		if got.events != 1 {
			t.Errorf("concurrent parse produced %d events, want 1", got.events)
		}
	}
}

// TestParseKeepsCursorWhenLedgerIsNotADatabase pins the failure discipline: a
// ledger that cannot be opened was not read, so the parser must hand the
// caller's cursor back untouched and let the next round retry it. Recording the
// file's fingerprint instead makes the next round's fast path skip a file that
// still holds rows nobody consumed.
func TestParseKeepsCursorWhenLedgerIsNotADatabase(t *testing.T) {
	root := t.TempDir()
	path := seedLedger(t, root, `SELECT 1;`)
	if err := os.WriteFile(path, []byte("this is not a sqlite database at all\n"), 0o644); err != nil {
		t.Fatalf("overwrite ledger: %v", err)
	}
	cur := harness.Cursor{}
	batch, err := NewWithRoots(root).Parse(context.Background(), oneSource(t, root), cur)
	if err != nil {
		t.Fatalf("Parse of an unopenable ledger returned an error: %v", err)
	}
	if len(batch.Events) != 0 {
		t.Fatalf("parsed %d events from a ledger that could not be opened", len(batch.Events))
	}
	if batch.Next != cur {
		t.Fatalf("Next = %+v, want the caller's cursor %+v: an unread ledger must not be marked as read", batch.Next, cur)
	}
}

// TestParseKeepsCursorWhenSchemaProbeFails pins the same discipline for the
// probe that follows a successful open: "I could not read the schema" is not
// "the ledger has no usage table", and only the latter may advance the cursor.
// The fixture injects a table that opens fine but cannot be described.
func TestParseKeepsCursorWhenSchemaProbeFails(t *testing.T) {
	root := t.TempDir()
	path := seedLedger(t, root, `CREATE TABLE unrelated(id INTEGER);`)
	execSQL(t, path, `
PRAGMA writable_schema=ON;
INSERT INTO sqlite_master(type,name,tbl_name,rootpage,sql) VALUES
  ('table','local_runtime_token_usage','local_runtime_token_usage',0,
   'CREATE VIRTUAL TABLE local_runtime_token_usage USING nonexistent_module');
PRAGMA writable_schema=OFF;`)

	// The fixture must really break the probe (and not the open): the parser
	// only takes the retry path when the probe is what failed.
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatalf("openReadOnly: %v", err)
	}
	if _, err := tableColumns(context.Background(), db, usageTable); err == nil {
		db.Close()
		t.Fatal("the fixture's schema probe succeeded; it must fail to cover the probe error path")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close probe handle: %v", err)
	}

	cur := harness.Cursor{}
	batch, err := NewWithRoots(root).Parse(context.Background(), oneSource(t, root), cur)
	if err != nil {
		t.Fatalf("Parse with an unreadable schema returned an error: %v", err)
	}
	if batch.Next != cur {
		t.Fatalf("Next = %+v, want the caller's cursor %+v: a failed schema probe must not mark the ledger as read", batch.Next, cur)
	}
}

// TestParseRetriesLedgerThatWasLocked is the user-visible shape of the same
// bug: an exclusive writer holds the ledger for the parser's whole busy timeout,
// so one round consumes nothing; once the writer leaves, rows that were already
// committed must still be read. Marking the file as read on the locked round
// makes the fast path skip it forever.
func TestParseRetriesLedgerThatWasLocked(t *testing.T) {
	root := t.TempDir()
	path := seedLedger(t, root, ledgerSchema+insert(`
  ('s1','build','pi-agent','turn-1','MiniMax-M2.7',1772445600000,
   100,20,5,7,3,0.5,'{"text":"x"}')`))

	ctx := context.Background()
	blocker, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatalf("open blocker: %v", err)
	}
	defer blocker.Close()
	conn, err := blocker.Conn(ctx)
	if err != nil {
		t.Fatalf("blocker conn: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		t.Fatalf("BEGIN EXCLUSIVE: %v", err)
	}

	p := NewWithRoots(root)
	src := oneSource(t, root)
	start := time.Now()
	locked, err := p.Parse(ctx, src, harness.Cursor{})
	lockedFor := time.Since(start)
	t.Logf("parse while locked: took %v, events=%d, err=%v", lockedFor, len(locked.Events), err)
	if err != nil {
		t.Fatalf("Parse while locked returned an error: %v", err)
	}
	if len(locked.Events) != 0 {
		t.Fatalf("parsed %d events while the ledger was locked, want 0", len(locked.Events))
	}
	if lockedFor < openTimeout {
		t.Fatalf("the parse returned after %v, before the %v busy timeout: the fixture did not lock the ledger", lockedFor, openTimeout)
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatalf("ROLLBACK: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close blocker conn: %v", err)
	}

	retry, err := p.Parse(ctx, src, locked.Next)
	if err != nil {
		t.Fatalf("Parse after the writer left returned an error: %v", err)
	}
	if len(retry.Events) != 1 {
		t.Fatalf("after the writer left, parsed %d events with the cursor the locked round returned (%+v), want the 1 committed row", len(retry.Events), locked.Next)
	}
	if got := retry.Events[0].DedupKey; got != "minimax-code:s1:1" {
		t.Errorf("DedupKey = %q, want minimax-code:s1:1", got)
	}
}

// TestParseKeepsCursorWhenContextIsCancelled pins that a cancelled parse does
// not consume anything either, even though it reports its error.
func TestParseKeepsCursorWhenContextIsCancelled(t *testing.T) {
	root := t.TempDir()
	seedLedger(t, root, ledgerSchema)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cur := harness.Cursor{}
	batch, err := NewWithRoots(root).Parse(ctx, oneSource(t, root), cur)
	if err == nil {
		t.Fatal("Parse with a cancelled context returned no error")
	}
	if batch.Next != cur {
		t.Fatalf("Next = %+v, want the caller's cursor %+v", batch.Next, cur)
	}
}
