package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzstar101/mytoken/internal/app"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/paths"
	"github.com/zzstar101/mytoken/internal/provider"
	"github.com/zzstar101/mytoken/internal/store"
)

// Stable provider identity CLI tests.
//
// The fixture below pins every endpoint shape the identity layer cares about:
//
//   - one endpoint reached under two different pricing provider names
//   - a path-distinct endpoint and a host-distinct (tenant) endpoint
//   - a raw endpoint whose query string would leak a secret if printed, which
//     NormalizeEndpoint rejects, yielding one unresolved identity per endpoint
//     instance (never one per session)
//   - a provider name carrying CSV-hostile characters
const (
	idMainEndpoint    = "https://api.example.com/v1"
	idAltEndpoint     = "https://api.example.com/v1/alt"
	idTenantEndpoint  = "https://tenant-b.example.com/v1"
	idRawEndpoint     = "https://api.example.com/v1?key=SECRETVALUE"
	idWeirdEndpoint   = "https://api.example.com/v1?token=SECRETVALUE"
	idWeirdProvider   = `weird,"label`
	idRangeSince      = "2026-03-01"
	idRangeUntil      = "2026-04-01"
	idSecretNeedle    = "SECRETVALUE"
	idRawNeedle       = "key="
	idWeirdLabelProbe = `weird,"label`
)

type idEvent struct {
	harness   model.Harness
	session   string
	provider  string
	base      string
	project   string
	input     int64
	dedup     string
	timestamp time.Time
}

// idAt builds a timestamp inside the fixture range.
func idAt(day int) time.Time {
	return time.Date(2026, 3, day, 12, 0, 0, 0, time.UTC)
}

// seedIdentities installs a fresh MYTOKEN_HOME holding the identity fixture and
// closes the store. Every write carries a resolution so the pricing fingerprint
// (deleted by a resolution-less Commit) survives, which is what the alias
// invariance test compares.
func seedIdentities(t *testing.T, events ...idEvent) {
	t.Helper()
	t.Setenv("MYTOKEN_HOME", t.TempDir())
	a, e := app.OpenLocal()
	if e != nil {
		t.Fatal(e)
	}
	for i, ev := range events {
		ev.dedup = fmt.Sprintf("d-%02d", i)
		if ev.timestamp.IsZero() {
			ev.timestamp = idAt(2 + i%20)
		}
		ue := model.UsageEvent{
			Harness:     ev.harness,
			DedupKey:    ev.dedup,
			SessionID:   ev.session,
			ProjectPath: ev.project,
			Timestamp:   ev.timestamp,
			Model:       "gpt-5",
			Provider:    ev.provider,
			BaseURL:     ev.base,
			Tokens:      model.Tokens{Input: ev.input, Output: 1},
		}
		res := []store.Resolution{{
			Provider: ev.provider,
			Attrib:   model.AttribLog,
			Cost:     0.002,
			Priced:   true,
		}}
		if e := a.Store.Commit(t.Context(), ev.harness, "test", harness.Batch{Events: []model.UsageEvent{ue}}, res); e != nil {
			a.Close()
			t.Fatalf("commit %s: %v", ev.dedup, e)
		}
	}
	if e := a.Close(); e != nil {
		t.Fatal(e)
	}
}

func identityFixture() []idEvent {
	return []idEvent{
		{harness: model.Codex, session: "s-main", provider: "name-a", base: idMainEndpoint, project: "/p/one", input: 100},
		{harness: model.ClaudeCode, session: "s-alt", provider: "name-b", base: idMainEndpoint, project: "/p/two", input: 200},
		{harness: model.Codex, session: "s-path", provider: "name-c", base: idAltEndpoint, project: "/p/one", input: 400},
		{harness: model.Codex, session: "s-tenant", provider: "name-c", base: idTenantEndpoint, project: "/p/one", input: 800},
		{harness: model.Codex, session: "s-raw-1", provider: "name-d", base: idRawEndpoint, project: "/p/one", input: 50},
		{harness: model.Codex, session: "s-raw-2", provider: "name-d", base: idRawEndpoint, project: "/p/one", input: 60},
		{harness: model.Codex, session: "s-weird", provider: idWeirdProvider, base: idWeirdEndpoint, project: "/p/one", input: 10},
	}
}

// idRangeArgs is the deterministic window every real-store command uses.
func idRangeArgs(extra ...string) []string {
	base := []string{"--since", idRangeSince, "--until", idRangeUntil, "--timezone", "UTC", "--offline"}
	return append(base, extra...)
}

func idEndpointID(t *testing.T, raw string) string {
	t.Helper()
	id, ok := provider.EndpointID(raw)
	if !ok {
		t.Fatalf("EndpointID(%q) not recognized", raw)
	}
	return id
}

type idGroupJSON struct {
	Key        string       `json:"key"`
	Label      string       `json:"label"`
	Identified bool         `json:"identified"`
	Requests   int64        `json:"requests"`
	Sessions   int64        `json:"sessions"`
	Tokens     model.Tokens `json:"tokens"`
	CostUSD    float64      `json:"costUsd"`
	Unpriced   int64        `json:"unpriced"`
	Members    []struct {
		Harness  string `json:"harness"`
		Provider string `json:"provider"`
	} `json:"members"`
}

type idListJSON struct {
	By     string        `json:"by"`
	Groups []idGroupJSON `json:"groups"`
}

func decodeIDList(t *testing.T, out string) idListJSON {
	t.Helper()
	var data idListJSON
	if e := json.Unmarshal([]byte(out), &data); e != nil {
		t.Fatalf("bad JSON: %v\n%s", e, out)
	}
	return data
}

func idGroup(t *testing.T, groups []idGroupJSON, key string) idGroupJSON {
	t.Helper()
	for _, g := range groups {
		if g.Key == key {
			return g
		}
	}
	t.Fatalf("no group %q in %+v", key, groups)
	return idGroupJSON{}
}

func idMemberNames(g idGroupJSON) []string {
	out := make([]string, 0, len(g.Members))
	for _, m := range g.Members {
		out = append(out, m.Harness+"/"+m.Provider)
	}
	return out
}

// TestProviderListEndpointIdentity is the core contract: `provider list` groups
// requests by stable endpoint identity, keeps two original names on one endpoint
// in a single group, separates tenant/path endpoints, merges every session of an
// unresolved endpoint into one unidentified entry, and never prints the raw
// endpoint query string.
func TestProviderListEndpointIdentity(t *testing.T) {
	seedIdentities(t, identityFixture()...)

	out, errout, code := runCLI(t, append([]string{"provider", "list", "--json"}, idRangeArgs()...)...)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	if strings.Contains(out, idSecretNeedle) || strings.Contains(out, idRawNeedle) {
		t.Fatalf("provider list leaked a raw endpoint query string:\n%s", out)
	}
	data := decodeIDList(t, out)
	if data.By != "endpoint" {
		t.Errorf("by = %q, want endpoint", data.By)
	}
	if len(data.Groups) != 5 {
		t.Fatalf("want 5 identities (main, alt path, tenant, 1 raw endpoint, 1 weird), got %d: %+v", len(data.Groups), data.Groups)
	}

	main := idGroup(t, data.Groups, idEndpointID(t, idMainEndpoint))
	if main.Label != idMainEndpoint || !main.Identified {
		t.Errorf("main group = %+v, want label %q identified", main, idMainEndpoint)
	}
	if main.Requests != 2 || main.Sessions != 2 {
		t.Errorf("main requests/sessions = %d/%d, want 2/2", main.Requests, main.Sessions)
	}
	if got := idMemberNames(main); len(got) != 2 || got[0] != "claude-code/name-b" || got[1] != "codex/name-a" {
		t.Errorf("main members = %v, want [claude-code/name-b codex/name-a]", got)
	}

	if alt := idGroup(t, data.Groups, idEndpointID(t, idAltEndpoint)); alt.Requests != 1 || !alt.Identified {
		t.Errorf("alt path group = %+v, want 1 identified request", alt)
	}
	if tenant := idGroup(t, data.Groups, idEndpointID(t, idTenantEndpoint)); tenant.Requests != 1 || !tenant.Identified {
		t.Errorf("tenant group = %+v, want 1 identified request", tenant)
	}

	byLabel := map[string]int{}
	byLabelRequests := map[string]int64{}
	byLabelSessions := map[string]int64{}
	unresolved := 0
	for _, g := range data.Groups {
		if g.Identified {
			continue
		}
		unresolved++
		byLabel[g.Label]++
		byLabelRequests[g.Label] += g.Requests
		byLabelSessions[g.Label] += g.Sessions
		if !strings.HasPrefix(g.Key, "unresolved:v1:") {
			t.Errorf("unresolved key = %q, want unresolved:v1: prefix", g.Key)
		}
		if g.Label == idWeirdLabelProbe && g.Requests != 1 {
			t.Errorf("unresolved escaped-label group requests = %d, want 1", g.Requests)
		}
	}
	if unresolved != 2 {
		t.Errorf("unresolved groups = %d, want 2 (one raw endpoint, one escaped label)", unresolved)
	}
	if byLabel["name-d"] != 1 {
		t.Errorf("unresolved name-d groups = %d, want 1 (both sessions share one endpoint)", byLabel["name-d"])
	}
	if byLabelRequests["name-d"] != 2 || byLabelSessions["name-d"] != 2 {
		t.Errorf("unresolved name-d requests/sessions = %d/%d, want 2/2 (two sessions merged)",
			byLabelRequests["name-d"], byLabelSessions["name-d"])
	}
	if byLabel[idWeirdLabelProbe] != 1 {
		t.Errorf("unresolved escaped label groups = %d, want 1", byLabel[idWeirdLabelProbe])
	}

	// Unknown IDs select nothing, never everything.
	out, errout, code = runCLI(t, append([]string{"provider", "list", "--json", "--provider-id", "endpoint:v1:deadbeef"}, idRangeArgs()...)...)
	if code != 0 {
		t.Fatalf("unknown id: code=%d stderr=%s", code, errout)
	}
	if got := len(decodeIDList(t, out).Groups); got != 0 {
		t.Errorf("unknown id listed %d groups, want 0", got)
	}

	// An identity filter narrows the list to the selected endpoint.
	out, errout, code = runCLI(t, append([]string{"provider", "list", "--json", "--provider-id", idEndpointID(t, idMainEndpoint)}, idRangeArgs()...)...)
	if code != 0 {
		t.Fatalf("filtered list: code=%d stderr=%s", code, errout)
	}
	filtered := decodeIDList(t, out)
	if len(filtered.Groups) != 1 || filtered.Groups[0].Requests != 2 {
		t.Errorf("filtered list = %+v, want only the 2-request main endpoint", filtered.Groups)
	}
}

// TestProviderListFormats pins the table and CSV shapes, including --no-cost and
// the hostile provider label that must survive CSV quoting.
func TestProviderListFormats(t *testing.T) {
	seedIdentities(t, identityFixture()...)
	mainID := idEndpointID(t, idMainEndpoint)

	t.Run("table", func(t *testing.T) {
		out, errout, code := runCLI(t, append([]string{"provider", "list"}, idRangeArgs()...)...)
		if code != 0 {
			t.Fatalf("code=%d stderr=%s", code, errout)
		}
		for _, want := range []string{"Identities: 5", "ID", "LABEL", "IDENTIFIED", "MEMBERS", mainID, "claude-code/name-b, codex/name-a"} {
			if !strings.Contains(out, want) {
				t.Errorf("table missing %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, idSecretNeedle) {
			t.Errorf("table leaked a secret:\n%s", out)
		}
	})

	t.Run("csv", func(t *testing.T) {
		out, errout, code := runCLI(t, append([]string{"provider", "list", "--format", "csv"}, idRangeArgs()...)...)
		if code != 0 {
			t.Fatalf("code=%d stderr=%s", code, errout)
		}
		recs := parseCSV(t, out)
		if len(recs) != 1+5 {
			t.Fatalf("want header + 5 rows, got %v", recs)
		}
		if got := recs[0]; len(got) != len(providerCSVHeader) {
			t.Fatalf("header width = %d, want %d: %v", len(got), len(providerCSVHeader), got)
		}
		var main, weird []string
		for _, r := range recs[1:] {
			if r[0] == mainID {
				main = r
			}
			if r[1] == idWeirdLabelProbe {
				weird = r
			}
		}
		if main == nil {
			t.Fatalf("no main row in %v", recs)
		}
		if main[1] != idMainEndpoint || main[2] != "true" || main[3] != "2" {
			t.Errorf("main row = %v", main)
		}
		if got := main[len(main)-1]; got != "claude-code/name-b;codex/name-a" {
			t.Errorf("members cell = %q", got)
		}
		if weird == nil {
			t.Errorf("hostile label row missing (CSV round-trip failed): %v", recs)
		}
	})

	t.Run("no-cost drops the column", func(t *testing.T) {
		out, errout, code := runCLI(t, append([]string{"provider", "list", "--format", "csv", "--no-cost"}, idRangeArgs()...)...)
		if code != 0 {
			t.Fatalf("code=%d stderr=%s", code, errout)
		}
		recs := parseCSV(t, out)
		want := csvHeaderFor(providerCSVHeader, true)
		if len(recs[0]) != len(want) {
			t.Fatalf("header = %v, want %v", recs[0], want)
		}
	})
}

// TestProviderListJSONEscaping keeps a provider name with a quote and comma
// intact through JSON.
func TestProviderListJSONEscaping(t *testing.T) {
	seedIdentities(t, identityFixture()...)
	out, errout, code := runCLI(t, append([]string{"provider", "list", "--json"}, idRangeArgs()...)...)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	data := decodeIDList(t, out)
	found := false
	for _, g := range data.Groups {
		if g.Label == idWeirdLabelProbe {
			found = true
		}
	}
	if !found {
		t.Errorf("JSON lost the quoted label %q: %+v", idWeirdLabelProbe, data.Groups)
	}
	if !json.Valid([]byte(out)) {
		t.Errorf("provider list --json is not valid JSON:\n%s", out)
	}
}

// TestStableIDFilteringAcrossCommands checks that stats, sessions and cache all
// accept the repeatable --provider-id filter, that --provider keeps its
// original pricing-name semantics, and that the two intersect rather than
// overriding each other.
func TestStableIDFilteringAcrossCommands(t *testing.T) {
	seedIdentities(t, identityFixture()...)
	mainID := idEndpointID(t, idMainEndpoint)
	tenantID := idEndpointID(t, idTenantEndpoint)

	requests := func(t *testing.T, args ...string) int64 {
		t.Helper()
		out, errout, code := runCLI(t, args...)
		if code != 0 {
			t.Fatalf("%v: code=%d stderr=%s", args, code, errout)
		}
		return decodeStats(t, out).Totals.Requests
	}

	if got := requests(t, append([]string{"stats", "--by", "model", "--json", "--provider-id", mainID}, idRangeArgs()...)...); got != 2 {
		t.Errorf("stats --provider-id main = %d requests, want 2", got)
	}
	if got := requests(t, append([]string{"stats", "--by", "model", "--json", "--provider-id", tenantID}, idRangeArgs()...)...); got != 1 {
		t.Errorf("stats --provider-id tenant = %d requests, want 1 (tenant separation)", got)
	}
	if got := requests(t, append([]string{"stats", "--by", "model", "--json", "--provider-id", "endpoint:v1:nope"}, idRangeArgs()...)...); got != 0 {
		t.Errorf("stats unknown --provider-id = %d requests, want 0 (never fallback to all)", got)
	}
	if got := requests(t, append([]string{"stats", "--by", "model", "--json", "--provider", "name-c"}, idRangeArgs()...)...); got != 2 {
		t.Errorf("stats --provider name-c = %d requests, want 2", got)
	}
	if got := requests(t, append([]string{"stats", "--by", "model", "--json", "--provider", "name-a", "--provider-id", mainID}, idRangeArgs()...)...); got != 1 {
		t.Errorf("--provider name-a ∩ --provider-id main = %d requests, want 1", got)
	}
	if got := requests(t, append([]string{"stats", "--by", "model", "--json", "--provider", "name-b", "--provider-id", mainID}, idRangeArgs()...)...); got != 1 {
		t.Errorf("--provider name-b ∩ --provider-id main = %d requests, want 1", got)
	}
	if got := requests(t, append([]string{"stats", "--by", "model", "--json", "--provider", "name-a", "--provider-id", tenantID}, idRangeArgs()...)...); got != 0 {
		t.Errorf("disjoint provider ∩ provider-id = %d requests, want 0", got)
	}

	t.Run("sessions", func(t *testing.T) {
		out, errout, code := runCLI(t, append([]string{"sessions", "--json", "--provider-id", mainID}, idRangeArgs()...)...)
		if code != 0 {
			t.Fatalf("code=%d stderr=%s", code, errout)
		}
		var rep struct {
			Total    int64 `json:"total"`
			Sessions []struct {
				SessionID string `json:"sessionId"`
			} `json:"sessions"`
		}
		if e := json.Unmarshal([]byte(out), &rep); e != nil {
			t.Fatalf("bad JSON: %v\n%s", e, out)
		}
		if rep.Total != 2 {
			t.Errorf("sessions --provider-id main total = %d, want 2", rep.Total)
		}
	})

	t.Run("cache", func(t *testing.T) {
		out, errout, code := runCLI(t, append([]string{"cache", "--by", "cause", "--json", "--provider-id", mainID}, idRangeArgs()...)...)
		if code != 0 {
			t.Fatalf("code=%d stderr=%s", code, errout)
		}
		var rep struct {
			Totals struct {
				Requests int64 `json:"requests"`
			} `json:"totals"`
		}
		if e := json.Unmarshal([]byte(out), &rep); e != nil {
			t.Fatalf("bad JSON: %v\n%s", e, out)
		}
		if rep.Totals.Requests != 2 {
			t.Errorf("cache --provider-id main requests = %d, want 2", rep.Totals.Requests)
		}
		out, errout, code = runCLI(t, append([]string{"cache", "--by", "cause", "--json", "--provider-id", "endpoint:v1:nope"}, idRangeArgs()...)...)
		if code != 0 {
			t.Fatalf("unknown id: code=%d stderr=%s", code, errout)
		}
		if e := json.Unmarshal([]byte(out), &rep); e != nil {
			t.Fatalf("bad JSON: %v\n%s", e, out)
		}
		if rep.Totals.Requests != 0 {
			t.Errorf("cache unknown --provider-id requests = %d, want 0", rep.Totals.Requests)
		}
	})
}

// TestStatsByEndpointGrouping adds an explicit identity grouping without
// touching the legacy --by provider view.
func TestStatsByEndpointGrouping(t *testing.T) {
	seedIdentities(t, identityFixture()...)
	mainID := idEndpointID(t, idMainEndpoint)

	t.Run("legacy by provider is unchanged", func(t *testing.T) {
		out, errout, code := runCLI(t, append([]string{"stats", "--by", "provider", "--json"}, idRangeArgs()...)...)
		if code != 0 {
			t.Fatalf("code=%d stderr=%s", code, errout)
		}
		var rep struct {
			By   string `json:"by"`
			Rows []struct {
				Key   string `json:"key"`
				Label string `json:"label"`
			} `json:"rows"`
		}
		if e := json.Unmarshal([]byte(out), &rep); e != nil {
			t.Fatalf("bad JSON: %v\n%s", e, out)
		}
		if rep.By != "provider" {
			t.Errorf("by = %q, want provider", rep.By)
		}
		keys := map[string]bool{}
		for _, r := range rep.Rows {
			keys[r.Key] = true
			if strings.HasPrefix(r.Key, "endpoint:v1:") {
				t.Errorf("legacy --by provider emitted an endpoint id: %v", r)
			}
		}
		for _, want := range []string{"name-a", "name-b", "name-c", "name-d"} {
			if !keys[want] {
				t.Errorf("legacy --by provider lost %q: %+v", want, rep.Rows)
			}
		}
		out, errout, code = runCLI(t, append([]string{"stats", "--by", "provider", "--format", "csv"}, idRangeArgs()...)...)
		if code != 0 {
			t.Fatalf("csv: code=%d stderr=%s", code, errout)
		}
		if recs := parseCSV(t, out); len(recs[0]) != len(bucketCSVHeader) {
			t.Errorf("legacy csv header = %v, want %v", recs[0], bucketCSVHeader)
		}
	})

	t.Run("by endpoint groups by identity", func(t *testing.T) {
		out, errout, code := runCLI(t, append([]string{"stats", "--by", "endpoint", "--json"}, idRangeArgs()...)...)
		if code != 0 {
			t.Fatalf("code=%d stderr=%s", code, errout)
		}
		var rep struct {
			By   string        `json:"by"`
			Rows []idGroupJSON `json:"rows"`
		}
		if e := json.Unmarshal([]byte(out), &rep); e != nil {
			t.Fatalf("bad JSON: %v\n%s", e, out)
		}
		if rep.By != "endpoint" {
			t.Errorf("by = %q, want endpoint", rep.By)
		}
		main := idGroup(t, rep.Rows, mainID)
		if main.Requests != 2 || len(main.Members) != 2 {
			t.Errorf("main group = %+v, want 2 requests / 2 members", main)
		}
		out, errout, code = runCLI(t, append([]string{"stats", "--by", "endpoint", "--format", "csv"}, idRangeArgs()...)...)
		if code != 0 {
			t.Fatalf("csv: code=%d stderr=%s", code, errout)
		}
		recs := parseCSV(t, out)
		if len(recs[0]) != len(providerCSVHeader) {
			t.Errorf("endpoint csv header = %v, want %v", recs[0], providerCSVHeader)
		}
	})

	t.Run("unknown by value", func(t *testing.T) {
		_, errout, code := runCLI(t, append([]string{"stats", "--by", "frobnicate"}, idRangeArgs()...)...)
		if code != 2 {
			t.Fatalf("code=%d, want 2 (stderr=%s)", code, errout)
		}
	})
}

// TestProviderAliasSetClear proves the alias is display-only: it changes the
// label, never the key, the pricing fingerprint or any historical row.
func TestProviderAliasSetClear(t *testing.T) {
	seedIdentities(t, identityFixture()...)
	mainID := idEndpointID(t, idMainEndpoint)
	beforeFP, beforeRows := identityHistory(t)

	out, errout, code := runCLI(t, "provider", "alias", "set", mainID, "Friendly Relay")
	if code != 0 {
		t.Fatalf("alias set: code=%d stderr=%s", code, errout)
	}
	if !strings.Contains(out, mainID) || !strings.Contains(out, "Friendly Relay") {
		t.Errorf("alias set output = %q", out)
	}

	list := func(t *testing.T) idListJSON {
		t.Helper()
		out, errout, code := runCLI(t, append([]string{"provider", "list", "--json"}, idRangeArgs()...)...)
		if code != 0 {
			t.Fatalf("list: code=%d stderr=%s", code, errout)
		}
		return decodeIDList(t, out)
	}
	g := idGroup(t, list(t).Groups, mainID)
	if g.Label != "Friendly Relay" {
		t.Errorf("label after set = %q, want Friendly Relay", g.Label)
	}
	if g.Requests != 2 || len(g.Members) != 2 {
		t.Errorf("alias changed aggregation: %+v", g)
	}
	if afterFP, afterRows := identityHistory(t); afterFP != beforeFP || !equalStrings(afterRows, beforeRows) {
		t.Errorf("alias set mutated history: fingerprint %q->%q rows %v->%v", beforeFP, afterFP, beforeRows, afterRows)
	}

	// A fresh selection by stable ID still selects exactly the same set.
	out, errout, code = runCLI(t, append([]string{"stats", "--by", "model", "--json", "--provider-id", mainID}, idRangeArgs()...)...)
	if code != 0 {
		t.Fatalf("stats: code=%d stderr=%s", code, errout)
	}
	if got := decodeStats(t, out).Totals.Requests; got != 2 {
		t.Errorf("alias changed selection: %d requests, want 2", got)
	}

	// Clearing restores the normalized endpoint label.
	if out, errout, code := runCLI(t, "provider", "alias", "clear", mainID); code != 0 {
		t.Fatalf("alias clear: code=%d stderr=%s", code, errout)
	} else if !strings.Contains(out, mainID) {
		t.Errorf("alias clear output = %q", out)
	}
	if g := idGroup(t, list(t).Groups, mainID); g.Label != idMainEndpoint {
		t.Errorf("label after clear = %q, want %q", g.Label, idMainEndpoint)
	}
}

// TestProviderAliasErrors covers the alias error surface: unknown ids, control
// characters, empty labels and malformed invocations.
func TestProviderAliasErrors(t *testing.T) {
	seedIdentities(t, identityFixture()...)
	mainID := idEndpointID(t, idMainEndpoint)

	// An unresolved identity has no endpoint row and therefore cannot be aliased.
	data := decodeIDList(t, func() string {
		out, errout, code := runCLI(t, append([]string{"provider", "list", "--json"}, idRangeArgs()...)...)
		if code != 0 {
			t.Fatalf("list: code=%d stderr=%s", code, errout)
		}
		return out
	}())
	var unresolvedID string
	for _, g := range data.Groups {
		if !g.Identified {
			unresolvedID = g.Key
			break
		}
	}
	if unresolvedID == "" {
		t.Fatal("fixture has no unresolved identity")
	}
	if _, errout, code := runCLI(t, "provider", "alias", "set", unresolvedID, "nope"); code != 1 {
		t.Errorf("aliasing an unresolved id: code=%d stderr=%s, want 1", code, errout)
	}

	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"unknown id", []string{"provider", "alias", "set", "endpoint:v1:deadbeef", "x"}, "unknown provider identity"},
		{"control char", []string{"provider", "alias", "set", mainID, "bad\nlabel"}, "control character"},
		{"clear unknown", []string{"provider", "alias", "clear", "endpoint:v1:deadbeef"}, "unknown provider identity"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, errout, code := runCLI(t, c.args...)
			if code != 1 {
				t.Fatalf("code=%d, want 1 (stderr=%s)", code, errout)
			}
			if !strings.Contains(errout, c.wantErr) {
				t.Errorf("stderr = %q, want %q", errout, c.wantErr)
			}
		})
	}

	// An empty label would silently clear the alias, so it is an argument error
	// (2), not an operational one: the store is never touched.
	for _, label := range []string{"", "   "} {
		t.Run("empty label "+strconv.Quote(label), func(t *testing.T) {
			_, errout, code := runCLI(t, "provider", "alias", "set", mainID, label)
			if code != 2 {
				t.Fatalf("code=%d, want 2 (stderr=%s)", code, errout)
			}
			if !strings.Contains(errout, "empty label") {
				t.Errorf("stderr = %q, want an empty-label error", errout)
			}
		})
	}
	// The rejected empty label must not have cleared anything.
	out, _, code := runCLI(t, append([]string{"provider", "list", "--json"}, idRangeArgs()...)...)
	if code != 0 {
		t.Fatalf("list: code=%d", code)
	}
	if g := idGroup(t, decodeIDList(t, out).Groups, mainID); g.Label != idMainEndpoint {
		t.Errorf("rejected empty label changed the label to %q", g.Label)
	}
}

// TestProviderDuplicateAliasLabels ensures the display label never becomes a
// selection key: two identities may share a label, and --provider-id still
// selects by identity.
func TestProviderDuplicateAliasLabels(t *testing.T) {
	seedIdentities(t, identityFixture()...)
	mainID := idEndpointID(t, idMainEndpoint)
	altID := idEndpointID(t, idAltEndpoint)

	for _, id := range []string{mainID, altID} {
		if _, errout, code := runCLI(t, "provider", "alias", "set", id, "Shared Label"); code != 0 {
			t.Fatalf("alias set %s: code=%d stderr=%s", id, code, errout)
		}
	}
	out, errout, code := runCLI(t, append([]string{"provider", "list", "--json"}, idRangeArgs()...)...)
	if code != 0 {
		t.Fatalf("list: code=%d stderr=%s", code, errout)
	}
	data := decodeIDList(t, out)
	if g := idGroup(t, data.Groups, mainID); g.Label != "Shared Label" {
		t.Errorf("main label = %q", g.Label)
	}
	if g := idGroup(t, data.Groups, altID); g.Label != "Shared Label" {
		t.Errorf("alt label = %q", g.Label)
	}

	requests := func(id string) int64 {
		out, errout, code := runCLI(t, append([]string{"stats", "--by", "model", "--json", "--provider-id", id}, idRangeArgs()...)...)
		if code != 0 {
			t.Fatalf("stats %s: code=%d stderr=%s", id, code, errout)
		}
		return decodeStats(t, out).Totals.Requests
	}
	if got := requests(mainID); got != 2 {
		t.Errorf("main selection after duplicate labels = %d, want 2", got)
	}
	if got := requests(altID); got != 1 {
		t.Errorf("alt selection after duplicate labels = %d, want 1", got)
	}
}

// TestProviderHelpAndFlagErrors pins discoverability and the invalid-flag
// contract.
func TestProviderHelpAndFlagErrors(t *testing.T) {
	out, _, code := runCLI(t, "help")
	if code != 0 {
		t.Fatalf("help code=%d", code)
	}
	for _, want := range []string{"provider list", "provider alias", "--provider-id", "--by endpoint"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q", want)
		}
	}

	if _, errout, code := runCLI(t, "provider"); code != 2 || !strings.Contains(errout+out, "provider list") {
		t.Errorf("bare provider: code=%d stderr=%s", code, errout)
	}
	if _, errout, code := runCLI(t, "provider", "frobnicate"); code != 2 || !strings.Contains(errout, "provider list") {
		t.Errorf("bad subcommand: code=%d stderr=%s", code, errout)
	}
	if _, _, code := runCLI(t, "provider", "list", "extra"); code != 2 {
		t.Errorf("provider list positional: code=%d, want 2", code)
	}
	if _, _, code := runCLI(t, "provider", "alias"); code != 2 {
		t.Errorf("bare alias: code=%d, want 2", code)
	}
	if _, _, code := runCLI(t, "provider", "alias", "frobnicate", "x"); code != 2 {
		t.Errorf("bad alias subcommand: code=%d, want 2", code)
	}
	if _, _, code := runCLI(t, "provider", "alias", "set", "id-only"); code != 2 {
		t.Errorf("alias set missing label: code=%d, want 2", code)
	}
	if _, _, code := runCLI(t, "provider", "alias", "set", "id", "label", "extra"); code != 2 {
		t.Errorf("alias set extra arg: code=%d, want 2", code)
	}
	if _, _, code := runCLI(t, "provider", "alias", "clear"); code != 2 {
		t.Errorf("alias clear missing id: code=%d, want 2", code)
	}
	if _, _, code := runCLI(t, append([]string{"provider", "list", "--format", "xml"}, idRangeArgs()...)...); code != 2 {
		t.Errorf("provider list bad format: code=%d, want 2", code)
	}

	// --provider-id requires a non-empty value.
	if _, _, code := runCLI(t, append([]string{"stats", "--by", "model", "--json", "--provider-id", ""}, idRangeArgs()...)...); code != 2 {
		t.Errorf("empty --provider-id: code=%d, want 2", code)
	}
	// A session timeline has no filter to attach to.
	seedIdentities(t, identityFixture()...)
	if _, errout, code := runCLI(t, "cache", "--provider-id", idEndpointID(t, idMainEndpoint), "s-main"); code != 2 {
		t.Errorf("cache timeline + --provider-id: code=%d stderr=%s, want 2", code, errout)
	}
	if _, errout, code := runCLI(t, "cache", "--provider", "name-a", "s-main"); code != 2 {
		t.Errorf("cache timeline + --provider: code=%d stderr=%s, want 2", code, errout)
	}
}

// TestProviderListIsOffline proves the new read-only command never performs a
// price refresh: a fresh home has no network cache written after listing.
func TestProviderListIsOffline(t *testing.T) {
	seedIdentities(t, identityFixture()...)
	dir, e := paths.DataDir()
	if e != nil {
		t.Fatal(e)
	}
	prices := filepath.Join(dir, "prices.json")
	if e := os.Remove(prices); e != nil && !os.IsNotExist(e) {
		t.Fatal(e)
	}
	if out, _, code := runCLI(t, append([]string{"provider", "list", "--json"}, idRangeArgs()...)...); code != 0 {
		t.Fatalf("list failed: %s", out)
	}
	if _, e := os.Stat(prices); e == nil {
		t.Errorf("provider list wrote %s: it must not refresh prices", prices)
	}
}

// TestProviderIDDoesNotChangeHistoryFingerprint guards the read-only contract of
// identity filters: filtering by stable ID never rewrites history or prices.
func TestProviderIDDoesNotChangeHistoryFingerprint(t *testing.T) {
	seedIdentities(t, identityFixture()...)
	mainID := idEndpointID(t, idMainEndpoint)
	beforeFP, beforeRows := identityHistory(t)

	if _, errout, code := runCLI(t, append([]string{"stats", "--by", "endpoint", "--json", "--provider-id", mainID}, idRangeArgs()...)...); code != 0 {
		t.Fatalf("stats: code=%d stderr=%s", code, errout)
	}
	if _, errout, code := runCLI(t, append([]string{"provider", "list", "--json"}, idRangeArgs()...)...); code != 0 {
		t.Fatalf("list: code=%d stderr=%s", code, errout)
	}
	afterFP, afterRows := identityHistory(t)
	if afterFP != beforeFP {
		t.Errorf("pricing fingerprint changed: %q -> %q", beforeFP, afterFP)
	}
	if !equalStrings(beforeRows, afterRows) {
		t.Errorf("history rows changed:\n%v\n%v", beforeRows, afterRows)
	}
}

// identityHistory snapshots the pricing fingerprint and every event's resolved
// provider/cost straight from the store, bypassing the CLI.
func identityHistory(t *testing.T) (string, []string) {
	t.Helper()
	st, e := store.Open(dbPath(t))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	fp, e := st.Setting(t.Context(), "pricing_fingerprint")
	if e != nil {
		t.Fatal(e)
	}
	rows, e := st.DB().QueryContext(t.Context(), "SELECT dedup_key,resolved_provider,cost,base_url FROM events ORDER BY dedup_key")
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var dedup, prov, base string
		var cost float64
		if e := rows.Scan(&dedup, &prov, &cost, &base); e != nil {
			t.Fatal(e)
		}
		out = append(out, fmt.Sprintf("%s|%s|%.6f|%s", dedup, prov, cost, base))
	}
	if e := rows.Err(); e != nil {
		t.Fatal(e)
	}
	return fp, out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// dbPath opens the active MYTOKEN_HOME's database without going through the app
// (so the assertion reads exactly what the CLI wrote).
func dbPath(t *testing.T) string {
	t.Helper()
	p, e := paths.DBPath()
	if e != nil {
		t.Fatal(e)
	}
	return p
}
