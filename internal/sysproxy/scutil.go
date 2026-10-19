package sysproxy

import (
	"net"
	"strconv"
	"strings"
)

// parseScutilProxy parses the dictionary printed by `scutil --proxy`, for
// example:
//
//	<dictionary> {
//	  ExceptionsList : <array> {
//	    0 : 127.0.0.1
//	    1 : *.local
//	    2 : <local>
//	  }
//	  HTTPEnable : 1
//	  HTTPPort : 7897
//	  HTTPProxy : 127.0.0.1
//	  HTTPSEnable : 1
//	  HTTPSPort : 7897
//	  HTTPSProxy : 127.0.0.1
//	  SOCKSEnable : 0
//	}
//
// It is deliberately tolerant: unknown keys, missing ports and an absent
// dictionary are all accepted, and a disabled protocol contributes no address.
func parseScutilProxy(out string) settings {
	values := make(map[string]string)
	var exceptions []string
	inExceptions := false

	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if inExceptions {
			if line == "}" {
				inExceptions = false
				continue
			}
			key, value, ok := splitScutilEntry(line)
			if ok && isDigits(key) && value != "" {
				exceptions = append(exceptions, value)
			}
			continue
		}
		if strings.HasPrefix(line, "ExceptionsList") {
			if strings.Contains(line, "<array>") {
				inExceptions = true
			}
			continue
		}
		key, value, ok := splitScutilEntry(line)
		if !ok || strings.HasPrefix(value, "<") || strings.HasSuffix(value, "{") {
			continue
		}
		values[key] = value
	}

	enabled := func(key string) bool {
		switch strings.ToLower(values[key]) {
		case "1", "true", "yes":
			return true
		}
		return false
	}
	address := func(hostKey, portKey string) string {
		host := strings.Trim(values[hostKey], "[]")
		if host == "" {
			return ""
		}
		if port := values[portKey]; port != "" {
			return net.JoinHostPort(host, port)
		}
		return host
	}

	s := settings{
		Exceptions:             exceptions,
		ExcludeSimpleHostnames: enabled("ExcludeSimpleHostnames"),
	}
	if enabled("HTTPEnable") {
		s.HTTP = address("HTTPProxy", "HTTPPort")
	}
	if enabled("HTTPSEnable") {
		s.HTTPS = address("HTTPSProxy", "HTTPSPort")
	}
	if enabled("SOCKSEnable") {
		s.SOCKS = address("SOCKSProxy", "SOCKSPort")
	}
	return s
}

// splitScutilEntry splits a "key : value" line at its first colon. Splitting at
// the first colon keeps IPv6 values (::1) intact.
func splitScutilEntry(line string) (key, value string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:i])
	if key == "" {
		return "", "", false
	}
	value = unquoteScutil(strings.TrimSpace(line[i+1:]))
	return key, value, true
}

// unquoteScutil removes the quotes scutil adds around strings that need them.
func unquoteScutil(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
		return s[1 : len(s)-1]
	}
	return s
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
