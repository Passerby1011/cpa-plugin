# CPA 插件仓库

[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 插件集合。当前提供
**WorkBuddy / CodeBuddy**（OAuth Provider）、**QoderWork**（OAuth/PAT Provider）、
**CommandCode**（套餐 Provider + 配额页）、**Open Design**（控制面额度 Provider）
与 **GeminiAPI**（托管本地 Gemini Web / AI Studio 后端的 Provider）。

## 插件

| ID | 说明 | 源码 |
|---|---|---|
| `workbuddy` | WorkBuddy / CodeBuddy 双通道（CN `copilot.tencent.com` + 国际 `workbuddy.ai`）：OAuth、动态模型、executor、CN 每日签到、**国际版每日活跃 30/50 积分领取**、积分生命周期、积分面板 | [workbuddy/](workbuddy/) |
| `commandcode` | CommandCode 套餐单一 provider（`commandcode/` 前缀）：OpenAI/Anthropic/Responses 三协议、ZDR、按 `supported_endpoints` 路由、推理保真与 off 档、Key 池调度、配额页 | [commandcode/](commandcode/) |
| `qoderwork` | QoderWork CN（`qoder.com.cn`）：OAuth 设备授权 + PAT 双登录（可共存）、登录自动领 Pro 包、COSY 签名推理、动态模型、每日签到、积分面板、token 保活 | [qoderwork/](qoderwork/) |
| `opendesign` | Open Design（Vela 控制面，`opendesign/` 前缀）：设备授权登录 + `sk-amr-…` 粘贴 key 双凭据、动态模型目录（含能力标签）、OpenAI/Anthropic/Responses 执行器、Key 池调度、**额度卡片**（5h/7d 窗口 + 钱包余额 + 套餐） | [opendesign/](opendesign/) |
| `geminiapi` | Gemini Web + AI Studio（`geminiapi/` 前缀）：插件自己安装/校验/托管两套本地 OpenAI 兼容后端（`aistudio2api` 0.3.5-magpie.1、`gemini-web2api-go` 4.28.0-magpie.1），各自独立数据目录/端口/进程组，目录按命名空间分流 | [geminiapi/](geminiapi/) |

## 多架构 Release

每个插件独立版本发 Release（tag `<id>-v*`），产物为 CPA 插件商店标准格式：

```text
<id>_<version>_linux_amd64.zip      # zip 根目录: <id>.so
<id>_<version>_linux_arm64.zip
<id>_<version>_darwin_amd64.zip     # <id>.dylib
<id>_<version>_darwin_arm64.zip
<id>_<version>_windows_amd64.zip    # <id>.dll
<id>_<version>_windows_arm64.zip
<id>_<version>_freebsd_amd64.zip
checksums.txt
```

命名规则与官方一致：`ArchiveName(id, version, goos, goarch) = {id}_{version}_{goos}_{goarch}.zip`
（见 CLIProxyAPI `internal/pluginstore`）。

CI：push / PR 全量构建（只出 artifacts）；tag `<id>-v*`（如 `workbuddy-v0.12.1`）
或 dispatch 触发**该插件独立版本**的 Release。

## 安装（linux/amd64 示例）

```bash
# 从 Release 下载（换成你要装的插件 id 与版本）
unzip workbuddy_0.13.6_linux_amd64.zip
# 扁平 plugins 目录（常见 docker 挂载）
cp workbuddy.so /path/to/cliproxyapi/plugins/workbuddy.so
# 或平台子目录布局
# mkdir -p plugins/linux/amd64 && cp workbuddy.so plugins/linux/amd64/
```

```yaml
plugins:
  enabled: true
  # 必须写绝对路径：相对路径按「进程工作目录」解析（官方镜像里是 /CLIProxyAPI），
  # 写 "plugins" 会解析成 /CLIProxyAPI/plugins 从而找不到 .so
  dir: "/path/to/cliproxyapi/plugins"
  configs:
    workbuddy:
      enabled: true
```

## 通过插件商店安装 / 更新

本仓库的 `registry.json` 可直接作为 CPA 插件商店的自定义源使用。插件以
`direct`（schema_version 2）方式声明各平台产物直链与 sha256——每次 Release 由
CI 自动刷新（`.github/scripts/sync-registry.py`），用户在商店 UI 即可一键安装/更新，
无需手工下载。

```text
https://raw.githubusercontent.com/Passerby1011/cpa-plugin/main/registry.json
```

添加后在商店 UI 安装/更新 **WorkBuddy**、**QoderWork**、**CommandCode**、
**Open Design** 或 **GeminiAPI**。

> **首次使用注意**：`registry.json` 里的 `artifacts` 由发版 CI 回填。新插件在
> 首个 Release 产出之前该字段为空数组，而 CPA 对
> `direct` 类型强制要求至少一个 artifact（`pluginstore/registry.go` 的
> `ValidateInstallPlan`），此时整个源会被判为无效并报
> `plugins[0]: direct install requires at least one artifact`。
> 先跑一次 Release（tag 或 workflow_dispatch）即可解除。
>
> **5 个插件全部纳入 CI 矩阵**（`test` / `build` / `build-cross`），每个发版产出
> 7 平台 zip：linux/amd64、linux/arm64、darwin/amd64、darwin/arm64、
> windows/amd64、windows/arm64、freebsd/amd64，外加 `checksums.txt`。
> 历史遗留：`scripts/ci-wiring-commandcode.patch` 与
> `scripts/release-commandcode.py` 是 commandcode 尚未进矩阵时的手工接线/发版脚本，
> 现已由 CI 取代，仅作存档。

## 插件维护要点

- **每个插件完全自包含**：独立 `go.mod`（go 1.26）、`go.sum`、`VERSION`、
  `Makefile`、`README.md`/`README_CN.md`、`LICENSE`。
- **module 路径**统一为 `github.com/hex-ci/cpa-plugin/<id>`。
- **版本注入**：构建用 `-ldflags "-X main.version=<version>"` 注入版本，插件在
  `main.go` 的 `version` 变量 + `init()` 里转发给内部包；`<id>/VERSION` 是版本源。
- **发版**：`make -C <id> tag`（读 `<id>/VERSION`）→ 推 tag → CI 出多平台产物 +
  回填 `registry.json`。5 个插件全部走 CI 矩阵；`commandcode` 早期的手工发版路径
  （`scripts/release-commandcode.py`）已作废。
- **`qoderwork/` 的模型目录是本地补丁**：静态兜底表
  （`models.go` 的 `qoderCNStaticCatalog20261019`）与别名映射（`body.go` 的
  `cpaToUpstreamKey`）按 Qoder CN 线上目录手工维护，模型换代时要改这两处 +
  `models_test.go` 的期望表，然后 bump 版本发一版。上游同步会覆盖它们，重放方式见
  `AGENTS.md` 的「上游同步」。
- **上游插件目录（`workbuddy/`、`qoderwork/`）其余内容保持上游原样**：更新用
  `git checkout upstream/main -- <id>` 覆盖，本仓库增量只落在根级文件（`registry.json`、
  `.github/workflows/build.yml`、`README.md`、`AGENTS.md`）；同步后重跑
  `go vet ./... && go test -race -count=1 ./...`（本机无 Go 用 `golang:1.26` 容器）。
  上游 `qwenwork/`、`traework/` 未纳入本仓库，需要时临时检出、手工构建上传。
- **`commandcode` 的 `api-keys` 为必填**：缺失时宿主报
  `plugin.register failed: api-keys: at least one key is required`，面板显示
  **未注册 / 未生效**——这是配置错误，不是加载或版本错误。
- **来源与再打包**：`commandcode` 是上游
  [`mczhoucn/commandcode`](https://github.com/mczhoucn/commandcode)
  （MIT）的再打包，改动范围见
  [commandcode/docs/PROVENANCE.md](commandcode/docs/PROVENANCE.md)。
