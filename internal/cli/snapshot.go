package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/app"
	"github.com/zzstar101/mytoken/internal/snapshot"
	"github.com/zzstar101/mytoken/internal/store"
)

const snapshotUsage = `Usage: mytoken snapshot --once [options]
  --once            print exactly one document and exit (required)
  --json            JSON envelope (default: human summary)
  --refresh         scan once before reading, then read (the only write path)
  --since DATE|DUR  --until DATE|DUR  --last WINDOW  --timezone TZ
  --provider NAME   --provider-id ID  --instance-id ID
  --no-cost         drop cost fields
  --db PATH         read another index (diagnostics only)
The default path is read-only: store.OpenReadOnly + paths.ResolveDBPath.
The reporting zone is --timezone, else the persisted stats_timezone, else the
system zone; a missing stats_timezone is reported as absent, never invented.
Missing, locked, corrupt, newer or unsupported indexes are reported with a
typed reason instead of being created, migrated or repriced. Reading a WAL index
lets SQLite create its own -wal/-shm coordination sidecars; no index data is
written, and an upgradable schema needs "mytoken scan" (a writable startup).`

func runSnapshot(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	fs.SetOutput(errout)
	var cf commonFlags
	addCommonFlags(fs, &cf, "")
	once := fs.Bool("once", false, "print one document and exit")
	refresh := fs.Bool("refresh", false, "scan once before reading")
	dbPath := fs.String("db", "", "index path override")
	if e := cf.parse(fs, args); e != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprint(errout, snapshotUsage)
		return 2
	}
	if !*once {
		fmt.Fprintln(errout, "snapshot requires --once; continuous mode is not implemented")
		fmt.Fprint(errout, snapshotUsage)
		return 2
	}
	opts, e := cf.resolve(time.Now())
	if e != nil {
		fmt.Fprintln(errout, e)
		return 2
	}
	performed := []string{}
	if *refresh {
		a, err := app.OpenLocal()
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		err = a.Scanner.Scan(ctx)
		a.Close()
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		performed = append(performed, "scan")
	}
	tz := ""
	if cf.set["timezone"] {
		tz = opts.loc.String()
	}
	// The reporting zone is resolved before the read so the envelope and the
	// queries agree: the persisted stats_timezone is used when it exists. An
	// unresolved calendar is reported as such (with the system zone as a
	// preview), never as a fixed zone the app did not persist.
	zc := reportingZone(ctx, tz, *dbPath)
	loc := zc.Loc
	return runInLocation(loc, func() int {
		snap := snapshot.Build(ctx, snapshot.Options{
			DBPath:             *dbPath,
			Filter:             opts.filter(),
			Performed:          performed,
			Timezone:           tz,
			Zone:               zc.Name,
			ZoneSource:         zc.Source,
			ZoneNeedsSelection: zc.NeedsSelection,
			ZoneProblem:        zc.Problem,
			ZoneSystemPreview:  zc.Preview,
		})
		if opts.format == formatJSON {
			payload := any(snap)
			if opts.noCost {
				payload, e = stripCost(snap)
				if e != nil {
					fmt.Fprintln(errout, e)
					return 1
				}
			}
			if e = json.NewEncoder(out).Encode(payload); e != nil {
				fmt.Fprintln(errout, e)
				return 1
			}
		} else if e = writeSnapshotText(out, snap, opts.noCost); e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		// A newer or upgradable schema is an actionable refusal from the frozen
		// contract (§4): report it, never migrate it here.
		switch snap.Schema.State {
		case string(store.SchemaNewer):
			fmt.Fprintln(errout, "本机数据库由更新版本的 MyToken 写入，请升级 MyToken")
			return 1
		case string(store.SchemaUpgradeRequired):
			fmt.Fprintln(errout, "the index schema needs an upgrade; run `mytoken scan` once (a normal writable startup), then retry")
			return 1
		}
		return 0
	})
}

func writeSnapshotText(out io.Writer, s *snapshot.Snapshot, noCost bool) error {
	fmt.Fprintf(out, "snapshot v%d  generated %s\n", s.SnapshotVersion, s.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(out, "index      %s\n", statusText(s.Sources.Index))
	fmt.Fprintf(out, "schema     %s", s.Schema.State)
	if s.Schema.Detail != "" {
		fmt.Fprintf(out, " (%s)", s.Schema.Detail)
	}
	fmt.Fprintln(out)
	if s.Data.Totals != nil {
		t := s.Data.Totals
		line := fmt.Sprintf("totals     requests=%d sessions=%d tokens=%d", t.Requests, t.Sessions, t.Tokens.Total())
		if !noCost {
			line += fmt.Sprintf(" cost=$%.4f", t.CostUSD)
		}
		fmt.Fprintf(out, "%s cacheHit=%.1f%% unpriced=%d\n", line, t.CacheHit*100, t.Unpriced)
	} else {
		fmt.Fprintln(out, "totals     unavailable")
	}
	if s.Data.Cache != nil {
		c := s.Data.Cache
		line := fmt.Sprintf("cache      requests=%d expected=%d read=%d miss=%d", c.Requests, c.Expected, c.Read, c.Miss)
		if !noCost {
			line += fmt.Sprintf(" usd=$%.4f", c.USD)
		}
		fmt.Fprintln(out, line)
	} else {
		fmt.Fprintln(out, "cache      unavailable")
	}
	fr := s.Freshness
	fmt.Fprintf(out, "freshness  eventsThrough=%s sessionsThrough=%s sourceModTime=%s\n", timeText(fr.EventsThrough), timeText(fr.SessionsThrough), timeText(fr.SourceModTime))
	fmt.Fprintf(out, "           rollupZone=%s localZone=%s rollupsCurrent=%t statsTimezone=%s tz=%s(%s)\n", zoneText(fr.RollupZone), zoneText(fr.LocalZone), fr.RollupsCurrent, statsZoneText(fr), zoneText(fr.EffectiveTimezone), fr.TimezoneSource)
	if fr.NeedsTimezoneSelection {
		fmt.Fprintf(out, "           timezoneNeedsSelection=true systemPreview=%s note=%q\n", zoneText(fr.SystemZonePreview), fr.TimezoneProblem)
	}
	fmt.Fprintf(out, "sources    pricing=%s balance=%s tokRate=%s quota=%s claudeQuota=%s diagnostics=%s\n", statusText(s.Sources.Pricing), statusText(s.Sources.Balance), statusText(s.Sources.TokRate), statusText(s.Sources.Quota), statusText(s.Sources.ClaudeQuota), statusText(s.Sources.Diagnostics))
	if err := writeTokRate(out, s); err != nil {
		return err
	}
	if err := writeDiagnosticsSummary(out, s.Data.Diagnostics); err != nil {
		return err
	}
	fmt.Fprintf(out, "writes     performed=%v potential=%v\n", s.Writes.Performed, s.Writes.Potential)
	if err := writeQuotaSummary(out, s.Data.CodexQuota); err != nil {
		return err
	}
	return writeClaudeQuotaSummary(out, s.Data.ClaudeQuota)
}

// writeTokRate prints the live-ingestion signal a running scan left in the
// index. An absent, expired or unusable signal names its reason; the number is
// never derived from stored token counts. The rate is an as-of measurement: it
// covers the window that ended at the timestamp, and the age says how far the
// query instant is from that measurement. It is never presented as a live
// moving-window aggregate at the instant of the query.
func writeTokRate(out io.Writer, s *snapshot.Snapshot) error {
	if !s.Sources.TokRate.Available || s.Data.TokRate == nil {
		_, err := fmt.Fprintf(out, "tokRate    %s\n", statusText(s.Sources.TokRate))
		return err
	}
	tr := s.Data.TokRate
	_, err := fmt.Fprintf(out, "tokRate    %.2f tok/s over %.0fs tokens=%d at=%s age=%.0fs (as-of, not a live window)\n",
		tr.Rate, tr.WindowSeconds, tr.Tokens, tr.At.Format(time.RFC3339), tr.AgeSeconds)
	return err
}

// writeClaudeQuotaSummary lists the persisted Claude quota instances. Like the
// Codex summary it never merges instances and never fills a missing window with
// zero. Unlike the Codex summary it is cached consent state: a disabled
// instance, a missing consent and an unfetched reading are all named as such,
// and no credential source reference is printed here — the snapshot is
// plaintext and the source is disclosed only by `quota claude info`.
func writeClaudeQuotaSummary(out io.Writer, entries []snapshot.ClaudeQuota) error {
	if len(entries) == 0 {
		fmt.Fprintln(out, "claudeQuota no stored Claude quota consent or reading")
		return nil
	}
	fmt.Fprintf(out, "claudeQuota %d instance(s), not combined\n", len(entries))
	for _, q := range entries {
		line := fmt.Sprintf("  %s enabled=%t status=%s", q.Instance, q.Enabled, statusText(q.Status))
		if q.ReadingStatus != "" {
			line += " reading=" + q.ReadingStatus
			if q.ReadingReason != "" {
				line += "(" + q.ReadingReason + ")"
			}
		}
		if q.FetchedAt != nil {
			line += " fetched=" + q.FetchedAt.Format(time.RFC3339)
			line += " age=" + claudeAgeText(q.AgeSeconds, q.StaleAfterSeconds)
		}
		for _, w := range q.Windows {
			line += " " + claudeWindowText(w)
		}
		line += " forecast=" + claudeForecastText(q.Forecast)
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return nil
}

// writeQuotaSummary lists the persisted Codex quota instances. It prints one
// line per instance: instances are never merged and a missing window is named,
// never filled in from another instance or from token counts.
func writeQuotaSummary(out io.Writer, entries []snapshot.CodexQuota) error {
	if len(entries) == 0 {
		fmt.Fprintln(out, "quota      no stored Codex quota observation")
		return nil
	}
	fmt.Fprintf(out, "quota      %d instance(s), not combined\n", len(entries))
	for _, q := range entries {
		line := fmt.Sprintf("  %s status=%s", q.Instance, statusText(q.Status))
		if q.ObservedAt != nil {
			line += " observed=" + q.ObservedAt.Format(time.RFC3339)
		}
		if q.IngestedAt != nil {
			line += " ingested=" + q.IngestedAt.Format(time.RFC3339)
		}
		line += " primary=" + quotaWindowText(q.Primary) + " secondary=" + quotaWindowText(q.Secondary)
		line += " forecast=" + quotaForecastText(q.Forecast)
		if pace := quotaPaceText(q.Pace); pace != "" {
			line += " pace=" + pace
		}
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return nil
}

func quotaWindowText(w *snapshot.QuotaWindow) string {
	if w == nil {
		return "not_reported"
	}
	parts := []string{string(w.Status)}
	if w.UsedPercent != nil {
		parts = append(parts, fmt.Sprintf("used=%.1f%%", *w.UsedPercent))
	}
	if w.RemainingPercent != nil {
		parts = append(parts, fmt.Sprintf("remaining=%.1f%%", *w.RemainingPercent))
	}
	if w.ResetsAt != nil {
		parts = append(parts, "resetsAt="+w.ResetsAt.Format(time.RFC3339))
	} else if w.ResetsInSeconds != nil {
		parts = append(parts, fmt.Sprintf("resetsIn=%ds", *w.ResetsInSeconds))
	}
	return strings.Join(parts, " ")
}

func quotaForecastText(f snapshot.QuotaForecast) string {
	if f.Available {
		return "available"
	}
	if f.Reason == "" {
		return "unavailable"
	}
	return "unavailable (" + f.Reason + ")"
}

func statusText(st snapshot.Status) string {
	if st.Available {
		if st.FetchedAt != nil {
			return "available (fetched " + st.FetchedAt.Format(time.RFC3339) + ")"
		}
		return "available"
	}
	if st.Reason == "" {
		return "unavailable"
	}
	return "unavailable (" + st.Reason + ")"
}

func timeText(t *time.Time) string {
	if t == nil {
		return "null"
	}
	return t.Format(time.RFC3339)
}

// zoneText keeps the human summary short: the full rollup-zone identity is a
// long string by design and stays available in JSON.
func zoneText(z string) string {
	switch {
	case z == "":
		return "not_recorded"
	case z == store.LocalDayZone():
		return "local"
	default:
		return z
	}
}

func statsZoneText(fr snapshot.Freshness) string {
	if !fr.StatsTimezoneRecorded {
		return "not_recorded"
	}
	if fr.StatsTimezone == "" {
		return "empty"
	}
	return fr.StatsTimezone
}
