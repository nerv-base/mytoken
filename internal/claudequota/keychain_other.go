//go:build !darwin

package claudequota

// ClaudeCodeKeychainService is the keychain service name the Claude CLI uses on
// macOS. On other platforms no keychain source exists.
const ClaudeCodeKeychainService = "Claude Code-credentials"

// DefaultKeychainRunner returns nil off macOS: the keychain source is
// unsupported and reports source_unsupported instead of being attempted.
func DefaultKeychainRunner() KeychainRunner { return nil }
