// Package claudequota reports the plan quota of a locally signed-in Claude Code.
//
// Claude Code stores its OAuth credential either in the macOS keychain (generic
// password "Claude Code-credentials") or in .credentials.json inside its config
// directory ($CLAUDE_CONFIG_DIR, default ~/.claude). When one is present, this
// package asks Anthropic's OAuth usage endpoint — the same one Claude Code's own
// /usage view is backed by — and maps the returned windows onto plan.Window.
//
// The request shape (endpoint, anthropic-beta: oauth-2025-04-20, user:profile
// scope, five_hour / seven_day / seven_day_opus / seven_day_sonnet windows)
// follows CodexBar's Claude provider:
// https://github.com/steipete/CodexBar/blob/main/docs/claude.md
//
// The credential is read for the duration of one request. It is not logged,
// not persisted and not included in errors or snapshots; only the quota numbers
// are returned. Expired tokens are reported as such instead of being refreshed:
// refreshing would require writing the credential back, which this package
// deliberately does not do.
package claudequota
