package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/zzstar101/mytoken/internal/app"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/traekey"
)

// runKey lists, shows, sets and clears the per-harness database keys that
// encrypted sources need.
func runKey(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("key", flag.ContinueOnError)
	fs.SetOutput(errout)
	clear := fs.Bool("clear", false, "forget the stored key")
	show := fs.Bool("show", false, "print the stored key")
	capture := fs.Bool("capture", false, "read the key from the running tool")
	if err := fs.Parse(flagsFirst(args)); err != nil {
		return 2
	}
	switch {
	case fs.NArg() == 0:
		return listKeys(*show, out, errout)
	case fs.NArg() > 2:
		fmt.Fprintln(errout, "usage: mytoken key <harness> [<key>] [--capture] [--clear] [--show]")
		return 2
	}

	h := model.Harness(strings.ToLower(strings.TrimSpace(fs.Arg(0))))
	takesKey, stored := app.SourceKeyStatus(h)
	if !takesKey {
		fmt.Fprintf(errout, "%s does not take a database key\n", h)
		return 2
	}

	if *clear {
		return applyKey(ctx, h, "", out, errout)
	}
	if fs.NArg() == 2 {
		return applyKey(ctx, h, fs.Arg(1), out, errout)
	}
	// With no key given, the ordinary thing a user wants is the tool's own key,
	// which is what an empty state asks for and what a stored one can redo.
	if *capture || !stored {
		return captureKey(ctx, h, out, errout)
	}
	if *show {
		fmt.Fprintln(out, app.SourceKey(h))
		return 0
	}
	fmt.Fprintf(out, "%s: key stored (%s…)\n", h, maskKey(app.SourceKey(h)))
	return 0
}

// flagsFirst moves leading-dash arguments ahead of the positional ones, so
// "key trae --capture" reads the same as "key --capture trae": the flag package
// stops at the first positional, and a harness name is always positional.
func flagsFirst(args []string) []string {
	flags, rest := make([]string, 0, len(args)), make([]string, 0, len(args))
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			continue
		}
		rest = append(rest, arg)
	}
	return append(flags, rest...)
}

// captureKey asks the tool for its own key, stores it and scans, so the usage
// appears in the same run.
func captureKey(ctx context.Context, h model.Harness, out, errout io.Writer) int {
	a, err := app.OpenLocal()
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	defer a.Close()
	err = a.CaptureSourceKey(ctx, h, func(step string) {
		fmt.Fprintf(out, "%s: %s…\n", h, captureText(step))
	})
	if err != nil {
		fmt.Fprintf(errout, "%s: %v\n", h, err)
		return 1
	}
	fmt.Fprintf(out, "%s: key stored (%s…)\n", h, maskKey(app.SourceKey(h)))
	if err := a.Scanner.Scan(ctx); err != nil {
		fmt.Fprintf(errout, "%s: key stored, but the scan failed: %v\n", h, err)
		return 1
	}
	fmt.Fprintf(out, "%s: database read\n", h)
	return 0
}

// captureText names one step of a capture for the command line.
func captureText(step string) string {
	switch step {
	case traekey.StepCopy:
		return "copying the app"
	case traekey.StepSign:
		return "preparing the copy"
	case traekey.StepLaunch:
		return "starting the tool"
	case traekey.StepScan:
		return "reading the key from memory"
	case traekey.StepStop:
		return "closing the tool again"
	}
	return step
}

// applyKey stores or forgets a key and then rescans, so the tool's data shows
// up in the same run.
func applyKey(ctx context.Context, h model.Harness, key string, out, errout io.Writer) int {
	a, err := app.OpenLocal()
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	defer a.Close()
	if err := a.SetSourceKey(ctx, h, key); err != nil {
		fmt.Fprintf(errout, "%s: %v\n", h, err)
		return 1
	}
	// The key only pays off in a scan, so run one now: it is incremental, and a
	// user who just pasted a key expects the usage to appear.
	if err := a.Scanner.Scan(ctx); err != nil {
		fmt.Fprintf(errout, "%s: key stored, but the scan failed: %v\n", h, err)
		return 1
	}
	if key == "" {
		fmt.Fprintf(out, "%s: key removed\n", h)
		return 0
	}
	fmt.Fprintf(out, "%s: key stored and its database read\n", h)
	return 0
}

// listKeys prints every harness that takes a key, and whether one is stored.
func listKeys(show bool, out, errout io.Writer) int {
	all := harness.All()
	sort.Slice(all, func(i, j int) bool { return all[i].Harness() < all[j].Harness() })
	any := false
	for _, p := range all {
		h := p.Harness()
		if takesKey, stored := app.SourceKeyStatus(h); takesKey {
			any = true
			state := "no key stored"
			switch {
			case show && stored:
				state = app.SourceKey(h)
			case stored:
				state = "key stored (" + maskKey(app.SourceKey(h)) + "…)"
			}
			fmt.Fprintf(out, "%-16s %s\n", h, state)
		}
	}
	if !any {
		fmt.Fprintln(out, "no harness needs a database key")
	}
	_ = errout
	return 0
}

// maskKey shows enough of a key to recognise it and not enough to use it.
func maskKey(key string) string {
	key = strings.TrimSpace(key)
	if len(key) <= 8 {
		return strings.Repeat("•", len(key))
	}
	return key[:8]
}
