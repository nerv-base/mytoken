//go:build linux

package traekey

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Supported reports whether this build can read a key out of memory.
func Supported() bool { return true }

// Capture reads the database key out of a running Trae agent.
//
// Trae is started when it is not running, and the key is taken from the first
// process that holds it. Reading another process's memory is what the kernel
// gates behind ptrace, so this attaches, walks the memory and detaches: Trae is
// paused for the length of the walk and nothing else changes.
func Capture(ctx context.Context, opt Options) (string, error) {
	if err := checkEncrypted(opt.DB); err != nil {
		return "", err
	}
	p, err := newProver(opt.DB)
	if err != nil {
		return "", err
	}
	if len(traeProcesses()) == 0 {
		opt.log(StepLaunch)
		if err := startTrae(opt.DB); err != nil {
			return "", err
		}
	}
	opt.log(StepScan)
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
	pid  int
	path string
}

// traeProcesses lists the running processes of a Trae installation by their
// executable path; the agent runs inside one of them.
func traeProcesses() []traeProcess {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []traeProcess
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		exe, err := os.Readlink(filepath.Join("/proc", entry.Name(), "exe"))
		if err != nil {
			continue
		}
		if isTraePath(exe) {
			out = append(out, traeProcess{pid: pid, path: exe})
		}
	}
	return out
}

// isTraePath reports whether an executable belongs to a Trae installation.
func isTraePath(path string) bool {
	path = strings.TrimSuffix(path, " (deleted)")
	return strings.Contains(strings.ToLower(filepath.Base(path)), "trae")
}

// startTrae starts the installed application if it is not already running.
func startTrae(dbPath string) error {
	for _, exe := range traeExes(dbPath) {
		if info, err := os.Stat(exe); err != nil || info.IsDir() {
			continue
		}
		cmd := exec.Command(exe)
		if err := cmd.Start(); err != nil {
			continue
		}
		// The process is left running: it is the user's editor, not a helper of
		// this capture, and quitting it would be rude.
		go func() { _ = cmd.Wait() }()
		return nil
	}
	return ErrNoApp
}

// traeExes lists the executables that can be the Trae installation owning
// dbPath. It covers a PATH install, the usual /opt and ~/.local locations, and
// the AppImage people keep in ~/Applications.
func traeExes(dbPath string) []string {
	var out []string
	add := func(path string) {
		if path != "" {
			out = append(out, path)
		}
	}
	// ~/.config/<product>/ModularData/ai-agent/database.db names the product.
	if dir := filepath.Dir(filepath.Dir(filepath.Dir(dbPath))); dir != "" {
		product := filepath.Base(dir)
		for _, base := range []string{"/opt", filepath.Join(os.Getenv("HOME"), ".local", "share")} {
			add(filepath.Join(base, product, strings.ToLower(product)))
			add(filepath.Join(base, product, product))
		}
	}
	for _, name := range []string{"trae", "trae-cn", "trae-cn.AppImage", "trae.AppImage"} {
		if path, err := exec.LookPath(name); err == nil {
			add(path)
		}
		add(filepath.Join("/opt", name, name))
		add(filepath.Join(os.Getenv("HOME"), ".local", "bin", name))
	}
	for _, glob := range []string{
		filepath.Join(os.Getenv("HOME"), "Applications", "*rae*.AppImage"),
		filepath.Join(os.Getenv("HOME"), "Applications", "*RAE*.AppImage"),
	} {
		matches, _ := filepath.Glob(glob)
		out = append(out, matches...)
	}
	return out
}

// regions attaches to the process, walks its readable memory and detaches again.
// The attach is what Linux requires before another process's memory can be read.
func (p traeProcess) regions(yield func([]byte) bool) error {
	if err := syscall.PtraceAttach(p.pid); err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			return fmt.Errorf("%w: another program may only be inspected by a debugger. "+
				"Either run `sudo sysctl -w kernel.yama.ptrace_scope=0` first, or start MyToken "+
				"from the same shell as Trae so it is a descendant", ErrPermission)
		}
		return err
	}
	defer func() { _ = syscall.PtraceDetach(p.pid) }()
	// Attaching is asynchronous: the process has to be reported stopped before
	// its memory can be walked consistently.
	var status syscall.WaitStatus
	if _, err := syscall.Wait4(p.pid, &status, syscall.WALL, nil); err != nil {
		return nil
	}

	maps, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(p.pid), "maps"))
	if err != nil {
		return nil
	}
	const chunk = 1 << 20
	buf := make([]byte, chunk)
	local := []unix.Iovec{{Base: &buf[0], Len: chunk}}
	for _, line := range strings.Split(string(maps), "\n") {
		start, end, ok := parseMapLine(line)
		if !ok {
			continue
		}
		for addr := start; addr < end; {
			n := end - addr
			if n > chunk {
				n = chunk
			}
			local[0].Len = uint64(n)
			read, err := unix.ProcessVMReadv(p.pid, local, []unix.RemoteIovec{{Base: addr, Len: int(n)}}, 0)
			if err != nil || read <= 0 {
				break
			}
			if !yield(buf[:read]) {
				return nil
			}
			addr += uintptr(read)
		}
	}
	return nil
}

// parseMapLine reads the "start-end perms ..." head of a /proc/<pid>/maps line
// and keeps the regions that can be read.
func parseMapLine(line string) (start, end uintptr, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 || !strings.Contains(fields[1], "r") {
		return 0, 0, false
	}
	rng := strings.SplitN(fields[0], "-", 2)
	if len(rng) != 2 {
		return 0, 0, false
	}
	lo, err := strconv.ParseUint(rng[0], 16, 64)
	if err != nil {
		return 0, 0, false
	}
	hi, err := strconv.ParseUint(rng[1], 16, 64)
	if err != nil || hi <= lo {
		return 0, 0, false
	}
	return uintptr(lo), uintptr(hi), true
}
