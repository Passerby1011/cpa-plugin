package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDoer is a scripted HTTP transport. It records requests so tests can assert
// on the exact URL/headers the plugin would send, and never touches a network.
type fakeDoer struct {
	handlers map[string]func(*HTTPRequest) (HTTPResponse, error)
	calls    []string
}

func (f *fakeDoer) Do(_ context.Context, req HTTPRequest) (HTTPResponse, error) {
	f.calls = append(f.calls, req.Method+" "+req.URL)
	path := urlPath(req.URL)
	for pattern, handler := range f.handlers {
		if pattern == path {
			return handler(&req)
		}
	}
	// Fall back to prefix matching for admin sub-paths.
	for pattern, handler := range f.handlers {
		if strings.HasPrefix(pattern, "/admin") && strings.HasPrefix(path, pattern) {
			return handler(&req)
		}
	}
	return HTTPResponse{StatusCode: 599, Body: []byte("no handler for " + req.URL)}, nil
}

// urlPath extracts the path of an absolute URL without importing net/url twice.
func urlPath(rawURL string) string {
	idx := strings.Index(rawURL, "//")
	if idx < 0 {
		return rawURL
	}
	rest := rawURL[idx+2:]
	if slash := strings.Index(rest, "/"); slash >= 0 {
		return rest[slash:]
	}
	return "/"
}

func jsonResponse(status int, body string) HTTPResponse {
	return HTTPResponse{StatusCode: status, Body: []byte(body), Headers: http.Header{"Content-Type": []string{"application/json"}}}
}

func newFakeBackend(apiKey string) *fakeDoer {
	return &fakeDoer{handlers: map[string]func(*HTTPRequest) (HTTPResponse, error){
		"/": func(req *HTTPRequest) (HTTPResponse, error) {
			return jsonResponse(200, `{"version":"v`+ReleaseVersion+`","models":["gemini-2.5-pro"]}`), nil
		},
		"/v1/models": func(req *HTTPRequest) (HTTPResponse, error) {
			if req.Headers.Get("Authorization") != "Bearer "+apiKey {
				return jsonResponse(401, `{"error":"bad key"}`), nil
			}
			return jsonResponse(200, `{"object":"list","data":[{"id":"gemini-2.5-pro","object":"model"},{"id":"gemini-2.5-flash","object":"model"}]}`), nil
		},
	}}
}

func testSetup(apiKey string) Setup {
	const origin = "http://127.0.0.1:2048"
	return Setup{Origin: origin, BaseURL: origin + "/v1", APIKey: apiKey, AdminToken: "admin"}
}

// TestProbeReportsUpWithMatchingKey is the healthy path.
func TestProbeReportsUpWithMatchingKey(t *testing.T) {
	doer := newFakeBackend("k1")
	health := Probe(context.Background(), doer, testSetup("k1"))
	if health.State != HealthUp {
		t.Fatalf("state = %s (%s), want up", health.State, health.Reason)
	}
	if !health.APIKeyOK {
		t.Fatal("API key check did not confirm the key")
	}
	if health.Version != "v"+ReleaseVersion {
		t.Fatalf("version = %q", health.Version)
	}
}

// TestProbeReportsForeignForKeyMismatch is why the key check exists: a process
// answering on the recorded port that rejects our key is NOT our backend.
func TestProbeReportsForeignForKeyMismatch(t *testing.T) {
	doer := newFakeBackend("theirs")
	health := Probe(context.Background(), doer, testSetup("ours"))
	if health.State != HealthForeign {
		t.Fatalf("state = %s, want foreign", health.State)
	}
	if health.APIKeyOK {
		t.Fatal("a rejected key was reported as accepted")
	}
}

func TestProbeReportsDownOnRefusedConnection(t *testing.T) {
	doer := &fakeDoer{handlers: map[string]func(*HTTPRequest) (HTTPResponse, error){
		"/": func(*HTTPRequest) (HTTPResponse, error) {
			return HTTPResponse{}, errString("dial tcp 127.0.0.1:2048: connect: connection refused")
		},
	}}
	health := Probe(context.Background(), doer, testSetup("k"))
	if health.State != HealthDown {
		t.Fatalf("state = %s (%s), want down", health.State, health.Reason)
	}
}

func TestProbeReportsUnresponsiveOnTimeout(t *testing.T) {
	doer := &fakeDoer{handlers: map[string]func(*HTTPRequest) (HTTPResponse, error){
		"/": func(*HTTPRequest) (HTTPResponse, error) {
			return HTTPResponse{}, errString("context deadline exceeded")
		},
	}}
	health := Probe(context.Background(), doer, testSetup("k"))
	if health.State != HealthUnresponsive {
		t.Fatalf("state = %s, want unresponsive", health.State)
	}
}

// errString is errString from health.go; the tests use it to script transport
// failures without importing errors.

// TestProbeNeverLeaksTheAPIKeyIntoReason asserts the redaction requirement:
// status text is rendered in a page.
func TestProbeNeverLeaksTheAPIKeyIntoReason(t *testing.T) {
	const key = "super-secret-key-value"
	doer := newFakeBackend("other-key")
	health := Probe(context.Background(), doer, testSetup(key))
	if strings.Contains(health.Reason, key) {
		t.Fatalf("probe reason contains the API key: %q", health.Reason)
	}
}

func TestFetchModelsDecodesCatalog(t *testing.T) {
	doer := newFakeBackend("k")
	list, err := FetchModels(context.Background(), doer, testSetup("k"), 0)
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(list.Data) != 2 || list.Data[0].ID != "gemini-2.5-pro" {
		t.Fatalf("catalog = %+v", list.Data)
	}
}

func TestFetchModelsReportsBadKey(t *testing.T) {
	doer := newFakeBackend("k")
	_, err := FetchModels(context.Background(), doer, testSetup("wrong"), 0)
	if err == nil {
		t.Fatal("FetchModels accepted a rejected key")
	}
	if !strings.Contains(err.Error(), "rejected the API key") {
		t.Fatalf("error text lost the cause: %v", err)
	}
}

func TestFetchModelsRejectsMalformedBody(t *testing.T) {
	doer := &fakeDoer{handlers: map[string]func(*HTTPRequest) (HTTPResponse, error){
		"/v1/models": func(*HTTPRequest) (HTTPResponse, error) { return jsonResponse(200, `{"data":"not-an-array"}`), nil },
	}}
	if _, err := FetchModels(context.Background(), doer, testSetup("k"), 0); err == nil {
		t.Fatal("FetchModels accepted a non-array data field")
	}
}

// --- setup file tests ---

func TestEnsureDataFilesCreatesLoopbackOnlyDefaults(t *testing.T) {
	dataDir := t.TempDir()
	setup, err := EnsureDataFiles(dataDir, 2048)
	if err != nil {
		t.Fatalf("EnsureDataFiles: %v", err)
	}
	if setup.Origin != "http://127.0.0.1:2048" {
		t.Fatalf("origin = %q", setup.Origin)
	}
	if setup.BaseURL != "http://127.0.0.1:2048/v1" {
		t.Fatalf("base url = %q", setup.BaseURL)
	}
	if setup.APIKey == "" || setup.AdminToken == "" {
		t.Fatal("generated secrets are empty")
	}
	if setup.APIKey == setup.AdminToken {
		t.Fatal("API key and admin token must not be the same generated value")
	}

	var cfg map[string]any
	raw, readErr := os.ReadFile(filepath.Join(dataDir, ConfigFileName))
	if readErr != nil {
		t.Fatalf("read config: %v", readErr)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if cfg["host"] != "127.0.0.1" {
		t.Fatalf("host = %v, must be 127.0.0.1", cfg["host"])
	}

	// File modes: secrets must not be world-readable.
	if fi, statErr := os.Stat(filepath.Join(dataDir, SecretsFileName)); statErr == nil {
		if mode := fi.Mode().Perm(); mode&0o077 != 0 {
			t.Fatalf("secrets file mode = %o, must not be group/other readable", mode)
		}
	}
}

// TestEnsureDataFilesRefusesNonLoopbackHost is a security boundary: this
// backend holds Google cookies and must never listen publicly.
func TestEnsureDataFilesRefusesNonLoopbackHost(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, ConfigFileName), []byte(`{"host":"0.0.0.0","port":2048}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := EnsureDataFiles(dataDir, 2048); err == nil {
		t.Fatal("EnsureDataFiles accepted a non-loopback bind address")
	} else if !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("error does not explain the loopback requirement: %v", err)
	}
}

func TestEnsureDataFilesKeepsExistingPort(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, ConfigFileName), []byte(`{"host":"127.0.0.1","port":9999}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	setup, err := EnsureDataFiles(dataDir, 2048)
	if err != nil {
		t.Fatalf("EnsureDataFiles: %v", err)
	}
	if setup.Port() != 9999 {
		t.Fatalf("port = %d, an existing config's port must win over the preferred one", setup.Port())
	}
}

// TestSetPortPersistsChoice proves the port-conflict resolution is durable and
// does not drop keys the backend wrote.
func TestSetPortPersistsChoice(t *testing.T) {
	dataDir := t.TempDir()
	original := `{"host":"127.0.0.1","port":2048,"log_requests":false,"multi_turn":true,"temporary_chat":true,"custom_backend_key":"keep-me"}`
	if err := os.WriteFile(filepath.Join(dataDir, ConfigFileName), []byte(original), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := SetPort(dataDir, 2053); err != nil {
		t.Fatalf("SetPort: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, ConfigFileName))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc["port"] != float64(2053) {
		t.Fatalf("port was not persisted: %v", doc["port"])
	}
	if doc["host"] != "127.0.0.1" {
		t.Fatalf("host changed to %v", doc["host"])
	}
	if doc["custom_backend_key"] != "keep-me" {
		t.Fatal("an unknown backend key was dropped by SetPort")
	}
	if doc["multi_turn"] != true {
		t.Fatal("an existing known key was dropped by SetPort")
	}
}

func TestReadSetupReportsAbsentBeforeInstall(t *testing.T) {
	dataDir := t.TempDir()
	if _, ok := ReadSetup(dataDir); ok {
		t.Fatal("ReadSetup claimed a setup exists in an empty directory")
	}
}

func TestReadSetupRejectsMalformedSecrets(t *testing.T) {
	dataDir := t.TempDir()
	if _, err := EnsureDataFiles(dataDir, 2048); err != nil {
		t.Fatalf("EnsureDataFiles: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, SecretsFileName), []byte(`{"apiKey":""}`), 0o600); err != nil {
		t.Fatalf("write secrets: %v", err)
	}
	if _, ok := ReadSetup(dataDir); ok {
		t.Fatal("ReadSetup accepted secrets with an empty API key")
	}
}

func TestLaunchArgsAreClosedAndExplicit(t *testing.T) {
	paths := PathsFor(t.TempDir())
	args, err := LaunchArgs(paths)
	if err != nil {
		t.Fatalf("LaunchArgs: %v", err)
	}
	// The argv shape is a security boundary: exactly these two flags.
	if len(args) != 4 {
		t.Fatalf("argv = %v, want exactly --config <path> --db <path>", args)
	}
	if args[0] != "--config" || args[1] != paths.ConfigFile || args[2] != "--db" || args[3] != paths.Database {
		t.Fatalf("argv = %v", args)
	}
}

func TestLaunchArgsRejectsEmptyPaths(t *testing.T) {
	if _, err := LaunchArgs(Paths{}); err == nil {
		t.Fatal("LaunchArgs accepted empty paths")
	}
}

// TestLaunchEnvCarriesNoProxyAndNoArgvSecrets asserts two boundaries: credentials
// travel in the environment (not argv, which is world-readable), and the host's
// HTTP_PROXY is never inherited (it would route localhost calls).
func TestLaunchEnvCarriesNoProxyAndNoArgvSecrets(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://host-proxy:3128")
	t.Setenv("HTTPS_PROXY", "http://host-proxy:3128")
	t.Setenv("ALL_PROXY", "socks5://host-proxy:1080")

	env := LaunchEnv(Secrets{APIKey: "api-key-value", AdminToken: "admin-token-value"}, nil)
	for _, entry := range env {
		upper := strings.ToUpper(entry)
		for _, banned := range []string{"HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY="} {
			if strings.HasPrefix(upper, banned) {
				t.Fatalf("proxy variable leaked into the backend environment: %s", entry)
			}
		}
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "API_KEY=api-key-value") {
		t.Fatal("API key was not passed to the backend")
	}
	if !strings.Contains(joined, "ADMIN_TOKEN=admin-token-value") {
		t.Fatal("admin token was not passed to the backend")
	}
}

func TestValidateBindTargetIsStrict(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "::", "localhost", "192.168.1.5", ""} {
		if err := ValidateBindTarget(host, 2048); err == nil {
			t.Fatalf("ValidateBindTarget accepted host %q", host)
		}
	}
	if err := ValidateBindTarget("127.0.0.1", 0); err == nil {
		t.Fatal("ValidateBindTarget accepted port 0")
	}
	if err := ValidateBindTarget("127.0.0.1", 2048); err != nil {
		t.Fatalf("ValidateBindTarget rejected a valid target: %v", err)
	}
}
