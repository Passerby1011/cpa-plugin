# 分析：QoderWork 真·OAuth 登录（设备授权流程，替代 PAT）

> 2026-07-27 · 目标：CPA auth 页面点 QoderWork OAuth 卡片 → 浏览器授权 → 插件拿到
> device token（dt-/drt-），全程不需要用户手工创建/粘贴 PAT。

## 已验证的客户端流程（三源交叉确认）

### 来源
1. **自研脚本** `/root/qoder-register/qoder_device_oauth.py`（2026-07-22 实测跑通 Global，
   拿到 dt- token + drt- refresh_token，`device_oauth/cli_oauth_result.json` 为证）
2. **CN 客户端逆向** `/tmp/qw_extract/app/resources/asar_out/out/main/main.js`（官方桌面端
   同款实现，含 refresh 端点与常量）
3. **GitHub 旁证** OmniRoute issues（qoder OAuth 在其他项目同样工作）

### 流程（QoderWork 自定义设备授权，类 OAuth device flow + PKCE）

```
1. 插件生成 PKCE：verifier(64字符) + challenge = base64url(SHA256(verifier))
   另生成 nonce(uuid) + machine_id(uuid)

2. StartLogin 返回授权 URL（用户浏览器打开）：
   https://<WEBSITE>/device/selectAccounts
     ?challenge=<challenge>&challenge_method=S256
     &nonce=<nonce>&machine_id=<machine_id>
     &client_id=<CLIENT_ID>&redirect_uri=<REDIRECT_URI>

3. 用户在浏览器登录/选账号/点 Continue 授权

4. 插件 PollLogin 轮询：
   GET https://<OPENAPI>/api/v1/deviceToken/poll
       ?nonce=<nonce>&verifier=<verifier>&challenge_method=S256
   - 404/202 → pending（继续等）
   - 200 → {token:"dt-...", refresh_token:"drt-...", user_id,
            expires_in(ms), refresh_token_expires_in(ms), expires_at, ...}

5. Refresh（无需 PAT，drt- 一年有效）：
   POST https://<OPENAPI>/api/v1/deviceToken/refresh
   body: {"refresh_token": "drt-..."}
   响应: {device_token|token, refresh_token, expires_at/expires_in, ...}
```

### CN 版常量（从 CN 客户端 main.js 提取）

| 常量 | CN 值 | Global 值（旧脚本） |
|---|---|---|
| WEBSITE_DOMAIN | `qoder.com.cn` | qoder.com |
| OPENAPI_DOMAIN | `openapi.qoder.com.cn` | openapi.qoder.sh |
| CLIENT_ID (prod) | `1c5e33e1-364d-4ce6-b02c-acaa81274a5c` | 同（全球共用 prod client） |
| REDIRECT_URI | `qoder-work-cn://` | `qoder-work://` |

token 生命周期（实测 Global 样本）：dt- ≈30 天，drt- ≈1 年。
**比 PAT 路线更健康**：不再需要 jt-/jrt- 两级（24h/48h）刷新，keepalive 压力骤减。

## 插件侧改造方案

### oauth.go
1. `handleStartLogin`：生成 PKCE + nonce/machine_id → 存 loginCtx(state→{verifier,nonce,
   machineID,expires}) → 返回 `AuthLoginStartResponse{URL: selectAccounts URL, State, Metadata{logo}}`
2. `handlePollLogin`：按 state 取回 verifier → GET deviceToken/poll
   - 404/202/网络错 → `AuthLoginStatusPending`
   - 200 → 用 dt- 调 `/api/v1/userinfo` 取 uid/nickname → buildStoredAuth → Success
3. `handleRefreshAuth`：POST deviceToken/refresh {refresh_token: drt-}
   - 成功 → 更新 AccessToken/RefreshToken/ExpiresAt
   - 失败 → **不再回退 PAT**（dt- 路线）；但为兼容存量 PAT 导入的账号
     （PersonalToken 非空），保留 PAT→jobToken 旧路径作为 fallback

### storedAuth 兼容
现有字段够用：AccessToken=dt-（或 jt-），RefreshToken=drt-（或 jrt-），
PersonalToken=PAT（仅存量账号有）。refresh 分流：
- PersonalToken 非空 → 旧 PAT/jobToken 两级刷新
- 否则 → deviceToken/refresh

**风险点（需在实现时验证）**：COSY 签名 cosySessionFor 当前吃 jt-；
dt- 是否同样被网关接受需要实测（客户端就是用 dt- 走 gateway 的，理论上可以，
因为这就是官方客户端的调用方式）。若不行，则 dt- 仅用于 openapi 业务端点，
chat 仍需 jt- —— 这种情况需要在 login 完成后再做一次 dt- → jobToken 交换
（如果有该端点；逆向未见到，优先实测 dt- 直接 chat）。

### keepalive.go
22:00 刷新逻辑改走 handleRefreshAuth 同一入口（已统一）。

### 面板
PAT 导入保留（存量账号+应急），文案补充"推荐 OAuth 登录"。

## 验证计划
1. 编译部署后，用 CPA management API 触发 StartLogin → 拿 URL
2. 浏览器（已登录 qoder.com.cn 的账号）打开 URL 授权
3. PollLogin 应返回 Success + auth 文件落盘（type=QoderWork）
4. curl chat 实测 dt- 是否可直接推理（关键未知数）
5. 强制 expiresAt 过期触发 refresh，确认 deviceToken/refresh 通路

---

## 本地测试结论（2026-07-27，全部实测通过）

脚本：`/tmp/qw_oauth_local_test.py`（login+refresh）+ `/tmp/qw_chat_local_test.py`（COSY chat）。
token 只存 /tmp，CPA 不接触，排除宿主干扰。

| 测试 | 结果 |
|---|---|
| OAuth 设备授权（PKCE + poll） | ✅ dt- + drt- 签发，user_id 正确 |
| dt- userinfo / quota/usage | ✅ 200（dt- 直接跑业务端点，无需 jt-） |
| **deviceToken/refresh** | ✅ drt- 换出**新 dt- + 新 drt-**（旋转式），新 dt- 立即可用 |
| **dt- + COSY chat** | ✅ 200，3.2s，真实流式回复 |

**关键事实**：
1. dt- 与 jt- 走完全相同的 COSY 签名链路（`security_oauth_token` 字段填什么用什么），
   chat 路径零改动。
2. **refresh 响应形状与 poll 不同**：`{"device_token":..., "refresh_token":...,
   "expires_at":"RFC3339", "refresh_token_expires_at":"RFC3339"}` —— 无 expires_in(ms)、
   token 键名是 `device_token` 不是 `token`。插件解析必须兼容两种形状
   （v0.2.3 已修 deviceExpiryUnix：expires_in → expires_at RFC3339 → 默认 30d）。
3. refresh 是**旋转式**（drt- 也换新），必须回写新 drt-，否则下次 refresh 失败。
4. dt- ≈30 天，drt- ≈1 年。

## 已发现并修复的坑（v0.2.0 → v0.2.3）

| 版本 | 坑 | 修复 |
|---|---|---|
| v0.2.0 | refresh 按 PersonalToken 是否存在路由 → 共存时 drt- 被劫持到 jobToken/refresh（400），PAT fallback 把 dt-/drt- 覆盖成 jt-/jrt- | v0.2.2：按 token 前缀路由（drt-→deviceToken/refresh，jrt-→jobToken/refresh） |
| v0.2.0 | OAuth 登录落盘时 PersonalToken 清空 → 破坏共存 | v0.2.2：existingPATForUID 保留同 uid 已有 PAT |
| v0.2.2 | deviceToken/refresh 响应只有 expires_at(RFC3339) 无 expires_in → expiresAt 算错 | v0.2.3：deviceExpiryUnix 兼容两种形状 |

## 待解决（plan 编写中）

host 15 分钟 auto-refresh 是否走插件 handleRefreshAuth 之外的 conductor 内部路径
（疑似直接用文件里的 PAT 撞 jobToken/exchange）——子 agent 深扫后定方案。
