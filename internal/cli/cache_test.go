package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

// fakeCache is a cacheService that records what the command asked for.
type fakeCache struct {
	totals  query.CacheTotals
	daily   []query.CachePoint
	buckets map[query.CacheDim][]query.CacheBucket
	steps   []query.CacheStep
	err     error

	calls   []string
	dim     query.CacheDim
	harness model.Harness
	session string
	empty   bool // return nil slices, like a store with no ledger rows
}

func (f *fakeCache) CacheTotals(context.Context, query.Filter) (query.CacheTotals, error) {
	f.calls = append(f.calls, "totals")
	if f.err != nil {
		return query.CacheTotals{}, f.err
	}
	if f.empty {
		return query.CacheTotals{}, nil
	}
	return f.totals, nil
}

func (f *fakeCache) CacheDaily(context.Context, query.Filter) ([]query.CachePoint, error) {
	f.calls = append(f.calls, "daily")
	if f.err != nil {
		return nil, f.err
	}
	return f.daily, nil
}

func (f *fakeCache) CacheBy(_ context.Context, _ query.Filter, dim query.CacheDim) ([]query.CacheBucket, error) {
	f.calls = append(f.calls, "by:"+string(dim))
	f.dim = dim
	if f.err != nil {
		return nil, f.err
	}
	return f.buckets[dim], nil
}

func (f *fakeCache) SessionCache(_ context.Context, h model.Harness, session string) ([]query.CacheStep, error) {
	f.calls = append(f.calls, "session")
	f.harness, f.session = h, session
	if f.err != nil {
		return nil, f.err
	}
	return f.steps, nil
}

func testCacheTotals() query.CacheTotals {
	return query.CacheTotals{
		Requests: 40, Cacheable: 32, Expected: 800, Read: 600, Miss: 200, USD: 0.1234,
		ByCause: []query.CacheCause{
			{Cause: "compact", Requests: 2, Miss: 60, USD: 0.03},
			{Cause: "ttl", Requests: 3, Miss: 40, USD: 0.02},
			{Cause: causeUnexplained, Requests: 1, Miss: 100, USD: 0.0734},
		},
	}
}

func testCacheSteps() []query.CacheStep {
	return []query.CacheStep{
		{At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Model: "gpt-5", Prompt: 900, Read: 800, Write: 0, Expected: 850, Miss: 50, Cause: "ttl", USD: 0.01},
		{At: time.Date(2026, 1, 2, 3, 5, 6, 0, time.UTC), Model: "gpt-5-mini", Prompt: 300, Read: 0, Write: 0, Expected: -1, Miss: 0, Cause: "", USD: 0, Boundary: "compact"},
	}
}

func cacheTestEnv(s cacheService, format outputFormat, noCost bool) cacheEnv {
	return cacheEnv{
		rng:     query.Range{From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 1, 8, 0, 0, 0, 0, time.UTC)},
		format:  format,
		noCost:  noCost,
		service: s,
	}
}

func runCacheEnv(t *testing.T, env cacheEnv, by, session string) (string, string, int) {
	t.Helper()
	var out, errout bytes.Buffer
	code := cacheRun(context.Background(), env, by, session, &out, &errout)
	return out.String(), errout.String(), code
}

func TestCacheParseErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"unknown by", []string{"--by", "nope"}, "invalid --by value"},
		{"by with session", []string{"--by", "model", "sess-1"}, "--by cannot be combined with a session timeline"},
		{"harness without session", []string{"--harness", "codex"}, "--harness is only valid with a session id"},
		{"two positionals", []string{"a", "b"}, "Usage: mytoken cache"},
		{"unknown harness", []string{"--harness", "nope", "s1"}, `invalid --harness "nope"`},
		{"since and last", []string{"--since", "7d", "--last", "7d"}, "mutually exclusive"},
		{"json with csv", []string{"--json", "--format", "csv"}, "--json cannot be combined with --format csv"},
		{"bad format", []string{"--format", "xml"}, `invalid --format "xml"`},
		{"bad last", []string{"--last", "fortnight"}, "invalid --last"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var errout bytes.Buffer
			req, code := parseCache(c.args, &errout)
			if code != 2 {
				t.Fatalf("code = %d, want 2 (req=%+v)", code, req)
			}
			if !strings.Contains(errout.String(), c.want) {
				t.Errorf("stderr = %q, want it to contain %q", errout.String(), c.want)
			}
		})
	}
}

func TestCacheParseOK(t *testing.T) {
	req, code := parseCache([]string{"--offline", "--since", "2026-01-01", "--until", "2026-01-08", "--by", "provider"}, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if req.by != "provider" || req.session != "" || req.harness != "" {
		t.Fatalf("req = %+v", req)
	}
	if !req.opts.offline || req.opts.format != formatTable || req.opts.noCost {
		t.Fatalf("opts = %+v", req.opts)
	}
	if got := req.opts.rng.From.UTC().Format(time.RFC3339); got != "2026-01-01T00:00:00Z" {
		t.Errorf("From = %s", got)
	}
	if got := req.opts.rng.To.UTC().Format(time.RFC3339); got != "2026-01-08T00:00:00Z" {
		t.Errorf("To = %s", got)
	}

	req, code = parseCache([]string{"sess-1", "--harness", "codex", "--json"}, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("session form: code = %d", code)
	}
	if req.session != "sess-1" || req.harness != "codex" || req.by != "" || req.opts.format != formatJSON {
		t.Fatalf("req = %+v", req)
	}
}

// TestCacheCauseViews covers the default view: the summary line, the cause
// breakdown, and --no-cost dropping every dollar field.
func TestCacheCauseViews(t *testing.T) {
	f := &fakeCache{totals: testCacheTotals()}

	t.Run("table", func(t *testing.T) {
		out, errout, code := runCacheEnv(t, cacheTestEnv(f, formatTable, false), "cause", "")
		if code != 0 {
			t.Fatalf("code=%d stderr=%s", code, errout)
		}
		for _, want := range []string{"Requests: 40", "Cacheable: 32", "Hit: 75.0%", "Miss cost: $0.1234", "CAUSE", "compact", "ttl", causeUnexplained, "0.0300"} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("table no cost", func(t *testing.T) {
		out, _, code := runCacheEnv(t, cacheTestEnv(f, formatTable, true), "cause", "")
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
		for _, bad := range []string{"USD", "Miss cost", "0.0300"} {
			if strings.Contains(out, bad) {
				t.Errorf("--no-cost output still has %q:\n%s", bad, out)
			}
		}
		if !strings.Contains(out, "compact") {
			t.Errorf("--no-cost output lost the rows:\n%s", out)
		}
	})

	t.Run("json", func(t *testing.T) {
		out, _, code := runCacheEnv(t, cacheTestEnv(f, formatJSON, false), "cause", "")
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
		v := decodeJSONAny(t, out)
		if !deepKey(v, "usd") {
			t.Errorf("JSON lost usd:\n%s", out)
		}
		root, _ := v.(map[string]any)
		if root["by"] != "cause" {
			t.Errorf(`by = %v, want "cause"`, root["by"])
		}
		totals, _ := root["totals"].(map[string]any)
		if totals["requests"] != float64(40) {
			t.Errorf("totals.requests = %v", totals["requests"])
		}
		if _, ok := totals["byCause"].([]any); !ok {
			t.Errorf("totals.byCause is not a list: %T", totals["byCause"])
		}
	})

	t.Run("json no cost", func(t *testing.T) {
		out, _, code := runCacheEnv(t, cacheTestEnv(f, formatJSON, true), "cause", "")
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
		if deepKey(decodeJSONAny(t, out), "usd") {
			t.Errorf("--no-cost JSON still has usd:\n%s", out)
		}
	})

	t.Run("csv", func(t *testing.T) {
		out, _, code := runCacheEnv(t, cacheTestEnv(f, formatCSV, false), "cause", "")
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
		recs := parseCSV(t, out)
		if got := strings.Join(recs[0], ","); got != "cause,requests,miss,cost_usd" {
			t.Errorf("header = %s", got)
		}
		if len(recs) != 4 {
			t.Fatalf("want header + 3 causes, got %d rows:\n%s", len(recs), out)
		}
		if got := strings.Join(recs[3], ","); got != "unexplained,1,100,0.073400" {
			t.Errorf("unexplained row = %s", got)
		}
	})

	t.Run("csv no cost", func(t *testing.T) {
		out, _, code := runCacheEnv(t, cacheTestEnv(f, formatCSV, true), "cause", "")
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
		recs := parseCSV(t, out)
		if got := strings.Join(recs[0], ","); got != "cause,requests,miss" {
			t.Errorf("header = %s", got)
		}
	})

	t.Run("empty ledger", func(t *testing.T) {
		empty := &fakeCache{empty: true}
		out, _, code := runCacheEnv(t, cacheTestEnv(empty, formatJSON, false), "cause", "")
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
		if !strings.Contains(out, `"byCause":[]`) {
			t.Errorf("empty view should still emit a list:\n%s", out)
		}
	})
}

// TestCacheBucketViews covers the grouped views and the provider health column.
func TestCacheBucketViews(t *testing.T) {
	f := &fakeCache{buckets: map[query.CacheDim][]query.CacheBucket{
		query.CacheByProvider: {{Key: "https://api.example.com", CacheTotals: testCacheTotals()}},
		query.CacheByModel:    {{Key: "gpt-5", CacheTotals: testCacheTotals()}},
	}}

	t.Run("provider table keeps health", func(t *testing.T) {
		out, _, code := runCacheEnv(t, cacheTestEnv(f, formatTable, false), "provider", "")
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
		if f.dim != query.CacheByProvider {
			t.Errorf("dim = %q", f.dim)
		}
		for _, want := range []string{"KEY", "https://api.example.com", "HEALTH", "12.5%", "MISS USD"} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("model table has no health", func(t *testing.T) {
		out, _, code := runCacheEnv(t, cacheTestEnv(f, formatTable, false), "model", "")
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
		if strings.Contains(out, "HEALTH") {
			t.Errorf("only --by provider shows health:\n%s", out)
		}
		if f.dim != query.CacheByModel {
			t.Errorf("dim = %q", f.dim)
		}
	})

	t.Run("provider csv", func(t *testing.T) {
		out, _, code := runCacheEnv(t, cacheTestEnv(f, formatCSV, false), "provider", "")
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
		recs := parseCSV(t, out)
		if got := strings.Join(recs[0], ","); got != "key,requests,cacheable,expected,read,miss,cost_usd,health" {
			t.Errorf("header = %s", got)
		}
		if got := recs[1][len(recs[1])-1]; got != "12.50" {
			t.Errorf("health = %s, want 12.50", got)
		}
	})

	t.Run("provider csv no cost", func(t *testing.T) {
		out, _, code := runCacheEnv(t, cacheTestEnv(f, formatCSV, true), "provider", "")
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
		recs := parseCSV(t, out)
		if got := strings.Join(recs[0], ","); got != "key,requests,cacheable,expected,read,miss,health" {
			t.Errorf("header = %s", got)
		}
	})

	t.Run("json rows keep the ledger shape", func(t *testing.T) {
		out, _, code := runCacheEnv(t, cacheTestEnv(f, formatJSON, false), "provider", "")
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
		root, _ := decodeJSONAny(t, out).(map[string]any)
		if root["by"] != "provider" {
			t.Errorf(`by = %v`, root["by"])
		}
		rows, _ := root["rows"].([]any)
		if len(rows) != 1 {
			t.Fatalf("rows = %v", root["rows"])
		}
		row, _ := rows[0].(map[string]any)
		if row["key"] != "https://api.example.com" {
			t.Errorf("key = %v", row["key"])
		}
		// CacheTotals is embedded, so the totals fields are promoted.
		if row["requests"] != float64(40) || row["usd"] != 0.1234 {
			t.Errorf("promoted totals missing: %v", row)
		}
		if _, ok := row["byCause"].([]any); !ok {
			t.Errorf("byCause should be promoted: %v", row)
		}
	})

	t.Run("health is a dash without expectations", func(t *testing.T) {
		none := &fakeCache{buckets: map[query.CacheDim][]query.CacheBucket{
			query.CacheByProvider: {{Key: "https://cold.example.com"}},
		}}
		out, _, code := runCacheEnv(t, cacheTestEnv(none, formatTable, false), "provider", "")
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
		if !strings.Contains(out, "https://cold.example.com") || !strings.Contains(out, "-") {
			t.Errorf("expected a dash for a site with no expected tokens:\n%s", out)
		}
	})
}

func TestCacheDayViews(t *testing.T) {
	f := &fakeCache{daily: []query.CachePoint{
		{Day: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), CacheTotals: testCacheTotals()},
	}}
	out, errout, code := runCacheEnv(t, cacheTestEnv(f, formatTable, false), "day", "")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	for _, want := range []string{"DAY", "2026-01-02", "MISS USD"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if len(f.calls) != 1 || f.calls[0] != "daily" {
		t.Errorf("calls = %v, want just daily", f.calls)
	}

	out, _, code = runCacheEnv(t, cacheTestEnv(f, formatCSV, false), "day", "")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	recs := parseCSV(t, out)
	if got := strings.Join(recs[0], ","); got != "day,requests,cacheable,expected,read,miss,cost_usd" {
		t.Errorf("header = %s", got)
	}
	if got := recs[1][0]; got != "2026-01-02T00:00:00Z" {
		t.Errorf("day = %s", got)
	}

	out, _, code = runCacheEnv(t, cacheTestEnv(f, formatJSON, true), "day", "")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	v := decodeJSONAny(t, out)
	if !deepKey(v, "t") || deepKey(v, "usd") {
		t.Errorf("day JSON should keep t and drop usd:\n%s", out)
	}
}

// TestCacheTimelineViews covers the session timeline, including an uncacheable
// step (Expected < 0) and a compaction boundary.
func TestCacheTimelineViews(t *testing.T) {
	f := &fakeCache{steps: testCacheSteps()}

	t.Run("table", func(t *testing.T) {
		env := cacheTestEnv(f, formatTable, false)
		env.harness = "codex"
		out, errout, code := runCacheEnv(t, env, "", "sess-1")
		if code != 0 {
			t.Fatalf("code=%d stderr=%s", code, errout)
		}
		for _, want := range []string{
			"Session: codex/sess-1", "Steps: 2", "Expected: 850", "Hit: 94.1%", "Miss cost: $0.0100",
			"AT", "MODEL", "PROMPT", "EXPECTED", "MISS USD", "BOUNDARY",
			"2026-01-02T03:04:05Z", "gpt-5", "ttl", "compact", "uncacheable",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
		if f.harness != model.Harness("codex") || f.session != "sess-1" {
			t.Errorf("asked for %s/%s", f.harness, f.session)
		}
	})

	t.Run("table no cost", func(t *testing.T) {
		env := cacheTestEnv(f, formatTable, true)
		env.harness = "codex"
		out, _, code := runCacheEnv(t, env, "", "sess-1")
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
		for _, bad := range []string{"MISS USD", "Miss cost"} {
			if strings.Contains(out, bad) {
				t.Errorf("--no-cost output still has %q:\n%s", bad, out)
			}
		}
		if !strings.Contains(out, "BOUNDARY") {
			t.Errorf("boundary column should stay:\n%s", out)
		}
	})

	t.Run("csv", func(t *testing.T) {
		env := cacheTestEnv(f, formatCSV, false)
		env.harness = "codex"
		out, _, code := runCacheEnv(t, env, "", "sess-1")
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
		recs := parseCSV(t, out)
		if got := strings.Join(recs[0], ","); got != "at,model,prompt,read,write,expected,miss,cause,cost_usd,boundary" {
			t.Errorf("header = %s", got)
		}
		if len(recs) != 3 {
			t.Fatalf("want header + 2 steps:\n%s", out)
		}
		if got := strings.Join(recs[2], ","); got != "2026-01-02T03:05:06Z,gpt-5-mini,300,0,0,-1,0,,0.000000,compact" {
			t.Errorf("uncacheable row = %s", got)
		}
	})

	t.Run("csv no cost", func(t *testing.T) {
		env := cacheTestEnv(f, formatCSV, true)
		env.harness = "codex"
		out, _, code := runCacheEnv(t, env, "", "sess-1")
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
		recs := parseCSV(t, out)
		if got := strings.Join(recs[0], ","); got != "at,model,prompt,read,write,expected,miss,cause,boundary" {
			t.Errorf("header = %s", got)
		}
	})

	t.Run("json", func(t *testing.T) {
		env := cacheTestEnv(f, formatJSON, false)
		env.harness = "codex"
		out, _, code := runCacheEnv(t, env, "", "sess-1")
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
		root, _ := decodeJSONAny(t, out).(map[string]any)
		if root["harness"] != "codex" || root["sessionId"] != "sess-1" {
			t.Errorf("root = %v", root)
		}
		steps, _ := root["steps"].([]any)
		if len(steps) != 2 {
			t.Fatalf("steps = %v", root["steps"])
		}
		second, _ := steps[1].(map[string]any)
		if second["boundary"] != "compact" || second["expected"] != float64(-1) {
			t.Errorf("second step = %v", second)
		}
	})

	t.Run("json no cost", func(t *testing.T) {
		env := cacheTestEnv(f, formatJSON, true)
		env.harness = "codex"
		out, _, code := runCacheEnv(t, env, "", "sess-1")
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
		if deepKey(decodeJSONAny(t, out), "usd") {
			t.Errorf("--no-cost timeline JSON still has usd:\n%s", out)
		}
	})
}

// TestCacheTimelineHarnessResolution covers resolving the owning harness from
// the ledger, and refusing to guess when the id is ambiguous.
func TestCacheTimelineHarnessResolution(t *testing.T) {
	t.Run("resolved", func(t *testing.T) {
		f := &fakeCache{
			steps:   testCacheSteps(),
			buckets: map[query.CacheDim][]query.CacheBucket{query.CacheBySession: {{Key: "codex/sess-1"}}},
		}
		out, errout, code := runCacheEnv(t, cacheTestEnv(f, formatJSON, false), "", "sess-1")
		if code != 0 {
			t.Fatalf("code=%d stderr=%s", code, errout)
		}
		if f.harness != model.Harness("codex") || f.session != "sess-1" {
			t.Errorf("asked for %s/%s", f.harness, f.session)
		}
		if !strings.Contains(out, `"harness":"codex"`) {
			t.Errorf("output should name the resolved harness:\n%s", out)
		}
	})

	t.Run("ambiguous", func(t *testing.T) {
		f := &fakeCache{buckets: map[query.CacheDim][]query.CacheBucket{
			query.CacheBySession: {{Key: "codex/sess-1"}, {Key: "dsh/sess-1"}},
		}}
		_, errout, code := runCacheEnv(t, cacheTestEnv(f, formatJSON, false), "", "sess-1")
		if code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
		for _, want := range []string{"appears under 2 harnesses", "codex", "dsh", "--harness"} {
			if !strings.Contains(errout, want) {
				t.Errorf("stderr missing %q: %s", want, errout)
			}
		}
		if f.session != "" {
			t.Errorf("must not read a timeline when ambiguous: %v", f.calls)
		}
	})

	t.Run("missing", func(t *testing.T) {
		f := &fakeCache{}
		_, errout, code := runCacheEnv(t, cacheTestEnv(f, formatJSON, false), "", "sess-1")
		if code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
		if !strings.Contains(errout, "no session") {
			t.Errorf("stderr = %s", errout)
		}
	})
}

func TestCacheServiceErrors(t *testing.T) {
	f := &fakeCache{err: errors.New("boom")}
	cases := []struct {
		name    string
		by      string
		session string
	}{
		{"cause", "cause", ""},
		{"day", "day", ""},
		{"provider", "provider", ""},
		{"timeline", "", "sess-1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, errout, code := runCacheEnv(t, cacheTestEnv(f, formatJSON, false), c.by, c.session)
			if code != 1 {
				t.Fatalf("code = %d, want 1", code)
			}
			if !strings.Contains(errout, "boom") {
				t.Errorf("stderr = %q", errout)
			}
		})
	}
}

// TestCacheDispatch checks the command is reachable from run and that help
// documents it.
func TestCacheDispatch(t *testing.T) {
	_, errout, code := runCLI(t, "cache", "--by", "nope")
	if code != 2 || !strings.Contains(errout, "invalid --by value") {
		t.Errorf("code=%d stderr=%s", code, errout)
	}
	out, errout, code := runCLI(t, "help")
	if code != 0 {
		t.Fatalf("help: code=%d stderr=%s", code, errout)
	}
	for _, want := range []string{"cache [options]", "cache <session-id>", "options for stats, sessions and cache"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q", want)
		}
	}
}

func decodeJSONAny(t *testing.T, out string) any {
	t.Helper()
	var v any
	if e := json.Unmarshal([]byte(out), &v); e != nil {
		t.Fatalf("bad JSON: %v\n%s", e, out)
	}
	return v
}

func deepKey(v any, key string) bool {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if k == key || deepKey(x, key) {
				return true
			}
		}
	case []any:
		for _, x := range t {
			if deepKey(x, key) {
				return true
			}
		}
	}
	return false
}
