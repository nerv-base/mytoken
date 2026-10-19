package trae

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCodecRoundTrip(t *testing.T) {
	want := map[string]any{
		"token":        "abc.def.ghi",
		"refreshToken": "refresh",
		"userId":       "123456789012345",
		"expiredAt":    "2026-10-28T15:12:17Z",
	}
	encoded := encodeEnvelope(t, want)
	body, err := clientDecode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Token != "abc.def.ghi" || string(env.UserID) != "123456789012345" {
		t.Fatalf("decoded = %+v", env)
	}
	if got := parseTimeValue(env.ExpiredAt); !got.Equal(time.Date(2026, 10, 28, 15, 12, 17, 0, time.UTC)) {
		t.Fatalf("expiry = %v", got)
	}
}

func TestCodecRejectsTampering(t *testing.T) {
	encoded := encodeEnvelope(t, map[string]any{"token": "x"})
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xff
	if _, err := clientDecode(base64.StdEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("tampered value decoded")
	}
	if _, err := clientDecode("not-base64"); err == nil {
		t.Fatal("garbage decoded")
	}
	if _, err := clientDecode(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 200)))); err == nil {
		t.Fatal("unknown header decoded")
	}
}

func TestParseTimestampForms(t *testing.T) {
	cases := map[string]time.Time{
		`"2026-10-28T15:12:17Z"`: time.Date(2026, 10, 28, 15, 12, 17, 0, time.UTC),
		`1793462399`:             time.Date(2026, 10, 31, 15, 59, 59, 0, time.UTC),
		`1793462399000`:          time.Date(2026, 10, 31, 15, 59, 59, 0, time.UTC),
	}
	for in, want := range cases {
		if got := parseTimeValue([]byte(in)); !got.Equal(want) {
			t.Errorf("parseTimeValue(%s) = %v, want %v", in, got, want)
		}
	}
}
