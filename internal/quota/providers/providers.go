// Package providers assembles every quota provider MyToken supports.
//
// The plan package polls providers; this package wires the concrete
// implementations so the application can start a service with a single call.
// Every provider reads local credentials to authenticate, and every quota
// value itself comes from the provider's server.
package providers

import (
	"github.com/nerv-base/mytoken/internal/claudequota"
	"github.com/nerv-base/mytoken/internal/quota/codex"
	"github.com/nerv-base/mytoken/internal/quota/copilot"
	"github.com/nerv-base/mytoken/internal/quota/cursor"
	"github.com/nerv-base/mytoken/internal/quota/deepseek"
	"github.com/nerv-base/mytoken/internal/quota/glm"
	"github.com/nerv-base/mytoken/internal/quota/plan"
	"github.com/nerv-base/mytoken/internal/quota/qoder"
	"github.com/nerv-base/mytoken/internal/quota/trae"
	"github.com/nerv-base/mytoken/internal/quota/workbuddy"
	"github.com/nerv-base/mytoken/internal/quota/zcode"
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
		zcode.New(zcode.Options{}),
	}
}
