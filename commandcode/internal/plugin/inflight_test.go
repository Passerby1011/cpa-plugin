package plugin

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// inflightYAML builds a go-cli pool with the given global in-flight cap.
func inflightYAML(max string) string {
	return "max-inflight: " + max + "\n" +
		"accounts:\n" +
		"  - label: go\n" +
		"    mode: go-cli\n" +
		"    credential: " + retryAccountKey + "\n" +
		"  - label: main\n" +
		"    credential: " + providerAccountKey + "\n"
}

// TestAcquireInflightSemantics pins the pure admission logic: off by default,
// and the boundary is inclusive of max.
func TestAcquireInflightSemantics(t *testing.T) {
	m := NewManager(nil)

	// max<=0 disables the cap entirely: every request is admitted.
	for i := 0; i < 100; i++ {
		if _, ok := m.acquireInflight(0); !ok {
			t.Fatalf("cap 0 must admit request %d (disabled)", i+1)
		}
	}
	if got := m.inflight.Load(); got != 0 {
		t.Fatalf("a disabled cap must not accumulate counts, got %d", got)
	}

	// max=2 admits exactly two, then rejects, and a release frees one slot.
	rel1, ok1 := m.acquireInflight(2)
	_, ok2 := m.acquireInflight(2)
	_, ok3 := m.acquireInflight(2)
	if !ok1 || !ok2 {
		t.Fatalf("cap 2 must admit the first two: ok1=%v ok2=%v", ok1, ok2)
	}
	if ok3 {
		t.Fatal("cap 2 must reject the third concurrent request")
	}
	if got := m.inflight.Load(); got != 2 {
		t.Fatalf("after two admissions and one rejection, inflight = %d, want 2", got)
	}
	rel1()
	if _, ok4 := m.acquireInflight(2); !ok4 {
		t.Fatal("a released slot must be reusable")
	}

	// Release must be idempotent: a double release would free a slot another
	// request is holding.
	rel1()
	rel1()
	if got := m.inflight.Load(); got != 2 {
		t.Fatalf("double release changed the count: inflight = %d, want 2", got)
	}
}

// TestInflightCapRejectsWith503 pins that a request past the cap gets a
// retryable 503 rather than being silently dropped or processed anyway.
func TestInflightCapRejectsWith503(t *testing.T) {
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
				StatusCode: 200,
				Headers:    map[string][]string{"Content-Type": {"text/event-stream"}},
				Body:       []byte(cliGoStream),
			}), nil
		}
		if isDeviceAnnounceURL(url) {
			return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{}`)}), nil
		}
		return hostErr("test", "unrouted"), nil
	})

	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(inflightYAML("1"))); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Occupy the single slot directly, then drive a request through the plugin.
	rel, ok := m.acquireInflight(1)
	if !ok {
		t.Fatal("the first direct admission must succeed")
	}
	defer rel()

	env := mustExecute(t, m, "commandcode/glm-5.3", "openai", []byte(ccRequestBody))
	if env.OK || env.Error == nil {
		t.Fatalf("a request past the cap must be rejected, got %s", env.Result)
	}
	if env.Error.HTTPStatus != 503 {
		t.Errorf("rejection status = %d, want 503", env.Error.HTTPStatus)
	}
	if !env.Error.Retryable {
		t.Error("the rejection must be retryable so the SDK backs off instead of failing hard")
	}
}

// TestInflightSlotIsReleasedAfterSuccess is the anti-leak test: after a
// completed call the count must return to zero. Without this a slot leaked on
// any path would slowly wedge the cap until every request 503s.
func TestInflightSlotIsReleasedAfterSuccess(t *testing.T) {
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
				StatusCode: 200,
				Headers:    map[string][]string{"Content-Type": {"text/event-stream"}},
				Body:       []byte(cliGoStream),
			}), nil
		}
		if isDeviceAnnounceURL(url) {
			return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{}`)}), nil
		}
		return hostErr("test", "unrouted"), nil
	})

	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(inflightYAML("2"))); err != nil {
		t.Fatalf("register: %v", err)
	}

	for i := 0; i < 5; i++ {
		env := mustExecute(t, m, "commandcode/glm-5.3", "openai", []byte(ccRequestBody))
		if !env.OK || env.Error != nil {
			t.Fatalf("call %d failed: %+v", i, env.Error)
		}
	}
	if got := m.inflight.Load(); got != 0 {
		t.Fatalf("inflight = %d after five completed calls, want 0 (a slot leaked)", got)
	}
}

// TestInflightSlotReleasedOnUpstreamFailure pins the leak fix on the error
// path: a request that fails before the pump starts must still free its slot.
func TestInflightSlotReleasedOnUpstreamFailure(t *testing.T) {
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
				StatusCode: 400,
				Headers:    map[string][]string{"Content-Type": {"application/json"}},
				Body:       []byte(`{"error":{"message":"bad"}}`),
			}), nil
		}
		if isDeviceAnnounceURL(url) {
			return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{}`)}), nil
		}
		return hostErr("test", "unrouted"), nil
	})

	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(inflightYAML("1"))); err != nil {
		t.Fatalf("register: %v", err)
	}

	for i := 0; i < 3; i++ {
		env := mustExecute(t, m, "commandcode/glm-5.3", "openai", []byte(ccRequestBody))
		if env.OK {
			t.Fatalf("call %d: a 400 upstream must surface as an error", i)
		}
	}
	if got := m.inflight.Load(); got != 0 {
		t.Fatalf("inflight = %d after three FAILED calls, want 0 (slot leaked on the error path)", got)
	}
}
