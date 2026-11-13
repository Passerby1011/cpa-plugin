# GeminiAPI —— CPA 的 Gemini Web / AI Studio 插件

单插件、单 provider（id `geminiapi`）：**插件自己托管、校验、启动并守护一个本机
OpenAI 兼容后端**，把它的模型以 Gemini 命名空间发布出去。

它不是一个"只写 provider 配置"的空壳：

```
CPA  ──►  geminiapi 插件 (.so)
              │
              ├── 托管 <data-dir>/ai-studio/package/aistudio2api      (AIStudio2API 0.3.5-magpie.1，压缩包 + 包内可执行文件双 pin)
              │      └── 只监听 127.0.0.1:<port>  ──►  发布 ai-studio/<model>
              │
              └── 托管 <data-dir>/gemini-web/bin/gemini-web2api-go    (gemini-web2api-go 4.28.0-magpie.1)
                     └── 只监听 127.0.0.1:<port>  ──►  发布 gemini-web/<model>
```

两套后端、两套 pin、两个进程。除了本插件这一个 provider id，两侧不共享任何东西。

## 为什么是"一个 provider + 命名空间前缀"

原生插件只能声明一个 `executor.identifier`，没法像 JS 扩展那样声明两个
provider。所以本插件只做**一个 provider**，把两个后端表达成**模型 id 命名空间**：

| 命名空间 | 模型 id | 后端（各自独立 pin） |
|---|---|---|
| `ai-studio` | `ai-studio/gemini-2.5-pro` | **AIStudio2API** `0.3.5-magpie.1` |
| `gemini-web` | `gemini-web/gemini-2.5-pro` | **gemini-web2api-go** `4.28.0-magpie.1` |

多级 `/` 是允许的（`commandcode/deepseek/...` 已证），路由只看 id 前缀，没有隐藏状态。

本构建**两个命名空间都发布**。各自拥有独立的 pin 表、独立的数据目录
（`<data-dir>/ai-studio`、`<data-dir>/gemini-web`）、独立端口、独立 pid 文件与独立
进程组——一套后端崩了或停了不会影响另一套。`gemini-web/<model>` 的请求绝不会走
AI Studio 的后端，反之亦然；前缀不属于这两者的 id 会被明确拒绝。

## 托管的后端

| 项 | `ai-studio` | `gemini-web` |
|---|---|---|
| 后端 | AIStudio2API `0.3.5-magpie.1` | gemini-web2api-go `4.28.0-magpie.1` |
| 来源 | `BUKOWSKIREAL/magpie-gemini-web`，tag `studio-v0.3.5-magpie.1` | 同一仓库，tag `backend-v4.28.0-magpie.1` |
| 资产 | darwin/linux/windows × amd64/arm64 的**压缩包**，同时 pin 压缩包 sha256 **和**包内可执行文件的 sha256 | linux/darwin/windows × amd64/arm64，逐个 sha256 pin |
| 默认端口 | `2049`（它自己的默认也是 2048，见下） | `2048` |
| 监听 | **仅** `127.0.0.1`（代码强制） | **仅** `127.0.0.1`（代码强制） |
| 设置来源 | 数据目录里的 `.env` **和**子进程环境（上游先读环境变量） | 只有两个凭据走环境变量 |

两套 pin 表分别在 `internal/backend/pin.go` 与 `internal/backend/flavors.go` 内，
取自上游 release 清单，别处不再重复抄写摘要。

AI Studio 的资产是压缩包，所以装两次校验：解包前校验压缩包摘要，解包后再校验可
执行文件摘要（可执行文件摘要每次启动前还会再校一次）。gemini-web2api-go 直接给裸
可执行文件，一道摘要即可。

给 AI Studio 用 `2049` 的原因：两套后端默认都是 2048，同一台机器上全新安装两套会
撞端口。数据目录里已经记录的端口之后永远优先。

首次使用流程：

1. **下载** pin 资产（可续传、有停滞超时、可走代理）。
2. **校验 sha256**。*不匹配绝不落盘到启动路径、绝不启动。* AI Studio 还多一道：
   包内可执行文件在解包后、以及每次启动前再校验一次。
3. **生成**后端自己的状态：gemini-web 是 `config.json`（仅 loopback）+ `secrets.json`
   （自动生成的 key + admin token，0600）；AI Studio 是 `config.json` + 它自己读的
   `.env`。
4. **启动**：Windows 放进 **Job Object**（关句柄即杀），Unix 用**进程组**
   （`setpgid` + `kill(-pgid)`），保证整棵树随 CPA 一起死。
5. **健康探测**按后端分：gemini-web 先 `GET /` 取版本/模型，再用本 setup 的 key 打
   `GET /v1/models`；AI Studio 先 `GET /api/status`（401 也证明管理面在），再用 key 打
   `GET /v1/models`。AI Studio 返回带自己 `service_stopped` 码的 503 时判为
   **up / 无账号**——既不当成端口被外人占了，也不当成"还在启动"。
6. **守护**：崩溃重启（最多 3 次）；pid 文件 + 身份核对供下次回收。

### 孤儿进程（本插件最大的风险）

后端是 CPA 宿主的长寿子进程。CPA 退出后若子进程还活着，它会一直占端口、一直
维持 Google 会话。三层防护：

- **操作系统层**：Windows 的 `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` / Unix 的
  `setpgid` + `kill(-pgid)`。即使宿主被硬杀，OS 也会回收整棵树。
- **插件层**：`plugin.shutdown` 显式停子进程。
- **下次启动**：按 pid 文件回收，但**必须**先通过 pid **且**可执行文件路径的身份
  核对。活着的 pid 若属于别的程序，只报告、**绝不杀**。

## 安全边界

- 启动路径只可能是 pin 校验通过的二进制，且**每次启动前**再校验一次哈希。
- 子进程 argv 是封闭形状：`--config <path> --db <path>`，任何配置值都注入不了
  新参数。
- 凭据走**环境变量**而不是 argv（argv 全机可读）。
- 子进程环境里的 `HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY` 会被**剔除**，
  宿主代理不可能把插件自己的 localhost 调用也带走。
- 记录端口被外部服务占用时，`backend/stop` 拒绝动手。

## 账号：两套后端，两条完全不同的路

| 命名空间 | 加账号的方式 | 落在哪 |
|---|---|---|
| `gemini-web` | 在插件页粘贴 Google `cookie` 请求头；插件仅经 loopback 转发到后端自己的 `POST /admin/api/cookies`，并由该后端探测登录态 | gemini-web2api-go 自己数据目录里的数据库 |
| `ai-studio` | 点"开始登录"；插件经 loopback 让 AIStudio2API 起它自己的浏览器登录任务（`/api/accounts/tasks`），操作者在该后端自己的窗口里完成 Google 登录 | AIStudio2API 自己数据目录里的授权状态 |

AIStudio2API **没有 cookie 端点**，gemini-web2api-go 也没有浏览器登录：把 cookie
发给 `ai-studio`、或期待 `gemini-web` 弹出登录窗口，都会被明确拒绝，而不是发给错的
服务。

`gemini-web` 的粘贴流程：

**F12 → Network → 任意一个 `gemini.google.com` 请求 → Request Headers →
复制 `cookie` 那一整行 → 粘贴到插件页。**

插件先校验（缺哪个 Cookie 会点名），探测不通过的账号会被删掉，不留死条目。

- Cookie **只**落在 `data-dir` 里该后端自己的数据库中；不进 CPA 配置、不进日志、
  不进任何响应。
- 请用**无痕窗口**。后端会轮换会话，和浏览器共用会被 Google 踢出登录。

> ### ⚠️ 多账号 ≠ 多个可选账号
>
> 多条 CPA auth 记录只是多个**调度槽位**；真正用哪个 Google 账号由**后端账号池**
> 决定，不是由 CPA 选中的那条 auth 记录决定。插件无法指定某个 Google 账号，也
> 读不到账号邮箱或 Cookie。
>
> 面板和 status 响应里都写明了这句，因为误解这点的运维一定会把"用错账号了"
> 当 bug 报上来。

## 模型：两套规则，各管一套

两套后端描述目录的形状不同，所以每个命名空间用自己的规则——一套规则管两个后端，
正是上一版把 `ai-studio` 发布成 0 个模型的原因：

- **`ai-studio`**（AIStudio2API）：目录里每个模型带**能力表**。必须声明 `chat_model`、
  不是 `assistant_route`、声明 `generateContent`、没有非文本能力（speech / live /
  transcription），并且在后端报 channel 覆盖时确有可用账号覆盖，才发布。思考档取自
  模型自己的 `thinking_levels`。**图片生成模型默认不发布**（CPA 没有 image 路由，
  chat 请求点到它只会失败）；`ai-studio.allow-image-models: true` 才发布并标注为
  image 输出。
- **`gemini-web`**（gemini-web2api-go）：目录是 **OpenAI 形状**
  （`id` / `object` / `owned_by`，没有别的），**没有能力表**。要求 `chat_model` 在这
  里等于发布 0 个模型，所以只要有 id 就发布；只把**名字**指向非 chat 模态的
  （image / tts / audio / live / video / music / canvas / research）过滤掉。后端列的
  `-thinking` / `-thinking-medium` 孪生 id 会折回基础模型，成为 `high` / `medium`
  两档（`low` 就是基础 id 本身）。
- 两个命名空间都一样：目录来自 `/v1/models`，每 10 分钟刷新，启动后与导入账号后立即
  刷新。**构建后 0 个模型的命名空间一律返回可读错误并说明原因**（后端整个目录为空，
  或哪条规则过滤了什么）——面板与 `/models` 响应都会显示它，而不是一张静默空表。

## 超时

| 配置 | 默认 | 含义 |
|---|---|---|
| `request-timeout` | `30m` | **整个请求** |
| `stream-idle-timeout` | `5m` | **两块 chunk 之间**的间隔，独立可配 |
| `model-timeout` | `15s` | 取目录 / 健康 / 账号读取 |

长时间但在持续出字的推理流不会被空闲预算杀掉；真正静默卡死的流会在总期限之前被
停掉。客户端断开会让请求上下文取消，并经宿主传输传播到后端——后端不会继续为没人
读的请求干活。

## 配置

```yaml
plugins:
  configs:
    geminiapi:
      data-dir: ""                  # 默认 <用户配置目录>/cpa-plugin/geminiapi
      proxy: ""                     # 只用于下载与 admin API，绝不用于 localhost
      request-timeout: 30m
      stream-idle-timeout: 5m
      model-timeout: 15s
      models:
        allow: []                   # 空 = 全部发布
        deny: []
      ai-studio:
        base-url: ""                # 填了 = 外部后端，插件不再托管
        api-key: ""                 # 与 base-url 成对，单填会被拒
        proxy: ""                   # 后端访问 Google 用的代理
        allow-image-models: false
        port: 2049
        context-limit: 0            # 0 = 用后端给的值
        output-limit: 0
      gemini-web:
        base-url: ""                # 与 ai-studio 完全独立的同形配置
        api-key: ""
        proxy: ""
        allow-image-models: false   # 这个后端本来也不发布图片模型
        port: 2048
        context-limit: 1000000      # 仅当后端不报限制时使用
        output-limit: 8192
```

两个命名空间各自配置：一侧填 `base-url` 不影响另一侧仍由插件托管；插件按命名空间
分数据目录（`<data-dir>/ai-studio`、`<data-dir>/gemini-web`）。

`base-url` 指向已经在跑的外部 OpenAI 兼容后端。填了之后该命名空间只做转发，
不安装、不启动、不停止——也不提供账号导入，因为它拿不到别人的 admin 口令。

## 安装

```bash
cd geminiapi
make build      # CGO_ENABLED=1 go build -buildmode=c-shared -o geminiapi.so .
make test       # go test -race -count=1 ./...
make lint       # gofmt + go vet
```

把 `geminiapi.so` 放进 CPA 插件目录，加上 `plugins.configs.geminiapi` 节点，
重启 CPA，然后打开：

```
/v0/resource/plugins/geminiapi/panel
```

## 验证状态

| 项 | 状态 |
|---|---|
| 两套 pin 表（各自独立，取自上游清单） | 单测 |
| sha256 不匹配拒绝安装/启动（gemini-web 与 AI Studio） | 单测 **且** 真字节负控 |
| AI Studio：压缩包 **与** 包内可执行文件双校验 | 单测 **且** 真字节验证（linux/amd64） |
| 每个命名空间独立数据目录 / 端口 / pid 文件 | 单测 |
| Studio 形状走能力表、OpenAI 形状只认 id | 单测 **且** 负控（拆分前 OpenAI 形状发布 0 个模型） |
| 发布 0 个模型时返回可读错误并在面板显示原因 | 单测 **且** 面板渲染 |
| AI Studio 自己的 503 `service_stopped` 判为 up/无账号（不是"外人占端口"） | 单测 **且** 真机验证 |
| 端口上未识别的 503 仍判为 foreign | 单测 |
| 账号导入按命名空间走各自面（`/admin/api/cookies` vs `/api/accounts/tasks`） | 单测；端点本身对着真后端核对 |
| 真机下载 → 启动 → 健康 → 停止（**两套后端**） | 见 `docs/real-backend-verification.md` |

凡未列入已验证的，都不宣称已验证。

## 许可

MIT。托管的后端是另一个独立作品，有自己的许可——见 `LICENSES/` 与上游 release。
