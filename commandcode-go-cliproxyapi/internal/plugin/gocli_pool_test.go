package plugin

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// goCliYAML is a pool with a go-cli account (selected first) and a provider
// account (required because only a provider credential can fetch /models).
func goCliYAML() string {
	return "accounts:\n" +
		"  - label: go\n" +
		"    mode: go-cli\n" +
		"    credential: " + goAccountKey + "\n" +
		"  - label: main\n" +
		"    credential: " + providerAccountKey + "\n"
}

// goCliStreamManager registers a go-cli pool and routes the CLI stream to the
// scripted upstream. Non-stream /alpha/generate calls get cliNonStream.
func goCliStreamManager(t *testing.T, script streamScript, cliNonStream func() ([]byte, error)) (*Manager, *fakeCaller) {
	t.Helper()
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	stream := streamResponder(script)
	f.responder = wrapWithCatalog(multiRouteCatalog, func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDo {
			wire := decodePayload(t, capturedCall{payload: payload})
			if url, _ := wire["url"].(string); strings.HasSuffix(url, "/alpha/generate") && cliNonStream != nil {
				return cliNonStream()
			}
		}
		return stream(method, payload)
	})
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(goCliYAML())); err != nil {
		t.Fatalf("register: %v", err)
	}
	return m, f
}

// TestGoCliStreamTruncatedFailsStream pins that a CLI stream which ends
// without a finish event is failed, not passed off as a completed answer.
func TestGoCliStreamTruncatedFailsStream(t *testing.T) {
	m, f := goCliStreamManager(t, streamScript{
		upstreamID: "up-trunc",
		frames: []string{
			// Text, then the connection simply ends: no finish, no [DONE].
			`data: {"type":"text-delta","text":"partial"}` + "\n\n",
		},
	}, nil)
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("commandcode/glm-5.3", "openai", []byte(ccRequestBody), "down-trunc"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("envelope = %+v", env.Error)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	downCloses := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(downCloses) != 1 || !strings.Contains(string(downCloses[0].payload), `"error"`) {
		t.Fatalf("truncated go-cli stream must close with an error, got %v", downCloses)
	}
}

// TestGoCliStreamCompleteClosesCleanly is the control: a CLI stream that ends
// with a finish event closes cleanly.
func TestGoCliStreamCompleteClosesCleanly(t *testing.T) {
	m, f := goCliStreamManager(t, streamScript{
		upstreamID: "up-ok",
		frames: []string{
			`data: {"type":"text-delta","text":"hi"}` + "\n\n",
			`data: {"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":1}}` + "\n\n",
			"data: [DONE]\n\n",
		},
	}, nil)
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("commandcode/glm-5.3", "openai", []byte(ccRequestBody), "down-ok"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("envelope = %+v", env.Error)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	downCloses := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(downCloses) != 1 || strings.Contains(string(downCloses[0].payload), `"error"`) {
		t.Fatalf("complete go-cli stream must close cleanly, got %v", downCloses)
	}
}

// TestGoCliStreamReleasesAccountOnce pins the pool lifecycle: the account is
// held for the whole stream and released exactly once, with no cooldown on a
// clean completion.
func TestGoCliStreamReleasesAccountOnce(t *testing.T) {
	m, _ := goCliStreamManager(t, streamScript{
		upstreamID: "up-rel",
		frames: []string{
			`data: {"type":"text-delta","text":"hi"}` + "\n\n",
			`data: {"type":"finish","finishReason":"stop"}` + "\n\n",
			"data: [DONE]\n\n",
		},
	}, nil)
	if _, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("commandcode/glm-5.3", "openai", []byte(ccRequestBody), "down-rel")); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	m.bridge.WaitForInFlight(5 * time.Second)
	st := m.pool.state(goAccountKey)
	if st.inFlight != 0 {
		t.Fatalf("inFlight = %d, want 0 (released exactly once)", st.inFlight)
	}
	if !st.cooldownUntil.IsZero() {
		t.Fatalf("cooldown = %v, want none after a clean stream", st.cooldownUntil)
	}
}

// TestGoCliNonStreamRateLimitCoolsAccount pins that a 429 from /alpha/generate
// parks the credential instead of leaving it hot.
func TestGoCliNonStreamRateLimitCoolsAccount(t *testing.T) {
	m, _ := goCliStreamManager(t, streamScript{}, func() ([]byte, error) {
		return hostOK(pluginapi.HTTPResponse{
			StatusCode: 429,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(`{"error":{"message":"rate limited"}}`),
		}), nil
	})
	resp, err := m.HandleCall("executor.execute", execReqBody("commandcode/glm-5.3", "openai", []byte(ccRequestBody), false))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if env := decodeEnv(t, resp); env.OK {
		t.Fatalf("429 must surface as an error envelope, got %s", resp)
	}
	st := m.pool.state(goAccountKey)
	if !st.cooldownUntil.After(time.Now()) {
		t.Fatalf("cooldown = %v, want a future time after a 429", st.cooldownUntil)
	}
	if st.inFlight != 0 {
		t.Fatalf("inFlight = %d, want 0", st.inFlight)
	}
}

// TestGoCliOnlyPoolFetchesLiveCatalogAndStillRoutesExecution pins the fix for
// the bug where a go-cli-only pool never fetched the live catalog.
//
// The old belief was that /models belongs to the Provider API and refuses
// Go-plan keys, so such a pool was locked out of live discovery and had to be
// hand-fed catalog.static. That is false: a Go key calls /models successfully.
// The pool must therefore fetch live models, and execution must still route to
// the CLI transport (/alpha/generate) rather than the Provider API.
func TestGoCliOnlyPoolFetchesLiveCatalogAndStillRoutesExecution(t *testing.T) {
	yamlText := "accounts:\n" +
		"  - label: go\n" +
		"    mode: go-cli\n" +
		"    credential: " + goAccountKey + "\n" +
		"catalog:\n" +
		"  static:\n" +
		"    - claude-sonnet-4-6\n" +
		"    - deepseek/deepseek-v4-pro\n"

	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	sawCatalogFetch := false
	f.responder = func(method string, payload []byte) ([]byte, error) {
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
		if strings.HasSuffix(url, "/models") {
			sawCatalogFetch = true
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(multiRouteCatalog)}), nil
		}
		if strings.HasSuffix(url, accountSubscriptionPath) {
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK,
				Body: []byte(`{"success":true,"data":{"planId":"test-ungated"}}`)}), nil
		}
		if strings.HasSuffix(url, accountCreditsPath) {
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK,
				Body: []byte(`{"credits":{"monthlyCredits":0,"purchasedCredits":0,"freeCredits":0}}`)}), nil
		}
		// Device announcements have their own account-surface endpoints; they
		// are not go-cli turns and must not trip the routing assertion below.
		if isDeviceAnnounceURL(url) {
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{}`)}), nil
		}
		t.Errorf("unexpected upstream url %v", url)
		return hostErr("test", "unrouted"), nil
	}
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(yamlText)); err != nil {
		t.Fatalf("register: %v", err)
	}

	// The core regression: a go-cli-only pool must reach the live catalog.
	if !sawCatalogFetch {
		t.Fatal("go-cli-only pool never fetched /models; it is locked out of live discovery again")
	}

	// The live catalog must be what is published, not the static fallback.
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	ids := map[string]bool{}
	for _, mi := range static.Models {
		ids[mi.ID] = true
	}
	if !ids["commandcode/glm-5.3"] {
		t.Fatalf("live catalog not published for a go-cli-only pool: %v", ids)
	}

	// And a call must route to the CLI transport.
	env := mustExecute(t, m, "commandcode/glm-5.3", "openai", []byte(ccRequestBody))
	if !env.OK || env.Error != nil {
		t.Fatalf("execute envelope error: %+v", env.Error)
	}
	wire := lastWire(t, f, pluginabi.MethodHostHTTPDo)
	if got := wire["url"].(string); !strings.HasSuffix(got, "/alpha/generate") {
		t.Fatalf("url = %v, want /alpha/generate", got)
	}
}
