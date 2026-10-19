package scan

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/store"
)

// A scan for watched changes parses a file again until a parse finds
// nothing new, then skips it while its stat (and its WAL's) holds still.
func TestQuickScanSkipsUnchangedFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	if e := os.WriteFile(path, []byte("a\n"), 0600); e != nil {
		t.Fatal(e)
	}
	st, e := store.Open(filepath.Join(dir, "db"))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	// The WAL step below is about a SQLite source, whose writes land in a
	// side WAL; give the fixture that kind so the scan stats the WAL.
	sc := New(st, nil, nil, walTestParser{testParser{dir}})
	ctx := context.Background()
	parses := func() int { sc.mu.Lock(); defer sc.mu.Unlock(); return sc.parses }
	step := func(quick bool, want int, why string) {
		t.Helper()
		before := parses()
		if e := sc.scan(ctx, quick); e != nil {
			t.Fatal(e)
		}
		if got := parses() - before; got != want {
			t.Fatalf("%s: %d parses, want %d", why, got, want)
		}
	}
	step(false, 1, "first scan")    // finds a: not remembered
	step(true, 1, "after new data") // nothing new: remembered
	step(true, 0, "unchanged")
	step(true, 0, "unchanged again")

	appendLine(t, path, "b\n")
	step(true, 1, "appended")
	step(true, 1, "confirm nothing new")
	step(true, 0, "unchanged after append")

	// A SQLite database takes writes in its WAL first.
	if e := os.WriteFile(path+"-wal", []byte("x"), 0600); e != nil {
		t.Fatal(e)
	}
	step(true, 1, "wal changed")

	step(false, 1, "periodic scan parses everything")

	var n int
	if e := st.DB().QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&n); e != nil {
		t.Fatal(e)
	}
	if n != 2 {
		t.Fatalf("%d events, want 2", n)
	}
}

// walTestParser marks its source as SQLite so the scanner checks its WAL.
type walTestParser struct{ testParser }

func (p walTestParser) Discover(ctx context.Context) ([]harness.Source, error) {
	srcs, err := p.testParser.Discover(ctx)
	for i := range srcs {
		srcs[i].Kind = "sqlite"
	}
	return srcs, err
}

func appendLine(t *testing.T, path, s string) {
	t.Helper()
	// Same-second writes keep the mtime on coarse file systems; the size
	// still moves, but be sure.
	time.Sleep(10 * time.Millisecond)
	f, e := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	if _, e := f.WriteString(s); e != nil {
		t.Fatal(e)
	}
}
