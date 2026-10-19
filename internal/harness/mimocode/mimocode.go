// Package mimocode parses MiMo Code's local usage database offline.
//
// MiMo Code (github.com/XiaomiMiMo/MiMo-Code) is a fork of OpenCode and keeps
// OpenCode's MessageV2 storage engine, so its usage is not read by copying
// OpenCode's parser: the read-only SQLite machinery both harnesses need lives
// once in internal/harness/opencode/sqlitecore, and this package is only the
// MiMo-specific surface — where MiMo Code keeps its data, which files count as
// its databases, its own source identity and its own event-key namespace.
//
// Storage, confirmed against the official sources:
//
//   - packages/shared/src/global.ts: a non-empty, absolute MIMOCODE_HOME moves
//     the data directory to <home>/data; otherwise the platform data directory
//     gains a `mimocode` level (XDG_DATA_HOME or the OS default).
//   - packages/cli/src/storage/db.ts: MIMOCODE_DB names the database file. It is
//     taken as an absolute path, or resolved relative to the data directory.
//     The default channel uses mimocode.db; every other channel uses
//     mimocode-<channel>.db. The database runs in WAL mode.
//   - packages/cli/src/session/session.sql.ts + message-v2.ts: the `session`
//     table carries parent_id and the conversation directory, and `message.data`
//     holds a MessageV2.Info document with the OpenCode v1 usage shape
//     (role, modelID/providerID, cost, tokens{input,output,reasoning,cache}).
//
// Only numbers and identifiers are ever reported. MiMo Code stores conversation
// text in session.title, in the assistant message body and in the `part` table;
// none of it is read, and the working directory is reduced to its basename
// before it is stored. Credentials (auth.json, .credentials.json) and config
// are never opened — discovery reports database files only.
//
// HarnessID is deliberately a local constant for now: wiring MiMo Code into the
// shared model.Harness constants and the app registry is the maintainer's step,
// and this package must not edit shared files. The value is what the identity
// must be until then.
package mimocode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/opencode/sqlitecore"
	"github.com/zzstar101/mytoken/internal/model"
)

// HarnessID is the source identity of MiMo Code. It is separate from
// model.OpenCode: the same conversation copied by both tools must stay two
// sources.
const HarnessID model.Harness = "mimo-code"

// keyPrefix namespaces every event key this parser emits. It must never collide
// with OpenCode's, because a MiMo Code row and an OpenCode row can carry the
// same message id.
const keyPrefix = string(HarnessID) + ":"

const (
	kindSQLite = "sqlite"

	// dbName is the default-channel database; channelDBPrefix/dbSuffix build the
	// `mimocode-<safe-channel>.db` names of every other channel.
	dbName         = "mimocode.db"
	channelDBGlob  = "mimocode-*.db"
	channelDBStart = "mimocode-"
	dbSuffix       = ".db"

	// dataDirName is the level MiMo Code adds to a platform data directory.
	dataDirName = "mimocode"
	// homeDataDir is the directory MIMOCODE_HOME puts the data in.
	homeDataDir = "data"

	envDirs = "MYTOKEN_MIMO_CODE_DIRS"
	envHome = "MIMOCODE_HOME"
	envDB   = "MIMOCODE_DB"

	// memoryDB is SQLite's in-memory database name. It is not a file, so it can
	// never be an offline source.
	memoryDB = ":memory:"
)

// profile tells the shared accumulator how this harness differs from OpenCode:
// its identity, its key namespace, and the two deliberate divergences — MiMo
// Code keeps a project's directory basename rather than the absolute path, and
// a session title is conversation text that is never stored (Title is nil).
var profile = sqlitecore.Profile{
	Harness:   HarnessID,
	KeyPrefix: keyPrefix,
	Project: func(info sqlitecore.Session, p sqlitecore.Payload) string {
		return baseName(sqlitecore.FirstNonEmpty(info.Directory, p.Path.Cwd, p.Path.Root))
	},
}

// Parser reads MiMo Code usage from one or more data directories.
type Parser struct {
	roots []string
	// prov recomputes this parser's default-root provenance from the current
	// environment. It is set only on the registry singleton; a parser built for
	// explicit roots reports those roots instead.
	prov func() ([]harness.Root, error)
}

var (
	_ harness.Parser     = (*Parser)(nil)
	_ harness.Rooted     = (*Parser)(nil)
	_ harness.Provenance = (*Parser)(nil)
)

func init() { harness.Register(New()) }

// New returns a parser rooted at the default MiMo Code data directory.
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

// rootProvenance reports MiMo Code's default roots and where each came from.
//
// MYTOKEN_MIMO_CODE_DIRS replaces the whole set, MIMOCODE_DB names one database
// file, and MIMOCODE_HOME moves the data directory. An explicit value is taken
// literally: a malformed one reports an error and yields no roots rather than
// falling back to a lower-priority default.
func rootProvenance() ([]harness.Root, error) {
	if v, ok := harness.Getenv(envDirs); ok {
		dirs, err := harness.SplitEnvList(envDirs, v)
		if err != nil {
			return nil, err
		}
		return harness.EnvRoots(envDirs, dirs...), nil
	}
	if v, ok := harness.Getenv(envDB); ok {
		path, err := resolveDB(envDB, v)
		if err != nil {
			return nil, err
		}
		return harness.EnvRoots(envDB, path), nil
	}
	dataDir, err := dataDirPath()
	if err != nil {
		return nil, err
	}
	if v, ok := harness.Getenv(envHome); ok {
		return harness.EnvRoots(envHome, filepath.Join(v, homeDataDir)), nil
	}
	return harness.BuiltinRoots(dataDir), nil
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

// dataDirPath is MiMo Code's data directory: <MIMOCODE_HOME>/data when the
// override is a non-empty absolute path, otherwise `mimocode` below the XDG
// data directory.
//
// Upstream is `path.join(xdgData!, "mimocode")` (packages/shared/src/global.ts)
// with xdg-basedir@5.1.0, whose xdgData is `$XDG_DATA_HOME` or
// `<os.homedir()>/.local/share` — on every OS: xdg-basedir branches on nothing
// but the environment, so `~/Library/Application Support/mimocode` and
// `%LOCALAPPDATA%/mimocode` (which the upstream README advertises) are paths
// MiMo Code never writes. An empty XDG_DATA_HOME means unset, matching that
// library's `||`.
func dataDirPath() (string, error) {
	if v, ok := harness.Getenv(envHome); ok {
		if !filepath.IsAbs(v) {
			return "", &envError{env: envHome, value: v, reason: "must be an absolute path"}
		}
		return filepath.Join(v, homeDataDir), nil
	}
	if v := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); v != "" {
		return filepath.Join(v, dataDirName), nil
	}
	return filepath.Join(harness.Home(), ".local", "share", dataDirName), nil
}

// resolveDB turns a MIMOCODE_DB value into the database file it names: an
// absolute path stays as given, anything else is relative to the data
// directory. SQLite's in-memory database is refused — it is a live process's
// storage, not a file this parser could read read-only.
func resolveDB(env, value string) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" || strings.EqualFold(v, memoryDB) || strings.HasPrefix(v, ":") {
		return "", &envError{env: env, value: value, reason: "does not name a database file"}
	}
	if filepath.IsAbs(v) {
		return filepath.Clean(v), nil
	}
	dataDir, err := dataDirPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, filepath.Clean(v)), nil
}

// envError reports an explicit environment override that names nothing this
// parser can read.
type envError struct {
	env    string
	value  string
	reason string
}

func (e *envError) Error() string {
	return "environment variable " + e.env + " is set to " + strconv.Quote(e.value) + " but " + e.reason
}

// RootProvenance implements harness.Provenance.
func (p *Parser) RootProvenance() ([]harness.Root, error) {
	if p.prov == nil {
		return harness.ExplicitRoots(p.roots...), nil
	}
	return p.prov()
}

// Harness implements harness.Parser.
func (p *Parser) Harness() model.Harness { return HarnessID }

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

// Roots implements harness.Parser.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

// Discover returns the MiMo Code databases below the configured roots: exactly
// the default-channel mimocode.db and the channel databases mimocode-<channel>.db
// directly inside a root directory. A root that is itself a file is taken
// literally, which is how an explicit MIMOCODE_DB is read. Nothing else is
// opened or reported — not the write-ahead log next to a database, not
// credentials, config or logs, and not nested directories.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	var out []harness.Source
	seen := map[string]bool{}
	add := func(path string) {
		path = filepath.Clean(path)
		if seen[path] {
			return
		}
		seen[path] = true
		out = append(out, harness.Source{Path: path, Kind: kindSQLite})
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
			add(root)
			continue
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !isDBName(e.Name()) {
				continue
			}
			add(filepath.Join(root, e.Name()))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// isDBName reports whether name is a MiMo Code database: the default channel
// database, or a `mimocode-<safe-channel>.db` channel database.
func isDBName(name string) bool {
	if name == dbName {
		return true
	}
	return strings.HasPrefix(name, channelDBStart) && strings.HasSuffix(name, dbSuffix)
}

// Parse reads the events a single source added since cur.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if err := ctx.Err(); err != nil {
		return harness.Batch{Next: cur}, err
	}
	if src.Kind != kindSQLite {
		// A source kind this parser does not read has nothing to contribute; the
		// cursor stays where it is.
		return harness.Batch{Next: cur}, nil
	}
	return parseDB(ctx, src, cur), nil
}

// ---------------------------------------------------------------------------
// sqlite
// ---------------------------------------------------------------------------

// dbState is the incremental state persisted in Cursor.Extra: the message
// table's (time_updated, id) watermark. MiMo Code's schema has one message
// table, so one watermark is enough to resume without re-reading rows.
type dbState struct {
	Version int                  `json:"v"`
	Message sqlitecore.Watermark `json:"message"`
}

// messageFilter selects the rows that carry assistant usage. It is the same
// predicate OpenCode v1 rows satisfy, because MiMo Code writes the same
// MessageV2.Info document; the JSON functions are SQLite's, so a malformed row
// is skipped rather than failing the scan.
const messageFilter = "json_valid(data) AND json_extract(data,'$.role')='assistant' AND json_extract(data,'$.tokens') IS NOT NULL"

// messageSpec is the message table of MiMo Code's schema.
func messageSpec() sqlitecore.Spec {
	return sqlitecore.Spec{Table: "message", Filter: messageFilter}
}

// parseDB reads one database incrementally. A database that vanished, is
// locked, or is not a SQLite database at all is not an error: the cursor is
// kept and the next scan retries, so a foreign file named mimocode.db cannot
// fail a scan.
func parseDB(ctx context.Context, src harness.Source, cur harness.Cursor) harness.Batch {
	sig, err := sqlitecore.Stat(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}
	}
	fp, err := sqlitecore.Fingerprint(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}
	}
	if sqlitecore.Unchanged(cur, sig, fp) {
		return harness.Batch{Next: cur}
	}

	var state dbState
	if cur.Extra != "" {
		_ = json.Unmarshal([]byte(cur.Extra), &state)
	}

	snap, err := sqlitecore.Open(src.Path)
	if err != nil {
		// Locked, unreadable or not a database: keep the cursor and retry on the
		// next scan.
		return harness.Batch{Next: cur}
	}
	defer snap.Close()

	state.Message = snap.Reset(ctx, messageSpec(), state.Message)

	acc := sqlitecore.NewAccumulator(profile)
	sessions := snap.Sessions(ctx)
	for _, id := range sqlitecore.SortedKeys(sessions) {
		acc.Session(sessions[id])
	}
	state.Message = snap.Messages(ctx, messageSpec(), state.Message, func(row sqlitecore.Row) {
		payload, ok := sqlitecore.Decode([]byte(row.Data))
		if !ok {
			// Not a JSON object, so not a message: nothing to count, and the
			// watermark keeps advancing. A wrong-typed optional field no longer
			// lands here (sqlitecore.Decode projects field by field), and
			// holding the watermark back would re-read the row forever. The
			// usage block is taken from the message itself, never from the
			// step-finish `part` row that copies it.
			return
		}
		sid := sqlitecore.FirstNonEmpty(row.SessionID, payload.SessionID, payload.SnakeSessionID)
		acc.Emit(row.ID, sid, payload, sessions[sid], row.Updated, false)
	})

	state.Version = 1
	extra, err := json.Marshal(state)
	if err != nil {
		extra = nil
	}
	return acc.Batch(sqlitecore.Next(sig, fp, string(extra)))
}

// baseName reduces a working directory to its basename. Nothing longer than the
// last path element is ever stored: an absolute project path leaks the user's
// directory layout.
func baseName(dir string) string {
	d := strings.TrimSpace(dir)
	if d == "" {
		return ""
	}
	base := filepath.Base(filepath.Clean(d))
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	return base
}
