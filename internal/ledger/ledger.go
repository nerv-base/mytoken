// Package ledger conservatively infers cache misses from token-only session history.
package ledger

import (
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
)

type Cause string

const (
	CauseNone        Cause = ""
	CauseCompact     Cause = "compact"
	CauseTTL         Cause = "ttl"
	CauseModel       Cause = "model"
	CauseUnexplained Cause = "unexplained"
)

// Request is one event of a session chain, in time order.
type Request struct {
	At                           time.Time
	Harness, Provider, Model     string
	Input, CacheRead, CacheWrite int64
	Boundary                     model.Boundary
}

// Entry is the ledger verdict for the request at the same index.
type Entry struct {
	Expected, Miss int64
	Cause          Cause
	Uncacheable    bool
}

// Version invalidates persisted verdicts whenever inference semantics change.
const Version = 1

// Analyze returns one verdict per request without mutating the time-ordered chain.
func Analyze(chain []Request) []Entry {
	entries := make([]Entry, len(chain))
	observable := false
	for _, r := range chain {
		if r.CacheRead > 0 || r.CacheWrite > 0 {
			observable = true
			break
		}
	}
	if !observable {
		for i := range entries {
			entries[i].Uncacheable = true
		}
		return entries
	}
	// Coordinate compression uses future lengths, never future cache evidence.
	// Trees contain only already-observed events. Keeping every prefix avoids a
	// bounded window forgetting an infrequent branch and blaming its next visit.
	type key struct {
		harness, provider, model string
		all                      bool
	}
	type state struct {
		prefixes, reads prefixIndex
		last            int
		rule            rules
		single          *state
		multiple        bool
	}
	groups := make(map[key]*state)
	states := make([][2]*state, len(chain))
	for i, r := range chain {
		for lane := 0; lane < 2; lane++ {
			k := key{r.Harness, r.Provider, r.Model, lane == 1}
			if lane == 1 {
				k.model = ""
			}
			g := groups[k]
			if g == nil {
				g = &state{rule: cacheRules(r)}
				groups[k] = g
			}
			if lane == 0 {
				g.prefixes.values = append(g.prefixes.values, cachedPrefix(r, g.rule))
				if r.CacheRead > 0 {
					g.reads.values = append(g.reads.values, r.CacheRead)
				}
			} else if g.single == nil {
				g.single = states[i][0]
			} else if g.single != states[i][0] {
				g.multiple = true
			}
			states[i][lane] = g
		}
	}
	for i, r := range chain {
		route := states[i][1]
		if route.multiple {
			route.prefixes.values = append(route.prefixes.values, cachedPrefix(r, states[i][0].rule))
		} else {
			states[i][1] = states[i][0]
		}
	}
	for _, g := range groups {
		g.prefixes.init()
		g.reads.init()
	}
	epoch := 1
	var starts [3]int
	for i, r := range chain {
		g, route := states[i][0], states[i][1]
		rules := g.rule
		p := prompt(r)
		cutoff := r.At.Add(-rules.ttl)
		slot := 0
		if rules.ttl == 10*time.Minute {
			slot = 1
		} else if rules.ttl == time.Hour {
			slot = 2
		}
		for starts[slot] < i && chain[starts[slot]].At.Before(cutoff) {
			starts[slot]++
		}
		liveStart := max(epoch, starts[slot]+1)
		live := min(p, g.prefixes.last(p+tolerance(p), liveStart))
		expired, other := int64(0), int64(0)
		sameModel := g.last >= epoch
		if live == 0 {
			expired = min(p, g.prefixes.last(p+tolerance(p), epoch))
		}
		if !sameModel {
			other = min(p, route.prefixes.last(p+tolerance(p), epoch))
		}
		// A previously observed CacheRead may be a persistent shared-system prefix,
		// not a full historical prompt (pi's recurring 4480-token floor is one example).
		shrink := confirmedShrink(chain, i, rules)
		needBranch := live == 0 || live-r.CacheRead > tolerance(live) || shrink
		branch := needBranch && r.CacheRead >= rules.minimum && (g.prefixes.near(r.CacheRead, liveStart) || g.reads.near(r.CacheRead, liveStart))
		e := Entry{}
		explicit := r.Boundary == model.BoundaryCompact || r.Boundary == model.BoundaryClear
		compact := explicit || (!branch && shrink)
		cause := CauseNone
		switch {
		case i == 0:
			// Even a boundary on the first observed event has no paid predecessor.
		case compact:
			e.Expected, cause = p, CauseCompact
		case branch && live > r.CacheRead+tolerance(live):
			e.Expected = r.CacheRead
		case branch && live == 0:
			e.Expected = r.CacheRead
		case live > 0:
			e.Expected, cause = live/rules.block*rules.block, CauseUnexplained
		case expired > 0:
			e.Expected, cause = expired/rules.block*rules.block, CauseTTL
		case !sameModel && other > 0:
			e.Expected, cause = other/rules.block*rules.block, CauseModel
		}
		if e.Expected < rules.minimum {
			e.Expected = 0
		}
		e.Miss = max(0, e.Expected-r.CacheRead)
		if e.Miss < tolerance(e.Expected) {
			e.Miss = 0
		}
		if e.Miss > 0 {
			e.Cause = cause
		}
		entries[i] = e
		if compact {
			epoch = i + 1
		}
		g.prefixes.add(cachedPrefix(r, rules), i+1)
		if r.CacheRead > 0 {
			g.reads.add(r.CacheRead, i+1)
		}
		g.last = i + 1
		if route != g {
			route.prefixes.add(cachedPrefix(r, rules), i+1)
		}
	}
	return entries
}

// prefixIndex is a segment tree of latest observation sequence numbers, keyed
// by token length. last finds the longest prefix below a cap within an epoch/TTL.
// All queries and updates are O(log n); zero means no observed evidence.
type prefixIndex struct {
	values []int64
	latest []int
	size   int
}

func (p *prefixIndex) init() {
	slices.Sort(p.values)
	p.values = slices.Compact(p.values)
	p.size = 1
	for p.size < len(p.values) {
		p.size *= 2
	}
	p.latest = make([]int, p.size*2)
}
func (p *prefixIndex) add(value int64, sequence int) {
	i := sort.Search(len(p.values), func(i int) bool { return p.values[i] >= value }) + p.size
	for i > 0 {
		p.latest[i] = sequence
		i /= 2
	}
}
func (p *prefixIndex) last(cap int64, sequence int) int64 {
	limit := sort.Search(len(p.values), func(i int) bool { return p.values[i] > cap }) - 1
	if limit < 0 || p.latest[1] < sequence {
		return 0
	}
	index := p.find(1, 0, p.size-1, limit, sequence)
	if index < 0 {
		return 0
	}
	return p.values[index]
}
func (p *prefixIndex) find(node, left, right, limit, sequence int) int {
	if left > limit || p.latest[node] < sequence {
		return -1
	}
	if left == right {
		return left
	}
	middle := (left + right) / 2
	if result := p.find(node*2+1, middle+1, right, limit, sequence); result >= 0 {
		return result
	}
	return p.find(node*2, left, middle, limit, sequence)
}
func (p *prefixIndex) near(value int64, sequence int) bool {
	slack := tolerance(value)
	found := p.last(value+slack-1, sequence)
	return found > 0 && found > value-slack
}

type rules struct {
	block, minimum int64
	ttl            time.Duration
}

func cacheRules(r Request) rules {
	// Model family takes precedence over an arbitrary relay display name.
	name := strings.ToLower(r.Model)
	provider := strings.ToLower(r.Provider)
	switch {
	case strings.Contains(name, "claude"):
		minimum := int64(1024)
		if strings.Contains(name, "opus") {
			minimum = 2048
		}
		return rules{1, minimum, 5 * time.Minute}
	case strings.Contains(name, "deepseek"):
		return rules{128, 1024, time.Hour}
	case strings.Contains(name, "gpt") || strings.HasPrefix(name, "o1") || strings.HasPrefix(name, "o3") || strings.HasPrefix(name, "o4") || strings.Contains(name, "codex"):
		return rules{128, 1024, 10 * time.Minute}
	case strings.Contains(provider, "anthropic"):
		return rules{1, 2048, 5 * time.Minute}
	case strings.Contains(provider, "deepseek"):
		return rules{128, 1024, time.Hour}
	case strings.Contains(provider, "openai"):
		return rules{128, 1024, 10 * time.Minute}
	default:
		return rules{128, 1024, 5 * time.Minute}
	}
}

func prompt(r Request) int64  { return max(0, r.Input) + max(0, r.CacheRead) + max(0, r.CacheWrite) }
func tolerance(p int64) int64 { return max(256, p/50) }

// Anthropic caches only explicit breakpoints. Uncached trailing Input is not
// evidence of a written prefix and must not be charged on the next request.
func cachedPrefix(r Request, rule rules) int64 {
	if rule.block == 1 {
		return max(0, r.CacheRead) + max(0, r.CacheWrite)
	}
	return prompt(r)
}

// A drop alone is NOT compaction. Require two subsequent same-route requests
// reusing the new prompt and staying below 60% of the old size; known branches
// take priority. This is intentionally unavailable at the incomplete tail.
func confirmedShrink(chain []Request, i int, rule rules) bool {
	if i == 0 || i+2 >= len(chain) {
		return false
	}
	r, prev := chain[i], chain[i-1]
	p, old := prompt(r), prompt(prev)
	if p < rule.minimum || p >= old*3/5 || prev.Model != r.Model || prev.Provider != r.Provider || prev.Harness != r.Harness || r.At.Sub(prev.At) > rule.ttl {
		return false
	}
	for _, next := range chain[i+1 : i+3] {
		if next.Model != r.Model || next.Provider != r.Provider || next.Harness != r.Harness || next.Boundary != "" || next.At.Sub(r.At) > rule.ttl || prompt(next) < p || prompt(next) >= old*3/5 || next.CacheRead < p-tolerance(p) {
			return false
		}
		p = prompt(next)
	}
	return true
}

// Counterfactual moves miss tokens from CacheWrite first, then Input, into CacheRead.
// It preserves all other event fields, including reported (not estimated) costs.
func Counterfactual(e model.UsageEvent, miss int64) model.UsageEvent {
	miss = max(0, miss)
	write := min(miss, max(0, e.Tokens.CacheWrite))
	input := min(miss-write, max(0, e.Tokens.Input))
	e.Tokens.CacheWrite -= write
	e.Tokens.Input -= input
	e.Tokens.CacheRead += write + input
	return e
}
