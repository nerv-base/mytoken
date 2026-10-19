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
		"quota.codex.default":    `{"status":"ok"}`,
		"quota.codex.alpha":      `{"status":"ok"}`,
		"quota.plan.codex":       `{"provider":"codex"}`,
		"quota.plan.trae-cn":     `{"provider":"trae-cn","source":"local-log","windows":[]}`,
		"quota.plan.workbuddy":   `not json`,
		"quota.plan.api":         `{"provider":"deepseek","source":"api","fetched_at":"2026-10-19T16:44:04Z"}`,
		"quota.plan.api.history": `[{"at":"2026-10-19T16:44:04Z"}]`,
		"quota.plan.deepseek":    `{"provider":"deepseek","source":"api"}`,
		"unrelated":              "keep",
	} {
		if err = st.SetSetting(ctx, key, value); err != nil {
			st.Close()
			t.Fatalf("SetSetting(%q): %v", key, err)
		}
	}
	if err = st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	want := map[string]string{
		"quota.codex.default":    "",
		"quota.codex.alpha":      "",
		"quota.plan.codex":       "",
		"quota.plan.trae-cn":     "",
		"quota.plan.workbuddy":   "",
		"quota.plan.api":         `{"provider":"deepseek","source":"api","fetched_at":"2026-10-19T16:44:04Z"}`,
		"quota.plan.api.history": `[{"at":"2026-10-19T16:44:04Z"}]`,
		"quota.plan.deepseek":    `{"provider":"deepseek","source":"api"}`,
		"unrelated":              "keep",
	}
	// Every read-write open prunes; the second open must change nothing.
	for round := 1; round <= 2; round++ {
		reopened, err := Open(path)
		if err != nil {
			t.Fatalf("reopen %d: %v", round, err)
		}
		for key, wantValue := range want {
			got, err := reopened.Setting(ctx, key)
			if err != nil {
				reopened.Close()
				t.Fatalf("reopen %d Setting(%q): %v", round, key, err)
			}
			if got != wantValue {
				t.Errorf("reopen %d: Setting(%q) = %q, want %q", round, key, got, wantValue)
			}
		}
		if err = reopened.Close(); err != nil {
			t.Fatalf("reopen %d close: %v", round, err)
		}
	}
}
