package query

import "time"

// 0.3 缓存账本 read types (docs/CACHE.md §6). A "miss" is a request whose
// cacheable prefix should have been a CacheRead but was re-sent as fresh
// input (docs/CACHE.md §1). MissUSD is the extra cost that miss incurred.

// CacheCause is one per-cause slice of a CacheTotals breakdown (docs/CACHE.md §3).
type CacheCause struct {
	Cause    string  `json:"cause"` // "" (no miss) | compact | ttl | model | unexplained
	Requests int64   `json:"requests"`
	Miss     int64   `json:"miss"`
	USD      float64 `json:"usd"`
}

// CacheTotals aggregates cache-ledger verdicts over a filter.
// Requests counts every request; Cacheable counts only those in the ledger
// (harness/model with cache fields; excludes cold-start first requests from
// Expected/Miss but they are still cacheable). Expected is the should-have-read
// total, Read the actual CacheRead total, Miss the unexplained shortfall, USD
// the MissUSD total. ByCause breaks Miss down by cause (sorted by Miss desc).
type CacheTotals struct {
	Requests  int64        `json:"requests"`
	Cacheable int64        `json:"cacheable"`
	Expected  int64        `json:"expected"`
	Read      int64        `json:"read"`
	Miss      int64        `json:"miss"`
	USD       float64      `json:"usd"`
	ByCause   []CacheCause `json:"byCause"`
}

// CacheBucket is one ranking row: Key plus its CacheTotals (embedded, so the
// totals fields are promoted — docs/CACHE.md §6).
type CacheBucket struct {
	Key string `json:"key"`
	CacheTotals
}

// CachePoint is one day of a cache time series. Day is the local start of day.
type CachePoint struct {
	Day time.Time `json:"t"`
	CacheTotals
}

// CacheStep is one request's cache verdict inside a session timeline.
type CacheStep struct {
	At       time.Time `json:"at"`
	Model    string    `json:"model"`
	Prompt   int64     `json:"prompt"`   // Input (fresh, non-cached); total P = Prompt + Read + Write
	Read     int64     `json:"read"`     // CacheRead
	Write    int64     `json:"write"`    // CacheWrite
	Expected int64     `json:"expected"` // should-have-read; <0 means uncacheable
	Miss     int64     `json:"miss"`
	Cause    string    `json:"cause"`
	USD      float64   `json:"usd"`      // MissUSD
	Boundary string    `json:"boundary"` // "" | compact | clear
}

// CacheDim selects a CacheBy bucket key.
type CacheDim string

const (
	CacheByProvider CacheDim = "provider"
	CacheByModel    CacheDim = "model"
	CacheByHarness  CacheDim = "harness"
	CacheByProject  CacheDim = "project"
	CacheBySession  CacheDim = "session" // Key = harness + "/" + session_id
)

// CacheSessionKey builds the CacheBySession key for a harness/session pair.
func CacheSessionKey(harness, session string) string {
	return harness + "/" + session
}
