package store

import (
	"context"
	"time"
)

// RelayModelUsage is one (harness, provider, model) triple that local usage
// actually produced. Relay snapshot rules are generated per provider, so the
// triple is what scopes a model to a site: a provider like "pi/kami" only
// serves models that the pi harness really sent to it.
type RelayModelUsage struct {
	Harness  string
	Provider string
	Model    string
}

// dayStamp truncates a time to the local calendar day in the same unit
// daily_usage.day uses (UnixNano, local timezone). It mirrors the registered
// mytoken_day scalar so a Go-side window lines up with the aggregate table.
func dayStamp(t time.Time) int64 {
	local := t.In(time.Local)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local).UnixNano()
}

// RelayModelScopes returns the distinct (harness, provider, model) triples
// recorded in daily_usage since the given time.
//
// daily_usage is session-aggregated, so its row count is far smaller than the
// event count: a 214K-event database collapses to ~2.4K rows and this query
// answers in ~2ms. It reads daily_usage rather than events because events is a
// compatibility view over the fact/dimension tables with no model index any
// more; scanning it would touch every event row.
//
// The window is advisory: it decides which models a relay price snapshot is
// worth generating rules for, so rows written after the last daily rollup only
// mean a model is missed for one sync and picked up by the next one.
func (s *Store) RelayModelScopes(ctx context.Context, since time.Time) ([]RelayModelUsage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT harness,resolved_provider,model FROM daily_usage WHERE day>=? ORDER BY harness,resolved_provider,model`, dayStamp(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RelayModelUsage{}
	for rows.Next() {
		var u RelayModelUsage
		if err := rows.Scan(&u.Harness, &u.Provider, &u.Model); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// RelayBillModels returns the distinct model names that the bills stored for
// one relay key mention. Bills are the ground truth for what a site actually
// charged, so a model seen only in bills still deserves a snapshot rule even
// when no local usage reached daily_usage under it yet.
func (s *Store) RelayBillModels(ctx context.Context, origin, keyID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT model FROM relay_bills WHERE origin=? AND key_id=? AND model!='' ORDER BY model`, origin, keyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}
