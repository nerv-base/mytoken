// Package zcode reports the ZCode Start Plan quota.
//
// ZCode (the z.ai coding desktop app) stores its account tokens in
// ~/.zcode/v2/credentials.json, each value sealed as
//
//	enc:v1:<base64url nonce>.<base64url tag>.<base64url ciphertext>
//
// with AES-256-GCM under a key derived as sha256(secret), where secret is
// ZCODE_CREDENTIAL_SECRET when set and otherwise the literal string
//
//	zcode-credential-fallback:<platform>:<homedir>:<username>
//
// The plan itself is not the z.ai platform Coding Plan: this account's plan is
// served by the ZCode gateway, so the quota comes from
//
//	GET https://zcode.z.ai/api/v1/zcode-plan/billing/balance
//	authorization: Bearer <zcodejwttoken>
//	x-device-mid: <uuid>
//
// and data.balances[] entries are the plan's windows (a daily token grant per
// model). The access token under oauth:zai:access_token is rejected by that
// endpoint, so only the zcodejwttoken is used.
package zcode

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

const (
	// dataRootName is the per-user ZCode data root, relative to home.
	dataRootName = ".zcode"
	// configDirName is the versioned config directory under the data root.
	configDirName = "v2"
	// credentialFileName holds the sealed account tokens.
	credentialFileName = "credentials.json"
	// tokenKey is the entry the balance endpoint accepts.
	tokenKey = "zcodejwttoken"
	// cipherPrefix marks a sealed value.
	cipherPrefix = "enc:v1:"
	// secretEnv overrides the derived at-rest secret.
	secretEnv = "ZCODE_CREDENTIAL_SECRET"
	// deviceMidFile holds the install's device id.
	deviceMidFile = "onboarding-record.json"
	// telemetryFile is the fallback source of the device id.
	telemetryFile = "telemetry-state.json"
	// zeroDeviceMid is sent when no stored device id can be read: the gateway
	// only requires a non-empty value.
	zeroDeviceMid = "00000000-0000-0000-0000-000000000000"
)

// credential is the sign-in this provider uses. It is never logged or
// persisted; only the quota it reads is.
type credential struct {
	token     string
	deviceMid string
}

// credentials reads the ZCode app's own sign-in. Nothing is written back.
func (p *Provider) credentials() (credential, error) {
	dir := p.configDir
	if dir == "" {
		dir = defaultConfigDir(p.home, p.getenv)
	}
	raw, err := os.ReadFile(filepath.Join(dir, credentialFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return credential{}, plan.NewError(plan.ReasonSourceMissing, err)
		}
		return credential{}, plan.NewError(plan.ReasonSourceUnreadable, err)
	}
	var doc map[string]string
	if err := json.Unmarshal(raw, &doc); err != nil {
		return credential{}, plan.NewError(plan.ReasonMalformed, err)
	}
	sealed := strings.TrimSpace(doc[tokenKey])
	if sealed == "" {
		return credential{}, plan.NewError(plan.ReasonNoCredentials, nil)
	}
	token, err := p.open(sealed)
	if err != nil {
		return credential{}, plan.NewError(plan.ReasonNoCredentials, err)
	}
	if strings.TrimSpace(token) == "" {
		return credential{}, plan.NewError(plan.ReasonNoCredentials, nil)
	}
	return credential{token: token, deviceMid: readDeviceMid(dir)}, nil
}

// open unseals one enc:v1 value, trying each plausible at-rest secret.
func (p *Provider) open(sealed string) (string, error) {
	body, ok := strings.CutPrefix(sealed, cipherPrefix)
	if !ok {
		return "", errors.New("zcode: value is not enc:v1")
	}
	parts := strings.Split(body, ".")
	if len(parts) != 3 {
		return "", errors.New("zcode: ciphertext format is invalid")
	}
	nonce, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[0], "="))
	if err != nil || len(nonce) != nonceBytes {
		return "", errors.New("zcode: nonce length is invalid")
	}
	tag, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil || len(tag) != tagBytes {
		return "", errors.New("zcode: auth tag length is invalid")
	}
	text, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[2], "="))
	if err != nil {
		return "", errors.New("zcode: ciphertext format is invalid")
	}
	var last error
	for _, secret := range candidateSecrets(p.home, p.goos, p.getenv) {
		key := sha256.Sum256([]byte(secret))
		out, err := gcmOpen(key[:], nonce, text, tag)
		if err == nil {
			return string(out), nil
		}
		last = err
	}
	if last == nil {
		last = errors.New("zcode: no at-rest secret is available")
	}
	return "", last
}

const (
	nonceBytes = 12
	tagBytes   = 16
)

func gcmOpen(key, nonce, text, tag []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, append(append([]byte(nil), text...), tag...), nil)
}

// candidateSecrets returns the at-rest secrets to try, most likely first.
// The app derives the fallback from platform(), homedir() and the OS
// username; Windows reports "win32" and a bare username, so every plausible
// spelling of the username is offered.
func candidateSecrets(home, goos string, getenv func(string) string) []string {
	if v := strings.TrimSpace(getenv(secretEnv)); v != "" {
		return []string{v}
	}
	prefix := "zcode-credential-fallback:" + nodePlatform(goos) + ":" + home + ":"
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, prefix+name)
	}
	add(baseName(getenv("USERNAME")))
	add(baseName(getenv("USER")))
	if u, err := user.Current(); err == nil {
		add(baseName(u.Username))
	}
	add(baseName(home))
	return out
}

// baseName strips a domain or directory prefix from an account name.
func baseName(name string) string {
	if i := strings.LastIndexAny(name, `\/`); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimSpace(name)
}

// nodePlatform mirrors Node's process.platform, which the app's fallback
// secret embeds.
func nodePlatform(goos string) string {
	switch goos {
	case "windows":
		return "win32"
	case "":
		return "win32"
	default:
		return goos
	}
}

// defaultConfigDir returns the directory holding credentials.json.
func defaultConfigDir(home string, getenv func(string) string) string {
	for _, key := range []string{"ZCODE_DATA_BASE_DIR", "ZCODE_DESKTOP_HOME_DIR", "HOME", "USERPROFILE"} {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return filepath.Join(v, dataRootName, configDirName)
		}
	}
	if home == "" {
		return filepath.Join(dataRootName, configDirName)
	}
	return filepath.Join(home, dataRootName, configDirName)
}

// readDeviceMid returns the install's device id, which the balance endpoint
// requires as a header. It is read, never written.
func readDeviceMid(dir string) string {
	for _, name := range []string{deviceMidFile, telemetryFile} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var doc struct {
			DeviceMid string `json:"deviceMid"`
		}
		if json.Unmarshal(raw, &doc) != nil {
			continue
		}
		if v := strings.TrimSpace(doc.DeviceMid); v != "" {
			return v
		}
	}
	return ""
}
