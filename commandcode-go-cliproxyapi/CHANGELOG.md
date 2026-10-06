# Changelog

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
