package claudequota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

// CredentialsFileName is the credential file Claude Code keeps inside its
// config directory.
const CredentialsFileName = ".credentials.json"

const (
	maxCredentialBytes = 64 << 10
	maxBareTokenBytes  = 4096
)

// SourceKind names where a credential comes from.
type SourceKind string

// Source kinds.
const (
	SourceFile     SourceKind = "file"
	SourceKeychain SourceKind = "keychain"
)

// Credential is a Claude Code OAuth access token. Its value never appears in
// String, GoString, Format or JSON output; Reveal is the only accessor and is
// counted so tests can assert the token is read but never rendered.
type Credential struct {
	token     string
	expiresAt time.Time
	hasExpiry bool
}

var credentialReveals atomic.Int64

// NewCredential wraps a bare access token.
func NewCredential(token string) Credential {
	return Credential{token: strings.TrimSpace(token)}
}

// CredentialExpiring wraps an access token with its expiry.
func CredentialExpiring(token string, expiresAt time.Time) Credential {
	c := NewCredential(token)
	c.expiresAt = expiresAt.UTC()
	c.hasExpiry = !expiresAt.IsZero()
	return c
}

// Reveal returns the token. Callers must not log, persist or render it.
func (c Credential) Reveal() string {
	credentialReveals.Add(1)
	return c.token
}

// Empty reports whether no token is present.
func (c Credential) Empty() bool { return strings.TrimSpace(c.token) == "" }

// HasExpiry reports whether the credential carried an expiry.
func (c Credential) HasExpiry() bool { return c.hasExpiry }

// ExpiresAt returns the stated expiry, or the zero time.
func (c Credential) ExpiresAt() time.Time { return c.expiresAt }

// Expired reports whether the credential is past its stated expiry.
func (c Credential) Expired(now time.Time) bool {
	return c.hasExpiry && !now.Before(c.expiresAt)
}

// CredentialReveals is the number of tokens revealed so far.
func CredentialReveals() int64 { return credentialReveals.Load() }

func (c Credential) String() string   { return "[redacted]" }
func (c Credential) GoString() string { return "claudequota.Credential{[redacted]}" }
func (c Credential) Format(f fmt.State, verb rune) {
	_, _ = io.WriteString(f, "[redacted]")
}

// MarshalJSON never serializes the token.
func (c Credential) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// Source is one place a Claude Code credential may be stored.
type Source struct {
	// Label is the human-readable account label.
	Label string
	// Kind is SourceFile or SourceKeychain.
	Kind SourceKind
	// Ref is the file path, or the keychain service name.
	Ref string
	// Profile is the config directory name the source belongs to; empty for the
	// default profile.
	Profile string

	read func(ctx context.Context) (Credential, error)
}

// Read returns the credential held by the source.
func (s Source) Read(ctx context.Context) (Credential, error) {
	if s.read == nil {
		return Credential{}, plan.NewError(plan.ReasonNoCredentials, nil)
	}
	return s.read(ctx)
}

// Present reports whether the source holds a readable credential. It reads the
// credential and discards it; it never performs a network request.
func (s Source) Present(ctx context.Context) bool {
	cred, err := s.Read(ctx)
	return err == nil && !cred.Empty()
}

// FileSource reads a credential JSON file.
func FileSource(label, path string) Source {
	return Source{Label: label, Kind: SourceFile, Ref: path, read: fileReader(path)}
}

func fileReader(path string) func(context.Context) (Credential, error) {
	return func(ctx context.Context) (Credential, error) {
		if err := ctx.Err(); err != nil {
			return Credential{}, plan.NewError(plan.ReasonCanceled, err)
		}
		f, err := os.Open(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return Credential{}, plan.NewError(plan.ReasonSourceMissing, err)
			}
			return Credential{}, plan.NewError(plan.ReasonSourceUnreadable, err)
		}
		defer f.Close()
		raw, err := io.ReadAll(io.LimitReader(f, maxCredentialBytes+1))
		if err != nil {
			return Credential{}, plan.NewError(plan.ReasonSourceUnreadable, err)
		}
		if len(raw) > maxCredentialBytes {
			return Credential{}, plan.NewError(plan.ReasonSourceUnreadable, nil)
		}
		return decodeCredential(raw)
	}
}

// KeychainRunner reads a generic password. It is a function so tests and other
// platforms can substitute one.
type KeychainRunner func(ctx context.Context, service, account string) ([]byte, error)

// KeychainSource reads a credential from the system keychain.
func KeychainSource(label, service, account string, run KeychainRunner) Source {
	ref := service
	if account != "" {
		ref += ":" + account
	}
	return Source{Label: label, Kind: SourceKeychain, Ref: ref, read: func(ctx context.Context) (Credential, error) {
		if run == nil {
			return Credential{}, plan.NewError(plan.ReasonUnsupported, nil)
		}
		raw, err := run(ctx, service, account)
		if err != nil {
			return Credential{}, plan.NewError(plan.ReasonSourceMissing, err)
		}
		if len(raw) > maxCredentialBytes {
			return Credential{}, plan.NewError(plan.ReasonSourceUnreadable, nil)
		}
		return decodeCredential(raw)
	}}
}

// credentialFile is the on-disk shape Claude Code writes.
type credentialFile struct {
	ClaudeAiOauth *struct {
		AccessToken string `json:"accessToken"`
		ExpiresAt   int64  `json:"expiresAt"`
	} `json:"claudeAiOauth"`
}

// decodeCredential accepts the credential JSON Claude Code writes, or a bare
// token (the shape some CLI releases stored). A file that carries only
// unrelated OAuth entries (for example mcpOAuth) is not Claude Code's OAuth
// credential and is refused.
func decodeCredential(raw []byte) (Credential, error) {
	var cf credentialFile
	if err := json.Unmarshal(raw, &cf); err == nil && cf.ClaudeAiOauth != nil {
		token := strings.TrimSpace(cf.ClaudeAiOauth.AccessToken)
		if token == "" {
			return Credential{}, plan.NewError(plan.ReasonNoCredentials, nil)
		}
		cred := NewCredential(token)
		if cf.ClaudeAiOauth.ExpiresAt > 0 {
			cred = CredentialExpiring(token, unixTime(cf.ClaudeAiOauth.ExpiresAt))
		}
		return cred, nil
	}
	if isBareToken(raw) {
		return NewCredential(string(raw)), nil
	}
	return Credential{}, plan.NewError(plan.ReasonSourceUnreadable, nil)
}

func isBareToken(raw []byte) bool {
	if len(raw) == 0 || len(raw) > maxBareTokenBytes {
		return false
	}
	for _, r := range string(raw) {
		if r == '\n' || r == '\r' || r == '{' || r == '}' || r == ' ' || r == '\t' {
			return false
		}
	}
	return true
}

// unixTime reads an epoch value as milliseconds, or as seconds when the value
// is too small to be milliseconds.
func unixTime(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	if v < 1e12 {
		return time.Unix(v, 0).UTC()
	}
	return time.UnixMilli(v).UTC()
}

// DefaultConfigDir is Claude Code's config directory for a home directory.
func DefaultConfigDir(home string) string { return filepath.Join(home, ".claude") }

// configDirs returns the config directories to scan: $CLAUDE_CONFIG_DIR entries
// first, then the default directory, without duplicates.
func configDirs(getenv func(string) string, home string) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(dir string) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			return
		}
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		dir = filepath.Clean(dir)
		if seen[dir] {
			return
		}
		seen[dir] = true
		out = append(out, dir)
	}
	if getenv != nil {
		for _, dir := range strings.Split(getenv("CLAUDE_CONFIG_DIR"), string(os.PathListSeparator)) {
			add(dir)
		}
	}
	if home != "" {
		add(DefaultConfigDir(home))
	}
	return out
}
