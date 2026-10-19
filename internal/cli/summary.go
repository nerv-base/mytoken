package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/paths"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/store"
)

// sumPeriod is one calendar period's API-equivalent summary.
type sumPeriod struct {
	Label    string       `json:"label"`
	From     time.Time    `json:"from"`
	To       time.Time    `json:"to"`
	Tokens   model.Tokens `json:"tokens"`
	CostUSD  float64      `json:"costUsd"`
	Unpriced int64        `json:"unpriced"`
	Requests int64        `json:"requests"`
}

type sumReport struct {
	Timezone string    `json:"timezone"`
	Today    sumPeriod `json:"today"`
	Month    sumPeriod `json:"month"`
}

// runSummary prints today's and this month's cost, the default command.
func runSummary(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("summary", flag.ContinueOnError)
	fs.SetOutput(errout)
	js := fs.Bool("json", false, "JSON output")
	noCost := fs.Bool("no-cost", false, "omit cost")
	tz := fs.String("timezone", "", "reporting timezone")
	dbPath := fs.String("db", "", "index path override")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprint(errout, "usage: mytoken [summary] [--json] [--no-cost] [--timezone TZ] [--db PATH]\n")
		return 2
	}
	loc, name, err := resolveReportingZone(ctx, *tz, *dbPath)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 2
	}
	ro, q, err := openQueryReadOnly(ctx, *dbPath)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	defer ro.Close()
	now := time.Now().In(loc)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	day, err := q.Totals(ctx, query.Filter{Range: query.Range{From: dayStart, To: now}})
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	month, err := q.Totals(ctx, query.Filter{Range: query.Range{From: monthStart, To: now}})
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	rep := sumReport{
		Timezone: name,
		Today:    sumPeriod{Label: "today", From: dayStart, To: now, Tokens: day.Tokens, CostUSD: day.CostUSD, Unpriced: day.Unpriced, Requests: day.Requests},
		Month:    sumPeriod{Label: "month", From: monthStart, To: now, Tokens: month.Tokens, CostUSD: month.CostUSD, Unpriced: month.Unpriced, Requests: month.Requests},
	}
	if *js {
		if *noCost {
			rep.Today.CostUSD, rep.Month.CostUSD = 0, 0
		}
		if err := json.NewEncoder(out).Encode(rep); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(out, "API-equivalent cost · %s\n", name)
	fmt.Fprintf(out, "Today  %s  %s  %d requests · cache hit %s\n", money(day.CostUSD, day.Unpriced, *noCost), tokensLine(day.Tokens), day.Requests, pct(day.Tokens.HitRate()))
	fmt.Fprintf(out, "Month  %s  %s  %d requests · cache hit %s\n", money(month.CostUSD, month.Unpriced, *noCost), tokensLine(month.Tokens), month.Requests, pct(month.Tokens.HitRate()))
	return 0
}

// runDaily prints one row per day, in the style of ccusage daily: token classes
// in separate columns and cost labelled as API-equivalent.
func runDaily(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("daily", flag.ContinueOnError)
	fs.SetOutput(errout)
	days := fs.Int("days", 30, "how many days back")
	js := fs.Bool("json", false, "JSON output")
	noCost := fs.Bool("no-cost", false, "omit cost")
	tz := fs.String("timezone", "", "reporting timezone")
	dbPath := fs.String("db", "", "index path override")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *days <= 0 {
		fmt.Fprint(errout, "usage: mytoken daily [--days N] [--json] [--no-cost] [--timezone TZ] [--db PATH]\n")
		return 2
	}
	loc, name, err := resolveReportingZone(ctx, *tz, *dbPath)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 2
	}
	// query.Daily buckets by time.Local (internal/query/service.go floor()), so
	// --timezone must become the process zone while the points are bucketed and
	// formatted; otherwise the JSON label and the day rows disagree with it.
	return runInLocation(loc, func() int {
		ro, q, err := openQueryReadOnly(ctx, *dbPath)
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		defer ro.Close()
		now := time.Now().In(loc)
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		start := today.AddDate(0, 0, -(*days - 1))
		// Whole days on both ends keep the read on the day rollups; a bound
		// inside a day makes the query re-aggregate raw events.
		pts, err := q.Daily(ctx, query.Filter{Range: query.Range{From: start, To: today.AddDate(0, 0, 1)}})
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		if *js {
			if *noCost {
				for i := range pts {
					pts[i].CostUSD = 0
				}
			}
			if err := json.NewEncoder(out).Encode(map[string]any{"timezone": name, "days": pts}); err != nil {
				fmt.Fprintln(errout, err)
				return 1
			}
			return 0
		}
		w := newTable(out)
		if *noCost {
			fmt.Fprintln(w, "DATE\tINPUT\tOUTPUT\tCACHE READ\tCACHE WRITE\tTOTAL\tCACHE HIT")
		} else {
			fmt.Fprintln(w, "DATE\tINPUT\tOUTPUT\tCACHE READ\tCACHE WRITE\tTOTAL\tAPI-EQUIV\tCACHE HIT")
		}
		for _, p := range pts {
			if *noCost {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.Day.Format("2006-01-02"), fmtInt(p.Tokens.Input), fmtInt(p.Tokens.Output), fmtInt(p.Tokens.CacheRead), fmtInt(p.Tokens.CacheWrite), fmtInt(p.Tokens.Total()), pct(p.Tokens.HitRate()))
				continue
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.Day.Format("2006-01-02"), fmtInt(p.Tokens.Input), fmtInt(p.Tokens.Output), fmtInt(p.Tokens.CacheRead), fmtInt(p.Tokens.CacheWrite), fmtInt(p.Tokens.Total()), money(p.CostUSD, 0, false), pct(p.Tokens.HitRate()))
		}
		if err := w.Flush(); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		return 0
	})
}

func resolveReportingZone(ctx context.Context, explicit, dbOverride string) (*time.Location, string, error) {
	zc := reportingZone(ctx, explicit, dbOverride)
	return zc.Loc, zc.Name, nil
}

// openQueryReadOnly opens the index without creating, migrating or repricing it.
func openQueryReadOnly(ctx context.Context, dbPath string) (*store.Store, query.Service, error) {
	path := dbPath
	if path == "" {
		p, err := paths.ResolveDBPath()
		if err != nil {
			return nil, nil, err
		}
		path = p
	}
	ro, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	return ro, query.NewService(ro), nil
}

func money(cost float64, unpriced int64, noCost bool) string {
	if noCost {
		return ""
	}
	s := fmt.Sprintf("$%.2f", cost)
	if unpriced > 0 {
		s += " (+unpriced)"
	}
	return s
}

func tokensLine(t model.Tokens) string {
	return fmt.Sprintf("%s tokens (in %s, out %s, cache read %s, write %s)", fmtInt(t.Total()), fmtInt(t.Input), fmtInt(t.Output), fmtInt(t.CacheRead), fmtInt(t.CacheWrite))
}

func pct(v float64) string { return fmt.Sprintf("%.1f%%", v*100) }

func fmtInt(v int64) string { return strconv.FormatInt(v, 10) }
