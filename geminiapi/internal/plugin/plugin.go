// Package plugin implements the CLIProxyAPI method dispatcher for the
// geminiapi provider: registration, model publication, execution routing by
// namespace prefix, backend supervision, and the Management API.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/geminiapi/internal/backend"
	"github.com/hex-ci/cpa-plugin/geminiapi/internal/catalog"
	"github.com/hex-ci/cpa-plugin/geminiapi/internal/config"
)

// ProviderID is the single provider key served by this plugin. A native plugin
// declares ONE executor identifier, so the two backends are two model
// namespaces ("ai-studio/<model>", "gemini-web/<model>") of this one provider.
const ProviderID = "geminiapi"

// pluginName is the bare plugin id: it must match the .so filename, the
// plugins.configs.<id> key and the management route prefix.
const pluginName = "geminiapi"

var pluginVersion = "0.1.0"

// SetVersion overrides the reported plugin version (build injects it via
// -ldflags "-X main.version=<v>").
func SetVersion(v string) {
	if v = strings.TrimSpace(v); v != "" {
		pluginVersion = v
	}
}

// githubRepoURL satisfies the host's validPlugin gate and is the address the
// plugin store polls.
const githubRepoURL = "https://github.com/Passerby1011/cpa-plugin"

// pluginLogoURL is served from this repo so an upstream URL change cannot blank
// the icon.
const pluginLogoURL = "https://raw.githubusercontent.com/Passerby1011/cpa-plugin/main/geminiapi/assets/logo.svg"

// registerRefreshTimeout bounds the synchronous catalog read at registration so
// a slow backend cannot stall host startup.
const registerRefreshTimeout = 10 * time.Second

// catalogRefreshInterval is how often the published catalog is refreshed while
// the plugin runs. A model appearing on the backend shows up within this window.
const catalogRefreshInterval = 10 * time.Minute

// Manager routes every RPC method. Safe for concurrent HandleCall use.
type Manager struct {
	bridge *HostBridge

	// lifeMu serializes register/reconfigure/shutdown.
	lifeMu sync.Mutex

	mu  sync.RWMutex
	cfg config.Config
	// catalog is the last good published snapshot. It survives a failed
	// refresh: an outage must not wipe a working model list.
	catalog catalog.BuildResult
	// backendMgr supervises the managed backend. nil when the namespace points
	// at an external backend.
	backendMgr *backend.Manager
	// skipSupervision is set when ai-studio.base-url is configured: the plugin
	// then proxies without owning a process.
	skipSupervision bool
	// modelsAt records when the snapshot was taken.
	modelsAt time.Time

	// resolveSetup, when non-nil, replaces the real backend resolution. It is a
	// test seam only: production leaves it nil.
	resolveSetup func(ctx context.Context) (backend.Setup, error)

	stop chan struct{}
	done chan struct{}
}

// NewManager returns a dispatcher whose outbound traffic flows through bridge.
func NewManager(bridge *HostBridge) *Manager {
	return &Manager{bridge: bridge}
}

// HandleCall dispatches one RPC method. Handler failures travel inside the
// envelope; a recovered panic becomes a plugin_error so the host never dies
// with us.
func (m *Manager) HandleCall(method string, request []byte) (resp []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			resp = ErrEnvelope("plugin_error", fmt.Sprintf("internal error handling %s", method))
			err = nil
		}
	}()
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return m.handleLifecycle(request)
	case pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth:
		return m.handleModels()
	case pluginabi.MethodPluginShutdown:
		return m.handleShutdown()
	case pluginabi.MethodExecutorExecute:
		return m.handleExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return m.handleExecuteStream(request)
	case pluginabi.MethodManagementRegister:
		return m.registerManagement(request)
	case pluginabi.MethodManagementHandle:
		return m.handleManagement(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID}), nil
	case pluginabi.MethodExecutorCountTokens:
		return ErrEnvelope("unsupported", "executor.count_tokens has no gemini-web equivalent"), nil
	case pluginabi.MethodExecutorHTTPRequest:
		return ErrEnvelope("unsupported", fmt.Sprintf("%s is not supported by geminiapi", method)), nil
	default:
		return ErrEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// lifecycleRequest mirrors rpcLifecycleRequest: config_yaml is base64.
type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type capabilities struct {
	ModelProvider bool `json:"model_provider"`
	// No AuthProvider: the ai-studio account is a Google cookie imported into
	// the backend's own pool, not a CPA auth record the host could parse or
	// refresh. Declaring it would publish an OAuth entry whose only outcome is
	// a failure. Multi-account is expressed as multiple auth records (see
	// authMaterialize), which is a scheduling hint, not a login flow.
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope,omitempty"`
	ExecutorInputFormats  []string `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string `json:"executor_output_formats,omitempty"`
	ManagementAPI         bool     `json:"management_api"`
}

type registrationResult struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

// configFields declares the knobs for the Management Center. Field names map
// 1:1 to keys under plugins.configs.geminiapi; nested options are object fields
// because the host shallow-merges an object into the config node (a dotted name
// would be stored as a literal key the plugin never reads).
func configFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{
			Name: "data-dir",
			Type: pluginapi.ConfigFieldTypeString,
			Description: "Directory holding the managed backend: binary, config.json, secrets.json, gemini.db and backend.log. " +
				"Defaults to <user config dir>/cpa-plugin/geminiapi. The Google account cookies live in gemini.db inside this " +
				"directory and are never sent anywhere else.",
		},
		{
			Name: "ai-studio",
			Type: pluginapi.ConfigFieldTypeObject,
			Description: `AI Studio namespace, e.g. {"base-url":"http://127.0.0.1:2048/v1","api-key":"...","proxy":"http://127.0.0.1:7890","allow-image-models":false,"port":2048}. ` +
				"base-url points at an EXTERNAL, already-running OpenAI-compatible backend: set it and the plugin stops " +
				"managing its own process (api-key is then required, and it must be that backend's key). Leave base-url " +
				"empty to let the plugin install, start and supervise its own backend. proxy is what the BACKEND uses to " +
				"reach Google — it is never applied to the localhost call between this plugin and the backend. " +
				"allow-image-models exposes image-generation models; CPA has no image generation route, so leaving it off " +
				"is the correct default. port is the port a NEW managed backend starts on (a port already recorded in " +
				"data-dir wins, and a conflict moves to the next free port and persists it).",
		},
		{
			Name: "proxy",
			Type: pluginapi.ConfigFieldTypeString,
			Description: "Proxy used ONLY to download the backend binary and to reach the backend's admin API. " +
				"Never applied to 127.0.0.1 traffic. http/https/socks5/socks5h.",
		},
		{
			Name:        "request-timeout",
			Type:        pluginapi.ConfigFieldTypeString,
			Description: "Base per-request deadline, e.g. 30m (default). An AI Studio turn can think for minutes, so this is generous by design.",
		},
		{
			Name: "stream-idle-timeout",
			Type: pluginapi.ConfigFieldTypeString,
			Description: "Independent stream-silence budget, e.g. 5m (default). Bounds the gap BETWEEN chunks, not the whole stream: " +
				"a long but productive reasoning stream is never killed, while a silently stalled one is. A client disconnect " +
				"cancels the upstream call immediately.",
		},
		{
			Name:        "model-timeout",
			Type:        pluginapi.ConfigFieldTypeString,
			Description: "Bounds catalog / health / account reads, e.g. 15s (default).",
		},
		{
			Name: "models",
			Type: pluginapi.ConfigFieldTypeObject,
			Description: `Restrict published models, e.g. {"allow":["ai-studio/gemini-2.5-pro"],"deny":["*-flash"]}. ` +
				"A '*' matches any run of characters. Empty allow publishes every discovered model; deny always wins. " +
				"Ids match either the bare backend id or the prefixed public id.",
		},
	}
}

func registrationEnvelope() []byte {
	formats := []string{"openai"}
	return okEnvelope(registrationResult{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           pluginName,
			GitHubRepository: githubRepoURL,
			Logo:             pluginLogoURL,
			ConfigFields:     configFields(),
		},
		Capabilities: capabilities{
			ModelProvider: true,
			// ExecutorModelScope "static": the published models are
			// plugin-owned, not bound to a host auth record. The host still
			// routes them here because it only requires Executor plus a
			// static/both scope.
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeStatic),
			ExecutorInputFormats:  formats,
			ExecutorOutputFormats: formats,
			ManagementAPI:         true,
		},
	})
}

// handleLifecycle implements plugin.register / plugin.reconfigure.
func (m *Manager) handleLifecycle(request []byte) ([]byte, error) {
	var req lifecycleRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed lifecycle request body"), nil
	}
	cfg, err := config.Load(req.ConfigYAML)
	if err != nil {
		return ErrEnvelope("invalid_config", err.Error()), nil
	}

	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()

	// Retire the old refresh loop before installing the new one.
	if oldDone := m.closeStop(); oldDone != nil {
		<-oldDone
	}

	doer := m.httpDoer()
	external := cfg.AIStudio.BaseURL != ""
	var mgr *backend.Manager
	if !external {
		mgr = backend.New(backend.Options{
			DataDir:       cfg.DataDir,
			PreferredPort: cfg.AIStudio.Port,
			SyncProxy:     cfg.SyncProxy,
			ModelTimeout:  cfg.ModelTimeout,
			Doer:          doer,
		})
	}

	m.mu.Lock()
	m.cfg = cfg
	m.backendMgr = mgr
	m.skipSupervision = external
	m.mu.Unlock()

	// Materialize auth records so the host can schedule this provider at all
	// (see auth.go), then read the catalog once. A failed first read is not
	// fatal: the plugin registers with an empty catalog and the refresh loop
	// retries, so the config form still renders.
	if m.bridge != nil {
		if errAuth := m.materializeAuth(context.Background()); errAuth != nil {
			_ = m.log("warn", "could not materialize auth records: "+errAuth.Error(), nil)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), registerRefreshTimeout)
	defer cancel()
	if _, errRefresh := m.refreshCatalog(ctx); errRefresh != nil {
		_ = m.log("warn", "initial model catalog refresh failed: "+errRefresh.Error(), nil)
	}

	stop, done := make(chan struct{}), make(chan struct{})
	m.mu.Lock()
	m.stop, m.done = stop, done
	m.mu.Unlock()
	m.startRefreshLoop(stop, done)

	return registrationEnvelope(), nil
}

// handleModels implements model.static / model.for_auth.
func (m *Manager) handleModels() ([]byte, error) {
	m.mu.RLock()
	result := m.catalog
	m.mu.RUnlock()

	models := make([]pluginapi.ModelInfo, 0, len(result.Records))
	for _, rec := range result.Records {
		info := pluginapi.ModelInfo{
			ID:                         rec.PublicID,
			Object:                     "model",
			OwnedBy:                    ProviderID,
			DisplayName:                rec.DisplayName,
			Name:                       rec.UpstreamID,
			ContextLength:              rec.ContextLimit,
			MaxCompletionTokens:        rec.OutputLimit,
			SupportedInputModalities:   rec.InputModes,
			SupportedOutputModalities:  rec.OutputModes,
			SupportedGenerationMethods: []string{"generateContent"},
		}
		if rec.Thinking != nil {
			info.Thinking = &pluginapi.ThinkingSupport{
				Levels:         rec.Thinking.Levels,
				DynamicAllowed: false,
				ZeroAllowed:    false,
			}
		}
		models = append(models, info)
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: ProviderID, Models: models}), nil
}

// handleShutdown stops the refresh loop and the supervised backend.
func (m *Manager) handleShutdown() ([]byte, error) {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	if oldDone := m.closeStop(); oldDone != nil {
		<-oldDone
	}
	m.mu.RLock()
	mgr := m.backendMgr
	m.mu.RUnlock()
	if mgr != nil {
		// Stopping the child here is what keeps a CPA exit from leaving an
		// orphan listening on the port forever.
		mgr.Shutdown()
	}
	if m.bridge != nil && !m.bridge.WaitForInFlight(shutdownDrainTimeout) {
		_ = m.log("warn", "shutdown proceeding with host callbacks still in flight", nil)
	}
	m.mu.Lock()
	m.cfg, m.catalog = config.Config{}, catalog.BuildResult{}
	m.backendMgr = nil
	m.mu.Unlock()
	return okEnvelope(struct{}{}), nil
}

// startRefreshLoop runs the catalog refresh ticker over the caller's stop/done
// pair. The loop captures its own snapshot so it never contends on m.mu.
func (m *Manager) startRefreshLoop(stop, done chan struct{}) {
	stopCtx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stop
		cancel()
	}()
	go func() {
		ticker := time.NewTicker(catalogRefreshInterval)
		defer close(done)
		defer ticker.Stop()
		defer cancel()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				func() {
					defer func() {
						if r := recover(); r != nil {
							_ = m.log("error", "catalog refresh panicked", nil)
						}
					}()
					ctx, cancelTick := context.WithTimeout(stopCtx, m.requestTimeout())
					defer cancelTick()
					_, _ = m.refreshCatalog(ctx)
				}()
			}
		}
	}()
}

// closeStop signals a running loop to exit and clears the pair. The caller
// waits on the returned done channel OUTSIDE the lock.
func (m *Manager) closeStop() chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stop == nil {
		return nil
	}
	done := m.done
	close(m.stop)
	m.stop, m.done = nil, nil
	return done
}

func (m *Manager) requestTimeout() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cfg.RequestTimeout > 0 {
		return m.cfg.RequestTimeout
	}
	return config.DefaultRequestTimeout
}

// refreshCatalog reads the backend's model list and replaces the published
// snapshot. On failure the previous snapshot is KEPT (stale-while-unavailable):
// a transient backend restart must not make every model disappear.
func (m *Manager) refreshCatalog(ctx context.Context) (catalog.BuildResult, error) {
	m.mu.RLock()
	cfg := m.cfg
	mgr := m.backendMgr
	external := m.skipSupervision
	m.mu.RUnlock()

	setup, err := m.setupFor(ctx, cfg, mgr, external)
	if err != nil {
		return catalog.BuildResult{}, err
	}
	doer := m.httpDoer()
	if doer == nil {
		return catalog.BuildResult{}, fmt.Errorf("no HTTP transport available")
	}
	list, err := backend.FetchModels(ctx, doer, setup, cfg.ModelTimeout)
	if err != nil {
		return catalog.BuildResult{}, err
	}
	result := catalog.Build(list.Data, catalog.BuildOptions{
		Namespace:        catalog.AIStudio,
		Filter:           cfg.Models,
		AllowImageModels: cfg.AIStudio.AllowImageModels,
		ContextLimit:     cfg.AIStudio.ContextLimit,
		OutputLimit:      cfg.AIStudio.OutputLimit,
	})
	if len(result.Records) == 0 {
		// An empty catalog is almost always a backend with no usable account.
		// Keep the last good snapshot and say why, rather than publishing
		// nothing and leaving every client with "unknown model".
		m.mu.Lock()
		previous := m.catalog
		m.mu.Unlock()
		if len(previous.Records) > 0 {
			return previous, fmt.Errorf("the backend listed no usable models; keeping the previous %d-model snapshot", len(previous.Records))
		}
	}
	m.mu.Lock()
	m.catalog = result
	m.modelsAt = time.Now()
	m.mu.Unlock()
	return result, nil
}

// setupFor resolves how to reach the backend: an external base-url, or the
// managed backend's own setup (starting it if needed).
//
// resolveSetup is a test seam: production leaves it nil and takes one of the two
// real paths below, while a test can hand back a prepared Setup without
// downloading or spawning anything.
func (m *Manager) setupFor(ctx context.Context, cfg config.Config, mgr *backend.Manager, external bool) (backend.Setup, error) {
	m.mu.RLock()
	override := m.resolveSetup
	m.mu.RUnlock()
	if override != nil {
		return override(ctx)
	}
	if external {
		return backend.Setup{
			Origin:  strings.TrimSuffix(cfg.AIStudio.BaseURL, "/v1"),
			BaseURL: cfg.AIStudio.BaseURL,
			APIKey:  cfg.AIStudio.APIKey,
			// No admin token: an external backend's admin surface is not this
			// plugin's to use, which is exactly why account import is refused for
			// it.
			AdminToken: "",
			Version:    "external",
		}, nil
	}
	if mgr == nil {
		return backend.Setup{}, fmt.Errorf("no managed backend is configured")
	}
	return mgr.EnsureBackend(ctx)
}

func (m *Manager) httpDoer() backend.HTTPDoer {
	if m.bridge == nil {
		return nil
	}
	return m.bridge
}

func (m *Manager) log(level, message string, fields map[string]any) error {
	if m.bridge == nil {
		return nil
	}
	return m.bridge.Log(level, message, fields)
}
