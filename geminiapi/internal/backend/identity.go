package backend

// Process identity: is this PID actually OUR backend binary?
//
// A PID alone is not an identity. After a crash, a reboot, or a PID wrap the
// recorded number can belong to an unrelated process — killing it would be
// destroying somebody else's work. magpie-gemini-web refuses to kill on PID
// alone and compares the executable path too; this package reproduces that
// boundary, and the comparison is a separate, unit-tested function so the
// refusal cannot regress silently.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// executablePathFunc returns the executable path for a live PID. It is a
// package variable so tests can inject a table instead of spawning processes.
var executablePathFunc = platformExecutablePath

// ProcessIdentity is what a platform lookup found for one PID.
type ProcessIdentity struct {
	// PID is the process the lookup was for.
	PID int
	// ExePath is the resolved executable path, empty when it could not be read.
	ExePath string
	// Err is why the lookup failed, when it did.
	Err error
}

// VerifyProcessIdentity reports whether PID is currently running the exact
// executable at expectedBinaryPath.
//
// It is deliberately strict: a missing binary, an unreadable executable path,
// or any comparison error yields false. "Cannot prove it is ours" must mean
// "do not kill it".
func VerifyProcessIdentity(pid int, expectedBinaryPath string) bool {
	identity := LookupIdentity(pid)
	if identity.Err != nil {
		return false
	}
	return sameExecutable(identity.ExePath, expectedBinaryPath)
}

// sameExecutable compares two executable paths leniently on Windows (case
// folding, both resolved) and exactly elsewhere. Both sides are resolved first
// so a symlinked install directory does not read as a different program.
func sameExecutable(actual, expected string) bool {
	if actual == "" || expected == "" {
		return false
	}
	actualReal, errActual := filepath.EvalSymlinks(actual)
	if errActual != nil {
		actualReal = actual
	}
	expectedReal, errExpected := filepath.EvalSymlinks(expected)
	if errExpected != nil {
		expectedReal = expected
	}
	actualReal = filepath.Clean(actualReal)
	expectedReal = filepath.Clean(expectedReal)
	if runtime.GOOS == "windows" {
		return equalFoldPath(actualReal, expectedReal)
	}
	return actualReal == expectedReal
}

// equalFoldPath folds ASCII case; Windows paths are case-insensitive and the
// shell records whatever case the exe was started with.
func equalFoldPath(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if asciiLower(a[i]) != asciiLower(b[i]) {
			return false
		}
	}
	return true
}

func asciiLower(b byte) byte {
	if 'A' <= b && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// LookupIdentity resolves a PID's executable path.
func LookupIdentity(pid int) ProcessIdentity {
	out := ProcessIdentity{PID: pid}
	if pid <= 0 {
		out.Err = fmt.Errorf("invalid pid %d", pid)
		return out
	}
	path, err := executablePathFunc(pid)
	if err != nil {
		out.Err = err
		return out
	}
	out.ExePath = path
	return out
}

// ProcessAlive reports whether a PID exists. EPERM still means the process
// exists: it belongs to another user.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return processAlive(pid)
}

// ReadPIDFile returns the recorded PID, or 0 when there is no readable record.
// A stale file is common: the process may be gone while the number remains.
func ReadPIDFile(dataDir string) int {
	raw, err := os.ReadFile(filepath.Join(dataDir, PIDFileName))
	if err != nil {
		return 0
	}
	pid := 0
	for _, r := range string(raw) {
		if r < '0' || r > '9' {
			break
		}
		pid = pid*10 + int(r-'0')
	}
	return pid
}

// WritePIDFile records the spawned PID. 0o600 because the file names a process
// this plugin owns; it is not a secret, but it is not public either.
func WritePIDFile(dataDir string, pid int) error {
	return os.WriteFile(filepath.Join(dataDir, PIDFileName), []byte(fmt.Sprintf("%d\n", pid)), 0o600)
}

// removePIDFile deletes the record; a missing file is not an error.
func removePIDFile(dataDir string) {
	_ = os.Remove(filepath.Join(dataDir, PIDFileName))
}
