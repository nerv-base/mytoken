// Package pi parses Pi coding-agent session logs.
//
// # Log layout
//
//	~/.pi/agent/sessions/<cwd-slug>/<timestamp>_<session-id>.jsonl      (top level)
//	~/.pi/agent/sessions/<cwd-slug>/<timestamp>_<session-id>/forks/<timestamp>_<session-id>.jsonl
//	~/.pi/agent/sessions/<cwd-slug>/<timestamp>_<session-id>/<child-id>/run-N/session.jsonl
//
// Plain, uncompressed JSONL, so it is resumed by byte offset.
//
// # Two log shapes
//
// Session logs (the vast majority) start with a session header; anything else is
// ignored by Discover. The exception is the async-subagent transcript of a
// background "reviewer" run, which is stored next to its siblings as
//
//	<slug>/subagent-artifacts/<run-id>_<agent>_transcript.jsonl
//
// and is a JSONL log of its own: its records are
// {"version":1,"recordType":"message","source":"async","runId":"<uuid>",
// "agent":"reviewer","cwd":"/path","ts":<ms>,"timestamp":"<RFC3339>",
// "sourceEventType":"initial_prompt"|"message_end"|"tool_start"|"tool_end",
// "role":..., "model"?, "text"?, "usage"?} — i.e. the usage object sits at the
// TOP level of the record instead of under "message". Those 91 assistant
// message_end records of the reference corpus appear in no session log at all
// (their (input, output, ts) tuples match nothing), so they are counted here;
// skipping them would undercount ~10.5M tokens. The sibling
// <run-id>_<agent>_meta.json is only read for the provider named by its "model"
// field (e.g. "rn" out of "rn/gpt-6.1-sol:high"); its aggregate "usage" is
// deliberately NOT counted (it would double count the transcript) and its
// "task" prompt is never read.
//
// # Record schema
//
// Line 1 is the session header:
//
//	{"type":"session","version":3,"id":"<uuidv7>","timestamp":"<RFC3339>",
//	 "cwd":"/path","parentSession":"/abs/path/to/parent.jsonl"?}
//
// Every later line is {"type":"...","id":"<hex>","parentId":"<id|null>",
// "timestamp":"<RFC3339>", ...}. Record types seen in the wild: message,
// model_change, thinking_level_change, session_info, custom_message,
// context_edit, compaction and custom.
//
// Only one shape carries usage: a record of type "message" whose
// message.role == "assistant" and which has message.usage:
//
//	message.usage = {input, output, cacheRead, cacheWrite, totalTokens,
//	                 cost:{input,output,cacheRead,cacheWrite,total},
//	                 reasoning?, cacheWrite1h?}
//	message.provider, message.model (or message.responseModel),
//	message.responseId ("resp_<hex>"), message.api, message.timestamp (ms epoch)
//
// A compaction record marks the point where the conversation was replaced by a
// summary, so the next usage-bearing message in the same session is the first
// request whose prompt is the compacted context and is marked
// model.BoundaryCompact. In the wild such a record also carries the compaction
// call's own top-level usage (with "cost" as an object); the record names no
// model, so that usage is not accounted, only the boundary is taken.
//
// # Token accounting
//
// Verified over all 14257 usage records of the reference corpus:
// totalTokens == input + output + cacheRead + cacheWrite for every record, and
// adding reasoning breaks that identity for the 9769 records with
// reasoning > 0. So input already excludes cacheRead and output already
// excludes reasoning; no normalization is needed and none is applied:
//
//	Input      = input        (excludes cacheRead/cacheWrite)
//	Output     = output       (excludes reasoning)
//	CacheRead  = cacheRead
//	CacheWrite = cacheWrite
//	Reasoning  = reasoning    (separate, additive)
//
// usage.cacheWrite1h is a subset of cacheWrite that totalTokens already counts
// (and that is always 0 in the reference corpus), so it is deliberately ignored
// to avoid double counting. CostUSD is usage.cost.total when it is non-zero.
//
// # Deduplication
//
// Pi rewrites a message every time a streamed response grows, so the same
// message appears up to five times (15895 assistant records vs 14391 distinct
// ids). DedupKey is message.responseId when present, else the record id, else
// "<session>#<timestamp>"; the last copy wins downstream.
//
// # Sessions
//
// Top-level sessions have no parentSession. A fork header carries
// parentSession as the parent log's absolute path, so ParentID is the session
// id parsed out of that file name. Subagent runs have no parentSession at all;
// their parent is the session directory they live under, so ParentID is parsed
// from the ancestor directory name "<timestamp>_<session-id>". An async-run
// transcript has no recorded parent either (its meta file names none), so its
// ParentID stays empty. The title is the first real user message: role ==
// "user" with non-empty text, skipping plugin-injected "<system-reminder>"
// blocks and slash commands such as "/compact" (a dragged-in path like
// "/Users/…" is not a command and is kept). For an async run the title comes
// from the initial_prompt record. Provider and model are only set when the log
// names them; no model→provider inference is done.
package pi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/sessionlog"
	"github.com/zzstar101/mytoken/internal/model"
)

const (
	kindJSONL = "jsonl"

	// The byte-offset cursor model, the first-line fence and the JSON scalar
	// encodings live in internal/harness/sessionlog, because Step Code — a Pi
	// fork with the same log layout — needs all three and must not copy this
	// parser. What stays here is Pi's own record schema.
	fingerprintBytes = sessionlog.FingerprintBytes
	readBuffer       = sessionlog.ReadBuffer
	headerLimit      = sessionlog.FenceLimit
)

// parserVersion 2 parses cacheWrite1h from the shared sessionlog usage block
// into Tokens.CacheWrite1h. A database ingested by an earlier release holds the
// cache write without the 1-hour split, so it re-reads this harness once.
const parserVersion = 2

// Parser reads Pi session logs. It holds no mutable state and is safe for
// concurrent use.
type Parser struct {
	roots []string
	// prov recomputes this parser's default-root provenance from the current
	// environment. It is set only on the registry singleton; a parser built for
	// explicit roots reports those roots instead.
	prov func() ([]harness.Root, error)
}

// New returns the parser for the default roots: $PI_HOME/sessions when PI_HOME
// is set, else ~/.pi/agent/sessions together with the sessions directory of an
// Oh My Pi install (see ompAgentDir) and of an OMO/senpi install (see
// omoSenpiAgentDir), each when one is configured.
func New() *Parser {
	p := NewWithRoots()
	p.prov = rootProvenance
	return p
}

// NewWithRoot returns a parser rooted at root. It exists for tests and for
// tools that keep logs outside the home directory.
func NewWithRoot(root string) *Parser { return NewWithRoots(root) }

// NewWithRoots returns a parser over the given roots. An empty list falls back
// to the default root.
func NewWithRoots(roots ...string) *Parser {
	cp := make([]string, 0, len(roots))
	for _, r := range roots {
		if r != "" {
			cp = append(cp, r)
		}
	}
	if len(cp) == 0 {
		cp = defaultRoots()
	}
	return &Parser{roots: cp}
}

// rootProvenance reports Pi's default roots and where they came from. Besides
// Pi's own root it reports the sessions directory of an Oh My Pi (OMP) install
// and of an OMO/senpi (Oh My OpenAgent) install, because both are Pi forks that
// write the very same session logs and have no parser of their own.
func rootProvenance() ([]harness.Root, error) {
	if v, ok := harness.Getenv(piDirsEnv); ok {
		dirs, err := harness.SplitEnvList(piDirsEnv, v)
		if err != nil {
			return nil, err
		}
		return harness.EnvRoots(piDirsEnv, dirs...), nil
	}
	// PI_HOME is this tool's own explicit base for a Pi install. Like the other
	// parsers' *_HOME overrides it replaces the whole default set, so an explicit
	// Pi base does not drag an unrelated OMP or OMO install in.
	if home, ok := harness.Getenv("PI_HOME"); ok {
		return harness.EnvRoots("PI_HOME", filepath.Join(home, "sessions")), nil
	}

	home := harness.Home()
	roots := harness.BuiltinRoots(filepath.Join(home, ".pi", "agent", "sessions"))
	if agent, env := ompAgentDir(home); agent != "" {
		roots = appendAgentSessions(roots, agent, env)
	}
	if agent, env := omoSenpiAgentDir(home); agent != "" {
		roots = appendAgentSessions(roots, agent, env)
	}
	return dedupeRoots(roots), nil
}

// appendAgentSessions appends the sessions directory of one agent directory,
// tagged env-origin when an environment variable determined the agent directory
// and built-in otherwise.
func appendAgentSessions(roots []harness.Root, agent, env string) []harness.Root {
	dir := filepath.Join(agent, "sessions")
	if env != "" {
		return append(roots, harness.EnvRoots(env, dir)...)
	}
	return append(roots, harness.BuiltinRoots(dir)...)
}

// piDirsEnv is the MyToken-side override that replaces the whole root set.
const piDirsEnv = "MYTOKEN_PI_DIRS"

// OMP agent-directory variables. OMP's resolver (packages/utils/src/dirs.ts)
// reads exactly these: PI_CONFIG_DIR names the config directory (".omp"), the
// two profile variables select a named profile, and PI_CODING_AGENT_DIR
// overrides the agent directory of the default profile.
const (
	ompConfigDirEnv = "PI_CONFIG_DIR"
	ompAgentDirEnv  = "PI_CODING_AGENT_DIR"

	ompConfigDirName = ".omp"
	ompDataDirName   = "omp"
	ompProfilesDir   = "profiles"
	ompAgentDirName  = "agent"
)

// OMO/senpi agent-home variables and layouts. Oh My OpenAgent (OMO) brands the
// senpi engine, and both share one resolver
// (packages/omo-senpi/src/components/agent-home/resolve-agent-home.ts): the
// three agent-directory variables in the order below name the agent home
// directly, and only when none is set does the resolver fall back to detection.
const (
	omoAgentDirEnv   = "OMO_CODING_AGENT_DIR"
	senpiAgentDirEnv = "SENPI_CODING_AGENT_DIR"

	// agentHomeSentinel is the marker proving a directory really holds engine
	// state rather than sharing its name.
	agentHomeSentinel = "settings.json"
	omoDirName        = ".omo"
	senpiDirName      = ".senpi"
)

// omoSenpiAgentDir reports the agent directory of the run-time OMO/senpi install
// and the environment variable that determined it ("" for a detected or
// fallback home, and "" for the directory itself when OMO should not be watched
// at all).
//
// Resolution follows the official resolver exactly:
//
//	env        $OMO_CODING_AGENT_DIR, then $SENPI_CODING_AGENT_DIR, then
//	           $PI_CODING_AGENT_DIR — trimmed, and resolved against the working
//	           directory when relative
//	canonical  ~/.omo/agent   when ~/.omo/agent/settings.json exists
//	legacy     ~/.omo         when ~/.omo/settings.json exists (pre-unification
//	                          releases wrote the state flat)
//	fallback   ~/.senpi/agent (standalone engine)
//
// A blank value counts as unset, exactly like harness.Getenv, so a
// whitespace-only variable never collapses the home onto the working directory.
// Only the sentinel turns ~/.omo into an engine home: a directory that merely
// carries the name is not state. Detection is existence-based, not
// directory-based, so a home whose sessions directory does not exist yet is
// still reported — the engine creates it lazily.
func omoSenpiAgentDir(home string) (dir, env string) {
	// The environment always wins over detection. PI_CODING_AGENT_DIR is shared
	// with the OMP tier, so an OMO install configured through it resolves to the
	// same home and dedupes onto the OMP root instead of adding a second one.
	for _, name := range []string{omoAgentDirEnv, senpiAgentDirEnv, ompAgentDirEnv} {
		if v, ok := harness.Getenv(name); ok {
			return resolveDir(v), name
		}
	}
	branded := filepath.Join(home, omoDirName)
	canonical := filepath.Join(branded, ompAgentDirName)
	if pathExists(filepath.Join(canonical, agentHomeSentinel)) {
		return canonical, ""
	}
	if pathExists(filepath.Join(branded, agentHomeSentinel)) {
		return branded, ""
	}
	return filepath.Join(home, senpiDirName, ompAgentDirName), ""
}

// resolveDir mirrors node:path.resolve for an environment-supplied directory: a
// relative value is anchored at the working directory and the result is
// cleaned. A value filepath.Abs cannot resolve is returned cleaned rather than
// dropped, so a usable user request is never lost.
func resolveDir(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return filepath.Clean(path)
}

// pathExists reports whether path exists at all (file or directory), matching
// the official resolver's existsSync gate on the settings.json sentinel.
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ompAgentDir reports the agent directory of the run-time OMP install and the
// environment variable that determined it ("" for a built-in default, and ""
// for the directory itself when OMP should not be watched at all).
//
// Resolution follows OMP's own order:
//
//	profile        ~/.omp/profiles/<name>/agent        (OMP_PROFILE, then
//	                                                    PI_PROFILE)
//	agent override $PI_CODING_AGENT_DIR                (default profile only)
//	XDG data       $XDG_DATA_HOME/omp                  (linux/darwin, default
//	                                                    profile, only when that
//	                                                    directory exists; the
//	                                                    agent/ level is
//	                                                    flattened away)
//	built-in       ~/.omp/agent
//
// A blank or unparsable profile name counts as the default profile rather than
// an error: an invalid environment variable must not stop a scan of Pi's own
// logs. Because the profile name becomes a path segment it is validated first
// (see validProfileName), so it can never escape the profiles directory.
func ompAgentDir(home string) (dir, env string) {
	configDir := ompConfigDirName
	// A blank value counts as unset, exactly like harness.Getenv: an empty
	// PI_CONFIG_DIR names no directory and must not collapse the config root
	// onto HOME.
	if v, ok := harness.Getenv(ompConfigDirEnv); ok {
		configDir = v
	}
	configRoot := filepath.Join(home, configDir)

	if profile := activeProfile(); profile != "" {
		return filepath.Join(configRoot, ompProfilesDir, profile, ompAgentDirName), profileEnv()
	}
	// PI_CODING_AGENT_DIR outranks the XDG redirect: OMP drops the override once
	// a profile is active and applies it before the XDG choice
	// (agentDirOverride ? agentDirOverride : defaultAgent), so a custom agent
	// directory is never silently replaced by the migrated XDG location.
	if agent, ok := harness.Getenv(ompAgentDirEnv); ok {
		return agent, ompAgentDirEnv
	}
	if xdgSupported() {
		if data, ok := harness.Getenv("XDG_DATA_HOME"); ok && isDir(filepath.Join(data, ompDataDirName)) {
			// XDG flattens the agent/ prefix: ~/.omp/agent/sessions becomes
			// $XDG_DATA_HOME/omp/sessions. A named profile never reaches this
			// branch, matching OMP, which keys the XDG choice on the
			// profile-specific path only.
			return filepath.Join(data, ompDataDirName), ""
		}
	}
	return filepath.Join(configRoot, ompAgentDirName), ""
}

// xdgSupported reports whether OMP honours the XDG environment at all: XDG is a
// Linux convention that OMP also applies on darwin, and on no other platform.
func xdgSupported() bool {
	return runtime.GOOS == "linux" || runtime.GOOS == "darwin"
}

// activeProfile returns the valid named profile from the environment, or "" for
// the default profile. OMP_PROFILE is canonical and takes precedence over the
// legacy PI_PROFILE; a blank one falls through to PI_PROFILE because the
// resolver treats them as a single OMP_PROFILE || PI_PROFILE expression. A
// blank, "default" or invalid name selects the default profile.
func activeProfile() string {
	// Truthiness, not set-ness: resolveProfileEnv reads OMP_PROFILE || PI_PROFILE,
	// so a blank OMP_PROFILE must fall through to PI_PROFILE.
	if v, ok := harness.Getenv("OMP_PROFILE"); ok {
		return profileName(v)
	}
	if v, ok := harness.Getenv("PI_PROFILE"); ok {
		return profileName(v)
	}
	return ""
}

// profileName normalizes a raw profile variable to a profile directory name: an
// empty or "default" value selects the default profile (""), and an invalid name
// is ignored rather than turned into a path segment.
func profileName(raw string) string {
	if raw == "default" || !validProfileName(raw) {
		return ""
	}
	return raw
}

// profileEnv names the variable that selected the active profile.
func profileEnv() string {
	if _, ok := harness.Getenv("OMP_PROFILE"); ok {
		return "OMP_PROFILE"
	}
	return "PI_PROFILE"
}

// profileNameRE mirrors OMP's PROFILE_NAME_RE.
var profileNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// windowsReservedRE mirrors OMP's reserved Windows device names, which cannot be
// a profile directory on Windows.
var windowsReservedRE = regexp.MustCompile(`(?i)^(?:CON|PRN|AUX|NUL|COM[0-9]|LPT[0-9])(?:\..*)?$`)

// validProfileName reports whether name is a profile OMP itself would accept.
// The rules are OMP's: a bounded character set, no "." or "..", no trailing dot,
// and no Windows device name.
func validProfileName(name string) bool {
	return name != "." && name != ".." && !strings.HasSuffix(name, ".") &&
		profileNameRE.MatchString(name) && !windowsReservedRE.MatchString(name)
}

// isDir reports whether path is an existing directory. Any stat failure counts
// as absent, so an unreadable or dangling path never redirects a root.
func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// dedupeRoots drops later roots that name a path an earlier root already
// reported, keeping the first occurrence's provenance and the input order.
func dedupeRoots(roots []harness.Root) []harness.Root {
	seen := make(map[string]bool, len(roots))
	out := roots[:0]
	for _, r := range roots {
		if r.Path == "" || seen[r.Path] {
			continue
		}
		seen[r.Path] = true
		out = append(out, r)
	}
	return out
}

// defaultRoots returns the paths of rootProvenance.
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

func (p *Parser) Harness() model.Harness { return model.Pi }

// ParserVersion implements harness.Versioned.
func (p *Parser) ParserVersion() int { return parserVersion }

// SourceSessions implements harness.SourceSessions. A Pi log's session id comes
// from its header, from an async transcript's run id, or from the path fallback
// newSession uses when no header has been written yet. All of them are reported:
// an async transcript's run id cannot be derived from its name, so a fallback
// alone would miss the rows that matter. The whole file is read because an async
// record can switch the session to its run id anywhere in the log, not only in
// the first line.
func (p *Parser) SourceSessions(ctx context.Context, src harness.Source) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	add(newSession(sessionlog.RootFor(p.roots, src.Path), src.Path).id)
	f, err := os.Open(src.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 {
			var r piRecord
			if json.Unmarshal(bytes.TrimSpace(line), &r) == nil {
				if r.Type == "session" {
					add(r.ID)
				}
				if r.RecordType == "message" && r.Source == "async" {
					add(r.RunID)
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	return out, nil
}

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

// Roots returns the directories to watch.
func (p *Parser) Roots() []string { return append([]string(nil), p.roots...) }

var _ harness.Parser = (*Parser)(nil)
var _ harness.SourceSessions = (*Parser)(nil)

func init() { harness.Register(New()) }

// Discover walks every root and returns one Source per session log, sorted by
// path so scans are deterministic.
func (p *Parser) Discover(ctx context.Context) ([]harness.Source, error) {
	return sessionlog.Walk(ctx, p.roots, piLogKind)
}

// piLogKind classifies a JSONL file by its first line: a Pi session log, an
// async-subagent transcript, or something else entirely. This is what keeps
// artifact mirrors and unrelated JSONL files out of the scan.
func piLogKind(path string) (string, bool) {
	line, ok := sessionlog.FirstLine(path)
	if !ok {
		return "", false // a header line is tiny; an unreadable file is not a log
	}
	var r piRecord
	if json.Unmarshal(line, &r) != nil {
		return "", false
	}
	switch {
	case r.Type == "session":
		return kindJSONL, true
	case r.RecordType == "message" && r.Source == "async":
		return kindJSONL, true
	}
	return "", false
}

// Parse reads one session log incrementally from cur.Offset. A trailing
// incomplete line is never consumed and never counted.
func (p *Parser) Parse(ctx context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if err := ctx.Err(); err != nil {
		return harness.Batch{}, err
	}
	st, err := os.Stat(src.Path)
	if err != nil {
		return harness.Batch{}, err
	}
	fp, err := sessionlog.Fingerprint(src.Path)
	if err != nil {
		return harness.Batch{}, err
	}
	if sessionlog.Unchanged(cur, st, fp) {
		return harness.Batch{Next: cur}, nil
	}
	// A shrunken file, an offset past EOF or a different fingerprint (the file
	// was rewritten/truncated) means the log no longer matches the cursor.
	off := sessionlog.Rewind(cur, st.Size(), fp)
	f, err := os.Open(src.Path)
	if err != nil {
		return harness.Batch{}, err
	}
	defer f.Close()
	if off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return harness.Batch{}, err
		}
	}
	s := newSession(sessionlog.RootFor(p.roots, src.Path), src.Path)
	if off > 0 {
		_ = json.Unmarshal([]byte(cur.Extra), &s.boundary)
	}
	if strings.Contains(filepath.Base(src.Path), "_transcript.jsonl") {
		s.provider = asyncMetaProvider(src.Path)
	}
	n, rerr := sessionlog.ReadLines(f, s.consume)
	b := s.batch()
	boundaryRaw, _ := json.Marshal(s.boundary)
	b.Next = harness.Cursor{
		Offset:      off + n,
		Size:        st.Size(),
		ModTime:     st.ModTime(),
		Fingerprint: fp,
		Extra:       string(boundaryRaw),
	}
	if rerr != nil {
		// Keep whatever complete lines were read and retry from there.
		return b, nil
	}
	return b, nil
}

// ---------------------------------------------------------------------------
// session accumulation (one file == one session)
// ---------------------------------------------------------------------------

// session accumulates the events and metadata of a single session log.
type session struct {
	id           string
	parentID     string
	project      string
	provider     string // async transcripts: read from the run's meta file
	userTitle    string
	startedAt    time.Time
	updatedAt    time.Time
	events       []model.UsageEvent
	boundary     harness.BoundaryState
	lastModel    string // last counted request's model; compaction records name none
	lastProvider string // ... and none either, so both are inherited
	modelInherit []int  // compaction events still waiting for a model from a later request
}

func newSession(root, path string) *session {
	// Fallbacks for a log whose header line has not been written yet: the file
	// name carries the id for top-level and fork logs, the relative directory
	// does for run logs, and an async transcript is named after its run id.
	id := idFromSessionPath(path)
	if id == "" {
		if base := filepath.Base(path); strings.HasSuffix(base, "_transcript.jsonl") {
			if rid := strings.TrimSuffix(base, "_transcript.jsonl"); looksLikeSessionID(rid) {
				id = rid
			}
		}
	}
	if id == "" {
		rel := filepath.Base(filepath.Dir(path))
		if r, err := filepath.Rel(root, filepath.Dir(path)); err == nil &&
			r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			rel = r
		}
		id = rel
	}
	s := &session{id: id}
	// Run logs and (rarely) fork logs carry no parentSession; their parent is
	// the session directory they live under.
	base := filepath.Base(path)
	if base == "session.jsonl" || filepath.Base(filepath.Dir(path)) == "forks" {
		s.parentID = parentFromPath(root, path)
	}
	return s
}

func (s *session) consume(line []byte) error {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil
	}
	var r piRecord
	if err := json.Unmarshal(line, &r); err != nil {
		// A record we cannot fully model must not hide a marker we need: if the
		// envelope says it is a compaction, still record the boundary and skip the
		// rest. (A compaction record's own usage decodes normally since
		// piAsyncUsage accepts both cost shapes; this is a safety net for a shape
		// we have not seen.)
		if sessionlog.EnvelopeType(line) == "compaction" {
			s.boundary.Pending = model.BoundaryCompact
		}
		return nil // partial or foreign record: skip
	}
	if t := sessionlog.ParseStamp(r.Timestamp); !t.IsZero() && t.After(s.updatedAt) {
		s.updatedAt = t
	}
	// Async subagent runs use a record shape of their own: no "type", a
	// "recordType"/"sourceEventType" pair, and a top-level usage object.
	if r.RecordType == "message" && r.Source == "async" {
		s.consumeAsync(r)
		return nil
	}
	switch r.Type {
	case "compaction":
		s.consumeCompaction(r)
	case "session":
		if r.ID != "" {
			s.id = r.ID
		}
		if r.Cwd != "" {
			s.project = r.Cwd
		}
		if ps := r.ParentSession; ps != "" {
			// A fork header points at the parent log's path, not its id.
			if id := idFromSessionPath(ps); id != "" {
				s.parentID = id
			}
		}
		if t := sessionlog.ParseStamp(r.Timestamp); !t.IsZero() && (s.startedAt.IsZero() || t.Before(s.startedAt)) {
			s.startedAt = t
		}

	case "message":
		m := r.Message
		if m == nil {
			break
		}
		if mt := sessionlog.Millis(m.Timestamp); !mt.IsZero() && mt.After(s.updatedAt) {
			s.updatedAt = mt
		}
		switch {
		case strings.EqualFold(m.Role, "assistant") && m.Usage != nil:
			if m.Usage.IsZero() {
				break
			}
			ts := sessionlog.Millis(m.Timestamp)
			if ts.IsZero() {
				ts = sessionlog.ParseStamp(r.Timestamp)
			}
			if ts.IsZero() {
				ts = s.startedAt
			}
			mdl := m.Model
			if mdl == "" {
				mdl = m.ResponseModel
			}
			if mdl != "" {
				s.lastModel = mdl
			}
			if m.Provider != "" {
				s.lastProvider = m.Provider
			}
			s.events = append(s.events, model.UsageEvent{
				Harness:     model.Pi,
				DedupKey:    sessionlog.FirstNonEmpty(m.ResponseID, r.ID, s.id+"#"+string(r.Timestamp)),
				RequestID:   m.ResponseID,
				Boundary:    s.boundary.Apply(sessionlog.FirstNonEmpty(m.ResponseID, r.ID, s.id+"#"+string(r.Timestamp))),
				SessionID:   s.id,
				ParentID:    s.parentID,
				ProjectPath: s.project,
				Timestamp:   ts,
				Model:       mdl,
				Provider:    m.Provider,
				Tokens:      m.Usage.Tokens(sessionlog.ReasoningSeparate),
				CostUSD:     m.Usage.CostUSD(),
			})
		case strings.EqualFold(m.Role, "user") && s.userTitle == "":
			if txt := firstText(m.Content); isTitleCandidate(txt) {
				s.userTitle = txt
			}
		}
	}
	return nil
}

// consumeCompaction folds one "compaction" record into the session. The record
// is the compaction request itself and carries the same top-level usage object
// as an async transcript (including a real cost), so it is counted as a request
// like any other. It names neither a model nor a provider: both are inherited
// from the nearest request in the session, the one before it when there is one
// (see the assistant branch) and otherwise the next one (filled in by batch).
// The marker it carries belongs to the first request after it, so the event
// itself is boundary-free.
func (s *session) consumeCompaction(r piRecord) {
	s.boundary.Pending = model.BoundaryCompact
	u := r.AsyncUsage
	if u == nil || u.IsZero() {
		return
	}
	ts := sessionlog.ParseStamp(r.Timestamp)
	if ts.IsZero() {
		ts = s.updatedAt
	}
	s.events = append(s.events, model.UsageEvent{
		Harness:     model.Pi,
		DedupKey:    sessionlog.FirstNonEmpty(r.ID, s.id+"#"+string(r.Timestamp)),
		SessionID:   s.id,
		ParentID:    s.parentID,
		ProjectPath: s.project,
		Timestamp:   ts,
		Model:       s.lastModel,
		Provider:    s.lastProvider,
		Tokens:      u.Tokens(sessionlog.ReasoningSeparate),
		CostUSD:     u.CostUSD(),
	})
	if s.lastModel == "" || s.lastProvider == "" {
		s.modelInherit = append(s.modelInherit, len(s.events)-1)
	}
}

// consumeAsync folds one record of an async subagent transcript into the
// session. The run is identified by runId, the cwd is on every record, and only
// "message_end" records with an assistant role carry usage.
func (s *session) consumeAsync(r piRecord) {
	if r.RunID != "" {
		s.id = r.RunID
	}
	if r.Cwd != "" {
		s.project = r.Cwd
	}
	ts := sessionlog.ParseStamp(r.Timestamp)
	if !ts.IsZero() && (s.startedAt.IsZero() || ts.Before(s.startedAt)) {
		s.startedAt = ts
	}
	switch r.SourceEvent {
	case "initial_prompt":
		if s.userTitle != "" {
			return
		}
		var content []piContent
		if r.Message != nil {
			content = r.Message.Content
		}
		txt := sessionlog.FirstNonEmpty(r.Text, firstText(content))
		if isTitleCandidate(txt) {
			s.userTitle = txt
		}
	case "message_end":
		if !strings.EqualFold(r.Role, "assistant") || r.AsyncUsage.IsZero() {
			return
		}
		if ts.IsZero() {
			ts = s.startedAt
		}
		if r.Model != "" {
			s.lastModel = r.Model
		}
		s.events = append(s.events, model.UsageEvent{
			Harness:     model.Pi,
			DedupKey:    s.id + "#" + strings.Trim(string(bytes.TrimSpace(r.Timestamp)), `"`),
			SessionID:   s.id,
			ParentID:    s.parentID,
			ProjectPath: s.project,
			Timestamp:   ts,
			Model:       r.Model,
			Provider:    s.provider,
			Tokens:      r.AsyncUsage.Tokens(sessionlog.ReasoningSeparate),
			CostUSD:     r.AsyncUsage.CostUSD(),
		})
	}
}

// batch finalizes the accumulated state into a Batch. Session fields are
// backfilled onto every event because the header is normally line 1 but a
// truncated log may not contain it yet.
func (s *session) batch() harness.Batch {
	for i := range s.events {
		if s.events[i].SessionID == "" {
			s.events[i].SessionID = s.id
		}
		if s.events[i].ParentID == "" {
			s.events[i].ParentID = s.parentID
		}
		if s.events[i].ProjectPath == "" {
			s.events[i].ProjectPath = s.project
		}
	}
	// A compaction record names no model or provider. The requests before it were
	// already used when the event was emitted, so what is left is the next request
	// in the log; a session that holds no other request keeps "unknown".
	for _, i := range s.modelInherit {
		for j := i + 1; j < len(s.events); j++ {
			if s.events[i].Model == "" && s.events[j].Model != "" {
				s.events[i].Model = s.events[j].Model
			}
			if s.events[i].Provider == "" && s.events[j].Provider != "" {
				s.events[i].Provider = s.events[j].Provider
			}
			if s.events[i].Model != "" && s.events[i].Provider != "" {
				break
			}
		}
		if s.events[i].Model == "" {
			s.events[i].Model = "unknown"
		}
	}
	updated := s.updatedAt
	if updated.IsZero() {
		updated = s.startedAt
	}
	return harness.Batch{
		Events: s.events,
		Sessions: []model.SessionMeta{{
			Harness:   model.Pi,
			SessionID: s.id,
			ParentID:  s.parentID,
			Title:     harness.Title(s.userTitle),
			Project:   s.project,
			StartedAt: s.startedAt,
			UpdatedAt: updated,
		}},
	}
}

// parentFromPath returns the session id embedded in an ancestor directory named
// "<timestamp>_<session-id>": Pi stores a session's runs under
// <session-dir>/<child-id>/run-N/ and its forks under <session-dir>/forks/,
// where <session-dir> is named after the session's log file.
func parentFromPath(root, path string) string {
	dir := filepath.Dir(path)
	if root != "" {
		rel, err := filepath.Rel(root, dir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "" // outside the configured root: no reliable ancestry
		}
		parts := strings.Split(rel, string(filepath.Separator))
		for i := len(parts) - 1; i >= 0; i-- {
			if id := idFromDirName(parts[i]); id != "" {
				return id
			}
		}
		return ""
	}
	for i := 0; i < 6 && dir != "" && dir != "." && dir != string(filepath.Separator); i++ {
		if id := idFromDirName(filepath.Base(dir)); id != "" {
			return id
		}
		dir = filepath.Dir(dir)
	}
	return ""
}

// idFromDirName extracts a session id from a "<timestamp>_<id>" directory name.
func idFromDirName(name string) string {
	i := strings.LastIndex(name, "_")
	if i < 0 || i+1 >= len(name) {
		return ""
	}
	if id := name[i+1:]; looksLikeSessionID(id) {
		return id
	}
	return ""
}

// idFromSessionPath extracts the session id from a session log path such as
// ".../2026-09-17T13-55-57-058Z_01a0afa7-1201-7289-8afe-c0ad477ee17b.jsonl".
func idFromSessionPath(p string) string {
	base := strings.TrimSuffix(filepath.Base(strings.TrimSpace(p)), ".jsonl")
	i := strings.LastIndex(base, "_")
	if i < 0 || i+1 >= len(base) {
		return ""
	}
	if id := base[i+1:]; looksLikeSessionID(id) {
		return id
	}
	return ""
}

// looksLikeSessionID matches the UUIDv7-shaped ids Pi writes (8-4-4-4-12 hex).
func looksLikeSessionID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// record schema
// ---------------------------------------------------------------------------

type piRecord struct {
	Type          string          `json:"type"`
	Version       int             `json:"version"`
	ID            string          `json:"id"`
	ParentID      string          `json:"parentId"`
	Cwd           string          `json:"cwd"`
	ParentSession string          `json:"parentSession"`
	Timestamp     json.RawMessage `json:"timestamp"`
	Message       *piMessage      `json:"message"`

	// Async-subagent transcripts ("source":"async") carry their usage object
	// at the top level instead of under "message".
	RecordType  string        `json:"recordType"`
	Source      string        `json:"source"`
	SourceEvent string        `json:"sourceEventType"`
	Role        string        `json:"role"`
	RunID       string        `json:"runId"`
	Model       string        `json:"model"`
	Text        string        `json:"text"`
	AsyncUsage  *piAsyncUsage `json:"usage"`
}

type piMessage struct {
	Role          string      `json:"role"`
	Provider      string      `json:"provider"`
	Model         string      `json:"model"`
	ResponseModel string      `json:"responseModel"`
	ResponseID    string      `json:"responseId"`
	API           string      `json:"api"`
	Timestamp     num         `json:"timestamp"`
	Usage         *piUsage    `json:"usage"`
	Content       []piContent `json:"content"`
}

// piContent is one content block. The type, like the rest of the JSON scalar
// and text handling, lives in sessionlog.
type piContent = sessionlog.Content

// firstText joins the text blocks of a message.
func firstText(cs []piContent) string { return sessionlog.FirstText(cs) }

// isTitleCandidate reports whether a user message can serve as the session
// title: plugin-injected system reminders and slash commands are skipped.
func isTitleCandidate(text string) bool { return sessionlog.TitleCandidate(text) }

// piUsage is message.usage. See the package doc for why these five fields map
// one-to-one onto model.Tokens without any subtraction — that reading is
// sessionlog.ReasoningSeparate, and Step Code, a Pi fork, reads its own usage
// block the other way round.
type piUsage = sessionlog.Usage

// piAsyncUsage is the top-level usage object of an async transcript record. It
// has no "totalTokens" field, and its cost may be a plain number rather than an
// object; sessionlog.Usage decodes both shapes so a compaction record — the same
// shape as message.usage.cost — reaches the ledger instead of failing to decode
// as a whole.
type piAsyncUsage = sessionlog.Usage

// asyncMetaProvider reads the provider an async run's meta file names, e.g. "rn"
// out of "rn/gpt-6.1-sol:high". Only the model field is decoded: the meta's
// "task" prompt and "usage" aggregate are never read, because the aggregate
// would double count the transcript's own records.
func asyncMetaProvider(path string) string {
	base := filepath.Base(path)
	i := strings.Index(base, "_transcript.jsonl")
	if i <= 0 {
		return ""
	}
	meta := filepath.Join(filepath.Dir(path), base[:i]+"_meta.json")
	data, err := os.ReadFile(meta)
	if err != nil {
		return ""
	}
	var m struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	if p, _, ok := strings.Cut(m.Model, "/"); ok && p != "" {
		return p
	}
	return ""
}

// num and fnum are sessionlog's JSON scalar decoders, aliased so Pi's record
// structs read exactly as they did before the extraction. The timestamp
// decoders (sessionlog.ParseStamp, sessionlog.Millis) and the small string
// helpers (sessionlog.FirstNonEmpty) are shared the same way: Step Code's logs
// use the same encodings.
type (
	num  = sessionlog.Num
	fnum = sessionlog.Fnum
)
