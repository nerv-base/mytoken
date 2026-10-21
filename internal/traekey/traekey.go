// Package traekey recovers the key Trae uses to encrypt its local agent
// database.
//
// The key is 32 random bytes generated per installation, and Trae keeps no copy
// of it anywhere a reader could find: not in its bundle, its settings, the
// macOS keychain or the database files. It exists only in the memory of the
// process that opened the database, so reading it means looking at that process
// once. Capture does that on macOS; elsewhere it reports ErrUnsupported and the
// key has to be pasted from a tool that already does this.
//
// Nothing here modifies the installed app: Capture works on a copy.
package traekey

import (
	_ "embed"
	"errors"
	"fmt"
	"strings"
)

// lldbScript is the debugger side of the capture: it walks process memory,
// collects the hex runs a key can hide in, and proves each candidate with an
// HMAC over page 1 of the database. It is written to a temporary file and
// imported into lldb, so nothing has to be installed beside the binary.
//
//go:embed scan_lldb.py
var lldbScript []byte

// Errors a capture can report.
var (
	// ErrUnsupported means this platform has no capture implementation.
	ErrUnsupported = errors.New("automatic key capture is only implemented on macOS")
	// ErrNoApp means no Trae application bundle was found.
	ErrNoApp = errors.New("no Trae application found")
	// ErrAppRunning means Trae is open, so the copy cannot start.
	ErrAppRunning = errors.New("Trae is running; quit it and try again")
	// ErrNoProcess means the copy started but never loaded its agent library.
	ErrNoProcess = errors.New("Trae started but its agent did not load")
	// ErrNoKey means the agent is running but no verified key was in its memory.
	ErrNoKey = errors.New("no key found in Trae's memory")
	// ErrPermission means the operating system refused access to the tool's
	// memory, which is what a debugger restriction looks like.
	ErrPermission = errors.New("not allowed to read Trae's memory")
)

// Steps a capture reports. They are stable tokens rather than sentences so a UI
// can translate them.
const (
	StepCopy   = "copy"
	StepSign   = "sign"
	StepLaunch = "launch"
	StepScan   = "scan"
	StepStop   = "stop"
)

// Options are the inputs of a capture.
type Options struct {
	// App is the application bundle to copy. Empty picks the first Trae bundle
	// that exists.
	App string
	// DB is the database the recovered key must open. Required: the key is
	// proved against page 1 of this file, so a key for another database is
	// never accepted.
	DB string
	// Log reports progress as one Step token at a time. Optional.
	Log func(string)
}

func (o Options) log(line string) {
	if o.Log != nil {
		o.Log(line)
	}
}

// ValidateKey normalises a pasted key and rejects what cannot be one. It
// accepts the bare 64 hex characters the capture prints, and the
// x'<key><salt>' form other tools print.
func ValidateKey(s string) (string, error) {
	k := strings.ToLower(strings.TrimSpace(s))
	k = strings.TrimPrefix(k, "x'")
	k = strings.TrimSuffix(k, "'")
	k = strings.ReplaceAll(k, " ", "")
	if k == "" {
		return "", errors.New("empty key")
	}
	for _, r := range k {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return "", fmt.Errorf("key is not hexadecimal: %q", s)
		}
	}
	switch len(k) {
	case 64:
		return k, nil
	case 96:
		// key followed by the salt, as SQLCipher prints it.
		return k[:64], nil
	}
	return "", fmt.Errorf("key is %d hex characters, want 64", len(k))
}
