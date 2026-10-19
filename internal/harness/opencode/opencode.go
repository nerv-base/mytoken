// Package opencode parses OpenCode session usage.
//
// OpenCode keeps its state in a data directory that defaults to
// ~/.local/share/opencode ($XDG_DATA_HOME/opencode). Two generations of
// on-disk storage exist and both are read:
//
//	sqlite  <root>/opencode.db (and channel builds such as opencode-stable.db)
//	        tables:
//	          session(id, parent_id, directory, title, time_created, time_updated, ...)
//	          message(id, session_id, time_created, time_updated, data)        -- v1
//	          session_message(id, session_id, type, seq, ..., data)            -- v2
//	        `data` holds the JSON message info (MessageV2.Info): role, tokens,
//	        cost, modelID/providerID, time{created,completed}, path{root,cwd}.
//	        Only rows with a usage payload are used: v1 rows need
//	        role='assistant' and a non-null tokens object, v2 rows need
//	        type='assistant' or type='compaction' (session_message also stores
//	        agent-switched / model-switched rows that carry no usage).
//	        `type='compaction'` rows are the compaction requests themselves; they
//	        carry their own usage and additionally mark a context replacement
//	        point, which is reported as model.BoundaryCompact on the first
//	        request after them.
//	json    <root>/storage/message/<sessionID>/<messageID>.json  (legacy, pre-sqlite)
//	        <root>/storage/session/<projectID>/<sessionID>.json
//
// Token accounting. opencode reports tokens{input,output,reasoning,cache{read,write}}
// where `input` already excludes the cache classes, so the classes map
// one-to-one onto model.Tokens with no subtraction:
//
//	input       -> Tokens.Input      (clamped at 0)
//	output      -> Tokens.Output
//	reasoning   -> Tokens.Reasoning
//	cache.read  -> Tokens.CacheRead
//	cache.write -> Tokens.CacheWrite
//
// Model and provider come from modelID/providerID (or the nested
// model{id,providerID}) and are never inferred: an absent provider stays empty
// (docs/ARCHITECTURE.md#cost-semantics). Cost is only reported when the log carries a finite, positive
// `cost` (opencode writes 0 for "unknown").
//
// Dedup. Every event is keyed "opencode:<message id>", which is stable across
// rescans and identical for a message seen through both the sqlite tables and a
// leftover legacy JSON file. Repeated observations of one key inside a single
// parse are merged field-wise with per-class maxima (opencode appends token
// usage to a message after the row first appears).
//
// Incrementality. Whole-file sources (legacy JSON) use the standard
// size/mtime/fingerprint cursor. SQLite is different: SQLite rewrites its
// header (change counter) on every commit, so the first-4KB fingerprint of a db
// changes whenever *anything* is written. The cursor therefore stores a
// combined signature (db size+mtime plus -wal/-shm size+mtime) for the cheap
// unchanged fast path and an (time_updated, id) watermark in Cursor.Extra to
// resume the message keyset; the watermark is dropped when it is ahead of the
// database's own MAX(time_updated), which is how a replaced or recreated db is
// detected. SQLite is opened read-only (file:...?mode=ro&immutable=0) with a
// busy timeout and `PRAGMA query_only`, and a locked or unreadable db simply
// returns the previous cursor so the next scan retries.
//
// The SQLite side of this parser — the read-only connection, the schema probe,
// the watermarks and the usage accumulator — is the shared sqlitecore module
// (internal/harness/opencode/sqlitecore), which the MiMo Code parser reads too:
// MiMo Code keeps OpenCode's storage engine, so the deep module is reused
// rather than copied, and this package keeps only what is OpenCode-specific.
package opencode

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/opencode/sqlitecore"
	"github.com/zzstar101/mytoken/internal/model"
)

const (
	kindJSON   = "json"
	kindSQLite = "sqlite"

	dbName        = "opencode.db"
	storageDir    = "storage"
	messageSubdir = "message"
	sessionSubdir = "session"
)

// Parser reads OpenCode usage from one or more data directories.
type Parser struct {
	roots []string
	// prov recomputes this parser's default-root provenance from the current
	// environment. It is set only on the registry singleton; a parser built for
	// explicit roots reports those roots instead.
	prov func() ([]harness.Root, error)
}

var _ harness.Parser = (*Parser)(nil)

func init() { harness.Register(New()) }

// New returns a parser rooted at the default OpenCode data directory.
func New() *Parser { return &Parser{roots: defaultRoots(), prov: rootProvenance} }

// NewWithRoot returns a parser rooted at root.
func NewWithRoot(root string) *Parser { return NewWithRoots(root) }

// NewWithRoots returns a parser rooted at the given directories. An empty list
// falls back to the default roots.
func NewWithRoots(roots ...string) *Parser {
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
	if len(out) == 0 {
		out = defaultRoots()
	}
	return &Parser{roots: out}
}

// rootProvenance reports OpenCode's default roots and where each came from.
// MYTOKEN_OPENCODE_DIRS replaces the whole set and OPENCODE_DATA_HOME is an
// explicit harness-specific override; the XDG lookup is a generic default.
func rootProvenance() ([]harness.Root, error) {
	if v, ok := harness.Getenv("MYTOKEN_OPENCODE_DIRS"); ok {
		dirs, err := harness.SplitEnvList("MYTOKEN_OPENCODE_DIRS", v)
		if err != nil {
			return nil, err
		}
		return harness.EnvRoots("MYTOKEN_OPENCODE_DIRS", dirs...), nil
	}
	if v, ok := harness.Getenv("OPENCODE_DATA_HOME"); ok {
		return harness.EnvRoots("OPENCODE_DATA_HOME", v), nil
	}
	return harness.BuiltinRoots(builtinRoots()...), nil
}

// builtinRoots is the generic platform data directory OpenCode falls back to.
func builtinRoots() []string {
	if v := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); v != "" {
		return []string{filepath.Join(v, "opencode")}
	}
	return []string{filepath.Join(harness.Home(), ".local", "share", "opencode")}
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

func (p *Parser) Harness() model.Harness { return model.OpenCode }

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

func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

// Discover returns every OpenCode database and legacy JSON file below the
// configured roots.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	var out []harness.Source
	seen := map[string]bool{}
	add := func(path, kind string) {
		path = filepath.Clean(path)
		if seen[path] {
			return
		}
		seen[path] = true
		out = append(out, harness.Source{Path: path, Kind: kind})
	}
	for _, root := range p.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fi, err := os.Stat(root)
		if err != nil {
			continue
		}
		if !fi.IsDir() {
			if isDBName(fi.Name()) {
				add(root, kindSQLite)
			}
			continue
		}
		if entries, err := os.ReadDir(root); err == nil {
			for _, e := range entries {
				if e.IsDir() || !isDBName(e.Name()) {
					continue
				}
				add(filepath.Join(root, e.Name()), kindSQLite)
			}
		}
		for _, dir := range storageDirs(root) {
			_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				if d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".json") {
					return nil
				}
				add(path, kindJSON)
				return nil
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// isDBName reports whether name looks like an OpenCode sqlite database.
func isDBName(name string) bool {
	if name == dbName {
		return true
	}
	return strings.HasPrefix(name, "opencode-") && strings.HasSuffix(name, ".db")
}

// storageDirs returns the legacy storage trees below root, if any.
func storageDirs(root string) []string {
	bases := []string{root, filepath.Join(root, storageDir)}
	var out []string
	for _, base := range bases {
		for _, name := range []string{messageSubdir, sessionSubdir} {
			dir := filepath.Join(base, name)
			if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
				out = append(out, dir)
			}
		}
	}
	return out
}

// Parse reads the events a single source added since cur.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if err := ctx.Err(); err != nil {
		return harness.Batch{Next: cur}, err
	}
	if src.Kind == kindSQLite {
		return p.parseDB(ctx, src, cur)
	}
	return p.parseLegacyJSON(ctx, src, cur)
}

// ---------------------------------------------------------------------------
// sqlite
// ---------------------------------------------------------------------------

// The read-only SQLite machinery — the read-only connection, schema probing,
// the (time_updated, id) watermarks and the usage accumulator — is the
// sqlitecore module this package shares with every harness whose storage engine
// is OpenCode's.
//
// profile tells that accumulator how OpenCode differs from another harness: its
// identity, its dedup-key namespace, how a session's directory becomes the
// stored project path, and how a stored session title is mapped.
var profile = sqlitecore.Profile{
	Harness:   model.OpenCode,
	KeyPrefix: "opencode:",
	Project: func(info sqlitecore.Session, p sqlitecore.Payload) string {
		return sqlitecore.FirstNonEmpty(info.Directory, p.Path.Root)
	},
	Title: harness.Title,
}

// dbState is the incremental watermark persisted in Cursor.Extra. Each message
// generation keeps its own watermark: the tables are read in sequence, so one
// shared watermark let the newest v1 row hide older v2 rows, which were then
// skipped by every later scan too.
type dbState struct {
	V1 sqlitecore.Watermark `json:"m"`
	V2 sqlitecore.Watermark `json:"sm"`
	// Compact is the per-session compaction boundary state. It lives in the
	// cursor because a compaction row and the first request after it routinely
	// arrive in different incremental batches.
	Compact map[string]sqlitecore.Mark `json:"c,omitempty"`
}

// UnmarshalJSON also accepts the legacy single-watermark cursor
// ({"u":…,"i":…}). That watermark was advanced by both tables, so it is only
// trustworthy for the v1 `message` table; the v2 `session_message` table is
// replayed from the beginning once, and the dedup keys keep the re-read
// idempotent for consumers that merge by DedupKey.
func (s *dbState) UnmarshalJSON(data []byte) error {
	var wire struct {
		M  *sqlitecore.Watermark      `json:"m"`
		SM *sqlitecore.Watermark      `json:"sm"`
		C  map[string]sqlitecore.Mark `json:"c"`
		U  *int64                     `json:"u"`
		I  *string                    `json:"i"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*s = dbState{}
	if wire.M != nil {
		s.V1 = *wire.M
	}
	if wire.SM != nil {
		s.V2 = *wire.SM
	}
	if wire.C != nil {
		s.Compact = wire.C
	}
	if wire.U != nil {
		s.V1.Updated = *wire.U
	}
	if wire.I != nil {
		s.V1.ID = *wire.I
	}
	return nil
}

// messageTable is one generation of message storage together with the cursor
// watermark that resumes it.
type messageTable struct {
	spec sqlitecore.Spec
	wm   *sqlitecore.Watermark
}

// messageTables names the message tables of both storage generations and the
// predicate that keeps usage-carrying rows from each.
//
// The v2 table also stores the compaction requests themselves
// (type 'compaction'). Their usage is a real request and must stay in the
// ledger; the row additionally marks a context replacement point. Rows still
// being written ('running') carry no tokens and are skipped.
func (s *dbState) messageTables() []messageTable {
	return []messageTable{
		{
			spec: sqlitecore.Spec{
				Table:  "message",
				Filter: "json_valid(data) AND json_extract(data,'$.role')='assistant' AND json_extract(data,'$.tokens') IS NOT NULL",
			},
			wm: &s.V1,
		},
		{
			spec: sqlitecore.Spec{
				Table:    "session_message",
				Filter:   "json_valid(data) AND type IN ('assistant','compaction') AND json_extract(data,'$.tokens') IS NOT NULL",
				KindExpr: "type",
			},
			wm: &s.V2,
		},
	}
}

// parseDB reads one database incrementally: each message generation from its
// own persisted watermark, the sessions from whichever session tables exist,
// and the compaction state from the cursor.
func (p *Parser) parseDB(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	sig, err := sqlitecore.Stat(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	fp, err := sqlitecore.Fingerprint(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	if sqlitecore.Unchanged(cur, sig, fp) {
		return harness.Batch{Next: cur}, nil
	}

	var state dbState
	if cur.Extra != "" {
		_ = json.Unmarshal([]byte(cur.Extra), &state)
	}

	snap, err := sqlitecore.Open(src.Path)
	if err != nil {
		// Locked, unreadable or not a database: keep the cursor and retry on
		// the next scan.
		return harness.Batch{Next: cur}, nil
	}
	defer snap.Close()

	acc := sqlitecore.NewAccumulator(profile)
	if state.Compact == nil {
		state.Compact = map[string]sqlitecore.Mark{}
	}
	acc.UseMarks(state.Compact)

	sessions := snap.Sessions(ctx)
	for _, id := range sqlitecore.SortedKeys(sessions) {
		acc.Session(sessions[id])
	}
	for _, t := range state.messageTables() {
		*t.wm = snap.Reset(ctx, t.spec, *t.wm)
		*t.wm = snap.Messages(ctx, t.spec, *t.wm, func(row sqlitecore.Row) {
			payload, ok := sqlitecore.Decode([]byte(row.Data))
			if !ok {
				// Not a JSON object, so not a message: the row states nothing
				// this parser could count. sqlitecore.Decode already tolerates a
				// wrong-typed optional field, so only a value that carries no
				// message at all reaches here, and the watermark above keeps
				// advancing: holding it would re-read this row and everything
				// newer on every pass without ever making progress.
				return
			}
			// The column is authoritative but not reliable: older rows carry a
			// NULL or empty session_id, and these generations copy the session
			// into the message payload as well. Without the fallback every such
			// row reaches the store without a session, the store rejects the
			// whole batch, and because the batch is never written the cursor
			// never moves: one row freezes the harness for good.
			sid := sqlitecore.FirstNonEmpty(row.SessionID, payload.SessionID, payload.SnakeSessionID)
			if sid == "" {
				// Nothing on the row names a session, not even the payload. The
				// row cannot be attributed at this generation, so it is skipped:
				// holding the watermark for it would stall every later row, and
				// inventing a session would file the usage under a fiction. The
				// watermark inside Messages has already moved past it.
				return
			}
			acc.Emit(row.ID, sid, payload, sessions[sid], row.Updated, row.Kind == "compaction")
		})
	}

	stateJSON, err := json.Marshal(state)
	if err != nil {
		stateJSON = nil
	}
	return acc.Batch(sqlitecore.Next(sig, fp, string(stateJSON))), nil
}

// ---------------------------------------------------------------------------
// legacy JSON storage
// ---------------------------------------------------------------------------

// legacyFile is the union of the legacy message and session info shapes; a
// non-empty role marks a message file.
type legacyFile struct {
	messagePayload
	Directory string `json:"directory"`
	ParentID  string `json:"parentID"`
	Title     string `json:"title"`
	ProjectID string `json:"projectID"`
	Version   string `json:"version"`
}

// decodeLegacy projects one legacy storage document onto legacyFile. The
// document is flat, so the message fields and the session fields sit side by
// side and one wrong-typed value used to cost the whole file — the tokens of a
// message, or the title and directory of a session — while the file cursor
// advanced past it. sqlitecore.Decode already projects the message fields
// tolerantly (they are the same MessageV2.Info shape, minus the wrapper), and
// the session-only fields beside them are read individually for the same reason.
// The document only has to be a JSON object to be read at all.
func decodeLegacy(raw []byte) (legacyFile, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return legacyFile{}, false
	}
	payload, _ := sqlitecore.Decode(raw)
	file := legacyFile{messagePayload: payload}
	legacyString(fields, "directory", &file.Directory)
	legacyString(fields, "parentID", &file.ParentID)
	legacyString(fields, "title", &file.Title)
	legacyString(fields, "projectID", &file.ProjectID)
	legacyString(fields, "version", &file.Version)
	return file, true
}

// legacyString reads one of the session-only fields of a legacy document,
// ignoring a value that does not fit: these fields have no usage to lose, so an
// unexpected type costs the field and nothing else.
func legacyString(fields map[string]json.RawMessage, key string, dst *string) {
	if value, ok := fields[key]; ok {
		_ = json.Unmarshal(value, dst)
	}
}

func (p *Parser) parseLegacyJSON(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	sig, err := sqlitecore.Stat(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	fp, err := sqlitecore.Fingerprint(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	next := sqlitecore.Next(sig, fp, "")
	if sqlitecore.Unchanged(cur, sig, fp) {
		return harness.Batch{Next: cur}, nil
	}
	raw, err := os.ReadFile(src.Path)
	if err != nil {
		return harness.Batch{Next: next}, nil
	}
	file, ok := decodeLegacy(raw)
	if !ok {
		return harness.Batch{Next: next}, nil
	}
	acc := sqlitecore.NewAccumulator(profile)
	stem := strings.TrimSuffix(filepath.Base(src.Path), filepath.Ext(src.Path))
	if file.Role == "" {
		// Session info file: <projectID>/<sessionID>.json
		acc.Session(sqlitecore.Session{
			ID:        sqlitecore.FirstNonEmpty(file.ID, stem),
			ParentID:  file.ParentID,
			Directory: file.Directory,
			Title:     file.Title,
			Created:   file.Time.Created.Int64(),
			Updated:   file.Time.Updated.Int64(),
		})
		return acc.Batch(next), nil
	}
	// Message file. As in the sqlite generation only assistant messages carry
	// usage; a file without a complete usage payload is a partial write and is
	// retried after the next modification.
	if file.Role != "assistant" || file.Tokens == nil || file.Tokens.Cache == nil || file.Time.Created == nil {
		return harness.Batch{Next: next}, nil
	}
	id := sqlitecore.FirstNonEmpty(file.ID, stem)
	sid := sqlitecore.FirstNonEmpty(file.SessionID, file.SnakeSessionID)
	if sid == "" {
		// <root>/storage/message/<sessionID>/<messageID>.json
		if parent := filepath.Base(filepath.Dir(src.Path)); parent != "" && parent != "." {
			sid = parent
		}
	}
	acc.Emit(id, sid, file.messagePayload, sqlitecore.Session{}, sig.Mod.UnixMilli(), false)
	return acc.Batch(next), nil
}

// ---------------------------------------------------------------------------
// payload decoding
// ---------------------------------------------------------------------------

// Usage payloads are decoded by sqlitecore.Payload, which models the
// MessageV2.Info document OpenCode stores in a message's `data` column. These
// aliases are the names this package has always used for it.
type (
	messagePayload = sqlitecore.Payload
	tokenPayload   = sqlitecore.TokenPayload
)
