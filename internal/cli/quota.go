package cli

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/quota"
	"github.com/zzstar101/mytoken/internal/snapshot"
)

const quotaUsage = `Usage: mytoken quota [list] [options]
       mytoken quota status [INSTANCE] [options]
       mytoken quota plan [options]
  --instance-id ID  stored instance id (repeatable; selects exactly this one)
  --json            JSON output  --format table|json|csv
  --db PATH         read another index (diagnostics only)

Offline. The persisted Codex quota observations are read from the index
(settings keys quota.codex.<instance>) through store.OpenReadOnly: no
credential, no network, no scan and no write. The stored keys are enumerated
with readonly SQL, never from a scanner, so the CLI works while no scanner is
running.
Instances are never combined: two instances are two accounts with their own
credit units. An instance with nothing stored is reported unavailable, and
--instance-id never hides it.
A window or reset is shown only when the stored observation carried one. When the
stored envelope also carries a retained pace forecast, its per-window rates are
shown with the instant that forecast's own observation was taken (as-of), never
as a value at the query instant. Without one the forecast stays unavailable with
a reason: a pace needs retained comparable observations and is never derived here
from token counts.

`

// quotaReport is the JSON shape of `quota list` and `quota status`.
type quotaReport struct {
	GeneratedAt time.Time             `json:"generatedAt"`
	Status      snapshot.Status       `json:"status"`
	Instances   []snapshot.CodexQuota `json:"instances"`
}

func runQuota(ctx context.Context, args []string, out, errout io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return runQuotaList(ctx, args, out, errout)
	}
	switch args[0] {
	case "plan", "plans":
		return runQuotaPlan(ctx, args[1:], out, errout)
	case "list":
		return runQuotaList(ctx, args[1:], out, errout)
	case "status":
		return runQuotaStatus(ctx, args[1:], out, errout)
	case "help", "-h", "--help":
		fmt.Fprint(out, quotaUsage)
		return 0
	default:
		fmt.Fprintln(errout, "unknown quota command:", args[0])
		fmt.Fprint(errout, quotaUsage)
		return 2
	}
}

// runQuotaPlan prints the plan-quota snapshots the background service stored
// under quota.plan.*, including providers that only exist over the network.
func runQuotaPlan(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("quota plan", flag.ContinueOnError)
	fs.SetOutput(errout)
	var qf quotaFlags
	addQuotaFlags(fs, &qf)
	if e := fs.Parse(args); e != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprint(errout, quotaUsage)
		return 2
	}
	snap := snapshot.Build(ctx, snapshot.Options{DBPath: qf.dbPath, Now: time.Now()})
	if qf.js || qf.format == "json" {
		body, _ := json.MarshalIndent(snap.Data.Plans, "", "  ")
		fmt.Fprintln(out, string(body))
		return 0
	}
	if len(snap.Data.Plans) == 0 {
		fmt.Fprintln(out, "no plan quota stored; open the app once or run `mytoken scan`")
		return 0
	}
	t := newTable(out)
	fmt.Fprintln(t, "PROVIDER\tPLAN\tSOURCE\tFETCHED\tWINDOWS")
	for _, p := range snap.Data.Plans {
		windows := ""
		for i, w := range p.Windows {
			if i > 0 {
				windows += "  "
			}
			windows += fmt.Sprintf("%s %.1f%%", w.Name, w.UsedPercent)
			if !w.ResetsAt.IsZero() {
				windows += " resets " + w.ResetsAt.Local().Format("15:04")
			}
		}
		if windows == "" {
			windows = "-"
		}
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\n", p.Provider, p.Plan, p.Source, p.FetchedAt.Local().Format("2006-01-02 15:04"), windows)
	}
	t.Flush()
	return 0
}

// quotaFlags are the flags both subcommands share.
type quotaFlags struct {
	js          bool
	format      string
	dbPath      string
	instanceIDs stringList
}

func addQuotaFlags(fs *flag.FlagSet, qf *quotaFlags) {
	fs.BoolVar(&qf.js, "json", false, "JSON output")
	fs.StringVar(&qf.format, "format", "", "table|json|csv")
	fs.StringVar(&qf.dbPath, "db", "", "index path override (diagnostics only)")
	fs.Var(&qf.instanceIDs, "instance-id", "stored instance id (repeatable)")
}

func runQuotaList(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("quota list", flag.ContinueOnError)
	fs.SetOutput(errout)
	var qf quotaFlags
	addQuotaFlags(fs, &qf)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprint(errout, quotaUsage)
		return 2
	}
	return quotaRead(ctx, qf, false, out, errout)
}

func runQuotaStatus(ctx context.Context, args []string, out, errout io.Writer) int {
	flags, positional, err := splitArgs(args, map[string]bool{"json": true})
	if err != nil {
		fmt.Fprintln(errout, err)
		return 2
	}
	fs := flag.NewFlagSet("quota status", flag.ContinueOnError)
	fs.SetOutput(errout)
	var qf quotaFlags
	addQuotaFlags(fs, &qf)
	if err = fs.Parse(flags); err != nil {
		return 2
	}
	if len(positional) > 1 {
		fmt.Fprint(errout, quotaUsage)
		return 2
	}
	if len(positional) == 1 {
		// A positional instance is just the first --instance-id, so both spell
		// the same selection and neither silently overrides the other.
		qf.instanceIDs = append(stringList{positional[0]}, qf.instanceIDs...)
	}
	return quotaRead(ctx, qf, true, out, errout)
}

func quotaRead(ctx context.Context, qf quotaFlags, verbose bool, out, errout io.Writer) int {
	of, err := resolveFormat(qf.format, qf.js, qf.format != "")
	if err != nil {
		fmt.Fprintln(errout, err)
		return 2
	}
	entries, status, err := snapshot.ReadCodexQuota(ctx, qf.dbPath, qf.instanceIDs.values())
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	report := quotaReport{GeneratedAt: time.Now().UTC(), Status: status, Instances: entries}
	switch of {
	case formatJSON:
		err = json.NewEncoder(out).Encode(report)
	case formatCSV:
		err = writeQuotaCSV(out, entries)
	default:
		err = writeQuotaText(out, report, verbose)
	}
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	return 0
}

func writeQuotaText(out io.Writer, r quotaReport, verbose bool) error {
	fmt.Fprintf(out, "QUOTA  generated %s  %s\n", r.GeneratedAt.Format(time.RFC3339), statusText(r.Status))
	if len(r.Instances) == 0 {
		fmt.Fprintln(out, "no stored Codex quota observation")
		fmt.Fprintln(out, "instances are never combined: two instances are two accounts with their own credit units")
		return nil
	}
	if verbose {
		return writeQuotaStatusText(out, r.Instances)
	}
	w := newTable(out)
	fmt.Fprintln(w, "INSTANCE\tSTATUS\tSOURCE\tOBSERVED\tPRIMARY\tSECONDARY\tFORECAST")
	for _, q := range r.Instances {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			q.Instance, quotaStatusText(q), sourceText(q.Source), timeText(q.ObservedAt),
			quotaWindowText(q.Primary), quotaWindowText(q.Secondary), quotaForecastCell(q))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(out, "windows, resets and pace are shown only when the stored observation supports them; instances are never combined")
	return nil
}

func writeQuotaStatusText(out io.Writer, entries []snapshot.CodexQuota) error {
	for i, q := range entries {
		if i > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintf(out, "instance   %s\n", q.Instance)
		fmt.Fprintf(out, "harness    %s\n", q.Harness)
		fmt.Fprintf(out, "key        %s\n", q.Key)
		fmt.Fprintf(out, "status     %s\n", statusText(q.Status))
		fmt.Fprintf(out, "source     %s\n", sourceText(q.Source))
		fmt.Fprintf(out, "observed   %s\n", timeText(q.ObservedAt))
		fmt.Fprintf(out, "ingested   %s\n", timeText(q.IngestedAt))
		if q.ReportStatus != "" {
			line := fmt.Sprintf("report     %s", q.ReportStatus)
			if q.ReportReason != "" {
				line += " (" + q.ReportReason + ")"
			}
			fmt.Fprintln(out, line)
		}
		if q.LimitID != "" || q.LimitName != "" {
			fmt.Fprintf(out, "limit      %s %s\n", q.LimitID, q.LimitName)
		}
		if len(q.OtherLimitIDs) > 0 {
			fmt.Fprintf(out, "otherIds   %s\n", strings.Join(q.OtherLimitIDs, ", "))
		}
		if q.PlanType != nil {
			fmt.Fprintf(out, "plan       %s\n", *q.PlanType)
		}
		if q.Credits != nil {
			fmt.Fprintf(out, "credits    %s\n", quotaCreditsText(q.Credits))
		}
		writeQuotaWindowLine(out, "primary", q.Primary)
		writeQuotaWindowLine(out, "secondary", q.Secondary)
		for _, is := range q.Issues {
			fmt.Fprintf(out, "issue      %s %s count=%d line=%d\n", is.Field, is.Reason, is.Count, is.Line)
		}
		line := "forecast   " + quotaForecastText(q.Forecast)
		if q.Forecast.Detail != "" {
			line += " - " + q.Forecast.Detail
		}
		fmt.Fprintln(out, line)
		if pace := quotaPaceText(q.Pace); pace != "" {
			fmt.Fprintf(out, "pace       %s\n", pace)
		}
	}
	return nil
}

// quotaForecastCell renders the FORECAST column: the verdict, plus the stored
// pace rates when the envelope carried a forecast. Nothing is appended when the
// envelope carried none.
func quotaForecastCell(q snapshot.CodexQuota) string {
	cell := quotaForecastText(q.Forecast)
	if pace := quotaPaceText(q.Pace); pace != "" {
		cell += "; " + pace
	}
	return cell
}

// quotaPaceText renders the retained pace forecast compactly, one part per
// window. It returns "" when the envelope carried no forecast, so a value
// stored before observation retention existed shows nothing rather than a
// number derived at read time. The instant the forecast's own observation was
// taken is printed as as-of: a stored pace describes that instant, not the
// instant of the query.
func quotaPaceText(f *quota.Forecast) string {
	if f == nil {
		return ""
	}
	parts := make([]string, 0, len(f.Windows))
	for _, w := range f.Windows {
		if w.Available {
			parts = append(parts, fmt.Sprintf("%s=%.2f%%/h (%.2f%%/day)", w.Window, w.Pace.PercentPerHour, w.Pace.PercentPerDay))
			continue
		}
		if w.Reason != "" {
			parts = append(parts, fmt.Sprintf("%s=unavailable (%s)", w.Window, w.Reason))
		}
	}
	if len(parts) == 0 {
		if !f.Available && f.Reason != "" {
			return "unavailable (" + string(f.Reason) + ")"
		}
		return ""
	}
	out := strings.Join(parts, " ")
	if f.ObservedAt != nil {
		out += " as-of " + f.ObservedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func writeQuotaWindowLine(out io.Writer, name string, w *snapshot.QuotaWindow) {
	if w == nil {
		fmt.Fprintf(out, "%-10s not_reported\n", name)
		return
	}
	line := fmt.Sprintf("%-10s %s", name, w.Status)
	if w.Reason != "" {
		line += " (" + w.Reason + ")"
	}
	if w.UsedPercent != nil {
		line += fmt.Sprintf(" used=%.1f%%", *w.UsedPercent)
	}
	if w.RemainingPercent != nil {
		line += fmt.Sprintf(" remaining=%.1f%%", *w.RemainingPercent)
		if w.RemainingDerived {
			line += "(derived)"
		}
	}
	if w.WindowMinutes != nil {
		line += fmt.Sprintf(" window=%dm", *w.WindowMinutes)
	}
	if w.ResetsAt != nil {
		line += " resetsAt=" + w.ResetsAt.Format(time.RFC3339)
	}
	if w.ResetsInSeconds != nil {
		line += fmt.Sprintf(" resetsIn=%ds", *w.ResetsInSeconds)
	}
	fmt.Fprintln(out, line)
}

func quotaStatusText(q snapshot.CodexQuota) string {
	if q.Available {
		return "available"
	}
	return statusText(q.Status)
}

func sourceText(src string) string {
	if src == "" {
		return "not_recorded"
	}
	return src
}

func quotaCreditsText(c *snapshot.QuotaCredits) string {
	if c == nil {
		return "not_reported"
	}
	parts := []string{}
	if c.HasCredits != nil {
		parts = append(parts, fmt.Sprintf("hasCredits=%t", *c.HasCredits))
	}
	if c.Unlimited != nil {
		parts = append(parts, fmt.Sprintf("unlimited=%t", *c.Unlimited))
	}
	if c.Balance != nil {
		parts = append(parts, "balance="+strconv.Quote(*c.Balance))
	}
	if len(parts) == 0 {
		return "reported_empty"
	}
	return strings.Join(parts, " ")
}

func writeQuotaCSV(out io.Writer, entries []snapshot.CodexQuota) error {
	w := csv.NewWriter(out)
	header := []string{
		"instance", "available", "reason", "source", "observed_at", "ingested_at",
		"report_status", "report_reason", "limit_id", "limit_name", "plan_type",
		"credits_has", "credits_unlimited", "credits_balance",
		"primary_status", "primary_used_percent", "primary_remaining_percent", "primary_window_minutes", "primary_resets_at",
		"secondary_status", "secondary_used_percent", "secondary_remaining_percent", "secondary_window_minutes", "secondary_resets_at",
		"forecast_available", "forecast_reason",
	}
	if err := w.Write(header); err != nil {
		return err
	}
	for _, q := range entries {
		p := quotaWindowCSV(q.Primary)
		s := quotaWindowCSV(q.Secondary)
		row := []string{
			q.Instance, strconv.FormatBool(q.Available), q.Status.Reason, q.Source, csvTime(q.ObservedAt), csvTime(q.IngestedAt),
			q.ReportStatus, q.ReportReason, q.LimitID, q.LimitName, csvOptStr(q.PlanType),
			creditsHas(q.Credits), creditsUnlimited(q.Credits), creditsBalance(q.Credits),
			p[0], p[1], p[2], p[3], p[4],
			s[0], s[1], s[2], s[3], s[4],
			strconv.FormatBool(q.Forecast.Available), q.Forecast.Reason,
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

func creditsHas(c *snapshot.QuotaCredits) string {
	if c == nil {
		return ""
	}
	return csvOptBool(c.HasCredits)
}

func creditsUnlimited(c *snapshot.QuotaCredits) string {
	if c == nil {
		return ""
	}
	return csvOptBool(c.Unlimited)
}

func creditsBalance(c *snapshot.QuotaCredits) string {
	if c == nil {
		return ""
	}
	return csvOptStr(c.Balance)
}

// quotaWindowCSV returns status, used, remaining, window minutes and reset
// instant. A value the observation did not carry is the empty string, never 0.
func quotaWindowCSV(w *snapshot.QuotaWindow) [5]string {
	var out [5]string
	if w == nil {
		return out
	}
	out[0] = w.Status
	if w.UsedPercent != nil {
		out[1] = strconv.FormatFloat(*w.UsedPercent, 'f', -1, 64)
	}
	if w.RemainingPercent != nil {
		out[2] = strconv.FormatFloat(*w.RemainingPercent, 'f', -1, 64)
	}
	if w.WindowMinutes != nil {
		out[3] = strconv.FormatInt(*w.WindowMinutes, 10)
	}
	out[4] = csvTime(w.ResetsAt)
	return out
}

func csvTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

func csvOptStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func csvOptBool(b *bool) string {
	if b == nil {
		return ""
	}
	return strconv.FormatBool(*b)
}

func statusText(st snapshot.Status) string {
	if st.Available {
		return "ok"
	}
	if st.Reason != "" {
		return st.Reason
	}
	return "unavailable"
}

func timeText(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func quotaWindowText(w *snapshot.QuotaWindow) string {
	if w == nil || w.UsedPercent == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", *w.UsedPercent)
}

func quotaForecastText(f snapshot.QuotaForecast) string {
	if !f.Available {
		return "-"
	}
	return "available"
}
