package qoder

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

const (
	keychainService = "Qoder CN App Safe Storage"
	keychainAccount = "Qoder CN App Key"
)

var (
	errUnexpectedFormat = errors.New("qoder: unexpected auth.v1.dat format")
	errDecrypt          = errors.New("qoder: auth.v1.dat could not be decrypted")
)

// authFile is the decrypted desktop sign-in.
type authFile struct {
	SchemaVersion int    `json:"schemaVersion"`
	Token         string `json:"token"`
	RefreshToken  string `json:"refreshToken"`
	ExpiresAt     string `json:"expiresAt"`
	RefreshUntil  string `json:"refreshTokenExpiresAt"`
	User          struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"user"`
}

// credential is the sign-in this provider uses. It is never logged.
type credential struct {
	token     string
	refresh   string
	uid       string
	name      string
	expiresAt time.Time
}

// credentials decrypts auth.v1.dat. On macOS the AES key comes from the login
// Keychain via `security`; that command is run only here, never in Detect.
func (p *Provider) credentials(ctx context.Context) (credential, error) {
	raw, err := os.ReadFile(p.authPath)
	if err != nil {
		return credential{}, plan.NewError(plan.ReasonNoCredentials, err)
	}
	password := p.password
	if password == "" {
		if p.goos != "darwin" {
			return credential{}, plan.NewError(plan.ReasonUnsupported, nil)
		}
		password, err = keychainPassword(ctx, p.keychainService, p.keychainAccount)
		if err != nil {
			return credential{}, plan.NewError(plan.ReasonSourceUnreadable, err)
		}
	}
	plain, err := decryptAuth(raw, password)
	if err != nil {
		return credential{}, plan.NewError(plan.ReasonSourceUnreadable, err)
	}
	var a authFile
	if err := json.Unmarshal(plain, &a); err != nil || strings.TrimSpace(a.Token) == "" {
		return credential{}, plan.NewError(plan.ReasonNoCredentials, err)
	}
	return credential{
		token:     a.Token,
		refresh:   a.RefreshToken,
		uid:       a.User.ID,
		name:      a.User.Name,
		expiresAt: parseRFC3339(a.ExpiresAt),
	}, nil
}

// decryptAuth undoes Chromium's macOS v10 safeStorage envelope: AES-128-CBC
// with a key derived by PBKDF2-HMAC-SHA1(password, "saltysalt", 1003, 16) and
// an IV of sixteen spaces.
func decryptAuth(raw []byte, password string) ([]byte, error) {
	if len(raw) <= 3 || string(raw[:3]) != "v10" {
		return nil, errUnexpectedFormat
	}
	key, err := pbkdf2.Key(sha1.New, password, []byte("saltysalt"), 1003, 16)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	body := raw[3:]
	if len(body) == 0 || len(body)%aes.BlockSize != 0 {
		return nil, errUnexpectedFormat
	}
	out := make([]byte, len(body))
	cipher.NewCBCDecrypter(block, []byte("                ")).CryptBlocks(out, body)
	out, err = pkcs7Unpad(out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func pkcs7Unpad(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, errDecrypt
	}
	n := int(b[len(b)-1])
	if n == 0 || n > aes.BlockSize || n > len(b) {
		return nil, errDecrypt
	}
	for _, c := range b[len(b)-n:] {
		if int(c) != n {
			return nil, errDecrypt
		}
	}
	return b[:len(b)-n], nil
}

// keychainPassword reads the item Qoder's safeStorage uses.
func keychainPassword(ctx context.Context, service, account string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "security", "find-generic-password", "-s", service, "-a", account, "-w")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	password := strings.TrimRight(string(out), "\r\n")
	if password == "" {
		return "", fmt.Errorf("qoder: empty Keychain item")
	}
	return password, nil
}

func parseRFC3339(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func defaultAuthPath(goos, home string, getenv func(string) string) string {
	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "com.qodercn.app.stable", "auth.v1.dat")
	case "windows":
		root := getenv("APPDATA")
		if root == "" {
			root = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(root, "Qoder CN", "auth.v1.dat")
	default:
		root := getenv("XDG_CONFIG_HOME")
		if root == "" {
			root = filepath.Join(home, ".config")
		}
		return filepath.Join(root, "Qoder CN", "auth.v1.dat")
	}
}
