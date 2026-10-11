package antigravity

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
	"github.com/zzstar101/mytoken/internal/model"
)

// The conformance fixture grows two cascade databases instead of shipping binary
// files: step 0 creates cascade-a.db with three committed generations (one of
// them without usage), step 1 appends a fourth generation, step 2 adds a second
// cascade. Every generation is a hand-built protobuf document, which is also what
// makes the fixture independent of any real conversation.
//
// seed/private-thread.txt stands in for the conversation body: its workspace URL
// is the only part the parser publishes, while the SENTINEL_PRIVATE_TEXT text it
// also contains is stored in the database (the trajectory blob and one step
// metadata) and must never reach the parsed snapshot.
const (
	conformanceDir = "testdata/conformance/antigravity"

	genMetadataDDL = `CREATE TABLE gen_metadata (idx INTEGER PRIMARY KEY, data BLOB)`
	stepsDDL       = `CREATE TABLE steps (idx INTEGER PRIMARY KEY, metadata BLOB)`
	blobDDL        = `CREATE TABLE trajectory_metadata_blob (id INTEGER PRIMARY KEY, data BLOB)`
)

func conformanceCase() harnesstest.Case {
	return harnesstest.Case{
		Name:    string(model.Antigravity),
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: conformanceDir,
		Grow:    growCascades,
	}
}

func TestConformance(t *testing.T) { harnesstest.Run(t, conformanceCase()) }

func BenchmarkConformance(b *testing.B) { harnesstest.Bench(b, conformanceCase()) }

func growCascades(t testing.TB, dir string, step int) error {
	t.Helper()
	private, err := os.ReadFile(filepath.Join(conformanceDir, "seed", "private-thread.txt"))
	if err != nil {
		return err
	}
	switch step {
	case 0:
		return withCascade(filepath.Join(dir, "cascade-a.db"), func(db *sql.DB) error {
			if err := createSchema(db); err != nil {
				return err
			}
			rows := []struct {
				idx  int64
				data []byte
			}{
				{1, generationRow(chatModel(modelUsage(1000, 200, 100, 400, 50, 150, "gen-a-1"),
					isoTime("2025-03-19T16:00:00Z"), "Gemini 3.1 Pro High", "gemini-3.1-pro-default", ""), nil)},
				{2, generationRow(chatModel(modelUsage(300, 40, 0, 50, 0, 40, "gen-a-2"),
					nil, "", "", "MODEL_PLACEHOLDER_M35"), []int64{1})},
				{3, generationRow(chatModel(nil, nil, "", "", "MODEL_PLACEHOLDER_M18"), nil)},
			}
			for _, r := range rows {
				if err := insertGeneration(db, r.idx, r.data); err != nil {
					return err
				}
			}
			if err := insertStep(db, 1, stepMetadata(1742400060, private)); err != nil {
				return err
			}
			return insertBlob(db, private)
		})
	case 1:
		return withCascade(filepath.Join(dir, "cascade-a.db"), func(db *sql.DB) error {
			return insertGeneration(db, 4, generationRow(
				chatModel(modelUsage(500, 80, 25, 200, 0, 0, ""),
					isoTime("2025-03-19T16:02:00Z"), "Gemini 3.5 Flash High", "", ""), nil))
		})
	case 2:
		return withCascade(filepath.Join(dir, "cascade-b.db"), func(db *sql.DB) error {
			if err := createSchema(db); err != nil {
				return err
			}
			if err := insertGeneration(db, 1, generationRow(
				chatModel(modelUsage(700, 120, 30, 150, 10, 110, "gen-b-1"),
					isoTime("2025-03-19T17:00:00Z"), "Gemini 3 Pro", "", ""), nil)); err != nil {
				return err
			}
			return insertBlob(db, []byte("workspace file:///Users/demo/other\n"+string(private)))
		})
	}
	return nil
}

func withCascade(path string, fn func(db *sql.DB) error) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	return fn(db)
}

func createSchema(db *sql.DB) error {
	for _, stmt := range []string{genMetadataDDL, stepsDDL, blobDDL} {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func insertGeneration(db *sql.DB, idx int64, data []byte) error {
	_, err := db.Exec(`INSERT INTO gen_metadata (idx, data) VALUES (?, ?)`, idx, data)
	return err
}

func insertStep(db *sql.DB, idx int64, metadata []byte) error {
	_, err := db.Exec(`INSERT INTO steps (idx, metadata) VALUES (?, ?)`, idx, metadata)
	return err
}

func insertBlob(db *sql.DB, data []byte) error {
	_, err := db.Exec(`INSERT INTO trajectory_metadata_blob (id, data) VALUES (1, ?)`, data)
	return err
}

// generationRow wraps a ChatModel (#1) and its packed step indices (#2).
func generationRow(chat []byte, stepIndices []int64) []byte {
	out := pbBytes(1, chat)
	if len(stepIndices) > 0 {
		var packed []byte
		for _, idx := range stepIndices {
			packed = append(packed, pbUvarint(uint64(idx))...)
		}
		out = append(out, pbBytes(2, packed)...)
	}
	return out
}

// chatModel builds ChatModel: #4 usage, #9 ChatStartMetadata, #19 configuration
// id, #20 model_enum attribute, #21 display name.
func chatModel(usage, createdAt []byte, displayName, configID, modelEnum string) []byte {
	var out []byte
	if usage != nil {
		out = append(out, pbBytes(4, usage)...)
	}
	if createdAt != nil {
		out = append(out, pbBytes(9, pbBytes(4, createdAt))...)
	}
	if configID != "" {
		out = append(out, pbBytes(19, []byte(configID))...)
	}
	if modelEnum != "" {
		pair := append(pbBytes(1, []byte("model_enum")), pbBytes(2, []byte(modelEnum))...)
		out = append(out, pbBytes(20, pair)...)
	}
	if displayName != "" {
		out = append(out, pbBytes(21, []byte(displayName))...)
	}
	return out
}

// modelUsage builds ModelUsageStats; a zero counter is omitted exactly the way
// the app omits it.
func modelUsage(input, total, cacheWrite, cacheRead, thinking, response uint64, responseID string) []byte {
	var out []byte
	for _, f := range []struct {
		number int
		value  uint64
	}{{2, input}, {3, total}, {4, cacheWrite}, {5, cacheRead}, {9, thinking}, {10, response}} {
		if f.value > 0 {
			out = append(out, pbVarint(f.number, f.value)...)
		}
	}
	if responseID != "" {
		out = append(out, pbBytes(11, []byte(responseID))...)
	}
	return out
}

// stepMetadata builds a step blob: #1 a Timestamp (epoch seconds) and #2 the
// conversation body the parser must ignore.
func stepMetadata(seconds uint64, body []byte) []byte {
	return append(pbVarint(1, seconds), pbBytes(2, body)...)
}

func isoTime(value string) []byte { return []byte(value) }

func pbBytes(number int, value []byte) []byte {
	out := pbKey(number, 2)
	out = append(out, pbUvarint(uint64(len(value)))...)
	return append(out, value...)
}

func pbVarint(number int, value uint64) []byte {
	return append(pbKey(number, 0), pbUvarint(value)...)
}

func pbKey(number, wire int) []byte { return pbUvarint(uint64(number<<3 | wire)) }

func pbUvarint(value uint64) []byte {
	var out []byte
	for value >= 0x80 {
		out = append(out, byte(value)|0x80)
		value >>= 7
	}
	return append(out, byte(value))
}
