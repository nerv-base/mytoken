package sysproxy

import "strings"

// parseProxyServer parses the ProxyServer registry value. Windows accepts
// either one address for every protocol ("host:port") or a semicolon separated
// per-protocol list ("http=host:port;https=host:port;socks=host:port").
func parseProxyServer(value string) (httpAddr, httpsAddr, socksAddr string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", ""
	}
	if !strings.Contains(value, "=") {
		addr := normalizeProxyAddr(value)
		return addr, addr, ""
	}
	for _, part := range strings.Split(value, ";") {
		key, val, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = normalizeProxyAddr(val)
		if val == "" {
			continue
		}
		switch key {
		case "http":
			httpAddr = val
		case "https":
			httpsAddr = val
		case "socks", "socks4", "socks5":
			socksAddr = val
		}
	}
	return httpAddr, httpsAddr, socksAddr
}

// parseProxyOverride parses the ProxyOverride registry value, which is a
// semicolon separated list of hosts, domains, wildcards, CIDR blocks and
// <local>. Empty entries are dropped; matching happens in matchException.
func parseProxyOverride(value string) []string {
	var entries []string
	for _, part := range strings.Split(value, ";") {
		if part = strings.TrimSpace(part); part != "" {
			entries = append(entries, part)
		}
	}
	return entries
}

// parseWindowsProxy combines the three Internet Settings values into the
// platform-neutral settings. ProxyEnable is the master switch.
func parseWindowsProxy(enabled bool, server, override string) settings {
	if !enabled {
		return settings{}
	}
	httpAddr, httpsAddr, socksAddr := parseProxyServer(server)
	return settings{
		HTTP:       httpAddr,
		HTTPS:      httpsAddr,
		SOCKS:      socksAddr,
		Exceptions: parseProxyOverride(override),
	}
}
