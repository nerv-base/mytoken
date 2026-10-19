package query

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/provider"
)

// ProviderMember retains original attribution for pricing/account drilldown.
// It is not a billing account identity; balances must never be summed here.
type ProviderMember struct {
	Harness  model.Harness `json:"harness"`
	Provider string        `json:"provider"`
}

type ProviderGroup struct {
	Bucket
	Identified bool             `json:"identified"`
	Members    []ProviderMember `json:"members"`
}

// Display identity key prefixes. displayProvider is the only producer of these
// keys, so a Filter.ProviderIDs entry matching neither prefix cannot match any
// event and needs no dictionary lookup.
const (
	providerIDEndpointPrefix   = "endpoint:v1:"
	providerIDUnresolvedPrefix = "unresolved:v1:"
)

func displayProvider(v AttributedEvent) (key, label string, identified bool) {
	if id, ok := provider.EndpointID(v.BaseURL); ok {
		label, _ = provider.NormalizeEndpoint(v.BaseURL)
		return id, label, true
	}
	// Unknown endpoints cannot establish cross-session/source equivalence.
	// Hash raw input rather than exposing possible userinfo/query credentials.
	// The instance is part of the tuple: two data sources that log the same
	// unnormalizable URL are still unrelated, and merging them would sum two
	// unknowns into one identity. The session is deliberately NOT part of the
	// tuple: a session is a unit of work, not an endpoint identity, so
	// including it would split one provider's history into one
	// unknown-provider row per session.
	raw, _ := json.Marshal([]string{string(v.Harness), v.InstanceID, v.ResolvedProvider, v.BaseURL})
	hash := sha256.Sum256(raw)
	label = v.ResolvedProvider
	if label == "" {
		label = "unknown"
	}
	return providerIDUnresolvedPrefix + hex.EncodeToString(hash[:]), label, false
}

// providerIDSelector matches events whose display identity is one of the
// requested IDs. A nil selector does not constrain anything. Matching is exact:
// an unknown or malformed ID selects nothing rather than falling back to every
// event, and ProviderIDs stays orthogonal to the pricing-name Providers filter
// because both predicates must hold for an event to survive.
type providerIDSelector struct{ ids map[string]bool }

func newProviderIDSelector(ids []string) *providerIDSelector {
	if len(ids) == 0 {
		return nil
	}
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return &providerIDSelector{ids: m}
}

func (p *providerIDSelector) match(v AttributedEvent) bool {
	if p == nil {
		return true
	}
	key, _, _ := displayProvider(v)
	return p.ids[key]
}

// providerIdentityPredicate renders a SQL predicate (with its bound args)
// selecting the events whose display identity is one of f.ProviderIDs, plus
// whether Filter.ProviderIDs was set: when it was, the predicate is complete,
// so the degenerate "0" means "matches nothing" rather than "no filter".
//
// displayProvider hashes in Go, so an identity cannot be a SQL expression.
// Instead the predicate is derived from event_dimensions, where every distinct
// identity appears once:
//
//   - an endpoint identity is a function of base_url alone. It becomes a
//     bounded `base_url IN (...)` list with one bound parameter per distinct
//     spelling in the dictionary, not one per session;
//   - an unresolved identity hashes
//     (harness, instance_id, resolved_provider, base_url), so every session that
//     logged the same unresolved endpoint maps to one dictionary row and becomes
//     a row-value tuple. Both instance_id and resolved_provider must stay in that
//     tuple: dropping resolved_provider lets two pricing names in one session and
//     base URL share a tuple and leak each other's cache facts, and dropping
//     instance_id merges two data sources' unrelated unknowns. session_id must
//     stay out of it so one provider's history is one identity, not one per
//     session.
//
// The argument count is therefore bounded by the distinct endpoint spellings
// plus the requested unresolved IDs; it never grows with the number of sessions
// and cannot approach SQLite's bound-parameter limit. The dictionary scan only
// runs for the prefixes actually requested.
//
// This runs on the caller's transaction. Calling s.load here would open a second
// transaction on the single-connection pool and deadlock.
func providerIdentityPredicate(ctx context.Context, tx *sql.Tx, f Filter) (predicate string, args []any, restricted bool, err error) {
	sel := newProviderIDSelector(f.ProviderIDs)
	if sel == nil {
		return "", nil, false, nil
	}
	needEndpoint, needUnresolved := false, false
	for id := range sel.ids {
		switch {
		case strings.HasPrefix(id, providerIDEndpointPrefix):
			needEndpoint = true
		case strings.HasPrefix(id, providerIDUnresolvedPrefix):
			needUnresolved = true
		}
	}
	endpoints := map[string]bool{}
	if needEndpoint {
		rows, e := tx.QueryContext(ctx, "SELECT DISTINCT base_url FROM event_dimensions")
		if e != nil {
			return "", nil, true, e
		}
		for rows.Next() {
			var raw string
			if e = rows.Scan(&raw); e != nil {
				rows.Close()
				return "", nil, true, e
			}
			if id, ok := provider.EndpointID(raw); ok && sel.ids[id] {
				endpoints[raw] = true
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return "", nil, true, e
		}
	}
	tuples := map[[4]string]bool{}
	if needUnresolved {
		rows, e := tx.QueryContext(ctx, "SELECT DISTINCT harness, instance_id, resolved_provider, base_url FROM event_dimensions")
		if e != nil {
			return "", nil, true, e
		}
		for rows.Next() {
			var v AttributedEvent
			if e = rows.Scan(&v.Harness, &v.InstanceID, &v.ResolvedProvider, &v.BaseURL); e != nil {
				rows.Close()
				return "", nil, true, e
			}
			if key, _, identified := displayProvider(v); !identified && sel.ids[key] {
				tuples[[4]string{string(v.Harness), v.InstanceID, v.ResolvedProvider, v.BaseURL}] = true
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return "", nil, true, e
		}
	}
	var clauses []string
	if len(endpoints) > 0 {
		urls := make([]string, 0, len(endpoints))
		for u := range endpoints {
			urls = append(urls, u)
		}
		sort.Strings(urls)
		clauses = append(clauses, "base_url IN ("+strings.TrimSuffix(strings.Repeat("?,", len(urls)), ",")+")")
		for _, u := range urls {
			args = append(args, u)
		}
	}
	if len(tuples) > 0 {
		rows := make([][4]string, 0, len(tuples))
		for t := range tuples {
			rows = append(rows, t)
		}
		sort.Slice(rows, func(i, j int) bool {
			for k := 0; k < 4; k++ {
				if rows[i][k] != rows[j][k] {
					return rows[i][k] < rows[j][k]
				}
			}
			return false
		})
		var b strings.Builder
		b.WriteString("(harness, instance_id, resolved_provider, base_url) IN (VALUES ")
		for i, t := range rows {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString("(?, ?, ?, ?)")
			args = append(args, t[0], t[1], t[2], t[3])
		}
		b.WriteString(")")
		clauses = append(clauses, b.String())
	}
	if len(clauses) == 0 {
		return "0", nil, true, nil
	}
	return "(" + strings.Join(clauses, " OR ") + ")", args, true, nil
}

func (s *service) ProviderGroups(ctx context.Context, f Filter) ([]ProviderGroup, error) {
	d, err := s.load(ctx, f, loadMode{identity: true})
	if err != nil {
		return nil, err
	}
	type group struct {
		value    ProviderGroup
		sessions map[sessionKey]bool
		members  map[ProviderMember]bool
	}
	groups := map[string]*group{}
	for _, v := range d.events {
		key, label, identified := displayProvider(v)
		if alias := d.providerAliases[key]; alias != "" {
			label = alias
		}
		g := groups[key]
		if g == nil {
			g = &group{value: ProviderGroup{Bucket: Bucket{Key: key, Label: label}, Identified: identified}, sessions: map[sessionKey]bool{}, members: map[ProviderMember]bool{}}
			groups[key] = g
		}
		g.value.Tokens = g.value.Tokens.Add(v.Tokens)
		g.value.Requests += v.requests
		g.value.Unpriced += v.unpriced
		g.value.CostUSD += v.Cost
		g.sessions[d.root(sessionKey{v.Harness, v.InstanceID, v.SessionID})] = true
		g.members[ProviderMember{v.Harness, v.ResolvedProvider}] = true
	}
	out := make([]ProviderGroup, 0, len(groups))
	for _, g := range groups {
		g.value.Sessions = int64(len(g.sessions))
		for member := range g.members {
			g.value.Members = append(g.value.Members, member)
		}
		sort.Slice(g.value.Members, func(i, j int) bool {
			a, b := g.value.Members[i], g.value.Members[j]
			if a.Harness != b.Harness {
				return a.Harness < b.Harness
			}
			return a.Provider < b.Provider
		})
		out = append(out, g.value)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Tokens.Total() != b.Tokens.Total() {
			return a.Tokens.Total() > b.Tokens.Total()
		}
		return a.Key < b.Key
	})
	return out, nil
}
