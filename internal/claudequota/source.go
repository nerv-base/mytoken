package claudequota

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// SourceKind identifies where a credential lives.
type SourceKind string

// Known source kinds.
const (
	// SourceFile is the Claude CLI credentials file.
	SourceFile SourceKind = "file"
	// SourceKeychain is the macOS login keychain entry the Claude CLI writes.
	SourceKeychain SourceKind = "keychain"
)

// Credential is one OAuth access token in memory. Every formatting and
// serialization path redacts it, so it cannot leak into a log, an error, a
// config or JSON output by accident.
type Credential struct {
	token     string
	expiresAt time.Time
	hasExpiry bool
}

// NewCredential wraps a token. It is used by tests and by the readers below.
func NewCredential(token string) Credential { return Credential{token: token} }

// Reveal returns the token itself. Callers must use it only to build the one
// authorized request and must never store, log or serialize the result.
func (c Credential) Reveal() string {
	credentialReveals.Add(1)
	return c.token
}

// ExpiresAt reports the stated expiry, when the source carried one.
func (c Credential) ExpiresAt() (time.Time, bool) { return c.expiresAt, c.hasExpiry }

// Expired reports whether the token is known to be expired at now. An
// unstated expiry is never treated as expired.
func (c Credential) Expired(now time.Time) bool {
	return c.hasExpiry && !c.expiresAt.After(now)
}

// String implements fmt.Stringer and always redacts.
func (c Credential) String() string { return redacted }

// GoString implements fmt.GoStringer and always redacts.
func (c Credential) GoString() string { return redacted }

// MarshalJSON emits the redaction marker instead of the token.
func (c Credential) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// Format implements fmt.Formatter and always redacts.
func (c Credential) Format(f fmt.State, verb rune) {
	_, _ = f.Write([]byte(redacted))
}

const redacted = "[redacted]"

var credentialReveals atomic.Int64

// CredentialReveals reports how many times a token was revealed in this
// process. It exists for the privacy tests and nothing else.
func CredentialReveals() int64 { return credentialReveals.Load() }

// Source is one credential source for one instance. Ref and Profile are
// non-secret text a UI may display to confirm what a consent would authorize;
// they are available without reading the credential.
//
// Read is called only after a matching consent has been loaded, and only from
// Refresh. A nil Read means the source cannot be read.
type Source struct {
	Instance string
	Label    string
	Kind     SourceKind
	// Ref is the exact, non-secret source reference: the credentials file path
	// or the keychain service and account.
	Ref string
	// Profile is the profile directory (file source) or the keychain account.
	Profile string
	Read    func(ctx context.Context) (Credential, error)
}

// ConfigDirFromRoots derives the Claude CLI config directory from an instance's
// project roots: a root named "projects" means its parent is the config
// directory. It reports false when no root looks like a project root, and also
// when the roots name more than one distinct config directory.
//
// The uniqueness rule is deliberate: several projects roots of one instance
// would mean several candidate credential origins, and picking the first would
// silently bind whichever account happened to be listed first. An ambiguous or
// unknown origin yields no origin at all, so a caller can never bind the wrong
// credential; it must not substitute a default instead.
func ConfigDirFromRoots(roots []string) (string, bool) {
	var found string
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		clean := filepath.Clean(r)
		if filepath.Base(clean) != "projects" {
			continue
		}
		dir := filepath.Dir(clean)
		if found == "" {
			found = dir
			continue
		}
		if dir != found {
			return "", false
		}
	}
	return found, found != ""
}

// DefaultConfigDir is the Claude CLI config directory for a home directory.
func DefaultConfigDir(home string) string { return filepath.Join(home, ".claude") }

// FileSource returns a source reading a Claude CLI credentials file. It reads
// nothing until Read is called.
func FileSource(instance, label, path string) Source {
	return Source{
		Instance: instance,
		Label:    label,
		Kind:     SourceFile,
		Ref:      path,
		Profile:  filepath.Dir(path),
		Read:     fileReader(path),
	}
}

func fileReader(path string) func(context.Context) (Credential, error) {
	return func(ctx context.Context) (Credential, error) {
		if err := ctx.Err(); err != nil {
			return Credential{}, errReason(ReasonCanceled)
		}
		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				return Credential{}, errReason(ReasonSourceMissing)
			}
			return Credential{}, errReason(ReasonSourceUnreadable)
		}
		defer f.Close()
		raw, err := io.ReadAll(io.LimitReader(f, maxCredentialBytes+1))
		if err != nil || len(raw) > maxCredentialBytes {
			return Credential{}, errReason(ReasonSourceUnreadable)
		}
		return decodeCredential(raw)
	}
}

// KeychainRunner performs one keychain lookup. Its output is bounded by the
// caller.
type KeychainRunner func(ctx context.Context, service, account string) ([]byte, error)

// KeychainSource returns a source reading the macOS keychain entry the Claude
// CLI wrote. run is injected so a test never touches a real keychain; a nil run
// makes the source permanently unsupported.
func KeychainSource(instance, label, service, account string, run KeychainRunner) Source {
	ref := service
	if account != "" {
		ref = service + ":" + account
	}
	src := Source{Instance: instance, Label: label, Kind: SourceKeychain, Ref: ref, Profile: account}
	if run == nil {
		src.Read = func(context.Context) (Credential, error) {
			return Credential{}, errReason(ReasonSourceUnsupported)
		}
		return src
	}
	src.Read = func(ctx context.Context) (Credential, error) {
		if err := ctx.Err(); err != nil {
			return Credential{}, errReason(ReasonCanceled)
		}
		raw, err := run(ctx, service, account)
		if err != nil {
			return Credential{}, errReason(ReasonSourceMissing)
		}
		if len(raw) > maxCredentialBytes {
			return Credential{}, errReason(ReasonSourceUnreadable)
		}
		return decodeCredential(raw)
	}
	return src
}

// credentialFile is the JSON envelope the Claude CLI writes. The refresh token
// is deliberately not decoded: this package never refreshes and never rewrites
// a credential.
type credentialFile struct {
	ClaudeAiOauth *struct {
		AccessToken string `json:"accessToken"`
		ExpiresAt   int64  `json:"expiresAt"` // Unix milliseconds
	} `json:"claudeAiOauth"`
}

func decodeCredential(raw []byte) (Credential, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return Credential{}, errReason(ReasonTokenMissing)
	}
	var f credentialFile
	if err := json.Unmarshal([]byte(trimmed), &f); err != nil {
		// The keychain entry is the same JSON document as the file; a bare
		// token is accepted only when it is a single, bounded token.
		if isBareToken(trimmed) {
			return Credential{token: trimmed}, nil
		}
		return Credential{}, errReason(ReasonSourceUnreadable)
	}
	if f.ClaudeAiOauth == nil || strings.TrimSpace(f.ClaudeAiOauth.AccessToken) == "" {
		return Credential{}, errReason(ReasonTokenMissing)
	}
	cred := Credential{token: strings.TrimSpace(f.ClaudeAiOauth.AccessToken)}
	if ms := f.ClaudeAiOauth.ExpiresAt; ms > 0 {
		t := time.UnixMilli(ms).UTC()
		if t.Year() >= 2000 && t.Year() < 2100 {
			cred.expiresAt, cred.hasExpiry = t, true
		}
	}
	return cred, nil
}

func isBareToken(s string) bool {
	if s == "" || len(s) > 4096 {
		return false
	}
	return !strings.ContainsAny(s, " \t\r\n{}\"")
}
