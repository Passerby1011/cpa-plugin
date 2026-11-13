// Package plugin — the Management API and the plugin identity.
package plugin

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/opendesign/resources"
)

// pluginLogoURL is the icon the panel and the auth rows show. It points at this
// repository rather than a vendor CDN, so an upstream URL change cannot break it.
const pluginLogoURL = "https://raw.githubusercontent.com/hex-ci/cpa-plugin/main/" + ProviderID + "/assets/logo.svg"

// managementBasePath is the host-injected management base, cached at
// management.register so a host path change does not need this plugin to guess.
//
// It is not only a route prefix: the panel is served from an UNAUTHENTICATED
// resource route, so it cannot learn the management base from the host at render
// time. The page is rendered with this value substituted in.
var managementBasePath = "/v0/management"

// panelResourcePath is the resource route the panel is served at.
//
// It must NOT be "/". The host trims a trailing slash off every declared
// resource path and then refuses the resulting empty path, so a route declared
// as "/" is dropped with a warning and the panel becomes unreachable. "/panel"
// is what the sibling plugins in this repository declare, and it resolves to
// /v0/resource/plugins/opendesign/panel.
const panelResourcePath = "/panel"

// managementRoute is one declared Management API route.
//
// Declared as a local struct rather than pluginapi.ManagementRoute: the SDK type
// carries a Handler field this plugin cannot fill in (the host attaches its own
// adapter after the response arrives), and marshalling it would put a null
// handler in the registration payload.
type managementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

// resourceRoute is one declared browser-navigable resource.
type resourceRoute struct {
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

// managementRegistration is what plugin.management.register answers.
type managementRegistration struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

// managementResponseBody is one route's answer.
//
// The host reads StatusCode/Headers/Body by those Go field names, so the shape
// is declared here rather than reusing pluginapi.ManagementResponse — the SDK
// type carries no JSON tags and would marshal every field capitalized.
// StatusCode 0 defaults to 200 in the host, so a miss states 404 explicitly
// instead of answering an empty 200 a page would read as a successful payload.
type managementResponseBody struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers,omitempty"`
	Body       []byte              `json:"Body,omitempty"`
}

// handleCountTokens answers executor.count_tokens.
//
// amr-link has no token-counting endpoint, and inventing a number would be a
// guess dressed as a measurement. A zero estimate is what a client falls back
// from to its own local count.
func (m *Manager) handleCountTokens(_ []byte) ([]byte, error) {
	return okEnvelope(pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":0}`)}), nil
}

// registerManagement declares this plugin's diagnostics routes and its panel.
func (m *Manager) registerManagement(request []byte) ([]byte, error) {
	var req pluginapi.ManagementRegistrationRequest
	if err := json.Unmarshal(request, &req); err == nil {
		if p := strings.TrimSpace(req.BasePath); p != "" {
			managementBasePath = p
		}
	}
	base := "/plugins/" + ProviderID
	return okEnvelope(managementRegistration{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: base + "/status", Description: "Accounts, planes and served model counts. Counts and names only — never a key."},
			{Method: http.MethodGet, Path: base + "/models", Description: "The served model list: each model's wire, limits and measured reasoning levels."},
		},
		Resources: []resourceRoute{
			{Path: panelResourcePath, Menu: displayName, Description: "Open Design accounts and served models."},
		},
	}), nil
}

// handleManagement answers the Management API routes and the panel resource.
//
// The host dispatches authenticated Management API routes and unauthenticated
// resource routes through this same method, so which one arrived is decided by
// the path. The panel branch is matched first because it lives under a different
// base and must not fall through to a JSON route.
func (m *Manager) handleManagement(request []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed management request body"), nil
	}
	path := strings.TrimRight(strings.TrimSpace(req.Path), "/")

	resourceBase := "/v0/resource/plugins/" + ProviderID
	if path == resourceBase+panelResourcePath {
		if req.Method != "" && !strings.EqualFold(req.Method, http.MethodGet) {
			return okEnvelope(methodNotAllowed()), nil
		}
		return okEnvelope(htmlResponse(m.panelPage())), nil
	}

	managementBase := strings.TrimRight(managementBasePath, "/") + "/plugins/" + ProviderID
	switch path {
	case managementBase + "/status":
		return okEnvelope(m.status()), nil
	case managementBase + "/models":
		return okEnvelope(m.servedModels()), nil
	default:
		return okEnvelope(notFound()), nil
	}
}

// htmlResponse renders the panel as a page.
func htmlResponse(body []byte) managementResponseBody {
	return managementResponseBody{
		StatusCode: http.StatusOK,
		Headers:    map[string][]string{"Content-Type": {"text/html; charset=utf-8"}},
		Body:       body,
	}
}

// notFound is the answer for a path this plugin does not serve.
func notFound() managementResponseBody {
	return managementResponseBody{
		StatusCode: http.StatusNotFound,
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       []byte(`{"error":"not found"}`),
	}
}

// methodNotAllowed is the answer for a non-GET request on a GET-only route.
func methodNotAllowed() managementResponseBody {
	return managementResponseBody{
		StatusCode: http.StatusMethodNotAllowed,
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       []byte(`{"error":"method not allowed"}`),
	}
}

// panelPage returns the embedded page with the management base substituted in.
//
// That substitution is the page's only dynamic part: everything it renders comes
// from the JSON routes above, so no credential, account or model list is baked
// into the HTML and the page can be checked into the repository as a static file.
func (m *Manager) panelPage() []byte {
	return resources.PanelWithBase(strings.TrimRight(managementBasePath, "/") + "/plugins/" + ProviderID)
}

// servedModels is the flattened list the panel renders.
func (m *Manager) servedModels() map[string]any {
	m.mu.RLock()
	seed := m.seed
	m.mu.RUnlock()
	type row struct {
		ID       string   `json:"id"`
		Upstream string   `json:"upstream"`
		Name     string   `json:"name"`
		API      string   `json:"api"`
		Context  int64    `json:"context"`
		Output   int64    `json:"output"`
		Efforts  []string `json:"efforts"`
		Input    []string `json:"input"`
		Thinking bool     `json:"thinking"`
	}
	rows := make([]row, 0)
	if seed != nil {
		for _, e := range seed.Entries {
			rows = append(rows, row{
				ID: e.PublicID, Upstream: e.UpstreamID, Name: e.Name, API: string(e.API),
				Context: e.Context, Output: e.Output, Efforts: e.Efforts,
				Input: e.InputModalities(), Thinking: e.Thinking,
			})
		}
	}
	return map[string]any{"provider": ProviderID, "models": rows}
}

// status is the diagnostics view: what the plugin serves and how it was produced.
// It reports counts and names only — never a key.
func (m *Manager) status() map[string]any {
	m.mu.RLock()
	cfg, seed := m.cfg, m.seed
	live := 0
	for _, snap := range m.snapshots {
		if snap != nil && snap.Live {
			live++
		}
	}
	m.mu.RUnlock()
	accounts := m.accountsOf(cfg)
	withPlan := 0
	for _, a := range accounts {
		if strings.HasPrefix(strings.TrimSpace(a.ControlKey), "vela_ctrl_") {
			withPlan++
		}
	}
	served := 0
	if seed != nil {
		served = len(seed.Entries)
	}
	return map[string]any{
		"provider":           ProviderID,
		"version":            pluginVersion,
		"base_url":           cfg.BaseURL,
		"control_url":        cfg.ControlURL,
		"prefix":             cfg.ModelPrefix.Value,
		"prefix_enabled":     cfg.ModelPrefix.Enabled,
		"accounts":           len(accounts),
		"accounts_with_plan": withPlan,
		"live_catalogs":      live,
		"seed_models":        served,
		"panel_path":         "/v0/resource/plugins/" + ProviderID + panelResourcePath,
	}
}
