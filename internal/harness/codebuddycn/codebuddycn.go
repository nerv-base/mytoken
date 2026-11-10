// Package codebuddycn parses CodeBuddy CN (国内版, codebuddy.cn) usage.
//
// CodeBuddy ships as two separate products with separate homes:
//
//	CodeBuddy     (international, codebuddy.ai)  $CODEBUDDY_HOME   default ~/.codebuddy
//	CodeBuddy CN  (mainland,     codebuddy.cn)   $CODEBUDDYCN_HOME default ~/.codebuddycn
//
// This package covers the mainland build. It reads two kinds of source, because
// the two shapes of that product keep their numbers in different places.
//
// # 1. Desktop IDE / VS Code extension logs (kind "log")
//
// The desktop app and the tencent-cloud.coding-copilot extension journal every
// finished request to a plain-text log, one line per request:
//
//	[2026/11/7 23:36:41.589] [Info] [CraftInvokableAgent] [<agent>]  Model prepared: Hy4 (hy4free)
//	[2026/11/7 23:36:41.589] [Info] [AgentReporter] [<agent>]  Agent execution successful with usage: {"inputTokens":3315976,"outputTokens":12645,"cacheTokens":3273408,"cachedMissTokens":42568,"outputTokens":12645,"cachedWriteTokens":0,"credit":0.2,"thinkingTokens":8417}
//
// Log locations, by platform. The macOS and Windows entries below were checked
// against real installs (every path marked "verified" either existed or was
// confirmed absent there); the Linux entries are inferred from the same Electron
// and VS Code conventions and have not been observed on a real machine, so the
// Linux walk searches every plausible base instead of one guessed path:
//
//	macOS    ~/Library/Application Support/CodeBuddy CN/logs                 verified
//	         ~/Library/Application Support/Code/logs                         verified absent
//	         ~/Library/Application Support/CodeBuddyExtension/Logs/CodeBuddyIDE  verified
//	         ~/Library/Application Support/CodeBuddyExtension/Logs/VSCode        verified
//	Windows  %APPDATA%\CodeBuddy CN\logs                                     verified
//	         %APPDATA%\Code\logs                                             verified
//	         %LOCALAPPDATA%\CodeBuddyExtension\Logs\CodeBuddyIDE             verified
//	         %LOCALAPPDATA%\CodeBuddyExtension\Logs\VSCode                   verified absent
//	Linux    $XDG_CONFIG_HOME/{CodeBuddy CN,Code}/logs                       UNVERIFIED
//	         $XDG_DATA_HOME/CodeBuddyExtension/Logs/{CodeBuddyIDE,VSCode}    UNVERIFIED
//
// The same request is journalled twice — once by the app's own log, once by the
// extension log — with timestamps one millisecond apart, so DedupKey is built
// from the agent id and the five token classes and never from the path or the
// millisecond.
//
// # 2. CLI session transcripts (kind "jsonl")
//
// The CLI writes the WorkBuddy record schema under the same project layout as
// the international build, so this package instantiates the WorkBuddy reader
// with the CodeBuddy CN harness id and home directory instead of duplicating it:
//
//	$CODEBUDDYCN_HOME/projects/<project-slug>/<session-id>.jsonl
//	$CODEBUDDYCN_HOME/projects/<project-slug>/<session-id>/subagents/agent-<hex>.jsonl
//
// CAVEAT: no CodeBuddy CN CLI transcript has been observed on this machine, so
// that half is inferred from the international build and is UNVERIFIED against
// real CodeBuddy CN data. The log half is verified against real logs.
package codebuddycn

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/nerv-base/mytoken/internal/harness"
	"github.com/nerv-base/mytoken/internal/harness/workbuddy"
	"github.com/nerv-base/mytoken/internal/model"
)

const (
	homeDirName = ".codebuddycn"
	// envHome is the CN build's own variable. envHomeAlias is accepted as a
	// fallback because the product has shipped both spellings.
	envHome      = "CODEBUDDYCN_HOME"
	envHomeAlias = "CODEBUDDY_CN_HOME"
)

func init() { harness.Register(New()) }

// Parser reads CodeBuddy CN: the CLI's jsonl transcripts and the desktop app's
// request logs.
type Parser struct {
	roots []string
	// explicit marks a parser built for user-chosen roots, which are walked
	// without the log-name filter.
	explicit bool
	// prov recomputes the default-root provenance from the environment. It is
	// set only on the registry singleton.
	prov func() ([]harness.Root, error)
}

// New returns a parser for the standard CodeBuddy CN locations.
func New() *Parser {
	p := &Parser{prov: rootProvenance}
	if roots, err := rootProvenance(); err == nil {
		p.roots = harness.RootPaths(roots)
	}
	return p
}

// NewWithRoots returns a parser reading only the given roots (tests and the
// Settings "choose folder" override). Blank entries are dropped, so an explicit
// empty list means "no roots" and never falls back to the defaults.
func NewWithRoots(roots ...string) *Parser {
	p := &Parser{explicit: true}
	for _, root := range roots {
		if clean := strings.TrimSpace(root); clean != "" {
			p.roots = append(p.roots, filepath.Clean(clean))
		}
	}
	return p
}

// NewForHarness keeps the old constructor's shape for callers that only want the
// CLI transcripts.
func NewForHarness(h model.Harness, roots ...string) harness.Parser {
	return NewWithRoots(roots...)
}

// Harness implements harness.Parser.
func (p *Parser) Harness() model.Harness { return model.CodeBuddyCN }

// Roots implements harness.Parser.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

// WithRoots implements harness.Rooted.
func (p *Parser) WithRoots(roots ...string) harness.Parser { return NewWithRoots(roots...) }

// RootProvenance implements harness.Provenance.
func (p *Parser) RootProvenance() ([]harness.Root, error) {
	if p.prov == nil {
		return harness.ExplicitRoots(p.roots...), nil
	}
	return p.prov()
}

// Discover implements harness.Parser.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	files, err := p.discoverJSONL(ctx)
	if err != nil {
		return nil, err
	}
	logs, err := p.discoverLogs(ctx)
	if err != nil {
		return nil, err
	}
	out := append(files, logs...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Parse implements harness.Parser.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if src.Kind == kindLog {
		return p.parseLog(ctx, src, cur)
	}
	return p.parseJSONL(ctx, src, cur)
}

// discoverJSONL lists the CLI transcripts under every root.
func (p *Parser) discoverJSONL(ctx context.Context) ([]harness.Source, error) {
	if len(p.roots) == 0 {
		return nil, nil
	}
	return workbuddy.NewForHarness(model.CodeBuddyCN, p.roots...).Discover(ctx)
}

// parseJSONL delegates a transcript to the WorkBuddy reader.
func (p *Parser) parseJSONL(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	return workbuddy.NewForHarness(model.CodeBuddyCN, filepath.Dir(src.Path)).Parse(ctx, src, cur)
}

// discoverLogs lists the app and extension logs under every root.
func (p *Parser) discoverLogs(ctx context.Context) ([]harness.Source, error) {
	var out []harness.Source
	seen := map[string]bool{}
	for _, root := range p.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !isDir(root) {
			continue
		}
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() {
				if path != root && strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !isLog(path) || seen[path] || !p.logAllowed(path) {
				return nil
			}
			seen[path] = true
			out = append(out, harness.Source{Path: path, Kind: kindLog})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// logAllowed keeps only the logs that belong to CodeBuddy, unless the caller
// chose the directory explicitly.
func (p *Parser) logAllowed(path string) bool {
	if p.explicit {
		return true
	}
	lower := strings.ToLower(filepath.ToSlash(path))
	if strings.Contains(lower, "coding-copilot") {
		return true
	}
	return strings.Contains(lower, "/logs/codebuddyide/") || strings.Contains(lower, "/logs/vscode/")
}

func isLog(name string) bool { return strings.HasSuffix(strings.ToLower(name), ".log") }

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// roots returns the CLI home plus the desktop app's log directories, tagged with
// where each came from.
func roots(home string, getenv func(string) string, goos string) []harness.Root {
	var out []harness.Root
	projectsRoot := filepath.Join(home, homeDirName, "projects")
	for _, env := range []string{envHome, envHomeAlias} {
		if dir, ok := getenvOK(getenv, env); ok {
			projectsRoot = filepath.Join(dir, "projects")
			out = append(out, harness.EnvRoots(env, projectsRoot)...)
			projectsRoot = ""
			break
		}
	}
	if projectsRoot != "" {
		out = append(out, harness.BuiltinRoots(projectsRoot)...)
	}

	var logDirs []string
	switch goos {
	case "windows":
		// A real install puts the app's userData under %APPDATA% and the shared
		// CodeBuddyExtension data under %LOCALAPPDATA%. Both trees are still
		// walked, for the editor variants and for builds that pick the other
		// one; a missing directory is simply ignored.
		appData := firstNonEmpty(getenv("APPDATA"), filepath.Join(home, "AppData", "Roaming"))
		localData := firstNonEmpty(getenv("LOCALAPPDATA"), filepath.Join(home, "AppData", "Local"))
		logDirs = append(logDirs, appLogDirs(appData)...)
		logDirs = append(logDirs, appLogDirs(localData)...)
		logDirs = append(logDirs, extensionLogDirs(appData)...)
		logDirs = append(logDirs, extensionLogDirs(localData)...)
	case "linux":
		// XDG variables win when the app honours them; the plain defaults stay
		// in the list because a bundled Electron app does not always do so.
		config := uniqueDirs(
			firstNonEmpty(getenv("XDG_CONFIG_HOME"), filepath.Join(home, ".config")),
			filepath.Join(home, ".config"),
		)
		data := uniqueDirs(
			firstNonEmpty(getenv("XDG_DATA_HOME"), filepath.Join(home, ".local", "share")),
			filepath.Join(home, ".local", "share"),
		)
		for _, base := range config {
			logDirs = append(logDirs, appLogDirs(base)...)
		}
		for _, base := range data {
			logDirs = append(logDirs, appLogDirs(base)...)
		}
		for _, base := range append(append([]string{}, config...), data...) {
			logDirs = append(logDirs, extensionLogDirs(base)...)
		}
	default:
		// macOS: the only tree verified against real files.
		base := filepath.Join(home, "Library", "Application Support")
		logDirs = append(logDirs, appLogDirs(base)...)
		logDirs = append(logDirs, extensionLogDirs(base)...)
	}
	return append(out, harness.BuiltinRoots(uniqueDirs(logDirs...)...)...)
}

// appLogDirs lists the desktop app's own log trees under an Electron userData
// base, for every IDE the CodeBuddy extension may be installed into.
func appLogDirs(base string) []string {
	out := make([]string, 0, 3)
	for _, product := range []string{"CodeBuddy CN", "Code", "Code - Insiders"} {
		out = append(out, filepath.Join(base, product, "logs"))
	}
	return out
}

// extensionLogDirs lists the extension's per-project logs under an app-data base.
func extensionLogDirs(base string) []string {
	return []string{
		filepath.Join(base, "CodeBuddyExtension", "Logs", "CodeBuddyIDE"),
		filepath.Join(base, "CodeBuddyExtension", "Logs", "VSCode"),
	}
}

// uniqueDirs drops blanks and repeats while keeping the first-seen order.
func uniqueDirs(dirs ...string) []string {
	seen := make(map[string]bool, len(dirs))
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		clean := filepath.Clean(dir)
		if clean == "" || clean == "." || seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, clean)
	}
	return out
}

// rootProvenance reports CodeBuddy CN's default roots and where they came from.
func rootProvenance() ([]harness.Root, error) {
	return roots(harness.Home(), os.Getenv, runtime.GOOS), nil
}

// DefaultRoots returns the CodeBuddy CN roots for this machine.
func DefaultRoots() []string {
	roots, err := rootProvenance()
	if err != nil {
		return nil
	}
	return harness.RootPaths(roots)
}

func getenvOK(getenv func(string) string, key string) (string, bool) {
	v := strings.TrimSpace(getenv(key))
	return v, v != ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
