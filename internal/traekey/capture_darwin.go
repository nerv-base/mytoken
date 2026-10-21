//go:build darwin

package traekey

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// traeApps are the bundles a Trae installation can live in, most specific first.
var traeApps = []string{
	"/Applications/Trae CN.app",
	"/Applications/TRAE SOLO CN.app",
	"/Applications/Trae.app",
}

// Supported reports whether this build can read a key out of memory.
func Supported() bool { return true }

// Capture reads the database key out of a running Trae agent.
//
// Trae runs with the hardened runtime, which makes the system refuse a debugger
// attach, so the installed app cannot be inspected as it stands. Capture
// therefore copies it, ad-hoc signs the copy and its helper executables (that
// signature carries no hardened-runtime flag), starts the copy, attaches lldb to
// the process that has the agent library mapped, and stops the copy again. The
// installed app is never touched, and the copy shares the same data directory,
// so the key it hands around is the one the real installation uses.
func Capture(ctx context.Context, opt Options) (string, error) {
	if opt.DB == "" {
		return "", errors.New("no database to prove the key against")
	}
	app := opt.App
	if app == "" {
		for _, candidate := range traeApps {
			if _, err := os.Stat(candidate); err == nil {
				app = candidate
				break
			}
		}
	}
	if app == "" {
		return "", ErrNoApp
	}
	if _, err := os.Stat(app); err != nil {
		return "", fmt.Errorf("%w: %s", ErrNoApp, app)
	}
	// Cheapest checks first: the database has to be an encrypted one, and the
	// tools have to exist, before an app is copied and started.
	if err := checkEncrypted(opt.DB); err != nil {
		return "", err
	}
	for _, tool := range []string{"codesign", "lldb", "vmmap", "pgrep"} {
		if _, err := exec.LookPath(tool); err != nil {
			return "", fmt.Errorf("%s is needed: install the Xcode command line tools", tool)
		}
	}
	if err := noRunning(app); err != nil {
		return "", err
	}

	work, err := os.MkdirTemp("", "mytoken-traekey-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	copyPath := filepath.Join(work, "TraeKeyScan.app")
	// Only the copy is ever stopped, and its name is unique enough that a pkill
	// pattern cannot reach the real installation.
	matchName := filepath.Base(copyPath)

	opt.log(StepCopy)
	if _, err := run(ctx, "cp", "-R", "-c", app, copyPath); err != nil {
		if _, err := run(ctx, "cp", "-R", app, copyPath); err != nil {
			return "", err
		}
	}

	opt.log(StepSign)
	// Only the top level and the helper executables are signed. `codesign
	// --deep` re-signs the embedded frameworks too, and AMFI then refuses to
	// load them, which makes the app exit immediately.
	if _, err := run(ctx, "codesign", "--force", "--sign", "-", copyPath); err != nil {
		return "", err
	}
	helpers, _ := filepath.Glob(filepath.Join(copyPath, "Contents", "Frameworks", "*.app"))
	for _, helper := range helpers {
		exe, err := bundleExecutable(helper)
		if err != nil {
			continue
		}
		if _, err := run(ctx, "codesign", "--force", "--sign", "-", filepath.Join(helper, "Contents", "MacOS", exe)); err != nil {
			return "", err
		}
	}

	exe, err := bundleExecutable(copyPath)
	if err != nil {
		return "", err
	}
	opt.log(StepLaunch)
	// ELECTRON_RUN_AS_NODE turns the launcher into a bare Node REPL that exits
	// at once, and some shells export it.
	env := launchEnv()
	launch := exec.Command(filepath.Join(copyPath, "Contents", "MacOS", exe))
	launch.Env = env
	if err := launch.Start(); err != nil {
		return "", err
	}
	defer func() {
		opt.log(StepStop)
		pkill(matchName)
		_ = launch.Process.Kill()
		_, _ = launch.Process.Wait()
	}()

	pid, err := waitForAgent(ctx, matchName, 120*time.Second)
	if err != nil {
		return "", err
	}
	opt.log(StepScan)

	script := filepath.Join(work, "scan_lldb.py")
	if err := os.WriteFile(script, lldbScript, 0o600); err != nil {
		return "", err
	}
	keyOut := filepath.Join(work, "key.json")
	scanCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	lldb := exec.CommandContext(scanCtx, "lldb", "-p", strconv.Itoa(pid), "-b",
		"-o", "command script import "+script,
		"-o", "trae_scan",
		"-o", "detach")
	lldb.Env = append(env, "TRAE_DB="+opt.DB, "TRAE_KEY_OUT="+keyOut)
	var scanLog bytes.Buffer
	lldb.Stdout, lldb.Stderr = &scanLog, &scanLog
	runErr := lldb.Run()
	out := scanLog.String()
	if strings.Contains(out, "no verified key") {
		return "", ErrNoKey
	}
	if runErr != nil {
		return "", fmt.Errorf("lldb: %v: %s", runErr, tail(out))
	}
	data, err := os.ReadFile(keyOut)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrNoKey, tail(out))
	}
	var result struct {
		EncKey string `json:"enc_key"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", err
	}
	return ValidateKey(result.EncKey)
}

// launchEnv is the current environment without ELECTRON_RUN_AS_NODE.
func launchEnv() []string {
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "ELECTRON_RUN_AS_NODE=") {
			env = append(env, kv)
		}
	}
	return env
}

// bundleExecutable reads CFBundleExecutable out of a bundle's Info.plist.
func bundleExecutable(bundle string) (string, error) {
	plist := filepath.Join(bundle, "Contents", "Info.plist")
	if out, err := exec.Command("defaults", "read", plist, "CFBundleExecutable").Output(); err == nil {
		if name := strings.TrimSpace(string(out)); name != "" {
			return name, nil
		}
	}
	// Electron helper bundles name their executable after the bundle.
	name := strings.TrimSuffix(filepath.Base(bundle), ".app")
	if name == "" {
		return "", fmt.Errorf("cannot find the executable of %s", bundle)
	}
	return name, nil
}

// noRunning refuses to start while the real app is open: a second instance of an
// Electron app quits on the singleton lock, leaving nothing to scan.
func noRunning(app string) error {
	out, err := exec.Command("pgrep", "-f", filepath.Base(app)+"/Contents/MacOS/").Output()
	if err == nil && strings.TrimSpace(string(out)) != "" {
		return ErrAppRunning
	}
	return nil
}

// waitForAgent waits for a process of the copy that has the agent library
// mapped, which is the process holding the database key.
func waitForAgent(ctx context.Context, match string, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if out, err := exec.Command("pgrep", "-f", match).Output(); err == nil {
			for _, field := range strings.Fields(string(out)) {
				pid, err := strconv.Atoi(field)
				if err != nil {
					continue
				}
				if maps, err := exec.Command("vmmap", strconv.Itoa(pid)).Output(); err == nil &&
					bytes.Contains(maps, []byte("libai_agent")) {
					return pid, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return 0, ErrNoProcess
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// pkill stops every process of the copy, and insists if the graceful signal is
// ignored, which Electron does often enough to matter.
func pkill(match string) {
	for range 10 {
		out, _ := exec.Command("pgrep", "-f", match).Output()
		if strings.TrimSpace(string(out)) == "" {
			return
		}
		_ = exec.Command("pkill", "-f", match).Run()
		time.Sleep(time.Second)
	}
	_ = exec.Command("pkill", "-9", "-f", match).Run()
}
