//go:build darwin

package claudequota

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

// ClaudeCodeKeychainService is the keychain service name the Claude CLI uses on
// macOS. It follows the public Claude Code reference and is not verified
// against a live account.
const ClaudeCodeKeychainService = "Claude Code-credentials"

const keychainTimeout = 10 * time.Second

// DefaultKeychainRunner reads one generic password with the fixed argument
// vector below. It never uses a shell, so neither the service nor the account
// can be interpreted as a command.
func DefaultKeychainRunner() KeychainRunner {
	return func(ctx context.Context, service, account string) ([]byte, error) {
		if service == "" {
			return nil, errors.New("claudequota: empty keychain service")
		}
		ctx, cancel := context.WithTimeout(ctx, keychainTimeout)
		defer cancel()
		args := []string{"find-generic-password", "-s", service}
		if account != "" {
			args = append(args, "-a", account)
		}
		args = append(args, "-w")
		var out, errBuf bytes.Buffer
		cmd := exec.CommandContext(ctx, "/usr/bin/security", args...)
		cmd.Stdout = &out
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			// The runner's error text is never surfaced: callers map any
			// failure to the sanitized source_missing reason.
			return nil, errors.New("claudequota: keychain lookup failed")
		}
		return out.Bytes(), nil
	}
}
