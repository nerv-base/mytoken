package scan

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/zzstar101/mytoken/internal/quota"
	"github.com/zzstar101/mytoken/internal/store"
)

// Codex quota state used to live only in the cursor a parse pass advances: the
// collector is filled from the records that pass reads and its encoded state is
// written back into the cursor, and the observation is published only when the
// pass also produced a write. A rollout already at EOF therefore never
// published one — which is the state every file ends in, and why `mytoken
// quota` reported no stored Codex observation at all.
//
// The tail read below closes that gap without touching any cursor: for each
// Codex instance it looks at the newest rollout files the scan already
// discovered, reads the last part of the newest one, and decodes it with the
// same reader a whole-file pass uses. It reads no cursor, writes no event and
// never derives quota from tokens or cost.
//
// The read is bounded on every axis: at most quotaTailMaxFiles files per
// instance, at most quotaTailMaxInstanceBytes bytes per instance, at most
// quotaTailMaxFileBytes bytes in one file, and quotaTailTimeout in total. A
// window that holds no usable observation grows by quotaTailWindowGrowth and is
// decoded again, so the work stays proportional to the distance to the nearest
// usable record rather than to the size of the log. An unchanged file (same
// size and modification time) is not read again, so a periodic or watched scan
// over an idle log costs one stat per file.
//
// A consequence of reading a window rather than the whole file: when the window
// does not start at offset 0, Report.Line numbers are relative to the window and
// Report.BytesRead counts the window, not the file. For files smaller than the
// first window both are exact. Nothing else in the report depends on the offset.
const (
	// quotaTailMaxFiles is how many of the newest rollout files of one instance
	// are considered before giving up. Measured on a real ~1,300 file Codex log:
	// the newest usable rate_limits record sat 17 files back, behind 16 files
	// whose records were all-null, so a bound of a few files published nothing at
	// all. The per-instance byte budget, not this count, is the real limit.
	quotaTailMaxFiles = 64
	// quotaTailMaxFileBytes bounds one file's read, including every growth step.
	// In the same log the newest usable record was at most ~342 KB from the end of
	// its file (median ~3 KB), so growth is bounded to half a megabyte and a large
	// rollout is never walked.
	quotaTailMaxFileBytes = 512 << 10
	// quotaTailFirstWindow is the first suffix read of a file. An appended rollout
	// almost always carries its newest rate_limits record in the last few KB, so
	// this finds it without reading the file.
	quotaTailFirstWindow = 256 << 10
	// quotaTailWindowGrowth multiplies the window when the current one held no
	// usable observation. One growth step reaches quotaTailMaxFileBytes, so a file
	// is decoded at most twice.
	quotaTailWindowGrowth = 4
	// quotaTailMaxInstanceBytes bounds the bytes one instance may cost a scan.
	// Reaching the newest usable record of the measured log cost ~9.5 MB over 18
	// files, hence 16 MB: enough for breadth, still bounded per scan.
	quotaTailMaxInstanceBytes = 16 << 20
	// quotaTailTimeout bounds the whole tail read, whatever the bytes say.
	quotaTailTimeout = 5 * time.Second
)

// quotaTailSource is one rollout file the scan's discovery pass found, with the
// stat that pass already took. Carrying it means the tail read adds no second
// directory walk.
type quotaTailSource struct {
	instance string
	path     string
	size     int64
	mod      time.Time
}

// QuotaTailOutcome records what the bounded tail read did for one Codex
// instance: whether an observation was published, which file it came from, and
// the status and reason of the last report read. When nothing was published the
// status and reason say why (for example unavailable/missing for a log whose
// records carry no usable window); both stay zero when every candidate had
// already been read unchanged, so nothing was decoded this scan. Err records a
// file that could not be read; it does not fail the scan, because a rollout may
// legitimately disappear between discovery and the read.
type QuotaTailOutcome struct {
	Instance  string
	Path      string
	Published bool
	Report    *quota.Report
	Status    quota.Status
	Reason    quota.Reason
	Files     int
	Bytes     int64
	Err       error
}

// publishQuotaTail publishes a Codex quota observation from the newest rollout
// files of each Codex instance that discovery saw, even when every event cursor
// is already at EOF. It is called once per scan, after the parse pass.
func (s *Scanner) publishQuotaTail(ctx context.Context, srcs []quotaTailSource) ([]QuotaTailOutcome, error) {
	if s.st == nil || len(srcs) == 0 {
		return nil, nil
	}
	if s.tailSeen == nil {
		s.tailSeen = map[string]fileStamp{}
		s.tailHit = map[string]bool{}
	}
	ctx, cancel := context.WithTimeout(ctx, quotaTailTimeout)
	defer cancel()

	byInstance := map[string][]quotaTailSource{}
	var order []string
	for _, src := range srcs {
		id := store.NormalizeInstanceID(src.instance)
		if _, ok := byInstance[id]; !ok {
			order = append(order, id)
		}
		byInstance[id] = append(byInstance[id], src)
	}
	sort.Strings(order)

	now := s.now()
	var (
		outcomes []QuotaTailOutcome
		errs     []error
	)
	for _, id := range order {
		out, err := s.publishQuotaTailInstance(ctx, id, byInstance[id], now)
		outcomes = append(outcomes, out)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return outcomes, errors.Join(errs...)
}

// publishQuotaTailInstance reads one instance's newest rollouts from the end
// until it finds a usable observation, and stops at the first one it publishes.
// The very first candidate that holds no usable observation is not an error: it
// is a rollout that simply has not recorded one yet, and the next scan retries
// it as soon as the file grows.
func (s *Scanner) publishQuotaTailInstance(ctx context.Context, instance string, srcs []quotaTailSource, now time.Time) (QuotaTailOutcome, error) {
	out := QuotaTailOutcome{Instance: instance}
	cands := append([]quotaTailSource(nil), srcs...)
	// Newest first; the path breaks a tie so the choice is deterministic.
	sort.Slice(cands, func(i, j int) bool {
		if !cands[i].mod.Equal(cands[j].mod) {
			return cands[i].mod.After(cands[j].mod)
		}
		return cands[i].path > cands[j].path
	})
	if len(cands) > quotaTailMaxFiles {
		cands = cands[:quotaTailMaxFiles]
	}

	budget := int64(quotaTailMaxInstanceBytes)
	var last *quota.Report
	for _, c := range cands {
		if ctx.Err() != nil {
			// The scan's own deadline, or this read's: stop quietly. The next
			// scan reads whatever is still new.
			break
		}
		key := instance + "\x00" + c.path
		stamp := fileStamp{size: c.size, mod: c.mod}
		if seen, ok := s.tailSeen[key]; ok && seen.size == stamp.size && seen.mod.Equal(stamp.mod) {
			if s.tailHit[key] {
				// Already published from this file, and nothing newer moved.
				break
			}
			continue
		}
		fileBudget := int64(quotaTailMaxFileBytes)
		if fileBudget > budget {
			fileBudget = budget
		}
		window := int64(quotaTailFirstWindow)
		if window > c.size {
			window = c.size
		}
		if window > fileBudget {
			window = fileBudget
		}
		if window <= 0 {
			break
		}
		var (
			rep     *quota.Report
			read    int64
			readErr error
		)
		for {
			var n int64
			rep, n, readErr = readQuotaTail(ctx, c.path, c.size, window)
			read += n
			if readErr != nil || (rep != nil && rep.Available()) {
				break
			}
			if window >= c.size || window >= fileBudget {
				break
			}
			next := window * quotaTailWindowGrowth
			if next > c.size {
				next = c.size
			}
			if next > fileBudget {
				next = fileBudget
			}
			if next <= window {
				break
			}
			window = next
		}
		out.Files++
		out.Bytes += read
		budget -= read
		if readErr != nil {
			out.Err = readErr
			if budget <= 0 {
				break
			}
			continue
		}
		// Remember the stat that was actually read: an unchanged file is skipped
		// by the next scan, and any append or replacement is read again.
		s.tailSeen[key] = stamp
		s.tailHit[key] = rep != nil && rep.Available()
		last = rep
		if rep != nil && rep.Available() {
			if err := quota.PublishLatest(ctx, s.st, instance, rep, now); err != nil {
				return out, fmt.Errorf("publish codex quota tail for instance %q: %w", instance, err)
			}
			out.Published = true
			out.Path = c.path
			out.Report = rep
			out.Status = rep.Status
			out.Reason = rep.Reason
			return out, nil
		}
		if budget <= 0 {
			break
		}
	}
	if last != nil {
		out.Status = last.Status
		out.Reason = last.Reason
	}
	return out, nil
}

// readQuotaTail decodes the last window bytes of path. A window that begins
// inside a line starts with a fragment, which is not a JSONL record and is
// dropped. The second return value is the bytes actually consumed, fragment
// included, so a caller can bound the total. A file that turns out to hold no
// complete line in the window yields an unavailable report rather than an
// error.
func readQuotaTail(ctx context.Context, path string, size, window int64) (*quota.Report, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	if window > size {
		window = size
	}
	start := size - window
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, 0, err
	}
	br := bufio.NewReaderSize(f, 1<<20)
	var skipped int64
	if start > 0 {
		line, rerr := br.ReadBytes('\n')
		skipped = int64(len(line))
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return emptyTailReport(path), skipped, nil
			}
			return nil, skipped, rerr
		}
	}
	rep, err := quota.ReadCodex(ctx, br, quota.CodexOptions{Source: path, MaxBytes: window})
	if err != nil {
		return nil, skipped, err
	}
	return rep, skipped + rep.BytesRead, nil
}

// emptyTailReport is the report for a window that held no complete record.
func emptyTailReport(source string) *quota.Report {
	return &quota.Report{Source: source, Status: quota.StatusUnavailable, Reason: quota.ReasonNoData}
}
