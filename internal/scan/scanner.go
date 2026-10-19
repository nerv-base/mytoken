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
	// interval is the full scan period (discovery plus every source);
	// discoverInterval walks the trees and parses what moved in between;
	// pollInterval stats the known sources without walking anything.
	interval         time.Duration
	discoverInterval time.Duration
	pollInterval     time.Duration
	debounce         time.Duration
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
	// discovered caches the sources each (harness, instance) last discovered.
	// A watched or polled scan reuses it instead of walking every harness
	// tree, so a scan for one appended line does not run all Discover
	// functions. It is guarded by scanMu: only a scan reads or replaces it.
	discovered map[string][]harness.Source
	// activity records the newest source modification time per directory, so
	// Run can spend its watch budget on the directories agents are writing.
	// It is replaced by every scan and read under mu.
	activity map[string]time.Time
	// priceFP caches the pricing fingerprint. Fingerprinting hashes the whole
	// price table, so a scan for one appended line reuses the value the last
	// full discovery computed; a full scan (and the first scan) recomputes it.
	priceFP string
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
}

// fileStamp is a source file's size and modification time, with those of
// a SQLite write-ahead log beside it, which takes writes the file doesn't
// show until a checkpoint.
type fileStamp struct {
	size, walSize int64
	mod, walMod   time.Time
}

func stampOf(path string, info os.FileInfo, withWAL bool) fileStamp {
	st := fileStamp{size: info.Size(), mod: info.ModTime()}
	if withWAL {
		if wal, e := os.Stat(path + "-wal"); e == nil {
			st.walSize, st.walMod = wal.Size(), wal.ModTime()
		}
	}
	return st
}

// sourceHasWAL reports whether a source is a SQLite database whose writes may
// land in a side WAL first. JSONL logs never have one, and skipping that stat
// halves the syscalls of a scan over thousands of session files.
func sourceHasWAL(src harness.Source) bool {
	switch src.Kind {
	case "jsonl", "json", "jsonl.zstd":
		return false
	}
	return true
}

// maxWatchDirs bounds the directories Run watches. On macOS each watched
// directory costs a descriptor for it and each file in it, and a harness
// home can hold a whole program (~/.hermes/hermes-agent: 150K files): the
// periodic scan covers whatever is past the bound.
const maxWatchDirs = 2048

// newDirBudget bounds how deep Run follows a directory created under a
// watched one, such as a new session folder, before the next scan.
const newDirBudget = 32

// maxWatchHints bounds the changed paths one scan remembers. Past it the
// scan discovers every parser instead, which is cheaper than checking a
// large hint set against every root.
const maxWatchHints = 256

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
	// bounded pool keeps peak RSS predictable while retaining parallel IO; the
	// corpus is dominated by a handful of very large logs, so a wider pool than
	// four still fills the disk-read pipeline on a typical machine.
	if workers > 8 {
		workers = 8
	}
	return &Scanner{
		st: st, resolver: resolver, pricer: pricer, workers: workers,
		// interval is the full scan period: discovery plus a parse of every
		// source, which also revalidates fingerprints. The cheap poll below
		// covers changes between those scans, and discoverInterval is the
		// longest a source can stay unseen, so all three are separate.
		interval:         10 * time.Minute,
		discoverInterval: 3 * time.Minute,
		pollInterval:     30 * time.Second,
		debounce:         250 * time.Millisecond,
		minGap:           750 * time.Millisecond,
		clock:            time.Now,
		ingest:           newIngestTracker(),
	}
}
func (s *Scanner) Progress() (done, total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done, s.total
}
func (s *Scanner) LastError() error { s.mu.Lock(); defer s.mu.Unlock(); return s.lastError }

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
	mode := scanFull
	if quick {
		mode = scanQuickAll
	}
	return s.scanMode(ctx, mode, nil)
}

// scanMode selects how much discovery and parsing a scan does.
type scanMode int

const (
	// scanFull discovers every parser and parses every source.
	scanFull scanMode = iota
	// scanQuickAll discovers every parser and parses only the sources whose
	// stat moved since their last clean parse.
	scanQuickAll
	// scanWatched reuses the sources discovery returned last time; only the
	// parsers whose roots a watched change touched are discovered again.
	scanWatched
	// scanPoll parses the known sources without walking any tree. It covers
	// appends in directories the watch budget could not afford.
	scanPoll
)

// settleParserVersions re-ingests, once, every harness whose parser generation
// changed since its rows were written. The current code would not produce those
// rows, so the scan that called this reads every source again.
//
// The reset is scoped, not a purge: sources that still exist on disk are
// discovered here, their sessions are collected from the parser, and only those
// rows and cursors are removed. A source the harness rotated away — Claude Code
// deletes transcripts after cleanupPeriodDays, for example — keeps its events,
// because the re-read cannot replace them and the database is their only copy.
// Its cursor stays too, so a file that reappears is still read from where the
// old parser stopped. A database that predates parser versions has no marker;
// its rows were written by InitialParserVersion, so an initial parser adopts the
// marker and keeps them.
//
// The marker and the scoped deletes are one store transaction. A crash before
// the commit leaves the old marker and the old rows; a crash after it leaves the
// new marker with the existing sources' cursors gone, and the next scan — even a
// quick one — reads those sources from the beginning because it has no cursor
// and no parse stamp for them.
func (s *Scanner) settleParserVersions(ctx context.Context) error {
	// ResetHarness is harness-wide, but a harness may have several instances
	// with disjoint roots. Scope must therefore cover every instance of the
	// harness, not only the one that happened to be visited first.
	groups := make(map[model.Harness][]Instance, len(s.instances))
	order := make([]model.Harness, 0, len(s.instances))
	for _, inst := range s.instances {
		h := inst.Parser.Harness()
		if _, ok := groups[h]; !ok {
			order = append(order, h)
		}
		groups[h] = append(groups[h], inst)
	}
	for _, h := range order {
		insts := groups[h]
		want := harness.ParserVersion(insts[0].Parser)
		have, err := s.st.HarnessParserVersion(ctx, h)
		if err != nil {
			return fmt.Errorf("parser version %s: %w", h, err)
		}
		if have == 0 {
			// No marker: the rows predate parser versions and were written by
			// the initial parser. Adopting the initial generation keeps them;
			// only a later generation invalidates what is stored.
			if want == harness.InitialParserVersion {
				if err := s.st.RecordHarnessParserVersion(ctx, h, want); err != nil {
					return fmt.Errorf("record parser version %s v%d: %w", h, want, err)
				}
				continue
			}
			have = harness.InitialParserVersion
		}
		if have == want {
			continue
		}
		scope, err := s.resetScope(ctx, h, want, insts)
		if err != nil {
			return err
		}
		if err := s.st.ResetHarness(ctx, h, want, scope); err != nil {
			return fmt.Errorf("re-ingest %s for parser v%d (was v%d): %w", h, want, have, err)
		}
		s.forgetHarness(h)
	}
	return nil
}

// resetScope discovers the sources of every instance of h that still exist and
// asks the parser which sessions their events can use. Those paths and sessions
// are exactly what the following scan is going to re-read and replace; anything
// else belongs to a source the harness has already deleted and must survive.
func (s *Scanner) resetScope(ctx context.Context, h model.Harness, version int, insts []Instance) (store.ResetScope, error) {
	var scope store.ResetScope
	paths := make(map[string]bool)
	sessions := make(map[string]bool)
	for _, inst := range insts {
		scoper, ok := inst.Parser.(harness.SourceSessions)
		if !ok {
			// Refusing is the safe half of the choice: purging would destroy
			// history the log rotation already took, and leaving every row
			// would record a generation that did not write them. An
			// implementation is one method away.
			return store.ResetScope{}, fmt.Errorf("re-ingest %s for parser v%d: the parser does not implement harness.SourceSessions, so its current rows cannot be separated from history whose source files are gone", h, version)
		}
		srcs, err := inst.Parser.Discover(ctx)
		if err != nil {
			return store.ResetScope{}, fmt.Errorf("discover %s to scope parser v%d: %w", h, version, err)
		}
		for _, src := range srcs {
			if err := ctx.Err(); err != nil {
				return store.ResetScope{}, err
			}
			ids, err := scoper.SourceSessions(ctx, src)
			if err != nil {
				if os.IsNotExist(err) {
					// Vanished between discovery and the read: no re-read will
					// replace it, so leave its rows and cursor alone.
					continue
				}
				return store.ResetScope{}, fmt.Errorf("collect sessions of %s for %s parser v%d: %w", src.Path, h, version, err)
			}
			if _, err := os.Stat(src.Path); err != nil {
				continue
			}
			if !paths[src.Path] {
				paths[src.Path] = true
				scope.Paths = append(scope.Paths, src.Path)
			}
			for _, id := range ids {
				if id != "" && !sessions[id] {
					sessions[id] = true
					scope.Sessions = append(scope.Sessions, id)
				}
			}
		}
	}
	// Deterministic order keeps tests and diagnostics stable; the SQL does not
	// care.
	sort.Strings(scope.Paths)
	sort.Strings(scope.Sessions)
	return scope, nil
}

// forgetHarness drops the in-memory parse stamps of one harness so a quick scan
// does not skip a source whose cursor the reset just removed.
func (s *Scanner) forgetHarness(h model.Harness) {
	prefix := string(h) + "\x00"
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.seen {
		if strings.HasPrefix(key, prefix) {
			delete(s.seen, key)
		}
	}
}

// scanMode is the one scan path. hints are the paths a watching Run saw
// change; scanWatched uses them to decide which parsers to discover.
func (s *Scanner) scanMode(ctx context.Context, mode scanMode, hints []string) (err error) {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	s.diagBegin()
	defer func() {
		s.mu.Lock()
		s.lastError = err
		s.mu.Unlock()
	}()
	// A parser upgrade changes what a source yields, and scanning is
	// incremental: without this, a database written by an older parser would
	// keep its rows forever. Only the scans that visit every harness may reset
	// one, because they can also re-read it in the same pass; a watched or poll
	// scan sees a subset and would leave the harness empty until the next full
	// scan. The reset is transactional in the store, so a crash re-triggers it.
	if mode == scanFull || mode == scanQuickAll {
		if e := s.settleParserVersions(ctx); e != nil {
			return e
		}
	}
	// Attribution config (cc-switch, harness configs) changes rarely; the
	// scans that walk the trees reload it, the cheap poll and watched scans
	// reuse it.
	if s.resolver != nil && (mode == scanFull || mode == scanQuickAll) {
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
	// liveCandidate records that this scan includes at least one source whose
	// appended output is live ingestion, so the scan may report a rate.
	liveCandidate := false
	quick := mode != scanFull
	s.mu.Lock()
	if !quick || s.seen == nil {
		s.seen = map[string]fileStamp{}
	}
	seen := s.seen
	dirs := make(map[string]bool, len(s.dirs))
	if quick {
		// A watched scan only looks at the instances a change touched, so it
		// must not forget the directories the others last contributed.
		for dir := range s.dirs {
			dirs[dir] = true
		}
	}
	activity := make(map[string]time.Time, len(s.activity))
	if quick {
		for dir, at := range s.activity {
			activity[dir] = at
		}
	}
	s.mu.Unlock()
	if s.discovered == nil {
		s.discovered = map[string][]harness.Source{}
	}
	for _, inst := range s.instances {
		p := inst.Parser
		if mode == scanWatched && !touchedByHint(p.Roots(), hints) {
			// Another harness changed; this one's sources stay as they are
			// until the next poll. That keeps the reaction to one appended
			// line proportional to the harness it belongs to.
			continue
		}
		instKey := instanceSourceKey(p.Harness(), inst.ID)
		srcs, known := s.discovered[instKey]
		// A quick scan trusts the previous discovery unless the mode asks for
		// a full walk (scanQuickAll) or the parser was never discovered.
		discover := !quick || mode == scanQuickAll || !known
		var hinted []harness.Source
		if mode == scanWatched {
			// The watcher names the changed path, so only that path (or a
			// directory holding it) is a candidate. A hint no known source
			// covers means a new file or folder: only then is a walk needed.
			var unknown bool
			hinted, unknown = hintSelect(srcs, hints)
			if !known || unknown {
				discover = true
			} else if len(hinted) == 0 {
				continue
			}
		}
		attempted := false
		if discover {
			attempted = true
			s.diagAttempt(p.Harness(), inst.ID)
			var e error
			srcs, e = p.Discover(ctx)
			if e != nil {
				s.diagFailure(p.Harness(), inst.ID)
				errs = append(errs, fmt.Errorf("discover %s: %w", p.Harness(), e))
				continue
			}
			s.discovered[instKey] = srcs
			if mode == scanWatched {
				hinted, _ = hintSelect(srcs, hints)
			}
		}
		if mode == scanWatched {
			srcs = hinted
			if len(srcs) == 0 {
				continue
			}
		}
		type candidate struct {
			src     harness.Source
			size    int64
			stamp   *fileStamp
			isDir   bool
			statErr error
			changed bool
		}
		candidates := make([]candidate, 0, len(srcs))
		anyChanged := false
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
					dir := filepath.Dir(src.Path)
					dirs[dir] = true
					st := stampOf(src.Path, info, sourceHasWAL(src))
					stamp = &st
					if st.mod.After(activity[dir]) {
						activity[dir] = st.mod
					}
				}
			}
			key := instKey + "\x00" + src.Path
			changed := !quick
			if quick {
				changed = stamp == nil
				if stamp != nil {
					s.mu.Lock()
					prev, ok := seen[key]
					s.mu.Unlock()
					changed = !ok || prev != *stamp
				}
			}
			anyChanged = anyChanged || changed
			candidates = append(candidates, candidate{src: src, size: size, stamp: stamp, isDir: isDir, statErr: statErr, changed: changed})
		}
		// A scan that neither discovered nor found a changed source proved
		// nothing about this instance, so it leaves the last diagnostics
		// record alone rather than re-dating evidence it did not collect.
		if !attempted && (anyChanged || mode == scanFull || mode == scanQuickAll) {
			attempted = true
			s.diagAttempt(p.Harness(), inst.ID)
		}
		if attempted {
			for _, c := range candidates {
				s.diagDiscover(p.Harness(), inst.ID, c.src, c.isDir, c.statErr)
			}
		}
		var cursors map[string]harness.Cursor
		for _, c := range candidates {
			if !c.changed {
				continue
			}
			if cursors == nil {
				if cursors, err = s.st.CursorsIn(ctx, p.Harness(), inst.ID); err != nil {
					s.diagFailure(p.Harness(), inst.ID)
					errs = append(errs, err)
					break
				}
			}
			cursor, read := cursors[c.src.Path]
			// A watched-change scan of a source already read once continues an
			// established stream. Everything else is history: startup, periodic,
			// manual and rebuild scans replay sources that were never read, and
			// a first read during a watched change is a backfill too.
			live := quick && read
			if live {
				liveCandidate = true
			}
			work = append(work, source{parser: p, instance: inst.ID, src: c.src, cur: cursor, size: c.size, key: instKey + "\x00" + c.src.Path, stamp: c.stamp, live: live})
		}
	}
	// Start large logs first to avoid a long single-worker tail.
	sort.SliceStable(work, func(i, j int) bool { return work[i].size > work[j].size })
	s.mu.Lock()
	s.done = 0
	s.total = len(work)
	s.dirs = dirs
	s.activity = activity
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
				// sources.
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
	// A commit stores the canonical project (the dimension and session upserts
	// run the same resolver), so a watched append — the latency-sensitive path —
	// needs no dictionary sweep; repairing legacy rows can wait for the periodic
	// and full scans.
	if !quick || (wrote && mode != scanWatched) {
		if e := s.st.NormalizeProjects(ctx); e != nil {
			errs = append(errs, e)
		}
	}
	if s.pricer != nil {
		fp := s.priceFP
		if mode == scanFull || fp == "" {
			fp = s.pricer.Fingerprint()
			s.priceFP = fp
		}
		if e := s.st.EnsurePrices(ctx, fp, s.pricer.Evaluate); e != nil {
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
	// kqueue spends one fd per entry of a watched directory, so a new tree
	// stops at the same budget watchPlan respects instead of opening an fd
	// for every file it happens to contain.
	watchNew := func(path string) {
		budget := newDirBudget
		var walk func(string)
		walk = func(dir string) {
			if budget <= 0 || len(watched) >= watchFDBudget {
				return
			}
			entries, e := os.ReadDir(dir)
			if e != nil {
				return
			}
			if len(entries) > budget*8 {
				// A program being installed, not a session folder: polling
				// covers it and its appends do not need instant eventing.
				return
			}
			if !watch(dir) {
				return
			}
			budget--
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
		activity := make(map[string]time.Time, len(s.activity))
		for dir, at := range s.activity {
			activity[dir] = at
		}
		s.mu.Unlock()
		for _, dir := range watchPlan(roots, dirs, activity, watchFDBudget) {
			watch(dir)
		}
		s.mu.Lock()
		s.watching = len(watched)
		s.mu.Unlock()
	}
	watchRoots()
	// A source that fails to parse must not stop watching the others.
	if s.Scan(ctx) != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	watchRoots()
	full := time.NewTicker(s.interval)
	defer full.Stop()
	discover := time.NewTicker(s.discoverInterval)
	defer discover.Stop()
	poll := time.NewTicker(s.pollInterval)
	defer poll.Stop()
	last := time.Now()
	var timer *time.Timer
	var debounced <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	// hints are the paths watched changes reported since the last scan. A
	// scan for them discovers only the parsers they fall under; past the
	// bound it is cheaper to discover everything.
	var hints []string
	seenHint := map[string]bool{}
	hintsFull := false
	resetHints := func() {
		hints = hints[:0]
		seenHint = map[string]bool{}
		hintsFull = false
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			// A SQLite reader, this scanner included, writes its read mark into
			// the -shm sidecar: reacting to it would rescan after every read.
			if strings.HasSuffix(ev.Name, "-shm") {
				continue
			}
			if ev.Has(fsnotify.Create) {
				watchNew(ev.Name)
			}
			if !hintsFull {
				if !seenHint[ev.Name] {
					seenHint[ev.Name] = true
					hints = append(hints, ev.Name)
				}
				if len(hints) > maxWatchHints {
					hintsFull = true
				}
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
			mode := scanWatched
			if hintsFull {
				mode = scanQuickAll
			}
			_ = s.scanMode(ctx, mode, hints)
			resetHints()
			last = time.Now()
		case <-poll.C:
			// Stat the known sources only: a new file in an unwatched
			// directory waits for the discovery tick.
			_ = s.scanMode(ctx, scanPoll, nil)
		case <-discover.C:
			// Walk every harness tree but parse only what moved. This is
			// how a new session folder becomes known.
			_ = s.scanMode(ctx, scanQuickAll, nil)
		case <-full.C:
			_ = s.Scan(ctx)
			watchRoots()
			last = time.Now()
		}
	}
}

// instanceSourceKey keys the discovery cache by harness and instance, the way
// every other per-instance key in this package does.
func instanceSourceKey(h model.Harness, instance string) string {
	return string(h) + "\x00" + store.NormalizeInstanceID(instance)
}

// hintSelect narrows a discovered source list to the paths a watched change
// named, and reports whether a hint names something no known source covers — a
// new session file or directory — which is the only reason a watched scan has
// to walk a harness again. Watching already says exactly what changed, so
// statting every other source of that harness would waste the event.
func hintSelect(srcs []harness.Source, hints []string) (selected []harness.Source, unknown bool) {
	if len(hints) == 0 {
		return srcs, false
	}
	known := make(map[string]bool, len(srcs))
	for _, src := range srcs {
		known[src.Path] = true
	}
	hit := make(map[string]bool, len(hints))
	for _, hint := range hints {
		hint = filepath.Clean(hint)
		if hint == "" || hint == "." {
			continue
		}
		found := false
		for _, src := range srcs {
			if src.Path == hint || strings.HasPrefix(src.Path, hint+string(filepath.Separator)) {
				hit[src.Path] = true
				found = true
			}
		}
		if !found && !known[hint] {
			unknown = true
		}
	}
	selected = make([]harness.Source, 0, len(hit))
	for _, src := range srcs {
		if hit[src.Path] {
			selected = append(selected, src)
		}
	}
	return selected, unknown
}

// touchedByHint reports whether any watched change lies at or under one of the
// parser's roots, so only that parser needs a discovery walk.
func touchedByHint(roots, hints []string) bool {
	for _, hint := range hints {
		hint = filepath.Clean(hint)
		for _, root := range roots {
			root = filepath.Clean(root)
			if root == "" || root == "." {
				continue
			}
			if hint == root || strings.HasPrefix(hint, root+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

// watchFDBudget bounds the descriptors Run's watches may cost. On macOS a
// watch on a directory opens one descriptor for the directory and one for
// every entry in it (github.com/fsnotify/fsnotify backend_kqueue.go), so a
// harness home with thousands of session folders cannot be watched at all:
// the poll scan covers it. The budget leaves room for the database and the
// rest of the app under the 150 descriptor target.
const watchFDBudget = 96

// watchPlan returns the directories to watch within budget: every root and
// ancestor that fits, then the source directories with the newest writes.
// The caller adds them in order; a directory whose entries alone would
// exceed the budget is left to the poll scan.
func watchPlan(roots, sourceDirs []string, activity map[string]time.Time, budget int) []string {
	candidates := watchDirs(roots, sourceDirs)
	rootSet := make(map[string]bool, len(roots))
	for _, root := range roots {
		if root != "" {
			rootSet[filepath.Clean(root)] = true
		}
	}
	// watchDirs puts roots first; keep that, and order the rest by write
	// activity so new session folders win the remaining budget.
	ordered := make([]string, 0, len(candidates))
	rest := make([]string, 0, len(candidates))
	for _, dir := range candidates {
		if rootSet[filepath.Clean(dir)] {
			ordered = append(ordered, dir)
		} else {
			rest = append(rest, dir)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool {
		ti, tj := activity[filepath.Clean(rest[i])], activity[filepath.Clean(rest[j])]
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return rest[i] < rest[j]
	})
	ordered = append(ordered, rest...)
	out := make([]string, 0, len(ordered))
	spent := 0
	seen := make(map[string]bool, len(ordered))
	for _, dir := range ordered {
		dir = filepath.Clean(dir)
		if dir == "" || seen[dir] {
			continue
		}
		entries, e := os.ReadDir(dir)
		if e != nil {
			continue
		}
		cost := 1 + len(entries)
		if spent+cost > budget {
			continue
		}
		seen[dir] = true
		spent += cost
		out = append(out, dir)
	}
	return out
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
