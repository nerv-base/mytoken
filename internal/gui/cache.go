package gui

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
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
	row query.SessionRow
}

func (s *State) loadCache() {
	want := s.ch.want
	f := query.Filter{Range: rangeOf(Span(s.span), s.Hooks.Now())}
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
			for _, b := range bs {
				if len(st.sessions) == 6 || b.USD <= 0 {
					break
				}
				h, id, ok := strings.Cut(b.Key, "/")
				if !ok {
					continue
				}
				row, _, _, err := q.Session(ctx, model.Harness(h), id)
				if err != nil {
					row = query.SessionRow{SessionMeta: model.SessionMeta{Harness: model.Harness(h), SessionID: id}}
				}
				st.sessions = append(st.sessions, cacheSession{b, row})
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
						cacheDays(c.Key("cache-days-"+ch.have), pal, ch.daily, 170)
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

// cacheDays stacks each day's expected prefix: read, then the misses by cause.
func cacheDays(c *ui.Context, pal palette, days []query.CachePoint, height float32) ui.Element {
	top := 0.0
	for _, d := range days {
		top = math.Max(top, float64(d.Expected))
	}
	top = niceMax(top)
	el := ui.Box(c).FillWidth().Height(height)
	g := entrance(el, 650*time.Millisecond)
	hx, hy, hover := el.PointerPosition()
	return el.Draw(func(p *ui.Painter, r ui.Rect) {
		const labelH = 18
		plot := ui.Rect{X: r.X, Y: r.Y, W: r.W, H: r.H - labelH}
		slot := plot.W / float32(len(days))
		bw := max(min(slot*0.62, 22), 2)
		hi := -1
		if hover {
			hi = max(0, min(int((hx-plot.X)/slot), len(days)-1))
		}
		step := max(1, len(days)/8)
		for i, d := range days {
			x := plot.X + slot*float32(i) + (slot-bw)/2
			y := plot.Y + plot.H
			seg := func(v int64, col ui.Color) {
				h := plot.H * g * float32(float64(v)/top)
				if h <= 0 {
					return
				}
				if hi >= 0 && hi != i {
					col = col.Alpha(0.4)
				}
				p.Fill(ui.Rect{X: x, Y: y - h, W: bw, H: h}, col, 2)
				y -= h
			}
			seg(hitTokens(d.CacheTotals), cacheColor.Alpha(0.75))
			for _, k := range causes {
				seg(causeOf(d.CacheTotals, k).Miss, causeColor(k))
			}
			if i%step == 0 {
				lbl := d.Day.Format("1/2")
				lw, _ := p.MeasureText(0, ui.Span{Text: lbl, Size: 10})
				p.RichText(x+bw/2-lw/2, plot.Y+plot.H+4, 0, ui.Span{Text: lbl, Size: 10, Color: pal.muted})
			}
		}
		if hi >= 0 {
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
			tooltipBox(p, pal, r, plot.X+slot*float32(hi)+slot/2, hy, d.Day.Format("2006-01-02"), rows, cols)
		}
	})
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
			s.Select(r.row.Harness, r.row.SessionID)
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

// cacheTimeline draws one bar per request: the prompt it sent, read back
// from the cache (green), missed (the cause's color) or simply new (faint).
// A compaction is a dashed line before the request it preceded.
func cacheTimeline(c *ui.Context, pal palette, steps []query.CacheStep, height float32) ui.Element {
	var top int64 = 1
	for _, st := range steps {
		top = max(top, st.Prompt+st.Read+st.Write)
	}
	el := ui.Box(c).FillWidth().Height(height)
	g := entrance(el, 650*time.Millisecond)
	hx, hy, hover := el.PointerPosition()
	return el.Draw(func(p *ui.Painter, r ui.Rect) {
		const labelH = 16
		plot := ui.Rect{X: r.X, Y: r.Y, W: r.W, H: r.H - labelH}
		slot := plot.W / float32(len(steps))
		bw := max(min(slot*0.7, 10), 1)
		hi := -1
		if hover {
			hi = max(0, min(int((hx-plot.X)/slot), len(steps)-1))
		}
		for i, st := range steps {
			x := plot.X + slot*float32(i) + (slot-bw)/2
			y := plot.Y + plot.H
			fade := hi >= 0 && hi != i
			seg := func(v int64, col ui.Color) {
				h := plot.H * g * float32(float64(v)/float64(top))
				if h <= 0 {
					return
				}
				if fade {
					col = col.Alpha(0.35)
				}
				p.Fill(ui.Rect{X: x, Y: y - h, W: bw, H: h}, col, 1.5)
				y -= h
			}
			if st.Expected < 0 {
				seg(st.Prompt+st.Read+st.Write, pal.ink.Alpha(0.12))
			} else {
				seg(st.Read, cacheColor)
				seg(min(st.Miss, st.Prompt), causeColor(st.Cause))
				seg(max(st.Prompt-st.Miss, 0)+st.Write, pal.ink.Alpha(0.18))
			}
			if st.Boundary != "" {
				bx := plot.X + slot*float32(i)
				for yy := plot.Y; yy < plot.Y+plot.H; yy += 6 {
					p.Line(bx, yy, bx, min(yy+3, plot.Y+plot.H), 1, Soyo)
				}
			}
		}
		p.Line(plot.X, plot.Y+plot.H, plot.X+plot.W, plot.Y+plot.H, 1, pal.faint)
		if n := len(steps); n > 0 {
			first, last := steps[0].At.Local().Format("15:04"), steps[n-1].At.Local().Format("15:04")
			p.RichText(plot.X, plot.Y+plot.H+3, 0, ui.Span{Text: first, Size: 10, Color: pal.muted})
			lw, _ := p.MeasureText(0, ui.Span{Text: last, Size: 10})
			p.RichText(plot.X+plot.W-lw, plot.Y+plot.H+3, 0, ui.Span{Text: last, Size: 10, Color: pal.muted})
		}
		if hi >= 0 {
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
			tooltipBox(p, pal, r, plot.X+slot*float32(hi)+slot/2, hy, title, rows, cols)
		}
	})
}
