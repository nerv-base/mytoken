package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/sqlitedsn"
)

func TestBackupIncludesWALAndPreservesHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err = st.Exec(ctx, "PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	}
	event := model.UsageEvent{DedupKey: "request", SessionID: "s", Model: "m", BaseURL: "https://api.example.com/v1", Tokens: model.Tokens{Input: 11, Output: 12, CacheRead: 13, CacheWrite: 14, Reasoning: 15}}
	if err = st.Commit(ctx, model.Codex, "fixture", harness.Batch{Events: []model.UsageEvent{event}}, []Resolution{{Provider: "alias", Cost: 1.25, Priced: true}}); err != nil {
		t.Fatal(err)
	}
	if err = st.SetSetting(ctx, "pricing_fingerprint", "keep"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("fixture has no live WAL: %v", err)
	}
	dest := filepath.Join(dir, "备份 with spaces.db")
	if err = st.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("backup permissions: %v", info.Mode())
	}
	// Mutating the live index after backup must not change the snapshot.
	if err = st.SetSetting(ctx, "pricing_fingerprint", "later"); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqlitedsn.URI(dest, "mode=ro"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n, in, out, read, write, reason int
	var cost float64
	err = db.QueryRow("SELECT count(*),sum(input),sum(output),sum(cache_read),sum(cache_write),sum(reasoning),sum(cost) FROM events").Scan(&n, &in, &out, &read, &write, &reason, &cost)
	if err != nil || n != 1 || in != 11 || out != 12 || read != 13 || write != 14 || reason != 15 || cost != 1.25 {
		t.Fatalf("history changed: %d %d %d %d %d %d %g; %v", n, in, out, read, write, reason, cost, err)
	}
	var fingerprint string
	if err = db.QueryRow("SELECT value FROM settings WHERE key='pricing_fingerprint'").Scan(&fingerprint); err != nil || fingerprint != "keep" {
		t.Fatalf("backup not independent: %q %v", fingerprint, err)
	}
}

func TestBackupRefusesExistingTargetAndInvalidParent(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dir := t.TempDir()
	dest := filepath.Join(dir, "existing.db")
	sentinel := []byte("must remain unchanged")
	if err = os.WriteFile(dest, sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	if err = st.Backup(context.Background(), dest); err == nil {
		t.Fatal("overwrote backup")
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != string(sentinel) {
		t.Fatal("existing backup changed")
	}
	if err = st.Backup(context.Background(), filepath.Join(dest, "cannot-create.db")); err == nil {
		t.Fatal("invalid destination succeeded")
	}
}

func TestBackupCancelledDoesNotCreateTarget(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dest := filepath.Join(t.TempDir(), "cancelled.db")
	if err = st.Backup(ctx, dest); err == nil {
		t.Fatal("cancelled backup succeeded")
	}
	if _, err = os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("cancelled call created target: %v", err)
	}
}
