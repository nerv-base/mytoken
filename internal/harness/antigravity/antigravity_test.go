package antigravity

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
)

func TestModelResolution(t *testing.T) {
	cases := []struct {
		name        string
		displayName string
		configID    string
		modelEnum   string
		want        string
	}{
		{"display name wins over the internal id", "Gemini 3.1 Pro High", "gemini-3.1-pro-default", "", "gemini-3.1-pro-high"},
		{"placeholder enum resolves to its catalog id", "", "", "MODEL_PLACEHOLDER_M35", "claude-sonnet-4-6"},
		{"placeholder enum with a display name", "Gemini 3 Flash", "", "MODEL_PLACEHOLDER_M18", "gemini-3-flash"},
		{"unknown placeholder", "", "", "MODEL_PLACEHOLDER_M99", "unknown"},
		{"configuration id kept as is", "", "gemini-2.5-pro-preview", "", "gemini-2.5-pro-preview"},
		{"nothing to go on", "", "", "", "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chat := parseProtoFields(chatModel(nil, nil, tc.displayName, tc.configID, tc.modelEnum))
			if got := resolveModel(chat); got != tc.want {
				t.Fatalf("resolveModel = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProtoTextRejectsBinary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value []byte
		want  string
	}{
		{"plain text", []byte("gen-1"), "gen-1"},
		{"newline inside", []byte("a\nb"), "a\nb"},
		{"nul byte", []byte("a\x00b"), ""},
		{"control byte", []byte("a\x1fb"), ""},
		{"delete byte", []byte("a\x7fb"), ""},
		{"invalid utf-8", []byte{0xff, 0xfe}, ""},
		{"empty", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			field := &protoField{number: 11, wire: 2, bytes: tc.value}
			if got := protoFieldText(field); got != tc.want {
				t.Fatalf("protoFieldText(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

func TestProtoTimestampForms(t *testing.T) {
	submessage := pbBytes(4, append(pbVarint(1, 1742400000), pbVarint(2, 500000000)...))
	for _, tc := range []struct {
		name  string
		field *protoField
		want  time.Time
	}{
		{"iso text", firstProtoField(parseProtoFields(pbBytes(4, []byte("2025-03-19T16:00:00Z"))), 4), time.Date(2025, 3, 19, 16, 0, 0, 0, time.UTC)},
		{"timestamp submessage", firstProtoField(parseProtoFields(submessage), 4), time.Date(2025, 3, 19, 16, 0, 0, 500000000, time.UTC)},
		{"epoch seconds", firstProtoField(parseProtoFields(pbVarint(4, 1742400000)), 4), time.Date(2025, 3, 19, 16, 0, 0, 0, time.UTC)},
		{"epoch milliseconds", firstProtoField(parseProtoFields(pbVarint(4, 1742400000000)), 4), time.Date(2025, 3, 19, 16, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := protoTimestamp(tc.field); !got.Equal(tc.want) {
				t.Fatalf("protoTimestamp = %s, want %s", got, tc.want)
			}
		})
	}
}

// A cascade whose committed generations spent nothing publishes neither an event
// nor an empty session.
func TestCascadeWithoutUsageStaysQuiet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cascade-empty.db")
	rows := [][]byte{
		generationRow(chatModel(nil, nil, "Gemini 3 Pro", "", ""), nil),                                     // no usage at all
		generationRow(chatModel(modelUsage(0, 0, 0, 0, 0, 0, "gen-zero"), nil, "", "", ""), nil),            // all-zero counters
		generationRow(chatModel(modelUsage(0, 0, 0, 0, 0, 0, "gen-inflight"), nil, "", "", ""), []int64{1}), // in-flight
	}
	if err := withCascade(path, func(db *sql.DB) error {
		if err := createSchema(db); err != nil {
			return err
		}
		for i, row := range rows {
			if err := insertGeneration(db, int64(i+1), row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	p := NewWithRoots(dir)
	batch, err := p.Parse(context.Background(), harness.Source{Path: path, Kind: kindSQLite}, harness.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Events) != 0 || len(batch.Sessions) != 0 {
		t.Fatalf("empty cascade emitted %d events / %d sessions", len(batch.Events), len(batch.Sessions))
	}
	if batch.Next.Fingerprint == "" || batch.Next.Extra != "" {
		t.Fatalf("next cursor = %+v, want a recorded fingerprint without a watermark", batch.Next)
	}
}

func TestDiscoverSkipsNonDatabases(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"cascade-a.db", "cascade-b.DB", "cascade.pb", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "nested.db"), 0o755); err != nil {
		t.Fatal(err)
	}

	p := NewWithRoots(root, filepath.Join(root, "missing"))
	sources, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, s := range sources {
		if s.Kind != kindSQLite {
			t.Fatalf("source %s has kind %q", s.Path, s.Kind)
		}
		paths = append(paths, filepath.Base(s.Path))
	}
	if want := []string{"cascade-a.db", "cascade-b.DB"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("discovered %v, want %v", paths, want)
	}
}

// An unchanged database must hand the cursor back untouched, otherwise the suite
// reports a cursor that moved on an unchanged source.
func TestUnchangedCascadeKeepsCursor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cascade-a.db")
	if err := withCascade(path, func(db *sql.DB) error {
		if err := createSchema(db); err != nil {
			return err
		}
		return insertGeneration(db, 1, generationRow(chatModel(
			modelUsage(100, 20, 0, 0, 0, 20, "gen-1"), isoTime("2025-03-19T16:00:00Z"), "Gemini 3 Pro", "", ""), nil))
	}); err != nil {
		t.Fatal(err)
	}

	p := NewWithRoots(dir)
	src := harness.Source{Path: path, Kind: kindSQLite}
	first, err := p.Parse(context.Background(), src, harness.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 1 {
		t.Fatalf("first parse emitted %d events, want 1", len(first.Events))
	}
	second, err := p.Parse(context.Background(), src, first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 0 || len(second.Sessions) != 0 {
		t.Fatalf("re-parse emitted %d events / %d sessions", len(second.Events), len(second.Sessions))
	}
	if !reflect.DeepEqual(second.Next, first.Next) {
		t.Fatalf("cursor moved on an unchanged source: was %+v, got %+v", first.Next, second.Next)
	}
}

// The watermark is the row index, so a generation appended after the first parse
// is the only one that comes back.
func TestAppendedGenerationIsIncremental(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cascade-a.db")
	if err := withCascade(path, func(db *sql.DB) error {
		if err := createSchema(db); err != nil {
			return err
		}
		return insertGeneration(db, 1, generationRow(chatModel(
			modelUsage(100, 20, 0, 0, 0, 20, "gen-1"), isoTime("2025-03-19T16:00:00Z"), "Gemini 3 Pro", "", ""), nil))
	}); err != nil {
		t.Fatal(err)
	}

	p := NewWithRoots(dir)
	src := harness.Source{Path: path, Kind: kindSQLite}
	first, err := p.Parse(context.Background(), src, harness.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if err := withCascade(path, func(db *sql.DB) error {
		return insertGeneration(db, 2, generationRow(chatModel(
			modelUsage(300, 40, 0, 10, 0, 40, "gen-2"), isoTime("2025-03-19T16:05:00Z"), "Gemini 3 Pro", "", ""), nil))
	}); err != nil {
		t.Fatal(err)
	}

	second, err := p.Parse(context.Background(), src, first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 1 || second.Events[0].DedupKey != "antigravity:cascade-a:gen-2" {
		t.Fatalf("incremental parse emitted %+v", second.Events)
	}
	if len(second.Sessions) != 1 || !second.Sessions[0].StartedAt.Equal(time.Date(2025, 3, 19, 16, 5, 0, 0, time.UTC)) {
		t.Fatalf("incremental sessions = %+v", second.Sessions)
	}
}
