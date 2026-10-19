package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

type sumPeriodJSON struct {
	Label    string       `json:"label"`
	From     time.Time    `json:"from"`
	To       time.Time    `json:"to"`
	Tokens   model.Tokens `json:"tokens"`
	CostUSD  float64      `json:"costUsd"`
	Unpriced int64        `json:"unpriced"`
	Requests int64        `json:"requests"`
}

type sumReportJSON struct {
	Timezone string        `json:"timezone"`
	Today    sumPeriodJSON `json:"today"`
	Month    sumPeriodJSON `json:"month"`
}

func TestSummary(t *testing.T) {
	f := newFixture(t)

	t.Run("default command is summary", func(t *testing.T) {
		out, errOut, code := runCLI(t)
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %s", code, errOut)
		}
		mustContain(t, out, "API-equivalent cost · ", "Today", "Month", "tokens (in ", "requests · cache hit ")
	})

	t.Run("json today", func(t *testing.T) {
		out, errOut, code := runCLI(t, "summary", "--json", "--timezone", "UTC", "--db", f.dbPath)
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %s", code, errOut)
		}
		var got sumReportJSON
		jsonInto(t, out, &got)
		if got.Timezone != "UTC" {
			t.Errorf("timezone = %q, want UTC", got.Timezone)
		}
		today := got.Today
		if today.Label != "today" {
			t.Errorf("today label = %q", today.Label)
		}
		wantTokens := model.Tokens{Input: 150, Output: 30, CacheRead: 300, CacheWrite: 40}
		// The 1h detail prices the cache write; the reporting surface exposes the
		// cache-write total, not the 1h split.
		today.Tokens.CacheWrite1h = 0
		if today.Tokens != wantTokens {
			t.Errorf("today tokens = %+v, want %+v", today.Tokens, wantTokens)
		}
		if today.Requests != 2 {
			t.Errorf("today requests = %d, want 2", today.Requests)
		}
		if today.CostUSD != 0.50 {
			t.Errorf("today cost = %v, want 0.50", today.CostUSD)
		}
		if today.Unpriced != 1 {
			t.Errorf("today unpriced = %d, want 1 (the codex event)", today.Unpriced)
		}
		if today.From.Location() != time.UTC || today.From.Format("2006-01-02") != f.day0.Format("2006-01-02") {
			t.Errorf("today from = %s, want UTC midnight", today.From)
		}
		if got.Month.Requests < 2 || got.Month.Requests > 5 {
			t.Errorf("month requests = %d, want 2..5", got.Month.Requests)
		}
		if got.Month.CostUSD < 0.50 {
			t.Errorf("month cost = %v, want at least today's 0.50", got.Month.CostUSD)
		}
	})

	t.Run("text labels cost API-equivalent", func(t *testing.T) {
		out, errOut, code := runCLI(t, "summary", "--timezone", "UTC", "--db", f.dbPath)
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %s", code, errOut)
		}
		mustContain(t, out,
			"API-equivalent cost · UTC",
			// The 1h cache write is part of cache write, never added twice.
			"Today  $0.50 (+unpriced)  520 tokens (in 150, out 30, cache read 300, write 40)  2 requests · cache hit 61.2%",
		)
	})

	t.Run("no-cost", func(t *testing.T) {
		out, errOut, code := runCLI(t, "summary", "--json", "--no-cost", "--timezone", "UTC", "--db", f.dbPath)
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %s", code, errOut)
		}
		var got sumReportJSON
		jsonInto(t, out, &got)
		if got.Today.CostUSD != 0 || got.Month.CostUSD != 0 {
			t.Errorf("--no-cost costs = today %v month %v, want 0", got.Today.CostUSD, got.Month.CostUSD)
		}
		if got.Today.Unpriced != 1 {
			t.Errorf("--no-cost unpriced = %d, want the unpriced count to stay", got.Today.Unpriced)
		}
		text, _, code := runCLI(t, "summary", "--no-cost", "--timezone", "UTC", "--db", f.dbPath)
		if code != 0 {
			t.Fatalf("text exit = %d", code)
		}
		if strings.Contains(text, "$") {
			t.Errorf("--no-cost text still shows money:\n%s", text)
		}
	})

	t.Run("positional argument is rejected", func(t *testing.T) {
		_, errOut, code := runCLI(t, "summary", "extra")
		if code != 2 {
			t.Fatalf("exit = %d, want 2", code)
		}
		mustContain(t, errOut, "usage: mytoken [summary]")
	})
}

type dayJSON struct {
	T       time.Time    `json:"t"`
	Tokens  model.Tokens `json:"tokens"`
	CostUSD float64      `json:"costUsd"`
}

type dailyJSON struct {
	Timezone string    `json:"timezone"`
	Days     []dayJSON `json:"days"`
}

func TestDaily(t *testing.T) {
	f := newFixture(t)
	// The child runs with TZ=Asia/Shanghai; --timezone UTC must still bucket by
	// UTC days. The 23:00Z events would otherwise land in the next day's bucket.
	out, errOut, code := runCLI(t, "daily", "--json", "--days", "3", "--timezone", "UTC", "--db", f.dbPath)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errOut)
	}
	var got dailyJSON
	jsonInto(t, out, &got)
	if got.Timezone != "UTC" {
		t.Errorf("timezone = %q, want UTC", got.Timezone)
	}
	if len(got.Days) != 3 {
		t.Fatalf("days = %d, want 3\n%s", len(got.Days), out)
	}
	want := []struct {
		offset int
		tokens model.Tokens
		cost   float64
	}{
		{-2, model.Tokens{Input: 7, Output: 3}, 0.10},
		{-1, model.Tokens{Input: 30, Output: 13, CacheRead: 100}, 0.25},
		{0, model.Tokens{Input: 150, Output: 30, CacheRead: 300, CacheWrite: 40}, 0.50},
	}
	for i, w := range want {
		day := got.Days[i]
		wantDay := f.day0.AddDate(0, 0, w.offset)
		if !day.T.Equal(wantDay) {
			t.Errorf("days[%d] = %s, want %s (UTC midnight)", i, day.T, wantDay)
		}
		day.Tokens.CacheWrite1h = 0
		if day.Tokens != w.tokens {
			t.Errorf("days[%d] tokens = %+v, want %+v", i, day.Tokens, w.tokens)
		}
		if day.CostUSD != w.cost {
			t.Errorf("days[%d] cost = %v, want %v", i, day.CostUSD, w.cost)
		}
	}

	text, errOut, code := runCLI(t, "daily", "--days", "3", "--timezone", "UTC", "--db", f.dbPath)
	if code != 0 {
		t.Fatalf("text exit = %d, stderr = %s", code, errOut)
	}
	mustContain(t, text,
		"DATE", "INPUT", "CACHE READ", "CACHE WRITE", "TOTAL", "API-EQUIV", "CACHE HIT",
		f.day0.AddDate(0, 0, -2).Format("2006-01-02"),
		f.day0.AddDate(0, 0, -1).Format("2006-01-02"),
		f.day0.Format("2006-01-02"),
	)
	if lines := strings.Count(strings.TrimRight(text, "\n"), "\n") + 1; lines != 4 {
		t.Errorf("text daily lines = %d, want header + 3 days:\n%s", lines, text)
	}

	noCost, _, code := runCLI(t, "daily", "--no-cost", "--days", "3", "--timezone", "UTC", "--db", f.dbPath)
	if code != 0 {
		t.Fatalf("no-cost exit = %d", code)
	}
	if strings.Contains(noCost, "API-EQUIV") {
		t.Errorf("--no-cost daily still labels an API-EQUIV cost column:\n%s", noCost)
	}
	mustContain(t, noCost, "CACHE WRITE", "TOTAL")

	_, errOut, code = runCLI(t, "daily", "--days", "0", "--db", f.dbPath)
	if code != 2 {
		t.Fatalf("--days 0 exit = %d, want 2", code)
	}
	mustContain(t, errOut, "usage: mytoken daily")
}

func TestQuota(t *testing.T) {
	f := newFixture(t)

	out, errOut, code := runCLI(t, "quota", "--db", f.dbPath)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errOut)
	}
	mustContain(t, out, "no plan quota stored")

	js, errOut, code := runCLI(t, "quota", "--json", "--db", f.dbPath)
	if code != 0 {
		t.Fatalf("json exit = %d, stderr = %s", code, errOut)
	}
	var empty []any
	jsonInto(t, js, &empty)
	if len(empty) != 0 {
		t.Errorf("plans = %d, want 0", len(empty))
	}

	// Seed server-fetched snapshots the way the plan service stores them: a
	// percentage window, a money balance and a capped credit allowance. Also
	// leave a stale local-log row behind; the reader must skip it even though the
	// read-write migration that prunes it has not run against this index.
	st, err := store.Open(f.dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	seed := map[string]string{
		"quota.plan.codex": `{"provider":"codex","plan":"plus","source":"api","fetched_at":"2026-10-20T00:00:00Z",` +
			`"windows":[{"name":"5h","used_percent":42.5},{"name":"week","used_percent":12,"resets_at":"2026-10-27T00:00:00Z"}]}`,
		"quota.plan.deepseek": `{"provider":"deepseek","source":"api","fetched_at":"2026-10-19T16:44:04Z",` +
			`"windows":[{"name":"balance","balance":true,"remaining":36.55,"unit":"cny"}]}`,
		"quota.plan.workbuddy": `{"provider":"workbuddy","plan":"self_serve_business_prolite","source":"api","fetched_at":"2026-10-19T16:44:04Z",` +
			`"windows":[{"name":"Credits","used_percent":54.9,"used":5581.34,"limit":10177,"remaining":4595.66,` +
			`"unit":"credits","expires_at":"2026-10-31T23:59:59+08:00"}]}`,
		"quota.plan.trae-cn": `{"provider":"trae-cn","plan":"Credits","source":"local-log","fetched_at":"2026-09-23T08:03:54Z",` +
			`"windows":[{"name":"Credits","used_percent":0}]}`,
	}
	for key, value := range seed {
		if err := st.SetSetting(t.Context(), key, value); err != nil {
			st.Close()
			t.Fatalf("SetSetting(%q): %v", key, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	out, errOut, code = runCLI(t, "quota", "--db", f.dbPath)
	if code != 0 {
		t.Fatalf("seeded exit = %d, stderr = %s", code, errOut)
	}
	mustContain(t, out, "PROVIDER", "codex", "Plus", "api", "5h 42.5%", "week 12.0%",
		"Business Prolite", "balance ¥36.55", "Credits 5,581.34/10,177 (54.9%)",
		"expires 2026-10-31")
	if strings.Contains(out, "trae-cn") {
		t.Errorf("stale local-log snapshot printed:\n%s", out)
	}
	if strings.Contains(out, "0001-01-01") {
		t.Errorf("zero fetch time printed:\n%s", out)
	}

	js, errOut, code = runCLI(t, "quota", "plan", "--json", "--db", f.dbPath)
	if code != 0 {
		t.Fatalf("seeded json exit = %d, stderr = %s", code, errOut)
	}
	type quotaWindowJSON struct {
		Name        string    `json:"name"`
		UsedPercent float64   `json:"usedPercent"`
		Remaining   float64   `json:"remaining"`
		Used        float64   `json:"used"`
		Limit       float64   `json:"limit"`
		Unit        string    `json:"unit"`
		Balance     bool      `json:"balance"`
		ExpiresAt   time.Time `json:"expiresAt"`
	}
	var plans []struct {
		Provider  string            `json:"provider"`
		Plan      string            `json:"plan"`
		Source    string            `json:"source"`
		FetchedAt time.Time         `json:"fetchedAt"`
		Windows   []quotaWindowJSON `json:"windows"`
	}
	jsonInto(t, js, &plans)
	if len(plans) != 3 {
		t.Fatalf("plans = %d, want 3 (stale row skipped): %+v", len(plans), plans)
	}
	byProvider := map[string]int{}
	for i, p := range plans {
		byProvider[p.Provider] = i
		if p.Source != "api" || p.FetchedAt.IsZero() {
			t.Errorf("plan %q has source %q fetchedAt %v", p.Provider, p.Source, p.FetchedAt)
		}
	}
	deepseek, ok := byProvider["deepseek"]
	if !ok || len(plans[deepseek].Windows) != 1 {
		t.Fatalf("deepseek plans = %+v", plans)
	}
	if w := plans[deepseek].Windows[0]; !w.Balance || w.Unit != "cny" || w.Remaining != 36.55 {
		t.Errorf("deepseek balance window = %+v", w)
	}
	workbuddy, ok := byProvider["workbuddy"]
	if !ok || len(plans[workbuddy].Windows) != 1 {
		t.Fatalf("workbuddy plans = %+v", plans)
	}
	if plans[workbuddy].Plan != "self_serve_business_prolite" {
		t.Errorf("json plan = %q, want raw id", plans[workbuddy].Plan)
	}
	if w := plans[workbuddy].Windows[0]; w.Used != 5581.34 || w.Limit != 10177 || w.Unit != "credits" ||
		w.Remaining != 4595.66 || w.ExpiresAt.Format("2006-01-02") != "2026-10-31" {
		t.Errorf("workbuddy credit window = %+v", w)
	}

	csv, errOut, code := runCLI(t, "quota", "--format", "csv", "--db", f.dbPath)
	if code != 0 {
		t.Fatalf("csv exit = %d, stderr = %s", code, errOut)
	}
	mustContain(t, csv,
		"provider,plan,source,fetched_at,window,used_percent,remaining,used,limit,unit,balance,resets_at,expires_at",
		"codex,plus,api",
		"deepseek,,api",
		",balance,0,36.55,0,0,cny,true,,",
		",Credits,54.9,4595.66,5581.34,10177,credits,false,")
	if strings.Contains(csv, "local-log") {
		t.Errorf("stale local-log row written to csv:\n%s", csv)
	}

	// list/status read local usage logs and were removed with that path.
	_, errOut, code = runCLI(t, "quota", "bogus", "--db", f.dbPath)
	if code != 2 {
		t.Fatalf("unknown quota exit = %d, want 2", code)
	}
	mustContain(t, errOut, "unknown quota command: bogus")
}

func TestStatusline(t *testing.T) {
	f := newFixture(t)

	out, errOut, code := runCLI(t, "statusline", "--timezone", "UTC", "--since", "1d", "--db", f.dbPath)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errOut)
	}
	mustContain(t, out,
		"claude-code/sess-tod",
		"$0.5000",
		"cache 68.2%",
		"1 req",
	)
	if strings.HasPrefix(out, "index ") {
		t.Errorf("statusline reported an unavailable index: %s", out)
	}

	js, errOut, code := runCLI(t, "statusline", "--json", "--timezone", "UTC", "--since", "1d", "--db", f.dbPath)
	if code != 0 {
		t.Fatalf("json exit = %d, stderr = %s", code, errOut)
	}
	var got struct {
		Index struct {
			Available bool `json:"available"`
		} `json:"index"`
		Session *struct {
			Harness   string  `json:"harness"`
			SessionID string  `json:"sessionId"`
			CostUSD   float64 `json:"costUsd"`
			CacheHit  float64 `json:"cacheHit"`
			Requests  int64   `json:"requests"`
			Tokens    struct {
				CacheWrite1h int64 `json:"cacheWrite1h"`
			} `json:"tokens"`
		} `json:"session"`
		Timezone string `json:"timezone"`
	}
	jsonInto(t, js, &got)
	if !got.Index.Available {
		t.Error("index is not available")
	}
	if got.Timezone != "UTC" {
		t.Errorf("timezone = %q, want UTC", got.Timezone)
	}
	if got.Session == nil {
		t.Fatalf("session is nil: %s", js)
	}
	if got.Session.Harness != "claude-code" || got.Session.SessionID != f.todaySession {
		t.Errorf("session = %s/%s, want claude-code/%s", got.Session.Harness, got.Session.SessionID, f.todaySession)
	}
	if got.Session.Requests != 1 || got.Session.CostUSD != 0.50 {
		t.Errorf("session requests/cost = %d/%v, want 1/0.50", got.Session.Requests, got.Session.CostUSD)
	}
	noCost, _, code := runCLI(t, "statusline", "--json", "--no-cost", "--timezone", "UTC", "--since", "1d", "--db", f.dbPath)
	if code != 0 {
		t.Fatalf("no-cost exit = %d", code)
	}
	if strings.Contains(noCost, "costUsd") {
		t.Errorf("--no-cost JSON still carries costUsd:\n%s", noCost)
	}
}

func TestDoctor(t *testing.T) {
	f := newFixture(t)

	out, errOut, code := runCLI(t, "doctor")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errOut)
	}
	mustContain(t, out,
		"Data directory: "+f.home,
		"Database: "+f.dbPath,
		"Pricing: embedded",
		"Index: read-only, schema ok",
		"HARNESS", "ENABLED", "DETECTED", "EVENTS",
		"claude-code", "codex",
	)

	js, errOut, code := runCLI(t, "doctor", "--json")
	if code != 0 {
		t.Fatalf("json exit = %d, stderr = %s", code, errOut)
	}
	var got doctorReport
	jsonInto(t, js, &got)
	if got.DBPath != f.dbPath {
		t.Errorf("dbPath = %q, want %q", got.DBPath, f.dbPath)
	}
	if got.DBUnavailable {
		t.Errorf("dbUnavailable = true: %s", got.DBDetail)
	}
	if got.Schema.State != "ok" {
		t.Errorf("schema = %q, want ok", got.Schema.State)
	}
	if got.PricingSource != "embedded" {
		t.Errorf("pricingSource = %q, want embedded", got.PricingSource)
	}
	events := map[string]int64{}
	for _, h := range got.Harnesses {
		events[h.Harness] = h.Events
		if !h.Enabled {
			t.Errorf("harness %s is disabled in a fresh index", h.Harness)
		}
	}
	if events["claude-code"] != 3 {
		t.Errorf("claude-code events = %d, want 3", events["claude-code"])
	}
	if events["codex"] != 2 {
		t.Errorf("codex events = %d, want 2", events["codex"])
	}
}

func TestVersionHelpAndUnknownCommand(t *testing.T) {
	newTestHome(t)

	out, errOut, code := runCLI(t, "version")
	if code != 0 {
		t.Fatalf("version exit = %d, stderr = %s", code, errOut)
	}
	if got := strings.TrimSpace(out); got != Version {
		t.Errorf("version = %q, want %q", got, Version)
	}

	out, errOut, code = runCLI(t, "help")
	if code != 0 {
		t.Fatalf("help exit = %d, stderr = %s", code, errOut)
	}
	mustContain(t, out, "Usage: mytoken", "mytoken daily", "mytoken doctor", "Read-only commands")

	_, errOut, code = runCLI(t, "version", "extra")
	if code != 2 {
		t.Fatalf("version extra exit = %d, want 2", code)
	}
	mustContain(t, errOut, "Usage: mytoken")

	out, errOut, code = runCLI(t, "frobnicate")
	if code == 0 {
		t.Fatal("unknown command exited 0, want non-zero")
	}
	if code != 2 {
		t.Errorf("unknown command exit = %d, want 2", code)
	}
	if out != "" {
		t.Errorf("unknown command wrote to stdout: %q", out)
	}
	mustContain(t, errOut, "unknown command: frobnicate", `Run "mytoken help" for usage.`)
}
