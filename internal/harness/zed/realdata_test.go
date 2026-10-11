package zed

import (
	"context"
	"os"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
)

// TestRealData parses whatever Zed thread databases exist on this machine. It is
// gated behind MYTOKEN_REALDATA=1 because most machines have no Zed history.
func TestRealData(t *testing.T) {
	if os.Getenv("MYTOKEN_REALDATA") != "1" {
		t.Skip("set MYTOKEN_REALDATA=1 to parse the local Zed data directory")
	}
	p := New()
	present := false
	for _, root := range p.Roots() {
		if fi, err := os.Stat(root); err == nil && fi.IsDir() {
			present = true
		}
	}
	if !present {
		t.Skipf("no Zed data directory under %v", p.Roots())
	}

	sources, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	events, sessions := 0, 0
	for _, src := range sources {
		batch, err := p.Parse(context.Background(), src, harness.Cursor{})
		if err != nil {
			t.Fatalf("parse %s: %v", src.Path, err)
		}
		events += len(batch.Events)
		sessions += len(batch.Sessions)
	}
	t.Logf("roots: %v, sources: %d, events: %d, sessions: %d", p.Roots(), len(sources), events, sessions)
	for _, src := range sources {
		t.Logf("source: %s (%s)", src.Path, src.Kind)
	}
}
