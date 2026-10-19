package query

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

// benchmarkEndpoints mixes the spellings real configs produce: two raw forms
// that normalize to one endpoint ID, several distinct paths, and raw URLs that
// cannot establish equivalence and therefore stay unresolved per session.
var benchmarkEndpoints = []string{
	"https://api.anthropic.com/v1",
	"https://api.anthropic.com:443/v1/",
	"https://api.openai.com/v1",
	"https://api.example.com/v1",
	"https://api.example.com/v1/alt",
	"https://api.example.com:8443/v1",
	"",
	"https://api.example.com/v1?key=benchmark",
}

// benchmarkFixture deliberately has 200K requests but only 600 sessions. It
// writes bounded batches so fixture creation does not mask query allocations.
func benchmarkFixture(tb testing.TB) (Service, time.Time) {
	tb.Helper()
	st, err := store.Open(filepath.Join(tb.TempDir(), "benchmark.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { st.Close() })
	ctx := context.Background()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local)
	const total = 200000
	for session := 0; session < 600; session++ {
		n := total / 600
		if session < total%600 {
			n++
		}
		id := fmt.Sprintf("session-%03d", session)
		parent := ""
		if session%10 == 1 {
			parent = fmt.Sprintf("session-%03d", session-1)
		}
		// One endpoint per session, like a configured harness. A few sessions
		// fail over mid-stream so multi-endpoint sessions stay represented.
		sessionBase := benchmarkEndpoints[session%len(benchmarkEndpoints)]
		events := make([]model.UsageEvent, n)
		res := make([]store.Resolution, n)
		for j := range events {
			at := start.Add(time.Duration(session)*2*time.Hour + time.Duration(j)*time.Minute)
			base := sessionBase
			if session%64 == 0 {
				base = benchmarkEndpoints[j%2]
			}
			events[j] = model.UsageEvent{DedupKey: fmt.Sprintf("event-%03d-%04d", session, j), SessionID: id, ParentID: parent, Timestamp: at, ProjectPath: fmt.Sprintf("/benchmark/project-%02d", session%20), Model: fmt.Sprintf("model-%d", session%4), BaseURL: base, Tokens: model.Tokens{Input: int64(100 + j%13), Output: 20, CacheRead: 200, CacheWrite: 10, Reasoning: 5}}
			// Exercise populated per-request metadata without adding rollup dimensions.
			events[j].RequestID = fmt.Sprintf("req-%03d-%04d-0123456789abcdef", session, j)
			if j%80 == 0 {
				events[j].Boundary = model.BoundaryCompact
			}
			if j%100 == 0 {
				events[j].Bill = &model.Bill{Amount: 1.25, Unit: "credits"}
			}
			res[j] = store.Resolution{Provider: fmt.Sprintf("provider-%d", session%3), Attrib: model.AttribLog, Cost: .001, Priced: true}
		}
		if err = st.Commit(ctx, model.Codex, id, harness.Batch{Events: events}, res); err != nil {
			tb.Fatal(err)
		}
	}
	return NewService(st), start
}

type queryCase struct {
	name string
	run  func() error
}

func benchmarkCases(q Service, start time.Time) []queryCase {
	f := Filter{Range: Range{From: start, To: start.Add(7 * 24 * time.Hour)}}
	return []queryCase{
		{"Totals", func() error { _, e := q.Totals(context.Background(), f); return e }},
		{"Daily", func() error { _, e := q.Daily(context.Background(), f); return e }},
		{"ByHarness", func() error { _, e := q.ByHarness(context.Background(), f); return e }},
		{"ByModel", func() error { _, e := q.ByModel(context.Background(), f); return e }},
		{"ByProvider", func() error { _, e := q.ByProvider(context.Background(), f); return e }},
		{"ByProject", func() error { _, e := q.ByProject(context.Background(), f); return e }},
		{"Sessions", func() error { _, _, e := q.Sessions(context.Background(), f, SortRecent, 100, 0); return e }},
	}
}

func BenchmarkService(b *testing.B) {
	q, start := benchmarkFixture(b)
	for _, tc := range benchmarkCases(q, start) {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := tc.run(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Opt-in because scheduling noise makes hard deadlines unsuitable for shared CI.
// The allocation ceiling is a conservative upper bound on additional Go heap
// retained by one query (not a process-RSS assertion about the test runner).
func TestQueryPerformanceBudget(t *testing.T) {
	if os.Getenv("MYTOKEN_STRICT_PERF") == "" {
		t.Skip("set MYTOKEN_STRICT_PERF=1 for the 200K performance gate")
	}
	q, start := benchmarkFixture(t)
	for _, tc := range benchmarkCases(q, start) {
		t.Run(tc.name, func(t *testing.T) {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			at := time.Now()
			err := tc.run()
			elapsed := time.Since(at)
			runtime.ReadMemStats(&after)
			if err != nil {
				t.Fatal(err)
			}
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("%s %d allocated bytes", elapsed, allocated)
			if elapsed >= 100*time.Millisecond {
				t.Errorf("query exceeded 100ms: %s", elapsed)
			}
			if allocated >= 50<<20 {
				t.Errorf("query exceeded 50MiB allocation budget: %d", allocated)
			}
		})
	}
}

// detailFixture is a miniature of the real corpus's worst case: one root
// session with thousands of events and a few descendant sessions whose facts
// only feed the children summary.
func detailFixture(tb testing.TB) (Service, model.Harness, string) {
	tb.Helper()
	st, err := store.Open(filepath.Join(tb.TempDir(), "detail.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { st.Close() })
	ctx := context.Background()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local)
	const root = "detail-root"
	for child := 0; child < 3; child++ {
		events := make([]model.UsageEvent, 400)
		res := make([]store.Resolution, 400)
		for j := range events {
			events[j] = model.UsageEvent{DedupKey: fmt.Sprintf("%s-%04d", root, j), SessionID: root, ParentID: "", Timestamp: start.Add(time.Duration(j) * time.Second), Model: "model-0", Tokens: model.Tokens{Input: 100, Output: 20, CacheRead: 200, CacheWrite: 10, CacheWrite1h: 2, Reasoning: 5}}
			res[j] = store.Resolution{Provider: "provider-0", Attrib: model.AttribLog, Cost: .001, Priced: true}
		}
		if err = st.Commit(ctx, model.Codex, root, harness.Batch{Events: events}, res); err != nil {
			tb.Fatal(err)
		}
		id := fmt.Sprintf("detail-child-%d", child)
		for j := range events {
			events[j].DedupKey = fmt.Sprintf("%s-%04d", id, j)
			events[j].SessionID = id
			events[j].ParentID = root
			events[j].Timestamp = start.Add(time.Duration(j) * time.Second)
		}
		if err = st.Commit(ctx, model.Codex, id, harness.Batch{Events: events}, res); err != nil {
			tb.Fatal(err)
		}
	}
	return NewService(st), model.Codex, root
}

func BenchmarkSessionDetail(b *testing.B) {
	q, h, id := detailFixture(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, _, err := q.Session(ctx, h, id); err != nil {
			b.Fatal(err)
		}
	}
}
