package ledger

import (
	"database/sql"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	_ "modernc.org/sqlite"
)

// TestReadOnlyCalibration is opt-in and deliberately accepts no arbitrary path.
// It never opens the live user database, selects content, or prints session IDs.
func TestReadOnlyCalibration(t *testing.T) {
	if os.Getenv("MYTOKEN_LEDGER_CALIBRATE") != "1" {
		t.Skip("set MYTOKEN_LEDGER_CALIBRATE=1 to inspect the authorized read-only copy")
	}
	db, err := sql.Open("sqlite", "file:/tmp/rd/c1/mytoken.db?mode=ro&_pragma=query_only(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT harness,session_id,resolved_provider,model,timestamp,input,cache_read,cache_write,boundary FROM events ORDER BY harness,session_id,timestamp,rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type stat struct{ events, miss, expected int64 }
	causes := map[Cause]stat{}
	routes := map[string]stat{}
	routeTotals := map[string]stat{}
	samples := map[string][]Request{}
	sessions, uncacheable := 0, 0
	var chain []Request
	last := ""
	finish := func() {
		if len(chain) == 0 {
			return
		}
		sessions++
		entries := Analyze(chain)
		if len(chain) > len(samples[chain[0].Harness]) {
			samples[chain[0].Harness] = append([]Request(nil), chain...)
		}
		for i, e := range entries {
			if e.Uncacheable {
				uncacheable++
				continue
			}
			s := causes[e.Cause]
			s.events++
			s.miss += e.Miss
			s.expected += e.Expected
			causes[e.Cause] = s
			key := chain[i].Harness + " / " + chain[i].Provider
			rt := routeTotals[key]
			rt.events++
			rt.expected += e.Expected
			routeTotals[key] = rt
			if e.Cause == CauseUnexplained {
				s := routes[key]
				s.events++
				s.miss += e.Miss
				routes[key] = s
			}
		}
	}
	total := 0
	for rows.Next() {
		var r Request
		var session, b string
		var ns int64
		if err := rows.Scan(&r.Harness, &session, &r.Provider, &r.Model, &ns, &r.Input, &r.CacheRead, &r.CacheWrite, &b); err != nil {
			t.Fatal(err)
		}
		r.At = time.Unix(0, ns)
		r.Boundary = model.Boundary(b)
		key := r.Harness + "\x00" + session
		if key != last {
			finish()
			chain = chain[:0]
			last = key
		}
		chain = append(chain, r)
		total++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	finish()
	t.Logf("TOTAL requests=%d sessions=%d uncacheable=%d", total, sessions, uncacheable)
	var misses int64
	for _, c := range []Cause{CauseNone, CauseCompact, CauseTTL, CauseModel, CauseUnexplained} {
		s := causes[c]
		misses += s.miss
		t.Logf("CAUSE %q requests=%d miss=%d expected=%d", c, s.events, s.miss, s.expected)
	}
	t.Logf("UNEXPLAINED share-of-miss=%.4f%%", 100*float64(causes[CauseUnexplained].miss)/float64(max(1, misses)))
	keys := make([]string, 0, len(routes))
	for k := range routes {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return routes[keys[i]].miss > routes[keys[j]].miss })
	for i, k := range keys {
		if i >= 12 {
			break
		}
		s := routes[k]
		t.Logf("ROUTE %s requests=%d miss=%d cacheable=%d expected=%d rate=%.4f%%", k, s.events, s.miss, routeTotals[k].events, routeTotals[k].expected, 100*float64(s.miss)/float64(max(1, routeTotals[k].expected)))
	}
	for sample, h := range []string{"codex", "dsh", "pi", "claude-code", "workbuddy", "opencode"} {
		c := samples[h]
		if len(c) == 0 {
			continue
		}
		es := Analyze(c)
		start := 0
		for i, e := range es {
			if e.Miss > 0 || (i > 2 && e.Expected == c[i].CacheRead && c[i].CacheRead > 0 && prompt(c[i]) > 2*c[i].CacheRead) {
				start = max(0, i-2)
				break
			}
		}
		t.Logf("SAMPLE S%d harness=%s requests=%d", sample+1, h, len(c))
		for i := start; i < min(start+12, len(c)); i++ {
			r := c[i]
			e := es[i]
			t.Logf("S%d #%d +%.0fs model=%s P=%d R=%d W=%d boundary=%s E=%d miss=%d cause=%s uncacheable=%t", sample+1, i, r.At.Sub(c[0].At).Seconds(), r.Model, prompt(r), r.CacheRead, r.CacheWrite, r.Boundary, e.Expected, e.Miss, e.Cause, e.Uncacheable)
		}
	}
}
