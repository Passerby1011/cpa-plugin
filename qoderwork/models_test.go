// models_test.go pins the static catalog and the client-facing → upstream key
// mapping to the Qoder CN model list observed on 2026-10-19.
//
// Sources (two independent snapshots agreeing on every key/name pair):
//   - techysy/10router providers/qoder-cn.json (registry, refreshed 2026-10-19)
//     plus its comment block for the DeepSeek promo window;
//   - shuishuipingan/qoder2api-hub qoder_catalog_cn.json (client catalog export
//     2026-09-29) for price factor / context / is_vl cross-check.
//
// docs.qoder.com/zh/qoder/model-selector and /zh/cli/model carry the same
// display names.
package main

import "testing"

// cnCatalog20261019 is the CN chat-scene catalog the plugin must expose as its
// static fallback, in upstream key order.
var cnCatalog20261019 = []struct {
	key  string
	name string
	ctx  int64
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

func TestStaticCatalogMatchesCNModelList(t *testing.T) {
	models := wbModels()
	if len(models) != len(cnCatalog20261019) {
		t.Fatalf("static catalog has %d models, want %d", len(models), len(cnCatalog20261019))
	}
	for i, want := range cnCatalog20261019 {
		got := models[i]
		if got.ID != want.key {
			t.Errorf("models[%d].ID = %q, want %q", i, got.ID, want.key)
		}
		if got.Name != want.name {
			t.Errorf("models[%d] (%s) Name = %q, want %q", i, got.ID, got.Name, want.name)
		}
		if got.ContextLength != want.ctx {
			t.Errorf("models[%d] (%s) ContextLength = %d, want %d", i, got.ID, got.ContextLength, want.ctx)
		}
		if got.OwnedBy != providerName {
			t.Errorf("models[%d] (%s) OwnedBy = %q, want %q", i, got.ID, got.OwnedBy, providerName)
		}
	}
}

// Retired keys must stay out of the fallback list: the CN catalog stopped
// publishing the marketing-era tier rows (ultimate / performance / efficient /
// lite) and the pre-3.8 Qwen/Kimi/GLM/MiniMax keys in September 2026.
func TestStaticCatalogHasNoRetiredKeys(t *testing.T) {
	retired := map[string]bool{
		"ultimate": true, "performance": true, "efficient": true, "lite": true,
		"qmodel_preview": true, "q36fmodel": true, "gm53model": true,
	}
	for _, m := range wbModels() {
		if retired[m.ID] {
			t.Errorf("retired key %q is still in the static catalog", m.ID)
		}
	}
}

// The client-facing aliases below are the names users and other gateways have
// always typed; each must resolve to the current upstream key instead of being
// forwarded raw (an unknown key is routed to auto server-side, which silently
// answers with a different model than the caller asked for).
func TestCPAToUpstreamKeyMapsCurrentCatalog(t *testing.T) {
	cases := map[string]string{
		// current wire keys pass through
		"auto": "auto", "qmodel_38max": "qmodel_38max", "qfmodel": "qfmodel",
		"qmodel_latest": "qmodel_latest", "qmodel": "qmodel", "q37fmodel": "q37fmodel",
		"dmodel": "dmodel", "dfmodel": "dfmodel", "gmodel": "gmodel",
		"gfmodel": "gfmodel", "gm51model": "gm51model",
		"kmodel_latest": "kmodel_latest", "kmodel": "kmodel", "mmodel": "mmodel",
		// display-name aliases
		"qwen3.8-max": "qmodel_38max", "qwen-3.8-max": "qmodel_38max",
		"qwen3.8-max-preview": "qmodel_38max", "qwen3.8-flash": "qfmodel",
		"qwen3.7-max": "qmodel_latest", "qwen3.7-plus": "qmodel",
		"qwen3.6-plus": "qmodel", "qwen3.7-flash": "q37fmodel",
		"deepseek-v4-pro": "dmodel", "deepseek-v4-flash": "dfmodel",
		"deepseek-v4.1-flash": "dfmodel", "deepseek-flash": "dfmodel",
		"glm-5.3": "gmodel", "glm-5.3-flash": "gfmodel", "glm-5.2": "gm51model",
		"glm-5.1": "gm51model", "kimi-k3": "kmodel_latest",
		"kimi-k2.8-preview": "kmodel", "kimi-k2.7-code": "kmodel",
		"kimi-k2.8": "kmodel", "minimax-m3": "mmodel", "minimax-m2.7": "mmodel",
		// legacy wire keys that still hit the gateway
		"qmodel_preview": "qmodel_38max", "q36fmodel": "q37fmodel",
		"gm53model": "gmodel",
	}
	for in, want := range cases {
		if got := cpaToUpstreamKey(in); got != want {
			t.Errorf("cpaToUpstreamKey(%q) = %q, want %q", in, got, want)
		}
	}
}
