# Changelog

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
