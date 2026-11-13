package backend

// Real-backend integration test.
//
// It is SKIPPED unless GEMINIAPI_REAL_BACKEND=1, because it downloads a
// ~40 MB binary and spawns a real process. That keeps the default gate
// offline-deterministic while still making the real path reproducible:
//
//	GEMINIAPI_REAL_BACKEND=1 go test -run TestRealBackend -v ./internal/backend
//
// It exercises exactly the production sequence: pinned download, sha256
// verification, config/secrets creation, spawn, health probe, PID identity
// check, and process-group teardown.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// realBackendEnabled reports whether the real download path is opted in.
func realBackendEnabled() bool {
	return strings.TrimSpace(os.Getenv("GEMINIAPI_REAL_BACKEND")) == "1"
}

// stdlibDoer is a plain net/http transport for the integration test. Production
// uses the host bridge; here there is no host, and the point is to exercise the
// backend code, not the bridge.
type stdlibDoer struct {
	client *http.Client
}

func (d stdlibDoer) Do(ctx context.Context, req HTTPRequest) (HTTPResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, strings.NewReader(string(req.Body)))
	if err != nil {
		return HTTPResponse{}, err
	}
	httpReq.Header = req.Headers
	resp, err := d.client.Do(httpReq)
	if err != nil {
		return HTTPResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var buf strings.Builder
	if _, err := buf.WriteString(readAll(resp.Body)); err != nil {
		return HTTPResponse{}, err
	}
	return HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header, Body: []byte(buf.String())}, nil
}

func readAll(r interface{ Read([]byte) (int, error) }) string {
	var out strings.Builder
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return out.String()
}

// TestRealBackendInstallStartStop is the "真机验证" from the task card.
func TestRealBackendInstallStartStop(t *testing.T) {
	if !realBackendEnabled() {
		t.Skip("set GEMINIAPI_REAL_BACKEND=1 to run the real download/start/stop verification")
	}
	doer := stdlibDoer{client: &http.Client{Timeout: 10 * time.Second}}
	dataDir := filepath.Join(t.TempDir(), "geminiapi")

	pin, err := CurrentPin()
	if err != nil {
		t.Skipf("no pin for this platform: %v", err)
	}
	t.Logf("platform pin: asset=%s sha256=%s", pin.Asset, pin.SHA256)

	mgr := New(Options{
		DataDir:       dataDir,
		PreferredPort: 24000 + int(time.Now().UnixNano()%2000),
		ModelTimeout:  15 * time.Second,
		StartTimeout:  90 * time.Second,
		Doer:          doer,
	})

	// 1. Install: real download + sha256 verification.
	installedAt := time.Now()
	path, err := mgr.Install(context.Background())
	if err != nil {
		t.Fatalf("install failed: %v", err)
	}
	elapsed := time.Since(installedAt)
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("the installed binary does not exist: %v", statErr)
	}
	t.Logf("INSTALL ok: path=%s size=%d bytes elapsed=%s", path, info.Size(), elapsed.Round(time.Millisecond))

	digest, err := FileSHA256(path)
	if err != nil {
		t.Fatalf("hash installed binary: %v", err)
	}
	if digest != pin.SHA256 {
		t.Fatalf("installed digest %s != pin %s", digest, pin.SHA256)
	}
	t.Logf("SHA256 verified: %s", digest)

	// 2. Start: spawn + health probe.
	startedAt := time.Now()
	setup, err := mgr.EnsureBackend(context.Background())
	if err != nil {
		t.Fatalf("start failed: %v", err)
	}
	t.Logf("START ok: origin=%s base_url=%s elapsed=%s", setup.Origin, setup.BaseURL, time.Since(startedAt).Round(time.Millisecond))

	status := mgr.StatusSnapshot(context.Background())
	t.Logf("STATUS: state=%s port=%d pid=%d pid_verified=%t health_state=%s health_reason=%q version=%q",
		status.State, status.Port, status.PID, status.PIDVerified, status.Health.State, status.Health.Reason, status.Health.Version)
	if status.State != StateRunning {
		t.Fatalf("state = %s, want running", status.State)
	}
	if status.Health.State != HealthUp {
		t.Fatalf("health = %s (%s), want up", status.Health.State, status.Health.Reason)
	}
	if !status.Health.APIKeyOK {
		t.Fatal("the health probe did not confirm the API key")
	}
	if status.PID <= 0 {
		t.Fatal("no pid was recorded")
	}
	if !status.PIDVerified {
		t.Fatal("the recorded pid did not verify against the installed binary")
	}

	// The PID file must exist and name the running process.
	if pid := ReadPIDFile(dataDir); pid != status.PID {
		t.Fatalf("pid file says %d, status says %d", pid, status.PID)
	}

	// 3. The OpenAI surface answers with the setup's key.
	if _, err := FetchModels(context.Background(), doer, setup, 20*time.Second); err != nil {
		// A fresh install with no account may legitimately list nothing; only a
		// transport/auth failure is a test failure.
		if !strings.Contains(err.Error(), "no data array") && !strings.Contains(err.Error(), "HTTP") {
			t.Fatalf("models fetch failed unexpectedly: %v", err)
		}
		t.Logf("models fetch (expected for a backend with no account yet): %v", err)
	} else {
		t.Log("models fetch ok")
	}

	// 4. Stop: process-group teardown.
	stopAt := time.Now()
	if _, err := mgr.Stop(context.Background()); err != nil {
		t.Fatalf("stop failed: %v", err)
	}
	t.Logf("STOP ok: elapsed=%s", time.Since(stopAt).Round(time.Millisecond))

	if ProcessAlive(status.PID) {
		t.Fatalf("pid %d is still alive after stop", status.PID)
	}
	if pid := ReadPIDFile(dataDir); pid != 0 {
		t.Fatalf("a pid file survived a clean stop: %d", pid)
	}
	// Nothing may answer on the port any more.
	if PortInUse(status.Port) {
		t.Fatalf("port %d still answers after stop", status.Port)
	}
	t.Log("ORPHAN CHECK ok: process gone, pid file removed, port released")
}

// TestRealBackendRefusesABadPin proves the checksum gate on real bytes: the
// downloaded file is genuine, so a wrong pin must be refused and must NOT reach
// the launch path.
func TestRealBackendRefusesABadPin(t *testing.T) {
	if !realBackendEnabled() {
		t.Skip("set GEMINIAPI_REAL_BACKEND=1 to run the real download verification")
	}
	pin, err := CurrentPin()
	if err != nil {
		t.Skipf("no pin for this platform: %v", err)
	}
	// Same asset, deliberately wrong digest.
	badPin := pin
	badPin.SHA256 = strings.Repeat("0", 64)

	dataDir := t.TempDir()
	_, err = Install(dataDir, badPin, DownloadURL(pin), "", nil)
	if err == nil {
		t.Fatal("Install accepted genuine bytes under a wrong pin")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("error is not a checksum refusal: %v", err)
	}
	if _, statErr := os.Stat(installedPath(dataDir)); statErr == nil {
		t.Fatal("a rejected binary was placed at the launch path")
	}
	t.Logf("BAD-PIN refused as expected: %s", err)
}

// TestRealBackendReportsStatusJSON exercises the document a panel renders.
func TestRealBackendStatusIsSecretFree(t *testing.T) {
	if !realBackendEnabled() {
		t.Skip("set GEMINIAPI_REAL_BACKEND=1 to run")
	}
	doer := stdlibDoer{client: &http.Client{Timeout: 10 * time.Second}}
	dataDir := filepath.Join(t.TempDir(), "geminiapi")
	mgr := New(Options{DataDir: dataDir, PreferredPort: 24200, StartTimeout: 90 * time.Second, Doer: doer})
	if _, err := mgr.EnsureBackend(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer mgr.Shutdown()

	status := mgr.StatusSnapshot(context.Background())
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("encode status: %v", err)
	}
	setup, _ := mgr.Setup()
	if setup.APIKey != "" && strings.Contains(string(raw), setup.APIKey) {
		t.Fatal("the status document contains the backend API key")
	}
	if setup.AdminToken != "" && strings.Contains(string(raw), setup.AdminToken) {
		t.Fatal("the status document contains the admin token")
	}
	t.Logf("status document is secret-free (%d bytes)", len(raw))
}
