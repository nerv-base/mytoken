package traekey

import "testing"

func TestValidateKey(t *testing.T) {
	key := "3605f6691095a993f03d5009c918352ef5be31ae31e8f000212b81ff058da773"
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{in: key, want: key, ok: true},
		{in: "  " + key + "\n", want: key, ok: true},
		{in: "x'" + key + "'", want: key, ok: true},
		{in: "x'" + key + "fa8183e03e3f2442643c87a4e8d06419'", want: key, ok: true},
		{in: key[:62], ok: false},
		{in: key + "00", ok: false},
		{in: "zz" + key[2:], ok: false},
		{in: "", ok: false},
	}
	for _, c := range cases {
		got, err := ValidateKey(c.in)
		if c.ok != (err == nil) {
			t.Fatalf("ValidateKey(%q) error = %v, want ok=%v", c.in, err, c.ok)
		}
		if c.ok && got != c.want {
			t.Fatalf("ValidateKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
