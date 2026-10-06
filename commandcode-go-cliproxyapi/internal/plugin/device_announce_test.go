package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// deviceAnnounceRoutesFor installs a responder that records every upstream URL
// and answers both the catalog and the CLI turn, so a test can assert exactly
// which endpoints were touched.
func deviceAnnounceRoutesFor(t *testing.T, seen *[]string) func(string, []byte) ([]byte, error) {
	t.Helper()
	return func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		wire := decodePayload(t, capturedCall{payload: payload})
		url, _ := wire["url"].(string)
		*seen = append(*seen, url)
		switch {
		case strings.HasSuffix(url, "/models"):
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(testCatalogJSON)}), nil
		case strings.HasSuffix(url, "/alpha/generate"):
			return hostOK(pluginapi.HTTPResponse{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       []byte(cliGoStream),
			}), nil
		case isDeviceAnnounceURL(url):
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{}`)}), nil
		}
		return hostErr("test", "unrouted"), nil
	}
}

// TestDeviceEnabledAnnouncesBeforeFirstGenerate pins the wiring itself: with
// the identity layer on, a go-cli turn must announce the device on the account
// surface before it generates. Without this, GenerateFingerprint is only a
// generator and the upstream never learns the device.
func TestDeviceEnabledAnnouncesBeforeFirstGenerate(t *testing.T) {
	var seen []string
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	f.responder = deviceAnnounceRoutesFor(t, &seen)
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(goCliYAML())); err != nil {
		t.Fatalf("register: %v", err)
	}

	if env := mustExecute(t, m, "commandcode/glm-5.3", "openai", []byte(ccRequestBody)); !env.OK {
		t.Fatalf("execute failed: %+v", env.Error)
	}

	var fingerprint, lifecycle, generate int
	for _, url := range seen {
		switch {
		case strings.HasSuffix(url, "/alpha/fingerprint/record"):
			fingerprint++
		case strings.HasSuffix(url, "/alpha/lifecycle-events"):
			lifecycle++
		case strings.HasSuffix(url, "/alpha/generate"):
			generate++
		}
	}
	if fingerprint != 1 {
		t.Errorf("fingerprint announcements = %d, want 1 (seen %v)", fingerprint, seen)
	}
	if lifecycle != 1 {
		t.Errorf("lifecycle announcements = %d, want 1 (seen %v)", lifecycle, seen)
	}
	if generate != 1 {
		t.Errorf("generate calls = %d, want 1 (seen %v)", generate, seen)
	}
}

// TestDeviceDisabledStopsEveryAnnouncement pins the off switch as ONE switch.
// An earlier revision dropped only x-project-slug, leaving the fingerprint and
// lifecycle announcements running — half an identity, which is worse than none
// because the upstream still sees a device this deployment never intended to
// present.
func TestDeviceDisabledStopsEveryAnnouncement(t *testing.T) {
	var seen []string
	yamlText := "device:\n  enabled: false\n" + goCliYAML()

	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	f.responder = deviceAnnounceRoutesFor(t, &seen)
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(yamlText)); err != nil {
		t.Fatalf("register: %v", err)
	}

	if env := mustExecute(t, m, "commandcode/glm-5.3", "openai", []byte(ccRequestBody)); !env.OK {
		t.Fatalf("execute failed: %+v", env.Error)
	}

	for _, url := range seen {
		if isDeviceAnnounceURL(url) {
			t.Errorf("device announcement sent while device.enabled=false: %s", url)
		}
	}
	// And the turn still happened: disabling identity must not disable serving.
	generate := 0
	for _, url := range seen {
		if strings.HasSuffix(url, "/alpha/generate") {
			generate++
		}
	}
	if generate != 1 {
		t.Errorf("generate calls = %d, want 1 while identity is off (seen %v)", generate, seen)
	}
}

// TestDeviceAnnouncementThrottledPerCredential pins the throttle: the
// announcement is a per-window registration, not a per-request call. Without
// it every turn would add two upstream calls.
func TestDeviceAnnouncementThrottledPerCredential(t *testing.T) {
	var seen []string
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	f.responder = deviceAnnounceRoutesFor(t, &seen)
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(goCliYAML())); err != nil {
		t.Fatalf("register: %v", err)
	}

	for i := 0; i < 3; i++ {
		if env := mustExecute(t, m, "commandcode/glm-5.3", "openai", []byte(ccRequestBody)); !env.OK {
			t.Fatalf("execute %d failed: %+v", i, env.Error)
		}
	}

	announcements := 0
	for _, url := range seen {
		if isDeviceAnnounceURL(url) {
			announcements++
		}
	}
	if announcements != 2 {
		t.Errorf("announcement calls = %d over three turns, want 2 (one pair); seen %v", announcements, seen)
	}
}

// TestProviderAccountDoesNotAnnounce pins that a provider-mode credential never
// sends a CLI device. The Provider API has no device concept, so an
// announcement there would invent a signal the transport contradicts.
func TestProviderAccountDoesNotAnnounce(t *testing.T) {
	var seen []string
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	f.responder = func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		wire := decodePayload(t, capturedCall{payload: payload})
		url, _ := wire["url"].(string)
		seen = append(seen, url)
		if strings.HasSuffix(url, "/models") {
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(testCatalogJSON)}), nil
		}
		return hostOK(pluginapi.HTTPResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(ccResponseBody),
		}), nil
	}
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	if env := mustExecute(t, m, "commandcode/glm-5.3", "openai", []byte(ccRequestBody)); !env.OK {
		t.Fatalf("execute failed: %+v", env.Error)
	}
	for _, url := range seen {
		if isDeviceAnnounceURL(url) {
			t.Errorf("provider account sent a CLI device announcement: %s", url)
		}
	}
}

// TestGoCliUpstreamStatusIsTranslated pins that a CLI status reaches the client
// as the status the client protocol expects: a quota wall (402) must read as a
// rate limit so a retrying SDK backs off, not as a payment error it ignores.
func TestGoCliUpstreamStatusIsTranslated(t *testing.T) {
	for _, tc := range []struct {
		upstream int
		wantHTTP int
	}{
		{402, http.StatusTooManyRequests},
		{403, http.StatusUnauthorized},
		{422, http.StatusBadRequest},
		{503, http.StatusServiceUnavailable},
	} {
		m, _ := goCliStreamManager(t, streamScript{}, func() ([]byte, error) {
			return hostOK(pluginapi.HTTPResponse{
				StatusCode: tc.upstream,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       []byte(`{"error":{"message":"wall"}}`),
			}), nil
		})
		resp, err := m.HandleCall("executor.execute",
			execReqBody("commandcode/glm-5.3", "openai", []byte(ccRequestBody), false))
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		env := decodeEnv(t, resp)
		if env.OK || env.Error == nil {
			t.Fatalf("upstream %d must surface as an error envelope, got %s", tc.upstream, resp)
		}
		if env.Error.HTTPStatus != tc.wantHTTP {
			t.Errorf("upstream %d -> client status %d, want %d", tc.upstream, env.Error.HTTPStatus, tc.wantHTTP)
		}
	}
}

// ensure the helpers above stay referenced in case of future refactors.
var _ = json.Marshal
