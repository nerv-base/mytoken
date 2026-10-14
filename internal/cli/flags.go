package cli

import (
	"fmt"
	"strings"
)

// stringList is a repeatable string flag: each occurrence appends one value, so
// `--provider-id a --provider-id b` selects the union of a and b (the filter
// intersects with the other filters, never overrides them). An empty value is
// rejected rather than silently ignored, because an empty filter would read as
// "no filter" and quietly widen the result set.
type stringList []string

// String renders the accumulated values for flag's default/usage printing.
func (l *stringList) String() string {
	if l == nil {
		return ""
	}
	return strings.Join(*l, ",")
}

// Set appends one trimmed value.
func (l *stringList) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return fmt.Errorf("empty value")
	}
	*l = append(*l, v)
	return nil
}

// values returns the accumulated values as a plain slice, never nil, so callers
// can pass it straight into a query filter.
func (l stringList) values() []string {
	if len(l) == 0 {
		return nil
	}
	out := make([]string, len(l))
	copy(out, l)
	return out
}

// providerFilterFlags names the provider identity flags the user actually
// passed, for error messages; "" when neither was used.
func providerFilterFlags(cf *commonFlags) string {
	var names []string
	if len(cf.providers) > 0 {
		names = append(names, "--provider")
	}
	if len(cf.providerIDs) > 0 {
		names = append(names, "--provider-id")
	}
	return strings.Join(names, " and ")
}

// ledgerFilterFlags names every ledger filter the user actually passed, for
// commands that cannot honour one: the provider identity flags plus
// --instance-id. Commands that have no ledger to narrow do not register these
// flags at all, so this is only for commands that take some ledger flags and
// must refuse the rest instead of silently dropping them.
func ledgerFilterFlags(cf *commonFlags) string {
	names := make([]string, 0, 3)
	if len(cf.providers) > 0 {
		names = append(names, "--provider")
	}
	if len(cf.providerIDs) > 0 {
		names = append(names, "--provider-id")
	}
	if len(cf.instanceIDs) > 0 {
		names = append(names, "--instance-id")
	}
	return strings.Join(names, " and ")
}
