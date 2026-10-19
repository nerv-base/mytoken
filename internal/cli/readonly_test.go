package cli

import (
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// dbSnapshot is everything a read-only command must leave alone: the database
// bytes, its mtime, and the size of the write-ahead log.
type dbSnapshot struct {
	sum    [sha256.Size]byte
	mtime  time.Time
	wal    int64
	walSet bool
}

func snapshotDB(t *testing.T, path string) dbSnapshot {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	snap := dbSnapshot{}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	copy(snap.sum[:], h.Sum(nil))
	info, err := f.Stat()
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	snap.mtime = info.ModTime()
	if wal, err := os.Stat(path + "-wal"); err == nil {
		snap.wal, snap.walSet = wal.Size(), true
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s-wal: %v", path, err)
	}
	return snap
}

func requireUnchanged(t *testing.T, before, after dbSnapshot) {
	t.Helper()
	if before.sum != after.sum {
		t.Error("database bytes changed")
	}
	if !before.mtime.Equal(after.mtime) {
		t.Errorf("database mtime changed: %s -> %s", before.mtime, after.mtime)
	}
	if after.wal > before.wal {
		t.Errorf("write-ahead log grew: %d -> %d bytes", before.wal, after.wal)
	}
	if !before.walSet && after.walSet && after.wal > 0 {
		t.Errorf("write-ahead log was created with %d bytes", after.wal)
	}
}

type readOnlyCase struct {
	name string
	args []string
	want []string
}

// readOnlyCases exercises every command that must never write. doctor resolves
// its path from MYTOKEN_HOME; the others are pointed at dbPath explicitly.
func readOnlyCases(dbPath string) []readOnlyCase {
	return []readOnlyCase{
		{
			name: "summary",
			args: []string{"summary", "--timezone", "UTC", "--db", dbPath},
			want: []string{"API-equivalent cost · UTC", "Today", "Month"},
		},
		{
			name: "daily",
			args: []string{"daily", "--timezone", "UTC", "--days", "3", "--db", dbPath},
			want: []string{"DATE", "CACHE WRITE", "API-EQUIV"},
		},
		{
			name: "quota",
			args: []string{"quota", "--json", "--db", dbPath},
			want: []string{`"generatedAt"`, `"no_observation"`},
		},
		{
			name: "statusline",
			args: []string{"statusline", "--timezone", "UTC", "--since", "1d", "--db", dbPath},
			want: []string{"claude-code/sess-tod", "1 req"},
		},
		{
			name: "doctor",
			args: []string{"doctor"},
			want: []string{"Index: read-only, schema ok", "HARNESS"},
		},
	}
}

func TestReadOnlyCommandsDoNotWrite(t *testing.T) {
	f := newFixture(t)
	for _, tc := range readOnlyCases(f.dbPath) {
		t.Run(tc.name, func(t *testing.T) {
			before := snapshotDB(t, f.dbPath)
			out, errOut, code := runCLI(t, tc.args...)
			if code != 0 {
				t.Fatalf("exit = %d, stderr = %s", code, errOut)
			}
			mustContain(t, out, tc.want...)
			requireUnchanged(t, before, snapshotDB(t, f.dbPath))
		})
	}
}

func TestReadOnlyCommandsWorkOnReadOnlyCopy(t *testing.T) {
	f := newFixture(t)
	copyHome := filepath.Join(t.TempDir(), "ro-home")
	mustMkdir(t, copyHome)
	copyPath := filepath.Join(copyHome, "mytoken.db")
	copyFile(t, f.dbPath, copyPath)
	if err := os.Chmod(copyPath, 0o444); err != nil {
		t.Fatalf("chmod 0444: %v", err)
	}
	t.Cleanup(func() { os.Chmod(copyPath, 0o600) })
	t.Setenv("MYTOKEN_HOME", copyHome)

	before := snapshotDB(t, copyPath)
	for _, tc := range readOnlyCases(copyPath) {
		t.Run(tc.name, func(t *testing.T) {
			out, errOut, code := runCLI(t, tc.args...)
			if code != 0 {
				t.Fatalf("exit = %d, stderr = %s", code, errOut)
			}
			mustContain(t, out, tc.want...)
		})
	}
	requireUnchanged(t, before, snapshotDB(t, copyPath))
	if info, err := os.Stat(copyPath); err != nil {
		t.Fatalf("stat copy: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0o444 {
		t.Errorf("copy mode = %o, want 444", perm)
	}
}

func TestReadOnlyCommandsDoNotCreateIndex(t *testing.T) {
	h := newTestHome(t)
	for _, args := range [][]string{
		{"summary"},
		{"daily"},
		{"quota"},
		{"statusline"},
		{"doctor"},
		{"doctor", "--json"},
	} {
		// summary/daily fail (exit 1) because there is no index; quota,
		// statusline and doctor report it. None may create the file.
		runCLI(t, args...)
		entries, err := os.ReadDir(h.home)
		if err != nil {
			t.Fatalf("read home: %v", err)
		}
		if len(entries) != 0 {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("%v created files in an empty MYTOKEN_HOME: %v", args, names)
		}
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open %s: %v", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("create %s: %v", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatalf("copy %s: %v", src, err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close %s: %v", dst, err)
	}
}
