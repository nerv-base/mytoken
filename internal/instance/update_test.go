package instance

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
)

// These tests exercise the additive serialized read-modify-write helper. They
// live in package instance (not instance_test) because they shorten the lock
// timeout and spawn this test binary as a real second process to contend on the
// OS lock. The lock is a genuine cross-process lock, so a lock file that no
// process has locked must never block a writer, and a lock file whose owner is
// alive must never be stolen however old it looks.

func readRaw(t *testing.T, dir string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("read %s: %v", FileName, err)
	}
	return raw
}

// TestUpdateHelperProcess is not a test. It is the separate process the lock
// tests spawn: mode "hold" takes the lock and holds it until it is killed, mode
// "update" runs one real Update. Every other invocation skips.
func TestUpdateHelperProcess(t *testing.T) {
	mode := os.Getenv(helperModeEnv)
	if mode == "" {
		t.Skip("helper process only")
	}
	dir := os.Getenv(helperDirEnv)
	switch mode {
	case "hold":
		unlock, err := acquireLock(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "helper: acquire: %v\n", err)
			os.Exit(3)
		}
		defer unlock()
		fmt.Println("locked")
		os.Stdout.Sync()
		_, _ = io.Copy(io.Discard, os.Stdin) // hold until the parent kills us
	case "update":
		if _, err := Update(dir, func(c *Config) error {
			_, e := c.Add(AddRequest{
				Label:   os.Getenv(helperLabelEnv),
				Harness: model.Harness("codex"),
				Roots:   []string{os.Getenv(helperRootEnv)},
			})
			return e
		}); err != nil {
			t.Fatalf("helper Update: %v", err)
		}
	default:
		fmt.Fprintf(os.Stderr, "helper: unknown mode %q\n", mode)
		os.Exit(4)
	}
}

const (
	helperModeEnv  = "MYTOKEN_INSTANCE_TEST_HELPER"
	helperDirEnv   = "MYTOKEN_INSTANCE_TEST_DIR"
	helperLabelEnv = "MYTOKEN_INSTANCE_TEST_LABEL"
	helperRootEnv  = "MYTOKEN_INSTANCE_TEST_ROOT"
)

// lockHolder is a live child process holding the data directory's lock.
type lockHolder struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
}

// startLockHolder starts a real second process that takes the lock on dir and
// blocks until it is killed.
func startLockHolder(t *testing.T, dir string) *lockHolder {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestUpdateHelperProcess$")
	cmd.Env = append(os.Environ(), helperModeEnv+"=hold", helperDirEnv+"="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start lock helper: %v", err)
	}
	h := &lockHolder{cmd: cmd, stdin: stdin}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		h.kill()
		t.Fatalf("lock helper did not report holding the lock (line=%q err=%v)", line, err)
	}
	return h
}

// kill terminates the holder with no chance to run its unlock path, which is
// what a crash looks like to the next writer.
func (h *lockHolder) kill() {
	_ = h.cmd.Process.Kill()
	_ = h.cmd.Wait()
}

// TestUpdateSerializesConcurrentWriters is the lost-update test: a plain
// Load/mutate/Save loop racing eight goroutines keeps only one or two entries.
func TestUpdateSerializesConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	roots := t.TempDir()

	const writers = 8
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := Update(dir, func(c *Config) error {
				_, e := c.Add(AddRequest{
					Label:   fmt.Sprintf("writer-%d", i),
					Harness: model.Harness("codex"),
					Roots:   []string{filepath.Join(roots, fmt.Sprintf("w%d", i))},
				})
				return e
			})
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Entries) != writers {
		t.Fatalf("lost update: %d entries, want %d", len(cfg.Entries), writers)
	}
	seen := map[string]bool{}
	for _, e := range cfg.Entries {
		if e.ID == "" {
			t.Fatal("entry with empty opaque id")
		}
		if seen[e.ID] {
			t.Fatalf("duplicate id %s", e.ID)
		}
		seen[e.ID] = true
	}
}

// TestUpdateConcurrentProcessesPreserveEntries is the same lost-update claim
// across real process boundaries: four separate processes each add one entry,
// and every entry must survive.
func TestUpdateConcurrentProcessesPreserveEntries(t *testing.T) {
	dir := t.TempDir()
	roots := t.TempDir()

	const procs = 4
	cmds := make([]*exec.Cmd, procs)
	bufs := make([]*bytes.Buffer, procs)
	for i := 0; i < procs; i++ {
		bufs[i] = &bytes.Buffer{}
		cmd := exec.Command(os.Args[0], "-test.run=^TestUpdateHelperProcess$")
		cmd.Env = append(os.Environ(),
			helperModeEnv+"=update",
			helperDirEnv+"="+dir,
			helperLabelEnv+"="+fmt.Sprintf("proc-%d", i),
			helperRootEnv+"="+filepath.Join(roots, fmt.Sprintf("p%d", i)),
		)
		cmd.Stdout = bufs[i]
		cmd.Stderr = bufs[i]
		if err := cmd.Start(); err != nil {
			t.Fatalf("start helper %d: %v", i, err)
		}
		cmds[i] = cmd
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper %d failed: %v\n%s", i, err, bufs[i])
		}
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Entries) != procs {
		t.Fatalf("lost cross-process update: %d entries, want %d", len(cfg.Entries), procs)
	}
	labels := map[string]bool{}
	ids := map[string]bool{}
	for _, e := range cfg.Entries {
		labels[e.Label] = true
		if ids[e.ID] {
			t.Fatalf("duplicate id %s", e.ID)
		}
		ids[e.ID] = true
	}
	if len(labels) != procs {
		t.Fatalf("labels = %v, want %d distinct", labels, procs)
	}
}

func TestUpdateReturnsWrittenConfig(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Update(dir, func(c *Config) error {
		_, e := c.Add(AddRequest{Harness: model.Harness("codex"), Roots: []string{"/tmp/root"}})
		return e
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if cfg == nil || len(cfg.Entries) != 1 {
		t.Fatalf("returned config = %+v", cfg)
	}
	onDisk, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(onDisk.Entries) != 1 || onDisk.Entries[0].ID != cfg.Entries[0].ID {
		t.Fatalf("on disk %+v, returned %+v", onDisk, cfg)
	}
}

// TestUpdateNeverRewritesMalformedConfig: a corrupt sources.json is reported
// and left byte-identical, the mutation never runs, and the refused update
// leaves no lock file behind either.
func TestUpdateNeverRewritesMalformedConfig(t *testing.T) {
	dir := t.TempDir()
	bad := []byte("{ this is not json\n")
	if err := os.WriteFile(filepath.Join(dir, FileName), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	ran := false
	_, err := Update(dir, func(*Config) error {
		ran = true
		return nil
	})
	if err == nil {
		t.Fatal("Update accepted a malformed config")
	}
	if ran {
		t.Fatal("mutation ran against a malformed config")
	}
	if got := readRaw(t, dir); !bytes.Equal(got, bad) {
		t.Fatalf("malformed config was rewritten: %q", got)
	}
	if _, statErr := os.Stat(filepath.Join(dir, LockFileName)); statErr == nil {
		t.Fatal("a refused update left a lock file behind")
	}
	assertNoTempLeftovers(t, dir)
}

func TestUpdateKeepsConfigWhenMutationFails(t *testing.T) {
	dir := t.TempDir()
	base := &Config{Version: SchemaVersion, Entries: []Entry{{
		ID: "inst_keep", Harness: model.Harness("codex"), Roots: []string{"/tmp/keep"}, Kind: KindInstance,
	}}}
	if err := base.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	before := readRaw(t, dir)
	sentinel := errors.New("mutation exploded")
	_, err := Update(dir, func(c *Config) error {
		c.Entries = nil
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the mutation error", err)
	}
	if got := readRaw(t, dir); !bytes.Equal(got, before) {
		t.Fatalf("config changed after failed mutation:\n%s", got)
	}
	assertNoTempLeftovers(t, dir)
}

// TestUpdateRefusesWhileChildHoldsOldLock is the lost-update regression: a real
// second process holds the lock and keeps holding it, and the lock file is aged
// far past any staleness window. The lock must not be stolen, Update must fail
// bounded and must not mutate anything.
func TestUpdateRefusesWhileChildHoldsOldLock(t *testing.T) {
	dir := t.TempDir()
	holder := startLockHolder(t, dir)
	defer holder.kill()

	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, LockFileName), old, old); err != nil {
		t.Fatal(err)
	}
	if held, err := lockIsHeld(dir); err != nil || !held {
		t.Fatalf("helper does not hold the lock: held=%v err=%v", held, err)
	}

	prev := lockTimeout
	lockTimeout = 80 * time.Millisecond
	defer func() { lockTimeout = prev }()

	start := time.Now()
	ran := false
	_, err := Update(dir, func(*Config) error {
		ran = true
		return nil
	})
	if err == nil {
		t.Fatal("Update wrote while a live child held the lock")
	}
	if ran {
		t.Fatal("mutation ran without the lock")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("lock wait was not bounded: %s", elapsed)
	}
	if !strings.Contains(err.Error(), "lock") {
		t.Fatalf("error %q does not mention the lock", err.Error())
	}
	if _, statErr := os.Stat(filepath.Join(dir, FileName)); statErr == nil {
		t.Fatal("a refused writer created sources.json")
	}
	if held, err := lockIsHeld(dir); err != nil || !held {
		t.Fatalf("a refused writer disturbed the holder's lock: held=%v err=%v", held, err)
	}
}

// TestUpdateSucceedsAfterLockOwnerIsKilled: the owner is killed with no chance
// to unlock, so the OS must release the lock and the next writer must get it
// immediately even though the lock file it left is old.
func TestUpdateSucceedsAfterLockOwnerIsKilled(t *testing.T) {
	dir := t.TempDir()
	holder := startLockHolder(t, dir)
	holder.kill()

	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, LockFileName), old, old); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if _, err := Update(dir, func(c *Config) error {
		_, e := c.Add(AddRequest{Harness: model.Harness("codex"), Roots: []string{"/tmp/after-crash"}})
		return e
	}); err != nil {
		t.Fatalf("Update after the lock owner died: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waited %s for a lock whose owner is gone", elapsed)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(cfg.Entries))
	}
}

// TestUpdateIgnoresLeftoverUnlockedLockFile: an old lock file that no process
// has locked (a leftover from a crashed writer, or written by hand) is not a
// lock, so it must not block. The file itself is a persistent inode and is left
// in place rather than unlinked.
func TestUpdateIgnoresLeftoverUnlockedLockFile(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, LockFileName)
	if err := os.WriteFile(lock, []byte("abandoned\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(dir, func(c *Config) error {
		_, e := c.Add(AddRequest{Harness: model.Harness("codex"), Roots: []string{"/tmp/stale"}})
		return e
	}); err != nil {
		t.Fatalf("a leftover unlocked lock file blocked Update: %v", err)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("the persistent lock file was unlinked: %v", err)
	}
	if held, err := lockIsHeld(dir); err != nil || held {
		t.Fatalf("lock still held after Update: held=%v err=%v", held, err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(cfg.Entries))
	}
	assertNoTempLeftovers(t, dir)
}

func TestUpdateCreatesMissingDataDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "nested", "data")
	if _, err := Update(dir, func(c *Config) error {
		_, e := c.Add(AddRequest{Harness: model.Harness("codex"), Roots: []string{"/tmp/new"}})
		return e
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// Both artifacts must exist on every platform; only the 0600 hardening is a
	// Unix guarantee.
	for _, name := range []string{FileName, LockFileName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
	}
	// Unix permission bits are not representable on Windows: os.Stat reports a
	// synthetic 0666 for regular files, so the exact-mode assertion can only
	// run where the platform enforces mode bits.
	if runtime.GOOS != "windows" {
		if mode := fileMode(t, filepath.Join(dir, FileName)); mode != 0o600 {
			t.Fatalf("sources.json mode = %o, want 600", mode)
		}
		if mode := fileMode(t, filepath.Join(dir, LockFileName)); mode != 0o600 {
			t.Fatalf("%s mode = %o, want 600", LockFileName, mode)
		}
	}
}

func TestUpdateSerializesSameProcessCallers(t *testing.T) {
	dir := t.TempDir()
	// A long mutation must exclude a second caller in this process even when
	// the on-disk lock would also do it; this pins the in-process mutex.
	var wg sync.WaitGroup
	order := make([]string, 0, 2)
	var mu sync.Mutex
	record := func(s string) {
		mu.Lock()
		order = append(order, s)
		mu.Unlock()
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = Update(dir, func(*Config) error {
			record("a-start")
			time.Sleep(80 * time.Millisecond)
			record("a-end")
			return nil
		})
	}()
	time.Sleep(20 * time.Millisecond)
	go func() {
		defer wg.Done()
		_, _ = Update(dir, func(*Config) error {
			record("b")
			return nil
		})
	}()
	wg.Wait()
	want := []string{"a-start", "a-end", "b"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("interleaved updates: %v", order)
	}
}

// assertNoTempLeftovers rejects anything a save should never leave behind. The
// lock file is expected to persist, because unlinking a lock inode while an OS
// lock can be held on it is exactly what would break mutual exclusion.
func assertNoTempLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == FileName || e.Name() == LockFileName {
			continue
		}
		t.Fatalf("leftover file %q in %s", e.Name(), dir)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}
