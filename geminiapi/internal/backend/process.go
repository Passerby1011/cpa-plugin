package backend

// Process lifecycle: spawn, identity-checked terminate, and process-group
// reclamation.
//
// # The orphan risk this code exists to close
//
// The backend is a long-lived child process of the CPA host process. If CPA
// exits without stopping it, the backend keeps listening on its port, keeps its
// Google session alive, and the next plugin load finds a port it cannot use.
// Two mechanisms prevent that:
//
//   - Windows: the child is placed in a Job Object with
//     JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so the OS terminates the whole job
//     (child and any grandchildren) when the last handle closes — including
//     when the host dies without running any cleanup.
//   - Unix: the child is started in its own process group and terminated with
//     a signal to the GROUP (-pid), so a wrapper's grandchildren die too.
//
// On top of that, the manager's shutdown path explicitly stops the backend, and
// the PID file lets a later run clean up an orphan left by a hard kill.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// spawnTimeout bounds how long starting the process may take. A start that
// blocks this long is a failure, not a slow machine.
var spawnTimeout = 20 * time.Second

// terminateGrace is how long a SIGTERM/taskkill-style request gets before the
// process is killed outright.
var terminateGrace = 8 * time.Second

// SpawnOptions is one backend launch.
type SpawnOptions struct {
	// BinaryPath MUST be a path this package verified against its pin.
	BinaryPath string
	Args       []string
	Env        []string
	Dir        string
	// LogPath receives stdout and stderr, opened append-only and 0o600.
	LogPath string
}

// Spawned is a running backend process. Its PID is recorded in the data
// directory so a later run can find and reclaim it.
type Spawned struct {
	PID int

	cmd   *exec.Cmd
	group platformGroup
	once  sync.Once
}

// spawnFunc is a seam for tests: it defaults to the real launcher.
var spawnFunc = spawnProcess

// Spawn starts the backend.
func Spawn(opts SpawnOptions) (*Spawned, error) {
	if opts.BinaryPath == "" {
		return nil, errors.New("backend binary path is empty")
	}
	if _, err := os.Stat(opts.BinaryPath); err != nil {
		return nil, fmt.Errorf("backend binary is not readable at %s: %w", opts.BinaryPath, err)
	}
	return spawnFunc(opts)
}

func spawnProcess(opts SpawnOptions) (*Spawned, error) {
	logFile, err := os.OpenFile(opts.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open backend log %s: %w", opts.LogPath, err)
	}
	cmd := exec.Command(opts.BinaryPath, opts.Args...)
	cmd.Dir = opts.Dir
	cmd.Env = opts.Env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	preparePlatformCommand(cmd)

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("start backend %s: %w", opts.BinaryPath, err)
	}
	// The log handle belongs to the child now; closing our copy is correct —
	// a second plugin load reopens it for its own child.
	_ = logFile.Close()

	spawned := &Spawned{PID: cmd.Process.Pid, cmd: cmd}
	group, err := adoptProcessGroup(spawned.PID)
	if err != nil {
		// Adoption failing is not fatal for a normal start, but it means the
		// orphan guarantee is weaker than promised, so it is reported upward
		// rather than swallowed.
		return spawned, fmt.Errorf("started backend pid %d but could not join it to a reclaimable process group: %w", spawned.PID, err)
	}
	spawned.group = group
	return spawned, nil
}

// Alive reports whether this process is still running.
func (s *Spawned) Alive() bool {
	if s == nil || s.PID <= 0 {
		return false
	}
	return ProcessAlive(s.PID)
}

// Terminate stops this process and everything in its group, waiting up to
// terminateGrace and then killing. It is idempotent.
func (s *Spawned) Terminate() error {
	if s == nil {
		return nil
	}
	s.once.Do(func() {
		terminateProcessGroup(s.PID, s.group, terminateGrace)
		releasePlatformGroup(s.group)
		s.group = platformGroup{}
	})
	if s.Alive() {
		return fmt.Errorf("backend pid %d is still running after terminate", s.PID)
	}
	return nil
}

// Release drops the platform handles without signalling. It is what the
// manager calls after a successful terminate; a no-op otherwise.
func (s *Spawned) Release() {
	if s == nil {
		return
	}
	releasePlatformGroup(s.group)
	s.group = platformGroup{}
}

// StopVerifiedPID terminates a process only when its executable path matches
// expectedBinaryPath. It is the function the manager uses to reclaim a backend
// left behind by a previous run — and the function that refuses to touch
// anything else, however convincing its PID looks.
//
// Returns (true, nil) when the process is gone, (false, nil) when the PID is
// not ours (nothing was killed), and an error when it was ours but survived.
func StopVerifiedPID(pid int, expectedBinaryPath string) (bool, error) {
	if pid <= 0 {
		return true, nil
	}
	if !ProcessAlive(pid) {
		return true, nil
	}
	if !VerifyProcessIdentity(pid, expectedBinaryPath) {
		// The PID is live but belongs to another program. Say so plainly; the
		// caller records it and moves on rather than forcing a kill.
		return false, nil
	}
	group, _ := adoptProcessGroup(0) // 0: no adoption, just a marker for the release
	if err := terminatePIDVerified(pid, expectedBinaryPath); err != nil {
		releasePlatformGroup(group)
		return false, err
	}
	releasePlatformGroup(group)
	if ProcessAlive(pid) {
		return true, fmt.Errorf("pid %d is still running after stop", pid)
	}
	return true, nil
}

// StalePIDReport describes what a leftover PID record turned out to be.
type StalePIDReport struct {
	// Recorded is the PID read from the file (0 when there was none).
	Recorded int
	// Alive reports whether that PID exists.
	Alive bool
	// Ours reports whether it is running this data directory's backend.
	Ours bool
	// Stopped reports whether it was ours and has now exited.
	Stopped bool
	// Message is a secret-free human-readable summary.
	Message string
}

// ReclaimStale stops a backend left by a previous run.
//
// The PID file is deleted in every outcome: a record that does not identify
// our binary is worthless, and keeping it would make the next run try again.
func ReclaimStale(dataDir string, binaryPath string) StalePIDReport {
	pid := ReadPIDFile(dataDir)
	report := StalePIDReport{Recorded: pid}
	if pid <= 0 {
		removePIDFile(dataDir)
		report.Message = "no recorded backend pid"
		return report
	}
	report.Alive = ProcessAlive(pid)
	if !report.Alive {
		removePIDFile(dataDir)
		report.Message = fmt.Sprintf("recorded pid %d is gone; record removed", pid)
		return report
	}
	if !VerifyProcessIdentity(pid, binaryPath) {
		// Something else owns this PID now: NEVER kill it.
		identity := LookupIdentity(pid)
		exe := identity.ExePath
		if exe == "" {
			exe = "unknown"
		}
		report.Message = fmt.Sprintf("recorded pid %d belongs to %s, not this backend's binary; refusing to stop it", pid, filepath.Base(exe))
		removePIDFile(dataDir)
		return report
	}
	report.Ours = true
	_, err := StopVerifiedPID(pid, binaryPath)
	if err != nil {
		report.Message = fmt.Sprintf("pid %d is this backend's, but it did not exit: %v", pid, err)
		return report
	}
	report.Stopped = true
	report.Message = fmt.Sprintf("stopped leftover backend pid %d", pid)
	removePIDFile(dataDir)
	return report
}
