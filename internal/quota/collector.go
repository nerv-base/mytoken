package quota

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// maxTrackedLimitIDs bounds how many distinct limit IDs a Collector remembers
// across incremental reads. ReadCodex reports at most maxOtherLimitIDs of them;
// a collector keeps a few more so the selected record's own identity is not the
// only survivor. The set is deliberately small because it is persisted in a
// parser's cursor: quota bookkeeping must never grow with the length of a log.
const maxTrackedLimitIDs = maxOtherLimitIDs * 2

// maxCollectorStateBytes is the hard bound on the JSON a Collector writes into
// a cursor. It is far above what the caps above can produce; it exists so a
// cursor can never grow without bound even if a future field is added.
const maxCollectorStateBytes = 8 << 10

// Collector accumulates the quota observations of one append-ordered rollout
// across incremental reads.
//
// ReadCodex selects over a whole stream at once. A parser that must not reread
// its file uses a Collector instead: it hands each newly seen record's
// rate_limits payload to ObserveRateLimits, keeps the selection state in a
// cursor, and asks Report for the same Report ReadCodex would have produced.
// Selection rules are identical because both paths build snapshots with
// decodeRateLimits and feed them to the same selector and finalize.
type Collector struct {
	sel     selector
	agg     *issueLog
	unknown map[string]struct{}
	// seen records that a rate_limits payload was encountered at all. Report
	// returns nil until then, so a parser can attach a nil observation to a
	// batch that never mentioned quota.
	seen bool
}

// collectorState is the bounded, JSON-encodable memory a Collector needs to
// keep selecting the same observation across incremental reads. It holds at
// most two observations (the selected one and the newest window-bearing one),
// the distinct limit IDs (capped) and the aggregated issues (whose fields are
// static schema paths, so the list is bounded by the schema). No log text is
// stored.
type collectorState struct {
	Seen     bool      `json:"seen,omitempty"`
	Best     *snapshot `json:"best,omitempty"`
	Latest   *snapshot `json:"latest,omitempty"`
	LimitIDs []string  `json:"limit_ids,omitempty"`
	Issues   []Issue   `json:"issues,omitempty"`
}

// NewCollector returns an empty Collector.
func NewCollector() *Collector {
	c := &Collector{}
	c.init()
	return c
}

func (c *Collector) init() {
	if c.sel.limitCap == 0 {
		c.sel.limitCap = maxTrackedLimitIDs
	}
	if c.agg == nil {
		c.agg = newIssueLog()
	}
	if c.unknown == nil {
		c.unknown = make(map[string]struct{})
	}
}

// LoadCollector restores a Collector from EncodeState output. An empty or
// unreadable state yields an empty Collector: a cursor this package cannot
// interpret restarts selection from the records that follow, which is the same
// position ReadCodex would take on a stream whose earlier records are gone.
func LoadCollector(state string) *Collector {
	c := NewCollector()
	if strings.TrimSpace(state) == "" {
		return c
	}
	var st collectorState
	if err := json.Unmarshal([]byte(state), &st); err != nil {
		return c
	}
	c.seen = st.Seen
	c.sel.best = st.Best
	c.sel.latest = st.Latest
	c.sel.limitIDs = st.LimitIDs
	if len(st.LimitIDs) > 0 {
		c.sel.seenIDs = make(map[string]struct{}, len(st.LimitIDs))
		for _, id := range st.LimitIDs {
			c.sel.seenIDs[id] = struct{}{}
		}
	}
	c.agg.restore(st.Issues)
	return c
}

// EncodeState returns the bounded state a parser stores in its cursor. It is
// empty when no rate_limits payload has been seen, so a source that has none
// carries no quota bookkeeping at all.
func (c *Collector) EncodeState() string {
	if c == nil || !c.seen {
		return ""
	}
	st := collectorState{Seen: true, Best: c.sel.best, Latest: c.sel.latest, LimitIDs: c.sel.limitIDs}
	if c.agg != nil {
		st.Issues = c.agg.list()
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return ""
	}
	if len(raw) > maxCollectorStateBytes {
		// The caps above make this unreachable; dropping the bookkeeping keeps
		// a cursor bounded rather than letting one grow with a hostile log.
		return ""
	}
	return string(raw)
}

// ObserveRateLimits records one record's rate_limits payload. record is the
// record's static type path, stamp is its timestamp text (empty when it carried
// none), line is its 1-based line, and rateLimits is the raw payload value.
//
// A malformed or implausible stamp is recorded as an issue and yields no
// observation instant, exactly as ReadCodex does. An empty or null payload
// contributes nothing.
func (c *Collector) ObserveRateLimits(record, stamp string, line int, rateLimits json.RawMessage) {
	if c == nil {
		return
	}
	c.init()
	trimmed := bytes.TrimSpace(rateLimits)
	if len(trimmed) == 0 || bytes.Equal(trimmed, nullLiteral) {
		return
	}
	var at *time.Time
	if stamp != "" {
		if ts, reason := decodeTimestamp(stamp); reason == ReasonNone {
			at = &ts
		} else {
			c.agg.add("timestamp", reason, line)
		}
	}
	c.seen = true
	c.sel.observe(decodeRateLimits(trimmed, sanitize(record, maxLabelLen), at, line, c.agg, c.unknown))
}

// Report returns the observation the collected records select, or nil when no
// rate_limits payload was ever observed. It applies the same finalize rules
// ReadCodex applies, so a live incremental parse and an offline read of the
// same records agree on status, currency, and provenance.
func (c *Collector) Report(opts CodexOptions) *Report {
	if c == nil || !c.seen {
		return nil
	}
	c.init()
	rep := &Report{Source: sanitize(opts.Source, maxSourceLen)}
	finalize(rep, &c.sel, c.agg, opts)
	rep.UnknownFields = len(c.unknown)
	return rep
}

// restore rebuilds an issue log from its encoded form. Counts are preserved;
// the list order is irrelevant because list() sorts.
func (l *issueLog) restore(issues []Issue) {
	for _, iss := range issues {
		if iss.Count <= 0 {
			iss.Count = 1
		}
		key := iss.Field + "\x00" + string(iss.Reason)
		if _, ok := l.byKey[key]; ok {
			continue
		}
		l.byKey[key] = &Issue{Field: iss.Field, Reason: iss.Reason, Count: iss.Count, Line: iss.Line}
		l.order = append(l.order, key)
	}
}
