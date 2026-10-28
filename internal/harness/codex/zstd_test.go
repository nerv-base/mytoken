package codex

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
)

// compressZstd compresses body into a complete single zstd frame, the shape
// Codex writes when it rotates a rollout.
func compressZstd(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// rotatedRollout writes body as a .jsonl.zst rollout next to the plain fixture
// tree and returns its path.
func rotatedRollout(t *testing.T, body []byte) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sessions", "2026", "01", "02")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-01-02T03-04-05-019aaa00-1111-7222-8333-444455556666.jsonl.zst")
	if err := os.WriteFile(path, compressZstd(t, body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// plainRolloutBody reads a plain fixture rollout as bytes.
func plainRolloutBody(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestZstdAndPlainAgree is the core guarantee: a compressed rollout yields
// exactly the events, sessions, tokens and cursor shape its plain form does.
// zstd is not seekable, so the compressed path re-reads the whole log every
// time; this locks that the re-read is indistinguishable from the incremental
// plain one.
func TestZstdAndPlainAgree(t *testing.T) {
	for _, src := range []string{mainRollout, subRollout, archRollout} {
		t.Run(filepath.Base(src), func(t *testing.T) {
			body := plainRolloutBody(t, src)
			p := NewWithRoots(filepath.Dir(src))
			want := parseFile(t, p, src, harness.Cursor{})

			path := rotatedRollout(t, body)
			pz := NewWithRoots(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(path)))))
			got := parseFile(t, pz, path, harness.Cursor{})

			if len(got.Events) != len(want.Events) {
				t.Fatalf("events = %d, want %d", len(got.Events), len(want.Events))
			}
			for i := range want.Events {
				if got.Events[i].DedupKey != want.Events[i].DedupKey {
					t.Errorf("event %d dedup = %q, want %q", i, got.Events[i].DedupKey, want.Events[i].DedupKey)
				}
				if got.Events[i].Tokens != want.Events[i].Tokens {
					t.Errorf("event %d tokens = %+v, want %+v", i, got.Events[i].Tokens, want.Events[i].Tokens)
				}
				if got.Events[i].SessionID != want.Events[i].SessionID || got.Events[i].ParentID != want.Events[i].ParentID {
					t.Errorf("event %d session/parent = %q/%q, want %q/%q", i,
						got.Events[i].SessionID, got.Events[i].ParentID, want.Events[i].SessionID, want.Events[i].ParentID)
				}
				if got.Events[i].Model != want.Events[i].Model {
					t.Errorf("event %d model = %q, want %q", i, got.Events[i].Model, want.Events[i].Model)
				}
			}
			if len(got.Sessions) != len(want.Sessions) {
				t.Fatalf("sessions = %d, want %d", len(got.Sessions), len(want.Sessions))
			}
			for i := range want.Sessions {
				if got.Sessions[i] != want.Sessions[i] {
					t.Errorf("session %d = %+v, want %+v", i, got.Sessions[i], want.Sessions[i])
				}
			}
			// The cursor records no offset — none applies — and carries the
			// compressed file's own stamp, which is what the unchanged fast
			// path compares.
			if got.Next.Offset != 0 {
				t.Errorf("Next.Offset = %d, want 0", got.Next.Offset)
			}
			if got.Next.Size == 0 || got.Next.Fingerprint == "" || got.Next.ModTime.IsZero() {
				t.Errorf("cursor not stamped: %+v", got.Next)
			}
			if got.Next.Extra != kindJSONLzs {
				t.Errorf("Next.Extra = %q, want %q", got.Next.Extra, kindJSONLzs)
			}
		})
	}
}

// TestZstdUnchangedIsSkipped locks the fast path: an unchanged compressed
// rollout is not decompressed again, which is what keeps a full scan of an
// archived Codex home affordable.
func TestZstdUnchangedIsSkipped(t *testing.T) {
	path := rotatedRollout(t, plainRolloutBody(t, mainRollout))
	p := NewWithRoots(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(path)))))
	first := parseFile(t, p, path, harness.Cursor{})
	if len(first.Events) == 0 {
		t.Fatal("no events on first parse")
	}
	again := parseFile(t, p, path, first.Next)
	if len(again.Events) != 0 || len(again.Sessions) != 0 {
		t.Fatalf("unchanged compressed rollout re-emitted: %d events, %d sessions", len(again.Events), len(again.Sessions))
	}
	if again.Next != first.Next {
		t.Fatalf("cursor moved: %+v -> %+v", first.Next, again.Next)
	}
}

// TestZstdGrowsAfterRotation covers the real rotation lifecycle: Codex writes a
// plain rollout, then replaces it with a compressed copy once the session ends.
// The compressed copy is read in full and DedupKey keeps the result identical.
func TestZstdGrowsAfterRotation(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "rollout-2026-01-02T03-04-05-019aaa00-1111-7222-8333-444455556666.jsonl")
	if err := os.WriteFile(plain, plainRolloutBody(t, mainRollout), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewWithRoots(dir)
	before := parseFile(t, p, plain, harness.Cursor{})

	// Rotate: the plain file is replaced by its compressed twin.
	if err := os.Remove(plain); err != nil {
		t.Fatal(err)
	}
	compressed := filepath.Join(dir, "rollout-2026-01-02T03-04-05-019aaa00-1111-7222-8333-444455556666.jsonl.zst")
	if err := os.WriteFile(compressed, compressZstd(t, plainRolloutBody(t, mainRollout)), 0o644); err != nil {
		t.Fatal(err)
	}
	// The cursor taken from the plain file must not make the compressed one
	// look already-read.
	after := parseFile(t, p, compressed, before.Next)
	if len(after.Events) != len(before.Events) {
		t.Fatalf("events = %d, want %d (a plain cursor must not hide the rotated copy)", len(after.Events), len(before.Events))
	}
	if sum(after.Events) != sum(before.Events) {
		t.Errorf("tokens = %+v, want %+v", sum(after.Events), sum(before.Events))
	}

	// And the store's dedup on DedupKey means replaying both is still right.
	merged, n := collapse(append(append([]model.UsageEvent{}, before.Events...), after.Events...))
	if n != len(before.Events) {
		t.Errorf("merged events = %d, want %d", n, len(before.Events))
	}
	if merged != sum(before.Events) {
		t.Errorf("merged tokens = %+v, want %+v", merged, sum(before.Events))
	}
}

// TestZstdTruncatedFrame covers a rotation interrupted mid-write: the frame
// decodes its prefix and the error is ignored, because the next scan rewrites
// the whole file anyway.
func TestZstdTruncatedFrame(t *testing.T) {
	body := plainRolloutBody(t, mainRollout)
	full := compressZstd(t, body)
	path := filepath.Join(t.TempDir(), "rollout-2026-01-02T03-04-05-019aaa00-1111-7222-8333-444455556666.jsonl.zst")
	if err := os.WriteFile(path, full[:len(full)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewWithRoots(filepath.Dir(path))
	b, err := p.Parse(context.Background(), harness.Source{Path: path, Kind: kindJSONLzs}, harness.Cursor{})
	if err != nil {
		t.Fatalf("Parse on a truncated frame must not fail: %v", err)
	}
	// Whatever survived must be internally consistent.
	if len(b.Events) > 0 {
		if sum(b.Events).Total() <= 0 {
			t.Errorf("tokens = %+v", sum(b.Events))
		}
		for _, e := range b.Events {
			if e.Timestamp.Location() != time.UTC {
				t.Errorf("timestamp not UTC: %s", e.Timestamp)
			}
		}
	}
	if b.Next.Size == 0 || b.Next.Fingerprint == "" {
		t.Errorf("cursor not stamped: %+v", b.Next)
	}
}

// TestZstdSourceSessions checks that a reset scopes the rows of a compressed
// rollout from its session_meta, read without decompressing the whole log.
func TestZstdSourceSessions(t *testing.T) {
	path := rotatedRollout(t, plainRolloutBody(t, mainRollout))
	p := NewWithRoots(filepath.Dir(path))
	ids, err := p.SourceSessions(context.Background(), harness.Source{Path: path, Kind: kindJSONLzs})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "019aaa00-1111-7222-8333-444455556666" {
		t.Fatalf("SourceSessions = %v, want the session_meta id", ids)
	}

	// A rollout whose session_meta line has not been written yet still scopes
	// from the file name, so its rows are never mistaken for history.
	nameOnly := filepath.Join(t.TempDir(), "rollout-2026-01-02T03-04-05-019ddd00-4444-7555-8666-777788889999.jsonl.zst")
	if err := os.WriteFile(nameOnly, compressZstd(t, []byte(`{"timestamp":"2026-01-02T03:04:06.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15},"last_token_usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}}`+"\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	ids, err = p.SourceSessions(context.Background(), harness.Source{Path: nameOnly, Kind: kindJSONLzs})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "019ddd00-4444-7555-8666-777788889999" {
		t.Fatalf("SourceSessions = %v, want the id from the file name", ids)
	}
}

// TestZstdSessionFromFilename checks the id recovered from a rotated name.
func TestZstdSessionFromFilename(t *testing.T) {
	cases := []struct{ in, want string }{
		{"rollout-2026-01-02T03-04-05-019aaa00-1111-7222-8333-444455556666.jsonl.zst", "019aaa00-1111-7222-8333-444455556666"},
		{"rollout-2026-05-20T09-38-01-019e4308-5a3d-70d3-84e2-a5ce8ffe3e15_01a05ac8-a300-74d2-8be9-57b06013fe12.jsonl.zst", "01a05ac8-a300-74d2-8be9-57b06013fe12"},
		{"not-a-rollout.jsonl.zst", "not-a-rollout"},
	}
	for _, tc := range cases {
		if got := sessionFromFilename(tc.in); got != tc.want {
			t.Errorf("sessionFromFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestZstdKindAndDiscover checks that a mixed directory — plain and rotated
// rollouts side by side, as on a real machine — yields both with the right Kind.
func TestZstdKindAndDiscover(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"rollout-2026-01-02T03-04-05-019aaa00-1111-7222-8333-444455556666.jsonl",
		"rollout-2026-01-02T04-00-00-019bbb00-2222-7333-8444-555566667777.jsonl.zst",
		"rollout-2026-01-02T05-00-00-019ccc00-3333-7444-8555-666677778888.jsonl.zstd",
		"notes.txt",
		"rollout-2026-01-02T06-00-00-019eee00-5555-7666-8777-888899990000.jsonl.gz",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := NewWithRoots(dir)
	got, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, s := range got {
		lines = append(lines, filepath.Base(s.Path)+"|"+s.Kind)
	}
	want := strings.Join([]string{
		"rollout-2026-01-02T03-04-05-019aaa00-1111-7222-8333-444455556666.jsonl|jsonl",
		"rollout-2026-01-02T04-00-00-019bbb00-2222-7333-8444-555566667777.jsonl.zst|jsonl.zstd",
		"rollout-2026-01-02T05-00-00-019ccc00-3333-7444-8555-666677778888.jsonl.zstd|jsonl.zstd",
	}, "\n")
	if strings.Join(lines, "\n") != want {
		t.Errorf("discovered:\n%s\nwant:\n%s", strings.Join(lines, "\n"), want)
	}
}

// TestZstdParserVersionBumped is the migration guard: version 2 exists so the
// store resets and re-reads the rollouts version 1 never opened.
func TestZstdParserVersionBumped(t *testing.T) {
	if parserVersion < 2 {
		t.Fatalf("parserVersion = %d, want >= 2 so compressed rollouts are re-ingested", parserVersion)
	}
}
