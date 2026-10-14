package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/instance"
)

// doctorDoc decodes doctor --json without assuming the instance fields, so the
// legacy keys can be asserted as unchanged.
func runDoctorJSON(t *testing.T) (map[string]json.RawMessage, string, string, int) {
	t.Helper()
	out, errout, code := runCLI(t, "doctor", "--json")
	var doc map[string]json.RawMessage
	if e := json.Unmarshal([]byte(out), &doc); e != nil {
		t.Fatalf("doctor --json: %v\nout=%s\nerr=%s", e, out, errout)
	}
	return doc, out, errout, code
}

func TestDoctorKeepsLegacyJSONFields(t *testing.T) {
	t.Setenv("MYTOKEN_HOME", t.TempDir())
	useTempFallback(t, t.TempDir())

	doc, out, errout, code := runDoctorJSON(t)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	for _, key := range []string{"dataDir", "dbPath", "dbBytes", "pricingSource", "pricingFetchedAt", "pricingAgeSeconds", "harnesses", "providerSources"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("doctor JSON lost legacy key %q: %s", key, out)
		}
	}
	// The harness rows keep their own shape.
	var harnesses []map[string]json.RawMessage
	if err := json.Unmarshal(doc["harnesses"], &harnesses); err != nil {
		t.Fatalf("harnesses: %v", err)
	}
	if len(harnesses) == 0 {
		t.Fatal("no harness rows")
	}
	for _, key := range []string{"harness", "roots", "sources", "events"} {
		if _, ok := harnesses[0][key]; !ok {
			t.Errorf("harness row lost key %q: %s", key, harnesses[0])
		}
	}
}

func TestDoctorReportsInstanceDiagnostics(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYTOKEN_HOME", home)
	root := t.TempDir()
	useTempFallback(t, t.TempDir())

	e := addInstance(t, "--root", root, "--label", "diagnosed")

	doc, out, errout, code := runDoctorJSON(t)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	for _, key := range []string{"configPath", "configPresent", "instances"} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("doctor JSON missing %q: %s", key, out)
		}
	}
	var present bool
	if err := json.Unmarshal(doc["configPresent"], &present); err != nil || !present {
		t.Fatalf("configPresent = %s (%v)", doc["configPresent"], err)
	}
	var path string
	if err := json.Unmarshal(doc["configPath"], &path); err != nil || path != filepath.Join(home, instance.FileName) {
		t.Fatalf("configPath = %s (%v)", doc["configPath"], err)
	}
	var instances []srcEntryJSON
	if err := json.Unmarshal(doc["instances"], &instances); err != nil {
		t.Fatalf("instances: %v", err)
	}
	got, ok := entryByID(srcReportJSON{Instances: instances}, e.ID)
	if !ok {
		t.Fatalf("instance %s missing from doctor: %s", e.ID, out)
	}
	if got.Origin != string(instance.PathUser) || got.Status != "enabled" || !got.Scannable {
		t.Fatalf("instance diagnostics = %+v", got)
	}
	if got.Label != "diagnosed" || len(got.Roots) != 1 || !got.Roots[0].Exists {
		t.Fatalf("instance roots/label = %+v", got)
	}
	// Counts are attached per instance without breaking the legacy harness rows.
	var withCounts []map[string]json.RawMessage
	if err := json.Unmarshal(doc["instances"], &withCounts); err != nil {
		t.Fatal(err)
	}
	for _, entry := range withCounts {
		if _, ok := entry["sources"]; !ok {
			t.Errorf("instance row missing sources count: %v", entry)
		}
		if _, ok := entry["events"]; !ok {
			t.Errorf("instance row missing events count: %v", entry)
		}
	}
}

func TestDoctorTextIncludesSourceDiagnostics(t *testing.T) {
	t.Setenv("MYTOKEN_HOME", t.TempDir())
	root := t.TempDir()
	useTempFallback(t, t.TempDir())
	e := addInstance(t, "--root", root)

	out, errout, code := runCLI(t, "doctor")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errout)
	}
	for _, want := range []string{"SOURCES", e.ID, "user", "enabled", "CONFIG"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor text missing %q:\n%s", want, out)
		}
	}
}

// TestDoctorAndSourcesDoNotLeakCredentials: no output may echo a secret-shaped
// environment value, and no log file is read to build the report.
func TestDoctorAndSourcesDoNotLeakCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MYTOKEN_HOME", home)
	root := t.TempDir()
	t.Setenv("OPENAI_API_KEY", "sk-live-SUPERSECRETVALUE")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-SUPERSECRETVALUE")
	t.Setenv("MYTOKEN_RELAY_KEY", "relay-SUPERSECRETVALUE")
	useTempFallback(t, t.TempDir())
	// A log file inside the configured root must never be opened by these
	// commands; its content is a canary.
	canary := filepath.Join(root, "sessions.log")
	if err := os.WriteFile(canary, []byte(`{"token":"CANARY-SUPERSECRETVALUE"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e := addInstance(t, "--root", root)

	for _, args := range [][]string{
		{"sources", "list"},
		{"sources", "list", "--json"},
		{"doctor"},
		{"doctor", "--json"},
	} {
		out, errout, _ := runCLI(t, args...)
		joined := out + errout
		for _, secret := range []string{"SUPERSECRETVALUE", "CANARY"} {
			if strings.Contains(joined, secret) {
				t.Errorf("%v leaked %q: %s", args, secret, joined)
			}
		}
		if !strings.Contains(joined, e.ID) && strings.Contains(strings.Join(args, " "), "sources list") {
			t.Errorf("%v did not name the configured instance", args)
		}
	}
}
