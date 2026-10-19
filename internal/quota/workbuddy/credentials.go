package workbuddy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	cnAuthID = "workbuddy-desktop"
	aiAuthID = "workbuddy-desktop-ai"
)

// stored is the desktop app's plaintext sign-in file. WorkBuddy 5.6 and later
// may seal accessToken/refreshToken as a {$wbEncrypted: ...} object; such a
// value cannot be read here and is treated as signed out.
type stored struct {
	Account struct {
		UID          string `json:"uid"`
		Nickname     string `json:"nickname"`
		PhoneNumber  string `json:"phoneNumber"`
		EnterpriseID string `json:"enterpriseId"`
	} `json:"account"`
	Auth struct {
		AccessToken      json.RawMessage `json:"accessToken"`
		RefreshToken     json.RawMessage `json:"refreshToken"`
		ExpiresAt        flexInt64       `json:"expiresAt"`
		ExpiresIn        flexInt64       `json:"expiresIn"`
		RefreshExpiresAt flexInt64       `json:"refreshExpiresAt"`
		RefreshExpiresIn flexInt64       `json:"refreshExpiresIn"`
		Domain           string          `json:"domain"`
		TokenType        string          `json:"tokenType"`
	} `json:"auth"`
}

// credential is the sign-in this provider uses. It is never logged.
type credential struct {
	access       string
	refresh      string
	uid          string
	domain       string
	enterpriseID string
	expiresAt    time.Time
}

func (p *Provider) credentials() (credential, error) {
	s, err := readStored(p.authPath)
	if err != nil {
		return credential{}, errNoCredentials
	}
	access, ok := tokenString(s.Auth.AccessToken)
	if !ok || strings.TrimSpace(s.Account.UID) == "" {
		return credential{}, errNoCredentials
	}
	expires := time.Time{}
	switch {
	case s.Auth.ExpiresAt > 0:
		expires = time.UnixMilli(int64(s.Auth.ExpiresAt))
	case s.Auth.ExpiresIn > 0:
		expires = p.now().Add(time.Duration(s.Auth.ExpiresIn) * time.Second)
	}
	return credential{
		access:       access,
		refresh:      stringValue(s.Auth.RefreshToken),
		uid:          s.Account.UID,
		domain:       s.Auth.Domain,
		enterpriseID: strings.TrimSpace(s.Account.EnterpriseID),
		expiresAt:    expires,
	}, nil
}

func readStored(path string) (stored, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return stored{}, err
	}
	var s stored
	if err := json.Unmarshal(b, &s); err != nil {
		return stored{}, err
	}
	return s, nil
}

// tokenString accepts only a JSON string; an encrypted object is not a token.
func tokenString(raw json.RawMessage) (string, bool) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || strings.TrimSpace(s) == "" {
		return "", false
	}
	return s, true
}

func stringValue(raw json.RawMessage) string {
	s, _ := tokenString(raw)
	return s
}

func defaultAuthPath(ai bool, goos, home string, getenv func(string) string) string {
	var base string
	switch goos {
	case "darwin":
		base = filepath.Join(home, "Library", "Application Support", "CodeBuddyExtension")
	case "windows":
		base = getenv("LOCALAPPDATA")
		if base == "" {
			base = filepath.Join(home, "AppData", "Local", "CodeBuddyExtension")
		} else {
			base = filepath.Join(base, "CodeBuddyExtension")
		}
	default:
		base = filepath.Join(home, ".local", "share", "CodeBuddyExtension")
	}
	name := cnAuthID
	if ai {
		name = aiAuthID
	}
	return filepath.Join(base, "Data", "Public", "auth", name+".info")
}

// flexInt64 accepts a JSON number or numeric string.
type flexInt64 int64

func (n *flexInt64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return err
		}
		v = int64(f)
	}
	*n = flexInt64(v)
	return nil
}
