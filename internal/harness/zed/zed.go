// Package zed parses the Zed editor's built-in agent threads.
//
// Roots: $MYTOKEN_ZED_DIRS when set, else the platform Zed threads directory —
// ~/Library/Application Support/Zed/threads on macOS, %LOCALAPPDATA%\Zed\threads
// on Windows, ~/.local/share/zed/threads elsewhere. Layout:
//
//	<root>/threads.db
//
// threads.db is a SQLite database with one row per agent thread. `data` holds the
// thread document, either zstd-compressed ("zstd") or plain JSON ("json"); a
// thread keeps its per-request token usage in request_token_usage and the whole
// thread's total in cumulative_token_usage. Only the model, that usage and the
// thread summary are read — message bodies are never parsed, and the request keys
// (user-message ids) are hashed before they enter a dedup key (docs/HARNESS.md §5).
//
// Token normalization (docs/HARNESS.md §4): Zed records Anthropic-shaped usage
// where input_tokens excludes cache hits, cache_creation_input_tokens is the
// cache write and cache_read_input_tokens the cache read. There is no reasoning
// field, so Reasoning stays 0 and no class is double counted.
package zed

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/sqlitedsn"
)

const (
	kindSQLite       = "sqlite"
	dbName           = "threads.db"
	fingerprintBytes = 4096
	busyTimeoutMS    = 5000
	maxThreadBytes   = 64 << 20 // decompressed thread document ceiling
	remainderKey     = "cumulative-remainder"
)

// Parser implements harness.Parser for the Zed editor.
type Parser struct {
	roots []string
}

// New returns a parser for the standard Zed locations.
func New() *Parser { return &Parser{roots: DefaultRoots()} }

// NewWithRoots returns a parser reading the given roots (tests).
func NewWithRoots(roots ...string) *Parser {
	return &Parser{roots: append([]string(nil), roots...)}
}

func init() { harness.Register(New()) }

// DefaultRoots returns the Zed threads directories for this machine.
func DefaultRoots() []string {
	if v := os.Getenv("MYTOKEN_ZED_DIRS"); v != "" {
		var out []string
		for _, p := range filepath.SplitList(v) {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, filepath.Clean(p))
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	switch runtime.GOOS {
	case "darwin":
		return []string{filepath.Join(home, "Library", "Application Support", "Zed", "threads")}
	case "windows":
		if appData := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); appData != "" {
			return []string{filepath.Join(appData, "Zed", "threads")}
		}
		return []string{filepath.Join(home, "AppData", "Local", "Zed", "threads")}
	default:
		return []string{filepath.Join(home, ".local", "share", "zed", "threads")}
	}
}

// Harness identifies this parser's logs.
func (p *Parser) Harness() model.Harness { return model.Zed }

// Roots returns the configured roots; missing ones are ignored.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

// Discover lists threads.db under every root. A root may also name the database
// itself when an override points straight at it.
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
		if root == "" {
			continue
		}
		st, err := os.Stat(root)
		if err != nil {
			continue
		}
		if st.IsDir() {
			add(filepath.Join(root, dbName))
			continue
		}
		if strings.EqualFold(filepath.Ext(root), ".db") {
			add(root)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Parse reads one threads.db from cur to the current end.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if src.Kind != kindSQLite {
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
	// Nothing changed since the last scan: hand the cursor back untouched so the
	// caller can verify it is stable.
	if cur.Fingerprint != "" && cur.Fingerprint == fp && cur.Size == sig.size && cur.ModTime.Equal(sig.mod) {
		return harness.Batch{Next: cur}, nil
	}
	db, err := openReadOnly(src.Path)
	if err != nil {
		// Locked or not a database: keep the cursor and retry on the next scan.
		return harness.Batch{Next: cur}, nil
	}
	defer db.Close()

	state := decodeState(cur.Extra)
	cols := tableColumns(ctx, db, "threads")
	if len(cols) == 0 {
		// Not a Zed database (or an old empty one); remember the signature so the
		// next scan can take the fast path.
		return harness.Batch{Next: cursorFor(cur, sig, fp, state)}, nil
	}

	query := fmt.Sprintf(
		"SELECT id, summary, updated_at, data_type, data, %s, %s, %s FROM threads ORDER BY updated_at ASC, id ASC",
		columnOr(cols, "parent_id", "''"),
		columnOr(cols, "folder_paths", "''"),
		columnOr(cols, "created_at", "''"),
	)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return harness.Batch{Next: cur}, nil
	}
	defer rows.Close()

	var batch harness.Batch
	for rows.Next() {
		var (
			id, summary, updatedAt, dataType sql.NullString
			parentID, folderPaths, createdAt sql.NullString
			data                             []byte
		)
		if err := rows.Scan(&id, &summary, &updatedAt, &dataType, &data, &parentID, &folderPaths, &createdAt); err != nil {
			continue
		}
		if !id.Valid || id.String == "" || len(data) == 0 {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(dataType.String))
		if kind != "zstd" && kind != "json" {
			continue
		}
		doc, err := decodeThread(data, kind)
		if err != nil {
			continue
		}

		threadID := id.String
		st := state.Threads[threadID]
		if st == nil {
			st = &threadState{}
			state.Threads[threadID] = st
		}

		// The thread's own creation time is the request timestamp. It is frozen on
		// first sight so a later row rewrite cannot move the timestamp of a request
		// that was already emitted; updated_at and finally the file mtime fill in
		// for older databases without the column.
		ts := firstTime(parseTime(createdAt.String), parseTime(updatedAt.String), sig.mod)
		if frozen := parseTime(st.Time); !frozen.IsZero() {
			ts = frozen
		} else if !ts.IsZero() {
			st.Time = ts.UTC().Format(time.RFC3339Nano)
		}
		ts = ts.UTC()

		projectPath := firstFolder(folderPaths.String)
		modelID := strings.TrimSpace(doc.Model.Model)
		if modelID == "" {
			modelID = "unknown"
		}

		known := make(map[string]bool, len(st.Keys))
		for _, k := range st.Keys {
			known[k] = true
		}
		var sum tokenUsage
		for _, u := range doc.RequestTokenUsage {
			sum = sum.add(u)
		}
		keys := make([]string, 0, len(doc.RequestTokenUsage))
		for k, u := range doc.RequestTokenUsage {
			if !u.empty() {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			hash := requestHash(k)
			if known[hash] {
				continue
			}
			known[hash] = true
			st.Keys = append(st.Keys, hash)
			batch.Events = append(batch.Events, model.UsageEvent{
				Harness:     model.Zed,
				DedupKey:    "zed:" + threadID + ":" + hash,
				SessionID:   threadID,
				ParentID:    strings.TrimSpace(parentID.String),
				ProjectPath: projectPath,
				Timestamp:   ts,
				Model:       modelID,
				Provider:    strings.TrimSpace(doc.Model.Provider),
				Tokens:      doc.RequestTokenUsage[k].tokens(),
			})
		}

		// Requests that the per-request map does not account for are reported once
		// as a top-up entry, keyed by a fixed suffix (upstream behaviour). A growing
		// thread re-emits it only when the remainder itself changes, so an unchanged
		// source stays quiet.
		if cum := doc.CumulativeTokenUsage; cum != nil && !cum.empty() {
			rem := cum.subtract(sum)
			if !rem.empty() {
				sig := rem.signature()
				if st.Remain != sig {
					st.Remain = sig
					batch.Events = append(batch.Events, model.UsageEvent{
						Harness:     model.Zed,
						DedupKey:    "zed:" + threadID + ":" + remainderKey,
						SessionID:   threadID,
						ParentID:    strings.TrimSpace(parentID.String),
						ProjectPath: projectPath,
						Timestamp:   ts,
						Model:       modelID,
						Provider:    strings.TrimSpace(doc.Model.Provider),
						Tokens:      rem.tokens(),
					})
				}
			}
		}

		started := firstTime(parseTime(createdAt.String), ts)
		updated := firstTime(parseTime(updatedAt.String), ts)
		batch.Sessions = append(batch.Sessions, model.SessionMeta{
			Harness:   model.Zed,
			SessionID: threadID,
			ParentID:  strings.TrimSpace(parentID.String),
			Title:     harness.Title(summary.String),
			Project:   projectName(projectPath),
			StartedAt: started.UTC(),
			UpdatedAt: updated.UTC(),
		})
	}
	batch.Next = cursorFor(cur, sig, fp, state)
	return batch, nil
}

// threadState is the persisted per-thread progress.
type threadState struct {
	Time   string   `json:"t,omitempty"` // frozen event timestamp (RFC 3339)
	Keys   []string `json:"k,omitempty"` // hashes of the requests already emitted
	Remain string   `json:"r,omitempty"` // last emitted cumulative remainder
}

type dbState struct {
	Threads map[string]*threadState `json:"threads,omitempty"`
}

func decodeState(extra string) *dbState {
	s := &dbState{Threads: map[string]*threadState{}}
	if extra == "" {
		return s
	}
	if err := json.Unmarshal([]byte(extra), s); err != nil || s.Threads == nil {
		return &dbState{Threads: map[string]*threadState{}}
	}
	return s
}

func cursorFor(cur harness.Cursor, sig signature, fp string, state *dbState) harness.Cursor {
	extra, err := json.Marshal(state)
	if err != nil {
		extra = nil
	}
	next := harness.Cursor{
		Offset:      cur.Offset,
		Size:        sig.size,
		ModTime:     sig.mod,
		Fingerprint: fp,
		Extra:       string(extra),
	}
	if len(state.Threads) == 0 {
		next.Extra = ""
	}
	return next
}

// threadDoc is the subset of a thread document this parser reads.
type threadDoc struct {
	Model struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	} `json:"model"`
	RequestTokenUsage    map[string]tokenUsage `json:"request_token_usage"`
	CumulativeTokenUsage *tokenUsage           `json:"cumulative_token_usage"`
}

// tokenUsage mirrors Zed's Anthropic-shaped counters.
type tokenUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

func (u tokenUsage) empty() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheCreationInputTokens == 0 && u.CacheReadInputTokens == 0
}

func (u tokenUsage) tokens() model.Tokens {
	return model.Tokens{
		Input:      u.InputTokens,
		Output:     u.OutputTokens,
		CacheRead:  u.CacheReadInputTokens,
		CacheWrite: u.CacheCreationInputTokens,
	}
}

func (u tokenUsage) add(o tokenUsage) tokenUsage {
	return tokenUsage{
		InputTokens:              u.InputTokens + o.InputTokens,
		OutputTokens:             u.OutputTokens + o.OutputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens + o.CacheCreationInputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens + o.CacheReadInputTokens,
	}
}

// subtract returns the part of u that the per-request entries did not cover.
func (u tokenUsage) subtract(o tokenUsage) tokenUsage {
	sub := func(a, b int64) int64 {
		if a-b < 0 {
			return 0
		}
		return a - b
	}
	return tokenUsage{
		InputTokens:              sub(u.InputTokens, o.InputTokens),
		OutputTokens:             sub(u.OutputTokens, o.OutputTokens),
		CacheCreationInputTokens: sub(u.CacheCreationInputTokens, o.CacheCreationInputTokens),
		CacheReadInputTokens:     sub(u.CacheReadInputTokens, o.CacheReadInputTokens),
	}
}

func (u tokenUsage) signature() string {
	return fmt.Sprintf("%d/%d/%d/%d", u.InputTokens, u.OutputTokens, u.CacheCreationInputTokens, u.CacheReadInputTokens)
}

// decodeThread decompresses and decodes one row's thread document.
func decodeThread(data []byte, kind string) (*threadDoc, error) {
	raw := data
	if kind == "zstd" {
		out, err := decodeZstd(data)
		if err != nil {
			return nil, err
		}
		raw = out
	}
	if len(raw) > maxThreadBytes {
		return nil, errors.New("zed: thread document too large")
	}
	var doc threadDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

var (
	decoderOnce sync.Once
	decoder     *zstd.Decoder
	decoderErr  error
)

func decodeZstd(data []byte) ([]byte, error) {
	decoderOnce.Do(func() {
		decoder, decoderErr = zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(maxThreadBytes),
		)
	})
	if decoderErr != nil {
		return nil, decoderErr
	}
	return decoder.DecodeAll(data, nil)
}

// requestHash shortens a request key to a stable, non-textual identifier; the
// keys Zed writes are user-message ids and must not reach a dedup key verbatim.
func requestHash(key string) string {
	sum := sha1.Sum([]byte(key))
	return hex.EncodeToString(sum[:8])
}

func projectName(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if base := filepath.Base(filepath.Clean(path)); base != "." && base != string(filepath.Separator) {
		return base
	}
	return ""
}

// firstFolder returns the first workspace folder of a folder_paths column.
func firstFolder(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var folders []string
	if err := json.Unmarshal([]byte(raw), &folders); err == nil {
		for _, f := range folders {
			if f = strings.TrimSpace(f); f != "" {
				return f
			}
		}
		return ""
	}
	return raw
}

func firstTime(times ...time.Time) time.Time {
	for _, t := range times {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

// parseTime accepts Zed's ISO-8601 timestamps plus the numeric forms seen in
// older rows (seconds or milliseconds since the epoch).
func parseTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05.999999999 -0700 MST", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t
		}
	}
	if n, err := parseInt(raw); err == nil {
		switch {
		case n > 1e15: // microseconds
			return time.UnixMicro(n).UTC()
		case n > 1e12: // milliseconds
			return time.UnixMilli(n).UTC()
		case n > 1e9: // seconds
			return time.Unix(n, 0).UTC()
		}
	}
	return time.Time{}
}

func parseInt(raw string) (int64, error) { return strconv.ParseInt(raw, 10, 64) }

type signature struct {
	size int64
	mod  time.Time
}

// signatureOf combines threads.db with its WAL sidecars, so a write that only
// touched the log still counts as a change.
func signatureOf(path string) (signature, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return signature{}, err
	}
	sig := signature{size: fi.Size(), mod: fi.ModTime()}
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

// openReadOnly opens path read-only; harness data is never written to.
func openReadOnly(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	q := url.Values{
		"mode":      {"ro"},
		"immutable": {"0"},
		"_pragma":   {fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS)},
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

// tableColumns lists the columns of a table, or nil when it does not exist.
func tableColumns(ctx context.Context, db *sql.DB, table string) map[string]bool {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notNull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			continue
		}
		cols[strings.ToLower(name)] = true
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	return cols
}

func columnOr(cols map[string]bool, name, fallback string) string {
	if cols[strings.ToLower(name)] {
		return name
	}
	return fallback + " AS " + name
}
