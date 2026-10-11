package reconcile

import (
	"context"
	"encoding/json"
	"time"

	"github.com/zzstar101/mytoken/internal/pricing"
	"github.com/zzstar101/mytoken/internal/store"
)

// priceRulesSetting is the settings key internal/query stores price rules
// under. Reconcile reads that row directly, the way it already reads
// "model-aliases" and "pricing_fingerprint".
const priceRulesSetting = "price-rules"

// storedRule is the part of the price-rule contract the merge needs to keep a
// rule alive: its selector, its source tag, and the rates that say whether it
// changed. internal/query owns the full contract; only these fields are read.
type storedRule struct {
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	Multiplier float64   `json:"multiplier"`
	Input      *float64  `json:"input"`
	Output     *float64  `json:"output"`
	CacheRead  *float64  `json:"cacheRead"`
	CacheWrite *float64  `json:"cacheWrite"`
	Source     string    `json:"source"`
	From       time.Time `json:"from"`
}

// keepRelayRules lists the rules already stored for one key's source tag.
//
// A sync whose price list failed to load still updates prices from the bills
// it did fetch. Without this, the merge would see the models the missing list
// would have covered as models the key no longer prices, and drop their rules
// until the next successful fetch — repricing that history as unpriced. Passing
// the stored rules back as part of the fresh set keeps them: the merge finds
// each one unchanged and leaves it alone.
func keepRelayRules(ctx context.Context, st *store.Store, source string) []pricing.Rule {
	raw, err := st.Setting(ctx, priceRulesSetting)
	if err != nil || raw == "" {
		return nil
	}
	var rows []storedRule
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return nil
	}
	out := make([]pricing.Rule, 0, len(rows))
	for _, r := range rows {
		if r.Source != source {
			continue
		}
		out = append(out, pricing.Rule{
			Provider:   r.Provider,
			Model:      r.Model,
			Multiplier: r.Multiplier,
			Input:      r.Input,
			Output:     r.Output,
			CacheRead:  r.CacheRead,
			CacheWrite: r.CacheWrite,
			Source:     r.Source,
			From:       r.From,
		})
	}
	return out
}
