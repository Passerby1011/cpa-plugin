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
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// qoderCNStaticCatalog20261019 is the Qoder CN chat-scene catalog as observed
// on 2026-10-19, kept as the offline fallback for /algo/api/v2/model/list.
//
// Two independently maintained snapshots agree on every entry below:
//   - techysy/10router providers/qoder-cn.json (refreshed 2026-10-19)
//   - shuishuipingan/qoder2api-hub qoder_catalog_cn.json (client catalog export
//     2026-09-29)
//
// docs.qoder.com/zh/qoder/model-selector and /zh/cli/model list the same
// display names. Keys are stable; display names drift with every model bump,
// which is exactly why the live catalog overrides this list at runtime.
//
// Dropped on purpose: the marketing-era tier keys (ultimate / performance /
// efficient / lite) — the CN catalog stopped publishing them in September 2026,
// though the gateway still accepts them — and the superseded model keys
// (qmodel_preview, q36fmodel, gm53model), which cpaToUpstreamKey still resolves
// so callers pinning an old id keep working.
var qoderCNStaticCatalog20261019 = []struct {
	id      string
	name    string
	context int64
}{
	{"auto", "Auto", 200000},
	{"qmodel_38max", "Qwen3.8-Max", 180000},
	{"qfmodel", "Qwen3.8-Flash", 180000},
	{"qmodel_latest", "Qwen3.7-Max", 180000},
	{"qmodel", "Qwen3.7-Plus", 180000},
	{"q37fmodel", "Qwen3.7-Flash", 180000},
	{"dmodel", "DeepSeek-V4-Pro", 96000},
	{"dfmodel", "DeepSeek-V4.1-Flash", 180000},
	{"gmodel", "GLM-5.3", 180000},
	{"gfmodel", "GLM-5.3-Flash", 1000000},
	{"gm51model", "GLM-5.2", 180000},
	{"kmodel_latest", "Kimi-K3", 180000},
	{"kmodel", "Kimi-K2.8-Preview", 180000},
	{"mmodel", "MiniMax-M2.7", 180000},
}

// wbModels is the static fallback model list for QoderWork CN. Aliases use the
// qoder/ prefix in AuthAttributes; bare IDs work too. Dynamic refresh via
// /algo/api/v2/model/list replaces this at runtime when an account is present.
func wbModels() []pluginapi.ModelInfo {
	models := make([]pluginapi.ModelInfo, 0, len(qoderCNStaticCatalog20261019))
	for _, entry := range qoderCNStaticCatalog20261019 {
		models = append(models, pluginapi.ModelInfo{
			ID:                         entry.id,
			Name:                       entry.name,
			ContextLength:              entry.context,
			MaxCompletionTokens:        8192,
			OwnedBy:                    providerName,
			SupportedGenerationMethods: []string{"chat"},
		})
	}
	return models
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
	if err != nil || len(files) == 0 {
		return models
	}
	// Strict filename-prefix match — same filter as host_auth.go hostAuthList.
	// (Earlier code also matched files containing "codebuddy" anywhere, which
	// would wrongly include workbuddy-*.json auths here and cause us to call
	// the QoderWork models API with a WorkBuddy token.)
	prefix := providerName + "-"
	for _, f := range files {
		if !strings.HasPrefix(strings.ToLower(f.Name), prefix) {
			continue
		}
		raw, err := hostAuthGetByIndex(f.AuthIndex)
		if err != nil {
			continue
		}
		sa, err := parseStored(raw)
		if err != nil || sa == nil {
			continue
		}
		dyn, err := callModelsAPI(sa)
		if err == nil && len(dyn) > 0 {
			storeDynamicModels(dyn)
			return dyn
		}
	}
	return models
}

func fetchDynamicModelsFromStorage(storageJSON []byte) []pluginapi.ModelInfo {
	if models, ok := cachedDynamicModels(); ok {
		return models
	}
	sa, err := parseStored(storageJSON)
	if err != nil || sa == nil {
		return fetchDynamicModels()
	}
	if dyn, err := callModelsAPI(sa); err == nil && len(dyn) > 0 {
		storeDynamicModels(dyn)
		return dyn
	}
	return fetchDynamicModels()
}

// fetchDynamicModels calls the QoderWork API to get the latest model list.
// Falls back to the hardcoded list on any error.
// callModelsAPI GETs /algo/api/v2/model/list from the QoderWork gateway
// with COSY signing (same as inference). Returns plain JSON (not QoderEncoding).
// Falls back to wbModels() on any error.
func callModelsAPI(sa *storedAuth) ([]pluginapi.ModelInfo, error) {
	return fetchModelListFrom(sa, endpointModels)
}

// fetchModelListFrom requests one catalog URL with COSY signing. Split out from
// callModelsAPI so tests can point it at a stub server.
func fetchModelListFrom(sa *storedAuth, rawURL string) ([]pluginapi.ModelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// The COSY signature covers the request body, so what is signed must be what
	// is sent. This GET carries no body: signing qoderEncode("{}") while sending
	// nothing made the gateway reject every refresh with 403 "Signature invalid",
	// which the plugin silently turned into a permanent fallback to the static
	// list (new upstream models never appeared).
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if err := applyCosyHeaders(req, sa, "", rawURL, "", false); err != nil {
		return nil, fmt.Errorf("cosy sign: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models API status %d", resp.StatusCode)
	}
	// Response is plain JSON: {"chat":[{key,display_name,...}], "developer":[...], ...}
	var apiResp map[string]json.RawMessage
	if err := json.Unmarshal(resp.Body, &apiResp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	// Prefer the "chat" scene (matches our inference use case).
	chatRaw, ok := apiResp["chat"]
	if !ok {
		return nil, fmt.Errorf("no chat scene in models response")
	}
	var models []struct {
		Key            string  `json:"key"`
		DisplayName    string  `json:"display_name"`
		Enable         bool    `json:"enable"`
		IsReasoning    bool    `json:"is_reasoning"`
		IsVL           bool    `json:"is_vl"`
		MaxInputTokens int64   `json:"max_input_tokens"`
		PriceFactor    float64 `json:"price_factor"`
	}
	if err := json.Unmarshal(chatRaw, &models); err != nil {
		return nil, fmt.Errorf("chat scene parse: %w", err)
	}
	var out []pluginapi.ModelInfo
	for _, m := range models {
		if !m.Enable {
			continue
		}
		ctx2 := int64(180000)
		if m.MaxInputTokens > 0 {
			ctx2 = m.MaxInputTokens
		}
		out = append(out, pluginapi.ModelInfo{
			ID:                         m.Key,
			Name:                       m.DisplayName,
			ContextLength:              ctx2,
			MaxCompletionTokens:        8192,
			OwnedBy:                    providerName,
			SupportedGenerationMethods: []string{"chat"},
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no enabled chat models")
	}
	return out, nil
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
// the QoderWork provider. The host passes this config via HostConfigSummary.
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
