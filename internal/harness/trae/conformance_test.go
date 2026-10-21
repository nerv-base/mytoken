package trae

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

// conformanceFixture is the ai-agent data directory: the directory whose child
// is database.db, which is what Discover reads.
const conformanceFixture = "testdata/conformance/trae"

// TestConformance runs the shared conformance suite (docs/ARCHITECTURE.md#conformance-suite) against
// the checked-in fixture in testdata/conformance/trae.
//
// Trae keeps its history in sqlite, so the fixture is not a set of files to
// copy: Case.Grow builds database.db from the seed scripts in
// testdata/conformance/trae/seed, one growth step per script. Step 0 creates
// the schema and the first turns, step 1 adds the turns that must be ignored.
// Case.Steps is 2 to match those two halves.
//
// The seed scripts are line-oriented: one SQL statement per line, with '--'
// comments and blank lines skipped. The closure executes them with
// database/sql (modernc.org/sqlite is the driver the shared sqlite module
// already uses) and never lets two statements share a line.
//
// The fixture database is plaintext on purpose. A conformance run has no key to
// give, and the parser reads a plaintext database without one, so the suite
// covers discovery, cursors, ids and privacy; the encrypted path is covered by
// sqlcipher_test.go, which builds its own ciphertext.
//
// Rows cover:
//   - two sessions with spend, one session with none (its title must not
//     become SessionMeta), one failed turn, one turn without chat_start_time
//   - three models across two sessions, cache read, cache write and reasoning,
//     prompt/completion that already include cache and reasoning tokens
//   - turns that must never emit an event: deleted, no token_usage, all-zero
//     usage, and a context that is not JSON
//   - conversation text in chat_message_general, chat_turn.rewritten_user_message
//     and user_configuration, which the parser never selects
//
// Project paths come from project.absolute_path, which the fixture pins to an
// absolute path rather than deriving from the database file's location, so no
// Case.Rewrite is needed and the golden is the same on any machine.
//
// SENTINEL_PRIVATE_TEXT sits in that conversation text: it proves the fixture is
// the privacy check's target without ever reaching the parsed snapshot (the
// parser keeps only numbers and identifiers, never text).
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

// growConformanceDB applies seed/step<N>.sql to dir/database.db. It runs one
// growth step at a time so the suite can replay the database in halves and
// compare the incrementally merged snapshot with a single full parse.
func growConformanceDB(t testing.TB, dir string, step int) error {
	t.Helper()
	seed := filepath.Join(conformanceFixture, "seed", fmt.Sprintf("step%d.sql", step))
	data, err := os.ReadFile(seed)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, dbName))
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
