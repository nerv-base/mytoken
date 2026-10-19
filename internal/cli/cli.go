// Package cli serves the non-GUI commands.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
)

// Version is the release version, injected with -ldflags.
var Version = "dev"

// Run runs the command line and returns its exit status.
func Run(args []string) int { return run(args, os.Stdout, os.Stderr) }

func run(args []string, out, errout io.Writer) int {
	if len(args) == 0 {
		return runSummary(context.Background(), nil, out, errout)
	}
	ctx := context.Background()
	switch args[0] {
	case "version", "help":
		if len(args) != 1 {
			fmt.Fprint(errout, usage)
			return 2
		}
		if args[0] == "version" {
			fmt.Fprintln(out, Version)
		} else {
			fmt.Fprint(out, usage)
		}
		return 0
	case "summary", "today", "month":
		return runSummary(ctx, args[1:], out, errout)
	case "daily":
		return runDaily(ctx, args[1:], out, errout)
	case "doctor":
		return runDoctor(ctx, args[1:], out, errout)
	case "scan":
		return runScan(ctx, args[1:], out, errout)
	case "statusline":
		return runStatusline(ctx, args[1:], out, errout)
	case "quota":
		return runQuota(ctx, args[1:], out, errout)
	default:
		fmt.Fprintf(errout, "unknown command: %s\nRun \"mytoken help\" for usage.\n", args[0])
		return 2
	}
}

const usage = `Usage: mytoken [summary] [options]   today's and this month's API-equivalent cost
       mytoken daily [--days N] [--json] [--no-cost]
       mytoken quota
       mytoken scan
       mytoken statusline
       mytoken doctor [--json]
       mytoken version | help

Read-only commands (summary, daily, doctor, statusline, quota) open the index
without migrating it and never reprice. Run "mytoken scan" once after an
upgrade to migrate the index.
`
