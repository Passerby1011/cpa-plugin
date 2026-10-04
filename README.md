# CPA 插件仓库

[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 插件集合。当前提供
**WorkBuddy Global**（OAuth Provider）与 **CommandCode Go/GOAT/Pro/Max**
（套餐 Provider + 配额页）。

## 插件

| ID | 说明 | 源码 |
|---|---|---|
| `workbuddy-global` | WorkBuddy 国际版（`workbuddy.ai`）OAuth、动态模型、executor、积分生命周期、积分面板 | [workbuddy-global/](workbuddy-global/) |
| `commandcode-go-cliproxyapi` | CommandCode Go/GOAT/Pro/Max 套餐单一 provider（`commandcode/` 前缀）：OpenAI/Anthropic/Responses 三协议、推理保真、Key 池调度、配额页 | [commandcode-go-cliproxyapi/](commandcode-go-cliproxyapi/) |

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

CI：push / PR 全量构建（只出 artifacts）；tag `<id>-v*`（如 `workbuddy-global-v0.11.0`）
或 dispatch 触发**该插件独立版本**的 Release。

## 安装（linux/amd64 示例）

```bash
# 从 Release 下载
unzip workbuddy-global_0.11.0_linux_amd64.zip
# 扁平 plugins 目录（常见 docker 挂载）
cp workbuddy-global.so /path/to/cliproxyapi/plugins/workbuddy-global.so
# 或平台子目录布局
# mkdir -p plugins/linux/amd64 && cp workbuddy-global.so plugins/linux/amd64/
```

```yaml
plugins:
  enabled: true
  # 必须写绝对路径：相对路径按「进程工作目录」解析（官方镜像里是 /CLIProxyAPI），
  # 写 "plugins" 会解析成 /CLIProxyAPI/plugins 从而找不到 .so
  dir: "/path/to/cliproxyapi/plugins"
  configs:
    workbuddy-global:
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

添加后在商店 UI 安装/更新 **WorkBuddy Global** 或 **CommandCode Go/GOAT/Pro/Max**。

> **首次使用注意**：`registry.json` 里的 `artifacts` 由发版 CI 回填。在
> `workbuddy-global-v*` 首个 Release 产出之前，该字段为空数组，而 CPA 对
> `direct` 类型强制要求至少一个 artifact（`pluginstore/registry.go` 的
> `ValidateInstallPlan`），此时整个源会被判为无效并报
> `plugins[0]: direct install requires at least one artifact`。
> 先跑一次 Release（tag 或 workflow_dispatch）即可解除。
>
> 新增插件同理：`commandcode-go-cliproxyapi` 的 `artifacts` 在
> `commandcode-go-cliproxyapi-v0.1.1` 首个 Release 产出后由 CI 自动回填。

## 插件维护要点

- **每个插件完全自包含**：独立 `go.mod`（go 1.26）、`go.sum`、`VERSION`、
  `Makefile`、`README.md`/`README_CN.md`、`LICENSE`。
- **module 路径**统一为 `github.com/hex-ci/cpa-plugin/<id>`。
- **版本注入**：CI 用 `-ldflags "-X main.version=<version>"` 注入版本，插件在
  `main.go` 的 `version` 变量 + `init()` 里转发给内部包；`<id>/VERSION` 是版本源。
- **发版**：`make -C <id> tag`（读 `<id>/VERSION`）→ 推 tag → CI 出 7 平台产物 +
  回填 `registry.json`。
- **`commandcode-go-cliproxyapi` 的 `api-keys` 为必填**：缺失时宿主报
  `plugin.register failed: api-keys: at least one key is required`，面板显示
  **未注册 / 未生效**——这是配置错误，不是加载或版本错误。
- **来源与再打包**：`commandcode-go-cliproxyapi` 是上游
  [`mczhoucn/commandcode-go-cliproxyapi`](https://github.com/mczhoucn/commandcode-go-cliproxyapi)
  （MIT）的再打包，改动范围见
  [commandcode-go-cliproxyapi/docs/PROVENANCE.md](commandcode-go-cliproxyapi/docs/PROVENANCE.md)。
