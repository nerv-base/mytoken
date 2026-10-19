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

	"github.com/zzstar101/mytoken/internal/quota/plan"
	"github.com/zzstar101/mytoken/internal/snapshot"
)

const quotaUsage = `Usage: mytoken quota [plan] [options]
  --json            JSON output  --format table|json|csv
  --db PATH         read another index (diagnostics only)

Read-only. Plan quotas are shown from the snapshots the background service
stored after fetching them from each provider's server (settings keys
quota.plan.<provider>). This command reads no usage log and no credential, and
it never contacts a provider itself: run the app or a scan to refresh. A
provider whose server has not answered yet is simply absent.
`

func runQuota(ctx context.Context, args []string, out, errout io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return runQuotaPlan(ctx, args, out, errout)
	}
	switch args[0] {
	case "plan", "plans":
		return runQuotaPlan(ctx, args[1:], out, errout)
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
	of, err := resolveFormat(qf.format, qf.js, qf.format != "")
	if err != nil {
		fmt.Fprintln(errout, err)
		return 2
	}
	snap := snapshot.Build(ctx, snapshot.Options{DBPath: qf.dbPath, Now: time.Now()})
	plans := snap.Data.Plans
	switch of {
	case formatJSON:
		body, err := json.MarshalIndent(plans, "", "  ")
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		fmt.Fprintln(out, string(body))
	case formatCSV:
		if err := writeQuotaCSV(out, plans); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
	default:
		writeQuotaText(out, plans)
	}
	return 0
}

// quotaFlags are the flags every quota view shares.
type quotaFlags struct {
	js     bool
	format string
	dbPath string
}

func addQuotaFlags(fs *flag.FlagSet, qf *quotaFlags) {
	fs.BoolVar(&qf.js, "json", false, "JSON output")
	fs.StringVar(&qf.format, "format", "", "table|json|csv")
	fs.StringVar(&qf.dbPath, "db", "", "index path override (diagnostics only)")
}

func writeQuotaText(out io.Writer, plans []snapshot.PlanQuota) {
	if len(plans) == 0 {
		fmt.Fprintln(out, "no plan quota stored; open the app once or run `mytoken scan`")
		return
	}
	t := newTable(out)
	fmt.Fprintln(t, "PROVIDER\tPLAN\tSOURCE\tFETCHED\tWINDOWS")
	for _, p := range plans {
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\n", p.Provider, planText(p.Plan), sourceText(p.Source),
			fetchedText(p.FetchedAt), planWindowsText(p.Windows))
	}
	t.Flush()
	fmt.Fprintln(out, "source api means the value came from the provider's server; a window or reset is shown only when the server sent one")
}

// planText renders a plan id the way the GUI names it. An empty result means
// the provider stated no tier worth showing.
func planText(raw string) string {
	if label := plan.PlanLabel(raw); label != "" {
		return label
	}
	return "-"
}

func sourceText(src string) string {
	if src == "" {
		return "-"
	}
	return src
}

// fetchedText shows when the server actually answered, never the zero time.
func fetchedText(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func planWindowsText(windows []snapshot.PlanWindow) string {
	if len(windows) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(windows))
	for _, w := range windows {
		parts = append(parts, planWindowText(w))
	}
	return strings.Join(parts, "  ")
}

// planWindowText renders one window in the form that matches what it measures:
// a percentage, a remaining credit/money balance, or a used/limit allowance.
// One input is one output cell, so the windows of a provider stay aligned.
func planWindowText(w snapshot.PlanWindow) string {
	name := strings.TrimSpace(w.Name)
	if name == "" {
		name = "window"
	}
	unit := strings.ToLower(strings.TrimSpace(w.Unit))
	var part string
	switch {
	case w.Balance:
		part = balanceWindowText(name, unit, w)
	case w.Limit > 0 && unit != "" && unit != "%":
		part = fmt.Sprintf("%s %s/%s (%.1f%%)", name, formatAmount(w.Used), formatAmount(w.Limit), w.UsedPercent)
	default:
		part = fmt.Sprintf("%s %.1f%%", name, w.UsedPercent)
	}
	if !w.ExpiresAt.IsZero() {
		part += ", expires " + w.ExpiresAt.Local().Format("2006-01-02")
	}
	if !w.ResetsAt.IsZero() {
		part += " resets " + w.ResetsAt.Local().Format("2006-01-02 15:04")
	}
	return part
}

// balanceWindowText prints what is left rather than a meaningless percentage.
// A balance amount of zero is real for money (an empty wallet) but "unstated"
// for credits with no limit (an unlimited pack).
func balanceWindowText(name, unit string, w snapshot.PlanWindow) string {
	switch unit {
	case "credits", "credit":
		if w.Remaining == 0 && w.Limit == 0 && w.Used == 0 {
			return name + " -"
		}
		return name + " " + formatAmount(w.Remaining) + " credits left"
	}
	if symbol := currencySymbol(unit); symbol != "" {
		return name + " " + symbol + formatMoney(w.Remaining)
	}
	amount := formatAmount(w.Remaining)
	if unit != "" {
		amount += " " + unit
	}
	return name + " " + amount + " left"
}

// currencySymbol maps a provider's unit code to its symbol.
func currencySymbol(unit string) string {
	switch unit {
	case "cny", "rmb":
		return "¥"
	case "usd":
		return "$"
	case "eur":
		return "€"
	case "gbp":
		return "£"
	}
	return ""
}

// formatAmount renders a quota amount with up to two decimals and thousands
// separators: 4679.71 stays "4,679.71" and 10177 becomes "10,177".
func formatAmount(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return groupNumber(s)
}

// formatMoney renders an amount of money with exactly two decimals.
func formatMoney(v float64) string {
	return groupNumber(strconv.FormatFloat(v, 'f', 2, 64))
}

// groupNumber inserts thousands separators into the integer part of s.
func groupNumber(s string) string {
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	intPart, frac, hasFrac := strings.Cut(s, ".")
	var b strings.Builder
	b.WriteString(sign)
	for i := 0; i < len(intPart); i++ {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(intPart[i])
	}
	if hasFrac {
		b.WriteByte('.')
		b.WriteString(frac)
	}
	return b.String()
}

func writeQuotaCSV(out io.Writer, plans []snapshot.PlanQuota) error {
	w := csv.NewWriter(out)
	header := []string{"provider", "plan", "source", "fetched_at", "window", "used_percent",
		"remaining", "used", "limit", "unit", "balance", "resets_at", "expires_at"}
	if err := w.Write(header); err != nil {
		return err
	}
	for _, p := range plans {
		if len(p.Windows) == 0 {
			if err := w.Write(quotaCSVRow(p, nil)); err != nil {
				return err
			}
			continue
		}
		for i := range p.Windows {
			if err := w.Write(quotaCSVRow(p, &p.Windows[i])); err != nil {
				return err
			}
		}
	}
	w.Flush()
	return w.Error()
}

// quotaCSVRow is one CSV record; win is nil for a provider with no windows.
func quotaCSVRow(p snapshot.PlanQuota, win *snapshot.PlanWindow) []string {
	row := []string{p.Provider, p.Plan, p.Source, csvTime(p.FetchedAt)}
	if win == nil {
		return append(row, "", "", "", "", "", "", "", "", "")
	}
	return append(row,
		win.Name, strconv.FormatFloat(win.UsedPercent, 'f', -1, 64),
		strconv.FormatFloat(win.Remaining, 'f', -1, 64),
		strconv.FormatFloat(win.Used, 'f', -1, 64),
		strconv.FormatFloat(win.Limit, 'f', -1, 64),
		win.Unit, strconv.FormatBool(win.Balance),
		csvTime(win.ResetsAt), csvTime(win.ExpiresAt))
}

func csvTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
