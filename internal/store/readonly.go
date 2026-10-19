package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	modernsqlite "modernc.org/sqlite"

	"github.com/zzstar101/mytoken/internal/sqlitedsn"
)

// This file is the read-only half of the store: it opens the existing index
// without performing any application write - no bootstrap DDL, no migration, no
// reprice, no rollup rebuild, no marker write, no backup, no network. The
// read-write Open (store.go) is deliberately not reused, because it runs
// bootstrap DDL, the schema migrations, EnsureLocalDays and NormalizeProjects on
// every call.
//
// The contract, stated exactly:
//
//  1. No application writes. The connection is opened with mode=ro and
//     query_only(1), so no statement can insert, update, delete or run DDL.
//     Nothing here creates a database or a parent directory, migrates a schema,
//     reprices a row, writes a marker or a backup, or touches the network.
//  2. No committed WAL frames are ever hidden. immutable=1 is never used: it
//     reports an empty database for a WAL index whose frames are still only in
//     the -wal file. A read-only handle sees the writer's committed frames and
//     keeps seeing frames committed while it stays open.
//  3. One consistent read transaction per query, so a snapshot can never pair
//     today's totals with yesterday's cache numbers.
//
// "No application writes" is not the same as "no filesystem bytes change".
// SQLite coordinates readers through the WAL sidecars, and that bookkeeping is
// part of opening a WAL database read-only:
//
//   - If the database is in WAL format and its -shm sidecar is absent - the
//     usual state of a cleanly closed WAL index - attaching read-only creates
//     <db>-shm and an empty <db>-wal. Measured on darwin/arm64 with
//     modernc.org/sqlite; no DSN or VFS avoids it (immutable/nolock hide
//     committed frames, unix-none refuses to open at all, locking_mode=EXCLUSIVE
//     fails with SQLITE_IOERR). Those sidecars may outlive the read.
//   - If the -shm sidecar already exists, attaching records a wal-index read
//     mark inside it: a change to shared memory, bounded to the read-mark slot
//     (byte 104 onward). The database file and the -wal frames stay
//     byte-identical.
//
// Both are SQLite's transient coordination bookkeeping, not index data. The
// database file and every committed WAL frame are never modified, no file is
// ever deleted, and no chmod, source copy or writable connection is involved.
// A caller that needs a strict "the file set is unchanged" guarantee cannot get
// it from SQLite while reading a WAL database; that is why this entry point
// discloses the sidecar effects instead of refusing ordinary cross-process
// reads.
//

// readOnlyBusyTimeoutMS bounds how long a read-only open waits on a lock before
// it gives up with a typed error, so contention can never hang a UI or a
// statusline. It is a variable so tests can shorten the wait.
var readOnlyBusyTimeoutMS = 5000

// SQLite result-code constants. They are spelled out here instead of imported
// from modernc.org/sqlite/lib so the store keeps exactly the module
// requirements it already has.
const (
	sqliteCodePerm    = 3  // SQLITE_PERM
	sqliteCodeBusy    = 5  // SQLITE_BUSY
	sqliteCodeLocked  = 6  // SQLITE_LOCKED
	sqliteCodeCorrupt = 11 // SQLITE_CORRUPT
	sqliteCodeNotADB  = 26 // SQLITE_NOTADB
)

// SchemaState classifies the on-disk schema generation of an existing index, as
// seen by a reader that is not allowed to migrate it.
type SchemaState string

const (
	// SchemaOK means every required marker is present with a value this build
	// knows, and the objects that value promises are actually there.
	SchemaOK SchemaState = "ok"
	// SchemaUpgradeRequired means the index predates a generation this build
	// writes, or is inconsistent with its own markers. A normal writable startup
	// (mytoken scan) migrates it; a read-only open never does, and mytoken
	// doctor is itself read-only so it cannot either.
	SchemaUpgradeRequired SchemaState = "upgrade_required"
	// SchemaNewer means a marker holds a value this build does not know: the
	// index was written by a newer MyToken. Never guess at such a schema.
	SchemaNewer SchemaState = "newer"
	// SchemaUnreadable means the schema generation could not be decided at all.
	// It is reported as the Schema.State of a typed refusal whose underlying
	// failure was an unreadable catalog, and never means "read it anyway".
	SchemaUnreadable SchemaState = "unreadable"
)

// schemaStateRank orders states from safest to least usable, so a report can
// keep the most serious finding when several markers disagree.
func schemaStateRank(state SchemaState) int {
	switch state {
	case SchemaOK:
		return 0
	case SchemaUpgradeRequired:
		return 1
	case SchemaNewer:
		return 2
	default:
		return 3
	}
}

func worseSchemaState(a, b SchemaState) SchemaState {
	if schemaStateRank(b) > schemaStateRank(a) {
		return b
	}
	return a
}

// SchemaReport is the read-only verdict on an index's schema generation. It
// carries the raw markers it saw so a caller can quote them instead of
// guessing, plus the marker mismatches that produced a non-ok state.
type SchemaReport struct {
	State   SchemaState       `json:"state"`
	Markers map[string]string `json:"markers,omitempty"`
	Missing []string          `json:"missing,omitempty"`
	Detail  string            `json:"detail,omitempty"`
}

// ReadOnlyReason is the machine-readable reason a read-only open refused.
type ReadOnlyReason string

const (
	// ReasonMissingDatabase means the file does not exist. It is never treated
	// as an empty index.
	ReasonMissingDatabase ReadOnlyReason = "missing_database"
	// ReasonNotRegularFile means the path exists but is not a regular file.
	ReasonNotRegularFile ReadOnlyReason = "not_regular_file"
	// ReasonPermissionDenied means the process may not read the database or its
	// sidecars.
	ReasonPermissionDenied ReadOnlyReason = "permission_denied"
	// ReasonCorrupt means SQLite reported the file as not a database or as a
	// malformed disk image.
	ReasonCorrupt ReadOnlyReason = "corrupt"
	// ReasonLocked means another process held a lock past the bounded wait.
	ReasonLocked ReadOnlyReason = "locked"
	// ReasonSchemaUpgradeRequired, ReasonSchemaNewer and ReasonSchemaUnreadable
	// mirror the non-ok SchemaState values.
	ReasonSchemaUpgradeRequired ReadOnlyReason = "schema_upgrade_required"
	ReasonSchemaNewer           ReadOnlyReason = "schema_newer"
	ReasonSchemaUnreadable      ReadOnlyReason = "schema_unreadable"
	// ReasonUnavailable is the catch-all for a failure whose exact class cannot
	// be established. It still means "do not read this index", never "read it
	// as empty".
	ReasonUnavailable ReadOnlyReason = "unavailable"
)

// ReadOnlyError is the typed refusal of OpenReadOnly / InspectSchemaReadOnly.
type ReadOnlyError struct {
	Reason ReadOnlyReason
	Path   string
	// Schema is the classification that was reached, when one was. Its Markers
	// field is non-nil exactly when classification ran.
	Schema SchemaReport
	Detail string
	// Err is the underlying failure, if any.
	Err error
}

func (e *ReadOnlyError) Error() string {
	var b strings.Builder
	b.WriteString("the index at ")
	if e.Path == "" {
		b.WriteString("(no path)")
	} else {
		b.WriteString(e.Path)
	}
	b.WriteString(" is not readable read-only [")
	b.WriteString(string(e.Reason))
	b.WriteString("]")
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	return b.String()
}

func (e *ReadOnlyError) Unwrap() error { return e.Err }

// Is makes errors.Is work against the sentinel errors below by comparing the
// reason, so callers can branch on a class without string matching.
func (e *ReadOnlyError) Is(target error) bool {
	t, ok := target.(*ReadOnlyError)
	return ok && t != nil && t.Reason == e.Reason
}

// Sentinels for errors.Is(err, ErrReadOnlyMissingDatabase) style branching.
var (
	ErrReadOnlyMissingDatabase  = &ReadOnlyError{Reason: ReasonMissingDatabase}
	ErrReadOnlyNotRegularFile   = &ReadOnlyError{Reason: ReasonNotRegularFile}
	ErrReadOnlyPermissionDenied = &ReadOnlyError{Reason: ReasonPermissionDenied}
	ErrReadOnlyCorrupt          = &ReadOnlyError{Reason: ReasonCorrupt}
	ErrReadOnlyLocked           = &ReadOnlyError{Reason: ReasonLocked}
	ErrReadOnlySchemaUpgrade    = &ReadOnlyError{Reason: ReasonSchemaUpgradeRequired}
	ErrReadOnlySchemaNewer      = &ReadOnlyError{Reason: ReasonSchemaNewer}
	ErrReadOnlySchemaUnreadable = &ReadOnlyError{Reason: ReasonSchemaUnreadable}
	ErrReadOnlyUnavailable      = &ReadOnlyError{Reason: ReasonUnavailable}
)

// ReadOnlyReasonOf reports the typed read-only refusal reason carried by err.
func ReadOnlyReasonOf(err error) (ReadOnlyReason, bool) {
	var roErr *ReadOnlyError
	if errors.As(err, &roErr) && roErr != nil {
		return roErr.Reason, true
	}
	return "", false
}

// ReadOnly reports whether this handle came from OpenReadOnly. Such a handle
// performs no application write: it never creates, migrates, reprices, rolls up
// or deletes anything. SQLite's own WAL read coordination may still touch the
// -shm/-wal sidecars; see the file comment.
func (s *Store) ReadOnly() bool { return s.readOnly }

// ReadOnlySchema returns the schema report captured when the read-only handle
// was opened. It is the zero report on a handle from Open.
func (s *Store) ReadOnlySchema() SchemaReport { return s.schema }

// readOnlyMarker is one schema-generation marker the reader requires, the value
// this build writes, and an optional shape check that the objects the marker
// promises really exist. The table is data-driven so a new generation adds one
// row instead of a new branch.
type readOnlyMarker struct {
	key     string
	current string
	// absenceIsOK marks a marker a healthy index may legitimately not carry yet,
	// because a lazy derived-cache pass writes it (EnsurePrices) rather than the
	// migration. Its absence is reported but is not an unsupported schema; any
	// other value is still "a newer build wrote this".
	absenceIsOK bool
	check       func(*Store, context.Context, string) error
}

var readOnlyMarkers = []readOnlyMarker{
	{key: providerIdentityVersion, current: "1", check: (*Store).checkProviderIndex},
	{key: providerRollupVersion, current: "1", check: (*Store).checkProviderRollupIndex},
	{key: instanceVersionKey, current: "1", check: (*Store).checkInstanceSchema},
}

// OpenReadOnly opens path through an independent, read-only SQLite connection
// that performs no application write: no DDL, no migration, no reprice, no
// marker, no backup. The returned handle is a normal *Store for readers -
// query.NewService accepts it unchanged - but every write path fails, because
// the connection is opened with mode=ro and query_only(1).
//
// It refuses, with a *ReadOnlyError, whenever the index cannot be read: a
// missing database or parent directory (neither is ever created), a path that is
// not a regular file, an unreadable file, a schema that needs an upgrade or was
// written by a newer build, a corrupt file, or a lock held past
// readOnlyBusyTimeoutMS.
//
// Opening a WAL database read-only is not byte-for-byte passive: SQLite creates
// <db>-shm and an empty <db>-wal when the -shm sidecar is absent, and records a
// wal-index read mark in an existing -shm. That is SQLite's own reader
// coordination, not index data; the database file and committed WAL frames are
// never modified. See the file comment for the measured details. This works for
// a database held open by a writer in another process, and for a cleanly closed
// WAL database, which is what makes snapshot/statusline/report/doctor usable.
func OpenReadOnly(ctx context.Context, path string) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(path) == "" {
		return nil, &ReadOnlyError{
			Reason: ReasonMissingDatabase,
			Path:   path,
			Detail: "no database path was given; a read-only open never creates a default one",
		}
	}
	if err := preflightReadOnly(path); err != nil {
		return nil, err
	}
	db, err := openReadOnlyDB(ctx, path)
	if err != nil {
		return nil, err
	}
	st := &Store{
		db:          db,
		subscribers: make(map[chan struct{}]struct{}),
		readOnly:    true,
	}
	report, err := st.classifySchema(ctx)
	if err != nil {
		st.db.Close()
		return nil, readOnlyErrorFrom(path, report, err)
	}
	st.schema = report
	if report.State != SchemaOK {
		st.db.Close()
		return nil, schemaRefusal(path, report)
	}
	return st, nil
}

// InspectSchemaReadOnly classifies the schema generation of an existing index
// without handing back a query handle. It applies exactly the same rules as
// OpenReadOnly, so when the index is readable the returned report is the one
// OpenReadOnly would have stored; when it is not, the error is the same typed
// refusal and its Schema field carries the report that was reached.
func InspectSchemaReadOnly(ctx context.Context, path string) (SchemaReport, error) {
	st, err := OpenReadOnly(ctx, path)
	if err == nil {
		defer st.Close()
		return st.ReadOnlySchema(), nil
	}
	var roErr *ReadOnlyError
	if errors.As(err, &roErr) && roErr.Schema.Markers != nil {
		return roErr.Schema, err
	}
	return SchemaReport{}, err
}

// preflightReadOnly checks everything that can be checked with pure reads, so
// an obviously unreadable index is refused before any SQLite connection exists.
// It never creates or changes anything, and it never inspects the sidecars: the
// sidecars are SQLite's own bookkeeping, not a precondition for reading.
func preflightReadOnly(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &ReadOnlyError{
				Reason: ReasonMissingDatabase,
				Path:   path,
				Detail: "the database file does not exist; a read-only open never creates it",
				Err:    err,
			}
		}
		if errors.Is(err, fs.ErrPermission) {
			return &ReadOnlyError{Reason: ReasonPermissionDenied, Path: path, Detail: err.Error(), Err: err}
		}
		return &ReadOnlyError{Reason: ReasonUnavailable, Path: path, Detail: err.Error(), Err: err}
	}
	if !info.Mode().IsRegular() {
		return &ReadOnlyError{
			Reason: ReasonNotRegularFile,
			Path:   path,
			Detail: fmt.Sprintf("the path is a %s, not a regular file", describeMode(info.Mode())),
		}
	}
	// A pure read-only probe: it makes an unreadable file a permission error
	// here instead of an ambiguous SQLITE_CANTOPEN later.
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return &ReadOnlyError{Reason: ReasonPermissionDenied, Path: path, Detail: err.Error(), Err: err}
		}
		return &ReadOnlyError{Reason: ReasonUnavailable, Path: path, Detail: err.Error(), Err: err}
	}
	return f.Close()
}

// openReadOnlyDB opens the connection itself. No journal_mode pragma is sent:
// a read-only connection must not try to switch modes, and the file already
// knows whether it is WAL.
func openReadOnlyDB(ctx context.Context, path string) (*sql.DB, error) {
	dsn := sqlitedsn.URI(path, fmt.Sprintf("mode=ro&_pragma=busy_timeout(%d)&_pragma=query_only(1)", readOnlyBusyTimeoutMS))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, &ReadOnlyError{Reason: ReasonUnavailable, Path: path, Detail: err.Error(), Err: err}
	}
	db.SetMaxOpenConns(1)
	// Ping opens the file, and the schema_version read forces SQLite to parse
	// the header, so both "not a database" and "malformed image" surface here.
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, readOnlyErrorFrom(path, SchemaReport{}, err)
	}
	var version int64
	if err := db.QueryRowContext(ctx, "PRAGMA schema_version").Scan(&version); err != nil {
		db.Close()
		return nil, readOnlyErrorFrom(path, SchemaReport{}, err)
	}
	return db, nil
}

// classifySchema reads the marker set and the object shapes it promises, and
// turns them into a SchemaReport. It performs reads only: it never calls a
// prepare* migration helper, which could write a backup file.
func (s *Store) classifySchema(ctx context.Context) (SchemaReport, error) {
	report := SchemaReport{Markers: map[string]string{}, State: SchemaOK}

	settingsKind, err := s.objectKind(ctx, "settings")
	if err != nil {
		report.State = SchemaUnreadable
		return report, err
	}
	if settingsKind != "table" {
		report.State = SchemaUpgradeRequired
		report.Missing = append(report.Missing, "settings")
		report.Detail = "the settings table is missing, so this index predates the marker-based schema generations; " +
			"run `mytoken scan` once - a normal writable startup - to migrate it (mytoken doctor is read-only and cannot)"
		return report, nil
	}
	eventsKind, err := s.objectKind(ctx, "events")
	if err != nil {
		report.State = SchemaUnreadable
		return report, err
	}
	if eventsKind == "" {
		report.State = SchemaUpgradeRequired
		report.Missing = append(report.Missing, "events")
		report.Detail = "the events read surface is missing; run `mytoken scan` once - a normal writable startup - to rebuild it"
		return report, nil
	}

	var problems []string
	for _, marker := range readOnlyMarkers {
		value, err := s.Setting(ctx, marker.key)
		if err != nil {
			report.State = SchemaUnreadable
			return report, err
		}
		report.Markers[marker.key] = value
		switch {
		case value == marker.current:
			if marker.check == nil {
				continue
			}
			if err := marker.check(s, ctx, value); err != nil {
				report.State = worseSchemaState(report.State, SchemaUpgradeRequired)
				report.Missing = append(report.Missing, marker.key)
				problems = append(problems, err.Error())
			}
		case value == "":
			if marker.absenceIsOK {
				continue
			}
			report.State = worseSchemaState(report.State, SchemaUpgradeRequired)
			report.Missing = append(report.Missing, marker.key)
			problems = append(problems, fmt.Sprintf("the migration marker %s is absent", marker.key))
		default:
			report.State = worseSchemaState(report.State, SchemaNewer)
			problems = append(problems, fmt.Sprintf("the migration marker %s holds %q, a value this build does not know", marker.key, value))
		}
	}
	report.Detail = strings.Join(problems, "; ")
	return report, nil
}

// schemaRefusal turns a non-ok report into its typed error.
func schemaRefusal(path string, report SchemaReport) error {
	reason := ReasonUnavailable
	switch report.State {
	case SchemaUpgradeRequired:
		reason = ReasonSchemaUpgradeRequired
	case SchemaNewer:
		reason = ReasonSchemaNewer
	case SchemaUnreadable:
		reason = ReasonSchemaUnreadable
	}
	return &ReadOnlyError{Reason: reason, Path: path, Schema: report, Detail: report.Detail}
}

// readOnlyErrorFrom classifies a raw SQLite failure into a typed refusal.
func readOnlyErrorFrom(path string, report SchemaReport, err error) error {
	var roErr *ReadOnlyError
	if errors.As(err, &roErr) && roErr != nil {
		if roErr.Schema.Markers == nil {
			roErr.Schema = report
		}
		return roErr
	}
	return &ReadOnlyError{
		Reason: readOnlyReasonFromSQLite(err),
		Path:   path,
		Schema: report,
		Detail: err.Error(),
		Err:    err,
	}
}

// readOnlyReasonFromSQLite maps a SQLite result code onto a typed reason.
// Anything unrecognized is unavailable, never a silent empty read.
func readOnlyReasonFromSQLite(err error) ReadOnlyReason {
	var sqliteErr *modernsqlite.Error
	if errors.As(err, &sqliteErr) && sqliteErr != nil {
		switch sqliteErr.Code() {
		case sqliteCodeNotADB, sqliteCodeCorrupt:
			return ReasonCorrupt
		case sqliteCodeBusy, sqliteCodeLocked:
			return ReasonLocked
		case sqliteCodePerm:
			return ReasonPermissionDenied
		}
	}
	return ReasonUnavailable
}

func describeMode(mode fs.FileMode) string {
	switch {
	case mode.IsDir():
		return "directory"
	case mode&fs.ModeSymlink != 0:
		return "symlink"
	case mode&fs.ModeNamedPipe != 0:
		return "named pipe"
	case mode&fs.ModeSocket != 0:
		return "socket"
	case mode&fs.ModeDevice != 0:
		return "device"
	default:
		return "special file"
	}
}
