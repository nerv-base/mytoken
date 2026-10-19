package cursor

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/zzstar101/mytoken/internal/quota/plan"
	"github.com/zzstar101/mytoken/internal/sqlitedsn"
)

const (
	// tokenKey is the ItemTable row holding the access token.
	tokenKey = "cursorAuth/accessToken"
	// stateDBFile is Cursor's global state database.
	stateDBFile = "state.vscdb"
	// tokenQueryTimeout bounds reading the copied database.
	tokenQueryTimeout = 5 * time.Second
)

// goosDefault is the platform this binary was built for.
const goosDefault = runtime.GOOS

// stateDBPaths returns Cursor's global state database locations for the
// platform, in priority order.
func stateDBPaths(getenv func(string) string, home, goos string) []string {
	var dirs []string
	switch goos {
	case "darwin":
		if home != "" {
			dirs = append(dirs, filepath.Join(home, "Library", "Application Support", "Cursor"))
		}
	case "windows":
		if appData := getenv("APPDATA"); appData != "" {
			dirs = append(dirs, filepath.Join(appData, "Cursor"))
		}
	default:
		if xdg := getenv("XDG_CONFIG_HOME"); xdg != "" {
			dirs = append(dirs, filepath.Join(xdg, "Cursor"))
		}
		if home != "" {
			dirs = append(dirs, filepath.Join(home, ".config", "Cursor"))
		}
	}
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		out = append(out, filepath.Join(dir, "User", "globalStorage", stateDBFile))
	}
	return out
}

// readAccessToken returns the first readable token across the candidate
// databases.
func readAccessToken(ctx context.Context, paths []string) (string, error) {
	var firstErr error
	for _, path := range paths {
		ctx, cancel := context.WithTimeout(ctx, tokenQueryTimeout)
		token, err := tokenFromDatabase(ctx, path)
		cancel()
		if err == nil && token != "" {
			return token, nil
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return "", firstErr
	}
	return "", plan.NewError(plan.ReasonNoCredentials, nil)
}

// tokenFromDatabase copies the state database (and its write-ahead log) to a
// temporary directory and reads the token there. Cursor may hold the database
// open, so the original is never opened for writing and never locked.
func tokenFromDatabase(ctx context.Context, path string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", plan.NewError(plan.ReasonSourceMissing, err)
		}
		return "", plan.NewError(plan.ReasonSourceUnreadable, err)
	}
	dir, err := os.MkdirTemp("", "mytoken-cursor-")
	if err != nil {
		return "", plan.NewError(plan.ReasonSourceUnreadable, err)
	}
	defer os.RemoveAll(dir)
	dst := filepath.Join(dir, stateDBFile)
	if err := copyFile(path, dst); err != nil {
		return "", plan.NewError(plan.ReasonSourceUnreadable, err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err == nil {
			_ = copyFile(path+suffix, dst+suffix)
		}
	}

	db, err := sql.Open("sqlite", sqlitedsn.URI(dst, "mode=ro"))
	if err != nil {
		return "", plan.NewError(plan.ReasonSourceUnreadable, err)
	}
	defer db.Close()
	var value []byte
	err = db.QueryRowContext(ctx, `SELECT value FROM ItemTable WHERE key = ?`, tokenKey).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", plan.NewError(plan.ReasonSourceMissing, nil)
	}
	if err != nil {
		return "", plan.NewError(plan.ReasonSourceUnreadable, err)
	}
	return decodeTokenValue(value), nil
}

// decodeTokenValue accepts the token stored as raw text or as a JSON string.
func decodeTokenValue(raw []byte) string {
	token := strings.TrimSpace(string(raw))
	if len(token) >= 2 && token[0] == '"' && token[len(token)-1] == '"' {
		token = strings.TrimSpace(token[1 : len(token)-1])
	}
	return token
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
