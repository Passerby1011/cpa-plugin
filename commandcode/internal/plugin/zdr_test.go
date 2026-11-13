package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/commandcode/internal/catalog"
	"github.com/hex-ci/cpa-plugin/commandcode/internal/config"
)

// The two refusals CommandCode answered for poolside/laguna-s-2.1-free with
// x-cmd-zdr: 1 (captured 2026-10-19): the Provider API's 422 and
// /alpha/generate's error line (which arrives inside a 200 reply).
const (
	providerZDRRefusal = `{"error":{"message":"This model has no zero-data-retention upstream. ` +
		`Remove the x-cmd-zdr header or choose a different model.","type":"invalid_request_error",` +
		`"code":"cmd_zdr_no_providers"}}`
	generateZDRRefusal = `{"type":"error","error":{"type":"cmd_zdr_no_providers","code":"CMD_ZDR_NO_PROVIDERS",` +
		`"message":"This model has no zero-data-retention upstream. Disable CMD_ZDR or pick a different model.",` +
		`"statusCode":400,"isRetryable":false}}`
)

// zdrActive pins the switch itself: config OR env, env re-read per call, and
// only the literal "1" counting (the CLI's own rule).
func TestZDRActiveReadsConfigAndEnvPerCall(t *testing.T) {
	t.Setenv(config.EnvZDR, "")

	if zdrActive(config.Config{}) {
		t.Fatal("ZDR is on by default; it must be off unless asked for")
	}
	if !zdrActive(config.Config{ZDR: true}) {
		t.Error("config zdr: true did not turn ZDR on")
	}
	// Read at CALL time: a value set after the config was loaded still counts,
	// so a deployment can flip it without a reconfigure.
	t.Setenv(config.EnvZDR, "1")
	if !zdrActive(config.Config{}) {
		t.Error("CMD_ZDR=1 did not turn ZDR on; the env must be read per request")
	}
	// Only "1", exactly as the vendor CLI reads it. Any other value must NOT
	// turn it on, or a deployment would believe it is protected while the
	// upstream was never asked.
	for _, notOne := range []string{"true", "TRUE", "yes", "on", "0", " 1", "1 "} {
		t.Setenv(config.EnvZDR, notOne)
		if zdrActive(config.Config{}) {
			t.Errorf("CMD_ZDR=%q turned ZDR on; only the literal \"1\" may", notOne)
		}
	}
	// Config wins over an env that is not "1", and survives it.
	t.Setenv(config.EnvZDR, "true")
	if !zdrActive(config.Config{ZDR: true}) {
		t.Error("config zdr: true was overridden by a non-\"1\" env value")
	}
}

// TestZDRHeaderOnProviderPath pins the Provider API leg: the header is present
// while the switch is on and absent while it is off, on every route.
func TestZDRHeaderOnProviderPath(t *testing.T) {
	for _, route := range []catalog.Route{
		catalog.RouteChatCompletions, catalog.RouteMessages, catalog.RouteResponses,
	} {
		t.Run(string(route), func(t *testing.T) {
			on := upstreamRequestHeaders(config.Config{ZDR: true}, route, "user_key", "sess")
			if got := on.Get(zdrHeader); got != zdrValue {
				t.Fatalf("%s: x-cmd-zdr = %q, want %q", route, got, zdrValue)
			}
			off := upstreamRequestHeaders(config.Config{}, route, "user_key", "sess")
			if got := off.Get(zdrHeader); got != "" {
				t.Fatalf("%s: x-cmd-zdr present (%q) while the switch is off", route, got)
			}
			// The rest of the identity must be untouched either way.
			if on.Get("x-commandcode-session") != "sess" || off.Get("x-commandcode-session") != "sess" {
				t.Fatalf("%s: session header changed: on=%q off=%q", route,
					on.Get("x-commandcode-session"), off.Get("x-commandcode-session"))
			}
		})
	}
	// The env switch alone is enough: no config change, no restart.
	t.Setenv(config.EnvZDR, "1")
	if got := upstreamRequestHeaders(config.Config{}, catalog.RouteChatCompletions, "user_key", "sess").Get(zdrHeader); got != zdrValue {
		t.Fatalf("CMD_ZDR=1 did not put the header on the Provider API request (%q)", got)
	}
	t.Setenv(config.EnvZDR, "")
}

// TestZDRHeaderOnGoCLIPath pins the CLI leg, which the vendor CLI sends the
// same header on. It must follow the switch identically - a Go-plan request
// that lost it would be a silently weaker guarantee than the Provider one.
func TestZDRHeaderOnGoCLIPath(t *testing.T) {
	on := goCLIRequestHeaders(config.Config{ZDR: true}, "user_key", "sess")
	if got := on.Get(zdrHeader); got != zdrValue {
		t.Fatalf("go-cli x-cmd-zdr = %q, want %q", got, zdrValue)
	}
	off := goCLIRequestHeaders(config.Config{}, "user_key", "sess")
	if got := off.Get(zdrHeader); got != "" {
		t.Fatalf("go-cli sent x-cmd-zdr (%q) while the switch is off", got)
	}
	t.Setenv(config.EnvZDR, "1")
	if got := goCLIRequestHeaders(config.Config{}, "user_key", "sess").Get(zdrHeader); got != zdrValue {
		t.Fatalf("CMD_ZDR=1 did not put the header on the go-cli request (%q)", got)
	}
	t.Setenv(config.EnvZDR, "")
}

// TestZDRRefusalIsRecognisedNotRetried pins the detector and the error shape
// for both documented spellings: the refusal is surfaced, never rewritten into
// something retryable (turning ZDR off and re-running would defeat the only
// thing the client asked for).
func TestZDRRefusalIsRecognisedNotRetried(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		status  int
		wording bool
	}{
		{"provider 422", providerZDRRefusal, http.StatusUnprocessableEntity, true},
		{"generate line", generateZDRRefusal, http.StatusBadRequest, true},
		// A body with no message at all still has to be recognised; the
		// fallback carries the redacted body as the evidence.
		{"upper-case code only", `{"error":{"code":"CMD_ZDR_NO_PROVIDERS"}}`, http.StatusBadRequest, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !zdrRefusedInError([]byte(tc.body)) {
				t.Fatalf("the upstream ZDR refusal was not recognised: %s", tc.body)
			}
			e := zdrRefusalError(tc.status, []byte(tc.body))
			if e.StatusCode != tc.status {
				t.Errorf("status = %d, want %d (the upstream's own)", e.StatusCode, tc.status)
			}
			if e.Retryable {
				t.Error("the ZDR refusal was marked retryable; re-running it would drop the guarantee")
			}
			if e.Message == "" {
				t.Error("empty message: it reads as a plugin fault rather than an upstream refusal")
			}
			if tc.wording && !strings.Contains(e.Message, "zero-data-retention") {
				t.Errorf("message = %q, want the upstream's own wording", e.Message)
			}
		})
	}
	// An ordinary 4xx is NOT a ZDR refusal and keeps the generic classification.
	if zdrRefusedInError([]byte(`{"error":{"message":"Invalid API key"}}`)) {
		t.Fatal("an unrelated error was mistaken for the ZDR refusal")
	}
}

// TestZDRRefusalPassthroughOnProviderPath drives the refusal through the real
// executor: the upstream's 422 must reach the client as a 422 with its message,
// and the account must not be retried without the header.
func TestZDRRefusalPassthroughOnProviderPath(t *testing.T) {
	f := &fakeCaller{}
	var refunds int
	// The bridge callback runs on its own goroutine, so it must never call
	// t.Fatalf (that only stops the CALLING goroutine and would leave the
	// request hanging). Observable facts are recorded and asserted after.
	sawZDRHeader := false
	f.responder = wrapWithCatalog(multiRouteCatalog, func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDo {
			var wire struct {
				URL     string      `json:"url"`
				Headers http.Header `json:"headers"`
			}
			_ = json.Unmarshal(payload, &wire)
			if strings.HasSuffix(wire.URL, "/chat/completions") {
				refunds++
				sawZDRHeader = wire.Headers.Get(zdrHeader) == zdrValue
				return hostOK(pluginapi.HTTPResponse{
					StatusCode: http.StatusUnprocessableEntity,
					Body:       []byte(providerZDRRefusal),
				}), nil
			}
		}
		return upstreamRouter(t, map[string]string{"": ""})(method, payload)
	})
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML+"\nzdr: true\n")); err != nil {
		t.Fatalf("register: %v", err)
	}

	env := mustExecute(t, m, "commandcode/glm-5.3", "openai", []byte(ccRequestBody))
	if env.OK {
		t.Fatalf("the ZDR refusal was reported as success: %+v", env)
	}
	if env.Error == nil || env.Error.HTTPStatus != http.StatusUnprocessableEntity {
		t.Fatalf("error status = %+v, want the upstream's 422", env.Error)
	}
	if !strings.Contains(env.Error.Message, "zero-data-retention") {
		t.Errorf("message = %q, want the upstream's own wording", env.Error.Message)
	}
	if env.Error.Retryable {
		t.Error("the ZDR refusal was reported retryable; nothing may re-run it without the header")
	}
	// Exactly one upstream attempt: the refusal is never silently retried with
	// ZDR dropped.
	if refunds != 1 {
		t.Errorf("upstream chat attempts = %d, want exactly 1 (no auto-disable retry)", refunds)
	}
	if !sawZDRHeader {
		t.Error("the Provider API request did not carry x-cmd-zdr: 1 although zdr: true")
	}
}

// TestZDRRefusalPassesThroughOnGoCLIPath pins the CLI surface's refusal, which
// arrives as an error EVENT inside a 200 reply rather than as an HTTP status.
// Both facts matter: the header was sent, and the refusal reached the client
// with the upstream's own 400 rather than becoming a retryable 502.
func TestZDRRefusalPassesThroughOnGoCLIPath(t *testing.T) {
	f := &fakeCaller{}
	var attempts int
	sawZDRHeader := false
	f.responder = wrapWithCatalog(multiRouteCatalog, func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDo {
			var wire struct {
				URL     string      `json:"url"`
				Headers http.Header `json:"headers"`
			}
			_ = json.Unmarshal(payload, &wire)
			if strings.HasSuffix(wire.URL, "/alpha/generate") {
				attempts++
				sawZDRHeader = wire.Headers.Get(zdrHeader) == zdrValue
				// The upstream answers 200 and puts the refusal in the stream.
				return hostOK(pluginapi.HTTPResponse{
					StatusCode: http.StatusOK,
					Body:       []byte(generateZDRRefusal + "\n"),
				}), nil
			}
		}
		return upstreamRouter(t, map[string]string{"": ""})(method, payload)
	})
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	// A go-cli account so the request takes the CLI transport.
	goYAML := "accounts:\n  - mode: go-cli\n    credential: " + testKey + "\nzdr: true\n"
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(goYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}

	env := mustExecute(t, m, "commandcode/glm-5.3", "openai", []byte(ccRequestBody))
	if env.OK {
		t.Fatalf("the CLI ZDR refusal was reported as success: %+v", env)
	}
	if env.Error == nil || env.Error.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("error = %+v, want the CLI refusal's 400", env.Error)
	}
	if !strings.Contains(env.Error.Message, "zero-data-retention") {
		t.Errorf("message = %q, want the upstream's own wording", env.Error.Message)
	}
	if env.Error.Retryable {
		t.Error("the CLI ZDR refusal was reported retryable")
	}
	if attempts != 1 {
		t.Errorf("/alpha/generate attempts = %d, want exactly 1 (no auto-disable retry)", attempts)
	}
	if !sawZDRHeader {
		t.Error("the /alpha/generate request did not carry x-cmd-zdr: 1 although zdr: true")
	}
}

// TestZDRRequestCarriesHeaderThroughExecutor closes the loop on the header:
// through the real executor the Provider API call carries x-cmd-zdr while the
// config says so, and does not while it does not.
func TestZDRRequestCarriesHeaderThroughExecutor(t *testing.T) {
	for _, on := range []bool{true, false} {
		f := &fakeCaller{}
		f.responder = wrapWithCatalog(multiRouteCatalog, upstreamRouter(t, map[string]string{
			"/v1/chat/completions": ccResponseBody,
		}))
		t.Setenv(config.EnvZDR, "")
		m := NewManager(NewHostBridge(f.call))
		t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
		yaml := testValidYAML
		if on {
			yaml += "\nzdr: true\n"
		}
		if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(yaml)); err != nil {
			t.Fatalf("register: %v", err)
		}
		env := mustExecute(t, m, "commandcode/glm-5.3", "openai", []byte(ccRequestBody))
		if !env.OK {
			t.Fatalf("execute failed: %+v", env.Error)
		}
		var wire struct {
			Headers http.Header `json:"headers"`
		}
		calls := f.callsOf(pluginabi.MethodHostHTTPDo)
		if len(calls) == 0 {
			t.Fatal("no upstream call recorded")
		}
		_ = json.Unmarshal(calls[len(calls)-1].payload, &wire)
		if on {
			if got := wire.Headers.Get(zdrHeader); got != zdrValue {
				t.Errorf("x-cmd-zdr = %q, want %q while zdr: true", got, zdrValue)
			}
		} else if got := wire.Headers.Get(zdrHeader); got != "" {
			t.Errorf("x-cmd-zdr present (%q) while zdr is off", got)
		}
	}
}
