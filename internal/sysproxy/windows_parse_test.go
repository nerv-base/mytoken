package sysproxy

import (
	"reflect"
	"testing"
)

func TestParseProxyServer(t *testing.T) {
	tests := []struct {
		name                string
		in                  string
		wantHTTP, wantHTTPS string
		wantSocks           string
	}{
		{
			name:      "single address applies to http and https",
			in:        "127.0.0.1:7897",
			wantHTTP:  "127.0.0.1:7897",
			wantHTTPS: "127.0.0.1:7897",
		},
		{
			name:      "scheme on a single address is stripped",
			in:        "http://proxy.example:3128",
			wantHTTP:  "proxy.example:3128",
			wantHTTPS: "proxy.example:3128",
		},
		{
			name:      "per protocol list",
			in:        "http=127.0.0.1:7897;https=127.0.0.1:7898;socks=127.0.0.1:7899",
			wantHTTP:  "127.0.0.1:7897",
			wantHTTPS: "127.0.0.1:7898",
			wantSocks: "127.0.0.1:7899",
		},
		{
			name:      "case, spaces and alternate socks names",
			in:        " HTTP = proxy.example:8080 ; SOCKS5 = socks.example:1080 ",
			wantHTTP:  "proxy.example:8080",
			wantSocks: "socks.example:1080",
		},
		{
			name:      "socks only",
			in:        "socks=127.0.0.1:1080",
			wantSocks: "127.0.0.1:1080",
		},
		{
			name:      "unknown protocols are ignored",
			in:        "http=p:80;ftp=p:21;https=p:443",
			wantHTTP:  "p:80",
			wantHTTPS: "p:443",
		},
		{
			name:      "empty values are dropped",
			in:        "http=;https=proxy.example:443",
			wantHTTPS: "proxy.example:443",
		},
		{
			name:      "trailing slash is dropped",
			in:        "proxy.example:8080/",
			wantHTTP:  "proxy.example:8080",
			wantHTTPS: "proxy.example:8080",
		},
		{
			name: "empty",
			in:   "",
		},
		{
			name: "whitespace only",
			in:   "   ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			httpAddr, httpsAddr, socksAddr := parseProxyServer(tt.in)
			if httpAddr != tt.wantHTTP || httpsAddr != tt.wantHTTPS || socksAddr != tt.wantSocks {
				t.Fatalf("parseProxyServer(%q) = (%q, %q, %q), want (%q, %q, %q)",
					tt.in, httpAddr, httpsAddr, socksAddr, tt.wantHTTP, tt.wantHTTPS, tt.wantSocks)
			}
		})
	}
}

func TestParseProxyOverride(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{
			name: "typical windows list",
			in:   `<local>;*.local;169.254/16`,
			want: []string{"<local>", "*.local", "169.254/16"},
		},
		{
			name: "spaces and empty entries",
			in:   ` *.example.com ;; 10.0.0.0/8 ; `,
			want: []string{"*.example.com", "10.0.0.0/8"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseProxyOverride(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseProxyOverride(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseWindowsProxy(t *testing.T) {
	tests := []struct {
		name     string
		enabled  bool
		server   string
		override string
		want     settings
	}{
		{
			name:     "enabled single server",
			enabled:  true,
			server:   "127.0.0.1:7897",
			override: "<local>;*.local",
			want: settings{
				HTTP:       "127.0.0.1:7897",
				HTTPS:      "127.0.0.1:7897",
				Exceptions: []string{"<local>", "*.local"},
			},
		},
		{
			name:     "enabled per protocol server",
			enabled:  true,
			server:   "http=p:80;https=p:443;socks=p:1080",
			override: "169.254/16",
			want: settings{
				HTTP:       "p:80",
				HTTPS:      "p:443",
				SOCKS:      "p:1080",
				Exceptions: []string{"169.254/16"},
			},
		},
		{
			name:     "disabled ignores every value",
			enabled:  false,
			server:   "127.0.0.1:7897",
			override: "<local>",
			want:     settings{},
		},
		{
			name:    "enabled but no server",
			enabled: true,
			want:    settings{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseWindowsProxy(tt.enabled, tt.server, tt.override)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseWindowsProxy(%v, %q, %q) = %#v, want %#v",
					tt.enabled, tt.server, tt.override, got, tt.want)
			}
		})
	}
}
