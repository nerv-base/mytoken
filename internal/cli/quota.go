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
			p.FetchedAt.Local().Format("2006-01-02 15:04"), planWindowsText(p.Windows))
	}
	t.Flush()
	fmt.Fprintln(out, "source api means the value came from the provider's server; a window or reset is shown only when the server sent one")
}

func planText(plan string) string {
	if plan == "" {
		return "-"
	}
	return plan
}

func sourceText(src string) string {
	if src == "" {
		return "-"
	}
	return src
}

func planWindowsText(windows []snapshot.PlanWindow) string {
	if len(windows) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(windows))
	for _, w := range windows {
		part := fmt.Sprintf("%s %.1f%%", w.Name, w.UsedPercent)
		if !w.ResetsAt.IsZero() {
			part += " resets " + w.ResetsAt.Local().Format("2006-01-02 15:04")
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "  ")
}

func writeQuotaCSV(out io.Writer, plans []snapshot.PlanQuota) error {
	w := csv.NewWriter(out)
	if err := w.Write([]string{"provider", "plan", "source", "fetched_at", "window", "used_percent", "resets_at"}); err != nil {
		return err
	}
	for _, p := range plans {
		if len(p.Windows) == 0 {
			if err := w.Write([]string{p.Provider, p.Plan, p.Source, csvTime(p.FetchedAt), "", "", ""}); err != nil {
				return err
			}
			continue
		}
		for _, win := range p.Windows {
			reset := ""
			if !win.ResetsAt.IsZero() {
				reset = win.ResetsAt.Format(time.RFC3339)
			}
			row := []string{p.Provider, p.Plan, p.Source, csvTime(p.FetchedAt),
				win.Name, strconv.FormatFloat(win.UsedPercent, 'f', -1, 64), reset}
			if err := w.Write(row); err != nil {
				return err
			}
		}
	}
	w.Flush()
	return w.Error()
}

func csvTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
