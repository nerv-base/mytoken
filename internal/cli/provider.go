package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/paths"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/store"
)

// providerUsage is the help for the stable provider identity commands. It spells
// out the two distinct selectors so scripts do not confuse the stable endpoint
// key with the original pricing provider name.
const providerUsage = `Usage: mytoken provider list [--since DATE|DUR] [--until DATE|DUR] [--last WINDOW]
                          [--provider NAME] [--provider-id ID] [--timezone TZ]
                          [--format table|json|csv] [--no-cost] [--offline]
       mytoken provider alias set <provider-id> <label>
       mytoken provider alias clear <provider-id>

provider list groups local usage by stable endpoint identity and prints, for
each one, the stable key (the value accepted by --provider-id), the display
label, whether the endpoint was recognized, and the harness/provider members
behind it. It reads the local index only and never refreshes prices. An endpoint
that could not be normalized (for example one carrying a query string) is listed
as unidentified, keeps its per-session identity and cannot be aliased.

provider alias set/clear changes only the display label. It never changes the
stable key, attribution, cost or the pricing fingerprint. Labels are display
text and may repeat across identities, so select with --provider-id, never by
label. An empty label is rejected; use "alias clear" instead.
`

// openProviderStore opens only the local index the provider identity commands
// need. It deliberately does not go through app.OpenLocal(): app.open()
// unconditionally calls Store.EnsurePrices(prices.Fingerprint(), prices.Evaluate),
// and whenever the on-disk settings.pricing_fingerprint is stale (any DB last
// priced by an older catalog) that call reprices every non-log-priced event and
// overwrites the checkpoint. These commands are presentation-only, so they must
// not touch a single historical fact.
//
// store.Open still applies schema migrations, so this is "no scan, no network,
// no reprice" — not a byte-for-byte read-only snapshot: migrations may add
// columns/tables and run the provider identity backfill.
func openProviderStore() (*store.Store, error) {
	db, e := paths.DBPath()
	if e != nil {
		return nil, e
	}
	return store.Open(db)
}

// runProvider dispatches the provider identity commands.
func runProvider(ctx context.Context, args []string, out, errout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(errout, providerUsage)
		return 2
	}
	switch args[0] {
	case "list":
		return providerList(ctx, args[1:], out, errout)
	case "alias":
		return providerAlias(ctx, args[1:], out, errout)
	default:
		fmt.Fprintln(errout, "unknown provider subcommand:", args[0])
		fmt.Fprint(errout, providerUsage)
		return 2
	}
}

// providerList parses the list flags. Its default range is unbounded: a
// directory of endpoints is more useful complete than truncated to 7d.
func providerList(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("provider list", flag.ContinueOnError)
	fs.SetOutput(errout)
	fs.Usage = func() { fmt.Fprint(errout, providerUsage) }
	var cf commonFlags
	addCommonFlags(fs, &cf, "")
	if e := cf.parse(fs, args); e != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	opts, e := cf.resolve(time.Now())
	if e != nil {
		fmt.Fprintln(errout, e)
		return 2
	}
	return runInLocation(opts.loc, func() int { return providerListCommand(ctx, opts, out, errout) })
}

// providerListCommand renders the identity directory. It always opens the local
// index: listing identities must never trigger the price-refresh worker or the
// startup reprice, and it never reads raw endpoint rows (which may carry
// secrets), only the identity keys and labels the query layer derived from them.
func providerListCommand(ctx context.Context, opts options, out, errout io.Writer) int {
	st, e := openProviderStore()
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	defer st.Close()
	groups, e := query.NewService(st).ProviderGroups(ctx, opts.filter())
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	if groups == nil {
		groups = []query.ProviderGroup{}
	}
	sortProviderGroups(groups)
	switch opts.format {
	case formatJSON:
		var payload any = providerListReport{By: "endpoint", Groups: groups}
		if opts.noCost {
			if payload, e = stripCost(payload); e != nil {
				fmt.Fprintln(errout, e)
				return 1
			}
		}
		e = json.NewEncoder(out).Encode(payload)
	case formatCSV:
		e = writeProviderGroupsCSV(out, groups, opts.noCost)
	default:
		if _, e = fmt.Fprintf(out, "Identities: %d\n", len(groups)); e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		e = writeProviderGroupsTable(out, groups, opts.noCost)
	}
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	return 0
}

// providerAlias dispatches `provider alias set|clear`.
func providerAlias(ctx context.Context, args []string, out, errout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(errout, providerUsage)
		return 2
	}
	switch args[0] {
	case "set":
		return providerAliasSet(ctx, args[1:], out, errout)
	case "clear":
		return providerAliasClear(ctx, args[1:], out, errout)
	default:
		fmt.Fprintln(errout, "unknown provider alias subcommand:", args[0])
		fmt.Fprint(errout, providerUsage)
		return 2
	}
}

// providerAliasSet sets the display label of one recognized endpoint identity.
// The label is positional, so it may begin with a dash; a label that trims to
// empty is rejected here because the store treats empty as "clear", which would
// silently delete an alias the user meant to write.
func providerAliasSet(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("provider alias set", flag.ContinueOnError)
	fs.SetOutput(errout)
	fs.Usage = func() { fmt.Fprint(errout, providerUsage) }
	if e := fs.Parse(args); e != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	id := strings.TrimSpace(fs.Arg(0))
	label := strings.TrimSpace(fs.Arg(1))
	if id == "" {
		fmt.Fprintln(errout, "provider alias set: empty provider-id")
		return 2
	}
	if label == "" {
		fmt.Fprintln(errout, "provider alias set: empty label (use \"provider alias clear\" to remove an alias)")
		return 2
	}
	st, e := openProviderStore()
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	defer st.Close()
	if e = st.SetProviderAlias(ctx, id, label); e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	fmt.Fprintf(out, "alias set: %s -> %s\n", id, label)
	return 0
}

// providerAliasClear restores an identity's normalized endpoint label.
func providerAliasClear(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("provider alias clear", flag.ContinueOnError)
	fs.SetOutput(errout)
	fs.Usage = func() { fmt.Fprint(errout, providerUsage) }
	if e := fs.Parse(args); e != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	id := strings.TrimSpace(fs.Arg(0))
	if id == "" {
		fmt.Fprintln(errout, "provider alias clear: empty provider-id")
		return 2
	}
	st, e := openProviderStore()
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	defer st.Close()
	if e = st.SetProviderAlias(ctx, id, ""); e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	fmt.Fprintf(out, "alias cleared: %s\n", id)
	return 0
}
