package plugin

// Registration, management routes, and the panel.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/geminiapi/internal/config"
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
	// The top-level pinned version is the ai-studio backend's; EACH namespace
	// also reports its own, because the two backends have different versions.
	if doc.PinnedVersion != "0.3.5-magpie.1" {
		t.Fatalf("pinned backend version = %q, want the ai-studio backend's", doc.PinnedVersion)
	}
	if len(doc.Backends) != 2 {
		t.Fatalf("backends = %+v, want one section per namespace", doc.Backends)
	}
	want := map[string]string{
		config.NamespaceAIStudio:  "0.3.5-magpie.1",
		config.NamespaceGeminiWeb: "4.28.0-magpie.1",
	}
	for _, section := range doc.Backends {
		if want[section.Name] != section.PinnedVersion {
			t.Fatalf("namespace %s pinned version = %q, want %q", section.Name, section.PinnedVersion, want[section.Name])
		}
		if section.Backend == "" {
			t.Fatalf("namespace %s reports no backend name", section.Name)
		}
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
		resp, err := m.handleManagementRequest(pluginapi.ManagementRequest{
			Method: http.MethodPost, Path: path,
			Query: url.Values{"namespace": []string{config.NamespaceAIStudio}},
		})
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
		Count      int `json:"count"`
		Namespaces []struct {
			Namespace string              `json:"namespace"`
			Count     int                 `json:"count"`
			Models    []catalogRecordJSON `json:"models"`
			Skipped   []struct {
				ID     string `json:"id"`
				Reason string `json:"reason"`
			} `json:"skipped"`
			Error string `json:"error"`
		} `json:"namespaces"`
	}
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Namespaces) != 2 {
		t.Fatalf("namespaces = %+v, want one section per namespace", doc.Namespaces)
	}
	ids := map[string][]string{}
	for _, section := range doc.Namespaces {
		for _, model := range section.Models {
			ids[section.Namespace] = append(ids[section.Namespace], model.PublicID)
		}
	}
	// Each namespace publishes under ITS OWN prefix. This is the assertion that
	// would have caught the shipped defect: the ai-studio namespace served a
	// gemini-web catalog and published nothing.
	if len(ids[config.NamespaceAIStudio]) != 1 || ids[config.NamespaceAIStudio][0] != "ai-studio/gemini-2.5-pro" {
		t.Fatalf("ai-studio models = %v", ids[config.NamespaceAIStudio])
	}
	if len(ids[config.NamespaceGeminiWeb]) == 0 {
		t.Fatalf("gemini-web published no models: %+v", doc.Namespaces)
	}
	for _, id := range ids[config.NamespaceGeminiWeb] {
		if !strings.HasPrefix(id, "gemini-web/") {
			t.Fatalf("gemini-web model %q lacks its own prefix", id)
		}
	}
	if doc.Count != len(ids[config.NamespaceAIStudio])+len(ids[config.NamespaceGeminiWeb]) {
		t.Fatalf("total count = %d, sections = %v", doc.Count, ids)
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
	m := managedTestManager(t, host, config.NamespaceGeminiWeb, "k", "t")

	resp, err := m.handleManagementRequest(pluginapi.ManagementRequest{
		Method: http.MethodGet, Path: accountsPath,
		Query: url.Values{"namespace": []string{config.NamespaceGeminiWeb}},
	})
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The count is nested under the account summary; what matters is that no
	// account identifier or label appears.
	for _, banned := range []string{"\"" + "id" + "\":", "email", "@"} {
		if strings.Contains(string(resp.Body), banned) {
			t.Fatalf("the accounts route leaks account detail (%q): %s", banned, resp.Body)
		}
	}
}

// TestAccountsRouteNamesBothNamespaces pins the two-namespace answer: a GET
// without a selector reports both, and each says which import mechanism it
// uses, because the two backends genuinely differ.
func TestAccountsRouteNamesBothNamespaces(t *testing.T) {
	host := newScriptedHost()
	m := newTestManager(t, host)
	resp, err := m.handleManagementRequest(pluginapi.ManagementRequest{Method: http.MethodGet, Path: accountsPath})
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	var doc struct {
		Backends []struct {
			Namespace string `json:"namespace"`
			Import    string `json:"import"`
		} `json:"backends"`
	}
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Backends) != 2 {
		t.Fatalf("backends = %+v, want both namespaces", doc.Backends)
	}
	imports := map[string]string{}
	for _, section := range doc.Backends {
		imports[section.Namespace] = section.Import
	}
	// AI Studio adds accounts with a browser, gemini-web with a pasted Cookie.
	// Reporting the same mechanism for both would be the lie this split fixes.
	if imports[config.NamespaceAIStudio] != "browser" {
		t.Fatalf("ai-studio import mechanism = %q, want browser", imports[config.NamespaceAIStudio])
	}
	if imports[config.NamespaceGeminiWeb] != "cookie" {
		t.Fatalf("gemini-web import mechanism = %q, want cookie", imports[config.NamespaceGeminiWeb])
	}
}

// TestBackendRoutesRequireANamespace: acting on the wrong backend is the failure
// this split exists to prevent, so a route that needs a namespace refuses rather
// than guessing.
func TestBackendRoutesRequireANamespace(t *testing.T) {
	host := newScriptedHost()
	m := newTestManager(t, host)
	for _, path := range []string{backendPath + "/install", backendPath + "/start", backendPath + "/stop"} {
		resp, err := m.handleManagementRequest(pluginapi.ManagementRequest{Method: http.MethodPost, Path: path})
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400 for a missing namespace", path, resp.StatusCode)
		}
		if !strings.Contains(string(resp.Body), "namespace") {
			t.Fatalf("%s refusal does not name the missing selector: %s", path, resp.Body)
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
