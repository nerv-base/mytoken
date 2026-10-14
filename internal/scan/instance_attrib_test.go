package scan

import (
	"context"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/attrib"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/store"
)

// fixedParser yields prepared events once per discovered source. It exists so a
// scanner test can hand the resolver an event with a recorded provider and base
// URL, which the path-based fixtures never do.
type fixedParser struct {
	id     model.Harness
	events []model.UsageEvent
}

func (p *fixedParser) Harness() model.Harness { return p.id }
func (p *fixedParser) Roots() []string        { return nil }
func (p *fixedParser) Discover(context.Context) ([]harness.Source, error) {
	return []harness.Source{{Path: "fixed-" + string(p.id), Kind: "jsonl"}}, nil
}
func (p *fixedParser) Parse(context.Context, harness.Source, harness.Cursor) (harness.Batch, error) {
	return harness.Batch{Events: p.events, Next: harness.Cursor{Offset: 1, Size: 1}}, nil
}

// resolvedRows reads each instance's recorded provider and attribution.
func resolvedRows(t *testing.T, st *store.Store) map[string]string {
	t.Helper()
	rows, err := st.DB().Query("SELECT instance_id, resolved_provider, attrib FROM events ORDER BY instance_id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var instance, provider, attrib string
		if err := rows.Scan(&instance, &provider, &attrib); err != nil {
			t.Fatal(err)
		}
		out[instance] = provider + "/" + attrib
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// seedProfileA writes a harness-global config timeline: the default instance is
// configured against ProfileA, and nothing binds that choice to any other id.
func seedProfileA(t *testing.T, st *store.Store, h model.Harness) {
	t.Helper()
	if err := st.Exec(context.Background(), "INSERT INTO provider_configs VALUES(?,?,?,?)", h, 0, "", "ProfileA"); err != nil {
		t.Fatal(err)
	}
}

func newInstanceResolver(t *testing.T, st *store.Store) *attrib.Resolver {
	t.Helper()
	r, err := attrib.NewWithSources(st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// Two instances over the same harness emit the same request identity. The
// default instance resolves from the global config timeline; the named one must
// not, because nothing binds that timeline to it.
func TestScanInstanceDoesNotInheritGlobalConfig(t *testing.T) {
	st := openInstanceStore(t)
	ctx := context.Background()
	h := model.Harness("fixture")
	seedProfileA(t, st, h)
	logged := model.UsageEvent{DedupKey: "same", SessionID: "s", Model: "gpt-5", Timestamp: time.Now(), Tokens: model.Tokens{Input: 1}}
	sc, err := NewWithInstances(st, newInstanceResolver(t, st), nil,
		Instance{ID: store.DefaultInstanceID, Parser: &fixedParser{id: h, events: []model.UsageEvent{logged}}},
		Instance{ID: "custom", Parser: &fixedParser{id: h, events: []model.UsageEvent{logged}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		store.DefaultInstanceID: "ProfileA/config-timeline",
		"custom":                "openai/inferred",
	}
	if got := resolvedRows(t, st); !equalRows(got, want) {
		t.Fatalf("rows=%v, want %v", got, want)
	}
	// Rescanning the same content keeps both rows, unchanged.
	if err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if got := resolvedRows(t, st); !equalRows(got, want) {
		t.Fatalf("rescan rows=%v, want %v", got, want)
	}
}

// A named instance's own recorded endpoint is exact and keeps its attribution,
// while the default instance still follows the configured timeline.
func TestScanInstanceKeepsRecordedEndpoint(t *testing.T) {
	st := openInstanceStore(t)
	ctx := context.Background()
	h := model.Harness("fixture")
	seedProfileA(t, st, h)
	plain := model.UsageEvent{DedupKey: "a", SessionID: "s", Model: "gpt-5", Timestamp: time.Now(), Tokens: model.Tokens{Input: 1}}
	endpoint := model.UsageEvent{DedupKey: "b", SessionID: "s", Model: "gpt-5", Timestamp: time.Now(), Tokens: model.Tokens{Input: 1}, Provider: "custom", BaseURL: "https://api.relay.example/v1"}
	sc, err := NewWithInstances(st, newInstanceResolver(t, st), nil,
		Instance{ID: store.DefaultInstanceID, Parser: &fixedParser{id: h, events: []model.UsageEvent{plain}}},
		Instance{ID: "custom", Parser: &fixedParser{id: h, events: []model.UsageEvent{endpoint}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		store.DefaultInstanceID: "ProfileA/config-timeline",
		"custom":                "api.relay.example/log",
	}
	if got := resolvedRows(t, st); !equalRows(got, want) {
		t.Fatalf("rows=%v, want %v", got, want)
	}
}

func equalRows(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}
