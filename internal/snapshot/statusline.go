package snapshot

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/paths"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/store"
)

// Statusline is the small statusline payload: one session's cost and cache hit
// rate plus the cached plan quotas. It is read-only and deliberately separate
// from the versioned envelope so it stays cheap.
type Statusline struct {
	GeneratedAt   time.Time    `json:"generatedAt"`
	Index         Status       `json:"index"`
	Session       *SessionLine `json:"session"`
	SessionStatus Status       `json:"sessionStatus"`
	// Timezone is the zone these numbers are reported in.
	Timezone               string      `json:"timezone"`
	TimezoneSource         string      `json:"timezoneSource"`
	NeedsTimezoneSelection bool        `json:"needsTimezoneSelection,omitempty"`
	TimezoneProblem        string      `json:"timezoneProblem,omitempty"`
	SystemZonePreview      string      `json:"systemZonePreview,omitempty"`
	TokRate                *TokRate    `json:"tokRate"`
	TokRateStatus          Status      `json:"tokRateStatus"`
	Plans                  []PlanQuota `json:"plans"`
	QuotaStatus            Status      `json:"quotaStatus"`
}

// SessionLine is the session the statusline is about.
type SessionLine struct {
	Harness    string       `json:"harness"`
	SessionID  string       `json:"sessionId"`
	Title      string       `json:"title,omitempty"`
	UpdatedAt  *time.Time   `json:"updatedAt,omitempty"`
	CostUSD    float64      `json:"costUsd"`
	CostSource string       `json:"costSource"`
	CacheHit   float64      `json:"cacheHit"`
	Tokens     model.Tokens `json:"tokens"`
	Requests   int64        `json:"requests"`
	Unpriced   int64        `json:"unpriced"`
}

// StatuslineOptions configure one statusline read.
type StatuslineOptions struct {
	DBPath             string
	SessionID          string
	InstanceID         string
	Harness            model.Harness
	Filter             query.Filter
	Now                time.Time
	Timezone           string
	Zone               string
	ZoneSource         string
	ZoneNeedsSelection bool
	ZoneProblem        string
	ZoneSystemPreview  string
}

// BuildStatusline reads the statusline from the index read-only. It reports
// availability instead of failing whenever the index is absent.
func BuildStatusline(ctx context.Context, opts StatuslineOptions) *Statusline {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	sl := &Statusline{GeneratedAt: now.UTC(), Plans: []PlanQuota{}, QuotaStatus: Status{Reason: ReasonQuotaUnavailable}}
	sl.Timezone, sl.TimezoneSource = reportingZone(opts.Zone, opts.ZoneSource, opts.Timezone)
	if opts.ZoneNeedsSelection && opts.Timezone == "" {
		sl.NeedsTimezoneSelection = true
		sl.TimezoneProblem = opts.ZoneProblem
		sl.SystemZonePreview = opts.ZoneSystemPreview
		sl.Timezone = systemZoneName
		sl.TimezoneSource = "needs_selection"
	}
	path := opts.DBPath
	if path == "" {
		p, err := paths.ResolveDBPath()
		if err != nil {
			sl.Index = Status{Reason: string(store.ReasonUnavailable), Detail: err.Error()}
			sl.SessionStatus = Status{Reason: string(store.ReasonUnavailable), Detail: err.Error()}
			return sl
		}
		path = p
	}
	ro, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		reason, _ := store.ReadOnlyReasonOf(err)
		if reason == "" {
			reason = store.ReasonUnavailable
		}
		sl.Index = Status{Reason: string(reason), Detail: err.Error()}
		sl.SessionStatus = Status{Reason: string(reason), Detail: err.Error()}
		return sl
	}
	defer ro.Close()
	sl.Index = Status{Available: true}
	sl.TokRateStatus, sl.TokRate = ingestRateOf(ctx, ro, now)
	sl.Plans, sl.QuotaStatus = readPlans(ctx, ro)
	svc := query.NewService(ro)
	row, err := selectSession(ctx, svc, opts)
	if err != nil {
		sl.SessionStatus = Status{Reason: "unavailable", Detail: err.Error()}
		return sl
	}
	at := row.UpdatedAt.UTC()
	line := &SessionLine{
		Harness: string(row.Harness), SessionID: row.SessionID, Title: row.Title,
		UpdatedAt: &at, CostUSD: row.CostUSD, Tokens: row.Tokens,
		Requests: row.Requests, Unpriced: row.Unpriced,
	}
	line.CacheHit = row.Tokens.HitRate()
	switch {
	case row.Requests == 0:
		line.CostSource = "none"
	case row.Unpriced == row.Requests:
		line.CostSource = string(query.CostSourceUnpriced)
	default:
		line.CostSource = string(query.CostSourceEstimate)
	}
	sl.Session = line
	sl.SessionStatus = Status{Available: true}
	return sl
}

// selectSession resolves the explicit session or the most recent one.
func selectSession(ctx context.Context, svc query.Service, opts StatuslineOptions) (query.SessionRow, error) {
	if opts.SessionID == "" {
		rows, _, err := svc.Sessions(ctx, opts.Filter, query.SortRecent, 1, 0)
		if err != nil {
			return query.SessionRow{}, err
		}
		if len(rows) == 0 {
			return query.SessionRow{}, errors.New("no session in the filter window")
		}
		return rows[0], nil
	}
	if opts.InstanceID != "" && opts.Harness != "" {
		row, _, _, err := query.SessionIn(ctx, svc, opts.Harness, opts.InstanceID, opts.SessionID)
		return row, err
	}
	rows, _, err := svc.Sessions(ctx, opts.Filter, query.SortRecent, 500, 0)
	if err != nil {
		return query.SessionRow{}, err
	}
	var found []query.SessionRow
	for _, r := range rows {
		if r.SessionID != opts.SessionID {
			continue
		}
		if opts.Harness != "" && r.Harness != opts.Harness {
			continue
		}
		if opts.InstanceID != "" && r.InstanceID != opts.InstanceID {
			continue
		}
		found = append(found, r)
	}
	switch len(found) {
	case 0:
		return query.SessionRow{}, fmt.Errorf("session %s is not in the index", opts.SessionID)
	case 1:
		return found[0], nil
	default:
		return query.SessionRow{}, fmt.Errorf("session %s exists in more than one instance; add --harness and --instance-id", opts.SessionID)
	}
}
