// Package claudequota is the opt-in Claude Code subscription-quota service:
// it reads the credential the Claude CLI already owns and asks Anthropic's
// OAuth usage endpoint what the account's rolling windows look like.
//
// # Contract
//
//   - Default off. Nothing in this package reads a credential, touches a
//     keychain or opens a socket until an explicit, persisted consent for one
//     instance and one credential source exists. Consent is verified before
//     the credential read and again before the result is published, so a
//     revocation always wins.
//   - Consent is per instance and per source, and records the exact source
//     reference, the exact request target, the consent schema version and the
//     instance identity. Changing the source or the target invalidates it.
//     Consent never contains a secret.
//   - The credential is used in memory for one request and is never written to
//     the settings store, a log line, a config file or an error string. Only
//     quota windows, their instants and the source/target identity are cached.
//   - The OAuth token is never refreshed and the credential is never rewritten:
//     an expired token is reported as unavailable with an instruction to renew
//     it with the Claude CLI.
//   - The endpoint is fixed (Target). Redirects are refused, the body and the
//     request are bounded, and every error is a sanitized, fixed message.
//   - Missing is not zero: a window the payload did not state stays absent
//     with no percentage, and a payload with no window at all is unavailable
//     rather than 0%.
//
// The endpoint path and the credentials-file shape follow public Claude Code
// references; they have not been verified against a live account, and the
// macOS keychain source is documented but not verified.
package claudequota

// Target is the fixed HTTPS OAuth usage endpoint. It is never configurable, so
// the target a consent was granted for can never drift.
const Target = "https://api.anthropic.com/api/oauth/usage"

// CredentialsFileName is the file the Claude CLI keeps its OAuth credential in,
// inside the profile's config directory (".credentials.json").
const CredentialsFileName = ".credentials.json"

// ConsentVersion is the consent schema this build writes and accepts. A stored
// consent from another version is treated as absent.
const ConsentVersion = 1

// Setting key prefixes. The full key is the prefix plus the instance id; the
// CLI and the GUI share the store, so they share one consent.
const (
	// SettingConsentPrefix holds the explicit consent record. An empty value
	// means revoked.
	SettingConsentPrefix = "quota.claude.consent."
	// SettingLatestPrefix holds the cached quota windows and their freshness.
	SettingLatestPrefix = "quota.claude.latest."
)

// maxCredentialBytes bounds one credential read.
const maxCredentialBytes = 64 << 10

// maxQuotaBytes bounds one persisted quota value.
const maxQuotaBytes = 64 << 10
