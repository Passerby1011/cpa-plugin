package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/commandcode/internal/catalog"
	"github.com/hex-ci/cpa-plugin/commandcode/internal/config"
)

// ---- test doubles -------------------------------------------------------

const (
	testKey         = "sk-test-secret-1"
	testCatalogJSON = `{"data":[{"id":"glm-5.3"}]}`
	testValidYAML   = "api-keys:\n  - value: " + testKey + "\n"
	// testRouteOverrides pins the fixture models onto the messages and
	// responses routes: CommandCode serves OSS models on chat/completions
	// only, so every other upstream leg must be requested explicitly.
	testRouteOverrides = "route-overrides:\n" +
		"  minimax-m3:\n    protocol: messages\n    endpoint: /v1/messages\n" +
		"  gpt-5.6-luna:\n    protocol: responses\n    endpoint: /v1/responses\n"
	dummyKey     = "sk-test"
	dummyKeyYAML = "api-keys:\n  - value: " + dummyKey + "\n"
)

type capturedCall struct {
	method  string
	payload []byte
}

type fakeCaller struct {
	mu        sync.Mutex
	calls     []capturedCall
	responder func(method string, payload []byte) ([]byte, error)
	authFiles map[string]struct{}
}

func (f *fakeCaller) call(method string, payload []byte) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, capturedCall{method: method, payload: payload})
	f.mu.Unlock()
	var raw []byte
	var err error
	if f.responder != nil {
		raw, err = f.responder(method, payload)
	} else {
		raw = hostOK(map[string]any{})
	}
	if method == pluginabi.MethodHostAuthList && err == nil && !hasExplicitAuthList(raw) {
		return f.authListResponse(), nil
	}
	if method == pluginabi.MethodHostAuthSave && err == nil && hostEnvelopeOK(raw) {
		var req pluginapi.HostAuthSaveRequest
		if json.Unmarshal(payload, &req) == nil && strings.TrimSpace(req.Name) != "" {
			f.mu.Lock()
			if f.authFiles == nil {
				f.authFiles = make(map[string]struct{})
			}
			f.authFiles[req.Name] = struct{}{}
			f.mu.Unlock()
		}
	}
	return raw, err
}

// savedAuthFiles returns the set of auth file names the host was asked to
// persist. Distinct names for one credential are exactly the duplicate-record
// bug this guards against, so callers assert on names, not call counts.
func (f *fakeCaller) savedAuthFiles() map[string]struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]struct{}, len(f.authFiles))
	for name := range f.authFiles {
		out[name] = struct{}{}
	}
	return out
}

func hostEnvelopeOK(raw []byte) bool {
	var env pluginabi.Envelope
	return json.Unmarshal(raw, &env) == nil && env.OK
}

func hasExplicitAuthList(raw []byte) bool {
	var env pluginabi.Envelope
	if json.Unmarshal(raw, &env) != nil || !env.OK {
		return true
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(env.Result, &result) != nil {
		return true
	}
	_, ok := result["files"]
	return ok
}

func (f *fakeCaller) authListResponse() []byte {
	f.mu.Lock()
	files := make([]pluginapi.HostAuthFileEntry, 0, len(f.authFiles))
	for name := range f.authFiles {
		files = append(files, pluginapi.HostAuthFileEntry{
			ID: strings.TrimSuffix(name, ".json"), Name: name, Source: "file", Path: name,
		})
	}
	f.mu.Unlock()
	return hostOK(hostAuthListResponse{Files: files})
}

func (f *fakeCaller) recorded() []capturedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]capturedCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeCaller) callsOf(method string) []capturedCall {
	var out []capturedCall
	for _, c := range f.recorded() {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

func hostOK(result any) []byte {
	raw, _ := json.Marshal(result)
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return out
}

func hostErr(code, msg string) []byte {
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: msg}})
	return out
}

// catalogResponder answers host.http.do with a canned upstream response.
func catalogResponder(success bool, body string) func(string, []byte) ([]byte, error) {
	return func(method string, _ []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		if !success {
			return hostErr("upstream_down", "simulated upstream failure"), nil
		}
		return hostOK(pluginapi.HTTPResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(body),
		}), nil
	}
}

func newTestManager(responder func(string, []byte) ([]byte, error)) (*Manager, *fakeCaller) {
	f := &fakeCaller{responder: responder}
	return NewManager(NewHostBridge(f.call)), f
}

func lifecycleRequestBody(yamlText string) []byte {
	b, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte(yamlText)})
	return b
}

func decodeEnv(t *testing.T, raw []byte) pluginabi.Envelope {
	t.Helper()
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, raw)
	}
	return env
}

func decodeResult(t *testing.T, raw []byte, v any) {
	t.Helper()
	env := decodeEnv(t, raw)
	if !env.OK || env.Error != nil {
		t.Fatalf("expected OK envelope, got %s", raw)
	}
	if err := json.Unmarshal(env.Result, v); err != nil {
		t.Fatalf("decode result: %v (%s)", err, env.Result)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// installFastLoop rebuilds lifecycle state around a 5ms ticker. Validated
// configs floor catalog.refresh-interval at 1m, so tick-driven tests cannot
// obtain an observable interval through register/reconfigure; they install
// the production loop (startRefreshLoop) directly instead.
func installFastLoop(t *testing.T, m *Manager, yamlText string) {
	t.Helper()
	cfg, err := config.Load([]byte(yamlText))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if oldDone := m.closeStop(); oldDone != nil {
		<-oldDone
	}
	mgr := catalog.New(cfg, m.bridge)
	m.mu.Lock()
	m.cfg, m.mgr = cfg, mgr
	stop, done := make(chan struct{}), make(chan struct{})
	m.stop, m.done = stop, done
	m.mu.Unlock()
	m.startRefreshLoop(cfg, mgr, 5*time.Millisecond, stop, done)
}

// manualTick runs one tick body (refreshOnce) over the currently served
// state — exactly the work a background tick performs — without waiting out
// the 1m config floor on refresh-interval.
func manualTick(t *testing.T, m *Manager) {
	t.Helper()
	m.mu.RLock()
	mgr, cfg := m.mgr, m.cfg
	m.mu.RUnlock()
	if mgr == nil {
		t.Fatal("no served manager to tick")
	}
	if err := refreshOnce(context.Background(), mgr, m.bridge, time.Second, cfg); err != nil {
		t.Fatalf("tick refresh: %v", err)
	}
}

// ---- HostBridge ---------------------------------------------------------

func TestBridgeDoRoundTrip(t *testing.T) {
	var got map[string]any
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			t.Errorf("method = %q, want host.http.do", method)
		}
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("payload not json: %v", err)
		}
		return hostOK(pluginapi.HTTPResponse{
			StatusCode: http.StatusCreated,
			Headers:    http.Header{"X-Lane": []string{"fast"}},
			Body:       []byte("payload-bytes"),
		}), nil
	}}
	bridge := NewHostBridge(f.call)
	req := pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     "https://upstream.test/v1/models",
		Headers: http.Header{"Authorization": []string{"Bearer " + testKey}},
		Body:    []byte(`{"a":1}`),
	}
	resp, err := bridge.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != http.StatusCreated || string(resp.Body) != "payload-bytes" ||
		!reflect.DeepEqual(resp.Headers.Get("X-Lane"), "fast") {
		t.Fatalf("decoded response wrong: %+v", resp)
	}
	if got["method"] != http.MethodGet || got["url"] != "https://upstream.test/v1/models" {
		t.Fatalf("wire payload wrong: %v", got)
	}
	headers, _ := got["headers"].(map[string]any)
	if headers["Authorization"].([]any)[0].(string) != "Bearer "+testKey {
		t.Fatalf("Authorization header missing: %v", headers)
	}
	if got["body"] != "eyJhIjoxfQ==" { // std base64 padding of {"a":1}
		t.Fatalf("body encoding wrong: %v", got["body"])
	}
}

func TestBridgeDoFailures(t *testing.T) {
	cases := []struct {
		name     string
		respond  func(string, []byte) ([]byte, error)
		wantSub  string
		zeroResp bool
	}{
		{"caller error", func(string, []byte) ([]byte, error) { return nil, errors.New("boom") }, "boom", true},
		{"undecodable envelope", func(string, []byte) ([]byte, error) { return []byte("not-json"), nil }, "undecodable host response", true},
		{"envelope error with message", func(string, []byte) ([]byte, error) { return hostErr("denied", "nope"), nil }, "nope", true},
		{"envelope error without object", func(string, []byte) ([]byte, error) { return []byte(`{"ok":false}`), nil }, "host http do failed:", true},
		{"malformed result", func(string, []byte) ([]byte, error) { return []byte(`{"ok":true,"result":"{bad"}`), nil }, "undecodable response body", true},
		{"empty result decodes zero response", func(string, []byte) ([]byte, error) { return []byte(`{"ok":true}`), nil }, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bridge := NewHostBridge((&fakeCaller{responder: tc.respond}).call)
			resp, err := bridge.Do(context.Background(), pluginapi.HTTPRequest{})
			if tc.zeroResp {
				if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
					t.Fatalf("err = %v, want substring %q", err, tc.wantSub)
				}
				if resp.StatusCode != 0 || resp.Body != nil {
					t.Fatalf("expected zero response, got %+v", resp)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestBridgeLogSuccess(t *testing.T) {
	var got map[string]any
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostLog {
			t.Errorf("method = %q, want host.log", method)
		}
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("payload not json: %v", err)
		}
		return hostOK(map[string]any{}), nil
	}}
	err := NewHostBridge(f.call).Log("warn", "something happened", map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if got["level"] != "warn" || got["message"] != "something happened" {
		t.Fatalf("log payload wrong: %v", got)
	}
	fields, _ := got["fields"].(map[string]any)
	if fields["k"] != "v" {
		t.Fatalf("fields wrong: %v", fields)
	}
}

func TestBridgeLogFailures(t *testing.T) {
	cases := []struct {
		name    string
		respond func(string, []byte) ([]byte, error)
		wantSub string
	}{
		{"caller error", func(string, []byte) ([]byte, error) { return nil, errors.New("log boom") }, "log boom"},
		{"undecodable envelope", func(string, []byte) ([]byte, error) { return []byte("{"), nil }, "undecodable host response"},
		{"envelope error with message", func(string, []byte) ([]byte, error) { return hostErr("busy", "later"), nil }, "later"},
		{"envelope error without object", func(string, []byte) ([]byte, error) { return []byte(`{"ok":false}`), nil }, "host log failed:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := NewHostBridge((&fakeCaller{responder: tc.respond}).call).Log("warn", "m", nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

func TestBridgeAuthSaveWireAndRedactsFailures(t *testing.T) {
	secret := "sk-auth-save-secret"
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostAuthSave {
			t.Fatalf("method = %q, want %q", method, pluginabi.MethodHostAuthSave)
		}
		var wire pluginapi.HostAuthSaveRequest
		if err := json.Unmarshal(payload, &wire); err != nil {
			t.Fatalf("payload not json: %v", err)
		}
		if wire.Name != "commandcode-key-test.json" || string(wire.JSON) != `{"type":"commandcode","api_key":"`+secret+`"}` {
			t.Fatalf("wire request = %+v", wire)
		}
		return hostOK(pluginapi.HostAuthSaveResponse{Name: wire.Name}), nil
	}}
	if err := NewHostBridge(f.call).AuthSave(context.Background(), pluginapi.HostAuthSaveRequest{
		Name: "commandcode-key-test.json",
		JSON: json.RawMessage(`{"type":"commandcode","api_key":"` + secret + `"}`),
	}); err != nil {
		t.Fatalf("AuthSave: %v", err)
	}

	secretErr := "host rejected " + secret
	err := NewHostBridge((&fakeCaller{responder: func(string, []byte) ([]byte, error) {
		return hostErr("denied", secretErr), nil
	}}).call).AuthSave(context.Background(), pluginapi.HostAuthSaveRequest{Name: "x.json", JSON: []byte(`{}`)})
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "host auth save failed") {
		t.Fatalf("redacted auth error = %v", err)
	}
	err = NewHostBridge((&fakeCaller{responder: func(string, []byte) ([]byte, error) {
		return []byte("not-json"), nil
	}}).call).AuthSave(context.Background(), pluginapi.HostAuthSaveRequest{Name: "x.json", JSON: []byte(`{}`)})
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "undecodable host response") {
		t.Fatalf("malformed auth response = %v", err)
	}
}

func TestBridgeAuthListDecodesEntries(t *testing.T) {
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostAuthList {
			t.Fatalf("method = %q, want %q", method, pluginabi.MethodHostAuthList)
		}
		var req map[string]any
		if err := json.Unmarshal(payload, &req); err != nil {
			t.Fatalf("list payload not json: %v", err)
		}
		return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{{
			ID: "commandcode-key-existing", Name: "commandcode-key-existing.json", Priority: 7,
		}}}), nil
	}}
	entries, err := NewHostBridge(f.call).AuthList(context.Background())
	if err != nil {
		t.Fatalf("AuthList: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "commandcode-key-existing" || entries[0].Name != "commandcode-key-existing.json" || entries[0].Priority != 7 {
		t.Fatalf("auth entries = %+v", entries)
	}
}

// ---- dispatcher: registration ------------------------------------------

func TestRegisterSuccessPublishesModels(t *testing.T) {
	m, f := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var reg registrationResult
	decodeResult(t, resp, &reg)
	if reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", reg.SchemaVersion, pluginabi.SchemaVersion)
	}
	if reg.Metadata.Name != pluginName || reg.Metadata.Version != pluginVersion {
		t.Fatalf("metadata wrong: %+v", reg.Metadata)
	}
	// The Management Center renders plugins.configs.<id> from these
	// declarations, so an empty list means the operator has no way to set
	// api-keys from the UI (the original "registered but not effective"
	// dead end). Assert the key field is advertised.
	if len(reg.Metadata.ConfigFields) == 0 {
		t.Fatalf("no config fields declared: %+v", reg.Metadata)
	}
	var hasKeys bool
	for _, f := range reg.Metadata.ConfigFields {
		if f.Name == "api-keys" {
			hasKeys = true
			if f.Type != pluginapi.ConfigFieldTypeArray {
				t.Fatalf("api-keys field type = %q, want array", f.Type)
			}
			if strings.TrimSpace(f.Description) == "" {
				t.Fatal("api-keys field has no description")
			}
		}
	}
	if !hasKeys {
		t.Fatalf("api-keys config field not declared: %+v", reg.Metadata.ConfigFields)
	}
	if !reg.Capabilities.ModelProvider {
		t.Fatalf("capabilities wrong: %+v", reg.Capabilities)
	}

	calls := f.callsOf(pluginabi.MethodHostHTTPDo)
	if len(calls) != 1 {
		t.Fatalf("host.http.do calls = %d, want 1", len(calls))
	}
	var wire map[string]any
	if err := json.Unmarshal(calls[0].payload, &wire); err != nil {
		t.Fatalf("bridge payload: %v", err)
	}
	if wire["method"] != http.MethodGet {
		t.Fatalf("bridged method = %v, want GET", wire["method"])
	}
	if wire["url"] != "https://api.commandcode.ai/provider/v1/models" {
		t.Fatalf("bridged url = %v", wire["url"])
	}
	headers := wire["headers"].(map[string]any)
	auth := headers["Authorization"].([]any)[0].(string)
	if auth != "Bearer "+testKey {
		t.Fatalf("bridged Authorization = %q", auth)
	}

	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	if static.Provider != ProviderID || len(static.Models) != 1 {
		t.Fatalf("static = %+v", static)
	}
	got := static.Models[0]
	if got.ID != "commandcode/glm-5.3" || got.Object != "model" || got.OwnedBy != ProviderID ||
		got.DisplayName != "glm-5.3" || got.ContextLength != 0 || got.MaxCompletionTokens != 0 ||
		len(got.SupportedInputModalities) != 0 || len(got.SupportedOutputModalities) != 0 {
		t.Fatalf("model info = %+v", got)
	}

	var forAuth pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.for_auth", []byte("{}")), &forAuth)
	if forAuth.Provider != ProviderID || len(forAuth.Models) != 1 || forAuth.Models[0].ID != got.ID {
		t.Fatalf("for_auth = %+v", forAuth)
	}
}

// TestAccountsConfigFieldIsArray pins the host-contract fix for the 0.1.4
// "saved through the WebUI, then the plugin stops working" class of bug.
//
// Basis, from the host source (internal/api/handlers/management/plugins.go):
//
//   - A field's Type is display metadata only. The host's sole reader is
//     pluginConfigFields (plugins.go:466), which forwards string(field.Type)
//     unchanged (plugins.go:471); nothing in the save or readback path inspects
//     or coerces it.
//   - Save path: PutPluginConfig (plugins.go:253) -> readPluginConfigObject
//     (plugins.go:511) -> yamlNodeFromJSONObject (plugins.go:570) ->
//     yamlNodeFromJSONValue, whose case []any (plugins.go:605) builds a
//     yaml.SequenceNode and whose case string (plugins.go:591) would instead
//     freeze the text into a scalar. PatchPluginConfig (plugins.go:281) reaches
//     the same conversion (plugins.go:306). The node is decoded into
//     config.PluginInstanceConfig (pluginInstanceConfigFromNode, plugins.go:559)
//     and survives verbatim in .Raw.
//   - Readback: GetPluginConfig (plugins.go:163) -> pluginConfigJSONObject
//     (plugins.go:547) -> yamlNodeToJSONValue, whose case yaml.SequenceNode
//     (plugins.go:642) returns a JSON array.
//
// So a JSON array submitted for "accounts" is preserved exactly as an array —
// which is what config.rawConfig.Accounts ([]rawAccount, config.go:233) decodes.
// Declaring Object would only steer the WebUI toward an object-shaped
// submission the plugin cannot decode, with no host-side rescue. Both
// "api-keys" and "accounts" carry the same JSON-array shape, so both must say
// Array; this guards against the type drifting away from the documented
// example again.
func TestAccountsConfigFieldIsArray(t *testing.T) {
	t.Parallel()

	fields := configFields()
	var accounts, keys *pluginapi.ConfigField
	for index := range fields {
		field := &fields[index]
		switch field.Name {
		case "accounts":
			accounts = field
		case "api-keys":
			keys = field
		}
	}
	if accounts == nil {
		t.Fatal("accounts config field not declared")
	}
	// The host persists whatever JSON the UI submits with no coercion, so the
	// declared type is the only thing steering the WebUI editor. Array matches
	// config.rawConfig.Accounts ([]rawAccount); Object would invite an object
	// the plugin cannot decode, which is the 0.1.4 outage class.
	if accounts.Type != pluginapi.ConfigFieldTypeArray {
		t.Fatalf("accounts field type = %q, want %q: the host persists the submitted JSON node verbatim "+
			"(yamlNodeFromJSONValue []any -> SequenceNode, plugins.go:605) and the plugin decodes it as "+
			"[]rawAccount (config.go:233), so an Object declaration contradicts the array example in the "+
			"description", accounts.Type, pluginapi.ConfigFieldTypeArray)
	}
	if !strings.Contains(accounts.Description, "JSON array") {
		t.Fatalf("accounts description does not state the array shape: %q", accounts.Description)
	}

	// Same no-coercion rule in the other direction: an Object field reaches
	// config.rawConfig as a YAML mapping decoded via yaml.Unmarshal
	// (config.go:382), which drops unknown keys as well as rejecting a scalar
	// or sequence in their place. Every non-Array object field must therefore
	// document an object example, never a bare array.
	for _, field := range fields {
		if field.Type != pluginapi.ConfigFieldTypeObject {
			continue
		}
		example := strings.TrimSpace(field.Description)
		if !strings.Contains(example, "{") {
			t.Fatalf("object field %q description carries no object-shaped example: %q", field.Name, field.Description)
		}
	}

	// api-keys carries the same array shape through the same host path; if one
	// moves to Array the other must not be left declaring Object.
	if keys == nil {
		t.Fatal("api-keys config field not declared")
	}
	if keys.Type != accounts.Type {
		t.Fatalf("api-keys type = %q, accounts type = %q; both are JSON arrays on the wire", keys.Type, accounts.Type)
	}
}

// ---- pending (unconfigured) registration -------------------------------

// TestRegisterWithoutKeysSucceedsInPendingState pins the fix for the
// store-install dead end: with no api-keys the plugin must still register
// (so the Management Center publishes metadata + config fields and the
// operator can enter a key), must NOT call the upstream, and must report a
// clear error if a request is attempted before configuration.
func TestRegisterWithoutKeysSucceedsInPendingState(t *testing.T) {
	m, f := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody("plugins:\n  enabled: true\n"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var reg registrationResult
	decodeResult(t, resp, &reg)
	if len(reg.Metadata.ConfigFields) == 0 {
		t.Fatalf("pending register returned no config fields: %+v", reg.Metadata)
	}

	// Nothing may have been fetched: there is no credential to fetch with.
	if calls := f.callsOf(pluginabi.MethodHostHTTPDo); len(calls) != 0 {
		t.Fatalf("host.http.do calls = %d, want 0 while unconfigured", len(calls))
	}

	// The catalog stays empty, so no model is routable yet.
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", []byte("{}")), &static)
	if len(static.Models) != 0 {
		t.Fatalf("models published while unconfigured: %+v", static.Models)
	}

	// A request must fail with a configuration error, not a panic.
	req, _ := json.Marshal(struct {
		pluginapi.ExecutorRequest
	}{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthProvider:   ProviderID,
			AuthAttributes: map[string]string{"api_key": "whatever"},
			Model:          "commandcode/anything",
			SourceFormat:   "openai",
		},
	})
	env := decodeEnv(t, mustHandle(t, m, "executor.execute", req))
	if env.OK || env.Error == nil {
		t.Fatalf("executor succeeded while unconfigured: %s", env.Result)
	}
	if !strings.Contains(env.Error.Message, "not configured") {
		t.Fatalf("unexpected error message: %q", env.Error.Message)
	}
}

// TestReconfigureWithKeysActivatesPlugin covers the second half of the fix:
// saving api-keys through the Management Center reconfigures the plugin,
// which fetches the catalog and publishes models.
func TestReconfigureWithKeysActivatesPlugin(t *testing.T) {
	m, f := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody("plugins: {}\n")); err != nil {
		t.Fatalf("register: %v", err)
	}
	if calls := f.callsOf(pluginabi.MethodHostHTTPDo); len(calls) != 0 {
		t.Fatalf("unexpected upstream call before configure: %d", len(calls))
	}

	if _, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}

	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", []byte("{}")), &static)
	if len(static.Models) == 0 {
		t.Fatal("no models published after configuring api-keys")
	}
	if calls := f.callsOf(pluginabi.MethodHostHTTPDo); len(calls) == 0 {
		t.Fatal("catalog was never fetched after keys were configured")
	}
}

// TestLifecycleMaterializesAuthRecordsForThePool pins the fix for the bug that
// made every real request fail with "auth_not_found: no auth available".
//
// The host finds an executor by walking its OWN auth table, so a plugin whose
// credentials exist only in its own pool is never reached. Registering the pool
// as auth records is what makes the provider schedulable. This test asserted
// the OPPOSITE before the fix - which is why the suite stayed green while the
// provider was unusable end to end.
func TestLifecycleMaterializesAuthRecordsForThePool(t *testing.T) {
	first, second := "user_first-key", "user_second-key"
	yamlText := "api-keys:\n  - value: " + first + "\n  - value: " + second + "\n"
	f := &fakeCaller{responder: catalogResponder(true, testCatalogJSON)}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(yamlText)); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Both credentials must be handed to the host exactly once.
	saves := f.callsOf(pluginabi.MethodHostAuthSave)
	if len(saves) != 2 {
		t.Fatalf("auth saves after register = %d, want 2 (the host cannot schedule what it cannot see)", len(saves))
	}
	seenIDs := map[string]bool{}
	for _, call := range saves {
		var wire pluginapi.HostAuthSaveRequest
		if err := json.Unmarshal(call.payload, &wire); err != nil {
			t.Fatalf("auth payload: %v", err)
		}
		var record struct {
			Type   string `json:"type"`
			ID     string `json:"id"`
			Label  string `json:"label"`
			APIKey string `json:"api_key"`
		}
		if err := json.Unmarshal(wire.JSON, &record); err != nil {
			t.Fatalf("auth record: %v", err)
		}
		// The provider name must be the model prefix the host routes by.
		if record.Type != ProviderID {
			t.Errorf("record type = %q, want %q", record.Type, ProviderID)
		}
		if record.APIKey != first && record.APIKey != second {
			t.Errorf("record does not carry a pool credential")
		}
		// The identity is digest-derived and never embeds the secret.
		hash := authKeyHash(record.APIKey)
		if record.ID != authRecordIDFromHash(hash) {
			t.Errorf("record id = %q, want digest-derived %q", record.ID, authRecordIDFromHash(hash))
		}
		if wire.Name != authFileNameFromHash(hash) {
			t.Errorf("record name = %q, want %q", wire.Name, authFileNameFromHash(hash))
		}
		if strings.Contains(record.ID, record.APIKey) || strings.Contains(wire.Name, record.APIKey) || strings.Contains(record.Label, record.APIKey) {
			t.Errorf("secret leaked into the auth identity")
		}
		seenIDs[record.ID] = true
	}
	if len(seenIDs) != 2 {
		t.Fatalf("auth records are not distinct per credential: %v", seenIDs)
	}

	// Re-registering an unchanged pool must be idempotent: the records are
	// already known, so no duplicate auth entries accumulate.
	before := len(f.callsOf(pluginabi.MethodHostAuthSave))
	if _, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody(yamlText)); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	if after := len(f.callsOf(pluginabi.MethodHostAuthSave)); after != before {
		t.Errorf("idempotent reconfigure saved %d more records", after-before)
	}
}

// TestNoAuthProviderCapabilityStaysOff pins that the fix did NOT re-add the
// OAuth capability. Writing auth records and advertising an interactive login
// flow are separate concerns: re-adding the capability brings back the dead
// "failed to generate authorization url" entry on the host login page.
func TestNoAuthProviderCapabilityStaysOff(t *testing.T) {
	f := &fakeCaller{responder: catalogResponder(true, testCatalogJSON)}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	raw, err := m.HandleCall("plugin.register", lifecycleRequestBody("api-keys:\n  - value: user_x\n"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	env, err := decodeEnvelope(raw)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	var reg registrationResult
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatalf("registration result: %v", err)
	}
	if !reg.Capabilities.ModelProvider {
		t.Fatal("model_provider capability must stay on")
	}
	// capabilities carries no auth_provider field at all, so the wire must not
	// contain one either - presence would make the host publish an OAuth entry.
	var wire struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(env.Result, &wire); err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	if v, present := wire.Capabilities["auth_provider"]; present && v != false {
		t.Fatalf("auth_provider must be absent or false, got %v", v)
	}
}

// TestLifecycleFailsWhenAuthStoreIsUnavailable pins the corrected contract: a
// registration that cannot publish its credentials must NOT report success.
//
// The previous version of this test asserted the opposite - that a broken auth
// store was harmless - which was only true while the plugin wrote no auth
// records at all, i.e. while the provider was unusable. Registering with a
// credential but leaving the host unable to schedule it is the exact state that
// produced "auth_not_found: no auth available" on every request, so it has to
// surface at configure time instead.
func TestLifecycleFailsWhenAuthStoreIsUnavailable(t *testing.T) {
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthList {
			return hostErr("auth_unavailable", "auth directory unavailable"), nil
		}
		if method == pluginabi.MethodHostHTTPDo {
			var wire map[string]any
			_ = json.Unmarshal(payload, &wire)
			if url, _ := wire["url"].(string); strings.HasSuffix(url, "/models") {
				return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(testCatalogJSON)}), nil
			}
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	env := decodeEnv(t, resp)
	if env.OK {
		t.Fatal("register reported success although no credential could be published to the host")
	}
	if env.Error == nil || !strings.Contains(env.Error.Code, "auth") {
		t.Fatalf("error should name the auth failure, got %+v", env.Error)
	}
}

// TestRegisterMetadataCarriesGitHubRepository pins the host validity gate
// (pinned SDK host.go validPlugin): an empty GitHubRepository makes the real
// host drop the plugin on every register/reconfigure, so the field must be
// non-empty AND actually marshal into the envelope bytes.
func TestRegisterMetadataCarriesGitHubRepository(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var reg registrationResult
	decodeResult(t, resp, &reg)
	if reg.Metadata.GitHubRepository == "" {
		t.Fatalf("metadata.GitHubRepository empty: host validPlugin would drop the plugin: %+v", reg.Metadata)
	}
	// Metadata structs have no json tags; assert the Go field name marshals.
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(resp, &env); err != nil {
		t.Fatalf("envelope unmarshal: %v", err)
	}
	if !bytes.Contains(env.Result, []byte(`"GitHubRepository":`)) ||
		bytes.Contains(env.Result, []byte(`"GitHubRepository":""`)) {
		t.Fatalf("envelope metadata lacks non-empty GitHubRepository: %s", env.Result)
	}
}

func TestRegisterIgnoresInjectedHostKeys(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	yamlText := testValidYAML + "enabled: true\npriority: 42\n"
	resp, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody(yamlText))
	if err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("host-injected keys rejected: %s", resp)
	}
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	if len(static.Models) != 1 || static.Models[0].ID != "commandcode/glm-5.3" {
		t.Fatalf("models = %+v", static.Models)
	}
}

func TestMalformedLifecycleAndUnknownMethods(t *testing.T) {
	m, _ := newTestManager(nil)
	for name, body := range map[string][]byte{
		"truncated json": []byte("{"),
		"bad base64":     []byte(`{"config_yaml":"!!!not-base64!!!","schema_version":3}`),
	} {
		resp, err := m.HandleCall("plugin.register", body)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		env := decodeEnv(t, resp)
		if env.OK || env.Error == nil || env.Error.Code != "invalid_request" {
			t.Fatalf("%s: envelope = %s", name, resp)
		}
	}
	resp, err := m.HandleCall("totally.bogus", nil)
	if err != nil {
		t.Fatalf("unknown method: %v", err)
	}
	env := decodeEnv(t, resp)
	if env.OK || env.Error == nil || env.Error.Code != "unknown_method" ||
		!strings.Contains(env.Error.Message, "totally.bogus") {
		t.Fatalf("envelope = %s", resp)
	}
}

func TestReconfigureSwapsPrefixIDs(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	noPrefix := testValidYAML + "model-prefix:\n  enabled: false\n"
	if _, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody(noPrefix)); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	if len(static.Models) != 1 || static.Models[0].ID != "glm-5.3" {
		t.Fatalf("after prefix-off reconfigure: %+v", static.Models)
	}
}

func TestRefreshFailureStillRegistersAndWarnsWithoutSecrets(t *testing.T) {
	m, f := newTestManager(catalogResponder(false, ""))
	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("refresh failure must not fail registration (FR-002): %s", resp)
	}
	logCalls := f.callsOf(pluginabi.MethodHostLog)
	if len(logCalls) != 1 {
		t.Fatalf("warn logs = %d, want 1", len(logCalls))
	}
	var entry map[string]any
	if err := json.Unmarshal(logCalls[0].payload, &entry); err != nil {
		t.Fatalf("log payload: %v", err)
	}
	if entry["level"] != "warn" || !strings.Contains(entry["message"].(string), "catalog refresh failed") {
		t.Fatalf("log entry = %v", entry)
	}
	blob := string(logCalls[0].payload)
	if strings.Contains(blob, testKey) || strings.Contains(blob, "Bearer") {
		t.Fatalf("log leaks credential material: %s", blob)
	}
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	if len(static.Models) != 0 {
		t.Fatalf("failed refresh must yield empty routable set: %+v", static.Models)
	}
}

func TestRegisterRefreshesWithConfiguredBearer(t *testing.T) {
	m, f := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(dummyKeyYAML))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("register rejected: %s", resp)
	}
	calls := f.callsOf(pluginabi.MethodHostHTTPDo)
	if len(calls) != 1 {
		t.Fatalf("host.http.do calls = %d, want 1", len(calls))
	}
	var wire map[string]any
	if err := json.Unmarshal(calls[0].payload, &wire); err != nil {
		t.Fatalf("bridge payload: %v", err)
	}
	headers := wire["headers"].(map[string]any)
	if headers["Authorization"].([]any)[0].(string) != "Bearer "+dummyKey {
		t.Fatalf("expected configured bearer key, got %v", headers)
	}
}

// TestRegisterWithBlockingCatalogReturnsQuickly pins the dedicated register
// timeout: the synchronous lifecycle refresh must expire at
// registerRefreshTimeout (10s), never at request-timeout (default 15m), and
// registration must still succeed with an empty routable set (FR-002).
func TestRegisterWithBlockingCatalogReturnsQuickly(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		var wire map[string]any
		if method == pluginabi.MethodHostHTTPDo && json.Unmarshal(payload, &wire) == nil {
			if url, _ := wire["url"].(string); strings.HasSuffix(url, "/models") {
				<-release // block far past the register timeout
				return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(testCatalogJSON)}), nil
			}
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	start := time.Now()
	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("registration must succeed despite blocked catalog: %s", resp)
	}
	if elapsed < 9*time.Second || elapsed > 13*time.Second {
		t.Fatalf("register elapsed = %v, want ~registerRefreshTimeout (10s), not 15m", elapsed)
	}
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	if len(static.Models) != 0 {
		t.Fatalf("blocked refresh must yield empty routable set: %+v", static.Models)
	}
}

// ---- dispatcher: models before register, shutdown, panic ---------------

func TestModelsBeforeRegisterIsEmptyNotError(t *testing.T) {
	m, _ := newTestManager(nil)
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	if static.Provider != ProviderID || len(static.Models) != 0 {
		t.Fatalf("pre-register static = %+v", static)
	}
}

func TestShutdownClearsStateAndStopsTicker(t *testing.T) {
	f := &fakeCaller{responder: catalogResponder(true, testCatalogJSON)}
	m := NewManager(NewHostBridge(f.call))
	installFastLoop(t, m, testValidYAML)
	waitFor(t, "periodic refresh ticks", func() bool {
		return len(f.callsOf(pluginabi.MethodHostHTTPDo)) >= 3
	})
	resp := mustHandle(t, m, "plugin.shutdown", nil)
	env := decodeEnv(t, resp)
	if !env.OK || string(env.Result) != "{}" {
		t.Fatalf("shutdown envelope = %s", resp)
	}
	m.mu.Lock()
	stopped := m.stop == nil && m.done == nil && m.mgr == nil && m.cfg.BaseURL == ""
	m.mu.Unlock()
	if !stopped {
		t.Fatal("shutdown left state behind")
	}
	before := len(f.callsOf(pluginabi.MethodHostHTTPDo))
	time.Sleep(40 * time.Millisecond)
	if after := len(f.callsOf(pluginabi.MethodHostHTTPDo)); after != before {
		t.Fatalf("ticks continued after shutdown: %d -> %d", before, after)
	}
	resp2 := mustHandle(t, m, "plugin.shutdown", nil)
	if env := decodeEnv(t, resp2); !env.OK {
		t.Fatalf("double shutdown must be idempotent: %s", resp2)
	}
}

// TestShutdownDrainsOrphanedHostCallbacks pins the Unix unload-safety fix:
// handleShutdown waits (bounded) for host-callback goroutines orphaned by
// timeouts, warns when any are still alive at the deadline, and a follow-up
// shutdown once the orphan unwinds reports nothing in flight.
func TestShutdownDrainsOrphanedHostCallbacks(t *testing.T) {
	oldDrain := shutdownDrainTimeout
	shutdownDrainTimeout = 60 * time.Millisecond
	defer func() { shutdownDrainTimeout = oldDrain }()

	release := make(chan struct{})
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostLog:
			return hostOK(map[string]any{}), nil
		case pluginabi.MethodHostHTTPDo:
			<-release // wedged host callback, no abandon cleanup attached
			return hostOK(map[string]any{}), nil
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	if _, err := m.bridge.Do(ctx, pluginapi.HTTPRequest{}); err == nil ||
		!strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected wedged Do to time out, got %v", err)
	}
	cancel()

	inFlightWarn := func() int {
		n := 0
		for _, c := range f.callsOf(pluginabi.MethodHostLog) {
			if strings.Contains(string(c.payload), "still in flight") {
				n++
			}
		}
		return n
	}

	resp := mustHandle(t, m, "plugin.shutdown", nil)
	if env := decodeEnv(t, resp); !env.OK || string(env.Result) != "{}" {
		t.Fatalf("shutdown envelope = %s", resp)
	}
	if n := inFlightWarn(); n != 1 {
		t.Fatalf("in-flight warns = %d, want exactly the stalled-shutdown warn", n)
	}

	close(release)
	resp2 := mustHandle(t, m, "plugin.shutdown", nil)
	if env := decodeEnv(t, resp2); !env.OK {
		t.Fatalf("second shutdown envelope = %s", resp2)
	}
	if n := inFlightWarn(); n != 1 {
		t.Fatalf("warns after orphan released = %d, want no repeat", n)
	}
}

func TestShutdownDrainTimeoutPolicy(t *testing.T) {
	if shutdownDrainTimeout != 15*time.Second {
		t.Fatalf("shutdown drain timeout = %s, want 15s", shutdownDrainTimeout)
	}
}

// TestRegisterSurvivesNilHostBridge pins F5 degradation: a zero-value
// Manager (nil host bridge, only reachable via direct construction) makes
// the initial refresh fail with the catalog package's classified
// "host client unavailable" error instead of panicking; registration still
// succeeds under FR-002 stale semantics.
func TestRegisterSurvivesNilHostBridge(t *testing.T) {
	m := &Manager{}
	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML))
	if err != nil {
		t.Fatalf("register must not surface as Go error: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("registration must survive unavailable host client: %s", resp)
	}
	var reg registrationResult
	if err := json.Unmarshal(env.Result, &reg); err != nil || reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("degraded registration malformed: %s", resp)
	}
}

// ---- concurrency --------------------------------------------------------

func TestConcurrentHandleCalls(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	methods := []string{"model.static", "model.for_auth", "bogus.method"}
	var wg sync.WaitGroup
	wg.Add(len(methods) + 1)
	for _, method := range methods {
		go func(method string) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if _, err := m.HandleCall(method, nil); err != nil {
					t.Errorf("%s: %v", method, err)
					return
				}
			}
		}(method)
	}
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			body := lifecycleRequestBody(testValidYAML)
			if i%2 == 1 {
				body = lifecycleRequestBody(testValidYAML + "model-prefix:\n  enabled: false\n")
			}
			if _, err := m.HandleCall("plugin.reconfigure", body); err != nil {
				t.Errorf("reconfigure: %v", err)
				return
			}
		}
	}()
	wg.Wait()
}

func TestOverlappingLifecyclesLeaveSingleTicker(t *testing.T) {
	// F2/F4 regression: concurrent register/reconfigure must serialize the
	// stop-wait-install sequence so exactly one ticker survives, and it must
	// exit on stop (no orphaned loops keep refreshing). Validated configs
	// floor refresh-interval at 1m, so the survivor is checked structurally
	// — one tracked loop that exits on stop — instead of by counting ticks.
	m, f := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			method := "plugin.register"
			if i == 1 {
				method = "plugin.reconfigure"
			}
			if _, err := m.HandleCall(method, lifecycleRequestBody(testValidYAML)); err != nil {
				t.Errorf("%s: %v", method, err)
			}
		}(i)
	}
	wg.Wait()
	// The two lifecycles may race, but auth materialization must stay
	// idempotent: the harness records each saved file by name, so a duplicated
	// record would show up as two distinct names for one credential. One
	// credential in testValidYAML therefore means exactly one auth file.
	files := f.savedAuthFiles()
	if len(files) != 1 {
		t.Fatalf("overlapping lifecycles saved %d auth files (%v), want 1", len(files), files)
	}
	done := m.closeStop()
	if done == nil {
		t.Fatal("concurrent lifecycles left no tracked ticker")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("surviving ticker did not exit on stop")
	}
	if again := m.closeStop(); again != nil {
		t.Fatal("more than one ticker was left tracked")
	}
}

func TestReconfigureFailedRefreshServesViaTicks(t *testing.T) {
	// F3 regression: when a reconfigure's synchronous refresh fails, the
	// stale-check keeps the SERVED manager; background ticks must refresh
	// that same manager so recovered upstream data becomes visible.
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	catalogV1 := `{"data":[{"id":"glm-5.3"}]}`
	catalogV2 := `{"data":[{"id":"minimax-m3"}]}`
	var fetches atomic.Int64
	f.responder = func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		var wire map[string]any
		_ = json.Unmarshal(payload, &wire)
		url, _ := wire["url"].(string)
		if !strings.HasSuffix(url, "/models") {
			return hostOK(map[string]any{}), nil
		}
		switch fetches.Add(1) {
		case 1:
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(catalogV1)}), nil
		case 2:
			return hostErr("upstream_down", "simulated refresh failure"), nil
		default:
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(catalogV2)}), nil
		}
	}

	// Validated configs floor refresh-interval at 1m, so ticks are driven
	// manually via manualTick (the exact body a background tick runs).
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	assertStaticModel(t, m, "commandcode/glm-5.3")
	if _, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	assertStaticModel(t, m, "commandcode/glm-5.3")
	manualTick(t, m)
	assertStaticModel(t, m, "commandcode/minimax-m3")
}

// TestShutdownAbortsInFlightTickRefresh pins the F4 fix: a tick's refresh
// context derives from stop, so close(stop) aborts an in-flight Refresh
// immediately and shutdown cannot sit holding lifeMu for up to
// request-timeout behind a hung upstream.
func TestShutdownAbortsInFlightTickRefresh(t *testing.T) {
	// The orphaned tick callback stays parked until cleanup releases it;
	// shrink the unload-safety drain so it cannot dominate this timing
	// assertion (the ctx-abort property under test is orthogonal).
	oldDrain := shutdownDrainTimeout
	shutdownDrainTimeout = 50 * time.Millisecond
	defer func() { shutdownDrainTimeout = oldDrain }()

	var syncServed atomic.Bool
	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		var wire map[string]any
		if err := json.Unmarshal(payload, &wire); err != nil {
			return hostOK(map[string]any{}), nil
		}
		if url, _ := wire["url"].(string); strings.HasSuffix(url, "/models") {
			if !syncServed.CompareAndSwap(false, true) {
				close(started) // a tick is now in flight against a hung upstream
				<-release      // block far past any sane shutdown wait
				return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(testCatalogJSON)}), nil
			}
			// Serve the synchronous register refresh instantly.
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(testCatalogJSON)}), nil
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Validated configs floor refresh-interval at 1m; install the production
	// loop directly at 5ms so a tick goes in flight immediately.
	installFastLoop(t, m, testValidYAML)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("tick refresh never started")
	}

	shutdownStart := time.Now()
	resp := mustHandle(t, m, "plugin.shutdown", nil)
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("shutdown envelope = %s", resp)
	}
	if elapsed := time.Since(shutdownStart); elapsed > 2*time.Second {
		t.Fatalf("shutdown waited %v on the in-flight tick; stop-derived ctx must abort it", elapsed)
	}
}

// TestReconfigureFailedRefreshSeedsCarryoverThenFetchesNewURL pins the F5
// fix: a failed-refresh reconfigure must adopt the NEW manager (seeded with
// the old last-good records) so carried-over models stay visible between the
// failure and the next success, and background ticks fetch the NEW
// catalog-url instead of the old one forever.
func TestReconfigureFailedRefreshSeedsCarryoverThenFetchesNewURL(t *testing.T) {
	oldURL := "https://api.commandcode.ai/provider/v1/models"
	newURL := "https://mirror.test/api/models"
	catalogV1 := `{"data":[{"id":"glm-5.3"}]}`
	catalogV2 := `{"data":[{"id":"minimax-m3"}]}`
	var allowOldURL atomic.Bool
	allowOldURL.Store(true)
	var failNewSync atomic.Bool
	failNewSync.Store(true)
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		var wire map[string]any
		_ = json.Unmarshal(payload, &wire)
		url, _ := wire["url"].(string)
		if !strings.HasSuffix(url, "/models") {
			return hostOK(map[string]any{}), nil
		}
		switch url {
		case oldURL:
			// Every /models fetch after the reconfigure returned must target
			// the NEW url; old-loop ticks all complete before it returns.
			if !allowOldURL.Load() {
				t.Error("tick used the OLD catalog-url after reconfigure")
				return hostErr("old_url", url), nil
			}
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(catalogV1)}), nil
		case newURL:
			if failNewSync.CompareAndSwap(true, false) {
				return hostErr("upstream_down", "simulated reconfigure refresh failure"), nil
			}
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(catalogV2)}), nil
		default:
			t.Errorf("unexpected catalog url %q", url)
			return hostErr("unexpected_url", url), nil
		}
	}}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	assertStaticModel(t, m, "commandcode/glm-5.3")

	newBaseYAML := testValidYAML + "base-url: https://mirror.test/api\n"
	if _, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody(newBaseYAML)); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	allowOldURL.Store(false)

	// Carried-over last-good model stays visible between the failed
	// reconfigure refresh and the next successful refresh.
	assertStaticModel(t, m, "commandcode/glm-5.3")

	// Validated configs floor refresh-interval at 1m; run the tick body
	// directly over the served state instead of waiting for the loop.
	manualTick(t, m)
	assertStaticModel(t, m, "commandcode/minimax-m3")
}

// TestReconfigureFailedRefreshHonorsStalePolicy pins the seed-gate fix: on a
// failed-refresh reconfigure the seed consults the NEW config's
// stale-while-unavailable — fail-closed serves nothing immediately (matching
// Manager.fail's ticker behavior), stale-enabled keeps carrying over.
func TestReconfigureFailedRefreshHonorsStalePolicy(t *testing.T) {
	catalogV1 := `{"data":[{"id":"glm-5.3"}]}`
	catalogV2 := `{"data":[{"id":"minimax-m3"}]}`
	for _, tc := range []struct {
		name          string
		policyYAML    string
		wantImmediate bool // model visible right after the failed reconfigure
	}{
		{name: "stale enabled keeps carryover", policyYAML: "", wantImmediate: true},
		{name: "fail closed empties immediately", policyYAML: "catalog:\n  stale-while-unavailable: false\n", wantImmediate: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fetches atomic.Int64
			f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
				if method != pluginabi.MethodHostHTTPDo {
					return hostOK(map[string]any{}), nil
				}
				var wire map[string]any
				_ = json.Unmarshal(payload, &wire)
				if url, _ := wire["url"].(string); !strings.HasSuffix(url, "/models") {
					return hostOK(map[string]any{}), nil
				}
				switch fetches.Add(1) {
				case 1:
					return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(catalogV1)}), nil
				case 2:
					return hostErr("upstream_down", "simulated refresh failure"), nil
				default:
					return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(catalogV2)}), nil
				}
			}}
			m := NewManager(NewHostBridge(f.call))
			t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

			if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
				t.Fatalf("register: %v", err)
			}
			assertStaticModel(t, m, "commandcode/glm-5.3")

			if _, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody(testValidYAML+tc.policyYAML)); err != nil {
				t.Fatalf("reconfigure: %v", err)
			}

			var static pluginapi.ModelResponse
			decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
			got := len(static.Models)
			if tc.wantImmediate && (got != 1 || static.Models[0].ID != "commandcode/glm-5.3") {
				t.Fatalf("carryover models = %+v, want glm-5.3", static.Models)
			}
			if !tc.wantImmediate && got != 0 {
				t.Fatalf("fail-closed served %d models immediately, want 0", got)
			}

			// Both policies recover via the next successful tick against the
			// NEW manager/config.
			manualTick(t, m)
			assertStaticModel(t, m, "commandcode/minimax-m3")
		})
	}
}

func staticModelID(t *testing.T, m *Manager) string {
	t.Helper()
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	if len(static.Models) != 1 {
		t.Fatalf("static models = %+v", static.Models)
	}
	return static.Models[0].ID
}

func assertStaticModel(t *testing.T, m *Manager, wantID string) {
	t.Helper()
	if got := staticModelID(t, m); got != wantID {
		t.Fatalf("static model = %q, want %q", got, wantID)
	}
}

func mustHandle(t *testing.T, m *Manager, method string, req []byte) []byte {
	t.Helper()
	resp, err := m.HandleCall(method, req)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return resp
}
