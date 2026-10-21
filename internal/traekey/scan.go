package traekey

// Finding the key in a process's memory.
//
// The key is 32 random bytes, and nothing about it is guessable: no passphrase,
// no machine identifier, no constant in the binary. Trae holds it as a
// hex string (that is the form it hands to `PRAGMA key`), so a capture reads the
// process's memory, keeps every run of hex digits, and tries the 64-character
// windows in each run against the database itself. Page 1's HMAC accepts exactly
// one of them, which is why the search needs no knowledge of how the key was
// made and cannot pick a wrong one.
//
// The same walk runs on every platform; only the way memory is read differs, so
// the platforms in capture_*.go supply bytes and this file does the rest.

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/zzstar101/mytoken/internal/harness/trae"
)

const (
	// keyHexLen is a raw 32-byte key written as hex, which is how the key
	// appears in memory.
	keyHexLen = 64
	// carryLimit is how much of a hex run that ends a chunk is kept for the next
	// one. A window that straddles the boundary starts at most keyHexLen-1
	// characters back, and a little more keeps long runs cheap to handle.
	carryLimit = 256
	// pageSize is the first page that has to be proved against.
	pageSize = 4096
)

// prover tries candidate keys against one database.
type prover struct {
	pageOne []byte
}

// newProver reads the first page of dbPath, which is the only part of a
// SQLCipher database needed to check a key.
func newProver(dbPath string) (*prover, error) {
	f, err := os.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	page := make([]byte, pageSize)
	if _, err := io.ReadFull(f, page); err != nil {
		return nil, fmt.Errorf("%s: %w", dbPath, err)
	}
	return &prover{pageOne: page}, nil
}

// check returns the key a hex run carries, or "" when it carries none.
func (p *prover) check(run []byte) string {
	for off := 0; off+keyHexLen <= len(run); off += 2 {
		var key [32]byte
		if _, err := hex.Decode(key[:], run[off:off+keyHexLen]); err != nil {
			continue
		}
		if trae.VerifyKey(p.pageOne, key[:]) == nil {
			return string(run[off : off+keyHexLen])
		}
	}
	return ""
}

// checker is what the scanner needs of a prover, so the boundary handling can be
// tested without a real database.
type checker interface {
	check(run []byte) string
}

// scanner holds the hex runs of a memory stream together across chunk
// boundaries and stops at the first run that yields a key.
type scanner struct {
	p     checker
	carry []byte
	key   string
}

// Feed takes the next chunk of memory and reports whether it completed a run
// holding the key. Successive chunks only have to be adjacent in address space
// in as much as a run does not straddle an unmapped hole.
func (s *scanner) Feed(chunk []byte) bool {
	buf := make([]byte, 0, len(s.carry)+len(chunk))
	buf = append(buf, s.carry...)
	buf = append(buf, chunk...)
	s.carry = nil
	start := 0
	for i := range buf {
		if isHex(buf[i]) {
			continue
		}
		if s.test(buf[start:i]) {
			return true
		}
		start = i + 1
	}
	run := buf[start:]
	if s.test(run) {
		return true
	}
	if len(run) > carryLimit {
		run = run[len(run)-carryLimit:]
	}
	s.carry = append([]byte(nil), run...)
	return false
}

// test proves one run and remembers the key when it holds it. Windows are tried
// from every other character because hex digits pair into bytes.
func (s *scanner) test(run []byte) bool {
	if len(run) < keyHexLen {
		return false
	}
	if key := s.p.check(run); key != "" {
		s.key = key
		return true
	}
	return false
}

// isHex reports whether b is a hex digit.
func isHex(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F'
}

// scanProcess walks the readable memory of one process and returns the key it
// found, or "" when the process does not hold it. read is handed every readable
// region in turn and returns the bytes it could read, which may be fewer than
// the region holds.
func scanProcess(ctx context.Context, p *prover, regions func(yield func([]byte) bool) error) (string, error) {
	s := &scanner{p: p}
	found := false
	err := regions(func(chunk []byte) bool {
		if found {
			return false
		}
		found = s.Feed(chunk)
		return !found
	})
	if err != nil && !found {
		return "", err
	}
	if !found {
		return "", nil
	}
	return s.key, nil
}

// checkEncrypted makes sure db is a SQLCipher database rather than a plaintext
// one, so no capture is started for a file that needs no key.
func checkEncrypted(db string) error {
	f, err := os.Open(db)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 16)
	if _, err := io.ReadFull(f, head); err != nil {
		return fmt.Errorf("%s: %w", db, err)
	}
	if bytes.HasPrefix(head, []byte("SQLite format 3\x00")) {
		return fmt.Errorf("%s is not encrypted, so it needs no key", db)
	}
	return nil
}

// run executes a tool and folds its output into the error it returns.
func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Run(); err != nil {
		return buf.String(), fmt.Errorf("%s: %v: %s", name, err, tail(buf.String()))
	}
	return buf.String(), nil
}

// tail keeps the end of a tool's output, which is where its error is.
func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		if i := strings.LastIndexByte(s, '\n'); i >= 0 && len(s)-i < 400 {
			return s[i+1:]
		}
		return s[:400] + "…"
	}
	return s
}
