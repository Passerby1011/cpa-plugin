package plugin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// cliGoStream is a minimal /alpha/generate SSE response: one text delta, a
// finish event with usage, then the terminator.
const cliGoStream = "data: {\"type\":\"text-delta\",\"text\":\"hello\"}\n\n" +
	"data: {\"type\":\"finish\",\"finishReason\":\"stop\",\"totalUsage\":{\"inputTokens\":1,\"outputTokens\":2}}\n\n" +
	"data: [DONE]\n\n"

const goAccountKey = "user_go_route_test"

const providerAccountKey = "user_provider_route_test"

// TestExecuteGoCliAccountRoutesToAlphaGenerate pins the whole point of the
// go-cli transport: a Go-plan credential must be sent to /alpha/generate with
// the CLI headers, never to the Provider API that refuses it.
func TestExecuteGoCliAccountRoutesToAlphaGenerate(t *testing.T) {
	// Two accounts: the go-cli one must serve the call, and a provider
	// account is required because only a provider credential can fetch the
	// /models catalog — the Provider API is exactly what Go plans cannot reach.
	// The go-cli entry is first so sticky selection picks it for the call.
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
		// The device identity layer announces itself on the account surface
		// before the first generate. Those calls are their own endpoints, not
		// go-cli turns, so this responder accepts them without touching the
		// /alpha/generate expectations below.
		if isDeviceAnnounceURL(url) {
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{}`)}), nil
		}
		t.Errorf("go-cli account hit unexpected upstream url %q", url)
		return hostErr("test", "unrouted"), nil
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
	if got := wire["url"].(string); strings.Contains(got, "/provider/v1") {
		t.Fatalf("go-cli account must not hit the provider surface: %v", got)
	}
	headers := wire["headers"].(map[string]any)
	if got := headers["Authorization"].([]any)[0].(string); got != "Bearer "+goAccountKey {
		t.Fatalf("Authorization = %q, want the pool credential", got)
	}
	if _, ok := headers["X-Command-Code-Version"]; !ok {
		t.Fatalf("x-command-code-version missing: %v", headers)
	}
	if got, _ := headers["User-Agent"].([]any)[0].(string); got != "cli" {
		t.Fatalf("User-Agent = %q, want cli", got)
	}

	// The envelope carries the rewritten upstream model id, not the public one.
	body := wireBody(t, wire, "body")
	if !bytes.Contains(body, []byte(`"model":"glm-5.3"`)) {
		t.Fatalf("CLI envelope missing upstream model: %s", body)
	}

	// And the aggregated answer reaches the client.
	var out pluginapi.ExecutorResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("result decode: %v", err)
	}
	if !bytes.Contains(out.Payload, []byte("hello")) {
		t.Fatalf("aggregated payload lost the text: %s", out.Payload)
	}
}

// TestConfigFieldsExposeAccountsAndPool pins the WebUI surface for the pool
// model: without these two fields a shop-installed plugin could not be
// configured at all (the 0.1.2 dead-loop, again).
func TestConfigFieldsExposeAccountsAndPool(t *testing.T) {
	byName := map[string]bool{}
	for _, f := range configFields() {
		byName[f.Name] = true
	}
	for _, want := range []string{"accounts", "pool", "api-keys"} {
		if !byName[want] {
			t.Errorf("config field %q missing; have %v", want, byName)
		}
	}
}

// TestExecuteProviderAccountStillUsesProviderAPI is the control: a
// provider-mode account keeps hitting the documented Provider API.
func TestExecuteProviderAccountStillUsesProviderAPI(t *testing.T) {
	yamlText := "accounts:\n" +
		"  - credential: " + goAccountKey + "\n"

	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	f.responder = wrapWithCatalog(testCatalogJSON, upstreamRouter(t, map[string]string{
		"/v1/chat/completions": ccResponseBody,
	}))
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(yamlText)); err != nil {
		t.Fatalf("register: %v", err)
	}

	env := mustExecute(t, m, "commandcode/glm-5.3", "openai", []byte(ccRequestBody))
	if !env.OK || env.Error != nil {
		t.Fatalf("execute envelope error: %+v", env.Error)
	}
	wire := lastWire(t, f, pluginabi.MethodHostHTTPDo)
	if got := wire["url"].(string); got != "https://api.commandcode.ai/provider/v1/chat/completions" {
		t.Fatalf("provider account url = %v", got)
	}
	if got := wire["headers"].(map[string]any)["Authorization"].([]any)[0].(string); got != "Bearer "+goAccountKey {
		t.Fatalf("Authorization = %q", got)
	}
}
