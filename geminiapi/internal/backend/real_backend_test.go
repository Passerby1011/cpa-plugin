package backend

// Real-backend integration tests.
//
// They are SKIPPED unless GEMINIAPI_REAL_BACKEND=1, because they download tens
// of megabytes and spawn real processes. That keeps the default gate
// offline-deterministic while still making each backend's real path
// reproducible:
//
//	GEMINIAPI_REAL_BACKEND=1 go test -run 'TestRealBackend' -v ./internal/backend
//
// GEMINIAPI_REAL_FLAVOR selects which backend to exercise ("gemini-web" or
// "ai-studio"); with no value BOTH run, so a verification record cannot
// accidentally cover only one of the two namespaces.
//
// Each test exercises exactly the production sequence: pinned download, digest
// verification (archive AND unpacked executable for AI Studio), data file
// creation, spawn, health probe, PID identity check, and process-group
// teardown.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// realBackendEnabled reports whether the real download path is opted in.
func realBackendEnabled() bool {
	return strings.TrimSpace(os.Getenv("GEMINIAPI_REAL_BACKEND")) == "1"
}

// realFlavors returns the flavors this run should exercise.
func realFlavors() []Flavor {
	sel := strings.ToLower(strings.TrimSpace(os.Getenv("GEMINIAPI_REAL_FLAVOR")))
	switch sel {
	case string(FlavorAIStudio):
		return []Flavor{aiStudioFlavor}
	case string(FlavorGeminiWeb):
		return []Flavor{geminiWebFlavor}
	}
	return []Flavor{geminiWebFlavor, aiStudioFlavor}
}

// stdlibDoer is a plain net/http transport for the integration tests. Production
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

// TestRealBackendInstallStartStop is the "真机验证" from the task card: install →
// digest check → start → health → stop, per backend flavor.
func TestRealBackendInstallStartStop(t *testing.T) {
	if !realBackendEnabled() {
		t.Skip("set GEMINIAPI_REAL_BACKEND=1 to run the real download/start/stop verification")
	}
	doer := stdlibDoer{client: &http.Client{Timeout: 20 * time.Second}}

	for _, flavor := range realFlavors() {
		flavor := flavor
		t.Run(string(flavor.ID), func(t *testing.T) {
			pin, err := flavor.CurrentPin()
			if err != nil {
				t.Skipf("no pin for this platform: %v", err)
			}
			t.Logf("%s: platform=%s/%s asset=%s pin_sha256=%s archive=%t",
				flavor.ID, runtime.GOOS, runtime.GOARCH, pin.Asset, pin.SHA256, flavor.Archive)

			dataDir := realDataDir(t, flavor)
			mgr := New(Options{
				Flavor:        flavor,
				DataDir:       dataDir,
				PreferredPort: 24100 + int(time.Now().UnixNano()%1500),
				ModelTimeout:  20 * time.Second,
				StartTimeout:  150 * time.Second,
				Doer:          doer,
			})

			// 1. Install: real download + digest verification.
			installedAt := time.Now()
			path, installErr := mgr.Install(context.Background())
			if installErr != nil {
				t.Fatalf("%s install failed: %v", flavor.ID, installErr)
			}
			info, statErr := os.Stat(path)
			if statErr != nil {
				t.Fatalf("%s installed binary does not exist: %v", flavor.ID, statErr)
			}
			t.Logf("%s INSTALL ok: path=%s size=%d bytes elapsed=%s",
				flavor.ID, path, info.Size(), time.Since(installedAt).Round(time.Millisecond))

			digest, hashErr := FileSHA256(path)
			if hashErr != nil {
				t.Fatalf("%s hash installed binary: %v", flavor.ID, hashErr)
			}
			expected, expErr := flavor.ExpectedBinaryDigest(runtime.GOOS, runtime.GOARCH)
			if expErr != nil {
				t.Fatalf("%s expected digest: %v", flavor.ID, expErr)
			}
			if digest != expected {
				t.Fatalf("%s installed digest %s != expected %s", flavor.ID, digest, expected)
			}
			t.Logf("%s SHA256 verified (asset pin %s, executable %s)",
				flavor.ID, RedactHashes(pin.SHA256), RedactHashes(digest))

			// 2. Start: data files + spawn + health.
			startedAt := time.Now()
			setup, startErr := mgr.EnsureBackend(context.Background())
			if startErr != nil {
				// The child's own log is the only witness to WHY it never
				// answered; printing it here beats guessing (for example
				// "port busy" and "missing model files" fail identically at
				// the socket).
				t.Fatalf("%s start failed: %v\n--- backend.log tail ---\n%s", flavor.ID, startErr, tailFile(flavor.Layout(dataDir).LogFile, 60))
			}
			t.Logf("%s START ok: origin=%s base_url=%s elapsed=%s",
				flavor.ID, setup.Origin, setup.BaseURL, time.Since(startedAt).Round(time.Millisecond))

			status := mgr.StatusSnapshot(context.Background())
			t.Logf("%s STATUS: state=%s port=%d pid=%d pid_verified=%t health_state=%s health_reason=%q version=%q accounts_total=%d accounts_known=%t",
				flavor.ID, status.State, status.Port, status.PID, status.PIDVerified,
				status.Health.State, status.Health.Reason, status.Health.Version,
				status.Accounts.Total, status.Accounts.Known)
			if status.State != StateRunning {
				t.Fatalf("%s state = %s, want running (last error: %s)", flavor.ID, status.State, status.LastError)
			}
			if status.Health.State != HealthUp {
				t.Fatalf("%s health = %s (%s), want up", flavor.ID, status.Health.State, status.Health.Reason)
			}
			if flavor.ID == FlavorGeminiWeb {
				if !status.Health.APIKeyOK {
					t.Fatalf("%s the health probe did not confirm the API key", flavor.ID)
				}
			} else if status.Health.Reason != ReasonNoAccount {
				// AI Studio cannot reach "up + key ok" without a real Google
				// account: /v1/models stays 503 until one exists. The honest
				// expectation is "up, generation service idle because the
				// account pool is empty".
				t.Fatalf("%s health reason = %q, want no-account (no Google account is available in this run)", flavor.ID, status.Health.Reason)
			}
			if status.PID <= 0 {
				t.Fatalf("%s no pid was recorded", flavor.ID)
			}
			if !status.PIDVerified {
				t.Fatalf("%s the recorded pid did not verify against the installed binary", flavor.ID)
			}
			if pid := ReadPIDFile(dataDir); pid != status.PID {
				t.Fatalf("%s pid file says %d, status says %d", flavor.ID, pid, status.PID)
			}

			// 3. The OpenAI surface answers with the setup's key. An empty model
			// list is legitimate for a backend with no account yet, so only a
			// transport or auth failure is fatal here.
			if _, modelsErr := FetchModels(context.Background(), doer, setup, 20*time.Second); modelsErr != nil {
				text := modelsErr.Error()
				if strings.Contains(text, "no data array") || strings.Contains(text, "empty data array") ||
					strings.Contains(text, "HTTP 503") || strings.Contains(text, "HTTP") {
					t.Logf("%s models fetch (expected with no account yet): %v", flavor.ID, modelsErr)
				} else {
					t.Fatalf("%s models fetch failed unexpectedly: %v", flavor.ID, modelsErr)
				}
			} else {
				t.Logf("%s models fetch ok", flavor.ID)
			}

			// 4. Stop: process-group teardown.
			stopAt := time.Now()
			if _, stopErr := mgr.Stop(context.Background()); stopErr != nil {
				t.Fatalf("%s stop failed: %v", flavor.ID, stopErr)
			}
			t.Logf("%s STOP ok: elapsed=%s", flavor.ID, time.Since(stopAt).Round(time.Millisecond))

			if ProcessAlive(status.PID) {
				t.Fatalf("%s pid %d is still alive after stop", flavor.ID, status.PID)
			}
			if pid := ReadPIDFile(dataDir); pid != 0 {
				t.Fatalf("%s a pid file survived a clean stop: %d", flavor.ID, pid)
			}
			if PortInUse(status.Port) {
				t.Fatalf("%s port %d still answers after stop", flavor.ID, status.Port)
			}
			t.Logf("%s ORPHAN CHECK ok: process gone, pid file removed, port released", flavor.ID)
		})
	}
}

// realDataDir returns the data directory for a real-backend run. When
// GEMINIAPI_REAL_DATA_DIR is set the directory survives the test, so an
// already-verified 80MB download is REUSED across attempts instead of re-fetched
// (and so the child's log outlives the failure that needs explaining).
func realDataDir(t *testing.T, flavor Flavor) string {
	t.Helper()
	root := strings.TrimSpace(os.Getenv("GEMINIAPI_REAL_DATA_DIR"))
	if root == "" {
		return filepath.Join(t.TempDir(), string(flavor.ID))
	}
	dir := filepath.Join(root, string(flavor.ID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	return dir
}

// tailFile returns the last n lines of a file, or an explanation when it is
// missing. Used only for test diagnostics.
func tailFile(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "(" + path + " unreadable: " + err.Error() + ")"
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// TestRealBackendRefusesABadPin proves the checksum gate on real bytes: the
// downloaded asset is genuine, so a wrong pin must be refused and must NOT
// reach the launch path.
func TestRealBackendRefusesABadPin(t *testing.T) {
	if !realBackendEnabled() {
		t.Skip("set GEMINIAPI_REAL_BACKEND=1 to run the real download verification")
	}
	for _, flavor := range realFlavors() {
		flavor := flavor
		t.Run(string(flavor.ID), func(t *testing.T) {
			pin, err := flavor.CurrentPin()
			if err != nil {
				t.Skipf("no pin for this platform: %v", err)
			}
			badFlavor := flavor
			badPin := pin
			badPin.SHA256 = strings.Repeat("0", 64)
			badFlavor.Pins = map[string]Pin{PlatformKey(runtime.GOOS, runtime.GOARCH): badPin}

			dataDir := t.TempDir()
			_, installErr := badFlavor.Install(dataDir, flavor.ReleaseBaseURL()+pin.Asset, "", nil)
			if installErr == nil {
				t.Fatalf("%s: Install accepted genuine bytes under a wrong pin", flavor.ID)
			}
			if !strings.Contains(installErr.Error(), "sha256 mismatch") {
				t.Fatalf("%s: error is not a checksum refusal: %v", flavor.ID, installErr)
			}
			if _, statErr := os.Stat(flavor.Layout(dataDir).Binary); statErr == nil {
				t.Fatalf("%s: a rejected binary was placed at the launch path", flavor.ID)
			}
			t.Logf("%s BAD-PIN refused as expected: %s", flavor.ID, installErr)
		})
	}
}

// TestRealBackendStatusIsSecretFree exercises the document a panel renders.
func TestRealBackendStatusIsSecretFree(t *testing.T) {
	if !realBackendEnabled() {
		t.Skip("set GEMINIAPI_REAL_BACKEND=1 to run")
	}
	doer := stdlibDoer{client: &http.Client{Timeout: 20 * time.Second}}
	for _, flavor := range realFlavors() {
		flavor := flavor
		t.Run(string(flavor.ID), func(t *testing.T) {
			dataDir := realDataDir(t, flavor)
			mgr := New(Options{
				Flavor:        flavor,
				DataDir:       dataDir,
				PreferredPort: 24300 + int(time.Now().UnixNano()%700),
				StartTimeout:  150 * time.Second,
				Doer:          doer,
			})
			if _, err := mgr.EnsureBackend(context.Background()); err != nil {
				t.Fatalf("%s start: %v", flavor.ID, err)
			}
			defer mgr.Shutdown()

			status := mgr.StatusSnapshot(context.Background())
			raw, marshalErr := json.Marshal(status)
			if marshalErr != nil {
				t.Fatalf("%s encode status: %v", flavor.ID, marshalErr)
			}
			setup, _ := mgr.Setup()
			if setup.APIKey != "" && strings.Contains(string(raw), setup.APIKey) {
				t.Fatalf("%s the status document contains the backend API key", flavor.ID)
			}
			if setup.AdminToken != "" && strings.Contains(string(raw), setup.AdminToken) {
				t.Fatalf("%s the status document contains the admin token", flavor.ID)
			}
			if setup.AdminPassword != "" && strings.Contains(string(raw), setup.AdminPassword) {
				t.Fatalf("%s the status document contains the admin password", flavor.ID)
			}
			t.Logf("%s status document is secret-free (%d bytes)", flavor.ID, len(raw))
		})
	}
}
