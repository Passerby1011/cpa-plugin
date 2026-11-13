package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// decodeTestEnvelope unmarshals a dispatcher envelope for assertions.
func decodeTestEnvelope(t *testing.T, raw []byte) pluginabiEnvelope {
	t.Helper()
	var env pluginabiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	return env
}

// pluginabiEnvelope is the subset of the host envelope these tests read.
type pluginabiEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// registrationPayload is what management.register answers.
type registrationPayload struct {
	Routes []struct {
		Method string `json:"method"`
		Path   string `json:"path"`
	} `json:"routes"`
	Resources []struct {
		Path string `json:"path"`
		Menu string `json:"menu"`
	} `json:"resources"`
}

func registerForTest(t *testing.T, m *Manager, basePath string) registrationPayload {
	t.Helper()
	env := decodeTestEnvelope(t, mustRegister(t, m, basePath))
	if !env.OK {
		t.Fatalf("management register failed: %+v", env.Error)
	}
	var reg registrationPayload
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatalf("registration payload: %v", err)
	}
	return reg
}

func mustRegister(t *testing.T, m *Manager, basePath string) []byte {
	t.Helper()
	body, _ := json.Marshal(pluginapi.ManagementRegistrationRequest{BasePath: basePath})
	raw, err := m.HandleCall("management.register", body)
	if err != nil {
		t.Fatalf("management register: %v", err)
	}
	return raw
}

// A declared resource path of "/" is silently DROPPED by the host: it trims a
// trailing slash and then refuses the empty result. That would leave the panel
// unreachable while the plugin still believes it published one, so no declared
// resource path may resolve to "" once trimmed.
func TestDeclaredResourcePathsSurviveTheHostsTrimming(t *testing.T) {
	m := NewManager(nil)
	reg := registerForTest(t, m, "/v0/management")
	if len(reg.Resources) == 0 {
		t.Fatalf("the panel resource route must be declared")
	}
	for _, r := range reg.Resources {
		trimmed := strings.TrimRight(strings.TrimSpace(r.Path), "/")
		if trimmed == "" {
			t.Errorf("resource path %q resolves to an empty path, which the host drops", r.Path)
		}
		if !strings.HasPrefix(trimmed, "/") {
			t.Errorf("resource path %q must be absolute (leading slash)", r.Path)
		}
	}
}

// The panel is served at the declared resource route, with the management base
// substituted in, so the page's requests reach the plugin's own routes.
//
// The base is registered as a NON-default value on purpose: asserting the
// default literal would pass even if the page hardcoded "/v0/management", which
// is exactly the bug a moved management mount would expose.
func TestPanelIsServedAtItsDeclaredRoute(t *testing.T) {
	m := NewManager(nil)
	reg := registerForTest(t, m, "/v0/control")
	if len(reg.Resources) != 1 {
		t.Fatalf("resources = %+v", reg.Resources)
	}
	resourcePath := reg.Resources[0].Path

	body, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/" + ProviderID + resourcePath,
	})
	raw, err := m.HandleCall("management.handle", body)
	if err != nil {
		t.Fatalf("management handle: %v", err)
	}
	env := decodeTestEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("panel request failed: %+v", env.Error)
	}
	var resp managementResponseBody
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if got := resp.Headers["Content-Type"]; len(got) == 0 || got[0] != "text/html; charset=utf-8" {
		t.Errorf("content-type = %v", got)
	}
	page := string(resp.Body)
	if !strings.Contains(page, "<html") {
		t.Errorf("the panel body is not a page: %q", page[:min(len(page), 60)])
	}
	// The placeholder must be substituted: an unsubstituted page would fetch
	// "__MANAGEMENT_BASE__/status" and 404 on every request.
	if strings.Contains(page, "__MANAGEMENT_BASE__") {
		t.Errorf("the panel still carries the base placeholder")
	}
	// The base the page carries must be the one the host INJECTED, not a default
	// this plugin assumed. The host mounted management at /v0/control here.
	if !strings.Contains(page, `/v0/control/plugins/`+ProviderID) {
		t.Errorf("the panel does not carry the host-injected management base")
	}
	if strings.Contains(page, `/v0/management/plugins/`) {
		t.Errorf("the panel hardcodes the default management base")
	}
}

// The two JSON routes answer at the host-injected base, not a hardcoded one: a
// host that mounts management elsewhere must still reach them.
func TestJSONRoutesHonourTheHostsBasePath(t *testing.T) {
	m := NewManager(nil)
	mustRegister(t, m, "/v0/control") // a host that mounts management elsewhere

	for _, tc := range []struct{ suffix, want string }{
		{"/status", "accounts"},
		{"/models", "models"},
	} {
		body, _ := json.Marshal(pluginapi.ManagementRequest{
			Method: http.MethodGet,
			Path:   "/v0/control/plugins/" + ProviderID + tc.suffix,
		})
		raw, err := m.HandleCall("management.handle", body)
		if err != nil {
			t.Fatalf("%s: %v", tc.suffix, err)
		}
		env := decodeTestEnvelope(t, raw)
		if !env.OK {
			t.Fatalf("%s: %+v", tc.suffix, env.Error)
		}
		if !strings.Contains(string(env.Result), tc.want) {
			t.Errorf("%s: result %q does not carry %q", tc.suffix, env.Result, tc.want)
		}
	}
	// The old base must not answer: a route that ignores the injected base is
	// how a moved host silently 404s everything.
	body, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/" + ProviderID + "/status"})
	raw, _ := m.HandleCall("management.handle", body)
	env := decodeTestEnvelope(t, raw)
	var resp managementResponseBody
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("response: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status on the stale base = %d, want 404", resp.StatusCode)
	}
}

// An unknown path answers a stated 404 rather than an empty 200, which a page
// would render as a successful payload with no data.
func TestUnknownPathIs404(t *testing.T) {
	m := NewManager(nil)
	mustRegister(t, m, "/v0/management")
	body, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/" + ProviderID + "/nope"})
	raw, _ := m.HandleCall("management.handle", body)
	env := decodeTestEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("envelope: %+v", env.Error)
	}
	var resp managementResponseBody
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("response: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// The panel is a GET-only resource: a POST to it is refused instead of rendering
// a page to a write.
func TestPanelRefusesNonGET(t *testing.T) {
	m := NewManager(nil)
	mustRegister(t, m, "/v0/management")
	body, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/resource/plugins/" + ProviderID + panelResourcePath})
	raw, _ := m.HandleCall("management.handle", body)
	env := decodeTestEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("envelope: %+v", env.Error)
	}
	var resp managementResponseBody
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("response: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

// Every diagnostics answer is key-free: the status view and the model list reach
// a browser, so a credential appearing in either would be a leak.
func TestDiagnosticsNeverCarryAKey(t *testing.T) {
	m := NewManager(nil)
	body, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte(
		"api-keys:\n  - value: \"sk-amr-the-runtime-key\"\n    control: \"vela_ctrl_the-control-key\"\n")})
	if _, err := m.HandleCall("plugin.register", body); err != nil {
		t.Fatalf("register: %v", err)
	}
	mustRegister(t, m, "/v0/management")

	for _, suffix := range []string{"/status", "/models"} {
		req, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/" + ProviderID + suffix})
		raw, err := m.HandleCall("management.handle", req)
		if err != nil {
			t.Fatalf("%s: %v", suffix, err)
		}
		env := decodeTestEnvelope(t, raw)
		if !env.OK {
			t.Fatalf("%s: %+v", suffix, env.Error)
		}
		payload := string(env.Result)
		for _, secret := range []string{"sk-amr-the-runtime-key", "vela_ctrl_the-control-key"} {
			if strings.Contains(payload, secret) {
				t.Errorf("%s leaked a key: %s", suffix, payload)
			}
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
