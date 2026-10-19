// Package sessionlog is the shared deep module behind MyToken's Pi-derived
// session-log parsers: internal/harness/pi reads Pi's logs and
// internal/harness/stepcode reads Step Code's, and Step Code's format is a fork
// of Pi's.
//
// What lives here is everything that is about *reading a growing JSONL session
// log* rather than about one product's records:
//
//   - the byte-offset cursor model: a leading-bytes fingerprint and a
//     (size, mtime, fingerprint) sameness test, plus a reader that only ever
//     consumes whole lines so a partially written record is left for the next
//     scan;
//   - discovery over a set of roots, with the errors a scan must survive;
//   - the JSON scalar encodings the logs actually use (numbers that arrive as
//     strings, a cost that is either a number or an object, ISO-8601 and
//     millisecond-epoch timestamps);
//   - the usage block and its mapping onto model.Tokens, and the ledger that
//     turns usage blocks into deduplicated model.UsageEvents.
//
// What stays with each harness is its *schema*: which record types exist, which
// of them account for a real model call, and how a forked log names its parent.
// Neither parser copies the other's record decoding.
package sessionlog

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

const (
	// FingerprintBytes is how many leading bytes of a log identify its content.
	// A log is append-only below that mark, so the leading bytes change exactly
	// when the file was rewritten or truncated.
	FingerprintBytes = 4096
	// ReadBuffer is the line reader's starting buffer size (it grows per line).
	ReadBuffer = 64 << 10
	// FenceLimit bounds the first line read when classifying a JSONL file.
	FenceLimit = 64 << 10
)

// ---------------------------------------------------------------------------
// cursor model
// ---------------------------------------------------------------------------

// Fingerprint returns the sha1 of the first FingerprintBytes bytes of path.
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

// Unchanged reports whether cur already describes the file exactly: same size,
// same modification time and same leading-bytes fingerprint. A cursor with no
// fingerprint is never "unchanged", because it was never produced by a parse.
func Unchanged(cur harness.Cursor, st fs.FileInfo, fp string) bool {
	return cur.Fingerprint != "" && cur.Fingerprint == fp &&
		cur.Size == st.Size() && cur.ModTime.Equal(st.ModTime())
}

// Rewind returns the offset a parse must start from: cur's offset when it still
// matches the file, and 0 when the log shrank, when the offset is past the end,
// or when the leading bytes changed (a rewrite or a truncate-and-refill).
func Rewind(cur harness.Cursor, size int64, fp string) int64 {
	if cur.Offset < 0 || cur.Offset > size || (cur.Fingerprint != "" && cur.Fingerprint != fp) {
		return 0
	}
	return cur.Offset
}

// ReadLines feeds every '\n'-terminated line of r to fn and returns the number
// of bytes consumed, which always ends on a line boundary. A trailing chunk
// without '\n' is never passed to fn and never counted, so a partially written
// record is left for the next scan.
func ReadLines(r io.Reader, fn func(line []byte) error) (int64, error) {
	br := bufio.NewReaderSize(r, ReadBuffer)
	var n int64
	for {
		line, err := br.ReadBytes('\n')
		if err == nil {
			if e := fn(line[:len(line)-1]); e != nil {
				return n, e
			}
			n += int64(len(line))
			continue
		}
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		return n, err
	}
}

// IsJSONL reports whether path has the session-log extension.
func IsJSONL(path string) bool { return strings.HasSuffix(path, ".jsonl") }

// Walk returns one Source per JSONL file under roots for which classify reports
// a kind, sorted by path so scans are deterministic. A missing or unreadable
// root is simply not discovered; only a context cancellation stops the walk.
func Walk(ctx context.Context, roots []string, classify func(path string) (kind string, ok bool)) ([]harness.Source, error) {
	var out []harness.Source
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		werr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// Unreadable entries (permissions, races with deletion) are
				// skipped; a missing root is simply not discovered.
				return nil
			}
			if d.IsDir() || !IsJSONL(path) {
				return nil
			}
			kind, ok := classify(path)
			if !ok {
				return nil
			}
			out = append(out, harness.Source{Path: path, Kind: kind})
			return nil
		})
		if werr != nil && !errors.Is(werr, fs.SkipAll) {
			return nil, werr
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// RootFor returns the configured root that contains path, preferring the
// longest match, or "" when no root does.
func RootFor(roots []string, path string) string {
	best := ""
	for _, r := range roots {
		if r == "" {
			continue
		}
		rel, err := filepath.Rel(r, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if len(r) > len(best) {
			best = r
		}
	}
	return best
}

// FirstLine returns the first line of path, without its newline. A file whose
// first line is longer than FenceLimit, and a file that cannot be opened, are
// both reported as not-a-first-line rather than as an error: classification is
// best effort by design.
func FirstLine(path string) ([]byte, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, FenceLimit)
	line, err := br.ReadSlice('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false // including bufio.ErrBufferFull: a header line is tiny
	}
	return bytes.TrimSpace(line), true
}

// envelope is the part of a record that is common to every Pi-derived schema.
type envelope struct {
	Type      string          `json:"type"`
	Version   int             `json:"version"`
	ID        string          `json:"id"`
	Timestamp json.RawMessage `json:"timestamp"`
}

// EnvelopeType returns a record's "type" field, or "" when the line is not a
// JSON object with one. It is the safety net that lets a parser still see a
// marker (a compaction, say) in a record it cannot fully decode.
func EnvelopeType(payload []byte) string {
	var e envelope
	if json.Unmarshal(payload, &e) != nil {
		return ""
	}
	return e.Type
}

// ---------------------------------------------------------------------------
// JSON scalars
// ---------------------------------------------------------------------------

// Num decodes a JSON number that may arrive as an integer, a float or a quoted
// string; null and absent values decode to 0.
type Num int64

func (n *Num) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err == nil {
		*n = Num(f)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return err
		}
		*n = Num(v)
		return nil
	}
	return fmt.Errorf("sessionlog: cannot decode %s as a number", b)
}

// Fnum decodes a JSON number without truncating it to an integer. It accepts
// the same shapes as Num.
type Fnum float64

func (f *Fnum) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	var v float64
	if err := json.Unmarshal(b, &v); err == nil {
		*f = Fnum(v)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		x, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return err
		}
		*f = Fnum(x)
		return nil
	}
	return fmt.Errorf("sessionlog: cannot decode %s as a number", b)
}

// Clamp narrows a decoded count to a non-negative int64.
func Clamp(n Num) int64 {
	if n < 0 {
		return 0
	}
	return int64(n)
}

// Millis converts a millisecond epoch into a UTC time. A non-positive value is
// the zero time, which callers read as "the record names no timestamp".
func Millis(ms Num) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(ms)).UTC()
}

// ParseStamp decodes a timestamp that arrives either as an RFC3339 string, as a
// string holding a millisecond epoch, or as a millisecond epoch number.
func ParseStamp(b json.RawMessage) time.Time {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return time.Time{}
	}
	var s string
	if json.Unmarshal(b, &s) == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return time.Time{}
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UTC()
		}
		if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
			return Millis(Num(ms))
		}
		return time.Time{}
	}
	var n Num
	if json.Unmarshal(b, &n) == nil {
		return Millis(n)
	}
	return time.Time{}
}

// FirstNonEmpty returns the first non-empty value.
func FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// message text
// ---------------------------------------------------------------------------

// Content is one content block of a message. Only the discriminator and the
// text survive decoding: image data and tool payloads are never kept.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// FirstText joins the text blocks of a message.
func FirstText(cs []Content) string {
	var b strings.Builder
	for _, c := range cs {
		if c.Type == "text" || c.Type == "" {
			b.WriteString(c.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

// SlashCommandRE matches "/name" or "/name args" but not a path such as
// "/Users/zzstar/Downloads/x.rar", whose next character is '/'.
var SlashCommandRE = regexp.MustCompile(`^/[A-Za-z][A-Za-z0-9_-]*(\s|$)`)

// TitleCandidate reports whether a user message can serve as the session title:
// plugin-injected system reminders and slash commands are skipped.
func TitleCandidate(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || strings.HasPrefix(t, "<system-reminder") || SlashCommandRE.MatchString(t) {
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// usage
// ---------------------------------------------------------------------------

// ReasoningMode tells Tokens how a schema's usage block relates reasoning to
// output. The two Pi-derived schemas disagree, and getting it wrong double
// counts or loses tokens.
type ReasoningMode bool

const (
	// ReasoningSeparate is Pi's reading: reasoning is its own class, reported
	// alongside output and not contained in it, so the five counts map onto
	// model.Tokens one-to-one.
	ReasoningSeparate ReasoningMode = false
	// ReasoningInOutput is Step Code's reading, spelled out by its Usage type:
	// "Reasoning/thinking tokens ... This is a subset of output: output already
	// includes these tokens." Its provider adapters follow through, taking
	// output straight from the API's completion_tokens, which already contains
	// reasoning_tokens. model.Tokens must not double count, so reasoning is
	// subtracted back out of Output.
	ReasoningInOutput ReasoningMode = true
)

// Cost is a usage block's cost breakdown. It decodes either shape the logs use:
// an object with the per-class fields, or the plain number an async record
// writes, which becomes Total.
type Cost struct {
	Input      Fnum `json:"input"`
	Output     Fnum `json:"output"`
	CacheRead  Fnum `json:"cacheRead"`
	CacheWrite Fnum `json:"cacheWrite"`
	Total      Fnum `json:"total"`
}

func (c *Cost) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] != '{' {
		return json.Unmarshal(b, &c.Total)
	}
	type plain Cost
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*c = Cost(p)
	return nil
}

// Usage is a provider usage block. See the package doc of each schema for which
// fields its product writes; the union is decoded so a schema never has to
// guess.
type Usage struct {
	Input        Num   `json:"input"`
	Output       Num   `json:"output"`
	CacheRead    Num   `json:"cacheRead"`
	CacheWrite   Num   `json:"cacheWrite"`
	TotalTokens  Num   `json:"totalTokens"`
	Reasoning    Num   `json:"reasoning"`
	CacheWrite1h Num   `json:"cacheWrite1h"` // Pi: a subset of cacheWrite, ignored
	Cost         *Cost `json:"cost"`
}

// IsZero reports whether the record accounts for no tokens at all. The real
// corpora contain usage objects whose fields are all zero (failed requests, for
// example HTTP 429 or 400 with a cost object attached), so the counted fields
// are compared rather than the struct value.
func (u *Usage) IsZero() bool {
	return u == nil || (u.Input == 0 && u.Output == 0 && u.CacheRead == 0 &&
		u.CacheWrite == 0 && u.Reasoning == 0)
}

// Tokens maps the usage block onto model.Tokens under the schema's reading of
// reasoning.
func (u *Usage) Tokens(mode ReasoningMode) model.Tokens {
	if u == nil {
		return model.Tokens{}
	}
	out, reasoning := Clamp(u.Output), Clamp(u.Reasoning)
	if mode == ReasoningInOutput {
		// The provider's output already contains reasoning. Subtract it back
		// out (never below zero: a log with reasoning but no output is
		// inconsistent, and dropping the reasoning count entirely would be
		// worse than reporting it as output).
		if reasoning > out {
			reasoning = out
		}
		out -= reasoning
	}
	return model.Tokens{
		Input:      Clamp(u.Input),
		Output:     out,
		CacheRead:  Clamp(u.CacheRead),
		CacheWrite: Clamp(u.CacheWrite),
		Reasoning:  reasoning,
	}
}

// CostUSD reports the log's own cost, or nil when it does not name one. A zero
// total is reported as absent rather than as 0.
func (u *Usage) CostUSD() *float64 {
	if u == nil || u.Cost == nil {
		return nil
	}
	t := float64(u.Cost.Total)
	if t == 0 {
		return nil
	}
	return &t
}

// ---------------------------------------------------------------------------
// event ledger
// ---------------------------------------------------------------------------

// Events accumulates the usage events of one session log. It owns the harness
// stamp, the dedup namespace and the boundary state, so two Pi-derived
// harnesses can never collide with each other and a log that repeats a record
// (a fork that copies its parent's entries, a rewrite that is re-parsed) can
// never count one model call twice.
type Events struct {
	harness  model.Harness
	prefix   string
	boundary harness.BoundaryState
	seen     map[string]bool
	list     []model.UsageEvent
}

// NewEvents returns a ledger for one session of harness h. dedupPrefix
// namespaces every key; pass "" only for a harness that already ships unprefixed
// keys.
func NewEvents(h model.Harness, dedupPrefix string) *Events {
	return &Events{harness: h, prefix: dedupPrefix}
}

// Add counts one model call. key identifies the provider request (a response id,
// a record id, or a synthesized "<session>#<timestamp>"); it is namespaced and
// used for both the dedup key and the boundary marker, so the marker a
// compaction leaves is always claimed by the first request that follows it. An
// event whose key was already counted is dropped.
func (e *Events) Add(ev model.UsageEvent, key string) {
	key = e.prefix + key
	if e.seen[key] {
		return
	}
	if e.seen == nil {
		e.seen = make(map[string]bool)
	}
	e.seen[key] = true
	ev.Harness = e.harness
	ev.DedupKey = key
	ev.Boundary = e.boundary.Apply(key)
	e.list = append(e.list, ev)
}

// MarkPending records a boundary marker that belongs to the next counted
// request.
func (e *Events) MarkPending(m model.Boundary) { e.boundary.Pending = m }

// Fill applies f to every counted event. It exists so a session can backfill the
// identity fields that the first records of a truncated log do not carry yet.
func (e *Events) Fill(f func(*model.UsageEvent)) {
	for i := range e.list {
		f(&e.list[i])
	}
}

// Len returns the number of counted events.
func (e *Events) Len() int { return len(e.list) }

// List returns the counted events. The slice is owned by the ledger.
func (e *Events) List() []model.UsageEvent { return e.list }

// RestoreBoundary restores the boundary state a previous parse stored in a
// cursor's Extra field. A malformed or absent payload leaves the ledger
// untouched.
func (e *Events) RestoreBoundary(extra string) {
	if extra == "" {
		return
	}
	_ = json.Unmarshal([]byte(extra), &e.boundary)
}

// BoundaryJSON returns the boundary state to store in a cursor's Extra field.
func (e *Events) BoundaryJSON() string {
	raw, _ := json.Marshal(e.boundary)
	return string(raw)
}
