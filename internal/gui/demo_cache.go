package gui

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

// The demo's cache ledger: a deterministic verdict for every demo request,
// shaped like the real one (docs/CACHE.md): most requests read the previous
// prompt back, idle gaps expire the cache, model switches start over, a
// compaction every so often re-sends the summary, and a few misses have no
// explanation at all.

type demoCache struct {
	steps map[string][]query.CacheStep // by harness/session, parallel to demo.events
}

var demoCaches sync.Map // *demo → *demoCache

func (d *demo) cache() *demoCache {
	if v, ok := demoCaches.Load(d); ok {
		return v.(*demoCache)
	}
	c := &demoCache{steps: map[string][]query.CacheStep{}}
	rnd := uint32(11)
	next := func() float64 { rnd = rnd*1664525 + 1013904223; return float64(rnd>>8) / float64(1<<24) }
	keys := make([]string, 0, len(d.events))
	for k := range d.events {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		evs := d.events[k]
		steps := make([]query.CacheStep, len(evs))
		// Like the real ledger, each model keeps its own cache: a request
		// reads back what the last one on its model sent.
		last := map[string]*query.AttributedEvent{}
		var prev *query.AttributedEvent
		for i := range evs {
			e := &evs[i]
			p := e.Tokens.Input + e.Tokens.CacheRead + e.Tokens.CacheWrite
			st := query.CacheStep{At: e.Timestamp, Model: e.Model, Write: e.Tokens.CacheWrite}
			if prev != nil {
				ref, seen := last[e.Model], last[e.Model] != nil
				if !seen {
					ref = prev
				}
				st.Expected = min(ref.Tokens.Input+ref.Tokens.CacheRead+ref.Tokens.CacheWrite, p-st.Write)
				switch {
				case !seen:
					st.Cause, st.Miss = "model", st.Expected
				case e.Timestamp.Sub(ref.Timestamp) > 5*time.Minute:
					st.Cause, st.Miss = "ttl", st.Expected
				case i%23 == 0:
					st.Boundary = string(model.BoundaryCompact)
					st.Cause, st.Miss = "compact", st.Expected*85/100
				case next() < 0.05:
					st.Cause, st.Miss = "unexplained", int64(float64(st.Expected)*(0.3+next()*0.7))
				}
			}
			st.Read = st.Expected - st.Miss
			st.Prompt = max(p-st.Read-st.Write, 0)
			st.USD = float64(st.Miss) * demoInputPrice(e.Model) / 1e6 * 0.9
			steps[i] = st
			last[e.Model], prev = e, e
		}
		c.steps[k] = steps
	}
	v, _ := demoCaches.LoadOrStore(d, c)
	return v.(*demoCache)
}

func demoInputPrice(name string) float64 {
	for _, ms := range demoModels {
		for _, m := range ms {
			if m.model == name {
				return m.in
			}
		}
	}
	return 1
}

// eachStep walks the demo requests in f with their verdicts.
func (d *demo) eachStep(f query.Filter, fn func(s *query.SessionRow, e *query.AttributedEvent, st query.CacheStep)) {
	c := d.cache()
	if !demoWantsInstance(f) {
		return
	}
	for i := range d.sessions {
		s := &d.sessions[i]
		k := string(s.Harness) + "/" + s.SessionID
		steps := c.steps[k]
		for j := range d.events[k] {
			e := &d.events[k][j]
			if inRange(f.Range, e.Timestamp) && j < len(steps) && demoWantsProvider(f.ProviderIDs, e) {
				fn(s, e, steps[j])
			}
		}
	}
}

// cacheAcc sums verdicts into CacheTotals.
type cacheAcc struct {
	t      query.CacheTotals
	causes map[string]*query.CacheCause
}

func (a *cacheAcc) add(st query.CacheStep) {
	a.t.Requests++
	if st.Expected < 0 {
		return
	}
	a.t.Cacheable++
	a.t.Expected += st.Expected
	a.t.Read += st.Read
	a.t.Miss += st.Miss
	a.t.USD += st.USD
	if st.Cause == "" {
		return
	}
	if a.causes == nil {
		a.causes = map[string]*query.CacheCause{}
	}
	c := a.causes[st.Cause]
	if c == nil {
		c = &query.CacheCause{Cause: st.Cause}
		a.causes[st.Cause] = c
	}
	c.Requests++
	c.Miss += st.Miss
	c.USD += st.USD
}

func (a *cacheAcc) totals() query.CacheTotals {
	t := a.t
	t.ByCause = nil
	for _, c := range a.causes {
		t.ByCause = append(t.ByCause, *c)
	}
	sort.Slice(t.ByCause, func(i, j int) bool { return t.ByCause[i].Miss > t.ByCause[j].Miss })
	return t
}

func (d *demo) CacheTotals(_ context.Context, f query.Filter) (query.CacheTotals, error) {
	var a cacheAcc
	d.eachStep(f, func(_ *query.SessionRow, _ *query.AttributedEvent, st query.CacheStep) { a.add(st) })
	return a.totals(), nil
}

func (d *demo) CacheDaily(_ context.Context, f query.Filter) ([]query.CachePoint, error) {
	days := map[time.Time]*cacheAcc{}
	d.eachStep(f, func(_ *query.SessionRow, e *query.AttributedEvent, st query.CacheStep) {
		t := e.Timestamp.Local()
		day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
		a := days[day]
		if a == nil {
			a = &cacheAcc{}
			days[day] = a
		}
		a.add(st)
	})
	out := make([]query.CachePoint, 0, len(days))
	for day, a := range days {
		out = append(out, query.CachePoint{Day: day, CacheTotals: a.totals()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day.Before(out[j].Day) })
	return out, nil
}

func (d *demo) CacheBy(_ context.Context, f query.Filter, dim query.CacheDim) ([]query.CacheBucket, error) {
	by := map[string]*cacheAcc{}
	d.eachStep(f, func(s *query.SessionRow, e *query.AttributedEvent, st query.CacheStep) {
		var k string
		switch dim {
		case query.CacheByProvider:
			k = e.ResolvedProvider
		case query.CacheByModel:
			k = e.Model
		case query.CacheByHarness:
			k = string(s.Harness)
		case query.CacheByProject:
			k = s.Project
		default:
			k = query.CacheSessionKey(string(s.Harness), s.SessionID)
		}
		a := by[k]
		if a == nil {
			a = &cacheAcc{}
			by[k] = a
		}
		a.add(st)
	})
	out := make([]query.CacheBucket, 0, len(by))
	for k, a := range by {
		out = append(out, query.CacheBucket{Key: k, CacheTotals: a.totals()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].USD != out[j].USD {
			return out[i].USD > out[j].USD
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

func (d *demo) SessionCache(_ context.Context, h model.Harness, session string) ([]query.CacheStep, error) {
	return d.cache().steps[string(h)+"/"+session], nil
}
