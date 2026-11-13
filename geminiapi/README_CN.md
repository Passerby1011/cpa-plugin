# GeminiAPI —— CPA 的 Gemini Web / AI Studio 插件

单插件、单 provider（id `geminiapi`）：**插件自己托管、校验、启动并守护一个本机
OpenAI 兼容后端**，把它的模型以 Gemini 命名空间发布出去。

它不是一个"只写 provider 配置"的空壳：

```
CPA  ──►  geminiapi 插件 (.so)
              │
              ├── 托管：<data-dir>/bin/gemini-web2api-go（pin 版本，sha256 校验）
              │        └── 只监听 127.0.0.1:<port>
              │
              └── 对外发布模型： ai-studio/<model>
```

## 为什么是"一个 provider + 命名空间前缀"

原生插件只能声明一个 `executor.identifier`，没法像 JS 扩展那样声明两个
provider。所以本插件只做**一个 provider**，把两个后端表达成**模型 id 命名空间**：

| 命名空间 | 模型 id | 后端 |
|---|---|---|
| `ai-studio` | `ai-studio/gemini-2.5-pro` | 本卡 |
| `gemini-web` | `gemini-web/<model>` | 下游卡 |

多级 `/` 是允许的（`commandcode/deepseek/...` 已证），路由只看 id 前缀，没有隐藏状态。

> 本卡只发 `ai-studio`。`gemini-web/<model>` 的请求会被明确拒绝，绝不会被错的后端
> 悄悄接走。

## 托管的后端

| 项 | 值 |
|---|---|
| 版本 | `4.28.0-magpie.1`（pin 在 `internal/backend/pin.go`） |
| 来源 | `BUKOWSKIREAL/magpie-gemini-web`，tag `backend-v4.28.0-magpie.1` |
| 资产 | linux/darwin/windows × amd64/arm64，逐个 sha256 pin |
| 默认端口 | `2048`；冲突时顺延到下一个可用端口 |
| 监听 | **仅** `127.0.0.1`（代码强制） |

首次使用流程：

1. **下载** pin 资产（可续传、有停滞超时、可走代理）。
2. **校验 sha256**。*不匹配绝不落盘到启动路径、绝不启动。*
3. **生成** `config.json`（仅 loopback）与 `secrets.json`（自动生成的 key +
   admin token，0600）。
4. **启动**：Windows 放进 **Job Object**（关句柄即杀），Unix 用**进程组**
   （`setpgid` + `kill(-pgid)`），保证整棵树随 CPA 一起死。
5. **健康探测**：`GET /` 取版本；再用本 setup 的 key 打 `GET /v1/models`，
   证明这个监听者**就是我们的后端**，而不是占着端口的外人。
6. **守护**：崩溃重启（最多 3 次）；pid 文件 + 身份核对供下次回收。

### 孤儿进程（本卡最大的风险）

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

## 账号，以及必须读的那句告示

AI Studio 要的是 **Google 登录态（Cookie）**，不是 API key。CPA 没有浏览器，
所以走手工路径：

**F12 → Network → 任意一个 `gemini.google.com` 请求 → Request Headers →
复制 `cookie` 那一整行 → 粘贴到插件页。**

插件先校验（缺哪个 Cookie 会点名），再**仅经 loopback** 交给**本地后端自己的
admin API**，由后端去探测登录态。探测不通过的账号会被删掉，不留死条目。

- Cookie **只**落在 `data-dir` 里后端自己的数据库中；不进 CPA 配置、不进日志、
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

## 模型

- 目录来自后端 `/v1/models`，每 10 分钟刷新一次；启动后与导入账号后立即刷新。
- 思考档 `minimal` / `low` / `medium` / `high` 按目录声明给出。
- **图片输入**默认开（多模态 chat 模型能读图）。
- 命名为非 chat 模态（image / tts / audio / live / video / music / canvas /
  research）的模型、`-build` 模型、`assistant_route` 模型都会被过滤。
- **图片生成模型默认不发布。** CPA 没有 image 路由，chat 请求点到它只会失败。
  `ai-studio.allow-image-models: true` 才会发布，并标注为 image 输出。

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
        port: 2048
        context-limit: 0            # 0 = 用后端给的值
        output-limit: 0
```

`ai-studio.base-url` 指向已经在跑的外部 OpenAI 兼容后端。填了之后插件只做转发，
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

分栏见 `README.md` 的 "Verification status"，真机记录见
`docs/real-backend-verification.md`。凡未列入已验证的，都不宣称已验证。

## 许可

MIT。托管的后端是另一个独立作品，有自己的许可——见 `LICENSES/` 与上游 release。
