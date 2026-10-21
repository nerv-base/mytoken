// Package trae reads the token usage Trae records in its local ai-agent
// database.
//
// Trae (trae.cn, ByteDance's AI IDE) keeps every conversation its agent ran in
// one SQLite database per installed product family member:
//
//	~/Library/Application Support/Trae CN/ModularData/ai-agent/database.db        (macOS)
//	%APPDATA%\Trae CN\ModularData\ai-agent\database.db                            (Windows)
//	~/.config/Trae CN/ModularData/ai-agent/database.db                            (Linux)
//
// The installed products are "Trae CN", "TRAE SOLO CN" and "Trae"; each may be
// present or absent, and Trae CN is where the SOLO agent's turns land too, so
// all of them are watched. MYTOKEN_TRAE_DIRS overrides the whole root list with
// a list of directories (the ai-agent directory, the ModularData directory, the
// product data directory, or the database file itself).
//
// The database is SQLCipher-encrypted with a raw 32-byte key: the file has no
// SQLite header, page 1 starts with a 16-byte salt, and every page carries an
// AES-256-CBC body plus a 16-byte IV and a 64-byte HMAC-SHA512 trailer (see
// sqlcipher.go). The key is generated per machine, not shipped with the app, and
// the app never writes it to disk — it lives only in the memory of a running
// Trae process — so MyToken needs the user to supply it once. It is machine
// wide: the same key opens every Trae product's database on this machine.
// MYTOKEN_TRAE_DBKEY, or the key stored in MyToken's settings, supplies it in
// the form SQLCipher itself uses for the pragma, x'<64 hex digits><32 hex
// digits>'; the trailing salt is per file and ignored.
//
// Schema. One row of chat_turn is one model request: the assistant's answer to
// one user message. Its `context` column is a JSON document holding the usage
// the provider reported for that turn:
//
//	context.token_usage.prompt_tokens              input tokens, cache included
//	context.token_usage.completion_tokens          output tokens, reasoning included
//	context.token_usage.total_tokens               prompt + completion
//	context.token_usage.reasoning_tokens           the reasoning part of completion
//	context.token_usage.cache_read_input_tokens    the cached part of prompt
//	context.token_usage.cache_creation_input_tokens
//	context.chat_start_time / chat_end_time        epoch milliseconds
//	context.persist_user_message_context.model_info.config_name  model id
//
// prompt_tokens is inclusive: total_tokens equals prompt_tokens plus
// completion_tokens, so the cache buckets are subtracted back out and reported
// as Tokens.CacheRead/CacheWrite, exactly as the ZCode reader does. An unknown
// bucket stays 0, a bucket larger than the prompt is clamped to it, and the
// classes always sum to the reported total. Tokens.Reasoning is the part of
// completion_tokens it was already counted in.
//
// Trae's other token numbers are deliberately not read. history_v2.token_usage
// and its server_history_info mirror count the tokens a context step held, not
// the tokens a request consumed (they sum to the prompt of the whole
// conversation), so adding them would double count every turn.
// chat_session_goal.tokens_used is a goal's running total, an aggregate of the
// very turns read here. chat_message_*, agent_run and history_v2 carry no
// per-request usage at all, and chat_turn.context.fee_usage is null unless the
// turn was billed in credits, whose unit Trae does not record — so no cost is
// reported and the dashboard prices the tokens itself.
//
// Read-only, offline. The reader never opens the encrypted database: it decodes
// it (and its write-ahead log) into a private temporary file, reads that with
// PRAGMA query_only, and removes it. Conversation text is never selected: the
// queries name the usage JSON paths and the session's title and project path
// only.
//
// Incrementality. The whole database is re-read on every scan and DedupKey is
// Trae's own turn id, so a rescan is idempotent. The narrow JSON projection
// keeps a full re-read cheap (a few hundred bytes per turn), and re-reading is
// what makes a turn whose usage grew while it streamed correct itself: the store
// upserts an event whose numbers changed. A turn is counted from the moment it
// reports usage, including a failed or cancelled one, because the tokens were
// spent.
package trae

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/sqlitedsn"

	// The parser reads Trae's sqlite database; the pure-Go driver is the only
	// one this module links.
	_ "modernc.org/sqlite"
)

const (
	// envDirs replaces the default root list with a path list.
	envDirs = "MYTOKEN_TRAE_DIRS"
	// envKey supplies the database key without storing it in MyToken's settings.
	envKey = "MYTOKEN_TRAE_DBKEY"

	dbName     = "database.db"
	walName    = "database.db-wal"
	kindSQLite = "sqlite"

	// modularDir and agentDir are the path segments below a product's data
	// directory that hold the agent database.
	modularDir = "ModularData"
	agentDir   = "ai-agent"

	fingerprintBytes = 4096

	// HarnessID is the model harness id this reader serves.
	HarnessID model.Harness = model.Trae

	// parserVersion is the parse generation; it changes whenever the events a
	// given database yields change.
	parserVersion = 1
)

// productDirs are the data directories Trae's products create, per platform
// convention. An absent one is simply not found.
var productDirs = []string{"Trae CN", "TRAE SOLO CN", "Trae"}

// ErrKeyNeeded reports an encrypted database with no usable key: the reader
// found Trae's data but cannot open it until the user supplies the key once.
var ErrKeyNeeded = errors.New("trae: the ai-agent database is encrypted; set Trae's database key")

// Parser reads the ai-agent database of every Trae product on this machine.
type Parser struct {
	roots []string
	// key is the decoded 32-byte SQLCipher key; empty means "not supplied yet".
	key []byte
	// keyErr records a configured key that does not decode, so the settings page
	// can say what is wrong instead of silently reading nothing.
	keyErr error
	// prov recomputes the default roots' provenance from the current
	// environment. It is set only on the registry singleton.
	prov func() ([]harness.Root, error)
}

var (
	_ harness.Parser         = (*Parser)(nil)
	_ harness.Rooted         = (*Parser)(nil)
	_ harness.Provenance     = (*Parser)(nil)
	_ harness.Versioned      = (*Parser)(nil)
	_ harness.SourceSessions = (*Parser)(nil)
	_ harness.Keyed          = (*Parser)(nil)
	_ harness.NotReady       = (*Parser)(nil)
)

func init() { harness.Register(New()) }

// New returns a parser rooted at the default Trae data directories, using the
// key from MYTOKEN_TRAE_DBKEY when one is set.
func New() *Parser {
	p := &Parser{roots: defaultRoots(), prov: rootProvenance}
	if v, ok := harness.Getenv(envKey); ok {
		key, err := decodeKey(v)
		if err != nil {
			p.keyErr = err
		} else {
			p.key = key
		}
	}
	return p
}

// NewWithRoots returns a parser over exactly the given roots: each one may be
// the ai-agent directory, the ModularData directory, a product's data directory
// or the database file. An empty list stays empty — it never falls back to the
// built-in defaults.
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

// rootProvenance reports the default roots and where each came from.
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

// builtinRoots joins every product's data directory with the agent database
// directory, using the convention of the platform Trae ships on.
func builtinRoots() []string {
	var bases []string
	switch runtime.GOOS {
	case "darwin":
		if home := harness.Home(); home != "" {
			base := filepath.Join(home, "Library", "Application Support")
			for _, product := range productDirs {
				bases = append(bases, filepath.Join(base, product))
			}
		}
	case "windows":
		if base := strings.TrimSpace(os.Getenv("APPDATA")); base != "" {
			for _, product := range productDirs {
				bases = append(bases, filepath.Join(base, product))
			}
		}
	default:
		base := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
		if base == "" {
			if home := harness.Home(); home != "" {
				base = filepath.Join(home, ".config")
			}
		}
		if base != "" {
			for _, product := range productDirs {
				bases = append(bases, filepath.Join(base, product))
			}
		}
	}
	roots := make([]string, 0, len(bases))
	for _, base := range bases {
		roots = append(roots, filepath.Join(base, modularDir, agentDir))
	}
	return roots
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

// Harness implements harness.Parser.
func (p *Parser) Harness() model.Harness { return model.Trae }

// ParserVersion implements harness.Versioned.
func (p *Parser) ParserVersion() int { return parserVersion }

// WithRoots implements harness.Rooted.
func (p *Parser) WithRoots(roots ...string) harness.Parser {
	next := NewWithRoots(roots...)
	next.key = append([]byte(nil), p.key...)
	next.keyErr = p.keyErr
	return next
}

// WithKey implements harness.Keyed. An empty key clears the key: the parser
// then reports ErrKeyNeeded instead of reading anything. A key that does not
// decode is rejected here so the settings page can show the reason.
func (p *Parser) WithKey(key string) (harness.Parser, error) {
	next := &Parser{
		roots: append([]string(nil), p.roots...),
		prov:  p.prov,
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return next, nil
	}
	raw, err := decodeKey(key)
	if err != nil {
		return nil, err
	}
	next.key = raw
	return next, nil
}

// NotReady implements harness.NotReady: it names the missing key so the sources
// page can ask for it before a scan fails.
func (p *Parser) NotReady() string {
	if len(p.key) > 0 {
		return ""
	}
	if p.keyErr != nil {
		return "Trae's database key is not valid: " + p.keyErr.Error()
	}
	return "Trae's ai-agent database is encrypted: set the database key"
}

// Roots returns the configured roots; missing ones are ignored.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

// Discover finds the ai-agent database under every root.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	paths, err := p.DatabaseFiles(ctx)
	if err != nil || len(paths) == 0 {
		return nil, err
	}
	// A plaintext copy — a database the user decoded themselves, or a fixture —
	// needs no key; an encrypted one does, and saying so is more useful than
	// reporting an empty scan.
	out := make([]harness.Source, 0, len(paths))
	for _, path := range paths {
		encrypted, err := isEncrypted(path)
		if err != nil {
			continue
		}
		if encrypted && len(p.key) == 0 {
			return nil, ErrKeyNeeded
		}
		out = append(out, harness.Source{Path: path, Kind: kindSQLite})
	}
	return out, nil
}

// DatabaseFiles lists every database this reader would open, whether or not a
// key is available. Discover needs a key before it can say the same thing, so
// the key capture uses this to know what to prove a candidate against.
func (p *Parser) DatabaseFiles(ctx context.Context) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(path string) {
		if path == "" || seen[path] {
			return
		}
		st, err := os.Stat(path)
		if err != nil || st.IsDir() {
			return
		}
		seen[path] = true
		out = append(out, path)
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
		add(filepath.Join(root, dbName))                       // the ai-agent directory
		add(filepath.Join(root, agentDir, dbName))             // the ModularData directory
		add(filepath.Join(root, modularDir, agentDir, dbName)) // a product data directory
	}
	sort.Strings(out)
	return out, nil
}

// Parse reads every turn the database at src holds.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if err := ctx.Err(); err != nil {
		return harness.Batch{Next: cur}, err
	}
	st, err := os.Stat(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	walPath := filepath.Join(filepath.Dir(src.Path), walName)
	next := harness.Cursor{
		Size:    st.Size(),
		ModTime: st.ModTime(),
	}
	if ws, err := os.Stat(walPath); err == nil {
		next.Size += ws.Size()
		if ws.ModTime().After(next.ModTime) {
			next.ModTime = ws.ModTime()
		}
	}
	next.Offset = next.Size
	if fp, err := fingerprint(src.Path); err == nil {
		next.Fingerprint = fp
	}
	if cur.Size == next.Size && cur.ModTime.Equal(next.ModTime) && cur.Fingerprint == next.Fingerprint {
		// Nothing has been committed since the last scan: Trae appends to the
		// write-ahead log, so a new turn always changes its size or mtime.
		return harness.Batch{Next: cur}, nil
	}

	readPath := src.Path
	encrypted, err := isEncrypted(src.Path)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	if encrypted {
		if len(p.key) == 0 {
			return harness.Batch{Next: cur}, ErrKeyNeeded
		}
		pages, count, err := decodeDatabase(src.Path, walPath, p.key)
		if err != nil {
			return harness.Batch{Next: cur}, err
		}
		dir, err := os.MkdirTemp("", "mytoken-trae-")
		if err != nil {
			return harness.Batch{Next: cur}, err
		}
		defer os.RemoveAll(dir)
		readPath = filepath.Join(dir, dbName)
		if err := writeDatabase(readPath, pages, count); err != nil {
			return harness.Batch{Next: cur}, err
		}
	}

	events, sessions, err := readUsage(ctx, readPath)
	if err != nil {
		return harness.Batch{Next: cur}, err
	}
	return harness.Batch{Events: events, Sessions: sessions, Next: next}, nil
}

// SourceSessions implements harness.SourceSessions: every session the database
// describes, so a parser-version reset can scope its deletes.
func (p *Parser) SourceSessions(ctx context.Context, src harness.Source) ([]string, error) {
	batch, err := p.Parse(ctx, src, harness.Cursor{})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(batch.Sessions))
	for _, s := range batch.Sessions {
		if s.SessionID == "" || seen[s.SessionID] {
			continue
		}
		seen[s.SessionID] = true
		out = append(out, s.SessionID)
	}
	sort.Strings(out)
	return out, nil
}

// isEncrypted reports whether the file at path is a SQLCipher database rather
// than a plaintext SQLite one.
func isEncrypted(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var head [len(sqliteMagic)]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return false, err
	}
	return string(head[:]) != sqliteMagic, nil
}

// fingerprint hashes the first bytes of the database file. SQLCipher leaves the
// header alone, so this only catches a replaced file — the size and mtime of
// both the database and its log are the real change signal.
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

// turnQuery reads one row per model request. json_extract keeps the wide
// context document — render state, document contexts, the rewritten query — out
// of the result, so a full re-read costs a few hundred bytes per turn.
const turnQuery = `
SELECT turn_id, session_id, created_at,
       coalesce(json_extract(context,'$.chat_start_time'),0),
       coalesce(json_extract(context,'$.token_usage.prompt_tokens'),0),
       coalesce(json_extract(context,'$.token_usage.completion_tokens'),0),
       coalesce(json_extract(context,'$.token_usage.total_tokens'),0),
       coalesce(json_extract(context,'$.token_usage.reasoning_tokens'),0),
       coalesce(json_extract(context,'$.token_usage.cache_read_input_tokens'),0),
       coalesce(json_extract(context,'$.token_usage.cache_creation_input_tokens'),0),
       coalesce(json_extract(context,'$.persist_user_message_context.model_info.config_name'),''),
       coalesce(json_extract(context,'$.persist_user_message_context.model_info.model_name'),''),
       coalesce(json_extract(context,'$.persist_user_message_context.model_info.display_model_name'),'')
FROM chat_turn
WHERE deleted_at = 0 AND turn_id != '' AND json_valid(context)
  AND json_type(context,'$.token_usage') = 'object'
ORDER BY id`

// sessionQuery reads each session's title and project path.
const sessionQuery = `
SELECT s.session_id, coalesce(s.session_title,''), coalesce(s.created_at,0),
       coalesce(p.absolute_path,'')
FROM chat_session s
LEFT JOIN project p ON p.project_id = s.project_id
WHERE s.deleted_at = 0`

// readUsage turns a plaintext database into usage events and session metadata.
func readUsage(ctx context.Context, path string) ([]model.UsageEvent, []model.SessionMeta, error) {
	db, err := sql.Open("sqlite", sqlitedsn.URI(path, "mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)"))
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return nil, nil, err
	}

	metas, err := readSessions(ctx, db)
	if err != nil {
		return nil, nil, err
	}

	rows, err := db.QueryContext(ctx, turnQuery)
	if err != nil {
		if missingTable(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	defer rows.Close()

	events := make([]model.UsageEvent, 0, 64)
	for rows.Next() {
		var (
			turnID, sessionID     string
			createdAt, startedAt  int64
			prompt, completion    int64
			total, reasoning      int64
			cacheRead, cacheWrite int64
			configName, modelName string
			displayName           string
		)
		if err := rows.Scan(&turnID, &sessionID, &createdAt, &startedAt,
			&prompt, &completion, &total, &reasoning,
			&cacheRead, &cacheWrite, &configName, &modelName, &displayName,
		); err != nil {
			if missingTable(err) {
				return nil, nil, nil
			}
			return nil, nil, err
		}
		tokens := tokenSplit(prompt, completion, reasoning, cacheRead, cacheWrite)
		if tokens.IsZero() {
			continue
		}
		events = append(events, model.UsageEvent{
			Harness:   model.Trae,
			DedupKey:  "trae:" + turnID,
			SessionID: sessionID,
			Timestamp: turnTime(startedAt, createdAt),
			Model:     pick(configName, modelName, displayName),
			Tokens:    tokens,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	// The project belongs to the session, not the turn; copying it onto the
	// event lets the dashboard attribute spend per workspace without a join.
	for i := range events {
		events[i].ProjectPath = metas[events[i].SessionID].Project
	}
	sort.SliceStable(events, func(i, j int) bool {
		if !events[i].Timestamp.Equal(events[j].Timestamp) {
			return events[i].Timestamp.Before(events[j].Timestamp)
		}
		return events[i].DedupKey < events[j].DedupKey
	})

	sessions := sessionMetas(metas, events)
	return events, sessions, nil
}

// readSessions reads every session's title and project path, keyed by id.
func readSessions(ctx context.Context, db *sql.DB) (map[string]model.SessionMeta, error) {
	rows, err := db.QueryContext(ctx, sessionQuery)
	if err != nil {
		if missingTable(err) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	out := map[string]model.SessionMeta{}
	for rows.Next() {
		var sessionID, title, project string
		var createdAt int64
		if err := rows.Scan(&sessionID, &title, &createdAt, &project); err != nil {
			if missingTable(err) {
				return nil, nil
			}
			return nil, err
		}
		if sessionID == "" {
			continue
		}
		out[sessionID] = model.SessionMeta{
			Harness:   model.Trae,
			SessionID: sessionID,
			Title:     harness.Title(title),
			Project:   project,
			StartedAt: epochSeconds(createdAt),
		}
	}
	return out, rows.Err()
}

// sessionMetas merges the session rows with the turns that were counted, so a
// session's bounds come from its usage and its title and project path from the
// conversation. A session whose turns are all empty keeps no metadata: a title
// with no spend behind it is not a session worth listing.
func sessionMetas(metas map[string]model.SessionMeta, events []model.UsageEvent) []model.SessionMeta {
	out := make([]model.SessionMeta, 0, len(metas))
	index := map[string]int{}
	for _, ev := range events {
		if ev.SessionID == "" {
			continue
		}
		i, ok := index[ev.SessionID]
		if !ok {
			meta := metas[ev.SessionID]
			meta.Harness = model.Trae
			meta.SessionID = ev.SessionID
			if meta.StartedAt.IsZero() {
				meta.StartedAt = ev.Timestamp
			}
			if meta.StartedAt.IsZero() || ev.Timestamp.Before(meta.StartedAt) {
				meta.StartedAt = ev.Timestamp
			}
			meta.UpdatedAt = ev.Timestamp
			index[ev.SessionID] = len(out)
			out = append(out, meta)
			continue
		}
		if ev.Timestamp.After(out[i].UpdatedAt) {
			out[i].UpdatedAt = ev.Timestamp
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}

// tokenSplit converts Trae's inclusive prompt and completion counts into
// MyToken's classes: cache reads and writes come out of the prompt, reasoning
// comes out of the completion, and both are clamped so a bucket can never push
// a class negative. prompt_tokens counts the cached tokens, and
// completion_tokens counts the reasoning tokens, so the classes sum to the
// total Trae reports.
func tokenSplit(prompt, completion, reasoning, cacheRead, cacheWrite int64) model.Tokens {
	prompt = clampInt(prompt)
	completion = clampInt(completion)
	reasoning = clampInt(reasoning)
	cacheRead = clampInt(cacheRead)
	cacheWrite = clampInt(cacheWrite)
	if cacheRead > prompt {
		cacheRead = prompt
	}
	rest := prompt - cacheRead
	if cacheWrite > rest {
		cacheWrite = rest
	}
	if reasoning > completion {
		reasoning = completion
	}
	return model.Tokens{
		Input:      prompt - cacheRead - cacheWrite,
		Output:     completion - reasoning,
		CacheRead:  cacheRead,
		CacheWrite: cacheWrite,
		Reasoning:  reasoning,
	}
}

func clampInt(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// pick returns the first non-empty value: the model id Trae bills with
// (config_name) first, then the model_name variant, then Trae's display label.
func pick(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// turnTime converts epoch milliseconds from the turn context, falling back to
// the row's epoch seconds. A missing timestamp stays unknown rather than
// becoming 1970.
func turnTime(millis, seconds int64) time.Time {
	if millis > 0 {
		return time.UnixMilli(millis).UTC()
	}
	return epochSeconds(seconds)
}

// epochSeconds converts an epoch-seconds column, which is 0 when unset.
func epochSeconds(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

// missingTable reports the driver error for a database that does not carry
// Trae's schema — an older build, or a different product's database.
func missingTable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no such table") || strings.Contains(msg, "no such column")
}
