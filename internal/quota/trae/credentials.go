package trae

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

const (
	// authKey is the storage.json key of the account envelope.
	authKey = "iCubeAuthInfo://icube.cloudide"
	// deviceKeyPrefix prefixes the per-device envelopes; the suffix is the
	// device id (and, decoded, the device's P-256 key pair).
	deviceKeyPrefix = "iCubeAuthInfo://icube-dc:"
)

var deviceIDPattern = regexp.MustCompile(`^\d{8,20}$`)

// credential is the sign-in this provider uses. It is never logged or
// persisted; only the quota it reads is.
type credential struct {
	token     string
	refresh   string
	uid       string
	deviceID  string
	machineID string
	expiresAt time.Time
	host      string
	region    string
	source    string
}

// envelope is the decoded sign-in stored under authKey.
type envelope struct {
	Token            string          `json:"token"`
	RefreshToken     string          `json:"refreshToken"`
	UserID           flexString      `json:"userId"`
	ExpiredAt        json.RawMessage `json:"expiredAt"`
	RefreshExpiredAt json.RawMessage `json:"refreshExpiredAt"`
	Host             string          `json:"host"`
	UserRegion       flexRegion      `json:"userRegion"`
	Region           string          `json:"region"`
	API              string          `json:"api"`
}

// flexRegion accepts Trae's region as either the plain string older builds
// write ("CN") or the object the current CN client writes
// ({"region":"CN","_aiRegion":"CN"}). Declaring it a plain string makes the
// whole envelope fail to unmarshal, which silently drops the sign-in and with
// it the Trae quota card.
type flexRegion string

func (r *flexRegion) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*r = ""
		return nil
	}
	switch b[0] {
	case '"':
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*r = flexRegion(v)
		return nil
	case '{':
		var obj struct {
			Region   string `json:"region"`
			AIRegion string `json:"_aiRegion"`
		}
		if err := json.Unmarshal(b, &obj); err != nil {
			return err
		}
		*r = flexRegion(firstNonEmpty(obj.Region, obj.AIRegion))
		return nil
	default:
		*r = flexRegion(string(b))
		return nil
	}
}

// flexString accepts a JSON string or number as text.
type flexString string

func (s *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*s = ""
		return nil
	}
	if b[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*s = flexString(v)
		return nil
	}
	*s = flexString(string(b))
	return nil
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
		v, err = strconv.ParseInt(strings.TrimSuffix(s, ".0"), 10, 64)
		if err != nil {
			return err
		}
	}
	*n = flexInt64(v)
	return nil
}

// credentials reads the client's own sign-in first, then the magpie fallback.
func (p *Provider) credentials() (credential, error) {
	if c, ok := p.appCredentials(); ok {
		return c, nil
	}
	if c, ok := p.magpieCredentials(); ok {
		return c, nil
	}
	return credential{}, plan.NewError(plan.ReasonNoCredentials, nil)
}

// appCredentials reads storage.json without modifying it.
func (p *Provider) appCredentials() (credential, bool) {
	for _, path := range p.storagePaths {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var doc map[string]json.RawMessage
		if json.Unmarshal(b, &doc) != nil {
			continue
		}
		env, ok := decodeEnvelope(doc[authKey])
		if !ok || strings.TrimSpace(env.Token) == "" {
			continue
		}
		uid := string(env.UserID)
		return credential{
			token:     env.Token,
			refresh:   env.RefreshToken,
			uid:       uid,
			deviceID:  deviceID(doc, uid),
			machineID: "",
			expiresAt: parseTimeValue(env.ExpiredAt),
			host:      env.Host,
			region:    firstNonEmpty(string(env.UserRegion), env.Region),
			source:    "app",
		}, true
	}
	return credential{}, false
}

// decodeEnvelope parses a stored envelope. A value that is not a JSON object
// is run through the client codec first.
func decodeEnvelope(raw json.RawMessage) (envelope, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return envelope{}, false
	}
	var text string
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil {
			return envelope{}, false
		}
	} else {
		text = string(raw)
	}
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "{") {
		body, err := clientDecode(text)
		if err != nil {
			return envelope{}, false
		}
		text = string(body)
	}
	var env envelope
	if json.Unmarshal([]byte(text), &env) != nil {
		return envelope{}, false
	}
	return env, true
}

// deviceID returns this machine's Trae CN device id when the client is signed
// in as uid. It is the suffix of the single device envelope whose decoded
// userId matches the account.
func deviceID(doc map[string]json.RawMessage, uid string) string {
	if uid == "" {
		return ""
	}
	var ids []string
	for key, raw := range doc {
		if !strings.HasPrefix(key, deviceKeyPrefix) {
			continue
		}
		id := strings.TrimPrefix(key, deviceKeyPrefix)
		if !deviceIDPattern.MatchString(id) {
			continue
		}
		env, ok := decodeEnvelope(raw)
		if !ok || string(env.UserID) != uid {
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 1 {
		return ids[0]
	}
	return ""
}

// magpieCredentials reads magpie's own sign-in for this site, if any.
func (p *Provider) magpieCredentials() (credential, bool) {
	if p.magpiePath == "" || p.magpieKey == "" {
		return credential{}, false
	}
	b, err := os.ReadFile(p.magpiePath)
	if err != nil {
		return credential{}, false
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(b, &doc) != nil {
		return credential{}, false
	}
	raw, ok := doc[p.magpieKey]
	if !ok {
		return credential{}, false
	}
	var a struct {
		Access    string    `json:"access"`
		Refresh   string    `json:"refresh"`
		Expires   flexInt64 `json:"expires"`
		UID       string    `json:"uid"`
		AccountID string    `json:"accountId"`
		DeviceID  string    `json:"deviceId"`
		MachineID string    `json:"machineId"`
	}
	if json.Unmarshal(raw, &a) != nil || strings.TrimSpace(a.Access) == "" {
		return credential{}, false
	}
	uid := firstNonEmpty(a.UID, a.AccountID)
	return credential{
		token:     a.Access,
		refresh:   a.Refresh,
		uid:       uid,
		deviceID:  a.DeviceID,
		machineID: a.MachineID,
		expiresAt: unixValue(int64(a.Expires)),
		source:    "magpie",
	}, true
}

func defaultStoragePaths(global bool, goos, home string, getenv func(string) string) []string {
	names := []string{"Trae CN", "trae-cn"}
	if global {
		names = []string{"Trae", "trae"}
	}
	file := func(root, name string) string {
		return filepath.Join(root, name, "User", "globalStorage", "storage.json")
	}
	switch goos {
	case "darwin":
		return []string{file(filepath.Join(home, "Library", "Application Support"), names[0])}
	case "windows":
		var roots []string
		if v := getenv("APPDATA"); v != "" {
			roots = append(roots, v)
		}
		if home != "" {
			fallback := filepath.Join(home, "AppData", "Roaming")
			if len(roots) == 0 || roots[0] != fallback {
				roots = append(roots, fallback)
			}
		}
		var out []string
		for _, root := range roots {
			for _, name := range names {
				out = append(out, file(root, name))
			}
		}
		return out
	default:
		root := getenv("XDG_CONFIG_HOME")
		if root == "" {
			root = filepath.Join(home, ".config")
		}
		return []string{file(root, names[0]), file(root, names[1])}
	}
}

func defaultMagpiePath(home string, getenv func(string) string) string {
	root := getenv("XDG_CONFIG_HOME")
	if root == "" {
		root = filepath.Join(home, ".config")
	}
	return filepath.Join(root, "magpie", "plugin-auth.json")
}

// parseTimeValue reads an envelope timestamp: RFC 3339 text, epoch seconds or
// epoch milliseconds.
func parseTimeValue(raw json.RawMessage) time.Time {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}
	}
	if raw[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			return parseTimeString(text)
		}
		return time.Time{}
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return unixValue(n)
}

func parseTimeString(text string) time.Time {
	text = strings.TrimSpace(text)
	if text == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, text); err == nil {
			return t
		}
	}
	return time.Time{}
}

// unixValue interprets a numeric timestamp as seconds, milliseconds or
// microseconds, whichever magnitude fits.
func unixValue(n int64) time.Time {
	switch {
	case n <= 0:
		return time.Time{}
	case n >= 1e15:
		return time.UnixMicro(n)
	case n >= 1e12:
		return time.UnixMilli(n)
	default:
		return time.Unix(n, 0)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
