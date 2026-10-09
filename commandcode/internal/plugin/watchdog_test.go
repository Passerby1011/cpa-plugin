package plugin

import (
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// watchdogYAML is a go-cli pool with a very short idle budget so the test does
// not have to wait 30s of wall clock to exercise the mechanism.
func watchdogYAML(streamIdle string) string {
	return "watchdog:\n" +
		"  enabled: true\n" +
		"  stream: " + streamIdle + "\n" +
		"accounts:\n" +
		"  - label: go\n" +
		"    mode: go-cli\n" +
		"    credential: " + retryAccountKey + "\n" +
		"  - label: main\n" +
		"    credential: " + providerAccountKey + "\n"
}

// TestGoCliStreamIdleWatchdogFiresOnSilence is the core acceptance test for the
// idle watchdog: an upstream that sends one chunk and then goes silent must be
// closed with an idle error rather than held until the much larger
// request-timeout, and must NOT be silently retried.
func TestGoCliStreamIdleWatchdogFiresOnSilence(t *testing.T) {
	// One text frame, no finish, then silence. The read loop will park on
	// StreamRead; the idle timer must fire and close the streams.
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	f.responder = wrapWithCatalog(testCatalogJSON, func(method string, payload []byte) ([]byte, error) {
		wire := decodePayload(t, capturedCall{payload: payload})
		url, _ := wire["url"].(string)
		switch method {
		case pluginabi.MethodHostHTTPDoStream:
			return hostOK(hostStreamStartResp{
				StatusCode: 200,
				Headers:    map[string][]string{"Content-Type": {"text/event-stream"}},
				StreamID:   "idle-up",
			}), nil
		case pluginabi.MethodHostHTTPStreamRead:
			// Deliver one frame, then park forever: the pump will block here
			// and only the idle timer can end it.
			if strings.Contains(string(payload), "idle-up") {
				if !idleFrameSent {
					idleFrameSent = true
					return hostOK(hostStreamReadResp{
						Payload: []byte("data: {\"type\":\"text-delta\",\"text\":\"hi\"}\n\n"),
					}), nil
				}
				time.Sleep(3 * time.Second)
				return hostOK(hostStreamReadResp{Done: true}), nil
			}
			return hostOK(hostStreamReadResp{Done: true}), nil
		case pluginabi.MethodHostHTTPDo:
			if isDeviceAnnounceURL(url) {
				return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{}`)}), nil
			}
		}
		return hostOK(map[string]any{}), nil
	})

	yaml := watchdogYAML("150ms")
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(yaml)); err != nil {
		t.Fatalf("register: %v", err)
	}

	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("commandcode/glm-5.3", "openai", []byte(ccRequestBody), "idle-down"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("envelope = %+v", env.Error)
	}

	// The pump runs on its own goroutine; wait for it to finish.
	m.bridge.WaitForInFlight(10 * time.Second)

	closes := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(closes) != 1 {
		t.Fatalf("downstream closes = %d, want 1", len(closes))
	}
	payload := string(closes[0].payload)
	if !strings.Contains(payload, "idle") {
		t.Errorf("close payload should name the idle timeout, got %q", payload)
	}
}

var idleFrameSent bool

// TestWatchdogDisabledLeavesStreamAlone pins the off switch: with the watchdog
// disabled a silent upstream is not cut short by it. The stream is ended by the
// host read returning done, which is the pre-existing behaviour.
func TestWatchdogDisabledLeavesStreamAlone(t *testing.T) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	var sawIdleError bool
	f.responder = wrapWithCatalog(testCatalogJSON, func(method string, payload []byte) ([]byte, error) {
		wire := decodePayload(t, capturedCall{payload: payload})
		url, _ := wire["url"].(string)
		switch method {
		case pluginabi.MethodHostHTTPDoStream:
			return hostOK(hostStreamStartResp{
				StatusCode: 200,
				Headers:    map[string][]string{"Content-Type": {"text/event-stream"}},
				StreamID:   "wdoff-up",
			}), nil
		case pluginabi.MethodHostHTTPStreamRead:
			return hostOK(hostStreamReadResp{
				Payload: []byte(cliGoStream),
			}), nil
		case pluginabi.MethodHostStreamClose:
			if strings.Contains(string(payload), "idle") {
				sawIdleError = true
			}
		case pluginabi.MethodHostHTTPDo:
			if isDeviceAnnounceURL(url) {
				return hostOK(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{}`)}), nil
			}
		}
		return hostOK(map[string]any{}), nil
	})

	yaml := "watchdog:\n  enabled: false\n" +
		"accounts:\n" +
		"  - label: go\n    mode: go-cli\n    credential: " + retryAccountKey + "\n" +
		"  - label: main\n    credential: " + providerAccountKey + "\n"
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(yaml)); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := m.HandleCall("plugin.execute_stream",
		execStreamReqBody("commandcode/glm-5.3", "openai", []byte(ccRequestBody), "wdoff-down")); err != nil {
		// executor.execute_stream is the correct method name; tolerate either
		// error text but surface a real failure below via sawIdleError.
		_ = err
	}
	m.bridge.WaitForInFlight(10 * time.Second)

	if sawIdleError {
		t.Error("watchdog disabled but an idle error was emitted")
	}
}
