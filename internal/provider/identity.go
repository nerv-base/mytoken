// Package provider defines display identities independent of pricing labels and
// billing Origin+KeyID. An unrecognized endpoint must never become a shared ID.
package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// NormalizeEndpoint returns a conservative, credential-free endpoint. It does
// not infer schemes, upstreams, equivalent API paths, or domain aliases. Query,
// userinfo, fragment and dot-segment URLs require explicit resolution elsewhere.
func NormalizeEndpoint(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if strings.ContainsAny(raw, "?#\\") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Opaque != "" || u.Host == "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.Contains(host, "%") {
		return "", false
	}
	local := strings.TrimSuffix(host, ".")
	if local == "localhost" || strings.HasSuffix(local, ".localhost") {
		return "", false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if ip.IsLoopback() || ip.IsUnspecified() {
			return "", false
		}
		host = ip.String()
	} else {
		// ASCII DNS only: do not guess IDNA or nonstandard numeric IP notation.
		labels := strings.Split(strings.TrimSuffix(host, "."), ".")
		last := labels[len(labels)-1]
		if strings.Trim(last, "0123456789") == "" {
			return "", false
		}
		if strings.HasPrefix(last, "0x") && strings.Trim(last[2:], "0123456789abcdef") == "" {
			return "", false
		}
		for _, label := range labels {
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return "", false
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return "", false
				}
			}
		}
	}
	port := u.Port()
	if port == "" && strings.HasSuffix(u.Host, ":") {
		return "", false
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", false
		}
		port = strconv.Itoa(n)
		if scheme == "https" && port == "443" || scheme == "http" && port == "80" {
			port = ""
		}
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	path := u.EscapedPath()
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return "", false
		}
	}
	// One conventional terminal slash is equivalent; repeated slashes may be
	// meaningful to a router and must not collapse to a different tenant path.
	if !strings.HasSuffix(path, "//") {
		path = strings.TrimSuffix(path, "/")
	}
	return scheme + "://" + host + path, true
}

// EndpointID is a stable versioned ID, not a billing account identifier. Keep
// labels and historical pricing providers separately; never use this to sum
// account balances or to deduplicate request events.
func EndpointID(raw string) (string, bool) {
	endpoint, ok := NormalizeEndpoint(raw)
	if !ok {
		return "", false
	}
	sum := sha256.Sum256([]byte("mytoken-provider-endpoint:v1:" + endpoint))
	return "endpoint:v1:" + hex.EncodeToString(sum[:]), true
}
