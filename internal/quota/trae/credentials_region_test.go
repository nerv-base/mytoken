package trae

import (
	"encoding/json"
	"testing"
)

// TestDecodeEnvelopeRegionForms pins the two shapes Trae's storage.json has
// used for userRegion. Older builds wrote a plain string; the current CN client
// writes an object. Declaring the field a plain string made the whole envelope
// fail to unmarshal, so the sign-in was silently dropped and no Trae quota card
// ever appeared.
func TestDecodeEnvelopeRegionForms(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "region as object (current CN client)",
			raw:  `{"token":"t","userId":"1","userRegion":{"region":"CN","_aiRegion":"CN"}}`,
			want: "CN",
		},
		{
			name: "region as object, only _aiRegion set",
			raw:  `{"token":"t","userId":"1","userRegion":{"_aiRegion":"US"}}`,
			want: "US",
		},
		{
			name: "region as plain string (older builds)",
			raw:  `{"token":"t","userId":"1","userRegion":"CN"}`,
			want: "CN",
		},
		{
			name: "region absent",
			raw:  `{"token":"t","userId":"1"}`,
			want: "",
		},
		{
			name: "region null",
			raw:  `{"token":"t","userId":"1","userRegion":null}`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, ok := decodeEnvelope(json.RawMessage(tc.raw))
			if !ok {
				t.Fatalf("decodeEnvelope(%s) returned ok=false", tc.raw)
			}
			if env.Token != "t" {
				t.Fatalf("token = %q, want %q", env.Token, "t")
			}
			if got := string(env.UserRegion); got != tc.want {
				t.Fatalf("region = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDecodeEnvelopeRegionObjectKeepsOtherFields guards against the failure mode
// this bug had: one unexpected field must not blank out the whole credential.
func TestDecodeEnvelopeRegionObjectKeepsOtherFields(t *testing.T) {
	raw := `{"token":"tok-123","refreshToken":"ref-1","userId":357809231237740,` +
		`"expiredAt":"2026-10-28T15:15:58.407Z","host":"https://api.trae.cn",` +
		`"userRegion":{"region":"CN","_aiRegion":"CN"}}`
	env, ok := decodeEnvelope(json.RawMessage(raw))
	if !ok {
		t.Fatal("decodeEnvelope returned ok=false on the real-world payload shape")
	}
	if env.Token != "tok-123" || string(env.UserID) != "357809231237740" {
		t.Fatalf("token/userId lost: %q / %q", env.Token, string(env.UserID))
	}
	if env.Host != "https://api.trae.cn" {
		t.Fatalf("host = %q", env.Host)
	}
	if got := firstNonEmpty(string(env.UserRegion), env.Region); got != "CN" {
		t.Fatalf("resolved region = %q, want CN", got)
	}
}
