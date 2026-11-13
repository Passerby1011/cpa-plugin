// Package plugin implements the CLIProxyAPI method dispatcher for the
// geminiapi provider: registration, model publication, execution routing by
// namespace prefix, backend supervision, and the Management API.
//
// # Two namespaces, two supervisors
//
// This plugin serves ONE provider id ("geminiapi") and TWO model namespaces:
// "ai-studio" (AIStudio2API) and "gemini-web" (gemini-web2api-go). Each
// namespace owns its own backend.Manager — its own data directory, port, pid
// file and process group — and its own catalog snapshot, refreshed
// independently. Nothing is shared between them but the provider id, which is
// what makes "the wrong backend answered" structurally impossible rather than
// merely unlikely.
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

var pluginVersion = "0.2.0"

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
	// catalogs are the last good published snapshots, one per namespace. They
	// survive a failed refresh: an outage must not wipe a working model list.
	catalogs map[string]catalog.BuildResult
	// catalogErrs records why a namespace's last refresh produced nothing. An
	// empty catalog is never silently accepted: the reason is carried here and
	// rendered on the page.
	catalogErrs map[string]string
	// backends supervises the managed backends, one per namespace. A namespace
	// pointing at an external backend has no entry here.
	backends map[string]*backend.Manager
	// externals marks the namespaces served by an operator-owned backend.
	externals map[string]bool
	// modelsAt records when each namespace's snapshot was taken.
	modelsAt map[string]time.Time

	// resolveSetup, when non-nil, replaces the real backend resolution. It is a
	// test seam only: production leaves it nil.
	resolveSetup func(ctx context.Context, namespace string) (backend.Setup, error)

	stop chan struct{}
	done chan struct{}
}

// NewManager returns a dispatcher whose outbound traffic flows through bridge.
func NewManager(bridge *HostBridge) *Manager {
	return &Manager{
		bridge:      bridge,
		catalogs:    map[string]catalog.BuildResult{},
		catalogErrs: map[string]string{},
		backends:    map[string]*backend.Manager{},
		externals:   map[string]bool{},
		modelsAt:    map[string]time.Time{},
	}
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
		return ErrEnvelope("unsupported", fmt.Sprintf("executor.count_tokens has no %s equivalent", ProviderID)), nil
	case pluginabi.MethodExecutorHTTPRequest:
		return ErrEnvelope("unsupported", fmt.Sprintf("%s is not supported by %s", method, pluginName)), nil
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
	// No AuthProvider: an account is a Google credential imported into the
	// backend's own pool, not a CPA auth record the host could parse or
	// refresh. Declaring it would publish an OAuth entry whose only outcome is
	// a failure.
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
			Description: "ROOT directory for the two managed backends. Each namespace keeps its own subdirectory under it " +
				"(<data-dir>/ai-studio and <data-dir>/gemini-web), because the two backends keep incompatible state under the " +
				"same file names. Defaults to <user config dir>/cpa-plugin/geminiapi. Google credentials live inside each " +
				"subdirectory and are never sent anywhere else.",
		},
		{
			Name: "ai-studio",
			Type: pluginapi.ConfigFieldTypeObject,
			Description: `AI Studio namespace (backend: AIStudio2API 0.3.5-magpie.1), e.g. ` +
				`{"base-url":"http://127.0.0.1:2049/v1","api-key":"...","proxy":"http://127.0.0.1:7890","allow-image-models":false,"port":2049}. ` +
				"base-url points at an EXTERNAL, already-running backend: set it and the plugin stops managing its own process " +
				"(api-key is then required). Leave base-url empty to let the plugin install, start and supervise AIStudio2API. " +
				"proxy is what the BACKEND uses to reach Google (written into its .env), never applied to the localhost call " +
				"between this plugin and the backend. allow-image-models exposes image-generation models; CPA has no image " +
				"generation route, so leaving it off is the correct default. port is where a NEW managed backend starts (a port " +
				"already recorded in data-dir wins).",
		},
		{
			Name: "gemini-web",
			Type: pluginapi.ConfigFieldTypeObject,
			Description: `Gemini Web namespace (backend: gemini-web2api-go 4.28.0-magpie.1), e.g. ` +
				`{"base-url":"http://127.0.0.1:2048/v1","api-key":"...","proxy":"http://127.0.0.1:7890","port":2048}. ` +
				"Same semantics as ai-studio: base-url makes it external, proxy is the backend's own proxy (registered in its " +
				"proxy pool), port is where a new managed backend starts. There is no allow-image-models switch: this backend " +
				"publishes no image models at all.",
		},
		{
			Name: "proxy",
			Type: pluginapi.ConfigFieldTypeString,
			Description: "Proxy used ONLY to download the backend binaries and to reach a backend's own admin surface. " +
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
				"a long but productive reasoning stream is never killed, while a silently stalled one is.",
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
			// plugin-owned, not bound to a host auth record.
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

	// Retire the previous backends: a reconfigure must not leave the old
	// supervisors' children running under the new configuration.
	m.mu.RLock()
	previous := m.backends
	m.mu.RUnlock()
	for _, mgr := range previous {
		if mgr != nil {
			mgr.Shutdown()
		}
	}

	doer := m.httpDoer()
	backends := map[string]*backend.Manager{}
	externals := map[string]bool{}
	for _, ns := range cfg.Namespaces {
		nsCfg, cfgErr := cfg.Namespace(ns)
		if cfgErr != nil {
			return ErrEnvelope("invalid_config", cfgErr.Error()), nil
		}
		if !nsCfg.Managed() {
			externals[ns] = true
			continue
		}
		flavor, flavorErr := backend.FlavorFor(backend.FlavorID(ns))
		if flavorErr != nil {
			return ErrEnvelope("invalid_config", flavorErr.Error()), nil
		}
		backends[ns] = backend.New(backend.Options{
			Flavor:        flavor,
			DataDir:       cfg.DataDirFor(ns),
			PreferredPort: nsCfg.Port,
			SyncProxy:     cfg.SyncProxy,
			ModelTimeout:  cfg.ModelTimeout,
			Doer:          doer,
		})
	}

	m.mu.Lock()
	m.cfg = cfg
	m.backends = backends
	m.externals = externals
	m.catalogs = map[string]catalog.BuildResult{}
	m.catalogErrs = map[string]string{}
	m.modelsAt = map[string]time.Time{}
	m.mu.Unlock()

	// Materialize auth records so the host can schedule this provider at all
	// (see auth.go), then read the catalogs once. A failed first read is not
	// fatal: the plugin registers with an empty catalog and the refresh loop
	// retries, so the config form still renders.
	if m.bridge != nil {
		if errAuth := m.materializeAuth(context.Background()); errAuth != nil {
			_ = m.log("warn", "could not materialize auth records: "+errAuth.Error(), nil)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), registerRefreshTimeout)
	defer cancel()
	if errRefresh := m.refreshCatalogs(ctx); errRefresh != nil {
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
//
// A namespace whose refresh produced nothing contributes no models: publishing
// a stale or wrong model under it would make every request fail at the backend
// instead of here, with a less useful error.
func (m *Manager) handleModels() ([]byte, error) {
	m.mu.RLock()
	catalogs := m.catalogs
	m.mu.RUnlock()

	total := 0
	for _, res := range catalogs {
		total += len(res.Records)
	}
	models := make([]pluginapi.ModelInfo, 0, total)
	for _, ns := range catalog.Namespaces {
		res, ok := catalogs[ns.Name]
		if !ok {
			continue
		}
		for _, rec := range res.Records {
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
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: ProviderID, Models: models}), nil
}

// handleShutdown stops the refresh loop and every supervised backend.
func (m *Manager) handleShutdown() ([]byte, error) {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	if oldDone := m.closeStop(); oldDone != nil {
		<-oldDone
	}
	m.mu.RLock()
	backends := m.backends
	m.mu.RUnlock()
	for _, mgr := range backends {
		if mgr != nil {
			// Stopping the children here is what keeps a CPA exit from leaving
			// an orphan listening on a port forever.
			mgr.Shutdown()
		}
	}
	if m.bridge != nil && !m.bridge.WaitForInFlight(shutdownDrainTimeout) {
		_ = m.log("warn", "shutdown proceeding with host callbacks still in flight", nil)
	}
	m.mu.Lock()
	m.cfg = config.Config{}
	m.catalogs = map[string]catalog.BuildResult{}
	m.catalogErrs = map[string]string{}
	m.backends = map[string]*backend.Manager{}
	m.externals = map[string]bool{}
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
					_ = m.refreshCatalogs(ctx)
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

// refreshCatalogs reads every namespace's model list and replaces that
// namespace's snapshot independently.
//
// Independence is the point: one backend being down must not blank the other
// namespace's models. On failure the previous snapshot is KEPT
// (stale-while-unavailable) for that namespace only.
func (m *Manager) refreshCatalogs(ctx context.Context) error {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()

	var firstErr error
	failures := 0
	for _, ns := range cfg.Namespaces {
		if err := m.refreshNamespace(ctx, cfg, ns); err != nil {
			failures++
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", ns, err)
			}
		}
	}
	if len(cfg.Namespaces) > 0 && failures == len(cfg.Namespaces) && firstErr != nil {
		// Every namespace failed: that is a whole-provider outage and the
		// caller should hear about it (the refresh loop logs it, and the panel
		// shows the per-namespace reasons).
		return fmt.Errorf("no namespace could be refreshed: %w", firstErr)
	}
	return nil
}

// refreshNamespace refreshes exactly one namespace's snapshot.
func (m *Manager) refreshNamespace(ctx context.Context, cfg config.Config, namespace string) error {
	nsCfg, err := cfg.Namespace(namespace)
	if err != nil {
		return err
	}
	ns, err := catalog.NamespaceFor(namespace)
	if err != nil {
		return err
	}
	setup, err := m.setupFor(ctx, cfg, namespace)
	if err != nil {
		m.recordCatalogError(namespace, "backend unavailable: "+err.Error())
		return err
	}
	doer := m.httpDoer()
	if doer == nil {
		err := fmt.Errorf("no HTTP transport available")
		m.recordCatalogError(namespace, err.Error())
		return err
	}
	list, err := backend.FetchModels(ctx, doer, setup, cfg.ModelTimeout)
	if err != nil {
		m.recordCatalogError(namespace, err.Error())
		return err
	}
	result, buildErr := catalog.Build(list.Data, catalog.BuildOptions{
		Namespace:        ns,
		Filter:           cfg.Models,
		AllowImageModels: nsCfg.AllowImageModels,
		ContextLimit:     nsCfg.ContextLimit,
		OutputLimit:      nsCfg.OutputLimit,
	})
	if buildErr != nil {
		// A zero-model catalog is refused rather than published: keep the last
		// good snapshot for this namespace and record WHY, which the page shows.
		m.mu.Lock()
		previous := m.catalogs[namespace]
		m.catalogErrs[namespace] = buildErr.Error()
		m.mu.Unlock()
		if len(previous.Records) > 0 {
			return fmt.Errorf("keeping the previous %d-model snapshot: %w", len(previous.Records), buildErr)
		}
		return buildErr
	}
	m.mu.Lock()
	m.catalogs[namespace] = result
	m.catalogErrs[namespace] = ""
	m.modelsAt[namespace] = time.Now()
	m.mu.Unlock()
	return nil
}

// recordCatalogError stores why a namespace produced nothing.
func (m *Manager) recordCatalogError(namespace, reason string) {
	m.mu.Lock()
	m.catalogErrs[namespace] = reason
	m.mu.Unlock()
}

// setupFor resolves how to reach one namespace's backend: an external base-url,
// or the managed backend's own setup (starting it if needed).
//
// resolveSetup is a test seam: production leaves it nil and takes one of the two
// real paths below, while a test can hand back a prepared Setup without
// downloading or spawning anything.
func (m *Manager) setupFor(ctx context.Context, cfg config.Config, namespace string) (backend.Setup, error) {
	m.mu.RLock()
	override := m.resolveSetup
	external := m.externals[namespace]
	mgr := m.backends[namespace]
	m.mu.RUnlock()
	if override != nil {
		return override(ctx, namespace)
	}
	nsCfg, err := cfg.Namespace(namespace)
	if err != nil {
		return backend.Setup{}, err
	}
	if external {
		origin := strings.TrimSuffix(nsCfg.BaseURL, "/v1")
		return backend.Setup{
			Origin:  origin,
			BaseURL: nsCfg.BaseURL,
			APIKey:  nsCfg.APIKey,
			// No admin credentials: an external backend's admin surface is not
			// this plugin's to use, which is why account import is refused for
			// it.
			Flavor:  backend.FlavorID(namespace),
			Version: "external",
		}, nil
	}
	if mgr == nil {
		return backend.Setup{}, fmt.Errorf("no managed backend is configured for namespace %s", namespace)
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
