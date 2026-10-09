# CommandCode Go/GOAT/Pro/Max CLIProxyAPI 插件

[English](README.md) | **简体中文**

一个 [CLIProxyAPI](https://help.router-for.me/plugin/development) 的原生动态 Go 插件，把 **CommandCode Go/GOAT/Pro/Max** 套餐作为单一 provider（`commandcode`；对外发布的模型 id 带 `commandcode/` 前缀）对外提供。

插件自己负责模型发现、协议翻译、请求执行、密钥调度和配额页面，因此一份 API key 池就能同时服务 OpenAI、Anthropic 和 Responses 三种客户端。

## 上游实际提供了什么

CommandCode Go/GOAT/Pro/Max 通过它的 OpenAI 兼容入口访问：

| 上游路由 | 状态 |
|---|---|
| `POST {base-url}/chat/completions` | 服务 OSS 模型（`deepseek/*`、`z-ai/*`、`Qwen/*`、`MiniMaxAI/*`、`moonshotai/*` …） |
| `GET {base-url}/models` | 模型目录 |
| `POST {base-url}/messages` | 只接受 Claude 模型 —— OSS 模型 id 会被拒绝：*"Model … is not supported on this endpoint"* |
| `POST {base-url}/responses` | 未注册的路由 |

默认上游地址是 `https://api.commandcode.ai/provider/v1`。

因为目录里的模型实际上只有一条可用路由，**从 `/models` 发现的所有模型默认都走 chat-completions**，不区分厂商前缀。`route-overrides` 是给"上游确实在别的端点提供其他协议"的部署准备的例外通道。

## 为什么需要这个插件

不用插件时，把 CommandCode Go/GOAT/Pro/Max 接入 CLIProxyAPI 需要手写多个 provider 段，而且几个客户端强依赖的能力要么缺失、要么是错的：

- **配置与密钥重复**：同一批 key 要在多个 provider 段里各配一遍。
- **调度被割裂**：轮换、限流、冷却无法共享，管理后台也看不到每个 key 的用量。
- **客户端得懂协议**：客户端必须事先知道某个模型对应哪个上游端点。
- **思考内容丢失**：CommandCode 的思考文本出现在 `reasoning` / `reasoning_details[].text` 里（从不使用标准的 `reasoning_content`），普通 OpenAI 兼容 provider 会让 Anthropic 和 Responses 客户端**整段丢失思维链**。
- **effort 被本地拒绝**：CommandCode 的 `/models` 不提供任何 thinking 元数据，于是本地校验 `reasoning_effort` 的实现会把上游明明接受的 `xhigh`/`max` 直接拒掉。

## 解决方式

- **统一密钥池**：插件把每个配置的 key 注册为 CLIProxyAPI 的 auth 记录，轮换、重试、错误冷却、会话粘性和每 key 统计全部交给宿主调度器。
- **透明翻译与路由**：客户端只写 `commandcode/<上游 id>`，不需要知道上游协议。
- **思考保真**：三种客户端格式都能拿到上游的思考文本；模型没有能力声明时，`reasoning_effort` 原样转发。
- **单一模型目录**：`{base-url}/models` 里的模型全部以 `commandcode/` 前缀出现在 `/v1/models`。

## 功能

- **单一 Provider 命名空间**：模型形如 `commandcode/deepseek/deepseek-v4.1-flash`、`commandcode/z-ai/glm-5.3-flash`（前缀可配置，也可关闭而直接使用上游裸 id）。
- **多协议客户端翻译**：OpenAI Chat Completions、Anthropic Messages、OpenAI Responses 三种请求都会转成上游 chat-completions 调用，响应（含流式）再转回来。
- **思考内容保真**：把上游的 `reasoning`、`reasoning_details[].text`、`reasoning_content` 归一化到各目标格式：
  - Claude 客户端：前置一个 `thinking` 块 + `thinking_delta` 事件；
  - Responses 客户端：前置一个 `reasoning` output item + `response.reasoning_summary_*` 事件；
  - OpenAI 客户端：每个 chunk 回填 `reasoning_content`，厂商原始字段保持不变。
- **能力感知的思考控制**：`route-overrides` 或模型目录里的 thinking 元数据一旦声明能力，就恢复严格校验与预算夹取；没有任何声明时，客户端的 `reasoning_effort` 原样转发（`auto`/`none` 省略该字段），以上游为准。
- **动态目录发现**：远程目录 + 本地兜底、去重，并对被排除的模型给出诊断。
- **CommandCode 配额页面**：管理后台单独一页，列出每个凭据对应的账号邮箱、套餐、剩余套餐额度和滚动 5 小时/每周窗口（已用、上限、重置时间），每张卡片按需独立刷新。
- **多密钥调度**：一份 key 池通过 CLIProxyAPI 原生调度器跨所有协议共享。

## 环境要求

- **CLIProxyAPI**：`v7.2.138+`
- **Go**：1.26+（`-buildmode=c-shared` 需要启用 CGO）

## 构建

```bash
# Linux (AMD64)
go build -buildmode=c-shared -o plugins/linux/amd64/commandcode-go-cliproxyapi.so .

# macOS (ARM64)
go build -buildmode=c-shared -o plugins/darwin/arm64/commandcode-go-cliproxyapi.dylib .

# Windows (AMD64)
go build -buildmode=c-shared -o plugins/windows/amd64/commandcode-go-cliproxyapi.dll .
```

把产物放到宿主的插件目录（例如 `<cliproxyapi_root>/plugins/<os>/<arch>/`）。

## 配置要点

> **`api-keys` 是运行必需，但不再阻断注册。** 没有 key 时插件以**待配置（pending）**
> 状态正常注册：管理面板显示"已注册"，并渲染出完整配置表单，可以直接在
> WebUI 里填 key。在保存 key 之前，插件不发布任何模型，调用会返回明确错误
> `commandcode plugin is registered but not configured: set api-keys in the plugin configuration`。
>
> 这修掉了之前的死循环：注册本身失败（报 `api-keys: at least one key is required`）、
> 面板显示 **未注册 / 未生效**，而插件又没有声明任何配置字段，
> 导致 UI 里根本没有地方填 key。

## 在管理面板中配置

所有配置项都已声明为插件配置字段，可直接在插件管理页设置，无需手改 `config.yaml`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `api-keys` | array（JSON） | `[{"value":"cc_..."}]`；支持 `${ENV_VAR}`；旧写法，等价于 provider 账号 |
| `accounts` | object（JSON 数组） | `[{"label":"go","mode":"go-cli","credential":"user_..."},{"credential":"user_..."}]`；`mode` 默认 `provider`，Go 套餐**必须**用 `go-cli` |
| `pool` | object（JSON） | `{"strategy":"sticky","max-concurrency-per-account":1}`；`strategy` 取 `sticky` 或 `round-robin` |
| `base-url` | string | 默认 `https://api.commandcode.ai/provider/v1` |
| `catalog-url` | string | 默认 `{base-url}/models` |
| `model-prefix` | object（JSON） | `{"enabled":true,"value":"commandcode"}` |
| `catalog` | object（JSON） | `{"refresh-interval":"15m","stale-while-unavailable":true,"static":["claude-sonnet-4-6"]}`；`static` 为纯 go-cli 池提供模型来源 |
| `protocols` | object（JSON） | `{"chat-completions":true,"messages":true,"responses":true}` |
| `route-overrides` | object（JSON） | `{"<模型>":{"protocol":"...","endpoint":"..."}}` |
| `device.enabled` | bool | `true` | go-cli 请求的伪设备身份层。开启时每个凭据呈现一台稳定的伪机器，并在首次生成前向上游登记（指纹 + 生命周期）。关闭后 `x-project-slug` 与两项上报**全部停止**。 |
| `device.project-dir` | string | `C:\Users\dev\projects\app` | 伪工作目录。同时喂给信封的 `config.workingDir` 与 `x-project-slug` 头，两者因此不可能互相矛盾。 |
| `device.identity-salt` | string | 空 | 改变某个凭据「映射到哪台伪机器」。凭据被风控时的逃生口；不参与哈希阶段，也不发给上游。 |
| `request-timeout` | string | 如 `5m` |
| `max-response-bytes` | integer | 默认 `67108864` |
| `allow-http` | boolean | 允许 http:// 上游，仅测试用 |

嵌套配置以 **object** 字段（填 JSON）暴露，**不**写成 `model-prefix.enabled` 这类点号键：
宿主对插件配置做浅合并，点号键会落成字面量键 `model-prefix.enabled`，插件读不到。

> **模型列表**：由 `{base-url}/models` 自动发现，无需手工添加。
> 对单个模型的控制通过 `model-prefix`、`protocols`、`route-overrides` 表达。

> **关于 key 的可见性**：CPA 配置表单没有 secret 类型，API key 会以明文显示并写入
> CPA 配置文件。请勿截图或分享配置页面。插件自身不输出 key 内容，只记录 key 数量和截断 hash。

## 配置

```yaml
plugins:
  enabled: true
  dir: /data/plugins
  configs:
    commandcode-go-cliproxyapi:
      # 上游地址（默认 "https://api.commandcode.ai/provider/v1"）
      base-url: "https://api.commandcode.ai/provider/v1"

      # 可选：目录地址覆盖（默认 "{base-url}/models"）
      # catalog-url: "https://api.commandcode.ai/provider/v1/models"

      # 客户端可见的模型 id 前缀
      model-prefix:
        enabled: true              # true -> "commandcode/<model>"（默认 true）
        value: "commandcode"

      # CommandCode 凭据：推荐用账号池（accounts）。
      # 每个账号声明它走哪个上游面：provider（默认，Provider API）或
      # go-cli（Go 套餐专用，走 /alpha/generate，因为 Provider API 会拒绝 Go 套餐）。
      accounts:
        - label: "go"
          mode: "go-cli"
          credential: "user_xxx"
        - label: "pro"
          credential: "user_yyy"
          # disabled: true            # 保留配置但不参与轮换
        - credential: "${COMMANDCODE_API_KEY}"   # 支持 ${ENV_VAR}

      # 账号选择策略
      pool:
        strategy: "sticky"              # sticky | round-robin
        max-concurrency-per-account: 1  # 0 = 不限

      # 旧写法：扁平 key 列表，会被折叠为 provider 账号
      # api-keys:
      #   - value: "user_xxx"

      # 目录发现
      catalog:
        refresh-interval: "15m"           # 最小 "1m"
        stale-while-unavailable: true
        # 可选：静态模型表。当池里没有 provider 账号（例如只配了 Go 套餐）时，
        # 上游 /models 属于 Provider API、会拒绝 Go key，此时用这份列表发布模型。
        static:
          - "claude-sonnet-4-6"
          - "deepseek/deepseek-v4-pro"

      # 协议开关：关掉某个协议会移除路由到它的全部模型
      protocols:
        chat-completions: true
        messages: true
        responses: true

      # 按模型钉路由；只有上游在别的端点提供该模型时才需要
      # （CommandCode 自身的 OSS 模型都在 chat-completions 上）
      route-overrides:
        "claude-sonnet-5":
          protocol: "messages"            # chat-completions | messages | responses
          endpoint: "/v1/messages"        # 必填，必须以 "/" 开头

      request-timeout: "5m"
      max-response-bytes: 67108864        # 64 MiB
      allow-http: false                   # 本地测试允许 http://
```

### 配置项

| 配置 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `api-keys` | `[]object` | *(旧写法)* | 形如 `- value: "..."` 的密钥列表；支持 `${ENV_VAR}`；重复值和空值会被拒绝。会折叠为 `provider` 账号，保留以兼容旧配置。 |
| `accounts` | `[]object` | `[]` | 账号池。每项：`label`（显示用）、`mode`（`provider` 默认 / `go-cli`）、`credential`（支持 `${ENV_VAR}`）、`disabled`。**Go 套餐必须写 `mode: go-cli`**，否则会被发往拒绝它的 Provider API。 |
| `pool.strategy` | `string` | `sticky` | `sticky`（优先最近的健康账号）或 `round-robin`。 |
| `pool.max-concurrency-per-account` | `int` | `0` | 每个账号的在途请求上限；`0` 为不限。参考实现默认 1，可避免单账号看起来像突发流量。 |
| `base-url` | `string` | `https://api.commandcode.ai/provider/v1` | 上游 provider 地址。必须是合法 HTTPS（`allow-http: true` 时可用 HTTP），不能带 query、fragment 或 userinfo。 |
| `catalog-url` | `string` | `{base-url}/models` | 目录发现地址。 |
| `model-prefix.enabled` | `bool` | `true` | `true` 时客户端用 `<prefix>/<model>`，`false` 时直接暴露上游裸 id。 |
| `model-prefix.value` | `string` | `commandcode` | provider 前缀。 |
| `catalog.refresh-interval` | `duration` | `15m` | 目录轮询周期（最小 `1m`）。 |
| `catalog.stale-while-unavailable` | `bool` | `true` | 刷新失败时继续提供上一份有效目录。 |
| `catalog.static` | `[]string` | `[]` | 静态模型 id 表。仅在拿不到实时 `/models` 时启用——最常见的是**池里没有 provider 账号**（`/models` 属于 Provider API，会拒绝 Go 套餐 key）。填了它，纯 `go-cli` 单账号也能发布 `/v1/models`。 |
| `plan-filter` | *(自动)* | 开 | 自 0.4.2 起实时模型目录按套餐过滤：只发布池内账号实际可调用的模型（多账号取并集；未知套餐/未知模型一律保留，服务器仍是最终闸门）。由各 go-cli 账号的订阅自动驱动，无需配置。被过滤的模型会以"not included in the account's plan"出现在 unsupported 诊断里。 |
| `protocols.*` | `bool` | `true` | 路由总开关；关闭的协议会带着诊断信息排除其模型。 |
| `route-overrides` | `map` | `{}` | `{ 模型: { protocol, endpoint } }`，把某个模型钉到别的上游路由；`endpoint` 必填。 |
| `request-timeout` | `duration` | `5m` | 上游 HTTP 超时（配额/账号请求另按 30s 上限）。 |
| `max-response-bytes` | `int64` | `67108864` | 非流式响应体上限。 |
| `allow-http` | `bool` | `false` | 允许 `http://` 上游，仅供本地测试。 |

### 配额页面

管理后台 → 插件 里的 `CommandCode Quota` 页面读取与 `base-url` 同源的账号接口：

| 接口 | 用途 |
|---|---|
| `GET {authority}/alpha/billing/credits` | 剩余套餐额度和 5 小时/每周窗口 |
| `GET {authority}/alpha/billing/subscriptions` | 套餐 id/状态 |
| `GET {authority}/alpha/whoami?limits=1` | 卡片标题用的账号邮箱 |

`{authority}` 由 `base-url` 去掉 provider 路径（`/provider/v1`）得到。每张卡片手动独立刷新，页面从不轮询，配额数值也不参与路由决策。

页面与 **WorkBuddy 面板共用同一套设计系统**（与 CPA 主面板 `key-policy / themes.scss` 同源的主题令牌、相同的组件类名，以及相同的 `data-theme` 桥接——嵌入时跟随父页）。版式也向该面板看齐：顶部工具栏（`刷新数据` 一次刷新全部）、带计数的筛选胶囊（`全部 / 正常 / 已超限 / 套餐未知`）、搜索框、剩余额度排序、用量汇总卡（剩余/已用/额度池/消耗占比 + 消耗进度条），以及每凭据一张卡片，把 5 小时 / 每周 / 月度额度全部**计量条**化，并按 50/75/90% 分级取色。

筛选、搜索、排序只作用于**已拉取的快照**，不触发任何配额请求——只有主动刷新才会请求。

页面还支持**直接添加 Key**（「＋ 添加 Key」）：凭据、备注（可选）、传输模式（`go-cli` 默认 / `provider`）。宿主不给插件写自身配置的能力，所以新增的 key 存成 **CPA auth 记录**，由插件并入账号池；配置里的 `accounts` 照旧有效，两者取并集（重复时配置优先）。密钥从不出现在响应、日志或错误里。

> 从**空配置**起步时注意：宿主只在启动/配置重载时读取插件的模型列表，所以添加**第一个** key 后需要重启一次 CPA（或重新保存一次插件配置）模型才会出现——页面在这种情形下会提示。已有配置账号时新 key **立即生效**。

### reasoning effort（思考强度）

CommandCode **没有能力查询接口**：`{base-url}/models` 只返回 `id`、`object`、`created`、`owned_by`、`name`、`context_length`，`/alpha/*` 账号面里也没有 models 或 capabilities 路由。真正的"每个模型支持哪些 effort"只存在于厂商自己的客户端里（`command-code` CLI 和网页端各自内置一份静态表），而上游网关本身只要值属于并集 `low | medium | high | xhigh | max` 就接受，并不按模型校验。

因此本插件的行为是：

- 目录里没有声明能力时，`reasoning_effort` 原样转发（`auto`/`none` 省略该字段），所以 `xhigh` 和 `max` 都能用；
- 一旦目录条目带了 `thinking` 对象（`levels`、`min`、`max`、`zero_allowed`、`dynamic_allowed`），就以该声明为准，恢复严格校验与预算夹取；
- 厂商的按模型对照表、上述结论的探测过程和"没有能力接口"的证据都记录在 [`docs/model-capabilities.md`](docs/model-capabilities.md)。

## 测试

```bash
go test ./...        # 单元测试 + 带 mock 的端到端测试
go test ./... -cover # 带覆盖率
go vet ./...         # 静态检查
```

## 来源

本插件在编写时参考了另外两个 CommandCode 插件：

- [opencode-go-cliproxyapi](https://github.com/massiveits/opencode-go-cliproxyapi)（v0.1.7）是直接的前身：adapter 内核、catalog、config、auth 和配额页骨架都来自那里；CommandCode 相关的改动——思考内容保真、无能力声明时的 effort 透传、chat-completions 单一默认路由、配额页改用真实账号接口——在其之上适配。
- [cpa-plugin-commandcode](https://github.com/ahoo/cpa-plugin-commandcode)（ahoo）是更早的一个独立 CommandCode 插件，它的文档最先记录了本插件必须复现的厂商行为：思考文本出现在 `reasoning` / `reasoning_details[].text` 而从不使用标准的 `reasoning_content`；因此必须在 CLIProxyAPI 的 openai→claude 翻译器读取之前回填 `reasoning_content`；流式 `/v1/messages` 要求每个分块带 SSE `data: ` 前缀；上游只接受完整厂商名（`deepseek/deepseek-v4.1-flash`）。

本仓库：<https://github.com/mczhoucn/commandcode-go-cliproxyapi>

## 许可证

[MIT](LICENSE)
