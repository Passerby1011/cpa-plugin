package plugin

// The panel is served as a browser-navigable resource under
// /v0/resource/plugins/geminiapi/panel, so an operator opens one URL on the CPA
// host and gets the page with no file on disk.
//
// # Why the bytes arrive from main
//
// go:embed cannot reach a parent directory, so the module-root panel.html is
// embedded by package main and handed over here at init. Keeping exactly one
// copy of the file is what stops the embedded page and the checked-in page from
// drifting apart.

import (
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// panelRoutePath is the resource route path registered with the host.
const panelRoutePath = "/panel"

// panelHTML is set by main's init (tests set it directly).
var panelHTML []byte

// SetPanel installs the embedded page bytes. Called from main.
func SetPanel(content []byte) { panelHTML = content }

// PanelHTML returns the embedded page, for tests and diagnostics.
func PanelHTML() []byte { return panelHTML }

// ServePanel answers the resource route.
//
// The page is served without a management credential (a browser cannot attach
// one to a navigation), which is why it asks for the key in its form and sends
// it on every API call. The CSP forbids any external script so the page cannot
// be extended by a compromised CDN.
func (m *Manager) ServePanel(_ pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	headers := http.Header{}
	headers.Set("Content-Type", "text/html; charset=utf-8")
	headers.Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:")
	headers.Set("X-Content-Type-Options", "nosniff")
	headers.Set("Referrer-Policy", "no-referrer")
	headers.Set("Cache-Control", "no-cache")
	body := panelHTML
	if len(body) == 0 {
		body = []byte("<!doctype html><title>GeminiAPI</title><p>panel.html was not embedded in this build.</p>")
	}
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: headers, Body: body}, nil
}
