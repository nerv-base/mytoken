package harness_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

// fakeParser accepts one flat root holding *.log files, which is enough to
// exercise detection and root validation without a real tool.
type fakeParser struct{ roots []string }

func (p fakeParser) Harness() model.Harness { return "fake" }

func (p fakeParser) Roots() []string { return append([]string(nil), p.roots...) }

func (p fakeParser) WithRoots(roots ...string) harness.Parser {
	return fakeParser{roots: append([]string(nil), roots...)}
}

func (p fakeParser) Discover(context.Context) ([]harness.Source, error) {
	var out []harness.Source
	for _, root := range p.roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() && filepath.Ext(e.Name()) == ".log" {
				out = append(out, harness.Source{Path: filepath.Join(root, e.Name()), Kind: "jsonl"})
			}
		}
	}
	return out, nil
}

func (p fakeParser) Parse(context.Context, harness.Source, harness.Cursor) (harness.Batch, error) {
	return harness.Batch{}, nil
}

func TestDetect(t *testing.T) {
	empty := t.TempDir()
	populated := t.TempDir()
	if err := os.WriteFile(filepath.Join(populated, "a.log"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A dot directory alone is not data.
	hidden := t.TempDir()
	if err := os.Mkdir(filepath.Join(hidden, ".cache"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		roots   []string
		want    bool
		problem string
	}{
		{"no roots", nil, false, "no data directory configured"},
		{"missing", []string{filepath.Join(empty, "nope")}, false, "data directory not found"},
		{"empty", []string{empty}, false, "data directory is empty"},
		{"hidden only", []string{hidden}, false, "data directory is empty"},
		{"populated", []string{filepath.Join(empty, "nope"), populated}, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := harness.Detect(fakeParser{roots: tc.roots})
			if st.Detected != tc.want || st.Problem != tc.problem {
				t.Fatalf("Detect = %+v, want detected=%v problem=%q", st, tc.want, tc.problem)
			}
			if st.Harness != "fake" {
				t.Fatalf("status harness = %q", st.Harness)
			}
		})
	}
}

func TestValidateRoot(t *testing.T) {
	empty := t.TempDir()
	file := filepath.Join(empty, "not-a-dir")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "a.log"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "a.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := fakeParser{roots: []string{t.TempDir()}}
	if err := harness.ValidateRoot(p, filepath.Join(empty, "nope")); !errors.Is(err, harness.ErrRootMissing) {
		t.Fatalf("missing root error = %v, want ErrRootMissing", err)
	}
	if err := harness.ValidateRoot(p, file); !errors.Is(err, harness.ErrRootNotDir) {
		t.Fatalf("file root error = %v, want ErrRootNotDir", err)
	}
	if err := harness.ValidateRoot(p, empty); !errors.Is(err, harness.ErrRootNoData) {
		t.Fatalf("empty root error = %v, want ErrRootNoData", err)
	}
	if err := harness.ValidateRoot(p, other); !errors.Is(err, harness.ErrRootNoData) {
		t.Fatalf("wrong-format root error = %v, want ErrRootNoData", err)
	}
	if err := harness.ValidateRoot(p, data); err != nil {
		t.Fatalf("valid root error = %v", err)
	}
	if got := p.Roots(); len(got) != 1 || got[0] == data {
		t.Fatalf("ValidateRoot changed the parser roots: %v", got)
	}
}

// Every registered parser must present a display name and the root-override
// capability the settings UI relies on.
func TestRegisteredParsersArePresentable(t *testing.T) {
	for _, p := range harness.All() {
		h := p.Harness()
		if !h.Known() {
			t.Errorf("%q has no display name", h)
		}
		if _, ok := p.(harness.Rooter); !ok {
			t.Errorf("%q does not implement WithRoots", h)
		}
	}
}
