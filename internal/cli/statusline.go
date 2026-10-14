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
  --provider NAME     --provider-id ID  --instance-id ID
  --no-cost           drop cost
  --db PATH           read another index (diagnostics only)
Read-only. Cost, cache hit rate and relay balance come from the existing index;
the live token rate is the signal a running scan published, never a number
derived here. A value that has no reliable source is reported as unavailable,
never as zero.
The window zone is --timezone, else the persisted stats_timezone, else the
system zone.`

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
		fmt.Fprintf(errout, "invalid --harness %q (want a harness id such as claude-code, codex or dsh)\n", *harness)
		return 2
	}
	instance := ""
	if len(opts.instanceIDs) == 1 {
		instance = opts.instanceIDs[0]
	}
	tz := ""
	if cf.set["timezone"] {
		tz = opts.loc.String()
	}
	zc := reportingZone(ctx, tz, *dbPath)
	loc := zc.Loc
	return runInLocation(loc, func() int {
		f := opts.filter()
		if *harness != "" {
			f.Harnesses = []model.Harness{model.Harness(*harness)}
		}
		sl := snapshot.BuildStatusline(ctx, snapshot.StatuslineOptions{
			DBPath:             *dbPath,
			SessionID:          *session,
			InstanceID:         instance,
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
				payload, e = stripCost(sl)
				if e != nil {
					fmt.Fprintln(errout, e)
					return 1
				}
			}
			if e = json.NewEncoder(out).Encode(payload); e != nil {
				fmt.Fprintln(errout, e)
				return 1
			}
			return 0
		}
		if _, e = fmt.Fprintln(out, statuslineText(sl, opts.noCost)); e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		return 0
	})
}

// statuslineText renders one line: cost, cache hit rate and balance. Every
// unavailable part says so; nothing is printed as a zero it did not read.
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
		if s.CostSource != "" && s.CostSource != "none" {
			parts = append(parts, s.CostSource)
		}
	}
	parts = append(parts, "balance "+statusBalanceText(sl.Balance))
	parts = append(parts, "ingest "+statusTokRateText(sl.TokRateStatus, sl.TokRate))
	if part := statusClaudeQuotaText(sl.ClaudeQuotaStatus, sl.ClaudeQuota); part != "" {
		parts = append(parts, part)
	}
	if sl.TimezoneSource != "" && sl.TimezoneSource != "system" {
		parts = append(parts, "tz "+sl.Timezone)
	}
	return strings.Join(parts, " · ")
}

// statusClaudeQuotaText renders a concise Claude quota part: one instance per
// account, never combined, and nothing invented. It stays silent when no
// Claude consent or reading is stored, so a statusline for a Codex-only user is
// unchanged. It never prints a credential source reference.
func statusClaudeQuotaText(st snapshot.Status, entries []snapshot.ClaudeQuota) string {
	if len(entries) == 0 {
		return ""
	}
	parts := make([]string, 0, len(entries))
	for _, q := range entries {
		part := q.Instance
		switch {
		case !q.Enabled:
			part += " off"
			if q.Status.Reason != "" {
				part += " (" + q.Status.Reason + ")"
			}
		case !q.Status.Available:
			part += " " + shortStatus(q.Status)
		default:
			used := 0
			for _, w := range q.Windows {
				if w.Present && w.UsedPercent != nil {
					part += fmt.Sprintf(" %s=%.1f%%", w.Kind, *w.UsedPercent)
					used++
				}
			}
			if used == 0 {
				part += " reading unavailable"
			}
			part += " " + claudeStatuslineAge(q)
		}
		if q.Forecast != nil && q.Forecast.Available && len(q.Forecast.Windows) > 0 {
			w := q.Forecast.Windows[0]
			if w.Available {
				part += fmt.Sprintf(" %.2f%%/h", w.Pace.PercentPerHour)
			}
		}
		parts = append(parts, "claude "+part)
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "claude " + strings.Join(parts, "; ")
}

// claudeStatuslineAge is the as-of age of the cached reading, prefixed so it
// cannot read as a live value.
func claudeStatuslineAge(q snapshot.ClaudeQuota) string {
	if q.FetchedAt == nil {
		return "no reading"
	}
	if q.AgeSeconds == nil {
		return "as-of " + q.FetchedAt.Format(time.RFC3339)
	}
	age := time.Duration(*q.AgeSeconds) * time.Second
	if q.Status.Reason == "stale" {
		return fmt.Sprintf("as-of %s stale", age.Round(time.Second))
	}
	return "as-of " + age.Round(time.Second).String()
}

// statusTokRateText renders the live-ingestion rate a running scan published.
// An absent, expired or unusable signal names its reason instead of a zero: a
// statusline process cannot measure ingestion itself. The rate is shown with
// its as-of age, because it measures the window that ended at that timestamp
// and is not a live moving-window aggregate at the instant of the query.
func statusTokRateText(st snapshot.Status, tr *snapshot.TokRate) string {
	if !st.Available || tr == nil {
		return shortStatus(st)
	}
	if tr.WindowSeconds > 0 {
		return fmt.Sprintf("%.2f tok/s over %.0fs (as-of %.0fs)", tr.Rate, tr.WindowSeconds, tr.AgeSeconds)
	}
	return fmt.Sprintf("%.2f tok/s (as-of %.0fs)", tr.Rate, tr.AgeSeconds)
}

// statusBalanceText renders the balance part of one line.
//
// A single unambiguous recorded account is shown as its own value. Anything
// else is shown as unavailable with the reason and with every account listed
// separately by its original origin + key id: amounts from different keys are
// never added, the first currency is never worn by the rest, and the freshest
// timestamp is never presented as the time of the whole reading.
func statusBalanceText(b snapshot.BalanceLine) string {
	if b.Available {
		if b.Unlimited {
			return "unlimited"
		}
		if b.RemainingUSD != nil {
			return moneyText(*b.RemainingUSD, b.Currency)
		}
		if b.UsedUSD != nil {
			return "used " + moneyText(*b.UsedUSD, b.Currency)
		}
		return "unavailable"
	}
	reason := "unavailable"
	if b.Reason != "" {
		reason = "unavailable (" + b.Reason + ")"
	}
	if len(b.Accounts) == 0 {
		return reason
	}
	parts := make([]string, 0, len(b.Accounts))
	for _, a := range b.Accounts {
		parts = append(parts, accountBalanceText(a))
	}
	if b.Partial {
		reason += ", partial"
	}
	return fmt.Sprintf("%s: %d account(s) %s", reason, len(b.Accounts), strings.Join(parts, "; "))
}

// accountBalanceText renders one relay account by its own identity, so several
// accounts stay several values a reader can tell apart. The account's own
// timestamp is printed with it: when accounts are listed separately there is no
// single "the balance time", so each value carries its own.
func accountBalanceText(a snapshot.Balance) string {
	id := shortID(a.KeyID)
	if a.Origin != "" {
		id = shortOrigin(a.Origin) + "/" + id
	}
	var text string
	switch {
	case a.Unlimited:
		text = id + "=unlimited"
	case a.RemainingUSD != nil:
		text = id + "=" + moneyText(*a.RemainingUSD, a.Currency)
	case a.UsedUSD != nil:
		text = id + "=used " + moneyText(*a.UsedUSD, a.Currency)
	default:
		text = id + "=no amount recorded"
	}
	if !a.At.IsZero() {
		text += " at " + a.At.UTC().Format(time.RFC3339)
	}
	return text
}

// moneyText renders an amount in its own currency. An empty currency is an
// unstated one, not a dollar sign: only a stated USD becomes a "$".
func moneyText(v float64, currency string) string {
	switch currency {
	case "":
		return fmt.Sprintf("%.2f", v)
	case "USD":
		return fmt.Sprintf("$%.2f", v)
	default:
		return fmt.Sprintf("%.2f %s", v, currency)
	}
}

// shortOrigin trims the scheme from a relay origin for a one-line display.
func shortOrigin(origin string) string {
	return strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
}

func shortStatus(st snapshot.Status) string {
	if st.Available {
		return "ok"
	}
	if st.Reason == "" {
		return "unavailable"
	}
	return "unavailable (" + st.Reason + ")"
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
