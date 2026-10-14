package instance

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/model"
)

// These tests pin the durability contract of Save: the parent directory is
// flushed after the rename, and a flush failure is reported as exactly what it
// is instead of as a write failure that did not happen.

func oneEntryCfg(id, root string) *Config {
	return &Config{Version: SchemaVersion, Entries: []Entry{{
		ID: id, Harness: model.Harness("codex"), Roots: []string{root}, Kind: KindInstance,
	}}}
}

func TestSaveSyncsParentDirectory(t *testing.T) {
	dir := t.TempDir()
	prev := syncParentDir
	var synced []string
	syncParentDir = func(d string) error {
		synced = append(synced, d)
		return nil
	}
	defer func() { syncParentDir = prev }()

	if err := oneEntryCfg("inst_new", "/tmp/new").Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(synced) != 1 || filepath.Clean(synced[0]) != filepath.Clean(dir) {
		t.Fatalf("synced dirs = %v, want [%s]", synced, dir)
	}
}

// TestSaveReportsRenameCommittedWhenDirectorySyncFails: the rename has already
// replaced sources.json, so a failed flush must say so, must never delete or
// rewrite the new config, and must leave no temporary file behind.
func TestSaveReportsRenameCommittedWhenDirectorySyncFails(t *testing.T) {
	dir := t.TempDir()
	if err := oneEntryCfg("inst_old", "/tmp/old").Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	prev := syncParentDir
	flushErr := errors.New("simulated directory flush failure")
	syncParentDir = func(string) error { return flushErr }
	defer func() { syncParentDir = prev }()

	err := oneEntryCfg("inst_new", "/tmp/new").Save(dir)
	if !errors.Is(err, flushErr) {
		t.Fatalf("Save err = %v, want the flush error", err)
	}
	var ce *ConfigError
	if !errors.As(err, &ce) || ce.Code != CodeIOError {
		t.Fatalf("err = %v, want a *ConfigError with code %q", err, CodeIOError)
	}
	if !strings.Contains(ce.Message, "replaced") {
		t.Fatalf("error %q does not state that the config was replaced", ce.Message)
	}

	onDisk, lerr := Load(dir)
	if lerr != nil {
		t.Fatalf("Load after a failed flush: %v", lerr)
	}
	if len(onDisk.Entries) != 1 || onDisk.Entries[0].ID != "inst_new" {
		t.Fatalf("on disk = %+v, want the new entry left in place", onDisk.Entries)
	}
	assertNoTempLeftovers(t, dir)
}
