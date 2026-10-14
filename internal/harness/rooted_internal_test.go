// Internal registry tests for NewRooted. These live in package harness because
// they must register deliberately broken parsers and then remove them again:
// the public suite must only ever see the real 28 harnesses.
package harness

import (
	"context"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
)

// noFactoryParser implements Parser without the optional Rooted capability.
type noFactoryParser struct{ h model.Harness }

func (p noFactoryParser) Harness() model.Harness                     { return p.h }
func (p noFactoryParser) Roots() []string                            { return nil }
func (p noFactoryParser) Discover(context.Context) ([]Source, error) { return nil, nil }
func (p noFactoryParser) Parse(context.Context, Source, Cursor) (Batch, error) {
	return Batch{}, nil
}

// blockingRooted signals when WithRoots has been entered and then blocks until
// the test releases it, so the test can probe the registry lock meanwhile.
type blockingRooted struct {
	h       model.Harness
	entered chan struct{}
	release chan struct{}
}

func (p *blockingRooted) Harness() model.Harness                     { return p.h }
func (p *blockingRooted) Roots() []string                            { return nil }
func (p *blockingRooted) Discover(context.Context) ([]Source, error) { return nil, nil }
func (p *blockingRooted) Parse(context.Context, Source, Cursor) (Batch, error) {
	return Batch{}, nil
}

func (p *blockingRooted) WithRoots(roots ...string) Parser {
	close(p.entered)
	<-p.release
	return &blockingRooted{h: p.h, entered: make(chan struct{}), release: make(chan struct{})}
}

// registerTemp installs p in the global registry for the duration of the test
// and restores the previous entry afterwards.
func registerTemp(t *testing.T, p Parser) {
	t.Helper()
	h := p.Harness()
	mu.Lock()
	previous, existed := registry[h]
	registry[h] = p
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if existed {
			registry[h] = previous
			return
		}
		delete(registry, h)
	})
}

// TestNewRootedRejectsParserWithoutCapability is the no-factory path: a
// registered parser that does not implement Rooted must fail explicitly instead
// of falling back to default roots.
func TestNewRootedRejectsParserWithoutCapability(t *testing.T) {
	const h model.Harness = "rooted-internal-no-factory"
	registerTemp(t, noFactoryParser{h: h})

	p, ok := NewRooted(h, "/tmp/rooted-internal-no-factory")
	if ok || p != nil {
		t.Fatalf("NewRooted(%s) = (%v, %v), want (nil, false) for a parser without WithRoots", h, p, ok)
	}
}

// TestNewRootedDoesNotCallWithRootsUnderLock proves the registry lock is
// released before parser code runs: WithRoots is held open while a second
// goroutine must be able to Register/All/Get.
func TestNewRootedDoesNotCallWithRootsUnderLock(t *testing.T) {
	const h model.Harness = "rooted-internal-blocking"
	bp := &blockingRooted{h: h, entered: make(chan struct{}), release: make(chan struct{})}
	registerTemp(t, bp)

	constructed := make(chan struct{})
	go func() {
		defer close(constructed)
		if _, ok := NewRooted(h, "/tmp/rooted-internal-blocking"); !ok {
			t.Errorf("NewRooted(%s) = false, want true", h)
		}
	}()

	select {
	case <-bp.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("WithRoots was never entered")
	}

	locked := make(chan struct{})
	go func() {
		defer close(locked)
		mu.Lock()
		mu.Unlock()
		All()
		Get(model.ClaudeCode)
	}()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		close(bp.release)
		t.Fatal("registry stays locked while a parser's WithRoots runs; constructors must be invoked outside the lock")
	}

	close(bp.release)
	select {
	case <-constructed:
	case <-time.After(5 * time.Second):
		t.Fatal("NewRooted did not return after WithRoots was released")
	}
}
