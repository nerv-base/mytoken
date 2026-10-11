package relay

import (
	"github.com/zzstar101/mytoken/internal/pricing"
)

// ModelFilter is the set of models a site's price snapshot is worth turning
// into rules for.
//
// A gateway lists every model it can serve, which for a busy new-api site is
// hundreds, while a user only ever calls a handful through one harness and one
// provider name. Generating a rule per (provider, model) for all of them grows
// the settings table and the fingerprint recompute without pricing a single
// extra request, so the snapshot is filtered down to what local usage and the
// key's own stored bills prove is relevant.
//
// Evidence is scoped the way a price rule is looked up: a rule naming
// "pi/kami" only applies to events the pi harness sent to the provider "kami",
// and a rule naming a bare "kami" applies to that provider in every harness
// (see pricing.Scoped and ScopedProvider).
type ModelFilter struct {
	byScope    map[scopeKey]map[string]bool // harness+provider -> models
	byProvider map[string]map[string]bool   // bare provider -> models, any harness
	byModel    map[string]bool              // models proven by bills, any provider
	all        bool
}

type scopeKey struct{ harness, provider string }

// NewModelFilter returns a filter with no evidence, which allows nothing.
func NewModelFilter() *ModelFilter {
	return &ModelFilter{
		byScope:    map[scopeKey]map[string]bool{},
		byProvider: map[string]map[string]bool{},
		byModel:    map[string]bool{},
	}
}

// AllowAll makes the filter allow every model. It is for callers that have no
// usage evidence to offer — an injected store in a test — and must not silently
// drop the rules the unfiltered behaviour produced.
func (f *ModelFilter) AllowAll() {
	if f != nil {
		f.all = true
	}
}

// Add records that one harness really used a model through one provider. The
// name is normalized exactly as pricing normalizes a rule key, so a logged
// "Claude-Opus-4-6:latest" and a snapshot's "claude-opus-4-6" meet.
func (f *ModelFilter) Add(harness, provider, model string) {
	if f == nil || model == "" {
		return
	}
	n := pricing.Normalize(model)
	if n == "" {
		return
	}
	scope := scopeKey{harness, provider}
	if f.byScope[scope] == nil {
		f.byScope[scope] = map[string]bool{}
	}
	f.byScope[scope][n] = true
	if f.byProvider[provider] == nil {
		f.byProvider[provider] = map[string]bool{}
	}
	f.byProvider[provider][n] = true
}

// AddModel records a model proven relevant without a provider scope: a model
// the key's stored bills charged, which is what the site itself said mattered.
func (f *ModelFilter) AddModel(model string) {
	if f == nil {
		return
	}
	if n := pricing.Normalize(model); n != "" {
		f.byModel[n] = true
	}
}

// Allows reports whether a provider entry of a site — "pi/kami" or a bare
// name — is worth a rule for model.
func (f *ModelFilter) Allows(provider, model string) bool {
	if f == nil || f.all {
		return true
	}
	n := pricing.Normalize(model)
	if n == "" || f.byModel[n] {
		return true
	}
	h, name := SplitProvider(provider)
	if h == "" {
		// A bare name claims the provider in every harness.
		return f.byProvider[name][n]
	}
	return f.byScope[scopeKey{string(h), name}][n]
}

// AllowsAny reports whether any of a site's provider entries is worth a rule
// for model.
func (f *ModelFilter) AllowsAny(providers []string, model string) bool {
	if f == nil || f.all {
		return true
	}
	for _, p := range providers {
		if f.Allows(p, model) {
			return true
		}
	}
	return false
}

// Empty reports whether the filter holds no evidence at all, which is how a
// caller tells "nothing was used here" from "everything was".
func (f *ModelFilter) Empty() bool {
	if f == nil || f.all {
		return false
	}
	return len(f.byScope) == 0 && len(f.byModel) == 0
}
