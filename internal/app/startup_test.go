package app

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// The message for a fatal startup failure is what the user sees instead of a
// silent exit, so it must name the database, repeat the real error and carry
// actionable recovery guidance — in the user's language.
func TestDescribeStartupFailureNamesDatabaseErrorAndRecovery(t *testing.T) {
	dbPath := filepath.Join("home", "user", "MyToken", "mytoken.db")
	cause := errors.New("provider migration stopped before any history was migrated: could not write a consistent pre-migration backup")

	for _, tc := range []struct {
		lang         string
		title        string
		dbLabel      string
		errLabel     string
		recoveryHint string
	}{
		{"zh", "MyToken 无法启动", "数据库位置", "错误详情", "退出所有 MyToken 进程"},
		{"zh-CN", "MyToken 无法启动", "数据库位置", "错误详情", "退出所有 MyToken 进程"},
		{"en", "MyToken could not start", "Database location", "Error details", "Quit every MyToken process"},
		{"en-US", "MyToken could not start", "Database location", "Error details", "Quit every MyToken process"},
	} {
		got := DescribeStartupFailure(tc.lang, dbPath, cause)
		if got.Title != tc.title {
			t.Errorf("%s: title = %q, want %q", tc.lang, got.Title, tc.title)
		}
		if got.DatabaseLabel != tc.dbLabel {
			t.Errorf("%s: database label = %q, want %q", tc.lang, got.DatabaseLabel, tc.dbLabel)
		}
		if got.Database != dbPath {
			t.Errorf("%s: database = %q, want %q", tc.lang, got.Database, dbPath)
		}
		if got.ErrorLabel != tc.errLabel {
			t.Errorf("%s: error label = %q, want %q", tc.lang, got.ErrorLabel, tc.errLabel)
		}
		if !strings.Contains(got.Error, cause.Error()) {
			t.Errorf("%s: error text %q does not carry the cause", tc.lang, got.Error)
		}
		if !strings.Contains(got.Guidance, tc.recoveryHint) {
			t.Errorf("%s: guidance %q is missing the recovery hint %q", tc.lang, got.Guidance, tc.recoveryHint)
		}
		// The guidance must never invite the user to delete their history.
		if !strings.Contains(got.Guidance, "-wal") || !strings.Contains(got.Guidance, "-shm") {
			t.Errorf("%s: guidance does not mention the WAL side files: %q", tc.lang, got.Guidance)
		}
		for name, value := range map[string]string{
			"summary": got.Summary, "guidance": got.Guidance,
			"reveal label": got.RevealLabel, "quit label": got.QuitLabel,
		} {
			if strings.TrimSpace(value) == "" {
				t.Errorf("%s: %s is empty", tc.lang, name)
			}
		}
	}
}

// A nil or blank cause must still produce a usable window rather than an empty
// error field.
func TestDescribeStartupFailureWithoutCause(t *testing.T) {
	for _, cause := range []error{nil, errors.New("   ")} {
		got := DescribeStartupFailure("en", "mytoken.db", cause)
		if strings.TrimSpace(got.Error) == "" {
			t.Errorf("cause %v: error text is empty", cause)
		}
		if got.Database != "mytoken.db" {
			t.Errorf("cause %v: database = %q", cause, got.Database)
		}
	}
}
