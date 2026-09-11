# QoderWork CPA 插件 — 现状 STATUS

> **更新时间：** 2026-07-26 02:00
> **目标：** 为 CPA 网关编写 `qoderwork` 插件，把 QoderWork（国内版）伪装成 openai-compatible provider。
> **核心结论（已端到端验证）：** ✅ **PAT → jobToken → 纯 Python COSY 签名 → SSE 对话 200 OK**。不需要 WASM、不需要 qodercli 子进程。

---

## 0. 一句话总结

| 项 | 状态 | 备注 |
|---|---|---|
| 推理端点 | ✅ 已打通 | `gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation` |
| 鉴权机制 | ✅ 已打通 | PAT → `jobToken/exchange` → `jt-` → COSY 签名 |
| COSY 签名 | ✅ Python 已通 | RSA + AES-128-CBC + MD5，**纯软件实现** |
| 端到端对话 | ✅ 200 OK | qwen3.8-max 实测返回 "Hi!"，SSE 流正常 |
| WASM 路线 | ❌ 已废弃 | 只用于解密响应 + httpdns，签名不需要 |
| qodercli 子进程 | ❌ 已废弃 | 桌面端 WorkerTransport 直签，不 spawn CLI |
| WorkBuddy 集成 | ⏸ 未开始 | 等 plugin 骨架 |
| CPA 部署 | ⏸ 未开始 | /root/cpa-manager-plus 还没装新插件 |

---

## 1. 认证链路（最终版，已实测）

### 1.1 PAT 获取（CN）

PAT 只能在**主站 web session** 里创建，openapi 域无此端点：

```
POST https://qoder.com.cn/api/v1/me/personal-access-tokens
Cookie: <web session>
Body: {"name":"cpa-auto","expires_at":<ms>}
→ 201 {"token":"pt-xxx_uuid","expires_at":...}
```

- ❌ device_token (`dt-`) 对这个端点无效（401/CSRF）
- ❌ openapi.qoder.com.cn 上无此端点（404）
- ✅ 脚本：`/root/qoderwork/scripts/qoder_cn_pat_login.py`
  - 首次：`--phone 18278724616` → 阿里云 SSO 短信 → 自动建 PAT
  - 后续：`--reuse-state /tmp/qw_web/storage_loggedin.json` → 免短信再造
- ✅ 当前 PAT 存 `/root/qoderwork_tokens.json`（100 年有效）

### 1.2 PAT → jobToken

```
POST https://openapi.qoder.com.cn/api/v1/jobToken/exchange
Body: {"personal_token":"pt-xxx"}
→ {"token":"jt-xxx"(24h), "refresh_token":"jrt-xxx"(48h)}
```

- PAT **不是** Bearer，只能用来换 jobToken
- `jt-` 过期用 `jrt-` refresh（48h 内），超过再换 PAT（100 年不用重登）

### 1.3 COSY 签名（纯软件，Python 已验证）

**完全不需要 WASM。** 以下算法在 cubk1/qoder2api（Java）和 bzym2/QoderGateway（Python）都有开源实现，已实测 CN 可用：

```python
# 1) 每次会话生成 16 字节随机 temp_key
temp_key = uuid4().hex[:16].encode()

# 2) RSA 加密 temp_key → cosy_key (硬编码公钥，见 §1.4)
cosy_key = base64( RSA_PKCS1v15_encrypt(server_pubkey, temp_key) )

# 3) AES-128-CBC 加密 identity JSON → info
identity = {name, aid, uid, yx_uid:"", organization_id:"", organization_name:"",
            user_type:"personal_professional_trial",
            security_oauth_token: jt-, refresh_token: jrt-}
info = base64( AES_CBC_encrypt(json(identity), key=temp_key, iv=temp_key) )

# 4) 构造 Bearer COSY
payload = {cosyVersion:"0.1.43", ideVersion:"", info, requestId:uuid, version:"v1"}
payload_b64 = base64(json(payload, sorted_keys))
path_sig = url.path.removeprefix("/algo")
sig = MD5(f"{payload_b64}\n{cosy_key}\n{unix_ts}\n{body}\n{path_sig}")
Authorization = f"Bearer COSY.{payload_b64}.{sig}"
```

**Headers（15 个，缺一不可）：**
```
cosy-data-policy: AGREE
content-type: application/json
cosy-machinetype: <18hex>
cosy-clienttype: 5
cosy-date: <unix_ts>
cosy-user: <uid>
cosy-key: <cosy_key>
accept: text/event-stream
cosy-clientip: 169.254.198.161
authorization: Bearer COSY.<payload_b64>.<sig>
accept-encoding: identity
cosy-version: 0.1.43
cosy-machineid: <uuid>
cosy-machinetoken: <urlsafe_b64_50chars>
login-version: v2
user-agent: Go-http-client/2.0
x-model-key: qwen3.8-max         # 路由到目标模型
x-model-source: system
cache-control: no-cache          # SSE only
```

**Body 编码（QoderEncoding）：**
```python
# 自定义 base64 字母表 + 三段重排 + $ 作为 padding
CUSTOM_ALPHABET = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
CUSTOM_PAD = '$'
encoded = rearrange(base64(json_body)) mapped to custom alphabet
```

**Body 模板：** `/tmp/qw_web/baseprompt_clean.json`（cubk1 仓库的 baseprompt.json，占位符已替换）。关键字段：
- `request_id` / `chat_record_id` / `request_set_id` / `session_id`: UUID
- `model_config.key`: 模型名（`qwen3.8-max` 等）
- `chat_context.text.text` / `chat_context.extra.originalContent.text`: 用户 prompt
- `messages`: OpenAI 格式 [{role, content}]
- `aliyun_user_type`: `personal_professional_trial` (CN trial) / `personal_standard`
- `session_type`: `qodercli`
- `agent_id`: `agent_common`

### 1.4 RSA 公钥（硬编码）

```
-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----
```

### 1.5 推理端点

```
POST https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation
     ?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1
```

响应：SSE 流 `data: {"body":"<json-string>","statusCodeValue":200}`，body 内嵌标准 OpenAI chunk JSON。

---

## 2. 实测记录（2026-07-26 01:37）

```
[*] realm=cn host=https://gateway.qoder.com.cn model=qwen3.8-max
[*] body plain bytes=45602 encoded=59980
STATUS: 200

data:{"body":"{\"choices\":[{\"delta\":{\"content\":\"\",\"role\":\"assistant\"}}...}"}
data:{"body":"{\"choices\":[{\"delta\":{\"content\":\"Hi\"}}...}"}
data:{"body":"{\"choices\":[{\"delta\":{\"content\":\"!\"}}...}"}
data:{"body":"{\"choices\":[{\"delta\":{\"content\":\"\",\"finish_reason\":\"stop\"}}...}"}
data:{"body":"{\"choices\":[],...,\"usage\":{\"completion_tokens\":2,\"prompt_tokens\":10657,...}}"}
data:{"body":"[DONE]"}
event:finish
data:{"firstTokenDuration":1053,"totalDuration":1114,"serverDuration":81}
```

**验证脚本：** `/tmp/qw_web/qoder_chat_test.py --realm cn --model qwen3.8-max`

---

## 2.5 模型路由真相（2026-07-26 11:39 深挖）

### 2.5.1 模型清单端点（已拿到）

```
GET https://gateway.qoder.com.cn/algo/api/v2/model/list?Encode=1
+COSY 签名（同推理）
→ 200 明文 JSON（**不是** QoderEncoding，虽然 URL 带 ?Encode=1）
```

**产物：** `/root/qoderwork/models_list.json` (39898 bytes)

**结构：** `{"chat":[...], "experts":[...], "developer":[...], "assistant":[...], "qwake":[...], "inline":[...], "quest":[...], "qwork":[...], "byok_enterprise":[], "byok_teams":[]}`

### 2.5.2 模型 key 全表（chat scene）

| key | display_name | price_factor | reasoning | vl | 备注 |
|---|---|---|---|---|---|
| `auto` | Auto | 0.5 | ✓ | ✓ | 默认 |
| `qmodel_preview` | **Qwen3.8-Max-Preview** | 0.05 | ✓ | ✓ | **真正的 qwen3.8-max** |
| `qmodel_latest` | Qwen3.7-Max | 0.25 | ✓ | ✓ | |
| `qmodel` | Qwen3.7-Plus | 0.1 | ✓ | ✓ | |
| `q36fmodel` | Qwen3.6-Flash | 0.1 | ✓ | ✓ | |
| `dmodel` | DeepSeek-V4-Pro | 0.5 | ✓ | ✓ | |
| `dfmodel` | DeepSeek-V4-Flash | 0.1 | ✗ | ✓ | |
| `gm51model` | GLM-5.2 | 0.6 | ✓ | ✓ | |
| `kmodel` | Kimi-K2.7-Code | 0.3 | ✓ | ✓ | |
| `mmodel` | MiniMax-M2.7 | 0.2 | ✗ | ✗ | |

### 2.5.3 响应里 `"model":"auto"` 的迷思

**所有 key 请求的响应 model 字段都是 `auto`**，这不是"路由失败 fallback"，是**服务端硬编码的标识符**。判断真实路由看：

1. **响应格式差异：**
   - `qmodel_preview` (Qwen3.8-Max): id 是 `chatcmpl-{uuid}`，delta 含 `reasoning_content`
   - `dmodel` (DeepSeek-V4-Pro): id 是 `{uuid}`（**无 chatcmpl- 前缀**），delta 含 `reasoning_content` + `system_fingerprint`
   - `auto`: id 是 `chatcmpl-{uuid}`，delta 含 `reasoning_content`
2. **不同 key 后端不同**，路由靠 `x-model-key` header + `model_config.key` body 字段（**两个都得给**）
3. **错误 key 不报错**（如 `qwen3.8-max` 这种不存在 key），服务端默默走 auto

### 2.5.4 国际版 vs 国内版推理端点

| | intl (qoder.com) | CN (qoder.com.cn) |
|---|---|---|
| 推理 host | `api2-v2.qoder.sh` / `api3.qoder.sh` | `gateway.qoder.com.cn` |
| 模型对话路径 | `/model/v1/chat/completions` (OpenAI 格式) | `/algo/api/v2/service/pro/sse/agent_chat_generation` (agent 格式) |
| 认证 | `Authorization: Bearer {jt-}` 直 bearer | COSY 签名 |
| 账号互通 | ❌ 不互通 | ❌ 不互通 |
| 模型清单 | ? | `GET /algo/api/v2/model/list?Encode=1` |

**关键证据：** worker runtime 里 `KhI={prod:"api2-v2.qoder.sh",...}` 是**全球统一**模型服务器，但 CN 的 `jt-` 在 api2-v2 上 401，证明**区域账号隔离**。

### 2.5.5 gRPC vs HTTP

worker 里模型对话**首选 gRPC**（`GrpcTransport`），HTTP `/model/v1/chat/completions` 是 fallback。CN 没暴露 gRPC，只能用 agent_chat_generation。

---

## 3. 已废弃方案（不要回头）

### 3.1 WASM 签名路线 ❌

- 之前以为要用 `qoder_auth_wasm_bg.wasm` 的 `prepareInferRequest` 签请求
- **真相：** WASM 在桌面端只用于 `decrypt_server_response` 和 httpdns（`get_httpdns_account_id`/`get_httpdns_secret_key`）
- 签名完全是 JS 里重写的 RSA+AES+MD5（`encryptUserInfo` + `generateAuthToken` @ main.js:2614695）
- `/tmp/wasm-poc/` 已删（trash），`/tmp/qw_extract/app/resources/qoder-auth-wasm/` 保留仅参考

### 3.2 qodercli 子进程路线 ❌

- 之前计划 spawn `qodercli --output-format stream-json` 通过 NDJSON 通信
- **真相：** 桌面端 WorkerTransport 直接在 Electron 进程内签名发请求，**不 spawn CLI**；ProcessTransport 只是 fallback
- `/root/.qodersec`（133M npm 装的 qodercli/qodersec）已 trash
- `/tmp/package`（CN CLI bundle）已 trash

### 3.3 device_token 直签 ❌

- `dt-` token 当 `security_oauth_token` 喂 WASM 一直 401/500
- **真相：** 需要先用 PAT 换 `jt-` 才行；`dt-` 只能刷 openapi 业务 API（quota/userinfo/签到），不能推理

---

## 4. 项目文件清单

### 4.1 关键资产（保留）

| 文件 | 用途 |
|---|---|
| `/root/qoderwork_tokens.json` | CN PAT + user_id + device_token |
| `/root/qoderwork/scripts/qoder_cn_pat_login.py` | PAT 获取脚本（短信 SSO） |
| `/tmp/qw_web/storage_loggedin.json` | qoder.com.cn web session cookie（再造 PAT 免短信） |
| `/tmp/qw_web/qoder_chat_test.py` | **端到端对话验证脚本**（Python COSY 签名） |
| `/tmp/qw_web/baseprompt_clean.json` | 请求 body 模板 |
| `/tmp/qw_extract/` | 桌面客户端解压（1.5G，保留参考 main.js） |

### 4.2 开源参考实现

| 仓库 | 语言 | 价值 |
|---|---|---|
| `cubk1/qoder2api` | Java | COSY 签名完整实现，baseprompt.json 模板 |
| `bzym2/QoderGateway` | Python | cubk1 的 Python 重写 + WebUI 多账号池 |
| `GitOfUser/qoderwork_checkin` | Python | 桌面 GUI 签到（pyautogui，非 API） |

### 4.3 已删除（trash）

- `/root/.qodersec` — npm 装的 qodercli/qodersec 二进制（133M）
- `/tmp/package` — CN CLI bundle 解压
- `/tmp/wasm-poc` — WASM PoC 脚本

---

## 5. 待办清单（按优先级）

- [ ] **P0**: 写 `qoderwork` Go plugin 骨架（基于 WorkBuddy 模板）
- [ ] **P0**: Go 实现 COSY 签名（RSA/AES/MD5 全标准库）+ QoderEncoding
- [ ] **P0**: Go 实现 PAT → jobToken exchange + refresh
- [ ] **P1**: OpenAI `/v1/chat/completions` → QoderWork body 转换 + SSE 转发
- [ ] **P1**: 模型路由（`x-model-key` → CPA model name）
- [ ] **P2**: 签到（`/sash/api/v1/me/daily-check-in/*`，device_token 即可）
- [ ] **P2**: 积分查询（`/api/v2/quota/usage`）
- [ ] **P3**: 多账号池 + 调度
- [ ] **P3**: 部署 CPA + E2E 验收

---

## 6. 关键技术决策记录

| 时间 | 决策 | 理由 |
|---|---|---|
| 2026-07-25 | 用 WASM 签名 | 以为 CLI 靠 WASM |
| 2026-07-26 | **废弃 WASM，纯软件签名** | asar 逆向 + cubk1 开源证实 |
| 2026-07-26 | **废弃 qodercli 子进程** | 桌面端 WorkerTransport 不 spawn CLI |
| 2026-07-26 | **PAT 为主认证** | device_token 不能推理，PAT 100 年有效 |
| 2026-07-26 | **走 plugin 而非独立 gateway** | 复用 CPA 调度/持久化/WebUI |

---

## 7. 关键参考

- COSY 签名 JS 实现：`/tmp/qw_extract/app/resources/asar_out/out/main/main.js:2614427-2616623`
- Python 验证脚本：`/tmp/qw_web/qoder_chat_test.py`
- Java 参考：`https://github.com/cubk1/qoder2api` (BearerBuilder.java / BearerApiClient.java / QoderEncoding.java)
- Python 参考：`https://github.com/bzym2/QoderGateway` (src/qoder2api/auth.py)
