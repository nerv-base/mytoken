//go:build windows

package traekey

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Supported reports whether this build can read a key out of memory.
func Supported() bool { return true }

// Capture reads the database key out of a running Trae agent.
//
// Windows has no hardened runtime to work around, so the installed app can be
// inspected as it is: Trae is started when it is not running, and the key is
// taken from the first process that holds it. Nothing is copied or re-signed,
// and the app is only read, never stopped.
func Capture(ctx context.Context, opt Options) (string, error) {
	if err := checkEncrypted(opt.DB); err != nil {
		return "", err
	}
	p, err := newProver(opt.DB)
	if err != nil {
		return "", err
	}
	procs := traeProcesses()
	if len(procs) == 0 {
		opt.log(StepLaunch)
		if err := startTrae(opt.DB); err != nil {
			return "", err
		}
	}
	opt.log(StepScan)
	// The agent opens the database a few seconds after the window appears, so
	// the search is repeated rather than run once against a process that has not
	// loaded its key yet.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var refused error
		for _, proc := range traeProcesses() {
			key, err := scanProcess(ctx, p, proc.regions)
			if key != "" {
				return ValidateKey(key)
			}
			if errors.Is(err, ErrPermission) {
				refused = err
			}
		}
		if time.Now().After(deadline) {
			if refused != nil {
				return "", refused
			}
			return "", ErrNoKey
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// traeProcess is one process whose memory may hold the key.
type traeProcess struct {
	pid  uint32
	path string
}

// traeProcesses lists the running processes of a Trae installation. The agent
// runs inside one of them, so their names are all that has to be recognised.
func traeProcesses() []traeProcess {
	buf := make([]uint32, 4096)
	var n uint32
	if err := windows.EnumProcesses(buf, &n); err != nil {
		return nil
	}
	var out []traeProcess
	for _, pid := range buf[:n/4] {
		if pid == 0 {
			continue
		}
		path, err := imagePath(pid)
		if err != nil {
			continue
		}
		if isTraePath(path) {
			out = append(out, traeProcess{pid: pid, path: path})
		}
	}
	return out
}

// isTraePath reports whether an executable belongs to a Trae installation,
// including the "[icube-subprocess]" copies the agent spawns from the same file.
func isTraePath(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	if !strings.HasSuffix(base, ".exe") {
		return false
	}
	base = strings.TrimSuffix(base, ".exe")
	return strings.Contains(base, "trae") || strings.Contains(base, "icube")
}

// imagePath returns the executable path of a process.
func imagePath(pid uint32) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:n]), nil
}

// startTrae starts the installed application if it is not already running. The
// product name comes from the database path, which lives in the app's roaming
// data directory, so this finds the right product without a table of paths.
func startTrae(dbPath string) error {
	for _, exe := range traeExes(dbPath) {
		if _, err := os.Stat(exe); err != nil {
			continue
		}
		cmd := exec.Command(exe)
		if err := cmd.Start(); err != nil {
			return err
		}
		// The process is left running: it is the user's editor, not a helper of
		// this capture, and quitting it would be rude.
		go func() { _ = cmd.Wait() }()
		return nil
	}
	return ErrNoApp
}

// traeExes lists the executables that can be the Trae installation owning
// dbPath, most likely first.
func traeExes(dbPath string) []string {
	var out []string
	// ...\AppData\Roaming\<product>\ModularData\ai-agent\database.db is the
	// database; the program lives in ...\AppData\Local\Programs\<product>\.
	if dir := filepath.Dir(filepath.Dir(filepath.Dir(dbPath))); dir != "" {
		product := filepath.Base(dir)
		local := os.Getenv("LOCALAPPDATA")
		if local != "" {
			out = append(out,
				filepath.Join(local, "Programs", product, product+".exe"),
				filepath.Join(local, "Programs", product, "Trae.exe"),
			)
		}
	}
	for _, env := range []string{"LOCALAPPDATA", "ProgramFiles", "ProgramFiles(x86)"} {
		dir := os.Getenv(env)
		if dir == "" {
			continue
		}
		for _, product := range []string{"Trae CN", "Trae", "TRAE SOLO CN", "Trae Solo CN"} {
			out = append(out,
				filepath.Join(dir, "Programs", product, product+".exe"),
				filepath.Join(dir, product, product+".exe"),
			)
		}
	}
	return out
}

// regions walks the readable memory of one process, region by region.
func (p traeProcess) regions(yield func([]byte) bool) error {
	h, err := windows.OpenProcess(windows.PROCESS_VM_READ|windows.PROCESS_QUERY_INFORMATION, false, p.pid)
	if err != nil {
		// A protected process (or one of another user) is not readable; the
		// capture moves on to the next candidate.
		return nil
	}
	defer windows.CloseHandle(h)
	const chunk = 1 << 20
	buf := make([]byte, chunk)
	var addr uintptr
	var info windows.MemoryBasicInformation
	for {
		if err := windows.VirtualQueryEx(h, addr, &info, unsafe.Sizeof(info)); err != nil {
			return nil
		}
		size := uintptr(info.RegionSize)
		if size == 0 {
			return nil
		}
		if readable(info) {
			for off := uintptr(0); off < size; off += chunk {
				n := size - off
				if n > chunk {
					n = chunk
				}
				var read uintptr
				if err := windows.ReadProcessMemory(h, addr+off, &buf[0], n, &read); err != nil || read == 0 {
					break
				}
				if !yield(buf[:read]) {
					return nil
				}
			}
		}
		next := addr + size
		if next <= addr {
			return nil
		}
		addr = next
	}
}

// readable reports whether a region's protection allows reading it. A guard page
// would fault on access, and an uncommitted region is not mapped at all.
func readable(info windows.MemoryBasicInformation) bool {
	if info.State != windows.MEM_COMMIT {
		return false
	}
	const readMask = windows.PAGE_READONLY | windows.PAGE_READWRITE | windows.PAGE_WRITECOPY |
		windows.PAGE_EXECUTE_READ | windows.PAGE_EXECUTE_READWRITE | windows.PAGE_EXECUTE_WRITECOPY
	return info.Protect&readMask != 0 && info.Protect&windows.PAGE_GUARD == 0
}
