// Package minimax reads MiniMax Code's local token ledger.
//
// MiniMax Code keeps its runtime state in a sqlite database under its data
// directory: <data dir>/v2/sqlite/runtime-state.sqlite, table
// local_runtime_token_usage (packages/local-runtime-v2/src/infra/db/client.ts
// and .../db/schema/usage.ts in https://github.com/MiniMax-AI/minimax-code).
// The runtime writes one row per committed assistant message that carries
// provider usage (…/service/session-system/usage/pi-usage.ts), so the ledger is
// append-only and each row maps 1:1 onto a model.UsageEvent.
//
// Everything here is offline: the parser never opens a network connection and
// never reads credentials. It queries one table, read-only, and selects only the
// usage columns.
//
// # Roots
//
// Roots resolve in this order, the first that is set winning:
//
//  1. MYTOKEN_MINIMAX_DIRS — a list of data directories (mytoken's own override).
//  2. MINIMAX_DATA_DIR — MiniMax Code's public data-directory variable.
//  3. MAVIS_DATA_DIR — the same variable under the legacy "mavis" name.
//  4. ~/.minimax and ~/.mavis — the built-in primary and legacy data directories.
//
// The runtime also supports per-profile data directories named
// `<basename>-<profile>` (for example ~/.minimax-work), but the active profile is
// chosen at process start (--profile, __MAVIS_RUNTIME_PROFILE, or git-branch
// inference) and is not recorded on disk, so it cannot be discovered offline.
// Name profile directories explicitly through MYTOKEN_MINIMAX_DIRS instead.
//
// # What the ledger does not carry
//
// The schema records no provider, base URL, project path, request id, git
// branch or compaction marker, and no message text. Those fields stay empty
// rather than being inferred. A NULL or blank model becomes "unknown"; a NULL,
// negative or non-finite cost is unknown and stays nil, while a finite zero is
// kept as a trustworthy $0: the official writer only stores 0 when the provider
// reported a zero cost. Timestamps are Unix milliseconds; a row without a usable
// timestamp keeps the zero time, which the store treats as unknown.
//
// A database that is readable but does not carry the ledger columns this parser
// needs is not claimed as supported: it contributes no events, and its read
// position is remembered so a scan does not reopen it on every pass. A database
// that cannot be opened or described is different — nothing was read, so the
// caller's cursor is returned unchanged and the next round retries the file.
//
// # Wiring
//
// Registration is wired by the caller (internal/app): import this package for
// its side effect, and give it a harness id and display name in
// internal/model. This package defines the id as Harness so it can be used
// before that constant exists.
package minimax

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/sqlitedsn"
)

const (
	// Harness identifies MiniMax Code's logs. internal/model gains the matching
	// constant when the registry is wired.
	Harness model.Harness = "minimax-code"

	// kindSQLite is the harness.Source.Kind for a sqlite ledger.
	kindSQLite = "sqlite"

	// usageTable is the only table this parser queries.
	usageTable = "local_runtime_token_usage"

	// dbName is the ledger's file name, dbRelPath its location under a data dir.
	dbName    = "runtime-state.sqlite"
	dbRelPath = "v2/sqlite/runtime-state.sqlite"

	// dirsEnv overrides the root list; the next two mirror MiniMax Code's own
	// public data-directory variables, primary name first.
	dirsEnv           = "MYTOKEN_MINIMAX_DIRS"
	primaryDataDirEnv = "MINIMAX_DATA_DIR"
	legacyDataDirEnv  = "MAVIS_DATA_DIR"

	// primaryDirName and legacyDirName are the built-in data directories.
	primaryDirName = ".minimax"
	legacyDirName  = ".mavis"

	// unknownModel marks a row whose model column is NULL or blank.
	unknownModel = "unknown"

	// fingerprintBytes is how much of the database the coalescing fingerprint
	// covers; busyTimeoutMS bounds a lock wait on a live runtime.
	fingerprintBytes = 4096
	busyTimeoutMS    = 5000

	// openTimeout bounds opening a database the runtime may be writing.
	openTimeout = 5 * time.Second

	// SQLite storage-class affinities used by the schema probe.
	affinityInteger = "INTEGER"
	affinityReal    = "REAL"
	affinityText    = "TEXT"
)

// Parser implements harness.Parser for MiniMax Code.
type Parser struct {
	roots []string
	prov  func() ([]harness.Root, error)
}

// New returns a parser for the standard MiniMax Code locations.
func New() *Parser {
	return &Parser{roots: DefaultRoots(), prov: rootProvenance}
}

// NewWithRoots returns a parser reading the given roots. An empty list reads
// nothing; it never falls back to the defaults.
func NewWithRoots(roots ...string) *Parser {
	return &Parser{roots: cleanRoots(roots)}
}

func init() { harness.Register(New()) }

// rootProvenance resolves the roots from the environment as it is now.
func rootProvenance() ([]harness.Root, error) {
	if dirs, ok := harness.Getenv(dirsEnv); ok {
		paths, err := harness.SplitEnvList(dirsEnv, dirs)
		if err != nil {
			return nil, err
		}
		return harness.EnvRoots(dirsEnv, paths...), nil
	}
	for _, env := range []string{primaryDataDirEnv, legacyDataDirEnv} {
		if dir, ok := harness.Getenv(env); ok {
			return harness.EnvRoots(env, dir), nil
		}
	}
	return harness.BuiltinRoots(builtinRoots()...), nil
}

// builtinRoots returns the primary data directory and its legacy compat name.
func builtinRoots() []string {
	home := harness.Home()
	if home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, primaryDirName),
		filepath.Join(home, legacyDirName),
	}
}

// DefaultRoots returns the paths the default parser watches. A malformed
// override yields no roots; callers that must report it use RootProvenance.
func DefaultRoots() []string {
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

// Harness identifies this parser's logs.
func (p *Parser) Harness() model.Harness { return Harness }

// WithRoots implements harness.Rooted. An explicit list replaces the defaults,
// and an explicit empty list stays empty.
func (p *Parser) WithRoots(roots ...string) harness.Parser {
	cleaned := cleanRoots(roots)
	if len(cleaned) == 0 {
		return &Parser{}
	}
	return &Parser{roots: cleaned}
}

// Roots returns a copy of the configured roots.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

// Discover finds the runtime ledger under every configured root. A missing or
// unreadable root is skipped, never an error, and a root that names the
// database itself is accepted. The legacy directory is often a symlink to the
// primary one, so candidates are deduplicated by file identity.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var (
		out  []harness.Source
		seen []os.FileInfo
	)
	add := func(path string) {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			return
		}
		for _, prev := range seen {
			if os.SameFile(prev, info) {
				return
			}
		}
		seen = append(seen, info)
		out = append(out, harness.Source{Path: path, Kind: kindSQLite})
	}
	for _, root := range p.roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		info, err := os.Stat(root)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			if filepath.Base(root) == dbName {
				add(root)
			}
			continue
		}
		add(filepath.Join(root, filepath.FromSlash(dbRelPath)))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Parse reads the ledger from cur to the current end in one read-only
// transaction: the row watermark and the rows after it are read from the same
// snapshot. A file that has not changed since cur is skipped.
//
// Every failure is inert: a cancelled context, a ledger that cannot be opened, a
// transaction that cannot start, and a schema probe that errors all report
// without moving the caller's cursor, so the next round retries the same work.
// Only a completed read advances the position — including the deliberate case of
// a database that is readable but does not carry the ledger columns.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if err := ctx.Err(); err != nil {
		return keepCursor(cur), err
	}
	sig, err := signatureOf(src.Path)
	if err != nil {
		return keepCursor(cur), nil
	}
	fp, err := fingerprint(src.Path)
	if err != nil {
		return keepCursor(cur), nil
	}
	if cur.Fingerprint != "" && cur.Fingerprint == fp && cur.Size == sig.size && cur.ModTime.Equal(sig.mod) {
		return keepCursor(cur), nil
	}
	state := decodeState(cur.Extra)
	db, err := openReadOnly(src.Path)
	if err != nil {
		return keepCursor(cur), nil
	}
	defer db.Close()

	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return keepCursor(cur), nil
	}
	defer tx.Rollback() //nolint:errcheck // read-only

	cols, err := tableColumns(ctx, tx, usageTable)
	if err != nil {
		// "Could not read the schema" is not "this ledger has no usage table":
		// a probe that could not answer must not mark the file as read.
		return keepCursor(cur), nil
	}
	if !supportedSchema(cols) {
		return harness.Batch{Next: cursorFor(cur, sig, fp, state)}, nil
	}

	from := int64(0)
	if state.ID > 0 {
		ok, err := watermarkIntact(ctx, tx, cols, state)
		if err != nil {
			return keepCursor(cur), fmt.Errorf("minimax: verify read position: %w", err)
		}
		if ok {
			from = state.ID
		} else {
			// The ledger was replaced or rewritten under the same path: read it
			// again from the start and rely on DedupKey for idempotence.
			state = ledgerState{}
		}
	}

	query := "SELECT " + selectList(cols) + " FROM " + quoteIdent(usageTable)
	var args []any
	if from > 0 {
		query += " WHERE id > ?"
		args = append(args, from)
	}
	query += " ORDER BY id ASC"

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return keepCursor(cur), fmt.Errorf("minimax: read %s: %w", usageTable, err)
	}
	defer rows.Close()

	var (
		events    []model.UsageEvent
		sessions  = map[string]*sessionAggregate{}
		maxID     = from
		watermark ledgerRow
		last      bool
	)
	for rows.Next() {
		var row ledgerRow
		if err := rows.Scan(rowScanTargets(&row)...); err != nil {
			return keepCursor(cur), fmt.Errorf("minimax: scan %s: %w", usageTable, err)
		}
		if row.id > maxID {
			maxID, watermark, last = row.id, row, true
		}
		// The watermark advances past rows that cannot be attributed to a
		// session, so they are not retried on every scan.
		ev, ok := row.event()
		if !ok {
			continue
		}
		events = append(events, ev)
		agg := sessions[ev.SessionID]
		if agg == nil {
			agg = &sessionAggregate{sessionID: ev.SessionID}
			sessions[ev.SessionID] = agg
		}
		agg.add(ev.Timestamp)
	}
	if err := rows.Err(); err != nil {
		return keepCursor(cur), fmt.Errorf("minimax: read %s: %w", usageTable, err)
	}

	if last {
		state = ledgerState{ID: maxID, Digest: watermark.digest()}
	}
	return harness.Batch{
		Events:   events,
		Sessions: sessionMetas(sessions),
		Next:     cursorFor(cur, sig, fp, state),
	}, nil
}

// keepCursor returns an inert batch: nothing was consumed, so the caller's
// cursor is handed back unchanged and the next round retries the same file.
// Advancing it here would make the next round's fast path skip rows nobody read.
func keepCursor(cur harness.Cursor) harness.Batch { return harness.Batch{Next: cur} }

// ledgerRow is one row of the columns this parser reads.
type ledgerRow struct {
	id         int64
	sessionID  sql.NullString
	ts         sql.NullInt64
	input      sql.NullInt64
	output     sql.NullInt64
	reasoning  sql.NullInt64
	cacheRead  sql.NullInt64
	cacheWrite sql.NullInt64
	model      sql.NullString
	cost       sql.NullFloat64
}

func rowScanTargets(r *ledgerRow) []any {
	return []any{
		&r.id, &r.sessionID, &r.ts, &r.input, &r.output,
		&r.reasoning, &r.cacheRead, &r.cacheWrite, &r.model, &r.cost,
	}
}

// event maps the row onto an event. ok is false for a row that names no
// session: it belongs to no usage history and is not reported as one.
func (r ledgerRow) event() (model.UsageEvent, bool) {
	session := strings.TrimSpace(r.sessionID.String)
	if session == "" {
		return model.UsageEvent{}, false
	}
	return model.UsageEvent{
		Harness:   Harness,
		DedupKey:  fmt.Sprintf("%s:%s:%d", Harness, session, r.id),
		SessionID: session,
		Timestamp: unixMillis(r.ts.Int64),
		Model:     modelName(r.model.String),
		Tokens: model.Tokens{
			Input:      clampCount(r.input),
			Output:     clampCount(r.output),
			CacheRead:  clampCount(r.cacheRead),
			CacheWrite: clampCount(r.cacheWrite),
			Reasoning:  clampCount(r.reasoning),
		},
		CostUSD: costPointer(r.cost),
	}, true
}

// digest identifies the row's content, so a read position can tell a grown
// ledger from one replaced under the same path with the same row ids.
func (r ledgerRow) digest() string {
	h := sha1.New()
	fmt.Fprintf(h, "%d\x1f%d\x1f%s\x1f%d\x1f%d\x1f%d\x1f%d\x1f%d\x1f%s\x1f%v",
		r.id, boolInt(r.ts.Valid), r.sessionID.String, r.input.Int64, r.output.Int64,
		r.reasoning.Int64, r.cacheRead.Int64, r.cacheWrite.Int64, r.model.String, r.cost)
	return hex.EncodeToString(h.Sum(nil))
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// sessionAggregate accumulates one session's observed time span within a batch.
// The store merges batches with min(started_at) and max(updated_at), so
// per-batch spans compose into the same answer as a full parse.
type sessionAggregate struct {
	sessionID string
	started   time.Time
	updated   time.Time
}

// add widens the span. A zero timestamp is unknown and never becomes a span
// boundary.
func (a *sessionAggregate) add(t time.Time) {
	if t.IsZero() {
		return
	}
	if a.started.IsZero() || t.Before(a.started) {
		a.started = t
	}
	if t.After(a.updated) {
		a.updated = t
	}
}

func sessionMetas(sessions map[string]*sessionAggregate) []model.SessionMeta {
	if len(sessions) == 0 {
		return nil
	}
	ids := make([]string, 0, len(sessions))
	for id := range sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]model.SessionMeta, 0, len(ids))
	for _, id := range ids {
		agg := sessions[id]
		out = append(out, model.SessionMeta{
			Harness:   Harness,
			SessionID: id,
			StartedAt: agg.started,
			UpdatedAt: agg.updated,
		})
	}
	return out
}

// unixMillis converts the ledger's millisecond timestamp. A non-positive value
// is unknown and stays the zero time.
func unixMillis(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// modelName reports the recorded model, or the explicit unknown marker.
func modelName(raw string) string {
	if name := strings.TrimSpace(raw); name != "" {
		return name
	}
	return unknownModel
}

// clampCount keeps an unusable counter from becoming a negative event.
func clampCount(v sql.NullInt64) int64 {
	if !v.Valid || v.Int64 < 0 {
		return 0
	}
	return v.Int64
}

// costPointer reports the recorded cost. The official writer stores NULL when
// the provider reported no cost and a finite amount otherwise, and it produces 0
// only for a turn the provider priced at exactly zero (pi-usage.ts keeps
// "missing provider usage distinct from a trustworthy zero-cost turn"), so a
// finite zero is kept as $0. A missing, negative or non-finite cost is unknown
// and stays nil.
func costPointer(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	cost := v.Float64
	if math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
		return nil
	}
	return &cost
}

// signature is the change detector for a ledger and its write-ahead sidecars.
type signature struct {
	size int64
	mod  time.Time
}

// signatureOf sums the database and its WAL sidecars' sizes and takes the most
// recent modification time, so rows still in an un-checkpointed WAL are seen.
func signatureOf(path string) (signature, error) {
	info, err := os.Stat(path)
	if err != nil {
		return signature{}, err
	}
	sig := signature{size: info.Size(), mod: info.ModTime()}
	// Not -shm: every reader, this one included, writes its read mark
	// there, so the signature would change on our own read.
	for _, suffix := range []string{"-wal"} {
		side, err := os.Stat(path + suffix)
		if err != nil {
			continue
		}
		sig.size += side.Size()
		if side.ModTime().After(sig.mod) {
			sig.mod = side.ModTime()
		}
	}
	return sig, nil
}

// fingerprint hashes the first fingerprintBytes of the database. sqlite's file
// header changes when the file is replaced, and the WAL sidecars cover the rest
// through signatureOf.
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

// openReadOnly opens the ledger read-only and query_only, with a bounded lock
// wait: no code path in this package can write to a user's database.
func openReadOnly(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	params := url.Values{}
	params.Set("mode", "ro")
	params.Set("immutable", "0")
	params.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS))
	params.Add("_pragma", "query_only(1)")
	db, err := sql.Open("sqlite", sqlitedsn.URI(abs, params.Encode()))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), openTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// queryer is what the schema probe needs, so it works inside the read
// transaction as well as on a bare connection.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// tableColumns reports the declared type of every column of table, keyed by
// lower-case column name. A table that does not exist yields an empty map and no
// error; a probe that could not be answered returns its error, so the caller can
// tell "this database has no ledger columns" from "the schema could not be read".
func tableColumns(ctx context.Context, q queryer, table string) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, "PRAGMA table_info("+quoteIdent(table)+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]string{}
	for rows.Next() {
		var (
			cid      int
			name     string
			ctype    string
			notNull  int
			dflt     sql.NullString
			primaryK int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &primaryK); err != nil {
			return nil, err
		}
		cols[strings.ToLower(name)] = ctype
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return cols, nil
}

// supportedSchema reports whether cols is a ledger generation this parser can
// read: every required column, with the affinity the official schema declares.
// An unknown column set is not treated as supported.
func supportedSchema(cols map[string]string) bool {
	required := []struct{ name, affinity string }{
		{"id", affinityInteger},
		{"session_id", affinityText},
		{"ts", affinityInteger},
		{"input_tokens", affinityInteger},
		{"output_tokens", affinityInteger},
	}
	if len(cols) == 0 {
		return false
	}
	for _, col := range required {
		if !hasAffinity(cols, col.name, col.affinity) {
			return false
		}
	}
	return true
}

// hasAffinity reports whether the column exists with the wanted affinity.
func hasAffinity(cols map[string]string, name, affinity string) bool {
	ctype, ok := cols[name]
	return ok && affinityOf(ctype) == affinity
}

// affinityOf resolves a declared type to a SQLite storage-class affinity
// (first matching rule, as SQLite itself does).
func affinityOf(ctype string) string {
	t := strings.ToUpper(ctype)
	switch {
	case strings.Contains(t, "INT"):
		return affinityInteger
	case strings.Contains(t, "CHAR"), strings.Contains(t, "CLOB"), strings.Contains(t, "TEXT"):
		return affinityText
	case strings.Contains(t, "BLOB"):
		return "BLOB"
	case strings.Contains(t, "REAL"), strings.Contains(t, "FLOA"), strings.Contains(t, "DOUB"):
		return affinityReal
	case t == "":
		return "BLOB"
	default:
		return "NUMERIC"
	}
}

// selectList builds the SELECT list: the columns this parser needs plus the
// optional usage classes, each either the real column or a constant that says
// "this generation does not record it". `raw` and every non-usage column are
// deliberately absent, so no message text or credential can be read.
func selectList(cols map[string]string) string {
	return strings.Join([]string{
		quoteIdent("id"),
		"CAST(" + quoteIdent("session_id") + " AS TEXT)",
		"CAST(" + quoteIdent("ts") + " AS INTEGER)",
		"CAST(" + quoteIdent("input_tokens") + " AS INTEGER)",
		"CAST(" + quoteIdent("output_tokens") + " AS INTEGER)",
		intOrZero(cols, "reasoning_tokens"),
		intOrZero(cols, "cache_read_tokens"),
		intOrZero(cols, "cache_write_tokens"),
		textOrEmpty(cols, "model"),
		realOrNull(cols, "cost_usd"),
	}, ", ")
}

func intOrZero(cols map[string]string, name string) string {
	if hasAffinity(cols, name, affinityInteger) {
		return "CAST(" + quoteIdent(name) + " AS INTEGER)"
	}
	return "0"
}

func textOrEmpty(cols map[string]string, name string) string {
	if hasAffinity(cols, name, affinityText) {
		return "CAST(" + quoteIdent(name) + " AS TEXT)"
	}
	return "''"
}

// realOrNull reads a numeric cost column. A column of any other kind is not a
// cost and is reported as unknown rather than parsed.
func realOrNull(cols map[string]string, name string) string {
	ctype, ok := cols[name]
	if !ok {
		return "NULL"
	}
	switch affinityOf(ctype) {
	case affinityReal, "NUMERIC":
		return "CAST(" + quoteIdent(name) + " AS REAL)"
	}
	return "NULL"
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// ledgerState is the persisted read position: the highest row id consumed and a
// digest of that row.
type ledgerState struct {
	ID     int64  `json:"id,omitempty"`
	Digest string `json:"h,omitempty"`
}

func decodeState(extra string) ledgerState {
	var state ledgerState
	if strings.TrimSpace(extra) == "" {
		return state
	}
	if err := json.Unmarshal([]byte(extra), &state); err != nil || state.ID < 0 {
		return ledgerState{}
	}
	return state
}

// watermarkIntact reports whether the row the cursor stopped at is still the
// row that was read: the test for a ledger replaced under the same path.
func watermarkIntact(ctx context.Context, tx *sql.Tx, cols map[string]string, state ledgerState) (bool, error) {
	query := "SELECT " + selectList(cols) + " FROM " + quoteIdent(usageTable) + " WHERE " + quoteIdent("id") + " = ?"
	var row ledgerRow
	err := tx.QueryRowContext(ctx, query, state.ID).Scan(rowScanTargets(&row)...)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return row.digest() == state.Digest, nil
}

// cursorFor carries the read position forward: the file signature so an
// unchanged ledger is skipped, and the row watermark so a grown one continues.
func cursorFor(cur harness.Cursor, sig signature, fp string, state ledgerState) harness.Cursor {
	next := harness.Cursor{
		Offset:      cur.Offset,
		Size:        sig.size,
		ModTime:     sig.mod,
		Fingerprint: fp,
	}
	if state.ID > 0 {
		if raw, err := json.Marshal(state); err == nil {
			next.Extra = string(raw)
		}
	}
	return next
}

// cleanRoots trims roots and drops blank entries.
func cleanRoots(roots []string) []string {
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		if root = strings.TrimSpace(root); root != "" {
			out = append(out, root)
		}
	}
	return out
}
