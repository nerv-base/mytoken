// Package providers assembles every quota provider MyToken supports.
//
// The plan package polls providers; this package wires the concrete
// implementations to their local inputs (the settings store for Codex) so the
// application can start a service with a single call.
package providers

import (
	"github.com/zzstar101/mytoken/internal/claudequota"
	"github.com/zzstar101/mytoken/internal/quota"
	"github.com/zzstar101/mytoken/internal/quota/codex"
	"github.com/zzstar101/mytoken/internal/quota/copilot"
	"github.com/zzstar101/mytoken/internal/quota/cursor"
	"github.com/zzstar101/mytoken/internal/quota/glm"
	"github.com/zzstar101/mytoken/internal/quota/plan"
)

// Options configures the provider set.
type Options struct {
	// Store exposes the settings rows the Codex provider reads. When nil the
	// Codex provider is omitted because its quota is published by the scanner.
	Store quota.Setting
	// Instance is the Codex instance to read; empty means the default one.
	Instance string
}

// All returns the supported providers in display order.
func All(opts Options) []plan.Provider {
	instance := opts.Instance
	if instance == "" {
		instance = quota.DefaultInstance
	}
	out := make([]plan.Provider, 0, 5)
	if opts.Store != nil {
		out = append(out, codex.New(codex.Options{Store: opts.Store, Instance: instance}))
	}
	out = append(out,
		claudequota.New(claudequota.Options{}),
		cursor.New(cursor.Options{}),
		copilot.New(copilot.Options{}),
		glm.New(glm.Options{}),
	)
	return out
}
