# 国际版适配溯源与待实测清单

本插件把上游 `workbuddy` 插件的国际版（Global）能力独立出来，作为一个只服务
`www.workbuddy.ai` 的 provider。本文档回答一个问题：**每一项国际版相关的行为，
依据是什么？哪些已验证、哪些没有？**

> 阅读约定：本文件里"实测"指有生产环境可复现的观测来源；"参考"指姊妹实现
> 的记录（非本仓库复现）；"推断"指无外部依据、由协议形态推导。**本仓库尚未
> 用真实国际版账号跑通过任何一条链路**，见文末待实测清单。

## 一、依据来源

| 代号 | 来源 | 性质 |
|---|---|---|
| **U** | 上游 `hex-ci/cpa-plugin` 的 `workbuddy/` 插件 | 与本插件同协议、跑在同一上游；其常量视为事实 |
| **R** | `Sliverkiss/workbuddy2api`（姊妹实现，1.4k star） | 其注释记录了观测过程；本仓库未复现 |
| **D** | 腾讯官方站点与文档（`workbuddy.ai`、`codebuddy.ai/docs`） | 官方口径，但非接口契约 |
| **I** | 本仓库推断 | 无外部依据 |

## 二、逐项判定

### A 档 — 高置信（U 档，已实现 + 单测覆盖）

| 项 | 值 | 依据 | 验证方式 |
|---|---|---|---|
| Global chat/auth 基址 | `https://www.workbuddy.ai` | U | `region_test.go` |
| Global Origin/Referer | `https://www.workbuddy.ai` | U | `region_test.go` |
| Global Accept-Language | `en-US` | U | `egress_headers_test.go` |
| realm 判定键 | JWT `iss` 主、`domain` 兜底 | U | `region_test.go` |
| OAuth 登录通道 | `platform=workbuddy-ai` | U | `oauth_profile_test.go` |
| 生命周期 | 耗尽删除 auth（Global）/ 禁用（CN） | U | `lifecycle_test.go` |
| CN token 不得送 Global 网关 | APISIX 返回 401 | U | `oauth_refresh_route_test.go` |

这些是**直接搬上游常量**，不是猜测。上游插件对 CN 分支已在生产运行，说明常量
与协议形态一致。

### B 档 — 有参考依据（R 档，**用候选链/降级实现，不硬编码单一值**）

| 项 | 实现 | 参考依据 | 为何不硬编码 |
|---|---|---|---|
| 计费路径无 `/v2` 前缀 | 候选序列 `/billing/meter/*` → `/v2/billing/meter/*`，**仅在 404/405 时降级** | R 注释标记 `R9: 国际版无 /v2 前缀` | 若该结论在真实环境不成立，插件自动落到 `/v2`，不会因猜错而废 |
| Global UA 平台段 `WorkBuddy AI` | `userAgentFor()` 按 realm 出段 | R 注释：`intl 逆向证据`；并警告送错可触发 `403 code 11140` | 保留单一实现，但**不依赖它做鉴权**；若被风控拒绝，日志可见 |
| Global 模型目录 | `/v2/enterprises/personal/models` | R `modelsPath` | 与 `/v3/config`（双域通用）并发探测，单端点失败降级为另一端点 |
| 6004 英文限流重置时间 | 解析 `will reset at` | R `client_english_test.go` | 解析失败只是日志缺失，不影响请求本身 |

**B 档的共同处理原则**：实现它，但让它在失效时降级而不是崩溃。
理由——这些结论来源可靠但不是本仓库复现的，用候选链可以在猜错时保留可用性。

### C 档 — 推断（**未实现，留 TODO**）

| 项 | 状态 | 说明 |
|---|---|---|
| OAuth `client_id` / 设备授权参数 | 未实现 | 上游协议是无 PKCE、state 由服务端签发，本插件沿用该形态；若国际版要求 client_id，需实测补齐 |
| COSY 签名细节 | **本插件不涉及** | 上游 `workbuddy` 插件无此机制（`qoderwork`/`qwenwork` 才有） |
| 国际版是否复用同一 auth 端点路径 | 沿用上游路径 | `/v2/plugin/auth/*` 在两域同构属于 **U 档假设**，见待实测第 2 项 |

## 三、活动能力判定（已核实，非推测）

查证结论：**国际版有"每日活跃奖励"，但它不是国内版的"签到"。**

| 能力 | 国内版 | 国际版 | 依据 |
|---|---|---|---|
| 每日积分 | Buddy 加油站**签到**（手动打卡，100 分/天） | **每日活跃奖励**（对话即算，30 分/天） | D：`workbuddy.ai/docs/zh/workbuddy/Subscription` 明确"面向 …国际站个人版 Free 版和 Pro 版用户"；`workbuddy.ai/pricing` 列 `Daily active reward 30 credits/day (promo)` |
| 新用户/试用 | 5000 分 | 250 分 + 7 天 Pro（含 500 分） | D：`workbuddy.ai/pricing` |
| 成长计划 / 猫咪旅行 / 增长任务 | 有 | **未见于官方文档** | D（查无） |

**处置**：CN 专属活动（`checkin` / `travel` / `growth_tasks`）**保留代码但默认关闭**，
并按 region 跳过 Global 账号；国际版的"每日活跃"能力对应本插件已有的
`activity` 上报路径，同样默认关闭，**待拿到账号实测后再决定启用**。

> 为什么不直接删：官方文档只说明"有活跃奖励"，未给出接口形态。删掉就等于放弃
> 一个可能有用的能力；保留但默认关，实测后打开即可，且不影响主链路。

## 四、待实测清单（**本仓库未验证，逐项列出**）

以下每一项都**没有**用真实国际版账号验证过，交付状态是"代码就绪、待实测"。

| # | 待验证项 | 期望结果 | 若失败的处置 |
|---|---|---|---|
| 1 | OAuth 登录能拿到 Global token（`iss` 含 `workbuddy.ai`） | 拿到 token，`domain` 为 `workbuddy.ai` | 补齐 `client_id` 等登录参数（C 档） |
| 2 | `/v2/plugin/auth/state` 在两域同构 | 返回授权 URL | 若国际版路径不同，需按实际抓包调整 |
| 3 | 模型目录 `/v3/config` 在 Global 返回 | 返回模型列表 | 降级到 `/v2/enterprises/personal/models`（已实现） |
| 4 | 计费接口是否需要 `/v2` 前缀 | 记录实际命中的候选路径 | 候选链自动降级，日志会显示命中的是哪一个 |
| 5 | UA 平台段 `WorkBuddy AI` 是否被接受 | 不返回 `403`/`11140` | 改为与 CN 同段，或按实际抓包取值 |
| 6 | 积分面板能否拉到 Global 额度 | 面板显示余额 | 检查 `billingBaseFor` 命中的 base |
| 7 | Activity 上报是否被 Global 接受 | 默认关闭，实测后再开 | 保持关闭 |
| 8 | Trial 领取（250 分）接口形态 | 面板可领取 | 若接口不符，按实际调整 |

**验证方法**：拿到 Global 账号后，按 1→7 顺序跑；每步的判定标准是"上游返回的
HTTP 状态与响应体形态"，不是"插件没报错"。

## 五、本轮修掉的真实缺陷

这些不是推断，是编译器和测试抓出来的：

| # | 缺陷 | 影响 |
|---|---|---|
| 1 | `providerName` 改名后 `isWorkbuddyAuthListName` 认不出旧 `workbuddy-*.json` | 老用户凭证全部失联 |
| 2 | CLI 模式 refresh 请求 UA 为空 | 请求形态异常 |
| 3 | desktop UA 硬编码旧值，platform 串被改名误伤 | 登录通道串错，可能触发风控 |
| 4 | `oauth_client_mode` 枚举值被改名误伤 | 配置校验与常量不一致，插件拒启 |

第 4 项尤其危险：`main.go` 的枚举常量与 `desensitize.go` 的校验值一旦不同步，
插件会在配置解析阶段直接失败。

## 六、已知取舍

- **`gofmt -l` 报告大量文件未格式化**：上游仓库即为此状态（原插件 109 个文件同样
  被报告），根因是 Windows CRLF 行尾。**不跑 `gofmt -w`**，否则会产生 100+ 文件的
  无意义行尾 diff，并让后续与上游同步变得困难。
- **CLI 模式不发送 Origin/Referer**：沿用上游行为（仅 desktop 模式镜像浏览器装饰头），
  已在测试中显式固化该契约并注明原因，避免后人误判为缺陷。
