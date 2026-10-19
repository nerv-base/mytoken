package scan

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

// versionedParser is a fake harness whose dedup key carries its parse version.
// A re-read with a new version therefore adds a row instead of updating the old
// one, exactly like a parser fix that changes what a log line means; the test
// can then tell "the old rows were purged" from "the old rows survived and a
// new one was appended". One session per source keeps multi-source harnesses
// distinguishable.
type versionedParser struct {
	id      model.Harness
	version int
	path    string
	// paths overrides path for tests that need several sources. Every path is
	// returned by Discover even when it no longer exists on disk; a real parser
	// would not do that, and it lets resetScope's existence check be tested.
	paths   []string
	mu      sync.Mutex
	offsets []int64
}

func (p *versionedParser) Harness() model.Harness { return p.id }
func (p *versionedParser) Roots() []string {
	return []string{filepath.Dir(p.allPaths()[0])}
}
func (p *versionedParser) ParserVersion() int { return p.version }

func (p *versionedParser) allPaths() []string {
	if len(p.paths) > 0 {
		return p.paths
	}
	return []string{p.path}
}

func (p *versionedParser) Discover(context.Context) ([]harness.Source, error) {
	paths := p.allPaths()
	out := make([]harness.Source, 0, len(paths))
	for _, path := range paths {
		out = append(out, harness.Source{Path: path, Kind: "jsonl"})
	}
	return out, nil
}

// SourceSessions implements harness.SourceSessions.
func (p *versionedParser) SourceSessions(_ context.Context, src harness.Source) ([]string, error) {
	return []string{p.sessionID(src)}, nil
}

// sessionID is unique per source, so two sources of one harness do not share a
// session row and the scoped reset can be checked row by row.
func (p *versionedParser) sessionID(src harness.Source) string {
	base := strings.TrimSuffix(filepath.Base(src.Path), filepath.Ext(src.Path))
	return string(p.id) + "-" + base
}

func (p *versionedParser) Parse(_ context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	p.mu.Lock()
	p.offsets = append(p.offsets, cur.Offset)
	p.mu.Unlock()
	raw, err := os.ReadFile(src.Path)
	if err != nil {
		return harness.Batch{}, err
	}
	b := harness.Batch{Next: harness.Cursor{Offset: int64(len(raw)), Size: int64(len(raw))}}
	if cur.Offset >= int64(len(raw)) {
		return b, nil
	}
	at := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	session := p.sessionID(src)
	b.Events = []model.UsageEvent{{
		Harness: p.id, DedupKey: fmt.Sprintf("%s:v%d", session, p.version),
		SessionID: session, Timestamp: at, Model: "m",
		Tokens: model.Tokens{Input: int64(p.version)},
	}}
	b.Sessions = []model.SessionMeta{{
		Harness: p.id, SessionID: session, StartedAt: at, UpdatedAt: at,
	}}
	return b, nil
}

// firstOffset reports the offset of the parser's first Parse call, and -1 when
// it was never called.
func (p *versionedParser) firstOffset() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.offsets) == 0 {
		return -1
	}
	return p.offsets[0]
}

func (p *versionedParser) lastOffset() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.offsets) == 0 {
		return -1
	}
	return p.offsets[len(p.offsets)-1]
}

func writeSource(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func harnessEventCount(t *testing.T, st *store.Store, h model.Harness) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM event_data WHERE harness=?`, h).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestParserVersionReingestsChangedHarnessOnly(t *testing.T) {
	st := openInstanceStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	alphaPath := filepath.Join(dir, "alpha.log")
	betaPath := filepath.Join(dir, "beta.log")
	writeSource(t, alphaPath, "alpha\n")
	writeSource(t, betaPath, "beta\n")

	// A fresh database records the current version and ingests each source
	// exactly once, with a cursor left behind for the next scan.
	alpha1 := &versionedParser{id: "alpha", version: 1, path: alphaPath}
	beta1 := &versionedParser{id: "beta", version: 1, path: betaPath}
	if err := New(st, nil, nil, alpha1, beta1).Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if got := totalEvents(t, st); got != 2 {
		t.Fatalf("fresh scan events=%d, want 2", got)
	}
	if got := alpha1.firstOffset(); got != 0 {
		t.Fatalf("fresh alpha first offset=%d, want 0", got)
	}
	if got := cursorRows(t, st, "alpha"); got != 1 {
		t.Fatalf("fresh alpha cursors=%d, want 1", got)
	}
	for _, tc := range []struct {
		h    model.Harness
		want int
	}{{"alpha", 1}, {"beta", 1}} {
		if got, err := st.HarnessParserVersion(ctx, tc.h); err != nil || got != tc.want {
			t.Fatalf("%s version=%d err=%v, want %d", tc.h, got, err, tc.want)
		}
	}

	// Matching versions do nothing: the scan still stats and parses each
	// source, but both cursors are already at EOF, so no row or cursor moves.
	alpha2 := &versionedParser{id: "alpha", version: 1, path: alphaPath}
	beta2 := &versionedParser{id: "beta", version: 1, path: betaPath}
	if err := New(st, nil, nil, alpha2, beta2).Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if got := totalEvents(t, st); got != 2 {
		t.Fatalf("matching scan events=%d, want 2", got)
	}
	if got := alpha2.lastOffset(); got != int64(len("alpha\n")) {
		t.Fatalf("matching alpha offset=%d, want EOF", got)
	}
	if got := cursorRows(t, st, "alpha"); got != 1 {
		t.Fatalf("matching alpha cursors=%d, want 1", got)
	}

	// A version bump purges only alpha's rows and cursors. Alpha is read from
	// offset 0 and lands as one v2 row; beta keeps its v1 row and its cursor.
	alpha3 := &versionedParser{id: "alpha", version: 2, path: alphaPath}
	beta3 := &versionedParser{id: "beta", version: 1, path: betaPath}
	if err := New(st, nil, nil, alpha3, beta3).Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if got := totalEvents(t, st); got != 2 {
		t.Fatalf("after bump events=%d, want 2 (old alpha purged, not merged)", got)
	}
	if got := harnessEventCount(t, st, "alpha"); got != 1 {
		t.Fatalf("alpha events=%d, want 1", got)
	}
	if got := harnessEventCount(t, st, "beta"); got != 1 {
		t.Fatalf("beta events=%d, want 1", got)
	}
	if got := alpha3.firstOffset(); got != 0 {
		t.Fatalf("bumped alpha first offset=%d, want 0", got)
	}
	if got := beta3.lastOffset(); got != int64(len("beta\n")) {
		t.Fatalf("untouched beta offset=%d, want EOF", got)
	}
	var key string
	var input int64
	if err := st.DB().QueryRow(`SELECT dedup_key,input FROM event_data WHERE harness='alpha'`).Scan(&key, &input); err != nil {
		t.Fatal(err)
	}
	if key != "alpha-alpha:v2" || input != 2 {
		t.Fatalf("alpha row=(%q,%d), want (alpha:v2,2)", key, input)
	}
	var betaKey string
	if err := st.DB().QueryRow(`SELECT dedup_key FROM event_data WHERE harness='beta'`).Scan(&betaKey); err != nil {
		t.Fatal(err)
	}
	if betaKey != "beta-beta:v1" {
		t.Fatalf("beta row=%q, want beta:v1", betaKey)
	}
	if got := cursorRows(t, st, "alpha"); got != 1 {
		t.Fatalf("after bump alpha cursors=%d, want 1", got)
	}
	if got := cursorRows(t, st, "beta"); got != 1 {
		t.Fatalf("after bump beta cursors=%d, want 1", got)
	}
	if got, err := st.HarnessParserVersion(ctx, "alpha"); err != nil || got != 2 {
		t.Fatalf("alpha version=%d err=%v, want 2", got, err)
	}
	if got, err := st.HarnessParserVersion(ctx, "beta"); err != nil || got != 1 {
		t.Fatalf("beta version=%d err=%v, want 1", got, err)
	}
	// The DELETE triggers must have reconciled the rollup: two live requests,
	// even though alpha was inserted twice across the two parser versions.
	var requests int
	if err := st.DB().QueryRow(`SELECT coalesce(sum(requests),0) FROM daily_usage`).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("daily requests=%d, want 2", requests)
	}

	// A crash after the reset commits but before the re-read must not lose the
	// harness: the marker and the cleared cursors are durable, and the next
	// start re-reads the sources even though the versions now match.
	if err := st.ResetHarness(ctx, "alpha", 3, store.ResetScope{
		Paths:    []string{alphaPath},
		Sessions: []string{"alpha-alpha"},
	}); err != nil {
		t.Fatal(err)
	}
	alpha4 := &versionedParser{id: "alpha", version: 3, path: alphaPath}
	beta4 := &versionedParser{id: "beta", version: 1, path: betaPath}
	if err := New(st, nil, nil, alpha4, beta4).Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if got := totalEvents(t, st); got != 2 {
		t.Fatalf("post-crash scan events=%d, want 2", got)
	}
	if got := alpha4.firstOffset(); got != 0 {
		t.Fatalf("post-crash alpha first offset=%d, want 0", got)
	}
	if err := st.DB().QueryRow(`SELECT dedup_key FROM event_data WHERE harness='alpha'`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if key != "alpha-alpha:v3" {
		t.Fatalf("post-crash alpha row=%q, want alpha:v3", key)
	}
	if got, err := st.HarnessParserVersion(ctx, "alpha"); err != nil || got != 3 {
		t.Fatalf("post-crash alpha version=%d err=%v, want 3", got, err)
	}
}

// TestParserVersionAdoptsUnversionedRows is the upgrade path from a database
// written before parser versions existed: a harness whose parser still reports
// the initial generation keeps its rows and only gains a marker, while a harness
// with a newer generation is purged and re-read.
func TestParserVersionAdoptsUnversionedRows(t *testing.T) {
	st := openInstanceStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	alphaPath := filepath.Join(dir, "alpha.log")
	betaPath := filepath.Join(dir, "beta.log")
	writeSource(t, alphaPath, "alpha\n")
	writeSource(t, betaPath, "beta\n")

	// beta has a legacy row and a cursor at EOF but no parser-version marker.
	at := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	legacy := harness.Batch{
		Events: []model.UsageEvent{{
			Harness: "beta", DedupKey: "beta:legacy", SessionID: "beta-session",
			Timestamp: at, Model: "m", Tokens: model.Tokens{Input: 99},
		}},
		Sessions: []model.SessionMeta{{
			Harness: "beta", SessionID: "beta-session", StartedAt: at, UpdatedAt: at,
		}},
		Next: harness.Cursor{Offset: int64(len("beta\n")), Size: int64(len("beta\n")), ModTime: at},
	}
	if err := st.Commit(ctx, "beta", betaPath, legacy, nil); err != nil {
		t.Fatal(err)
	}

	alpha := &versionedParser{id: "alpha", version: 2, path: alphaPath}
	beta := &versionedParser{id: "beta", version: 1, path: betaPath}
	if err := New(st, nil, nil, alpha, beta).Scan(ctx); err != nil {
		t.Fatal(err)
	}

	if got := totalEvents(t, st); got != 2 {
		t.Fatalf("events=%d, want 2 (legacy beta + re-read alpha)", got)
	}
	var key string
	var input int64
	if err := st.DB().QueryRow(`SELECT dedup_key,input FROM event_data WHERE harness='alpha'`).Scan(&key, &input); err != nil {
		t.Fatal(err)
	}
	if key != "alpha-alpha:v2" || input != 2 {
		t.Fatalf("alpha row=(%q,%d), want (alpha:v2,2)", key, input)
	}
	if got := alpha.firstOffset(); got != 0 {
		t.Fatalf("alpha first offset=%d, want 0", got)
	}
	if err := st.DB().QueryRow(`SELECT dedup_key,input FROM event_data WHERE harness='beta'`).Scan(&key, &input); err != nil {
		t.Fatal(err)
	}
	if key != "beta:legacy" || input != 99 {
		t.Fatalf("beta row=(%q,%d), want legacy row (beta:legacy,99)", key, input)
	}
	if got := beta.lastOffset(); got != int64(len("beta\n")) {
		t.Fatalf("beta parsed at offset=%d, want EOF", got)
	}
	if got := cursorRows(t, st, "beta"); got != 1 {
		t.Fatalf("beta cursors=%d, want 1", got)
	}
	if got, err := st.HarnessParserVersion(ctx, "alpha"); err != nil || got != 2 {
		t.Fatalf("alpha version=%d err=%v, want 2", got, err)
	}
	if got, err := st.HarnessParserVersion(ctx, "beta"); err != nil || got != 1 {
		t.Fatalf("beta version=%d err=%v, want 1", got, err)
	}

	// The adopted marker makes the next start a no-op.
	alpha2 := &versionedParser{id: "alpha", version: 2, path: alphaPath}
	beta2 := &versionedParser{id: "beta", version: 1, path: betaPath}
	if err := New(st, nil, nil, alpha2, beta2).Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if got := totalEvents(t, st); got != 2 {
		t.Fatalf("second scan events=%d, want 2", got)
	}
}

func harnessSessionCount(t *testing.T, st *store.Store, h model.Harness) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM sessions WHERE harness=?`, h).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestParserVersionKeepsVanishedSource covers the loss this reset exists for: a
// harness rotated a log away before the parser changed. The source cannot be
// re-read, so the previous parser's rows are the only copy of that history and
// must survive, cursor included. Sources that still exist are re-read from the
// start, and a different harness is untouched.
func TestParserVersionKeepsVanishedSource(t *testing.T) {
	st := openInstanceStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	keepPath := filepath.Join(dir, "keep.log")
	gonePath := filepath.Join(dir, "gone.log")
	betaPath := filepath.Join(dir, "beta.log")
	writeSource(t, keepPath, "keep\n")
	writeSource(t, gonePath, "gone\n")
	writeSource(t, betaPath, "beta\n")

	alpha1 := &versionedParser{id: "alpha", version: 1, paths: []string{keepPath, gonePath}}
	beta1 := &versionedParser{id: "beta", version: 1, path: betaPath}
	if err := New(st, nil, nil, alpha1, beta1).Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if got := harnessEventCount(t, st, "alpha"); got != 2 {
		t.Fatalf("alpha events=%d, want 2", got)
	}
	if got := cursorRows(t, st, "alpha"); got != 2 {
		t.Fatalf("alpha cursors=%d, want 2", got)
	}

	// The harness deleted one transcript before the parser version changed, so
	// discovery no longer returns it.
	if err := os.Remove(gonePath); err != nil {
		t.Fatal(err)
	}
	alpha2 := &versionedParser{id: "alpha", version: 2, paths: []string{keepPath}}
	beta2 := &versionedParser{id: "beta", version: 1, path: betaPath}
	if err := New(st, nil, nil, alpha2, beta2).Scan(ctx); err != nil {
		t.Fatal(err)
	}

	if got := harnessEventCount(t, st, "alpha"); got != 2 {
		t.Fatalf("after bump alpha events=%d, want 2 (gone kept + keep re-read)", got)
	}
	var key string
	var input int64
	if err := st.DB().QueryRow(`SELECT dedup_key,input FROM event_data WHERE harness='alpha' AND dedup_key='alpha-gone:v1'`).Scan(&key, &input); err != nil {
		t.Fatalf("vanished source's event was lost: %v", err)
	}
	if input != 1 {
		t.Fatalf("vanished event input=%d, want 1", input)
	}
	if err := st.DB().QueryRow(`SELECT dedup_key,input FROM event_data WHERE harness='alpha' AND dedup_key='alpha-keep:v2'`).Scan(&key, &input); err != nil {
		t.Fatal(err)
	}
	if input != 2 {
		t.Fatalf("re-read event input=%d, want 2", input)
	}
	var stale int
	if err := st.DB().QueryRow(`SELECT count(*) FROM event_data WHERE harness='alpha' AND dedup_key='alpha-keep:v1'`).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Fatalf("stale keep row=%d, want 0", stale)
	}
	// The existing source was re-read from the beginning, and the vanished
	// source's cursor was not deleted, so both cursors are still there.
	if got := cursorRows(t, st, "alpha"); got != 2 {
		t.Fatalf("alpha cursors=%d, want 2", got)
	}
	if got := alpha2.firstOffset(); got != 0 {
		t.Fatalf("existing source first offset=%d, want 0", got)
	}
	if got := harnessSessionCount(t, st, "alpha"); got != 2 {
		t.Fatalf("alpha sessions=%d, want 2 (one per source)", got)
	}

	// The untouched harness keeps its row, cursor and marker.
	if got := harnessEventCount(t, st, "beta"); got != 1 {
		t.Fatalf("beta events=%d, want 1", got)
	}
	if got := cursorRows(t, st, "beta"); got != 1 {
		t.Fatalf("beta cursors=%d, want 1", got)
	}
	if got, err := st.HarnessParserVersion(ctx, "beta"); err != nil || got != 1 {
		t.Fatalf("beta version=%d err=%v, want 1", got, err)
	}
	if got, err := st.HarnessParserVersion(ctx, "alpha"); err != nil || got != 2 {
		t.Fatalf("alpha version=%d err=%v, want 2", got, err)
	}
	if got := beta2.lastOffset(); got != int64(len("beta\n")) {
		t.Fatalf("beta offset=%d, want EOF", got)
	}

	// A second scan at the same version does not duplicate anything.
	alpha3 := &versionedParser{id: "alpha", version: 2, paths: []string{keepPath}}
	beta3 := &versionedParser{id: "beta", version: 1, path: betaPath}
	if err := New(st, nil, nil, alpha3, beta3).Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if got := harnessEventCount(t, st, "alpha"); got != 2 {
		t.Fatalf("second scan alpha events=%d, want 2", got)
	}
	if got := totalEvents(t, st); got != 3 {
		t.Fatalf("second scan events=%d, want 3", got)
	}
}

// unscopedParser reports a new parser generation without implementing
// harness.SourceSessions: the scanner must refuse to guess a delete scope.
type unscopedParser struct {
	id      model.Harness
	version int
	path    string
}

func (p *unscopedParser) Harness() model.Harness { return p.id }
func (p *unscopedParser) Roots() []string        { return []string{filepath.Dir(p.path)} }
func (p *unscopedParser) ParserVersion() int     { return p.version }
func (p *unscopedParser) Discover(context.Context) ([]harness.Source, error) {
	return []harness.Source{{Path: p.path, Kind: "jsonl"}}, nil
}
func (p *unscopedParser) Parse(context.Context, harness.Source, harness.Cursor) (harness.Batch, error) {
	return harness.Batch{}, nil
}

func TestParserVersionRefusesUnscopedParser(t *testing.T) {
	st := openInstanceStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "alpha.log")
	writeSource(t, path, "alpha\n")

	if err := New(st, nil, nil, &versionedParser{id: "alpha", version: 1, path: path}).Scan(ctx); err != nil {
		t.Fatal(err)
	}
	err := New(st, nil, nil, &unscopedParser{id: "alpha", version: 2, path: path}).Scan(ctx)
	if err == nil || !strings.Contains(err.Error(), "does not implement harness.SourceSessions") {
		t.Fatalf("scan error=%v, want missing SourceSessions refusal", err)
	}
	// Nothing was deleted and no marker was written, so the harness can be
	// re-ingested as soon as its parser can report a scope.
	if got := harnessEventCount(t, st, "alpha"); got != 1 {
		t.Fatalf("events=%d, want 1", got)
	}
	if got := cursorRows(t, st, "alpha"); got != 1 {
		t.Fatalf("cursors=%d, want 1", got)
	}
	if got, err := st.HarnessParserVersion(ctx, "alpha"); err != nil || got != 1 {
		t.Fatalf("version=%d err=%v, want 1", got, err)
	}
}

// TestResetScopeSkipsVanishedSources pins the race guard: discovery still lists
// a path that disappeared before the read, so resetScope must leave it and its
// sessions out of the scope.
func TestResetScopeSkipsVanishedSources(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	keepPath := filepath.Join(dir, "keep.log")
	gonePath := filepath.Join(dir, "gone.log")
	writeSource(t, keepPath, "keep\n")

	p := &versionedParser{id: "alpha", version: 2, paths: []string{keepPath, gonePath}}
	scope, err := (&Scanner{}).resetScope(ctx, "alpha", 2, []Instance{{ID: "default", Parser: p}})
	if err != nil {
		t.Fatal(err)
	}
	if len(scope.Paths) != 1 || scope.Paths[0] != keepPath {
		t.Fatalf("paths=%v, want only %q", scope.Paths, keepPath)
	}
	if len(scope.Sessions) != 1 || scope.Sessions[0] != "alpha-keep" {
		t.Fatalf("sessions=%v, want only alpha-keep", scope.Sessions)
	}
}
