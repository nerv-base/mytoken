package pi

import (
	"testing"

	"github.com/nerv-base/mytoken/internal/harness"
	"github.com/nerv-base/mytoken/internal/harness/harnesstest"
	"github.com/nerv-base/mytoken/internal/model"
)

// TestConformance runs the shared conformance suite (docs/ARCHITECTURE.md#conformance-suite) against
// the checked-in fixture in testdata/conformance/pi. The fixture root is the
// sessions layer itself (PI_HOME/agent/sessions): a top-level session log and
// the async subagent run log stored under its session directory.
func TestConformance(t *testing.T) {
	harnesstest.Run(t, conformanceCase())
}

// BenchmarkConformance is the per-harness parse benchmark.
func BenchmarkConformance(b *testing.B) {
	harnesstest.Bench(b, conformanceCase())
}

func conformanceCase() harnesstest.Case {
	return harnesstest.Case{
		Name:    string(model.Pi),
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: "testdata/conformance/pi",
	}
}
