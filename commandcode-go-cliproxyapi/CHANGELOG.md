# Changelog

## 0.4.5

修复**两个让 go-cli 通道事实上不可用**的缺陷：请求时好时坏的 400，以及流式响应永远收不了尾。

### 缺陷 1：`threadId` 不是合法 UUID（~75% 请求被拒）

`NormalizeThreadID` 从 session 摘要派生线程 id 时，第 4 段直接取了 `hexs[16:20]`，
**没有设置 RFC 4122 的 variant 位**。上游对 `threadId` 做严格 UUID 校验，第 4 段
必须以 `8/9/a/b` 开头 —— 也就是只有 4/16 的取值合法。

**后果**：约 **3/4 的请求**被上游以
`400 Invalid UUID at "threadId"` 拒绝，同一个请求**是否成功取决于哈希撞上哪一位**。
实测 2000 个 session 中 1498 个（75%）生成非法 id。

**修复**：按 UUIDv5 正确构造 —— 版本位 `0x50`、variant 位 `| 0x80`。

### 缺陷 2：流式帧解析只认 `data:` 前缀，上游发的是裸 NDJSON

`cliSSEData` 要求每行以 `data:` 开头，否则整行丢弃。而 `/alpha/generate` 实际发送的是
**NDJSON**（`{"type":"start"}` 直接一行，无前缀）。

**后果**：**每一行都被丢弃**，包括终止事件 `finish` —— 于是即便上游成功返回，
插件也会报 `500 upstream CLI stream ended without a finish event`。

**修复**：同时接受裸 JSON 行与 `data:` 前缀行（后者保留以兼容未来切换）。

### 验证（真机，非仅单测）

- 在 Docker 里起**本地 CPA + 0.4.5 插件**，把 `base-url` 指向一个记录代理，
  **抓取插件发出的真实请求与上游完整响应**（前几轮定位不到，就是因为手工探针
  用了真 UUID，而真实链路的 id 是哈希派生的）；
- 修复前：上游 `400 Invalid UUID at "threadId"`；
- 修复后：**HTTP 200 + 完整流式响应**，`threadId` 形如
  `fe1475f6-fba6-5da7-9d58-...`（variant 合法）；
- 新增 6 个测试 + **变异验证**：还原缺陷后
  `TestNormalizeThreadIDIsAStrictUUID` 报 "1498 of 2000 ... not strict UUIDs"，
  精确复现线上比例。

## 0.4.4

修复**工具调用必 400** 的缺陷：`Invalid option: expected one of "user"|"assistant"`。

### 根因

`/alpha/generate` 的 `tool-result` 内容块要求 **`toolName` 是必填 string**，
但 `convertMessages` 的 `case "tool"` 只写了 `type` / `toolCallId` / `output`。

- 缺 `toolName` → 上游整请求 400（报文里表现为 messages 的 role/content
  校验失败，很容易误判成 role 问题）；
- 而 `toolName` 只在**更早的 assistant 消息**里（OpenAI 的 tool 消息只带
  `tool_call_id`），单次正序遍历取不到 → 需要预扫描建立
  `toolCallId → toolName` 映射。

**症状之所以诡异**：普通对话正常，**只要用工具就 400**——因为只有工具调用
才会产生 tool 消息。Claude Code、Codex 这类重度用工具的客户端因此必炸。

### 修复

- `convertMessages` 增加预扫描，从所有 assistant 的 `tool_calls` 建立
  `toolCallId → toolName` 映射；
- `tool-result` 补上 `toolName`，解析顺序为 映射 → 消息自带 `name` → `""`，
  **保证始终是 string**（缺字段/`null` 就是上游拒绝的原因）；
- 参考实现 `commandcode-proxy` 同样有 `toolNameMap` 回溯，本次与其对齐。

### 验证（真机端到端，非仅单测）

- **用修复后的 Go 代码产生的真实信封**直接打上游 `/alpha/generate`
  （含 system + 工具调用 + tool-result）→ **HTTP 200，49 个事件正常收尾**；
- 同一信封**去掉 `toolName`** → 复现老板的 400，报文一致；
- 3 个新单测 + 变异验证：移除 `toolName` 后三个测试精确失败。

## 0.4.3

修复**所有真实请求都无法调用**的致命缺陷：`auth_not_found: no auth available`。

### 根因（0.2.0 的连带误删）

0.2.0 为了去掉宿主上那个永远失败的 OAuth 登录入口，删除了 `auth_provider`
capability —— 这一步**是对的**。但同一次改动把 `materializeAuthRecords()`
（遍历账号、为每个凭据写一条 CPA auth 记录）一并删掉了，而它**与 OAuth 无关**：

- **host 靠自己的 auth 表挑选执行器**（`pickNextMixed` 遍历 `m.auths` 按
  provider 匹配；`model_router` 用 `HasProviderAuth(provider)` 判断可用性）；
- 插件不写 auth 记录 → 宿主认为 `commandcode` 没有任何可用凭据 →
  请求在**到达插件执行器之前**就被拒，返回
  `auth_not_found: no auth available`；
- 模型列表和配额页照常工作（走 `model_provider` 能力，另一条路），
  所以故障看起来像路由问题，实际是**少了一次注册**。

更糟的是，0.2.0 把测试改成了**断言这个错误行为**
（`TestLifecycleDoesNotWriteCPAAuthFiles`），所以门禁一直全绿。该提交自述也写着
"Real Go-plan credential end-to-end is NOT verified" —— 这个缺陷从未被端到端发现。

### 修复

- 恢复 auth 记录物化，并适配新的 `accounts` 池（不只是旧的 `api-keys`）：
  `register` / `reconfigure` 时为每个有效凭据生成
  `{type: commandcode, id: commandcode-key-<sha256前12>, api_key}` 并经
  `host.auth.save` 写入。凭据仍以插件池为权威，auth 记录只让宿主能调度它。
- **不恢复** `auth_provider` capability：写记录与开放 OAuth 登录是两件事，
  只有后者不需要（新增测试钉住 capability 里没有 `auth_provider`）。
- 幂等：按凭据摘要派生 id/文件名，宿主已存在则跳过，重复 register 不产生重复记录；
  已存在的记录不覆盖，保留宿主写入的元数据。
- 身份不泄露密钥：id/文件名/标签只用摘要，标签用 `label (摘要前12)`。
- **auth 存储不可用时注册必须失败**（此前返回成功，结果是"配好了但用不了"）。

### 验证

- 变异验证：把 auth 物化禁用回 bug 状态，两个新测试**精确失败**
  （`auth saves = 0, want 2`、`register reported success although no credential could be published`）；
- 全量 12 包 0 失败，build/vet/gofmt 干净。

## 0.4.2

模型列表改为**上游实时来源 + 套餐过滤**，并修复 go-cli 池被锁在实时发现之外的核心缺陷。

### 根因（两个都实测确认）

1. **误判纠正**：此前认为 `/models` 拒绝 Go 套餐——实测 Go key 访问
   `provider/v1/models` 返回 **HTTP 200**。真正的问题在插件：catalog 凭据只从
   provider 模式账号取，**纯 go-cli 池从未发起过实时拉取**，只能靠手填 static 或
   第三方文件。
2. **第三方文件过期**：参考反代的模型表（26 个）最后更新 2026-10-01，而实时接口
   已有 **87 个**（含 claude-sonnet-5-5、gpt-6.1-sol、Kimi-K3、GLM-5.3、Qwen3.8-Max
   等新模型）。

### 变更

- **go-cli 账号现在也会拉实时 catalog**：凭据选择改为「provider 优先，其次任意
  可用账号」，Go 池自动获得 87 个最新模型。
- **新增套餐过滤（`internal/planfilter`）**：实时接口不按套餐过滤（`?plan=go`
  被忽略，仍返回 87），插件本地按套餐表过滤，**只发布当前套餐能用的模型**。
  - 依据 `@mars-sea/dsh-commandcode-provider@0.12.4` 的官方判定逻辑
    （`modelVisibleInPlan` / `modelVisibleForAnyAccount`），85 条套餐表逐字镜像；
  - **多账号/多套餐取并集**（任一账号可用即发布）；
  - **fail-open**：未知套餐、未知模型、有按量额度时一律保留——宁可多显示一个
    （服务器 403 兜底），绝不隐藏一个能跑的；
  - 过滤在 catalog 快照构建时应用，隐藏的模型进入 unsupported 诊断
    （reason: `not included in the account's plan`），列表、查找、执行口径一致。
- **移除 `catalog.remote`**：第三方文件方案整体删除（配置字段、拉取器、解析器、
  WebUI 字段），实时接口才是权威来源。
- 实测校准：Go 套餐在 87 个实时模型中可见 **55 个**（52 个 Go 档 + 3 个表外
  fail-open），golden 测试逐条钉死。

### 兼容性

- `catalog.static` 仍是兜底（实时失败时使用）；`retry`/`watchdog`/`max-inflight`
  不变；配置文件里遗留的 `catalog-remote` 键会被忽略（无害）。

## 0.4.1

修复 0.4.0 的一个**发布即带病**的缺陷：`catalog.remote` 在 WebUI 里填了也不生效。

- **根因**：管理面板把每个 ConfigField 存成**顶层键**（`Name is the configuration key
  under plugins.configs.<pluginID>`，见宿主 `sdk/pluginapi/types.go`），所以实际落盘是
  扁平的 `catalog-remote:`。而解析只认**嵌套**写法 `catalog: { remote: }`，键名对不上，
  URL 被静默丢弃 → 不拉取 → 退回 static → static 也空 → **模型列表全空，且无任何报错**。
- **修复**：`catalog-remote` 现在两种写法都认（扁平 / 嵌套），**扁平优先**（它才是面板
  真正写入的那个，操作者在 UI 上的修改必须能覆盖文件里的旧值）。
- `retry` / `watchdog` / `max-inflight` 三个字段本就与面板键名一致（顶层），**不受影响**；
  本轮同时加了测试钉住"面板键名 == 接受的 YAML 键"这一契约。

**为什么 0.4.0 没测出来**：0.4.0 的集成测试用的是嵌套写法，与面板真实写法不一致，
于是测试绿而面板路径坏。修复后集成测试改用手面板真实写法，并已验证：把修复还原成
错误代码时，两个测试都会精确失败（`flat catalog-remote was dropped`）。

## 0.4.0

补齐参考反代的**三项传输层能力**，并新增**模型列表远程同步**。

### 新增：传输层三项

- **上游闪断透明重试（`retry`）**：上游在高峰期会中途掐断连接（`connection reset`
  / `terminated` / `socket hang up` 等）。当闪断发生在**向下游吐出任何字节之前**时，
  客户端从未看到这次请求开始，因此重试对调用方不可见。默认重试 2 次（共 3 次尝试），
  退避 `base × 尝试序号`（默认 400ms）。
  **只重试传输层闪断**：429/503 这类是上游有意传下来的语义信号，重试会吞掉它并
  在上游本就吃紧时放大负载，因此一律不重试。
  可配置 `retry.max`（0 关闭）、`retry.base-backoff`。
- **流式空闲看门狗（`watchdog`）**：只计**两次读取之间的间隔**，不是整个请求时长——
  厂商 CLI 对上游没有任何 idle 超时，合法的长思考停顿可达数分钟，用总时长上限会误杀
  健康请求。每收到一个 chunk 就重置计时；默认流式 30s、非流式 90s，触发时按可重试的
  空闲错误关闭下游，而不是悄悄挂住流（同时占着账号租约）。
  可配置 `watchdog.enabled`、`watchdog.stream`、`watchdog.non-stream`。
- **全局在途上限（`max-inflight`）**：超限返回 503（可重试），避免"裸跑反代"场景下
  并发请求各自缓冲整份响应把进程 OOM 掉。**默认 0=不限**：并发控制通常属于前面的
  反向代理（nginx `limit_conn`），那是唯一知道本机能吃多少的层。

### 新增：模型列表远程同步（`catalog.remote`）

**为什么需要**：Go 套餐无法访问 Provider API 的 `/models`，而实测 `/alpha/*` 上
**没有任何模型列表端点**（探测 18 个候选路由，全部 404），因此**无法从上游自动发现
模型**。参考反代自己的模型表也是源码里的字面量列表。

`catalog.remote` 指向一个会被解析出模型 id 的 URL（默认可用参考反代的 `proxy.mjs`），
让模型列表跟着那份会持续维护的表走，而不是手填。

- **这不是上游自动发现**，而是跟踪第三方文件，其格式可能变化；因此**默认关闭**，
  且 `catalog.static` 仍是它下面的兜底。
- 拉取失败时**保留上一次成功的列表**（CDN 抖动不会清空目录）；首次失败则退回 static。
- 支持 JS 字面量（`{ id: '...' }`）与 JSON（`"id": "..."`）两种形态。
- 非模型字符串（字段名、散文）不会被误发布；URL 中的凭据在日志前被剥离。

### 已知缺口

- 仍未在真实 CommandCode 凭据上做**完整推理**端到端实测；Go 套餐的配额端点与 go-cli
  通道已用真实 key 验证可用（0.3.0 之后）。
- `catalog.remote` 依赖的第三方文件格式若变更，解析会失败并回退 static（有日志提示），
  不会静默产出错误列表。

## 0.3.1

配额页面中文化。纯界面文案改动，**不改任何协议、路由或数据逻辑**。

### 变更

- 配额页全部 UI 文案改为简体中文：标题、行标签（5 小时窗口 / 每周窗口 / 月度额度）、
  三块余额（剩余套餐额度 / 充值额度 / 赠送额度）、状态词（正常 / 已超限 / 不限量）、
  重置与剩余天数、预警徽章（余额偏低 / 本期末取消 / 已被拦截：xx 窗口）、
  详情项（请求数 / 成功率 / 输入 Token / 输出 Token / 已消耗额度 / 单次均值）、
  按钮（详情 / 刷新）与空状态文案。
- `<html lang>` 改为 `zh-CN`。
- 移除了 `.quota-label` 的 `text-transform: capitalize`（英文首字母大写规则对中文无意义）。

### 说明

- 功能与结构完全不变：颜色分级（50/75/90%）、三块余额、预警徽章、详情展开、
  会话缓存与刷新行为均与 0.3.0 一致。
- 已知未翻译项：`pendingPhase` 的取值（如 `cancel`）原样透出——它是上游返回的枚举，
  翻译后反而会掩盖真实状态。
- 中文文案已用 jsdom 对三个场景（有数据 / 耗尽与未知套餐 / 无凭据）做真实 DOM 渲染
  断言，并检查无残留英文、无 undefined/NaN。

## 0.3.0

对齐参考反代（MAXeaglet/commandcode-proxy）的设备身份层，并补齐 CLI 面的错误语义与
配额面板的明细。**设备身份层默认开启，可在配置里整体关闭。**

### 新增

- **设备身份层（`device`）**：为每个 API Key 确定性地派生一台稳定的「伪设备」，
  并在首次 go-cli 生成前向上游登记：
  `POST /alpha/fingerprint/record`（指纹）与 `POST /alpha/lifecycle-events`
  （生命周期）。派生算法与参考反代**逐字节一致**，并用从参考实现捕获的黄金向量
  回归锁定（字段名 `cpu`/`mem`、CPU 池标签 `model|cores` 都属于派生结果的一部分，
  改错会静默把每个凭据换到另一台机器）。
- go-cli 请求头补齐：`User-Agent: cli`、`x-command-code-version`、
  `x-cli-environment`、`x-project-slug`、`x-taste-learning`、`traceparent`。
- `device` 配置块：`enabled`（默认 true）、`project-dir`、`identity-salt`，
  并在 WebUI 暴露。
- 信封 `config.environment` 改为与设备档案同源的 `win32`（原为 `linux-x64`）：
  「指纹说 Windows、信封说 Linux」正是共用设备档案要消除的自相矛盾。
- 配额面板：三块余额（套餐/充值/免费）、预警徽章（被拦窗口、低余额、
  到期取消、套餐变更中）、详情展开区（账期请求数、成功率、Tokens 进出、消耗、
  单均成本，并标注口径）、50/75/90% 颜色分级。
- 配额数据层接入 `/alpha/usage/summary`（账期聚合），兼容 `{data:{...}}` 与外层
  直出两种形态；端口失败不影响余额显示。

### 修复

- **`device.enabled=false` 是单一开关**：关闭后 `x-project-slug`、指纹与生命周期
  上报**全部停止**，而不是只停掉一半（半套身份比没有更容易被识别）。
- **CLI 状态码映射生效**：`402 -> 429`、`403 -> 401`、`422 -> 400`、`500 -> 502`、
  `503 -> 503`。此前 402（额度墙）会原样透给客户端，SDK 既不退避也不重试。
- **零输出守卫接入非流式路径**：CLI 流正常收尾但没有任何可见输出时，
  按 429 报错而不是把空回答当成功交给客户端。
- `identity-salt` 现在真正参与派生（此前只是解析、无效果）。
- 移除从未被读取的 `project-slug-overrides`（误导性配置）。

### 说明

- `go build ./... && go vet ./... && go test ./...` 全绿；`-race`（CGO 开启）
  覆盖 `internal/plugin`、`internal/adapter/gocli`、`internal/config`。
- 设备身份层的派生结果已与参考实现做**跨实现逐字节比对**（4/4 用例一致）。
- **尚未实现**：参考反代的传输层闪断透明重试、流式空闲看门狗、全局在途上限。
  这些是已识别的缺口，不是已完成项。
- 仍未在真实 CommandCode 凭据上做端到端实测（本机无该凭据），标记 **NOT_VERIFIED**。

## 0.2.0

### 变更

- **不再声明 `AuthProvider`**：CommandCode 用 API Key 认证，没有 OAuth/设备码流程。
  此前该能力会带来一个必然失败的登录按钮（`failed to generate authorization url`）。
  现在直接去掉，在**未打宿主补丁**的官方 CLIProxyAPI 上登录入口即消失；`ExecutorModelScope`
  同时改为 `Static`（模型由插件自己拥有，不绑定宿主 auth 记录），不再写任何 auth 文件。
- **账号池（`accounts` / `pool`）**：扁平 `api-keys` 升级为账号池。每个账号显式声明
  `mode`（`provider` 或 `go-cli`）、可选 `label`、`credential`（支持 `${ENV_VAR}`）与
  `disabled`。`pool` 控制选择策略（`sticky` / `round-robin`）与每账号并发上限。
  旧 `api-keys` 仍然兼容，会折叠为 provider 账号。
- **`go-cli` 传输**：Go 套餐是唯一被 Provider API 以 `upgrade_required` 拒绝的套餐，只能走
  厂商 CLI 的 `/alpha/generate` 信封。新增 `internal/adapter/gocli`：信封构造、SSE 事件解析、
  tool-call / reasoning 保真，并复用既有 chat-completions 内核产出 openai / claude /
  openai-response 三种客户端格式。`mode: go-cli` 的账号不再发往 Provider API。
- **`catalog.static` 静态模型表**：`/models` 属于 Provider API，会拒绝 Go 套餐 key，因此
  **纯 go-cli 池拿不到实时目录**。新增静态模型 id 列表配置；池中没有 provider 账号时以它发布
  模型，使 Go 单账号部署可用。
- **WebUI 新增 `accounts`、`pool` 配置字段**，可在插件管理页直接配置账号池。

### 修复

- **CLI 流被截断时不再伪装成功**：上游缺 finish 事件（掉线）时，现在明确报错关闭下游
  （`upstream stream ended without a finish event`），而不是把半截回答当成完整回答。
  **非流式**路径同样处理：新增 `gocli.ConvertNonStreamChecked`，`handleExecuteGoCLI`
  在截断时返回分类错误，不再把半截响应当成正常 completion。
- **账号池生命周期**：流式请求在整个流生命周期持有账号（`max-concurrency-per-account` 真正生效），
  且每次成功获取的账号**恰好释放一次**；所有失败出口（会话解析 / 信封构造 / URL 构造 / 传输错误）
  都会释放，不再泄漏 in-flight 计数。
  归属也修正：一次请求把它取号的那个池记在 `resolvedExecution.pool` 上，释放回**同一个池**——
  此前用 `m.pool`，reconfigure 换池后会把旧请求归还到新池，导致旧池 in-flight 永远不归零、
  新池被无关扣减。
- **`pool.strategy=sticky` 实现与注释相反**：原实现按 `lastUsed` **最早**挑选，
  等于在每个请求间轮换账号，与"同一账号持续服务"的声明矛盾。现改为优先**最近用过**的健康账号，
  仅当它进入 cooldown 或达到并发上限时才切换；注释同步说明这是**进程级**粘性（`acquire` 无会话句柄）。
- **`accounts` 声明类型与形态不符**：由 `Object` 改为 `Array`。宿主把提交的 JSON 原样落成 YAML 节点
  （`yamlNodeFromJSONValue` 的 `[]any` → `SequenceNode`），`field.Type` 只是前端渲染提示、不做强制转换；
  而 `accounts` 的示例一直是 JSON 数组，与 `api-keys` 同类。错声明为 `Object` 会诱导出插件无法解码的
  对象形态，正是 0.1.4「保存了却不生效」那类事故。
- **`catalog.static` 回退行为补全**：此前只在**池中没有 provider 账号**时启用，
  与文档「拿不到实时 `/models` 时启用」不符。现在实时拉取失败时也回退，且顺序为
  **先保留可用旧快照（`stale-while-unavailable`），再用静态表**——避免用较粗的静态表覆盖好数据。

### 说明

- `go build ./... && go vet ./... && go test ./...` 全绿，`go test -race ./internal/plugin/` 亦通过。
  新增测试覆盖：`go-cli` 账号路由到 `/alpha/generate` 且带 `Authorization: Bearer <cred>` 与
  `x-command-code-version`、`provider` 账号仍走 `/v1/chat/completions`；CLI SSE 解析（多事件、
  跨帧切分、error 事件、非流式聚合与截断）；sticky/round-robin 选择；账号释放与换池归属；
  `accounts` 字段类型；静态 catalog 回退矩阵（allow/deny、prefix、协议开关、去重、空列表、
  失败重配置后恢复）。
- 尚未在真实 CommandCode Go 套餐凭据上做端到端实测（本机无该凭据），标记为 **NOT_VERIFIED**。

## 0.1.4

### 修复

- **WebUI 里保存 API Key 会导致插件失效（严重）**：管理面板把 `api-keys` 渲染成
  通用 JSON 数组，用户按界面提示填进去的是**裸字符串**：

  ```yaml
  api-keys:
    - cc_xxx          # ← 面板实际写入的形态
  ```

  而插件此前只认对象形态 `- value: cc_xxx`，于是保存时 `plugin.reconfigure`
  报 `decode config: invalid YAML structure near line 4`，宿主随即把插件撤销注册，
  面板显示 **未注册 / 未生效**——用户完全按 UI 操作，插件却挂了。
  现在 `api-keys` **两种形态都接受**：裸字符串、`{value: ...}`（并兼容 `{key: ...}`）；
  `${ENV_VAR}` 展开对两种形态都生效。

  严格校验保持不变：空值拒绝、重复值拒绝、非字符串/非对象条目拒绝。

### 说明

- 已在**未修改的官方 CLIProxyAPI v8.0.13** 上实测：初始无 key 时注册成功；
  通过管理 API 保存裸字符串 key 后仍为 `registered=true / effective_enabled=true`；
  `/v1/models` 返回 `commandcode/*`；非流式与流式调用正常；切回对象形态同样正常。
- **`interactive_login` 需要宿主支持**：原版 CPA 会忽略该字段，因此仅升级插件
  **不能**隐藏 OAuth 登录入口。彻底解决需要 0.2.0 架构调整（不再声明 `AuthProvider`），
  见下个版本。

## 0.1.3
## 0.1.3

### 修复

- **插件被错挂到 OAuth 登录页**：注册时声明的新能力字段 `interactive_login`
  现在为 `false`。CommandCode 用 API Key 认证，没有 OAuth/设备码流程，
  因此登录页上那个按钮点下去必然报 `failed to generate authorization url`。
  插件仍保留 `auth_provider`（宿主靠它解析 auth 记录、把 `api_key` 交给
  executor），只是不再被当作可交互登录的 provider。
  > **此修复需要宿主支持 `interactive_login` 字段。** 旧宿主会忽略该字段，
  > OAuth 入口仍会显示。宿主补丁见仓库 `scripts/host-interactive-login.patch`。

### 新增

- **`models.allow` / `models.deny` 模型过滤**：可只发布指定模型、屏蔽指定模型。
  语义：`allow` 为空=全部放行；`allow` 非空=仅放行列出项；`deny` 永远优先。
  条目可写上游 id（`deepseek/deepseek-v4.1-flash`）或带前缀的公开 id
  （`commandcode/deepseek/deepseek-v4.1-flash`）。过滤在 catalog 快照构建时执行，
  被排除的模型同时从 `/v1/models`、`model.static`、`model.for_auth`、
  宿主模型注册表和 executor 查找中消失，并给出 `excluded by models.allow/deny` 诊断。
- **WebUI 新增 `models` 字段**（object/JSON），可直接在插件管理页配置。

## 0.1.2
## 0.1.2

### 修复

- **商店安装后无法配置的死循环**：此前 `api-keys` 为空时 `plugin.register`
  直接失败（`api-keys: at least one key is required`），宿主因此显示
  **未注册 / 未生效**；而该插件又没有声明任何可视化配置字段，面板里也没有
  输入 API Key 的地方，导致从插件商店安装后**永远无法配置**。
  现在注册路径改用 `config.LoadForRegistration`：空 key 列表以
  **待配置（pending）** 状态正常注册，不访问上游、不拉模型、不 panic，
  仅在尝试调用时返回明确错误
  `commandcode plugin is registered but not configured: set api-keys in the plugin configuration`。
  严格校验（`config.Load`）保持不变，已配置的实例行为完全不变。

### 新增

- **完整 WebUI 配置表单**：在注册元数据中声明 10 个配置字段
  （`api-keys`、`base-url`、`catalog-url`、`model-prefix`、`catalog`、
  `protocols`、`route-overrides`、`request-timeout`、`max-response-bytes`、
  `allow-http`），现在全部可以在插件管理页直接配置，无需再手改
  `plugins.configs.<id>`。
  嵌套选项（`model-prefix` / `catalog` / `protocols` / `route-overrides`）
  以 JSON 对象字段暴露——不写成 `model-prefix.enabled` 这类点号键，
  因为宿主对插件配置做浅合并，点号键会被写成字面量键从而生成插件读不到的 YAML。

### 说明

- **`api-keys` 仍是必填项**，只是缺省不再阻断注册。
- 已验证：真实 CLIProxyAPI v8.0.13 宿主上，商店安装形态（版本化文件名 +
  仅 `enabled`）注册成功、面板显示 10 个配置字段；通过管理 API 保存
  `api-keys` 后宿主自动 reconfigure、模型注册、auth 记录生成，
  `/v1/models` 出现 `commandcode/...`，非流式与流式调用均正常。

## 0.1.1

### 变更

- **打包进入本插件集合**：以独立 module 形式引入上游
  [`mczhoucn/commandcode-go-cliproxyapi`](https://github.com/mczhoucn/commandcode-go-cliproxyapi)
  v0.1.1（MIT），面向 CommandCode Go/GOAT/Pro/Max 套餐，提供 provider
  `commandcode`（模型 id 带 `commandcode/` 前缀）、多协议翻译、推理内容保真、
  Key 池调度与配额页面。

## 0.1.1

### 变更

- **打包进入本插件集合**：以独立 module 形式引入上游
  [`mczhoucn/commandcode-go-cliproxyapi`](https://github.com/mczhoucn/commandcode-go-cliproxyapi)
  v0.1.1（MIT），面向 CommandCode Go/GOAT/Pro/Max 套餐，提供 provider
  `commandcode`（模型 id 带 `commandcode/` 前缀）、多协议翻译、推理内容保真、
  Key 池调度与配额页面。
- **仅做机械适配**：module 路径改为
  `github.com/hex-ci/cpa-plugin/commandcode-go-cliproxyapi`；注册元数据 `Name`
  改为裸插件 id（与其 `.so` 文件名、`plugins.configs.<id>` 配置键、管理路由一致）；
  `GitHubRepository` 指向本仓库。协议、路由、推理、调度与配额逻辑未改动。
- **补齐打包文件**：新增 `VERSION`、`Makefile`、`.gitignore`、`CHANGELOG.md`、
  `docs/PROVENANCE.md`，并加入 `registry.json`（`direct` / schema_version 2）。

### 说明

- **`api-keys` 为必填**。缺失时宿主日志报
  `plugin.register failed: api-keys: at least one key is required`，管理面板显示
  **未注册 / 未生效**——这是配置错误，不是加载或版本错误。
- 已在 **CLIProxyAPI v8.0.13** 实测加载、注册、发布模型与注册配额页菜单。

上游 v0.1.1 的功能说明见 [RELEASE_NOTES.md](RELEASE_NOTES.md)。
