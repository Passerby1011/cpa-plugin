# QoderWork CN — 完整逆向与对接知识库

> **整理时间：** 2026-07-26 12:10
> **状态：** 全部经实测验证，端到端 200 OK 打通。
> **目标读者：** 下次写 CPA 插件 / 任何对接 QoderWork CN 的项目。

---

## 0. TL;DR

| 项 | 结论 |
|---|---|
| 可用凭证 | **PAT (`pt-`)**（网页端创建，长期有效） |
| 短期 token | `jt-`（24h）+ `jrt-`（48h refresh），PAT 换来 |
| 推理端点 | `gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation` |
| 签名算法 | **COSY**：RSA + AES-128-CBC + MD5，纯软件可实现 |
| Body 编码 | **QoderEncoding**：base64 + 自定义字母表 + 三段重排 |
| 模型清单 | `GET /algo/api/v2/model/list?Encode=1` + COSY 签名 |
| 响应 model 字段 | **恒为 `auto`**（服务端硬编码，不代表路由失败） |
| 不需要 | ❌ WASM ❌ qodercli 子进程 ❌ device_token 直签 ❌ gRPC |

---

## 1. 凭证体系

### 1.1 PAT（Personal Access Token）

**格式：** `pt-{24字符}_{uuid}`，例：`pt-XAoLmz72sTkTDxNEDs04PPXQ_019f9bfb-8bc3-7f15-9ac3-b2bd8f2ea9cc`

**创建（只能在主站 web session，openapi 域无此端点）：**

```http
POST https://qoder.com.cn/api/v1/me/personal-access-tokens
Cookie: <web session>
Content-Type: application/json
X-Requested-With: XMLHttpRequest

{"name":"cpa-auto","expires_at":<unix_ms>}

→ 201 {"token_id":"...","name":"cpa-auto","token":"pt-...","created_at":...,"expires_at":...,"is_expired":false}
```

- ❌ `device_token (dt-)` 对这个端点无效（401/CSRF）
- ❌ `openapi.qoder.com.cn` 上无此端点（404）
- ✅ expires_at 自填，可填 100 年
- ✅ 自动化脚本：`/root/qoderwork/scripts/qoder_cn_pat_login.py`
  - 首次：`--phone 18278724616` → 阿里云 SSO 短信 → 建 PAT
  - 后续：`--reuse-state /tmp/qw_web/storage_loggedin.json` → 免短信

**当前 PAT 存：** `/root/qoderwork_tokens.json`

### 1.2 PAT → jobToken 交换

```http
POST https://openapi.qoder.com.cn/api/v1/jobToken/exchange
Content-Type: application/json

{"personal_token": "pt-xxx"}

→ 200 {
  "token": "jt-xxx",                          // 24h
  "refresh_token": "jrt-xxx",                 // 48h
  "expires_at": "2026-07-27T01:13:24Z",
  "expires_in": 86400000,                     // ms
  "refresh_token_expires_at": "2026-07-28T01:13:24Z",
  "refresh_token_expires_in": 172800000       // ms
}
```

**refresh：**

```http
POST https://openapi.qoder.com.cn/api/v1/jobToken/refresh

{"refresh_token": "jrt-xxx"}
→ 同 exchange 响应格式
```

**规则：**
- PAT 只能用于换 jobToken，**不是 Bearer**（拿来调业务 API 401 TOKEN_EXPIRE）
- jt- 24h 有效，jrt- 48h 内可 refresh
- 超过 48h 用 PAT 重新换（无感）

### 1.3 凭证能力矩阵

| 能力 | PAT (pt-) | jobToken (jt-) | device_token (dt-) |
|---|---|---|---|
| 换 jobToken | ✅ | — | — |
| 推理对话 | ❌ | ✅ + COSY 签名 | ❌ |
| 签到 | ❌ | ✅ Bearer | ✅ Bearer |
| 积分查询 | ❌ | ✅ Bearer | ✅ Bearer |
| 用户信息 | ❌ | ✅ Bearer | ✅ Bearer |
| 模型清单 | ❌ | ✅ + COSY 签名 | ❌ |
| 有效期 | 自填（可 100 年） | 24h | 30d |

---

## 2. 业务 API（jt- Bearer，无需签名）

Base: `https://openapi.qoder.com.cn`

```
GET  /api/v1/userinfo                            → 用户信息
GET  /api/v2/quota/usage                         → 积分（含 total/used/remaining）
GET  /api/v2/user/plan                           → 计划
GET  /sash/api/v1/me/daily-check-in/status       → 签到状态 (CLAIMABLE/CLAIMED)
POST /sash/api/v1/me/daily-check-in/claim        → 签到 (+100/天)
POST /sash/api/v1/me/pro-upgrade/claim           → Pro Upgrade (+1800 一次性)
GET  /sash/api/v1/me/invitationCode              → 邀请码
GET  /algo/api/v3/service/region/endpoints       → 区域节点（响应是 QoderEncoding）
```

**统一 Header：** `Authorization: Bearer {jt-}`

---

## 3. COSY 签名（纯软件，Go/Python/Java 都可实现）

### 3.1 算法

```python
# 1) 每次进程启动生成 session（每账号一个）
temp_key = uuid4().hex[:16].encode()                 # 16 字节 ASCII
cosy_key = base64(RSA_PKCS1v15_encrypt(server_pubkey, temp_key))
identity = {
    "name": name, "aid": uid, "uid": uid,
    "yx_uid": "", "organization_id": "", "organization_name": "",
    "user_type": "personal_professional_trial",       # 或 personal_standard
    "security_oauth_token": jt,
    "refresh_token": jrt,
}
info = base64(AES_128_CBC(json_sorted_compact(identity), key=temp_key, iv=temp_key))
machine_id = uuid4()                                  # 持久化同账号
machine_token = urlsafe_b64_no_pad((uuid4()+uuid4())[:50])
machine_type = uuid4().hex[:18]

# 2) 每次请求
payload = {"cosyVersion":"0.1.43","ideVersion":"","info":info,"requestId":uuid4(),"version":"v1"}
payload_b64 = base64(json_sorted_compact(payload))
path_sig = url.path.removeprefix("/algo")
date = str(int(time.time()))
sig = md5_hex(f"{payload_b64}\n{cosy_key}\n{date}\n{body}\n{path_sig}")
Authorization = f"Bearer COSY.{payload_b64}.{sig}"
```

### 3.2 RSA 公钥（硬编码）

```
-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----
```

### 3.3 完整 Headers（15 个，全部必带）

```
cosy-data-policy: AGREE
content-type: application/json
cosy-machinetype: {machineType}
cosy-clienttype: 5
cosy-date: {unix_ts}
cosy-user: {uid}
cosy-key: {cosyKey}
accept: text/event-stream              (SSE) 或 application/json
cosy-clientip: 169.254.198.161
authorization: Bearer COSY.{payload_b64}.{sig}
accept-encoding: identity
cosy-version: 0.1.43
cosy-machineid: {machineId}
cosy-machinetoken: {machineToken}
login-version: v2
user-agent: Go-http-client/2.0
cache-control: no-cache                (仅 SSE)

# 推理时附加：
x-model-key: {model_key}
x-model-source: system
```

---

## 4. QoderEncoding（body 编码）

```python
CUSTOM_ALPHABET = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
STD_ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
CUSTOM_PAD = '$'

def encode(plain: bytes) -> str:
    std = base64.b64encode(plain).decode()
    n = len(std); a = n // 3
    rearranged = std[n-a:] + std[a:n-a] + std[:a]
    return ''.join(CUSTOM_ALPHABET[STD_ALPHABET.index(c)] if c != '=' else '$' for c in rearranged)

def decode(encoded: str) -> bytes:
    mapped = ''.join(STD_ALPHABET[CUSTOM_ALPHABET.index(c)] if c != '$' else '=' for c in encoded)
    n = len(mapped); a = n // 3
    std = mapped[n-a:] + mapped[a:n-a] + mapped[:a]
    return base64.b64decode(std)
```

---

## 5. 推理端点

### 5.1 agent_chat_generation（CN 唯一对话端点）

```
POST https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation
     ?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1

Headers: 全部 15 个 cosy-* + x-model-key + x-model-source
Body: qoderEncode(json(baseprompt_template))
```

**响应：** SSE 流，每行：

```
data:{"headers":{"Content-Type":["application/json"]},"body":"<json-string>","statusCodeValue":200,"statusCode":"OK"}
```

**`body` 字段是 JSON 字符串**，需二次解析得到标准 OpenAI chunk：

```json
{"choices":[{"delta":{"content":"Hi"},"index":0}],"created":...,"id":"chatcmpl-...","model":"auto","object":"chat.completion.chunk"}
```

**结束：** `data:{"body":"[DONE]"}` + `event:finish\n\ndata:{"firstTokenDuration":1053,"totalDuration":1114,"serverDuration":81}`

### 5.2 请求 Body 模板

**模板：** `/tmp/cpa-plugin/qoderwork/baseprompt.json`（从 cubk1/qoder2api 抄的 baseprompt.json，占位符已清）

每次请求覆盖：

```python
base["request_id"] = base["chat_record_id"] = uuid4()
base["request_set_id"] = uuid4()
base["session_id"] = uuid4()
base["stream"] = True
base["aliyun_user_type"] = "personal_professional_trial"
base["agent_id"] = "agent_common"
base["model_config"]["key"] = model_key              # ⚠️ 必须改这里
base["chat_context"]["text"]["text"] = prompt
base["chat_context"]["extra"]["originalContent"]["text"] = prompt
base["chat_context"]["extra"]["modelConfig"]["key"] = model_key  # ⚠️ 也要改
base["messages"] = [system_from_template] + openai_messages     # 保留模板 system
base["business"]["id"] = uuid4()
base["business"]["begin_at"] = unix_ms
base["business"]["name"] = prompt[:30]
```

**模板自带：** 10657 tokens 的 system prompt（Qoder CLI 的提示词）。保留它服务器行为才正常。

---

## 6. 模型清单

### 6.1 拉取

```
GET https://gateway.qoder.com.cn/algo/api/v2/model/list?Encode=1
+COSY 签名（同推理）
→ 200 明文 JSON（**不是** QoderEncoding）
```

**实测产物：** `/root/qoderwork/models_list.json` (39898 bytes)

**结构：** `{"chat":[...], "experts":[...], "developer":[...], "assistant":[...], "qwake":[...], "inline":[...], "quest":[...], "qwork":[...], "byok_enterprise":[], "byok_teams":[]}`

### 6.2 chat scene 模型 key（已实测可用）

| key | display_name | price_factor | reasoning | vl |
|---|---|---|---|---|
| `auto` | Auto | 0.5 | ✓ | ✓ |
| `qmodel_preview` | **Qwen3.8-Max-Preview** | 0.05 | ✓ | ✓ |
| `qmodel_latest` | Qwen3.7-Max | 0.25 | ✓ | ✓ |
| `qmodel` | Qwen3.7-Plus | 0.1 | ✓ | ✓ |
| `q36fmodel` | Qwen3.6-Flash | 0.1 | ✓ | ✓ |
| `dmodel` | DeepSeek-V4-Pro | 0.5 | ✓ | ✓ |
| `dfmodel` | DeepSeek-V4-Flash | 0.1 | ✗ | ✓ |
| `gm51model` | GLM-5.2 | 0.6 | ✓ | ✓ |
| `kmodel` | Kimi-K2.7-Code | 0.3 | ✓ | ✓ |
| `mmodel` | MiniMax-M2.7 | 0.2 | ✗ | ✗ |

### 6.3 响应里 `"model":"auto"` 的迷思

**所有 key 请求的响应 model 字段都是 `auto`**，这不是路由失败，是**服务端硬编码的标识符**。

**判断真实路由看：**
- `qmodel_preview` / `auto`：id 是 `chatcmpl-{uuid}`，delta 含 `reasoning_content`
- `dmodel` (DeepSeek-V4-Pro)：id 是 `{uuid}`（**无 chatcmpl- 前缀**），含 `system_fingerprint`

**路由必须：**
- `x-model-key` header
- `model_config.key` body 字段
- **两个都得给，且 key 必须真实存在**

**错误 key 不报错**（如 `qwen3.8-max`），服务端默默走 auto——这是之前"qwen3.8-max 返回 auto"的根因。

---

## 7. 国际版 vs 国内版

| | intl (qoder.com) | CN (qoder.com.cn) |
|---|---|---|
| 主站 | qoder.com | qoder.com.cn |
| OpenAPI | openapi.qoder.sh | openapi.qoder.com.cn |
| Gateway | api3.qoder.sh / **api2-v2.qoder.sh** | gateway.qoder.com.cn |
| 推理路径 | `/model/v1/chat/completions` (OpenAI 格式) | `/algo/api/v2/service/pro/sse/agent_chat_generation` (agent 格式) |
| 认证 | `Bearer {jt-}` 直 bearer | COSY 签名 |
| 账号互通 | ❌ | ❌ |
| 模型清单 | ? | `GET /algo/api/v2/model/list` |
| gRPC | ✅（worker 首选） | ❌（只 HTTP agent） |

**证据：** worker runtime `KhI={prod:"api2-v2.qoder.sh",...}` 是全球统一 host，但 CN `jt-` 在 api2-v2 上 401，**区域账号隔离**。

---

## 8. 网页登录（PAT 创建）

**流程：**

```
qoder.com.cn/users/sign-in
  → 使用阿里云登录
  → account.aliyun.com/sso/login.htm (client_id=qoder)
  → iframe: passport.aliyun.com/havanaone/login/login.htm?appEntrance=qoder_sms
    → input[name=fm-sms-login-id]  填手机号
    → input[name=fm-agreement-checkbox]  勾选协议
    → a.send-btn-link  点击发验证码（⚠️ 不是 <button>）
    → input[name=fm-smscode]  填验证码
    → 提交
  → 回跳 qoder.com.cn（web session cookie 拿到）
```

**关键 CSS selector：**
- SMS iframe: 含 `appEntrance=qoder_sms` 的 frame
- 发送按钮：`a.send-btn-link`（不是 button）
- 协议勾选：`input[name='fm-agreement-checkbox']`（force=True check）

**Playwright 启动参数（本机 arm64 必须）：**
```python
["--no-sandbox","--disable-dev-shm-usage","--disable-gpu","--single-process","--no-zygote"]
```
缺 `--single-process` 会 page crashed。

---

## 9. 已废弃方案（不要回头）

### 9.1 WASM 签名 ❌

- 之前以为要用 `qoder_auth_wasm_bg.wasm` 的 `prepareInferRequest` 签请求
- **真相：** WASM 只用于 `decrypt_server_response` 和 httpdns（`get_httpdns_account_id`/`get_httpdns_secret_key`）
- 签名是 JS 重写的 RSA+AES+MD5（`encryptUserInfo` + `generateAuthToken` @ main.js:2614695）

### 9.2 qodercli 子进程 ❌

- 之前计划 spawn `qodercli --output-format stream-json` 通过 NDJSON 通信
- **真相：** 桌面端 WorkerTransport 在 Electron 进程内直签，**不 spawn CLI**；ProcessTransport 只是 fallback

### 9.3 device_token 直签 ❌

- `dt-` 喂 WASM 一直 401/500
- **真相：** 需要 PAT 换 `jt-`；`dt-` 只能刷 openapi 业务 API（quota/userinfo/签到），不能推理

### 9.4 "qwen3.8-max" 当模型 key ❌

- 服务端不认识这个 key，默默走 auto
- **真相：** 真实 key 是 `qmodel_preview`（Qwen3.8-Max-Preview）

---

## 10. 产物清单

### 10.1 关键文件

| 文件 | 用途 |
|---|---|
| `/root/qoderwork_tokens.json` | CN PAT + user_id + device_token |
| `/root/qoderwork/models_list.json` | 模型清单实测（39898 bytes） |
| `/root/qoderwork/scripts/qoder_cn_pat_login.py` | PAT 获取脚本（短信 SSO） |
| `/tmp/qw_web/storage_loggedin.json` | web session cookie（再造 PAT 免短信） |
| `/tmp/qw_web/qoder_chat_test.py` | Python 端到端验证脚本 |
| `/tmp/cpa-plugin/qoderwork/baseprompt.json` | 请求 body 模板 |
| `/tmp/cpa-plugin/qoderwork/sign.go` | Go COSY 签名实现（已测） |
| `/tmp/cpa-plugin/qoderwork/encoding.go` | Go QoderEncoding（已测） |
| `/tmp/cpa-plugin/qoderwork/body.go` | Go body 模板构造 |
| `/tmp/qw_extract/` | 桌面客户端解压（1.5G，参考） |

**注意：** `/tmp/cpa-plugin/qoderwork/` 只保留**已验证的库代码**（sign/encoding/body）。之前写的 main.go/auth.go/executor.go 已删——直接抄 WorkBuddy 的 RPC 层结构再改更安全，那次部署出错是 RPC 层返回空 `stream_id` 导致 scheduler 死循环。

### 10.2 开源参考

| 仓库 | 语言 | 价值 |
|---|---|---|
| `cubk1/qoder2api` | Java (73★) | COSY 签名完整实现 + baseprompt.json 模板 |
| `bzym2/QoderGateway` | Python (13★) | cubk1 的 Python 重写 + WebUI 多账号池 |

### 10.3 文档

- `STATUS.md` — 现状 + 实测记录
- `plan.md` — v4 实现计划
- `loop.md` — v3 Loop 计划
- `KNOWLEDGE.md` — 本文档

---

## 11. 实测记录

### 11.1 qwen3.8-max-preview 端到端（2026-07-26 11:39）

```
[*] AgentId=agent_common model=qmodel_preview
[*] STATUS: 200 OK

data:{"body":"{\"choices\":[{\"delta\":{\"content\":\"\",\"reasoning_content\":\"\",\"role\":\"assistant\"}}...],\"model\":\"auto\"}"}
data:{"body":"{\"choices\":[{\"delta\":{\"content\":\"Hello\"}}...]}"}
data:{"body":"{\"choices\":[{\"delta\":{\"content\":\"!\"}}...]}"}
data:{"body":"{\"choices\":[{\"delta\":{\"content\":\"\",\"finish_reason\":\"stop\"}}...]}"}
data:{"body":"{\"choices\":[],...,\"usage\":{\"completion_tokens\":2,\"prompt_tokens\":10657,...}}"}
data:{"body":"[DONE]"}
event:finish
data:{"firstTokenDuration":1096,"totalDuration":1155,"serverDuration":172}
```

**注意 `reasoning_content` 字段** —— Qwen3.8-Max 是 reasoning 模型。

### 11.2 dmodel (DeepSeek-V4-Pro) 对比

```
data:{"body":"{\"choices\":[{\"delta\":{\"reasoning_content\":\"\",\"role\":\"assistant\"}}...],
  \"id\":\"bb1ed1da-cc2f-4dd0-8727-3a67ed9be5de\",          ← 无 chatcmpl- 前缀
  \"system_fingerprint\":\"fp_9954b31ca7_prod0820_fp8_kvcache_20260402\"}"}
```

---

## 12. 下一步（写 CPA 插件时）

**教训（2026-07-26）：** 第一次部署失败——我自己写的 main.go RPC 层 `stream_id` 返回空导致 scheduler 死循环报错。**不要从零写 RPC 层**，直接**抄 WorkBuddy 的 main.go 整个文件**，然后只替换以下内容：

按这个顺序：

1. **Loop 1 骨架**：**完整复制 WorkBuddy main.go** → 改 providerName/logo/models → 删 CodeBuddy 特有逻辑（OAuth flow、CN/Global 切换）
2. **Loop 2 auth**：**保留 WorkBuddy auth.go 骨架**（storedAuth、authStorage、handleParseAuth 等），把 OAuth token exchange 换成 PAT → jobToken
3. **Loop 3 sign**：把 WorkBuddy 里所有 `backendHeaders()` 换成 QoderWork 的 COSY 签名（参考 `/tmp/cpa-plugin/qoderwork/sign.go`）
4. **Loop 4 executor**：**保留 WorkBuddy executor.go 骨架**（streamEmit/pumpUpstreamStream/aggregateCompletion），把 CodeBuddy SSE 格式换成 QoderWork 的 `data:{"body":"..."}` 嵌套格式
5. **Loop 5 checkin/credits**：抄 WorkBuddy 的 CN 签到逻辑，端点换成 QoderWork 的 `/sash/api/v1/me/daily-check-in/*`
6. **Loop 6 scheduler/lifecycle**：**完全照抄 WorkBuddy**，不改
7. **Loop 7 management/panel**：**完全照抄 WorkBuddy**，路由换 QoderWork 端点

**关键参考代码（已验证，直接可用）：**
- COSY 签名：`/tmp/cpa-plugin/qoderwork/sign.go`
- QoderEncoding：`/tmp/cpa-plugin/qoderwork/encoding.go`
- Body 模板构造：`/tmp/cpa-plugin/qoderwork/body.go`
- Python 参考：`/tmp/qw_web/qoder_chat_test.py`
- WorkBuddy 完整骨架：`/tmp/cpa-plugin/workbuddy-ref/`

---

**END**
