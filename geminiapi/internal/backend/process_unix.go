//go:build !windows

package backend

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// platformGroup is the Unix process group marker. The child is started as a
// session leader (Setpgid), so its PGID equals its PID and a single kill(-pid)
// reaches every process it spawned.
type platformGroup struct {
	pgid int
}

// preparePlatformCommand puts the child in its own process group.
func preparePlatformCommand(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pgid = 0
}

// adoptProcessGroup records the child's process group so Terminate can signal
// the whole group. pid <= 0 yields a marker with no group.
func adoptProcessGroup(pid int) (platformGroup, error) {
	if pid <= 0 {
		return platformGroup{}, nil
	}
	return platformGroup{pgid: pid}, nil
}

// releasePlatformGroup is a no-op on Unix: the group dies with its members.
func releasePlatformGroup(_ platformGroup) {}

// terminateProcessGroup sends SIGTERM to the whole group, waits, then SIGKILLs
// what remains.
func terminateProcessGroup(pid int, group platformGroup, grace time.Duration) {
	pgid := group.pgid
	if pgid <= 0 {
		pgid = pid
	}
	// Negative pid = the process group.
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	if waitForExit(pid, grace) {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	waitForExit(pid, 2*time.Second)
}

func terminatePIDVerified(pid int, _ string) error {
	_ = syscall.Kill(pid, syscall.SIGTERM)
	if waitForExit(pid, terminateGrace) {
		return nil
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	if waitForExit(pid, 2*time.Second) {
		return nil
	}
	return fmt.Errorf("pid %d survived SIGKILL", pid)
}

func waitForExit(pid int, budget time.Duration) bool {
	if pid <= 0 {
		return true
	}
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if !ProcessAlive(pid) {
			return true
		}
		time.Sleep(80 * time.Millisecond)
	}
	return !ProcessAlive(pid)
}

// processAlive uses signal 0: it succeeds for a live process, EPERM means it
// exists but belongs to another user.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return err == syscall.EPERM
}

// platformExecutablePath resolves /proc/<pid>/exe, falling back to ps for
// platforms without procfs (macOS).
func platformExecutablePath(pid int) (string, error) {
	procExe := filepath.Join("/proc", fmt.Sprint(pid), "exe")
	if target, err := os.Readlink(procExe); err == nil && target != "" {
		return target, nil
	}
	out, err := exec.Command("ps", "-p", fmt.Sprint(pid), "-o", "comm=").Output()
	if err != nil {
		return "", fmt.Errorf("resolve executable for pid %d: %w", pid, err)
	}
	path := trimSpaceBytes(out)
	if path == "" {
		return "", fmt.Errorf("pid %d has no readable executable path", pid)
	}
	return path, nil
}

func trimSpaceBytes(raw []byte) string {
	start, end := 0, len(raw)
	for start < end && isSpaceByte(raw[start]) {
		start++
	}
	for end > start && isSpaceByte(raw[end-1]) {
		end--
	}
	return string(raw[start:end])
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
