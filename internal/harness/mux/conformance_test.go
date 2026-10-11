package mux

import (
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
	"github.com/zzstar101/mytoken/internal/model"
)

const conformanceFixture = "testdata/conformance/mux"

func TestConformance(t *testing.T) {
	harnesstest.Run(t, conformanceCase())
}

func BenchmarkConformance(b *testing.B) {
	harnesstest.Bench(b, conformanceCase())
}

// conformanceCase needs no Grow: chat.jsonl is append-only, so the suite's
// default growth publishes a longer line prefix at every step
// (docs/HARNESS.md §6). config.json and the session-usage.json decoy are single
// line files and are therefore complete from step 0 on.
func conformanceCase() harnesstest.Case {
	return harnesstest.Case{
		Name:    string(model.Mux),
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: conformanceFixture,
	}
}
