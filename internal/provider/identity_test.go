package provider

import (
	"strings"
	"testing"
)

func TestNormalizeEndpoint(t *testing.T) {
	for _, tt := range []struct{ raw, want string }{
		{" HTTPS://API.Example.COM:443/v1/ ", "https://api.example.com/v1"},
		{"http://API.Example.COM:80/", "http://api.example.com"},
		{"https://api.example.com:8443/tenant/A/", "https://api.example.com:8443/tenant/A"},
		{"https://api.example.com/tenant/B", "https://api.example.com/tenant/B"},
		{"https://api.example.com/v1", "https://api.example.com/v1"},
		{"https://api.example.com/v2", "https://api.example.com/v2"},
		{"https://[2001:db8::1]:443/v1", "https://[2001:db8::1]/v1"},
		{"https://api.example.com/a%2Fb", "https://api.example.com/a%2Fb"},
		{"https://api.example.com/a/b", "https://api.example.com/a/b"},
		{"https://api.example.com/tenant//", "https://api.example.com/tenant//"},
	} {
		t.Run(tt.raw, func(t *testing.T) {
			got, ok := NormalizeEndpoint(tt.raw)
			if !ok || got != tt.want {
				t.Fatalf("got %q,%v; want %q,true", got, ok, tt.want)
			}
			again, ok := NormalizeEndpoint(got)
			if !ok || again != got {
				t.Fatalf("not idempotent: %q -> %q", got, again)
			}
		})
	}
}

func TestNormalizeEndpointRejectsUncertainOrSensitiveAddresses(t *testing.T) {
	for _, raw := range []string{"", "example.com/v1", "//example.com", "ftp://example.com", "https:///v1", "https://user:secret@example.com/v1", "https://example.com/v1?api_key=secret", "https://example.com/v1?", "https://example.com/v1#secret", "https://example.com/v1#", "http://localhost:15721/v1", "http://127.0.0.2/v1", "http://[::1]/v1", "http://[::ffff:127.0.0.1]/v1", "http://0.0.0.0/v1", "http://[::]/v1", "http://proxy.localhost/v1", "http://localhost./v1", "https://example.com:0/v1", "https://example.com:65536/v1", "https://example.com:/v1", "https://example.com:word/v1", "https://example.com/a/../tenant", "https://example.com/a/./tenant", "https://example.com/%2e%2e/tenant", "https://example.com/%zz", "https://exa mple.com", "https://example.com\\tenant"} {
		t.Run(raw, func(t *testing.T) {
			if got, ok := NormalizeEndpoint(raw); ok || got != "" {
				t.Fatalf("accepted uncertain endpoint: %q", got)
			}
			if got, ok := EndpointID(raw); ok || got != "" {
				t.Fatal("invalid endpoint has identity")
			}
		})
	}
}

func TestAmbiguousNumericAndBracketedHosts(t *testing.T) {
	for _, raw := range []string{"http://127.1/v1", "http://2130706433/v1", "http://0177.0.0.1/v1", "http://0x7f000001/v1", "https://[example.com]/v1", "https://[192.0.2.1]/v1"} {
		if got, ok := NormalizeEndpoint(raw); ok {
			t.Errorf("ambiguous host accepted: %q", got)
		}
	}
}

func TestEndpointIdentity(t *testing.T) {
	a, ok := EndpointID("https://API.example.com:443/tenant/A/")
	if !ok || !strings.HasPrefix(a, "endpoint:v1:") || len(a) != len("endpoint:v1:")+64 {
		t.Fatalf("invalid stable identity %q", a)
	}
	b, ok := EndpointID("https://api.example.com/tenant/A")
	if !ok || a != b {
		t.Fatal("equivalent endpoint did not share identity")
	}
	for _, raw := range []string{"https://api.example.com/tenant/B", "https://api.example.com/tenant/a", "https://other.example.com/tenant/A", "http://api.example.com/tenant/A", "https://api.example.com:8443/tenant/A"} {
		c, ok := EndpointID(raw)
		if !ok || c == a {
			t.Fatalf("distinct endpoint merged: %s", raw)
		}
	}
}
