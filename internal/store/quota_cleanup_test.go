package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestMigrationDeletesLocalLogQuotaSettings(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for key, value := range map[string]string{
		"quota.codex.default": `{"status":"ok"}`,
		"quota.codex.alpha":   `{"status":"ok"}`,
		"quota.plan.codex":    `{"provider":"codex"}`,
		"unrelated":           "keep",
	} {
		if err = st.SetSetting(ctx, key, value); err != nil {
			st.Close()
			t.Fatalf("SetSetting(%q): %v", key, err)
		}
	}
	if err = st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	for key, want := range map[string]string{
		"quota.codex.default": "",
		"quota.codex.alpha":   "",
		"quota.plan.codex":    `{"provider":"codex"}`,
		"unrelated":           "keep",
	} {
		got, err := reopened.Setting(ctx, key)
		if err != nil {
			t.Fatalf("Setting(%q): %v", key, err)
		}
		if got != want {
			t.Errorf("Setting(%q) = %q, want %q", key, got, want)
		}
	}
}
