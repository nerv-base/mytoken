package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

// causeUnexplained mirrors ledger.CauseUnexplained: a miss that no compaction,
// prompt TTL or model switch explains, and therefore the only signal a relay's
// cache health is built from (docs/CACHE.md §3, §6).
const causeUnexplained = "unexplained"

// cacheTotalsReport is the --format json payload of `cache --by cause`.
type cacheTotalsReport struct {
	By     string            `json:"by"`
	Totals query.CacheTotals `json:"totals"`
}

// cacheRowsReport is the --format json payload of the grouped views. Rows holds
// the ledger's own []query.CacheBucket or []query.CachePoint, so --json stays
// exactly the query contract (docs/CACHE.md §6); the provider health column is
// computed by the consumer from byCause.
type cacheRowsReport struct {
	By   string `json:"by"`
	Rows any    `json:"rows"`
}

// cacheTimelineReport is the --format json payload of a session timeline.
type cacheTimelineReport struct {
	Harness   model.Harness     `json:"harness"`
	SessionID string            `json:"sessionId"`
	Steps     []query.CacheStep `json:"steps"`
}

// CSV headers for the cache views. cost_usd is the miss cost (MissUSD); it is
// dropped, not blanked, under --no-cost.
var (
	cacheCauseCSVHeader    = []string{"cause", "requests", "miss", "cost_usd"}
	cacheBucketCSVHeader   = []string{"key", "requests", "cacheable", "expected", "read", "miss", "cost_usd"}
	cacheDayCSVHeader      = []string{"day", "requests", "cacheable", "expected", "read", "miss", "cost_usd"}
	cacheTimelineCSVHeader = []string{"at", "model", "prompt", "read", "write", "expected", "miss", "cause", "cost_usd", "boundary"}
)

// writeCacheView renders one parsed cache view in the requested format.
func writeCacheView(out io.Writer, env cacheEnv, by string, payload any) error {
	if env.format == formatJSON {
		v := payload
		if env.noCost {
			var e error
			if v, e = stripCostFields(payload, "usd", "costUsd"); e != nil {
				return e
			}
		}
		return json.NewEncoder(out).Encode(v)
	}
	switch v := payload.(type) {
	case cacheTotalsReport:
		if env.format == formatCSV {
			return writeCacheCauseCSV(out, v.Totals, env.noCost)
		}
		return writeCacheTotalsTable(out, v.Totals, env.noCost)
	case cacheRowsReport:
		switch rows := v.Rows.(type) {
		case []query.CacheBucket:
			if env.format == formatCSV {
				return writeCacheBucketsCSV(out, rows, env.noCost, v.By == "provider")
			}
			return writeCacheBucketsTable(out, rows, env.noCost, v.By == "provider")
		case []query.CachePoint:
			if env.format == formatCSV {
				return writeCacheDayCSV(out, rows, env.noCost)
			}
			return writeCacheDayTable(out, rows, env.noCost)
		}
		return fmt.Errorf("no cache view for %T rows", v.Rows)
	case cacheTimelineReport:
		if env.format == formatCSV {
			return writeCacheTimelineCSV(out, v.Steps, env.noCost)
		}
		return writeCacheTimelineTable(out, v.Harness, v.SessionID, v.Steps, env.noCost)
	}
	return fmt.Errorf("no cache view for %T", payload)
}

// cacheSummary is the header line shared by the aggregate views.
func cacheSummary(t query.CacheTotals, noCost bool) string {
	s := fmt.Sprintf("Requests: %d  Cacheable: %d  Expected: %d  Read: %d  Miss: %d  Hit: %s",
		t.Requests, t.Cacheable, t.Expected, t.Read, t.Miss, hitRate(t.Expected, t.Read))
	if !noCost {
		s += fmt.Sprintf("  Miss cost: $%.4f", t.USD)
	}
	return s
}

// hitRate is the share of the tokens that should have been read from cache and
// were: Read / Expected.
func hitRate(expected, read int64) string {
	if expected <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", float64(read)/float64(expected)*100)
}

// healthText is a relay's cache health: its unexplained misses over the tokens
// that should have been read from cache (docs/CACHE.md §6). "-" when nothing was
// expected, so a site with no traffic is not reported as perfect.
func healthText(t query.CacheTotals) string {
	if t.Expected <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", float64(unexplainedMiss(t))/float64(t.Expected)*100)
}

// healthCSV is healthText as a bare percentage for spreadsheets (no "%").
func healthCSV(t query.CacheTotals) string {
	if t.Expected <= 0 {
		return ""
	}
	return fmt.Sprintf("%.2f", float64(unexplainedMiss(t))/float64(t.Expected)*100)
}

func unexplainedMiss(t query.CacheTotals) int64 {
	for _, c := range t.ByCause {
		if c.Cause == causeUnexplained {
			return c.Miss
		}
	}
	return 0
}

// causeText labels a timeline step's miss cause; an uncacheable step has no
// cause at all (Expected < 0).
func causeText(s query.CacheStep) string {
	if s.Cause != "" {
		return s.Cause
	}
	if s.Expected < 0 {
		return "uncacheable"
	}
	return "-"
}

// expectedText renders Expected, where a negative value means the request was
// not cacheable at all.
func expectedText(expected int64) string {
	if expected < 0 {
		return "-"
	}
	return num(expected)
}

func missText(s query.CacheStep) string {
	if s.Expected < 0 {
		return "-"
	}
	return num(s.Miss)
}

// timelineTotals sums a session's steps for the timeline header line.
func timelineTotals(steps []query.CacheStep) query.CacheTotals {
	var t query.CacheTotals
	t.Requests = int64(len(steps))
	for _, s := range steps {
		if s.Expected >= 0 {
			t.Cacheable++
			t.Expected += s.Expected
		}
		t.Read += s.Read
		t.Miss += s.Miss
		t.USD += s.USD
	}
	return t
}

// writeCacheTotalsTable prints the cause breakdown.
func writeCacheTotalsTable(out io.Writer, t query.CacheTotals, noCost bool) error {
	if _, e := fmt.Fprintln(out, cacheSummary(t, noCost)); e != nil {
		return e
	}
	w := newTable(out)
	if noCost {
		fmt.Fprintln(w, "CAUSE\tREQUESTS\tMISS")
	} else {
		fmt.Fprintln(w, "CAUSE\tREQUESTS\tMISS\tMISS USD")
	}
	for _, c := range t.ByCause {
		cause := c.Cause
		if cause == "" {
			cause = "(no miss)"
		}
		if noCost {
			fmt.Fprintf(w, "%s\t%d\t%d\n", cause, c.Requests, c.Miss)
		} else {
			fmt.Fprintf(w, "%s\t%d\t%d\t%.4f\n", cause, c.Requests, c.Miss, c.USD)
		}
	}
	return w.Flush()
}

func writeCacheCauseCSV(out io.Writer, t query.CacheTotals, noCost bool) error {
	recs := make([][]string, 0, len(t.ByCause))
	for _, c := range t.ByCause {
		row := []string{c.Cause, num(c.Requests), num(c.Miss)}
		if !noCost {
			row = append(row, cost(c.USD))
		}
		recs = append(recs, row)
	}
	return writeCSV(out, csvHeaderFor(cacheCauseCSVHeader, noCost), recs)
}

// writeCacheBucketsTable prints one grouped view. health adds the relay cache
// health column and is only used for --by provider.
func writeCacheBucketsTable(out io.Writer, buckets []query.CacheBucket, noCost, health bool) error {
	w := newTable(out)
	header := "KEY\tREQUESTS\tCACHEABLE\tEXPECTED\tREAD\tMISS"
	if !noCost {
		header += "\tMISS USD"
	}
	if health {
		header += "\tHEALTH"
	}
	fmt.Fprintln(w, header)
	for _, b := range buckets {
		t := b.CacheTotals
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%d", b.Key, t.Requests, t.Cacheable, t.Expected, t.Read, t.Miss)
		if !noCost {
			fmt.Fprintf(w, "\t%.4f", t.USD)
		}
		if health {
			fmt.Fprintf(w, "\t%s", healthText(t))
		}
		fmt.Fprintln(w)
	}
	return w.Flush()
}

func writeCacheBucketsCSV(out io.Writer, buckets []query.CacheBucket, noCost, health bool) error {
	header := csvHeaderFor(cacheBucketCSVHeader, noCost)
	if health {
		// Copy: csvHeaderFor returns the shared header slice when cost is kept.
		header = append(append([]string{}, header...), "health")
	}
	recs := make([][]string, 0, len(buckets))
	for _, b := range buckets {
		t := b.CacheTotals
		row := []string{b.Key, num(t.Requests), num(t.Cacheable), num(t.Expected), num(t.Read), num(t.Miss)}
		if !noCost {
			row = append(row, cost(t.USD))
		}
		if health {
			row = append(row, healthCSV(t))
		}
		recs = append(recs, row)
	}
	return writeCSV(out, header, recs)
}

func writeCacheDayTable(out io.Writer, points []query.CachePoint, noCost bool) error {
	w := newTable(out)
	if noCost {
		fmt.Fprintln(w, "DAY\tREQUESTS\tCACHEABLE\tEXPECTED\tREAD\tMISS")
	} else {
		fmt.Fprintln(w, "DAY\tREQUESTS\tCACHEABLE\tEXPECTED\tREAD\tMISS\tMISS USD")
	}
	for _, p := range points {
		t := p.CacheTotals
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%d", p.Day.Format("2006-01-02"), t.Requests, t.Cacheable, t.Expected, t.Read, t.Miss)
		if !noCost {
			fmt.Fprintf(w, "\t%.4f", t.USD)
		}
		fmt.Fprintln(w)
	}
	return w.Flush()
}

func writeCacheDayCSV(out io.Writer, points []query.CachePoint, noCost bool) error {
	recs := make([][]string, 0, len(points))
	for _, p := range points {
		t := p.CacheTotals
		row := []string{p.Day.Format(time.RFC3339), num(t.Requests), num(t.Cacheable), num(t.Expected), num(t.Read), num(t.Miss)}
		if !noCost {
			row = append(row, cost(t.USD))
		}
		recs = append(recs, row)
	}
	return writeCSV(out, csvHeaderFor(cacheDayCSVHeader, noCost), recs)
}

// writeCacheTimelineTable prints one session's per-request cache verdicts.
func writeCacheTimelineTable(out io.Writer, h model.Harness, session string, steps []query.CacheStep, noCost bool) error {
	t := timelineTotals(steps)
	line := fmt.Sprintf("Session: %s/%s  Steps: %d  Expected: %d  Read: %d  Miss: %d  Hit: %s",
		h, session, len(steps), t.Expected, t.Read, t.Miss, hitRate(t.Expected, t.Read))
	if !noCost {
		line += fmt.Sprintf("  Miss cost: $%.4f", t.USD)
	}
	if _, e := fmt.Fprintln(out, line); e != nil {
		return e
	}
	w := newTable(out)
	header := "AT\tMODEL\tPROMPT\tREAD\tWRITE\tEXPECTED\tMISS\tCAUSE"
	if !noCost {
		header += "\tMISS USD"
	}
	header += "\tBOUNDARY"
	fmt.Fprintln(w, header)
	for _, s := range steps {
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s", s.At.Format(time.RFC3339), s.Model, s.Prompt, s.Read, s.Write, expectedText(s.Expected), missText(s), causeText(s))
		if !noCost {
			fmt.Fprintf(w, "\t%.4f", s.USD)
		}
		fmt.Fprintf(w, "\t%s\n", dash(s.Boundary))
	}
	return w.Flush()
}

func writeCacheTimelineCSV(out io.Writer, steps []query.CacheStep, noCost bool) error {
	recs := make([][]string, 0, len(steps))
	for _, s := range steps {
		row := []string{s.At.Format(time.RFC3339), s.Model, num(s.Prompt), num(s.Read), num(s.Write), num(s.Expected), num(s.Miss), s.Cause}
		if !noCost {
			row = append(row, cost(s.USD))
		}
		recs = append(recs, append(row, s.Boundary))
	}
	return writeCSV(out, csvHeaderFor(cacheTimelineCSVHeader, noCost), recs)
}
