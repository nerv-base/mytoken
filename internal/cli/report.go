package cli

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/zzstar101/mytoken/internal/paths"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/reporttime"
	"github.com/zzstar101/mytoken/internal/store"
)

const reportUsage = `Usage: mytoken report [day|week|month] [options]
  --timezone TZ     reporting timezone (default: the persisted stats_timezone,
                    else the system zone)
  --json            JSON output  --format table|json|csv
  --by session|project|branch   add a grouped breakdown (repeatable)
  --provider NAME   --provider-id ID  --instance-id ID
  --no-cost         drop cost
  --db PATH         read another index (diagnostics only)
The period boundaries come from reporttime.Resolve: weeks start on Monday and
the comparison is the previous period's same elapsed span. Read-only.
--by project groups by the canonical repository column: the store resolves a
recorded path to its repository root at write time, so linked worktrees and
separate paths of one repository are already folded before the report runs.
--by session lists the busiest sessions of each period (20) and a parent's row
includes its children. Groups are ranked inside the same read-only transaction
and with the same filters as the period totals.
--by branch groups by repository and the branch the log recorded for each
request - never the working tree's current HEAD. A request whose log stated no
branch appears as its own "(unknown)" row instead of being dropped, and a branch
name alone is not a group because two repositories can share one. An index
written before branch metadata existed reports every request as unknown; reading
a report never rescans, so branch fills in only when a normal scan runs.`

// reportGroupSessions caps a --by session breakdown per period.
const reportGroupSessions = 20

// reportView is one period's measured values.
type reportView struct {
	Kind   string            `json:"kind"`
	ID     string            `json:"id"`
	Start  time.Time         `json:"start"`
	End    time.Time         `json:"end"`
	Totals query.Totals      `json:"totals"`
	Cache  query.CacheTotals `json:"cache"`
}

// reportGroup is one --by dimension ranked for both periods. Current keeps the
// query order (tokens desc); Comparison is the same ranking for the previous
// span, matched by key so an entry that only exists in one period is visible.
type reportGroup struct {
	Dim        string           `json:"dim"`
	Note       string           `json:"note"`
	Total      int              `json:"total"`
	Current    []reportGroupRow `json:"current"`
	Comparison []reportGroupRow `json:"comparison"`
}

type reportGroupRow struct {
	Key      string  `json:"key"`
	Label    string  `json:"label,omitempty"`
	Project  string  `json:"project,omitempty"`
	Branch   string  `json:"branch,omitempty"`
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"costUsd"`
	Requests int64   `json:"requests"`
	Sessions int64   `json:"sessions,omitempty"`
}

// reportUnknownBranch labels a repository×branch bucket whose log recorded no
// branch. Reporting it explicitly is the point: an unknown branch must stay
// visible as its own row instead of looking like usage that was dropped.
const reportUnknownBranch = "(unknown)"

type reportReport struct {
	Timezone       string        `json:"timezone"`
	TimezoneSource string        `json:"timezoneSource"`
	Now            time.Time     `json:"now"`
	Current        reportView    `json:"current"`
	Comparison     reportView    `json:"comparison"`
	Change         reportChange  `json:"change"`
	Groups         []reportGroup `json:"groups,omitempty"`
}

type reportChange struct {
	CostPct     *float64 `json:"costPct"`
	RequestsPct *float64 `json:"requestsPct"`
	TokensPct   *float64 `json:"tokensPct"`
	CacheHitPP  *float64 `json:"cacheHitPp"`
}

func runReport(ctx context.Context, args []string, out, errout io.Writer) int {
	flags, positional, err := splitArgs(args, map[string]bool{"json": true, "no-cost": true})
	if err != nil {
		fmt.Fprintln(errout, err)
		return 2
	}
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(errout)
	js := fs.Bool("json", false, "JSON output")
	noCost := fs.Bool("no-cost", false, "drop cost")
	format := fs.String("format", "", "table|json|csv")
	tz := fs.String("timezone", "", "reporting timezone")
	dbPath := fs.String("db", "", "index path override")
	var providers, providerIDs, instanceIDs stringList
	fs.Var(&providers, "provider", "pricing provider name")
	fs.Var(&providerIDs, "provider-id", "stable endpoint identity key")
	fs.Var(&instanceIDs, "instance-id", "data source instance id")
	var by stringList
	fs.Var(&by, "by", "group by session|project|branch (repeatable)")
	if err = fs.Parse(flags); err != nil {
		return 2
	}
	if len(positional) > 1 {
		fmt.Fprint(errout, reportUsage)
		return 2
	}
	kind := reporttime.Day
	if len(positional) == 1 {
		k, e := reporttime.ParseKind(positional[0])
		if e != nil {
			fmt.Fprintln(errout, e)
			fmt.Fprint(errout, reportUsage)
			return 2
		}
		kind = k
	}
	groups, err := reportGroupDims(by.values())
	if err != nil {
		fmt.Fprintln(errout, err)
		fmt.Fprint(errout, reportUsage)
		return 2
	}
	of, err := resolveFormat(*format, *js, *format != "")
	if err != nil {
		fmt.Fprintln(errout, err)
		return 2
	}
	if *tz != "" {
		if _, e := time.LoadLocation(*tz); e != nil {
			fmt.Fprintln(errout, "invalid --timezone:", e)
			return 2
		}
	}
	// Same rule as snapshot/statusline: an explicit --timezone wins, then the
	// persisted stats_timezone, then the process zone, and the period
	// boundaries are computed in exactly the zone that is reported. With no
	// usable fixed calendar the periods are a preview in the process zone, and
	// the header says so instead of claiming a calendar.
	zc := reportingZone(ctx, *tz, *dbPath)
	if zc.NeedsSelection {
		fmt.Fprintln(errout, "warning: reporting timezone is unconfigured:", zc.Problem)
	}
	loc, zone, zoneSource := zc.Loc, zc.Name, zc.Source
	now := time.Now()
	state, err := reporttime.Resolve(now, kind, loc)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 2
	}
	filter := func(w reporttime.Window) query.Filter {
		return query.Filter{
			Range:       query.Range{From: w.Start, To: w.End},
			Providers:   providers.values(),
			ProviderIDs: providerIDs.values(),
			InstanceIDs: instanceIDs.values(),
		}
	}
	opts := reportOptions{
		dbPath:       *dbPath,
		format:       of,
		noCost:       *noCost,
		timezone:     zone,
		timezoneFrom: zoneSource,
		current:      filter(state.Current.Window()),
		compare:      filter(state.Comparison),
		groups:       groups,
	}
	return runInLocation(loc, func() int { return reportCommand(ctx, state, opts, out, errout) })
}

// reportGroupDims validates --by and keeps the caller's order, without repeats.
func reportGroupDims(values []string) ([]query.GroupDim, error) {
	var dims []query.GroupDim
	seen := map[query.GroupDim]bool{}
	for _, v := range values {
		var dim query.GroupDim
		switch v {
		case string(query.GroupByProject):
			dim = query.GroupByProject
		case string(query.GroupBySession):
			dim = query.GroupBySession
		case string(query.GroupByBranch):
			dim = query.GroupByBranch
		default:
			return nil, fmt.Errorf("invalid --by %q (want session, project or branch)", v)
		}
		if seen[dim] {
			continue
		}
		seen[dim] = true
		dims = append(dims, dim)
	}
	return dims, nil
}

type reportOptions struct {
	dbPath       string
	format       outputFormat
	noCost       bool
	timezone     string
	timezoneFrom string
	current      query.Filter
	compare      query.Filter
	groups       []query.GroupDim
}

func reportCommand(ctx context.Context, state reporttime.State, opts reportOptions, out, errout io.Writer) int {
	db := opts.dbPath
	if db == "" {
		p, err := paths.ResolveDBPath()
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		db = p
	}
	ro, err := store.OpenReadOnly(ctx, db)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	defer ro.Close()
	// The groups are ranked inside the same read-only transaction and with the
	// same filters as the totals, so a group can never disagree with its period.
	envOpts := query.EnvelopeOptions{Groups: opts.groups, SessionLimit: reportGroupSessions}
	cur, err := query.ReadEnvelope(ctx, ro, opts.current, envOpts)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	prev, err := query.ReadEnvelope(ctx, ro, opts.compare, envOpts)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	view := func(p reporttime.Period, env query.Envelope) reportView {
		return reportView{Kind: string(p.Kind), ID: p.ID(), Start: p.Start, End: p.End, Totals: env.Totals, Cache: env.Cache}
	}
	report := reportReport{
		Timezone:       opts.timezone,
		TimezoneSource: opts.timezoneFrom,
		Now:            state.Now.UTC(),
		Current:        view(state.Current, cur),
		Comparison:     view(state.Previous, prev),
		Change:         reportChangeOf(cur.Totals, prev.Totals),
		Groups:         reportGroupsOf(cur.Groups, prev.Groups),
	}
	switch opts.format {
	case formatJSON:
		payload := any(report)
		if opts.noCost {
			if payload, err = stripCost(report); err != nil {
				fmt.Fprintln(errout, err)
				return 1
			}
		}
		if err = json.NewEncoder(out).Encode(payload); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
	case formatCSV:
		w := csv.NewWriter(out)
		// With --by the header gains group and key, and the two period rows
		// leave them empty. A group row carries the period bounds and the
		// token/request/cost columns; the cache columns stay empty because the
		// envelope ranks usage, not cache, per group.
		grouped := len(report.Groups) > 0
		header := []string{"period", "start", "end", "requests", "sessions", "tokens", "input", "output", "cache_read", "cache_write", "reasoning"}
		if !opts.noCost {
			header = append(header, "cost_usd")
		}
		header = append(header, "cache_hit", "cache_expected", "cache_read_expected", "cache_miss", "cache_miss_usd")
		if grouped {
			header = append(header, "group", "key")
		}
		if err = w.Write(header); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		for _, v := range []reportView{report.Current, report.Comparison} {
			row := []string{v.Kind, v.Start.Format(time.RFC3339), v.End.Format(time.RFC3339), num(v.Totals.Requests), num(v.Totals.Sessions), num(v.Totals.Tokens.Total()), num(v.Totals.Tokens.Input), num(v.Totals.Tokens.Output), num(v.Totals.Tokens.CacheRead), num(v.Totals.Tokens.CacheWrite), num(v.Totals.Tokens.Reasoning)}
			if !opts.noCost {
				row = append(row, cost(v.Totals.CostUSD))
			}
			row = append(row, strconv.FormatFloat(v.Totals.CacheHit, 'f', 6, 64), num(v.Cache.Expected), num(v.Cache.Read), num(v.Cache.Miss), cost(v.Cache.USD))
			if grouped {
				row = append(row, "", "")
			}
			if err = w.Write(row); err != nil {
				fmt.Fprintln(errout, err)
				return 1
			}
		}
		for _, g := range report.Groups {
			for _, side := range []struct {
				period string
				view   reportView
				rows   []reportGroupRow
			}{
				{"current", report.Current, g.Current},
				{"comparison", report.Comparison, g.Comparison},
			} {
				for _, gr := range side.rows {
					row := []string{side.period, side.view.Start.Format(time.RFC3339), side.view.End.Format(time.RFC3339), num(gr.Requests), num(gr.Sessions), num(gr.Tokens), "", "", "", "", ""}
					if !opts.noCost {
						row = append(row, cost(gr.CostUSD))
					}
					row = append(row, "", "", "", "", "", g.Dim, gr.Key)
					if err = w.Write(row); err != nil {
						fmt.Fprintln(errout, err)
						return 1
					}
				}
			}
		}
		w.Flush()
		if err = w.Error(); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
	default:
		if err = writeReportText(out, report, opts.noCost); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
	}
	return 0
}

func writeReportText(out io.Writer, r reportReport, noCost bool) error {
	fmt.Fprintf(out, "REPORT %s  %s  zone %s (%s)\n", r.Current.Kind, r.Current.ID, r.Timezone, r.TimezoneSource)
	fmt.Fprintf(out, "current    %s → %s\n", r.Current.Start.Format(time.RFC3339), r.Current.End.Format(time.RFC3339))
	fmt.Fprintf(out, "comparison %s → %s (previous, same elapsed span)\n", r.Comparison.Start.Format(time.RFC3339), r.Comparison.End.Format(time.RFC3339))
	w := newTable(out)
	fmt.Fprintln(w, "METRIC\tCURRENT\tCOMPARISON\tCHANGE")
	row := func(metric, cur, cmp, change string) { fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", metric, cur, cmp, change) }
	row("requests", num(r.Current.Totals.Requests), num(r.Comparison.Totals.Requests), pctText(r.Change.RequestsPct))
	row("sessions", num(r.Current.Totals.Sessions), num(r.Comparison.Totals.Sessions), "")
	row("tokens", num(r.Current.Totals.Tokens.Total()), num(r.Comparison.Totals.Tokens.Total()), pctText(r.Change.TokensPct))
	if !noCost {
		row("costUsd", cost(r.Current.Totals.CostUSD), cost(r.Comparison.Totals.CostUSD), pctText(r.Change.CostPct))
	}
	row("cacheHit", fmt.Sprintf("%.1f%%", r.Current.Totals.CacheHit*100), fmt.Sprintf("%.1f%%", r.Comparison.Totals.CacheHit*100), ppText(r.Change.CacheHitPP))
	row("cacheRead", num(r.Current.Cache.Read), num(r.Comparison.Cache.Read), "")
	row("cacheExpected", num(r.Current.Cache.Expected), num(r.Comparison.Cache.Expected), "")
	row("cacheMiss", num(r.Current.Cache.Miss), num(r.Comparison.Cache.Miss), "")
	if !noCost {
		row("cacheMissUsd", cost(r.Current.Cache.USD), cost(r.Comparison.Cache.USD), "")
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, g := range r.Groups {
		if err := writeReportGroup(out, g, noCost); err != nil {
			return err
		}
	}
	return nil
}

// reportGroupsOf pairs each requested dimension with the previous period's
// ranking of the same dimension. Both sides come from the query package, so the
// key is the canonical project or the session id and is never re-derived here.
func reportGroupsOf(cur, prev []query.Group) []reportGroup {
	if len(cur) == 0 && len(prev) == 0 {
		return nil
	}
	prevByDim := map[query.GroupDim]query.Group{}
	for _, g := range prev {
		prevByDim[g.Dim] = g
	}
	out := make([]reportGroup, 0, len(cur))
	for _, g := range cur {
		out = append(out, reportGroup{
			Dim:        string(g.Dim),
			Note:       reportGroupNote(g.Dim),
			Total:      g.Total,
			Current:    reportGroupRows(g),
			Comparison: reportGroupRows(prevByDim[g.Dim]),
		})
	}
	return out
}

func reportGroupNote(dim query.GroupDim) string {
	switch dim {
	case query.GroupByProject:
		return "canonical repository column: the store resolved each recorded path to its repository root at write time, so linked worktrees and other paths of one repository are already folded; the raw path stays as provenance"
	case query.GroupBySession:
		return fmt.Sprintf("busiest %d sessions per period, tokens desc; a parent row includes its children", reportGroupSessions)
	case query.GroupByBranch:
		return "repository x branch as recorded by the log for each request, never the working tree's current HEAD; a branch name alone is not a group because two repositories can share one, and " + reportUnknownBranch + " is usage whose log stated no branch"
	default:
		return ""
	}
}

func reportGroupRows(g query.Group) []reportGroupRow {
	rows := make([]reportGroupRow, 0, len(g.Buckets)+len(g.Sessions))
	branch := g.Dim == query.GroupByBranch
	for _, b := range g.Buckets {
		row := reportGroupRow{Key: b.Key, Label: b.Label, Tokens: b.Tokens.Total(), CostUSD: b.CostUSD, Requests: b.Requests, Sessions: b.Sessions}
		if branch {
			// The bucket key is the repository×branch pair; splitting it back out
			// keeps the repository's provenance and the recorded branch in their
			// own columns instead of showing one opaque joined string.
			row.Project, row.Branch = query.SplitBranchKey(b.Key)
			if row.Branch == "" {
				row.Branch = reportUnknownBranch
			}
			row.Label = row.Branch
			row.Key = row.Project + "@" + row.Branch
		}
		rows = append(rows, row)
	}
	for _, s := range g.Sessions {
		rows = append(rows, reportGroupRow{Key: s.SessionID, Label: s.Title, Project: s.Project, Tokens: s.Tokens.Total(), CostUSD: s.CostUSD, Requests: s.Requests})
	}
	return rows
}

func writeReportGroup(out io.Writer, g reportGroup, noCost bool) error {
	fmt.Fprintf(out, "\nby %s  %s\n", g.Dim, g.Note)
	if g.Total > len(g.Current) {
		fmt.Fprintf(out, "showing %d of %d rows (current period)\n", len(g.Current), g.Total)
	}
	prev := map[string]reportGroupRow{}
	for _, row := range g.Comparison {
		prev[row.Key] = row
	}
	w := newTable(out)
	switch g.Dim {
	case string(query.GroupBySession):
		if noCost {
			fmt.Fprintln(w, "SESSION\tTITLE\tPROJECT\tCURRENT_TOKENS\tCURRENT_REQ\tCOMPARISON_TOKENS\tCOMPARISON_REQ\tCHANGE")
		} else {
			fmt.Fprintln(w, "SESSION\tTITLE\tPROJECT\tCURRENT_TOKENS\tCURRENT_USD\tCURRENT_REQ\tCOMPARISON_TOKENS\tCOMPARISON_USD\tCOMPARISON_REQ\tCHANGE")
		}
	case string(query.GroupByBranch):
		if noCost {
			fmt.Fprintln(w, "REPOSITORY\tBRANCH\tCURRENT_TOKENS\tCURRENT_REQ\tCURRENT_SESSIONS\tCOMPARISON_TOKENS\tCOMPARISON_REQ\tCHANGE")
		} else {
			fmt.Fprintln(w, "REPOSITORY\tBRANCH\tCURRENT_TOKENS\tCURRENT_USD\tCURRENT_REQ\tCURRENT_SESSIONS\tCOMPARISON_TOKENS\tCOMPARISON_USD\tCOMPARISON_REQ\tCHANGE")
		}
	default:
		if noCost {
			fmt.Fprintln(w, "PROJECT\tCURRENT_TOKENS\tCURRENT_REQ\tCURRENT_SESSIONS\tCOMPARISON_TOKENS\tCOMPARISON_REQ\tCHANGE")
		} else {
			fmt.Fprintln(w, "PROJECT\tCURRENT_TOKENS\tCURRENT_USD\tCURRENT_REQ\tCURRENT_SESSIONS\tCOMPARISON_TOKENS\tCOMPARISON_USD\tCOMPARISON_REQ\tCHANGE")
		}
	}
	seen := map[string]bool{}
	for _, row := range g.Current {
		seen[row.Key] = true
		writeReportGroupRow(w, g.Dim, row, prev[row.Key], noCost)
	}
	for _, row := range g.Comparison {
		if seen[row.Key] {
			continue
		}
		// A row that only exists in the comparison period is shown with zero
		// current values, so a project that stopped being used stays visible.
		writeReportGroupRow(w, g.Dim, reportGroupRow{Key: row.Key, Label: row.Label, Project: row.Project, Branch: row.Branch}, row, noCost)
	}
	return w.Flush()
}

func writeReportGroupRow(w io.Writer, dim string, cur, prev reportGroupRow, noCost bool) {
	change := ""
	if prev.Tokens > 0 {
		change = pctText(pct(float64(cur.Tokens), float64(prev.Tokens)))
	}
	label, project, branch := cur.Label, cur.Project, cur.Branch
	if label == "" {
		label = prev.Label
	}
	if project == "" {
		project = prev.Project
	}
	if branch == "" {
		branch = prev.Branch
	}
	if dim == string(query.GroupBySession) {
		key := cur.Key
		if key == "" {
			key = prev.Key
		}
		if noCost {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%s\n", key, label, project, cur.Tokens, cur.Requests, prev.Tokens, prev.Requests, change)
			return
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%d\t%d\t%s\t%d\t%s\n", key, label, project, cur.Tokens, cost(cur.CostUSD), cur.Requests, prev.Tokens, cost(prev.CostUSD), prev.Requests, change)
		return
	}
	if dim == string(query.GroupByBranch) {
		// An empty branch is a real state, not a missing column: name it so the
		// row is readable and never silently looks like a repository-wide total.
		if branch == "" {
			branch = reportUnknownBranch
		}
		if noCost {
			fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%s\n", project, branch, cur.Tokens, cur.Requests, cur.Sessions, prev.Tokens, prev.Requests, change)
			return
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%d\t%d\t%d\t%s\t%d\t%s\n", project, branch, cur.Tokens, cost(cur.CostUSD), cur.Requests, cur.Sessions, prev.Tokens, cost(prev.CostUSD), prev.Requests, change)
		return
	}
	key := cur.Key
	if key == "" {
		key = prev.Key
	}
	if noCost {
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%d\t%s\n", key, cur.Tokens, cur.Requests, cur.Sessions, prev.Tokens, prev.Requests, change)
		return
	}
	fmt.Fprintf(w, "%s\t%d\t%s\t%d\t%d\t%d\t%s\t%d\t%s\n", key, cur.Tokens, cost(cur.CostUSD), cur.Requests, cur.Sessions, prev.Tokens, cost(prev.CostUSD), prev.Requests, change)
}

// reportChangeOf compares two periods. A previous value of zero has no
// percentage; it is reported as null instead of +inf or a fake +100%.
func reportChangeOf(cur, prev query.Totals) reportChange {
	var out reportChange
	if prev.CostUSD > 0 {
		out.CostPct = pct(cur.CostUSD, prev.CostUSD)
	}
	if prev.Requests > 0 {
		out.RequestsPct = pct(float64(cur.Requests), float64(prev.Requests))
	}
	if prev.Tokens.Total() > 0 {
		out.TokensPct = pct(float64(cur.Tokens.Total()), float64(prev.Tokens.Total()))
	}
	if prev.Tokens.Input+prev.Tokens.CacheRead+prev.Tokens.CacheWrite > 0 || cur.Tokens.Input+cur.Tokens.CacheRead+cur.Tokens.CacheWrite > 0 {
		pp := (cur.CacheHit - prev.CacheHit) * 100
		out.CacheHitPP = &pp
	}
	return out
}

func pct(cur, prev float64) *float64 {
	v := (cur - prev) / prev * 100
	return &v
}

func pctText(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%+.1f%%", *v)
}

func ppText(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%+.1fpp", *v)
}
