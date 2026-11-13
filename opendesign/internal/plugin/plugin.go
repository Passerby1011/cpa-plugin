// Package plugin — the dispatcher: registration, lifecycle, model publication,
// the device sign-in RPCs and the executors.
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

	"github.com/hex-ci/cpa-plugin/opendesign/internal/catalog"
	"github.com/hex-ci/cpa-plugin/opendesign/internal/config"
)

// ProviderID is the single provider key served by this plugin. It is also the
// plugin id, the plugins.configs.<id> key, the auth file name prefix and the
// public model id prefix.
const ProviderID = "opendesign"

// displayName is the user-facing name. The bare id stays the identifier
// everywhere the host matches on.
const displayName = "Open Design"

// githubRepoURL satisfies the host's validPlugin gate (an empty
// GitHubRepository is rejected) and is the address a plugin store polls for
// updates.
const githubRepoURL = "https://github.com/hex-ci/cpa-plugin"

// pluginVersion is reported at registration; the build overrides it with
// -ldflags "…/internal/plugin.pluginVersion=<version>" (main.go forwards
// main.version).
var pluginVersion = "0.1.0"

// SetVersion overrides the reported version. An empty value keeps the default.
func SetVersion(v string) {
	if v = strings.TrimSpace(v); v != "" {
		pluginVersion = v
	}
}

// registerRefreshTimeout bounds ONLY the synchronous registration refresh, so a
// slow catalog read cannot hold host startup for the full request-timeout.
const registerRefreshTimeout = 10 * time.Second

// Manager owns the dispatcher state and routes every RPC method. It is safe for
// concurrent HandleCall use.
type Manager struct {
	bridge *HostBridge

	// lifeMu serializes whole register/reconfigure/shutdown sequences so
	// their stop-wait-install steps cannot interleave.
	lifeMu sync.Mutex

	mu  sync.RWMutex
	cfg config.Config
	// snapshots holds one served model list per account (keyed by runtime key
	// digest, never by the key itself) plus the seed list under "".
	snapshots map[string]*catalog.Snapshot
	// seed is the list served when no account has a live catalog.
	seed *catalog.Snapshot

	// logins holds in-flight device authorizations by state token.
	logins sync.Map

	// pool owns per-credential selection and cooldowns.
	pool *poolState
}

// NewManager returns a dispatcher whose outbound traffic flows through bridge.
func NewManager(bridge *HostBridge) *Manager {
	return &Manager{
		bridge:    bridge,
		pool:      newPoolState(),
		snapshots: map[string]*catalog.Snapshot{},
	}
}

// HandleCall dispatches one RPC method and returns envelope bytes. Handler
// failures travel inside the envelope; a recovered panic becomes a
// plugin_error envelope so the host process never dies with us.
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
		return m.handleModels(request)
	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID}), nil
	case pluginabi.MethodAuthParse:
		return m.handleParseAuth(request)
	case pluginabi.MethodAuthLoginStart:
		return m.handleLoginStart(request)
	case pluginabi.MethodAuthLoginPoll:
		return m.handleLoginPoll(request)
	case pluginabi.MethodAuthRefresh:
		// The two keys do not expire, so there is nothing to renew. Reporting
		// the same record back is what stops the host from treating the auth
		// as stale.
		return m.handleAuthRefresh(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID}), nil
	case pluginabi.MethodExecutorExecute:
		return m.handleExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return m.handleExecuteStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return m.handleCountTokens(request)
	case pluginabi.MethodPluginShutdown:
		return m.handleShutdown()
	case pluginabi.MethodManagementRegister:
		return m.registerManagement(request)
	case pluginabi.MethodManagementHandle:
		return m.handleManagement(request)
	default:
		return ErrEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// ---------------------------------------------------------------------------
// registration
// ---------------------------------------------------------------------------

// lifecycleRequest mirrors rpcLifecycleRequest: config_yaml is base64 (the
// encoding/json []byte decoding handles it).
type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type capabilities struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
	ManagementAPI         bool                         `json:"management_api"`
}

type registrationResult struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

// formats are the three protocols this executor both accepts and emits.
var formats = []string{"chat-completions", "messages", "responses"}

// configFields declares the plugin's knobs for the host's Management Center.
// Names map 1:1 onto top-level keys under plugins.configs.<id>; nested options
// are declared as object fields rather than dotted names, because the host
// shallow-merges the submitted object.
func configFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{
			Name:        "api-keys",
			Type:        pluginapi.ConfigFieldTypeArray,
			Description: `Credentials: a JSON array of "sk-amr-…" keys or of {"value":"sk-amr-…","control":"vela_ctrl-…","label":"work"} objects. "${ENV_VAR}" is expanded inside the CPA process. A control key is optional and only an account sign-in produces one; a pasted key cannot read the plan allowance.`,
		},
		{
			Name:        "base-url",
			Type:        pluginapi.ConfigFieldTypeString,
			Description: "Model plane base URL. Default https://amr-link.open-design.ai/v1. https only unless allow-http is on; no query, fragment or userinfo.",
		},
		{
			Name:        "control-url",
			Type:        pluginapi.ConfigFieldTypeString,
			Description: "Vela control plane base URL, where the device sign-in is opened. Default https://amr-api.open-design.ai.",
		},
		{
			Name:        "models",
			Type:        pluginapi.ConfigFieldTypeObject,
			Description: `Restrict which models are published, e.g. {"allow":["glm-5.3-flash"],"deny":["claude-*"]}. A '*' matches any run of characters. An empty allow publishes everything; deny always wins. Ids match either the upstream id or the prefixed public id.`,
		},
		{
			Name:        "pool",
			Type:        pluginapi.ConfigFieldTypeObject,
			Description: `How one credential is chosen per request, e.g. {"strategy":"sticky","max-concurrency-per-account":0}. strategy is sticky (default) or round-robin; 0 means unlimited concurrency.`,
		},
		{
			Name:        "model-prefix",
			Type:        pluginapi.ConfigFieldTypeObject,
			Description: `Client-facing model id prefix, e.g. {"enabled":true,"value":"opendesign"}. When enabled, ids are published as <value>/<model>.`,
		},
		{
			Name:        "request-timeout",
			Type:        pluginapi.ConfigFieldTypeString,
			Description: "Upstream HTTP timeout, e.g. 5m. Also bounds the catalog and plan reads.",
		},
		{
			Name:        "max-response-bytes",
			Type:        pluginapi.ConfigFieldTypeInteger,
			Description: "Maximum non-streaming response body size in bytes. Default 67108864 (64 MiB).",
		},
		{
			Name:        "allow-http",
			Type:        pluginapi.ConfigFieldTypeBoolean,
			Description: "Permit http:// upstream URLs. For local testing only.",
		},
	}
}

func registrationEnvelope() []byte {
	return okEnvelope(registrationResult{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             displayName,
			Version:          pluginVersion,
			Author:           "Hex",
			GitHubRepository: githubRepoURL,
			ConfigFields:     configFields(),
		},
		Capabilities: capabilities{
			// This plugin IS a login provider: the account sign-in is what
			// produces the control key the plan allowance is read with, so
			// auth_provider is genuinely claimed here (unlike a plugin whose
			// credentials are all pasted keys).
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:  formats,
			ExecutorOutputFormats: formats,
			ManagementAPI:         true,
		},
	})
}

// handleLifecycle implements plugin.register / plugin.reconfigure: load the
// config, build the served model lists, publish state.
//
// Registration succeeds even when no live catalog can be read: the seed is a
// complete, usable list, and refusing to register would leave the operator with
// no way to fix the config from the UI.
func (m *Manager) handleLifecycle(request []byte) ([]byte, error) {
	var req lifecycleRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed lifecycle request body"), nil
	}
	cfg, err := config.LoadForRegistration(req.ConfigYAML)
	if err != nil {
		return ErrEnvelope("invalid_config", err.Error()), nil
	}
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()

	seed := catalog.NewSnapshot(catalog.Finalize(catalog.SeedEntries(cfg), cfg))
	snapshots := map[string]*catalog.Snapshot{}
	accounts := m.accountsOf(cfg)
	if len(accounts) > 0 && m.bridge != nil {
		ctx, cancel := context.WithTimeout(context.Background(), registerRefreshTimeout)
		for _, a := range accounts {
			result := catalog.Fetch(ctx, m.bridge.Transport(), cfg, a.RuntimeKey)
			switch result.Kind {
			case catalog.LiveOK:
				snapshots[accountKey(a.RuntimeKey)] = snapshotFrom(result)
			case catalog.LiveRefused:
				// Keep the seed for this account and let the operator see it.
				_ = m.log("warn", "an account's key was refused while reading its model catalog", map[string]any{
					"account": a.Label, "reason": result.Message,
				})
			default:
				_ = m.log("warn", "the model catalog could not be read; serving the seed", map[string]any{
					"account": a.Label, "error": errorText(result.Err),
				})
			}
		}
		cancel()
	} else if len(accounts) == 0 {
		_ = m.log("info", "opendesign plugin registered without credentials; add one in the plugin config or sign in", nil)
	}

	m.mu.Lock()
	m.cfg = cfg
	m.seed = seed
	m.snapshots = snapshots
	m.mu.Unlock()

	// The pool keeps its cooldowns across a reconfigure: a credential cooling
	// down because it was refused upstream is still the same credential.
	m.pool.retain(accounts)
	return registrationEnvelope(), nil
}

func (m *Manager) accountsOf(cfg config.Config) []config.Account {
	accounts := cfg.EffectiveAccounts()
	// A credential added from the Management Center lands as an auth record
	// rather than in the config, so the auth store is a second source.
	authAccounts := m.authRecordAccounts()
	seen := make(map[string]struct{}, len(accounts)+len(authAccounts))
	out := make([]config.Account, 0, len(accounts)+len(authAccounts))
	for _, a := range append(accounts, authAccounts...) {
		key := strings.TrimSpace(a.RuntimeKey)
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, a)
	}
	return out
}

func snapshotFrom(result catalog.LiveResult) *catalog.Snapshot {
	s := catalog.NewSnapshot(result.Model)
	s.Live = true
	return s
}

func (m *Manager) log(level, message string, fields map[string]any) error {
	if m.bridge == nil {
		return nil
	}
	return m.bridge.Log(level, message, fields)
}

// Shutdown stops every background activity and drains in-flight host callbacks.
//
// The drain matters on Unix: the loader frees host_api and dlclose's this library
// immediately after the shutdown export returns, so a goroutine still inside a
// host FFI frame at that moment would crash the process.
func (m *Manager) Shutdown() {
	_, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil)
}

// handleShutdown stops every background activity and drains in-flight host
// callbacks.
func (m *Manager) handleShutdown() ([]byte, error) {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	if m.bridge != nil && !m.bridge.WaitForInFlight(shutdownDrainTimeout) {
		_ = m.log("warn", "shutdown proceeding with host callbacks still in flight", nil)
	}
	m.mu.Lock()
	m.cfg, m.seed, m.snapshots = config.Config{}, nil, nil
	m.mu.Unlock()
	m.logins.Range(func(key, _ any) bool {
		m.logins.Delete(key)
		return true
	})
	return okEnvelope(struct{}{}), nil
}

// shutdownDrainTimeout bounds how long shutdown waits for orphaned callbacks.
var shutdownDrainTimeout = 15 * time.Second

// ---------------------------------------------------------------------------
// models
// ---------------------------------------------------------------------------

// modelsRequest covers model.static and model.for_auth: the static form carries
// no auth, the per-auth form carries one account's storage.
type modelsRequest struct {
	AuthID       string            `json:"AuthID"`
	AuthProvider string            `json:"AuthProvider"`
	StorageJSON  []byte            `json:"StorageJSON"`
	Metadata     map[string]any    `json:"Metadata"`
	Attributes   map[string]string `json:"Attributes"`
}

// handleModels implements model.static / model.for_auth.
//
// The per-auth form answers with THAT account's list, because the plan decides
// which models an account gets. A refusal (401/403) is reported through the
// response's AuthUpdate so the host can mark the sign-in; it is never silently
// replaced by the seed, which would make a dead key look healthy.
func (m *Manager) handleModels(request []byte) ([]byte, error) {
	var req modelsRequest
	if len(request) > 0 {
		_ = json.Unmarshal(request, &req)
	}
	m.mu.RLock()
	cfg, seed, snapshots := m.cfg, m.seed, m.snapshots
	m.mu.RUnlock()

	if seed == nil {
		// Asked before registration: publish nothing rather than serving a
		// list the config has not been validated against.
		return okEnvelope(pluginapi.ModelResponse{Provider: ProviderID}), nil
	}

	if req.AuthID != "" || len(req.StorageJSON) > 0 {
		record, errParse := m.parseAuthRecord(req.StorageJSON)
		if errParse == nil && record.hasRuntimeKey() {
			if snap, ok := m.snapshotFor(record, cfg, seed); ok {
				return okEnvelope(modelResponseOf(snap)), nil
			}
			// No usable snapshot: fetch one now, at this account's key.
			if m.bridge != nil {
				ctx, cancel := context.WithTimeout(context.Background(), registerRefreshTimeout)
				result := catalog.Fetch(ctx, m.bridge.Transport(), cfg, record.RuntimeKey)
				cancel()
				switch result.Kind {
				case catalog.LiveOK:
					snap := snapshotFrom(result)
					m.storeSnapshot(record.RuntimeKey, snap)
					return okEnvelope(modelResponseOf(snap)), nil
				case catalog.LiveRefused:
					resp := modelResponseOf(seed)
					resp.AuthUpdate = refusedAuthUpdate(record, result.Message)
					return okEnvelope(resp), nil
				}
			}
		}
		return okEnvelope(modelResponseOf(seed)), nil
	}
	// model.static: the seed plus any snapshots already fetched, so one
	// account's live list is not hidden by another's absence.
	return okEnvelope(modelResponseOf(m.unionSnapshot(seed, snapshots))), nil
}

// unionSnapshot merges a base list with every live snapshot's entries.
func (m *Manager) unionSnapshot(base *catalog.Snapshot, snapshots map[string]*catalog.Snapshot) *catalog.Snapshot {
	if len(snapshots) == 0 {
		return base
	}
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()
	seen := make(map[string]struct{})
	var entries []catalog.Entry
	var add func(*catalog.Snapshot)
	add = func(s *catalog.Snapshot) {
		if s == nil {
			return
		}
		for _, e := range s.Entries {
			if _, dup := seen[e.UpstreamID]; dup {
				continue
			}
			seen[e.UpstreamID] = struct{}{}
			entries = append(entries, e)
		}
	}
	for _, s := range snapshots {
		add(s)
	}
	add(base)
	return catalog.NewSnapshot(catalog.Finalize(entries, cfg))
}

func (m *Manager) snapshotFor(record *authRecord, cfg config.Config, seed *catalog.Snapshot) (*catalog.Snapshot, bool) {
	m.mu.RLock()
	snap, ok := m.snapshots[accountKey(record.RuntimeKey)]
	m.mu.RUnlock()
	if ok && snap != nil {
		return snap, true
	}
	return nil, false
}

func (m *Manager) storeSnapshot(runtimeKey string, snap *catalog.Snapshot) {
	m.mu.Lock()
	if m.snapshots == nil {
		m.snapshots = map[string]*catalog.Snapshot{}
	}
	m.snapshots[accountKey(runtimeKey)] = snap
	m.mu.Unlock()
}

func modelResponseOf(snap *catalog.Snapshot) pluginapi.ModelResponse {
	models := make([]pluginapi.ModelInfo, 0, len(snap.Entries))
	for _, e := range snap.Entries {
		models = append(models, modelInfoOf(e))
	}
	return pluginapi.ModelResponse{Provider: ProviderID, Models: models}
}

func modelInfoOf(e catalog.Entry) pluginapi.ModelInfo {
	info := pluginapi.ModelInfo{
		ID:                         e.PublicID,
		Object:                     "model",
		OwnedBy:                    ProviderID,
		DisplayName:                e.Name,
		Name:                       e.UpstreamID,
		ContextLength:              e.Context,
		MaxCompletionTokens:        e.Output,
		InputTokenLimit:            e.Context,
		OutputTokenLimit:           e.Output,
		SupportedInputModalities:   e.InputModalities(),
		SupportedOutputModalities:  e.OutputModalities(),
		SupportedGenerationMethods: []string{"chat.completions", "messages", "responses"},
		SupportedParameters:        supportedParameters(e),
	}
	if e.Thinking {
		info.Thinking = &pluginapi.ThinkingSupport{Levels: append([]string(nil), e.Efforts...)}
		if len(e.Efforts) == 0 {
			// A model that reasons without a level vocabulary: the client may
			// ask whether it thinks, and the answer is "yes, at no level".
			info.Thinking = &pluginapi.ThinkingSupport{}
		}
	}
	return info
}

func supportedParameters(e catalog.Entry) []string {
	params := []string{"max_tokens", "stream", "tools"}
	if e.Thinking {
		params = append(params, "reasoning_effort")
	}
	return params
}

// refusedAuthUpdate marks one account's sign-in as expired.
func refusedAuthUpdate(record *authRecord, message string) pluginapi.AuthData {
	meta := map[string]any{
		"type":     ProviderID,
		"status":   "error",
		"disabled": true,
	}
	if message != "" {
		meta["status_message"] = message
	}
	return pluginapi.AuthData{
		Provider:    ProviderID,
		ID:          record.AuthID(),
		FileName:    record.FileName(),
		Label:       record.label(),
		Disabled:    true,
		StorageJSON: record.storageJSON(),
		Metadata:    meta,
	}
}

// ---------------------------------------------------------------------------
// envelopes
// ---------------------------------------------------------------------------

func okEnvelope(result any) []byte {
	raw, _ := json.Marshal(result)
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return out
}

// ErrEnvelope renders a failure envelope.
func ErrEnvelope(code, message string) []byte {
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: message}})
	return out
}

// errorEnvelopeWithStatus renders a failure envelope carrying an HTTP status, so
// the host maps it onto the client response instead of a generic 502.
func errorEnvelopeWithStatus(code, message string, status int, retryable bool) []byte {
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{
		Code:       code,
		Message:    message,
		HTTPStatus: status,
		Retryable:  retryable,
	}})
	return out
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
