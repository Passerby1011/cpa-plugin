# 0.4.2 实现规格：模型列表改为上游实时 + 套餐过滤

## 背景（已实测确认）

插件 `commandcode-go-cliproxyapi` 的模型列表当前**不来自上游实时接口**，而是靠手填
`catalog.static` 或第三方文件 `catalog.remote`。实测发现：

1. **`https://api.commandcode.ai/provider/v1/models` 对 Go 套餐 key 返回 200 + 87 个模型**
   （此前"Go 套餐访问不了 /models"是**误判**）。
2. 插件 `internal/plugin/plugin.go` 的 `refreshOnce` 里，catalog 凭据**只从
   `TransportProvider` 账号取**；纯 go-cli 池 `credential == ""`，于是**实时接口从未被调用**，
   直接走了 static/remote 兜底分支。**这是真正要修的 bug。**
3. 该接口**不按套餐过滤**（`?plan=go` 等参数被忽略，仍返回 87）。
4. 上游**没有** plan→model 接口（探测 `/alpha/plans`、`/alpha/entitlements` 等 12 个候选全 404）。

## 目标

- **以上游实时接口为主来源**（87 个全量拉取），**移除第三方文件 `catalog.remote`**。
- 拉取后按**本地套餐表 + 账号订阅等级**过滤，**只发布当前套餐能用的模型**。
- **多账号/多套餐取并集**（任一账号可用即发布）。
- 过滤遵循"**不确定就保留**"（fail-open）：未知账号、未知套餐、表里没有的模型，一律保留。

## 参考实现（权威，本地已 clone）

`C:/Users/Administrator/AppData/Local/Temp/cpa-work/ref-dsh-commandcode-provider`
（`@mars-sea/dsh-commandcode-provider` v0.12.4，2026-10-04）
—— `src/capabilities.ts` 里的 `modelVisibleInPlan` / `modelVisibleForAnyAccount`，
其编译产物 `lib/index.js` 我已抽成 oracle，**逐条比对过**。

## 要实现的判定逻辑（必须与 oracle 等价）

```
PLAN_TIER_ORDER = [go, goat, pro, provider, max]
PLAN_ORDER = {go:0, goat:1, pro:2, provider:3, max:4}

func modelVisibleInPlan(modelID string, access) bool:
    if access is undefined:            return true   # 不确定 -> 保留
    if access.onDemandCredits > 0:     return true   # 有按量额度 -> 全开
    if access.tierWeight is undefined or not finite: return true
    tier = KNOWN_PLANS[modelID]
    if tier missing:                   return true   # 表外 -> 保留
    weight = PLAN_ORDER[tier]
    if weight undefined:               return true
    return weight <= access.tierWeight

func modelVisibleForAnyAccount(modelID, accounts[]):
    if accounts empty or undefined:    return true   # 空池 -> 全显示
    return any(modelVisibleInPlan(modelID, a) for a in accounts)
```

账号 `tierWeight` 来源：`/alpha/billing/subscriptions` 的 `planId` → 订阅表：

```
individual-go       -> Go,        tier 0
individual-go-v1    -> Go,        tier 0
individual-goat     -> GOAT,      tier 1
individual-pro      -> Pro,       tier 2
individual-pro-v1   -> Pro,       tier 2
individual-provider -> Provider,  tier 3
individual-max      -> Max,       tier 4
individual-ultra    -> Ultra,     tier 4
teams-pro           -> Teams Pro, tier 2
```

（插件 `internal/plugin/quota.go` 里**已有** `planAllowances`/`planNames` 表，
可复用同款"最长前缀匹配"写法；`planFor` 已有该实现。）

`onDemandCredits` = `/alpha/billing/credits` 的 `purchasedCredits + freeCredits`
（插件 `quotaUsage` 里已有这两个字段）。

## KNOWN_PLANS 全表（85 条，必须逐字照抄）

```go
	// --- go (53) ---
	"MiniMaxAI/MiniMax-M2.5": "go",
	"MiniMaxAI/MiniMax-M2.7": "go",
	"MiniMaxAI/MiniMax-M3": "go",
	"Qwen/Qwen3.6-Max-Preview": "go",
	"Qwen/Qwen3.6-Plus": "go",
	"Qwen/Qwen3.7-Flash": "go",
	"Qwen/Qwen3.7-Max": "go",
	"Qwen/Qwen3.7-Plus": "go",
	"Qwen/Qwen3.8-27B": "go",
	"Qwen/Qwen3.8-Flash": "go",
	"Qwen/Qwen3.8-Max": "go",
	"Qwen/Qwen3.8-Max-0902": "go",
	"Qwen/Qwen3.8-Omni-Flash": "go",
	"deepseek/deepseek-v4-flash": "go",
	"deepseek/deepseek-v4-flash-fast": "go",
	"deepseek/deepseek-v4-flash-vision-exp": "go",
	"deepseek/deepseek-v4-pro": "go",
	"deepseek/deepseek-v4.1-flash": "go",
	"deepseek/deepseek-v4.1-flash-fast": "go",
	"gpt-5.6-luna": "go",
	"gpt-6-luna": "go",
	"inclusionai/ling-3.0-flash-sante:free": "go",
	"inclusionai/ling-3.1-flash:free": "go",
	"meituan/LongCat-2.0": "go",
	"meta/muse-spark-1.2-contributor": "go",
	"meta/muse-spark-1.3-contributor": "go",
	"moonshotai/Kimi-K2.5": "go",
	"moonshotai/Kimi-K2.6": "go",
	"moonshotai/Kimi-K2.7-Code": "go",
	"moonshotai/Kimi-K2.7-Code-Highspeed": "go",
	"moonshotai/Kimi-K3": "go",
	"nvidia/nemotron-3-ultra-550b-a55b": "go",
	"poolside/laguna-s-2.1-free": "go",
	"stealth/space-bunny-alpha": "go",
	"stepfun/Step-3.5-Flash": "go",
	"stepfun/Step-3.7-Flash": "go",
	"stepfun/Step-5-Preview": "go",
	"tencent/hy3-paid": "go",
	"tencent/hy4-preview": "go",
	"thinkingmachines/inkling": "go",
	"thinkingmachines/inkling-small": "go",
	"xai/grok-4.5": "go",
	"xiaomi/mimo-v2.5": "go",
	"xiaomi/mimo-v2.5-pro": "go",
	"xiaomi/mimo-v2.6-flash": "go",
	"xiaomi/mimo-v2.6-pro": "go",
	"z-ai/glm-5.3-flash": "go",
	"z-ai/glm-5.3-flashx": "go",
	"zai-org/GLM-5": "go",
	"zai-org/GLM-5.1": "go",
	"zai-org/GLM-5.2": "go",
	"zai-org/GLM-5.2-Fast": "go",
	"zai-org/GLM-5.3": "go",
	// --- goat (9) ---
	"claude-sonnet-5-5": "goat",
	"google/gemini-3.7-flash": "goat",
	"google/gemini-3.8-flash": "goat",
	"gpt-5.6-sol": "goat",
	"meta/muse-spark-1.2": "goat",
	"meta/muse-spark-1.3": "goat",
	"xai/grok-4.6": "goat",
	"xai/grok-4.7": "goat",
	"xiaomi/mimo-v2.6-pro-ultraspeed": "goat",
	// --- pro (14) ---
	"claude-haiku-4-5-20251001": "pro",
	"claude-sonnet-4-6": "pro",
	"claude-sonnet-5": "pro",
	"google/gemini-3.1-flash-lite": "pro",
	"google/gemini-3.5-flash": "pro",
	"google/gemini-3.5-flash-lite": "pro",
	"google/gemini-3.6-flash": "pro",
	"gpt-5.3-codex": "pro",
	"gpt-5.4": "pro",
	"gpt-5.4-mini": "pro",
	"gpt-5.5": "pro",
	"gpt-5.6-terra": "pro",
	"gpt-6-sol": "pro",
	"meta/muse-spark-1.1": "pro",
	// --- provider (8) ---
	"claude-fable-5": "provider",
	"claude-fable-5-1": "provider",
	"claude-opus-4-7": "provider",
	"claude-opus-4-8": "provider",
	"claude-opus-5": "provider",
	"claude-opus-5-5": "provider",
	"gpt-6-astra": "provider",
	"sakana/fugu-ultra": "provider",
	// --- max (1) ---
	"gpt-6.1-sol": "max",
```

## 验收向量（我用参考实现编译产物跑出来的真值）

真实目录 87 条（`catalog87.json` 同目录）。对每个套餐，可见模型数：

| planId | name | tierWeight | 可见数 |
|---|---|---|---|
| individual-go | Go | 0 | **55** |
| individual-go-v1 | Go | 0 | **55** |
| individual-goat | GOAT | 1 | 64 |
| individual-pro | Pro | 2 | 78 |
| individual-pro-v1 | Pro | 2 | 78 |
| individual-provider | Provider | 3 | 86 |
| individual-max | Max | 4 | 87 |
| individual-ultra | Ultra | 4 | 87 |
| teams-pro | Teams Pro | 2 | 78 |
| unknown-plan-x | (无) | (无) | 87 |

边界（都必须 87 全显示）：
- `access == undefined` → 87
- `onDemandCredits > 0`（tier=go）→ 87
- 空池 / undefined 池 → 87
- 并集语义：Go ∪ Pro → **78**（等于单独 Pro，因 Pro ⊃ Go）

目录中**不在套餐表里**的 3 个模型（必须保留可见）：
`claude-haiku-5-5, stealth/glyph-cluster:free, mistral/mistral-large-4`

**`individual-go-v1` 的 55 个可见模型完整清单**（验收时逐条比对，文件 `go-visible-55.txt`）：

```
claude-haiku-5-5
gpt-6-luna
gpt-5.6-luna
deepseek/deepseek-v4-pro
deepseek/deepseek-v4-flash
deepseek/deepseek-v4-flash-vision-exp
deepseek/deepseek-v4-flash-fast
deepseek/deepseek-v4.1-flash
deepseek/deepseek-v4.1-flash-fast
moonshotai/Kimi-K3
moonshotai/Kimi-K2.7-Code
moonshotai/Kimi-K2.7-Code-Highspeed
moonshotai/Kimi-K2.6
moonshotai/Kimi-K2.5
z-ai/glm-5.3-flash
z-ai/glm-5.3-flashx
zai-org/GLM-5.3
zai-org/GLM-5.2
zai-org/GLM-5.2-Fast
zai-org/GLM-5.1
zai-org/GLM-5
MiniMaxAI/MiniMax-M3
MiniMaxAI/MiniMax-M2.7
MiniMaxAI/MiniMax-M2.5
xiaomi/mimo-v2.6-pro
xiaomi/mimo-v2.6-flash
xiaomi/mimo-v2.5-pro
xiaomi/mimo-v2.5
Qwen/Qwen3.8-Omni-Flash
Qwen/Qwen3.8-Max-0902
Qwen/Qwen3.8-Max
Qwen/Qwen3.8-27B
Qwen/Qwen3.8-Flash
Qwen/Qwen3.7-Max
Qwen/Qwen3.7-Plus
Qwen/Qwen3.7-Flash
Qwen/Qwen3.6-Max-Preview
Qwen/Qwen3.6-Plus
meituan/LongCat-2.0
stepfun/Step-5-Preview
stepfun/Step-3.7-Flash
stepfun/Step-3.5-Flash
tencent/hy3-paid
tencent/hy4-preview
nvidia/nemotron-3-ultra-550b-a55b
thinkingmachines/inkling
thinkingmachines/inkling-small
stealth/glyph-cluster:free
poolside/laguna-s-2.1-free
inclusionai/ling-3.0-flash-sante:free
inclusionai/ling-3.1-flash:free
meta/muse-spark-1.2-contributor
meta/muse-spark-1.3-contributor
xai/grok-4.5
mistral/mistral-large-4
```

## 关键实现约束

1. **过滤必须用 `ModelRecord.UpstreamID`**（原始 id，即表的键），
   不能用 `PublicID`（带 `commandcode/` 前缀）。
2. **移除 `catalog.remote`**：删掉配置字段、`fetchRemoteCatalog`、`parseRemoteModelIDs`、
   相关测试与 WebUI 字段。用户已明确要以上游接口为准。
3. **go-cli 账号必须能拉实时 catalog**：`refreshOnce` 里 `credential` 的取法要覆盖
   go-cli 账号（用其 `Credential` 作 Bearer）。若确实多个账号，取第一个可用的即可
   （catalog 是全局的，与哪个账号无关）。
4. **过滤失败要 fail-open 且可观测**：日志记录 "published N of M models (plan filter)"。
5. **不得破坏既有行为**：provider 账号路径、`catalog.static` 兜底、`stale-while-unavailable`
   都要保留。过滤只作用于"发布什么"，不改路由。
6. **表要标注来源与日期**，并提示"上游发布可能使表过期"。

## 必须写的测试

- `TestModelVisibleInPlanMatchesReference`：把上面验收向量（含 55 个清单）写成表驱动，
  用**固定 87 条目录**（内嵌测试数据）跑，断言每个套餐的可见集合与向量**完全一致**。
- `TestPlanFilterFailsOpen`：undefined access / onDemandCredits>0 / 空池 / 表外模型 → 全可见。
- `TestPlanFilterUsesUpstreamID`：带前缀时用 UpstreamID 匹配，不能因前缀漏过滤。
- `TestGoCliAccountFetchesLiveCatalog`：纯 go-cli 池必须真的去请求 `/models` 并发布模型
  （这是本次修复的核心回归——以前它不会发这个请求）。
- 回归：`catalog.remote` 移除后，所有相关旧测试删除/改写，全量测试绿。

## 环境

- 仓库：`D:/hermes-data/TOOL/cpa-plugin/commandcode-go-cliproxyapi`
- Go 不在 PATH，必须走容器门禁：`/d/hermes-data/cache/scratch/cpa-030/gate-bash.sh '<cmd>'`
- 文件是 **CRLF**；改动后用 `tr -d "\r"` 归一给 gofmt 检查。
- 跑测试：`gate-bash.sh 'go test ./...'`

## 不在范围内

- 不发布、不打 tag、不改 registry（我统一做发布）。
- 不动 `retry`/`watchdog`/`max-inflight`。
- 不改 quota 页面。
