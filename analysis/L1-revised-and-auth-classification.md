# 分析：L1 修订（删除 OAuth 登录卡片）+ auth 分类串扰修复

> 2026-07-27 · Hermes 主进程分析（未改代码）

## 背景

用户反馈两个问题：

1. **L1 未达预期**：v0.1.12 把 StartLogin 指向面板 PAT 表单，但 CPA 前端仍然渲染
   "通过 QoderWork 插件的 OAuth 流程登录"卡片。用户要求：**直接不展示这张卡片**，
   但不得影响 ①oauth 别名（oauth-model-alias）②oauth 模型禁用（excluded-models）。
2. **auth 分类串扰**：workbuddy/qoderwork 各自插件面板都正确只显示自己账号，
   但 CPA 的 auth 文件管理页分类显示"30 个 QoderWork 文件"，WorkBuddy 分类消失。

## 问题 2 根因（分类串扰）—— 高置信度

CPA 宿主侧解析链路（sdk/pluginhost + internal/watcher/synthesizer/file.go）：

```
watcher 扫到 auth 文件
  → synthesizeFileAuths()：provider = 文件 JSON 的 "type" 字段（小写）
  → 若 type 为空：provider=""，遍历所有插件的 AuthProvider.ParseAuth，
    **第一个返回 Handled=true 的插件拥有此文件**
```

关键事实（已验证）：

- WorkBuddy 的 29 个 auth 文件**没有 "type"/"provider" 字段**
  （legacy 格式：`{"auth":{...},"account":{...}}`）。
- QoderWork 的文件**有** `"type":"qoderwork"`。
- WorkBuddy 和 QoderWork 的 `parseStored()` **完全同构**（嵌套 `{"auth":{accessToken...}}`，
  只检查 accessToken 非空，**不检查 type/provider/domain**）。
- 对无 type 的 WorkBuddy 文件，宿主按插件注册顺序轮询 ParseAuth：
  - WorkBuddy 先注册 → 认领 ✓（正常情况）
  - **QoderWork 先注册（或某次重载后顺序变化）→ QoderWork 的 parseStored
    成功解析 WorkBuddy 文件 → Handled=true → 文件被标为 provider=QoderWork** ✗

实测验证：`workbuddy-00e26541...json` 满足 QoderWork parseStored 的全部条件
（嵌套 auth + accessToken 非空），QoderWork 会认领它。

这就是"30 个文件全部归到 QoderWork、WorkBuddy 分类消失"的根因：
**QoderWork 的 ParseAuth 缺少 provider 身份校验，把 WorkBuddy 文件认领走了。**

WorkBuddy 文件 domain=`www.codebuddy.cn`，QoderWork domain=`qoder.com.cn`，
天然有区分特征。

### 修法（参考社区惯例 + 对称防御）

在 QoderWork 的 `handleParseAuth` / `parseStored` 加**所有权校验**：

1. 文件带 `"type"` 字段时：仅当 `type == "qoderwork"` 才 Handled=true，
   其他（如 "WorkBuddy"）直接 Handled=false。
2. 文件无 type（legacy）：看 `auth.domain`——
   `qoder.com.cn` / `qoder.com` → 认领；`codebuddy.cn` / `workbuddy.ai` → 拒绝。
3. 两者都无 → 拒绝（Handled=false，保守不抢）。

同时建议 WorkBuddy 侧也加对称校验（但它不归本 LOOP 边界管，只提建议，
不动它源码——本 LOOP 只改 QoderWork，修完即可解决，因为 QoderWork 拒绝后
宿主会轮询到 WorkBuddy 自己认领）。

## 问题 1（OAuth 卡片）—— 机制约束

CPA 前端卡片来自 `AuthLoginStartResponse`（StartLogin 返回 URL+State+Metadata）。
SDK `pluginapi.AuthProvider` 接口**没有**"隐藏卡片"标志位；宿主
`ServePluginAuthURL` 只对注册了 AuthProvider 的 provider 提供 `/v0/management/<p>-auth-url`。

**结论：删除卡片 = 移除插件的 AuthProvider 能力注册中的 login 部分。**
但必须保住：
- `ParseAuth`（watcher 认领文件 → 分类）— 也是问题 2 的修复点，必须保留
- `RefreshAuth`（token 刷新 / keepalive）— 必须保留
- oauth-model-alias / excluded-models：这两者是宿主从 auth 文件 metadata
  读取（`extractOAuthModelAliasesFromMetadata` / `extractExcludedModelsFromMetadata`，
  file.go:95-96,157-158），**与 login 卡片无关**，只要 ParseAuth 仍在就继续生效。

### 修法

AuthProvider 是接口：`Identifier/ParseAuth/StartLogin/PollLogin/RefreshAuth`。
宿主通过 `Capabilities.AuthProvider != nil` 判断有无 auth provider。
无法"只删 StartLogin/PollLogin 而保留接口"（接口是整体）。

方案：让 `StartLogin` 返回一个**明确的"请走面板"错误/提示**而非可点击卡片——
但用户要的是"不展示"。查宿主：无 AuthProvider 则无卡片入口。
所以需要：**把 login 能力从注册中摘除，但保留 ParseAuth+RefreshAuth。**

查 pluginapi：AuthProvider 接口四方法必须全实现。若插件不注册 AuthProvider
capability，则 ParseAuth/RefreshAuth 也不会被调用——不行。

→ 需要进一步确认宿主是否支持"AuthProvider 存在但 StartLogin 直接报错时不渲染卡片"，
或前端是否有隐藏开关。实施前先查宿主 ServePluginAuthURL 的错误路径与前端行为，
取最小可行：StartLogin 返回 error，前端应显示错误而不是卡片（待实测）。

## 实施顺序

1. 先修问题 2（parseStored/handleParseAuth 所有权校验）——纯后端，收益明确。
2. 再查 AuthProvider 摘除对 RefreshAuth/keepalive 的影响，定问题 1 方案。
