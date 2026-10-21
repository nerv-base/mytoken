package traekey

import (
	"bytes"
	"strings"
	"testing"
)

// fakeChecker stands in for the page-1 proof so the scanner's handling of memory
// chunks can be tested without a database.
type fakeChecker struct {
	want string
}

func (f fakeChecker) check(run []byte) string {
	if bytes.Contains(run, []byte(f.want)) {
		return f.want
	}
	return ""
}

// noise is filler with no hex runs in it, so runs stay where a test puts them.
func noise(n int) []byte { return bytes.Repeat([]byte{0x00, 0x01, 0xff, ' '}, n/4+1)[:n] }

func TestScannerFindsKeyInsideChunk(t *testing.T) {
	key := strings.Repeat("a1b2c3d4", 8)
	chunk := append(noise(500), []byte("x'"+key+"'")...)
	chunk = append(chunk, noise(500)...)
	s := &scanner{p: fakeChecker{want: key}}
	if !s.Feed(chunk) {
		t.Fatal("key inside one chunk was not found")
	}
	if s.key != key {
		t.Fatalf("key = %q, want %q", s.key, key)
	}
}

func TestScannerFindsKeyAcrossChunks(t *testing.T) {
	key := strings.Repeat("0123456789abcdef", 4)
	mem := append(noise(64), []byte(key)...)
	mem = append(mem, noise(64)...)
	// Split in the middle of the key: the run has to be carried over.
	for _, cut := range []int{1, 17, 32, 63} {
		s := &scanner{p: fakeChecker{want: key}}
		if !s.Feed(mem[:len(mem)-64-len(key)+cut]) {
			if !s.Feed(mem[len(mem)-64-len(key)+cut:]) {
				t.Fatalf("key split at %d was not found", cut)
			}
		}
		if s.key != key {
			t.Fatalf("key split at %d = %q, want %q", cut, s.key, key)
		}
	}
}

func TestScannerFindsKeyInLongRun(t *testing.T) {
	// A long hex run whose key only ends in the second chunk: the carried
	// prefix has to keep enough of it.
	run := strings.Repeat("deadbeef", 200)
	s := &scanner{p: fakeChecker{want: run[len(run)-keyHexLen:]}}
	half := len(run) / 2
	if !s.Feed([]byte(run[:half])) && !s.Feed([]byte(run[half:])) {
		t.Fatal("key at the end of a long run was not found")
	}
}

func TestScannerKeepsCarryBounded(t *testing.T) {
	s := &scanner{p: fakeChecker{}}
	s.Feed([]byte(strings.Repeat("a", 1<<16)))
	if len(s.carry) > carryLimit {
		t.Fatalf("carry = %d bytes, want at most %d", len(s.carry), carryLimit)
	}
}

func TestScannerIgnoresShortRuns(t *testing.T) {
	s := &scanner{p: fakeChecker{want: "abcd"}}
	if s.Feed([]byte("abcd")) {
		t.Fatal("a run shorter than a key was accepted")
	}
	if s.key != "" {
		t.Fatalf("key = %q, want none", s.key)
	}
}
