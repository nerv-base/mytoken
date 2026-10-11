package reconcile

import (
	"context"
	"encoding/json"
	"time"

	"github.com/zzstar101/mytoken/internal/relay"
	"github.com/zzstar101/mytoken/internal/store"
)

// modelWindowDays is how far back local usage counts as "this model is used
// here". A gateway lists every model it can serve — hundreds on a busy site —
// while a user calls a handful, so the window only has to be long enough to
// cover a model that is used now and then. A model last used longer ago is not
// worth a price rule.
const modelWindowDays = 90

// usedModels builds the filter that decides which models of a site's snapshot
// deserve price rules: the models each (harness, provider) scope really used
// locally, plus the models this key's stored bills charged.
//
// Names are matched the way pricing matches a rule key: an alias is resolved
// first (provider-scoped, then any provider), then the name is normalized. A
// snapshot model is recorded both as the site spells it and as the alias maps
// it, because a rule is only reachable under the name the events normalize to.
//
// A filter with no evidence at all means "nothing is used here", which cannot
// be told apart from "the usage is not visible" — a fresh install, or a
// database whose daily rollup has not caught up. It then allows every model, so
// the worst case is the behaviour before filtering.
func usedModels(ctx context.Context, st *store.Store, origin, keyID string, since time.Time) (*relay.ModelFilter, error) {
	aliases := modelAliases(ctx, st)
	scopes, err := st.RelayModelScopes(ctx, since)
	if err != nil {
		return nil, err
	}
	billed, err := st.RelayBillModels(ctx, origin, keyID)
	if err != nil {
		return nil, err
	}
	f := relay.NewModelFilter()
	for _, u := range scopes {
		f.Add(u.Harness, u.Provider, u.Model)
		if to := aliasTarget(u.Model, u.Provider, aliases); to != u.Model {
			f.Add(u.Harness, u.Provider, to)
		}
	}
	for _, m := range billed {
		f.AddModel(m)
		if to := aliasTarget(m, "", aliases); to != m {
			f.AddModel(to)
		}
	}
	if f.Empty() {
		f.AllowAll()
	}
	return f, nil
}

// modelAliases reads the alias table from the settings row query maintains.
func modelAliases(ctx context.Context, st *store.Store) []alias {
	raw, err := st.Setting(ctx, "model-aliases")
	if err != nil || raw == "" {
		return nil
	}
	var out []alias
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// aliasTarget resolves a logged model name the way pricing's canonical() does:
// the provider's own alias first, then one that applies to any provider.
func aliasTarget(name, provider string, aliases []alias) string {
	for _, a := range aliases {
		if a.From == name && a.Provider == provider {
			return a.To
		}
	}
	for _, a := range aliases {
		if a.From == name && a.Provider == "" {
			return a.To
		}
	}
	return name
}
