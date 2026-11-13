package plugin

// Management API: each backend supervisor's control surface plus the panel.
//
// Routes (all under /v0/management/plugins/geminiapi/):
//
//	GET  /status          – both namespaces' state, health, versions, catalogs
//	POST /backend/status  – same, forced fresh probe (page refresh)
//	POST /backend/install – download + verify only (no start)
//	POST /backend/start   – install if needed, then start + wait for health
//	POST /backend/stop    – stop the supervised process group
//	POST /accounts        – add an account to the backend's own pool (its own
//	                        mechanism: a pasted Cookie for gemini-web, a browser
//	                        sign-in task for AI Studio)
//	GET  /accounts        – pool counts ONLY (never ids, labels or cookies)
//	GET  /models          – published catalog + why anything was skipped
//
// Every backend route takes a `namespace` selector (query string, or a
// "namespace" field in the JSON body). There is no default: acting on the
// wrong backend is the failure this split exists to prevent, so a request that
// does not name a namespace is refused rather than guessed at.
//
// The panel itself is a browser-navigable resource at
// /v0/resource/plugins/geminiapi/panel.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/geminiapi/internal/backend"
	"github.com/hex-ci/cpa-plugin/geminiapi/internal/catalog"
	"github.com/hex-ci/cpa-plugin/geminiapi/internal/config"
)

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
			{Path: panelPath, Menu: "GeminiAPI", Description: "GeminiAPI backends: two namespaces (ai-studio / gemini-web), each with its own service control, account import and published models."},
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

// namespaceFrom selects the namespace a management request acts on.
//
// The order is: the "namespace" query parameter, then a "namespace" field in the
// JSON body. There is deliberately NO default: acting on the wrong backend is
// exactly the failure this plugin's split exists to prevent, so a route that
// needs a namespace refuses rather than guesses.
func namespaceFrom(req pluginapi.ManagementRequest) (string, error) {
	if req.Query != nil {
		if ns := strings.TrimSpace(req.Query.Get("namespace")); ns != "" {
			return ns, nil
		}
	}
	if len(req.Body) > 0 {
		var body struct {
			Namespace string `json:"namespace"`
		}
		if err := json.Unmarshal(req.Body, &body); err == nil && strings.TrimSpace(body.Namespace) != "" {
			return strings.TrimSpace(body.Namespace), nil
		}
	}
	return "", nil
}

// setNamespaceQuery appends the namespace selector to a paginated URL, so a
// second page load (or a refresh) keeps acting on the same backend.
func setNamespaceQuery(query url.Values, namespace string) url.Values {
	if query == nil {
		query = url.Values{}
	}
	query.Set("namespace", namespace)
	return query
}

// namespaceFleet is the per-namespace state a management action operates on.
type namespaceFleet struct {
	namespace string
	flavor    backend.Flavor
	cfg       config.NamespaceConfig
	manager   *backend.Manager
	external  bool
}

// fleetFor resolves the namespace a management request names.
func (m *Manager) fleetFor(req pluginapi.ManagementRequest) (namespaceFleet, error) {
	ns, err := namespaceFrom(req)
	if err != nil {
		return namespaceFleet{}, err
	}
	if ns == "" {
		return namespaceFleet{}, &namespaceRequiredError{}
	}
	m.mu.RLock()
	cfg := m.cfg
	mgr := m.backends[ns]
	external := m.externals[ns]
	m.mu.RUnlock()
	nsCfg, cfgErr := cfg.Namespace(ns)
	if cfgErr != nil {
		return namespaceFleet{}, cfgErr
	}
	flavor, flavorErr := backend.FlavorFor(backend.FlavorID(ns))
	if flavorErr != nil {
		return namespaceFleet{}, flavorErr
	}
	return namespaceFleet{namespace: ns, flavor: flavor, cfg: nsCfg, manager: mgr, external: external}, nil
}

// namespaceRequiredError tells a caller that the selector is missing, listing
// the namespaces so a script can act on the message.
type namespaceRequiredError struct{}

func (e *namespaceRequiredError) Error() string {
	return "this route acts on ONE backend and needs a namespace selector: add ?namespace=" + config.NamespaceAIStudio +
		" or ?namespace=" + config.NamespaceGeminiWeb + " (or a \"namespace\" field in the JSON body); " +
		"the plugin will not guess which backend you meant"
}

// statusResponse reports both backends and both catalogs. It is the page's
// polling endpoint and the JSON an operator can curl.
func (m *Manager) statusResponse(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	return jsonResponse(http.StatusOK, m.snapshotStatus(req))
}

// installResponse downloads and verifies one namespace's binary without
// starting it.
func (m *Manager) installResponse(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	fleet, err := m.fleetFor(req)
	if err != nil {
		return jsonResponse(statusForManagementError(err), map[string]any{"error": err.Error(), "namespace": fleet.namespace})
	}
	if fleet.external {
		return jsonResponse(http.StatusBadRequest, map[string]any{
			"namespace": fleet.namespace,
			"error": fleet.namespace + ".base-url is configured, so this plugin does not manage a backend and cannot install one; " +
				"clear base-url to let the plugin install and supervise its own backend",
		})
	}
	if fleet.manager == nil {
		return jsonResponse(http.StatusConflict, map[string]any{"namespace": fleet.namespace, "error": "the plugin is not configured yet"})
	}
	ctx, cancel := contextWithTimeout(installTimeout)
	defer cancel()
	path, instErr := fleet.manager.Install(ctx)
	if instErr != nil {
		return jsonResponse(ensureHTTPStatus(instErr), map[string]any{
			"namespace": fleet.namespace, "error": instErr.Error(), "status": m.snapshotStatus(req),
		})
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"ok": true, "namespace": fleet.namespace, "path": path, "status": m.snapshotStatus(req),
	})
}

// startResponse installs if needed and starts one namespace's backend, waiting
// for health.
func (m *Manager) startResponse(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	fleet, err := m.fleetFor(req)
	if err != nil {
		return jsonResponse(statusForManagementError(err), map[string]any{"error": err.Error(), "namespace": fleet.namespace})
	}
	if fleet.external {
		return jsonResponse(http.StatusBadRequest, map[string]any{
			"namespace": fleet.namespace,
			"error":     fleet.namespace + ".base-url is configured: the plugin does not own that backend and cannot start it",
		})
	}
	if fleet.manager == nil {
		return jsonResponse(http.StatusConflict, map[string]any{"namespace": fleet.namespace, "error": "the plugin is not configured yet"})
	}
	ctx, cancel := contextWithTimeout(startTimeoutBudget)
	defer cancel()
	if _, startErr := fleet.manager.Start(ctx); startErr != nil {
		return jsonResponse(ensureHTTPStatus(startErr), map[string]any{
			"namespace": fleet.namespace, "error": startErr.Error(), "status": m.snapshotStatus(req),
		})
	}
	// A successful start invalidates that namespace's catalog: read it now so
	// the models list matches what the backend actually serves.
	refreshCtx, cancelRefresh := contextWithTimeout(registerRefreshTimeout)
	defer cancelRefresh()
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()
	if refreshErr := m.refreshNamespace(refreshCtx, cfg, fleet.namespace); refreshErr != nil {
		_ = m.log("warn", "catalog refresh after start failed for "+fleet.namespace+": "+refreshErr.Error(), nil)
	}
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "namespace": fleet.namespace, "status": m.snapshotStatus(req)})
}

// stopResponse stops one namespace's supervised process group.
func (m *Manager) stopResponse(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	fleet, err := m.fleetFor(req)
	if err != nil {
		return jsonResponse(statusForManagementError(err), map[string]any{"error": err.Error(), "namespace": fleet.namespace})
	}
	if fleet.external {
		return jsonResponse(http.StatusBadRequest, map[string]any{
			"namespace": fleet.namespace,
			"error":     fleet.namespace + ".base-url is configured: the plugin does not own that backend and cannot stop it",
		})
	}
	if fleet.manager == nil {
		return jsonResponse(http.StatusConflict, map[string]any{"namespace": fleet.namespace, "error": "the plugin is not configured yet"})
	}
	ctx, cancel := contextWithTimeout(stopTimeoutBudget)
	defer cancel()
	if _, stopErr := fleet.manager.Stop(ctx); stopErr != nil {
		return jsonResponse(ensureHTTPStatus(stopErr), map[string]any{
			"namespace": fleet.namespace, "error": stopErr.Error(), "status": m.snapshotStatus(req),
		})
	}
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "namespace": fleet.namespace, "status": m.snapshotStatus(req)})
}

// accountsResponse reports pool COUNTS only. Account ids, labels and cookies
// never leave the backend, so a page refresh cannot leak one.
func (m *Manager) accountsResponse(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	payload := map[string]any{
		"known":   false,
		"managed": true,
		"note": "Account identifiers and credentials stay in the backend's own store. This endpoint reports counts only, " +
			"and which Google account serves a request is decided by the backend's pool, not by any CPA auth record.",
	}
	namespace, nsErr := namespaceFrom(req)
	if nsErr != nil {
		return jsonResponse(http.StatusBadRequest, payload)
	}
	if namespace == "" {
		// Without a selector, report BOTH namespaces: a GET is a read, and
		// reading both is what the page wants. An unreadable namespace gets its
		// reason rather than being omitted.
		status := m.snapshotStatus(req)
		sections := make([]map[string]any, 0, len(status.Backends))
		for _, section := range status.Backends {
			sections = append(sections, map[string]any{
				"namespace": section.Name,
				"managed":   section.Managed,
				"known":     section.Accounts.Counts.Known,
				"counts":    section.Accounts.Counts,
				"import":    section.Accounts.ImportKind,
				"models":    section.Models,
			})
		}
		payload["backends"] = sections
		return jsonResponse(http.StatusOK, payload)
	}
	fleet, fleetErr := m.fleetFor(req)
	if fleetErr != nil {
		payload["namespace"] = namespace
		payload["error"] = fleetErr.Error()
		return jsonResponse(statusForManagementError(fleetErr), payload)
	}
	payload["namespace"] = fleet.namespace
	payload["managed"] = !fleet.external
	payload["import"] = importKindFor(fleet.flavor.ID)
	if fleet.external {
		payload["error"] = "an external backend's account pool is not reachable from this plugin"
		return jsonResponse(http.StatusOK, payload)
	}
	if fleet.manager == nil {
		payload["error"] = "the plugin is not configured yet"
		return jsonResponse(http.StatusOK, payload)
	}
	status := m.snapshotStatus(req)
	section, ok := status.namespaceSection(fleet.namespace)
	if !ok {
		payload["error"] = "no status for namespace " + fleet.namespace
		return jsonResponse(http.StatusOK, payload)
	}
	payload["known"] = section.Accounts.Counts.Known
	payload["accounts"] = section.Accounts.Counts
	return jsonResponse(http.StatusOK, payload)
}

// modelsResponse reports the published catalogs and the skip reasons per
// namespace.
func (m *Manager) modelsResponse(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	m.mu.RLock()
	catalogs := m.catalogs
	catalogErrs := m.catalogErrs
	modelsAt := m.modelsAt
	m.mu.RUnlock()

	sections := make([]map[string]any, 0, len(catalog.Namespaces))
	total := 0
	for _, ns := range catalog.Namespaces {
		res := catalogs[ns.Name]
		total += len(res.Records)
		section := map[string]any{
			"namespace":        ns.Name,
			"backend":          string(ns.Backend),
			"count":            len(res.Records),
			"models":           res.Records,
			"skipped":          res.Skipped,
			"notes":            res.Notes,
			"refresh_interval": catalogRefreshInterval.String(),
		}
		if errText := catalogErrs[ns.Name]; errText != "" {
			section["error"] = errText
		}
		if at := modelsAt[ns.Name]; !at.IsZero() {
			section["fetched_at"] = at.Format(timeLayout)
		}
		sections = append(sections, section)
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"count":      total,
		"namespaces": sections,
	})
}

// statusForManagementError maps a fleet lookup failure to a status code.
func statusForManagementError(err error) int {
	if err == nil {
		return http.StatusOK
	}
	var required *namespaceRequiredError
	if asNamespaceRequired(err, &required) {
		return http.StatusBadRequest
	}
	if strings.Contains(strings.ToLower(err.Error()), "unknown namespace") {
		return http.StatusBadRequest
	}
	return ensureHTTPStatus(err)
}

// asNamespaceRequired is errors.As without importing errors in this file.
func asNamespaceRequired(err error, target **namespaceRequiredError) bool {
	if err == nil {
		return false
	}
	if typed, ok := err.(*namespaceRequiredError); ok {
		*target = typed
		return true
	}
	return false
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
