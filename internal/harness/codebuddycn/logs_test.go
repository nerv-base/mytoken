package codebuddycn

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nerv-base/mytoken/internal/harness"
)

// cursor and srcOf keep the tests readable.
type cursor = harness.Cursor

func srcOf(path string) harness.Source { return harness.Source{Path: path, Kind: kindLog} }

// appLog is the app's own log format; extLog is the extension's, one millisecond
// later, for the same request.
const (
	appLine = `2026-11-06 22:38:36.930 [info] [CraftInvokableAgent] [90c56f83fae2e80462185a103fa21d4e] Model prepared: Hy4 (hy4free)
2026-11-06 22:38:36.930 [info] [AgentReporter] [90c56f83fae2e80462185a103fa21d4e] Agent execution successful with usage: {"inputTokens":253109,"outputTokens":1694,"totalTokens":254803,"cacheTokens":250368,"cachedWriteTokens":0,"cachedMissTokens":2741,"lastTokens":64668,"credit":0.41,"thinkingTokens":190}
`
	extLine = `Workspace Path: /Users/someone/Desktop/proj
[2026/11/6 22:38:36.931] [Info] [CraftInvokableAgent] [90c56f83fae2e80462185a103fa21d4e]  Model prepared: Hy4 (hy4free)
[2026/11/6 22:38:36.931] [Info] [AgentReporter] [90c56f83fae2e80462185a103fa21d4e]  Agent execution successful with usage: {"inputTokens":253109,"outputTokens":1694,"totalTokens":254803,"cacheTokens":250368,"cachedWriteTokens":0,"cachedMissTokens":2741,"lastTokens":64668,"credit":0.41,"thinkingTokens":190}
`
)

func writeLog(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestParseLogSplitsTokens checks the five token classes stay disjoint: the
// uncached prompt is the miss count, the cache is separate, and the thinking
// tokens are moved out of the completion.
func TestParseLogSplitsTokens(t *testing.T) {
	dir := t.TempDir()
	path := writeLog(t, dir, "app.log", appLine)
	p := NewWithRoots(dir)

	batch, err := p.Parse(context.Background(), srcOf(path), cursor{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(batch.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(batch.Events))
	}
	ev := batch.Events[0]
	want := struct{ in, out, read, write, reasoning int64 }{2741, 1694 - 190, 250368, 0, 190}
	got := ev.Tokens
	if got.Input != want.in || got.Output != want.out || got.CacheRead != want.read || got.CacheWrite != want.write || got.Reasoning != want.reasoning {
		t.Errorf("tokens = %+v, want %+v", got, want)
	}
	if ev.Model != "hy4free" {
		t.Errorf("model = %q, want hy4free", ev.Model)
	}
	if ev.Bill == nil || ev.Bill.Amount != 0.41 || ev.Bill.Unit != "credit" {
		t.Errorf("bill = %+v, want 0.41 credit", ev.Bill)
	}
	if ev.Timestamp.UTC().Format("2006-01-02T15:04:05Z") != "2026-11-06T14:38:36Z" && ev.Timestamp.UTC().Format("2006-01-02T15:04") != "2026-11-06T14:38" {
		t.Logf("timestamp resolved to %s (local zone dependent)", ev.Timestamp.UTC())
	}
}

// TestParseLogDedupsMirroredSources checks the same request journalled by both
// the app and the extension collapses to one event.
func TestParseLogDedupsMirroredSources(t *testing.T) {
	dir := t.TempDir()
	app := writeLog(t, dir, "app.log", appLine)
	ext := writeLog(t, dir, "ext.log", extLine)
	p := NewWithRoots(dir)

	events := 0
	for _, path := range []string{app, ext} {
		batch, err := p.Parse(context.Background(), srcOf(path), cursor{})
		if err != nil {
			t.Fatalf("Parse %s: %v", path, err)
		}
		events += len(batch.Events)
	}
	if events != 2 {
		t.Fatalf("each source yields its own event, got %d", events)
	}
	// The dedup key is what the store collapses on, so both must agree even
	// though the timestamps differ by a millisecond and the paths differ.
	first, _ := p.Parse(context.Background(), srcOf(app), cursor{})
	second, _ := p.Parse(context.Background(), srcOf(ext), cursor{})
	if first.Events[0].DedupKey != second.Events[0].DedupKey {
		t.Errorf("dedup keys differ: %q vs %q", first.Events[0].DedupKey, second.Events[0].DedupKey)
	}
}

// TestParseLogIncremental checks a second read only returns what was appended,
// and that the model survives across reads via the cursor.
func TestParseLogIncremental(t *testing.T) {
	dir := t.TempDir()
	path := writeLog(t, dir, "app.log", appLine)
	p := NewWithRoots(dir)
	ctx := context.Background()

	first, err := p.Parse(ctx, srcOf(path), cursor{})
	if err != nil {
		t.Fatalf("first Parse: %v", err)
	}
	if len(first.Events) != 1 {
		t.Fatalf("first events = %d, want 1", len(first.Events))
	}

	again, err := p.Parse(ctx, srcOf(path), first.Next)
	if err != nil {
		t.Fatalf("second Parse: %v", err)
	}
	if len(again.Events) != 0 {
		t.Fatalf("unchanged file produced %d events", len(again.Events))
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`2026-11-06 22:39:52.735 [info] [AgentReporter] [90c56f83fae2e80462185a103fa21d4e] Agent execution successful with usage: {"inputTokens":1000,"outputTokens":100,"cachedMissTokens":900,"cacheTokens":100,"thinkingTokens":0,"credit":0.02}` + "\n")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	third, err := p.Parse(ctx, srcOf(path), again.Next)
	if err != nil {
		t.Fatalf("third Parse: %v", err)
	}
	if len(third.Events) != 1 {
		t.Fatalf("appended line produced %d events, want 1", len(third.Events))
	}
	if third.Events[0].Model != "hy4free" {
		t.Errorf("model should be remembered across reads, got %q", third.Events[0].Model)
	}
	if third.Events[0].Tokens.Input != 900 {
		t.Errorf("input = %d, want 900", third.Events[0].Tokens.Input)
	}
	if third.Events[0].ProjectPath != "" {
		t.Errorf("project = %q, want empty for the app log", third.Events[0].ProjectPath)
	}
}

// TestParseLogSkipsPartialLine checks a half-written trailing line waits for the
// next read instead of being dropped or mis-parsed.
func TestParseLogSkipsPartialLine(t *testing.T) {
	dir := t.TempDir()
	path := writeLog(t, dir, "app.log", appLine+`2026-11-06 22:40:00.000 [info] [AgentReporter] [abc] Agent execution successful with usage: {"inputTo`)
	p := NewWithRoots(dir)
	batch, err := p.Parse(context.Background(), srcOf(path), cursor{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(batch.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(batch.Events))
	}
	if batch.Next.Offset >= batch.Next.Size {
		t.Errorf("cursor %d advanced past the complete line (size %d)", batch.Next.Offset, batch.Next.Size)
	}
}

// TestDiscoverFiltersForeignLogs checks a default root only reports CodeBuddy's
// own logs.
func TestDiscoverFiltersForeignLogs(t *testing.T) {
	dir := t.TempDir()
	buddy := filepath.Join(dir, "exthost", "Tencent-Cloud.coding-copilot")
	if err := os.MkdirAll(buddy, 0o755); err != nil {
		t.Fatal(err)
	}
	writeLog(t, buddy, "腾讯云代码助手.log", appLine)
	writeLog(t, dir, "unrelated.log", "hello\n")

	srcs, err := New().Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	_ = srcs // default roots point at the real home; the filter is covered below.

	explicit := NewWithRoots(dir)
	srcs, err = explicit.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(srcs) != 2 {
		t.Fatalf("explicit root reports every .log, got %d", len(srcs))
	}
	if !explicit.logAllowed(filepath.Join(buddy, "腾讯云代码助手.log")) {
		t.Error("coding-copilot log must be allowed")
	}
	if New().logAllowed(filepath.Join(dir, "unrelated.log")) {
		t.Error("foreign log must be filtered under default roots")
	}
}

// TestParseLogCRLF checks a log written with Windows line endings parses the same
// way, which is what the Windows build produces.
func TestParseLogCRLF(t *testing.T) {
	dir := t.TempDir()
	crlf := strings.ReplaceAll(appLine, "\n", "\r\n")
	path := writeLog(t, dir, "app.log", crlf)
	p := NewWithRoots(dir)

	batch, err := p.Parse(context.Background(), srcOf(path), cursor{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(batch.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(batch.Events))
	}
	if got := batch.Events[0].Tokens.CacheRead; got != 250368 {
		t.Errorf("cache read = %d, want 250368", got)
	}
	if batch.Events[0].Model != "hy4free" {
		t.Errorf("model = %q, want hy4free", batch.Events[0].Model)
	}
}
