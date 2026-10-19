package sysproxy

import (
	"reflect"
	"testing"
)

const scutilSample = `<dictionary> {
  ExceptionsList : <array> {
    0 : 127.0.0.1
    1 : 192.168.0.0/16
    2 : 10.0.0.0/8
    3 : 172.16.0.0/12
    4 : localhost
    5 : *.local
    6 : *.crashlytics.com
    7 : <local>
  }
  FTPPassive : 1
  HTTPEnable : 1
  HTTPPort : 7897
  HTTPProxy : 127.0.0.1
  HTTPSEnable : 1
  HTTPSPort : 7897
  HTTPSProxy : 127.0.0.1
  ProxyAutoConfigEnable : 0
  ProxyAutoConfigURLString : http://example.invalid/proxy.pac
  SOCKSEnable : 1
  SOCKSPort : 7897
  SOCKSProxy : 127.0.0.1
}`

func TestParseScutilProxy(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want settings
	}{
		{
			name: "clash style: all protocols enabled",
			in:   scutilSample,
			want: settings{
				HTTP:  "127.0.0.1:7897",
				HTTPS: "127.0.0.1:7897",
				SOCKS: "127.0.0.1:7897",
				Exceptions: []string{
					"127.0.0.1",
					"192.168.0.0/16",
					"10.0.0.0/8",
					"172.16.0.0/12",
					"localhost",
					"*.local",
					"*.crashlytics.com",
					"<local>",
				},
			},
		},
		{
			name: "disabled protocols contribute nothing",
			in: `<dictionary> {
  HTTPEnable : 0
  HTTPPort : 7897
  HTTPProxy : 127.0.0.1
  HTTPSEnable : 0
  HTTPSPort : 7897
  HTTPSProxy : 127.0.0.1
  SOCKSEnable : 0
  SOCKSPort : 1080
  SOCKSProxy : 127.0.0.1
}`,
			want: settings{},
		},
		{
			name: "socks only",
			in: `<dictionary> {
  HTTPEnable : 0
  HTTPSEnable : 0
  SOCKSEnable : 1
  SOCKSPort : 1080
  SOCKSProxy : 127.0.0.1
}`,
			want: settings{SOCKS: "127.0.0.1:1080"},
		},
		{
			name: "exclude simple hostnames flag",
			in: `<dictionary> {
  ExcludeSimpleHostnames : 1
  HTTPSEnable : 1
  HTTPSPort : 8443
  HTTPSProxy : proxy.example
}`,
			want: settings{
				HTTPS:                  "proxy.example:8443",
				ExcludeSimpleHostnames: true,
			},
		},
		{
			name: "quoted values and IPv6",
			in: `<dictionary> {
  HTTPEnable : 1
  HTTPPort : 8080
  HTTPProxy : "::1"
  HTTPSEnable : true
  HTTPSProxy : "proxy.example"
}`,
			want: settings{
				HTTP:  "[::1]:8080",
				HTTPS: "proxy.example",
			},
		},
		{
			name: "missing port leaves the default",
			in: `<dictionary> {
  HTTPEnable : 1
  HTTPProxy : 192.0.2.10
}`,
			want: settings{HTTP: "192.0.2.10"},
		},
		{
			name: "empty and malformed output",
			in:   "not a dictionary\n\n",
			want: settings{},
		},
		{
			name: "empty dictionary",
			in:   "<dictionary> {\n}\n",
			want: settings{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseScutilProxy(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseScutilProxy() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestMatchException(t *testing.T) {
	tests := []struct {
		name  string
		entry string
		host  string
		want  bool
	}{
		{"glob local", "*.local", "printer.local", true},
		{"glob local does not match elsewhere", "*.local", "example.com", false},
		{"glob middle", "*crashlytics*", "api.crashlytics.com", true},
		{"shorthand cidr", "169.254/16", "169.254.10.20", true},
		{"shorthand cidr outside", "169.254/16", "169.253.1.1", false},
		{"full cidr", "10.0.0.0/8", "10.1.2.3", true},
		{"full cidr outside", "10.0.0.0/8", "11.1.2.3", false},
		{"ipv6 cidr", "2001:db8::/32", "2001:db8::1", true},
		{"plain ip", "192.168.1.5", "192.168.1.5", true},
		{"plain ip different", "192.168.1.5", "192.168.1.6", false},
		{"exact domain", "example.com", "example.com", true},
		{"subdomain", "example.com", "api.example.com", true},
		{"not a suffix match", "example.com", "notexample.com", false},
		{"leading dot", ".example.com", "api.example.com", true},
		{"local marker simple host", "<local>", "printer", true},
		{"local marker mdns", "<local>", "printer.local", true},
		{"local marker dotted domain", "<local>", "printer.lan", false},
		{"star matches all", "*", "anything.example", true},
		{"case insensitive wildcard", "*.Example.COM", "API.example.com", true},
		{"case insensitive domain", "Example.COM", "example.com", true},
		{"empty entry", "", "example.com", false},
		{"localhost", "localhost", "localhost", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchException(tt.entry, tt.host); got != tt.want {
				t.Fatalf("matchException(%q, %q) = %v, want %v", tt.entry, tt.host, got, tt.want)
			}
		})
	}
}
