// Package instance owns the multi-instance source configuration: the versioned
// sources.json file, its in-memory representation, and the single resolution
// step that turns a config plus the registered harness parsers into the list of
// dataset roots that may be scanned.
//
// # Scope and non-goals
//
// This package is a foundation only. It is not yet wired into discovery,
// ingestion, watching or reporting; nothing in the running collector calls it
// today. It never reads source data, never deletes source files, never reads
// credentials, never enumerates the user's home directory and never changes the
// process time zone or any global state.
//
// # Identity model
//
// An instance is identified by an opaque random ID (see NewID), never by its
// path, its contents or its label. Labels are display aliases only: two entries
// with the same label are still two distinct instances, and two byte-identical
// datasets on different paths are still two distinct instances, so they are
// never deduplicated. The reserved ID DefaultID ("default") is not a user
// instance; it is used only for the legacy/fallback instance synthesized in
// Resolve when the config does not name a given harness.
//
// # Kinds
//
// KindInstance is a dataset that may be scanned. KindBackupCopy is a stored,
// diagnosed copy that is deliberately excluded from scanning and counting; it
// exists so the user can record it without it affecting reports. Promoting a
// backup-copy to an instance is always an explicit call to Config.Promote, and
// a move (Config.Move) always retains the entry's ID.
//
// # Filesystem contract
//
// Only Save writes the config, and it writes only sources.json: a 0600 file
// inside the data directory, replaced atomically via a temporary file plus
// rename so a failure leaves the previous config byte-identical, followed by a
// parent-directory flush where the platform supports one. Update additionally
// creates the persistent LockFileName lock file and holds an OS lock on it for
// the duration of the read-modify-write; it never unlinks that file, because
// doing so would let another writer lock a fresh inode concurrently. Load may
// create nothing. Add, Move, Promote and Remove are pure config operations and
// never touch the filesystem; a config file may therefore describe an offline
// or archived root. Paths are verified only by Resolve, and only for entries
// that are actually enabled instances.
//
// Files that do not exist are not an error: Load returns an empty config so the
// caller can fall back to the injected default providers. A config that exists
// but is malformed, is a newer schema, or names an invalid explicit path is an
// error and must never silently fall back to a different dataset.
//
// Exported API (the surface downstream agents may rely on)
//
//	const FileName = "sources.json"
//	const DefaultID = "default"
//	const SchemaVersion = 1
//
//	type Kind string        // KindInstance, KindBackupCopy
//	type PathSource string  // PathUser, PathEnv, PathDefault
//	type Code string        // diagnostic reason codes, see the Code constants
//
//	type ConfigError struct { Code Code; Path, ID string; Harness model.Harness; Message string; Err error }
//	func (*ConfigError) Error() string
//	func (*ConfigError) Unwrap() error
//
//	type Entry struct { ID, Label string; Harness model.Harness; Roots []string; Kind Kind; Disabled bool }
//	type Config struct { Version int; Entries []Entry }
//	type AddRequest struct { ID, Label string; Harness model.Harness; Roots []string; Kind Kind; Disabled bool }
//
//	func NewID() (string, error)
//	func Load(dataDir string) (*Config, error)
//	func (*Config) Save(dataDir string) error
//	func (*Config) Validate() error
//	func (*Config) Add(req AddRequest) (Entry, error)
//	func (*Config) Move(id string, roots []string) (Entry, error)
//	func (*Config) Promote(id string) (Entry, error)
//	func (*Config) Remove(id string) error
//
//	type Fallback interface { Harness() model.Harness; Roots() []string }
//	type Provenanced interface { Fallback; RootProvenance() ([]harness.Root, error) }
//	type Resolved struct { ID, Label string; Harness model.Harness; Roots []string; Source PathSource; Kind Kind; Disabled bool }
//	func (Resolved) Scannable() bool
//	type Problem struct { Code Code; ID string; Harness model.Harness; Path, Message string; Fatal bool }
//	func Resolve(cfg *Config, fallbacks []Fallback) ([]Resolved, []Problem)
//	func DefaultFallbacks() []Fallback
//
// internal/harness is imported for the provenance vocabulary only. The harness
// root package imports nothing but internal/model, so importing it pulls in no
// parser package and creates no cycle; Fallback is still satisfied structurally
// by harness.Parser, so plain fakes keep working. A Fallback that does not
// implement Provenanced is accepted and its roots are treated as platform
// defaults.
//
// Downstream wiring is therefore a single call:
//
//	roots, problems := instance.Resolve(cfg, instance.DefaultFallbacks())
//
// and feed each Resolved whose Scannable() is true into harness.NewRooted.
// Resolve only decides which roots are eligible; it never constructs parsers,
// touches the registry or starts any collection.
package instance

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
	"unicode"

	"github.com/zzstar101/mytoken/internal/model"
)

const (
	// FileName is the versioned config file inside the data directory.
	FileName = "sources.json"
	// DefaultID is reserved for the legacy/fallback instance. It can never be
	// assigned to a user-created entry.
	DefaultID = "default"
	// SchemaVersion is the config layout this build reads and writes.
	SchemaVersion = 1
	// MaxIDLen bounds an instance ID so a hand-edited file cannot smuggle in
	// unbounded opaque data.
	MaxIDLen = 128
)

// Kind distinguishes a scanned dataset from a stored copy that is deliberately
// excluded from scanning and counting.
type Kind string

const (
	KindInstance   Kind = "instance"
	KindBackupCopy Kind = "backup-copy"
)

func (k Kind) valid() bool { return k == KindInstance || k == KindBackupCopy }

// PathSource records where a resolved root came from, so diagnostics can say
// why a dataset is being read without echoing any environment value.
type PathSource string

const (
	// PathUser is an explicit root from the config file.
	PathUser PathSource = "user"
	// PathEnv is a root a harness parser derived from an explicit environment
	// override (MYTOKEN_*_DIRS or a harness-specific directory variable). Such a
	// root is a deliberate choice, exactly like a configured one: when it is
	// missing or invalid resolution reports a fatal problem instead of falling
	// back to another dataset.
	PathEnv PathSource = "env"
	// PathDefault is a root injected by a harness parser's own built-in or
	// generic platform default (home directory, XDG or app-data location). A
	// missing default root is a non-fatal skip, because the harness may simply
	// not be installed on this machine.
	PathDefault PathSource = "default"
)

// Code is a stable diagnostic reason code. Codes are part of the contract; the
// human-readable Message next to them is not.
type Code string

const (
	CodeInvalidConfig      Code = "invalid-config"
	CodeUnsupportedVersion Code = "unsupported-version"
	CodeDuplicateID        Code = "duplicate-id"
	CodeReservedID         Code = "reserved-id"
	CodeUnknownHarness     Code = "unknown-harness"
	CodeInvalidKind        Code = "invalid-kind"
	CodeNoRoots            Code = "no-roots"
	CodeNotFound           Code = "not-found"
	CodeIOError            Code = "io-error"
	CodeInvalidRoot        Code = "invalid-root"
	CodeMissingDir         Code = "missing-dir"
	CodeNotDir             Code = "not-dir"
	CodePermission         Code = "permission"
	CodeDuplicateRoot      Code = "duplicate-root"
	CodeOverlap            Code = "overlap"
)

// ConfigError is the typed error returned by every entry point in this package.
// Code carries the machine-readable reason, the remaining fields add context,
// and Err preserves the underlying cause so callers can keep using errors.Is
// against fs errors or json syntax errors.
type ConfigError struct {
	Code    Code
	Path    string
	ID      string
	Harness model.Harness
	Message string
	Err     error
}

func (e *ConfigError) Error() string {
	var b strings.Builder
	b.WriteString("instance: ")
	b.WriteString(string(e.Code))
	if e.ID != "" {
		b.WriteString(" id=")
		b.WriteString(e.ID)
	}
	if e.Harness != "" {
		b.WriteString(" harness=")
		b.WriteString(string(e.Harness))
	}
	if e.Path != "" {
		b.WriteString(" path=")
		b.WriteString(e.Path)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap exposes the underlying filesystem or encoding error.
func (e *ConfigError) Unwrap() error { return e.Err }

// NewID returns a fresh opaque instance ID: the "inst_" prefix plus 128 random
// bits encoded as unpadded lowercase base32. The value is independent of any
// path, label or content, and survives a Move unchanged.
func NewID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", &ConfigError{Code: CodeIOError, Message: "generate instance id", Err: err}
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	return "inst_" + strings.ToLower(enc.EncodeToString(buf[:])), nil
}

// validateID enforces the ID rules that do not depend on the rest of the file.
func validateID(id, path string, h model.Harness) error {
	if id == DefaultID {
		return &ConfigError{
			Code: CodeReservedID, Path: path, ID: id, Harness: h,
			Message: `"` + DefaultID + `" is reserved for the legacy fallback instance`,
		}
	}
	if id == "" {
		return &ConfigError{Code: CodeInvalidConfig, Path: path, Harness: h, Message: "entry is missing an id"}
	}
	if len(id) > MaxIDLen {
		return &ConfigError{
			Code: CodeInvalidConfig, Path: path, ID: id, Harness: h,
			Message: fmt.Sprintf("id is longer than %d bytes", MaxIDLen),
		}
	}
	for _, r := range id {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return &ConfigError{
				Code: CodeInvalidConfig, Path: path, ID: id, Harness: h,
				Message: "id must not contain whitespace or control characters",
			}
		}
	}
	return nil
}

// copyStrings returns an independent copy of in so no caller, config and
// result ever alias the same backing array. A nil input stays nil.
func copyStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
