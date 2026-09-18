// models.go implements the ModelProvider capability: static and per-auth
// model lists, dynamic model discovery via the upstream models API, alias
// reverse resolution (client-facing alias → upstream model id), and the
// host-config oauth-excluded-models filter.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// wbModels is the static fallback model list for QwenWork. Keys are the
// upstream model keys (pro/flash/qwen3.8-max-preview, qwork scene). Dynamic
// refresh via /algo/api/v2/model/list replaces this at runtime when an account
// is present.
//
// DisplayName here carries the server display_name ONLY — never the charge
// rate. The rate (price_factor) is volatile: it was observed flipping
// 1.1 -> 1.8 for qwen3.8-max-preview within ~30 minutes (server-side
// repricing/promotion). A hardcoded rate in the static fallback would drift and
// show a wrong multiplier whenever the dynamic fetch is unavailable. The rate is
// attached ONLY by parseQworkModels, which reads the live price_factor — same
// discipline as the WorkBuddy plugin (display_name shows the rate only when the
// catalog actually supplied one). Display names mirror the live qwork scene:
// pro=高级, flash=标准｜Qwen3.8-Flash, qwen3.8-max-preview=Qwen3.8-Max.
func wbModels() []pluginapi.ModelInfo {
	return []pluginapi.ModelInfo{
		{ID: "pro", Name: "QwenWork 高级 (Pro)", DisplayName: "高级", ContextLength: 1000000, MaxCompletionTokens: 32768, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "flash", Name: "QwenWork Qwen3.8-Flash", DisplayName: "标准｜Qwen3.8-Flash", ContextLength: 1000000, MaxCompletionTokens: 32768, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "qwen3.8-max-preview", Name: "QwenWork Qwen3.8-Max", DisplayName: "Qwen3.8-Max", ContextLength: 1000000, MaxCompletionTokens: 32768, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
	}
}

func cachedDynamicModels() ([]pluginapi.ModelInfo, bool) {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	if len(dynamicModelsCache.models) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsCacheTTL {
		return dynamicModelsCache.models, true
	}
	return nil, false
}

func storeDynamicModels(models []pluginapi.ModelInfo) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.models = models
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
}

func fetchDynamicModels() []pluginapi.ModelInfo {
	if models, ok := cachedDynamicModels(); ok {
		return models
	}
	models := wbModels()
	files, err := hostAuthListFiles()
	if err != nil {
		modelLogf("auth_list", "auth list unavailable (%v) — serving static model table", err)
		return models
	}
	if len(files) == 0 {
		modelLogf("auth_empty", "no auth files visible via host bridge — serving static model table")
		return models
	}
	// Strict filename-prefix match — same filter as host_auth.go hostAuthList.
	// (Earlier code also matched files containing "codebuddy" anywhere, which
	// would wrongly include workbuddy-*.json auths here and cause us to call
	// the QwenWork models API with a WorkBuddy token.)
	prefix := providerName + "-"
	for _, f := range files {
		if !strings.HasPrefix(strings.ToLower(f.Name), prefix) {
			continue
		}
		raw, err := hostAuthGetByIndex(f.AuthIndex)
		if err != nil {
			modelLogf("auth_get", "auth %s unreadable (%v) — trying next", f.Name, err)
			continue
		}
		sa, err := parseStored(raw)
		if err != nil || sa == nil {
			modelLogf("auth_parse", "auth %s unparseable (%v) — trying next", f.Name, err)
			continue
		}
		dyn, err := callModelsAPI(sa)
		if err == nil && len(dyn) > 0 {
			storeDynamicModels(dyn)
			return dyn
		}
		modelLogf("models_api", "models API via %s failed: %v", f.Name, err)
	}
	return models
}

func fetchDynamicModelsFromStorage(storageJSON []byte) []pluginapi.ModelInfo {
	if models, ok := cachedDynamicModels(); ok {
		return models
	}
	sa, err := parseStored(storageJSON)
	if err != nil || sa == nil {
		modelLogf("storage_parse", "for_auth storage JSON unparseable (%v) — serving static model table", err)
		return fetchDynamicModels()
	}
	if dyn, err := callModelsAPI(sa); err == nil && len(dyn) > 0 {
		storeDynamicModels(dyn)
		return dyn
	} else if err != nil {
		modelLogf("models_api", "for_auth models API failed: %v", err)
	}
	return fetchDynamicModels()
}

// fetchDynamicModels calls the QwenWork API to get the latest model list.
// Falls back to the hardcoded list on any error.
// callModelsAPI GETs /algo/api/v2/model/list from the QwenWork gateway with
// COSY signing (same as inference). QwenWork signs the raw body directly (no
// QoderEncoding); for a GET there is no body, so sign with an empty string.
// Falls back to wbModels() on any error.
func callModelsAPI(sa *storedAuth) ([]pluginapi.ModelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	bodyStr := ""
	rawURL := endpointModels
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if err := applyCosyHeaders(req, sa, bodyStr, rawURL, "", false); err != nil {
		return nil, fmt.Errorf("cosy sign: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
	// Direct HTTP, not hostHTTPDo: this runs inside synchronous model.static /
	// model.for_auth RPCs, where nested host-bridge calls fail at the transport
	// layer (observed "models API status 0" on linux, matching the stack-move
	// mitigation Windows already bypasses below). The models API is an idempotent
	// GET with no payload to audit, so the plugin's own client is fine here —
	// the chat executor still routes through the bridge for request logging.
	resp, err := hostHTTPDoDirect(req, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models API status %d", resp.StatusCode)
	}
	// Response is plain JSON: {"qwork":[...], "chat":[], "developer":[], ...}
	// QwenWork reports its models under the "qwork" scene (the "chat" scene is
	// empty). See live /algo/api/v2/model/list (pro/flash/qwen3.8-max-preview).
	var apiResp map[string]json.RawMessage
	if err := json.Unmarshal(resp.Body, &apiResp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	rawList, ok := apiResp["qwork"]
	if !ok {
		return nil, fmt.Errorf("no qwork scene in models response")
	}
	return parseQworkModels(rawList)
}

// qworkModelEntry is one upstream model record from the qwork scene of
// /algo/api/v2/model/list.
type qworkModelEntry struct {
	Key            string  `json:"key"`
	DisplayName    string  `json:"display_name"`
	Enable         bool    `json:"enable"`
	IsReasoning    bool    `json:"is_reasoning"`
	IsVL           bool    `json:"is_vl"`
	Format         string  `json:"format"`
	Source         string  `json:"source"`
	MaxInputTokens int64   `json:"max_input_tokens"`
	PriceFactor    float64 `json:"price_factor"`
	// ContextConfig mirrors the upstream {"1M":{token_count,is_default},...}
	// map. The desktop client picks the is_default entry's token_count as the
	// effective context window (currently 1M), so we do the same.
	ContextConfig map[string]contextWindowEntry `json:"context_config"`
}

// parseQworkModels maps the raw qwork-scene JSON array onto host ModelInfo
// entries. Extracted from callModelsAPI so the mapping is testable against a
// recorded fixture without touching the network or COSY signing.
func parseQworkModels(rawList json.RawMessage) ([]pluginapi.ModelInfo, error) {
	var models []qworkModelEntry
	if err := json.Unmarshal(rawList, &models); err != nil {
		return nil, fmt.Errorf("qwork scene parse: %w", err)
	}
	var out []pluginapi.ModelInfo
	for _, m := range models {
		if !m.Enable {
			continue
		}
		// Context window: the desktop client derives it from context_config's
		// is_default entry (currently 1M), NOT max_input_tokens (180k). Mirror
		// that so /v1/models advertises the same window the client shows.
		ctx2 := defaultContextWindow(m.ContextConfig)
		if ctx2 <= 0 && m.MaxInputTokens > 0 {
			ctx2 = m.MaxInputTokens
		}
		if ctx2 <= 0 {
			ctx2 = 180000
		}
		out = append(out, pluginapi.ModelInfo{
			ID:                         m.Key,
			Name:                       m.DisplayName,
			DisplayName:                modelDisplayName(m.DisplayName, formatPriceFactor(m.PriceFactor)),
			ContextLength:              ctx2,
			MaxCompletionTokens:        8192,
			OwnedBy:                    providerName,
			SupportedGenerationMethods: []string{"chat"},
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no enabled chat models")
	}
	storeModelMeta(models)
	return out, nil
}

// modelMeta carries the server-driven fields the desktop client uses to build
// the chat request's model_config (qoder-agent-sdk o9c). The plugin previously
// hardcoded display_name=key, is_vl=true, is_reasoning=false; the client reads
// ALL of these from the live model record. Mirroring that means a future
// reasoning model lights up automatically (is_reasoning / thinking behaviour)
// instead of being mislabelled.
type modelMeta struct {
	DisplayName    string
	IsReasoning    bool
	IsVL           bool
	Format         string
	Source         string
	MaxInputTokens int64
}

// modelMetaCache maps upstream model key -> server metadata, refreshed by the
// same dynamic catalog fetch that fills dynamicModelsCache. Keyed by upstream
// key (pro/flash/qwen3.8-max-preview), which is what buildQwenBody receives.
var modelMetaCache struct {
	sync.RWMutex
	byKey map[string]modelMeta
}

// defaultModelMeta is the fallback when the dynamic catalog has not been
// fetched (no account, API down, TTL miss). It reproduces the values the plugin
// hardcoded before this change so behaviour is unchanged without live metadata:
// display_name falls back to the key (client: n?.display_name ?? r), is_vl
// stays true, is_reasoning false, format openai, source system, max 180000.
func defaultModelMeta(key string) modelMeta {
	return modelMeta{DisplayName: key, IsVL: true, Format: "openai", Source: "system", MaxInputTokens: 180000}
}

func storeModelMeta(models []qworkModelEntry) {
	byKey := make(map[string]modelMeta, len(models))
	for _, m := range models {
		if !m.Enable {
			continue
		}
		byKey[m.Key] = modelMeta{
			DisplayName:    m.DisplayName,
			IsReasoning:    m.IsReasoning,
			IsVL:           m.IsVL,
			Format:         m.Format,
			Source:         m.Source,
			MaxInputTokens: m.MaxInputTokens,
		}
	}
	modelMetaCache.Lock()
	modelMetaCache.byKey = byKey
	modelMetaCache.Unlock()
}

// lookupModelMeta returns the cached server metadata for an upstream key, or
// defaultModelMeta when the dynamic catalog has not populated it. This is the
// plugin analogue of the client's e.getModel(key) ?? defaults in o9c.
func lookupModelMeta(key string) modelMeta {
	modelMetaCache.RLock()
	m, ok := modelMetaCache.byKey[key]
	modelMetaCache.RUnlock()
	if !ok {
		return defaultModelMeta(key)
	}
	// Fill any field the server omitted with the same defaults the client uses,
	// so a partially-populated record still produces a valid model_config.
	if m.DisplayName == "" {
		m.DisplayName = key
	}
	if m.Format == "" {
		m.Format = "openai"
	}
	if m.Source == "" {
		m.Source = "system"
	}
	if m.MaxInputTokens <= 0 {
		m.MaxInputTokens = 200000 // client: n?.max_input_tokens ?? 2e5
	}
	return m
}

// buildModelConfig renders the chat body's model_config map exactly as the
// desktop client does (qoder-agent-sdk o9c, system-model branch): every field
// is server-driven, with the client's own fallbacks. The BYOK/custom-model
// branch is unreachable here — QwenWork's qwork scene returns no
// outer_provider/custom_provider_adapter, so source is always "system".
func buildModelConfig(key string, meta modelMeta) map[string]any {
	return map[string]any{
		"key":              key,
		"display_name":     meta.DisplayName,
		"model":            "",
		"format":           meta.Format,
		"is_vl":            meta.IsVL,
		"is_reasoning":     meta.IsReasoning,
		"api_key":          "",
		"url":              "",
		"source":           meta.Source,
		"max_input_tokens": meta.MaxInputTokens,
	}
}

// contextWindowEntry is one entry of the upstream context_config map.
type contextWindowEntry struct {
	TokenCount int64 `json:"token_count"`
	IsDefault  bool  `json:"is_default"`
}

// defaultContextWindow returns the token_count of the is_default entry in
// context_config, matching the desktop client's getDefaultContextConfigTokenCount.
// Returns 0 when the map is absent or has no usable default.
func defaultContextWindow(cfg map[string]contextWindowEntry) int64 {
	var best int64
	for _, e := range cfg {
		if e.TokenCount <= 0 {
			continue
		}
		if e.IsDefault {
			return e.TokenCount
		}
		if e.TokenCount > best {
			best = e.TokenCount
		}
	}
	return best
}

// modelDisplayName appends the upstream charge rate to the model name, same
// shape as the WorkBuddy plugin ("名称 · x倍率"). Empty rate keeps the plain
// name; the host falls back to the model ID only when DisplayName is empty,
// so a nameless+rateless model intentionally returns "".
func modelDisplayName(name, rate string) string {
	name = strings.TrimSpace(name)
	rate = strings.TrimSpace(rate)
	if rate == "" {
		return name
	}
	if name == "" {
		return rate
	}
	return name + " · " + rate
}

// formatPriceFactor renders the upstream price_factor as "x1" / "x0.1" /
// "x1.1" (trailing zeros trimmed). A factor <= 0 means "not published" and
// renders as "" so the display name stays unadorned.
func formatPriceFactor(f float64) string {
	if f <= 0 {
		return ""
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	return "x" + s
}

func cacheModelAliases(host pluginapi.HostConfigSummary) {
	entries := host.OAuthModelAlias[providerName]
	if len(entries) == 0 {
		// Host may key the channel case-insensitively; fall back to a scan.
		for channel, list := range host.OAuthModelAlias {
			if strings.EqualFold(strings.TrimSpace(channel), providerName) {
				entries = list
				break
			}
		}
	}
	byAlias := make(map[string]string, len(entries))
	for _, e := range entries {
		name := strings.TrimSpace(e.Name)
		alias := strings.TrimSpace(e.Alias)
		if name == "" || alias == "" || strings.EqualFold(name, alias) {
			continue
		}
		byAlias[strings.ToLower(alias)] = name
	}
	modelAliasCache.Lock()
	modelAliasCache.byAlias = byAlias
	modelAliasCache.Unlock()
}

// filterExcludedModels removes models listed in oauth-excluded-models for
// the QwenWork provider. The host passes this config via HostConfigSummary.
func filterExcludedModels(models []pluginapi.ModelInfo, host pluginapi.HostConfigSummary) []pluginapi.ModelInfo {
	if len(host.ExcludedModels) == 0 {
		return models
	}
	// Try exact provider match, then case-insensitive scan.
	excluded := host.ExcludedModels[providerName]
	if len(excluded) == 0 {
		for channel, list := range host.ExcludedModels {
			if strings.EqualFold(strings.TrimSpace(channel), providerName) {
				excluded = list
				break
			}
		}
	}
	if len(excluded) == 0 {
		return models
	}
	excludeSet := make(map[string]struct{}, len(excluded))
	for _, m := range excluded {
		excludeSet[strings.ToLower(strings.TrimSpace(m))] = struct{}{}
	}
	// Use a fresh slice — models[:0] would alias the input's backing array,
	// which may be the dynamicModelsCache's own slice. Mutating it in place
	// would corrupt the cache for subsequent callers (P0 bug: after one
	// filterExcludedModels call, cache returns the filtered list as the
	// "full" list on the next fetch).
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if _, skip := excludeSet[strings.ToLower(m.ID)]; skip {
			continue
		}
		out = append(out, m)
	}
	return out
}

// publishUsage reports one upstream attempt into CPAMP request monitoring.
// requestedModel is client-facing (may be alias); upstreamModel is resolved.

func handleModelStatic(raw []byte) ([]byte, error) {
	var req pluginapi.StaticModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cacheModelAliases(req.Host)
	models := fetchDynamicModels()
	models = filterExcludedModels(models, req.Host)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}

func handleModelForAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// Always return the plugin's canonical provider key. The host skips any
	// response whose Provider doesn't match the auth's provider, so echoing
	// req.AuthProvider back would silently drop the model list whenever the
	// auth file carries a non-canonical provider string.
	cacheModelAliases(req.Host)
	models := fetchDynamicModelsFromStorage(req.StorageJSON)
	models = filterExcludedModels(models, req.Host)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}
