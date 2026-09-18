package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// qworkSceneFixture is the qwork-scene array recorded from the live
// /algo/api/v2/model/list on 2026-09-17 (gateway.qwenwork.cn), trimmed to the
// fields the parser reads. The full-width bar (｜) and i18n are part of the
// real upstream display_name values — keep them byte-identical.
const qworkSceneFixture = `[
  {
    "key": "pro",
    "display_name": "高级",
    "enable": true,
    "is_reasoning": false,
    "is_vl": true,
    "max_input_tokens": 180000,
    "price_factor": 1,
    "context_config": {
      "1M": {"is_default": true, "token_count": 1000000},
      "200K": {"token_count": 200000},
      "400K": {"token_count": 400000}
    }
  },
  {
    "key": "flash",
    "display_name": "标准｜Qwen3.8-Flash",
    "enable": true,
    "is_reasoning": false,
    "is_vl": true,
    "max_input_tokens": 180000,
    "price_factor": 0.1,
    "tags": ["is_recommend"],
    "context_config": {
      "1M": {"is_default": true, "token_count": 1000000},
      "200K": {"token_count": 200000},
      "400K": {"token_count": 400000}
    }
  },
  {
    "key": "qwen3.8-max-preview",
    "display_name": "Qwen3.8-Max",
    "enable": true,
    "is_reasoning": false,
    "is_vl": true,
    "max_input_tokens": 180000,
    "price_factor": 1.1,
    "context_config": {
      "1M": {"is_default": true, "token_count": 1000000},
      "200K": {"token_count": 200000},
      "400K": {"token_count": 400000}
    }
  }
]`

func TestParseQworkModelsMatchesLiveCatalog(t *testing.T) {
	got, err := parseQworkModels(json.RawMessage(qworkSceneFixture))
	if err != nil {
		t.Fatalf("parseQworkModels: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	byID := map[string]int{}
	for i, m := range got {
		byID[m.ID] = i
	}

	// The three qwork keys and their display_name + rate, exactly as the
	// desktop client shows them (display name + 倍率 in the model popup).
	want := []struct {
		id, display string
		ctx         int64
	}{
		{"pro", "高级 · x1", 1000000},
		{"flash", "标准｜Qwen3.8-Flash · x0.1", 1000000},
		{"qwen3.8-max-preview", "Qwen3.8-Max · x1.1", 1000000},
	}
	for _, w := range want {
		i, ok := byID[w.id]
		if !ok {
			t.Fatalf("model %q missing from parse result", w.id)
		}
		m := got[i]
		if m.DisplayName != w.display {
			t.Errorf("%s DisplayName = %q, want %q", w.id, m.DisplayName, w.display)
		}
		// The client derives the window from context_config's is_default
		// entry (1M), NOT max_input_tokens (180k).
		if m.ContextLength != w.ctx {
			t.Errorf("%s ContextLength = %d, want %d", w.id, m.ContextLength, w.ctx)
		}
		if m.OwnedBy != providerName {
			t.Errorf("%s OwnedBy = %q, want %q", w.id, m.OwnedBy, providerName)
		}
	}
}

func TestParseQworkModelsSkipsDisabled(t *testing.T) {
	raw := `[
      {"key":"pro","display_name":"高级","enable":true,"price_factor":1},
      {"key":"safety","display_name":"企业专属","enable":false,"price_factor":1}
    ]`
	got, err := parseQworkModels(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("parseQworkModels: %v", err)
	}
	if len(got) != 1 || got[0].ID != "pro" {
		t.Fatalf("want only pro, got %+v", got)
	}
}

func TestParseQworkModelsAllDisabledIsError(t *testing.T) {
	raw := `[{"key":"pro","display_name":"高级","enable":false,"price_factor":1}]`
	if _, err := parseQworkModels(json.RawMessage(raw)); err == nil {
		t.Fatal("want error when every model is disabled")
	}
}

func TestParseQworkModelsContextFallbacks(t *testing.T) {
	// No context_config -> fall back to max_input_tokens.
	raw := `[{"key":"pro","display_name":"高级","enable":true,"max_input_tokens":180000,"price_factor":1}]`
	got, err := parseQworkModels(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got[0].ContextLength != 180000 {
		t.Errorf("ContextLength = %d, want 180000 (max_input_tokens fallback)", got[0].ContextLength)
	}
	// Neither context_config nor max_input_tokens -> 180000 floor.
	raw2 := `[{"key":"pro","display_name":"高级","enable":true,"price_factor":1}]`
	got2, err := parseQworkModels(json.RawMessage(raw2))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got2[0].ContextLength != 180000 {
		t.Errorf("ContextLength = %d, want 180000 floor", got2[0].ContextLength)
	}
	// context_config without is_default -> largest entry wins.
	raw3 := `[{"key":"pro","display_name":"高级","enable":true,"price_factor":1,
      "context_config":{"200K":{"token_count":200000},"400K":{"token_count":400000}}}]`
	got3, err := parseQworkModels(json.RawMessage(raw3))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got3[0].ContextLength != 400000 {
		t.Errorf("ContextLength = %d, want 400000 (largest when no default)", got3[0].ContextLength)
	}
}

func TestFormatPriceFactor(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{1, "x1"},
		{0.1, "x0.1"},
		{1.1, "x1.1"},
		{0.79, "x0.79"},
		{2, "x2"},
		{0, ""},  // not published
		{-1, ""}, // defensive
		{0.05, "x0.05"},
	}
	for _, c := range cases {
		if got := formatPriceFactor(c.in); got != c.want {
			t.Errorf("formatPriceFactor(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestModelDisplayName(t *testing.T) {
	cases := []struct {
		name, rate, want string
	}{
		{"高级", "x1", "高级 · x1"},
		{"标准｜Qwen3.8-Flash", "x0.1", "标准｜Qwen3.8-Flash · x0.1"},
		{"Qwen3.8-Max", "x1.1", "Qwen3.8-Max · x1.1"},
		{"高级", "", "高级"},              // no rate -> plain name
		{"", "x0.5", "x0.5"},          // rate without a name
		{"  高级  ", " x1 ", "高级 · x1"}, // padding trimmed
	}
	for _, c := range cases {
		if got := modelDisplayName(c.name, c.rate); got != c.want {
			t.Errorf("modelDisplayName(%q,%q) = %q, want %q", c.name, c.rate, got, c.want)
		}
	}
}

// The static fallback and the dynamic catalog must agree on the three keys and
// the 1M context window, but they DELIBERATELY differ on DisplayName: the
// dynamic path appends the live charge rate ("名称 · x倍率") while the static
// fallback carries the plain name only. The rate is volatile (observed
// 1.1 -> 1.8 within ~30 min), so the static fallback must never bake one in —
// otherwise it shows a stale multiplier whenever the dynamic fetch is down.
func TestWbModelsStaticMatchesDynamicShape(t *testing.T) {
	static := wbModels()
	dyn, err := parseQworkModels(json.RawMessage(qworkSceneFixture))
	if err != nil {
		t.Fatalf("fixture parse: %v", err)
	}
	if len(static) != len(dyn) {
		t.Fatalf("static=%d dynamic=%d", len(static), len(dyn))
	}
	byID := map[string]int{}
	for i, m := range dyn {
		byID[m.ID] = i
	}
	// rate the fixture publishes per key, used to derive the expected dynamic
	// display name from the static (plain) one.
	rateByID := map[string]string{
		"pro":                 "x1",
		"flash":               "x0.1",
		"qwen3.8-max-preview": "x1.1",
	}
	for _, s := range static {
		i, ok := byID[s.ID]
		if !ok {
			t.Fatalf("static model %q not in dynamic catalog", s.ID)
		}
		d := dyn[i]
		if s.ContextLength != d.ContextLength {
			t.Errorf("%s static ContextLength=%d dynamic=%d", s.ID, s.ContextLength, d.ContextLength)
		}
		// Static must carry the plain server display_name with NO rate suffix.
		if strings.Contains(s.DisplayName, " · x") {
			t.Errorf("%s static DisplayName=%q must not embed a volatile rate", s.ID, s.DisplayName)
		}
		// Dynamic must equal the plain name plus the live rate.
		wantDyn := s.DisplayName + " · " + rateByID[s.ID]
		if d.DisplayName != wantDyn {
			t.Errorf("%s dynamic DisplayName=%q, want %q", s.ID, d.DisplayName, wantDyn)
		}
	}
}

// reasoningSceneFixture proves is_reasoning / is_vl / display_name flow from the
// server into the chat body's model_config. The live qwork scene currently has
// all three models is_reasoning=false, so a hardcoded false was harmless TODAY —
// but the desktop client (o9c) reads every field from getModel(key). This
// fixture carries a reasoning model so the server-driven path is actually
// exercised: if the plugin ever reverts to hardcoding, this test fails.
const reasoningSceneFixture = `[
  {
    "key": "thinker",
    "display_name": "深度思考",
    "enable": true,
    "is_reasoning": true,
    "is_vl": false,
    "format": "openai",
    "source": "system",
    "max_input_tokens": 131072,
    "price_factor": 2
  },
  {
    "key": "pro",
    "display_name": "高级",
    "enable": true,
    "is_reasoning": false,
    "is_vl": true,
    "format": "openai",
    "source": "system",
    "max_input_tokens": 180000,
    "price_factor": 1
  }
]`

func TestModelConfigIsServerDriven(t *testing.T) {
	// Populate the metadata cache from the fixture (parseQworkModels stores it).
	if _, err := parseQworkModels(json.RawMessage(reasoningSceneFixture)); err != nil {
		t.Fatalf("fixture parse: %v", err)
	}

	// A reasoning model must surface is_reasoning=true, is_vl=false and its real
	// server display_name / format / source / max_input_tokens — NOT the old
	// hardcoded {display_name:key, is_vl:true, is_reasoning:false, 180000}.
	meta := lookupModelMeta("thinker")
	if !meta.IsReasoning {
		t.Errorf("thinker IsReasoning = false, want true (server-driven)")
	}
	if meta.IsVL {
		t.Errorf("thinker IsVL = true, want false (server-driven)")
	}
	if meta.DisplayName != "深度思考" {
		t.Errorf("thinker DisplayName = %q, want 深度思考", meta.DisplayName)
	}
	if meta.MaxInputTokens != 131072 {
		t.Errorf("thinker MaxInputTokens = %d, want 131072", meta.MaxInputTokens)
	}

	mc := buildModelConfig("thinker", meta)
	if mc["is_reasoning"] != true {
		t.Errorf("model_config.is_reasoning = %v, want true", mc["is_reasoning"])
	}
	if mc["is_vl"] != false {
		t.Errorf("model_config.is_vl = %v, want false", mc["is_vl"])
	}
	if mc["display_name"] != "深度思考" {
		t.Errorf("model_config.display_name = %v, want 深度思考", mc["display_name"])
	}
	if mc["source"] != "system" {
		t.Errorf("model_config.source = %v, want system", mc["source"])
	}
	// The non-reasoning sibling stays is_reasoning=false (no cross-contamination).
	if got := lookupModelMeta("pro").IsReasoning; got {
		t.Errorf("pro IsReasoning = true, want false")
	}
}

// The chat body must carry the server-driven is_reasoning in BOTH places the
// client puts it: outer model_config.is_reasoning and inner
// chat_context.extra.modelConfig.is_reasoning (c9c).
func TestBuildQwenBodyPropagatesReasoning(t *testing.T) {
	if _, err := parseQworkModels(json.RawMessage(reasoningSceneFixture)); err != nil {
		t.Fatalf("fixture parse: %v", err)
	}
	raw, err := buildQwenBody(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, "thinker")
	if err != nil {
		t.Fatalf("buildQwenBody: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("body unmarshal: %v", err)
	}
	mc, _ := body["model_config"].(map[string]any)
	if mc == nil || mc["is_reasoning"] != true {
		t.Fatalf("outer model_config.is_reasoning = %v, want true", mc["is_reasoning"])
	}
	cc, _ := body["chat_context"].(map[string]any)
	extra, _ := cc["extra"].(map[string]any)
	inner, _ := extra["modelConfig"].(map[string]any)
	if inner == nil || inner["is_reasoning"] != true {
		t.Fatalf("inner modelConfig.is_reasoning = %v, want true", inner["is_reasoning"])
	}
	if inner["key"] != "thinker" {
		t.Errorf("inner modelConfig.key = %v, want thinker", inner["key"])
	}
}

// With no catalog fetched (cache empty) the plugin must fall back to the exact
// shape it used before this change, so an account-less / API-down request is
// byte-identical to the previous behaviour.
func TestModelConfigDefaultsWhenCatalogUnfetched(t *testing.T) {
	modelMetaCache.Lock()
	modelMetaCache.byKey = nil
	modelMetaCache.Unlock()

	meta := lookupModelMeta("pro")
	if meta.IsReasoning {
		t.Errorf("default IsReasoning = true, want false")
	}
	if !meta.IsVL {
		t.Errorf("default IsVL = false, want true (legacy hardcoded)")
	}
	if meta.DisplayName != "pro" {
		t.Errorf("default DisplayName = %q, want key 'pro'", meta.DisplayName)
	}
	if meta.MaxInputTokens != 180000 {
		t.Errorf("default MaxInputTokens = %d, want 180000", meta.MaxInputTokens)
	}
	mc := buildModelConfig("pro", meta)
	if mc["is_vl"] != true || mc["is_reasoning"] != false || mc["format"] != "openai" || mc["source"] != "system" {
		t.Errorf("default model_config = %+v, want legacy hardcoded shape", mc)
	}
}

// A partially-populated record (server omitted format/source/max_input_tokens)
// must still get the client's own per-field fallbacks, not zero values.
func TestLookupModelMetaFillsClientFallbacks(t *testing.T) {
	raw := `[{"key":"sparse","display_name":"","enable":true,"is_reasoning":false,"is_vl":true}]`
	if _, err := parseQworkModels(json.RawMessage(raw)); err != nil {
		t.Fatalf("fixture parse: %v", err)
	}
	meta := lookupModelMeta("sparse")
	if meta.DisplayName != "sparse" {
		t.Errorf("empty display_name should fall back to key, got %q", meta.DisplayName)
	}
	if meta.Format != "openai" {
		t.Errorf("missing format should fall back to openai, got %q", meta.Format)
	}
	if meta.Source != "system" {
		t.Errorf("missing source should fall back to system, got %q", meta.Source)
	}
	if meta.MaxInputTokens != 200000 {
		t.Errorf("missing max_input_tokens should fall back to 200000 (client 2e5), got %d", meta.MaxInputTokens)
	}
}
