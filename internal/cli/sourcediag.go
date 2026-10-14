package cli

// This file joins the collection evidence the index holds onto the resolved
// sources report, so "sources list", "doctor" and the snapshot's sources
// section all show what a scan actually did instead of what is merely
// configured.
//
// It is the only part of the sources report that opens the index, and it opens
// it read-only: the evidence is read, never written, and reading never re-dates
// a record. A source that is disabled or a backup-copy is never scanned, so it
// has no record of its own and says so; it keeps its configuration status.
// Absent evidence is a reason, never a zero count.

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/paths"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/snapshot"
	"github.com/zzstar101/mytoken/internal/store"
)

// attachSourceDiagnostics fills each report entry's Diagnostics field from the
// index. ro is an already-open read-only handle when the caller has one (doctor
// does); when ro is nil the handle is opened here read-only, and openErr may
// explain why the index is not readable at all.
//
// Only sources a scan would attempt get their own record: a disabled entry, a
// backup-copy and an entry whose roots could not be resolved are configured but
// never collected, and they say exactly that rather than reporting no record.
func attachSourceDiagnostics(ctx context.Context, rep *sourceReport, ro *store.Store, openErr error) {
	if rep == nil {
		return
	}
	if ro == nil && openErr == nil {
		for _, e := range rep.Instances {
			if e.Scannable {
				opened, err := readOnlySources(ctx)
				if err != nil {
					openErr = err
				} else {
					ro = opened
					defer ro.Close()
				}
				break
			}
		}
	}

	records := map[string]snapshot.InstanceDiagnostics{}
	if ro != nil {
		obs, err := query.ReadDiagnostics(ctx, ro, nil)
		if err != nil {
			openErr = fmt.Errorf("the collection diagnostics could not be read: %w", err)
		} else {
			for _, d := range snapshot.BuildDiagnostics(obs, time.Now()) {
				records[sourceKey(d.Harness, d.Instance)] = d
			}
		}
	}

	detail := "the index is not readable"
	if openErr != nil {
		detail = openErr.Error()
	}
	for i := range rep.Instances {
		e := &rep.Instances[i]
		harness := model.Harness(e.Harness)
		switch {
		case !e.Scannable:
			e.Diagnostics = notCollectedDiagnostics(*e)
		case ro == nil:
			e.Diagnostics = snapshot.InstanceDiagnostics{
				Harness: harness, Instance: e.ID,
				Reason: snapshot.ReasonDiagnosticsUnavailable, Detail: detail,
				Sources: []snapshot.DiagnosticsSource{},
			}
		default:
			if d, ok := records[sourceKey(harness, e.ID)]; ok {
				d.Historical = snapshot.DiagnosticsPathsHistorical(diagnosticsPaths(d), entryRootPaths(*e))
				e.Diagnostics = d
				continue
			}
			e.Diagnostics = snapshot.InstanceDiagnostics{
				Harness: harness, Instance: e.ID,
				Reason:  snapshot.ReasonDiagnosticsNoRecord,
				Detail:  "no collection diagnostics stored for this instance; a scan writes them",
				Sources: []snapshot.DiagnosticsSource{},
			}
		}
	}
}

// notCollectedDiagnostics explains why a non-scannable entry has no evidence of
// its own, keeping its configuration status visible next to it.
func notCollectedDiagnostics(e sourceEntry) snapshot.InstanceDiagnostics {
	detail := "not scanned, so it has no collection evidence of its own"
	switch e.Status {
	case "disabled":
		detail = "disabled: never attempted by a scan, so it has no collection evidence of its own"
	case "backup-copy":
		detail = "backup copy: kept for reference, never attempted by a scan, so it has no collection evidence of its own"
	case "error":
		detail = "not scannable: the resolution error must be fixed before a scan can attempt it"
	}
	return snapshot.InstanceDiagnostics{
		Harness: model.Harness(e.Harness), Instance: e.ID,
		Reason: snapshot.ReasonDiagnosticsNotCollected, Detail: detail,
		Sources: []snapshot.DiagnosticsSource{},
	}
}

// diagnosticsPaths lists the source paths a record names, which is what makes a
// record historical relative to an instance's current roots.
func diagnosticsPaths(d snapshot.InstanceDiagnostics) []string {
	out := make([]string, 0, len(d.Sources))
	for _, s := range d.Sources {
		out = append(out, s.Path)
	}
	return out
}

// entryRootPaths lists the roots a report entry names, including the missing
// ones, because a missing root is still the location a record refers to.
func entryRootPaths(e sourceEntry) []string {
	out := make([]string, 0, len(e.Roots))
	for _, r := range e.Roots {
		out = append(out, r.Path)
	}
	return out
}

// diagnosticsCell is the one-word verdict the tables can carry. The full
// evidence is in the JSON and in the detail lines.
func diagnosticsCell(d snapshot.InstanceDiagnostics) string {
	if !d.Available {
		if d.Reason == "" {
			return "unavailable"
		}
		return d.Reason
	}
	return "recorded"
}

// diagnosticsSummaryText renders one entry's evidence as one line: when the
// scanner last attempted and last succeeded, what that attempt found, and the
// scanner's own reason. A timestamp in the future is shown as itself, never
// rounded down into "just now", and a count the record could not state is shown
// as unstated rather than as zero.
func diagnosticsSummaryText(d snapshot.InstanceDiagnostics) string {
	if !d.Available {
		s := d.Reason
		if s == "" {
			s = "unavailable"
		}
		if d.Historical {
			s += " (historical: the record names locations outside the current roots)"
		}
		return s + ": " + d.Detail
	}
	line := fmt.Sprintf("attempt=%s success=%s files=%s events=%s sessions=%s reason=%s",
		diagnosticsAgeText(d.AttemptAgeSeconds, d.LastAttemptAt),
		diagnosticsAgeText(d.SuccessAgeSeconds, d.LastSuccessAt),
		intText(d.DiscoveredFiles), int64Text(d.NewEvents), int64Text(d.IndexedSessions),
		orText(d.CollectionReason, "unstated"))
	if d.SourcesTruncated != nil && *d.SourcesTruncated > 0 {
		line += fmt.Sprintf(" truncated=%d", *d.SourcesTruncated)
	}
	if d.NeverSucceeded {
		line += " never-succeeded"
	}
	if d.Historical {
		line += " historical"
	}
	return line
}

// writeDiagnosticsDetail prints the evidence under a table, one block per
// entry, including the source paths the scan itself reported. Nothing here
// walks a directory or opens a file.
func writeDiagnosticsDetail(out io.Writer, entries []sourceEntry) {
	for _, e := range entries {
		fmt.Fprintf(out, "  diagnostics %s/%s  %s\n", e.Harness, e.ID, diagnosticsSummaryText(e.Diagnostics))
		for _, s := range e.Diagnostics.Sources {
			fmt.Fprintf(out, "    source %s (%s) reason=%s events=%s sessions=%s\n",
				s.Path, orText(s.Kind, "unknown"), orText(s.Reason, "unstated"),
				int64Text(s.Events), int64Text(s.Sessions))
		}
	}
}

// writeDiagnosticsSummary prints the collection evidence the index holds for an
// instance, one line per instance. Instances are never combined. An instance
// with no stored record names that as a reason rather than showing zero counts,
// and a record whose paths are outside the instance's current roots is marked
// historical so its timestamps are not read as current.
func writeDiagnosticsSummary(out io.Writer, entries []snapshot.InstanceDiagnostics) error {
	if len(entries) == 0 {
		fmt.Fprintln(out, "diagnostics no stored collection diagnostics")
		return nil
	}
	fmt.Fprintf(out, "diagnostics %d instance(s), not combined\n", len(entries))
	for _, d := range entries {
		if _, err := fmt.Fprintf(out, "  %s %s\n", diagnosticsEntryLabel(d), diagnosticsSummaryText(d)); err != nil {
			return err
		}
		for _, s := range d.Sources {
			if _, err := fmt.Fprintf(out, "    source %s (%s) reason=%s events=%s sessions=%s\n",
				s.Path, orText(s.Kind, "unknown"), orText(s.Reason, "unstated"),
				int64Text(s.Events), int64Text(s.Sessions)); err != nil {
				return err
			}
		}
	}
	return nil
}

// diagnosticsEntryLabel names one evidence entry. An instance filter can ask
// about an instance id without naming a harness, and the record's key alone
// does not prove which harness wrote it, so the harness is omitted rather than
// guessed.
func diagnosticsEntryLabel(d snapshot.InstanceDiagnostics) string {
	if d.Harness == "" {
		return d.Instance
	}
	return string(d.Harness) + "/" + d.Instance
}

// diagnosticsAgeText renders an instant as an age. A missing instant is
// "never"; one in the future, or one whose age could not be measured, is shown
// as its own timestamp rather than as a fresh-looking zero.
func diagnosticsAgeText(seconds *float64, at *time.Time) string {
	if at == nil {
		return "never"
	}
	if seconds == nil {
		return at.UTC().Format(time.RFC3339)
	}
	return (time.Duration(*seconds) * time.Second).Round(time.Second).String()
}

func intText(v *int) string {
	if v == nil {
		return "unstated"
	}
	return fmt.Sprintf("%d", *v)
}

func int64Text(v *int64) string {
	if v == nil {
		return "unstated"
	}
	return fmt.Sprintf("%d", *v)
}

func orText(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// readOnlySources is the shared read-only open used when a report needs the
// index and the caller has no handle yet. It never creates the database.
func readOnlySources(ctx context.Context) (*store.Store, error) {
	db, err := paths.ResolveDBPath()
	if err != nil {
		return nil, err
	}
	return store.OpenReadOnly(ctx, db)
}
