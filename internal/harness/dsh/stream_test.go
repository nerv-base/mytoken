package dsh

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
)

// Real record shapes, shortened: a chunk carries its own absolute time, a packed
// run carries time0 plus per-member deltas.
func TestFirstTokenTime(t *testing.T) {
	const at = 1791201159021
	cases := []struct {
		name string
		data string
		want int64
	}{
		{"no stream", `{"turn":1,"step":1,"usage":{"outputTokens":5}}`, 0},
		{"empty stream", `{"stream":[]}`, 0},
		{"only blocks", `{"stream":[{"type":"chunk","time":10,"chunk":{"type":"block-start","index":0,"blockType":"reasoning"}}]}`, 0},
		{"text delta", `{"stream":[{"type":"chunk","time":11,"chunk":{"type":"text-delta","text":"Hi"}}]}`, 11},
		{"empty delta is not a token", `{"stream":[{"type":"chunk","time":11,"chunk":{"type":"text-delta","text":""}},{"type":"chunk","time":13,"chunk":{"type":"text-delta","text":"Hi"}}]}`, 13},
		{"tool call delta by args", `{"stream":[{"type":"chunk","time":12,"chunk":{"type":"tool-call-delta","argumentsDelta":"{\"a\":"}}]}`, 12},
		{"tool call delta by name", `{"stream":[{"type":"chunk","time":12,"chunk":{"type":"tool-call-delta","name":"read"}}]}`, 12},
		// A reasoning run whose first members are empty starts at the first
		// fragment that carries text: 1000 + 7 + 3.
		{"reasoning run", `{"stream":[{"type":"reasoning-chunks","time0":1000,"index":0,"dt":[7,3],"texts":["",""," owner"]}]}`, 1010},
		{"text run", `{"stream":[{"type":"text-chunks","time0":2000,"index":1,"dt":[4],"texts":[" there"]}]}`, 2000},
		{"tool run named up front", `{"stream":[{"type":"tool-call-chunks","time0":3000,"id":"c1","index":1,"name":"read","dt":[5],"args":["", "{\"p\""]}]}`, 3000},
		{"tool run unnamed", `{"stream":[{"type":"tool-call-chunks","time0":3000,"id":"c1","index":1,"dt":[5],"args":["", "{\"p\""]}]}`, 3005},
		{"run without text", `{"stream":[{"type":"text-chunks","time0":4000,"index":0,"dt":[1],"texts":["",""]}]}`, 0},
		// A conversation body can contain anything, including the shape of the
		// journal itself; only the top-level member is the stream.
		{"stream lookalike inside the message",
			`{"message":{"content":[{"type":"text","text":"{\"stream\":[{\"type\":\"chunk\",\"time\":9,\"chunk\":{\"type\":\"text-delta\",\"text\":\"x\"}}]}"}]},"stream":[{"type":"chunk","time":` + fmt.Sprint(at) + `,"chunk":{"type":"text-delta","text":"ok"}}]}`, at},
		{"escaped quotes and braces", `{"message":{"content":[{"type":"text","text":"a \" } ] b"}]},"stream":[{"type":"chunk","time":21,"chunk":{"type":"reasoning-delta","text":"\\\"}"}}]}`, 21},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := firstTokenTime([]byte(c.data)); got != c.want {
				t.Fatalf("firstTokenTime = %d, want %d", got, c.want)
			}
		})
	}
}

func TestDecodeWindow(t *testing.T) {
	line := []byte(`{"type":"assistant/message","seq":3,"time":5000,"data":{"usage":{"outputTokens":1},"stream":[{"type":"chunk","time":1000,"chunk":{"type":"text-delta","text":"Hi"}}]}}`)
	if got := decodeWindow(5000, line); got != 4000 {
		t.Fatalf("decodeWindow = %d, want 4000", got)
	}
	// A stream with no token, a missing time and a clock that runs backwards
	// all report no measurement rather than a guess.
	empty := []byte(`{"type":"assistant/message","time":5000,"data":{"stream":[]}}`)
	noData := []byte(`{"type":"assistant/message","time":5000}`)
	for _, c := range []struct {
		at   num
		line []byte
	}{
		{5000, empty},
		{0, line},
		{500, line},
		{5000, noData},
		{0, nil},
	} {
		if got := decodeWindow(c.at, c.line); got != 0 {
			t.Fatalf("decodeWindow(%d, %s) = %d, want 0", c.at, c.line, got)
		}
	}
}

// assistantMessageLog writes one plain session log whose single usage record
// carries the given data object.
func assistantMessageLog(t *testing.T, data string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "session.jsonl")
	log := `{"type":"session","version":4,"id":"session-abc","createdAt":1000,"cwd":"/w"}` + "\n" +
		`{"type":"assistant/message","seq":3,"time":5000,"data":` + data + `}` + "\n"
	if err := os.WriteFile(path, []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

// The parsed event carries the window DSH itself would divide by.
func TestParseRecordsDecodeWindow(t *testing.T) {
	dir, path := assistantMessageLog(t, `{"turn":1,"step":1,`+
		`"message":{"id":"msg-1","role":"assistant","source":{"kind":"model","provider":"deepseek-official","model":"deepseek-flash"}},`+
		`"usage":{"inputTokens":100,"outputTokens":50,"totalTokens":150},`+
		`"stream":[{"type":"chunk","time":1000,"chunk":{"type":"block-start","index":0,"blockType":"reasoning"}},`+
		`{"type":"reasoning-chunks","time0":1010,"index":0,"dt":[0,5],"texts":["","Hi"]}]}`)
	p := NewWithRoots(dir)
	b, err := p.Parse(context.Background(), harness.Source{Path: path, Kind: kindJSONL}, harness.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(b.Events))
	}
	if got := b.Events[0].DecodeMS; got != 3990 { // 5000 - 1010: the run's first member with text
		t.Fatalf("DecodeMS = %d, want 3990", got)
	}
	if got := b.Events[0].Tokens.Output; got != 50 {
		t.Fatalf("output = %d, want 50", got)
	}
}

// A log whose records predate stream timings (or a compaction summary, which has
// no stream at all) reports no window, never a fabricated one.
func TestParseWithoutStreamReportsNoWindow(t *testing.T) {
	dir, path := assistantMessageLog(t, `{"turn":1,"step":1,`+
		`"message":{"id":"msg-1","role":"assistant","source":{"kind":"model","provider":"p","model":"m"}},`+
		`"usage":{"inputTokens":100,"outputTokens":50,"totalTokens":150}}`)
	p := NewWithRoots(dir)
	b, err := p.Parse(context.Background(), harness.Source{Path: path, Kind: kindJSONL}, harness.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Events) != 1 || b.Events[0].DecodeMS != 0 {
		t.Fatalf("events = %+v, want one event without a window", b.Events)
	}
}

// SourceSessions must name the session the file's events belong to: the header
// id when the header exists, the directory name when it does not.
func TestSourceSessions(t *testing.T) {
	dir, path := assistantMessageLog(t, `{"usage":{"outputTokens":1}}`)
	p := NewWithRoots(dir)
	ids, err := p.SourceSessions(context.Background(), harness.Source{Path: path, Kind: kindJSONL})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "session-abc" {
		t.Fatalf("ids = %v, want [session-abc]", ids)
	}

	// No header yet: the same fallback Parse uses for a truncated log.
	headless := filepath.Join(dir, "session-xyz")
	if err := os.MkdirAll(headless, 0o755); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(headless, "session.jsonl")
	if err := os.WriteFile(bare, []byte(`{"type":"tool/call","seq":2,"time":7,"data":{}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ids, err = p.SourceSessions(context.Background(), harness.Source{Path: bare, Kind: kindJSONL})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "session-xyz" {
		t.Fatalf("ids = %v, want [session-xyz]", ids)
	}
}
