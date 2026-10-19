package zcode

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
)

// TestConformance drives the shared harness contract end to end: discovery,
// golden output, incremental growth, cursor stability, idempotency, privacy
// and the missing-installation path.
//
// The fixture is synthesized instead of copied because the ledger is a SQLite
// database: seed/schema.sql plus seed/step*.sql are replayed into a fresh
// database for every growth step, with step0.sql as the baseline.
func TestConformance(t *testing.T) {
	harnesstest.Run(t, harnesstest.Case{
		Name:    "zcode",
		Fixture: filepath.Join("testdata", "conformance", "zcode"),
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Grow:    grow,
	})
}

func BenchmarkConformance(b *testing.B) {
	harnesstest.Bench(b, harnesstest.Case{
		Name:    "zcode",
		Fixture: filepath.Join("testdata", "conformance", "zcode"),
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Grow:    grow,
	})
}

// seedDir is where the SQL statements that build the fixture live.
var seedDir = filepath.Join("testdata", "conformance", "zcode", "seed")

// grow rebuilds the ledger for step n from the seed statements, so every step
// is a deterministic superset of the previous one: the database is recreated
// from scratch and replayed up to the requested step. Seeding the parser with a
// database instead of an append-only log is exactly why Case.Grow exists.
func grow(t testing.TB, dir string, step int) error {
	t.Helper()
	path := filepath.Join(dir, dbName)
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer db.Close()
	for _, name := range append([]string{"schema.sql", "step0.sql"}, stepFiles(step)...) {
		if err := execSeedFile(db, filepath.Join(seedDir, name)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// stepFiles are the growth scripts applied on top of step0.sql, in order.
func stepFiles(step int) []string {
	out := make([]string, 0, step)
	for n := 1; n <= step; n++ {
		out = append(out, fmt.Sprintf("step%d.sql", n))
	}
	return out
}

func execSeedFile(db *sql.DB, path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	_, err = db.Exec(string(body))
	return err
}
