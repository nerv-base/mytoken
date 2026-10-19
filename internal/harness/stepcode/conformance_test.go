package stepcode

import (
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
)

// TestConformance runs the shared conformance suite (docs/HARNESS.md) against
// the synthetic fixture in testdata/conformance/step-code. The fixture root is
// the sessions layer itself (~/.stepcode/agent/sessions): two session logs in
// one project directory, one of them a fork whose header names the other as its
// parentSession, plus a credentials file, a models file, a non-session JSONL
// sink and the golden file, none of which may be discovered or read.
//
// The fixture is hand-written from the documented Step Code session schema; it
// is not a real user log and contains no real credential, path or conversation.
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
		Fixture: "testdata/conformance/step-code",
	}
}
