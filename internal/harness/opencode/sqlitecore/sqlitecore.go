// Package sqlitecore is the read-only SQLite snapshot shared by the harness
// parsers whose harness stores its sessions in a MessageV2-shaped database.
//
// MiMo Code is a fork of OpenCode and keeps the same storage engine: a
// `session` table with the conversation directory and the parent link, and a
// `message` table whose `data` column holds a MessageV2.Info JSON document
// with role, tokens, cost, modelID/providerID, time{created,completed} and
// path{root,cwd}. Parsing that shape is a deep module in its own right —
// read-only open, schema probing, per-table incremental watermarks, session
// metadata, payload decoding and event accumulation — so it lives here once
// and both harnesses reuse it instead of carrying two copies. The harness
// package that owns the layout (internal/harness/opencode) is where it sits;
// the module itself knows no harness identity: a Profile supplies the harness
// id, the dedup key prefix and how a session directory becomes the stored
// project path.
//
// Everything here is deliberately read-only. Harness state belongs to the tool
// that owns it, so the module never migrates, repairs or rewrites a source: it
// opens the database with mode=ro and query_only=1, keeps a busy timeout so a
// concurrent writer is waited for rather than corrupted, and treats a locked or
// unreadable database as "no data this pass" so the next scan retries.
//
// Incrementality. SQLite rewrites its header on every commit, so the first-4KB
// fingerprint of the file changes whenever anything at all is written; the
// cursor therefore stores a combined signature (database size+mtime plus
// -wal/-shm size+mtime) for the cheap unchanged fast path, and each message
// table keeps its own (time_updated, id) watermark in the caller's Cursor.Extra
// so a scan resumes without re-reading rows it already saw. A watermark ahead
// of the table's own MAX(time_updated) is dropped: that is how a replaced or
// rewound database is detected.
package sqlitecore

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
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/sqlitedsn"

	_ "modernc.org/sqlite"

	"github.com/zzstar101/mytoken/internal/harness"
)

const (
	// FingerprintBytes is how much of the database file the fast-path
	// fingerprint covers. SQLite's header, and therefore its change counter,
	// lives in the first 100 bytes.
	FingerprintBytes = 4096
	// BusyTimeoutMS is how long a read waits for a writer holding the database.
	BusyTimeoutMS = 5000
)

// ---------------------------------------------------------------------------
// cursor support
// ---------------------------------------------------------------------------

// Signature is the cheap "did anything change" identity of a database: the main
// file plus its write-ahead log, which is where in-flight commits land before a
// checkpoint.
type Signature struct {
	Size int64
	Mod  time.Time
}

// Stat computes the signature of the database at path, including -wal and -shm.
func Stat(path string) (Signature, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Signature{}, err
	}
	sig := Signature{Size: fi.Size(), Mod: fi.ModTime()}
	for _, suffix := range []string{"-wal", "-shm"} {
		w, err := os.Stat(path + suffix)
		if err != nil {
			continue
		}
		sig.Size += w.Size()
		if w.ModTime().After(sig.Mod) {
			sig.Mod = w.ModTime()
		}
	}
	return sig, nil
}

// Fingerprint returns the sha1 of the first FingerprintBytes of path.
func Fingerprint(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.CopyN(h, f, FingerprintBytes); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Unchanged reports whether cur already describes exactly this database state,
// which lets a scan return without opening the database at all.
func Unchanged(cur harness.Cursor, sig Signature, fp string) bool {
	return cur.Fingerprint != "" && cur.Fingerprint == fp &&
		cur.Size == sig.Size && cur.ModTime.Equal(sig.Mod)
}

// Next builds the cursor that resumes after this database state.
func Next(sig Signature, fp, extra string) harness.Cursor {
	return harness.Cursor{
		Offset:      0,
		Size:        sig.Size,
		ModTime:     sig.Mod,
		Fingerprint: fp,
		Extra:       extra,
	}
}

// Watermark is the (time_updated, id) resume point of one message table.
type Watermark struct {
	Updated int64  `json:"u"`
	ID      string `json:"i"`
}

// Mark remembers the newest context-replacement row seen for a session (At) and
// the one whose boundary has already been reported (Done). It is persisted in
// the cursor because the replacing row and the first request after it routinely
// arrive in different incremental batches.
type Mark struct {
	At   int64 `json:"a,omitempty"`
	Done int64 `json:"d,omitempty"`
}

// ---------------------------------------------------------------------------
// snapshot
// ---------------------------------------------------------------------------

// Snapshot is a read-only view of one harness database.
type Snapshot struct {
	db     *sql.DB
	tables map[string]map[string]bool
}

// Open opens path read-only and probes its schema. A database that is locked,
// missing or not a database at all is an error, which the caller normally
// answers by keeping its cursor and retrying on the next scan.
func Open(path string) (*Snapshot, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	q := url.Values{
		"mode":      {"ro"},
		"immutable": {"0"},
		"_pragma": {
			fmt.Sprintf("busy_timeout(%d)", BusyTimeoutMS),
			"query_only(1)",
		},
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
	return &Snapshot{db: db, tables: tableColumns(ctx, db)}, nil
}

// Close releases the snapshot.
func (s *Snapshot) Close() error { return s.db.Close() }

// DB exposes the underlying read-only connection for callers that need to probe
// the snapshot directly (diagnostics, tests).
func (s *Snapshot) DB() *sql.DB { return s.db }

// HasColumns reports whether table exists and carries every named column; the
// queries degrade gracefully across schema generations with it.
func (s *Snapshot) HasColumns(table string, names ...string) bool {
	cols, ok := s.tables[table]
	if !ok {
		return false
	}
	for _, name := range names {
		if !cols[name] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// table scanning
// ---------------------------------------------------------------------------

// Session is the per-session metadata a harness stores next to its messages.
type Session struct {
	ID        string
	ParentID  string
	Directory string
	Title     string
	Created   int64
	Updated   int64
}

// Spec selects the usage rows of one message table.
type Spec struct {
	// Table is the table to read.
	Table string
	// Filter is the SQL predicate that keeps usage-carrying rows only.
	Filter string
	// KindExpr is the column (or constant) whose value distinguishes a row that
	// also marks a context replacement point; it may be empty.
	KindExpr string
}

// requiredColumns lists every column the query Messages builds for spec reads:
// the four the row scan binds, plus the KindExpr when it names a column. Reset
// asks for the same set, so a watermark can never outlive the schema it needs.
func (spec Spec) requiredColumns() []string {
	cols := []string{"id", "session_id", "data", "time_updated"}
	name, ok := kindColumn(spec.KindExpr)
	if !ok {
		return cols
	}
	for _, c := range cols {
		if c == name {
			return cols
		}
	}
	return append(cols, name)
}

// kindColumn reports the column a KindExpr names. An empty value, a SQL string
// constant and a real expression name none of them; those are the query's
// business, not the schema probe's.
func kindColumn(expr string) (string, bool) {
	if expr == "" {
		return "", false
	}
	for i, r := range expr {
		switch {
		case r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
			if i == 0 {
				return "", false
			}
		default:
			return "", false
		}
	}
	return expr, true
}

// Row is one usage-carrying message row. SessionID is the stored column
// verbatim, or empty when the column is NULL: the message's own payload also
// names its session, and the callers prefer the stored column only when it has
// something in it (FirstNonEmpty).
type Row struct {
	ID        string
	SessionID string
	Data      string
	Kind      string
	Updated   int64
}

// Sessions reads every session row. A harness may keep its sessions in more than
// one table generation; the layouts this module serves use `session` (and the
// `session_v2` name OpenCode later introduced), so both are read when present
// and merged by id.
func (s *Snapshot) Sessions(ctx context.Context) map[string]Session {
	out := map[string]Session{}
	for _, name := range []string{"session", "session_v2"} {
		if !s.HasColumns(name, "id") {
			continue
		}
		q := "SELECT " + sessionColumns(s.tables[name]) + " FROM " + quoteIdent(name)
		rows, err := s.db.QueryContext(ctx, q)
		if err != nil {
			continue
		}
		for rows.Next() {
			var info Session
			if err := rows.Scan(&info.ID, &info.ParentID, &info.Directory, &info.Title, &info.Created, &info.Updated); err != nil {
				continue
			}
			if info.ID == "" {
				continue
			}
			prev, seen := out[info.ID]
			if !seen {
				out[info.ID] = info
				continue
			}
			out[info.ID] = mergeSession(prev, info)
		}
		rows.Close()
	}
	return out
}

// sessionColumns builds the projection for a session table, substituting
// constants for columns an older schema does not have.
func sessionColumns(cols map[string]bool) string {
	text := func(name string) string {
		if !cols[name] {
			return "''"
		}
		return "COALESCE(" + quoteIdent(name) + ",'')"
	}
	integer := func(name string) string {
		if !cols[name] {
			return "0"
		}
		return "COALESCE(" + quoteIdent(name) + ",0)"
	}
	return strings.Join([]string{
		text("id"), text("parent_id"), text("directory"), text("title"),
		integer("time_created"), integer("time_updated"),
	}, ", ")
}

func mergeSession(a, b Session) Session {
	a.ParentID = FirstNonEmpty(a.ParentID, b.ParentID)
	a.Directory = FirstNonEmpty(a.Directory, b.Directory)
	a.Title = FirstNonEmpty(a.Title, b.Title)
	if a.Created == 0 || (b.Created != 0 && b.Created < a.Created) {
		a.Created = b.Created
	}
	if b.Updated > a.Updated {
		a.Updated = b.Updated
	}
	return a
}

// Messages walks the rows of one message table newer than wm, in
// (time_updated, id) order, and returns the advanced watermark. The visitor is
// called once per row. A table that is absent, or that is missing a column the
// query needs, yields no rows and a zero watermark: a resume point no read can
// honour would never move again, so every row the table later exposes at or
// before it would be skipped for good.
//
// The returned watermark only ever advances past rows this call read to the end
// of, which makes two kinds of unreadable row a deliberate pair:
//
//   - An optional NULL is read, not skipped. `session_id` is metadata the
//     payload itself also states, and SQLite stores NULL in it, so the column is
//     scanned into a sql.NullString and normalised to the empty string the
//     callers already fall back from to the payload's own sessionID. A row whose
//     only defect is an optional NULL still carries usage, so it is delivered.
//   - Anything else that fails to scan is a failed pass, not a row to skip. The
//     scan is where the row's position (`time_updated`) and identity (`id`) are
//     read; when it fails, nothing about the row is known, so no later row may
//     be treated as read and the caller's watermark comes back unchanged. That
//     one rule also covers an interrupted read — the caller's context is
//     cancelled, the connection dies, the disk errors — whose failure surfaces
//     from rows.Err below. The rows already delivered in such a pass are real,
//     but the point the loop reached is not a resume point: nothing proves the
//     table holds no further row at or before it, and the caller persists
//     whatever comes back. So a failed pass returns the watermark it was called
//     with and the next pass re-reads the range. Rows the failed pass already
//     visited are delivered again, which is safe because every consumer folds
//     rows by DedupKey; losing them is not.
//
// The price of the second rule is deliberate: while a row holds a permanently
// unreadable value — the reported case is a TEXT `time_updated` in this
// dynamically typed column — the watermark never advances past it, so that
// table's watermark stalls and its range is re-read whenever the database
// changes. That is the conservative side of the trade: it preserves a
// recoverable resume point and resumes as soon as the value is repaired
// (upstream stamps any really-updated row with a new time_updated), while a
// skipped row would be silently gone from the ledger for good.
//
// A row whose `id` is empty is still skipped without failing the pass: the
// keyset cannot name it as a resume point, and Emit ignores a row without a
// message id, so there is nothing countable to lose. SQLite's TEXT PRIMARY KEY
// accepts NULL, so the column is read as a NullString for the same reason: a
// NULL id is not a value the ledger could ever key, and reading it into a plain
// string failed the pass and pinned that table's watermark, hiding every legal
// row behind it on every later pass.
func (s *Snapshot) Messages(ctx context.Context, spec Spec, wm Watermark, visit func(Row)) Watermark {
	if !s.scannable(spec) {
		return Watermark{}
	}
	kind := spec.KindExpr
	if kind == "" {
		kind = "''"
	}
	q := "SELECT id, session_id, data, time_updated, " + kind + " FROM " + quoteIdent(spec.Table) +
		" WHERE " + spec.Filter +
		" AND (time_updated > ? OR (time_updated = ? AND id > ?))" +
		" ORDER BY time_updated, id"
	rows, err := s.db.QueryContext(ctx, q, wm.Updated, wm.Updated, wm.ID)
	if err != nil {
		return wm
	}
	defer rows.Close()
	before := wm
	for rows.Next() {
		var (
			row Row
			// id is the keyset position and the dedup key, so a row without one
			// can neither be counted nor resumed from; the column is NULL-able
			// in practice and a NullString keeps the NULL from failing the scan
			// and stalling the table behind it.
			id sql.NullString
			// session_id is optional metadata the payload also states, and
			// SQLite happily stores NULL in the column. Scanning NULL into a
			// string fails, so the row would never be delivered; a NullString
			// accepts it and normalises to the empty string the callers already
			// treat as "ask the payload".
			session sql.NullString
		)
		if err := rows.Scan(&id, &session, &row.Data, &row.Updated, &row.Kind); err != nil {
			return before
		}
		row.ID = id.String
		row.SessionID = session.String
		if row.ID == "" {
			continue
		}
		visit(row)
		if row.Updated > wm.Updated || (row.Updated == wm.Updated && row.ID > wm.ID) {
			wm.Updated, wm.ID = row.Updated, row.ID
		}
	}
	if err := rows.Err(); err != nil {
		return before
	}
	return wm
}

// MaxUpdated reports the newest time_updated present in one table.
func (s *Snapshot) MaxUpdated(ctx context.Context, table string) (int64, bool) {
	if !s.HasColumns(table, "time_updated") {
		return 0, false
	}
	var v sql.NullInt64
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(time_updated),0) FROM "+quoteIdent(table)).Scan(&v); err != nil {
		return 0, false
	}
	return v.Int64, true
}

// scannable reports whether the query Messages builds for spec can run against
// this snapshot at all.
func (s *Snapshot) scannable(spec Spec) bool {
	return s.HasColumns(spec.Table, spec.requiredColumns()...)
}

// Reset drops a watermark that can no longer be valid: a negative value, one
// carried for a table this Spec cannot read (an absent generation, or one still
// missing a column the query needs), or one newer than the table's own newest
// row, which means the database was replaced or rewound behind it. The schema
// test is exactly the one Messages applies, so a watermark can never survive a
// pass in which nothing was read and then hide the rows that pass left behind.
func (s *Snapshot) Reset(ctx context.Context, spec Spec, wm Watermark) Watermark {
	if wm.Updated < 0 || !s.scannable(spec) {
		return Watermark{}
	}
	max, ok := s.MaxUpdated(ctx, spec.Table)
	if !ok || wm.Updated > max {
		return Watermark{}
	}
	return wm
}

// tableColumns maps every table (and view) to its column set, so the queries can
// degrade gracefully across schema generations.
func tableColumns(ctx context.Context, db *sql.DB) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type IN ('table','view')`)
	if err != nil {
		return out
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		names = append(names, name)
	}
	rows.Close()
	for _, name := range names {
		cols := map[string]bool{}
		rs, err := db.QueryContext(ctx, `PRAGMA table_info(`+quoteIdent(name)+`)`)
		if err != nil {
			continue
		}
		for rs.Next() {
			var (
				cid, notnull, pk int
				col, ctype       string
				dflt             sql.NullString
			)
			if err := rs.Scan(&cid, &col, &ctype, &notnull, &dflt, &pk); err != nil {
				continue
			}
			cols[col] = true
		}
		rs.Close()
		out[name] = cols
	}
	return out
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// ---------------------------------------------------------------------------
// payload decoding
// ---------------------------------------------------------------------------

// Payload is the usage-relevant subset of a MessageV2.Info document. OpenCode
// and MiMo Code write the same shape, so one decoder serves both.
type Payload struct {
	ID             string        `json:"id"`
	SessionID      string        `json:"sessionID"`
	SnakeSessionID string        `json:"session_id"`
	Role           string        `json:"role"`
	ModelID        string        `json:"modelID"`
	ProviderID     string        `json:"providerID"`
	Model          ModelRef      `json:"model"`
	Cost           *float64      `json:"cost"`
	Tokens         *TokenPayload `json:"tokens"`
	Time           MessageTime   `json:"time"`
	Path           MessagePath   `json:"path"`
	Agent          string        `json:"agent"`
	Mode           string        `json:"mode"`
	Variant        string        `json:"variant"`
}

// ModelRef is the nested model descriptor some rows carry instead of the
// top-level modelID/providerID pair.
type ModelRef struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
}

// MessageTime is the message's own timestamps, in milliseconds.
type MessageTime struct {
	Created   *Num `json:"created"`
	Completed *Num `json:"completed"`
	Updated   *Num `json:"updated"`
}

// MessagePath is the project directory the request ran in.
type MessagePath struct {
	Root string `json:"root"`
	Cwd  string `json:"cwd"`
}

// TokenPayload is the usage block of a message. `input` already excludes the
// cache classes, so the classes map one-to-one onto model.Tokens with no
// subtraction.
type TokenPayload struct {
	Input     Num         `json:"input"`
	Output    Num         `json:"output"`
	Reasoning *Num        `json:"reasoning"`
	Cache     *CacheUsage `json:"cache"`
}

// CacheUsage is the cache read/write classes of a usage block.
type CacheUsage struct {
	Read  *Num `json:"read"`
	Write *Num `json:"write"`
}

// Classes maps the token classes onto model.Tokens, clamping negatives.
func (t *TokenPayload) Classes() model.Tokens {
	if t == nil {
		return model.Tokens{}
	}
	tk := model.Tokens{
		Input:  t.Input.Int64(),
		Output: t.Output.Int64(),
	}
	if t.Reasoning != nil {
		tk.Reasoning = t.Reasoning.Int64()
	}
	if t.Cache != nil {
		if t.Cache.Read != nil {
			tk.CacheRead = t.Cache.Read.Int64()
		}
		if t.Cache.Write != nil {
			tk.CacheWrite = t.Cache.Write.Int64()
		}
	}
	return tk
}

// Decode projects one message row's data column onto Payload.
//
// The column is written by another program, so Decode is deliberately not one
// strict json.Unmarshal. A strict decode reported the first value it could not
// fit into a Go field — a quoted cost, a `path` that is a string, a
// `time.created` that is not a number — as "this row is not a message", and the
// caller, which cannot tell the two apart, dropped the whole row. Because the
// read cursor advances with the scan, a dropped row is never offered again: an
// optional field with an unexpected JSON type silently became a permanent
// undercount of the tokens that row carried.
//
// The projection is therefore two-stage. The outer value gates the row: data
// that is not a JSON object is not a message (`null` is not an object either, it
// is the absence of one), so it reports ok == false. Every field inside that
// object is then projected on its own, so a value that does not fit costs only
// itself and the usage block — the reason the row was selected at all — still
// arrives with whatever classes are legible.
//
// `cost` is the one field outside the usage block whose *value* matters rather
// than its mere presence, so it also tolerates the quoted numbers the rest of
// the module already accepts (see projectCost).
func Decode(data []byte) (Payload, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return Payload{}, false
	}
	var p Payload
	project(fields, "id", &p.ID)
	project(fields, "sessionID", &p.SessionID)
	project(fields, "session_id", &p.SnakeSessionID)
	project(fields, "role", &p.Role)
	project(fields, "modelID", &p.ModelID)
	project(fields, "providerID", &p.ProviderID)
	projectModel(fields, &p.Model)
	projectTokens(fields, &p.Tokens)
	project(fields, "time", &p.Time)
	projectPath(fields, &p.Path)
	project(fields, "agent", &p.Agent)
	project(fields, "mode", &p.Mode)
	project(fields, "variant", &p.Variant)
	projectCost(fields, &p.Cost)
	return p, true
}

// project decodes the named field of a message object into dst and ignores a
// value that does not fit. Every field Decode projects is optional metadata
// whose absence the callers already handle — an unknown path, an unknown
// timestamp, an unknown model — so losing one field is always better than losing
// the usage block beside it. A value that does not fit leaves the field absent
// rather than half-decoded, which is what keeps `tokens: 5` (a usage block that
// is not an object) reporting no usage instead of an empty one.
func project[T any](fields map[string]json.RawMessage, key string, dst *T) {
	raw, ok := fields[key]
	if !ok {
		return
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		var zero T
		*dst = zero
		return
	}
	*dst = v
}

// object returns the named field as a JSON object, for the nested documents
// whose fields are projected individually. A field that is absent, null, or not
// an object at all reports false, and the caller then leaves its destination
// absent: there is nothing inside to project.
func object(fields map[string]json.RawMessage, key string) (map[string]json.RawMessage, bool) {
	raw, ok := fields[key]
	if !ok {
		return nil, false
	}
	var sub map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sub); err != nil || sub == nil {
		return nil, false
	}
	return sub, true
}

// projectTokens decodes the usage block field by field. This is the block the
// read exists for, so it is projected as deep as it goes: an unreadable `input`
// must not cost `output`, and a `cache` that is not an object must not cost the
// classes beside it.
func projectTokens(fields map[string]json.RawMessage, dst **TokenPayload) {
	sub, ok := object(fields, "tokens")
	if !ok {
		return
	}
	tk := &TokenPayload{}
	project(sub, "input", &tk.Input)
	project(sub, "output", &tk.Output)
	project(sub, "reasoning", &tk.Reasoning)
	if cache, ok := object(sub, "cache"); ok {
		c := &CacheUsage{}
		project(cache, "read", &c.Read)
		project(cache, "write", &c.Write)
		tk.Cache = c
	}
	*dst = tk
}

// projectModel decodes the nested model descriptor field by field, so an
// unreadable `model.id` cannot take its `model.providerID` with it: both are
// fallbacks the callers report as the model and provider names.
func projectModel(fields map[string]json.RawMessage, dst *ModelRef) {
	sub, ok := object(fields, "model")
	if !ok {
		return
	}
	var m ModelRef
	project(sub, "id", &m.ID)
	project(sub, "providerID", &m.ProviderID)
	*dst = m
}

// projectPath decodes the nested project path field by field for the same
// reason: Project() reports the working directory and falls back to the root,
// so an unreadable `path.cwd` must not cost `path.root`.
func projectPath(fields map[string]json.RawMessage, dst *MessagePath) {
	sub, ok := object(fields, "path")
	if !ok {
		return
	}
	var p MessagePath
	project(sub, "root", &p.Root)
	project(sub, "cwd", &p.Cwd)
	*dst = p
}

// projectCost reads the reported cost, the one field outside the usage block
// whose *value* matters. It accepts a JSON number first and then the quoted
// numbers the module accepts elsewhere, so a cost written as "0.0123" is read as
// the number the row states instead of being dropped next to the tokens it
// belongs to. It parses the quoted form directly rather than through Num, which
// counts whole tokens and would round a cost to zero.
func projectCost(fields map[string]json.RawMessage, dst **float64) {
	raw, ok := fields["cost"]
	if !ok {
		return
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		*dst = &f
		return
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 1 && s[0] == '"' && s[len(s)-1] == '"' {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		*dst = &v
	}
}

// Timestamp returns the message's creation timestamp (UTC, zero when the log
// carries none).
func (p Payload) Timestamp() time.Time { return EpochMS(int64Of(p.Time.Created)) }

// ModelName reports the model id the row itself states.
func (p Payload) ModelName() string { return FirstNonEmpty(p.ModelID, p.Model.ID) }

// ProviderName reports the provider the row itself states, never an inference.
func (p Payload) ProviderName() string { return FirstNonEmpty(p.ProviderID, p.Model.ProviderID) }

// Project returns the project directory the row ran in, if it states one.
func (p Payload) Project() string { return FirstNonEmpty(p.Path.Cwd, p.Path.Root) }

// Num decodes a JSON number that may arrive as an integer, a float, a quoted
// string or null. It is deliberately total: a value it cannot read as a number
// is zero, not an error. A counter that does not fit is one legible class fewer,
// never a row lost — and an error would also abort the rest of the enclosing
// object, so a single unreadable class would take its siblings down with it. The
// price is that a corrupt counter no longer announces itself: the usage block is
// reported as understood, and a wholly unreadable one arrives as zero classes,
// which the accumulator already treats as "no usage".
//
// The one value it must never invent is a maximum. Casting a float64 to int64
// SATURATES on 64-bit targets, so a counter written as 1e30 or "Inf" used to
// enter the ledger as math.MaxInt64; two such messages then made SQLite fail
// their SUM with "integer overflow", and one corrupt row took a whole day's
// report down. A value is read here only when it is an int64 the number really
// names — exact integer literals are parsed as such, so a large but legitimate
// integer keeps every digit — while overflow, NaN and the infinities are
// unknown (zero), the same class as any other unreadable counter.
type Num int64

// numLimit is 2^63 as a float64: at or beyond it no integer cast is meaningful,
// and the nearest float64 to math.MaxInt64 is already this value.
const numLimit = 9223372036854775808.0

// UnmarshalJSON implements json.Unmarshaler.
func (n *Num) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	if len(s) > 1 && s[0] == '"' && s[len(s)-1] == '"' {
		s = strings.TrimSpace(s[1 : len(s)-1])
		if s == "" {
			*n = 0
			return nil
		}
	}
	// An exact integer is taken as written, so a legitimate large counter is
	// never rounded through a float64.
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		*n = Num(i)
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f >= numLimit || f < -numLimit {
		*n = 0
		return nil
	}
	*n = Num(f)
	return nil
}

// Int64 returns the value as a non-negative int64. It is nil-safe: an absent
// number reads as zero.
func (n *Num) Int64() int64 {
	if n == nil || *n < 0 {
		return 0
	}
	return int64(*n)
}

func int64Of(n *Num) int64 { return n.Int64() }

// plausibleUntil is the exclusive upper bound of a timestamp this module is
// willing to report. Every generation it reads stamps a message in seconds,
// milliseconds or nanoseconds since the unix epoch, so a value that lands
// centuries away is not a date — it is a corrupt counter, and passing it on as a
// year-292-million instant would only move the corruption into the ledger. Such
// a value is absent, and the caller falls back to the row's own time_updated.
var plausibleUntil = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)

// EpochMS normalises a unix timestamp expressed in seconds, milliseconds or
// nanoseconds; the magnitude picks the scale, the same ladder the other
// SQLite-backed harnesses use. A value it cannot read as a plausible instant is
// the zero time, never a saturated one.
func EpochMS(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	var t time.Time
	switch {
	case v < 1e12: // seconds
		t = time.Unix(v, 0).UTC()
	case v < 1e15: // milliseconds
		t = time.UnixMilli(v).UTC()
	default: // nanoseconds
		t = time.Unix(0, v).UTC()
	}
	if !t.Before(plausibleUntil) {
		return time.Time{}
	}
	return t
}

// ReportedCost keeps only a finite, positive cost: harnesses write 0 for
// "unknown", and a provider-reported cost is never estimated locally.
func ReportedCost(c *float64) *float64 {
	if c == nil {
		return nil
	}
	v := *c
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return nil
	}
	return &v
}

// FirstNonEmpty returns the first argument that is not blank.
func FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// accumulator
// ---------------------------------------------------------------------------

// Profile is what the shared accumulator needs to know about a harness: its
// identity, its dedup key namespace, how a session's directory becomes the
// stored project path, and how a stored session title is mapped (a nil Title
// means the harness stores none).
type Profile struct {
	Harness   model.Harness
	KeyPrefix string
	Project   func(Session, Payload) string
	Title     func(string) string
}

// Accumulator folds rows into a deduplicated, ordered harness.Batch. Repeated
// observations of one dedup key inside a single parse are merged field-wise with
// per-class maxima: a harness appends token usage to a message after the row
// first appears.
type Accumulator struct {
	prof Profile

	evs   map[string]model.UsageEvent
	order []string
	metas map[string]model.SessionMeta
	mord  []string

	// marks is the per-session context-replacement state. For database sources
	// it is the same map the cursor state holds, so a boundary found in one
	// incremental batch survives into the next.
	marks map[string]Mark
	// compactions holds the dedup keys of the rows that are themselves a
	// replacement: they are emitted like any other request but never carry the
	// boundary mark.
	compactions map[string]bool
}

// NewAccumulator returns an accumulator for one harness profile.
func NewAccumulator(prof Profile) *Accumulator {
	return &Accumulator{
		prof:        prof,
		evs:         map[string]model.UsageEvent{},
		metas:       map[string]model.SessionMeta{},
		marks:       map[string]Mark{},
		compactions: map[string]bool{},
	}
}

// UseMarks attaches the cursor's persisted replacement state so mutations are
// written back with the next cursor.
func (a *Accumulator) UseMarks(m map[string]Mark) {
	if m == nil {
		m = map[string]Mark{}
	}
	a.marks = m
}

func (a *Accumulator) project(info Session, p Payload) string {
	if a.prof.Project == nil {
		return ""
	}
	return a.prof.Project(info, p)
}

func (a *Accumulator) title(stored string) string {
	if a.prof.Title == nil {
		return ""
	}
	return a.prof.Title(stored)
}

// Emit folds one usage row into the batch. A row without a usage block, or one
// whose usage is entirely zero and which reports no cost, produces no event:
// harnesses write such rows for aborted or fully cached turns.
func (a *Accumulator) Emit(messageID, sessionID string, p Payload, info Session, fallbackUpdatedMS int64, compaction bool) {
	if messageID == "" || p.Tokens == nil {
		return
	}
	tk := p.Tokens.Classes()
	cost := ReportedCost(p.Cost)
	if tk.IsZero() && cost == nil {
		return
	}
	ts := p.Timestamp()
	if ts.IsZero() {
		ts = EpochMS(fallbackUpdatedMS)
	}
	key := a.prof.KeyPrefix + messageID
	if compaction {
		a.markCompaction(sessionID, ts)
		a.compactions[key] = true
	}
	a.add(model.UsageEvent{
		Harness:     a.prof.Harness,
		DedupKey:    key,
		SessionID:   sessionID,
		ParentID:    info.ParentID,
		ProjectPath: a.project(info, p),
		Timestamp:   ts,
		Model:       p.ModelName(),
		Provider:    p.ProviderName(),
		Tokens:      tk,
		CostUSD:     cost,
	})
}

// markCompaction records that the session's context was replaced at ts. Only the
// newest mark per session is kept: older replacements are already covered by it.
func (a *Accumulator) markCompaction(sessionID string, ts time.Time) {
	if sessionID == "" || ts.IsZero() {
		return
	}
	if a.marks == nil {
		a.marks = map[string]Mark{}
	}
	m := a.marks[sessionID]
	if at := ts.UnixMilli(); at > m.At {
		m.At = at
	}
	a.marks[sessionID] = m
}

// Session folds one session row into the batch.
func (a *Accumulator) Session(info Session) {
	if info.ID == "" {
		return
	}
	started := EpochMS(info.Created)
	updated := EpochMS(info.Updated)
	if updated.Before(started) {
		updated = started
	}
	meta := model.SessionMeta{
		Harness:   a.prof.Harness,
		SessionID: info.ID,
		ParentID:  info.ParentID,
		Title:     a.title(info.Title),
		Project:   a.project(info, Payload{}),
		StartedAt: started,
		UpdatedAt: updated,
	}
	if prev, ok := a.metas[info.ID]; ok {
		meta = mergeMeta(prev, meta)
	} else {
		a.mord = append(a.mord, info.ID)
	}
	a.metas[info.ID] = meta
}

func (a *Accumulator) add(ev model.UsageEvent) {
	if ev.DedupKey == "" {
		return
	}
	if prev, ok := a.evs[ev.DedupKey]; ok {
		prev.Tokens = maxTokens(prev.Tokens, ev.Tokens)
		if ev.CostUSD != nil && (prev.CostUSD == nil || *ev.CostUSD > *prev.CostUSD) {
			prev.CostUSD = ev.CostUSD
		}
		prev.SessionID = FirstNonEmpty(prev.SessionID, ev.SessionID)
		prev.ParentID = FirstNonEmpty(prev.ParentID, ev.ParentID)
		prev.ProjectPath = FirstNonEmpty(prev.ProjectPath, ev.ProjectPath)
		prev.Model = FirstNonEmpty(prev.Model, ev.Model)
		prev.Provider = FirstNonEmpty(prev.Provider, ev.Provider)
		if prev.Timestamp.IsZero() {
			prev.Timestamp = ev.Timestamp
		}
		a.evs[ev.DedupKey] = prev
		return
	}
	a.evs[ev.DedupKey] = ev
	a.order = append(a.order, ev.DedupKey)
}

// Batch sorts the accumulated events and sessions and attaches the next cursor.
// It marks the first request at or after a session's newest context replacement
// with model.BoundaryCompact, once per replacement, reading the persisted state
// so a replacement seen in an earlier incremental batch still marks the next
// batch's first request.
func (a *Accumulator) Batch(next harness.Cursor) harness.Batch {
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
	a.applyCompactionBoundaries(events)
	metas := make([]model.SessionMeta, 0, len(a.mord))
	for _, id := range a.mord {
		metas = append(metas, a.metas[id])
	}
	sort.SliceStable(metas, func(i, j int) bool { return metas[i].SessionID < metas[j].SessionID })
	return harness.Batch{Events: events, Sessions: metas, Next: next}
}

func (a *Accumulator) applyCompactionBoundaries(events []model.UsageEvent) {
	if len(a.marks) == 0 {
		return
	}
	for i := range events {
		m, ok := a.marks[events[i].SessionID]
		if !ok || m.At == 0 || m.Done == m.At {
			continue
		}
		if a.compactions[events[i].DedupKey] {
			continue
		}
		if events[i].Timestamp.UnixMilli() < m.At {
			continue
		}
		events[i].Boundary = model.BoundaryCompact
		m.Done = m.At
		a.marks[events[i].SessionID] = m
	}
}

func mergeMeta(a, b model.SessionMeta) model.SessionMeta {
	a.ParentID = FirstNonEmpty(a.ParentID, b.ParentID)
	a.Title = FirstNonEmpty(a.Title, b.Title)
	a.Project = FirstNonEmpty(a.Project, b.Project)
	if a.StartedAt.IsZero() || (!b.StartedAt.IsZero() && b.StartedAt.Before(a.StartedAt)) {
		a.StartedAt = b.StartedAt
	}
	if b.UpdatedAt.After(a.UpdatedAt) {
		a.UpdatedAt = b.UpdatedAt
	}
	return a
}

func maxTokens(a, b model.Tokens) model.Tokens {
	if b.Input > a.Input {
		a.Input = b.Input
	}
	if b.Output > a.Output {
		a.Output = b.Output
	}
	if b.CacheRead > a.CacheRead {
		a.CacheRead = b.CacheRead
	}
	if b.CacheWrite > a.CacheWrite {
		a.CacheWrite = b.CacheWrite
	}
	if b.Reasoning > a.Reasoning {
		a.Reasoning = b.Reasoning
	}
	return a
}

// SortedKeys returns the keys of m in sorted order.
func SortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
