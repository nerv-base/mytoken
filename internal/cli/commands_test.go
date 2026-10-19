package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
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
	mustContain(t, out, "QUOTA  generated ", "no stored Codex quota observation", "instances are never combined")

	js, errOut, code := runCLI(t, "quota", "--json", "--db", f.dbPath)
	if code != 0 {
		t.Fatalf("json exit = %d, stderr = %s", code, errOut)
	}
	var got struct {
		GeneratedAt time.Time `json:"generatedAt"`
		Status      struct {
			Available bool   `json:"available"`
			Reason    string `json:"reason"`
		} `json:"status"`
		Instances []any `json:"instances"`
	}
	jsonInto(t, js, &got)
	if got.GeneratedAt.IsZero() {
		t.Error("generatedAt is zero")
	}
	if got.Status.Available {
		t.Errorf("status.available = true with no observations")
	}
	if got.Status.Reason != "no_observation" {
		t.Errorf("status.reason = %q, want no_observation", got.Status.Reason)
	}
	if len(got.Instances) != 0 {
		t.Errorf("instances = %d, want 0", len(got.Instances))
	}

	plan, _, code := runCLI(t, "quota", "plan", "--db", f.dbPath)
	if code != 0 {
		t.Fatalf("plan exit = %d", code)
	}
	mustContain(t, plan, "no plan quota stored")

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
