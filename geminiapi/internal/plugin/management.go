package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Management API: the backend supervisor's control surface plus the panel.
//
// Routes (all under /v0/management/plugins/geminiapi/):
//
//	GET  /status          – backend state, health, versions, catalog summary
//	POST /backend/status  – same, forced fresh probe (page refresh)
//	POST /backend/install – download + verify only (no start)
//	POST /backend/start   – install if needed, then start + wait for health
//	POST /backend/stop    – stop the supervised process group
//	POST /accounts        – import a Google cookie / account JSON into the
//	                        backend's own account pool and probe it
//	GET  /accounts        – pool counts ONLY (never ids, labels or cookies)
//	GET  /models          – published catalog + why anything was skipped
//
// The panel itself is a browser-navigable resource at
// /v0/resource/plugins/geminiapi/panel.

const (
	statusPath   = "/plugins/" + pluginName + "/status"
	backendPath  = "/plugins/" + pluginName + "/backend"
	accountsPath = "/plugins/" + pluginName + "/accounts"
	modelsPath   = "/plugins/" + pluginName + "/models"
	panelPath    = "/panel"
)

// shutdownDrainTimeout bounds how long shutdown waits for in-flight host
// callbacks before returning.
var shutdownDrainTimeout = 15 * time.Second

// registerManagement declares the routes and the panel resource.
func (m *Manager) registerManagement(request []byte) ([]byte, error) {
	var req struct {
		Plugin           pluginapi.Metadata `json:"Plugin"`
		BasePath         string             `json:"BasePath"`
		ResourceBasePath string             `json:"ResourceBasePath"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed management registration request body"), nil
	}
	return okEnvelope(struct {
		Routes []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"routes"`
		Resources []struct {
			Path        string `json:"path"`
			Menu        string `json:"menu"`
			Description string `json:"description"`
		} `json:"resources"`
	}{
		Routes: []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		}{
			{Method: http.MethodGet, Path: statusPath},
			{Method: http.MethodPost, Path: backendPath + "/status"},
			{Method: http.MethodPost, Path: backendPath + "/install"},
			{Method: http.MethodPost, Path: backendPath + "/start"},
			{Method: http.MethodPost, Path: backendPath + "/stop"},
			{Method: http.MethodGet, Path: accountsPath},
			{Method: http.MethodPost, Path: accountsPath},
			{Method: http.MethodGet, Path: modelsPath},
		},
		Resources: []struct {
			Path        string `json:"path"`
			Menu        string `json:"menu"`
			Description string `json:"description"`
		}{
			{Path: panelPath, Menu: "GeminiAPI", Description: "GeminiAPI backend: local service control, AI Studio account import, published models."},
		},
	}), nil
}

// handleManagement dispatches one management request.
func (m *Manager) handleManagement(request []byte) ([]byte, error) {
	var req struct {
		pluginapi.ManagementRequest
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed management request body"), nil
	}
	resp, err := m.handleManagementRequest(req.ManagementRequest)
	if err != nil {
		return ErrEnvelope("management_failure", err.Error()), nil
	}
	return okEnvelope(resp), nil
}

// handleManagementRequest is the testable core: it takes a ManagementRequest
// and returns a ManagementResponse.
func (m *Manager) handleManagementRequest(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	path := normalizeManagementPath(req.Path)
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}

	switch {
	case path == statusPath && (method == http.MethodGet || method == http.MethodPost):
		return m.statusResponse(req)
	case path == backendPath+"/install" && method == http.MethodPost:
		return m.installResponse(req)
	case path == backendPath+"/start" && method == http.MethodPost:
		return m.startResponse(req)
	case path == backendPath+"/stop" && method == http.MethodPost:
		return m.stopResponse(req)
	case path == accountsPath && method == http.MethodGet:
		return m.accountsResponse(req)
	case path == accountsPath && method == http.MethodPost:
		return m.importAccountResponse(req)
	case path == modelsPath && method == http.MethodGet:
		return m.modelsResponse(req)
	case path == panelRoutePath && method == http.MethodGet:
		// The resource route reaches the panel through the same handler, so the
		// page works both as a resource and via the management namespace.
		return m.ServePanel(req)
	}
	return jsonResponse(http.StatusNotFound, map[string]any{"error": "no such route: " + path})
}

// normalizeManagementPath strips the /v0/management prefix and any trailing
// slash so both shapes the host may pass match.
func normalizeManagementPath(path string) string {
	path = strings.TrimSpace(path)
	if idx := strings.Index(path, "/plugins/"); idx >= 0 {
		path = path[idx:]
	} else if strings.HasPrefix(path, "plugins/") {
		path = "/" + path
	}
	path = strings.TrimRight(path, "/")
	if path == "" {
		return "/"
	}
	return path
}

// statusResponse reports the backend and catalog state. It is the page's
// polling endpoint and the JSON an operator can curl.
func (m *Manager) statusResponse(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	status := m.snapshotStatus(req)
	return jsonResponse(http.StatusOK, status)
}

// installResponse downloads and verifies without starting.
func (m *Manager) installResponse(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	m.mu.RLock()
	mgr := m.backendMgr
	external := m.skipSupervision
	m.mu.RUnlock()
	if external {
		return jsonResponse(http.StatusBadRequest, map[string]any{
			"error": "ai-studio.base-url is configured, so this plugin does not manage a backend and cannot install one; " +
				"clear base-url to let the plugin install and supervise its own backend",
		})
	}
	if mgr == nil {
		return jsonResponse(http.StatusConflict, map[string]any{"error": "the plugin is not configured yet"})
	}
	ctx, cancel := contextWithTimeout(installTimeout)
	defer cancel()
	path, err := mgr.Install(ctx)
	if err != nil {
		return jsonResponse(ensureHTTPStatus(err), map[string]any{"error": err.Error(), "status": m.snapshotStatus(req)})
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"ok":     true,
		"path":   path,
		"status": m.snapshotStatus(req),
	})
}

// startResponse installs if needed and starts, waiting for health.
func (m *Manager) startResponse(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	m.mu.RLock()
	mgr := m.backendMgr
	external := m.skipSupervision
	m.mu.RUnlock()
	if external {
		return jsonResponse(http.StatusBadRequest, map[string]any{
			"error": "ai-studio.base-url is configured: the plugin does not own that backend and cannot start it",
		})
	}
	if mgr == nil {
		return jsonResponse(http.StatusConflict, map[string]any{"error": "the plugin is not configured yet"})
	}
	ctx, cancel := contextWithTimeout(startTimeoutBudget)
	defer cancel()
	if _, err := mgr.Start(ctx); err != nil {
		return jsonResponse(ensureHTTPStatus(err), map[string]any{"error": err.Error(), "status": m.snapshotStatus(req)})
	}
	// A successful start invalidates the catalog: read it now so the models
	// list matches what the backend actually serves.
	refreshCtx, cancelRefresh := contextWithTimeout(registerRefreshTimeout)
	defer cancelRefresh()
	if _, err := m.refreshCatalog(refreshCtx); err != nil {
		_ = m.log("warn", "catalog refresh after start failed: "+err.Error(), nil)
	}
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "status": m.snapshotStatus(req)})
}

// stopResponse stops the supervised process group.
func (m *Manager) stopResponse(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	m.mu.RLock()
	mgr := m.backendMgr
	m.mu.RUnlock()
	if mgr == nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{
			"error": "ai-studio.base-url is configured: the plugin does not own that backend and cannot stop it",
		})
	}
	ctx, cancel := contextWithTimeout(stopTimeoutBudget)
	defer cancel()
	if _, err := mgr.Stop(ctx); err != nil {
		return jsonResponse(ensureHTTPStatus(err), map[string]any{"error": err.Error(), "status": m.snapshotStatus(req)})
	}
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "status": m.snapshotStatus(req)})
}

// accountsResponse reports pool COUNTS only. Account ids, labels and cookies
// never leave the backend, so a page refresh cannot leak one.
func (m *Manager) accountsResponse(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	m.mu.RLock()
	mgr := m.backendMgr
	external := m.skipSupervision
	m.mu.RUnlock()
	payload := map[string]any{
		"known":   false,
		"managed": !external,
		"note": "Account identifiers and cookies stay in the backend's own database. This endpoint reports counts only, " +
			"and which Google account serves a request is decided by the backend's pool, not by any CPA auth record.",
	}
	if external {
		payload["error"] = "an external backend's account pool is not reachable from this plugin"
		return jsonResponse(http.StatusOK, payload)
	}
	if mgr == nil {
		payload["error"] = "the plugin is not configured yet"
		return jsonResponse(http.StatusOK, payload)
	}
	status := m.snapshotStatus(req)
	payload["accounts"] = status.Accounts
	return jsonResponse(http.StatusOK, payload)
}

// modelsResponse reports the published catalog and the skip reasons.
func (m *Manager) modelsResponse(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	m.mu.RLock()
	result := m.catalog
	at := m.modelsAt
	m.mu.RUnlock()
	payload := map[string]any{
		"count":            len(result.Records),
		"models":           result.Records,
		"skipped":          result.Skipped,
		"refresh_interval": catalogRefreshInterval.String(),
	}
	if !at.IsZero() {
		payload["fetched_at"] = at.Format(timeLayout)
	}
	return jsonResponse(http.StatusOK, payload)
}

func jsonResponse(status int, payload any) (pluginapi.ManagementResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return pluginapi.ManagementResponse{}, err
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}, nil
}
