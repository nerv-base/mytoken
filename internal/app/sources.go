package app

// This file is the single wiring step between sources.json and the scanner:
// instance.Load (in app.go) -> instance.Resolve -> rooted parsers ->
// scan.NewWithInstances. It also carries the resolved-source diagnostics that
// the GUI/CLI read, so both entry points fail and report identically.

import (
	"strings"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/instance"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/scan"
)

// WiringCode is a stable, machine-readable reason for a wiring failure the
// instance package has no code for.
type WiringCode string

// CodeNoFactory reports a scannable instance whose harness has no registered
// rooted parser. Its roots are not silently dropped: the app refuses to start.
const CodeNoFactory WiringCode = "no-factory"

// Source is one instance the loaded configuration resolved to, plus what the
// app actually did with it. It is the read-only diagnostics surface for the
// forthcoming GUI/CLI: it deliberately carries no parser, so a consumer cannot
// re-drive the scanner through it.
type Source struct {
	instance.Resolved
	// Scanned is true when this source was handed to the scanner as a rooted
	// parser. Disabled and backup-copy entries are never scanned.
	Scanned bool
	// Reason is the first non-fatal resolution problem recorded for this
	// source ("" when none). Fatal problems never reach a built App: Open and
	// OpenLocal return them as *SourceError instead.
	Reason string
}

// SourceError is a fatal wiring failure, returned instead of a partially wired
// App: a malformed sources.json, an unusable explicit root, a fatal resolution
// problem, or a configured root whose parser cannot be built. Code is an
// instance.Code for resolution problems and a WiringCode for app-level ones.
type SourceError struct {
	Code    string
	ID      string
	Harness model.Harness
	Path    string
	Message string
}

func (e *SourceError) Error() string {
	var b strings.Builder
	b.WriteString("sources: ")
	b.WriteString(e.Code)
	if e.ID != "" {
		b.WriteString(" instance=")
		b.WriteString(e.ID)
	}
	if e.Harness != "" {
		b.WriteString(" harness=")
		b.WriteString(string(e.Harness))
	}
	if e.Path != "" {
		b.WriteString(" path=")
		b.WriteString(e.Path)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

// resolvedSources is the app's one answer to "what should be scanned": the full
// resolved set for diagnostics, the scanner targets built from the scannable
// subset, and the non-fatal problems.
type resolvedSources struct {
	sources  []Source
	targets  []scan.Instance
	problems []instance.Problem
}

// rootedFactory mirrors harness.NewRooted. It is a parameter so the
// missing-factory guard can be tested without an unregistered harness.
type rootedFactory func(model.Harness, ...string) (harness.Parser, bool)

func problemError(p instance.Problem) *SourceError {
	return &SourceError{
		Code:    string(p.Code),
		ID:      p.ID,
		Harness: p.Harness,
		Path:    p.Path,
		Message: p.Message,
	}
}

// resolveSources turns the loaded configuration into scan targets and refuses
// anything it cannot honour:
//
//   - any fatal Problem from instance.Resolve stops here, before a store, a
//     scan or a network worker exists;
//   - a configured (explicit) instance that ends up with no root is an error,
//     so an unsupported or unusable configured root is never dropped; a default
//     harness with no surviving built-in root is instead a diagnostic, because
//     the tool may simply not be installed here;
//   - a scannable instance whose parser factory is missing is an error, so an
//     unsupported root is never silently dropped;
//   - disabled and backup-copy entries become diagnostics only. They still
//     claim their harness (instance.Resolve already suppresses that harness's
//     default fallback) but they are never scanned.
//
// Beyond the stat/canonicalization instance.Resolve already performs on the
// configured roots, it does no I/O.
func resolveSources(cfg *instance.Config, fallbacks []instance.Fallback, newRooted rootedFactory) (resolvedSources, error) {
	resolved, problems := instance.Resolve(cfg, fallbacks)
	for _, p := range problems {
		if p.Fatal {
			return resolvedSources{problems: problems}, problemError(p)
		}
	}
	out := resolvedSources{problems: problems}
	reason := make(map[string]string, len(problems))
	for _, p := range problems {
		key := string(p.Harness) + "\x00" + p.ID
		if _, seen := reason[key]; !seen {
			reason[key] = p.Message
		}
	}
	for _, r := range resolved {
		src := Source{Resolved: r, Reason: reason[string(r.Harness)+"\x00"+r.ID]}
		if !r.Scannable() {
			out.sources = append(out.sources, src)
			continue
		}
		if len(r.Roots) == 0 {
			// A default harness every built-in root of which is missing is
			// simply not installed on this machine: instance.Resolve reports
			// each missing root non-fatally and yields the instance with no
			// roots. There is nothing to scan, so it stays a diagnostic.
			// An explicit entry cannot reach this point with no roots today
			// (every unusable explicit root is fatal in instance.Resolve); the
			// guard is kept so a resolution change can never silently drop a
			// root the user configured.
			if r.Source == instance.PathUser {
				return out, &SourceError{
					Code: string(instance.CodeNoRoots), ID: r.ID, Harness: r.Harness,
					Message: "configured instance has no usable root to scan",
				}
			}
			out.sources = append(out.sources, src)
			continue
		}
		parser, ok := newRooted(r.Harness, r.Roots...)
		if !ok {
			return out, &SourceError{
				Code: string(CodeNoFactory), ID: r.ID, Harness: r.Harness,
				Message: "no registered rooted parser for this harness; its roots would be dropped",
			}
		}
		src.Scanned = true
		out.sources = append(out.sources, src)
		out.targets = append(out.targets, scan.Instance{ID: r.ID, Parser: parser})
	}
	return out, nil
}

// ScannedSources returns the subset of the current resolution the scanner is
// actually reading, as a copy, for a GUI/CLI that wants the live instance
// list. It follows a successful ReloadSources.
func (a *App) ScannedSources() []Source {
	sources := a.LiveSources()
	out := make([]Source, 0, len(sources))
	for _, s := range sources {
		if s.Scanned {
			out = append(out, s)
		}
	}
	return out
}
