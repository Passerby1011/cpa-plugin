package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// goCliBody reads the /alpha/generate envelope body the plugin sent.
func goCliBody(t *testing.T, wire map[string]any) map[string]any {
	t.Helper()
	raw := wireBody(t, wire, "body")
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope decode: %v (%s)", err, raw)
	}
	return env
}

// TestGoCliSendsFullDeviceHeaderSet pins the wired header set end to end: the
// header-level unit tests prove GenerateHeadersWithOptions builds the right map,
// but only this test proves the executor actually CALLS it with the device
// config instead of the old two-argument form.
func TestGoCliSendsFullDeviceHeaderSet(t *testing.T) {
	yamlText := "accounts:\n" +
		"  - label: go\n" +
		"    mode: go-cli\n" +
		"    credential: " + goAccountKey + "\n" +
		"  - label: main\n" +
		"    credential: " + providerAccountKey + "\n"

	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	f.responder = wrapWithCatalog(testCatalogJSON, func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		wire := decodePayload(t, capturedCall{payload: payload})
		url, _ := wire["url"].(string)
		if strings.HasSuffix(url, "/alpha/generate") {
			return hostOK(pluginapi.HTTPResponse{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       []byte(cliGoStream),
			}), nil
		}
		return hostOK(map[string]any{}), nil
	})
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(yamlText)); err != nil {
		t.Fatalf("register: %v", err)
	}

	env := mustExecute(t, m, "commandcode/glm-5.3", "openai", []byte(ccRequestBody))
	if !env.OK || env.Error != nil {
		t.Fatalf("execute envelope error: %+v", env.Error)
	}

	wire := lastWire(t, f, pluginabi.MethodHostHTTPDo)
	if got := wire["url"].(string); !strings.HasSuffix(got, "/alpha/generate") {
		t.Fatalf("bridged url = %v, want .../alpha/generate", got)
	}
	headers := wire["headers"].(map[string]any)

	// The wire shape the vendor CLI sends. A header that silently stops being
	// sent is invisible until the upstream reacts to it, so assert presence
	// explicitly rather than only checking the values that happen to be there.
	for _, name := range []string{
		"Content-Type", "User-Agent", "X-Command-Code-Version", "X-Cli-Environment",
		"X-Project-Slug", "X-Taste-Learning", "Authorization", "Traceparent", "X-Session-Id",
	} {
		if _, ok := headers[name]; !ok {
			t.Errorf("go-cli request is missing the %s header; have %v", name, headerNames(headers))
		}
	}
	if got, _ := headers["User-Agent"].([]any)[0].(string); got != "cli" {
		t.Errorf("User-Agent = %q, want cli", got)
	}
	if got, _ := headers["X-Cli-Environment"].([]any)[0].(string); got != "production" {
		t.Errorf("x-cli-environment = %q, want production", got)
	}
	if got, _ := headers["X-Taste-Learning"].([]any)[0].(string); got != "false" {
		t.Errorf("x-taste-learning = %q, want false", got)
	}
	slug, _ := headers["X-Project-Slug"].([]any)[0].(string)
	if slug != "c-users-dev-projects-app" {
		t.Errorf("x-project-slug = %q, want the slug of the default project dir", slug)
	}
	if tp, _ := headers["Traceparent"].([]any)[0].(string); !strings.HasPrefix(tp, "00-") {
		t.Errorf("traceparent = %q, want a W3C traceparent", tp)
	}
}

// TestGoCliEnvelopeEnvironmentMatchesDeviceProfile pins the self-consistency
// contract: the envelope's config.workingDir/environment must be derived from
// the same device profile as the identity layer. A "linux-x64" envelope beside
// a win32 fingerprint is precisely the contradiction the shared profile exists
// to prevent.
func TestGoCliEnvelopeEnvironmentMatchesDeviceProfile(t *testing.T) {
	yamlText := "accounts:\n" +
		"  - label: go\n" +
		"    mode: go-cli\n" +
		"    credential: " + goAccountKey + "\n" +
		"  - label: main\n" +
		"    credential: " + providerAccountKey + "\n"

	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	f.responder = wrapWithCatalog(testCatalogJSON, func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		wire := decodePayload(t, capturedCall{payload: payload})
		if url, _ := wire["url"].(string); strings.HasSuffix(url, "/alpha/generate") {
			return hostOK(pluginapi.HTTPResponse{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       []byte(cliGoStream),
			}), nil
		}
		return hostOK(map[string]any{}), nil
	})
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(yamlText)); err != nil {
		t.Fatalf("register: %v", err)
	}
	if env := mustExecute(t, m, "commandcode/glm-5.3", "openai", []byte(ccRequestBody)); !env.OK {
		t.Fatalf("execute failed: %+v", env.Error)
	}

	wire := lastWire(t, f, pluginabi.MethodHostHTTPDo)
	env := goCliBody(t, wire)
	cfg, _ := env["config"].(map[string]any)
	if cfg == nil {
		t.Fatalf("envelope has no config block: %v", env)
	}
	if got, _ := cfg["environment"].(string); got != "win32" {
		t.Errorf("config.environment = %q, want win32 (the device profile's platform)", got)
	}
	if got, _ := cfg["workingDir"].(string); got != `C:\Users\dev\projects\app` {
		t.Errorf("config.workingDir = %q, want the fabricated project dir", got)
	}
}

// TestDeviceConfigDisablesIdentityLayer pins the off switch: with device.enabled
// false the header set drops the fabricated slug while keeping the transport
// headers, so an operator can fall back to the pre-0.3.0 behaviour.
func TestDeviceConfigDisablesIdentityLayer(t *testing.T) {
	yamlText := "device:\n" +
		"  enabled: false\n" +
		"accounts:\n" +
		"  - label: go\n" +
		"    mode: go-cli\n" +
		"    credential: " + goAccountKey + "\n" +
		"  - label: main\n" +
		"    credential: " + providerAccountKey + "\n"

	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	f.responder = wrapWithCatalog(testCatalogJSON, func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		wire := decodePayload(t, capturedCall{payload: payload})
		if url, _ := wire["url"].(string); strings.HasSuffix(url, "/alpha/generate") {
			return hostOK(pluginapi.HTTPResponse{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       []byte(cliGoStream),
			}), nil
		}
		return hostOK(map[string]any{}), nil
	})
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(yamlText)); err != nil {
		t.Fatalf("register: %v", err)
	}
	if env := mustExecute(t, m, "commandcode/glm-5.3", "openai", []byte(ccRequestBody)); !env.OK {
		t.Fatalf("execute failed: %+v", env.Error)
	}

	wire := lastWire(t, f, pluginabi.MethodHostHTTPDo)
	combined, _ := json.Marshal(wire)
	if strings.Contains(string(combined), "c-users-dev-projects-app") {
		t.Errorf("device layer stayed on with device.enabled=false: %s", combined)
	}
}

// TestDeviceConfigFieldExposed pins the WebUI surface: without the field an
// operator cannot configure the identity layer from the Management Center.
func TestDeviceConfigFieldExposed(t *testing.T) {
	byName := map[string]pluginapi.ConfigField{}
	for _, f := range configFields() {
		byName[f.Name] = f
	}
	field, ok := byName["device"]
	if !ok {
		t.Fatalf("config field \"device\" missing; have %v", headerNames(nil))
	}
	if field.Type != pluginapi.ConfigFieldTypeObject {
		t.Errorf("device field type = %v, want Object (it decodes into a YAML mapping)", field.Type)
	}
}

// headerNames renders a captured wire header map's names for failure messages.
func headerNames(headers map[string]any) []string {
	if headers == nil {
		return nil
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	return names
}
