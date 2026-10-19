// Package store owns the local, derived SQLite index.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zzstar101/mytoken/internal/provider"
	"github.com/zzstar101/mytoken/internal/sqlitedsn"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	_ "modernc.org/sqlite"
)

// DefaultInstanceID is the reserved instance every legacy caller writes to. An
// empty Write.InstanceID, an empty filter instance and an old parser all map to
// it, so the instance dimension is additive: an index with one data source keeps
// exactly the behaviour it had before the dimension existed.
const DefaultInstanceID = "default"

// normalizeInstance maps the empty (legacy) instance to the reserved default.
func normalizeInstance(instance string) string {
	if instance == "" {
		return DefaultInstanceID
	}
	return instance
}

// NormalizeInstanceID is the exported form of the empty-to-default mapping, so
// read-side consumers report the same instance a legacy write landed on.
func NormalizeInstanceID(instance string) string { return normalizeInstance(instance) }

type Resolution struct {
	Provider string
	Attrib   model.AttribSource
	Cost     float64
	Priced   bool
}
type Store struct {
	db *sql.DB
	// rdb is a separate read-only pool over the same file. Every query path that
	// is allowed to run concurrently with a writer uses it, so a GUI refresh can
	// read a WAL snapshot while a scan commit is in flight instead of queueing
	// behind the single writer connection. It is opened on first use, so a store
	// that only scans never holds a second connection (and its WAL read mark);
	// it stays nil for :memory: databases, where a second pool would be a
	// different database entirely.
	rdb           *sql.DB
	rdbOnce       sync.Once
	memory        bool
	path          string
	writer        sync.Mutex
	mu            sync.Mutex
	subscribers   map[chan struct{}]struct{}
	projects      *projectResolver
	closed        bool
	aggregateZone string
	// readOnly marks a handle produced by OpenReadOnly (readonly.go). Such a
	// handle performs no application write; schema is the schema report captured
	// when it was opened, so a caller can explain exactly which generation of the
	// index it is reading.
	readOnly bool
	schema   SchemaReport
	// costFn is the local estimate price function, captured from the most recent
	// EnsurePrices/ReplacePricing call (always under the writer lock). It powers
	// MissUSD recomputation on commit and rebuild; nil until prices are first
	// ensured, in which case miss_cost stays 0 until the next price pass.
	costFn func(model.UsageEvent) (float64, bool)
}

func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
	}
	dsn := path
	if path != ":memory:" {
		dsn = sqlitedsn.URI(path, "_pragma=busy_timeout(5000)")
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	st := &Store{db: db, path: path, memory: path == ":memory:", subscribers: make(map[chan struct{}]struct{})}
	// Keep bulk-index pages and temporary journals in memory without changing
	// WAL durability. A negative cache_size is a KiB budget, allocated on demand.
	//
	// This is also the bootstrap that converts the file to WAL and creates its
	// -wal/-shm sidecars. A read-only open of a WAL database uses SQLite's own
	// reader coordination for those sidecars (readonly.go) and never writes
	// index data.
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA busy_timeout=5000; PRAGMA temp_store=MEMORY; PRAGMA cache_size=-8192;
CREATE TABLE IF NOT EXISTS events (
 harness TEXT NOT NULL,dedup_key TEXT NOT NULL,session_id TEXT NOT NULL,parent_id TEXT NOT NULL DEFAULT '',project TEXT NOT NULL DEFAULT '',timestamp INTEGER NOT NULL,
 model TEXT NOT NULL,provider TEXT NOT NULL DEFAULT '',base_url TEXT NOT NULL DEFAULT '',input INTEGER NOT NULL,output INTEGER NOT NULL,cache_read INTEGER NOT NULL,cache_write INTEGER NOT NULL,reasoning INTEGER NOT NULL,
 log_cost REAL,resolved_provider TEXT NOT NULL,attrib TEXT NOT NULL,cost REAL NOT NULL,PRIMARY KEY(harness,dedup_key));
CREATE TABLE IF NOT EXISTS sessions(harness TEXT NOT NULL,session_id TEXT NOT NULL,parent_id TEXT NOT NULL DEFAULT '',title TEXT NOT NULL DEFAULT '',project TEXT NOT NULL DEFAULT '',started_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,PRIMARY KEY(harness,session_id));
CREATE INDEX IF NOT EXISTS sessions_parent ON sessions(harness,parent_id);
CREATE TABLE IF NOT EXISTS cursors(harness TEXT NOT NULL,path TEXT NOT NULL,cursor TEXT NOT NULL,PRIMARY KEY(harness,path));
CREATE TABLE IF NOT EXISTS provider_configs(harness TEXT NOT NULL,from_time INTEGER NOT NULL,base_url TEXT NOT NULL,provider TEXT NOT NULL,PRIMARY KEY(harness,from_time));
CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY,value TEXT NOT NULL);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err = st.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return st, nil
}
func (s *Store) DB() *sql.DB { return s.db }

// ReadDB is the connection pool for read queries that must not queue behind the
// writer. It is a separate read-only (query_only) pool over the same WAL
// database, with several connections, so a UI refresh and a session detail read
// can overlap a scan commit. For :memory: databases it is the single writer
// connection, because a second pool would see a different, empty database.
func (s *Store) ReadDB() *sql.DB {
	s.rdbOnce.Do(func() {
		if s.memory || s.readOnly || s.db == nil {
			return
		}
		path := s.path
		if path == "" {
			return
		}
		rdb, err := sql.Open("sqlite", sqlitedsn.URI(path, "_pragma=busy_timeout(5000)&_pragma=query_only(1)&_pragma=temp_store(MEMORY)&_pragma=cache_size(-16384)"))
		if err != nil {
			return
		}
		// A handful of connections is enough for the detail and list queries
		// the UI issues; each one holds its own WAL read snapshot.
		rdb.SetMaxOpenConns(4)
		rdb.SetMaxIdleConns(4)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = rdb.PingContext(ctx)
		cancel()
		if err != nil {
			// Falling back to the writer keeps the store usable (for example on
			// a filesystem where a second read-only connection is refused); it
			// only loses read/write concurrency.
			rdb.Close()
			return
		}
		s.rdb = rdb
	})
	if s.rdb != nil {
		return s.rdb
	}
	return s.db
}
func (s *Store) Close() error {
	s.mu.Lock()
	s.closed = true
	for ch := range s.subscribers {
		close(ch)
		delete(s.subscribers, ch)
	}
	s.mu.Unlock()
	// Closing the last writable connection checkpoints the WAL and unlinks the
	// sidecars. A read-only handle never depends on this process owning them, so
	// there is nothing to release here. The reader pool goes first so no open
	// read transaction can hold a WAL read mark past the writer's close.
	if s.rdb != nil {
		s.rdb.Close()
	}
	return s.db.Close()
}
func (s *Store) Exec(ctx context.Context, q string, args ...any) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

// Cursor returns the reserved default instance's checkpoint for one source. The
// pre-instance signature is preserved so existing callers are unchanged.
func (s *Store) Cursor(ctx context.Context, h model.Harness, path string) (harness.Cursor, error) {
	return s.CursorIn(ctx, h, DefaultInstanceID, path)
}

// CursorIn returns one instance's checkpoint for one source. Two instances that
// scan the same path keep independent cursors.
func (s *Store) CursorIn(ctx context.Context, h model.Harness, instance, path string) (harness.Cursor, error) {
	var c harness.Cursor
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT cursor FROM cursors WHERE harness=? AND instance_id=? AND path=?", h, normalizeInstance(instance), path).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	err = json.Unmarshal([]byte(raw), &c)
	return c, err
}

// Cursors returns the cursors of a harness's sources by path for the reserved
// default instance. The pre-instance signature is preserved.
func (s *Store) Cursors(ctx context.Context, h model.Harness) (map[string]harness.Cursor, error) {
	return s.CursorsIn(ctx, h, DefaultInstanceID)
}

// CursorsIn returns one instance's source checkpoints by path. Scanner wiring for
// a configured instance calls this with that instance's id.
func (s *Store) CursorsIn(ctx context.Context, h model.Harness, instance string) (map[string]harness.Cursor, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT path, cursor FROM cursors WHERE harness=? AND instance_id=?", h, normalizeInstance(instance))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]harness.Cursor{}
	for rows.Next() {
		var path, raw string
		if err := rows.Scan(&path, &raw); err != nil {
			return nil, err
		}
		var c harness.Cursor
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return nil, fmt.Errorf("cursor %s %s: %w", h, path, err)
		}
		out[path] = c
	}
	return out, rows.Err()
}
func stamp(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// sessionSQL upserts a session. The WHERE clause skips a row the upsert would
// leave unchanged, so RowsAffected reports a visible change and a re-read of an
// untouched source does not wake readers.
const sessionSQL = `INSERT INTO sessions(harness,instance_id,session_id,parent_id,title,project,started_at,updated_at,raw_project) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(harness,instance_id,session_id) DO UPDATE SET parent_id=` + sessParent + `,title=` + sessTitle + `,project=` + sessProject + `,raw_project=` + sessRaw + `,started_at=` + sessStarted + `,updated_at=` + sessUpdated +
	` WHERE (sessions.parent_id,sessions.title,sessions.project,sessions.raw_project,sessions.started_at,sessions.updated_at) IS NOT (` + sessParent + `,` + sessTitle + `,` + sessProject + `,` + sessRaw + `,` + sessStarted + `,` + sessUpdated + `)`

const (
	sessParent  = `CASE WHEN excluded.parent_id!='' THEN excluded.parent_id ELSE sessions.parent_id END`
	sessTitle   = `CASE WHEN excluded.title!='' THEN excluded.title ELSE sessions.title END`
	sessProject = `CASE WHEN excluded.project!='' THEN excluded.project ELSE sessions.project END`
	sessRaw     = `CASE WHEN excluded.raw_project!='' THEN excluded.raw_project ELSE sessions.raw_project END`
	sessStarted = `CASE WHEN sessions.started_at=0 THEN excluded.started_at WHEN excluded.started_at=0 THEN sessions.started_at ELSE min(sessions.started_at,excluded.started_at) END`
	sessUpdated = `max(sessions.updated_at,excluded.updated_at)`
)

// Write couples a parsed source with its resolutions and checkpoint.
//
// InstanceID is additive: the empty string means the reserved default instance,
// so every pre-instance caller keeps its exact behaviour. A scanner that knows
// which configured instance a source belongs to sets it explicitly; the instance
// is a column, never a prefix or hash inside the dedup key or session id.
type Write struct {
	Harness     model.Harness
	InstanceID  string
	Path        string
	Batch       harness.Batch
	Resolutions []Resolution
}

func (s *Store) Commit(ctx context.Context, h model.Harness, path string, b harness.Batch, res []Resolution) error {
	return s.CommitMany(ctx, []Write{{Harness: h, Path: path, Batch: b, Resolutions: res}})
}

// CommitMany atomically persists a group of sources and their cursors.
func (s *Store) CommitMany(ctx context.Context, writes []Write) error {
	_, err := s.commitMany(ctx, writes, false, recentWindow{})
	return err
}

// CommitManyOutcome persists writes exactly like CommitMany and additionally
// reports, per write, what this commit inserted for the first time: see
// CommitOutcome. The report costs one indexed existence probe per flush and is
// only paid by callers that ask for it.
func (s *Store) CommitManyOutcome(ctx context.Context, writes []Write) (CommitOutcome, error) {
	return s.commitMany(ctx, writes, true, recentWindow{})
}

func (s *Store) commitMany(ctx context.Context, writes []Write, report bool, recent recentWindow) (CommitOutcome, error) {
	var outcome CommitOutcome
	if report {
		outcome.Writes = make([]WriteOutcome, len(writes))
	}
	s.writer.Lock()
	defer s.writer.Unlock()
	for _, w := range writes {
		for _, e := range w.Batch.Events {
			s.projects.probe(e.ProjectPath)
		}
		for _, m := range w.Batch.Sessions {
			s.projects.probe(m.Project)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CommitOutcome{}, err
	}
	defer tx.Rollback()
	prepared := make(map[string]*sql.Stmt)
	defer func() {
		for _, stmt := range prepared {
			stmt.Close()
		}
	}()
	prepare := func(query string) (*sql.Stmt, error) {
		if stmt := prepared[query]; stmt != nil {
			return stmt, nil
		}
		stmt, err := tx.PrepareContext(ctx, query)
		if err == nil {
			prepared[query] = stmt
		}
		return stmt, err
	}
	// changed counts the event and session rows the writes actually altered.
	var changed int64
	for i, w := range writes {
		if len(w.Batch.Events) > 0 && len(w.Resolutions) == 0 {
			if _, err := tx.ExecContext(ctx, "DELETE FROM settings WHERE key='pricing_fingerprint'"); err != nil {
				return CommitOutcome{}, err
			}
		}
		var stats *commitCollector
		if report {
			stats = &commitCollector{recent: recent}
		}
		if err := s.commit(ctx, w.Harness, w.InstanceID, w.Path, w.Batch, w.Resolutions, prepare, stats, &changed); err != nil {
			return CommitOutcome{}, err
		}
		if stats != nil {
			outcome.Writes[i] = WriteOutcome{
				NewEvents:             stats.newEvents,
				NewOutputTokens:       stats.newOutputTokens,
				NewRecentOutputTokens: stats.newRecentOutputTokens,
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return CommitOutcome{}, err
	}
	// Readers see usage and sessions, not cursors: a write that only moved a
	// cursor or re-read rows it already held (a SQLite source whose file was
	// touched without new requests) must not wake every subscriber into a
	// full refresh. Subscribe coalesces
	// the rest, so a burst of small commits still costs one refresh.
	if changed > 0 {
		s.notify()
	}
	return outcome, nil
}
func (s *Store) commit(ctx context.Context, h model.Harness, instance, path string, b harness.Batch, res []Resolution, prepare func(string) (*sql.Stmt, error), stats *commitCollector, changed *int64) error {
	if len(res) != 0 && len(res) != len(b.Events) {
		return errors.New("resolution count differs from event count")
	}
	instance = normalizeInstance(instance)
	var err error

	const row = `(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	const suffix = ` ON CONFLICT(harness,instance_id,dedup_key) DO UPDATE SET dimension_id=excluded.dimension_id,timestamp=excluded.timestamp,input=excluded.input,output=excluded.output,cache_read=excluded.cache_read,cache_write=excluded.cache_write,reasoning=excluded.reasoning,log_cost=excluded.log_cost,cost=excluded.cost,priced=excluded.priced,request_id=coalesce(nullif(excluded.request_id,''),event_data.request_id),boundary=coalesce(nullif(excluded.boundary,''),event_data.boundary),bill_amount=coalesce(excluded.bill_amount,event_data.bill_amount),bill_unit=CASE WHEN excluded.bill_amount IS NOT NULL THEN excluded.bill_unit ELSE event_data.bill_unit END WHERE (event_data.dimension_id,event_data.timestamp,event_data.input,event_data.output,event_data.cache_read,event_data.cache_write,event_data.reasoning,event_data.log_cost,event_data.cost,event_data.priced,event_data.request_id,event_data.boundary,event_data.bill_amount,event_data.bill_unit) IS NOT (excluded.dimension_id,excluded.timestamp,excluded.input,excluded.output,excluded.cache_read,excluded.cache_write,excluded.reasoning,excluded.log_cost,excluded.cost,excluded.priced,coalesce(nullif(excluded.request_id,''),event_data.request_id),coalesce(nullif(excluded.boundary,''),event_data.boundary),coalesce(excluded.bill_amount,event_data.bill_amount),CASE WHEN excluded.bill_amount IS NOT NULL THEN excluded.bill_unit ELSE event_data.bill_unit END) AND (SELECT old.session_id=new.session_id OR (old.parent_id!='' AND new.parent_id='') FROM event_dimensions old JOIN event_dimensions new ON new.id=excluded.dimension_id WHERE old.id=event_data.dimension_id)`
	const batchSize = 128
	var values []any
	// chunkKeys/chunkOutput run parallel to the events buffered in values, so a
	// flush can ask the index which of them it already held before the upsert.
	// counted remembers keys this write already reported, including repeats
	// inside a chunk the index did not hold yet.
	var chunkKeys []string
	var chunkOutput []int64
	// chunkTime runs parallel too, so a flush can split the output it reports by
	// the event's own timestamp without a second pass over the batch.
	var chunkTime []time.Time
	var counted map[string]bool
	if stats != nil {
		counted = map[string]bool{}
	}
	// Resolve each distinct dimension tuple once per parser batch, rather than
	// doing two dictionary lookups per event through the compatibility view.
	// Keep display identity metadata in the same transaction as facts/cursors.
	// Deduplicate lookups per batch, including endpoints we cannot identify.
	endpoints := map[string]bool{}
	ensureEndpoint := func(raw string) error {
		if endpoints[raw] {
			return nil
		}
		endpoint, ok := provider.NormalizeEndpoint(raw)
		if ok {
			id, _ := provider.EndpointID(raw)
			stmt, err := prepare(`INSERT INTO provider_endpoints(raw_endpoint,provider_id,endpoint) VALUES(?,?,?) ON CONFLICT(raw_endpoint) DO NOTHING`)
			if err != nil {
				return err
			}
			if _, err = stmt.ExecContext(ctx, raw, id, endpoint); err != nil {
				return err
			}
		}
		endpoints[raw] = true
		return nil
	}
	dimensions := map[[11]string]int64{}
	dimensionID := func(key [11]string) (int64, error) {
		if id, ok := dimensions[key]; ok {
			return id, nil
		}
		stmt, err := prepare(`INSERT INTO event_dimensions(` + dimensionColumns + `) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(` + dimensionColumns + `) DO UPDATE SET id=id RETURNING id`)
		if err != nil {
			return 0, err
		}
		var id int64
		err = stmt.QueryRowContext(ctx, key[0], key[1], key[2], key[3], key[4], key[5], key[6], key[7], key[8], key[9], key[10]).Scan(&id)
		if err == nil {
			dimensions[key] = id
		}
		return id, err
	}
	flush := func() error {
		if len(values) == 0 {
			return nil
		}
		if stats != nil {
			existing, err := existingKeys(ctx, prepare, h, instance, chunkKeys)
			if err != nil {
				return err
			}
			for i, key := range chunkKeys {
				if existing[key] || counted[key] {
					continue
				}
				counted[key] = true
				stats.newEvents++
				stats.newOutputTokens = addOutput(stats.newOutputTokens, chunkOutput[i])
				if stats.recent.eligible(chunkTime[i]) {
					stats.newRecentOutputTokens = addOutput(stats.newRecentOutputTokens, chunkOutput[i])
				}
			}
		}
		query := `INSERT INTO event_data(` + factColumns + `) VALUES ` + strings.TrimSuffix(strings.Repeat(row+",", len(values)/18), ",") + suffix
		stmt, e := prepare(query)
		if e == nil {
			var r sql.Result
			if r, e = stmt.ExecContext(ctx, values...); e == nil {
				if n, err := r.RowsAffected(); err == nil {
					*changed += n
				}
			}
		}
		values = values[:0]
		chunkKeys = chunkKeys[:0]
		chunkOutput = chunkOutput[:0]
		chunkTime = chunkTime[:0]
		return e
	}
	sessions, err := prepare(sessionSQL)
	if err != nil {
		return err
	}
	// The branch upsert is prepared lazily: a write whose events carry no usable
	// branch evidence pays nothing for the table, and the statement is still
	// bound to the same transaction as the facts it describes. Unusable or
	// absent branch text records nothing, so a malformed metadata field can
	// never drop a request or erase a branch a previous replay recorded.
	var branchStmt *sql.Stmt
	upsertBranch := func(harness, instance, key, branch string) error {
		branch, ok := branchEvidence(branch)
		if !ok {
			return nil
		}
		if branchStmt == nil {
			stmt, e := prepare(branchSQL)
			if e != nil {
				return e
			}
			branchStmt = stmt
		}
		_, e := branchStmt.ExecContext(ctx, harness, instance, key, branch)
		return e
	}
	upsert := func(m model.SessionMeta) error {
		if m.SessionID == "" {
			return nil
		}
		if m.Harness == "" {
			m.Harness = h
		}
		r, e := sessions.ExecContext(ctx, m.Harness, instance, m.SessionID, m.ParentID, harness.Title(m.Title), s.projects.resolve(m.Project), stamp(m.StartedAt), stamp(m.UpdatedAt), m.Project)
		if e == nil {
			if n, err := r.RowsAffected(); err == nil {
				*changed += n
			}
		}
		return e
	}
	meta := make(map[string]model.SessionMeta)
	collect := func(m model.SessionMeta) {
		if m.SessionID == "" {
			return
		}
		if m.Harness == "" {
			m.Harness = h
		}
		key := string(m.Harness) + "\x00" + instance + "\x00" + m.SessionID
		old, ok := meta[key]
		if !ok {
			meta[key] = m
			return
		}
		if m.ParentID != "" {
			old.ParentID = m.ParentID
		}
		if m.Title != "" {
			old.Title = m.Title
		}
		if m.Project != "" {
			old.Project = m.Project
		}
		if old.StartedAt.IsZero() || (!m.StartedAt.IsZero() && m.StartedAt.Before(old.StartedAt)) {
			old.StartedAt = m.StartedAt
		}
		if m.UpdatedAt.After(old.UpdatedAt) {
			old.UpdatedAt = m.UpdatedAt
		}
		meta[key] = old
	}
	for i, e := range b.Events {
		if e.Harness == "" {
			e.Harness = h
		}
		if e.DedupKey == "" || e.SessionID == "" || e.Harness != h {
			return fmt.Errorf("invalid event: harness/session/dedup key")
		}
		if err := ensureEndpoint(e.BaseURL); err != nil {
			return err
		}
		r := Resolution{Provider: e.Provider, Attrib: model.AttribLog}
		if e.CostUSD != nil {
			r.Cost = *e.CostUSD
		}
		if len(res) > 0 {
			r = res[i]
		}
		// Nonzero legacy resolutions are priced; explicit zero requires Priced.
		r.Priced = r.Priced || r.Cost != 0
		if e.CostUSD != nil {
			r.Cost = *e.CostUSD
			r.Priced = true
		}
		id, err := dimensionID([11]string{string(e.Harness), instance, e.SessionID, e.ParentID, s.projects.resolve(e.ProjectPath), e.Model, e.Provider, e.BaseURL, r.Provider, string(r.Attrib), e.ProjectPath})
		if err != nil {
			return err
		}
		var billAmount any
		billUnit := ""
		if e.Bill != nil {
			billAmount = e.Bill.Amount
			billUnit = e.Bill.Unit
		}
		values = append(values, e.Harness, instance, e.DedupKey, id, stamp(e.Timestamp), e.Tokens.Input, e.Tokens.Output, e.Tokens.CacheRead, e.Tokens.CacheWrite, e.Tokens.CacheWrite1h, e.Tokens.Reasoning, e.CostUSD, r.Cost, r.Priced, e.RequestID, e.Boundary, billAmount, billUnit)
		// The branch a log stated rides in the same transaction as its fact, so
		// metadata and usage commit or roll back together.
		if err = upsertBranch(string(e.Harness), instance, e.DedupKey, e.GitBranch); err != nil {
			return err
		}
		if stats != nil {
			chunkKeys = append(chunkKeys, e.DedupKey)
			chunkOutput = append(chunkOutput, e.Tokens.Output)
			chunkTime = append(chunkTime, e.Timestamp)
		}
		if len(values) == batchSize*18 {
			if err = flush(); err != nil {
				return err
			}
		}
		collect(model.SessionMeta{Harness: e.Harness, SessionID: e.SessionID, ParentID: e.ParentID, Project: e.ProjectPath, StartedAt: e.Timestamp, UpdatedAt: e.Timestamp})
		if e.ParentID != "" {
			collect(model.SessionMeta{Harness: e.Harness, SessionID: e.ParentID, Project: e.ProjectPath, StartedAt: e.Timestamp, UpdatedAt: e.Timestamp})
		}
	}
	if err = flush(); err != nil {
		return err
	}
	for _, m := range b.Sessions {
		collect(m)
	}
	for _, m := range meta {
		if err = upsert(m); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(b.Next)
	if err != nil {
		return err
	}
	cursor, err := prepare("INSERT INTO cursors(harness,instance_id,path,cursor) VALUES(?,?,?,?) ON CONFLICT(harness,instance_id,path) DO UPDATE SET cursor=excluded.cursor")
	if err != nil {
		return err
	}
	_, err = cursor.ExecContext(ctx, h, instance, path, string(raw))
	return err
}

// Rebuild resets checkpoints for a full replay, preserving historical events whose
// source logs may have been rotated away. Replayed events are idempotent upserts.
func (s *Store) Rebuild(ctx context.Context) error {
	s.writer.Lock()
	defer s.writer.Unlock()
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, "DELETE FROM cursors;"); e != nil {
		return e
	}
	// A full replay is the maintenance point for the additive branch metadata:
	// rows whose request no longer exists are dropped before the rescan, which
	// then rewrites the branch of every event it can still see.
	if _, e = pruneEventBranches(ctx, tx); e != nil {
		return e
	}
	if e = tx.Commit(); e == nil {
		s.projects = newProjectResolver()
		s.notify()
	}
	return e
}
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	e := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", key).Scan(&v)
	if errors.Is(e, sql.ErrNoRows) {
		e = nil
	}
	return v, e
}
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	return s.Exec(ctx, "INSERT INTO settings VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
}

// Subscribe is a coalescing commit signal. Query service applies its one-second throttle.
func (s *Store) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	if s.closed {
		close(ch)
	} else {
		s.subscribers[ch] = struct{}{}
	}
	s.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			if _, ok := s.subscribers[ch]; ok {
				delete(s.subscribers, ch)
				close(ch)
			}
			s.mu.Unlock()
		})
	}
}
func (s *Store) notify() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
