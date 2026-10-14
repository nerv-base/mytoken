// Package scan incrementally indexes registered harness sources.
package scan

import (
	"context"
	"errors"
	"fmt"
	"github.com/fsnotify/fsnotify"
	"github.com/zzstar101/mytoken/internal/attrib"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/ingestrate"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/pricing"
	"github.com/zzstar101/mytoken/internal/store"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type Scanner struct {
	st       *store.Store
	resolver *attrib.Resolver
	pricer   *pricing.Pricer
	// instances are the (instance id, parser) pairs this scanner indexes.
	// Every piece of per-source state is keyed by harness, instance id and
	// path, so the same harness may be scanned several times over.
	instances   []Instance
	mu          sync.Mutex
	scanMu      sync.Mutex
	done, total int
	lastError   error
	workers     int
	interval    time.Duration
	debounce    time.Duration
	// minGap is the least time from one watched-change scan to the next:
	// agents append to their logs all the time, and each scan stats every
	// source, so back-to-back scans would keep a core busy.
	minGap time.Duration
	// dirs holds the directories of the sources the last scan found, which
	// Run watches with the directories between them and their roots.
	dirs map[string]bool
	// watching counts the directories Run watches, for tests.
	watching int
	// seen holds each file source's stat as of its last parse that found
	// nothing new. A
	// scan for watched changes skips a file whose stat has not moved,
	// without a cursor read or an open; the periodic scan parses all.
	seen map[string]fileStamp
	// parses counts Parse calls, for tests.
	parses int
	// ingest tracks committed live output tokens over a trailing window. It is
	// fed only by writes a watched change produced, never by startup, periodic,
	// manual or backfill scans.
	ingest *ingestrate.Tracker
	// ingestMu serializes every observation and snapshot, and ingestAt is the
	// latest instant handed to the tracker: both directions clamp to it so the
	// scanner's own clock can never look out of order to the tracker.
	ingestMu sync.Mutex
	ingestAt time.Time
	// clock is the scanner's wall clock, replaced in tests.
	clock func() time.Time
	// diag accumulates this scan's collection evidence, one entry per harness
	// instance, and diagMu guards it: worker goroutines record parse results
	// while the scanning goroutine records discovery and commits. It is reset at
	// the start of every scan and persisted once per instance when the scan
	// completes, and it never holds file contents or parser errors.
	diagMu sync.Mutex
	diag   map[string]*diagInstance
	// tailSeen memoizes the stat of every Codex rollout whose quota tail was
	// read, so an unchanged file is not read again. Guarded by scanMu, like
	// tailOutcomes, which is what the last scan's bounded tail read did: one
	// entry per Codex instance, with the reason nothing was published.
	tailSeen     map[string]fileStamp
	tailOutcomes []QuotaTailOutcome
}

// fileStamp is a source file's size and modification time, with those of
// a SQLite write-ahead log beside it, which takes writes the file doesn't
// show until a checkpoint.
type fileStamp struct {
	size, walSize int64
	mod, walMod   time.Time
}

func stampOf(path string, info os.FileInfo) fileStamp {
	st := fileStamp{size: info.Size(), mod: info.ModTime()}
	if wal, e := os.Stat(path + "-wal"); e == nil {
		st.walSize, st.walMod = wal.Size(), wal.ModTime()
	}
	return st
}

// maxWatchDirs bounds the directories Run watches. On macOS each watched
// directory costs a descriptor for it and each file in it, and a harness
// home can hold a whole program (~/.hermes/hermes-agent: 150K files): the
// periodic scan covers whatever is past the bound.
const maxWatchDirs = 2048

// newDirBudget bounds how deep Run follows a directory created under a
// watched one, such as a new session folder, before the next scan.
const newDirBudget = 32

// New returns a Scanner over the given parsers, each recorded under the
// store's default instance. With no parsers it scans every registered
// harness; use NewWithInstances to scan named instances.
func New(st *store.Store, resolver *attrib.Resolver, pricer *pricing.Pricer, parsers ...harness.Parser) *Scanner {
	if len(parsers) == 0 {
		parsers = harness.All()
	}
	s := newScanner(st, resolver, pricer)
	for _, p := range parsers {
		s.instances = append(s.instances, Instance{ID: store.DefaultInstanceID, Parser: p})
	}
	return s
}

func newScanner(st *store.Store, resolver *attrib.Resolver, pricer *pricing.Pricer) *Scanner {
	workers := runtime.NumCPU()
	// Parsing many large JSONL files concurrently multiplies decoder buffers. A
	// bounded pool keeps peak RSS predictable while retaining parallel IO.
	if workers > 4 {
		workers = 4
	}
	return &Scanner{st: st, resolver: resolver, pricer: pricer, workers: workers, interval: 2 * time.Minute, debounce: 250 * time.Millisecond, minGap: 2 * time.Second, clock: time.Now, ingest: newIngestTracker()}
}
func (s *Scanner) Progress() (done, total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done, s.total
}
func (s *Scanner) LastError() error { s.mu.Lock(); defer s.mu.Unlock(); return s.lastError }

// QuotaTailOutcomes reports what the last completed scan's bounded Codex quota
// tail read did, one entry per Codex instance discovery saw. It is empty when
// the scan found no Codex source, and an entry with Published false says in
// Status and Reason why nothing was published.
func (s *Scanner) QuotaTailOutcomes() []QuotaTailOutcome {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	return append([]QuotaTailOutcome(nil), s.tailOutcomes...)
}

func (s *Scanner) Subscribe() (<-chan struct{}, func()) { return s.st.Subscribe() }

type source struct {
	parser   harness.Parser
	instance string
	src      harness.Source
	cur      harness.Cursor
	size     int64
	key      string     // harness, instance and path, for Scanner.seen
	stamp    *fileStamp // nil for a directory or a missing file
	// live is set for a watched-change scan of a source that was read before:
	// that is the only path where appended output is genuinely new ingestion
	// rather than the first pass over history.
	live bool
}

// Scan parses every source.
func (s *Scanner) Scan(ctx context.Context) error { return s.scan(ctx, false) }

// scan parses every source, or with quick only the files whose stat moved
// since their last clean parse, and the directories.
func (s *Scanner) scan(ctx context.Context, quick bool) (err error) {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	s.diagBegin()
	defer func() {
		s.mu.Lock()
		s.lastError = err
		s.mu.Unlock()
	}()
	if s.resolver != nil {
		if e := s.resolver.Refresh(ctx); e != nil {
			return e
		}
	}
	restoreIndexes, err := s.st.DeferEmptyIndexes(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, restoreIndexes()) }()
	var work []source
	var errs []error
	// tail collects the Codex rollout files discovery saw, so the bounded quota
	// tail read below needs no second walk of the log tree.
	var tail []quotaTailSource
	// liveCandidate records that this scan includes at least one source whose
	// appended output is live ingestion, so the scan may report a rate.
	liveCandidate := false
	dirs := map[string]bool{}
	s.mu.Lock()
	if !quick || s.seen == nil {
		s.seen = map[string]fileStamp{}
	}
	seen := s.seen
	s.mu.Unlock()
	for _, inst := range s.instances {
		p := inst.Parser
		s.diagAttempt(p.Harness(), inst.ID)
		srcs, e := p.Discover(ctx)
		if e != nil {
			s.diagFailure(p.Harness(), inst.ID)
			errs = append(errs, fmt.Errorf("discover %s: %w", p.Harness(), e))
			continue
		}
		var cursors map[string]harness.Cursor
		for _, src := range srcs {
			if err := ctx.Err(); err != nil {
				return err
			}
			var size int64
			var stamp *fileStamp
			isDir := false
			info, statErr := os.Stat(src.Path)
			if statErr == nil {
				size = info.Size()
				if info.IsDir() {
					isDir = true
					dirs[src.Path] = true
				} else {
					dirs[filepath.Dir(src.Path)] = true
					st := stampOf(src.Path, info)
					stamp = &st
				}
			}
			s.diagDiscover(p.Harness(), inst.ID, src, isDir, statErr)
			if p.Harness() == model.Codex && statErr == nil && !isDir {
				tail = append(tail, quotaTailSource{instance: inst.ID, path: src.Path, size: size, mod: info.ModTime()})
			}
			key := string(p.Harness()) + "\x00" + inst.ID + "\x00" + src.Path
			if quick && stamp != nil {
				s.mu.Lock()
				old, ok := seen[key]
				s.mu.Unlock()
				if ok && old == *stamp {
					s.diagSkipped(p.Harness(), inst.ID, src.Path)
					continue
				}
			}
			if cursors == nil {
				if cursors, err = s.st.CursorsIn(ctx, p.Harness(), inst.ID); err != nil {
					s.diagFailure(p.Harness(), inst.ID)
					errs = append(errs, err)
					break
				}
			}
			cursor, read := cursors[src.Path]
			// A watched-change scan of a source already read once continues an
			// established stream. Everything else is history: startup, periodic,
			// manual and rebuild scans replay sources that were never read, and
			// a first read during a watched change is a backfill too.
			live := quick && read
			if live {
				liveCandidate = true
			}
			work = append(work, source{parser: p, instance: inst.ID, src: src, cur: cursor, size: size, key: key, stamp: stamp, live: live})
		}
	}
	// Start large logs first to avoid a long single-worker tail.
	sort.SliceStable(work, func(i, j int) bool { return work[i].size > work[j].size })
	s.mu.Lock()
	s.done = 0
	s.total = len(work)
	s.dirs = dirs
	s.mu.Unlock()
	ch := make(chan source)
	type result struct {
		write *store.Write
		live  bool
		err   error
	}
	results := make(chan result, s.workers+1)
	var wg sync.WaitGroup
	n := s.workers
	if n < 1 {
		n = 1
	}
	if n > len(work) {
		n = len(work)
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range ch {
				if ctx.Err() != nil {
					return
				}
				write, out, err := s.scanOne(ctx, item)
				s.diagParse(item.parser.Harness(), item.instance, item.src, out, err)
				results <- result{write, item.live, err}
				s.mu.Lock()
				s.done++
				s.parses++
				// Remember only a parse that found nothing new: one that did
				// may have stopped short, and the next parse tells.
				if err == nil && write == nil && item.stamp != nil {
					seen[item.key] = *item.stamp
				}
				s.mu.Unlock()
			}
		}()
	}
	go func() {
	dispatch:
		for _, item := range work {
			select {
			case <-ctx.Done():
				break dispatch
			case ch <- item:
			}
		}
		close(ch)
		wg.Wait()
		close(results)
	}()
	type queued struct {
		write store.Write
		live  bool
	}
	var pending []queued
	eventCount := 0
	wrote := false
	// liveFailed records that a live source could not be read or committed, so
	// this scan must not report a rate: an unreadable live stream is unknown,
	// not zero.
	liveFailed := false
	flush := func() {
		if len(pending) == 0 {
			return
		}
		wrote = true
		committed := pending
		pending = nil
		eventCount = 0
		writes := make([]store.Write, len(committed))
		live := false
		for i := range committed {
			writes[i] = committed[i].write
			if committed[i].live {
				live = true
			}
		}
		if !live || s.ingest == nil {
			// The outcome is what the index actually gained: it reports only
			// rows this commit inserted for the first time, so a replay of
			// already-indexed records cannot be recorded as new collection.
			outcome, err := s.st.CommitManyOutcome(ctx, writes)
			if err != nil {
				// The cursors did not advance, so the next scan reparses these
				// sources. Quota is deliberately not published: a failed commit
				// must not make a fresh observation visible.
				errs = append(errs, err)
				liveFailed = liveFailed || live
				s.diagCommitFailed(writes)
				return
			}
			s.diagCommitted(writes, outcome)
		} else {
			// The window ends at the instant of this commit, and only events the
			// index did not already hold are eligible. A historical record
			// appended to an already watched file is a genuinely new row, but its
			// own timestamp places it outside the window, so it cannot inflate a
			// rate that describes recent ingestion.
			at := s.now()
			outcome, err := s.st.CommitManyOutcomeRecent(ctx, writes, at.Add(-ingestrate.Window), at)
			if err != nil {
				// Nothing was inserted, so nothing may be observed: a rolled
				// back commit must not leak tokens into the rate.
				errs = append(errs, err)
				liveFailed = true
				s.diagCommitFailed(writes)
				return
			}
			var tokens int64
			for i := range committed {
				if committed[i].live && i < len(outcome.Writes) {
					tokens = addOutput(tokens, outcome.Writes[i].NewRecentOutputTokens)
				}
			}
			s.observeIngestAt(tokens, at)
			s.diagCommitted(writes, outcome)
		}
		if err := s.publishQuota(ctx, writes); err != nil {
			errs = append(errs, err)
		}
	}
	for r := range results {
		if r.err != nil {
			errs = append(errs, r.err)
			liveFailed = liveFailed || r.live
		}
		if r.write != nil {
			pending = append(pending, queued{write: *r.write, live: r.live})
			eventCount += len(r.write.Batch.Events)
			if len(pending) >= 32 || eventCount >= 8192 {
				flush()
			}
		}
	}
	flush()
	// Codex quota state is carried in the cursor a parse pass advances, so a
	// source already at EOF never publishes one, and a pass that produced no
	// write never publishes at all. A bounded tail read of the newest rollout
	// files fills that gap: it reads no cursor and writes no event.
	if s.st != nil {
		outcomes, e := s.publishQuotaTail(ctx, tail)
		s.tailOutcomes = outcomes
		if e != nil {
			errs = append(errs, e)
		}
	}
	// A watched scan that read its live sources cleanly reports the rate even
	// when nothing was appended: zero new output over a live window is a real
	// zero, and the heartbeat keeps the signal fresh. A scanner that only ever
	// full-scans never reports one, so an idle historical index cannot fake a
	// live zero.
	if liveCandidate && !liveFailed {
		if at, snap := s.heartbeatIngest(); !at.IsZero() {
			if e := s.publishIngestRate(ctx, at, snap); e != nil {
				errs = append(errs, e)
			}
		}
	}
	// Only new writes can bring a project path to normalize.
	if wrote || !quick {
		if e := s.st.NormalizeProjects(ctx); e != nil {
			errs = append(errs, e)
		}
	}
	if s.pricer != nil {
		if e := s.st.EnsurePrices(ctx, s.pricer.Fingerprint(), s.pricer.Evaluate); e != nil {
			errs = append(errs, e)
		}
	}
	if e := ctx.Err(); e != nil {
		errs = append(errs, e)
	} else if e := s.persistDiagnostics(ctx, s.now()); e != nil {
		// The record is written once per completed scan, after every source and
		// commit is accounted for. A cancelled scan writes nothing, so the prior
		// evidence survives it.
		errs = append(errs, e)
	}
	return errors.Join(errs...)
}
func (s *Scanner) scanOne(ctx context.Context, item source) (*store.Write, parseOutcome, error) {
	h := item.parser.Harness()
	cur := item.cur
	b, e := item.parser.Parse(ctx, item.src, cur)
	if e != nil {
		return nil, parseOutcome{}, fmt.Errorf("parse %s %s: %w", h, item.src.Path, e)
	}
	out := parseOutcome{events: int64(len(b.Events)), sessions: int64(len(b.Sessions)), advanced: b.Next != cur}
	if len(b.Events) == 0 && len(b.Sessions) == 0 && !out.advanced {
		return nil, out, nil
	}
	if len(b.Events) > 0 {
		out.zeroTokens = true
		for i := range b.Events {
			if !b.Events[i].Tokens.IsZero() {
				out.zeroTokens = false
				break
			}
		}
	}
	resolved := make([]store.Resolution, len(b.Events))
	for i, event := range b.Events {
		if event.Harness == "" {
			event.Harness = h
		}
		provider, kind := "", model.AttribInferred
		if s.resolver != nil {
			// The instance is the one this source was discovered under, so a
			// named instance never inherits the default instance's global
			// request matches or config timeline.
			provider, kind = s.resolver.ResolveInstance(ctx, item.instance, event)
		} else if event.Provider != "" {
			provider, kind = event.Provider, model.AttribLog
		} else {
			provider = attrib.Infer(event.Model)
		}
		event.Provider = provider
		var cost float64
		priced := event.CostUSD != nil
		if s.pricer != nil {
			cost, priced = s.pricer.Evaluate(event)
		} else if event.CostUSD != nil {
			cost = *event.CostUSD
		}
		resolved[i] = store.Resolution{Provider: provider, Attrib: kind, Cost: cost, Priced: priced}
	}
	return &store.Write{Harness: h, InstanceID: item.instance, Path: item.src.Path, Batch: b, Resolutions: resolved}, out, nil
}
func (s *Scanner) Rebuild(ctx context.Context) error {
	if e := s.st.Rebuild(ctx); e != nil {
		return e
	}
	return s.Scan(ctx)
}
func (s *Scanner) Run(ctx context.Context) error {
	watcher, e := fsnotify.NewWatcher()
	if e != nil {
		return e
	}
	defer watcher.Close()
	watched := map[string]bool{}
	watch := func(path string) bool {
		if watched[path] {
			return true
		}
		if len(watched) >= maxWatchDirs {
			return false
		}
		if fi, e := os.Stat(path); e != nil || !fi.IsDir() {
			return false
		}
		if watcher.Add(path) != nil {
			return false
		}
		watched[path] = true
		return true
	}
	// watchNew follows a directory created under a watched one a few
	// levels down: a new session folder, not a program being installed.
	watchNew := func(path string) {
		budget := newDirBudget
		var walk func(string)
		walk = func(dir string) {
			if budget <= 0 || !watch(dir) {
				return
			}
			budget--
			entries, e := os.ReadDir(dir)
			if e != nil {
				return
			}
			for _, entry := range entries {
				if entry.IsDir() {
					walk(filepath.Join(dir, entry.Name()))
				}
			}
		}
		walk(path)
	}
	// watchRoots watches each root, and the directories from it down to
	// each source found: never a whole root, which may hold anything.
	watchRoots := func() {
		var roots []string
		for _, inst := range s.instances {
			roots = append(roots, inst.Parser.Roots()...)
		}
		s.mu.Lock()
		dirs := make([]string, 0, len(s.dirs))
		for dir := range s.dirs {
			dirs = append(dirs, dir)
		}
		s.mu.Unlock()
		for _, dir := range watchDirs(roots, dirs) {
			watch(dir)
		}
		defer func() {
			s.mu.Lock()
			s.watching = len(watched)
			s.mu.Unlock()
		}()
		if s.resolver != nil {
			if home, e := os.UserHomeDir(); e == nil {
				for _, name := range []string{".claude", ".codex"} {
					watch(filepath.Join(home, name))
				}
			}
		}
	}
	watchRoots()
	// A source that fails to parse must not stop watching the others.
	if s.Scan(ctx) != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	watchRoots()
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	last := time.Now()
	var timer *time.Timer
	var debounced <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if ev.Has(fsnotify.Create) {
				watchNew(ev.Name)
			}
			// The first change arms the timer and later ones ride on it:
			// re-arming on each would never scan while an agent writes on.
			if timer == nil {
				timer = time.NewTimer(s.debounce)
				debounced = timer.C
			}
		case _, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
		case <-debounced:
			if wait := s.minGap - time.Since(last); wait > 0 {
				timer.Reset(wait)
				continue
			}
			debounced = nil
			timer = nil
			_ = s.scan(ctx, true)
			last = time.Now()
		case <-ticker.C:
			_ = s.Scan(ctx)
			watchRoots()
			last = time.Now()
		}
	}
}

// watchDirs returns the directories to watch, roots first: each root, and
// for each source directory the directories from it up to its root.
func watchDirs(roots, dirs []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(dir string) {
		if !seen[dir] {
			seen[dir] = true
			out = append(out, dir)
		}
	}
	clean := make([]string, 0, len(roots))
	for _, root := range roots {
		if root != "" {
			root = filepath.Clean(root)
			clean = append(clean, root)
			add(root)
		}
	}
	dirs = append([]string(nil), dirs...)
	sort.Strings(dirs)
	for _, dir := range dirs {
		dir = filepath.Clean(dir)
		var chain []string
		for _, root := range clean {
			rel, e := filepath.Rel(root, dir)
			if e != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			for d := filepath.Dir(dir); len(d) > len(root); d = filepath.Dir(d) {
				chain = append(chain, d)
			}
		}
		for i := len(chain) - 1; i >= 0; i-- {
			add(chain[i])
		}
		add(dir)
	}
	return out
}
