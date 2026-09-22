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

## 四、已验证：插件加载级（本机 Docker 实测）

**验证环境**（未使用真实账号，不依赖任何外部凭据）：

| 项 | 值 |
|---|---|
| CPA | 源码编译 `v7.2.30`（与插件 `go.mod` 锁定版本严格一致） |
| ABI | 宿主 `sdk/pluginabi/types.go` 与插件均为 `ABIVersion = 1` |
| 方式 | Docker 容器内运行真实服务，`plugins.dir` 挂载 `.so` |
| 边界 | 仅监听 `127.0.0.1`，无 api-keys，不连真实上游 |

**实测结果**：

| 证据 | 结果 |
|---|---|
| 宿主日志 | `[host.go:232] pluginhost: plugin loaded` |
| 宿主日志 | `[server.go:596] management routes registered after secret key configuration` |
| `/v0/resource/plugins/workbuddy-global/panel` | **200**，返回插件自身 HTML（`<title>WorkBuddy 面板</title>`） |
| 对照组 `/v0/resource/plugins/does-not-exist/panel` | 404 |

### 4.1 宿主自报的插件状态（`GET /v0/management/plugins`）

```json
{"id":"workbuddy-global","path":"plugins/workbuddy-global.so",
 "configured":true,"registered":true,"enabled":true,
 "effective_enabled":true,"supports_oauth":true}
```

四个标志位同时为真，且 **17 个配置字段完整下发**：

| 字段 | 实测值 |
|---|---|
| `oauth_client_mode` | enum `['cli','workbuddy','workbuddy-ai']` |
| `scheduler_mode` | enum `['off','credits']` |
| `models` / `desensitize_terms` | array |
| 其余 13 项 | boolean / integer / string |

`oauth_client_mode` 的枚举值正确，直接验证了本轮修掉的第 4 个缺陷（改造前被
改名误伤成 `workbuddy-global`，会导致宿主侧配置校验失败）。

### 4.2 端点与鉴权矩阵

| 端点 | 结果 |
|---|---|
| `/v0/resource/plugins/workbuddy-global/panel` | **200** |
| `/v0/management/plugins/workbuddy-global/config` | **200** `{"enabled":true,"priority":1}` |
| `/v0/management/plugins` | **200** |
| 无 `Authorization` | **401** |
| 错误 Bearer key | **401** |

**这些证据证明了什么**：

1. `.so` 的 C ABI 被宿主接受（ABI 匹配，无版本冲突）
2. `Register` 被真实调用，插件被登记并置为 `effective_enabled`
3. 路由名 `workbuddy-global` 与 `registry.json` / CI tag / `Makefile` 的标识符派生链路一致
4. `panel.html` 经 `go:embed` 正确嵌入并被宿主取回
5. 配置契约（字段名/类型/枚举）与宿主解析一致
6. 管理鉴权边界正确（无密钥/错密钥均 401）

**没有证明什么**：OAuth 登录、模型目录拉取、计费路径、积分面板数据 —— 这些
需要真实国际版账号，仍列在下面的待实测清单里。

> **关于 management 端点 404 的原因（修正）**：早期一版记录把它归因于
> `remote-management.allow-remote: false` 且未配 api-keys，**这个归因不准确**。
> 读 `internal/api/server.go:366` 确认，管理路由是**懒注册**的：
>
> ```go
> hasManagementSecret := cfg.RemoteManagement.SecretKey != "" || envManagementSecret || s.localPassword != ""
> if hasManagementSecret { s.registerManagementRoutes() }
> ```
>
> 真正缺的是 `secret-key`；`allow-remote: false` 不阻止本地挂载。补上密钥后
> 管理端点即返回 200。插件本身只注册 `/v0/resource/plugins/` 与
> `/v0/management` 两类路由，`panel.go` 仅接受 `""` / `/` / `/panel` / `/panel.html`。

## 五、待实测清单（需真实国际版账号）

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

## 六、本轮修掉的真实缺陷

这些不是推断，是编译器和测试抓出来的：

| # | 缺陷 | 影响 |
|---|---|---|
| 1 | `providerName` 改名后 `isWorkbuddyAuthListName` 认不出旧 `workbuddy-*.json` | 老用户凭证全部失联 |
| 2 | CLI 模式 refresh 请求 UA 为空 | 请求形态异常 |
| 3 | desktop UA 硬编码旧值，platform 串被改名误伤 | 登录通道串错，可能触发风控 |
| 4 | `oauth_client_mode` 枚举值被改名误伤 | 配置校验与常量不一致，插件拒启 |

第 4 项尤其危险：`main.go` 的枚举常量与 `desensitize.go` 的校验值一旦不同步，
插件会在配置解析阶段直接失败。

## 七、已知取舍

- **`gofmt -l` 报告大量文件未格式化**：上游仓库即为此状态（原插件 109 个文件同样
  被报告），根因是 Windows CRLF 行尾。**不跑 `gofmt -w`**，否则会产生 100+ 文件的
  无意义行尾 diff，并让后续与上游同步变得困难。
- **CLI 模式不发送 Origin/Referer**：沿用上游行为（仅 desktop 模式镜像浏览器装饰头），
  已在测试中显式固化该契约并注明原因，避免后人误判为缺陷。
