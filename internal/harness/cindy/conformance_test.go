package cindy

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nerv-base/mytoken/internal/harness"
	"github.com/nerv-base/mytoken/internal/harness/harnesstest"
	"github.com/nerv-base/mytoken/internal/model"
)

const conformanceFixture = "testdata/conformance/cindy"

// TestConformance runs the shared conformance suite against the synthetic
// Cindy database. Step 0 creates the schema and seeds the first slice; step 1
// appends a new session and its daily aggregate.
func TestConformance(t *testing.T) {
	harnesstest.Run(t, conformanceCase())
}

// BenchmarkConformance is the per-harness parse benchmark.
func BenchmarkConformance(b *testing.B) {
	harnesstest.Bench(b, conformanceCase())
}

func conformanceCase() harnesstest.Case {
	return harnesstest.Case{
		Name:    string(model.Cindy),
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: conformanceFixture,
		Steps:   2,
		Grow:    growConformanceDB,
	}
}

// growConformanceDB applies seed/step<N>.sql to dir/cindy-test.db. The seed
// scripts are line-oriented: one statement per line, comments skipped.
func growConformanceDB(t testing.TB, dir string, step int) error {
	t.Helper()
	seed := filepath.Join(conformanceFixture, "seed", fmt.Sprintf("step%d.sql", step))
	data, err := os.ReadFile(seed)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "cindy-test.db"))
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
