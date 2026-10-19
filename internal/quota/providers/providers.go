// Package providers assembles every quota provider MyToken supports.
//
// The plan package polls providers; this package wires the concrete
// implementations so the application can start a service with a single call.
// Every provider reads local credentials to authenticate, and every quota
// value itself comes from the provider's server.
package providers

import (
	"github.com/zzstar101/mytoken/internal/claudequota"
	"github.com/zzstar101/mytoken/internal/quota/codex"
	"github.com/zzstar101/mytoken/internal/quota/copilot"
	"github.com/zzstar101/mytoken/internal/quota/cursor"
	"github.com/zzstar101/mytoken/internal/quota/deepseek"
	"github.com/zzstar101/mytoken/internal/quota/glm"
	"github.com/zzstar101/mytoken/internal/quota/plan"
	"github.com/zzstar101/mytoken/internal/quota/qoder"
	"github.com/zzstar101/mytoken/internal/quota/trae"
	"github.com/zzstar101/mytoken/internal/quota/workbuddy"
)

// All returns the supported providers in display order.
func All() []plan.Provider {
	return []plan.Provider{
		codex.New(codex.Options{}),
		claudequota.New(claudequota.Options{}),
		cursor.New(cursor.Options{}),
		copilot.New(copilot.Options{}),
		glm.New(glm.Options{}),
		deepseek.New(deepseek.Options{}),
		trae.New(trae.Options{}),
		trae.NewGlobal(trae.Options{}),
		workbuddy.New(workbuddy.Options{}),
		workbuddy.NewAI(workbuddy.Options{}),
		qoder.New(qoder.Options{}),
	}
}
