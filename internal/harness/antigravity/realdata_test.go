package antigravity

import (
	"context"
	"os"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
)

// TestRealData parses whatever Antigravity cascades exist on this machine. It is
// gated behind MYTOKEN_REALDATA=1 because the conversation directories are empty
// unless the app has actually run an agent session.
func TestRealData(t *testing.T) {
	if os.Getenv("MYTOKEN_REALDATA") != "1" {
		t.Skip("set MYTOKEN_REALDATA=1 to parse the local Antigravity conversation directories")
	}
	p := New()
	present := false
	for _, root := range p.Roots() {
		if fi, err := os.Stat(root); err == nil && fi.IsDir() {
			present = true
		}
	}
	if !present {
		t.Skipf("no Antigravity conversation directory under %v", p.Roots())
	}

	sources, err := p.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	events, sessions, projects := 0, 0, map[string]bool{}
	for _, src := range sources {
		batch, err := p.Parse(context.Background(), src, harness.Cursor{})
		if err != nil {
			t.Fatalf("parse %s: %v", src.Path, err)
		}
		events += len(batch.Events)
		sessions += len(batch.Sessions)
		for _, ev := range batch.Events {
			projects[ev.Model] = true
		}
	}
	t.Logf("roots: %v, sources: %d, events: %d, sessions: %d, models: %d",
		p.Roots(), len(sources), events, sessions, len(projects))
	for _, src := range sources {
		t.Logf("source: %s (%s)", src.Path, src.Kind)
	}
}
