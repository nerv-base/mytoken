package attrib

import (
	"context"
	"database/sql"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/source"
	"github.com/zzstar101/mytoken/internal/store"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGenericProviderHost(t *testing.T) {
	r := &Resolver{}
	for _, provider := range []string{"custom", "openai-compatible", "default", "proxy", "", " CUSTOM "} {
		for _, tt := range []struct{ base, want string }{{"https://api.relay.example:443/v1", "api.relay.example"}, {"http://api.relay.example:80/v1", "api.relay.example"}, {"https://api.relay.example:8443/v1", "api.relay.example:8443"}, {"https://api.openai.com/v1", "api.openai.com"}, {"http://[::1]:8080/v1", "[::1]:8080"}} {
			got, _ := r.Resolve(t.Context(), model.UsageEvent{Provider: provider, BaseURL: tt.base, Model: "gpt-5"})
			if got != tt.want {
				t.Errorf("%q %q: got %q want %q", provider, tt.base, got, tt.want)
			}
		}
	}
	got, _ := r.Resolve(t.Context(), model.UsageEvent{Provider: "Named", BaseURL: "https://api.relay.example"})
	if got != "Named" {
		t.Fatal(got)
	}
	r.rules = []Rule{{Provider: "custom"}}
	got, kind := r.Resolve(t.Context(), model.UsageEvent{BaseURL: "https://api.relay.example"})
	if got != "custom" || kind != model.AttribUserRule {
		t.Fatal(got, kind)
	}
}

func TestResolutionChain(t *testing.T) {
	dir := t.TempDir()
	dbpath := filepath.Join(dir, "cc.db")
	db, e := sql.Open("sqlite", dbpath)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC().Truncate(time.Second)
	_, e = db.Exec(`CREATE TABLE providers(id TEXT,app_type TEXT,name TEXT,is_current INTEGER);CREATE TABLE proxy_request_logs(provider_id TEXT,app_type TEXT,model TEXT,request_model TEXT,input_tokens INTEGER,output_tokens INTEGER,cache_read_tokens INTEGER,cache_creation_tokens INTEGER,created_at INTEGER,data_source TEXT);INSERT INTO providers VALUES('p','claude','Gateway',1);INSERT INTO proxy_request_logs VALUES('p','claude','routed','claude-sonnet-4',10,20,3,4,?,'proxy');`, now.Unix())
	if e != nil {
		t.Fatal(e)
	}
	db.Close()
	st, e := store.Open(filepath.Join(dir, "own.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	r, e := NewWithPaths(st, dir, dbpath)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	ctx := context.Background()
	event := model.UsageEvent{Harness: model.ClaudeCode, Model: "claude-sonnet-4", Timestamp: now, Tokens: model.Tokens{Input: 10, Output: 20, CacheRead: 3, CacheWrite: 4}}
	p, a := r.Resolve(ctx, event)
	if p != "Gateway" || a != model.AttribCCSwitch {
		t.Fatalf("match %s %s", p, a)
	}
	event.Provider = "Log"
	p, a = r.Resolve(ctx, event)
	if p != "Log" || a != model.AttribLog {
		t.Fatalf("log %s %s", p, a)
	}
	event.Provider = ""
	event.Timestamp = now.Add(121 * time.Second)
	_, a = r.Resolve(ctx, event)
	if a == model.AttribCCSwitch {
		t.Fatal("matched outside time window")
	}
	event.Tokens.Input++
	event.Harness = model.Pi
	p, a = r.Resolve(ctx, event)
	if p != "anthropic" || a != model.AttribInferred {
		t.Fatalf("infer %s %s", p, a)
	}
}
func TestConfigTimelineAndMissingCCSwitch(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".claude"), 0700)
	os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://api.deepseek.com/v1","ANTHROPIC_AUTH_TOKEN":"do-not-store"}}`), 0600)
	st, e := store.Open(filepath.Join(dir, "own.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	r, e := NewWithPaths(st, dir, filepath.Join(dir, "missing.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	p, a := r.Resolve(context.Background(), model.UsageEvent{Harness: model.ClaudeCode, Model: "claude-sonnet-4", Timestamp: time.Now().Add(time.Second)})
	if p != "deepseek" || a != model.AttribConfig {
		t.Fatalf("config %q %q", p, a)
	}
	if _, e = os.Stat(filepath.Join(dir, "missing.db")); !os.IsNotExist(e) {
		t.Fatal("created cc-switch DB")
	}
}

// A resolver with no provider source at all still attributes from user rules,
// the log's own provider and the model name.
func TestResolveWithoutSources(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "own.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r, err := NewWithSources(st)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if len(r.Sources()) != 0 {
		t.Fatalf("sources=%v", r.Sources())
	}
	ctx := context.Background()
	if _, a := r.Resolve(ctx, model.UsageEvent{Provider: "Gateway", Model: "claude-sonnet-4"}); a != model.AttribLog {
		t.Fatal(a)
	}
	p, a := r.Resolve(ctx, model.UsageEvent{Model: "claude-sonnet-4", Timestamp: time.Now()})
	if p != "anthropic" || a != model.AttribInferred {
		t.Fatalf("infer %q %q", p, a)
	}
	// A source that is present but empty behaves the same way.
	r2, err := NewWithSources(st, source.NewCCSwitch(filepath.Join(dir, "absent.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if p, a := r2.Resolve(ctx, model.UsageEvent{Model: "claude-sonnet-4", Timestamp: time.Now()}); p != "anthropic" || a != model.AttribInferred {
		t.Fatalf("empty source %q %q", p, a)
	}
	if s := r2.Sources(); len(s) != 1 || s[0].Status().Exists {
		t.Fatalf("status=%+v", s)
	}
}

// A named instance must not inherit the default instance's global provenance:
// the request matchers and the provider_configs timeline are keyed by harness
// alone, so an instance whose request identity collides with the default's
// still resolves from its own log endpoint, its scoped rules, or the model
// name, never from the default's configured upstream.
func TestResolveInstanceIsolation(t *testing.T) {
	dir := t.TempDir()
	dbpath := filepath.Join(dir, "cc.db")
	db, e := sql.Open("sqlite", dbpath)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC().Truncate(time.Second)
	_, e = db.Exec(`CREATE TABLE providers(id TEXT,app_type TEXT,name TEXT,is_current INTEGER);CREATE TABLE proxy_request_logs(provider_id TEXT,app_type TEXT,model TEXT,request_model TEXT,input_tokens INTEGER,output_tokens INTEGER,cache_read_tokens INTEGER,cache_creation_tokens INTEGER,created_at INTEGER,data_source TEXT);INSERT INTO providers VALUES('p','claude','Gateway',1);INSERT INTO proxy_request_logs VALUES('p','claude','routed','claude-sonnet-4',10,20,3,4,?,'proxy');`, now.Unix())
	if e != nil {
		t.Fatal(e)
	}
	db.Close()
	st, e := store.Open(filepath.Join(dir, "own.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	r, e := NewWithPaths(st, dir, dbpath)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	ctx := context.Background()
	// The same logged request identity under either instance: model, tokens and
	// timestamp all collide, which is exactly what the global matcher keys on.
	logged := model.UsageEvent{Harness: model.ClaudeCode, Model: "claude-sonnet-4", RequestID: "collide", Timestamp: now, Tokens: model.Tokens{Input: 10, Output: 20, CacheRead: 3, CacheWrite: 4}}

	// Default behavior is preserved: Resolve and the default instance still see
	// cc-switch's request match.
	p, a := r.Resolve(ctx, logged)
	if p != "Gateway" || a != model.AttribCCSwitch {
		t.Fatalf("Resolve: %q %q, want Gateway/cc-switch", p, a)
	}
	if p, a = r.ResolveInstance(ctx, store.DefaultInstanceID, logged); p != "Gateway" || a != model.AttribCCSwitch {
		t.Fatalf("default instance: %q %q, want Gateway/cc-switch", p, a)
	}

	// The named instance is excluded from that matcher: no instance-bound
	// provenance exists, so it falls back to the inferred model rather than
	// claiming the default's upstream.
	p, a = r.ResolveInstance(ctx, "custom", logged)
	if p != "anthropic" || a != model.AttribInferred {
		t.Fatalf("colliding request under custom: %q %q, want anthropic/inferred", p, a)
	}

	// A recorded endpoint belongs to the instance that wrote it and stays exact.
	endpoint := logged
	endpoint.RequestID = "own"
	endpoint.Provider = "custom"
	endpoint.BaseURL = "https://api.relay.example:8443/v1"
	p, a = r.ResolveInstance(ctx, "custom", endpoint)
	if p != "api.relay.example:8443" || a != model.AttribLog {
		t.Fatalf("recorded endpoint: %q %q, want api.relay.example:8443/log", p, a)
	}

	// An explicit global (instance-less) user rule keeps its deliberate scope.
	r.rules = []Rule{{Provider: "Global"}}
	if p, a = r.ResolveInstance(ctx, "custom", logged); p != "Global" || a != model.AttribUserRule {
		t.Fatalf("global rule under custom: %q %q", p, a)
	}
	if p, a = r.ResolveInstance(ctx, store.DefaultInstanceID, logged); p != "Global" || a != model.AttribUserRule {
		t.Fatalf("global rule under default: %q %q", p, a)
	}

	// A rule naming an instance applies there and nowhere else.
	r.rules = []Rule{{Instance: "custom", Provider: "OnlyCustom"}}
	if p, a = r.ResolveInstance(ctx, "custom", logged); p != "OnlyCustom" || a != model.AttribUserRule {
		t.Fatalf("scoped rule under custom: %q %q", p, a)
	}
	future := model.UsageEvent{Harness: model.ClaudeCode, Model: "claude-sonnet-4", Timestamp: now.Add(10 * time.Minute)}
	// The same rule does not leak to another named instance, nor to the default
	// one: each keeps its own provenance.
	if p, a = r.ResolveInstance(ctx, "other", future); p != "anthropic" || a != model.AttribInferred {
		t.Fatalf("scoped rule leaked to another instance: %q %q", p, a)
	}
	if p, a = r.ResolveInstance(ctx, "custom", future); p != "OnlyCustom" || a != model.AttribUserRule {
		t.Fatalf("scoped rule under custom (later event): %q %q", p, a)
	}
	// The default instance's seeded config timeline (A = "Gateway") applies to
	// it and must not stand in for the named instance above.
	if p, a = r.ResolveInstance(ctx, store.DefaultInstanceID, future); p != "Gateway" || a != model.AttribConfig {
		t.Fatalf("default config timeline: %q %q, want Gateway/config-timeline", p, a)
	}
	// The empty instance id is the default instance: same timeline, same result.
	if p, a = r.ResolveInstance(ctx, "", future); p != "Gateway" || a != model.AttribConfig {
		t.Fatalf("empty instance id: %q %q, want Gateway/config-timeline", p, a)
	}
}
