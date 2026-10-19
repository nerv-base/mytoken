package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/snapshot"
)

const statuslineUsage = `Usage: mytoken statusline [options]
  --json              JSON payload (default: one line)
  --session ID        session to report (default: most recent in the window)
  --harness NAME      harness of --session
  --since DATE|DUR    --until DATE|DUR  --last WINDOW  --timezone TZ
  --provider NAME     --provider-id ID
  --no-cost           drop cost
  --db PATH           read another index (diagnostics only)
Read-only. Cost, cache hit rate and plan quotas come from the existing index;
the live token rate is the signal a running scan published, never a number
derived here. A value that has no reliable source is reported as unavailable,
never as zero.`

func runStatusline(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("statusline", flag.ContinueOnError)
	fs.SetOutput(errout)
	var cf commonFlags
	addCommonFlags(fs, &cf, "1d")
	session := fs.String("session", "", "session id")
	harness := fs.String("harness", "", "harness of --session")
	dbPath := fs.String("db", "", "index path override")
	if e := cf.parse(fs, args); e != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprint(errout, statuslineUsage)
		return 2
	}
	opts, e := cf.resolve(time.Now())
	if e != nil {
		fmt.Fprintln(errout, e)
		return 2
	}
	if *harness != "" && !knownHarness(*harness) {
		fmt.Fprintf(errout, "invalid --harness %q\n", *harness)
		return 2
	}
	tz := ""
	if cf.set["timezone"] {
		tz = opts.loc.String()
	}
	zc := reportingZone(ctx, tz, *dbPath)
	return runInLocation(zc.Loc, func() int {
		f := opts.filter()
		if *harness != "" {
			f.Harnesses = []model.Harness{model.Harness(*harness)}
		}
		sl := snapshot.BuildStatusline(ctx, snapshot.StatuslineOptions{
			DBPath:             *dbPath,
			SessionID:          *session,
			Harness:            model.Harness(*harness),
			Filter:             f,
			Timezone:           tz,
			Zone:               zc.Name,
			ZoneSource:         zc.Source,
			ZoneNeedsSelection: zc.NeedsSelection,
			ZoneProblem:        zc.Problem,
			ZoneSystemPreview:  zc.Preview,
		})
		if cf.json || opts.format == formatJSON {
			payload := any(sl)
			if opts.noCost {
				p, err := stripCost(sl)
				if err != nil {
					fmt.Fprintln(errout, err)
					return 1
				}
				payload = p
			}
			if e := json.NewEncoder(out).Encode(payload); e != nil {
				fmt.Fprintln(errout, e)
				return 1
			}
			return 0
		}
		if _, e := fmt.Fprintln(out, statuslineText(sl, opts.noCost)); e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		return 0
	})
}

// statuslineText renders one line: cost, cache hit rate, ingestion rate and
// plan quotas. Every unavailable part says so; nothing is printed as a zero it
// did not read.
func statuslineText(sl *snapshot.Statusline, noCost bool) string {
	if !sl.Index.Available {
		return "index " + shortStatus(sl.Index)
	}
	var parts []string
	if sl.Session == nil {
		parts = append(parts, "session "+shortStatus(sl.SessionStatus))
	} else {
		s := sl.Session
		if !noCost {
			parts = append(parts, fmt.Sprintf("$%.4f", s.CostUSD))
		}
		parts = append(parts, fmt.Sprintf("cache %.1f%%", s.CacheHit*100))
		parts = append(parts, fmt.Sprintf("%d req", s.Requests))
		parts = append(parts, s.Harness+"/"+shortID(s.SessionID))
	}
	parts = append(parts, "ingest "+statusTokRateText(sl.TokRateStatus, sl.TokRate))
	parts = append(parts, planQuotaText(sl.QuotaStatus, sl.Plans))
	if sl.TimezoneSource != "" && sl.TimezoneSource != "system" {
		parts = append(parts, "tz "+sl.Timezone)
	}
	return strings.Join(parts, " · ")
}

func planQuotaText(st snapshot.Status, plans []snapshot.PlanQuota) string {
	if len(plans) == 0 {
		return "quota " + shortStatus(st)
	}
	parts := make([]string, 0, len(plans))
	for _, p := range plans {
		part := p.Provider
		if p.Plan != "" {
			part = p.Provider + " " + p.Plan
		}
		for _, w := range p.Windows {
			part += fmt.Sprintf(" %s=%.0f%%", w.Name, w.UsedPercent)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

func statusTokRateText(st snapshot.Status, tr *snapshot.TokRate) string {
	if !st.Available || tr == nil {
		return shortStatus(st)
	}
	if tr.WindowSeconds > 0 {
		return fmt.Sprintf("%.2f tok/s over %.0fs (as-of %.0fs)", tr.Rate, tr.WindowSeconds, tr.AgeSeconds)
	}
	return fmt.Sprintf("%.2f tok/s (as-of %.0fs)", tr.Rate, tr.AgeSeconds)
}

func shortStatus(st snapshot.Status) string {
	if st.Available {
		return "ok"
	}
	reason := st.Reason
	if reason == "" {
		reason = "unavailable"
	}
	if reason == "no_observation" || reason == "no_data" {
		return "none"
	}
	return reason
}

func knownHarness(name string) bool { return model.Harness(name).Known() }

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
