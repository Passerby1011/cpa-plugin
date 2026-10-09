package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/internal/catalog"
	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/internal/config"
	"github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi/internal/errclass"
)

// ProviderID is the single provider key served by this plugin (FR-001).
const ProviderID = "commandcode"

// pluginName is reported in registration metadata as the bare plugin id, so it
// matches the .so filename, the plugins.configs.<id> key, and the management
// routes. pluginVersion is reported in registration metadata and is overridden
// at build time with
// -ldflags "-X .../internal/plugin.pluginVersion=<version>".
const pluginName = "commandcode-go-cliproxyapi"

var pluginVersion = "0.4.7"

// SetVersion overrides the reported plugin version; the build injects it via
// main.version (-ldflags). An empty value keeps the vendored default.
func SetVersion(v string) {
	if v = strings.TrimSpace(v); v != "" {
		pluginVersion = v
	}
}

// githubRepoURL satisfies the host's validPlugin gate (host.go
// validPlugin rejects empty Metadata.GitHubRepository) and is the address a
// plugin store polls for updates, so it must be the repository this build is
// published from. The project it was derived from is
// https://github.com/massiveits/opencode-go-cliproxyapi.
const githubRepoURL = "https://github.com/Passerby1011/cpa-plugin"

// registerRefreshTimeout bounds ONLY the synchronous initial/reconfigure
// refreshOnce so a slow catalog cannot block host startup/reconfigure for a
// full request-timeout (default 15m). On expiry registration proceeds per
// FR-002 empty/stale semantics; the ticker retries at refresh-interval with
// the full request-timeout.
const registerRefreshTimeout = 10 * time.Second

// Manager owns dispatcher state (config snapshot, catalog manager, refresh
// loop) and routes every RPC method. Safe for concurrent HandleCall use.
type Manager struct {
	bridge *HostBridge // immutable after NewManager

	// pool owns per-credential selection and cooldowns.
	pool *poolState

	// device throttles the per-credential fingerprint/lifecycle announcement.
	device *deviceAnnouncer

	// inflight counts requests currently being served, for the optional global
	// in-flight cap. It lives on the Manager (not a package global) so two
	// Manager instances — as tests build — do not share a budget.
	inflight atomic.Int64

	// lifeMu serializes whole register/reconfigure/shutdown sequences so
	// their stop-wait-install steps cannot interleave into orphaned tickers.
	lifeMu sync.Mutex

	// credMu guards the auth-record credential snapshot. It is a SEPARATE
	// lock from mu so refreshAuthCredentials (which does host round-trips)
	// never blocks request-path readers of the config snapshot.
	credMu          sync.Mutex
	authCreds       []config.Account
	authCredsLoaded bool

	mu  sync.RWMutex
	cfg config.Config
	mgr *catalog.Manager
	// stop/done manage the one background refresh goroutine; both nil
	// when no loop is running.
	stop chan struct{}
	done chan struct{}
}

// NewManager returns a dispatcher whose outbound traffic flows through bridge.
func NewManager(bridge *HostBridge) *Manager {
	return &Manager{bridge: bridge, pool: newPoolState(), device: newDeviceAnnouncer()}
}

// HandleCall dispatches one RPC method and returns envelope bytes. Handler
// failures travel inside the envelope; a recovered panic becomes a
// "plugin_error" envelope so the host process never dies with us.
func (m *Manager) HandleCall(method string, request []byte) (resp []byte, err error) {
	debugTrace("handle method=%s request_bytes=%d", method, len(request))
	defer func() {
		// Defensive: host-boundary panics are goroutine-contained (see
		// HostBridge.callWithTimeout); this guards future handler bugs.
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
		return classEnvelope(&errclass.Error{
			Class:   errclass.ClassUnsupported,
			Message: "executor.count_tokens has no CommandCode equivalent",
		}), nil
	case pluginabi.MethodExecutorHTTPRequest:
		return classEnvelope(&errclass.Error{
			Class:   errclass.ClassUnsupported,
			Message: fmt.Sprintf("%s endpoint is not supported by commandcode", pluginabi.MethodExecutorHTTPRequest),
		}), nil
	default:
		return ErrEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// lifecycleRequest mirrors rpcLifecycleRequest: config_yaml is base64.
// The request's schema_version is decoded-and-ignored; registration echoes
// pluginabi.SchemaVersion.
type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type capabilities struct {
	ModelProvider bool `json:"model_provider"`
	// No auth_provider: CommandCode credentials are a plugin-owned pool, not
	// CPA auth records. Declaring it made the host list us on the OAuth login
	// page, where the only possible outcome was "failed to generate
	// authorization url", and the capability cannot be split into "parse only".
	// Dropping it removes the OAuth entry on an unmodified host and stops the
	// plugin writing auth files into CPA.
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope,omitempty"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
	ManagementAPI         bool                         `json:"management_api"`
}

type registrationResult struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

// configFields declares the plugin's configuration knobs for the host's
// Management Center, so every supported option can be set from the UI
// instead of hand-editing plugins.configs.<id> in config.yaml.
//
// Field names map 1:1 to top-level keys under plugins.configs.<id>. Nested
// options are declared as object fields (JSON-edited) rather than dotted
// names like "model-prefix.enabled": the host shallow-merges the submitted
// object into the plugin config node, so a dotted key would be written as
// the literal key "model-prefix.enabled" instead of nesting under
// "model-prefix", producing YAML the plugin does not read.
func configFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{
			Name: "api-keys",
			Type: pluginapi.ConfigFieldTypeArray,
			Description: "Required. JSON array of CommandCode API keys, e.g. " +
				`[{"value":"cc_..."}]. Supports ${ENV_VAR} expansion (resolved inside the CPA process). ` +
				"The value is stored in the CPA config file and shown in plain text here, so avoid sharing the config page.",
		},
		{
			Name: "accounts",
			// Array, not Object. The declared type is display metadata — the
			// host's only reader is pluginConfigFields, which forwards it
			// unchanged — so the WebUI picks the editor from it and the host
			// coerces nothing: whatever the UI submits is stored as the YAML
			// node management/plugins.go yamlNodeFromJSONValue builds (a JSON
			// array -> yaml.SequenceNode) and read back by yamlNodeToJSONValue
			// (SequenceNode -> JSON array). The plugin decodes that node into
			// []rawAccount (config rawConfig.Accounts). An Object declaration
			// would steer the UI toward an object-shaped submission the plugin
			// cannot decode — the 0.1.4 "saved but not effective" failure.
			// Sibling fields follow the same rule in the other direction:
			// "pool"/"model-prefix" are Objects because the plugin decodes them
			// into YAML mappings, and "api-keys" is an Array for the same reason
			// "accounts" is.
			Type: pluginapi.ConfigFieldTypeArray,
			Description: `Account pool: JSON array of credentials and the surface each one uses, e.g. ` +
				`[{"label":"go","mode":"go-cli","credential":"user_..."},{"credential":"user_..."}]. ` +
				"Must be a JSON array; the host saves it as a YAML sequence. " +
				"mode is provider (default) or go-cli; a Go-plan key MUST use go-cli, which talks to " +
				"/alpha/generate because the Provider API refuses Go plans. credential supports " +
				"${ENV_VAR} expansion; disabled keeps an entry in config but out of rotation. " +
				"A bare credential string is accepted in place of the object form, and legacy " +
				"api-keys entries are folded in as provider accounts.",
		},
		{
			Name: "pool",
			Type: pluginapi.ConfigFieldTypeObject,
			Description: `How accounts are chosen per request, e.g. ` +
				`{"strategy":"sticky","max-concurrency-per-account":1}. ` +
				"strategy is sticky (default, prefer the last healthy account) or round-robin; " +
				"max-concurrency-per-account 0 means unlimited.",
		},
		{
			Name: "base-url",
			Type: pluginapi.ConfigFieldTypeString,
			Description: "Upstream provider base URL. Default https://api.commandcode.ai/provider/v1. " +
				"https only unless allow-http is enabled; no query, fragment, or userinfo.",
		},
		{
			Name:        "catalog-url",
			Type:        pluginapi.ConfigFieldTypeString,
			Description: "Model catalog URL. Default {base-url}/models.",
		},
		{
			Name: "model-prefix",
			Type: pluginapi.ConfigFieldTypeObject,
			Description: `Client-facing model id prefix, e.g. {"enabled":true,"value":"commandcode"}. ` +
				"When enabled, ids are published as <value>/<model>; when disabled, bare upstream ids.",
		},
		{
			Name: "catalog",
			Type: pluginapi.ConfigFieldTypeObject,
			Description: `Catalog refresh behaviour, e.g. ` +
				`{"refresh-interval":"15m","stale-while-unavailable":true,"static":["claude-sonnet-4-6","deepseek/deepseek-v4-pro"]}. ` +
				"refresh-interval minimum is 1m. static is an optional model-id list served when the live " +
				"{base-url}/models cannot be fetched — notably a go-cli-only pool, since the Provider API " +
				"refuses Go-plan keys.",
		},
		{
			Name: "protocols",
			Type: pluginapi.ConfigFieldTypeObject,
			Description: `Route kill switches, e.g. {"chat-completions":true,"messages":true,"responses":true}. ` +
				"Disabling one excludes its models with a diagnostic.",
		},
		{
			Name: "models",
			Type: pluginapi.ConfigFieldTypeObject,
			Description: `Restrict which discovered models are published, e.g. ` +
				`{"allow":["deepseek/deepseek-v4.1-flash"],"deny":["Qwen/qwen3-max"]}. ` +
				"Empty allow publishes every discovered model; deny always wins. " +
				"Ids match either the upstream id or the prefixed public id.",
		},
		{
			Name: "route-overrides",
			Type: pluginapi.ConfigFieldTypeObject,
			Description: `Pin a model onto another upstream route, e.g. ` +
				`{"deepseek/deepseek-v4.1-flash":{"protocol":"messages","endpoint":"/v1/messages"}}. ` +
				"Both protocol and endpoint are required.",
		},
		{
			Name: "device",
			Type: pluginapi.ConfigFieldTypeObject,
			Description: `Fabricated device identity for go-cli requests, e.g. ` +
				`{"enabled":true,"project-dir":"C:\\Users\\dev\\projects\\app","identity-salt":""}. ` +
				"enabled defaults to true: the go-cli transport exists to look like the vendor CLI on a real " +
				"machine, and the fingerprint, the x-project-slug header and the envelope's workingDir are all " +
				"derived from this one block so they can never disagree. project-dir defaults to the reference " +
				"fabricated path; identity-salt only shifts WHICH fake machine a credential maps to (an escape " +
				"hatch for a flagged credential, not a per-request knob).",
		},
		{
			Name: "retry",
			Type: pluginapi.ConfigFieldTypeObject,
			Description: `上游闪断透明重试，例如 {"max":2,"base-backoff":"400ms"}。` +
				"仅重试传输层闪断（连接被重置等），且只在尚未向下游吐出任何字节时进行；" +
				"429/503 这类上游语义信号绝不重试。max=0 关闭重试。",
		},
		{
			Name: "watchdog",
			Type: pluginapi.ConfigFieldTypeObject,
			Description: `流式空闲看门狗，例如 {"enabled":true,"stream":"30s","non-stream":"90s"}。` +
				"只计两次上游读取之间的间隔（每收到一个 chunk 重置），不是整个请求时长；" +
				"厂商 CLI 对上游没有 idle 超时，用总时长上限会误杀合法的长思考请求。",
		},
		{
			Name:        "max-inflight",
			Type:        pluginapi.ConfigFieldTypeInteger,
			Description: "全局在途请求上限；超限返回 503。默认 0=不限（并发控制通常交给前面的反向代理）。",
		},
		{
			Name:        "request-timeout",
			Type:        pluginapi.ConfigFieldTypeString,
			Description: "Upstream HTTP timeout, e.g. 5m. Also bounds account/quota calls to 30s.",
		},
		{
			Name:        "max-response-bytes",
			Type:        pluginapi.ConfigFieldTypeInteger,
			Description: "Maximum non-streaming response body size in bytes. Default 67108864 (64 MiB).",
		},
		{
			Name:        "allow-http",
			Type:        pluginapi.ConfigFieldTypeBoolean,
			Description: "Permit http:// upstreams and catalog URLs. For local testing only.",
		},
	}
}

func registrationEnvelope() []byte {
	formats := []string{"openai", "claude", "openai-response"}
	return okEnvelope(registrationResult{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           pluginName,
			GitHubRepository: githubRepoURL,
			ConfigFields:     configFields(),
		},
		Capabilities: capabilities{
			ModelProvider: true,
			// No AuthProvider on purpose.
			//
			// In CPA that capability means two things at once: "can parse
			// auth files" AND "is an interactive login provider". CommandCode
			// credentials are API keys managed by this plugin, so declaring it
			// only bought us a dead OAuth button ("failed to generate
			// authorization url") — and it cannot be turned off per-half.
			// Dropping it removes the OAuth entry on an UNMODIFIED host, with
			// no host patch and no auth files written into CPA.
			//
			// ExecutorModelScope becomes Static: our models are plugin-owned,
			// not bound to a host auth record. The host still routes them to
			// this executor because it only requires Executor + a static/both
			// scope.
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeStatic,
			ExecutorInputFormats:  formats,
			ExecutorOutputFormats: formats,
			ManagementAPI:         true,
		},
	})
}

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
			{Method: "POST", Path: "/plugins/" + pluginName + "/quota-usage"},
			// Adding a credential is a WRITE the page performs; the host
			// exposes no plugin-config write, so the key is stored as an auth
			// record and folded into the pool from there.
			{Method: "POST", Path: accountsPath},
		},
		Resources: []struct {
			Path        string `json:"path"`
			Menu        string `json:"menu"`
			Description string `json:"description"`
		}{{Path: "/quota", Menu: "CommandCode Quota", Description: "View CommandCode quota windows."}},
	}), nil
}

func (m *Manager) handleManagement(request []byte) ([]byte, error) {
	var req struct {
		pluginapi.ManagementRequest
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed management request body"), nil
	}
	resp, err := m.HandleManagement(context.Background(), req.ManagementRequest)
	if err != nil {
		return ErrEnvelope("management_failure", err.Error()), nil
	}
	return okEnvelope(resp), nil
}

// handleLifecycle implements plugin.register / plugin.reconfigure: load
// config, refresh the catalog once, publish state, restart the refresh
// loop. Registration succeeds even when the initial refresh fails (FR-002).
func (m *Manager) handleLifecycle(request []byte) ([]byte, error) {
	var req lifecycleRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed lifecycle request body"), nil
	}
	cfg, err := config.LoadForRegistration(req.ConfigYAML)
	if err != nil {
		debugTrace("lifecycle config_error=%s", err.Error())
		return ErrEnvelope("invalid_config", err.Error()), nil
	}
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	debugTrace("lifecycle config_loaded accounts=%d prefix_enabled=%t prefix=%s", len(cfg.EffectiveAccounts()), cfg.ModelPrefix.Enabled, cfg.ModelPrefix.Value)
	// Materialize the pool into CPA auth records. This is required, not
	// cosmetic: the host's auth selection walks ITS OWN auth table to find a
	// candidate executor, so a provider with no auth record is never reached -
	// every request fails with "auth_not_found: no auth available" before the
	// plugin's executor runs. The credential still lives in this plugin's
	// pool; the auth record exists so the host can schedule it.
	//
	// This is deliberately NOT the auth_provider capability: declaring that
	// additionally publishes an interactive OAuth login entry, whose only
	// possible outcome here is "failed to generate authorization url". Writing
	// records and advertising a login flow are separate concerns, and only the
	// latter is unwanted.
	//
	// A pending registration has no credentials, so there is nothing to
	// materialize; skip the host round-trip entirely.
	if !cfg.Pending && m.bridge != nil {
		if errAuth := m.materializeAuthRecords(context.Background(), cfg); errAuth != nil {
			debugTrace("lifecycle auth_materialize_error=%s", errAuth.Error())
			return ErrEnvelope("auth_materialize_failed", errAuth.Error()), nil
		}
	}
	// Learn the credentials stored as auth records - that is where a key added
	// from the quota page lives. A failure is logged, not fatal: the config's
	// own accounts remain a complete credential set without it.
	if m.bridge != nil {
		if errCred := m.refreshAuthCredentials(context.Background(), m.bridge); errCred != nil {
			debugTrace("lifecycle auth_credential_refresh_error=%s", errCred.Error())
		}
	}
	// A pending registration (no credentials yet) must not touch the network:
	// there is no credential to fetch a catalog with, and the /models call
	// would just fail. Register successfully with an empty catalog so the
	// Management Center can render the config form; the reconfigure that
	// follows a saved key performs the real refresh.
	// A nil *HostBridge must not enter the interface as a typed nil, or
	// catalog's nil-client guard never fires and Refresh panics inside Do.
	var client catalog.HostClient
	if m.bridge != nil {
		client = m.bridge
	}
	mgr := catalog.New(cfg, client)
	var refreshErr error
	// "Has a credential" must count BOTH sources. A deployment whose keys were
	// all added from the quota page keeps them as auth records, and keying this
	// decision on the config alone would leave its catalog permanently empty -
	// the page would add a working credential and no models would appear.
	if len(m.poolAccounts(cfg)) == 0 {
		if m.bridge != nil {
			_ = m.bridge.Log("info", "commandcode plugin registered without credentials; add one in the plugin config or on the Quota page", nil)
		}
	} else {
		refreshErr = m.refreshOnce(context.Background(), mgr, m.bridge, registerRefreshTimeout, cfg)
	}
	debugTrace("lifecycle refresh_complete model_count=%d refresh_error=%t pool=%d", len(mgr.Models()), refreshErr != nil, len(m.poolAccounts(cfg)))
	// Retire any running loop and wait for its exit outside m.mu: a mid-refresh
	// tick must never stall readers holding RLock (F4). lifeMu keeps the
	// stop-wait-install sequence atomic against other lifecycles.
	if oldDone := m.closeStop(); oldDone != nil {
		<-oldDone
	}

	m.mu.Lock()
	m.cfg = cfg
	// FR-002 stale-while-unavailable: a failed refresh must neither wipe a
	// non-empty previous snapshot nor keep the OLD manager, whose stored cfg
	// would pin the old base-url/catalog-url/protocols/prefix so ticks
	// fetched the old URL with the new config's keys forever (F5). The NEW
	// manager is adopted unconditionally; when a previous snapshot exists
	// AND the NEW config keeps stale-while-unavailable enabled, it is
	// seeded into the new one, so serving is unchanged until the next
	// successful tick refreshes against the NEW config. Under fail-closed
	// (stale-while-unavailable:false) no seed happens — same as the ticker
	// failure path — so a reconfigure during an outage serves nothing until
	// a refresh succeeds, honoring the operator's policy on BOTH paths.
	if refreshErr != nil && cfg.Catalog.StaleWhileUnavailable && m.mgr != nil && len(m.mgr.Models()) > 0 {
		mgr.SeedFrom(m.mgr)
	}
	m.mgr = mgr
	stop, done := make(chan struct{}), make(chan struct{})
	m.stop, m.done = stop, done
	interval := cfg.Catalog.RefreshInterval
	m.mu.Unlock()

	m.startRefreshLoop(cfg, mgr, interval, stop, done)
	return registrationEnvelope(), nil
}

// authKeyHash is the non-secret digest that identifies one config key. It is
// independent of config ordering, which is what keeps the records derived from
// it idempotent across restarts.
func authKeyHash(key string) string {
	digest := sha256.Sum256([]byte(key))
	return hex.EncodeToString(digest[:])
}

// authRecordIDFromHash and authFileNameFromHash are the runtime auth ID and the
// auth file name for one key digest. Both derive from the digest alone so every
// producer agrees without recomputing it.
func authRecordIDFromHash(hash string) string { return ProviderID + "-key-" + hash }
func authFileNameFromHash(hash string) string { return authRecordIDFromHash(hash) + ".json" }

// handleModels implements model.static / model.for_auth (FR-003): the last
// good catalog snapshot mapped to wire ModelInfos; empty catalog yields an
// empty slice, not an error.
func (m *Manager) handleModels() ([]byte, error) {
	m.mu.RLock()
	mgr := m.mgr
	m.mu.RUnlock()
	models := make([]pluginapi.ModelInfo, 0)
	if mgr != nil {
		for _, rec := range mgr.Models() {
			models = append(models, pluginapi.ModelInfo{
				ID:                        rec.PublicID,
				Object:                    "model",
				OwnedBy:                   ProviderID,
				DisplayName:               rec.DisplayName,
				ContextLength:             rec.ContextLimit,
				MaxCompletionTokens:       rec.OutputLimit,
				SupportedInputModalities:  rec.InputModes,
				SupportedOutputModalities: rec.OutputModes,
				Thinking:                  rec.Thinking,
			})
		}
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: ProviderID, Models: models}), nil
}

// shutdownDrainTimeout bounds how long handleShutdown waits for orphaned
// host-callback goroutines (timed-out callbacks and their abandon/drain
// cleanup) to finish before returning. Package var so tests can shrink it.
var shutdownDrainTimeout = 15 * time.Second

// handleShutdown stops the refresh loop, drains orphaned host callbacks,
// and clears state. The drain matters on Unix: the SDK loader frees host_api
// and dlclose's the plugin immediately after this export returns
// (loader_unix.go), so a goroutine still calling into the host would crash
// the process — see COMPATIBILITY.md limitations.
func (m *Manager) handleShutdown() ([]byte, error) {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	oldDone := m.closeStop()
	if oldDone != nil {
		<-oldDone
	}
	if m.bridge != nil && !m.bridge.WaitForInFlight(shutdownDrainTimeout) {
		// Non-Windows unload during this residual window can crash the
		// host; the warn makes the stall visible without blocking forever.
		_ = m.bridge.Log("warn", "shutdown proceeding with host callbacks still in flight", nil)
	}
	m.mu.Lock()
	m.cfg, m.mgr = config.Config{}, nil
	m.mu.Unlock()
	return okEnvelope(struct{}{}), nil
}

// startRefreshLoop spawns the single background ticker goroutine over the
// caller-supplied stop/done pair (already stored under m.mu) and the served
// catalog manager. The loop captures its own cfg/mgr/bridge snapshot so
// it never contends on m.mu; reconfigure swaps state and restarts the loop.
func (m *Manager) startRefreshLoop(cfg config.Config, mgr *catalog.Manager, interval time.Duration, stop, done chan struct{}) {
	// F4: tick refresh contexts derive from stopCtx so close(stop) aborts
	// an in-flight Refresh immediately instead of leaving lifeMu held until
	// the old config's request-timeout expires. The watcher goroutine is
	// required because the loop body blocks inside refreshOnce while a tick
	// runs and cannot select on stop itself.
	stopCtx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stop
		cancel()
	}()
	go func() {
		ticker := time.NewTicker(interval)
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
						// CGO bridge calls can panic; a tick must never
						// take the host process down.
						if r := recover(); r != nil && m.bridge != nil {
							_ = m.bridge.Log("error", "catalog refresh panicked", nil)
						}
					}()
					m.refreshOnce(stopCtx, mgr, m.bridge, cfg.RequestTimeout, cfg)
				}()
			}
		}
	}()
}

// closeStop signals a running loop to exit and clears the stop/done pair.
// It returns the loop's done channel — nil when no loop was running — and
// the CALLER waits on it after this returns, never while holding m.mu (F4).
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

// refreshOnce runs one bounded catalog refresh. Catalog refresh is not a client
// request, so the fallback uses the first configured key only and has no client
// selection, rotation, cooldown, or retry state. parent bounds-and-cancels
// the attempt: the lifecycle path passes context.Background() plus
// registerRefreshTimeout; ticker ticks pass the loop's stop-derived context
// plus the full request-timeout, so close(stop) aborts an in-flight tick
// (F4).
//
// Catalog resolution order (documented in the README): the live /models fetch
// when a provider-mode account exists, and on failure the configured static
// model list — with a usable previous snapshot always winning over the static
// list. Concretely:
//
//   - no provider-mode account -> cfg.Catalog.Static, else empty;
//   - provider account, live fetch OK -> the fetched catalog;
//   - provider account, live fetch failed, a previous snapshot is being served
//     and stale-while-unavailable is enabled -> keep serving that snapshot;
//   - provider account, live fetch failed, no usable snapshot -> static list
//     when configured, else the error (fail-closed, FR-002).
//
// Both catalog sources funnel through catalog.Manager's one snapshot builder,
// so the static path obeys the same models.allow/deny, model-prefix, protocol
// kill-switch, route-override, and dedup rules as the live path.
//
// Warnings log a redacted category label from the catalog package via
// host.log, never key material (FR-011).
// refreshOnceFn is the pre-remote-catalog entry point, kept so existing tests
// (and any future caller with no Manager) still compile. It delegates to a
// throwaway Manager, which is safe because the only Manager state the
// refresh chain touches is the remote-catalog memo.
func refreshOnce(parent context.Context, mgr *catalog.Manager, bridge *HostBridge, timeout time.Duration, cfg config.Config) error {
	return NewManager(bridge).refreshOnce(parent, mgr, bridge, timeout, cfg)
}

func (m *Manager) refreshOnce(parent context.Context, mgr *catalog.Manager, bridge *HostBridge, timeout time.Duration, cfg config.Config) error {
	accounts := m.poolAccounts(cfg)
	if len(accounts) == 0 {
		// Pending registration: nothing to authenticate a catalog fetch
		// with, and indexing below would panic. Serving stays empty until a
		// credential is saved and a reconfigure runs.
		return nil
	}
	// The catalog is fetched with any usable credential. /models is served by
	// the Provider API, and a Go-plan key can call it too (verified: HTTP 200),
	// so a go-cli-only pool must NOT be locked out of live discovery - that was
	// the bug that left such pools with no models at all.
	credential := ""
	for _, a := range accounts {
		if a.Mode == config.TransportProvider && a.Credential != "" {
			credential = a.Credential
			break
		}
	}
	if credential == "" {
		for _, a := range accounts {
			if a.Credential != "" {
				credential = a.Credential
				break
			}
		}
	}
	if credential == "" {
		// Nothing to authenticate a catalog fetch with. The static table is the
		// only source left; an empty one means an empty list until a credential
		// is saved.
		if len(cfg.Catalog.Static) > 0 {
			mgr.SeedStatic(cfg.Catalog.Static)
			if bridge != nil {
				_ = bridge.Log("info", "no provider-mode account; catalog served from the configured static model list", map[string]any{"models": len(cfg.Catalog.Static)})
			}
			return nil
		}
		if bridge != nil {
			_ = bridge.Log("info", "no usable credential and no static catalog configured; model list will be empty", nil)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	// Install the plan filter before the refresh so the snapshot is built with
	// it: a pool should not advertise models its accounts cannot call.
	mgr.SetPlanGate(m.buildPlanGate(parent, cfg, bridge))
	err := mgr.Refresh(ctx, credential)
	if err != nil {
		// Fallback order matters: a usable snapshot already being served wins
		// over the static list, so an outage never overwrites good data with
		// the (necessarily coarser) static table. mgr.Models() is non-empty
		// here only when a prior snapshot survived — Manager.fail clears the
		// snapshot when stale-while-unavailable is off, so the flag and the
		// snapshot agree by construction.
		if cfg.Catalog.StaleWhileUnavailable && len(mgr.Models()) > 0 {
			if bridge != nil {
				_ = bridge.Log("warn", "catalog refresh failed; serving stale catalog", map[string]any{"error": err.Error()})
			}
			return nil
		}
		if len(cfg.Catalog.Static) > 0 {
			mgr.SeedStatic(cfg.Catalog.Static)
			if bridge != nil {
				_ = bridge.Log("warn", "catalog refresh failed; catalog served from the configured static model list", map[string]any{"error": err.Error(), "models": len(cfg.Catalog.Static)})
			}
			return nil
		}
		if bridge != nil {
			_ = bridge.Log("warn", "catalog refresh failed", map[string]any{"error": err.Error()})
		}
		return err
	}
	// FR-010: surface normalization diagnostics with the post-refresh log.
	warnings := mgr.Warnings()
	if uns := mgr.Unsupported(); len(uns) > 0 {
		parts := make([]string, len(uns))
		for i, u := range uns {
			parts[i] = fmt.Sprintf("%q: %s", u.UpstreamID, u.Reason)
		}
		fields := map[string]any{"models": strings.Join(parts, ", ")}
		if len(warnings) > 0 {
			fields["warnings"] = strings.Join(warnings, "; ")
		}
		_ = bridge.Log("warn", "unsupported models excluded from routable catalog", fields)
	} else if len(warnings) > 0 {
		_ = bridge.Log("warn", "catalog normalization warnings",
			map[string]any{"warnings": strings.Join(warnings, "; ")})
	}
	return nil
}

func okEnvelope(result any) []byte {
	raw, _ := json.Marshal(result)
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return out
}

func ErrEnvelope(code, message string) []byte {
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: message}})
	return out
}
