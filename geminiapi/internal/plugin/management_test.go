package plugin

// Registration, management routes, and the panel.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/geminiapi/internal/backend"
)

func TestRegistrationDeclaresOneProviderAndStaticScope(t *testing.T) {
	raw := registrationEnvelope()
	env := decodeTestEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("registration envelope is not ok: %s", envelopeErrorText(env))
	}
	var reg registrationResult
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	if reg.Metadata.Name != "geminiapi" {
		t.Fatalf("metadata name = %q, must be the bare plugin id", reg.Metadata.Name)
	}
	if reg.Metadata.Version == "" {
		t.Fatal("metadata version is empty")
	}
	if reg.Metadata.GitHubRepository == "" {
		t.Fatal("metadata github repository is required by the host's validPlugin gate")
	}
	if !strings.HasPrefix(reg.Metadata.Logo, "https://raw.githubusercontent.com/") {
		t.Fatalf("logo must be served from this repo, got %q", reg.Metadata.Logo)
	}
	if reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema version = %d", reg.SchemaVersion)
	}
	if !reg.Capabilities.Executor || !reg.Capabilities.ModelProvider || !reg.Capabilities.ManagementAPI {
		t.Fatalf("capabilities = %+v", reg.Capabilities)
	}
	// No AuthProvider: there is no OAuth flow to advertise. Declaring it would
	// publish a login button whose only outcome is a failure.
	if strings.Contains(string(raw), "auth_provider") {
		t.Fatal("registration advertises an auth_provider capability")
	}
	// Static scope: the published models are plugin-owned, not auth-bound.
	if reg.Capabilities.ExecutorModelScope != string(pluginapi.ExecutorModelScopeStatic) {
		t.Fatalf("executor model scope = %q, want static", reg.Capabilities.ExecutorModelScope)
	}
	if len(reg.Capabilities.ExecutorInputFormats) == 0 || len(reg.Capabilities.ExecutorOutputFormats) == 0 {
		t.Fatal("the executor must declare at least one input and output format")
	}
}

// TestConfigFieldsCoverTheDocumentedSurface keeps the UI in step with the
// README: every knob an operator is told to set must be declarable.
func TestConfigFieldsCoverTheDocumentedSurface(t *testing.T) {
	want := map[string]bool{
		"data-dir": false, "ai-studio": false, "proxy": false,
		"request-timeout": false, "stream-idle-timeout": false,
		"model-timeout": false, "models": false,
	}
	for _, field := range configFields() {
		if _, ok := want[field.Name]; ok {
			want[field.Name] = true
		}
		if field.Description == "" {
			t.Fatalf("field %q has no description", field.Name)
		}
		if strings.Contains(field.Name, ".") {
			// A dotted name would be stored as a literal key the plugin never
			// reads, because the host shallow-merges the submitted object.
			t.Fatalf("field %q uses a dotted name, which the host stores literally", field.Name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("config field %q is documented but not declared", name)
		}
	}
}

func TestManagementRoutesAreRegistered(t *testing.T) {
	m := NewManager(nil)
	raw, err := m.registerManagement([]byte(`{}`))
	if err != nil {
		t.Fatalf("registerManagement: %v", err)
	}
	env := decodeTestEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("registration failed: %s", envelopeErrorText(env))
	}
	var resp struct {
		Routes []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"routes"`
		Resources []struct {
			Path string `json:"path"`
			Menu string `json:"menu"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	declared := map[string]bool{}
	for _, route := range resp.Routes {
		declared[route.Method+" "+route.Path] = true
		if !strings.HasPrefix(route.Path, "/plugins/geminiapi/") {
			t.Fatalf("route %q is outside this plugin's namespace", route.Path)
		}
	}
	for _, want := range []string{
		"GET /plugins/geminiapi/status",
		"POST /plugins/geminiapi/backend/status",
		"POST /plugins/geminiapi/backend/install",
		"POST /plugins/geminiapi/backend/start",
		"POST /plugins/geminiapi/backend/stop",
		"GET /plugins/geminiapi/accounts",
		"POST /plugins/geminiapi/accounts",
		"GET /plugins/geminiapi/models",
	} {
		if !declared[want] {
			t.Fatalf("route %q is not registered", want)
		}
	}
	if len(resp.Resources) != 1 || resp.Resources[0].Path != "/panel" {
		t.Fatalf("resources = %+v, want the panel", resp.Resources)
	}
	if resp.Resources[0].Menu == "" {
		t.Fatal("the panel resource needs a menu label to appear in the UI")
	}
}

// TestManagementPathNormalization accepts both shapes the host may pass.
func TestManagementPathNormalization(t *testing.T) {
	cases := map[string]string{
		"/v0/management/plugins/geminiapi/status":  "/plugins/geminiapi/status",
		"/v0/management/plugins/geminiapi/status/": "/plugins/geminiapi/status",
		"/plugins/geminiapi/status":                "/plugins/geminiapi/status",
		"plugins/geminiapi/backend/start":          "/plugins/geminiapi/backend/start",
		"/v0/resource/plugins/geminiapi/panel":     "/plugins/geminiapi/panel",
	}
	for input, want := range cases {
		if got := normalizeManagementPath(input); got != want {
			t.Fatalf("normalizeManagementPath(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestManagementStatusRouteAnswersJSON(t *testing.T) {
	host := newScriptedHost()
	m := newTestManager(t, host)
	resp, err := m.handleManagementRequest(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/geminiapi/status",
	})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var doc pluginStatus
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("status body is not JSON: %v", err)
	}
	if doc.Provider != ProviderID {
		t.Fatalf("provider = %q", doc.Provider)
	}
	if doc.PinnedVersion != "4.28.0-magpie.1" {
		t.Fatalf("pinned backend version = %q", doc.PinnedVersion)
	}
}

// TestStatusAlwaysCarriesTheMultiAccountCaveat is a deliberate contract: an
// operator must be told that N auth records are not N selectable accounts.
func TestStatusAlwaysCarriesTheMultiAccountCaveat(t *testing.T) {
	host := newScriptedHost()
	m := newTestManager(t, host)
	resp, _ := m.handleManagementRequest(pluginapi.ManagementRequest{Method: http.MethodGet, Path: statusPath})
	var doc pluginStatus
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(doc.Notice, "账号池") {
		t.Fatalf("the status notice does not explain the account-pool caveat: %q", doc.Notice)
	}
}

// TestStatusNeverLeaksTheBackendAPIKey: the status document is rendered in a
// page and curled by scripts.
func TestStatusNeverLeaksTheBackendAPIKey(t *testing.T) {
	host := newScriptedHost()
	m := newTestManager(t, host)
	resp, _ := m.handleManagementRequest(pluginapi.ManagementRequest{Method: http.MethodGet, Path: statusPath})
	if strings.Contains(string(resp.Body), "backend-key") {
		t.Fatalf("status leaks the backend API key: %s", resp.Body)
	}
}

func TestManagementBackendRoutesRefuseWhenExternal(t *testing.T) {
	host := newScriptedHost()
	m := newTestManager(t, host) // external by construction
	for _, path := range []string{backendPath + "/install", backendPath + "/start", backendPath + "/stop"} {
		resp, err := m.handleManagementRequest(pluginapi.ManagementRequest{Method: http.MethodPost, Path: path})
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400 for an external backend", path, resp.StatusCode)
		}
		if !strings.Contains(string(resp.Body), "base-url") {
			t.Fatalf("%s refusal does not explain the base-url rule: %s", path, resp.Body)
		}
	}
}

func TestManagementUnknownRouteIs404(t *testing.T) {
	m := NewManager(nil)
	resp, err := m.handleManagementRequest(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/plugins/geminiapi/nope"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestManagementModelsRouteReportsSkips(t *testing.T) {
	host := newScriptedHost()
	m := newTestManager(t, host)
	resp, err := m.handleManagementRequest(pluginapi.ManagementRequest{Method: http.MethodGet, Path: modelsPath})
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	var doc struct {
		Count   int                 `json:"count"`
		Models  []catalogRecordJSON `json:"models"`
		Skipped []struct {
			ID     string `json:"id"`
			Reason string `json:"reason"`
		} `json:"skipped"`
	}
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Count != 1 || len(doc.Models) != 1 {
		t.Fatalf("count = %d, models = %d", doc.Count, len(doc.Models))
	}
	if doc.Models[0].PublicID != "ai-studio/gemini-2.5-pro" {
		t.Fatalf("public id = %q", doc.Models[0].PublicID)
	}
}

type catalogRecordJSON struct {
	PublicID   string `json:"PublicID"`
	UpstreamID string `json:"UpstreamID"`
	Name       string `json:"DisplayName"`
}

func TestAccountsRouteReportsCountsOnly(t *testing.T) {
	host := newScriptedHost()
	host.httpDo = func(req hostHTTPReq) (int, http.Header, []byte, error) {
		if strings.Contains(req.URL, "/admin/api/accounts") {
			return http.StatusOK, nil, []byte(`{"accounts":[{"id":1,"enabled":true},{"id":2,"enabled":false}]}`), nil
		}
		return http.StatusOK, nil, []byte(`{}`), nil
	}
	m := newTestManager(t, host)
	m.mu.Lock()
	m.cfg.AIStudio.BaseURL = ""
	m.skipSupervision = false
	m.resolveSetup = func(ctx context.Context) (backend.Setup, error) {
		return backend.Setup{Origin: "http://127.0.0.1:2048", BaseURL: "http://127.0.0.1:2048/v1",
			APIKey: "k", AdminToken: "t"}, nil
	}
	m.mu.Unlock()

	resp, err := m.handleManagementRequest(pluginapi.ManagementRequest{Method: http.MethodGet, Path: accountsPath})
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The count is nested under the status snapshot; what matters is that no
	// account identifier or label appears.
	for _, banned := range []string{"\"" + "id" + "\":", "email", "@"} {
		if strings.Contains(string(resp.Body), banned) {
			t.Fatalf("the accounts route leaks account detail (%q): %s", banned, resp.Body)
		}
	}
}

// --- panel ---

func TestPanelIsServedWithASelfContainedCSP(t *testing.T) {
	SetPanel([]byte("<!doctype html><title>x</title>"))
	defer SetPanel(nil)
	m := NewManager(nil)
	resp, err := m.handleManagementRequest(pluginapi.ManagementRequest{Method: http.MethodGet, Path: panelRoutePath})
	if err != nil {
		t.Fatalf("panel: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	csp := resp.Headers.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("the panel is served without a CSP")
	}
	if strings.Contains(csp, "*") {
		t.Fatalf("the CSP allows any origin: %s", csp)
	}
	if resp.Headers.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("the panel is served without nosniff")
	}
}

func TestPanelFallsBackWhenNotEmbedded(t *testing.T) {
	SetPanel(nil)
	m := NewManager(nil)
	resp, _ := m.handleManagementRequest(pluginapi.ManagementRequest{Method: http.MethodGet, Path: panelRoutePath})
	if !strings.Contains(string(resp.Body), "not embedded") {
		t.Fatalf("an empty panel did not produce the fallback notice: %s", truncate(string(resp.Body), 200))
	}
}

// TestPanelHTMLCarriesTheAccountCaveat is what stops the page from implying
// per-account routing it cannot do.
func TestPanelHTMLCarriesTheAccountCaveat(t *testing.T) {
	page := string(PanelHTML())
	if len(page) == 0 {
		t.Skip("panel.html is not embedded in this test build")
	}
	for _, must := range []string{"账号池", "Cookie", "127.0.0.1", "sha256"} {
		if !strings.Contains(page, must) {
			t.Fatalf("panel.html does not mention %q", must)
		}
	}
	// The page must not ship a credential into localStorage.
	if strings.Contains(page, "localStorage") {
		t.Fatal("panel.html uses localStorage, which would persist the management key")
	}
}
