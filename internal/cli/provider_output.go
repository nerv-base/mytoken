package cli

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/zzstar101/mytoken/internal/query"
)

// providerListReport is the --format json payload of `provider list` and of
// `stats --by endpoint`. Groups are the query package's own identity rows, so
// --json stays exactly the query contract: stable key, display label, whether
// the endpoint was recognized, and the harness/provider members behind it.
type providerListReport struct {
	By     string                `json:"by"`
	Groups []query.ProviderGroup `json:"groups"`
}

// providerCSVHeader is the identity view's CSV shape. It mirrors the bucket
// header, inserting identified after label and appending members last; the
// legacy bucket/session/day headers are untouched.
var providerCSVHeader = []string{"key", "label", "identified", "requests", "sessions", "tokens", "input", "output", "cache_read", "cache_write", "reasoning", "cost_usd", "unpriced", "members"}

// providerGroupCSVRow renders one identity group. members joins the
// harness/provider pairs with ";" so one cell stays one CSV field.
func providerGroupCSVRow(g query.ProviderGroup, noCost bool) []string {
	row := []string{g.Key, g.Label, strconv.FormatBool(g.Identified), num(g.Requests), num(g.Sessions)}
	row = append(row, tokensCSV(g.Tokens)...)
	if !noCost {
		row = append(row, cost(g.CostUSD))
	}
	return append(row, num(g.Unpriced), providerMembersText(g.Members, ";"))
}

// providerMembersText renders the members behind one endpoint. Legacy rows never
// contain a comma or semicolon in a harness or provider name, so no quoting is
// needed; provider names come from the pricing catalog, not from a raw log.
func providerMembersText(members []query.ProviderMember, sep string) string {
	parts := make([]string, 0, len(members))
	for _, m := range members {
		parts = append(parts, fmt.Sprintf("%s/%s", m.Harness, m.Provider))
	}
	return strings.Join(parts, sep)
}

// writeProviderGroupsTable prints the identity table. It is shared by
// `provider list` and `stats --by endpoint` so both views stay identical.
func writeProviderGroupsTable(out io.Writer, groups []query.ProviderGroup, noCost bool) error {
	w := newTable(out)
	if noCost {
		fmt.Fprintln(w, "ID\tLABEL\tIDENTIFIED\tREQUESTS\tTOKENS\tUNPRICED\tMEMBERS")
	} else {
		fmt.Fprintln(w, "ID\tLABEL\tIDENTIFIED\tREQUESTS\tTOKENS\tCOST USD\tUNPRICED\tMEMBERS")
	}
	for _, g := range groups {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d", g.Key, g.Label, strconv.FormatBool(g.Identified), g.Requests, g.Tokens.Total())
		if !noCost {
			fmt.Fprintf(w, "\t%.4f", g.CostUSD)
		}
		fmt.Fprintf(w, "\t%d\t%s\n", g.Unpriced, providerMembersText(g.Members, ", "))
	}
	return w.Flush()
}

// writeProviderGroupsCSV prints the identity view as CSV rows.
func writeProviderGroupsCSV(out io.Writer, groups []query.ProviderGroup, noCost bool) error {
	recs := make([][]string, 0, len(groups))
	for _, g := range groups {
		recs = append(recs, providerGroupCSVRow(g, noCost))
	}
	return writeCSV(out, csvHeaderFor(providerCSVHeader, noCost), recs)
}

// sortProviderGroups orders identities by stable key. ProviderGroups already
// sorts by traffic, but the list command is a directory: a stable, greppable
// key order is friendlier than a decaying traffic order.
func sortProviderGroups(groups []query.ProviderGroup) {
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].Key < groups[j].Key })
}
