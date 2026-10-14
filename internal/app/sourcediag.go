package app

// This file is the read side of the scanner's persisted collection evidence.
// The scanner writes one record per harness instance at the end of every
// completed scan (internal/scan/diagnostics.go); this reads it back for the
// GUI, which shows what the store actually recorded instead of inferring it
// from what is configured.

import (
	"context"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/scan"
)

// SourceDiagnostics returns the collection evidence the store holds for one
// harness instance: when the scanner last attempted it and last succeeded, how
// many files discovery recognized, how many new events the scan committed and
// how many sessions the index holds for it.
//
// The second result is false when there is no usable record — absent, corrupt
// or unreadable — and that is unknown, never zero: a caller must not read a
// missing record as a scan that found nothing. The record is returned exactly
// as written, with the timestamps of the scan that wrote it; reading never
// re-dates evidence. A disabled or backup-copy entry is never attempted, so it
// has no record of its own.
func (a *App) SourceDiagnostics(ctx context.Context, h model.Harness, instanceID string) (scan.Diagnostics, bool) {
	if a == nil || a.Store == nil || h == "" || instanceID == "" {
		return scan.Diagnostics{}, false
	}
	d, ok, err := scan.LoadDiagnostics(ctx, a.Store, h, instanceID)
	if err != nil {
		// A record that cannot be read is absent evidence: ErrCorruptDiagnostics
		// is explicitly a record to treat as absent, and a store read failure
		// says no more than that. Zero is a claim about a scan, and there is no
		// scan to make it about.
		return scan.Diagnostics{}, false
	}
	return d, ok
}
