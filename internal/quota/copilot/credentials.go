package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

const (
	// credentialDir is the directory the legacy Copilot extensions write to.
	credentialDir = "github-copilot"
	// maxCredentialBytes bounds each credential file read.
	maxCredentialBytes = 64 << 10
)

// credentialFiles are the files GitHub Copilot wrote its sign-in token to,
// newest first.
var credentialFiles = []string{"apps.json", "hosts.json"}

// credentialPaths returns candidate credential files for the platform.
func credentialPaths(getenv func(string) string, home, goos string) []string {
	var dirs []string
	switch goos {
	case "windows":
		if dir := getenv("LOCALAPPDATA"); dir != "" {
			dirs = append(dirs, filepath.Join(dir, credentialDir))
		}
	default:
		if dir := getenv("XDG_CONFIG_HOME"); dir != "" {
			dirs = append(dirs, filepath.Join(dir, credentialDir))
		}
		if home != "" {
			dirs = append(dirs, filepath.Join(home, ".config", credentialDir))
		}
	}
	out := make([]string, 0, len(dirs)*len(credentialFiles))
	for _, dir := range dirs {
		for _, name := range credentialFiles {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out
}

// readOAuthToken returns the first GitHub OAuth token found on disk.
func readOAuthToken(ctx context.Context, paths []string) (string, error) {
	var unreadable error
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return "", plan.NewError(plan.ReasonCanceled, err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if unreadable == nil {
				unreadable = plan.NewError(plan.ReasonSourceUnreadable, err)
			}
			continue
		}
		if len(raw) > maxCredentialBytes {
			raw = raw[:maxCredentialBytes]
		}
		if token := tokenFromCredentials(raw); token != "" {
			return token, nil
		}
	}
	if unreadable != nil {
		return "", unreadable
	}
	return "", plan.NewError(plan.ReasonNoCredentials, nil)
}

// tokenFromCredentials extracts oauth_token for github.com from a Copilot
// credential document. Both extensions store a map keyed by host name.
func tokenFromCredentials(raw []byte) string {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		return ""
	}
	for host, entry := range document {
		if host != "github.com" && !strings.HasSuffix(host, ".github.com") {
			continue
		}
		var fields struct {
			OAuthToken string `json:"oauth_token"`
		}
		if err := json.Unmarshal(entry, &fields); err != nil {
			continue
		}
		if token := strings.TrimSpace(fields.OAuthToken); token != "" {
			return token
		}
	}
	return ""
}
