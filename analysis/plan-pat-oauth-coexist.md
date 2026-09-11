# PLAN：PAT 与 OAuth 共存兼容（L12 收官）

> 2026-07-27 · 基于子 agent 深扫（credential 接触点全清单）+ 本地实测结论
> 目标：PAT（pt-→jt-/jrt-）与 OAuth（dt-/drt-）两种登录方式完美共存于同一 auth 文件，
> 互相不覆盖、不劫持、不丢字段。

## 一、现状确认（已完成）

### 本地实测（全部 ✅，/tmp/qw_oauth_local_test.py + qw_chat_local_test.py）

| 项 | dt- 结果 | 结论 |
|---|---|---|
| OAuth 设备授权（PKCE+poll） | ✅ dt-+drt- 签发 | 真 OAuth 可用 |
| userinfo / quota/usage / user/plan | ✅ 200 | dt- 直接跑业务端点 |
| daily-check-in/status | ✅ 200（CLAIMED_TODAY） | dt- 签到端点兼容 |
| deviceToken/refresh | ✅ 旋转式新 dt-+drt- | 刷新通路正确 |
| COSY chat（gateway） | ✅ 200 真实回复 | dt- 与 jt- 同签名链路 |

**子 agent 标记的 P1「dt- 上游接受度未验证」全部实测排除**——billing/COSY/sash
端点对 dt-/jt- 一视同仁（`security_oauth_token` 填什么用什么）。

### 已修复（v0.2.0 → v0.2.3，线上运行中）

| 修复 | 版本 | 内容 |
|---|---|---|
| refresh 路由 | v0.2.2 | 按 token 前缀（drt-→deviceToken/refresh，jrt-→jobToken/refresh），不再按 PersonalToken 存在性 |
| OAuth 落盘保 PAT | v0.2.2 | `existingPATForUID`：OAuth 登录保留同 uid 已有 PAT |
| expires_at 解析 | v0.2.3 | `deviceExpiryUnix`：expires_in(ms) → expires_at(RFC3339) → 默认 30d |

## 二、扫描确认的共存模型

```
auth 文件（nested 形状，5 字段共存）：
{
  "type": "qoderwork", "provider": "qoderwork",
  "auth": {
    "accessToken":  "dt-..." 或 "jt-...",   // 当前活跃 token（按家族）
    "refreshToken": "drt-..." 或 "jrt-...", // 当前活跃 refresh（按家族）
    "personalToken": "pt-...",              // 长期兜底（导入即永久，两家族共存）
    "expiresAt": <unix>,
    "domain": "qoder.com.cn"
  },
  "account": {"uid": "...", "nickname": "..."}
}
```

**路由规则（单一权威）**：`RefreshToken` 前缀决定 refresh 走哪个端点：
- `drt-` → `POST /api/v1/deviceToken/refresh`（旋转式，回写新 dt-+drt-）
- `jrt-` → `POST /api/v1/jobToken/refresh`，失败 → `jobToken/exchange`（用 personalToken）
- `personalToken` 永远只做 fallback，**永不主动覆盖活跃 token**

## 三、待修清单（按严重度）

### P0-1：parseStored flat 分支丢 PersonalToken
- 位置：main.go:497-511
- 问题：flat 形状（CPAMP 早期 `qoderwork.json`）只搬 4 字段，personalToken 被丢
- 修法：flat struct 补 `PersonalToken string \`json:"personalToken"\`` 并写入 sa.Auth
- 影响：共存文件若以 flat 存盘，读回丢 PAT → OAuth 失败时无 fallback

### P0-2：persistAuthTokens 裸序列化丢顶层字段
- 位置：keepalive.go:162-176
- 问题：`json.Marshal(sa)` 直写，丢 type/provider/logo/disabled/note
- 后果：lifecycle 禁用（disabled:true）的账号被 22:00 keepalive 刷新后复活
- 修法：改用 `buildAuthFileJSON(sa, phys.Disabled, note, nil)`（disabled/note 从 phys.JSON 读）
- 注意：markSessionDead（同文件）已正确保字段，两函数行为需对齐

### P1-3：PAT 导入/PAT-paste 不对称保留 OAuth
- 位置：credits_handler.go handleImportPAT、oauth.go handlePollLogin PAT 路径
- 问题：对已有 OAuth 账号导入 PAT 时，buildStoredAuthFromJobToken 是全新 sa，
  覆盖 dt-/drt- 为 jt-/jrt-，且不保留 drt-（与 buildStoredAuthFromDeviceToken
  保留 PAT 的设计不对称）
- 决策（按「PAT 优先」用户原则）：**导入 PAT = 显式切换家族，允许覆盖**，
  但要在 panel UI 提示"导入 PAT 将替换当前 OAuth 凭证"。不加 existingOAuth 保留
  （避免永远甩不掉 OAuth）。PAT-paste 路径同理。
- 注：这与「共存」不矛盾——共存指 personalToken 字段与活跃 token 并存；
  家族切换是显式用户动作。

### P2-4：注释与文档更新
- storedTokens 注释补 dt-/drt- 30d/1y 家族说明（main.go:451-462）
- billing.go:17-18 注释 jt- → "jt- or dt-"
- KNOWLEDGE.md 补 OAuth 设备授权流程章节

### P2-5（可选）：routeRefresh 抽公共函数
- handleRefreshAuth 与 refreshCall 双份路由逻辑，抽 `routeRefresh(sa)` 防漂移
- 低优先（当前一致），可与 P0-2 同批做

## 四、不做的事（明确边界）

- ❌ 不改 chat 路径（dt-/jt- 同 COSY 链路，已实测）
- ❌ 不改 panel.html（JS 契约不变）
- ❌ 不动 host auto-refresh 的调用方式（宿主行为，插件侧 handleRefreshAuth 已正确路由）
- ❌ 不加 existingOAuthForUID（PAT 导入是显式切换，见 P1-3 决策）
- ❌ 不动 WorkBuddy 插件

## 五、实施顺序（每步 commit）

1. **P0-1** parseStored flat 补 PersonalToken → 单测：构造 flat JSON 验证不丢
2. **P0-2** persistAuthTokens 用 buildAuthFileJSON → 单测：禁用的账号 refresh 后仍 disabled
3. **P2-4** 注释更新（顺手）
4. 编译 v0.2.4 → 部署 → 实测：
   a. 当前 PAT 家族账号（jt-/jrt-）chat 不回归
   b. 手动 keepalive → 文件字段不丢（type/disabled/note 保留）
   c. OAuth 登录（需用户授权一次）→ 落盘 dt-/drt-+pt- 共存
   d. 手动 keepalive → **刷新后仍是 dt-/drt-**（deviceToken/refresh 通路）
   e. chat 用刷新后的新 dt- 仍 200
5. 更新 LOOP.md L12 勾完 + 推送

## 六、验证矩阵（交付前全绿才算完）

| 场景 | 预期 |
|---|---|
| 纯 PAT 账号 keepalive | jt-/jrt- 正常刷新，字段不丢 |
| 共存账号 keepalive（drt- 活跃） | deviceToken/refresh，dt-/drt- 旋转更新，pt- 不动 |
| 共存账号 keepalive（jrt- 活跃） | jobToken/refresh，jt- 更新，pt- 不动 |
| 禁用账号 keepalive | 刷新成功但 disabled 保持 true（P0-2 修复点） |
| flat 形状 auth 文件读回 | personalToken 不丢（P0-1 修复点） |
| OAuth 登录（有存量 PAT） | 落盘 dt-/drt-/pt- 三字段共存 |
| chat（任意家族） | 200 |
