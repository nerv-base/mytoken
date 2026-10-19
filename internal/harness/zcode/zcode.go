// Package zcode parses ZCode's local model-usage ledger.
//
// ZCode (zcode.z.ai, Z.ai's CLI coding agent) is a separate harness from the
// other Z.ai tools: it keeps its own home directory, and per the official FAQ
// its configuration lives in ~/.zcode/v2/config.json. MyToken reads none of
// that: the ledger below is the only input, and the configuration directory
// (credentials.json is encrypted) is never opened.
//
// Layout:
//
//	~/.zcode/cli/db/db.sqlite        the usage ledger (the documented path)
//
// The default root is the directory holding that database. ZCODE_HOME is
// deliberately not consulted: no first-party source confirms it (only
// third-party documentation mentions it), so the explicit multi-root override
//
//	MYTOKEN_ZCODE_DIRS=/path/a:/path/b
//
// is the supported way to point MyToken at ledgers outside ~/.zcode. A root may
// be the database directory, the cli directory, the ZCode home, or the database
// file itself.
//
// Schema (documented by ZCode's usage tooling):
//
//	model_usage(id TEXT PRIMARY KEY, session_id TEXT, started_at INTEGER,
//	            model_id TEXT, status TEXT, input_tokens INTEGER,
//	            output_tokens INTEGER, [cache_read_input_tokens INTEGER],
//	            [cache_creation_input_tokens INTEGER], [provider TEXT], ...)
//	session(id TEXT PRIMARY KEY, directory TEXT, ...)
//
// started_at is epoch milliseconds. The cache columns are optional — the ledger
// evolved — so they are probed per database instead of assumed, and a database
// without them still counts its usage. Every other column the parser does not
// need (prompt text, titles, credentials) is left unread; only id, session_id,
// started_at, model_id, status, input_tokens, output_tokens and, when present,
// the cache buckets and provider are selected.
//
// Token accounting. Only rows with status = "completed" are counted: an
// in-flight or failed turn is not usage yet. input_tokens is inclusive — it
// already contains the cached tokens — so the cache buckets are subtracted back
// out and reported as Tokens.CacheRead/CacheWrite, leaving Tokens.Input as the
// uncached remainder. A missing cache column stays 0, a bucket larger than
// input_tokens is clamped to it, and the three classes always sum to the
// recorded input_tokens. ZCode stores no per-request cost and no base URL here,
// so CostUSD stays nil and BaseURL stays "" (docs/SPEC.md: never invent).
//
// Incrementality. SQLite rewrites its header (change counter) on every commit,
// so the first-4KB fingerprint changes whenever anything is written; the cursor
// stores the database signature (size+mtime plus the -wal/-shm size+mtime) for
// the cheap unchanged fast path, plus a per-database state in Cursor.Extra: the
// highest rowid examined and the rowids of every turn that was still in flight.
//
// A scan of a rowid table makes two reads that cover disjoint rowid ranges. The
// first re-reads the in-flight rowids, in bounded chunks, so a turn that flipped
// from running to completed in place is counted and a turn that ended terminally
// is forgotten. The second reads `rowid > watermark` up to maxRowsPerParse rows:
// the rows never examined before, counted when they are completed and remembered
// while they are in flight. The in-flight set therefore never holds the
// watermark back, and a long-lived in-flight set cannot starve new rows. The
// watermark makes the row cap safe, because the next scan continues past the
// rowids this one examined. Row ids going backwards (the table was rebuilt)
// reset the watermark and drop the in-flight set, forcing a rescan. DedupKey is
// `zcode:<row id>`, so a rescan can never double count.
//
// The in-flight set is deliberately not capped. Its ids are the only record of
// their rows, which sit at or below the watermark: a row that is neither above
// the watermark nor in the set is never read again, so a cap silently turns
// "counted when it completes" into "never counted" for every id it drops, while
// the dropped row is still in the ledger and may still complete. The set grows
// only with rowids observed in flight, and each one leaves it as soon as its row
// is seen terminal or completed, so its size tracks the ledger's live in-flight
// turns. An id whose row has disappeared is kept rather than pruned: SQLite may
// hand a freed rowid to a later insert, and a reused rowid below the watermark
// is only ever read because it is still in the set.
//
// A WITHOUT ROWID model_usage has no watermark to page on: its primary key is
// not a monotonic cursor (ledger ids may be arbitrary and arrive out of order),
// so a capped `ORDER BY id` read would return the same prefix on every scan and
// strand every row past the cap. Those tables are read in full instead — every
// completed row, no LIMIT — and the stable DedupKey keeps the re-read
// idempotent.
//
// Read-only access is required: the database is opened file:...?mode=ro with a
// busy timeout and PRAGMA query_only. There is deliberately no immutable=1
// fallback: reading the last checkpointed snapshot would silently hide every row
// that is still in the write-ahead log. A missing database, a non-SQLite file,
// an incomplete schema or a database whose log cannot be read yields an empty
// batch and keeps the cursor, so the scan is retried; a schema probe that fails
// at runtime — as opposed to a database that genuinely lacks the columns — is
// reported as an error, likewise with the cursor unchanged.
package zcode

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/sqlitedsn"

	sqlite "modernc.org/sqlite"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

const (
	fingerprintBytes = 4096
	busyTimeoutMS    = 5000

	kindSQLite = "sqlite"

	// zcodeDirName is ZCode's home directory under $HOME.
	zcodeDirName = ".zcode"
	// cliDirName/dbDirName/dbName locate the ledger inside that home:
	// ~/.zcode/cli/db/db.sqlite.
	cliDirName = "cli"
	dbDirName  = "db"
	dbName     = "db.sqlite"

	// envDirs is the explicit multi-root override.
	envDirs = "MYTOKEN_ZCODE_DIRS"

	// usageTable/sessionTable are the ledger tables.
	usageTable   = "model_usage"
	sessionTable = "session"

	// completedStatus is the only status whose usage counts.
	completedStatus = "completed"

	// maxRowsPerParse bounds one batch of never-examined rows (rowid >
	// watermark). The watermark advances to the last row examined, so the next
	// scan resumes exactly where this one stopped: the bound is a window, not a
	// truncation. A scan that stopped on the window records a backlog in the
	// cursor, so the next scan opens the database again even if the file has not
	// changed since, and only a scan that reaches the end of the ledger restores
	// the unchanged-database fast path. It does not apply to a WITHOUT ROWID
	// table, which has no watermark to resume from and is therefore read in full,
	// nor to the in-flight re-read, which pendingBatch bounds.
	maxRowsPerParse = 20000

	// pendingBatch bounds the rowids named by one in-flight re-read statement.
	// SQLite's historic SQLITE_MAX_VARIABLE_NUMBER is 999, so a batch of 500
	// leaves room for the statement's other parameters and keeps every statement
	// small. The whole in-flight set is still revisited, one chunk at a time.
	pendingBatch = 500

	// cursorVersion invalidates a cursor written by an older parser revision.
	// v2 fixed two silent undercounts: a WITHOUT ROWID table was truncated at
	// maxRowsPerParse, and a failed schema probe advanced the cursor. v3 fixes a
	// third: the in-flight rowid set was capped at 1024 ids, so every id it
	// dropped below the watermark could never be revisited and a turn that
	// completed afterwards was never counted. v4 fixes a fourth: a ledger larger
	// than maxRowsPerParse left the rest of the rows behind the
	// unchanged-database fast path, which no later scan ever crossed. A v3 cursor
	// may already be stranded at the row window on a ledger that has not changed
	// since, so the version bump forces the one rescan that drains it. The bump
	// also gives any older cursor a fresh watermark and anchor.
	cursorVersion = 4
)

// requiredUsageColumns are the documented model_usage columns. A database that
// lacks one of them is not a ZCode ledger this parser can count, so it yields
// an empty batch instead of a guess.
var requiredUsageColumns = []string{
	"id", "session_id", "started_at", "model_id", "status", "input_tokens", "output_tokens",
}

// The optional column names, in probe order. input_tokens is inclusive of both
// cache buckets when they exist.
var (
	cacheReadColumns  = []string{"cache_read_input_tokens", "cache_read_tokens", "cache_read"}
	cacheWriteColumns = []string{"cache_creation_input_tokens", "cache_creation_tokens", "cache_write_input_tokens", "cache_write"}
	providerColumns   = []string{"provider"}
)

// terminalStatuses are statuses that will not change again: their usage is not
// counted and their row is not revisited. Any other non-completed status is
// treated as in flight and remembered, because the ledger may still flip it to
// completed in place.
var terminalStatuses = map[string]bool{
	"failed": true, "error": true, "cancelled": true, "canceled": true,
	"aborted": true, "killed": true, "timeout": true, "timed_out": true,
	"expired": true, "incomplete": true,
}

// Parser reads ZCode usage from one or more ZCode directories.
type Parser struct {
	roots []string
	// prov recomputes this parser's default-root provenance from the current
	// environment. It is set only on the registry singleton; a parser built for
	// explicit roots reports those roots instead.
	prov func() ([]harness.Root, error)
}

var _ harness.Parser = (*Parser)(nil)

func init() { harness.Register(New()) }

// New returns a parser rooted at the default ZCode directories.
func New() *Parser { return &Parser{roots: defaultRoots(), prov: rootProvenance} }

// NewWithRoot returns a parser rooted at root.
func NewWithRoot(root string) *Parser { return NewWithRoots(root) }

// NewWithRoots returns a parser over exactly the given roots: each one is a
// database directory, a cli directory, a ZCode home, or the database file. An
// empty list stays empty — it never falls back to the built-in defaults, so an
// explicit request cannot silently read ~/.zcode.
func NewWithRoots(roots ...string) *Parser {
	return &Parser{roots: cleanRoots(roots)}
}

// cleanRoots trims, cleans and deduplicates roots, dropping blank entries.
func cleanRoots(roots []string) []string {
	seen := make(map[string]bool, len(roots))
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		r = filepath.Clean(r)
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

// rootProvenance reports ZCode's default roots and where each came from.
// MYTOKEN_ZCODE_DIRS replaces the whole set; otherwise the root is the platform
// default ~/.zcode/cli/db. ZCODE_HOME is not consulted: no first-party source
// documents it (see the package comment).
func rootProvenance() ([]harness.Root, error) {
	if v, ok := harness.Getenv(envDirs); ok {
		dirs, err := harness.SplitEnvList(envDirs, v)
		if err != nil {
			return nil, err
		}
		return harness.EnvRoots(envDirs, dirs...), nil
	}
	return harness.BuiltinRoots(builtinRoots()...), nil
}

// builtinRoots is the documented ledger directory. ZCode resolves its home
// through the user's home directory on every platform, so the same join applies
// everywhere; an unknown home leaves no root at all rather than a relative one.
func builtinRoots() []string {
	home := harness.Home()
	if home == "" {
		return nil
	}
	return []string{filepath.Join(home, zcodeDirName, cliDirName, dbDirName)}
}

// defaultRoots returns the paths of rootProvenance. A malformed explicit
// override yields no roots: it is never replaced by the built-in default.
func defaultRoots() []string {
	roots, err := rootProvenance()
	if err != nil {
		return nil
	}
	return harness.RootPaths(roots)
}

// RootProvenance implements harness.Provenance.
func (p *Parser) RootProvenance() ([]harness.Root, error) {
	if p.prov == nil {
		return harness.ExplicitRoots(p.roots...), nil
	}
	return p.prov()
}

func (p *Parser) Harness() model.Harness { return model.ZCode }

// WithRoots implements harness.Rooted. It returns a fresh parser over exactly
// the given roots; an explicit empty list stays empty instead of falling back
// to this harness's default roots.
func (p *Parser) WithRoots(roots ...string) harness.Parser {
	for _, r := range roots {
		if strings.TrimSpace(r) != "" {
			return NewWithRoots(roots...)
		}
	}
	return &Parser{}
}

// Roots returns the configured roots; missing ones are ignored.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

// Discover lists the usage ledger under every root. A root may be the ZCode
// home (~/.zcode), the cli directory (~/.zcode/cli), the database directory
// (~/.zcode/cli/db) or the database file itself. Only the documented ledger
// name is looked for, so an unrelated SQLite file next to it is never read.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	seen := map[string]bool{}
	var out []harness.Source
	add := func(path string) {
		if path == "" || seen[path] {
			return
		}
		st, err := os.Stat(path)
		if err != nil || st.IsDir() {
			return
		}
		seen[path] = true
		out = append(out, harness.Source{Path: path, Kind: kindSQLite})
	}
	for _, root := range p.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		root = filepath.Clean(root)
		st, err := os.Stat(root)
		if err != nil {
			continue
		}
		if !st.IsDir() {
			// An override may point straight at the database file.
			if strings.EqualFold(filepath.Base(root), dbName) {
				add(root)
			}
			continue
		}
		add(filepath.Join(root, dbName))                        // the database directory
		add(filepath.Join(root, dbDirName, dbName))             // the cli directory
		add(filepath.Join(root, cliDirName, dbDirName, dbName)) // the ZCode home
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Parse reads the usage a single ledger added since cur.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if err := ctx.Err(); err != nil {
		return harness.Batch{Next: cur}, err
	}
	if _, err := os.Stat(src.Path); err != nil {
		return harness.Batch{Next: cur}, nil
	}
	sig, err := signatureOf(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	fp, err := fingerprint(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}

	state := decodeState(cur)
	next := func() harness.Cursor { return cursorFor(sig, fp, state) }
	if cur.Fingerprint != "" && cur.Fingerprint == fp && cur.Size == sig.size &&
		cur.ModTime.Equal(sig.mod) && state.Version == cursorVersion && !state.Drain {
		// Unchanged database and no backlog: nothing to read, and the cursor is
		// untouched. A recorded backlog deliberately skips this shortcut, because
		// the unchanged file is exactly the case that must keep reading.
		return harness.Batch{Next: cur}, nil
	}

	db, err := openReadOnly(src.Path)
	if err != nil {
		// Locked or unreadable: retry on the next scan instead of recording a
		// signature that would suppress the retry.
		return harness.Batch{Next: cur}, nil
	}
	defer db.Close()

	tables, err := tableColumns(ctx, db)
	if err != nil {
		// Probing the schema failed: this says nothing about the ledger, so the
		// cursor must not move. Reporting the error keeps the scan retryable.
		if cerr := ctx.Err(); cerr != nil {
			return harness.Batch{Next: cur}, cerr
		}
		return harness.Batch{Next: cur}, fmt.Errorf("zcode: probe schema of %s: %w", src.Path, err)
	}
	usage, ok := tables[usageTable]
	if !ok || !hasColumns(usage, requiredUsageColumns...) {
		// Not a ZCode ledger (or one without the documented columns): advance
		// the cursor so the file is not reopened until it changes. There is no
		// row window here to keep draining, so a backlog recorded for an earlier
		// incarnation of the file no longer applies; the watermark, the in-flight
		// set and the anchor stay, and a later ledger with the same rowids can
		// still resume from them.
		state.Drain = false
		return harness.Batch{Next: next()}, nil
	}

	acc, err := collect(ctx, db, tables, usage, &state)
	if err != nil {
		// A failed read keeps the caller's cursor: advancing the watermark past
		// rows the failed statement never returned would lose them for good. The
		// failure is also reported, because a row that cannot be read at all (for
		// example a value that does not scan) would otherwise hold the ledger
		// silent forever while every scan advanced past it. A transient failure
		// only costs the caller one reported error, since the cursor is intact.
		if cerr := ctx.Err(); cerr != nil {
			return harness.Batch{Next: cur}, cerr
		}
		return harness.Batch{Next: cur}, fmt.Errorf("zcode: read usage from %s: %w", src.Path, err)
	}
	return acc.batch(next()), nil
}

// decodeState reads the per-database cursor state. A cursor written by another
// parser revision (or a damaged one) is ignored, which forces a rescan.
func decodeState(cur harness.Cursor) zcodeState {
	if cur.Extra == "" {
		return zcodeState{}
	}
	var loaded zcodeState
	if err := json.Unmarshal([]byte(cur.Extra), &loaded); err != nil || loaded.Version != cursorVersion {
		return zcodeState{}
	}
	return loaded
}

// collect reads the rows a scan has not seen yet: completed ones are counted,
// in-flight ones are remembered and revisited on the next scan. It advances
// state as it goes and returns an error only when the caller must keep its
// cursor and retry later.
func collect(ctx context.Context, db *sql.DB, tables map[string]map[string]bool, usage map[string]bool, state *zcodeState) (*accumulator, error) {
	hasRowid, err := usageHasRowid(ctx, db, usageTable)
	if err != nil {
		return nil, err
	}
	dirs := readSessionDirs(ctx, db, tables)
	acc := newAccumulator(dirs)

	if !hasRowid {
		// A WITHOUT ROWID table has no watermark to resume from: read every
		// completed row again and rely on the stable DedupKey for idempotence.
		// The full read has no window and therefore never leaves a backlog.
		state.forgetResumePoint()
		state.Drain = false
		query, args := fullScanQuery(usage)
		if err := readUsage(ctx, db, query, args, func(row usageRow) {
			acc.usage(row.event(dirs))
		}); err != nil {
			return nil, err
		}
		return acc, nil
	}

	max, err := maxRowid(ctx, db)
	if err != nil {
		return nil, err
	}
	if max < state.Rowid {
		// The row ids went backwards because the table was rebuilt: rescan
		// everything and rely on the stable DedupKey for idempotence. The tracked
		// ids name rows of the previous table, so they go with the watermark.
		state.forgetResumePoint()
	}
	if state.Rowid > 0 {
		same, err := watermarkRowStillThere(ctx, db, usage, state.Rowid, state.Anchor)
		if err != nil {
			return nil, err
		}
		if !same {
			// The row the watermark names is gone, or its rowid now holds a
			// different row. `MAX(rowid) == watermark` does not prove the rows at
			// or below it were examined: SQLite hands a freed rowid straight back
			// out when it is the table's highest one, so a ledger deleted and
			// refilled in place can carry a watermark that looks current while
			// every row below it is new. Only the row itself settles it. Start
			// over; the stable DedupKey makes the rescan idempotent.
			state.forgetResumePoint()
		}
	}

	// Re-read the in-flight rowids first. They all sit at or below the watermark,
	// so the two reads below stay disjoint while the scan still emits rows in
	// ascending rowid order.
	pending := make([]int64, 0, len(state.Pending))
	for _, chunk := range pendingChunks(state.Pending) {
		query, args := pendingQuery(usage, chunk)
		live := make(map[int64]bool, len(chunk))
		err := readUsage(ctx, db, query, args, func(row usageRow) {
			live[row.rowid] = true
			if isCompleted(row) {
				acc.usage(row.event(dirs))
				return
			}
			if keepPending(row) {
				pending = append(pending, row.rowid)
			}
		})
		if err != nil {
			return nil, err
		}
		for _, id := range chunk {
			if !live[id] {
				// The row is gone from the ledger. Its rowid stays tracked: only
				// a tracked id is read below the watermark, and SQLite may reuse a
				// freed rowid for a later insert.
				pending = append(pending, id)
			}
		}
	}

	// Then the rows never examined before, bounded so one scan stays cheap. The
	// watermark advances to the last row examined and the anchor records which
	// row that was, so the next scan continues where this one stopped and can
	// still tell whether the row it resumes from is the row it left behind.
	watermark := state.Rowid
	anchor := state.Anchor
	query, args := newRowsQuery(usage, state.Rowid)
	err = readUsage(ctx, db, query, args, func(row usageRow) {
		if row.rowid > watermark {
			watermark = row.rowid
			anchor = dedupKey(row.id.String, row.sessionID.String, row.startedAt.Int64, row.rowid)
		}
		if isCompleted(row) {
			acc.usage(row.event(dirs))
			return
		}
		if keepPending(row) {
			pending = append(pending, row.rowid)
		}
	})
	if err != nil {
		return nil, err
	}

	state.Rowid = watermark
	state.Pending = normalizePending(pending)
	state.Anchor = anchor
	// A read that stopped on the row window left rows behind it. The next scan
	// must open the database again even though the file has not changed, and this
	// flag is the only thing that tells that backlog apart from a drained
	// ledger; once the read reaches the last row, the shortcut is restored. max
	// comes from the same snapshot the read above walked, so a row committed
	// afterwards is either seen now or caught by the changed signature.
	state.Drain = watermark < max
	return acc, nil
}

// isCompleted reports whether a row holds countable usage. status is a
// documented enum, compared exactly: "COMPLETED" is not "completed" and must
// not count.
func isCompleted(row usageRow) bool {
	return strings.TrimSpace(row.status.String) == completedStatus
}

// keepPending reports whether a non-completed row is worth revisiting: only a
// row that may still change, and only one whose rowid can be named again (a
// WITHOUT ROWID table has none).
func keepPending(row usageRow) bool {
	return row.rowid > 0 && !terminalStatuses[strings.TrimSpace(row.status.String)]
}

// readUsage runs one usage query and visits every row it returns. A failure is
// returned, never swallowed: the caller keeps its cursor and retries the whole
// scan instead of recording a partial read.
func readUsage(ctx context.Context, db *sql.DB, query string, args []any, visit func(usageRow)) error {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		row, err := scanUsage(rows)
		if err != nil {
			// Do not skip: a row this statement cannot read must leave the whole
			// statement unread, so the caller keeps its cursor and a later scan
			// can read the row again instead of stepping over it.
			return fmt.Errorf("scan usage row: %w", err)
		}
		visit(row)
	}
	return rows.Err()
}

// watermarkRowStillThere reports whether the row the watermark names is still
// the row the cursor recorded there. anchor is the DedupKey of that row, so an
// unchanged row needs no reset while a vanished or replaced one does. An empty
// anchor never matches: a cursor that cannot name its row is treated as unknown
// and rescanned.
func watermarkRowStillThere(ctx context.Context, db *sql.DB, cols map[string]bool, rowid int64, anchor string) (bool, error) {
	found := false
	same := false
	query, args := anchorQuery(cols, rowid)
	if err := readUsage(ctx, db, query, args, func(row usageRow) {
		found = true
		same = dedupKey(row.id.String, row.sessionID.String, row.startedAt.Int64, row.rowid) == anchor
	}); err != nil {
		return false, err
	}
	return found && same, nil
}

// ---------------------------------------------------------------------------
// row mapping
// ---------------------------------------------------------------------------

// usageRow is one selected model_usage row. Optional columns scan as NULL when
// the column is absent from the database.
type usageRow struct {
	rowid      int64
	id         sql.NullString
	sessionID  sql.NullString
	startedAt  sql.NullInt64
	modelID    sql.NullString
	status     sql.NullString
	input      sql.NullInt64
	output     sql.NullInt64
	cacheRead  sql.NullInt64
	cacheWrite sql.NullInt64
	provider   sql.NullString
}

func scanUsage(rows *sql.Rows) (usageRow, error) {
	var r usageRow
	err := rows.Scan(&r.rowid, &r.id, &r.sessionID, &r.startedAt, &r.modelID, &r.status,
		&r.input, &r.output, &r.cacheRead, &r.cacheWrite, &r.provider)
	return r, err
}

// event maps a completed row onto a usage event. ProjectPath comes from the
// session's directory when the session table knows it, and stays empty
// otherwise — never guessed from the database location.
func (r usageRow) event(dirs map[string]string) model.UsageEvent {
	ts := zcodeTime(r.startedAt.Int64)
	sessionID := r.sessionID.String
	return model.UsageEvent{
		Harness:     model.ZCode,
		DedupKey:    dedupKey(r.id.String, sessionID, r.startedAt.Int64, r.rowid),
		SessionID:   sessionID,
		ProjectPath: dirs[sessionID],
		Timestamp:   ts,
		Model:       r.modelID.String,
		Provider:    r.provider.String,
		Tokens:      tokenSplit(r.input.Int64, r.output.Int64, r.cacheRead.Int64, r.cacheWrite.Int64),
	}
}

// dedupKey is stable for a row across scans: the ledger's own row id when it
// has one, else a digest of the fields that identify the row.
func dedupKey(id, sessionID string, startedAt, rowid int64) string {
	if id != "" {
		return "zcode:" + id
	}
	sum := sha256.Sum256([]byte(sessionID + "\x00" + strconv.FormatInt(startedAt, 10) + "\x00" + strconv.FormatInt(rowid, 10)))
	return "zcode:row:" + hex.EncodeToString(sum[:8])
}

// zcodeTime normalises started_at, which is epoch milliseconds, and scales a
// seconds value defensively. A missing or non-positive value stays unknown.
func zcodeTime(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	if v < 100_000_000_000 {
		v *= 1000
	}
	return time.UnixMilli(v).UTC()
}

// tokenSplit turns the ledger's inclusive input_tokens into MyToken's four
// classes: the cache buckets are subtracted from input_tokens, a bucket larger
// than the total is clamped to it, and a negative value counts as zero.
func tokenSplit(input, output, read, write int64) model.Tokens {
	input = clampInt(input)
	read = clampInt(read)
	write = clampInt(write)
	if read > input {
		read = input
	}
	rest := input - read
	if write > rest {
		write = rest
	}
	return model.Tokens{
		Input:      input - read - write,
		Output:     clampInt(output),
		CacheRead:  read,
		CacheWrite: write,
	}
}

func clampInt(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// normalizePending sorts and deduplicates the in-flight rowids kept in the
// cursor. It deliberately does not bound them: an id in this set is the only
// reason its row, which sits at or below the watermark, is ever read again, so
// dropping one turns a turn that completes later into usage that is never
// counted. Ids leave the set when their row is observed terminal or completed —
// not by eviction.
func normalizePending(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := ids[:0]
	var prev int64 = -1
	for _, id := range ids {
		if id <= 0 || id == prev {
			continue
		}
		prev = id
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---------------------------------------------------------------------------
// cursor
// ---------------------------------------------------------------------------

// zcodeState is the per-database cursor state stored in Cursor.Extra.
//
// Rowid, Pending and Anchor describe one resume point and move together: the
// watermark is meaningless without the row it names, and the tracked in-flight
// ids only name rows of the ledger that watermark belongs to. Anything that
// invalidates the resume point — a rebuilt table, a replaced row at the
// watermark — must drop all three, because a stale anchor read against a new
// table is an assertion about the wrong rows.
type zcodeState struct {
	// Version invalidates a cursor written by an older parser revision.
	Version int `json:"v"`
	// Rowid is the highest rowid already examined.
	Rowid int64 `json:"r,omitempty"`
	// Pending holds the rowids of turns seen in flight at or below the
	// watermark; a scan re-reads them so a turn the ledger updates in place is
	// counted. An id only leaves the set once its row was observed completed or
	// terminal, or the table was rebuilt: an id discarded any earlier can never
	// be rediscovered, so its row would be lost for good.
	Pending []int64 `json:"p,omitempty"`
	// Anchor is the DedupKey of the row at Rowid. It lets a scan check that the
	// row it wants to resume from is still the row it left behind, which a bare
	// watermark cannot say: SQLite reuses a freed rowid.
	Anchor string `json:"a,omitempty"`
	// Drain records that the last scan stopped on the maxRowsPerParse window,
	// with rows beyond the watermark still unread. It suspends the
	// unchanged-database shortcut until a scan reaches the end of the ledger, so
	// a ledger larger than one window is drained even while nothing writes to it.
	Drain bool `json:"d,omitempty"`
}

// forgetResumePoint drops the watermark, the rows it named and the safe point to
// continue from, so the next read starts at the beginning of the ledger. Drain
// is not part of the resume point: it is recomputed from what the resumed read
// reaches, and the one caller that has no window to recompute it from clears it
// itself.
func (s *zcodeState) forgetResumePoint() {
	s.Rowid = 0
	s.Pending = nil
	s.Anchor = ""
}

func stateOf(s zcodeState) string {
	s.Version = cursorVersion
	raw, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(raw)
}

func cursorFor(sig dbSignature, fp string, state zcodeState) harness.Cursor {
	return harness.Cursor{
		Offset:      0,
		Size:        sig.size,
		ModTime:     sig.mod,
		Fingerprint: fp,
		Extra:       stateOf(state),
	}
}

// ---------------------------------------------------------------------------
// database
// ---------------------------------------------------------------------------

type dbSignature struct {
	size int64
	mod  time.Time
}

// signatureOf combines the database's size and mtime with its write-ahead log's
// and shared-memory file's, so a commit that only touches -wal still changes
// the signature.
func signatureOf(path string) (dbSignature, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return dbSignature{}, err
	}
	sig := dbSignature{size: fi.Size(), mod: fi.ModTime()}
	for _, suffix := range []string{"-wal", "-shm"} {
		w, err := os.Stat(path + suffix)
		if err != nil {
			continue
		}
		sig.size += w.Size()
		if w.ModTime().After(sig.mod) {
			sig.mod = w.ModTime()
		}
	}
	return sig, nil
}

// openReadOnly opens path without ever taking a write lock: mode=ro plus
// PRAGMA query_only, with a busy timeout.
//
// It never falls back to immutable=1. That fallback made a database whose
// write-ahead log cannot be opened look readable while it silently served the
// last checkpointed snapshot: every row still in the log was lost, and because
// the sidecar's size and mtime (and so the cursor signature) survive a
// permission change, the loss became permanent. Failing here instead lets Parse
// keep the cursor and retry the whole read.
func openReadOnly(path string) (*sql.DB, error) {
	return openWithPragmas(path, "mode=ro")
}

func openWithPragmas(path string, params ...string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	q := url.Values{"_pragma": {
		fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS),
		"query_only(1)",
	}}
	for _, p := range params {
		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 {
			continue
		}
		q.Set(kv[0], kv[1])
	}
	db, err := sql.Open("sqlite", sqlitedsn.URI(abs, q.Encode()))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// tableColumns reports every table (or view) and its columns, with column names
// lower-cased for case-insensitive lookup. A failed query is returned as an
// error: it means the schema could not be probed, which is not the same as a
// database that has no usage columns, and the caller must not advance its cursor
// over rows it never managed to look at.
func tableColumns(ctx context.Context, db *sql.DB) (map[string]map[string]bool, error) {
	out := map[string]map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type IN ('table','view')`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for _, name := range names {
		cols, err := tableInfo(ctx, db, name)
		if err != nil {
			return nil, fmt.Errorf("table_info(%s): %w", name, err)
		}
		out[name] = cols
	}
	return out, nil
}

// tableInfo reads the columns of one table or view.
func tableInfo(ctx context.Context, db *sql.DB, name string) (map[string]bool, error) {
	cols := map[string]bool{}
	rs, err := db.QueryContext(ctx, `PRAGMA table_info(`+quoteIdent(name)+`)`)
	if err != nil {
		return nil, err
	}
	for rs.Next() {
		var (
			cid, notnull, pk int
			col, ctype       string
			dflt             sql.NullString
		)
		if err := rs.Scan(&cid, &col, &ctype, &notnull, &dflt, &pk); err != nil {
			rs.Close()
			return nil, err
		}
		cols[strings.ToLower(col)] = true
	}
	if err := rs.Err(); err != nil {
		rs.Close()
		return nil, err
	}
	rs.Close()
	return cols, nil
}

// hasColumns reports whether every wanted column exists.
func hasColumns(cols map[string]bool, wanted ...string) bool {
	for _, w := range wanted {
		if !cols[w] {
			return false
		}
	}
	return true
}

// pickColumn returns the first present alias, or "" when none is.
func pickColumn(cols map[string]bool, aliases []string) string {
	for _, a := range aliases {
		if cols[a] {
			return a
		}
	}
	return ""
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// sqliteErrError is SQLITE_ERROR, the code SQLite reports when a statement names
// a column that does not exist.
const sqliteErrError = 1

// usageHasRowid reports whether the usage table has a rowid: a WITHOUT ROWID
// table (or a view) rejects the reference with "no such column: rowid", an empty
// rowid table answers with no rows. That message is a property of the schema and
// is reported as false; any other failure is a failed probe and is returned,
// because guessing "no rowid" there would hide a read that never happened.
func usageHasRowid(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var probe int64
	err := db.QueryRowContext(ctx, "SELECT rowid FROM "+quoteIdent(table)+" LIMIT 1").Scan(&probe)
	switch {
	case err == nil, errors.Is(err, sql.ErrNoRows):
		return true, nil
	case missingRowid(err):
		return false, nil
	default:
		return false, err
	}
}

// missingRowid reports whether err is SQLite's "no such column: rowid", the only
// way a WITHOUT ROWID table rejects a rowid reference.
func missingRowid(err error) bool {
	var serr *sqlite.Error
	if !errors.As(err, &serr) || serr.Code() != sqliteErrError {
		return false
	}
	return strings.Contains(strings.ToLower(serr.Error()), "no such column")
}

// maxRowid reports the highest rowid in the usage table, or 0 when it is empty.
func maxRowid(ctx context.Context, db *sql.DB) (int64, error) {
	var max int64
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(rowid),0) FROM "+quoteIdent(usageTable)).Scan(&max); err != nil {
		return 0, err
	}
	return max, nil
}

// selectColumns renders the projection every usage query shares: the rowid (or a
// constant 0 for a WITHOUT ROWID table, which has none), the required columns,
// and the optional cache/provider columns replaced by a constant when the schema
// lacks them, so one read path covers every schema.
func selectColumns(hasRowid bool, cols map[string]bool) string {
	first := "0"
	if hasRowid {
		first = "rowid"
	}
	expr := func(col string, absent string) string {
		if col == "" {
			return absent
		}
		return quoteIdent(col)
	}
	var b strings.Builder
	b.WriteString("SELECT " + first)
	for _, c := range requiredUsageColumns {
		b.WriteString(", " + quoteIdent(c))
	}
	b.WriteString(", " + expr(pickColumn(cols, cacheReadColumns), "0"))
	b.WriteString(", " + expr(pickColumn(cols, cacheWriteColumns), "0"))
	b.WriteString(", " + expr(pickColumn(cols, providerColumns), "NULL"))
	b.WriteString(" FROM " + quoteIdent(usageTable))
	return b.String()
}

// newRowsQuery selects the rows never examined before — those past the watermark
// — in ascending rowid order, bounded at maxRowsPerParse rows. The watermark
// advances to the last row this read returns, so the bound strands nothing: the
// next scan resumes there. The caller records a backlog for the rows this read
// did not reach, so an unchanged ledger is still reopened until it is drained.
func newRowsQuery(cols map[string]bool, watermark int64) (string, []any) {
	return selectColumns(true, cols) + " WHERE rowid > ? ORDER BY rowid LIMIT " + strconv.Itoa(maxRowsPerParse), []any{watermark}
}

// anchorQuery selects the single row at rowid: the row the watermark names.
func anchorQuery(cols map[string]bool, rowid int64) (string, []any) {
	return selectColumns(true, cols) + " WHERE rowid = ?", []any{rowid}
}

// pendingQuery selects one bounded chunk of the tracked in-flight rowids. Every
// tracked rowid is at or below the watermark, so this read and the watermark
// read cover disjoint rowid ranges: a row is never examined twice in one scan.
func pendingQuery(cols map[string]bool, ids []int64) (string, []any) {
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	return selectColumns(true, cols) + " WHERE rowid IN (" + placeholders(len(ids)) + ")", args
}

// pendingChunks cuts the tracked in-flight rowids into bounded chunks: every
// rowid appears in exactly one chunk, and no chunk asks a single statement to
// name more than pendingBatch of them. The whole set is still revisited, one
// cheap statement at a time, so neither the oldest nor the newest end of it is
// starved.
func pendingChunks(ids []int64) [][]int64 {
	if len(ids) == 0 {
		return nil
	}
	out := make([][]int64, 0, (len(ids)+pendingBatch-1)/pendingBatch)
	for start := 0; start < len(ids); start += pendingBatch {
		end := start + pendingBatch
		if end > len(ids) {
			end = len(ids)
		}
		out = append(out, ids[start:end])
	}
	return out
}

// fullScanQuery reads every completed row of a table that has no rowid. Such a
// table has no watermark, and its primary key is not a cursor either: ledger ids
// may be arbitrary and arrive out of order, so `id > last` would skip rows too.
// Reading it in full is the only correct option — the stable DedupKey makes the
// repeated read idempotent — whereas a LIMIT there returns the same prefix on
// every scan and silently drops every row past the cap.
func fullScanQuery(cols map[string]bool) (string, []any) {
	return selectColumns(false, cols) + " WHERE " + quoteIdent("status") + " = ? ORDER BY " + quoteIdent("id"), []any{completedStatus}
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// readSessionDirs maps session id to the directory the session ran in. A
// database without the session table (or without those columns) still counts
// usage; only ProjectPath stays unknown.
func readSessionDirs(ctx context.Context, db *sql.DB, tables map[string]map[string]bool) map[string]string {
	out := map[string]string{}
	cols, ok := tables[sessionTable]
	if !ok || !cols["id"] || !cols["directory"] {
		return out
	}
	rows, err := db.QueryContext(ctx, "SELECT "+quoteIdent("id")+", "+quoteIdent("directory")+" FROM "+quoteIdent(sessionTable))
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, dir sql.NullString
		if err := rows.Scan(&id, &dir); err != nil {
			continue
		}
		if id.String != "" {
			out[id.String] = strings.TrimSpace(dir.String)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// accumulator
// ---------------------------------------------------------------------------

// accumulator collects one batch, keyed by DedupKey so a rescan cannot emit the
// same row twice, and derives each session's window from the rows it saw.
type accumulator struct {
	dirs map[string]string

	evs   map[string]model.UsageEvent
	order []string
	metas map[string]model.SessionMeta
	mord  []string
}

func newAccumulator(dirs map[string]string) *accumulator {
	return &accumulator{
		dirs:  dirs,
		evs:   map[string]model.UsageEvent{},
		metas: map[string]model.SessionMeta{},
	}
}

func (a *accumulator) usage(ev model.UsageEvent) {
	if _, ok := a.evs[ev.DedupKey]; ok {
		return
	}
	a.evs[ev.DedupKey] = ev
	a.order = append(a.order, ev.DedupKey)
	if ev.SessionID == "" {
		return
	}
	meta, ok := a.metas[ev.SessionID]
	if !ok {
		meta = model.SessionMeta{
			Harness:   model.ZCode,
			SessionID: ev.SessionID,
			Project:   a.dirs[ev.SessionID],
			StartedAt: ev.Timestamp,
			UpdatedAt: ev.Timestamp,
		}
		a.mord = append(a.mord, ev.SessionID)
	}
	if !ev.Timestamp.IsZero() {
		if meta.StartedAt.IsZero() || ev.Timestamp.Before(meta.StartedAt) {
			meta.StartedAt = ev.Timestamp
		}
		if ev.Timestamp.After(meta.UpdatedAt) {
			meta.UpdatedAt = ev.Timestamp
		}
	}
	a.metas[ev.SessionID] = meta
}

func (a *accumulator) batch(next harness.Cursor) harness.Batch {
	events := make([]model.UsageEvent, 0, len(a.order))
	for _, k := range a.order {
		events = append(events, a.evs[k])
	}
	sort.SliceStable(events, func(i, j int) bool {
		if !events[i].Timestamp.Equal(events[j].Timestamp) {
			return events[i].Timestamp.Before(events[j].Timestamp)
		}
		return events[i].DedupKey < events[j].DedupKey
	})
	metas := make([]model.SessionMeta, 0, len(a.mord))
	for _, id := range a.mord {
		metas = append(metas, a.metas[id])
	}
	sort.SliceStable(metas, func(i, j int) bool { return metas[i].SessionID < metas[j].SessionID })
	return harness.Batch{Events: events, Sessions: metas, Next: next}
}

func fingerprint(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.CopyN(h, f, fingerprintBytes); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
