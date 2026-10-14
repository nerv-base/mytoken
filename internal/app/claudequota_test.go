package app

// Tests for the app-level Claude quota wiring. They run inside the throwaway
// HOME/MYTOKEN_HOME sandbox, use OpenLocal (offline) and feed a synthetic
// credentials file, so no real home directory, keychain or network is touched.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzstar101/mytoken/internal/claudequota"
	"github.com/zzstar101/mytoken/internal/instance"
	"github.com/zzstar101/mytoken/internal/model"
)

const sentinelToken = "sk-ant-oat01-SENTINEL-MUST-NEVER-BE-READ"

func TestClaudeQuotaSourcesMapping(t *testing.T) {
	cfgDir := t.TempDir()
	projects := filepath.Join(cfgDir, "projects")
	srcs := []Source{
		{Resolved: instance.Resolved{ID: "cc", Label: "Claude Code", Harness: model.ClaudeCode,
			Roots: []string{projects}, Source: instance.PathUser, Kind: instance.KindInstance}},
		{Resolved: instance.Resolved{ID: "cx", Label: "Codex", Harness: model.Codex,
			Roots: []string{t.TempDir()}, Source: instance.PathUser, Kind: instance.KindInstance}},
		{Resolved: instance.Resolved{ID: "off", Label: "Off", Harness: model.ClaudeCode,
			Roots: []string{projects}, Source: instance.PathUser, Kind: instance.KindInstance, Disabled: true}},
	}
	got := claudeQuotaSources(srcs)
	if len(got) == 0 {
		t.Fatal("no Claude quota source derived from a claude-code instance")
	}
	if got[0].Kind != claudequota.SourceFile {
		t.Fatalf("the documented credentials file must come first, got %s", got[0].Kind)
	}
	want := filepath.Join(cfgDir, claudequota.CredentialsFileName)
	if got[0].Ref != want || got[0].Instance != "cc" {
		t.Fatalf("derived source = %+v, want file %s for instance cc", got[0], want)
	}
	for _, s := range got {
		if s.Instance != "cc" {
			t.Fatalf("a source leaked from another instance: %+v", s)
		}
		if s.Read == nil {
			t.Fatalf("source is not readable: %+v", s)
		}
		if strings.Contains(s.Ref, sentinelToken) {
			t.Fatal("source ref carries a secret")
		}
	}
}

// TestOpenLocalClaudeQuotaDefaultOff is the app-level default-off and
// offline contract: an OpenLocal app exposes consent state, records an explicit
// enable in the shared store, and still reads no credential and sends no
// request — the credential file is not even opened (it is unreadable here).
func TestOpenLocalClaudeQuotaDefaultOff(t *testing.T) {
	home, data := sandbox(t)
	projects := filepath.Join(home, ".claude", "projects")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	credPath := filepath.Join(home, ".claude", claudequota.CredentialsFileName)
	body := `{"claudeAiOauth":{"accessToken":"` + sentinelToken + `","expiresAt":4102444800000}}`
	if err := os.WriteFile(credPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Make any read fail: only a path that never opens the file can succeed.
	if err := os.Chmod(credPath, 0o000); err != nil {
		t.Fatal(err)
	}
	saveConfig(t, data, &instance.Config{Entries: []instance.Entry{{
		ID: "cc", Label: "Claude Code", Harness: model.ClaudeCode,
		Roots: []string{projects}, Kind: instance.KindInstance,
	}}})
	a := openScanned(t)
	ctx := context.Background()

	svc := a.ClaudeQuota()
	if svc == nil {
		t.Fatal("the app must expose the quota service for consent state")
	}
	if !svc.Offline() {
		t.Fatal("OpenLocal must be offline")
	}
	info, err := a.ClaudeQuotaInfo(ctx, "cc")
	if err != nil {
		t.Fatalf("ClaudeQuotaInfo: %v", err)
	}
	if info.Enabled {
		t.Fatalf("default off reported enabled: %+v", info)
	}
	// roots may be symlink-resolved (t.TempDir lives under /var -> /private/var),
	// so compare the stable, non-secret suffix instead of the whole path.
	if info.Target != claudequota.Target ||
		!strings.HasSuffix(info.SourceRef, filepath.Join(".claude", claudequota.CredentialsFileName)) {
		t.Fatalf("confirmation text must name the source and target without a secret read: %+v", info)
	}

	enabled, err := a.EnableClaudeQuota(ctx, "cc", "")
	if err != nil {
		t.Fatalf("EnableClaudeQuota: %v", err)
	}
	if !enabled.Enabled || !strings.HasSuffix(enabled.SourceRef, filepath.Join(".claude", claudequota.CredentialsFileName)) {
		t.Fatalf("enable did not record the source: %+v", enabled)
	}
	// Offline refuses before any credential read, so the unreadable file is
	// never even consulted.
	if _, err := a.RefreshClaudeQuota(ctx, "cc"); !errors.Is(err, claudequota.ErrOffline) {
		t.Fatalf("offline refresh = %v, want offline", err)
	}
	q, err := a.ClaudeQuotaSnapshot(ctx, "cc")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if q.Status != claudequota.StatusUnavailable || len(q.Windows) != 0 {
		t.Fatalf("offline app published a reading: %+v", q)
	}

	consent, err := a.Store.Setting(ctx, claudequota.ConsentKey("cc"))
	if err != nil {
		t.Fatal(err)
	}
	if consent == "" || strings.Contains(consent, sentinelToken) {
		t.Fatalf("consent record wrong or secret-bearing: %q", consent)
	}
	latest, err := a.Store.Setting(ctx, claudequota.LatestKey("cc"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(latest, sentinelToken) {
		t.Fatalf("cached quota carries a secret: %q", latest)
	}
	raw, err := os.ReadFile(credPath)
	if err == nil && string(raw) != body {
		t.Fatal("the credential file was rewritten")
	}

	if err := a.DisableClaudeQuota(ctx, "cc"); err != nil {
		t.Fatalf("DisableClaudeQuota: %v", err)
	}
	after, err := a.ClaudeQuotaInfo(ctx, "cc")
	if err != nil {
		t.Fatal(err)
	}
	if after.Enabled {
		t.Fatalf("disable did not revoke: %+v", after)
	}
	if revoked, err := a.Store.Setting(ctx, claudequota.ConsentKey("cc")); err != nil || revoked != "" {
		t.Fatalf("consent not cleared: %q (%v)", revoked, err)
	}
}

// TestClaudeQuotaUnknownInstance covers the additive method's refusal for an
// instance with no configured source.
func TestClaudeQuotaUnknownInstance(t *testing.T) {
	_, data := sandbox(t)
	saveConfig(t, data, &instance.Config{Entries: []instance.Entry{{
		ID: "cc", Label: "Claude Code", Harness: model.ClaudeCode,
		Roots: []string{t.TempDir()}, Kind: instance.KindInstance,
	}}})
	a := openScanned(t)
	ctx := context.Background()

	if _, err := a.EnableClaudeQuota(ctx, "nope", ""); !errors.Is(err, claudequota.ErrNoSource) {
		t.Fatalf("enable for an unknown instance = %v, want no_source", err)
	}
	info, err := a.ClaudeQuotaInfo(ctx, "nope")
	if err != nil {
		t.Fatalf("ClaudeQuotaInfo: %v", err)
	}
	if info.Enabled {
		t.Fatalf("unknown instance reported enabled: %+v", info)
	}
	if refs := a.ClaudeQuotaSources("nope"); len(refs) != 0 {
		t.Fatalf("unknown instance has sources: %+v", refs)
	}
	// The offline app refuses before it looks at any consent, which is exactly
	// why an OpenLocal app can never reach the network.
	if _, err := a.RefreshClaudeQuota(ctx, "nope"); !errors.Is(err, claudequota.ErrOffline) {
		t.Fatalf("refresh for an unknown instance = %v, want offline", err)
	}
}

// TestClaudeQuotaSourcesUnknownRootIsUnavailable pins that a Claude Code
// instance whose roots do not identify a single Claude config directory has no
// credential source at all. It must never silently borrow the machine's default
// ~/.claude credential, which belongs to a possibly different account.
func TestClaudeQuotaSourcesUnknownRootIsUnavailable(t *testing.T) {
	home, _ := sandbox(t)
	defCred := filepath.Join(home, ".claude", claudequota.CredentialsFileName)
	custom := t.TempDir() // no "projects" component: the config dir is unknown
	srcs := []Source{{Resolved: instance.Resolved{
		ID: "custom", Label: "Custom", Harness: model.ClaudeCode,
		Roots: []string{custom}, Source: instance.PathUser, Kind: instance.KindInstance,
	}}}
	for _, s := range claudeQuotaSources(srcs) {
		if s.Ref == defCred {
			t.Fatalf("unknown custom root borrowed the default credential origin: %+v", s)
		}
		if s.Instance == "custom" {
			t.Fatalf("unknown custom root produced a source: %+v", s)
		}
	}
	// A relative root names no absolute credential origin either.
	rel := []Source{{Resolved: instance.Resolved{
		ID: "rel", Label: "Rel", Harness: model.ClaudeCode,
		Roots:  []string{filepath.Join("relcfg", "projects")},
		Source: instance.PathUser, Kind: instance.KindInstance,
	}}}
	if got := claudeQuotaSources(rel); len(got) != 0 {
		t.Fatalf("a relative root must not bind an origin: %+v", got)
	}
}

// TestClaudeQuotaSourcesAmbiguousRootsAreUnavailable pins that two distinct
// projects roots never resolve to one arbitrarily chosen account.
func TestClaudeQuotaSourcesAmbiguousRootsAreUnavailable(t *testing.T) {
	a := filepath.Join(t.TempDir(), "cfg-a", "projects")
	b := filepath.Join(t.TempDir(), "cfg-b", "projects")
	srcs := []Source{{Resolved: instance.Resolved{
		ID: "two", Label: "Two", Harness: model.ClaudeCode,
		Roots: []string{a, b}, Source: instance.PathUser, Kind: instance.KindInstance,
	}}}
	if got := claudeQuotaSources(srcs); len(got) != 0 {
		t.Fatalf("two distinct projects roots must not pick one account, got %+v", got)
	}
}

// TestClaudeQuotaSourcesExcludeBackupCopyAndDisabled pins that neither a
// backup-copy nor a disabled Claude profile offers a quota source.
func TestClaudeQuotaSourcesExcludeBackupCopyAndDisabled(t *testing.T) {
	projects := filepath.Join(t.TempDir(), "cfg", "projects")
	srcs := []Source{
		{Resolved: instance.Resolved{ID: "copy", Label: "Copy", Harness: model.ClaudeCode,
			Roots: []string{projects}, Source: instance.PathUser, Kind: instance.KindBackupCopy}},
		{Resolved: instance.Resolved{ID: "off", Label: "Off", Harness: model.ClaudeCode,
			Roots: []string{projects}, Source: instance.PathUser, Kind: instance.KindInstance, Disabled: true}},
	}
	if got := claudeQuotaSources(srcs); len(got) != 0 {
		t.Fatalf("backup-copy/disabled profiles must have no quota source, got %+v", got)
	}
}

// TestClaudeQuotaSourcesKeychainOnlyForActualDefaultProfile pins the binding of
// the account-global macOS keychain entry: only the actual default profile may
// offer the default service, and the default profile always names the
// documented default credential file.
func TestClaudeQuotaSourcesKeychainOnlyForActualDefaultProfile(t *testing.T) {
	home, _ := sandbox(t)
	cfgDir := t.TempDir()
	custom := Source{Resolved: instance.Resolved{
		ID: "work", Label: "Work", Harness: model.ClaudeCode,
		Roots:  []string{filepath.Join(cfgDir, "projects")},
		Source: instance.PathUser, Kind: instance.KindInstance,
	}}
	got := claudeQuotaSources([]Source{custom})
	wantFile := filepath.Join(cfgDir, claudequota.CredentialsFileName)
	if len(got) != 1 || got[0].Kind != claudequota.SourceFile || got[0].Ref != wantFile {
		t.Fatalf("custom profile sources = %+v, want exactly the derived file %s", got, wantFile)
	}
	if got[0].Profile != cfgDir {
		t.Fatalf("custom profile origin = %q, want the exact config dir %q", got[0].Profile, cfgDir)
	}

	def := Source{Resolved: instance.Resolved{
		ID: instance.DefaultID, Label: "Default", Harness: model.ClaudeCode,
		Roots:  []string{filepath.Join(home, ".claude", "projects")},
		Source: instance.PathDefault, Kind: instance.KindInstance,
	}}
	got = claudeQuotaSources([]Source{def})
	wantRef := filepath.Join(home, ".claude", claudequota.CredentialsFileName)
	if len(got) == 0 || got[0].Kind != claudequota.SourceFile || got[0].Ref != wantRef {
		t.Fatalf("default profile sources = %+v, want the documented file %s first", got, wantRef)
	}
	if claudequota.DefaultKeychainRunner() != nil {
		if len(got) != 2 || got[1].Kind != claudequota.SourceKeychain {
			t.Fatalf("the actual default profile must offer the default keychain entry: %+v", got)
		}
	}
}

// TestOpenLocalUnknownCredentialRootStaysUnavailable is the app-level contract:
// with a real default credential present on the machine, an instance whose
// custom root identifies no Claude config directory exposes no source, refuses
// an enable and reports unavailable instead of binding the wrong account.
func TestOpenLocalUnknownCredentialRootStaysUnavailable(t *testing.T) {
	home, data := sandbox(t)
	defDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(defDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"claudeAiOauth":{"accessToken":"` + sentinelToken + `","expiresAt":4102444800000}}`
	if err := os.WriteFile(filepath.Join(defDir, claudequota.CredentialsFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(t.TempDir(), "claude-cfg", "logs")
	if err := os.MkdirAll(custom, 0o755); err != nil {
		t.Fatal(err)
	}
	saveConfig(t, data, &instance.Config{Entries: []instance.Entry{{
		ID: "custom", Label: "Custom", Harness: model.ClaudeCode,
		Roots: []string{custom}, Kind: instance.KindInstance,
	}}})
	a := openScanned(t)
	ctx := context.Background()

	if refs := a.ClaudeQuotaSources("custom"); len(refs) != 0 {
		t.Fatalf("unknown credential root exposed a source: %+v", refs)
	}
	if _, err := a.EnableClaudeQuota(ctx, "custom", ""); !errors.Is(err, claudequota.ErrNoSource) {
		t.Fatalf("enable without a derivable origin = %v, want no_source", err)
	}
	info, err := a.ClaudeQuotaInfo(ctx, "custom")
	if err != nil {
		t.Fatalf("ClaudeQuotaInfo: %v", err)
	}
	if info.Enabled || info.SourceRef != "" || info.SourceAvailable {
		t.Fatalf("unknown credential root must be unavailable: %+v", info)
	}
}

// TestClaudeQuotaFileSourceReportsAMissingCredentialsFile pins the hint a card
// renders before an enable: a file source whose credentials file is absent says
// so, and an existing one does not. Deciding it reads no credential.
func TestClaudeQuotaFileSourceReportsAMissingCredentialsFile(t *testing.T) {
	cfgDir := t.TempDir()
	projects := filepath.Join(cfgDir, "projects")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	srcs := []Source{{Resolved: instance.Resolved{ID: "cc", Label: "Claude Code",
		Harness: model.ClaudeCode, Roots: []string{projects}, Source: instance.PathUser,
		Kind: instance.KindInstance}}}
	got := claudeQuotaSources(srcs)
	if len(got) != 1 || got[0].Kind != claudequota.SourceFile {
		t.Fatalf("want exactly the credentials file source, got %+v", got)
	}
	if !got[0].FileMissing {
		t.Fatalf("a missing credentials file was not reported: %+v", got[0])
	}
	path := filepath.Join(cfgDir, claudequota.CredentialsFileName)
	if err := os.WriteFile(path, []byte(`{"claudeAiOauth":{"accessToken":"sentinel"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got = claudeQuotaSources(srcs)
	if len(got) != 1 || got[0].FileMissing {
		t.Fatalf("an existing credentials file was reported missing: %+v", got)
	}
}
