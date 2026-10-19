package mimocode

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
)

// conformanceFixture is the MiMo Code data-directory level: the directory whose
// direct child is mimocode.db, which is exactly what Discover reads.
const conformanceFixture = "testdata/conformance/mimocode"

// TestConformance runs the shared conformance suite (docs/ARCHITECTURE.md#conformance-suite) against
// the checked-in fixture in testdata/conformance/mimocode.
//
// MiMo Code is sqlite, so the fixture is not a set of files to copy: Case.Grow
// builds the database from the seed scripts in
// testdata/conformance/mimocode/seed, one growth step per script. Step 0
// creates the schema and inserts the first half of the rows; step 1 inserts the
// second half. Case.Steps is 2 to match those two halves.
//
// The seed scripts are line-oriented: one SQL statement per line, with '--'
// comments and blank lines skipped. The closure executes them with
// database/sql (modernc.org/sqlite is already the driver the shared sqlite
// module uses) and never lets two statements share a line.
//
// Rows cover:
//   - two sessions, one of them a child (session.parent_id)
//   - three models across three providers, cache read and cache write,
//     reasoning, an assistant row without `cost`, and no-usage rows that must
//     never emit an event (missing `tokens`; all-zero tokens with zero cost)
//   - the `part` table, which holds conversation content and is never read
//   - session titles, which hold the first user message and are never stored
//
// Project paths come from session.directory, which is an absolute path inside
// the fixture and not derived from the sqlite file's location; MiMo Code keeps
// only its basename, so no Case.Rewrite is needed and the golden is
// reproducible on any machine.
//
// SENTINEL_PRIVATE_TEXT sits in the assistant message text and in a `part` row:
// it proves the fixture is the privacy check's target without ever reaching the
// parsed snapshot (the parser keeps only numbers and identifiers, never text).
func TestConformance(t *testing.T) {
	harnesstest.Run(t, conformanceCase())
}

// BenchmarkConformance is the per-harness parse benchmark.
func BenchmarkConformance(b *testing.B) {
	harnesstest.Bench(b, conformanceCase())
}

func conformanceCase() harnesstest.Case {
	return harnesstest.Case{
		Name:    string(HarnessID),
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: conformanceFixture,
		Steps:   2,
		Grow:    growConformanceDB,
	}
}

// growConformanceDB applies seed/step<N>.sql to dir/mimocode.db. It runs one
// growth step at a time so the suite can replay the database in halves and
// compare the incrementally merged snapshot with a single full parse.
func growConformanceDB(t testing.TB, dir string, step int) error {
	t.Helper()
	seed := filepath.Join(conformanceFixture, "seed", fmt.Sprintf("step%d.sql", step))
	data, err := os.ReadFile(seed)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "mimocode.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	for i, stmt := range strings.Split(string(data), "\n") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" || strings.HasPrefix(stmt, "--") {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("%s line %d: %w", seed, i+1, err)
		}
	}
	return nil
}
