package query

import (
	"context"
	"time"
)

// PriceRule adjusts computed costs (docs/SPEC.md §7). Rules never touch a
// cost the log itself reported.
//
//   - Model != "": a custom per-1M-token price for that model (optionally only
//     under Provider), used instead of the catalog price.
//   - Provider != "" && Model == "": provider-wide rates, used as the last
//     custom-price fallback for that provider's models.
//
// Prices walk provider+model → model → provider → catalog, class by class, so
// a narrower rule overrides one rate while a wider one fills the rest.
// From is when the rule starts applying (zero = always). Several rules may
// share (provider, model); the one with the latest From ≤ the event's
// timestamp wins.
type PriceRule struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// Custom prices in USD per 1M tokens; nil = use the catalog's.
	Input      *float64  `json:"input,omitempty"`
	Output     *float64  `json:"output,omitempty"`
	CacheRead  *float64  `json:"cacheRead,omitempty"`
	CacheWrite *float64  `json:"cacheWrite,omitempty"`
	Source     string    `json:"source,omitempty"` // "user" (legacy rows are normalized)
	From       time.Time `json:"from,omitzero"`    // zero = always effective
}

// ModelAlias maps a model name as the harness logged it (often a user-defined
// alias in a gateway or config, e.g. "claude-opus-5-5-high", "deepseek-v41") to
// the catalog model it really is. An alias affects pricing AND display: every
// query (ByModel, Breakdown, events' Model) reports To instead of From, so the
// two merge in rankings. The raw name stays in the index; removing the alias
// restores it.
type ModelAlias struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Provider string `json:"provider,omitempty"` // "" = any provider
}

// UnpricedModel is a logged model name with no price, for the "fix it" hint.
type UnpricedModel struct {
	Model       string   `json:"model"`
	Provider    string   `json:"provider"`
	Requests    int64    `json:"requests"`
	Tokens      int64    `json:"tokens"`
	Suggestions []string `json:"suggestions"` // closest catalog ids, best first (≤5)
}

// Settings is the write side used by the Settings page and the CLI. Every
// mutation that changes costs reprices the index and notifies subscribers.
type Settings interface {
	PriceRules(ctx context.Context) ([]PriceRule, error)
	// SetPriceRules replaces all rules.
	SetPriceRules(ctx context.Context, rules []PriceRule) error
	ModelAliases(ctx context.Context) ([]ModelAlias, error)
	// SetModelAliases replaces all aliases; reprices and notifies.
	SetModelAliases(ctx context.Context, aliases []ModelAlias) error
	// UnpricedModels lists models lacking a price, most tokens first, each with
	// fuzzy-matched catalog suggestions (normalize, strip -high/-preview/-expires…
	// suffixes, version digits like v41 ≈ v4.1).
	UnpricedModels(ctx context.Context) ([]UnpricedModel, error)
	// SearchCatalog returns catalog model ids matching q (substring/fuzzy), ≤limit.
	SearchCatalog(ctx context.Context, q string, limit int) ([]string, error)
	// Providers lists every resolved provider seen in the index, for pickers.
	Providers(ctx context.Context) ([]string, error)
}

// aliasMap indexes aliases by (provider, from) and by ("", from).
func aliasMap(aliases []ModelAlias) map[[2]string]string {
	out := make(map[[2]string]string, len(aliases)*2)
	for _, a := range aliases {
		if a.From == "" || a.To == "" {
			continue
		}
		out[[2]string{a.Provider, a.From}] = a.To
	}
	return out
}
