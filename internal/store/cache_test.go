package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/ledger"
	"github.com/zzstar101/mytoken/internal/model"
)

// priced is a token-dependent cost fn so the counterfactual (which moves miss
// tokens from Input/CacheWrite into CacheRead) yields a nonzero MissUSD.
func priced(e model.UsageEvent) (float64, bool) {
	return float64(e.Tokens.Input)*3e-6 + float64(e.Tokens.CacheRead)*0.3e-6 + float64(e.Tokens.CacheWrite)*3.75e-6, true
}

type verdict struct {
	expected, miss int64
	cause          string
	usd            float64
}

// dbVerdicts reads the persisted per-request cache verdicts keyed by dedup_key.
func dbVerdicts(t *testing.T, st *Store) map[string]verdict {
	t.Helper()
	rows, err := st.DB().Query(`SELECT dedup_key,cache_expected,cache_miss,miss_cause,miss_cost FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]verdict{}
	for rows.Next() {
		var k, cause string
		var exp, miss int64
		var usd float64
		if err := rows.Scan(&k, &exp, &miss, &cause, &usd); err != nil {
			t.Fatal(err)
		}
		out[k] = verdict{exp, miss, cause, usd}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// chainReq pairs one session's ordered chain with the dedup_keys of its requests,
// since the store analyzes per (harness,session) but the ledger sees one chain.
type chainReq struct {
	reqs []ledger.Request
	keys []string
}

// assertMatchesLedger checks the DB verdicts equal ledger.Analyze over every
// chain, with the uncacheable convention (Expected=-1) applied, and that misses
// carry a MissUSD. keys[i] is the dedup_key of reqs[i].
func assertMatchesLedger(t *testing.T, st *Store, chains ...chainReq) {
	t.Helper()
	got := dbVerdicts(t, st)
	want := map[string]ledger.Entry{}
	for _, ch := range chains {
		for i, e := range ledger.Analyze(ch.reqs) {
			want[ch.keys[i]] = e
		}
	}
	if len(got) != len(want) {
		t.Fatalf("stored %d verdicts, chains produce %d", len(got), len(want))
	}
	sawMiss := false
	for key, w := range want {
		g, ok := got[key]
		if !ok {
			t.Fatalf("no stored verdict for %q", key)
		}
		exp := w.Expected
		if w.Uncacheable {
			exp = -1
		}
		if g.expected != exp || g.miss != w.Miss || g.cause != string(w.Cause) {
			t.Fatalf("%s: db={exp:%d miss:%d cause:%q}, ledger={exp:%d miss:%d cause:%q uncacheable:%v}",
				key, g.expected, g.miss, g.cause, exp, w.Miss, w.Cause, w.Uncacheable)
		}
		if w.Miss > 0 {
			sawMiss = true
			if g.usd <= 0 {
				t.Fatalf("%s: miss=%d but MissUSD=%v, want >0", key, w.Miss, g.usd)
			}
		}
	}
	if !sawMiss {
		t.Fatalf("test data produced no miss; add a case that exercises the miss path")
	}
}

// TestCacheLedgerMigration upgrades a pre-0.3 on-disk DB (legacy events table, no
// cache columns) in place and verifies the ledger is built and stays stable.
func TestCacheLedgerMigration(t *testing.T) {
	const legacyEventsSchema = `CREATE TABLE events(harness TEXT NOT NULL,dedup_key TEXT NOT NULL,session_id TEXT NOT NULL,parent_id TEXT NOT NULL DEFAULT '',project TEXT NOT NULL DEFAULT '',timestamp INTEGER NOT NULL,model TEXT NOT NULL,provider TEXT NOT NULL DEFAULT '',base_url TEXT NOT NULL DEFAULT '',input INTEGER NOT NULL,output INTEGER NOT NULL,cache_read INTEGER NOT NULL,cache_write INTEGER NOT NULL,reasoning INTEGER NOT NULL,log_cost REAL,resolved_provider TEXT NOT NULL,attrib TEXT NOT NULL,cost REAL NOT NULL,PRIMARY KEY(harness,dedup_key))`
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// k1 caches a 3072-token prefix; k2 re-sends it as fresh input and reads nothing.
	_, err = db.Exec(legacyEventsSchema + `;
INSERT INTO events VALUES('codex','k1','s1','','',1000,'gpt-5','openai','',0,0,0,3072,0,NULL,'openai','log',0),
('codex','k2','s1','','',61000,'gpt-5','openai','',3584,0,0,0,0,NULL,'openai','log',0);`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	reqs := []ledger.Request{
		{At: time.Unix(0, 1000).UTC(), Harness: "codex", Provider: "openai", Model: "gpt-5", CacheWrite: 3072},
		{At: time.Unix(0, 61000).UTC(), Harness: "codex", Provider: "openai", Model: "gpt-5", Input: 3584},
	}
	ctx := context.Background()
	for round := 0; round < 2; round++ {
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		// app.go ensures prices right after Open; that triggers the ledger build.
		if err = st.EnsurePrices(ctx, "fp-priced", priced); err != nil {
			t.Fatal(err)
		}
		assertMatchesLedger(t, st, chainReq{reqs, []string{"k1", "k2"}})
		if ver, err := st.Setting(ctx, "ledger_version"); err != nil || ver != ledgerVersion {
			t.Fatalf("round %d: ledger_version=%q err=%v, want %q", round, ver, err, ledgerVersion)
		}
		var rollReq, rollCacheable int64
		if err = st.DB().QueryRow(`SELECT sum(requests),sum(cacheable) FROM cache_daily`).Scan(&rollReq, &rollCacheable); err != nil {
			t.Fatal(err)
		}
		if rollReq != 2 || rollCacheable != 2 {
			t.Fatalf("round %d: rollup requests=%d cacheable=%d, want 2/2", round, rollReq, rollCacheable)
		}
		st.Close()
	}
}

func TestCacheLedgerFunctional(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	base := time.Date(2026, 1, 2, 10, 0, 0, 0, time.Local)
	// s1: cold start -> cache hit -> compact-boundary miss. s2: no cache activity.
	events := []model.UsageEvent{
		{Harness: model.Codex, DedupKey: "a1", SessionID: "s1", Timestamp: base, Model: "gpt-5", Provider: "openai", Tokens: model.Tokens{Input: 3000, CacheWrite: 3000}},
		{Harness: model.Codex, DedupKey: "a2", SessionID: "s1", Timestamp: base.Add(time.Minute), Model: "gpt-5", Provider: "openai", Tokens: model.Tokens{Input: 500, CacheRead: 3000}},
		{Harness: model.Codex, DedupKey: "a3", SessionID: "s1", Timestamp: base.Add(2 * time.Minute), Model: "gpt-5", Provider: "openai", Boundary: model.BoundaryCompact, Tokens: model.Tokens{Input: 3500, CacheWrite: 3500}},
		{Harness: model.Codex, DedupKey: "b1", SessionID: "s2", Timestamp: base, Model: "gpt-5", Provider: "openai", Tokens: model.Tokens{Input: 100}},
	}
	if err = st.Commit(ctx, model.Codex, "test", harness.Batch{Events: events}, nil); err != nil {
		t.Fatal(err)
	}
	// Before pricing, verdicts exist but MissUSD is 0.
	if pre := dbVerdicts(t, st)["a3"]; pre.usd != 0 {
		t.Fatalf("MissUSD should be 0 before EnsurePrices, got %v", pre.usd)
	}
	if err = st.EnsurePrices(ctx, "fp-priced", priced); err != nil {
		t.Fatal(err)
	}
	s1 := []ledger.Request{
		{At: base.UTC(), Harness: "codex", Provider: "openai", Model: "gpt-5", Input: 3000, CacheWrite: 3000},
		{At: base.Add(time.Minute).UTC(), Harness: "codex", Provider: "openai", Model: "gpt-5", Input: 500, CacheRead: 3000},
		{At: base.Add(2 * time.Minute).UTC(), Harness: "codex", Provider: "openai", Model: "gpt-5", Input: 3500, CacheWrite: 3500, Boundary: model.BoundaryCompact},
	}
	s2 := []ledger.Request{
		{At: base.UTC(), Harness: "codex", Provider: "openai", Model: "gpt-5", Input: 100},
	}
	assertMatchesLedger(t, st, chainReq{s1, []string{"a1", "a2", "a3"}}, chainReq{s2, []string{"b1"}})
	// s2 has no cache activity, so it is uncacheable (Expected=-1) and excluded from
	// the cacheable rollup even though it still counts as a request.
	if b1 := dbVerdicts(t, st)["b1"]; b1.expected != -1 {
		t.Fatalf("b1 uncacheable Expected=%d, want -1", b1.expected)
	}
	var rollReq, rollCacheable int64
	if err = st.DB().QueryRow(`SELECT sum(requests),sum(cacheable) FROM cache_daily`).Scan(&rollReq, &rollCacheable); err != nil {
		t.Fatal(err)
	}
	if rollReq != 4 || rollCacheable != 3 {
		t.Fatalf("rollup requests=%d cacheable=%d, want 4/3", rollReq, rollCacheable)
	}
}

func TestCacheLedgerRebuildOnVersionMismatch(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	base := time.Date(2026, 1, 2, 10, 0, 0, 0, time.Local)
	events := []model.UsageEvent{
		{Harness: model.Codex, DedupKey: "a1", SessionID: "s1", Timestamp: base, Model: "gpt-5", Provider: "openai", Tokens: model.Tokens{CacheWrite: 3072}},
		{Harness: model.Codex, DedupKey: "a2", SessionID: "s1", Timestamp: base.Add(time.Minute), Model: "gpt-5", Provider: "openai", Tokens: model.Tokens{Input: 3584}},
	}
	if err = st.Commit(ctx, model.Codex, "test", harness.Batch{Events: events}, nil); err != nil {
		t.Fatal(err)
	}
	reqs := []ledger.Request{
		{At: base.UTC(), Harness: "codex", Provider: "openai", Model: "gpt-5", CacheWrite: 3072},
		{At: base.Add(time.Minute).UTC(), Harness: "codex", Provider: "openai", Model: "gpt-5", Input: 3584},
	}
	// Simulate a stale ledger (algorithm version bumped) with wiped verdicts.
	if err = st.SetSetting(ctx, "ledger_version", "0"); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE event_data SET cache_expected=0,cache_miss=0,miss_cause='',miss_cost=0`); err != nil {
		t.Fatal(err)
	}
	if err = st.EnsurePrices(ctx, "fp-priced", priced); err != nil {
		t.Fatal(err)
	}
	assertMatchesLedger(t, st, chainReq{reqs, []string{"a1", "a2"}})
	if ver, err := st.Setting(ctx, "ledger_version"); err != nil || ver != ledgerVersion {
		t.Fatalf("ledger_version=%q after rebuild, want %q (err=%v)", ver, ledgerVersion, err)
	}
}
