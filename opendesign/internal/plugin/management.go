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
var managementBasePath = "/v0/management"

// handleCountTokens answers executor.count_tokens.
//
// amr-link has no token-counting endpoint, and inventing a number would be a
// guess dressed as a measurement. A zero estimate is what a client falls back
// from to its own local count.
func (m *Manager) handleCountTokens(_ []byte) ([]byte, error) {
	return okEnvelope(pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":0}`)}), nil
}

// registerManagement declares this plugin's diagnostics route and its panel.
func (m *Manager) registerManagement(request []byte) ([]byte, error) {
	var req pluginapi.ManagementRegistrationRequest
	if err := json.Unmarshal(request, &req); err == nil {
		if p := strings.TrimSpace(req.BasePath); p != "" {
			managementBasePath = p
		}
	}
	return okEnvelope(struct {
		Routes    []pluginapi.ManagementRoute `json:"routes"`
		Resources []pluginapi.ResourceRoute   `json:"resources"`
	}{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: "/plugins/" + ProviderID + "/status"},
			{Method: http.MethodGet, Path: "/plugins/" + ProviderID + "/models"},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: "/", Menu: displayName, Description: "Open Design accounts and served models."},
		},
	}), nil
}

// handleManagement answers the management routes.
func (m *Manager) handleManagement(request []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed management request body"), nil
	}
	switch {
	case strings.HasSuffix(req.Path, "/status"):
		return okEnvelope(m.status()), nil
	case strings.HasSuffix(req.Path, "/models"):
		return okEnvelope(m.servedModels()), nil
	default:
		return okEnvelope(struct{}{}), nil
	}
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
	view := map[string]any{
		"provider":           ProviderID,
		"version":            pluginVersion,
		"base_url":           cfg.BaseURL,
		"control_url":        cfg.ControlURL,
		"prefix":             cfg.ModelPrefix.Value,
		"prefix_enabled":     cfg.ModelPrefix.Enabled,
		"accounts":           len(accounts),
		"accounts_with_plan": withPlan,
		"live_catalogs":      live,
	}
	if seed != nil {
		view["seed_models"] = len(seed.Entries)
	}
	return view
}

// panelPageBody is the embedded resource page. The page itself is a static
// informational panel: models, accounts, and where the plan allowance lives.
func panelPageBody() []byte { return resources.Panel() }
