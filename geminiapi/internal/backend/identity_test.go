package backend

import (
	"fmt"
	"os"
	"path/filepath"
	runtimepkg "runtime"
	"testing"
)

// TestVerifyProcessIdentityRefusesUnrelatedProcess is the acceptance criterion
// "PID 身份核对拒绝误杀": a live PID belonging to a different program must never
// be treated as ours.
func TestVerifyProcessIdentityRefusesUnrelatedProcess(t *testing.T) {
	binaryA := filepath.Join(t.TempDir(), "a.exe")
	binaryB := filepath.Join(t.TempDir(), "b.exe")
	for _, path := range []string{binaryA, binaryB} {
		if err := os.WriteFile(path, []byte("x"), 0o700); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	// The PID really belongs to B.
	restore := executablePathFunc
	executablePathFunc = func(int) (string, error) { return binaryB, nil }
	defer func() { executablePathFunc = restore }()

	if VerifyProcessIdentity(4242, binaryA) {
		t.Fatal("identity check accepted a PID running a different executable")
	}
	if !VerifyProcessIdentity(4242, binaryB) {
		t.Fatal("identity check rejected a PID running the expected executable")
	}
}

// TestVerifyProcessIdentityTreatsLookupFailureAsNotOurs: "cannot prove it is
// ours" must mean "do not kill it".
func TestVerifyProcessIdentityTreatsLookupFailureAsNotOurs(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "backend")
	if err := os.WriteFile(binaryPath, []byte("x"), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}
	restore := executablePathFunc
	executablePathFunc = func(int) (string, error) { return "", fmt.Errorf("access denied") }
	defer func() { executablePathFunc = restore }()

	if VerifyProcessIdentity(4242, binaryPath) {
		t.Fatal("identity check passed when the executable path could not be read")
	}
}

func TestVerifyProcessIdentityRejectsMissingExpectedBinary(t *testing.T) {
	restore := executablePathFunc
	executablePathFunc = func(int) (string, error) { return "/tmp/whatever", nil }
	defer func() { executablePathFunc = restore }()

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if VerifyProcessIdentity(4242, missing) {
		t.Fatal("identity check accepted a PID when the expected binary does not exist")
	}
}

func TestVerifyProcessIdentityRejectsInvalidPID(t *testing.T) {
	for _, pid := range []int{0, -1, -99} {
		if VerifyProcessIdentity(pid, "/bin/sh") {
			t.Fatalf("identity check accepted invalid pid %d", pid)
		}
	}
}

func TestSameExecutableIsCaseSensitiveOutsideWindows(t *testing.T) {
	if runtimepkg.GOOS == "windows" {
		t.Skip("windows paths are compared case-insensitively by design")
	}
	if sameExecutable("/tmp/Backend", "/tmp/backend") {
		t.Fatal("case-different paths were treated as the same executable on a case-sensitive OS")
	}
}

func TestSameExecutableRejectsEmptyInputs(t *testing.T) {
	if sameExecutable("", "/tmp/x") || sameExecutable("/tmp/x", "") || sameExecutable("", "") {
		t.Fatal("an empty path must never match")
	}
}

// TestStopVerifiedPIDRefusesForeignProcess proves the negative-control path
// end-to-end at the function the manager calls: nothing is killed.
func TestStopVerifiedPIDRefusesForeignProcess(t *testing.T) {
	expected := filepath.Join(t.TempDir(), "ours")
	if err := os.WriteFile(expected, []byte("x"), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}
	restore := executablePathFunc
	executablePathFunc = func(int) (string, error) { return "/usr/bin/other", nil }
	defer func() { executablePathFunc = restore }()

	// A live PID (this test process) that is NOT our binary.
	stopped, err := StopVerifiedPID(os.Getpid(), expected)
	if stopped {
		t.Fatal("StopVerifiedPID claimed to stop a foreign process")
	}
	if err != nil {
		t.Fatalf("refusing a foreign process is not an error: %v", err)
	}
}

func TestReclaimStaleRemovesRecordForDeadPID(t *testing.T) {
	dataDir := t.TempDir()
	binaryPath := filepath.Join(dataDir, "bin", "backend")
	// A PID that cannot exist.
	if err := WritePIDFile(dataDir, 2147483646); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	report := ReclaimStale(dataDir, binaryPath)
	if report.Recorded != 2147483646 {
		t.Fatalf("recorded = %d", report.Recorded)
	}
	if report.Alive {
		t.Fatal("an impossible PID reported as alive")
	}
	if _, err := os.Stat(filepath.Join(dataDir, PIDFileName)); !os.IsNotExist(err) {
		t.Fatal("stale pid file was not removed")
	}
}

func TestReclaimStaleNeverKillsForeignPID(t *testing.T) {
	dataDir := t.TempDir()
	binaryPath := filepath.Join(t.TempDir(), "backend")
	if err := os.WriteFile(binaryPath, []byte("x"), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Record THIS test process, but make the identity lookup claim another
	// program: the reclaim must report but never stop.
	if err := WritePIDFile(dataDir, os.Getpid()); err != nil {
		t.Fatalf("write pid file: %v", err)
	}
	restore := executablePathFunc
	executablePathFunc = func(int) (string, error) { return "/usr/bin/bash", nil }
	defer func() { executablePathFunc = restore }()

	report := ReclaimStale(dataDir, binaryPath)
	if report.Recorded == 0 {
		t.Fatal("report lost the recorded pid")
	}
	if !report.Alive {
		t.Fatal("this test process should be alive")
	}
	if report.Ours {
		t.Fatal("a PID running another program was claimed as ours")
	}
	if report.Stopped {
		t.Fatal("a foreign PID was reported as stopped")
	}
}

func TestReadPIDFileIgnoresGarbage(t *testing.T) {
	dataDir := t.TempDir()
	if pid := ReadPIDFile(filepath.Join(dataDir)); pid != 0 {
		t.Fatalf("missing pid file yielded %d", pid)
	}
	if err := os.WriteFile(filepath.Join(dataDir, PIDFileName), []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if pid := ReadPIDFile(dataDir); pid != 0 {
		t.Fatalf("garbage pid file yielded %d", pid)
	}
	if err := os.WriteFile(filepath.Join(dataDir, PIDFileName), []byte("1234\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if pid := ReadPIDFile(dataDir); pid != 1234 {
		t.Fatalf("pid = %d, want 1234", pid)
	}
}
