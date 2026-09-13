package main

import (
	"encoding/json"
	"testing"
)

// The fixtures below are verbatim entries captured from the live upstream
// catalogs (both account routes), trimmed to the fields the parser reads.
// They exist to pin the real key names: the parser previously decoded
// maxTokens/contextWindow, which the upstream never sends, so every model
// silently fell back to models.dev limits (up to 5x the real allowance).

// /v3/config -> data.models[]  (personal AND enterprise send this shape)
const realV3Catalog = `{"code":0,"data":{
  "agents":[{"name":"cli","models":["hy4-preview","hy3","hy3-x","deepseek-v4.1-flash","glm-5.3","glm-5.3-flash"]}],
  "models":[
    {"credits":"x0.03 credits","descriptionEn":"DeepSeek flagship model, supporting 1M context window, native multimodal model","descriptionZh":"DeepSeek 旗舰模型，支持 1M 上下文窗口，原生多模态","id":"deepseek-v4.1-flash","maxAllowedSize":1000000,"maxInputTokens":1000000,"maxOutputTokens":128000,"name":"Deepseek-V4.1-Flash","onlyReasoning":true,"reasoning":{"effort":"high","summary":"auto"},"supportsImages":true,"supportsReasoning":true,"supportsToolCall":true,"temperature":1,"vendor":"f"},
    {"credits":"x0.16 credits","id":"glm-5.3","maxAllowedSize":1000000,"maxInputTokens":1000000,"maxOutputTokens":48000,"name":"GLM-5.3","supportsImages":true,"supportsToolCall":true,"vendor":"z"},
    {"id":"hy3-x","maxAllowedSize":192000,"maxInputTokens":192000,"maxOutputTokens":64000,"name":"Hunyuan-X","vendor":"h"}
  ]}}`

// /console/enterprises/personal/models (and .../{enterpriseId}/models) — richer:
// adds iconUrl/isDefault/top_k/repetition_penalty, same limit keys.
const realFullCatalog = `{"code":0,"data":{"models":[
  {"credits":"x0.03 credits","descriptionEn":"DeepSeek flagship model, supporting 1M context window, native multimodal model","descriptionZh":"DeepSeek 旗舰模型，支持 1M 上下文窗口，原生多模态","disabledMultimodal":false,"id":"deepseek-v4.1-flash","isDefault":false,"maxAllowedSize":1000000,"maxInputTokens":1000000,"maxOutputTokens":128000,"name":"Deepseek-V4.1-Flash","onlyReasoning":true,"reasoning":{"effort":"high","summary":"auto"},"supportsImages":true,"supportsReasoning":true,"supportsToolCall":true,"tags":["craft"],"temperature":1,"top_p":1,"vendor":"f"},
  {"id":"glm-5.3-flash","maxAllowedSize":1000000,"maxInputTokens":1000000,"maxOutputTokens":32000,"name":"GLM-5.3-Flash","reasoning":{"canDisableThinking":true,"defaultEffort":"high","summary":"auto","supportedEfforts":["low","high","max"]}}
]}}`

func TestParseRealV3CatalogCarriesUpstreamLimits(t *testing.T) {
	got, err := parseWorkBuddyV3Config([]byte(realV3Catalog))
	if err != nil {
		t.Fatal(err)
	}
	// roster order wins, and detail-only models are not invented
	if len(got) != 6 {
		t.Fatalf("got %d models, want 6 (roster size): %#v", len(got), got)
	}
	if got[0].ID != "hy4-preview" || got[3].ID != "deepseek-v4.1-flash" {
		t.Fatalf("roster order lost: %#v", got)
	}
	byID := map[string]modelFacts{}
	for _, m := range got {
		byID[m.ID] = m
	}
	ds := byID["deepseek-v4.1-flash"]
	if ds.MaxCompletionTokens == nil || *ds.MaxCompletionTokens != 128000 {
		t.Fatalf("max completion = %#v, want 128000", ds.MaxCompletionTokens)
	}
	if ds.ContextLength == nil || *ds.ContextLength != 1000000 {
		t.Fatalf("context = %#v, want 1000000", ds.ContextLength)
	}
	if ds.Name != "Deepseek-V4.1-Flash" {
		t.Fatalf("name = %q", ds.Name)
	}
	if ds.Description == "" {
		t.Fatal("description lost (should fall back to descriptionEn)")
	}
	// roster ids without a detail row survive with nil limits
	if byID["hy4-preview"].MaxCompletionTokens != nil {
		t.Fatalf("hy4-preview should have nil limits: %#v", byID["hy4-preview"])
	}
}

func TestParseRealFullCatalogCarriesUpstreamLimits(t *testing.T) {
	got, err := parseWorkBuddyLegacyModels([]byte(realFullCatalog))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d models", len(got))
	}
	byID := map[string]modelFacts{}
	for _, m := range got {
		byID[m.ID] = m
	}
	ds := byID["deepseek-v4.1-flash"]
	if ds.MaxCompletionTokens == nil || *ds.MaxCompletionTokens != 128000 {
		t.Fatalf("max completion = %#v, want 128000", ds.MaxCompletionTokens)
	}
	if ds.ContextLength == nil || *ds.ContextLength != 1000000 {
		t.Fatalf("context = %#v, want 1000000", ds.ContextLength)
	}
	glm := byID["glm-5.3-flash"]
	if glm.MaxCompletionTokens == nil || *glm.MaxCompletionTokens != 32000 {
		t.Fatalf("glm-5.3-flash max completion = %#v, want 32000", glm.MaxCompletionTokens)
	}
}

// Both account routes serve the same catalog shape; the parser must not depend
// on enterpriseId being present (personal accounts work identically).
func TestParseCatalogIsRouteIndependent(t *testing.T) {
	viaV3, err := parseWorkBuddyV3Config([]byte(realV3Catalog))
	if err != nil {
		t.Fatal(err)
	}
	viaFull, err := parseWorkBuddyLegacyModels([]byte(realFullCatalog))
	if err != nil {
		t.Fatal(err)
	}
	for _, set := range [][]modelFacts{viaV3, viaFull} {
		for _, m := range set {
			if m.ID == "deepseek-v4.1-flash" {
				if m.MaxCompletionTokens == nil || *m.MaxCompletionTokens != 128000 {
					t.Fatalf("route gave different limits: %#v", m)
				}
			}
		}
	}
	// round-trip must be clean JSON
	if _, err := json.Marshal(viaV3); err != nil {
		t.Fatal(err)
	}
}
