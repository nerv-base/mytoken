package codex

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
	"github.com/zzstar101/mytoken/internal/model"
)

// The materialized conformance tree mirrors ~/.codex, where a live rollout and
// one Codex already rotated live side by side:
//
//	sessions/2026/01/02/rollout-2026-01-02T03-04-05-sess-a.jsonl       plain, growing
//	sessions/2026/01/02/rollout-2026-01-02T03-06-00-agent-1.jsonl.zst  rotated, growing
//
// Both suffixes are read from one root, so the suite also covers the mixed
// directory this parser sees on a real machine.
const (
	conformanceSteps = 3 // mirrors harnesstest's default Steps
	conformanceDir   = "sessions/2026/01/02"
	conformancePlain = "rollout-2026-01-02T03-04-05-sess-a.jsonl"
	conformanceZstd  = "rollout-2026-01-02T03-06-00-agent-1.jsonl.zst"
)

// TestConformance runs the shared conformance suite (docs/ARCHITECTURE.md#conformance-suite) against
// the checked-in fixture in testdata/conformance/codex.
func TestConformance(t *testing.T) {
	harnesstest.Run(t, conformanceCase())
}

// BenchmarkConformance is the template for the per-harness parse benchmark.
func BenchmarkConformance(b *testing.B) {
	harnesstest.Bench(b, conformanceCase())
}

func conformanceCase() harnesstest.Case {
	return harnesstest.Case{
		Name:    string(model.Codex),
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: "testdata/conformance/codex",
		Steps:   conformanceSteps,
		Grow:    growConformance,
	}
}

// growConformance materializes one growth step of the fixture. Each step adds
// roughly a third of every seed log (whole lines only). The subagent rollout is
// the one Codex rotated: a zstd frame is not appendable, and this parser
// re-reads a compressed log in full on every parse, so a step writes a complete
// frame holding the records seen so far rather than appending to it.
func growConformance(t testing.TB, dir string, step int) error {
	t.Helper()
	if err := writePlainStep(filepath.Join(dir, filepath.FromSlash(conformanceDir), conformancePlain), "parent.jsonl", step); err != nil {
		return err
	}
	return writeZstdStep(filepath.Join(dir, filepath.FromSlash(conformanceDir), conformanceZstd), "agent-1.jsonl", step)
}

// seedLines reads one seed log and returns its records as newline-terminated
// lines, so a growth step can never split a JSON record.
func seedLines(name string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join("testdata", "conformance", "codex", "seed", name))
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line+"\n")
		}
	}
	return lines, nil
}

// stepPrefix is the number of records visible at step, matching the suite's
// ceil(records*(step+1)/Steps) growth rule.
func stepPrefix(total, step int) int {
	take := (total*(step+1) + conformanceSteps - 1) / conformanceSteps
	if take > total {
		take = total
	}
	return take
}

func writePlainStep(path, seed string, step int) error {
	lines, err := seedLines(seed)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Join(lines[:stepPrefix(len(lines), step)], "")), 0o644)
}

func writeZstdStep(path, seed string, step int) error {
	lines, err := seedLines(seed)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		return err
	}
	if _, err := zw.Write([]byte(strings.Join(lines[:stepPrefix(len(lines), step)], ""))); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
