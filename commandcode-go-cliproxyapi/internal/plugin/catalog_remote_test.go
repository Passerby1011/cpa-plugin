package plugin

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// realReferenceSnippet is a verbatim slice of MAXeaglet/commandcode-proxy's
// MODELS table, including the surrounding prose that must NOT be mistaken for
// model ids. Using the real shape (not an invented one) is the point: the
// extractor's job is to survive the actual file.
const realReferenceSnippet = `
// ── 模型列表 ───────────────────────────────────────
const MODELS = [
  // Anthropic
  { id: 'claude-sonnet-4-6', name: 'Claude Sonnet 4.6' },
  { id: 'claude-opus-4-8', name: 'Claude Opus 4.8' },
  { id: 'claude-haiku-4-5-20251001', name: 'Claude Haiku 4.5' },
  // OpenAI
  { id: 'gpt-5.5', name: 'GPT-5.5' },
  { id: 'gpt-5.4-mini', name: 'GPT-5.4 Mini' },
  // DeepSeek
  { id: 'deepseek/deepseek-v4-pro', name: 'DeepSeek V4 Pro' },
  { id: 'deepseek/deepseek-v4-flash', name: 'DeepSeek V4 Flash' },
  // Qwen
  { id: 'Qwen/Qwen3.7-Max', name: 'Qwen 3.7 Max' },
];
`

// TestParseRemoteModelIDsOnRealReferenceShape checks the extractor against the
// real file's shape, and that structure words and prose are not published as
// models.
func TestParseRemoteModelIDsOnRealReferenceShape(t *testing.T) {
	ids := parseRemoteModelIDs(realReferenceSnippet)
	want := map[string]bool{
		"claude-sonnet-4-6":          true,
		"claude-opus-4-8":            true,
		"claude-haiku-4-5-20251001":  true,
		"gpt-5.5":                    true,
		"gpt-5.4-mini":               true,
		"deepseek/deepseek-v4-pro":   true,
		"deepseek/deepseek-v4-flash": true,
		"Qwen/Qwen3.7-Max":           true,
	}
	if len(ids) != len(want) {
		t.Fatalf("extracted %d ids, want %d: %v", len(ids), len(want), ids)
	}
	for _, id := range ids {
		if !want[id] {
			t.Errorf("unexpected id extracted: %q", id)
		}
	}
	// Sorted output keeps the published list stable across fetches.
	for i := 1; i < len(ids); i++ {
		if ids[i-1] > ids[i] {
			t.Errorf("ids not sorted: %v", ids)
			break
		}
	}
}

// TestParseRemoteModelIDsRejectsNonModels pins the filter: the extractor must
// not publish field names or prose as models just because they appear next to
// an `id:` key.
func TestParseRemoteModelIDsRejectsNonModels(t *testing.T) {
	src := `
{"id": "name", "name": "just a label"}
{ id: 'id', name: 'x' }
{ id: 'some random sentence', name: 'y' }
{ id: 'type', name: 'z' }
`
	if got := parseRemoteModelIDs(src); len(got) != 0 {
		t.Errorf("non-model strings were published as models: %v", got)
	}
}

// TestParseRemoteModelIDsAcceptsJSONShape pins that a JSON-shaped source works
// too, so the feature does not silently break if the source file is re-emitted
// as data rather than as JS literals.
func TestParseRemoteModelIDsAcceptsJSONShape(t *testing.T) {
	src := `{"models":[{"id":"claude-sonnet-4-6"},{"id":"deepseek/deepseek-v4-pro"}]}`
	ids := parseRemoteModelIDs(src)
	if len(ids) != 2 {
		t.Fatalf("JSON-shaped source extracted %d ids, want 2: %v", len(ids), ids)
	}
}

// TestRemoteCatalogIsUsedForGoCliOnlyPool is the integration test for the
// feature: a go-cli-only pool with a remote source publishes the models from
// that source even though /models is unreachable.
func TestRemoteCatalogIsUsedForGoCliOnlyPool(t *testing.T) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	f.responder = func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		wire := decodePayload(t, capturedCall{payload: payload})
		url, _ := wire["url"].(string)
		if strings.HasSuffix(url, "/remote-models.mjs") {
			return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(realReferenceSnippet)}), nil
		}
		if isDeviceAnnounceURL(url) {
			return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{}`)}), nil
		}
		// /models must NOT be required: this is a go-cli-only pool.
		if strings.Contains(url, "/models") {
			t.Errorf("go-cli-only pool must not be asked to fetch /models: %s", url)
		}
		return hostErr("test", "unrouted"), nil
	}

	yaml := "catalog:\n" +
		"  remote: \"https://example.test/remote-models.mjs\"\n" +
		"accounts:\n" +
		"  - label: go\n" +
		"    mode: go-cli\n" +
		"    credential: " + retryAccountKey + "\n"

	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(yaml)); err != nil {
		t.Fatalf("register: %v", err)
	}

	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", []byte("{}")), &static)
	if len(static.Models) == 0 {
		t.Fatal("remote source configured but no models were published")
	}
	found := false
	for _, mi := range static.Models {
		if mi.ID == "commandcode/deepseek/deepseek-v4-pro" {
			found = true
		}
	}
	if !found {
		ids := make([]string, 0, len(static.Models))
		for _, mi := range static.Models {
			ids = append(ids, mi.ID)
		}
		t.Fatalf("remote model missing from the published list: %v", ids)
	}
}

// TestRemoteCatalogFailureFallsBackToStatic pins the fallback floor: when the
// remote source is unreachable the static list still publishes, so a CDN outage
// cannot leave the deployment with no models.
func TestRemoteCatalogFailureFallsBackToStatic(t *testing.T) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	f.responder = func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		wire := decodePayload(t, capturedCall{payload: payload})
		url, _ := wire["url"].(string)
		if strings.HasSuffix(url, "/remote-models.mjs") {
			return hostOK(pluginapi.HTTPResponse{StatusCode: 500, Body: []byte("boom")}), nil
		}
		if isDeviceAnnounceURL(url) {
			return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{}`)}), nil
		}
		return hostErr("test", "unrouted"), nil
	}

	yaml := "catalog:\n" +
		"  remote: \"https://example.test/remote-models.mjs\"\n" +
		"  static:\n" +
		"    - claude-sonnet-4-6\n" +
		"accounts:\n" +
		"  - label: go\n" +
		"    mode: go-cli\n" +
		"    credential: " + retryAccountKey + "\n"

	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(yaml)); err != nil {
		t.Fatalf("register: %v", err)
	}

	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", []byte("{}")), &static)
	if len(static.Models) == 0 {
		t.Fatal("a failed remote source must fall back to the static list")
	}
}

// TestRemoteCatalogFailureKeepsLastGood pins the anti-blanking rule: once a
// fetch has succeeded, a later failure must keep serving the previous list
// rather than dropping every model.
func TestRemoteCatalogFailureKeepsLastGood(t *testing.T) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	fail := false
	f.responder = func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		wire := decodePayload(t, capturedCall{payload: payload})
		url, _ := wire["url"].(string)
		if strings.HasSuffix(url, "/remote-models.mjs") {
			if fail {
				return hostOK(pluginapi.HTTPResponse{StatusCode: 503, Body: []byte("down")}), nil
			}
			return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(realReferenceSnippet)}), nil
		}
		if isDeviceAnnounceURL(url) {
			return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{}`)}), nil
		}
		return hostErr("test", "unrouted"), nil
	}

	yaml := "catalog:\n" +
		"  remote: \"https://example.test/remote-models.mjs\"\n" +
		"accounts:\n" +
		"  - label: go\n" +
		"    mode: go-cli\n" +
		"    credential: " + retryAccountKey + "\n"

	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(yaml)); err != nil {
		t.Fatalf("register: %v", err)
	}
	var first pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", []byte("{}")), &first)
	if len(first.Models) == 0 {
		t.Fatal("first fetch should have published models")
	}

	// Now the source goes down and a refresh runs. Bypass the memo interval by
	// calling the fetcher directly with no minimum interval.
	fail = true
	ids, err := m.fetchRemoteCatalog(t.Context(), "https://example.test/remote-models.mjs", 0)
	if err == nil {
		t.Fatal("a failing source should report an error")
	}
	if len(ids) == 0 {
		t.Fatal("a failed fetch must return the last good list, not an empty one")
	}
	if len(ids) != len(parseRemoteModelIDs(realReferenceSnippet)) {
		t.Errorf("last-good list changed size: %d", len(ids))
	}
}

// TestRedactCatalogURLStripsCredentials pins that a token in the source URL
// never reaches a log line.
func TestRedactCatalogURLStripsCredentials(t *testing.T) {
	cases := map[string]string{
		"https://example.test/models.mjs":                 "https://example.test/models.mjs",
		"https://example.test/models.mjs?token=SECRET":    "https://example.test/models.mjs",
		"https://user:SECRET@example.test/models.mjs":     "https://example.test/models.mjs",
		"https://user:SECRET@example.test/models.mjs?t=X": "https://example.test/models.mjs",
	}
	for in, want := range cases {
		if got := redactCatalogURL(in); got != want {
			t.Errorf("redactCatalogURL(%q) = %q, want %q", in, got, want)
		}
	}
}
