package snapshot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/paths"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/store"
)

// Statusline is the small statusline payload: one session's cost and cache hit
// rate plus the relay balance when the index has one. It is read-only and
// deliberately separate from the versioned envelope so it stays cheap.
type Statusline struct {
	GeneratedAt time.Time    `json:"generatedAt"`
	Index       Status       `json:"index"`
	Session     *SessionLine `json:"session"`
	// SessionStatus explains a null Session instead of leaving it ambiguous.
	SessionStatus Status      `json:"sessionStatus"`
	Balance       BalanceLine `json:"balance"`
	// Timezone is the zone these numbers are reported in; TimezoneSource is
	// "stats_timezone", "override", "system" or "needs_selection", so absent
	// metadata is visible and an unresolved calendar is never hidden.
	Timezone       string `json:"timezone"`
	TimezoneSource string `json:"timezoneSource"`
	// NeedsTimezoneSelection marks an unresolved calendar: Timezone is then a
	// label, not a zone, and SystemZonePreview is a preview only.
	NeedsTimezoneSelection bool   `json:"needsTimezoneSelection,omitempty"`
	TimezoneProblem        string `json:"timezoneProblem,omitempty"`
	SystemZonePreview      string `json:"systemZonePreview,omitempty"`
	// TokRate is the live-ingestion rate a running scan published under
	// ingest_rate.latest.v1. A statusline process cannot measure it itself, so
	// it is the published signal or null; TokRateStatus always says which.
	TokRate       *TokRate `json:"tokRate"`
	TokRateStatus Status   `json:"tokRateStatus"`
	// ClaudeQuota is the cached Claude Code consent and reading view per
	// instance, read from the settings table only. The credential source path
	// is never included; the explicit `quota claude info` command discloses it.
	ClaudeQuota []ClaudeQuota `json:"claudeQuota"`
	// ClaudeQuotaStatus is the section verdict; the default state ("nothing
	// consented") is a reason, never a zero percentage.
	ClaudeQuotaStatus Status `json:"claudeQuotaStatus"`
}

// SessionLine is the session the statusline is about. CostSource is "log",
// "estimate", "mixed" or "unpriced"; it is never inferred when no event was read.
type SessionLine struct {
	Harness    string       `json:"harness"`
	InstanceID string       `json:"instanceId"`
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

// BalanceLine is the relay balance view.
//
// A scalar is reported (Available=true with RemainingUSD/UsedUSD/Currency/At)
// only when the index recorded exactly one unambiguous account: one relay site
// identity (origin + key id) with a recorded balance. Several keys are several
// accounts that may share one upstream account, so their amounts are never
// summed, their currencies are never mixed and no single timestamp is presented
// as "the" balance time: Available is then false with Reason multiple_balances
// (or mixed_currency) and Accounts carries every row verbatim. An account with
// no recorded amount keeps null amounts in Accounts and is counted in Sites,
// never in Recorded, so a partial reading is visible as Partial.
type BalanceLine struct {
	Available    bool       `json:"available"`
	Reason       string     `json:"reason,omitempty"`
	Detail       string     `json:"detail,omitempty"`
	RemainingUSD *float64   `json:"remainingUsd"`
	UsedUSD      *float64   `json:"usedUsd"`
	Unlimited    bool       `json:"unlimited"`
	Currency     string     `json:"currency,omitempty"`
	Sites        int        `json:"sites"`
	At           *time.Time `json:"at,omitempty"`
	// Accounts is every recorded relay site row, keyed by its original
	// origin + key id, in index order. It is never empty when a site exists, and
	// it is the only place a per-account amount is exposed when no single scalar
	// may be reported.
	Accounts []Balance `json:"accounts"`
	// Recorded is how many accounts have a recorded balance (an amount or
	// "unlimited"). Sites - Recorded accounts have no balance recorded.
	Recorded int `json:"recorded"`
	// Partial is true when at least one account recorded a balance and at least
	// one did not, so a consumer never reads a scalar as the whole picture.
	Partial bool `json:"partial"`
}

// StatuslineOptions configure one statusline read.
type StatuslineOptions struct {
	DBPath string
	// SessionID selects one session. Empty means the most recent session in the
	// filter window; that is the "what am I doing right now" default.
	SessionID  string
	InstanceID string
	Harness    model.Harness
	Filter     query.Filter
	Now        time.Time
	// Timezone is an explicit --timezone override; empty falls back to the
	// process zone. A persisted stats_timezone is only used when the caller
	// resolved it and computed in it, and passed through Zone/ZoneSource.
	Timezone string
	// Zone and ZoneSource are the already-resolved reporting zone (see
	// Options.Zone); they are reported verbatim.
	Zone       string
	ZoneSource string
	// ZoneNeedsSelection and ZoneProblem report an unresolved calendar; the
	// system zone is then a preview (ZoneSystemPreview), never the calendar.
	ZoneNeedsSelection bool
	ZoneProblem        string
	ZoneSystemPreview  string
}

// BuildStatusline reads the statusline from the index read-only. It reports
// availability instead of failing whenever the index or the balance is absent.
func BuildStatusline(ctx context.Context, opts StatuslineOptions) *Statusline {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	sl := &Statusline{GeneratedAt: now.UTC(), Balance: BalanceLine{RemainingUSD: nil, UsedUSD: nil, Accounts: []Balance{}}, ClaudeQuota: []ClaudeQuota{}}
	// The zone is a property of the invocation, not of the read: it is stated
	// even when the index turns out to be unavailable.
	sl.Timezone, sl.TimezoneSource = reportingZone(opts.Zone, opts.ZoneSource, opts.Timezone)
	// An explicit override stands on its own; without one, an unresolved
	// calendar is stated as unresolved rather than as a system calendar.
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
		return sl
	}
	defer ro.Close()
	sl.Index = Status{Available: true}
	// The ingest rate is a property of the index, not of the selected session:
	// it is reported even when no session matches the filter.
	sl.TokRateStatus, sl.TokRate = ingestRateOf(ctx, ro, now)
	// Like the ingest rate, the cached Claude quota view is a property of the
	// index, so it is reported even when no session matches the filter.
	sl.ClaudeQuota, sl.ClaudeQuotaStatus = statuslineClaudeQuota(ctx, ro, opts, now)

	svc := query.NewService(ro)
	row, err := selectSession(ctx, svc, opts)
	if err != nil {
		sl.SessionStatus = Status{Reason: "session_unavailable", Detail: err.Error()}
		sl.Balance = statuslineBalance(ctx, ro)
		return sl
	}
	line := &SessionLine{
		Harness:    string(row.Harness),
		InstanceID: row.InstanceID,
		SessionID:  row.SessionID,
		Title:      row.Title,
		CostUSD:    row.CostUSD,
		Tokens:     row.Tokens,
		Requests:   row.Requests,
		Unpriced:   row.Unpriced,
	}
	if !row.UpdatedAt.IsZero() {
		t := row.UpdatedAt.UTC()
		line.UpdatedAt = &t
	}
	denom := row.Tokens.Input + row.Tokens.CacheRead + row.Tokens.CacheWrite
	if denom > 0 {
		line.CacheHit = float64(row.Tokens.CacheRead) / float64(denom)
	}
	line.CostSource = costSourceLabel(ctx, svc, row)
	sl.SessionStatus = Status{Available: true}
	sl.Session = line
	sl.Balance = statuslineBalance(ctx, ro)
	return sl
}

// statuslineClaudeQuota reads the cached Claude quota view for the statusline's
// own instance selection. It reads the settings table only, so the statusline
// never needs the network or a credential.
func statuslineClaudeQuota(ctx context.Context, ro *store.Store, opts StatuslineOptions, now time.Time) ([]ClaudeQuota, Status) {
	ids := opts.Filter.InstanceIDs
	if len(ids) == 0 && opts.InstanceID != "" {
		ids = []string{opts.InstanceID}
	}
	obs, err := query.ReadClaudeQuota(ctx, ro, ids)
	if err != nil {
		return []ClaudeQuota{}, Status{Reason: ReasonClaudeQuotaNoConsent, Detail: err.Error()}
	}
	entries := BuildClaudeQuota(obs, now)
	return entries, ClaudeQuotaSectionStatus(entries)
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
	// An explicit id with an explicit instance is unambiguous and cheap.
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

// costSourceLabel reads the cost provenance of the session's own events instead
// of guessing from the aggregate, so a mixed session is labelled as such. The
// session's cost includes its children; provenance is reported for its own
// events, which is what the statusline explains.
func costSourceLabel(ctx context.Context, svc query.Service, row query.SessionRow) string {
	_, _, events, err := query.SessionIn(ctx, svc, row.Harness, row.InstanceID, row.SessionID)
	if err != nil {
		if row.Requests == 0 {
			return "none"
		}
		if row.Unpriced == row.Requests {
			return string(query.CostSourceUnpriced)
		}
		return string(query.CostSourceLog)
	}
	var log, estimate, unpriced int
	for _, e := range events {
		switch e.CostSource {
		case query.CostSourceLog:
			log++
		case query.CostSourceEstimate:
			estimate++
		default:
			unpriced++
		}
	}
	switch {
	case len(events) == 0:
		return "none"
	case unpriced == len(events):
		return string(query.CostSourceUnpriced)
	case log > 0 && estimate == 0 && unpriced == 0:
		return string(query.CostSourceLog)
	case estimate > 0 && log == 0 && unpriced == 0:
		return string(query.CostSourceEstimate)
	default:
		return "mixed"
	}
}

// statuslineBalance renders the persisted relay balances without ever merging
// distinct accounts. readBalances already returns at most one row per relay site
// identity (origin + key id), so a row is an account's own last reading. The
// number of known accounts is the whole decision:
//
//   - no relay site: unavailable, keeping readBalances' reason;
//   - exactly one key, with a reading: that account's values are copied verbatim,
//     including its own timestamp;
//   - exactly one key, no reading: unavailable, never $0;
//   - several keys: no scalar can be true, because the keys may belong to the
//     same upstream account (or to different ones) and nothing proves otherwise.
//     That holds even when only one of them recorded an amount: the aggregate
//     stays unavailable and every row is listed in Accounts, with its own amount,
//     currency and timestamp. A currency disagreement is named separately so a
//     reader never tries to add the numbers up.
func statuslineBalance(ctx context.Context, ro *store.Store) BalanceLine {
	rows, st := BalanceSites(ctx, ro)
	if rows == nil {
		rows = []Balance{}
	}
	out := BalanceLine{
		Available: st.Available, Reason: st.Reason, Detail: st.Detail,
		Sites: len(rows), Accounts: rows,
	}
	var recorded []Balance
	for _, b := range rows {
		// "Recorded" means the site reported something: an amount, a usage
		// figure, or unlimited. A row with none of them is a known site with no
		// reading, and stays null rather than becoming $0.
		if b.RemainingUSD != nil || b.UsedUSD != nil || b.Unlimited {
			recorded = append(recorded, b)
		}
	}
	out.Recorded = len(recorded)
	out.Partial = len(recorded) > 0 && len(recorded) < len(rows)
	switch {
	case len(rows) == 0:
		// No relay site at all: readBalances already named why.
		if out.Reason == "" {
			out.Reason = ReasonNoRelaySite
		}
		return out
	case len(rows) == 1 && len(recorded) == 1:
		b := recorded[0]
		out.Available = true
		out.Reason = ""
		out.Detail = ""
		out.Unlimited = b.Unlimited
		out.Currency = b.Currency
		if b.RemainingUSD != nil {
			v := *b.RemainingUSD
			out.RemainingUSD = &v
		}
		if b.UsedUSD != nil {
			v := *b.UsedUSD
			out.UsedUSD = &v
		}
		if !b.At.IsZero() {
			at := b.At.UTC()
			out.At = &at
		}
		if out.RemainingUSD == nil && out.UsedUSD == nil && !out.Unlimited {
			// Unreachable while "recorded" above means one of the three, but a
			// future field change must not turn an empty reading into $0.
			out.Available = false
			out.Reason = ReasonBalanceNotReport
			out.At = nil
		}
		return out
	case len(rows) == 1:
		// One known key that never recorded a balance.
		if out.Reason == "" {
			out.Reason = ReasonBalanceNotReport
		}
		return out
	default:
		// Several keys. Even when only one recorded an amount, that key is one
		// of several accounts that may share one upstream balance, so no scalar
		// is reported: every row keeps its own value, currency and timestamp.
		out.Available = false
		out.RemainingUSD = nil
		out.UsedUSD = nil
		out.Unlimited = false
		out.Currency = ""
		out.At = nil
		out.Reason = ReasonMultipleBalances
		if len(recorded) < len(rows) {
			out.Detail = fmt.Sprintf("%d relay keys exist and only %d recorded a balance; accounts are listed separately and never summed", len(rows), len(recorded))
		} else {
			out.Detail = fmt.Sprintf("%d relay keys recorded a balance; accounts are listed separately and never summed", len(recorded))
		}
		if cur, mixed := currenciesOf(recorded); mixed {
			out.Reason = ReasonMixedCurrency
			out.Detail = fmt.Sprintf("%d relay keys recorded balances in %s; amounts in different currencies are never combined", len(recorded), cur)
		}
		return out
	}
}

// currenciesOf reports the recorded currencies and whether they disagree.
// An empty currency is unknown, not a second currency: it only disagrees when
// the other rows name one, which is exactly when adding them up would be a
// guess about the unit.
func currenciesOf(rows []Balance) (string, bool) {
	var named []string
	blank := false
	for _, b := range rows {
		if b.Currency == "" {
			blank = true
			continue
		}
		seen := false
		for _, c := range named {
			if c == b.Currency {
				seen = true
				break
			}
		}
		if !seen {
			named = append(named, b.Currency)
		}
	}
	if len(named) > 1 {
		return strings.Join(named, "+"), true
	}
	if len(named) == 1 && blank {
		return named[0], true
	}
	return "", false
}
