package planfilter

// Plan-tier gating for the published model list.
//
// WHY THIS EXISTS
//
// The live catalog (GET {base-url}/models) returns EVERY model the platform
// serves - 87 of them - with no plan metadata and no way to ask "what can THIS
// account run?" (`?plan=go` and friends are silently ignored; the /alpha/*
// plan routes all 404). The server remains the final gate
// (`403 MODEL_NOT_IN_PLAN`), but a list that advertises models the operator
// cannot actually call is worse than a shorter, honest one.
//
// So the plan->model mapping is kept here, as data, and mirrored from the
// reference implementation's verified snapshot. The ordering rule is
// monotonic: each plan's list is a superset of the one below it
// (Go subset GOAT subset Pro subset Provider/Max).
//
// THE TABLE CAN GO STALE. A model added upstream after this snapshot is not in
// the table, and unknown models are deliberately KEPT VISIBLE (see
// ModelVisibleInPlan), so a stale table degrades to "shows a bit too much" -
// never to "hides something you can run".

// PLAN_TIER_ORDER is the account-ranking scale, weakest first. A model is
// visible to an account when the model's own tier is at or below the
// account's.
var planTierOrder = []string{"go", "goat", "pro", "provider", "max"}

// planOrder maps a tier name to its rank on PLAN_TIER_ORDER.
var planOrder = func() map[string]int {
	out := make(map[string]int, len(planTierOrder))
	for i, tier := range planTierOrder {
		out[tier] = i
	}
	return out
}()

// knownPlans is the minimum subscription tier that includes each model id, per
// the vendor's official plan pages. Keys are UPSTREAM model ids (the
// `/models` `id`), never the plugin's prefixed public id.
//
// Mirror of @mars-sea/dsh-commandcode-provider@0.12.4 (2026-10-04),
// re-verified against its compiled lib/index.js. 85 entries: go=53, goat=9,
// pro=14, provider=8, max=1.
var knownPlans = map[string]string{
	// Model -> the LOWEST plan tier that can run it, synced from the reference
	// provider package's plan table (itself synced from the official command-code
	// CLI bundle). A model absent here is treated as visible (fail-open), so this
	// table must be refreshed when the vendor ships new models - a missing entry
	// advertises a model the plan cannot run, and the request fails upstream with
	// 403 MODEL_NOT_IN_PLAN after the client has already picked it.
	"MiniMaxAI/MiniMax-M2.5":                "go",
	"MiniMaxAI/MiniMax-M2.7":                "go",
	"MiniMaxAI/MiniMax-M3":                  "go",
	"Qwen/Qwen3.6-Max-Preview":              "go",
	"Qwen/Qwen3.6-Plus":                     "go",
	"Qwen/Qwen3.7-Flash":                    "go",
	"Qwen/Qwen3.7-Max":                      "go",
	"Qwen/Qwen3.7-Plus":                     "go",
	"Qwen/Qwen3.8-27B":                      "go",
	"Qwen/Qwen3.8-Flash":                    "go",
	"Qwen/Qwen3.8-Max":                      "go",
	"Qwen/Qwen3.8-Max-0902":                 "go",
	"Qwen/Qwen3.8-Omni-Flash":               "go",
	"claude-fable-5":                        "provider",
	"claude-fable-5-1":                      "provider",
	"claude-haiku-4-5-20251001":             "pro",
	"claude-haiku-5-5":                      "goat",
	"claude-opus-4-7":                       "provider",
	"claude-opus-4-8":                       "provider",
	"claude-opus-5":                         "provider",
	"claude-opus-5-5":                       "provider",
	"claude-sonnet-4-6":                     "pro",
	"claude-sonnet-5":                       "pro",
	"claude-sonnet-5-5":                     "goat",
	"deepseek/deepseek-v4-flash":            "go",
	"deepseek/deepseek-v4-flash-fast":       "go",
	"deepseek/deepseek-v4-flash-vision-exp": "go",
	"deepseek/deepseek-v4-pro":              "go",
	"deepseek/deepseek-v4.1-flash":          "go",
	"deepseek/deepseek-v4.1-flash-fast":     "go",
	"google/gemini-3.1-flash-lite":          "pro",
	"google/gemini-3.5-flash":               "pro",
	"google/gemini-3.5-flash-lite":          "pro",
	"google/gemini-3.6-flash":               "pro",
	"google/gemini-3.7-flash":               "goat",
	"google/gemini-3.8-flash":               "goat",
	"gpt-5.3-codex":                         "pro",
	"gpt-5.4":                               "pro",
	"gpt-5.4-mini":                          "pro",
	"gpt-5.5":                               "pro",
	"gpt-5.6-luna":                          "go",
	"gpt-5.6-sol":                           "goat",
	"gpt-5.6-terra":                         "pro",
	"gpt-6-astra":                           "provider",
	"gpt-6-luna":                            "go",
	"gpt-6-sol":                             "pro",
	"gpt-6.1-sol":                           "max",
	"inclusionai/ling-3.0-flash-sante:free": "go",
	"inclusionai/ling-3.1-flash:free":       "go",
	"meituan/LongCat-2.0":                   "go",
	"meta/muse-spark-1.1":                   "pro",
	"meta/muse-spark-1.2":                   "goat",
	"meta/muse-spark-1.2-contributor":       "go",
	"meta/muse-spark-1.3":                   "goat",
	"meta/muse-spark-1.3-contributor":       "go",
	"mistral/mistral-large-4":               "go",
	"moonshotai/Kimi-K2.5":                  "go",
	"moonshotai/Kimi-K2.6":                  "go",
	"moonshotai/Kimi-K2.7-Code":             "go",
	"moonshotai/Kimi-K2.7-Code-Highspeed":   "go",
	"moonshotai/Kimi-K3":                    "go",
	"nvidia/nemotron-3-ultra-550b-a55b":     "go",
	"poolside/laguna-s-2.1-free":            "go",
	"sakana/fugu-ultra":                     "provider",
	"stealth/glyph-cluster:free":            "go",
	"stepfun/Step-3.5-Flash":                "go",
	"stepfun/Step-3.7-Flash":                "go",
	"stepfun/Step-5-Preview":                "go",
	"tencent/hy3-paid":                      "go",
	"tencent/hy4-preview":                   "go",
	"thinkingmachines/inkling":              "go",
	"thinkingmachines/inkling-small":        "go",
	"xai/grok-4.5":                          "go",
	"xai/grok-4.6":                          "goat",
	"xai/grok-4.7":                          "goat",
	"xiaomi/mimo-v2.5":                      "go",
	"xiaomi/mimo-v2.5-pro":                  "go",
	"xiaomi/mimo-v2.6-flash":                "go",
	"xiaomi/mimo-v2.6-pro":                  "go",
	"xiaomi/mimo-v2.6-pro-ultraspeed":       "goat",
	"z-ai/glm-5.3-flash":                    "go",
	"z-ai/glm-5.3-flashx":                   "go",
	"zai-org/GLM-5":                         "go",
	"zai-org/GLM-5.1":                       "go",
	"zai-org/GLM-5.2":                       "go",
	"zai-org/GLM-5.2-Fast":                  "go",
	"zai-org/GLM-5.3":                       "go",
}

// SubscriptionPlan describes an account's plan identity and its rank.
type SubscriptionPlan struct {
	Name           string
	MonthlyCredits float64
	TierWeight     int
}

// subscriptionPlans maps a planId from /alpha/billing/subscriptions to its
// facts. Prefix-matched longest-first so "individual-goat" is never read as
// "individual-go".
var subscriptionPlans = map[string]SubscriptionPlan{
	"individual-go":       {Name: "Go", MonthlyCredits: 10, TierWeight: 0},
	"individual-go-v1":    {Name: "Go", MonthlyCredits: 10, TierWeight: 0},
	"individual-goat":     {Name: "GOAT", MonthlyCredits: 70, TierWeight: 1},
	"individual-pro":      {Name: "Pro", MonthlyCredits: 30, TierWeight: 2},
	"individual-pro-v1":   {Name: "Pro", MonthlyCredits: 80, TierWeight: 2},
	"individual-provider": {Name: "Provider", MonthlyCredits: 15, TierWeight: 3},
	"individual-max":      {Name: "Max", MonthlyCredits: 150, TierWeight: 4},
	"individual-ultra":    {Name: "Ultra", MonthlyCredits: 300, TierWeight: 4},
	"teams-pro":           {Name: "Teams Pro", MonthlyCredits: 40, TierWeight: 2},
}

// subscriptionPlanPrefixes holds subscriptionPlans' keys, longest first, for
// prefix matching.
var subscriptionPlanPrefixes = func() []string {
	out := make([]string, 0, len(subscriptionPlans))
	for k := range subscriptionPlans {
		out = append(out, k)
	}
	// longest first: "individual-goat" must win over "individual-go".
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if len(out[j]) > len(out[i]) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}()
