package zed

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
	"github.com/zzstar101/mytoken/internal/model"
)

// The conformance database is grown in three steps rather than copied, because
// threads.db is rewritten in place: step 0 creates the schema and three threads
// (main, subagent, one without usage), step 1 rewrites the main thread with a
// second request, step 2 appends a fourth thread. `data` is zstd-compressed for
// most rows and plain JSON for one, so both data_type values stay covered.
//
// Everything in seed/ is hand-written, fixed-UTC sample data. Zed's thread
// document has no reasoning counter, so Reasoning is always 0 for this harness;
// the fixture covers two models, the cache read/write split, a subagent thread
// and a thread whose requests spent no tokens.
const (
	conformanceDir = "testdata/conformance/zed"

	threadsDDL = `CREATE TABLE threads (
	id TEXT PRIMARY KEY,
	summary TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	data_type TEXT NOT NULL,
	data BLOB NOT NULL,
	parent_id TEXT,
	folder_paths TEXT,
	folder_paths_order TEXT,
	created_at TEXT
)`
)

func conformanceCase() harnesstest.Case {
	return harnesstest.Case{
		Name:    string(model.Zed),
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: conformanceDir,
		Grow:    growThreads,
	}
}

func TestConformance(t *testing.T) { harnesstest.Run(t, conformanceCase()) }

func BenchmarkConformance(b *testing.B) { harnesstest.Bench(b, conformanceCase()) }

type threadRow struct {
	id        string
	summary   string
	updatedAt string
	createdAt string
	dataType  string
	seed      string
	parent    string
	folders   string
}

func growThreads(t testing.TB, dir string, step int) error {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "threads.db"))
	if err != nil {
		return err
	}
	defer db.Close()

	switch step {
	case 0:
		if _, err := db.Exec(threadsDDL); err != nil {
			return err
		}
		for _, r := range []threadRow{
			{
				id: "z-main", summary: "Refactor the token accounting",
				updatedAt: "2025-03-19T16:01:00Z", createdAt: "2025-03-19T16:00:00Z",
				dataType: "zstd", seed: "z-main.v1.json", folders: `["/Users/demo/proj"]`,
			},
			{
				id: "z-sub", summary: "Explore the cache accounting",
				updatedAt: "2025-03-19T16:00:45Z", createdAt: "2025-03-19T16:00:30Z",
				dataType: "json", seed: "z-sub.json", parent: "z-main",
			},
			{
				id: "z-empty", summary: "Thread with no usage yet",
				updatedAt: "2025-03-19T16:03:00Z", createdAt: "2025-03-19T16:03:00Z",
				dataType: "zstd", seed: "z-empty.json", folders: `["/Users/demo/proj"]`,
			},
		} {
			if err := insertThread(db, r); err != nil {
				return err
			}
		}
	case 1:
		data, err := seedBytes("z-main.v2.json", "zstd")
		if err != nil {
			return err
		}
		if _, err := db.Exec(
			`UPDATE threads SET data = ?, updated_at = ? WHERE id = 'z-main'`,
			data, "2025-03-19T16:02:00Z",
		); err != nil {
			return err
		}
	case 2:
		return insertThread(db, threadRow{
			id: "z-late", summary: "Summarize the migration plan",
			updatedAt: "2025-03-19T16:04:10Z", createdAt: "2025-03-19T16:04:00Z",
			dataType: "zstd", seed: "z-late.json",
		})
	}
	return nil
}

func insertThread(db *sql.DB, r threadRow) error {
	data, err := seedBytes(r.seed, r.dataType)
	if err != nil {
		return err
	}
	_, err = db.Exec(
		`INSERT INTO threads (id, summary, updated_at, data_type, data, parent_id, folder_paths, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.id, r.summary, r.updatedAt, r.dataType, data, r.parent, r.folders, r.createdAt,
	)
	return err
}

// seedBytes loads a hand-written thread document from seed/, compressing it when
// the row claims the zstd encoding.
func seedBytes(name, dataType string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(conformanceDir, "seed", name))
	if err != nil {
		return nil, err
	}
	if dataType != "zstd" {
		return raw, nil
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	return enc.EncodeAll(raw, nil), nil
}
