package gui

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/chart"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/store"
)

// The cache page is the cache ledger (docs/CACHE.md): of the prompt that
// should have been read back from the provider's cache, how much was, how
// much was sent again in full, why, and what that cost.

// cacheColor is the page's own: Rana's green, the color of a cache read.
var cacheColor = Rana

// causes are the miss causes in the order the page lists them.
var causes = []string{"compact", "ttl", "model", "unexplained"}

func causeColor(cause string) ui.Color {
	switch cause {
	case "compact":
		return Soyo
	case "ttl":
		return Tomori
	case "model":
		return Taki
	case "unexplained":
		return Anon
	}
	return cacheColor
}

// cacheState backs the cache page.
type cacheState struct {
	want, have string // span key of the data wanted / shown
	err        error
	tot        query.CacheTotals
	daily      []query.CachePoint
	providers  []query.CacheBucket
	sessions   []cacheSession
}

// cacheSession is a costly session with its row, for the title.
type cacheSession struct {
	query.CacheBucket
	// ref is the session the bucket's key names, instance included: the same
	// session id in another instance is another session.
	ref sessionRef
	row query.SessionRow
}

// cacheSessionLimit caps the sessions list on the cache page.
const cacheSessionLimit = 6

// parseCacheSessionKey reads back the key query.CacheSessionKeyIn builds: the
// reserved default instance keeps the historical "harness/session" shape, any
// other instance is "instance:<id>:harness/session". The reverse is lossy for
// ids that contain the separators themselves; nothing in the store forbids
// that, so a key that does not parse is skipped rather than guessed at.
func parseCacheSessionKey(key string) (sessionRef, bool) {
	inst := store.DefaultInstanceID
	if rest, ok := strings.CutPrefix(key, "instance:"); ok {
		i, tail, ok := strings.Cut(rest, ":")
		if !ok || i == "" {
			return sessionRef{}, false
		}
		inst, key = i, tail
	}
	h, id, ok := strings.Cut(key, "/")
	if !ok || h == "" || id == "" {
		return sessionRef{}, false
	}
	return sessionRef{Harness: model.Harness(h), InstanceID: inst, SessionID: id}, true
}

// cacheSessionsFrom resolves the sessions behind the cache-by-session buckets:
// the buckets are ordered by miss cost, and the list stops at the first
// session that cost nothing.
func cacheSessionsFrom(ctx context.Context, q query.Service, bs []query.CacheBucket) []cacheSession {
	var out []cacheSession
	for _, b := range bs {
		if len(out) == cacheSessionLimit || b.USD <= 0 {
			break
		}
		ref, ok := parseCacheSessionKey(b.Key)
		if !ok {
			continue
		}
		row, err := sessionRowIn(ctx, q, ref)
		if err != nil {
			row = query.SessionRow{SessionMeta: model.SessionMeta{Harness: ref.Harness, SessionID: ref.SessionID}}
		}
		out = append(out, cacheSession{CacheBucket: b, ref: ref, row: row})
	}
	return out
}

func (s *State) loadCache() {
	want := s.ch.want
	f := query.Filter{Range: rangeOf(Span(s.span), s.Hooks.Now(), s.location())}
	q := s.Q
	s.run("cache", func(ctx context.Context) func() {
		var st cacheState
		st.tot, st.err = q.CacheTotals(ctx, f)
		if st.err == nil {
			st.daily, st.err = q.CacheDaily(ctx, f)
		}
		if st.err == nil {
			st.providers, st.err = q.CacheBy(ctx, f, query.CacheByProvider)
		}
		if st.err == nil {
			var bs []query.CacheBucket
			bs, st.err = q.CacheBy(ctx, f, query.CacheBySession)
			if st.err == nil {
				st.sessions = cacheSessionsFrom(ctx, q, bs)
			}
		}
		return func() {
			if s.ch.want != want {
				return // the span moved on; a newer load follows
			}
			st.want, st.have = want, want
			s.ch = st
		}
	})
}

// hitOf is the share of the expected prefix that was read from the cache.
// A request may read more than was expected (a prefix shared with another
// session), so hits are what was expected and not missed.
func hitOf(t query.CacheTotals) float64 {
	if t.Expected <= 0 {
		return 0
	}
	return float64(hitTokens(t)) / float64(t.Expected)
}

// hitTokens is the expected prefix that was not missed.
func hitTokens(t query.CacheTotals) int64 {
	return max(t.Expected-t.Miss, 0)
}

// unexplainedOf is the miss nothing on our side explains, over Expected: a
// relay that drops the cache shows up here.
func unexplainedOf(t query.CacheTotals) float64 {
	if t.Expected <= 0 {
		return 0
	}
	for _, c := range t.ByCause {
		if c.Cause == "unexplained" {
			return float64(c.Miss) / float64(t.Expected)
		}
	}
	return 0
}

func causeOf(t query.CacheTotals, cause string) query.CacheCause {
	for _, c := range t.ByCause {
		if c.Cause == cause {
			return c
		}
	}
	return query.CacheCause{Cause: cause}
}

func (s *State) cachePage(c *ui.Context, pal palette) {
	pageHeader(c, pal, "cache", tr("cacheSub"), func() { s.spanSwitch(c, pal) })
	if want := spanKeys[clampSpan(s.span)]; s.ch.want != want {
		s.ch.want = want
		s.loadCache()
	}
	ch := s.ch
	ui.Scroll(c).Grow(1).Children(func() {
		ui.Column(c).Padding(8, 8, 16, 8).Gap(14).Children(func() {
			switch {
			case ch.have == "":
				pane(c, pal).Padding(60).Center().Children(func() { ui.Spinner(c) })
				return
			case ch.err != nil:
				pane(c, pal).Padding(40).Children(func() { emptyState(c, pal, artPicks, tr("failed"), ch.err.Error()) })
				return
			case ch.tot.Expected <= 0:
				pane(c, pal).Padding(40).Children(func() { emptyState(c, pal, artStage, tr("noCache"), tr("noCacheSub")) })
				return
			}
			rise(c, 0, func() { cacheHero(c, pal, ch.tot) })
			if len(ch.daily) > 1 {
				rise(c, 1, func() {
					card(c, pal, tr("cacheByDay"), func() { causeLegend(c, pal) }, func() {
						cacheDays(c.Key("cache-days-"+ch.have), pal, ch.daily, 170, tr("cacheByDay"))
					})
				})
			}
			rise(c, 2, func() {
				ui.Row(c).Gap(14).AlignItems(ui.Stretch).Children(func() {
					card(c, pal, tr("cacheCauses"), nil, func() { causeTable(c, pal, ch.tot) }).Grow(1).Basis(0)
					card(c, pal, tr("cacheProviders"), nil, func() { cacheProviders(c, pal, ch.providers) }).Grow(1).Basis(0)
				})
			})
			if len(ch.sessions) > 0 {
				rise(c, 3, func() {
					card(c, pal, tr("cacheSessions"), nil, func() { s.cacheSessions(c, pal, ch.sessions) })
				})
			}
		})
	})
}

// cacheHero: the hit rate, the tokens sent again, what they cost, and how
// much of it nothing explains; under it one bar splits the expected prefix.
func cacheHero(c *ui.Context, pal palette, t query.CacheTotals) {
	pane(c, pal).Padding(22, 26).Gap(18).Children(func() {
		ui.Row(c).Gap(40).AlignItems(ui.End).Children(func() {
			ui.Column(c).Gap(4).Children(func() {
				kicker(c, pal, tr("cacheHitExp"))
				ui.Text(c, fmtPct(hitOf(t))).Font(display).FontSize(44).FontWeight(780).TextColor(cacheColor).FontFeatures("tnum")
			})
			ui.Column(c).Gap(4).Children(func() {
				kicker(c, pal, tr("cacheMissed"))
				ui.Text(c, fmtTokens(t.Miss)).Font(display).FontSize(30).FontWeight(750).TextColor(pal.ink).FontFeatures("tnum")
			})
			ui.Column(c).Gap(4).Children(func() {
				kicker(c, pal, tr("cacheExtra"))
				ui.Text(c, fmtCost(t.USD)).Font(display).FontSize(30).FontWeight(750).TextColor(Anon).FontFeatures("tnum")
			})
			ui.Spacer(c)
			ui.Column(c).Gap(8).AlignItems(ui.End).Children(func() {
				miniStat(c, pal, tr("cacheUnexplained"), fmtPct(unexplainedOf(t)))
				miniStat(c, pal, tr("cacheRequests"), fmtInt(t.Cacheable))
			})
		})
		expectedBar(c, pal, t, 12)
		ui.Text(c, trf("cacheHeroSub", fmtTokens(t.Expected), fmtTokens(t.Read))).FontSize(11.5).TextColor(pal.muted)
	})
}

// expectedBar is the expected prefix as one bar: read, then each cause's miss.
func expectedBar(c *ui.Context, pal palette, t query.CacheTotals, h float32) ui.Element {
	el := ui.Box(c).FillWidth().Height(h)
	g := entrance(el, 700*time.Millisecond)
	return el.Draw(func(p *ui.Painter, r ui.Rect) {
		p.Fill(r, pal.well, h/2)
		if t.Expected <= 0 {
			return
		}
		x := r.X
		seg := func(v int64, col ui.Color) {
			w := r.W * g * float32(float64(v)/float64(t.Expected))
			if w <= 0 {
				return
			}
			p.Fill(ui.Rect{X: x, Y: r.Y, W: max(w, 2), H: r.H}, col, h/2)
			x += w
		}
		seg(hitTokens(t), cacheColor)
		for _, k := range causes {
			seg(causeOf(t, k).Miss, causeColor(k))
		}
	})
}

func causeLegend(c *ui.Context, pal palette) {
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		for _, k := range append([]string{"read"}, causes...) {
			ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
				dot(c, causeColor(k), 7)
				ui.Text(c, tr("cause-"+k)).FontSize(11).TextColor(pal.muted)
			})
		}
	})
}

// cacheDays stacks each day's expected prefix on a band and a linear scale:
// read, then the misses by cause, with a tip by the chosen day.
func cacheDays(c *ui.Context, pal palette, days []query.CachePoint, height float32, name string) ui.Element {
	el := ui.Box(c).FillWidth().Height(height)
	g := entrance(el, 650*time.Millisecond)
	if len(days) == 0 {
		return el
	}
	var mx float64
	for _, d := range days {
		mx = math.Max(mx, float64(d.Expected))
	}
	labels := make([]string, len(days))
	step := max(1, len(days)/8)
	for i, d := range days {
		if i%step == 0 {
			labels[i] = d.Day.Format("1/2")
		}
	}
	x := chart.NewBand(labels...).Padding(0.38, 0)
	y := chart.NewLinear(0, math.Max(mx, 1)).Nice(4)
	series := [][]float64{make([]float64, len(days))}
	colors := []ui.Color{cacheColor.Alpha(0.75)}
	for i, d := range days {
		series[0][i] = float64(hitTokens(d.CacheTotals))
	}
	for _, k := range causes {
		row := make([]float64, len(days))
		for i, d := range days {
			row[i] = float64(causeOf(d.CacheTotals, k).Miss)
		}
		series = append(series, row)
		colors = append(colors, causeColor(k))
	}
	stacked := chart.Stack(series, chart.StackStandard)
	xs := make([]float32, len(days))
	for i := range days {
		xs[i] = x.At(i)
	}
	el.Children(func() {
		chart.Plot(c, chart.Axes{
			Bottom: x.Ticks(),
			Label:  func(side chart.Side, t chart.Tick) { tickLabel(c, pal, side, t) },
		}, func(area ui.Element) {
			area.Label(name)
			sel := ui.Local(area, "chartSel", func() int { return -1 })
			if sel == nil {
				none := -1
				sel = &none
			}
			chart.Select(c, area, sel, xs, func(i int) string {
				return days[i].Day.Format("2006-01-02") + " · " + fmtPct(hitOf(days[i].CacheTotals))
			})
			area.Draw(func(p *ui.Painter, r ui.Rect) {
				hi := *sel
				if hi >= len(days) {
					hi = -1
				}
				if hi >= 0 {
					sp := x.Band(hi)
					p.Fill(ui.Rect{X: r.X + sp.Lo*r.W, Y: r.Y, W: (sp.Hi - sp.Lo) * r.W, H: r.H}, pal.hover, 6)
				}
				for i := range days {
					sp := x.Band(i)
					bw := max(min((sp.Hi-sp.Lo)*r.W, 22), 2)
					bx := r.X + sp.Mid()*r.W - bw/2
					for si := range stacked {
						seg := stacked[si][i]
						if seg.Hi <= seg.Lo {
							continue
						}
						col := colors[si]
						if hi >= 0 && hi != i {
							col = col.Alpha(0.4)
						}
						y0 := chart.Y(r, y.At(seg.Lo*float64(g)))
						y1 := chart.Y(r, y.At(seg.Hi*float64(g)))
						p.Fill(ui.Rect{X: bx, Y: y1, W: bw, H: y0 - y1}, col, 2)
					}
				}
			})
			if hi := *sel; hi >= 0 && hi < len(days) {
				d := days[hi]
				rows := [][2]string{{tr("cacheHitExp"), fmtPct(hitOf(d.CacheTotals))}, {tr("cause-read"), fmtTokens(d.Read)}}
				cols := []ui.Color{pal.ink.Alpha(0.5), cacheColor}
				for _, k := range causes {
					if m := causeOf(d.CacheTotals, k).Miss; m > 0 {
						rows = append(rows, [2]string{tr("cause-" + k), fmtTokens(m)})
						cols = append(cols, causeColor(k))
					}
				}
				rows = append(rows, [2]string{tr("cacheExtra"), fmtCost(d.USD)})
				cols = append(cols, Anon)
				total := 0.0
				for si := range stacked {
					total = math.Max(total, stacked[si][hi].Hi)
				}
				h, v := tipAlign(hi, len(days), y.At(total*float64(g)))
				chart.Anchor(c, x.At(hi), max(y.At(total*float64(g)), 0.06), h, v, func() {
					tipCard(c, pal, d.Day.Format("2006-01-02"), rows, cols)
				})
			}
		}).FillWidth().Height(height)
	})
	return el
}

// causeTable explains each cause with its requests, tokens and money.
func causeTable(c *ui.Context, pal palette, t query.CacheTotals) {
	for i, k := range causes {
		cc := causeOf(t, k)
		if i > 0 {
			ui.Divider(c)
		}
		ui.Column(c).Gap(3).Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				dot(c, causeColor(k), 8)
				ui.Text(c, tr("cause-"+k)).FontSize(13).FontWeight(650).TextColor(pal.ink)
				ui.Text(c, trf("cacheTimes", fmtInt(cc.Requests))).FontSize(11).TextColor(pal.muted).FontFeatures("tnum")
				ui.Spacer(c)
				ui.Text(c, fmtTokens(cc.Miss)).FontSize(12).TextColor(pal.muted).FontFeatures("tnum")
				ui.Text(c, fmtCost(cc.USD)).FontSize(12.5).FontWeight(650).TextColor(pal.ink).FontFeatures("tnum").Width(64).TextAlign(ui.End)
			})
			ui.Text(c, tr("causesub-"+k)).FontSize(11).TextColor(pal.muted).LineHeight(1.4).Margin(0, 0, 0, 16)
		})
	}
}

// cacheProviders ranks providers by what misses cost there, with each one's
// hit rate and share of misses nothing explains.
func cacheProviders(c *ui.Context, pal palette, rows []query.CacheBucket) {
	if len(rows) == 0 {
		ui.Text(c, tr("noData")).FontSize(12).TextColor(pal.muted)
		return
	}
	for i, b := range rows {
		if i >= 7 {
			ui.Text(c, trf("relayMore", len(rows)-i)).FontSize(11).TextColor(pal.muted)
			break
		}
		name := b.Key
		if name == "" {
			name = tr("unknownProvider")
		}
		un := unexplainedOf(b.CacheTotals)
		ui.Column(c).Gap(5).Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, name).FontSize(12.5).FontWeight(620).TextColor(pal.ink).SingleLine().Ellipsis("…").Grow(1).Basis(0)
				if un >= 0.05 {
					chipOn(c, pal, trf("cacheUnexplainedChip", fmtPct(un)), Anon)
				}
				ui.Text(c, fmtPct(hitOf(b.CacheTotals))).FontSize(11.5).TextColor(pal.muted).FontFeatures("tnum")
				ui.Text(c, fmtCost(b.USD)).FontSize(12).FontWeight(650).TextColor(pal.ink).FontFeatures("tnum").Width(64).TextAlign(ui.End)
			})
			expectedBar(c, pal, b.CacheTotals, 5)
		})
	}
}

// cacheSessions lists the sessions whose misses cost the most; a click opens
// the session with its cache timeline.
func (s *State) cacheSessions(c *ui.Context, pal palette, rows []cacheSession) {
	for i, r := range rows {
		if i > 0 {
			ui.Divider(c)
		}
		title := r.row.Title
		if title == "" {
			title = r.row.SessionID
		}
		row := ui.Row(c.Key("cs-"+r.Key)).Gap(10).Padding(6, 8).Radius(10).AlignItems(ui.Center).Cursor(ui.CursorPointer).Role(ui.RoleButton).Transition(hoverFade)
		if row.Hovered() {
			row.Background(pal.hover)
		}
		row.Children(func() {
			harnessBadge(c, r.row.Harness, 26)
			if inst := r.ref.instanceLabel(); inst != "" {
				chip(c, inst, Soyo)
			}
			ui.Column(c).Gap(2).Grow(1).Basis(0).Children(func() {
				ui.Text(c, title).FontSize(13).FontWeight(620).TextColor(pal.ink).SingleLine().Ellipsis("…")
				sub := fmtPct(hitOf(r.CacheTotals)) + " · " + trf("cacheMissTokens", fmtTokens(r.Miss))
				if top := topCause(r.CacheTotals); top != "" {
					sub += " · " + tr("cause-"+top)
				}
				ui.Text(c, sub).FontSize(11).TextColor(pal.muted).SingleLine()
			})
			ui.Text(c, fmtCost(r.USD)).FontSize(13).FontWeight(700).TextColor(Anon).FontFeatures("tnum")
		})
		if row.Clicked() {
			s.page = "sessions"
			s.SelectRef(r.ref)
		}
	}
}

func topCause(t query.CacheTotals) string {
	best, cause := int64(0), ""
	for _, c := range t.ByCause {
		if c.Cause != "" && c.Miss > best {
			best, cause = c.Miss, c.Cause
		}
	}
	return cause
}

var icCache = icon(`<path d="M13 2 4 14h7l-1 8 9-12h-7z"/>`)

func init() {
	for k, v := range cacheStrs {
		strs[k] = v
	}
}

var cacheStrs = map[string][2]string{
	"cache":                {"缓存", "Cache"},
	"cacheSub":             {"该命中的缓存，命中了多少", "How much of the cache you should have hit, you did"},
	"noCache":              {"这段时间没有可缓存的请求", "No cacheable requests in this span"},
	"noCacheSub":           {"换一个时间段，或者等工具记下更多请求", "Pick another span, or wait for more requests"},
	"cacheHitExp":          {"应命中的命中率", "Hit rate (of expected)"},
	"cacheMissed":          {"重发的 token", "Tokens sent again"},
	"cacheExtra":           {"因此多花", "Extra cost"},
	"cacheUnexplained":     {"说不清的未命中", "Unexplained misses"},
	"cacheRequests":        {"参与判定的请求", "Requests judged"},
	"cacheHeroSub":         {"上一轮的提示本该从缓存读回 %s，实际读回 %s", "%s should have come back from the cache; %s did"},
	"cacheByDay":           {"按天", "By day"},
	"cacheCauses":          {"为什么没命中", "Why it missed"},
	"cacheProviders":       {"按供应商", "By provider"},
	"cacheSessions":        {"多花最多的会话", "Sessions that paid the most"},
	"cacheTimes":           {"%s 次", "%s×"},
	"cacheMissTokens":      {"重发 %s", "%s re-sent"},
	"cacheUnexplainedChip": {"%s 说不清", "%s unexplained"},
	"cacheTimeline":        {"缓存时间轴", "Cache timeline"},
	"cacheTimelineSub":     {"每条请求读回的缓存与该读回的差距", "What each request read back, against what it should have"},
	"cachePrompt":          {"提示", "Prompt"},
	"cacheExpected":        {"应命中", "Expected"},
	"cacheUncacheable":     {"不可缓存", "Not cacheable"},
	"unknownProvider":      {"未知供应商", "Unknown provider"},
	"cause-read":           {"命中", "Read"},
	"cause-compact":        {"压缩", "Compaction"},
	"cause-ttl":            {"过期", "Expired"},
	"cause-model":          {"换模型", "Model switch"},
	"cause-unexplained":    {"说不清", "Unexplained"},
	"causesub-compact":     {"对话被压缩，摘要成了新的前缀，只能重新写缓存", "The conversation was compacted; the summary is a new prefix, cached afresh"},
	"causesub-ttl":         {"两次请求隔得太久，缓存已过期（Anthropic 5 分钟/1 小时，OpenAI 约 10 分钟）", "Too long between requests; the cache expired (Anthropic 5 min / 1 h, OpenAI about 10 min)"},
	"causesub-model":       {"换了模型，缓存不跨模型", "The model changed; caches are per model"},
	"causesub-unexplained": {"前缀没变、也没过期，却没命中：常见于中转站不透传缓存或轮换上游", "Same prefix, not expired, still a miss: often a relay that drops the cache or rotates upstreams"},
}

// cacheJudged counts the steps the ledger judged (cacheable ones).
func cacheJudged(steps []query.CacheStep) int {
	n := 0
	for _, st := range steps {
		if st.Expected >= 0 {
			n++
		}
	}
	return n
}

// cacheSummary is the timeline card's trailing line: hit rate and extra cost.
func cacheSummary(c *ui.Context, pal palette, steps []query.CacheStep) {
	var t query.CacheTotals
	for _, st := range steps {
		if st.Expected > 0 {
			t.Expected += st.Expected
			t.Read += st.Read
		}
		t.USD += st.USD
	}
	ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
		ui.Text(c, tr("cacheHitExp")+" "+fmtPct(hitOf(t))).FontSize(11).TextColor(pal.muted).FontFeatures("tnum")
		if t.USD > 0 {
			ui.Text(c, tr("cacheExtra")+" "+fmtCost(t.USD)).FontSize(11).FontWeight(650).TextColor(Anon).FontFeatures("tnum")
		}
	})
}

// cacheTimeline draws one bar per request on a band and a linear scale: the
// prompt it sent, read back from the cache (green), missed (the cause's
// color) or simply new (faint), with a tip by the chosen request. A
// compaction is a dashed line before the request it preceded.
func cacheTimeline(c *ui.Context, pal palette, steps []query.CacheStep, height float32, name string) ui.Element {
	el := ui.Box(c).FillWidth().Height(height)
	g := entrance(el, 650*time.Millisecond)
	if len(steps) == 0 {
		return el
	}
	var top int64 = 1
	for _, st := range steps {
		top = max(top, st.Prompt+st.Read+st.Write)
	}
	labels := make([]string, len(steps))
	labels[0] = steps[0].At.Local().Format("15:04")
	if len(steps) > 1 {
		labels[len(steps)-1] = steps[len(steps)-1].At.Local().Format("15:04")
	}
	x := chart.NewBand(labels...).Padding(0.3, 0)
	y := chart.NewLinear(0, float64(top))
	// Read back, then the miss, then what was new plus what was written: an
	// uncacheable request is one faint segment in the third series.
	series := make([][]float64, 3)
	for si := range series {
		series[si] = make([]float64, len(steps))
	}
	for i, st := range steps {
		if st.Expected < 0 {
			series[2][i] = float64(st.Prompt + st.Read + st.Write)
			continue
		}
		series[0][i] = float64(st.Read)
		series[1][i] = float64(min(st.Miss, st.Prompt))
		series[2][i] = float64(max(st.Prompt-st.Miss, 0) + st.Write)
	}
	stacked := chart.Stack(series, chart.StackStandard)
	xs := make([]float32, len(steps))
	for i := range steps {
		xs[i] = x.At(i)
	}
	el.Children(func() {
		chart.Plot(c, chart.Axes{
			Bottom: x.Ticks(),
			Label:  func(side chart.Side, t chart.Tick) { tickLabel(c, pal, side, t) },
		}, func(area ui.Element) {
			area.Label(name)
			sel := ui.Local(area, "chartSel", func() int { return -1 })
			if sel == nil {
				none := -1
				sel = &none
			}
			chart.Select(c, area, sel, xs, func(i int) string {
				return steps[i].At.Local().Format("15:04:05") + " · " + steps[i].Model
			})
			area.Draw(func(p *ui.Painter, r ui.Rect) {
				hi := *sel
				if hi >= len(steps) {
					hi = -1
				}
				if hi >= 0 {
					sp := x.Band(hi)
					p.Fill(ui.Rect{X: r.X + sp.Lo*r.W, Y: r.Y, W: (sp.Hi - sp.Lo) * r.W, H: r.H}, pal.hover, 6)
				}
				for i, st := range steps {
					sp := x.Band(i)
					bw := max(min((sp.Hi-sp.Lo)*r.W, 10), 1)
					bx := r.X + sp.Mid()*r.W - bw/2
					fade := hi >= 0 && hi != i
					for si := range stacked {
						seg := stacked[si][i]
						if seg.Hi <= seg.Lo {
							continue
						}
						var col ui.Color
						switch {
						case si == 0:
							col = cacheColor
						case si == 1:
							col = causeColor(st.Cause)
						case st.Expected < 0:
							col = pal.ink.Alpha(0.12)
						default:
							col = pal.ink.Alpha(0.18)
						}
						if fade {
							col = col.Alpha(0.35)
						}
						y0 := chart.Y(r, y.At(seg.Lo*float64(g)))
						y1 := chart.Y(r, y.At(seg.Hi*float64(g)))
						p.Fill(ui.Rect{X: bx, Y: y1, W: bw, H: y0 - y1}, col, 1.5)
					}
					if st.Boundary != "" {
						bx := r.X + sp.Lo*r.W
						for yy := r.Y; yy < r.Y+r.H; yy += 6 {
							p.Line(bx, yy, bx, min(yy+3, r.Y+r.H), 1, Soyo)
						}
					}
				}
				p.Line(r.X, r.Y+r.H, r.X+r.W, r.Y+r.H, 1, pal.faint)
			})
			if hi := *sel; hi >= 0 && hi < len(steps) {
				st := steps[hi]
				rows := [][2]string{{tr("cachePrompt"), fmtTokens(st.Prompt + st.Read + st.Write)}, {tr("cause-read"), fmtTokens(st.Read)}}
				cols := []ui.Color{pal.ink.Alpha(0.4), cacheColor}
				switch {
				case st.Expected < 0:
					rows = append(rows, [2]string{tr("cacheExpected"), tr("cacheUncacheable")})
					cols = append(cols, pal.ink.Alpha(0.3))
				default:
					rows = append(rows, [2]string{tr("cacheExpected"), fmtTokens(st.Expected)})
					cols = append(cols, pal.ink.Alpha(0.4))
					if st.Miss > 0 {
						rows = append(rows, [2]string{tr("cause-" + st.Cause), fmtTokens(st.Miss)}, [2]string{tr("cacheExtra"), fmtCost(st.USD)})
						cols = append(cols, causeColor(st.Cause), Anon)
					}
				}
				title := st.At.Local().Format("15:04:05") + " · " + st.Model
				if st.Boundary != "" {
					title += " · " + tr("cause-compact")
				}
				total := 0.0
				for si := range stacked {
					total = math.Max(total, stacked[si][hi].Hi)
				}
				h, v := tipAlign(hi, len(steps), y.At(total*float64(g)))
				chart.Anchor(c, x.At(hi), max(y.At(total*float64(g)), 0.06), h, v, func() {
					tipCard(c, pal, title, rows, cols)
				})
			}
		}).FillWidth().Height(height)
	})
	return el
}
