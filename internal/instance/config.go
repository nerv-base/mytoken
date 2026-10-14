package instance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/zzstar101/mytoken/internal/model"
)

// Entry is one configured instance. ID is the identity; Label is a display
// alias only and is never used for matching or deduplication.
type Entry struct {
	ID       string        `json:"id"`
	Label    string        `json:"label,omitempty"`
	Harness  model.Harness `json:"harness"`
	Roots    []string      `json:"roots"`
	Kind     Kind          `json:"kind"`
	Disabled bool          `json:"disabled,omitempty"`
}

// Config is the whole sources.json document. Entries keep their file order,
// which is the order Resolve reports them in.
type Config struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

// AddRequest describes a new entry. An empty ID asks Add to generate an opaque
// one; an empty Kind means KindInstance.
type AddRequest struct {
	ID       string
	Label    string
	Harness  model.Harness
	Roots    []string
	Kind     Kind
	Disabled bool
}

// Load reads the config from dataDir. A missing file is not an error: it yields
// an empty schema-version config and creates nothing, which is what lets the
// caller fall back to the injected default roots. A file that exists but is
// malformed, has a missing/newer version, carries unknown fields, or fails
// validation returns a *ConfigError and never falls back.
func Load(dataDir string) (*Config, error) {
	path := filepath.Join(dataDir, FileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Config{Version: SchemaVersion}, nil
		}
		return nil, &ConfigError{Code: CodeIOError, Path: path, Message: "read config", Err: err}
	}

	// First pass: the version alone, so a future schema gets its own code
	// instead of being lost in entry-decoding errors.
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, &ConfigError{Code: CodeInvalidConfig, Path: path, Message: "parse config", Err: err}
	}
	switch {
	case head.Version < 1:
		return nil, &ConfigError{
			Code: CodeInvalidConfig, Path: path,
			Message: fmt.Sprintf("missing or invalid version %d", head.Version),
		}
	case head.Version > SchemaVersion:
		return nil, &ConfigError{
			Code: CodeUnsupportedVersion, Path: path,
			Message: fmt.Sprintf("config version %d is newer than supported version %d", head.Version, SchemaVersion),
		}
	}

	// Second pass: strict decoding, so an unknown key is an error rather than a
	// silently empty config that would fall back to a different dataset.
	cfg := &Config{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, &ConfigError{Code: CodeInvalidConfig, Path: path, Message: "parse config", Err: err}
	}
	if err := cfg.validate(path); err != nil {
		return nil, err
	}
	return cfg, nil
}

// syncParentDir flushes the directory that holds the renamed config. It is a
// variable so tests can pin the rename-committed-but-not-durable path.
var syncParentDir = syncDir

// Save validates the config and replaces sources.json atomically with a 0600
// file inside a 0700 data directory. A validation failure or an I/O failure
// before the rename leaves the previous file byte-identical and no temporary
// file behind.
//
// Durability is reported truthfully: after the rename the new contents are
// already installed, so the parent directory is flushed and, if that flush
// fails, Save returns a *ConfigError saying the config was replaced but may not
// survive a crash. It never removes or rewrites the config in that case,
// because the previous contents are already gone and deleting the new file
// would destroy data rather than report a failure; on platforms without
// directory flushing the flush is a no-op.
func (c *Config) Save(dataDir string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return &ConfigError{Code: CodeIOError, Path: dataDir, Message: "create data directory", Err: err}
	}
	payload, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return &ConfigError{Code: CodeInvalidConfig, Path: dataDir, Message: "encode config", Err: err}
	}
	payload = append(payload, '\n')

	path := filepath.Join(dataDir, FileName)
	f, err := os.CreateTemp(dataDir, ".sources-*.tmp")
	if err != nil {
		return &ConfigError{Code: CodeIOError, Path: path, Message: "create temporary config", Err: err}
	}
	tmp := f.Name()
	fail := func(msg string, cause error) error {
		_ = f.Close()
		_ = os.Remove(tmp)
		return &ConfigError{Code: CodeIOError, Path: path, Message: msg, Err: cause}
	}
	if err := f.Chmod(0o600); err != nil {
		return fail("secure temporary config", err)
	}
	if _, err := f.Write(payload); err != nil {
		return fail("write temporary config", err)
	}
	if err := f.Sync(); err != nil {
		return fail("sync temporary config", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return &ConfigError{Code: CodeIOError, Path: path, Message: "close temporary config", Err: err}
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return &ConfigError{Code: CodeIOError, Path: path, Message: "replace config", Err: err}
	}
	// The rename is committed: path already names the new contents. Flush the
	// directory so the new name survives a crash, and if that flush fails say
	// exactly that instead of reporting a write failure that did not happen.
	if err := syncParentDir(dataDir); err != nil {
		return &ConfigError{
			Code:    CodeIOError,
			Path:    path,
			Message: "config was replaced but the data directory could not be synced, so it may not survive a crash",
			Err:     err,
		}
	}
	return nil
}

// Validate checks the config without touching the filesystem: schema version,
// reserved and duplicate IDs, known harnesses, known kinds and root presence.
// Disabled entries are validated exactly like enabled ones; only the
// enabled-instance-requires-a-root rule is relaxed for them and for
// backup-copies, whose archived paths are allowed to be offline.
func (c *Config) Validate() error { return c.validate("") }

func (c *Config) validate(path string) error {
	if c == nil {
		return &ConfigError{Code: CodeInvalidConfig, Path: path, Message: "nil config"}
	}
	switch {
	case c.Version < 1:
		return &ConfigError{
			Code: CodeInvalidConfig, Path: path,
			Message: fmt.Sprintf("missing or invalid version %d", c.Version),
		}
	case c.Version > SchemaVersion:
		return &ConfigError{
			Code: CodeUnsupportedVersion, Path: path,
			Message: fmt.Sprintf("config version %d is newer than supported version %d", c.Version, SchemaVersion),
		}
	}
	seen := make(map[string]bool, len(c.Entries))
	for i := range c.Entries {
		e := &c.Entries[i]
		if err := e.validate(path); err != nil {
			return err
		}
		if seen[e.ID] {
			return &ConfigError{
				Code: CodeDuplicateID, Path: path, ID: e.ID, Harness: e.Harness,
				Message: "duplicate instance id",
			}
		}
		seen[e.ID] = true
	}
	return nil
}

func (e *Entry) validate(path string) error {
	if err := validateID(e.ID, path, e.Harness); err != nil {
		return err
	}
	if !e.Harness.Known() {
		return &ConfigError{
			Code: CodeUnknownHarness, Path: path, ID: e.ID, Harness: e.Harness,
			Message: "unknown harness",
		}
	}
	if !e.Kind.valid() {
		return &ConfigError{
			Code: CodeInvalidKind, Path: path, ID: e.ID, Harness: e.Harness,
			Message: fmt.Sprintf("invalid kind %q", string(e.Kind)),
		}
	}
	for _, root := range e.Roots {
		if root == "" {
			return &ConfigError{
				Code: CodeInvalidRoot, Path: path, ID: e.ID, Harness: e.Harness,
				Message: "empty root path",
			}
		}
	}
	if !e.Disabled && e.Kind == KindInstance && len(e.Roots) == 0 {
		return &ConfigError{
			Code: CodeNoRoots, Path: path, ID: e.ID, Harness: e.Harness,
			Message: "enabled instance needs at least one root",
		}
	}
	return nil
}

// Add appends an entry and returns the stored copy. It validates a candidate
// config first, so a rejected add leaves the receiver untouched. Add never
// touches the filesystem.
func (c *Config) Add(req AddRequest) (Entry, error) {
	if c == nil {
		return Entry{}, &ConfigError{Code: CodeInvalidConfig, Message: "nil config"}
	}
	id := req.ID
	if id == "" {
		generated, err := NewID()
		if err != nil {
			return Entry{}, err
		}
		id = generated
	}
	kind := req.Kind
	if kind == "" {
		kind = KindInstance
	}
	entry := Entry{
		ID:       id,
		Label:    req.Label,
		Harness:  req.Harness,
		Roots:    copyStrings(req.Roots),
		Kind:     kind,
		Disabled: req.Disabled,
	}
	candidate := c.clone()
	candidate.Entries = append(candidate.Entries, entry)
	if err := candidate.Validate(); err != nil {
		return Entry{}, err
	}
	*c = *candidate
	return copyEntry(entry), nil
}

// Move replaces an entry's roots while keeping its ID, label, harness, kind and
// disabled flag: moving a dataset must not create a new identity. It never
// touches the filesystem.
func (c *Config) Move(id string, roots []string) (Entry, error) {
	idx := c.indexOf(id)
	if idx < 0 {
		return Entry{}, &ConfigError{Code: CodeNotFound, ID: id, Message: "unknown instance id"}
	}
	candidate := c.clone()
	candidate.Entries[idx].Roots = copyStrings(roots)
	if err := candidate.Validate(); err != nil {
		return Entry{}, err
	}
	*c = *candidate
	return copyEntry(c.Entries[idx]), nil
}

// Promote turns an explicitly identified backup-copy into a scanned instance,
// keeping the ID. Promoting anything else is an error: a copy is never
// promoted implicitly.
func (c *Config) Promote(id string) (Entry, error) {
	idx := c.indexOf(id)
	if idx < 0 {
		return Entry{}, &ConfigError{Code: CodeNotFound, ID: id, Message: "unknown instance id"}
	}
	if kind := c.Entries[idx].Kind; kind != KindBackupCopy {
		return Entry{}, &ConfigError{
			Code: CodeInvalidKind, ID: id, Harness: c.Entries[idx].Harness,
			Message: "only a " + string(KindBackupCopy) + " entry can be promoted to " + string(KindInstance),
		}
	}
	candidate := c.clone()
	candidate.Entries[idx].Kind = KindInstance
	if err := candidate.Validate(); err != nil {
		return Entry{}, err
	}
	*c = *candidate
	return copyEntry(c.Entries[idx]), nil
}

// Remove deletes an entry from the config only. It never deletes, renames or
// otherwise touches the source data the entry pointed at.
func (c *Config) Remove(id string) error {
	idx := c.indexOf(id)
	if idx < 0 {
		return &ConfigError{Code: CodeNotFound, ID: id, Message: "unknown instance id"}
	}
	candidate := c.clone()
	candidate.Entries = append(candidate.Entries[:idx], candidate.Entries[idx+1:]...)
	if err := candidate.Validate(); err != nil {
		return err
	}
	*c = *candidate
	return nil
}

func (c *Config) indexOf(id string) int {
	if c == nil {
		return -1
	}
	for i := range c.Entries {
		if c.Entries[i].ID == id {
			return i
		}
	}
	return -1
}

// clone deep-copies the config so a rejected mutation can never be observed.
func (c *Config) clone() *Config {
	out := &Config{Version: c.Version}
	if c.Entries == nil {
		return out
	}
	out.Entries = make([]Entry, len(c.Entries))
	for i, e := range c.Entries {
		out.Entries[i] = copyEntry(e)
	}
	return out
}

func copyEntry(e Entry) Entry {
	e.Roots = copyStrings(e.Roots)
	return e
}
