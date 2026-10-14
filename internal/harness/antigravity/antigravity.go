// Package antigravity parses the Antigravity app and IDE agent conversations.
//
// Roots: $MYTOKEN_ANTIGRAVITY_DIRS when set, else the per-surface conversation
// directories under $GEMINI_CLI_HOME (default ~/.gemini):
//
//	~/.gemini/antigravity/conversations       standalone app
//	~/.gemini/antigravity-cli/conversations   CLI
//	~/.gemini/antigravity-cli/implicit
//	~/.gemini/antigravity-ide/conversations   IDE
//	~/.gemini/antigravity-ide/implicit
//
// Only the SQLite cascades (`*.db`) are read. Newer builds also write `*.pb`
// cascade files, whose contents are only decodable through the language server's
// RPC, so they are not discovered; a machine that only has `.pb` files reports no
// Antigravity usage.
//
// One row of gen_metadata carries the protobuf of a committed generation: the
// model configuration and exa.codeium_common_pb.ModelUsageStats. The usage is
// normalized the way the model reports it — input excludes cache hits, cache
// writes and reads are separate classes, output is response text, and thinking is
// billed separately — so Output and Reasoning never double count. Message and
// tool text is never read (only token counts, ids and the workspace URL), and the
// response id is only used as a RequestID when the log itself carries one.
package antigravity

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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/sqlitedsn"
)

const (
	kindSQLite       = "sqlite"
	fingerprintBytes = 4096
	busyTimeoutMS    = 5000
)

// Parser implements harness.Parser for Antigravity.
type Parser struct {
	roots []string
	// prov recomputes this parser's default-root provenance from the current
	// environment. It is set only on the registry singleton; a parser built for
	// explicit roots reports those roots instead.
	prov func() ([]harness.Root, error)
}

// New returns a parser for the standard Antigravity locations.
func New() *Parser { return &Parser{roots: DefaultRoots(), prov: rootProvenance} }

// NewWithRoots returns a parser reading the given conversation roots (tests).
func NewWithRoots(roots ...string) *Parser {
	return &Parser{roots: append([]string(nil), roots...)}
}

func init() { harness.Register(New()) }

// rootProvenance reports Antigravity's default roots and where each came from.
// MYTOKEN_ANTIGRAVITY_DIRS replaces the whole set; GEMINI_CLI_HOME is an
// explicit harness-specific base; otherwise the base is the platform default.
func rootProvenance() ([]harness.Root, error) {
	if v, ok := harness.Getenv("MYTOKEN_ANTIGRAVITY_DIRS"); ok {
		dirs, err := harness.SplitEnvList("MYTOKEN_ANTIGRAVITY_DIRS", v)
		if err != nil {
			return nil, err
		}
		return harness.EnvRoots("MYTOKEN_ANTIGRAVITY_DIRS", dirs...), nil
	}
	if base, ok := harness.Getenv("GEMINI_CLI_HOME"); ok {
		return harness.EnvRoots("GEMINI_CLI_HOME", conversationDirs(base)...), nil
	}
	return harness.BuiltinRoots(conversationDirs(filepath.Join(harness.Home(), ".gemini"))...), nil
}

// conversationDirs returns the five conversation directories under base.
func conversationDirs(base string) []string {
	return []string{
		filepath.Join(base, "antigravity", "conversations"),
		filepath.Join(base, "antigravity-cli", "conversations"),
		filepath.Join(base, "antigravity-cli", "implicit"),
		filepath.Join(base, "antigravity-ide", "conversations"),
		filepath.Join(base, "antigravity-ide", "implicit"),
	}
}

// DefaultRoots returns the Antigravity conversation directories for this machine.
func DefaultRoots() []string { return defaultRoots() }

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

// Harness identifies this parser's logs.
func (p *Parser) Harness() model.Harness { return model.Antigravity }

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

// Discover lists the cascade databases under every root.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	seen := map[string]bool{}
	var out []harness.Source
	for _, root := range p.roots {
		if root == "" {
			continue
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.EqualFold(filepath.Ext(name), ".db") {
				continue
			}
			path := filepath.Join(root, name)
			if seen[path] {
				continue
			}
			seen[path] = true
			out = append(out, harness.Source{Path: path, Kind: kindSQLite})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Parse reads one cascade database from cur to the current end. gen_metadata only
// grows while a cascade is committed, so the row index is the watermark.
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
	cascadeID := strings.TrimSuffix(filepath.Base(src.Path), filepath.Ext(src.Path))
	label := projectLabel(filepath.Dir(src.Path))

	rows, err := loadGenerations(ctx, db, state.Idx)
	if err != nil {
		// No gen_metadata table: not an Antigravity cascade database.
		return harness.Batch{Next: cursorFor(cur, sig, fp, state)}, nil
	}
	steps := loadSteps(ctx, db)
	workspace := readWorkspace(ctx, db)
	project, eventPath := label, ""
	if label != projectCLI && workspace != "" {
		project, eventPath = projectName(workspace), workspace
	}

	var batch harness.Batch
	var started, updated time.Time
	seen := map[string]bool{}
	for _, row := range rows {
		fields := parseProtoFields(row.data)
		chat := parseProtoFields(protoFieldBytes(firstProtoField(fields, 1)))
		usage := parseProtoFields(protoFieldBytes(firstProtoField(chat, 4)))
		if len(usage) == 0 {
			continue
		}

		input := protoFieldPositiveInteger(firstProtoField(usage, 2))
		total := protoFieldPositiveInteger(firstProtoField(usage, 3))
		cacheWrite := protoFieldPositiveInteger(firstProtoField(usage, 4))
		cacheRead := protoFieldPositiveInteger(firstProtoField(usage, 5))
		thinking := protoFieldPositiveInteger(firstProtoField(usage, 9))
		response := protoFieldPositiveInteger(firstProtoField(usage, 10))
		switch {
		case response == 0 && thinking == 0:
			response = total
		case total > 0 && response+thinking != total:
			if adjusted := total - thinking; adjusted >= 0 {
				response = adjusted
			}
		}
		if input == 0 && total == 0 {
			continue // nothing was spent (in-flight or aborted generation)
		}

		responseID, fromLog := protoFieldText(firstProtoField(usage, 11)), true
		if responseID == "" || strings.ContainsAny(responseID, " \t\r\n") {
			responseID, fromLog = strconv.FormatInt(row.idx, 10), false
		}
		key := "antigravity:" + cascadeID + ":" + responseID
		if seen[key] {
			continue
		}
		seen[key] = true

		ts := createdAt(chat)
		if ts.IsZero() {
			ts = firstStepTime(fields, steps)
		}
		if ts.IsZero() {
			ts = sig.mod
		}
		ts = ts.UTC()
		if started.IsZero() || ts.Before(started) {
			started = ts
		}
		if ts.After(updated) {
			updated = ts
		}

		ev := model.UsageEvent{
			Harness:     model.Antigravity,
			DedupKey:    key,
			SessionID:   cascadeID,
			ProjectPath: eventPath,
			Timestamp:   ts,
			Model:       resolveModel(chat),
			Tokens: model.Tokens{
				Input:      input,
				Output:     response,
				CacheRead:  cacheRead,
				CacheWrite: cacheWrite,
				Reasoning:  thinking,
			},
		}
		if fromLog {
			ev.RequestID = responseID
		}
		batch.Events = append(batch.Events, ev)
		if row.idx > state.Idx {
			state.Idx = row.idx
		}
	}

	// The cascade database carries no user-visible title, so only the session
	// identity, the workspace and the time span it spent are published — and only
	// once the cascade actually spent something.
	if len(batch.Events) == 0 {
		batch.Next = cursorFor(cur, sig, fp, state)
		return batch, nil
	}
	batch.Sessions = append(batch.Sessions, model.SessionMeta{
		Harness:   model.Antigravity,
		SessionID: cascadeID,
		Project:   project,
		StartedAt: started.UTC(),
		UpdatedAt: updated.UTC(),
	})
	batch.Next = cursorFor(cur, sig, fp, state)
	return batch, nil
}

const projectCLI = "antigravity-cli"

// projectLabel names the surface a root belongs to.
func projectLabel(dir string) string {
	lower := strings.ToLower(filepath.ToSlash(dir))
	switch {
	case strings.Contains(lower, "antigravity-cli"):
		return projectCLI
	case strings.Contains(lower, "antigravity-ide"):
		return "antigravity-ide"
	default:
		return "antigravity"
	}
}

func projectName(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	base := filepath.Base(filepath.Clean(path))
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	return base
}

type generation struct {
	idx  int64
	data []byte
}

func loadGenerations(ctx context.Context, db *sql.DB, after int64) ([]generation, error) {
	rows, err := db.QueryContext(ctx, "SELECT idx, data FROM gen_metadata WHERE idx > ? ORDER BY idx", after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []generation
	for rows.Next() {
		var (
			idx  int64
			data []byte
		)
		if err := rows.Scan(&idx, &data); err != nil {
			continue
		}
		out = append(out, generation{idx: idx, data: data})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// loadSteps reads the step metadata blobs keyed by index; a missing table only
// costs the created_at fallback.
func loadSteps(ctx context.Context, db *sql.DB) map[int64][]byte {
	rows, err := db.QueryContext(ctx, "SELECT idx, metadata FROM steps ORDER BY idx")
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[int64][]byte{}
	for rows.Next() {
		var (
			idx  int64
			data []byte
		)
		if err := rows.Scan(&idx, &data); err != nil {
			continue
		}
		out[idx] = data
	}
	return out
}

// firstStepTime resolves the generation's first referenced step and reads its
// Timestamp (steps.metadata #1); the standalone app leaves created_at out.
func firstStepTime(fields []protoField, steps map[int64][]byte) time.Time {
	var indices []int64
	for _, f := range fields {
		if f.number == 2 && f.bytes != nil {
			indices = append(indices, decodePackedVarints(f.bytes)...)
		}
	}
	if len(indices) == 0 {
		return time.Time{}
	}
	meta, ok := steps[indices[0]]
	if !ok {
		return time.Time{}
	}
	return protoTimestamp(firstProtoField(parseProtoFields(meta), 1))
}

// createdAt reads ChatStartMetadata (#9) . created_at (#4).
func createdAt(chat []protoField) time.Time {
	meta := protoFieldBytes(firstProtoField(chat, 9))
	if meta == nil {
		return time.Time{}
	}
	return protoTimestamp(firstProtoField(parseProtoFields(meta), 4))
}

var fileURLPattern = regexp.MustCompile(`(?i)file:///[^\x00-\x1F\x7F"'\s]+`)

// readWorkspace extracts the workspace URL from the trajectory metadata blob.
func readWorkspace(ctx context.Context, db *sql.DB) string {
	rows, err := db.QueryContext(ctx, "SELECT data FROM trajectory_metadata_blob")
	if err != nil {
		return ""
	}
	defer rows.Close()
	var buf []byte
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			continue
		}
		buf = append(buf, b...)
	}
	match := fileURLPattern.Find(buf)
	if match == nil {
		return ""
	}
	raw := string(match)
	if u, err := url.Parse(raw); err == nil && u.Path != "" {
		path := u.Path
		if len(path) > 2 && path[0] == '/' && path[2] == ':' { // file:///C:/x on Windows
			path = path[1:]
		}
		return path
	}
	return strings.TrimPrefix(raw, "file://")
}

// resolveModel ports codeburn's resolution order: a placeholder enum resolves to
// the catalog id it stood for unless the row also carries a display name, and a
// real configuration id is canonicalized through the display name.
func resolveModel(chat []protoField) string {
	attrs := metadataAttributes(chat)
	displayName := protoFieldText(firstProtoField(chat, 21))
	if placeholder := placeholderModels[attrs["model_enum"]]; displayName == "" && placeholder != "" {
		return placeholder
	}
	raw := protoFieldText(firstProtoField(chat, 19))
	if raw == "" {
		raw = attrs["model_enum"]
	}
	if raw == "" {
		raw = displayName
	}
	if raw == "" {
		return "unknown"
	}
	return canonicalModel(raw, displayName)
}

// metadataAttributes collects the key/value pairs in ChatModel #20.
func metadataAttributes(chat []protoField) map[string]string {
	attrs := map[string]string{}
	for _, f := range chat {
		if f.number != 20 || f.bytes == nil {
			continue
		}
		pair := parseProtoFields(f.bytes)
		key := protoFieldText(firstProtoField(pair, 1))
		value := protoFieldText(firstProtoField(pair, 2))
		if key != "" && value != "" {
			attrs[key] = value
		}
	}
	return attrs
}

// placeholderModels maps the placeholder enums the standalone app writes to the
// catalog ids they resolved to; a later app build can repoint one.
var placeholderModels = map[string]string{
	"MODEL_PLACEHOLDER_M16": "gemini-3.1-pro-high",
	"MODEL_PLACEHOLDER_M37": "gemini-3.1-pro-high",
	"MODEL_PLACEHOLDER_M36": "gemini-3.1-pro-low",
	"MODEL_PLACEHOLDER_M84": "gemini-3.5-flash-high",
	"MODEL_PLACEHOLDER_M18": "gemini-3-flash",
	"MODEL_PLACEHOLDER_M35": "claude-sonnet-4-6",
	"MODEL_PLACEHOLDER_M26": "claude-opus-4-6-thinking",
}

// canonicalModel prefers the human display name over an internal configuration
// id, and never lets a raw placeholder reach the store.
func canonicalModel(key, displayName string) string {
	if displayName != "" {
		lower := strings.ToLower(displayName)
		switch {
		case strings.Contains(lower, "3.5 flash"):
			switch {
			case strings.Contains(lower, "high"):
				return "gemini-3.5-flash-high"
			case strings.Contains(lower, "medium"):
				return "gemini-3.5-flash-medium"
			case strings.Contains(lower, "low"):
				return "gemini-3.5-flash-low"
			}
			return "gemini-3.5-flash"
		case strings.Contains(lower, "3.1 pro"):
			switch {
			case strings.Contains(lower, "high"):
				return "gemini-3.1-pro-high"
			case strings.Contains(lower, "low"):
				return "gemini-3.1-pro-low"
			}
			return "gemini-3.1-pro"
		case strings.Contains(lower, "3.1 flash"):
			switch {
			case strings.Contains(lower, "image"):
				return "gemini-3.1-flash-image"
			case strings.Contains(lower, "lite"):
				return "gemini-3.1-flash-lite"
			}
			return "gemini-3.1-flash"
		case strings.Contains(lower, "3 flash"):
			return "gemini-3-flash"
		case strings.Contains(lower, "3 pro"):
			return "gemini-3-pro"
		}
	}
	if strings.HasPrefix(key, "MODEL_PLACEHOLDER_") {
		return "unknown"
	}
	return key
}

// protoField is one decoded protobuf field.
type protoField struct {
	number int
	wire   int
	value  uint64
	bytes  []byte
}

// parseProtoFields walks a protobuf message. Unknown wire types end the walk, as
// in codeburn: the stream is no longer trustworthy past one.
func parseProtoFields(data []byte) []protoField {
	var fields []protoField
	offset := 0
	for offset < len(data) {
		key, next, ok := readProtoVarint(data, offset)
		if !ok {
			break
		}
		offset = next
		number := int(key >> 3)
		wire := int(key & 7)
		if number <= 0 {
			break
		}
		switch wire {
		case 0:
			value, after, ok := readProtoVarint(data, offset)
			if !ok {
				return fields
			}
			fields = append(fields, protoField{number: number, wire: wire, value: value})
			offset = after
		case 1:
			if offset+8 > len(data) {
				return fields
			}
			fields = append(fields, protoField{number: number, wire: wire, bytes: data[offset : offset+8]})
			offset += 8
		case 2:
			length, after, ok := readProtoVarint(data, offset)
			if !ok || length > uint64(len(data)) {
				return fields
			}
			offset = after
			if offset+int(length) > len(data) {
				return fields
			}
			fields = append(fields, protoField{number: number, wire: wire, bytes: data[offset : offset+int(length)]})
			offset += int(length)
		case 5:
			if offset+4 > len(data) {
				return fields
			}
			fields = append(fields, protoField{number: number, wire: wire, bytes: data[offset : offset+4]})
			offset += 4
		default:
			return fields
		}
	}
	return fields
}

func readProtoVarint(data []byte, offset int) (uint64, int, bool) {
	var value uint64
	var shift uint
	for offset < len(data) {
		b := data[offset]
		offset++
		if shift < 64 {
			value |= uint64(b&0x7f) << shift
		}
		if b&0x80 == 0 {
			return value, offset, true
		}
		shift += 7
		if shift > 70 {
			return 0, offset, false
		}
	}
	return 0, offset, false
}

func decodePackedVarints(data []byte) []int64 {
	var out []int64
	offset := 0
	for offset < len(data) {
		value, next, ok := readProtoVarint(data, offset)
		if !ok {
			break
		}
		out = append(out, int64(value))
		offset = next
	}
	return out
}

func firstProtoField(fields []protoField, number int) *protoField {
	for i := range fields {
		if fields[i].number == number {
			return &fields[i]
		}
	}
	return nil
}

func protoFieldBytes(f *protoField) []byte {
	if f == nil {
		return nil
	}
	return f.bytes
}

// protoFieldText decodes a length-delimited field as UTF-8 text, rejecting
// payloads that are binary rather than text.
func protoFieldText(f *protoField) string {
	if f == nil || len(f.bytes) == 0 {
		return ""
	}
	text := string(f.bytes)
	if !isProtoText(text) {
		return ""
	}
	return text
}

func isProtoText(s string) bool {
	for _, r := range s {
		switch {
		case r == 0xFFFD:
			return false
		case r <= 0x08:
			return false
		case r >= 0x0E && r <= 0x1F:
			return false
		case r == 0x7F:
			return false
		}
	}
	return s != ""
}

func protoFieldPositiveInteger(f *protoField) int64 {
	if f == nil || f.wire != 0 {
		return 0
	}
	if f.value == 0 || f.value > uint64(1)<<62 {
		return 0
	}
	return int64(f.value)
}

// protoTimestamp decodes a time field: an ISO-8601 string, a
// google.protobuf.Timestamp submessage (#1 seconds, #2 nanos) or a bare epoch
// varint in seconds or milliseconds.
func protoTimestamp(f *protoField) time.Time {
	if f == nil {
		return time.Time{}
	}
	if text := protoFieldText(f); text != "" {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
			if t, err := time.Parse(layout, text); err == nil {
				return t.UTC()
			}
		}
		return time.Time{}
	}
	if f.bytes != nil {
		fields := parseProtoFields(f.bytes)
		seconds := firstProtoField(fields, 1)
		if seconds != nil && seconds.wire == 0 && seconds.value > 0 && seconds.value < 1<<40 {
			var nanos uint64
			if n := firstProtoField(fields, 2); n != nil && n.wire == 0 && n.value < 1e9 {
				nanos = n.value
			}
			return time.UnixMilli(int64(seconds.value)*1000 + int64(nanos/1e6)).UTC()
		}
		return time.Time{}
	}
	if f.wire == 0 && f.value > 0 && f.value < 1<<53 { // JS-safe integer range
		if f.value < 1e12 { // seconds until the year 33658; anything longer is milliseconds
			return time.Unix(int64(f.value), 0).UTC()
		}
		return time.UnixMilli(int64(f.value)).UTC()
	}
	return time.Time{}
}

type dbState struct {
	Idx int64 `json:"i,omitempty"`
}

func decodeState(extra string) *dbState {
	s := &dbState{}
	if extra == "" {
		return s
	}
	if err := json.Unmarshal([]byte(extra), s); err != nil {
		return &dbState{}
	}
	return s
}

func cursorFor(cur harness.Cursor, sig signature, fp string, state *dbState) harness.Cursor {
	extra, err := json.Marshal(state)
	if err != nil || state.Idx == 0 {
		extra = nil
	}
	return harness.Cursor{
		Offset:      cur.Offset,
		Size:        sig.size,
		ModTime:     sig.mod,
		Fingerprint: fp,
		Extra:       string(extra),
	}
}

type signature struct {
	size int64
	mod  time.Time
}

// signatureOf combines the database with its WAL sidecars, so a write that only
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
