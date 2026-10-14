// Package cline parses Cline (saoudrizwan.claude-dev) task logs.
//
// # On-disk layout
//
// Cline stores one directory per task under VS Code's globalStorage:
//
//	~/Library/Application Support/<app>/User/globalStorage/saoudrizwan.claude-dev/tasks/<taskId>/   (macOS)
//	~/.config/<app>/User/globalStorage/saoudrizwan.claude-dev/tasks/<taskId>/                     (Linux)
//	%APPDATA%\<app>\User\globalStorage\saoudrizwan.claude-dev\tasks\<taskId>\                     (Windows)
//
// for every app in the VS Code family (Code, Code - Insiders, Cursor,
// Windsurf, VSCodium, Trae), plus the standalone build's
// ~/.cline/data/tasks/<taskId>/ (see apps/vscode/src/standalone/
// vscode-context.ts, which points globalStorageUri at $CLINE_DIR/data).
//
// The task directory holds:
//
//	ui_messages.json              JSON array of ClineMessage — the usage log
//	api_conversation_history.json  JSON array of the API conversation
//	task_metadata.json             {files_in_context, model_usage, environment_history}
//	history_item.json              Roo-style history record (newer revisions)
//
// and the standalone Cline CLI additionally writes SDK sessions at
// ~/.cline/data/sessions/<sessionId>/<sessionId>.messages.json +
// <sessionId>.json (see sdk/packages/shared/src/storage/paths.ts
// resolveSessionDataDir). See package clinetask for the format details and the
// token accounting rules.
package cline

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/clinetask"
	"github.com/zzstar101/mytoken/internal/model"
)

// Parser reads Cline task logs. It holds no mutable state and is safe for
// concurrent use.
type Parser struct {
	roots []string
	// prov recomputes this parser's default-root provenance from the current
	// environment. It is set only on the registry singleton; a parser built for
	// explicit roots reports those roots instead.
	prov func() ([]harness.Root, error)
}

// New returns the parser for the default roots: every VS Code family
// globalStorage tasks directory plus the standalone Cline data directories.
func New() *Parser {
	p := NewWithRoots()
	p.prov = rootProvenance
	return p
}

// NewWithRoot returns a parser rooted at root. It exists for tests and for
// tools that keep logs outside the home directory.
func NewWithRoot(root string) *Parser { return NewWithRoots(root) }

// NewWithRoots returns a parser over the given roots. An empty list falls back
// to the default roots. Each root is either a globalStorage directory, a tasks
// directory, or a Cline data directory; Discover finds the task files wherever
// they sit underneath it. Roots are cleaned and deduplicated (first-seen order
// kept), so an override or a platform that resolves two roots to one directory
// never scans the same tree twice.
func NewWithRoots(roots ...string) *Parser {
	cp := make([]string, 0, len(roots))
	seen := make(map[string]bool, len(roots))
	for _, r := range roots {
		if r = strings.TrimSpace(r); r != "" {
			r = filepath.Clean(r)
			if seen[r] {
				continue
			}
			seen[r] = true
			cp = append(cp, r)
		}
	}
	if len(cp) == 0 {
		cp = defaultRoots()
	}
	return &Parser{roots: cp}
}

// rootProvenance reports Cline's default roots and where each came from.
// MYTOKEN_CLINE_DIRS replaces the whole set; the CLINE_* variables are explicit
// harness-specific overrides for the Cline-owned roots only, while the VS Code
// globalStorage roots stay platform defaults.
func rootProvenance() ([]harness.Root, error) {
	if v, ok := harness.Getenv("MYTOKEN_CLINE_DIRS"); ok {
		dirs, err := harness.SplitEnvList("MYTOKEN_CLINE_DIRS", v)
		if err != nil {
			return nil, err
		}
		return harness.EnvRoots("MYTOKEN_CLINE_DIRS", dirs...), nil
	}
	roots := harness.BuiltinRoots(clinetask.VSCodeTaskDirs(clinetask.ExtensionIDCline)...)
	dataEnv := ""
	if _, ok := harness.Getenv("CLINE_DATA_DIR"); ok {
		dataEnv = "CLINE_DATA_DIR"
	} else if _, ok := harness.Getenv("CLINE_DIR"); ok {
		dataEnv = "CLINE_DIR"
	}
	for _, d := range clinetask.ClineDataDirs() {
		roots = append(roots, taggedRoot(dataEnv, filepath.Join(d, "tasks")))
	}
	sessEnv := dataEnv
	if _, ok := harness.Getenv("CLINE_SESSION_DATA_DIR"); ok {
		sessEnv = "CLINE_SESSION_DATA_DIR"
	}
	for _, d := range clinetask.ClineSessionDirs() {
		roots = append(roots, taggedRoot(sessEnv, d))
	}
	return roots, nil
}

// taggedRoot tags path with env when it came from an explicit environment
// variable and with the built-in origin otherwise.
func taggedRoot(env, path string) harness.Root {
	if env == "" {
		return harness.Root{Path: path, Origin: harness.OriginBuiltin}
	}
	return harness.Root{Path: path, Origin: harness.OriginEnv, Env: env}
}

func defaultRoots() []string {
	roots, err := rootProvenance()
	if err != nil {
		return nil
	}
	return harness.RootPaths(roots)
}

// RootProvenance implements harness.Provenance.
func (p *Parser) RootProvenance() ([]harness.Root, error) {
	if p.prov == nil {
		return harness.ExplicitRoots(p.roots...), nil
	}
	return p.prov()
}

func (p *Parser) Harness() model.Harness { return model.Cline }

// WithRoots implements harness.Rooted. It returns a fresh parser over exactly
// the given roots; an explicit empty list stays empty instead of falling back
// to this harness's default roots.
func (p *Parser) WithRoots(roots ...string) harness.Parser {
	for _, r := range roots {
		if strings.TrimSpace(r) != "" {
			return NewWithRoots(roots...)
		}
	}
	return &Parser{}
}

// Roots returns the directories to watch.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

var _ harness.Parser = (*Parser)(nil)

func init() { harness.Register(New()) }

func (p *Parser) options() clinetask.Options {
	return clinetask.Options{
		Harness:            model.Cline,
		ExtensionID:        clinetask.ExtensionIDCline,
		InputIncludesCache: clineInputIncludesCache,
		// Cline also writes the standalone CLI session format.
		Sessions: true,
	}
}

// Discover walks every root and returns one Source per task file, sorted by
// path so scans are deterministic.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	opts := p.options()
	var out []harness.Source
	for _, root := range p.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out = append(out, clinetask.Discover(root, opts)...)
	}
	return out, nil
}

// Parse reads one task file incrementally. Task files are JSON documents
// rewritten in place, so the cursor records the file fingerprint plus how many
// usage entries were already emitted; a changed fingerprint means the whole file
// is re-read and only the entries past that counter are emitted.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if err := ctx.Err(); err != nil {
		return harness.Batch{}, err
	}
	return clinetask.Parse(src, cur, p.options())
}

// clineInputIncludesCache reports whether an api_req_started payload's tokensIn
// already contains the cached tokens reported in its cacheReads/cacheWrites.
//
// Cline normalises usage events before writing them — apps/vscode/src/sdk/
// message-translator.ts normalizeUsageEvent: "SDK provider usage reports
// inputTokens as the full request size, with cache reads/writes included.
// Classic Cline/webview metrics expect tokensIn, cacheReads, and cacheWrites to
// be disjoint buckets" — so a current Cline task writes a disjoint tokensIn.
// Legacy Cline wrote the provider's raw input, and for OpenAI-style providers
// that raw value is prompt_tokens, which already includes the cached tokens
// (src/api/providers/openai-native.ts: "Keep inputTokens as TOTAL input to
// preserve correct context length").
//
// Cline never records apiProtocol, so the decision falls back to the provider
// id, mirroring Roo's getApiProtocol (packages/types/src/provider-settings.ts):
// anthropic, bedrock and minimax are Anthropic-style, as are Claude models on
// vertex and vercel-ai-gateway; everything else is OpenAI-style.
func clineInputIncludesCache(protocol, providerID, modelID string) bool {
	switch protocol {
	case "anthropic":
		return false
	case "openai":
		return true
	}
	return !isAnthropicStyle(providerID, modelID)
}

func isAnthropicStyle(providerID, modelID string) bool {
	switch providerID {
	case "anthropic", "bedrock", "minimax":
		return true
	case "vertex":
		return strings.Contains(strings.ToLower(modelID), "claude")
	case "vercel-ai-gateway":
		return strings.HasPrefix(modelID, "anthropic/")
	}
	return false
}
