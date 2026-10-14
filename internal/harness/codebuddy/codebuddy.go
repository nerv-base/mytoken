// Package codebuddy parses CodeBuddy session logs.
//
// CodeBuddy writes the same record schema and project layout as WorkBuddy, so
// this package instantiates the WorkBuddy reader with the CodeBuddy harness id
// and home directory instead of duplicating it:
//
//	$CODEBUDDY_HOME/projects/<project-slug>/<session-id>.jsonl
//	$CODEBUDDY_HOME/projects/<project-slug>/<session-id>/subagents/agent-<hex>.jsonl
//
// The record schema (message/function_call/reasoning/function_call_result/
// ai-title/file-history-snapshot, providerData.rawUsage, credit bills) is the
// one documented in package workbuddy.
//
// CodeBuddy is Tencent's terminal coding agent (npm @tencent-ai/codebuddy-code,
// <https://cnb.cool/codebuddy/codebuddy-code>), and its home is $CODEBUDDY_HOME
// or ~/.codebuddy.
//
// CAVEAT: no CodeBuddy session log has been observed on this machine — the
// ~/.codebuddy home only holds diagnostics/, logs/memwatch/ and skills/ — so the
// layout above is inferred from WorkBuddy and is UNVERIFIED against real
// CodeBuddy data.
package codebuddy

import (
	"path/filepath"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/workbuddy"
	"github.com/zzstar101/mytoken/internal/model"
)

const (
	homeDirName = ".codebuddy"
	envHome     = "CODEBUDDY_HOME"
)

func init() { harness.Register(New()) }

// New returns a parser for the standard CodeBuddy locations.
func New() *workbuddy.Parser { return workbuddy.NewDefault(model.CodeBuddy, rootProvenance) }

// NewWithRoots returns a CodeBuddy parser reading the given project roots.
func NewWithRoots(roots ...string) *workbuddy.Parser {
	return workbuddy.NewForHarness(model.CodeBuddy, roots...)
}

// rootProvenance reports CodeBuddy's default roots and where they came from.
func rootProvenance() ([]harness.Root, error) {
	if dir, ok := harness.Getenv(envHome); ok {
		return harness.EnvRoots(envHome, filepath.Join(dir, "projects")), nil
	}
	return harness.BuiltinRoots(filepath.Join(harness.Home(), homeDirName, "projects")), nil
}

// DefaultRoots returns the CodeBuddy project roots for this machine.
func DefaultRoots() []string {
	roots, err := rootProvenance()
	if err != nil {
		return nil
	}
	return harness.RootPaths(roots)
}
