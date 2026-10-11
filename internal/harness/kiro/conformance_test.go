// Package kiro_test holds the conformance suite for the Kiro parser.
package kiro_test

import (
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
	"github.com/zzstar101/mytoken/internal/harness/kiro"
	"github.com/zzstar101/mytoken/internal/model"
)

// conformanceCase describes the Kiro fixture to the shared harness suite.
func conformanceCase() harnesstest.Case {
	return harnesstest.Case{
		Name:    string(model.Kiro),
		New:     func(root string) harness.Parser { return kiro.NewWithRoots(root) },
		Fixture: "testdata/conformance/kiro",
	}
}

// TestConformance runs the shared harness conformance checks over the fixture.
func TestConformance(t *testing.T) { harnesstest.Run(t, conformanceCase()) }

// BenchmarkConformance measures a cold parse of the fixture.
func BenchmarkConformance(b *testing.B) { harnesstest.Bench(b, conformanceCase()) }
