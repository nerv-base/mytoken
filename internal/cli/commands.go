package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/app"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/paths"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/source"
	"github.com/zzstar101/mytoken/internal/store"
)

// Version is set at build time with -ldflags -X.
var Version = "dev"

const usage = `Usage: mytoken <command> [options]
  scan
  stats [options] [--by session|provider|endpoint|model|project|day|harness]
  cache [options] [--by cause|provider|model|harness|project|session|day]
  cache <session-id> [--harness NAME] [options]
  sessions [--limit N] [options]
  sessions <session-id> [--harness NAME] [--instance-id ID] [options]
  provider list [options]
  provider alias set <provider-id> <label>
  provider alias clear <provider-id>
  sources list [--json]
  sources add --harness NAME --root PATH [--root PATH ...] [--id ID] [--label TEXT]
              [--kind instance|backup-copy] [--disabled] [--json]
  sources move <id> --root PATH [--root PATH ...] [--json]
  sources remove <id> [--json]
  sources promote <id> [--json]
  snapshot --once [--json] [--refresh] [options]
  statusline [--json] [--session ID] [--harness NAME] [options]
  report [day|week|month] [--timezone TZ] [--json] [options]
  quota list [--instance-id ID] [--format table|json|csv] [--json]
  quota status [INSTANCE] [--instance-id ID] [--format table|json|csv] [--json]
  doctor [--json]
  version
  help
  prices [list|import-ccswitch]
  prices set --provider NAME [--model NAME] [--multiplier N] [--input N]
             [--output N] [--cache-read N] [--cache-write N]
  relay list [--format table|json|csv] [--json] [--offline]
  relay enable <origin|provider> [--layers ratio,balance,bills] [--key-id ID]
  relay disable <origin|provider> [--key-id ID]
  relay sync [<origin|provider>] [--key-id ID]
  reconcile [<origin|provider>] [--by category|model|day] [options]

options for stats, sessions and cache:
  --since DATE|DUR    range start (stats and cache default 7d; sessions and
                      provider list default all time)
  --until DATE|DUR    range end, not included
  --last WINDOW       today|week|month or 7d/2w/3m/12h/30s (ends now);
                      exclusive with --since/--until
  --provider NAME     filter by original provider name (repeatable); this is the
                      resolved pricing name, unchanged
  --provider-id ID    filter by stable endpoint identity key (repeatable); get
                      keys from "provider list". Unknown or malformed ids match
                      nothing. --provider and --provider-id intersect.
  --instance-id ID    filter by instance id from sources.json (repeatable);
                      stats, sessions, cache and provider list intersect them,
                      and an unknown id matches nothing. A session drilldown
                      takes at most one, because it reads one instance's copy.
  --timezone TZ       IANA zone for date parsing and --by day (default local)
  --offline           never use the network (default: refresh prices first)
  --no-cost           leave out cost columns and costUsd fields
  --format FMT        table (default), json or csv
  --json              alias for --format json
(provider list accepts the same options)

DATE is YYYY-MM-DD, YYYYMMDD or RFC3339. DUR is Ns, Nh, Nd, Nw or Nm
(Nm is a calendar month; minutes are written 90s).
stats defaults: --since 7d --by session
--by provider keeps the original pricing-name grouping; --by endpoint groups by
stable endpoint identity instead (the same rows as "provider list").
cache defaults: --since 7d --by cause; a session id prints that session's
cache timeline instead (whole session; no --by, no --provider/--provider-id).
sessions defaults: --limit 50 (0 means all); most recently updated first.
"sessions <session-id>" prints that one session (the same row shape as the list)
plus its child sessions and its own events; --harness picks the owner when the id
exists under several harnesses, and --instance-id picks the data source when the
same session was read from more than one. Both are optional when unambiguous.
sources reads and edits sources.json in the data directory: the shared instance
configuration every reader of the index agrees on. list is read-only and does
not create the data directory; the other subcommands read-modify-write the file
under a lock, keep it mode 0600 and never leave a partial file behind (a
malformed config is refused, never overwritten). add/move require an existing
directory for an enabled instance; a backup-copy (or --disabled) entry keeps its
roots verbatim and is never scanned or counted, and promote is the only way it
becomes scannable. ids are opaque and survive move/promote. remove deletes the
configuration entry only - no database, cursor, log or directory is deleted.
sources never opens the database, never refreshes prices and never reads a log.
provider list prints each stable endpoint identity (key, display label, whether
it was recognized, and its harness/provider members). It always reads the local
index only. "provider alias set/clear" changes only the display label, never the
key, attribution, cost or the pricing fingerprint; labels are display text and
may repeat, so select with --provider-id, never by label. Endpoints whose raw
URL carries a query string are listed as unidentified and cannot be aliased.
relay list, reconcile and cache never use the network; relay enable and relay
sync do, and refuse --offline. A relay site stays off until you enable it, and
its key is never stored or printed - only its key ID (a truncated hash) appears.
quota reads the persisted Codex quota observations offline and read-only: the
stored keys are enumerated with readonly SQL from the index, never from a
running scanner, and nothing is created, refreshed or written. It needs no
credential and no network. Instances are never combined - two instances are two
accounts with their own credit units - and an instance with nothing stored is
reported unavailable, never hidden. A window, reset or pace appears only when
the stored observation supports it; a pace stays unavailable with a reason,
because one stored observation per instance cannot yield a two-point pace.
Exit status: 0 success, 1 operational error, 2 invalid arguments.
`

// cacheUsage is the cache-specific help, shown on a usage error.
const cacheUsage = `Usage: mytoken cache [--by cause|provider|model|harness|project|session|day] [options]
       mytoken cache <session-id> [--harness NAME] [options]

--by defaults to cause (the miss breakdown); day groups by local day, and the
other values group the ledger by that dimension. --by provider adds a HEALTH
column: the site's unexplained misses over the tokens it should have cached.
A session id switches to that session's cache timeline (time, model, prompt,
read, write, expected, miss, cause, miss cost, boundary). The timeline ignores
the range flags and covers the whole session, and it has no --by; --harness
picks the owner when the same session id exists under several harnesses.
--provider and --provider-id narrow the ledger views by original provider name
or stable endpoint identity key (see "mytoken provider list"); the timeline
rejects both, because it reads one session directly and has no filter to narrow.
The ledger comes from local data only: cache never uses the network, with or
without --offline.
See "mytoken help" for --since/--until/--last/--timezone/--no-cost/--format.
`

// sessionsUsage is the sessions-specific help, shown on a usage error.
const sessionsUsage = `Usage: mytoken sessions [--limit N] [--harness NAME] [--instance-id ID] [options]
       mytoken sessions <session-id> [--harness NAME] [--instance-id ID] [options]

--limit caps the list (default 50; 0 means all) and is only valid for the list.
A session id switches to that session's drilldown: the session row, its child
sessions and its own attributed events. --harness picks the owning harness and
--instance-id the owning data source when the id is not unique; with no selector
they are resolved from the index, and an ambiguous id is an error rather than an
arbitrary pick. The drilldown has no --limit, rejects --provider/--provider-id
(it reads one session directly, so it has no list to narrow), and covers the
whole session, so it ignores the range flags (--since/--until/--last/--timezone).
See "mytoken help" for the shared options.
`

func runSessions(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	fs.SetOutput(errout)
	limit := fs.Int("limit", 50, "maximum sessions (0 for all; list only)")
	harnessFlag := fs.String("harness", "", "harness owning the session (drilldown only)")
	var cf commonFlags
	addCommonFlags(fs, &cf, "")
	flags, positional, splitErr := splitArgs(args, map[string]bool{"offline": true, "no-cost": true, "json": true})
	if splitErr != nil {
		fmt.Fprintln(errout, splitErr)
		return 2
	}
	if err := cf.parseFlags(fs, flags); err != nil {
		return 2
	}
	if len(positional) > 1 {
		fmt.Fprint(errout, sessionsUsage)
		return 2
	}
	harnessName := strings.TrimSpace(*harnessFlag)
	if harnessName != "" && !knownHarness(harnessName) {
		fmt.Fprintf(errout, "invalid --harness %q (want a harness id such as claude-code, codex or dsh)\n", harnessName)
		return 2
	}
	session := ""
	if len(positional) == 1 {
		session = positional[0]
		if cf.set["limit"] {
			fmt.Fprintln(errout, "sessions: --limit cannot be combined with a session id")
			return 2
		}
		// A drilldown reads one instance's copy of the session; intersecting
		// several copies is not a view this command can print, so it is a usage
		// error instead of a silently narrowed-or-merged read.
		if len(cf.instanceIDs) > 1 {
			fmt.Fprintln(errout, "sessions: a session drilldown reads one instance; pass at most one --instance-id")
			return 2
		}
		// The drilldown resolves one session directly, so a provider filter has
		// no list to narrow; rejecting it keeps --provider from being applied to
		// the harness lookup only and quietly dropped from the read itself.
		if flags := providerFilterFlags(&cf); flags != "" {
			fmt.Fprintf(errout, "sessions: %s cannot be combined with a session id\n", flags)
			return 2
		}
	} else {
		if *limit < 0 {
			fmt.Fprint(errout, sessionsUsage)
			return 2
		}
		if harnessName != "" {
			fmt.Fprintln(errout, "sessions: --harness is only valid with a session id")
			return 2
		}
	}
	opts, err := cf.resolve(time.Now())
	if err != nil {
		fmt.Fprintln(errout, err)
		return 2
	}
	return runInLocation(opts.loc, func() int {
		if session != "" {
			return sessionsDrilldown(ctx, opts, harnessName, session, out, errout)
		}
		return sessionsCommand(ctx, opts, *limit, out, errout)
	})
}

// sessionDetailReport is the JSON shape of a session drilldown.
type sessionDetailReport struct {
	Session  query.SessionRow        `json:"session"`
	Children []query.SessionRow      `json:"children"`
	Events   []query.AttributedEvent `json:"events"`
}

// sessionsDrilldown prints one session, its children and its own events. A
// session id is unique per (harness, instance), so an instance-less read of an
// id that two data sources both hold is an error, never an arbitrary pick.
func sessionsDrilldown(ctx context.Context, opts options, harnessName, session string, out, errout io.Writer) int {
	a, err := openApp(opts.offline)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	defer a.Close()
	h := model.Harness(harnessName)
	if h == "" {
		rows, _, e := a.Query.Sessions(ctx, opts.filter(), query.SortRecent, 0, 0)
		if e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		var found []string
		for _, r := range rows {
			if r.SessionID == session {
				found = append(found, string(r.Harness))
			}
		}
		switch len(found) {
		case 0:
			fmt.Fprintf(errout, "sessions: no session %q in the index; pass --harness <name> to read it by harness\n", session)
			return 1
		case 1:
			h = model.Harness(found[0])
		default:
			fmt.Fprintf(errout, "sessions: session %q appears under %d harnesses; pass --harness to pick one:\n", session, len(found))
			for _, name := range found {
				fmt.Fprintf(errout, "  %s\n", name)
			}
			return 1
		}
	}
	var row query.SessionRow
	var children []query.SessionRow
	var events []query.AttributedEvent
	if len(opts.instanceIDs) == 1 {
		row, children, events, err = query.SessionIn(ctx, a.Query, h, opts.instanceIDs[0], session)
	} else {
		row, children, events, err = a.Query.Session(ctx, h, session)
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			fmt.Fprintf(errout, "sessions: no session %q for harness %s\n", session, h)
			return 1
		}
		fmt.Fprintln(errout, err)
		return 1
	}
	if children == nil {
		children = []query.SessionRow{}
	}
	if events == nil {
		events = []query.AttributedEvent{}
	}
	rows := append([]query.SessionRow{row}, children...)
	switch opts.format {
	case formatJSON:
		var payload any = sessionDetailReport{Session: row, Children: children, Events: events}
		if opts.noCost {
			if payload, err = stripCost(payload); err != nil {
				fmt.Fprintln(errout, err)
				return 1
			}
		}
		err = json.NewEncoder(out).Encode(payload)
	case formatCSV:
		err = writeSessionsCSV(out, rows, opts.noCost)
	default:
		if err = writeSessionsTable(out, rows, opts.noCost); err == nil {
			_, err = fmt.Fprintf(out, "Events: %d\n", len(events))
		}
	}
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	return 0
}

func sessionsCommand(ctx context.Context, opts options, limit int, out, errout io.Writer) int {
	a, err := openApp(opts.offline)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	defer a.Close()
	rows, total, err := a.Query.Sessions(ctx, opts.filter(), query.SortRecent, limit, 0)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	if rows == nil {
		rows = []query.SessionRow{}
	}
	switch opts.format {
	case formatJSON:
		var payload any = sessionsReport{Total: total, Sessions: rows}
		if opts.noCost {
			if payload, err = stripCost(payload); err != nil {
				fmt.Fprintln(errout, err)
				return 1
			}
		}
		err = json.NewEncoder(out).Encode(payload)
	case formatCSV:
		err = writeSessionsCSV(out, rows, opts.noCost)
	default:
		err = writeSessionsTable(out, rows, opts.noCost)
	}
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	return 0
}

type doctorRoot struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
}
type doctorHarness struct {
	Harness string       `json:"harness"`
	Roots   []doctorRoot `json:"roots"`
	Sources int64        `json:"sources"`
	Events  int64        `json:"events"`
}

// doctorInstance is one resolved data source with its index coverage, so a
// config entry that is silently unscanned is visible as 0 sources/0 events.
type doctorInstance struct {
	sourceEntry
	Sources int64 `json:"sources"`
	Events  int64 `json:"events"`
}

// sourceEntries unwraps the embedded source entry from the doctor rows so the
// shared diagnostics renderer can take them without duplicating its logic.
func sourceEntries(rows []doctorInstance) []sourceEntry {
	out := make([]sourceEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.sourceEntry)
	}
	return out
}

type doctorReport struct {
	DataDir           string          `json:"dataDir"`
	DBPath            string          `json:"dbPath"`
	DBBytes           int64           `json:"dbBytes"`
	PricingSource     string          `json:"pricingSource"`
	PricingFetchedAt  *time.Time      `json:"pricingFetchedAt"`
	PricingAgeSeconds *int64          `json:"pricingAgeSeconds"`
	Harnesses         []doctorHarness `json:"harnesses"`
	ProviderSources   []doctorSource  `json:"providerSources"`

	// Instance diagnostics (additive): the shared config's path/state and one
	// row per resolved source. Existing fields are unchanged so older parsers
	// keep working.
	ConfigPath    string           `json:"configPath"`
	ConfigPresent bool             `json:"configPresent"`
	ConfigError   string           `json:"configError,omitempty"`
	Instances     []doctorInstance `json:"instances"`

	// Read-only index diagnostics (additive). ReadOnly is always true: doctor
	// opens the index through store.OpenReadOnly and never creates, migrates or
	// reprices it. When the index cannot be read at all, DBUnavailable is true,
	// DBReason is a store read-only reason code and every count stays 0 rather
	// than being invented.
	ReadOnly      bool         `json:"readOnly"`
	DBUnavailable bool         `json:"dbUnavailable"`
	DBReason      string       `json:"dbReason,omitempty"`
	DBDetail      string       `json:"dbDetail,omitempty"`
	Schema        doctorSchema `json:"schema"`
}

// doctorSchema is the read-only schema verdict; a missing required marker is
// reported here and never repaired by doctor.
type doctorSchema struct {
	State   string   `json:"state"`
	Missing []string `json:"missing,omitempty"`
	Detail  string   `json:"detail,omitempty"`
}

// doctorSource is one provider source's health; credentials are never printed.
type doctorSource struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Exists    bool   `json:"exists"`
	ReadAt    string `json:"readAt,omitempty"`
	Providers int    `json:"providers"`
	Matches   int    `json:"matches"`
	Error     string `json:"error,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

func runDoctor(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(errout)
	js := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(errout, "usage: mytoken doctor [--json]")
		return 2
	}
	dir, err := paths.ResolveDataDir()
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	db, err := paths.ResolveDBPath()
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	// The index is opened read-only. Doctor never creates, migrates or reprices
	// it: a missing or unsupported index is reported with its reason, and the
	// message names the command that would perform the write if the user wants
	// it. This is why doctor no longer starts the read-write app.
	report := doctorReport{
		DataDir: dir, DBPath: db, PricingSource: "embedded",
		ReadOnly: true, Harnesses: []doctorHarness{}, Instances: []doctorInstance{},
		Schema: doctorSchema{State: string(store.SchemaUnreadable)},
	}
	if info, e := os.Stat(db); e == nil {
		report.DBBytes = info.Size()
	} else if !os.IsNotExist(e) {
		fmt.Fprintln(errout, e)
		return 1
	}
	var dbh *sql.DB
	ro, roErr := store.OpenReadOnly(ctx, db)
	if roErr != nil {
		report.DBUnavailable = true
		report.DBDetail = roErr.Error()
		reason, _ := store.ReadOnlyReasonOf(roErr)
		if reason == "" {
			reason = store.ReasonUnavailable
		}
		report.DBReason = string(reason)
		if rerr, ok := roErr.(*store.ReadOnlyError); ok && rerr.Schema.State != "" {
			report.Schema = doctorSchema{State: string(rerr.Schema.State), Missing: rerr.Schema.Missing, Detail: rerr.Schema.Detail}
		} else {
			// No schema could be classified (a missing or unopenable file):
			// report it as unreadable and keep the reason code in DBReason.
			report.Schema.Detail = roErr.Error()
		}
	} else {
		defer ro.Close()
		dbh = ro.DB()
		sr := ro.ReadOnlySchema()
		report.Schema = doctorSchema{State: string(sr.State), Missing: sr.Missing, Detail: sr.Detail}
	}
	if raw, e := os.ReadFile(filepath.Join(dir, "prices.json")); e == nil {
		var cache struct {
			Fetched time.Time `json:"fetched"`
		}
		if json.Unmarshal(raw, &cache) == nil && !cache.Fetched.IsZero() {
			report.PricingSource = "cache"
			report.PricingFetchedAt = &cache.Fetched
			age := int64(time.Since(cache.Fetched).Seconds())
			if age < 0 {
				age = 0
			}
			report.PricingAgeSeconds = &age
		}
	}
	for _, p := range harness.All() {
		h := doctorHarness{Harness: string(p.Harness()), Roots: []doctorRoot{}}
		for _, root := range p.Roots() {
			_, e := os.Stat(root)
			h.Roots = append(h.Roots, doctorRoot{root, e == nil})
		}
		if dbh != nil {
			err = dbh.QueryRowContext(ctx, "SELECT count(*) FROM cursors WHERE harness=?", p.Harness()).Scan(&h.Sources)
			if err == nil {
				err = dbh.QueryRowContext(ctx, "SELECT count(*) FROM events WHERE harness=?", p.Harness()).Scan(&h.Events)
			}
			if err != nil {
				fmt.Fprintln(errout, err)
				return 1
			}
		}
		report.Harnesses = append(report.Harnesses, h)
	}
	// Instance diagnostics: resolve the shared config exactly as a scan would,
	// then report how much of the index each source actually covers. A config
	// that cannot be read is reported, not hidden: doctor still exits 0 because
	// the index itself is fine (run "mytoken sources list" for the fatal view).
	sources := buildSourceReport(dir, sourceFallbacks())
	report.ConfigPath = sources.ConfigPath
	report.ConfigPresent = sources.ConfigPresent
	report.ConfigError = sources.ConfigError
	// per-harness/instance collection evidence from the read-only handle doctor
	// already holds. An instance with no stored record reads as unavailable with
	// a reason, never as an invented zero, and a disabled or backup-copy entry
	// keeps its configuration status with an explicit "not collected" reason.
	attachSourceDiagnostics(ctx, &sources, ro, roErr)
	for _, e := range sources.Instances {
		row := doctorInstance{sourceEntry: e}
		if dbh != nil {
			err = dbh.QueryRowContext(ctx, "SELECT count(*) FROM cursors WHERE harness=? AND instance_id=?", e.Harness, e.ID).Scan(&row.Sources)
			if err == nil {
				err = dbh.QueryRowContext(ctx, "SELECT count(*) FROM events WHERE harness=? AND instance_id=?", e.Harness, e.ID).Scan(&row.Events)
			}
			if err != nil {
				fmt.Fprintln(errout, err)
				return 1
			}
		}
		report.Instances = append(report.Instances, row)
	}
	// Provider sources are loaded directly and read-only (a mode=ro SQLite
	// handle and local config files). Credentials are never read or printed,
	// and no attribution is refreshed, so doctor performs no write.
	home, _ := os.UserHomeDir()
	for _, s := range []source.Source{source.NewCCSwitch(source.CCSwitchPath()), source.NewHarnessConfig(home)} {
		_ = s.Load(ctx)
		defer s.Close()
		st := s.Status()
		entry := doctorSource{Name: s.Name(), Path: st.Path, Exists: st.Exists, Providers: st.Providers, Matches: st.Matches, Error: st.Err, Detail: st.Detail}
		if !st.ReadAt.IsZero() {
			entry.ReadAt = st.ReadAt.UTC().Format(time.RFC3339)
		}
		if lister, ok := s.(source.Providers); ok {
			if n := len(lister.Providers()); n > 0 {
				entry.Providers = n
			}
		}
		report.ProviderSources = append(report.ProviderSources, entry)
	}
	if *js {
		err = json.NewEncoder(out).Encode(report)
	} else {
		fmt.Fprintf(out, "Data directory: %s\nDatabase: %s (%d bytes)\nPricing: %s", report.DataDir, report.DBPath, report.DBBytes, report.PricingSource)
		if report.PricingAgeSeconds != nil {
			fmt.Fprintf(out, " (age %ds)", *report.PricingAgeSeconds)
		} else {
			fmt.Fprint(out, " (age unknown)")
		}
		fmt.Fprintln(out)
		if report.DBUnavailable {
			fmt.Fprintf(out, "Index: unavailable (%s) — read-only, nothing was created, migrated or repriced\n", report.DBReason)
		} else {
			fmt.Fprintf(out, "Index: read-only, schema %s", report.Schema.State)
			if len(report.Schema.Missing) > 0 {
				fmt.Fprintf(out, " (missing: %s)", strings.Join(report.Schema.Missing, ","))
			}
			fmt.Fprintln(out)
		}
		w := newTable(out)
		fmt.Fprintf(out, "SOURCES CONFIG: %s (%s)\n", report.ConfigPath, presentWord(report.ConfigPresent))
		if report.ConfigError != "" {
			fmt.Fprintf(out, "  error: %s\n", report.ConfigError)
		}
		fmt.Fprintln(w, "ID\tHARNESS\tORIGIN\tKIND\tSTATUS\tSCANNABLE\tSOURCES\tEVENTS\tROOTS\tREASON\tDIAGNOSTICS")
		for _, e := range report.Instances {
			roots := make([]string, 0, len(e.Roots))
			for _, r := range e.Roots {
				label := r.Path
				if !r.Exists {
					label += " (missing)"
				}
				roots = append(roots, label)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%t\t%d\t%d\t%s\t%s\t%s\n", e.ID, e.Harness, e.Origin, e.Kind, e.Status, e.Scannable, e.Sources, e.Events, strings.Join(roots, ","), e.Reason, diagnosticsCell(e.Diagnostics))
		}
		// Flush the column writer before the evidence blocks so their lines are
		// never reflowed into the table's columns.
		if err = w.Flush(); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		writeDiagnosticsDetail(out, sourceEntries(report.Instances))

		w = newTable(out)
		fmt.Fprintln(w, "HARNESS\tSOURCES\tEVENTS\tROOT\tEXISTS")
		for _, h := range report.Harnesses {
			for _, r := range h.Roots {
				fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%t\n", h.Harness, h.Sources, h.Events, r.Path, r.Exists)
			}
			if len(h.Roots) == 0 {
				fmt.Fprintf(w, "%s\t%d\t%d\t-\tfalse\n", h.Harness, h.Sources, h.Events)
			}
		}
		if len(report.ProviderSources) > 0 {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "PROVIDER SOURCE\tEXISTS\tPROVIDERS\tMATCHES\tREAD AT\tERROR")
			for _, s := range report.ProviderSources {
				fmt.Fprintf(w, "%s\t%t\t%d\t%d\t%s\t%s\n", s.Name, s.Exists, s.Providers, s.Matches, s.ReadAt, s.Error)
			}
		}
		err = w.Flush()
	}
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	return 0
}

func runScan(ctx context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(errout)
	cpu := fs.String("cpuprofile", "", "CPU profile path")
	mem := fs.String("memprofile", "", "allocation profile path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(errout, "usage: mytoken scan")
		return 2
	}
	if *cpu != "" {
		f, err := os.Create(*cpu)
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		if err = pprof.StartCPUProfile(f); err != nil {
			f.Close()
			fmt.Fprintln(errout, err)
			return 1
		}
		defer func() { pprof.StopCPUProfile(); f.Close() }()
	}
	a, err := app.OpenLocal()
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	defer a.Close()
	start := time.Now()
	if err = a.Scanner.Scan(ctx); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	var events, sessions int64
	err = a.Store.DB().QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&events)
	if err == nil {
		err = a.Store.DB().QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE parent_id='' AND EXISTS (SELECT 1 FROM events WHERE events.harness=sessions.harness AND events.session_id=sessions.session_id)").Scan(&sessions)
	}
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	done, total := a.Scanner.Progress()
	fmt.Fprintf(out, "Scanned %d/%d sources: %d events, %d sessions in %s\n", done, total, events, sessions, time.Since(start).Round(time.Millisecond))
	if *mem != "" {
		f, e := os.Create(*mem)
		if e == nil {
			runtime.GC()
			e = pprof.Lookup("allocs").WriteTo(f, 0)
			closeErr := f.Close()
			if e == nil {
				e = closeErr
			}
		}
		if e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
	}
	return 0
}
