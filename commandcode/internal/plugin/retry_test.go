package plugin

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const retryAccountKey = "user_retry_route_test"

// retryYAML is a go-cli pool with the provider account needed for /models.
func retryYAML() string {
	return "accounts:\n" +
		"  - label: go\n" +
		"    mode: go-cli\n" +
		"    credential: " + retryAccountKey + "\n" +
		"  - label: main\n" +
		"    credential: " + providerAccountKey + "\n"
}

// TestIsRetryableTransportFlip pins the classification that decides whether a
// failed attempt is redone. Getting this wrong in the permissive direction
// retries a deliberate 503/429 backoff signal and multiplies load during the
// exact incident the signal is meant to relieve; getting it wrong in the strict
// direction makes the whole mechanism dead code.
func TestIsRetryableTransportFlip(t *testing.T) {
	retryable := []string{
		"read tcp 10.0.0.1:443: connection reset by peer",
		"unexpected EOF",
		"use of closed network connection: socket hang up",
		"undici: terminated",
		"write: broken pipe",
		"dial tcp: connect: connection refused",
		"i/o timeout", // etimedout
		"other side closed",
		"fetch failed",
		"http2: server closed connection",
	}
	for _, msg := range retryable {
		if !isRetryableTransportFlip(msg) {
			t.Errorf("isRetryableTransportFlip(%q) = false, want true", msg)
		}
	}

	notRetryable := []string{
		"", // no message at all: nothing to justify a retry
		"upstream CLI stream ended without a finish event", // semantic, not transport
		"idle watchdog tripped",                            // OUR signal, never retried
		"stream idle timeout",
		"rate limit exceeded",
		"context deadline exceeded",
		"invalid model",
	}
	for _, msg := range notRetryable {
		if isRetryableTransportFlip(msg) {
			t.Errorf("isRetryableTransportFlip(%q) = true, want false", msg)
		}
	}
}

// TestRetryPolicyBackoff pins the backoff schedule and the disable semantics.
func TestRetryPolicyBackoff(t *testing.T) {
	p := newRetryPolicy(2, 400*time.Millisecond)
	if !p.enabled() {
		t.Fatal("policy with max=2 must be enabled")
	}
	if got := p.backoffFor(1); got != 400*time.Millisecond {
		t.Errorf("backoffFor(1) = %v, want 400ms", got)
	}
	if got := p.backoffFor(2); got != 800*time.Millisecond {
		t.Errorf("backoffFor(2) = %v, want 800ms (base*n)", got)
	}

	off := newRetryPolicy(0, 400*time.Millisecond)
	if off.enabled() {
		t.Error("max=0 must disable the policy")
	}
	// A negative max (from a hand-written config) must not panic or enable.
	neg := newRetryPolicy(-5, 0)
	if neg.enabled() {
		t.Error("negative max must not enable retries")
	}
	if neg.baseBackoff != 400*time.Millisecond {
		t.Errorf("zero base backoff must fall back to the default, got %v", neg.baseBackoff)
	}
}

// TestGoCliRetriesTransportFlipThenSucceeds is the core acceptance test: the
// first attempt fails with a transport flip, the retry succeeds, and the CLIENT
// sees a normal completion. It also pins that the retry actually happened
// (call counting), not that the first attempt merely worked.
func TestGoCliRetriesTransportFlipThenSucceeds(t *testing.T) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	var generateCalls int
	f.responder = wrapWithCatalog(testCatalogJSON, func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		wire := decodePayload(t, capturedCall{payload: payload})
		url, _ := wire["url"].(string)
		switch {
		case strings.HasSuffix(url, "/alpha/generate"):
			generateCalls++
			if generateCalls == 1 {
				// First attempt: the upstream drops the connection mid-flight.
				return nil, errors.New("read tcp 10.0.0.1:443: connection reset by peer")
			}
			return hostOK(pluginapi.HTTPResponse{
				StatusCode: 200,
				Headers:    map[string][]string{"Content-Type": {"text/event-stream"}},
				Body:       []byte(cliGoStream),
			}), nil
		case isDeviceAnnounceURL(url):
			return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{}`)}), nil
		}
		return hostErr("test", "unrouted"), nil
	})

	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(retryYAML())); err != nil {
		t.Fatalf("register: %v", err)
	}

	env := mustExecute(t, m, "commandcode/glm-5.3", "openai", []byte(ccRequestBody))
	if !env.OK || env.Error != nil {
		t.Fatalf("a retried request must succeed, got error %+v", env.Error)
	}
	if generateCalls != 2 {
		t.Fatalf("generate attempts = %d, want 2 (one failed, one retried)", generateCalls)
	}
}

// TestGoCliDoesNotRetrySemanticStatus pins the guard in the other direction: an
// upstream 503 is a deliberate signal and must reach the client on the FIRST
// response, with no second attempt.
func TestGoCliDoesNotRetrySemanticStatus(t *testing.T) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	var generateCalls int
	f.responder = wrapWithCatalog(testCatalogJSON, func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		wire := decodePayload(t, capturedCall{payload: payload})
		url, _ := wire["url"].(string)
		switch {
		case strings.HasSuffix(url, "/alpha/generate"):
			generateCalls++
			return hostOK(pluginapi.HTTPResponse{
				StatusCode: 503,
				Headers:    map[string][]string{"Content-Type": {"application/json"}},
				Body:       []byte(`{"error":{"message":"overloaded"}}`),
			}), nil
		case isDeviceAnnounceURL(url):
			return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{}`)}), nil
		}
		return hostErr("test", "unrouted"), nil
	})

	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(retryYAML())); err != nil {
		t.Fatalf("register: %v", err)
	}

	env := mustExecute(t, m, "commandcode/glm-5.3", "openai", []byte(ccRequestBody))
	if env.OK || env.Error == nil {
		t.Fatalf("503 must surface as an error envelope, got %s", env.Result)
	}
	if generateCalls != 1 {
		t.Fatalf("generate attempts = %d, want 1 (a 503 is never retried)", generateCalls)
	}
	if env.Error.HTTPStatus != 503 {
		t.Errorf("client status = %d, want 503 (translated 1:1)", env.Error.HTTPStatus)
	}
}

// TestGoCliRetryExhaustionSurfacesError pins that a persistently flapping
// upstream fails after exactly max+1 attempts rather than looping forever.
func TestGoCliRetryExhaustionSurfacesError(t *testing.T) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	var generateCalls int
	f.responder = wrapWithCatalog(testCatalogJSON, func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		wire := decodePayload(t, capturedCall{payload: payload})
		url, _ := wire["url"].(string)
		switch {
		case strings.HasSuffix(url, "/alpha/generate"):
			generateCalls++
			return nil, errors.New("socket hang up")
		case isDeviceAnnounceURL(url):
			return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{}`)}), nil
		}
		return hostErr("test", "unrouted"), nil
	})

	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(retryYAML())); err != nil {
		t.Fatalf("register: %v", err)
	}

	env := mustExecute(t, m, "commandcode/glm-5.3", "openai", []byte(ccRequestBody))
	if env.OK || env.Error == nil {
		t.Fatalf("exhausted retries must surface an error, got %s", env.Result)
	}
	// Default max=2 => 3 attempts total. Pinned so a config change that
	// silently turns retries into an unbounded loop fails here.
	if generateCalls != 3 {
		t.Fatalf("generate attempts = %d, want 3 (1 + 2 retries)", generateCalls)
	}
	if !env.Error.Retryable {
		t.Errorf("a transport flip must stay client-retryable: %+v", env.Error)
	}
}

// TestRetryDisabledMakesSingleAttempt pins the off switch: retry.max=0 must
// mean exactly one attempt, so an operator debugging upstream behaviour can
// turn the mechanism off entirely.
func TestRetryDisabledMakesSingleAttempt(t *testing.T) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	var generateCalls int
	f.responder = wrapWithCatalog(testCatalogJSON, func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		wire := decodePayload(t, capturedCall{payload: payload})
		url, _ := wire["url"].(string)
		switch {
		case strings.HasSuffix(url, "/alpha/generate"):
			generateCalls++
			return nil, errors.New("connection reset by peer")
		case isDeviceAnnounceURL(url):
			return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{}`)}), nil
		}
		return hostErr("test", "unrouted"), nil
	})

	yaml := "retry:\n  max: 0\n" + retryYAML()
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(yaml)); err != nil {
		t.Fatalf("register: %v", err)
	}

	env := mustExecute(t, m, "commandcode/glm-5.3", "openai", []byte(ccRequestBody))
	if env.OK {
		t.Fatal("a disabled retry must still surface the failure")
	}
	if generateCalls != 1 {
		t.Fatalf("generate attempts = %d, want 1 with retry.max=0", generateCalls)
	}
}
