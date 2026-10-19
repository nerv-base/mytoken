package minimax

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

// conformanceSeed holds the fixture's SQL seed. MiniMax Code's runtime ledger is
// a sqlite database that grows in place, so Grow cannot copy fixture files: it
// replays the schema plus one more slice of rows per step.
const conformanceSeed = "testdata/conformance/minimax/seed"

// conformanceSteps are the seed's growth slices, applied in order.
var conformanceSteps = []string{"step0.sql", "step1.sql", "step2.sql"}

func conformanceCase() harnesstest.Case {
	return harnesstest.Case{
		Name:    string(Harness),
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: "testdata/conformance/minimax",
		Steps:   len(conformanceSteps),
		Grow:    growConformance,
	}
}

func TestConformance(t *testing.T) { harnesstest.Run(t, conformanceCase()) }

func BenchmarkConformance(b *testing.B) { harnesstest.Bench(b, conformanceCase()) }

// growConformance rebuilds <dir>/v2/sqlite/runtime-state.sqlite from the seed's
// schema and its first step+1 row slices, so every step is the same database
// grown by one more batch of usage rows.
func growConformance(_ testing.TB, dir string, step int) error {
	path := filepath.Join(dir, filepath.FromSlash(dbRelPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	names := append([]string{"schema.sql"}, conformanceSteps[:step+1]...)
	for _, name := range names {
		script, err := os.ReadFile(filepath.Join(conformanceSeed, name))
		if err != nil {
			return err
		}
		if _, err := db.Exec(string(script)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}
