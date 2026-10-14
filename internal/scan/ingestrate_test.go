package scan

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/ingestrate"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

var rateBase = time.Unix(1_700_000_000, 0).UTC()

// rateParser indexes one log file whose every non-blank line is "key:output"
// or "key:output@offset", where offset is whole seconds relative to rateBase.
// A test therefore controls dedup keys, output tokens and each event's own
// timestamp exactly; without an offset every event is stamped at rateBase.
type rateParser struct {
	root string
	// replay makes Parse ignore the cursor and re-emit the whole file, the way
	// a whole-file parser does when it is read again.
	replay bool
	// badSession makes Parse emit an event the store rejects, to fail a commit.
	badSession bool
	// fail makes Parse fail.
	fail bool
}

func (p *rateParser) Harness() model.Harness { return model.Harness("ratefixture") }
func (p *rateParser) Roots() []string        { return []string{p.root} }

func (p *rateParser) Discover(context.Context) ([]harness.Source, error) {
	return []harness.Source{{Path: filepath.Join(p.root, "log"), Kind: "jsonl"}}, nil
}

func (p *rateParser) Parse(_ context.Context, src harness.Source, cur harness.Cursor) (harness.Batch, error) {
	if p.fail {
		return harness.Batch{}, errors.New("rateParser: synthetic parse failure")
	}
	raw, err := os.ReadFile(src.Path)
	if err != nil {
		return harness.Batch{}, err
	}
	start := cur.Offset
	if p.replay {
		start = 0
	}
	if start > int64(len(raw)) {
		start = int64(len(raw))
	}
	b := harness.Batch{Next: harness.Cursor{Offset: int64(len(raw)), Size: int64(len(raw))}}
	for _, line := range strings.Split(string(raw[start:]), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, out, ok := strings.Cut(line, ":")
		if !ok {
			return harness.Batch{}, fmt.Errorf("rateParser: bad line %q", line)
		}
		at := rateBase
		if rest, offset, ok := strings.Cut(out, "@"); ok {
			out = rest
			secs, err := strconv.ParseInt(offset, 10, 64)
			if err != nil {
				return harness.Batch{}, fmt.Errorf("rateParser: bad timestamp %q: %w", offset, err)
			}
			at = rateBase.Add(time.Duration(secs) * time.Second)
		}
		n, err := strconv.ParseInt(out, 10, 64)
		if err != nil {
			return harness.Batch{}, err
		}
		session := "s1"
		if p.badSession {
			session = ""
		}
		b.Events = append(b.Events, model.UsageEvent{DedupKey: key, SessionID: session, Model: "gpt-5", Timestamp: at, Tokens: model.Tokens{Output: n}})
	}
	return b, nil
}

type rateClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *rateClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *rateClock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
}

func newRateScanner(t *testing.T, p harness.Parser) (*Scanner, *store.Store, *rateClock) {
	t.Helper()
	st := openStore(t)
	sc := New(st, nil, nil, p)
	clk := &rateClock{at: rateBase}
	sc.clock = clk.now
	return sc, st, clk
}

func writeRateLog(t *testing.T, root string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "log"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendRateLog(t *testing.T, root string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(root, "log"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
}

func TestIngestRateLiveAppendCountsOnlyNewOutput(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeRateLog(t, root, "hist:900") // history is never ingestion
	sc, st, clk := newRateScanner(t, &rateParser{root: root})

	if err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	// The first pass is history, so there is no live signal at all.
	if snap := sc.IngestRate(rateBase); snap.Available || snap.Reason != ingestrate.ReasonNoLiveSignal {
		t.Fatalf("after backfill: %+v", snap)
	}
	if _, ok, err := LoadIngestRateSignal(ctx, st); err != nil || ok {
		t.Fatalf("backfill published a signal: ok=%v err=%v", ok, err)
	}

	appendRateLog(t, root, "live:200")
	at := rateBase.Add(5 * time.Second)
	clk.set(at)
	if err := sc.scan(ctx, true); err != nil {
		t.Fatal(err)
	}
	snap := sc.IngestRate(at)
	if !snap.Available || snap.Tokens != 200 || snap.Window != ingestrate.Window {
		t.Fatalf("live append: %+v", snap)
	}
	want := 200.0 / ingestrate.Window.Seconds()
	if snap.Rate != want {
		t.Fatalf("rate=%v want %v", snap.Rate, want)
	}
	sig, ok, err := LoadIngestRateSignal(ctx, st)
	if err != nil || !ok {
		t.Fatalf("signal: ok=%v err=%v", ok, err)
	}
	if !sig.Available || sig.Tokens != 200 || sig.Reason != "" || sig.Window != 60 || sig.Rate != want {
		t.Fatalf("persisted: %+v", sig)
	}
	if !sig.Timestamp.Equal(at) {
		t.Fatalf("persisted timestamp = %v want %v", sig.Timestamp, at)
	}
}

func TestIngestRateBackfillNeverReportsLive(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeRateLog(t, root, "a:100")
	sc, st, clk := newRateScanner(t, &rateParser{root: root})

	for i := 0; i < 3; i++ {
		if err := sc.Scan(ctx); err != nil {
			t.Fatal(err)
		}
		clk.set(rateBase.Add(time.Duration(i+1) * time.Second))
	}
	if snap := sc.IngestRate(rateBase.Add(3 * time.Second)); snap.Available || snap.Reason != ingestrate.ReasonNoLiveSignal {
		t.Fatalf("historical-only scanner reported live: %+v", snap)
	}
	if _, ok, err := LoadIngestRateSignal(ctx, st); err != nil || ok {
		t.Fatalf("historical-only scanner published: ok=%v err=%v", ok, err)
	}
}

func TestIngestRateDuplicateReplayIsZeroNotNew(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeRateLog(t, root, "a:100")
	sc, st, clk := newRateScanner(t, &rateParser{root: root, replay: true})

	if err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	at := rateBase.Add(3 * time.Second)
	clk.set(at)
	if err := sc.scan(ctx, true); err != nil {
		t.Fatal(err)
	}
	snap := sc.IngestRate(at)
	if !snap.Available || snap.Tokens != 0 || snap.Rate != 0 {
		t.Fatalf("duplicate replay counted: %+v", snap)
	}
	if rows := countRows(t, st, "SELECT count(*) FROM events"); rows != 1 {
		t.Fatalf("rows=%d", rows)
	}
}

func TestIngestRateFailedCommitLeaksNothing(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeRateLog(t, root, "a:100")
	parser := &rateParser{root: root}
	sc, st, clk := newRateScanner(t, parser)
	if err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}

	// A live append the store rejects: the commit rolls back, so nothing may be
	// observed and no zero heartbeat may stand in for the unknown.
	parser.badSession = true
	appendRateLog(t, root, "b:200")
	at := rateBase.Add(7 * time.Second)
	clk.set(at)
	if err := sc.scan(ctx, true); err == nil {
		t.Fatal("want commit failure")
	}
	if snap := sc.IngestRate(at); snap.Available || snap.Reason != ingestrate.ReasonNoLiveSignal {
		t.Fatalf("failed commit reported: %+v", snap)
	}
	if _, ok, err := LoadIngestRateSignal(ctx, st); err != nil || ok {
		t.Fatalf("failed commit published: ok=%v err=%v", ok, err)
	}
	if rows := countRows(t, st, "SELECT count(*) FROM events"); rows != 1 {
		t.Fatalf("rows=%d", rows)
	}
}

func TestIngestRateStaleAndNeverRedated(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeRateLog(t, root, "a:100")
	sc, st, _ := newRateScanner(t, &rateParser{root: root})
	if err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	appendRateLog(t, root, "b:200")
	live := rateBase.Add(time.Second)
	sc.clock = func() time.Time { return live }
	if err := sc.scan(ctx, true); err != nil {
		t.Fatal(err)
	}
	before, ok, err := LoadIngestRateSignal(ctx, st)
	if err != nil || !ok {
		t.Fatalf("signal: ok=%v err=%v", ok, err)
	}

	// Beyond one window the telemetry is stale...
	if snap := sc.IngestRate(live.Add(ingestrate.Window + time.Second)); snap.Available || snap.Reason != ingestrate.ReasonStale {
		t.Fatalf("stale: %+v", snap)
	}
	// ...and reading it must not re-date what was last published.
	after, ok, err := LoadIngestRateSignal(ctx, st)
	if err != nil || !ok {
		t.Fatalf("signal: ok=%v err=%v", ok, err)
	}
	if !after.Timestamp.Equal(before.Timestamp) || after.Tokens != before.Tokens {
		t.Fatalf("read re-dated the signal: before=%+v after=%+v", before, after)
	}
}

func TestIngestRateSubsecondWindow(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeRateLog(t, root, "a:1")
	sc, _, clk := newRateScanner(t, &rateParser{root: root})
	if err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	appendRateLog(t, root, "b:600")
	at := rateBase.Add(900 * time.Millisecond)
	clk.set(at)
	if err := sc.scan(ctx, true); err != nil {
		t.Fatal(err)
	}

	// The window is a whole minute, whatever the sub-second elapsed time.
	if snap := sc.IngestRate(at); !snap.Available || snap.Tokens != 600 || snap.Rate != 10 {
		t.Fatalf("subsecond rate: %+v", snap)
	}
	if snap := sc.IngestRate(rateBase.Add(ingestrate.Window)); !snap.Available || snap.Tokens != 600 {
		t.Fatalf("at window: %+v", snap)
	}
	// The sub-second stamp expires at its own timestamp, not a rounded second.
	expiry := rateBase.Add(ingestrate.Window + 900*time.Millisecond)
	if snap := sc.IngestRate(expiry); !snap.Available || snap.Tokens != 0 || snap.Rate != 0 {
		t.Fatalf("at subsecond expiry: %+v", snap)
	}
	if snap := sc.IngestRate(expiry.Add(time.Nanosecond)); snap.Available || snap.Reason != ingestrate.ReasonStale {
		t.Fatalf("past expiry: %+v", snap)
	}
}

func TestIngestRateIgnoresHistoricalCommits(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeRateLog(t, root, "a:100")
	sc, st, clk := newRateScanner(t, &rateParser{root: root})
	if err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	appendRateLog(t, root, "b:200")
	clk.set(rateBase.Add(2 * time.Second))
	if err := sc.scan(ctx, true); err != nil {
		t.Fatal(err)
	}
	before, ok, err := LoadIngestRateSignal(ctx, st)
	if err != nil || !ok {
		t.Fatalf("signal: ok=%v err=%v", ok, err)
	}

	// A full scan importing a historical event must not enter the live rate,
	// and must not touch the published signal.
	appendRateLog(t, root, "c:300")
	at := rateBase.Add(3 * time.Second)
	clk.set(at)
	if err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if snap := sc.IngestRate(at); !snap.Available || snap.Tokens != 200 {
		t.Fatalf("historical import counted: %+v", snap)
	}
	after, ok, err := LoadIngestRateSignal(ctx, st)
	if err != nil || !ok {
		t.Fatalf("signal: ok=%v err=%v", ok, err)
	}
	if !after.Timestamp.Equal(before.Timestamp) || after.Tokens != before.Tokens {
		t.Fatalf("historical scan re-dated the signal: before=%+v after=%+v", before, after)
	}
}

// TestIngestRateClockNeverRegresses checks the serialization contract: a reader
// that asks for a time before the scanner's own clock must not make the next
// observation illegal or the snapshot regress.
func TestIngestRateClockNeverRegresses(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeRateLog(t, root, "a:100")
	sc, _, clk := newRateScanner(t, &rateParser{root: root})
	if err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	appendRateLog(t, root, "b:200")
	live := rateBase.Add(time.Second)
	clk.set(live)
	if err := sc.scan(ctx, true); err != nil {
		t.Fatal(err)
	}
	if snap := sc.IngestRate(live.Add(-time.Hour)); snap.Reason == ingestrate.ReasonClockRegression || !snap.Available || snap.Tokens != 200 {
		t.Fatalf("backwards read: %+v", snap)
	}
	appendRateLog(t, root, "c:300")
	later := live.Add(10 * time.Second)
	clk.set(later)
	if err := sc.scan(ctx, true); err != nil {
		t.Fatal(err)
	}
	if snap := sc.IngestRate(later); !snap.Available || snap.Tokens != 500 {
		t.Fatalf("after backwards read: %+v", snap)
	}
}

// TestIngestRateWatchedBackfillAppendIsNotLive is the regression this file
// exists for: a file that already has a cursor is watched, and historical
// records are appended to it. The rows are new to the index, but their own
// timestamps are hours old, so they must be stored without entering the live
// rate.
func TestIngestRateWatchedBackfillAppendIsNotLive(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeRateLog(t, root, "hist:900")
	sc, st, clk := newRateScanner(t, &rateParser{root: root})

	if err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	// Two hours of history appended behind an established cursor.
	appendRateLog(t, root, "backfill:500@-7200")
	at := rateBase.Add(5 * time.Second)
	clk.set(at)
	if err := sc.scan(ctx, true); err != nil {
		t.Fatal(err)
	}

	if rows := countRows(t, st, "SELECT count(*) FROM events"); rows != 2 {
		t.Fatalf("appended history was not stored: rows=%d", rows)
	}
	if snap := sc.IngestRate(at); !snap.Available || snap.Tokens != 0 || snap.Rate != 0 {
		t.Fatalf("historical append inflated the live rate: %+v", snap)
	}
}

// TestIngestRateWatchedMixedAppendCountsFreshOnly commits one batch that mixes
// historical and fresh rows: the fresh output is the only live ingestion, and
// the stored history is untouched.
func TestIngestRateWatchedMixedAppendCountsFreshOnly(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeRateLog(t, root, "hist:900")
	sc, st, clk := newRateScanner(t, &rateParser{root: root})

	if err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	appendRateLog(t, root, "backfill:500@-7200", "fresh:200@5")
	at := rateBase.Add(10 * time.Second)
	clk.set(at)
	if err := sc.scan(ctx, true); err != nil {
		t.Fatal(err)
	}

	if rows := countRows(t, st, "SELECT count(*) FROM events"); rows != 3 {
		t.Fatalf("mixed batch was not stored whole: rows=%d", rows)
	}
	if snap := sc.IngestRate(at); !snap.Available || snap.Tokens != 200 || snap.Rate != 200.0/ingestrate.Window.Seconds() {
		t.Fatalf("mixed append: %+v", snap)
	}
}

// TestIngestRateFutureTimestampIsNotLive checks the upper edge of the freshness
// rule: an event stamped ahead of the commit is not presumed live, so it cannot
// inflate the rate. A missing timestamp is covered by the store's own outcome
// test, since a parser here always stamps an event.
func TestIngestRateFutureTimestampIsNotLive(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeRateLog(t, root, "hist:900")
	sc, _, clk := newRateScanner(t, &rateParser{root: root})

	if err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	appendRateLog(t, root, "ahead:400@3600")
	at := rateBase.Add(5 * time.Second)
	clk.set(at)
	if err := sc.scan(ctx, true); err != nil {
		t.Fatal(err)
	}
	if snap := sc.IngestRate(at); !snap.Available || snap.Tokens != 0 {
		t.Fatalf("future timestamp counted: %+v", snap)
	}
}

// TestIngestRateBoundaryIsExclusiveAtWindow checks the exact window edge the
// scanner hands the store: an event exactly one window old has left the
// trailing interval and is history, one a second later is live.
func TestIngestRateBoundaryIsExclusiveAtWindow(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeRateLog(t, root, "hist:900")
	sc, _, clk := newRateScanner(t, &rateParser{root: root})

	if err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	// at = rateBase+5s, so an event at rateBase-55s is exactly one window old.
	appendRateLog(t, root, "edge:100@-55", "inside:200@-54")
	at := rateBase.Add(5 * time.Second)
	clk.set(at)
	if err := sc.scan(ctx, true); err != nil {
		t.Fatal(err)
	}
	if snap := sc.IngestRate(at); !snap.Available || snap.Tokens != 200 {
		t.Fatalf("window edge: %+v", snap)
	}
}

// storedIngestSignal is the read side LoadIngestRateSignal needs, backed by a
// literal string so a test can store a hand-written value.
type storedIngestSignal map[string]string

func (s storedIngestSignal) Setting(_ context.Context, key string) (string, error) {
	return s[key], nil
}

func TestLoadIngestRateSignalRejectsCorruptValues(t *testing.T) {
	ctx := context.Background()
	at := rateBase.UTC()
	valid := `{"timestamp":"` + at.Format(time.RFC3339Nano) + `","available":true,"window_seconds":60,"tokens":600,"rate":10}`
	cases := []struct {
		name string
		raw  string
	}{
		{"empty object", `{}`},
		{"null", `null`},
		{"missing timestamp", `{"available":true,"window_seconds":60,"tokens":600,"rate":10}`},
		{"zero timestamp", `{"timestamp":"0001-01-01T00:00:00Z","available":true,"window_seconds":60,"tokens":600,"rate":10}`},
		{"missing window", `{"timestamp":"` + at.Format(time.RFC3339Nano) + `","available":true,"tokens":600,"rate":10}`},
		{"wrong window", `{"timestamp":"` + at.Format(time.RFC3339Nano) + `","available":true,"window_seconds":30,"tokens":600,"rate":20}`},
		{"negative window", `{"timestamp":"` + at.Format(time.RFC3339Nano) + `","available":true,"window_seconds":-60,"tokens":600,"rate":-10}`},
		{"negative tokens", `{"timestamp":"` + at.Format(time.RFC3339Nano) + `","available":true,"window_seconds":60,"tokens":-1,"rate":-0.0167}`},
		{"negative rate", `{"timestamp":"` + at.Format(time.RFC3339Nano) + `","available":true,"window_seconds":60,"tokens":600,"rate":-10}`},
		{"rate disagrees with tokens", `{"timestamp":"` + at.Format(time.RFC3339Nano) + `","available":true,"window_seconds":60,"tokens":600,"rate":9}`},
		{"available with reason", `{"timestamp":"` + at.Format(time.RFC3339Nano) + `","available":true,"reason":"stale_telemetry","window_seconds":60,"tokens":600,"rate":10}`},
		{"unavailable with tokens", `{"timestamp":"` + at.Format(time.RFC3339Nano) + `","available":false,"reason":"no_live_signal","window_seconds":60,"tokens":600,"rate":10}`},
		{"unavailable without reason", `{"timestamp":"` + at.Format(time.RFC3339Nano) + `","available":false,"window_seconds":60,"tokens":0,"rate":0}`},
		{"unknown reason", `{"timestamp":"` + at.Format(time.RFC3339Nano) + `","available":false,"reason":"made_up","window_seconds":60,"tokens":0,"rate":0}`},
		{"wrong type", `{"timestamp":"` + at.Format(time.RFC3339Nano) + `","available":true,"window_seconds":"60","tokens":600,"rate":10}`},
		{"truncated", `{"timestamp":`},
		{"oversized", `{"timestamp":"` + at.Format(time.RFC3339Nano) + `","available":true,"window_seconds":60,"tokens":600,"rate":10,"reason":"` + strings.Repeat("x", maxIngestRateSignalBytes) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sig, ok, err := LoadIngestRateSignal(ctx, storedIngestSignal{IngestRateSettingKey: tc.raw})
			if !errors.Is(err, ErrCorruptIngestRateSignal) {
				t.Fatalf("LoadIngestRateSignal = %+v/%v/%v, want ErrCorruptIngestRateSignal", sig, ok, err)
			}
			if ok {
				t.Fatalf("corrupt value reported as present: %+v", sig)
			}
		})
	}

	// The producer's own output still loads: available and unavailable forms.
	sig, ok, err := LoadIngestRateSignal(ctx, storedIngestSignal{IngestRateSettingKey: valid})
	if err != nil || !ok || !sig.Available || sig.Tokens != 600 || sig.Rate != 10 || sig.Window != 60 || !sig.Timestamp.Equal(at) {
		t.Fatalf("valid signal: %+v ok=%v err=%v", sig, ok, err)
	}
	unavailable := `{"timestamp":"` + at.Format(time.RFC3339Nano) + `","available":false,"reason":"stale_telemetry","window_seconds":60,"tokens":0,"rate":0}`
	if sig, ok, err := LoadIngestRateSignal(ctx, storedIngestSignal{IngestRateSettingKey: unavailable}); err != nil || !ok || sig.Available || sig.Reason != "stale_telemetry" {
		t.Fatalf("unavailable signal: %+v ok=%v err=%v", sig, ok, err)
	}
}

// TestLoadIngestRateSignalKeepsStoredTimestamp proves the read is truthful:
// loading a signal never re-dates it, so a stale measurement cannot be
// refreshed by reading it.
func TestLoadIngestRateSignalKeepsStoredTimestamp(t *testing.T) {
	ctx := context.Background()
	at := rateBase.UTC()
	raw := `{"timestamp":"` + at.Format(time.RFC3339Nano) + `","available":true,"window_seconds":60,"tokens":0,"rate":0}`
	sig, ok, err := LoadIngestRateSignal(ctx, storedIngestSignal{IngestRateSettingKey: raw})
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if !sig.Timestamp.Equal(at) {
		t.Fatalf("timestamp = %v, want the stored %v", sig.Timestamp, at)
	}
}
