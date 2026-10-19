package scan

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

// genParser serves a corpus of plain JSONL-shaped files and turns every line
// into one event, which is enough to exercise discovery, incremental cursors,
// batching and commit cost without any harness-specific parsing.
type genParser struct {
	root  string
	files int
}

func (p genParser) Harness() model.Harness { return model.Harness("gen") }
func (p genParser) Roots() []string        { return []string{p.root} }
func (p genParser) Discover(ctx context.Context) ([]harness.Source, error) {
	out := make([]harness.Source, 0, p.files)
	for i := 0; i < p.files; i++ {
		out = append(out, harness.Source{Path: filepath.Join(p.root, fmt.Sprintf("log-%03d", i)), Kind: "jsonl"})
	}
	return out, nil
}
func (p genParser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	raw, e := os.ReadFile(src.Path)
	if e != nil {
		return harness.Batch{}, e
	}
	b := harness.Batch{Next: harness.Cursor{Offset: int64(len(raw)), Size: int64(len(raw))}}
	base := filepath.Base(src.Path)
	for i := cur.Offset; i < int64(len(raw)); i++ {
		if raw[i] != '\n' {
			continue
		}
		b.Events = append(b.Events, model.UsageEvent{
			DedupKey:  fmt.Sprintf("%s-%d", base, i),
			SessionID: base,
			Timestamp: time.Unix(0, i),
			Model:     "gen-1",
			Tokens:    model.Tokens{Input: 10, Output: 2, CacheRead: 5},
		})
	}
	return b, nil
}

func genCorpus(tb testing.TB, files, perFile int) (string, *store.Store, *Scanner) {
	tb.Helper()
	dir := tb.TempDir()
	for f := 0; f < files; f++ {
		path := filepath.Join(dir, fmt.Sprintf("log-%03d", f))
		buf := make([]byte, 0, perFile*2)
		for i := 0; i < perFile; i++ {
			buf = append(buf, "x\n"...)
		}
		if err := os.WriteFile(path, buf, 0o600); err != nil {
			tb.Fatal(err)
		}
	}
	st, err := store.Open(filepath.Join(dir, "db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { st.Close() })
	return dir, st, New(st, nil, nil, genParser{root: dir, files: files})
}

// TestFreshScanIsLinear guards the scan pipeline against the quadratic session
// recompute that made a 255K-event scan take 23 minutes: doubling the event
// count may double the work, but it must not quadruple the time.
func TestFreshScanIsLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	measure := func(files, perFile int) time.Duration {
		best := time.Duration(0)
		for i := 0; i < 2; i++ {
			_, _, sc := genCorpus(t, files, perFile)
			start := time.Now()
			if err := sc.Scan(context.Background()); err != nil {
				t.Fatal(err)
			}
			d := time.Since(start)
			if best == 0 || d < best {
				best = d
			}
		}
		return best
	}
	one := measure(4, 4000) // 16K events
	two := measure(8, 4000) // 32K events
	t.Logf("fresh scan 16K events=%v, 32K events=%v (%.2fx)", one, two, float64(two)/float64(one))
	// A quadratic pipeline would be ~4x; allow generous slack for a loaded
	// machine while still catching superlinear behaviour.
	if limit := 3*one + 50*time.Millisecond; two > limit {
		t.Fatalf("fresh scan is superlinear: 16K=%v, 32K=%v", one, two)
	}
}

// BenchmarkIncrementalAppend measures the watcher path after one line is
// appended to an already-indexed source: a quick scan must parse only the new
// bytes and commit one event.
func BenchmarkIncrementalAppend(b *testing.B) {
	dir, _, sc := genCorpus(b, 1, 20000)
	ctx := context.Background()
	if err := sc.Scan(ctx); err != nil {
		b.Fatal(err)
	}
	path := filepath.Join(dir, "log-000")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := f.WriteString("x\n"); err != nil {
			b.Fatal(err)
		}
		if err := sc.Scan(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
