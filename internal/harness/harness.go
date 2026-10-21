// Package harness defines the parser contract (docs/ARCHITECTURE.md#parser-contract) and a registry.
package harness

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zzstar101/mytoken/internal/model"
)

// Source is one discovered log file or database.
type Source struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // "jsonl", "json", "jsonl.zstd", "sqlite"
}

// Cursor is the incremental read state of a Source, persisted by the store.
type Cursor struct {
	Offset      int64     `json:"offset"` // bytes consumed (end of last complete line)
	Size        int64     `json:"size"`
	ModTime     time.Time `json:"modTime"`
	Fingerprint string    `json:"fingerprint"` // sha1 of the first 4KB; change => reread from 0
	Extra       string    `json:"extra,omitempty"`
}

// BoundaryState persists a lifecycle marker across incremental parser batches.
// The marker belongs to the next distinct usage request, not a replay of the
// preceding request. Consecutive streaming copies retain the assigned marker.
type BoundaryState struct {
	Pending      model.Boundary `json:"pending,omitempty"`
	LastKey      string         `json:"lastKey,omitempty"`
	LastBoundary model.Boundary `json:"lastBoundary,omitempty"`
}

func (s *BoundaryState) Apply(key string) model.Boundary {
	if key != "" && key == s.LastKey {
		return s.LastBoundary
	}
	boundary := s.Pending
	s.Pending = ""
	s.LastKey = key
	s.LastBoundary = boundary
	return boundary
}

// Batch is what one Parse call produces.
type Batch struct {
	Events   []model.UsageEvent
	Sessions []model.SessionMeta
	Next     Cursor
}

// Parser reads one harness's local logs. Implementations must be safe to call
// from a single scan goroutine (no concurrent calls on the same Source).
type Parser interface {
	Harness() model.Harness
	// Roots returns directories to watch (env vars and ~ already expanded).
	// Non-existent roots are allowed and simply ignored.
	Roots() []string
	Discover(ctx context.Context) ([]Source, error)
	// Parse reads src incrementally from cur to the current end. Parsers that
	// cannot read incrementally (whole-file JSON, zstd) reparse fully and rely on
	// DedupKey; they still must return an accurate Next.
	Parse(ctx context.Context, src Source, cur Cursor) (Batch, error)
}

// Versioned is an optional capability of a registered parser: ParserVersion
// identifies the parse semantics of the events the parser produces. It must
// increase whenever a change alters the events or sessions a given source
// yields — a token split, a deduplication rule, a cost field — because scanning
// is incremental and a database written by an older parser otherwise keeps its
// rows forever. The scanner compares the version with the one recorded for the
// harness and re-ingests a mismatch once (see store.ResetHarness).
//
// A parser that does not implement Versioned reports InitialParserVersion.
type Versioned interface {
	ParserVersion() int
}

// SourceSessions is an optional capability of a Versioned parser: it reports
// every session id the events parsed from src can carry. The scanner consults
// it when a parser generation changes, so a re-ingest deletes only the rows the
// source still on disk will replace. Without it a reset could not tell a
// current source's rows from the rows of a log the harness rotated away, and
// would have to choose between leaving stale rows and destroying history whose
// only copy is the database.
//
// Implementations must report at least the session ids of the events Parse
// emits for src — including ids that only appear in the file body (inline
// sidechains, subagent transcripts) and ids a fresh parse would now drop (a
// ledger record deduplicated against a transcript). They may report extra ids;
// an extra id can also delete rows of a *different* source that shares that
// session, so a source's own sessions are the safe report and the whole
// harness is not.
//
// A parser that raises ParserVersion must implement SourceSessions: the
// scanner refuses a destructive reset it cannot scope.
type SourceSessions interface {
	SourceSessions(ctx context.Context, src Source) ([]string, error)
}

// InitialParserVersion is the generation of every parser that predates version
// tags, and of parsers whose output has never changed since.
const InitialParserVersion = 1

// ParserVersion reports the parse generation of p. A non-positive report is
// treated as InitialParserVersion so a malformed implementation cannot make the
// scanner re-ingest on every scan.
func ParserVersion(p Parser) int {
	if v, ok := p.(Versioned); ok {
		if n := v.ParserVersion(); n > 0 {
			return n
		}
	}
	return InitialParserVersion
}

var (
	mu       sync.RWMutex
	registry = map[model.Harness]Parser{}
)

// Register adds a parser; call from init() of each harness package.
func Register(p Parser) {
	mu.Lock()
	defer mu.Unlock()
	registry[p.Harness()] = p
}

// All returns registered parsers sorted by harness id.
func All() []Parser {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Parser, 0, len(registry))
	for _, p := range registry {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Harness() < out[j].Harness() })
	return out
}

// Get returns the parser for h.
func Get(h model.Harness) (Parser, bool) {
	mu.RLock()
	defer mu.RUnlock()
	p, ok := registry[h]
	return p, ok
}

// Rooted is an optional capability of a registered parser: it builds a fresh
// parser for an explicit root list, leaving the singleton that Register
// installed untouched.
//
// WithRoots must return a parser that owns a defensive copy of roots and shares
// no mutable state with the receiver or with any parser it returned earlier.
// An explicit empty list means "no roots": it must stay empty, never fall back
// to the harness's default roots. Implementations must not read credentials,
// consult the environment, or touch the filesystem while constructing.
type Rooted interface {
	WithRoots(roots ...string) Parser
}

// Keyed is an optional capability of a registered parser whose sources are
// encrypted with a secret the user supplies once: a database key, a password, a
// passphrase. The app reads that secret from its own settings and hands it to
// the parser, so a harness never stores credentials itself and the secret never
// has to live in a file the tool reads.
//
// WithKey must return a parser that shares no mutable state with the receiver,
// and must not touch the filesystem or the environment while constructing. An
// empty key means "no key": the returned parser reads nothing and explains why
// through NotReady. A key the parser cannot use is returned as an error, so the
// settings page can reject it instead of leaving a harness silently unreadable.
type Keyed interface {
	WithKey(key string) (Parser, error)
}

// DatabaseLister is an optional capability of a parser whose source files can
// be named without being readable — a locked database still has a path. The key
// capture needs that path to prove a candidate key against, before any key is
// available to open it.
type DatabaseLister interface {
	DatabaseFiles(ctx context.Context) ([]string, error)
}

// NotReady is an optional capability of a parser that is installed but cannot
// read its sources yet — an encrypted database with no key, an expired
// credential. NotReady returns a one-line English reason for the sources page,
// or "" when the parser is ready. Detect reports it as Status.Problem on a
// harness it detected, so the page can ask for what is missing before a scan
// produces nothing.
type NotReady interface {
	NotReady() string
}

// NewRooted builds a fresh parser for h over exactly roots. It reports false
// when h is not registered or when its registered parser does not implement
// Rooted; callers must treat that as a hard error rather than falling back to
// default roots. With no roots the returned parser has no roots at all.
//
// The registry lock guards only the map lookup; WithRoots runs after the lock is
// released, so constructing a parser can never block Register/All/Get.
func NewRooted(h model.Harness, roots ...string) (Parser, bool) {
	mu.RLock()
	p, ok := registry[h]
	mu.RUnlock()
	if !ok {
		return nil, false
	}
	r, ok := p.(Rooted)
	if !ok {
		return nil, false
	}
	return r.WithRoots(roots...), true
}

// RootOrigin classifies where a default watch root came from.
type RootOrigin string

const (
	// OriginExplicit marks a root the caller supplied to WithRoots/NewRooted.
	OriginExplicit RootOrigin = "explicit"
	// OriginEnv marks a root derived from an explicit environment override: a
	// MYTOKEN_*_DIRS list or a harness-specific directory variable such as
	// CODEX_HOME. Such a root is a user request, so a missing or unusable path
	// is an error rather than a silent skip.
	OriginEnv RootOrigin = "env"
	// OriginBuiltin marks a root derived from a built-in or generic platform
	// default (HOME, XDG_*, APPDATA, a fixed directory name). A missing root
	// here is not an error: it is simply absent on this machine.
	OriginBuiltin RootOrigin = "builtin"
)

// Root is one default watch directory together with its provenance.
type Root struct {
	Path   string     `json:"path"`
	Origin RootOrigin `json:"origin"`
	// Env names the environment variable that produced Path when Origin is
	// OriginEnv; it is empty otherwise.
	Env string `json:"env,omitempty"`
}

// BuiltinRoots tags every path as a built-in default.
func BuiltinRoots(paths ...string) []Root {
	out := make([]Root, 0, len(paths))
	for _, p := range paths {
		out = append(out, Root{Path: p, Origin: OriginBuiltin})
	}
	return out
}

// EnvRoots tags every path as coming from the named environment variable.
func EnvRoots(env string, paths ...string) []Root {
	out := make([]Root, 0, len(paths))
	for _, p := range paths {
		out = append(out, Root{Path: p, Origin: OriginEnv, Env: env})
	}
	return out
}

// ExplicitRoots tags every path as caller-supplied.
func ExplicitRoots(paths ...string) []Root {
	out := make([]Root, 0, len(paths))
	for _, p := range paths {
		out = append(out, Root{Path: p, Origin: OriginExplicit})
	}
	return out
}

// RootPaths returns the paths of roots, in order.
func RootPaths(roots []Root) []string {
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		out = append(out, r.Path)
	}
	return out
}

// Getenv returns the whitespace-trimmed value of key and reports whether it is
// non-blank. A blank value (unset, or only whitespace) counts as absent so a
// whitespace-only override never shadows the built-in default, while a
// malformed non-blank list is preserved and can be rejected by SplitEnvList.
func Getenv(key string) (string, bool) {
	v := strings.TrimSpace(os.Getenv(key))
	return v, v != ""
}

// SplitEnvList parses a non-blank list-valued environment override. It splits
// on the platform list separator, trims every entry, drops the empty ones and
// cleans what remains. When nothing usable is left it returns *EnvListError
// rather than an empty list, so a malformed explicit request is never silently
// replaced by built-in defaults.
func SplitEnvList(env, value string) ([]string, error) {
	var out []string
	for _, part := range filepath.SplitList(value) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, filepath.Clean(part))
		}
	}
	if len(out) == 0 {
		return nil, &EnvListError{Env: env, Value: value}
	}
	return out, nil
}

// EnvListError reports a non-blank environment override that names no usable
// directory.
type EnvListError struct {
	Env   string
	Value string
}

func (e *EnvListError) Error() string {
	return "environment variable " + e.Env + " is set to " + strconv.Quote(e.Value) +
		" but names no directory"
}

// Provenance is an optional capability of a registered parser: it reports the
// roots the parser would watch by default, each tagged with where it came from.
//
// RootProvenance must answer from the environment as it is at call time. The
// registry singleton is built during package init, so an answer captured at
// construction time would be stale and a rescan would miss an override set
// after startup. It must not read credentials.
type Provenance interface {
	RootProvenance() ([]Root, error)
}

// RegisteredProvenance reports the default roots and their provenance for the
// registered parser of h. ok is false when h is not registered or when its
// registered parser does not implement Provenance. err reports a non-blank
// explicit override that names no usable directory; callers must treat that as
// a hard error and must not substitute built-in defaults.
func RegisteredProvenance(h model.Harness) (roots []Root, ok bool, err error) {
	p, found := Get(h)
	if !found {
		return nil, false, nil
	}
	pv, found := p.(Provenance)
	if !found {
		return nil, false, nil
	}
	roots, err = pv.RootProvenance()
	return roots, true, err
}

// Home returns the user's home directory ("" if unknown).
func Home() string {
	h, _ := os.UserHomeDir()
	return h
}

// EnvOr returns $key if set, else filepath.Join(Home(), rel...).
func EnvOr(key string, rel ...string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return filepath.Join(append([]string{Home()}, rel...)...)
}

// Title normalizes a first user message into a session title: whitespace
// collapsed, at most 60 runes.
func Title(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= 60 {
		return s
	}
	r := []rune(s)
	return string(r[:60])
}
