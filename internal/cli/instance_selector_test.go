package cli

import (
	"context"
	"encoding/json"
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/app"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
	"github.com/zzstar101/mytoken/internal/store"
)

// seedInstances installs a fresh MYTOKEN_HOME and commits one batch per write,
// each with its own instance id, so instance intersections are observable.
func seedInstances(t *testing.T, writes ...store.Write) {
	t.Helper()
	t.Setenv("MYTOKEN_HOME", t.TempDir())
	a, err := app.OpenLocal()
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Store.CommitMany(t.Context(), writes); err != nil {
		a.Close()
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
}

func instanceWrite(instanceID, dedup, session string) store.Write {
	return store.Write{
		Harness:    model.Codex,
		InstanceID: instanceID,
		Path:       "fixture/" + instanceID,
		Batch: harness.Batch{Events: []model.UsageEvent{{
			Harness: model.Codex, DedupKey: dedup, SessionID: session, Model: "gpt-5",
			Timestamp: time.Now(), Tokens: model.Tokens{Input: 20},
		}}},
	}
}

// TestInstanceIDFlagIsRepeatableAndIntersecting pins the shared flag itself.
func TestInstanceIDFlagIsRepeatableAndIntersecting(t *testing.T) {
	var cf commonFlags
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	addCommonFlags(fs, &cf, "7d")
	if err := cf.parse(fs, []string{"--instance-id", "a", "--instance-id", "b"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	opts, err := cf.resolve(time.Now())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := opts.filter().InstanceIDs
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("InstanceIDs = %v", got)
	}
	// No selector at all stays "every instance", never a single implicit one.
	var bare commonFlags
	fs = flag.NewFlagSet("test", flag.ContinueOnError)
	addCommonFlags(fs, &bare, "7d")
	if err := bare.parse(fs, nil); err != nil {
		t.Fatal(err)
	}
	opts, err = bare.resolve(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ids := opts.filter().InstanceIDs; len(ids) != 0 {
		t.Fatalf("unset --instance-id produced %v", ids)
	}
}

func TestInstanceIDEmptyValueIsRejected(t *testing.T) {
	t.Setenv("MYTOKEN_HOME", t.TempDir())
	for _, args := range [][]string{
		{"stats", "--instance-id", "", "--json"},
		{"sessions", "--instance-id", "", "--json"},
		{"cache", "--instance-id", "", "--json"},
		{"provider", "list", "--instance-id", "", "--json"},
	} {
		_, errout, code := runCLI(t, args...)
		if code != 2 {
			t.Errorf("%v: code=%d stderr=%s", args, code, errout)
		}
	}
}

// TestInstanceIDNarrowsStatsAndSessions is the end-to-end intersection check:
// the same index holds two instances, and the selector must narrow every view.
func TestInstanceIDNarrowsStatsAndSessions(t *testing.T) {
	seedInstances(t, instanceWrite("inst_a", "a1", "sa"), instanceWrite("inst_b", "b1", "sb"))

	out, errout, code := runCLI(t, "stats", "--offline", "--json", "--by", "model", "--since", "7d")
	if code != 0 {
		t.Fatalf("all-instances stats code=%d stderr=%s", code, errout)
	}
	var all statsJSON
	if err := json.Unmarshal([]byte(out), &all); err != nil {
		t.Fatal(err)
	}
	if all.Totals.Requests != 2 {
		t.Fatalf("all instances: requests=%d, want 2", all.Totals.Requests)
	}

	out, errout, code = runCLI(t, "stats", "--offline", "--json", "--by", "model", "--since", "7d", "--instance-id", "inst_a")
	if code != 0 {
		t.Fatalf("scoped stats code=%d stderr=%s", code, errout)
	}
	var scoped statsJSON
	if err := json.Unmarshal([]byte(out), &scoped); err != nil {
		t.Fatal(err)
	}
	if scoped.Totals.Requests != 1 {
		t.Fatalf("inst_a: requests=%d, want 1", scoped.Totals.Requests)
	}

	out, errout, code = runCLI(t, "sessions", "--offline", "--json", "--instance-id", "inst_b")
	if code != 0 {
		t.Fatalf("scoped sessions code=%d stderr=%s", code, errout)
	}
	var sess struct {
		Total    int `json:"total"`
		Sessions []struct {
			SessionID  string `json:"sessionId"`
			InstanceID string `json:"instanceId"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out), &sess); err != nil {
		t.Fatal(err)
	}
	if sess.Total != 1 || len(sess.Sessions) != 1 || sess.Sessions[0].SessionID != "sb" || sess.Sessions[0].InstanceID != "inst_b" {
		t.Fatalf("sessions = %s", out)
	}

	// An unknown instance matches nothing rather than everything.
	out, _, code = runCLI(t, "stats", "--offline", "--json", "--by", "model", "--since", "7d", "--instance-id", "inst_missing")
	if code != 0 {
		t.Fatalf("unknown-instance stats code=%d", code)
	}
	var missing statsJSON
	if err := json.Unmarshal([]byte(out), &missing); err != nil {
		t.Fatal(err)
	}
	if missing.Totals.Requests != 0 {
		t.Fatalf("unknown instance matched %d requests", missing.Totals.Requests)
	}

	// cache and provider list accept the selector on their ledger paths.
	if _, errout, code = runCLI(t, "cache", "--offline", "--json", "--instance-id", "inst_a"); code != 0 {
		t.Fatalf("scoped cache code=%d stderr=%s", code, errout)
	}
	if _, errout, code = runCLI(t, "provider", "list", "--offline", "--json", "--instance-id", "inst_a"); code != 0 {
		t.Fatalf("scoped provider list code=%d stderr=%s", code, errout)
	}
}

// TestSessionDrilldownIsInstanceAware covers the explicit drilldown: the same
// session id exists in two instances, so the instance-less read is ambiguous
// and the instance-scoped read must return exactly that instance's session.
func TestSessionDrilldownIsInstanceAware(t *testing.T) {
	seedInstances(t, instanceWrite("inst_a", "a1", "shared"), instanceWrite("inst_b", "b1", "shared"))

	out, errout, code := runCLI(t, "sessions", "shared", "--offline", "--harness", "codex", "--json")
	if code != 1 || !strings.Contains(errout, "more than one instance") {
		t.Fatalf("ambiguous drilldown: code=%d stderr=%s out=%s", code, errout, out)
	}

	out, errout, code = runCLI(t, "sessions", "shared", "--offline", "--harness", "codex", "--instance-id", "inst_a", "--json")
	if code != 0 {
		t.Fatalf("scoped drilldown code=%d stderr=%s", code, errout)
	}
	var detail struct {
		Session struct {
			SessionID  string `json:"sessionId"`
			InstanceID string `json:"instanceId"`
			Harness    string `json:"harness"`
		} `json:"session"`
		Children []json.RawMessage `json:"children"`
		Events   []struct {
			InstanceID string `json:"instanceId"`
			DedupKey   string `json:"dedupKey"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(out), &detail); err != nil {
		t.Fatalf("drilldown json %s: %v", out, err)
	}
	if detail.Session.SessionID != "shared" || detail.Session.InstanceID != "inst_a" || detail.Session.Harness != "codex" {
		t.Fatalf("session = %+v", detail.Session)
	}
	if len(detail.Events) != 1 || detail.Events[0].InstanceID != "inst_a" || detail.Events[0].DedupKey != "a1" {
		t.Fatalf("events = %+v", detail.Events)
	}

	// Without an instance selector the same session is readable when unique.
	seedInstances(t, instanceWrite("inst_a", "a1", "lonely"))
	if out, errout, code = runCLI(t, "sessions", "lonely", "--offline", "--json"); code != 0 {
		t.Fatalf("unique drilldown code=%d stderr=%s", code, errout)
	}
	var lonely struct {
		Session struct {
			Harness string `json:"harness"`
		} `json:"session"`
	}
	if err := json.Unmarshal([]byte(out), &lonely); err != nil {
		t.Fatal(err)
	}
	if lonely.Session.Harness != "codex" {
		t.Fatalf("harness resolved to %q", lonely.Session.Harness)
	}

	// A session that does not exist is a clear operational error.
	_, errout, code = runCLI(t, "sessions", "nosuch", "--offline", "--harness", "codex", "--json")
	if code != 1 || !strings.Contains(errout, "no session") {
		t.Fatalf("missing session: code=%d stderr=%s", code, errout)
	}
}

func TestSessionDrilldownRejectsUnsupportedCombinations(t *testing.T) {
	t.Setenv("MYTOKEN_HOME", t.TempDir())
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"sessions", "s1", "s2"}, "Usage: mytoken sessions"},
		{[]string{"sessions", "s1", "--instance-id", "a", "--instance-id", "b"}, "at most one --instance-id"},
		{[]string{"sessions", "s1", "--limit", "5"}, "--limit cannot be combined"},
		{[]string{"sessions", "s1", "--harness", "nope"}, "invalid --harness"},
		{[]string{"sessions", "s1", "--provider", "openai"}, "--provider cannot be combined with a session id"},
		{[]string{"sessions", "s1", "--provider-id", "abc"}, "--provider-id cannot be combined with a session id"},
	}
	for _, c := range cases {
		_, errout, code := runCLI(t, c.args...)
		if code != 2 {
			t.Errorf("%v: code=%d stderr=%s", c.args, code, errout)
		}
		if !strings.Contains(errout, c.want) {
			t.Errorf("%v: stderr %q missing %q", c.args, errout, c.want)
		}
	}
}

// TestCacheTimelineUsesInstanceScopedLookup drives the timeline with a fake so
// the exact call is observable.
func TestCacheTimelineUsesInstanceScopedLookup(t *testing.T) {
	svc := &instanceFakeCache{steps: testCacheSteps()}
	env := cacheTestEnv(svc, formatJSON, false)
	env.harness = "codex"
	env.instanceIDs = []string{"inst_a"}
	_, errout, code := runCacheEnv(t, env, "cause", "sess-1")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	if len(svc.inCalls) != 1 || svc.inCalls[0] != "inst_a/sess-1" {
		t.Fatalf("SessionCacheIn calls = %v (plain calls %v)", svc.inCalls, svc.calls)
	}
}

// instanceFakeCache is a cacheService plus the instance-aware timeline read.
type instanceFakeCache struct {
	steps   []query.CacheStep
	err     error
	calls   []string
	inCalls []string
}

func (f *instanceFakeCache) CacheTotals(context.Context, query.Filter) (query.CacheTotals, error) {
	f.calls = append(f.calls, "totals")
	return query.CacheTotals{}, f.err
}

func (f *instanceFakeCache) CacheDaily(context.Context, query.Filter) ([]query.CachePoint, error) {
	f.calls = append(f.calls, "daily")
	return nil, f.err
}

func (f *instanceFakeCache) CacheBy(_ context.Context, _ query.Filter, dim query.CacheDim) ([]query.CacheBucket, error) {
	f.calls = append(f.calls, "by:"+string(dim))
	return nil, f.err
}

func (f *instanceFakeCache) SessionCache(_ context.Context, h model.Harness, session string) ([]query.CacheStep, error) {
	f.calls = append(f.calls, "session")
	return f.steps, f.err
}

func (f *instanceFakeCache) SessionCacheIn(_ context.Context, h model.Harness, instance, session string) ([]query.CacheStep, error) {
	f.inCalls = append(f.inCalls, instance+"/"+session)
	return f.steps, f.err
}

func TestCacheTimelineRejectsSeveralInstances(t *testing.T) {
	t.Setenv("MYTOKEN_HOME", t.TempDir())
	_, errout, code := runCLI(t, "cache", "sess-1", "--instance-id", "a", "--instance-id", "b")
	if code != 2 || !strings.Contains(errout, "at most one --instance-id") {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
}

// TestUnsupportedCommandsRejectInstanceID: a command with no instance dimension
// must fail loudly instead of accepting and ignoring the selector.
func TestUnsupportedCommandsRejectInstanceID(t *testing.T) {
	t.Setenv("MYTOKEN_HOME", t.TempDir())
	for _, args := range [][]string{
		{"reconcile", "--instance-id", "inst_a"},
		{"scan", "--instance-id", "inst_a"},
		{"relay", "list", "--instance-id", "inst_a"},
		{"prices", "list", "--instance-id", "inst_a"},
		{"doctor", "--instance-id", "inst_a"},
	} {
		_, errout, code := runCLI(t, args...)
		if code != 2 {
			t.Errorf("%v: code=%d stderr=%s", args, code, errout)
		}
		if errout == "" {
			t.Errorf("%v: silently accepted --instance-id", args)
		}
	}
}

// TestReconcileNamesInstanceIDAsUnsupportedSelector: reconcile must explain why
// the ledger filter cannot narrow a relay comparison.
func TestReconcileNamesInstanceIDAsUnsupportedSelector(t *testing.T) {
	t.Setenv("MYTOKEN_HOME", t.TempDir())
	_, errout, code := runCLI(t, "reconcile", "--instance-id", "inst_a")
	if code != 2 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	if !strings.Contains(errout, "unsupported selector --instance-id") {
		t.Fatalf("stderr = %q", errout)
	}
}
