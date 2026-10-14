package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/store"
)

// sessionTotal decodes the `total` field shared by `sessions --json`.
func sessionTotal(t *testing.T, out string) int64 {
	t.Helper()
	var rep struct {
		Total int64 `json:"total"`
	}
	if e := json.Unmarshal([]byte(out), &rep); e != nil {
		t.Fatalf("bad sessions JSON: %v\n%s", e, out)
	}
	return rep.Total
}

// cacheRequests decodes `cache --json` totals.requests.
func cacheRequests(t *testing.T, out string) int64 {
	t.Helper()
	var rep struct {
		Totals struct {
			Requests int64 `json:"requests"`
		} `json:"totals"`
	}
	if e := json.Unmarshal([]byte(out), &rep); e != nil {
		t.Fatalf("bad cache JSON: %v\n%s", e, out)
	}
	return rep.Totals.Requests
}

// Regression coverage for the provider identity startup path.
//
// The bug: `provider list` and `provider alias set/clear` called
// app.OpenLocal(), and app.open() unconditionally runs
// Store.EnsurePrices(currentFingerprint, ...). Whenever the on-disk
// settings.pricing_fingerprint is stale (any DB written by an older price
// catalog), EnsurePrices does not merely compare: it calls replacePricing,
// which walks every non-log-priced event, recomputes its cost from the *current*
// catalog and overwrites pricing_fingerprint. So "list my endpoints" or "give
// this endpoint a display name" silently repriced historical facts.
//
// The pre-existing TestProviderIDDoesNotChangeHistoryFingerprint could not see
// this: its fixture is seeded through app.OpenLocal(), so the fingerprint is
// already current and EnsurePrices takes the no-op branch. These tests seed a
// deliberately stale checkpoint instead.

// idStaleFingerprint is a checkpoint that cannot equal any real catalog
// fingerprint, so any EnsurePrices call with a loaded catalog must reprice.
const idStaleFingerprint = "stale-historical-pricing-fingerprint-v0"

// seedStalePricingFixture seeds the shared identity fixture, then rewrites
// pricing_fingerprint to a stale value, reproducing a DB last priced by an older
// catalog. It returns the exact fingerprint and fact snapshot the command under
// test must leave untouched.
func seedStalePricingFixture(t *testing.T) (string, []string) {
	t.Helper()
	seedIdentities(t, identityFixture()...)

	st, e := store.Open(dbPath(t))
	if e != nil {
		t.Fatal(e)
	}
	if e := st.SetSetting(t.Context(), "pricing_fingerprint", idStaleFingerprint); e != nil {
		st.Close()
		t.Fatal(e)
	}
	if e := st.Close(); e != nil {
		t.Fatal(e)
	}

	fp, rows := identityHistory(t)
	// Guard the fixture itself: the regression is only meaningful if the facts
	// carry nonzero costs and the stale checkpoint really is what we wrote.
	if fp != idStaleFingerprint {
		t.Fatalf("fixture fingerprint = %q, want %q", fp, idStaleFingerprint)
	}
	if len(rows) == 0 {
		t.Fatal("fixture has no events")
	}
	nonzero := false
	for _, r := range rows {
		if !strings.Contains(r, "|0.000000|") {
			nonzero = true
		}
	}
	if !nonzero {
		t.Fatalf("fixture has no nonzero costs: %v", rows)
	}
	return fp, rows
}

// TestProviderCommandsDoNotRepriceHistory is the regression for the startup bug.
// Each of the three new commands must leave both the historical pricing
// fingerprint and every historical fact byte-identical, even when the on-disk
// checkpoint is stale.
func TestProviderCommandsDoNotRepriceHistory(t *testing.T) {
	mainID := idEndpointID(t, idMainEndpoint)

	cases := []struct {
		name string
		args []string
	}{
		{"list", append([]string{"provider", "list", "--json"}, idRangeArgs()...)},
		{"alias set", []string{"provider", "alias", "set", mainID, "Friendly Relay"}},
		{"alias clear", []string{"provider", "alias", "clear", mainID}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			beforeFP, beforeRows := seedStalePricingFixture(t)

			out, errout, code := runCLI(t, c.args...)
			if code != 0 {
				t.Fatalf("code=%d stderr=%s", code, errout)
			}
			_ = out

			afterFP, afterRows := identityHistory(t)
			if afterFP != beforeFP {
				t.Errorf("%s replaced the historical pricing_fingerprint: %q -> %q", c.name, beforeFP, afterFP)
			}
			if !equalStrings(beforeRows, afterRows) {
				t.Errorf("%s repriced historical facts:\nbefore: %v\nafter:  %v", c.name, beforeRows, afterRows)
			}
		})
	}
}

// TestProviderCommandsNeedNoCredentialSource proves the new commands never
// require (or surface) a relay/harness credential source: HOME is pointed at a
// tree whose cc-switch path is a directory that cannot be opened as a database
// and whose harness config carries a secret key. All three commands must still
// succeed and must never print the secret.
func TestProviderCommandsNeedNoCredentialSource(t *testing.T) {
	const secret = "sk-credential-should-never-surface"
	home := t.TempDir()
	// A directory where the cc-switch SQLite file is expected: any source that
	// tried to discover credentials here would fail to open it.
	if e := os.MkdirAll(filepath.Join(home, ".cc-switch", "cc-switch.db"), 0o700); e != nil {
		t.Fatal(e)
	}
	if e := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); e != nil {
		t.Fatal(e)
	}
	cfg := "model_provider = \"relay\"\n[model_providers.relay]\nbase_url = \"https://relay.invalid/v1\"\napi_key = \"" + secret + "\"\n"
	if e := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(cfg), 0o600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("HOME", home)

	// seedIdentities overwrites MYTOKEN_HOME but leaves HOME as the poisoned tree.
	seedIdentities(t, identityFixture()...)
	mainID := idEndpointID(t, idMainEndpoint)

	for _, args := range [][]string{
		append([]string{"provider", "list", "--json"}, idRangeArgs()...),
		{"provider", "alias", "set", mainID, "No Credentials Needed"},
		{"provider", "alias", "clear", mainID},
	} {
		out, errout, code := runCLI(t, args...)
		if code != 0 {
			t.Fatalf("%v: code=%d stderr=%s", args, code, errout)
		}
		if strings.Contains(out+errout, secret) {
			t.Fatalf("%v surfaced a credential: %s", args, out+errout)
		}
	}
}

// TestProviderAndProviderIDIntersectionAcrossCommands pins the AND semantics of
// the two provider flags on every command that accepts them. --provider keeps
// the original pricing-name meaning and --provider-id the stable endpoint key;
// combining them narrows to the intersection and never widens to everything.
func TestProviderAndProviderIDIntersectionAcrossCommands(t *testing.T) {
	seedIdentities(t, identityFixture()...)
	mainID := idEndpointID(t, idMainEndpoint)
	tenantID := idEndpointID(t, idTenantEndpoint)

	t.Run("stats", func(t *testing.T) {
		for _, c := range []struct {
			name string
			want int64
		}{
			{"name-a + main", 1},
			{"name-b + main", 1},
			{"name-a + tenant", 0}, // disjoint intersection
			{"name-c + main", 0},   // name-c lives on other endpoints
		} {
			args := []string{"stats", "--by", "model", "--json", "--provider-id", mainID}
			if strings.Contains(c.name, "tenant") {
				args = []string{"stats", "--by", "model", "--json", "--provider-id", tenantID}
			}
			args = append(args, "--provider", strings.Fields(c.name)[0])
			args = append(args, idRangeArgs()...)
			out, errout, code := runCLI(t, args...)
			if code != 0 {
				t.Fatalf("%s: code=%d stderr=%s", c.name, code, errout)
			}
			if got := decodeStats(t, out).Totals.Requests; got != c.want {
				t.Errorf("%s = %d requests, want %d", c.name, got, c.want)
			}
		}
	})

	t.Run("sessions", func(t *testing.T) {
		out, errout, code := runCLI(t, append([]string{"sessions", "--json", "--provider", "name-a", "--provider-id", mainID}, idRangeArgs()...)...)
		if code != 0 {
			t.Fatalf("code=%d stderr=%s", code, errout)
		}
		if got := sessionTotal(t, out); got != 1 {
			t.Errorf("sessions name-a ∩ main = %d, want 1", got)
		}
		out, errout, code = runCLI(t, append([]string{"sessions", "--json", "--provider", "name-b", "--provider-id", tenantID}, idRangeArgs()...)...)
		if code != 0 {
			t.Fatalf("code=%d stderr=%s", code, errout)
		}
		if got := sessionTotal(t, out); got != 0 {
			t.Errorf("sessions name-b ∩ tenant = %d, want 0 (disjoint)", got)
		}
	})

	t.Run("cache", func(t *testing.T) {
		out, errout, code := runCLI(t, append([]string{"cache", "--by", "cause", "--json", "--provider", "name-b", "--provider-id", mainID}, idRangeArgs()...)...)
		if code != 0 {
			t.Fatalf("code=%d stderr=%s", code, errout)
		}
		if got := cacheRequests(t, out); got != 1 {
			t.Errorf("cache name-b ∩ main = %d, want 1", got)
		}
		out, errout, code = runCLI(t, append([]string{"cache", "--by", "cause", "--json", "--provider", "name-a", "--provider-id", tenantID}, idRangeArgs()...)...)
		if code != 0 {
			t.Fatalf("code=%d stderr=%s", code, errout)
		}
		if got := cacheRequests(t, out); got != 0 {
			t.Errorf("cache name-a ∩ tenant = %d, want 0 (disjoint)", got)
		}
	})
}
