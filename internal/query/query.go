// Package query is the read API shared by the UI and the CLI (docs/ARCHITECTURE.md#query).
// Service includes cost provenance and additive 0.4.x endpoint identity queries.
// NewService implements the API on top of internal/store.
package query

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
)

// Range is a half-open local-time interval [From, To). Zero values mean unbounded.
type Range struct {
	From, To time.Time
}

// Filter narrows every query. Empty slices mean "all".
type Filter struct {
	Range     Range
	Harnesses []model.Harness
	// Providers matches the resolved pricing provider name. It keeps the
	// historical pricing identity and never merges different endpoints.
	Providers []string
	// ProviderIDs matches the display identity keys returned by
	// ProviderGroups: "endpoint:v1:…" for a recognized endpoint and
	// "unresolved:v1:…" for one that cannot establish equivalence. It is
	// orthogonal to Providers: when both are set only events matching both are
	// returned. Matching is exact, so a malformed or unknown ID yields no
	// results rather than everything, and an unknown endpoint stays selectable
	// through the exact ID ProviderGroups returned for it.
	//
	// Endpoint identity is not stored in the daily_usage or cache_daily rollups,
	// so a non-empty ProviderIDs always reads facts from events instead of the
	// rollup. Session(ctx, h, id) and SessionCache(ctx, h, id) take no Filter and
	// therefore cannot express this filter.
	ProviderIDs []string
	// InstanceIDs matches the data source instance an event was ingested from.
	// It is optional and orthogonal to every other field: an empty slice means
	// "all instances", which is today's behaviour when only one exists, and a
	// set slice narrows every aggregate (totals, series, buckets, sessions,
	// cache and provider groups) to those instances. Old input that never
	// carried an instance is stored under store.DefaultInstanceID, so
	// Filter{InstanceIDs: []string{store.DefaultInstanceID}} selects exactly the
	// pre-0.4 history.
	InstanceIDs []string
	Models      []string
	Project     string
}

// Totals is the aggregate over a filter.
type Totals struct {
	Tokens   model.Tokens `json:"tokens"`
	CostUSD  float64      `json:"costUsd"`
	Requests int64        `json:"requests"`
	Sessions int64        `json:"sessions"`
	// Unpriced counts requests with neither a log cost nor a known price;
	// CostUSD excludes them. The UI shows "—" when Unpriced == Requests.
	Unpriced int64 `json:"unpriced"`
	// CacheHit is model.Tokens.HitRate(): CacheRead / (Input + CacheRead +
	// CacheWrite - CacheMissing); 0 when that denominator is 0.
	CacheHit float64 `json:"cacheHit"`
}

// Point is one bucket of a time series. Day is the local start of the day (Daily)
// or of the hour (Hourly).
type Point struct {
	Day     time.Time    `json:"t"`
	Tokens  model.Tokens `json:"tokens"`
	CostUSD float64      `json:"costUsd"`
}

// Bucket is one row of a ranking. Key is the stable id, Label the display name.
type Bucket struct {
	Key      string       `json:"key"`
	Label    string       `json:"label"`
	Tokens   model.Tokens `json:"tokens"`
	CostUSD  float64      `json:"costUsd"`
	Requests int64        `json:"requests"`
	Sessions int64        `json:"sessions"`
	Unpriced int64        `json:"unpriced"` // see Totals.Unpriced
}

// BranchKey builds the grouping key for the branch dimension. A branch is only
// meaningful together with the repository it was checked out in — two
// repositories may both be on "main" — so the key is the pair and the two parts
// are joined with a byte that cannot appear in a path or a branch name. The
// repository part is the store's canonical project, so linked worktrees of one
// repository already fold together before the branch is appended.
func BranchKey(project, branch string) string {
	return project + "\x00" + branch
}

// SplitBranchKey reverses BranchKey. An empty branch returns as an empty string
// rather than as a missing value, because "no branch was recorded" is itself a
// reportable state and must stay distinguishable from a wrong branch.
func SplitBranchKey(key string) (project, branch string) {
	if i := strings.IndexByte(key, 0); i >= 0 {
		return key[:i], key[i+1:]
	}
	return key, ""
}

// ProviderModel is a session's usage split by provider × model.
type ProviderModel struct {
	Provider string             `json:"provider"`
	Model    string             `json:"model"`
	Attrib   model.AttribSource `json:"attrib"`
	Tokens   model.Tokens       `json:"tokens"`
	CostUSD  float64            `json:"costUsd"`
	Requests int64              `json:"requests"`
	Unpriced int64              `json:"unpriced"` // see Totals.Unpriced
}

// SessionRow is a session with its aggregates. For a parent session, Tokens,
// CostUSD and Requests INCLUDE its children (subagents); Breakdown too.
type SessionRow struct {
	model.SessionMeta
	// InstanceID is the data source instance this session was read from. It is
	// additive provenance: two instances can hold the same harness and session
	// id as two independent sessions, and this field tells them apart. Rows
	// written before 0.4 report store.DefaultInstanceID.
	InstanceID string          `json:"instanceId"`
	Tokens     model.Tokens    `json:"tokens"`
	CostUSD    float64         `json:"costUsd"`
	Requests   int64           `json:"requests"`
	Unpriced   int64           `json:"unpriced"` // see Totals.Unpriced
	Children   int             `json:"children"`
	Breakdown  []ProviderModel `json:"breakdown"` // sorted by Tokens.Total() desc
}

// CostSource identifies monetary provenance, independently of provider attribution.
// relay-bill is reserved for authoritative relay billing integrations.
type CostSource string

const (
	CostSourceLog      CostSource = "log"
	CostSourceEstimate CostSource = "estimate"
	CostSourceUnpriced CostSource = "unpriced"
)

// AttributedEvent is a usage event with its resolved provider and cost.
type AttributedEvent struct {
	model.UsageEvent
	CostSource            CostSource `json:"costSource"`
	requests, unpriced    int64
	costLog, costEstimate float64
	ResolvedProvider      string             `json:"resolvedProvider"`
	Attrib                model.AttribSource `json:"attrib"`
	Cost                  float64            `json:"cost"`
	Priced                bool               `json:"priced"` // false: no log cost and no known price
	// InstanceID is the data source instance the request was ingested from,
	// additive provenance for the same reasons as SessionRow.InstanceID.
	InstanceID string `json:"instanceId"`
}

// Session sort keys.
const (
	SortRecent = "recent" // UpdatedAt desc (default)
	SortTokens = "tokens" // Tokens.Total() desc
	SortCost   = "cost"   // CostUSD desc
)

// Service is the read API. Sessions lists only top-level sessions (ParentID == "")
// whose events intersect the filter.
type Service interface {
	Totals(ctx context.Context, f Filter) (Totals, error)
	Daily(ctx context.Context, f Filter) ([]Point, error)  // one point per local day in range, zero-filled
	Hourly(ctx context.Context, f Filter) ([]Point, error) // one point per local hour in range, zero-filled
	ByProvider(ctx context.Context, f Filter) ([]Bucket, error)
	// ProviderGroups groups display identities without changing pricing labels.
	ProviderGroups(ctx context.Context, f Filter) ([]ProviderGroup, error)
	ByModel(ctx context.Context, f Filter) ([]Bucket, error)
	ByProject(ctx context.Context, f Filter) ([]Bucket, error)
	ByHarness(ctx context.Context, f Filter) ([]Bucket, error)
	// Usage is the range grouped by local day, tool, model and provider.
	Usage(ctx context.Context, f Filter) ([]UsageRow, error)
	// HourlyUsage is Usage bucketed by local hour: each row's Day is the start
	// of a local clock hour (time.Local), grouped by harness, model and provider.
	HourlyUsage(ctx context.Context, f Filter) ([]UsageRow, error)
	Sessions(ctx context.Context, f Filter, sort string, limit, offset int) (rows []SessionRow, total int, err error)
	// Session returns the session, its children, and its own events (not children's), time-ordered.
	Session(ctx context.Context, h model.Harness, id string) (SessionRow, []SessionRow, []AttributedEvent, error)
	// Subscribe notifies (throttled to ≤1/s) whenever new data is committed.
	Subscribe() (<-chan struct{}, func())
}

// ErrAmbiguousInstance reports that a session id exists in more than one
// instance, so the instance-less Session(ctx, h, id) and
// SessionCache(ctx, h, id) calls cannot choose. Callers must retry with an
// explicit instance through SessionIn or SessionCacheIn; the legacy calls never
// silently pick an arbitrary instance, because that would attribute another
// data source's requests to this session.
var ErrAmbiguousInstance = errors.New("query: session exists in more than one instance; use SessionIn or SessionCacheIn")

// InstanceService is the instance-aware extension of Service. It is deliberately
// a separate interface so that existing Service implementations (the GUI demo,
// test doubles) keep compiling unchanged: callers type-assert when they need an
// explicit instance, and Service itself is untouched.
type InstanceService interface {
	Service
	// SessionIn is Session for one explicit instance.
	SessionIn(ctx context.Context, h model.Harness, instance, id string) (SessionRow, []SessionRow, []AttributedEvent, error)
}

// SessionIn resolves one session in one explicit instance. It is a free
// function so that callers holding a plain Service do not have to know about
// InstanceService, and it fails clearly when the implementation is not the
// SQL-backed service instead of silently falling back to an instance-less read.
func SessionIn(ctx context.Context, s Service, h model.Harness, instance, id string) (SessionRow, []SessionRow, []AttributedEvent, error) {
	is, ok := s.(InstanceService)
	if !ok {
		return SessionRow{}, nil, nil, fmt.Errorf("query: %T does not support instance-aware session lookups", s)
	}
	return is.SessionIn(ctx, h, instance, id)
}
