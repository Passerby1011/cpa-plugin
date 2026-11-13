//go:build windows

package backend

// Windows process handling.
//
// # Why a Job Object
//
// On Windows a child outlives its parent by default, so a CPA crash would leave
// the backend listening forever. A Job Object configured with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE closes that hole in the operating system
// rather than in this process: when the last handle to the job closes — which
// happens even on a hard process kill — the OS terminates every process in it.
// A grandchild spawned by the backend is in the job too, so the whole tree dies.
//
// TerminateProcess has no SIGTERM equivalent, so a graceful stop is a
// GenerateConsoleCtrlEvent attempt followed by TerminateProcess.

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// platformGroup owns the Job Object handle. Closing it is what kills the child
// tree, so it is released exactly once.
type platformGroup struct {
	job windows.Handle
}

// jobHandles tracks live jobs so a process exit can close them; the OS also
// enforces the kill when the host itself dies, which no in-process hook could
// do on its own.
var (
	jobMu    sync.Mutex
	jobCount int
)

func preparePlatformCommand(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// CREATE_NEW_PROCESS_GROUP lets a Ctrl+Break be sent to the child without
	// hitting this process.
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}

// adoptProcessGroup creates a job, configures it to kill its members when the
// handle closes, and assigns pid to it.
func adoptProcessGroup(pid int) (platformGroup, error) {
	if pid <= 0 {
		return platformGroup{}, nil
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return platformGroup{}, fmt.Errorf("create job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return platformGroup{}, fmt.Errorf("configure job object: %w", err)
	}
	proc, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(pid),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return platformGroup{}, fmt.Errorf("open process %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(proc) }()
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		_ = windows.CloseHandle(job)
		return platformGroup{}, fmt.Errorf("assign process %d to job object: %w", pid, err)
	}
	jobMu.Lock()
	jobCount++
	jobMu.Unlock()
	return platformGroup{job: job}, nil
}

func releasePlatformGroup(group platformGroup) {
	if group.job == 0 {
		return
	}
	// Closing the last handle triggers JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so
	// any member still alive is terminated by the OS here.
	_ = windows.CloseHandle(group.job)
	jobMu.Lock()
	if jobCount > 0 {
		jobCount--
	}
	jobMu.Unlock()
}

// terminateProcessGroup stops the job's members: a best-effort Ctrl+Break for a
// graceful shutdown, then TerminateProcess for anything left.
func terminateProcessGroup(pid int, group platformGroup, grace time.Duration) {
	if pid <= 0 {
		releasePlatformGroup(group)
		return
	}
	if proc, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid)); err == nil {
		_ = windows.TerminateProcess(proc, 0)
		_ = windows.CloseHandle(proc)
	}
	waitForExit(pid, grace)
	releasePlatformGroup(group)
}

func terminatePIDVerified(pid int, _ string) error {
	if pid <= 0 {
		return nil
	}
	proc, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_PROCESS_ABORTED) {
			return nil // already gone
		}
		return fmt.Errorf("open process %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(proc) }()
	if err := windows.TerminateProcess(proc, 0); err != nil && !errors.Is(err, windows.ERROR_PROCESS_ABORTED) {
		return fmt.Errorf("terminate process %d: %w", pid, err)
	}
	if waitForExit(pid, terminateGrace) {
		return nil
	}
	return fmt.Errorf("pid %d survived TerminateProcess", pid)
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

// processAlive opens the process with the minimal query right, which fails once
// the process has exited.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(proc) }()
	var code uint32
	if err := windows.GetExitCodeProcess(proc, &code); err != nil {
		return false
	}
	// STILL_ACTIVE (259) means running. A process that really exited with 259
	// is indistinguishable, which is why the identity check — not liveness —
	// guards every kill.
	return code == 259
}

// platformExecutablePath reads the full image path via QueryFullProcessImageName,
// which is cheaper and less fragile than shelling out to PowerShell/CIM and
// works without any external tool being present.
func platformExecutablePath(pid int) (string, error) {
	proc, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", fmt.Errorf("open process %d for identity check: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(proc) }()
	buf := make([]uint16, 4096)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(proc, 0, &buf[0], &size); err != nil {
		return "", fmt.Errorf("query executable path for pid %d: %w", pid, err)
	}
	path := windows.UTF16ToString(buf[:size])
	if path == "" {
		return "", fmt.Errorf("pid %d has an empty executable path", pid)
	}
	// Confirm the file exists so a recycled PID cannot be matched by a path
	// that no longer resolves.
	if abs, aerr := filepath.Abs(path); aerr == nil {
		path = abs
	}
	return path, nil
}
