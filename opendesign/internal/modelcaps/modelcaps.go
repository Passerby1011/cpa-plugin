// Package modelcaps is this plugin's knowledge of Open Design's models: the
// seed catalog served before (or instead of) a live one, the API each model is
// declared on, and the measured reasoning levels.
//
// Everything here is transcribed from magpie's `@magpie-community/opendesign-auth`
// (models.mjs / rates.mjs / index.mjs), which is the only place those numbers
// were measured against amr-link. Nothing is invented: a field the source does
// not carry stays zero and the live catalog fills it.
//
// The vendor's own words matter in two places:
//
//	cost        "low"/"medium"/"high"/"very_high" — the plan's deduction
//	            multiplier fallback for a model rates.mjs does not name
//	capability  "advanced"/"standard" — whether the model thinks
//
// and they disagree with the measured tables often enough that neither is
// authoritative on its own (see rates.mjs's header).
package modelcaps

// API is the wire a model is declared on. amr-link serves every model on all
// three (magpie plugins#888), but a model thinks on its own one, so this is the
// default a request is shaped for.
type API string

const (
	// APIChatCompletions is /v1/chat/completions — the default for every model
	// this table does not name.
	APIChatCompletions API = "chat-completions"
	// APIResponses is /v1/responses.
	APIResponses API = "responses"
	// APIMessages is /v1/messages, Anthropic's own schema.
	APIMessages API = "messages"
)

// APIFor returns the wire one upstream model id is declared on. Everything the
// table below does not name is asked on chat completions, where ten of the
// eleven measured models thought (plugins#141).
func APIFor(upstreamID string) API {
	if api, ok := declaredAPI[upstreamID]; ok {
		return api
	}
	return APIChatCompletions
}

// declaredAPI is the per-model wire. The two Responses entries think longer
// there, and glm-5.3-flash only thinks on Anthropic's messages among the two
// others.
var declaredAPI = map[string]API{
	"claude-haiku-5.5": APIResponses,
	"deepseek-v4-pro":  APIResponses,
	"glm-5.3-flash":    APIMessages,
}

// OutputCap is the most output amr-link actually takes where its catalog
// (metadata.context_budget) publishes more. Asking for the published number is
// a 400 that names no field, so nothing can lower it and retry — and a client
// that names no limit (Codex) is given the published one, which would fail
// every turn (magpie #827, measured 2026-11-09 on two accounts: glm-5.3-flash
// took 131072 and refused 132000; gemini-3.8-flash took 65536 and refused
// 65537). The cap is applied at serving time, never written into the seed, so
// a live catalog that says less is taken as it says.
var OutputCap = map[string]int64{
	"glm-5.3-flash":    131072,
	"gemini-3.8-flash": 65536,
}

// CapOutput lowers a model's output limit to what amr-link takes. limit <= 0
// means "nothing declared" and is returned unchanged; a cap the model already
// fits under is not applied. A limit above the cap is clamped.
func CapOutput(upstreamID string, limit int64) int64 {
	limitCap, ok := OutputCap[upstreamID]
	if !ok || limit <= 0 {
		return limit
	}
	if limit > limitCap {
		return limitCap
	}
	return limit
}

// SeedModel is one entry of the seed catalog, in the shape models.mjs carries.
type SeedModel struct {
	ID string
	// Name is what the model is called; the live catalog carries ids only.
	Name string
	// Context / Output are its token limits.
	Context int64
	Output  int64
	// Input lists the non-text modalities it takes. "text" is implicit.
	Input []string
	// Cost is the vendor's spending word (low/medium/high/very_high) and
	// Capability its quality word (advanced/standard).
	Cost       string
	Capability string
	// Efforts are the reasoning levels amr-link was seen to take AND think at,
	// measured rather than read (magpie #137/#141). An empty list means the
	// model takes the level and never thinks — GPT-6 Luna's case.
	Efforts []string
}

// Seed is the models an account is offered before anyone has signed in, or
// when the live catalog cannot be read. Transcribed from models.mjs verbatim;
// see TestSeedMatchesMagpieSeed, which pins every field.
//
// models.mjs carries twelve entries. rates.mjs names fourteen — the two extra
// (hy4-preview, minimax-m3.1-flash) are catalog-only models a plan may list,
// and neither appears in the seed, so a live catalog entry for them falls back
// to the generic 200K/32K rather than an invented limit.
var Seed = []SeedModel{
	{ID: "claude-haiku-5.5", Name: "Claude Haiku 5.5", Context: 1000000, Output: 132000, Input: []string{"image", "file"}, Cost: "low", Capability: "standard", Efforts: []string{"low", "high", "max"}},
	{ID: "deepseek-v4-flash", Name: "DeepSeek V4 Flash", Context: 1048000, Output: 232000, Cost: "medium", Capability: "standard", Efforts: []string{"low", "high", "max"}},
	{ID: "deepseek-v4-flash-vision-exp", Name: "DeepSeek V4 Flash Vision (exp)", Context: 1048000, Output: 232000, Input: []string{"image"}, Cost: "medium", Capability: "standard", Efforts: []string{"low", "high", "max"}},
	{ID: "deepseek-v4-pro", Name: "DeepSeek V4 Pro", Context: 1048000, Output: 232000, Cost: "high", Capability: "advanced", Efforts: []string{"low", "high", "max"}},
	{ID: "deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", Context: 1048000, Output: 232000, Input: []string{"image"}, Cost: "low", Capability: "advanced", Efforts: []string{"low", "high", "max"}},
	{ID: "deepseek-v4.1-flash-eco", Name: "DeepSeek V4.1 Flash Eco", Context: 1048000, Output: 232000, Input: []string{"image"}, Cost: "low", Capability: "advanced", Efforts: []string{"low", "high", "max"}},
	{ID: "glm-5.3-flash", Name: "GLM 5.3 Flash", Context: 1048000, Output: 232000, Input: []string{"image", "video"}, Cost: "low", Capability: "advanced", Efforts: []string{"low", "high", "max"}},
	{ID: "glm-5.3-flashx", Name: "GLM 5.3 FlashX", Context: 1000000, Output: 232000, Input: []string{"image", "file", "video"}, Cost: "medium", Capability: "advanced", Efforts: []string{"low", "high", "max"}},
	{ID: "gpt-6-luna", Name: "GPT-6 Luna", Context: 1050000, Output: 304000, Input: []string{"image", "file"}, Cost: "low", Capability: "standard", Efforts: []string{}},
	{ID: "kimi-k2.7-code", Name: "Kimi K2.7 Code", Context: 262000, Output: 262000, Input: []string{"image"}, Cost: "medium", Capability: "advanced", Efforts: []string{"low", "high", "max"}},
	{ID: "mimo-v2.6-flash", Name: "MiMo V2.6 Flash", Context: 1048000, Output: 232000, Input: []string{"image", "audio", "video"}, Cost: "low", Capability: "standard", Efforts: []string{"low", "high", "max"}},
	{ID: "mimo-v2.6-pro", Name: "MiMo V2.6 Pro", Context: 1048000, Output: 232000, Input: []string{"image", "audio", "video"}, Cost: "medium", Capability: "advanced", Efforts: []string{"low", "high", "max"}},
}

// SeedByID indexes Seed.
var SeedByID = func() map[string]SeedModel {
	out := make(map[string]SeedModel, len(Seed))
	for _, m := range Seed {
		out[m.ID] = m
	}
	return out
}()

// Kinds are the input and output modalities a catalog entry can name, in the
// order the panel shows them.
var Kinds = []string{"text", "image", "audio", "video", "pdf", "file"}

// Rates is the plan's own deduction multiplier per model — the vendor's
// "额度扣除倍率" table (rates.mjs). A rate is what one dollar of model spend
// takes out of the plan's allowance: 0.27 is cheaper than 1, 4 is four times
// dearer. The catalog's cost words disagree for most models, so this table
// wins and only a model it does not name falls back to its cost word.
var Rates = map[string]float64{
	"claude-haiku-5.5":             2,
	"deepseek-v4-flash":            1,
	"deepseek-v4-flash-vision-exp": 1,
	"deepseek-v4-pro":              1,
	"deepseek-v4.1-flash":          0.5,
	"deepseek-v4.1-flash-eco":      0.27,
	"glm-5.3-flash":                1,
	"glm-5.3-flashx":               2,
	"gpt-6-luna":                   4,
	"hy4-preview":                  1,
	"kimi-k2.7-code":               1,
	"mimo-v2.6-flash":              1,
	"mimo-v2.6-pro":                1,
	"minimax-m3.1-flash":           1,
}

// costRate is the fallback for a model Rates does not name.
var costRate = map[string]float64{
	"low":       0.5,
	"medium":    1,
	"high":      2,
	"very_high": 4,
}

// RateOf is a model's quota multiplier: the table's value when it names the
// model, else its catalog cost word, else zero ("no rate known").
func RateOf(upstreamID, costWord string) float64 {
	if r, ok := Rates[upstreamID]; ok {
		return r
	}
	return costRate[costWord]
}

// ThinksFromCapability maps the vendor's quality word onto the thinking flag
// for a model with no measured levels. "standard" models DID think when asked
// (plugins#137) — which is why the flag is not read from this word when levels
// exist — but a model with neither levels nor this word is assumed not to.
var ThinksFromCapability = map[string]bool{
	"advanced": true,
	"standard": false,
}
