// Package sysproxy teaches Go's default HTTP transport to honour the proxy
// configured in the operating system.
//
// A GUI app started by Finder, the Dock or a login item inherits no
// HTTP_PROXY/HTTPS_PROXY/NO_PROXY variables, so http.ProxyFromEnvironment sees
// no proxy and every request goes direct. Install replaces the default
// transport's Proxy with Func, which keeps the environment when it is set and
// otherwise reads the system settings (macOS scutil, Windows Internet
// Settings; Linux has no system-wide proxy Go can read).
//
// The system lookup is cached for cacheTTL, so toggling the system proxy is
// picked up without restarting the app, and every platform implementation
// bounds its work (macOS kills scutil after two seconds) and falls back to a
// direct connection on any error.
package sysproxy

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cacheTTL is how long a system lookup is reused. Long enough that requests do
// not spawn scutil each time, short enough that toggling the system proxy
// takes effect without restarting the app.
const cacheTTL = 30 * time.Second

// proxyEnvKeys are the variables that make the environment authoritative.
// net/http reads both spellings on every platform, so both count as "set".
var proxyEnvKeys = []string{
	"HTTPS_PROXY", "https_proxy",
	"HTTP_PROXY", "http_proxy",
	"NO_PROXY", "no_proxy",
}

// settings is one platform's proxy configuration, with addresses in host:port
// form. An empty address means that protocol is disabled.
type settings struct {
	HTTP                   string
	HTTPS                  string
	SOCKS                  string
	Exceptions             []string
	ExcludeSimpleHostnames bool
}

var (
	// envProxyFunc is http.ProxyFromEnvironment, indirected so tests can
	// observe the env-first branch without relying on the process-wide
	// environment cache inside net/http.
	envProxyFunc = http.ProxyFromEnvironment
	// systemLookup is the platform implementation; tests replace it.
	systemLookup = systemSettings

	cacheMu sync.Mutex
	cache   settings
	cacheAt time.Time
	cacheOK bool
)

// Install points http.DefaultTransport's Proxy at Func. Call it once, before
// the program performs any network request.
func Install() {
	if tr, ok := http.DefaultTransport.(*http.Transport); ok && tr != nil {
		tr.Proxy = Func
		return
	}
	// A program that replaced DefaultTransport with something else still gets
	// a working transport rather than silently going direct.
	http.DefaultTransport = &http.Transport{Proxy: Func}
}

// Func is the proxy selector Install installs. It follows HTTP_PROXY,
// HTTPS_PROXY and NO_PROXY when any of them is set, and otherwise asks the
// operating system. It never proxies a loopback address and never returns an
// error: an unavailable system proxy means a direct connection.
//
// Code that builds its own transport instead of using http.DefaultTransport
// can assign Func to that transport's Proxy field.
func Func(req *http.Request) (*url.URL, error) {
	if req == nil || req.URL == nil {
		return nil, nil
	}
	if envProxiesSet() {
		return envProxyFunc(req)
	}
	return selectProxy(currentSettings(), req.URL.Scheme, req.URL.Hostname()), nil
}

// envProxiesSet reports whether the environment explicitly configures proxying
// (or its exception list). An empty variable counts as unset: launchd and
// systemd routinely export empty placeholders.
func envProxiesSet() bool {
	for _, key := range proxyEnvKeys {
		if os.Getenv(key) != "" {
			return true
		}
	}
	return false
}

// currentSettings returns the cached system configuration, looking it up again
// when the cache is older than cacheTTL. The lookup happens under the lock, so
// concurrent requests wait at most one platform timeout (2s on macOS).
func currentSettings() settings {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if cacheOK && time.Since(cacheAt) < cacheTTL {
		return cache
	}
	cache = systemLookup()
	cacheAt = time.Now()
	cacheOK = true
	return cache
}

// resetCache drops the cached lookup; tests use it between cases.
func resetCache() {
	cacheMu.Lock()
	cache = settings{}
	cacheAt = time.Time{}
	cacheOK = false
	cacheMu.Unlock()
}

// selectProxy chooses the proxy for one request. It is pure, so the platform
// independent rules (scheme preference, SOCKS fallback, bypass list, loopback)
// are testable on every OS.
func selectProxy(s settings, scheme, host string) *url.URL {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "" || isLoopbackHost(h) {
		return nil
	}
	scheme = strings.ToLower(scheme)
	if scheme != "http" && scheme != "https" {
		return nil
	}
	if s.bypass(h) {
		return nil
	}

	var addr, proxyScheme string
	switch scheme {
	case "http":
		addr, proxyScheme = s.HTTP, "http"
		if addr == "" {
			addr, proxyScheme = s.HTTPS, "http"
		}
	case "https":
		addr, proxyScheme = s.HTTPS, "http"
		if addr == "" {
			addr, proxyScheme = s.HTTP, "http"
		}
	}
	// A SOCKS proxy is only the fallback: when the system also has an HTTP(S)
	// proxy, that one understands both schemes and is preferred.
	if addr == "" && s.SOCKS != "" {
		addr, proxyScheme = s.SOCKS, "socks5"
	}
	if addr == "" {
		return nil
	}
	return newProxyURL(proxyScheme, addr)
}

// isLoopbackHost reports whether host names this machine. Loopback traffic
// must never go through a proxy, even one the user configured for everything
// else.
func isLoopbackHost(host string) bool {
	h := strings.Trim(host, "[]")
	if strings.EqualFold(h, "localhost") || strings.HasSuffix(strings.ToLower(h), ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// isSimpleHostname reports whether host has no domain part, which is what
// ExcludeSimpleHostnames and Windows' <local> exception mean.
func isSimpleHostname(host string) bool {
	return host != "" && !strings.Contains(host, ".") && net.ParseIP(host) == nil
}

// bypass applies the platform exception list to a lowercase hostname.
func (s settings) bypass(host string) bool {
	if host == "" {
		return false
	}
	if s.ExcludeSimpleHostnames && isSimpleHostname(host) {
		return true
	}
	for _, entry := range s.Exceptions {
		if matchException(entry, host) {
			return true
		}
	}
	return false
}

// matchException applies one exception entry. It accepts the forms macOS and
// Windows use: exact hosts, domains (with subdomains), glob wildcards, IP
// addresses, CIDR blocks including the shorthand 169.254/16, <local>, and *.
func matchException(entry, host string) bool {
	e := strings.ToLower(strings.TrimSpace(entry))
	e = strings.Trim(e, `"`)
	if e == "" {
		return false
	}
	switch e {
	case "<local>":
		return isSimpleHostname(host) || strings.HasSuffix(host, ".local")
	case "*":
		return true
	}
	if strings.Contains(e, "/") {
		n := parseCIDRLoose(e)
		ip := net.ParseIP(host)
		return n != nil && ip != nil && n.Contains(ip)
	}
	if ip := net.ParseIP(e); ip != nil {
		h := net.ParseIP(host)
		return h != nil && ip.Equal(h)
	}
	if strings.ContainsAny(e, "*?[") {
		ok, err := path.Match(e, host)
		return err == nil && ok
	}
	e = strings.TrimPrefix(e, ".")
	return host == e || strings.HasSuffix(host, "."+e)
}

// parseCIDRLoose parses a CIDR block, padding the address with zero octets when
// the platform omitted them (macOS prints "169.254/16", not "169.254.0.0/16").
func parseCIDRLoose(entry string) *net.IPNet {
	if _, n, err := net.ParseCIDR(entry); err == nil {
		return n
	}
	i := strings.IndexByte(entry, '/')
	if i <= 0 || strings.Contains(entry[:i], ":") {
		return nil
	}
	addr, bits := entry[:i], entry[i+1:]
	for strings.Count(addr, ".") < 3 {
		addr += ".0"
	}
	if _, n, err := net.ParseCIDR(addr + "/" + bits); err == nil {
		return n
	}
	return nil
}

// newProxyURL turns a host:port address into a proxy URL. A missing port is
// left off so the scheme's default applies.
func newProxyURL(scheme, addr string) *url.URL {
	host, port := splitHostPort(addr)
	if host == "" {
		return nil
	}
	u := &url.URL{Scheme: scheme}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else {
		u.Host = host
	}
	return u
}

// normalizeProxyAddr strips a scheme and a trailing slash from an address.
func normalizeProxyAddr(v string) string {
	v = strings.TrimSpace(v)
	for _, prefix := range []string{
		"http://", "https://",
		"socks5h://", "socks5://", "socks4://", "socks://",
	} {
		if len(v) >= len(prefix) && strings.EqualFold(v[:len(prefix)], prefix) {
			v = v[len(prefix):]
			break
		}
	}
	return strings.TrimSuffix(v, "/")
}

// splitHostPort splits host:port, a bare host, or a bracketed IPv6 address.
func splitHostPort(addr string) (host, port string) {
	addr = normalizeProxyAddr(addr)
	if addr == "" {
		return "", ""
	}
	if strings.HasPrefix(addr, "[") {
		if h, p, err := net.SplitHostPort(addr); err == nil {
			return h, p
		}
		return strings.Trim(addr, "[]"), ""
	}
	if h, p, err := net.SplitHostPort(addr); err == nil {
		return h, p
	}
	// A bare IPv6 address has more than one colon and no port.
	if strings.Count(addr, ":") > 1 {
		return addr, ""
	}
	if i := strings.LastIndexByte(addr, ':'); i > 0 {
		if _, err := strconv.Atoi(addr[i+1:]); err == nil {
			return addr[:i], addr[i+1:]
		}
	}
	return addr, ""
}
