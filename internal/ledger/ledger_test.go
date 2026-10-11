package ledger

import (
	"reflect"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
)

func request(seconds int, prompt, read int64) Request {
	return Request{At: time.Unix(int64(seconds), 0), Harness: "test", Provider: "relay", Model: "gpt-test", Input: prompt - read, CacheRead: read}
}

func TestAnalyze(t *testing.T) {
	change := func(r Request, modelName string) Request { r.Model = modelName; return r }
	boundary := func(r Request, b model.Boundary) Request { r.Boundary = b; return r }
	provider := func(r Request) Request { r.Provider = "other"; return r }
	written := func(r Request) Request { r.CacheWrite = r.Input; r.Input = 0; return r }
	for _, tt := range []struct {
		name  string
		chain []Request
		index int
		want  Entry
	}{
		{"cold start", []Request{request(0, 8192, 4096)}, 0, Entry{}},
		{"uncacheable", []Request{request(0, 8192, 0), request(1, 9000, 0)}, 1, Entry{Uncacheable: true}},
		{"later cache makes chain observable", []Request{request(0, 8192, 0), request(1, 9000, 8192)}, 0, Entry{}},
		{"normal growth", []Request{request(0, 8192, 4096), request(1, 9000, 8192)}, 1, Entry{Expected: 8192}},
		{"unexplained", []Request{request(0, 8192, 4096), request(1, 9000, 1024)}, 1, Entry{Expected: 8192, Miss: 7168, Cause: CauseUnexplained}},
		{"zero read can miss", []Request{request(0, 8192, 4096), request(1, 9000, 0)}, 1, Entry{Expected: 8192, Miss: 8192, Cause: CauseUnexplained}},
		{"floor 128", []Request{request(0, 8201, 4096), request(1, 9000, 0)}, 1, Entry{Expected: 8192, Miss: 8192, Cause: CauseUnexplained}},
		{"dust below 256", []Request{request(0, 8192, 4096), request(1, 9000, 8000)}, 1, Entry{Expected: 8192}},
		{"relative tolerance", []Request{request(0, 100000, 4096), request(1, 101000, 98560)}, 1, Entry{Expected: 99968}},
		{"minimum length", []Request{request(0, 800, 400), request(1, 900, 0)}, 1, Entry{}},
		{"TTL expiry", []Request{request(0, 8192, 4096), request(601, 9000, 0)}, 1, Entry{Expected: 8192, Miss: 8192, Cause: CauseTTL}},
		{"TTL equality is live", []Request{request(0, 8192, 4096), request(600, 9000, 0)}, 1, Entry{Expected: 8192, Miss: 8192, Cause: CauseUnexplained}},
		{"new model", []Request{request(0, 8192, 4096), change(request(1, 9000, 0), "gpt-other")}, 1, Entry{Expected: 8192, Miss: 8192, Cause: CauseModel}},
		{"provider change is incomparable", []Request{request(0, 8192, 4096), provider(request(1, 9000, 0))}, 1, Entry{}},
		{"explicit compact", []Request{request(0, 8192, 4096), boundary(request(1, 4096, 1024), model.BoundaryCompact)}, 1, Entry{Expected: 4096, Miss: 3072, Cause: CauseCompact}},
		{"explicit clear", []Request{request(0, 8192, 4096), boundary(request(1, 4096, 1024), model.BoundaryClear)}, 1, Entry{Expected: 4096, Miss: 3072, Cause: CauseCompact}},
		{"boundary applied once", []Request{request(0, 8192, 4096), boundary(request(1, 4096, 1024), model.BoundaryCompact), request(2, 5000, 4096)}, 2, Entry{Expected: 4096}},
		{"resume is not compact", []Request{request(0, 8192, 4096), boundary(request(1, 9000, 0), model.BoundaryResume)}, 1, Entry{Expected: 8192, Miss: 8192, Cause: CauseUnexplained}},
		{"known short full prefix", []Request{request(0, 4096, 1024), request(1, 8192, 4096), request(2, 10000, 4096)}, 2, Entry{Expected: 4096}},
		{"known shared read prefix", []Request{request(0, 9102, 4480), request(1, 9356, 4480), request(2, 9556, 9344), request(3, 15787, 4480)}, 3, Entry{Expected: 4480}},
		{"short side request is not compaction", []Request{request(0, 16000, 4096), request(1, 5000, 4096), request(2, 18000, 15872)}, 1, Entry{Expected: 4096}},
		{"shrinking without evidence is cold branch", []Request{request(0, 16000, 4096), request(1, 3000, 1024)}, 1, Entry{}},
		{"confirmed shrink", []Request{request(0, 16000, 4096), request(1, 5000, 0), request(2, 5500, 4992), request(3, 6000, 5504)}, 1, Entry{Expected: 5000, Miss: 5000, Cause: CauseCompact}},
		{"anthropic exact prefix", []Request{written(change(request(0, 8201, 3000), "claude-sonnet")), change(request(1, 9000, 0), "claude-sonnet")}, 1, Entry{Expected: 8201, Miss: 8201, Cause: CauseUnexplained}},
		{"opus higher minimum", []Request{change(request(0, 1500, 1100), "claude-opus-5-5"), change(request(1, 2000, 0), "claude-opus-5-5")}, 1, Entry{}},
		{"anthropic TTL", []Request{written(change(request(0, 8192, 4096), "claude-sonnet")), change(request(301, 9000, 0), "claude-sonnet")}, 1, Entry{Expected: 8192, Miss: 8192, Cause: CauseTTL}},
		{"deepseek disk TTL", []Request{change(request(0, 8192, 4096), "deepseek-flash"), change(request(601, 9000, 0), "deepseek-flash")}, 1, Entry{Expected: 8192, Miss: 8192, Cause: CauseUnexplained}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := append([]Request(nil), tt.chain...)
			got := Analyze(tt.chain)
			if len(got) != len(tt.chain) || got[tt.index] != tt.want {
				t.Fatalf("got %+v, want at %d %+v", got, tt.index, tt.want)
			}
			if !reflect.DeepEqual(before, tt.chain) {
				t.Fatal("mutated input")
			}
		})
	}
	if len(Analyze(nil)) != 0 {
		t.Fatal("nonempty nil analysis")
	}
}

func TestAnthropicUncachedSuffix(t *testing.T) {
	first := Request{At: time.Unix(0, 0), Model: "claude-opus-5-5", Input: 12185, CacheWrite: 60789}
	next := Request{At: time.Unix(11, 0), Model: "claude-opus-5-5", Input: 26, CacheRead: 60789, CacheWrite: 15523}
	got := Analyze([]Request{first, next})[1]
	if got != (Entry{Expected: 60789}) {
		t.Fatalf("uncached suffix must not be charged: %+v", got)
	}
}

func TestDistantBranch(t *testing.T) {
	chain := []Request{request(0, 8192, 4480)}
	for i := 1; i <= 200; i++ {
		chain = append(chain, request(i, int64(9000+i*128), int64(8704+i*128)))
	}
	chain = append(chain, request(201, 60000, 4480))
	got := Analyze(chain)[len(chain)-1]
	if got != (Entry{Expected: 4480}) {
		t.Fatalf("forgot live historical branch: %+v", got)
	}
}

func TestCounterfactual(t *testing.T) {
	cost := 3.0
	e := model.UsageEvent{Model: "gpt-test", Boundary: model.BoundaryCompact, CostUSD: &cost, Tokens: model.Tokens{Input: 1000, CacheWrite: 400, CacheRead: 300, Output: 20, Reasoning: 10}}
	for _, tt := range []struct{ miss, input, write, read int64 }{{-1, 1000, 400, 300}, {0, 1000, 400, 300}, {200, 1000, 200, 500}, {600, 800, 0, 900}, {9999, 0, 0, 1700}} {
		got := Counterfactual(e, tt.miss)
		want := e
		want.Tokens.Input = tt.input
		want.Tokens.CacheWrite = tt.write
		want.Tokens.CacheRead = tt.read
		if got != want || got.Tokens.Total() != e.Tokens.Total() {
			t.Fatalf("miss %d: %+v", tt.miss, got)
		}
	}
}

func TestPrefixIndex(t *testing.T) {
	values := []int64{0, 1, 2, 5, 5, 13, 21, 34, 55, 89, 144}
	index := prefixIndex{values: append([]int64(nil), values...)}
	index.init()
	seen := map[int64]int{}
	for sequence := 1; sequence <= 100; sequence++ {
		value := values[(sequence*7)%len(values)]
		seen[value] = sequence
		index.add(value, sequence)
		for cap := int64(0); cap <= 150; cap++ {
			for _, cutoff := range []int{1, max(1, sequence-10), sequence, sequence + 1} {
				var want int64
				for v, s := range seen {
					if v <= cap && s >= cutoff {
						want = max(want, v)
					}
				}
				if got := index.last(cap, cutoff); got != want {
					t.Fatalf("sequence=%d cap=%d cutoff=%d got=%d want=%d", sequence, cap, cutoff, got, want)
				}
			}
		}
	}
}

func BenchmarkAnalyze200K(b *testing.B) {
	chain := make([]Request, 200000)
	for i := range chain {
		p := int64(8192 + (i%10000)*128)
		chain[i] = request(i, p, p-128)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		Analyze(chain)
	}
}
