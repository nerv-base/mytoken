package zcode

import (
	"reflect"
	"testing"
)

// The pending re-read is cut into bounded chunks: every rowid must appear in
// exactly one chunk (nothing starved, nothing re-read twice), and no chunk may
// ask one statement to name more rowids than the parameter bound allows.
func TestPendingChunksCoverEveryRowidWithinTheBound(t *testing.T) {
	for _, n := range []int{0, 1, 499, 500, 501, 1025, 2500} {
		var ids []int64
		if n > 0 {
			ids = make([]int64, 0, n)
		}
		for i := 1; i <= n; i++ {
			ids = append(ids, int64(i))
		}
		chunks := pendingChunks(ids)
		if want := (n + pendingBatch - 1) / pendingBatch; len(chunks) != want {
			t.Fatalf("pendingChunks(%d) returned %d chunks, want %d", n, len(chunks), want)
		}
		var seen []int64
		for _, chunk := range chunks {
			if len(chunk) == 0 {
				t.Fatalf("pendingChunks(%d) returned an empty chunk", n)
			}
			if len(chunk) > pendingBatch {
				t.Fatalf("pendingChunks(%d) produced a chunk of %d rowids, above the %d-parameter bound", n, len(chunk), pendingBatch)
			}
			seen = append(seen, chunk...)
		}
		if !reflect.DeepEqual(seen, ids) {
			t.Fatalf("pendingChunks(%d) covered %v, want every rowid exactly once in order", n, seen)
		}
	}
}
