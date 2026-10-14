package cli

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/claudequota"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/instance"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/paths"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/quota"
	"github.com/zzstar101/mytoken/internal/snapshot"
	"github.com/zzstar101/mytoken/internal/store"
)

const claudeQuotaUsage = `Usage: mytoken quota claude info [INSTANCE] [options]
       mytoken quota claude enable --source REF [--instance-id ID] [--yes]
       mytoken quota claude disable [--instance-id ID] [--yes]
       mytoken quota claude refresh [--instance-id ID] [--yes] [--offline]
  --instance-id ID  instance id (repeatable for info; exactly one for a
                    mutation)
  --source REF      exact credential source reference to authorize (the path
                    or keychain service info prints). Required with --yes.
  --yes             confirm an enable or disable without an interactive prompt
  --offline         refuse any network access (refresh is rejected)
  --json            JSON output  --format table|json|csv
  --db PATH         read another index (diagnostics only)

Claude quota is OFF by default and stays off until an explicit consent is
recorded, per instance, for one exact credential source and the one fixed
request target https://api.anthropic.com/api/oauth/usage.

info is read-only. It reads the index through store.OpenReadOnly (settings keys
quota.claude.consent.<instance> and quota.claude.latest.<instance>), never reads
a credential, never touches the network and never writes. It discloses the
exact credential source reference — a file path or a keychain service — and the
fixed HTTPS target, so an enable is always preceded by exactly what it would
authorize.

enable records the consent. It prints the source and target first, then requires
an unambiguous confirmation: interactively you must type the exact source
reference, or with --yes you must pass both --source and --instance-id. It opens
the index writable (store.Open) to persist the shared consent record and does
NOT fetch: no credential is read and no request is made. Use refresh for that.

disable clears the consent and the cached reading in one shared record, so a
running GUI or service stops fetching: every publish re-checks the persisted
consent.

refresh is the only command that uses the network. It refuses when --offline is
set, when no consent is stored, and when the consent does not authorize exactly
this instance and source; it then reads the consented credential and performs
one request to the fixed target. No source is ever inferred: an instance with no
resolvable Claude Code credential source has nothing to authorize.`

// claudeQuotaPrompt is the reader interactive confirmations come from. It is a
// package var so tests never wait on a terminal.
var claudeQuotaPrompt io.Reader = os.Stdin

// claudeQuotaNow is the clock the CLI stamps and ages readings with.
var claudeQuotaNow = time.Now

// claudeQuotaTransport is the transport an explicit refresh uses. It is a
// package var so tests substitute a fake client and never touch the network.
var claudeQuotaTransport = claudequota.DefaultTransport

// claudeQuotaResolveSources is the CLI-local credential source binding. It
// mirrors internal/app's binding so the CLI and the GUI persist into the same
// consent records; it is a package var so tests substitute fake sources.
var claudeQuotaResolveSources = resolveClaudeQuotaSources

// claudeQuotaFlags are the flags every `quota claude` subcommand shares.
type claudeQuotaFlags struct {
	js          bool
	format      string
	dbPath      string
	source      string
	instanceIDs stringList
	yes         bool
	offline     bool
	help        bool
}

// claudeQuotaCandidate is one credential source offered for an instance.
type claudeQuotaCandidate struct {
	SourceKind  string `json:"sourceKind"`
	SourceLabel string `json:"sourceLabel,omitempty"`
	// SourceRef is the exact, non-secret reference: a credentials file path or
	// a keychain service/account.
	SourceRef string `json:"sourceRef"`
	// Selected marks the source an enable without --source would authorize.
	Selected bool `json:"selected"`
}

// claudeQuotaInstanceReport is one instance's consent and cached reading.
type claudeQuotaInstanceReport struct {
	Instance string `json:"instance"`
	// Enabled is true only when a usable consent is stored AND its exact source
	// still resolves here; a consent whose source is gone is not enabled.
	Enabled     bool       `json:"enabled"`
	Reason      string     `json:"reason,omitempty"`
	Detail      string     `json:"detail,omitempty"`
	Target      string     `json:"target"`
	ConsentVer  int        `json:"consentVersion,omitempty"`
	GrantedAt   *time.Time `json:"grantedAt,omitempty"`
	SourceKind  string     `json:"sourceKind,omitempty"`
	SourceLabel string     `json:"sourceLabel,omitempty"`
	// SourceRef is the exact authorized credential source. It is disclosed only
	// by this explicit info command and never by a cached snapshot.
	SourceRef       string `json:"sourceRef,omitempty"`
	SourceAvailable bool   `json:"sourceAvailable"`
	// Candidates are every credential source that resolves for this instance,
	// so the reference to authorize is visible before enable.
	Candidates []claudeQuotaCandidate `json:"candidates"`
	// Quota is the cached reading in the shared snapshot shape: no secret, no
	// credential path, and missing windows are absent rather than zero.
	Quota snapshot.ClaudeQuota `json:"quota"`
}

// claudeQuotaReport is the JSON and text payload of `quota claude info`.
type claudeQuotaReport struct {
	GeneratedAt time.Time                   `json:"generatedAt"`
	Target      string                      `json:"target"`
	Status      snapshot.Status             `json:"status"`
	Instances   []claudeQuotaInstanceReport `json:"instances"`
}

// claudeQuotaMutationReport is the payload of enable/disable/refresh.
type claudeQuotaMutationReport struct {
	GeneratedAt time.Time             `json:"generatedAt"`
	Action      string                `json:"action"`
	Instance    string                `json:"instance"`
	Target      string                `json:"target"`
	OK          bool                  `json:"ok"`
	Reason      string                `json:"reason,omitempty"`
	Detail      string                `json:"detail,omitempty"`
	SourceKind  string                `json:"sourceKind,omitempty"`
	SourceRef   string                `json:"sourceRef,omitempty"`
	Quota       *snapshot.ClaudeQuota `json:"quota,omitempty"`
}

func runQuotaClaude(ctx context.Context, args []string, out, errout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(errout, claudeQuotaUsage)
		return 2
	}
	switch args[0] {
	case "info":
		return runQuotaClaudeInfo(ctx, args[1:], out, errout)
	case "enable":
		return runQuotaClaudeEnable(ctx, args[1:], out, errout)
	case "disable":
		return runQuotaClaudeDisable(ctx, args[1:], out, errout)
	case "refresh":
		return runQuotaClaudeRefresh(ctx, args[1:], out, errout)
	case "help", "-h", "--help":
		fmt.Fprint(out, claudeQuotaUsage)
		return 0
	default:
		fmt.Fprintln(errout, "unknown quota claude command:", args[0])
		fmt.Fprint(errout, claudeQuotaUsage)
		return 2
	}
}

// parseClaudeQuotaArgs parses shared flags plus at most one positional
// INSTANCE. A usage problem is returned so the caller decides the exit code.
func parseClaudeQuotaArgs(command string, args []string, errout io.Writer) (claudeQuotaFlags, []string, error) {
	bools := map[string]bool{"json": true, "yes": true, "offline": true, "help": true, "h": true}
	flags, positional, err := splitArgs(args, bools)
	if err != nil {
		return claudeQuotaFlags{}, nil, err
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(errout)
	var qf claudeQuotaFlags
	fs.BoolVar(&qf.js, "json", false, "JSON output")
	fs.StringVar(&qf.format, "format", "", "table|json|csv")
	fs.StringVar(&qf.dbPath, "db", "", "index path override (diagnostics only)")
	fs.StringVar(&qf.source, "source", "", "exact credential source reference")
	fs.Var(&qf.instanceIDs, "instance-id", "instance id (repeatable)")
	fs.BoolVar(&qf.yes, "yes", false, "confirm without an interactive prompt")
	fs.BoolVar(&qf.offline, "offline", false, "refuse any network access")
	fs.BoolVar(&qf.help, "help", false, "show help")
	fs.BoolVar(&qf.help, "h", false, "show help")
	if err = fs.Parse(flags); err != nil {
		return claudeQuotaFlags{}, nil, err
	}
	if len(positional) > 1 {
		return claudeQuotaFlags{}, nil, errors.New("at most one positional INSTANCE is accepted")
	}
	return qf, positional, nil
}

// claudeQuotaBinding is the CLI's resolution of the Claude Code credential
// sources: the sources a consent may name, and the instances that declare them.
type claudeQuotaBinding struct {
	Sources   []claudequota.Source
	Instances []instance.Resolved
}

// resolveClaudeQuotaSources mirrors internal/app's binding exactly: only
// scannable Claude Code instances contribute, the default profile derives the
// credential from the Claude home directory, and a custom profile derives it
// from its project roots. Nothing is inferred when no origin can be
// established: an unresolved origin contributes no source at all.
func resolveClaudeQuotaSources(dataDir string, fallbacks []instance.Fallback) (claudeQuotaBinding, error) {
	cfg, err := instance.Load(dataDir)
	if err != nil {
		return claudeQuotaBinding{}, err
	}
	resolved, _ := instance.Resolve(cfg, fallbacks)
	var out claudeQuotaBinding
	for _, src := range resolved {
		if src.Harness != model.ClaudeCode || !src.Scannable() {
			continue
		}
		out.Instances = append(out.Instances, src)
		out.Sources = append(out.Sources, claudeSourcesFor(src)...)
	}
	return out, nil
}

// isDefaultClaudeProfile matches internal/app's rule for the built-in profile.
func isDefaultClaudeProfile(src instance.Resolved) bool {
	return src.Harness == model.ClaudeCode && src.ID == instance.DefaultID && src.Source == instance.PathDefault
}

func claudeSourcesFor(src instance.Resolved) []claudequota.Source {
	if isDefaultClaudeProfile(src) {
		home := harness.Home()
		if home == "" {
			return nil
		}
		dir := claudequota.DefaultConfigDir(home)
		out := []claudequota.Source{
			claudequota.FileSource(src.ID, src.Label, filepath.Join(dir, claudequota.CredentialsFileName)),
		}
		if run := claudequota.DefaultKeychainRunner(); run != nil {
			out = append(out, claudequota.KeychainSource(src.ID, src.Label, claudequota.ClaudeCodeKeychainService, "", run))
		}
		return out
	}
	dir, ok := claudequota.ConfigDirFromRoots(src.Roots)
	if !ok || !filepath.IsAbs(dir) {
		return nil
	}
	return []claudequota.Source{claudequota.FileSource(src.ID, src.Label, filepath.Join(dir, claudequota.CredentialsFileName))}
}

// claudeQuotaService builds a service over one store. Offline is true for every
// read and for enable/disable; only refresh passes false, and then only after
// the persisted consent has been checked.
func claudeQuotaService(st *store.Store, sources []claudequota.Source, offline bool) *claudequota.Service {
	opts := claudequota.Options{Sources: sources, Offline: offline, Now: claudeQuotaNow}
	if st != nil {
		opts.Store = st
	}
	if !offline {
		opts.Transport = claudeQuotaTransport()
	}
	return claudequota.New(opts)
}

// openClaudeQuotaReadStore opens the index read-only; it never creates a
// directory or a database, and no credential is read.
func openClaudeQuotaReadStore(ctx context.Context, dbPath string) (*store.Store, error) {
	path := dbPath
	if path == "" {
		p, err := paths.ResolveDBPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	return store.OpenReadOnly(ctx, path)
}

// openClaudeQuotaWriteStore opens the index writable for a mutation. It is
// store.Open, never app.Open/OpenLocal: the store's own bootstrap DDL and
// migration run, and nothing is repriced and no discovery request is made.
func openClaudeQuotaWriteStore(dbPath string) (*store.Store, error) {
	path := dbPath
	if path == "" {
		p, err := paths.ResolveDBPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	return store.Open(path)
}

// claudeErrorReason extracts the package's stable reason from an error.
func claudeErrorReason(err error) string {
	var ce *claudequota.Error
	if errors.As(err, &ce) && ce != nil {
		return string(ce.Reason)
	}
	return string(claudequota.ReasonStoreUnavailable)
}

// ---------------------------------------------------------------------------
// info

func runQuotaClaudeInfo(ctx context.Context, args []string, out, errout io.Writer) int {
	qf, positional, err := parseClaudeQuotaArgs("quota claude info", args, errout)
	if err != nil {
		fmt.Fprintln(errout, err)
		fmt.Fprint(errout, claudeQuotaUsage)
		return 2
	}
	if qf.help {
		fmt.Fprint(out, claudeQuotaUsage)
		return 0
	}
	of, err := resolveFormat(qf.format, qf.js, qf.format != "")
	if err != nil {
		fmt.Fprintln(errout, err)
		return 2
	}
	now := claudeQuotaNow().UTC()
	selected := combineClaudeInstances(positional, qf.instanceIDs.values())

	dataDir, err := paths.ResolveDataDir()
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	binding, err := claudeQuotaResolveSources(dataDir, sourceFallbacks())
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}

	// A missing or unreadable index is not an operational failure: the
	// configured sources are still disclosed and the consent state is reported
	// as unavailable, never as an empty grant.
	ro, rerr := openClaudeQuotaReadStore(ctx, qf.dbPath)
	if ro != nil {
		defer ro.Close()
	}
	if len(selected) == 0 {
		stored := []string{}
		if ro != nil {
			if obs, oerr := query.ReadClaudeQuota(ctx, ro, nil); oerr == nil {
				for _, o := range obs {
					stored = append(stored, o.Instance)
				}
			}
		}
		selected = unionClaudeInstances(bindingInstanceIDs(binding), stored)
	}

	var cached []snapshot.ClaudeQuota
	if ro != nil {
		obs, oerr := query.ReadClaudeQuota(ctx, ro, selected)
		if oerr != nil {
			fmt.Fprintln(errout, oerr)
			return 1
		}
		cached = snapshot.BuildClaudeQuota(obs, now)
	} else {
		cached = snapshot.BuildClaudeQuota(query.MissingClaudeQuotaObservations(selected,
			"the index could not be read; nothing was created or opened writable"), now)
	}

	svc := claudeQuotaService(ro, binding.Sources, true)
	report := claudeQuotaReport{
		GeneratedAt: now,
		Target:      claudequota.Target,
		Instances:   make([]claudeQuotaInstanceReport, 0, len(selected)),
	}
	for i, id := range selected {
		entry := claudeQuotaInstanceReport{Instance: id, Target: claudequota.Target}
		info, ierr := svc.ConsentInfo(ctx, id)
		fillClaudeQuotaInstance(&entry, info, ierr)
		if i < len(cached) {
			entry.Quota = cached[i]
		}
		entry.Candidates = claudeCandidates(binding, id)
		report.Instances = append(report.Instances, entry)
	}
	if rerr != nil && len(selected) == 0 {
		fmt.Fprintln(errout, "the index could not be read:", rerr)
		return 1
	}
	report.Status = claudeQuotaInfoStatus(report.Instances)

	switch of {
	case formatJSON:
		err = json.NewEncoder(out).Encode(report)
	case formatCSV:
		err = writeClaudeQuotaCSV(out, report)
	default:
		err = writeClaudeQuotaText(out, report)
	}
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	return 0
}

// fillClaudeQuotaInstance copies the consent view into the report. The source
// disclosure is copied even when the read failed: a missing or unreadable index
// is the common first-run case, and it must still show exactly which credential
// source and target an enable would authorize, because that disclosure is what
// the confirmation is based on. Only the verdict is overridden by the error.
func fillClaudeQuotaInstance(entry *claudeQuotaInstanceReport, info claudequota.Info, err error) {
	entry.Enabled = info.Enabled
	entry.Reason = string(info.Reason)
	if info.Target != "" {
		entry.Target = info.Target
	}
	entry.ConsentVer = info.ConsentVersion
	if !info.GrantedAt.IsZero() {
		t := info.GrantedAt.UTC()
		entry.GrantedAt = &t
	}
	entry.SourceKind = string(info.SourceKind)
	entry.SourceLabel = info.SourceLabel
	entry.SourceRef = info.SourceRef
	entry.SourceAvailable = info.SourceAvailable
	if err != nil {
		entry.Enabled = false
		entry.Reason = claudeErrorReason(err)
		entry.Detail = err.Error()
	}
}

// claudeQuotaInfoStatus is the whole-section verdict for the info report.
func claudeQuotaInfoStatus(entries []claudeQuotaInstanceReport) snapshot.Status {
	enabled, usable := 0, 0
	for _, e := range entries {
		if e.Enabled {
			enabled++
		}
		if e.Enabled && e.Quota.Status.Available {
			usable++
		}
	}
	if usable > 0 {
		return snapshot.Status{Available: true}
	}
	if len(entries) == 0 {
		return snapshot.Status{Reason: snapshot.ReasonClaudeQuotaNoConsent, Detail: "no Claude Code instance is configured and no consent is stored"}
	}
	if enabled == 0 {
		return snapshot.Status{Reason: snapshot.ReasonClaudeQuotaNotEnabled, Detail: "Claude quota is disabled; it is off by default"}
	}
	for _, e := range entries {
		if e.Enabled && e.Quota.Status.Reason != "" {
			return e.Quota.Status
		}
	}
	return snapshot.Status{Reason: snapshot.ReasonClaudeQuotaNoReading, Detail: "Claude quota is enabled but nothing has been fetched yet"}
}

func writeClaudeQuotaText(out io.Writer, r claudeQuotaReport) error {
	fmt.Fprintf(out, "CLAUDE QUOTA  generated %s  target %s  %s\n",
		r.GeneratedAt.Format(time.RFC3339), r.Target, statusText(r.Status))
	// Off is the default, not a fault: say plainly that nothing is authorized
	// and therefore nothing was read, rather than leaving the reader to decode
	// a status word.
	if !r.Status.Available && (r.Status.Reason == snapshot.ReasonClaudeQuotaNoConsent || r.Status.Reason == snapshot.ReasonClaudeQuotaNotEnabled) {
		fmt.Fprintln(out, "no consent is recorded: no credential source is read and no request is made")
		fmt.Fprintln(out, "enable it with: mytoken quota claude enable --instance-id ID --source REF --yes")
	}
	if len(r.Instances) == 0 {
		fmt.Fprintln(out, "no Claude Code instance is configured and no consent is stored")
		fmt.Fprintln(out, "Claude quota is off by default: mytoken quota claude enable --instance-id ID --source REF --yes")
		return nil
	}
	for i, e := range r.Instances {
		if i > 0 {
			fmt.Fprintln(out)
		}
		if err := writeClaudeQuotaInstanceText(out, e); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, "cached state comes from the index only: no credential is read and no request is made")
	fmt.Fprintln(out, "Claude quota is off by default; instances are never combined and missing windows are absent, never 0%")
	return nil
}

func writeClaudeQuotaInstanceText(out io.Writer, e claudeQuotaInstanceReport) error {
	fmt.Fprintf(out, "instance   %s\n", e.Instance)
	enabled := "no"
	if e.Enabled {
		enabled = "yes"
	}
	if e.Reason != "" {
		enabled += " (" + e.Reason + ")"
	}
	fmt.Fprintf(out, "enabled    %s\n", enabled)
	if e.Detail != "" {
		fmt.Fprintf(out, "detail     %s\n", e.Detail)
	}
	fmt.Fprintf(out, "target     %s\n", e.Target)
	if e.ConsentVer != 0 {
		fmt.Fprintf(out, "consent    version=%d granted=%s\n", e.ConsentVer, timeText(e.GrantedAt))
	} else if !e.Enabled {
		fmt.Fprintln(out, "consent    none recorded")
	}
	if e.SourceKind != "" || e.SourceRef != "" {
		fmt.Fprintf(out, "source     kind=%s ref=%s available=%s\n",
			claudeSourceKindText(e.SourceKind), e.SourceRef, boolText(e.SourceAvailable))
	}
	if e.SourceLabel != "" {
		fmt.Fprintf(out, "label      %s\n", e.SourceLabel)
	}
	fmt.Fprintf(out, "candidates %s\n", claudeCandidateText(e.Candidates))
	cached := statusText(e.Quota.Status)
	if e.Quota.Status.Detail != "" {
		// The stored reading's own verdict, in the words the snapshot built it
		// with: an enabled instance that is failing must say why.
		cached += ": " + e.Quota.Status.Detail
	}
	fmt.Fprintf(out, "cached     %s\n", cached)
	if e.Quota.ReadingStatus != "" {
		line := "reading    " + e.Quota.ReadingStatus
		if e.Quota.ReadingReason != "" {
			line += " (" + e.Quota.ReadingReason + ")"
		}
		fmt.Fprintln(out, line)
	}
	if e.Quota.FetchedAt != nil {
		fmt.Fprintf(out, "fetched    %s  age=%s\n", timeText(e.Quota.FetchedAt), claudeAgeText(e.Quota.AgeSeconds, e.Quota.StaleAfterSeconds))
	}
	if e.Quota.AttemptedAt != nil {
		fmt.Fprintf(out, "attempted  %s\n", timeText(e.Quota.AttemptedAt))
	}
	for _, w := range e.Quota.Windows {
		fmt.Fprintf(out, "window     %s\n", claudeWindowText(w))
	}
	fmt.Fprintf(out, "forecast   %s\n", claudeForecastText(e.Quota.Forecast))
	return nil
}

func claudeCandidateText(cands []claudeQuotaCandidate) string {
	if len(cands) == 0 {
		return "none: no Claude Code credential source resolves for this instance"
	}
	parts := make([]string, 0, len(cands))
	for _, c := range cands {
		p := c.SourceKind + " " + c.SourceRef
		if c.SourceLabel != "" {
			p += " [" + c.SourceLabel + "]"
		}
		if c.Selected {
			p += " (default)"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "; ")
}

func claudeWindowText(w claudequota.Window) string {
	if w.Kind == "" {
		return "unnamed window"
	}
	if !w.Present || w.UsedPercent == nil {
		// A window the endpoint did not state is absent, never 0% used.
		return w.Kind + "=absent (not reported; never 0%)"
	}
	s := fmt.Sprintf("%s=%.1f%% used", w.Kind, *w.UsedPercent)
	if w.RemainingPercent != nil {
		s += fmt.Sprintf(" / %.1f%% left", *w.RemainingPercent)
	}
	if w.WindowMinutes != nil {
		s += fmt.Sprintf(" (%dm)", *w.WindowMinutes)
	}
	if w.ResetsAt != nil {
		s += " resets " + w.ResetsAt.UTC().Format(time.RFC3339)
	}
	return s
}

func claudeForecastText(f *quota.Forecast) string {
	if f == nil {
		return "not recorded (an older reading carries no retained samples)"
	}
	if !f.Available {
		if f.Reason != "" {
			return "unavailable (" + string(f.Reason) + ")"
		}
		return "unavailable"
	}
	parts := make([]string, 0, len(f.Windows))
	for _, w := range f.Windows {
		if !w.Available {
			continue
		}
		p := fmt.Sprintf("%s %.2f%%/h", w.Window, w.Pace.PercentPerHour)
		switch {
		case w.Pace.ExhaustsAt != nil:
			p += " exhausts " + w.Pace.ExhaustsAt.UTC().Format(time.RFC3339)
		case w.Pace.ResetsBeforeExhaustion:
			p += " resets before exhaustion"
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return "available"
	}
	return strings.Join(parts, ", ")
}

func claudeSourceKindText(kind string) string {
	if kind == "" {
		return "none"
	}
	return kind
}

func claudeAgeText(age *int64, staleAfter int64) string {
	if age == nil {
		return "-"
	}
	out := (time.Duration(*age) * time.Second).String()
	if staleAfter > 0 && *age > staleAfter {
		out += " (older than the " + (time.Duration(staleAfter) * time.Second).String() + " freshness budget)"
	}
	return out
}

func writeClaudeQuotaCSV(out io.Writer, r claudeQuotaReport) error {
	w := csv.NewWriter(out)
	if err := w.Write([]string{
		"instance", "enabled", "reason", "target", "source_kind", "source_ref", "source_available",
		"reading_status", "reading_reason", "fetched_at", "age_seconds", "forecast_available", "forecast_reason",
	}); err != nil {
		return err
	}
	for _, e := range r.Instances {
		forecastAvail, forecastReason := "", ""
		if e.Quota.Forecast != nil {
			forecastAvail = boolText(e.Quota.Forecast.Available)
			forecastReason = string(e.Quota.Forecast.Reason)
		}
		row := []string{
			e.Instance, boolText(e.Enabled), e.Reason, e.Target,
			e.SourceKind, e.SourceRef, boolText(e.SourceAvailable),
			e.Quota.ReadingStatus, e.Quota.ReadingReason,
			csvTime(e.Quota.FetchedAt), csvInt64(e.Quota.AgeSeconds),
			forecastAvail, forecastReason,
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// ---------------------------------------------------------------------------
// enable

func runQuotaClaudeEnable(ctx context.Context, args []string, out, errout io.Writer) int {
	qf, positional, err := parseClaudeQuotaArgs("quota claude enable", args, errout)
	if err != nil {
		fmt.Fprintln(errout, err)
		fmt.Fprint(errout, claudeQuotaUsage)
		return 2
	}
	if qf.help {
		fmt.Fprint(out, claudeQuotaUsage)
		return 0
	}
	explicitInstance := len(qf.instanceIDs.values()) > 0 || len(positional) == 1
	if qf.yes && !explicitInstance {
		fmt.Fprintln(errout, "quota claude enable --yes requires an explicit --instance-id: an unattended consent must name exactly the instance it authorizes")
		return 2
	}
	if qf.yes && qf.source == "" {
		fmt.Fprintln(errout, "quota claude enable --yes requires an explicit --source: an unattended consent must name exactly the credential source it authorizes")
		return 2
	}

	now := claudeQuotaNow().UTC()
	dataDir, err := paths.ResolveDataDir()
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	binding, err := claudeQuotaResolveSources(dataDir, sourceFallbacks())
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	selected := combineClaudeInstances(positional, qf.instanceIDs.values())
	id, err := selectClaudeInstance(binding, selected, bindingInstanceIDs(binding), qf.yes)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 2
	}

	cands := sourcesForClaudeInstance(binding, id)
	if len(cands) == 0 {
		fmt.Fprintf(errout, "no Claude Code credential source resolves for instance %s; nothing can be authorized and no source is inferred\n", id)
		return 1
	}
	chosen, ok := chooseClaudeSource(cands, qf.source)
	if !ok {
		fmt.Fprintf(errout, "--source %q is not a configured credential source for instance %s\n", qf.source, id)
		fmt.Fprintf(errout, "candidates: %s\n", claudeCandidateText(claudeCandidates(binding, id)))
		return 2
	}

	// The disclosure comes before any confirmation or write.
	fmt.Fprintf(out, "enabling Claude quota for instance %s\n", id)
	fmt.Fprintf(out, "  credential source  %s  %s\n", chosen.Kind, chosen.Ref)
	if chosen.Label != "" {
		fmt.Fprintf(out, "  source label       %s\n", chosen.Label)
	}
	fmt.Fprintf(out, "  request target     %s\n", claudequota.Target)
	fmt.Fprintln(out, "  this authorizes reading that one credential and sending it to that one HTTPS target;")
	fmt.Fprintln(out, "  the credential itself is never stored, logged or displayed")

	if !qf.yes {
		if err = confirmClaudeQuota(out, chosen.Ref); err != nil {
			fmt.Fprintln(errout, "not enabled:", err)
			return 1
		}
	}

	st, err := openClaudeQuotaWriteStore(qf.dbPath)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	defer st.Close()
	svc := claudeQuotaService(st, binding.Sources, true)
	info, err := svc.Enable(ctx, id, chosen.Ref)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	report := claudeQuotaMutationReport{
		GeneratedAt: now, Action: "enable", Instance: id, Target: claudequota.Target,
		OK: true, SourceKind: string(info.SourceKind), SourceRef: info.SourceRef,
	}
	return writeClaudeMutation(ctx, st, out, errout, report, info)
}

// selectClaudeInstance resolves the one instance a mutation applies to. An
// explicit selection wins; otherwise a single configured instance decides it,
// and anything ambiguous must be named rather than guessed.
func selectClaudeInstance(binding claudeQuotaBinding, selected, fallback []string, requireExplicit bool) (string, error) {
	if len(selected) > 1 {
		return "", errors.New("exactly one instance is required: pass one --instance-id")
	}
	if len(selected) == 1 {
		return selected[0], nil
	}
	if requireExplicit {
		return "", errors.New("--yes requires an explicit --instance-id")
	}
	switch len(fallback) {
	case 0:
		return instance.DefaultID, nil
	case 1:
		return fallback[0], nil
	default:
		return "", fmt.Errorf("more than one instance is configured (%s); pass --instance-id", strings.Join(fallback, ", "))
	}
}

// chooseClaudeSource selects the exact source by reference. An empty reference
// selects the instance's first configured source, which is the file source; the
// caller still discloses and confirms that exact reference.
func chooseClaudeSource(cands []claudequota.Source, ref string) (claudequota.Source, bool) {
	if ref == "" {
		return cands[0], true
	}
	for _, c := range cands {
		if c.Ref == ref {
			return c, true
		}
	}
	return claudequota.Source{}, false
}

// confirmClaudeQuota requires the exact reference to be typed, so a stray
// newline cannot authorize an enable.
func confirmClaudeQuota(out io.Writer, want string) error {
	fmt.Fprintf(out, "type the exact source reference to confirm, or anything else to abort:\n%s\n> ", want)
	line, err := readClaudeConfirmation(claudeQuotaPrompt)
	if err != nil {
		return err
	}
	if line != want {
		return errors.New("confirmation did not match the source reference; nothing was changed")
	}
	return nil
}

// confirmClaudeDisable requires the exact instance id to be typed.
func confirmClaudeDisable(out io.Writer, want string) error {
	fmt.Fprintf(out, "type the instance id to confirm disabling Claude quota and clearing its cached reading:\n%s\n> ", want)
	line, err := readClaudeConfirmation(claudeQuotaPrompt)
	if err != nil {
		return err
	}
	if line != want {
		return errors.New("confirmation did not match the instance id; nothing was changed")
	}
	return nil
}

func readClaudeConfirmation(r io.Reader) (string, error) {
	if r == nil {
		return "", errors.New("no confirmation input is available; pass --yes with an explicit selection")
	}
	reader := bufio.NewReader(io.LimitReader(r, 4096))
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		if errors.Is(err, io.EOF) {
			return "", errors.New("confirmation input ended before a line was read; nothing was changed")
		}
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// ---------------------------------------------------------------------------
// disable

func runQuotaClaudeDisable(ctx context.Context, args []string, out, errout io.Writer) int {
	qf, positional, err := parseClaudeQuotaArgs("quota claude disable", args, errout)
	if err != nil {
		fmt.Fprintln(errout, err)
		fmt.Fprint(errout, claudeQuotaUsage)
		return 2
	}
	if qf.help {
		fmt.Fprint(out, claudeQuotaUsage)
		return 0
	}
	explicitInstance := len(qf.instanceIDs.values()) > 0 || len(positional) == 1
	if qf.yes && !explicitInstance {
		fmt.Fprintln(errout, "quota claude disable --yes requires an explicit --instance-id")
		return 2
	}
	now := claudeQuotaNow().UTC()
	dataDir, err := paths.ResolveDataDir()
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	binding, err := claudeQuotaResolveSources(dataDir, sourceFallbacks())
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	// The index is read first, read-only: disable must not create an index just
	// to find out that nothing was ever enabled.
	ro, _ := openClaudeQuotaReadStore(ctx, qf.dbPath)
	stored := []string{}
	if ro != nil {
		if obs, oerr := query.ReadClaudeQuota(ctx, ro, nil); oerr == nil {
			for _, o := range obs {
				stored = append(stored, o.Instance)
			}
		}
		ro.Close()
	}
	selected := combineClaudeInstances(positional, qf.instanceIDs.values())
	id, err := selectClaudeInstance(binding, selected, unionClaudeInstances(bindingInstanceIDs(binding), stored), false)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 2
	}

	if !qf.yes {
		fmt.Fprintf(out, "disabling Claude quota for instance %s\n", id)
		fmt.Fprintln(out, "  this clears the stored consent and the cached reading; a running GUI or service stops fetching because every publish re-checks the persisted consent")
		if err = confirmClaudeDisable(out, id); err != nil {
			fmt.Fprintln(errout, "not disabled:", err)
			return 1
		}
	}

	st, err := openClaudeQuotaWriteStore(qf.dbPath)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	defer st.Close()
	svc := claudeQuotaService(st, binding.Sources, true)
	if err = svc.Disable(ctx, id); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	report := claudeQuotaMutationReport{
		GeneratedAt: now, Action: "disable", Instance: id, Target: claudequota.Target, OK: true,
	}
	return writeClaudeMutation(ctx, st, out, errout, report, claudequota.Info{})
}

// ---------------------------------------------------------------------------
// refresh

func runQuotaClaudeRefresh(ctx context.Context, args []string, out, errout io.Writer) int {
	qf, positional, err := parseClaudeQuotaArgs("quota claude refresh", args, errout)
	if err != nil {
		fmt.Fprintln(errout, err)
		fmt.Fprint(errout, claudeQuotaUsage)
		return 2
	}
	if qf.help {
		fmt.Fprint(out, claudeQuotaUsage)
		return 0
	}
	if qf.offline {
		fmt.Fprintln(errout, "quota claude refresh refuses to run with --offline: an explicit refresh is the only command that uses the network, and it is rejected offline")
		return 2
	}
	now := claudeQuotaNow().UTC()
	dataDir, err := paths.ResolveDataDir()
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	binding, err := claudeQuotaResolveSources(dataDir, sourceFallbacks())
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	selected := combineClaudeInstances(positional, qf.instanceIDs.values())

	// The consent is checked read-only first, so a refresh without a matching
	// consent neither creates an index nor opens a writable store, and the
	// network is never reached.
	ro, rerr := openClaudeQuotaReadStore(ctx, qf.dbPath)
	if rerr != nil {
		fmt.Fprintf(errout, "claude quota is not enabled: the index could not be read read-only (%s); nothing was fetched\n", claudeReadOnlyReason(rerr))
		return 1
	}
	stored := []string{}
	if obs, oerr := query.ReadClaudeQuota(ctx, ro, nil); oerr == nil {
		for _, o := range obs {
			stored = append(stored, o.Instance)
		}
	}
	id, ierr := selectClaudeInstance(binding, selected, unionClaudeInstances(bindingInstanceIDs(binding), stored), false)
	if ierr != nil {
		ro.Close()
		fmt.Fprintln(errout, ierr)
		return 2
	}
	pre := claudeQuotaService(ro, binding.Sources, true)
	info, cerr := pre.ConsentInfo(ctx, id)
	ro.Close()
	if cerr != nil {
		fmt.Fprintf(errout, "claude quota is not enabled for instance %s: %s\n", id, cerr)
		return 1
	}
	if !info.Enabled {
		reason := string(info.Reason)
		if reason == "" {
			reason = string(claudequota.ReasonNotEnabled)
		}
		fmt.Fprintf(errout, "claude quota is not enabled for instance %s (%s); nothing was fetched\n", id, reason)
		return 1
	}
	if !info.SourceAvailable {
		fmt.Fprintf(errout, "the consented credential source %s is no longer available for instance %s; nothing was fetched and no source is substituted\n", info.SourceRef, id)
		return 1
	}

	fmt.Fprintf(out, "claude quota refresh: instance %s  target %s  source %s\n", id, claudequota.Target, info.SourceRef)
	st, err := openClaudeQuotaWriteStore(qf.dbPath)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	defer st.Close()
	svc := claudeQuotaService(st, binding.Sources, false)
	q, err := svc.Refresh(ctx, id)
	if err != nil {
		report := claudeQuotaMutationReport{
			GeneratedAt: now, Action: "refresh", Instance: id, Target: claudequota.Target,
			Reason: claudeErrorReason(err), Detail: err.Error(),
		}
		return writeClaudeMutation(ctx, st, out, errout, report, claudequota.Info{})
	}
	report := claudeQuotaMutationReport{
		GeneratedAt: now, Action: "refresh", Instance: id, Target: claudequota.Target, OK: true,
		SourceKind: string(q.SourceKind), SourceRef: q.SourceRef,
	}
	return writeClaudeMutation(ctx, st, out, errout, report, claudequota.Info{})
}

func claudeReadOnlyReason(err error) string {
	if reason, ok := store.ReadOnlyReasonOf(err); ok {
		return string(reason)
	}
	return err.Error()
}

// ---------------------------------------------------------------------------
// shared output

// writeClaudeMutation renders the result of enable/disable/refresh. When the
// store is available it re-reads the persisted state, so the printed reading is
// the one actually stored rather than the in-memory value, and a refresh shows
// its own published windows.
func writeClaudeMutation(ctx context.Context, st *store.Store, out, errout io.Writer, report claudeQuotaMutationReport, info claudequota.Info) int {
	if report.Detail != "" {
		fmt.Fprintln(errout, report.Detail)
	}
	fmt.Fprintf(out, "%s  instance %s  target %s  %s\n", report.Action, report.Instance, report.Target, statusText(mutationStatus(report)))
	if !report.OK {
		return 1
	}
	switch report.Action {
	case "enable":
		fmt.Fprintf(out, "consent  kind=%s ref=%s\n", info.SourceKind, info.SourceRef)
		if !info.GrantedAt.IsZero() {
			fmt.Fprintf(out, "granted  %s\n", info.GrantedAt.UTC().Format(time.RFC3339))
		}
		fmt.Fprintln(out, "no credential was read and no request was made")
		fmt.Fprintf(out, "next     mytoken quota claude refresh --instance-id %s\n", report.Instance)
	case "disable":
		fmt.Fprintln(out, "consent cleared and cached reading cleared; a running GUI or service stops fetching because every publish re-checks the persisted consent")
	}
	if st == nil {
		return 0
	}
	obs, err := query.ReadClaudeQuota(ctx, st, []string{report.Instance})
	if err != nil {
		return 0
	}
	entries := snapshot.BuildClaudeQuota(obs, report.GeneratedAt)
	if len(entries) == 0 {
		return 0
	}
	entry := entries[0]
	fmt.Fprintf(out, "cached   %s\n", statusText(entry.Status))
	if entry.ReadingStatus != "" {
		line := "reading  " + entry.ReadingStatus
		if entry.ReadingReason != "" {
			line += " (" + entry.ReadingReason + ")"
		}
		fmt.Fprintln(out, line)
	}
	if entry.FetchedAt != nil {
		fmt.Fprintf(out, "fetched  %s  age=%s\n", timeText(entry.FetchedAt), claudeAgeText(entry.AgeSeconds, entry.StaleAfterSeconds))
	}
	for _, w := range entry.Windows {
		fmt.Fprintf(out, "window   %s\n", claudeWindowText(w))
	}
	fmt.Fprintf(out, "forecast %s\n", claudeForecastText(entry.Forecast))
	return 0
}

func mutationStatus(r claudeQuotaMutationReport) snapshot.Status {
	if r.OK {
		return snapshot.Status{Available: true}
	}
	return snapshot.Status{Reason: r.Reason, Detail: r.Detail}
}

// ---------------------------------------------------------------------------
// small shared helpers

func combineClaudeInstances(positional, flagged []string) []string {
	out := make([]string, 0, len(positional)+len(flagged))
	for _, id := range positional {
		out = append(out, id)
	}
	out = append(out, flagged...)
	return dedupeClaudeInstances(out)
}

func dedupeClaudeInstances(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func unionClaudeInstances(groups ...[]string) []string {
	var all []string
	for _, g := range groups {
		all = append(all, g...)
	}
	return dedupeClaudeInstances(all)
}

func bindingInstanceIDs(binding claudeQuotaBinding) []string {
	out := make([]string, 0, len(binding.Instances))
	for _, src := range binding.Instances {
		out = append(out, src.ID)
	}
	return dedupeClaudeInstances(out)
}

func sourcesForClaudeInstance(binding claudeQuotaBinding, id string) []claudequota.Source {
	var out []claudequota.Source
	for _, s := range binding.Sources {
		if s.Instance == id {
			out = append(out, s)
		}
	}
	return out
}

func claudeCandidates(binding claudeQuotaBinding, id string) []claudeQuotaCandidate {
	srcs := sourcesForClaudeInstance(binding, id)
	out := make([]claudeQuotaCandidate, 0, len(srcs))
	for i, s := range srcs {
		out = append(out, claudeQuotaCandidate{
			SourceKind:  string(s.Kind),
			SourceLabel: s.Label,
			SourceRef:   s.Ref,
			Selected:    i == 0,
		})
	}
	return out
}

func boolText(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func csvInt64(v *int64) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%d", *v)
}
