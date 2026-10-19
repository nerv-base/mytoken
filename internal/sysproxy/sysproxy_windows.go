//go:build windows

package sysproxy

import (
	"strconv"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// internetSettingsKey is where Windows stores the per-user WinINET proxy
// configuration that the Settings app and most browsers write.
const internetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

// systemSettings reads ProxyEnable, ProxyServer and ProxyOverride from the
// current user's Internet Settings key. Missing or unreadable values mean a
// direct connection.
func systemSettings() settings {
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		return settings{}
	}
	defer k.Close()

	enabled := false
	if v, _, err := k.GetIntegerValue("ProxyEnable"); err == nil {
		enabled = v != 0
	} else if s, _, err := k.GetStringValue("ProxyEnable"); err == nil {
		// Some policy tools write "1" as a string instead of a DWORD.
		enabled = parseWindowsBool(s)
	}
	server, _, _ := k.GetStringValue("ProxyServer")
	override, _, _ := k.GetStringValue("ProxyOverride")
	return parseWindowsProxy(enabled, server, override)
}

func parseWindowsBool(s string) bool {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "1", "true", "yes":
		return true
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n != 0
	}
	return false
}
