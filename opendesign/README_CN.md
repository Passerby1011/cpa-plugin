# Open Design CLIProxyAPI 插件

[English](README.md) | **简体中文**

[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 的原生动态 Go 插件，把 **Open Design** 套餐作为单一 provider（`opendesign`；对外模型 id 带 `opendesign/` 前缀）。

登录、模型目录、三协议执行器与 key 调度都由插件自己负责：一个账号（或一枚粘贴进来的 `sk-amr-…` key）即可同时服务 OpenAI、Anthropic、Responses 三类客户端。

> **来源与打包** —— 本插件是 magpie 的
> [`@magpie-community/opencode-opendesign-auth`](https://www.npmjs.com/package/@magpie-community/opencode-opendesign-auth)
> （MIT）的 Go 移植。下面每条行为都是从该包源码与单测抄写来的，注释里因此会引用当初学到该行为的 issue 编号。差别只在打包形态（c-shared Go 插件而非 OpenCode hook）与新增的 CPA provider 面。

## Open Design 实际提供什么

两个平面，各自认不同的凭证：

| 平面 | 默认地址 | 凭证 | 用途 |
|---|---|---|---|
| 模型面（`amr-link`） | `https://amr-link.open-design.ai/v1` | `sk-amr-…` runtime key | 所有模型请求，以及 `GET /models` |
| 控制面（`amr-api`） | `https://amr-api.open-design.ai` | `vela_ctrl_…` control key | 设备授权登录，以及读取套餐额度 |

控制面**不接受 API key**。就是这一条把「登录的账号」和「粘贴的 key」分成了两件事：

- **设备授权登录**同时拿到两把 key，因此账号的套餐额度可读。
- **粘贴 `sk-amr-…`** 只有 runtime key，模型目录照样拉得到，但额度不可读——auth 行会写明原因，而不是让操作者自己猜。

所有模型在三种协议（`/chat/completions`、`/messages`、`/responses`）上都能被服务，但模型只在它被声明的那一种协议上「思考」。

## 安装

### 从 Release（推荐）

```bash
# 按你的平台下载（示例 linux/arm64）
unzip opendesign_0.2.0_linux_arm64.zip
cp opendesign.so /path/to/cliproxyapi/plugins/opendesign.so
```

### 从源码

```bash
cd opendesign
make build           # CGO_ENABLED=1 go build -buildmode=c-shared -o opendesign.so .
cp opendesign.so /path/to/cliproxyapi/plugins/
```

必须 CGO：插件是 `c-shared` 库，跑在宿主进程内，不能独立运行。

### config.yaml

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    opendesign:
      enabled: true

      # 粘贴的 key 也能用，但读不到套餐额度。
      api-keys:
        - value: "${OPENDESIGN_RUNTIME_KEY}"      # sk-amr-…
          # control: "vela_ctrl_…"                # 可选；只有登录才会产生
          label: work

      # 以下均可省略，注释里是默认值。
      base-url: "https://amr-link.open-design.ai/v1"
      control-url: "https://amr-api.open-design.ai"
      request-timeout: "5m"
      models:
        allow: []                                # 空 = 全部发布
        deny: ["claude-*"]                       # 恒压过 allow
      pool:
        strategy: sticky                         # 或 round-robin
        max-concurrency-per-account: 0           # 0 = 不限并发
```

`${ENV_VAR}` 只在凭证字段内由 CPA 进程展开。带 userinfo、query 或 fragment 的 URL 一律拒绝：凭证绝不允许搭在 upstream URL 上。

## 使用

### 用 Open Design 账号登录（推荐）

1. CPA 管理面板 → Auth 文件 → Open Design 登录卡片。
2. 在打开的页面上确认那串码——插件会一直轮询到批准，然后自己收尾。
3. 两把 key 落盘到 `opendesign-<uid>.json`，该账号的模型随即出现，不需要手动重建配置。

轮询期间**网络抖动不判登录失败**：循环会继续请求直到这串码自己过期——一次抖动不该让操作者重新要一个码。只有 404、终态状态（`expired` / `canceled` / `denied` / `rejected`）或 401 才结束流程。

### 粘贴 API key

把 `sk-amr-…` 加进 `api-keys`（或粘到登录弹窗里）。模型目录仍按这枚 key 拉取；套餐额度读不到，auth 行会写明原因。

### 面板

`/v0/resource/plugins/opendesign/panel` —— 每账号一张卡片，显示该账号的 Design Plan 额度（窗口、钱包余额、套餐周期），以及已服务的模型、每个模型声明的协议、实测出来的推理档位。页面直接读 CPA 面板原本就持有的管理密钥，不再二次登录。

## 套餐额度

额度卡片是**插件自己的**，不是宿主的：CPA 的插件 API 没有任何 provider 级 quota 位，所以数字由两条 Management API 路由提供，由上面那页面板渲染。

| 路由 | 作用 |
|---|---|
| `GET /v0/management/plugins/opendesign/allowance` | 每账号一张卡片：标签、额度是否可读、登录状态。**不发任何上游请求**。 |
| `POST /v0/management/plugins/opendesign/allowance` | 读**单个**账号（`{"id": "<卡片 id>"}`）：窗口、钱包余额、套餐周期。 |

卡片 id 是 runtime key 的摘要，**不是 key 本身**。

一次读取包含三项，全部用账号的 **control key**（`vela_ctrl_…`），绝不用模型请求那枚 key：

| 读取 | 路径 | 得到 |
|---|---|---|
| workspace | `GET /api/v1/workspaces/current` | 活动 workspace 与个人档位 |
| preflight | `GET /api/v1/workspaces/{id}/billing/preflight` | `codingPlan`（含 `eligible`）与 `balanceUsd` |
| summary | `GET /api/v1/billing/summary` | `membershipTier`、订阅状态与本期到期时间 |

workspace 按 control key **缓存 10 分钟**；preflight 与 summary 每次刷新都实时读，因为它们才是会变的数字。

窗口换算照 magpie 那张卡片：百分比 `round(max(0, used/limit*100), 1)`；`limit<=0` 的窗口**跳过**而不是画一根空进度条；列表**按窗口时长升序**（短的在前）；名称是 `N hours` / `N days`（`604800` → `7 days`）；credits 缩写为 `9999` / `16.3k` / `150k` / `2.3M`。`resetsAt` 接受 epoch 秒 / epoch 毫秒 / RFC 3339 三种形态，输出统一。

三种状态**不等于同一种失败**，各自说各自的话：

| 状态 | 呈现 |
|---|---|
| 粘贴的 `sk-amr-…` key（无 control key） | `plan_readable: false` ——「Signed in with an API key: the Design Plan allowance needs the account sign-in.」完全不发上游请求。 |
| `codingPlan.eligible === false` | 「This account has no Design Plan allowance: its requests draw on the wallet balance.」钱包余额仍然显示。 |
| preflight 返回 401/403 | `sign_in: "expired"` ——「Open Design refused the account's sign-in — sign in again.」标记留在内存里，刷新页面仍显示失效；之后一次干净的读取会清掉它（`sign_in: "renewed"`）。 |

5xx 或传输失败**不标记任何东西**：只有「被拒」才意味着要重登，把好账号送去重登正是这个区分要防的错误。额度读取有自己独立的 30 秒预算，不受 `request-timeout` 影响。

## 模型

目录按该账号的 runtime key 从 `{base-url}/models` 拉取。遇到 401/403 判为「登录失效」并标记，不用种子目录悄悄顶替；其它失败才回落种子目录，操作者仍能拿到一份可用清单。

种子目录（12 条，抄自 magpie 的 `models.mjs`）负责补足目录缺失的字段：

- 目录给了的限制优先，缺的由种子补——一律回落通用 200K/32K 会把一个只是没在目录里声明限制的模型悄悄砍半；
- **种子声明的输入类型保留**，即使目录没列。这是刻意的取舍：多列一个模型其实会拒绝的类型，是厂商可见的报错；少列一个是静默的——调用方传的图被当文本发过去，模型答非所问；
- 目录声明的输出上限按 API 实际接受量硬夹（`glm-5.3-flash` → 131072，`gemini-3.8-flash` → 65536）。

各模型声明的协议：

| 模型 | 协议 |
|---|---|
| `claude-haiku-5.5`、`deepseek-v4-pro` | `/responses` |
| `glm-5.3-flash` | `/messages`（Anthropic schema） |
| 其余 | `/chat/completions` |

推理档位在实测到的地方是 `low`/`high`/`max`；`gpt-6-luna` 一档都不声明，而「不声明档位」的模型**完全不发**思考控制字段——这是它不会被要求按一个它忽略的档位去思考的唯一办法。

## 协议行为

- **签名按实际路径，不看模型声明的 API。** `/messages` 用 `x-api-key` 并补 `anthropic-version: 2023-06-01`；其余路径用 `Bearer`。请求完全可以为一个声明在 chat-completions 上的模型走到 `/messages`，key 就按那条路径要求的法子带。
- **思考文本三向保真。** chat 的 `reasoning_content`、Anthropic 的 `thinking` block（保留 signature）、Responses 的 `reasoning` item，都是在协议之间搬过去，而不是丢掉。九条两两翻译路径就是九个丢掉思考的机会；本插件只用一个内部表示，每种协议各一个 decoder 和一个 encoder。
- Chat / Responses / Anthropic 客户端都能访问任意模型，无论它声明在哪种协议上。

## 过滤

`models.allow` / `models.deny` 既能匹配上游 id（`glm-5.3-flash`），也能匹配带前缀的对外 id（`opendesign/glm-5.3-flash`）。`*` 匹配任意长度的字符（含空），所以 `deny: ["claude-*"]` 一句就能摘掉一整个家族。`allow` 为空即全部发布；`deny` 恒优先。

## 开发

```bash
cd opendesign
make build   # c-shared 插件
make test    # go test -race -count=1 ./...
make lint    # gofmt 必须为空（fail-fast），然后 go vet
make release # 交叉编 linux/amd64 + linux/arm64
```

`panel.html` 经 `go:embed` 嵌入二进制，改完必须重新 `make build`——`gofmt` 和 `go vet` 不碰它。

## License

MIT。移植自 magpie 的 `opencode-opendesign-auth`（MIT），见 [LICENSE](LICENSE)。
