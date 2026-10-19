package sysproxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func clearProxyEnv(t *testing.T) {
	t.Helper()
	for _, key := range proxyEnvKeys {
		t.Setenv(key, "")
	}
}

// stubSystem replaces the platform lookup and returns a counter of calls.
func stubSystem(t *testing.T, s settings) *int {
	t.Helper()
	calls := 0
	old := systemLookup
	systemLookup = func() settings {
		calls++
		return s
	}
	resetCache()
	t.Cleanup(func() {
		systemLookup = old
		resetCache()
	})
	return &calls
}

func stubEnvFunc(t *testing.T, fn func(*http.Request) (*url.URL, error)) {
	t.Helper()
	old := envProxyFunc
	envProxyFunc = fn
	t.Cleanup(func() { envProxyFunc = old })
}

func request(t *testing.T, target string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodGet, target, nil)
}

func TestEnvProxiesSet(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		want  bool
	}{
		{name: "none"},
		{name: "https upper", key: "HTTPS_PROXY", value: "http://p:3128", want: true},
		{name: "https lower", key: "https_proxy", value: "http://p:3128", want: true},
		{name: "http upper", key: "HTTP_PROXY", value: "http://p:3128", want: true},
		{name: "http lower", key: "http_proxy", value: "http://p:3128", want: true},
		{name: "no proxy upper", key: "NO_PROXY", value: "example.com", want: true},
		{name: "no proxy lower", key: "no_proxy", value: "example.com", want: true},
		{name: "empty placeholder is unset", key: "HTTPS_PROXY", value: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearProxyEnv(t)
			if tt.key != "" {
				t.Setenv(tt.key, tt.value)
			}
			if got := envProxiesSet(); got != tt.want {
				t.Fatalf("envProxiesSet() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFuncHonoursEnvironmentFirst(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://env.example:3128")

	sentinel := &url.URL{Scheme: "http", Host: "env.example:3128"}
	envCalled := false
	stubEnvFunc(t, func(*http.Request) (*url.URL, error) {
		envCalled = true
		return sentinel, nil
	})
	lookups := stubSystem(t, settings{HTTPS: "127.0.0.1:7897"})

	got, err := Func(request(t, "https://github.com/"))
	if err != nil {
		t.Fatalf("Func() error = %v", err)
	}
	if !envCalled {
		t.Fatal("Func did not consult the environment proxy function")
	}
	if got != sentinel {
		t.Fatalf("Func() = %v, want the environment proxy %v", got, sentinel)
	}
	if *lookups != 0 {
		t.Fatalf("system lookup ran %d times with proxy env set, want 0", *lookups)
	}
}

func TestFuncNeverProxiesLoopback(t *testing.T) {
	clearProxyEnv(t)
	stubSystem(t, settings{
		HTTP:  "127.0.0.1:7897",
		HTTPS: "127.0.0.1:7897",
		SOCKS: "127.0.0.1:7897",
	})

	targets := []string{
		"http://127.0.0.1/",
		"http://127.0.0.1:8080/path",
		"http://127.5.5.5/",
		"http://localhost/",
		"http://api.localhost/",
		"http://[::1]/",
		"https://[::1]:8443/path",
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			got, err := Func(request(t, target))
			if err != nil {
				t.Fatalf("Func(%s) error = %v", target, err)
			}
			if got != nil {
				t.Fatalf("Func(%s) = %v, want no proxy for loopback", target, got)
			}
		})
	}
}

func TestSelectProxy(t *testing.T) {
	both := settings{HTTP: "127.0.0.1:7897", HTTPS: "127.0.0.1:7897"}
	tests := []struct {
		name   string
		s      settings
		scheme string
		host   string
		want   string
	}{
		{name: "https prefers the https proxy", s: both, scheme: "https", host: "github.com", want: "http://127.0.0.1:7897"},
		{name: "http prefers the http proxy", s: both, scheme: "http", host: "example.com", want: "http://127.0.0.1:7897"},
		{
			name:   "https falls back to the http proxy",
			s:      settings{HTTP: "proxy.example:8080"},
			scheme: "https",
			host:   "github.com",
			want:   "http://proxy.example:8080",
		},
		{
			name:   "http falls back to the https proxy",
			s:      settings{HTTPS: "proxy.example:8443"},
			scheme: "http",
			host:   "example.com",
			want:   "http://proxy.example:8443",
		},
		{
			name:   "socks only uses socks5",
			s:      settings{SOCKS: "127.0.0.1:1080"},
			scheme: "https",
			host:   "github.com",
			want:   "socks5://127.0.0.1:1080",
		},
		{
			name:   "http proxy wins over socks",
			s:      settings{HTTP: "proxy.example:8080", SOCKS: "127.0.0.1:1080"},
			scheme: "https",
			host:   "github.com",
			want:   "http://proxy.example:8080",
		},
		{
			name:   "bypass entry",
			s:      settings{HTTPS: "proxy.example:8443", Exceptions: []string{"*.internal"}},
			scheme: "https",
			host:   "api.internal",
			want:   "",
		},
		{
			name:   "exclude simple hostnames",
			s:      settings{HTTPS: "proxy.example:8443", ExcludeSimpleHostnames: true},
			scheme: "https",
			host:   "intranet",
			want:   "",
		},
		{
			name:   "exclude simple hostnames leaves domains alone",
			s:      settings{HTTPS: "proxy.example:8443", ExcludeSimpleHostnames: true},
			scheme: "https",
			host:   "intranet.example",
			want:   "http://proxy.example:8443",
		},
		{
			name:   "loopback",
			s:      both,
			scheme: "https",
			host:   "127.0.0.1",
			want:   "",
		},
		{
			name:   "no configuration",
			s:      settings{},
			scheme: "https",
			host:   "github.com",
			want:   "",
		},
		{
			name:   "non http scheme",
			s:      both,
			scheme: "ftp",
			host:   "github.com",
			want:   "",
		},
		{
			name:   "trailing dot is normalised",
			s:      settings{HTTPS: "proxy.example:8443", Exceptions: []string{"example.com"}},
			scheme: "https",
			host:   "api.example.com.",
			want:   "",
		},
		{
			name:   "ipv6 proxy",
			s:      settings{HTTPS: "[::1]:7897"},
			scheme: "https",
			host:   "github.com",
			want:   "http://[::1]:7897",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := selectProxy(tt.s, tt.scheme, tt.host)
			if tt.want == "" {
				if got != nil {
					t.Fatalf("selectProxy() = %v, want no proxy", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("selectProxy() = nil, want %s", tt.want)
			}
			if got.String() != tt.want {
				t.Fatalf("selectProxy() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestSystemLookupIsCached(t *testing.T) {
	clearProxyEnv(t)
	calls := stubSystem(t, settings{HTTPS: "127.0.0.1:7897"})

	for i := 0; i < 3; i++ {
		if got, _ := Func(request(t, "https://github.com/")); got == nil {
			t.Fatal("Func() returned no proxy while the system proxy is enabled")
		}
	}
	if *calls != 1 {
		t.Fatalf("system lookup ran %d times, want 1 (cached)", *calls)
	}

	// Age the cache past its TTL and the next request looks the system up
	// again, which is how a proxy toggled in System Settings is picked up.
	cacheMu.Lock()
	cacheAt = time.Now().Add(-cacheTTL - time.Second)
	cacheMu.Unlock()
	if got, _ := Func(request(t, "https://github.com/")); got == nil {
		t.Fatal("Func() returned no proxy after the cache expired")
	}
	if *calls != 2 {
		t.Fatalf("system lookup ran %d times after expiry, want 2", *calls)
	}
}

func TestInstallSetsDefaultTransportProxy(t *testing.T) {
	clearProxyEnv(t)
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })

	tr := &http.Transport{}
	http.DefaultTransport = tr
	Install()

	if tr.Proxy == nil {
		t.Fatal("Install() did not set Proxy on http.DefaultTransport")
	}
	// The installed selector must not proxy loopback.
	got, err := tr.Proxy(request(t, "http://127.0.0.1/"))
	if err != nil {
		t.Fatalf("installed Proxy() error = %v", err)
	}
	if got != nil {
		t.Fatalf("installed Proxy() = %v, want no proxy for loopback", got)
	}
}

func TestInstallReplacesForeignTransport(t *testing.T) {
	clearProxyEnv(t)
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })

	http.DefaultTransport = roundTripperFunc(nil)
	Install()

	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport is %T, want *http.Transport", http.DefaultTransport)
	}
	if tr.Proxy == nil {
		t.Fatal("Install() did not set Proxy on the replacement transport")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(*http.Request) (*http.Response, error) {
	if f == nil {
		return nil, http.ErrNotSupported
	}
	return f(nil)
}
