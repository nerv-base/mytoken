//go:build darwin

package sysproxy

import (
	"context"
	"os"
	"os/exec"
	"time"
)

// scutilTimeout bounds the system lookup. A wedged scutil must never hold a
// request for longer than this; on timeout the request goes direct.
const scutilTimeout = 2 * time.Second

// systemSettings reads the proxy dictionaries macOS keeps in the system
// configuration. scutil lives in /usr/sbin, which is on the minimal PATH a
// Finder launch starts with; the absolute path also protects against a
// user-supplied scutil earlier on PATH.
func systemSettings() settings {
	path := "/usr/sbin/scutil"
	if _, err := os.Stat(path); err != nil {
		path = "scutil"
	}
	ctx, cancel := context.WithTimeout(context.Background(), scutilTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--proxy")
	cmd.WaitDelay = 500 * time.Millisecond
	out, err := cmd.Output()
	if err != nil {
		return settings{}
	}
	return parseScutilProxy(string(out))
}
