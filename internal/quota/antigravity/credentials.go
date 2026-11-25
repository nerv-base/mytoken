package antigravity

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nerv-base/mytoken/internal/quota/plan"
)

const (
	keychainService = "gemini"
	keychainAccount = "antigravity"
	// keychainPrefix marks the go-keyring encoding agy uses on macOS.
	keychainPrefix = "go-keyring-base64:"
)

// credential is one stored Antigravity sign-in. It is never logged.
type credential struct {
	token     string
	expiresAt time.Time
}

// tokenEnvelope covers both the agy Keychain JSON and T3 Code's
// antigravity-oauth-token file. Their token object is identical.
type tokenEnvelope struct {
	AuthMethod  string `json:"auth_method"`
	AccessToken string `json:"access_token"`
	Token       struct {
		AccessToken string `json:"access_token"`
		Expiry      string `json:"expiry"`
		TokenType   string `json:"token_type"`
	} `json:"token"`
}

// credentials returns the first usable stored token. Keychain wins over T3
// Code; an expired candidate is returned only when no unexpired one exists, so
// Fetch can report token_expired.
func (p *Provider) credentials(ctx context.Context) (credential, error) {
	var lastErr error
	var expired *credential
	consider := func(raw []byte) (credential, bool) {
		c, err := parseCredential(raw)
		if err != nil {
			lastErr = err
			return credential{}, false
		}
		if c.expiresAt.IsZero() || p.now().Before(c.expiresAt) {
			return c, true
		}
		if expired == nil {
			c := c
			expired = &c
		}
		return credential{}, false
	}

	if p.keychainValue != "" {
		if c, ok := consider([]byte(p.keychainValue)); ok {
			return c, nil
		}
	} else if p.goos == "darwin" {
		raw, err := p.keychainLookup(ctx, p.keychainService, p.keychainAccount)
		if err == nil {
			if c, ok := consider([]byte(raw)); ok {
				return c, nil
			}
		} else {
			lastErr = err
		}
	}

	for _, path := range p.t3TokenFiles() {
		raw, err := os.ReadFile(path)
		if err != nil {
			lastErr = err
			continue
		}
		if c, ok := consider(raw); ok {
			return c, nil
		}
	}

	if expired != nil {
		return *expired, nil
	}
	if lastErr != nil {
		return credential{}, plan.NewError(plan.ReasonSourceUnreadable, lastErr)
	}
	return credential{}, plan.NewError(plan.ReasonNoCredentials, nil)
}

// parseCredential decodes either the go-keyring wrapper or a plain JSON token
// file.
func parseCredential(raw []byte) (credential, error) {
	raw = bytes.TrimSpace(raw)
	if bytes.HasPrefix(raw, []byte(keychainPrefix)) {
		encoded := bytes.TrimSpace(raw[len(keychainPrefix):])
		decoded, err := base64.StdEncoding.DecodeString(string(encoded))
		if err != nil {
			decoded, err = base64.RawStdEncoding.DecodeString(string(encoded))
		}
		if err != nil {
			return credential{}, err
		}
		raw = decoded
	}
	var env tokenEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return credential{}, err
	}
	token := strings.TrimSpace(env.Token.AccessToken)
	if token == "" {
		token = strings.TrimSpace(env.AccessToken)
	}
	if token == "" {
		return credential{}, errors.New("antigravity: token has no access_token")
	}
	return credential{token: token, expiresAt: parseExpiry(env.Token.Expiry)}, nil
}

func parseExpiry(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// keychainPassword reads agy's login item. It is called only from
// credentials, never from Detect.
func keychainPassword(ctx context.Context, service, account string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "security", "find-generic-password", "-s", service, "-a", account, "-w")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	value := strings.TrimRight(string(out), "\r\n")
	if value == "" {
		return "", fmt.Errorf("antigravity: empty Keychain item")
	}
	return value, nil
}
