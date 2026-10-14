package snapshot

// This file is the snapshot's view of the collection evidence the index holds
// for each harness instance. The record is written once per completed scan by
// internal/scan and keyed by harness + instance; this reads it through the
// query layer and reports it verbatim. Nothing here sums instances together:
// two instances are two sources, and two harnesses can share one instance id.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/scan"
	"github.com/zzstar101/mytoken/internal/store"
)

// DiagnosticsSource is one source path the scan actually recognised, with the
// parser's own classification of it. The path is one discovery already
// reported by the scanner; this never walks a directory or reads a file.
type DiagnosticsSource struct {
	Path   string `json:"path"`
	Kind   string `json:"kind,omitempty"`
	Reason string `json:"reason"`
	// Events and Sessions are zero only when the record said zero: a source
	// with nothing to say carries no counts at all.
	Events   *int64 `json:"events,omitempty"`
	Sessions *int64 `json:"sessions,omitempty"`
}

// InstanceDiagnostics is what the index actually recorded for one harness
// instance: when the scanner last attempted it, when it last succeeded, what
// that attempt found and why. Absent or unusable evidence is unavailable, with
// a reason, and never a zero count.
type InstanceDiagnostics struct {
	Harness  model.Harness `json:"harness,omitempty"`
	Instance string        `json:"instance"`
	// Key is the exact settings key the record was stored under, so a reader
	// can check the harness + instance it was keyed by.
	Key       string `json:"key,omitempty"`
	Available bool   `json:"available"`
	// Reason is a snapshot reason constant: no_record, corrupt_record,
	// not_collected, index_unavailable, or "" when Available.
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`

	// LastAttemptAt and LastSuccessAt are the timestamps the scan that wrote
	// the record recorded. Reading never re-dates them: an old record keeps its
	// real age, and a record with no success yet has no LastSuccessAt.
	LastAttemptAt *time.Time `json:"lastAttemptAt,omitempty"`
	LastSuccessAt *time.Time `json:"lastSuccessAt,omitempty"`
	// AttemptAgeSeconds and SuccessAgeSeconds are the ages of those timestamps
	// against this snapshot's now. They are absent for a timestamp in the
	// future (a clock that moved), which is shown as itself and never rounded
	// down to "just now".
	AttemptAgeSeconds *float64 `json:"attemptAgeSeconds,omitempty"`
	SuccessAgeSeconds *float64 `json:"successAgeSeconds,omitempty"`
	// NeverSucceeded is true when the record has an attempt with no clean
	// success after it.
	NeverSucceeded bool `json:"neverSucceeded,omitempty"`

	// CollectionReason is the scanner's own classification of the last attempt
	// (for example zero-usage or no-token-data), which is how "nothing to
	// collect" stays distinct from "zero usage". It is empty, not guessed, for
	// an unavailable record.
	CollectionReason string `json:"collectionReason,omitempty"`
	// DiscoveredFiles, NewEvents and IndexedSessions are the counts the record
	// stated. IndexedSessions is absent when the record could not state it,
	// which is not the same as zero sessions.
	DiscoveredFiles  *int   `json:"discoveredFiles,omitempty"`
	NewEvents        *int64 `json:"newEvents,omitempty"`
	IndexedSessions  *int64 `json:"indexedSessions,omitempty"`
	SourcesTruncated *int   `json:"sourcesTruncated,omitempty"`
	// Sources is every source path the scan recognised, in record order.
	Sources []DiagnosticsSource `json:"sources"`
	// Historical is set by a consumer that knows the instance's current roots:
	// it means every source in the record lies outside them, so the record is
	// last-known evidence about where the instance used to be and not a
	// statement about the roots now in force.
	Historical bool `json:"historical,omitempty"`
}

// BuildDiagnostics renders stored collection diagnostics as one entry per
// harness instance, never merged. now dates the ages; it never replaces a
// stored timestamp.
func BuildDiagnostics(obs []query.DiagnosticsObservation, now time.Time) []InstanceDiagnostics {
	out := make([]InstanceDiagnostics, 0, len(obs))
	for _, o := range obs {
		entry := InstanceDiagnostics{
			Harness:  o.Harness,
			Instance: o.Instance,
			Key:      o.Key,
			Sources:  []DiagnosticsSource{},
		}
		switch {
		case o.Missing:
			entry.Reason = ReasonDiagnosticsNoRecord
			entry.Detail = o.Detail
		case o.Corrupt || o.Record == nil:
			entry.Reason = ReasonDiagnosticsCorrupt
			entry.Detail = o.Detail
		default:
			d := *o.Record
			entry.Available = true
			entry.LastAttemptAt = utcOrNil(d.LastAttemptAt)
			entry.LastSuccessAt = utcOrNil(d.LastSuccessAt)
			entry.AttemptAgeSeconds = ageSeconds(now, d.LastAttemptAt)
			entry.SuccessAgeSeconds = ageSeconds(now, d.LastSuccessAt)
			entry.NeverSucceeded = d.LastSuccessAt.IsZero()
			entry.CollectionReason = string(d.Reason)
			files := d.DiscoveredFiles
			entry.DiscoveredFiles = &files
			events := d.NewEvents
			entry.NewEvents = &events
			if d.IndexedSessions != nil {
				sessions := *d.IndexedSessions
				entry.IndexedSessions = &sessions
			}
			truncated := d.SourcesTruncated
			entry.SourcesTruncated = &truncated
			for _, src := range d.Sources {
				line := DiagnosticsSource{Path: src.Path, Kind: src.Kind, Reason: string(src.Reason)}
				e, ss := src.Events, src.Sessions
				line.Events, line.Sessions = &e, &ss
				entry.Sources = append(entry.Sources, line)
			}
		}
		out = append(out, entry)
	}
	return out
}

// DiagnosticsUnavailable renders the entries for an explicit instance selection
// that could not be read at all. The selection is still answered exactly: every
// requested instance is named, with the reason, and no counts.
func DiagnosticsUnavailable(instanceIDs []string, detail string) []InstanceDiagnostics {
	out := make([]InstanceDiagnostics, 0, len(instanceIDs))
	seen := map[string]bool{}
	for _, id := range instanceIDs {
		instance := store.NormalizeInstanceID(id)
		if seen[instance] {
			continue
		}
		seen[instance] = true
		out = append(out, InstanceDiagnostics{
			Instance: instance,
			Reason:   ReasonDiagnosticsUnavailable,
			Detail:   detail,
			Sources:  []DiagnosticsSource{},
		})
	}
	return out
}

// DiagnosticsSectionStatus summarises the section without combining it: it
// reports whether any instance has a usable record, the newest stored attempt
// time, and otherwise the reason the whole section has nothing to show.
func DiagnosticsSectionStatus(entries []InstanceDiagnostics) Status {
	if len(entries) == 0 {
		return Status{Reason: ReasonDiagnosticsNoRecord, Detail: "no scan has stored collection diagnostics"}
	}
	var newest *time.Time
	available := 0
	corrupt := 0
	unavailable := 0
	for i := range entries {
		e := entries[i]
		switch {
		case e.Available:
			available++
			if e.LastAttemptAt != nil && (newest == nil || e.LastAttemptAt.After(*newest)) {
				at := *e.LastAttemptAt
				newest = &at
			}
		case e.Reason == ReasonDiagnosticsCorrupt:
			corrupt++
		case e.Reason == ReasonDiagnosticsUnavailable:
			unavailable++
		}
	}
	if available > 0 {
		st := Status{Available: true, FetchedAt: newest}
		if available < len(entries) {
			st.Detail = fmt.Sprintf("%d of %d instance(s) have a stored record; the rest are listed with their own reason", available, len(entries))
		}
		return st
	}
	switch {
	case corrupt > 0:
		return Status{Reason: ReasonDiagnosticsCorrupt, Detail: fmt.Sprintf("%d stored record(s) are not usable", corrupt)}
	case unavailable > 0:
		return Status{Reason: ReasonDiagnosticsUnavailable, Detail: "the collection diagnostics could not be read"}
	default:
		return Status{Reason: ReasonDiagnosticsNoRecord, Detail: "no stored collection diagnostics for the selected instances"}
	}
}

// DiagnosticsHistorical reports whether every source path in a record lies
// outside the instance's current roots. A record is keyed by instance id, which
// survives a move, so until a fresh scan overwrites it its paths name the
// previous location. With no known roots this is false: the claim needs the
// roots.
func DiagnosticsHistorical(d scan.Diagnostics, roots []string) bool {
	paths := make([]string, 0, len(d.Sources))
	for _, src := range d.Sources {
		paths = append(paths, src.Path)
	}
	return DiagnosticsPathsHistorical(paths, roots)
}

// DiagnosticsPathsHistorical is DiagnosticsHistorical for an already-joined
// record: it reports whether every recorded source path lies outside the
// instance's current roots. With no recorded paths or no known roots this is
// false, because the claim needs both sides.
func DiagnosticsPathsHistorical(paths, roots []string) bool {
	if len(paths) == 0 || len(roots) == 0 {
		return false
	}
	for _, p := range paths {
		for _, root := range roots {
			if pathWithin(p, root) {
				return false
			}
		}
	}
	return true
}

// pathWithin reports whether p is root itself or lies under it.
func pathWithin(p, root string) bool {
	if p == "" || root == "" {
		return false
	}
	p, root = filepath.Clean(p), filepath.Clean(root)
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// utcOrNil reports an instant in UTC, or nil when it was never recorded.
func utcOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// ageSeconds is how old t is against now, or nil when t is unset or lies in the
// future. A future timestamp keeps its own value upstream and is never rendered
// as an age of zero.
func ageSeconds(now, t time.Time) *float64 {
	if t.IsZero() || t.After(now) {
		return nil
	}
	v := now.Sub(t).Seconds()
	if v < 0 {
		v = 0
	}
	return &v
}

// readDiagnosticsSection reads and renders the section in one place, so every
// caller reports the same evidence with the same reasons.
func readDiagnosticsSection(ctx context.Context, ro *store.Store, instanceIDs []string, now time.Time) ([]InstanceDiagnostics, Status) {
	if ro == nil {
		entries := DiagnosticsUnavailable(instanceIDs, "the index is not readable")
		return entries, DiagnosticsSectionStatus(entries)
	}
	obs, err := query.ReadDiagnostics(ctx, ro, instanceIDs)
	if err != nil {
		entries := DiagnosticsUnavailable(instanceIDs, "the collection diagnostics could not be read: "+err.Error())
		return entries, DiagnosticsSectionStatus(entries)
	}
	entries := BuildDiagnostics(obs, now)
	return entries, DiagnosticsSectionStatus(entries)
}
