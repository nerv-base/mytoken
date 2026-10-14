package gui

// This file is the sources card's view of the scanner's persisted collection
// evidence. The record itself lives in the store's settings, is written once
// per completed scan by internal/scan, and is read back through
// app.SourceDiagnostics in the same background pass as the configuration — so
// drawing a frame never touches the store.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/scan"
)

// DiagnosticsReader reads the collection evidence the store recorded for one
// harness instance. ok is false when there is no usable record, which is
// unknown and never zero. It must not scan, query or fetch anything: reading
// the record is all it does.
type DiagnosticsReader func(ctx context.Context, h model.Harness, instance string) (scan.Diagnostics, bool)

// diagnosticsAge renders an instant as an age, and never as fresher than it
// is: a timestamp after now (a clock that moved) is shown as itself instead of
// being rounded down into "just now", and an old record keeps its real age.
func diagnosticsAge(now, t time.Time) string {
	if t.IsZero() {
		return tr("srcDiagNever")
	}
	if t.After(now) {
		return fmtWhen(t)
	}
	return fmtAgo(now, t)
}

// diagnosticsLines renders one record the way the card shows it: when the last
// attempt and the last clean success happened, then what that attempt found.
// A session count the record could not state reads "—", never 0.
func diagnosticsLines(now time.Time, d scan.Diagnostics) (when, found string) {
	sessions := "—"
	if d.IndexedSessions != nil {
		sessions = fmtInt(*d.IndexedSessions)
	}
	when = strings.Join([]string{
		fmt.Sprintf(tr("srcDiagAttempt"), diagnosticsAge(now, d.LastAttemptAt)),
		fmt.Sprintf(tr("srcDiagSuccess"), diagnosticsAge(now, d.LastSuccessAt)),
	}, " · ")
	found = strings.Join([]string{
		fmt.Sprintf(tr("srcDiagFiles"), fmtInt(int64(d.DiscoveredFiles))),
		fmt.Sprintf(tr("srcDiagEvents"), fmtInt(d.NewEvents)),
		fmt.Sprintf(tr("srcDiagSessions"), sessions),
		fmt.Sprintf(tr("srcDiagReason"), string(d.Reason)),
	}, " · ")
	if d.SourcesTruncated > 0 {
		found += " · " + fmt.Sprintf(tr("srcDiagTruncated"), d.SourcesTruncated)
	}
	return when, found
}

// diagnosticsHistorical reports whether a record still describes the instance's
// current roots. A record outlives a Move — it is keyed by instance id, and the
// id survives the move — so until a fresh scan overwrites it its paths name the
// old location. Such a record is last-known evidence about where the instance
// used to be, never a statement about the roots now in force.
func diagnosticsHistorical(d scan.Diagnostics, roots []string) bool {
	if len(d.Sources) == 0 {
		return false
	}
	for _, src := range d.Sources {
		for _, root := range roots {
			if pathWithin(src.Path, root) {
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
