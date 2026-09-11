# QoderWork CPA Plugin — 实现计划 v4

> **2026-07-26 重写。PAT + 纯软件 COSY 签名已端到端验证（qwen3.8-max 实测 200）。**
> **已废弃：WASM 签名、qodercli 子进程、device_token 直签。**

**目标：** 构建 QoderWork CPA 插件（Go c-shared），把 QoderWork CN 暴露为 openai-compatible provider。

**架构：**

```
CPA Gateway
  └─ QoderWork plugin (Go, c-shared)
       ├─ auth: PAT (pt-) → jobToken/exchange → jt- (24h) → jrt- refresh (48h)
       ├─ sign: COSY 纯 Go 实现 (RSA PKCS1v15 + AES-128-CBC + MD5)
       ├─ encode: QoderEncoding (自定义 base64 字母表 + 三段重排)
       ├─ infer: POST gateway.qoder.com.cn/algo/.../agent_chat_generation (SSE)
       ├─ checkin: /sash/api/v1/me/daily-check-in/* (jt- Bearer)
       └─ quota: /api/v2/quota/usage (jt- Bearer)
```

---

## 一、认证链路（最终版，已验证）

### 1.1 PAT → jobToken

```http
POST https://openapi.qoder.com.cn/api/v1/jobToken/exchange
Content-Type: application/json

{"personal_token": "pt-xxx_uuid"}

→ {"token":"jt-xxx", "refresh_token":"jrt-xxx",
   "expires_at":"2026-07-27T01:13:24Z", "expires_in":86400000,
   "refresh_token_expires_at":"2026-07-28T01:13:24Z"}
```

- PAT 永不过期（创建时 expires_at 自填），只用于换 jobToken
- jt- 24h 有效，jrt- 48h 内可 refresh：`POST /api/v1/jobToken/refresh {refresh_token}`
- 超过 48h 重新用 PAT 换

### 1.2 COSY 签名（纯 Go 可实现）

```go
// 1) temp_key (16 bytes random per session)
tempKey := []byte(strings.ReplaceAll(uuid.New().String(), "-", "")[:16])

// 2) cosy_key = base64(RSA_PKCS1v15_encrypt(serverPubKey, tempKey))
cosyKey := base64(rsa.EncryptPKCS1v15(rand, pubKey, tempKey))

// 3) info = base64(AES-128-CBC(json(identity), key=tempKey, iv=tempKey))
identity := map[string]string{
    "name": name, "aid": uid, "uid": uid,
    "yx_uid": "", "organization_id": "", "organization_name": "",
    "user_type": "personal_professional_trial",
    "security_oauth_token": jt, "refresh_token": jrt,
}
info := base64(aesCBCEncrypt(json(identity), tempKey, tempKey))

// 4) Bearer COSY
payload := map[string]string{
    "cosyVersion": "0.1.43", "ideVersion": "",
    "info": info, "requestId": uuid.New().String(), "version": "v1",
}
payloadB64 := base64(jsonSorted(payload))
pathSig := strings.TrimPrefix(url.Path, "/algo")
sig := md5(fmt.Sprintf("%s\n%s\n%d\n%s\n%s", payloadB64, cosyKey, unixTs, body, pathSig))
Authorization := fmt.Sprintf("Bearer COSY.%s.%s", payloadB64, sig)
```

### 1.3 QoderEncoding（body 编码）

```go
const customAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
const customPad = '$'

func qoderEncode(plain []byte) string {
    std := base64.StdEncoding.EncodeToString(plain)
    n := len(std); a := n / 3
    rearranged := std[n-a:] + std[a:n-a] + std[:a]
    // map each char via custom alphabet, '=' → '$'
}
```

### 1.4 推理请求

```http
POST https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation
     ?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1

Headers: (15 个 cosy-* + x-model-key + x-model-source)
Body: qoderEncode(json(bodyTemplate))
```

**Body 模板：** 见 `/tmp/qw_web/baseprompt_clean.json`，关键字段：
- `model_config.key` = 模型名
- `chat_context.text.text` = 用户 prompt
- `messages` = OpenAI 格式
- `aliyun_user_type` = `personal_professional_trial`

**响应：** SSE `data: {"body":"<openai-chunk-json-string>"}`，body 字段是 JSON 字符串需二次解析。

---

## 二、Loop 计划（修订）

### Loop 1: Plugin 骨架（~30min）

- go.mod + CPA SDK v7
- C ABI: init/call/free/shutdown
- RPC dispatch: auth/executor/scheduler/lifecycle stub
- model.static: qwen3.8-max, qwen3.7-max, lite, ultimate, performance, efficient
- Makefile → .so

**验收：** plugin 加载，CPAMP 可见

### Loop 2: Auth 模块（~1h）

- `auth.login_start`: 返回提示「手动填 PAT」或触发 device flow（可选）
- `auth.parse`: 接受 `{"pat":"pt-xxx"}` 或 `{"token":"dt-xxx","refresh_token":"drt-xxx"}`
- `auth.refresh`: jobToken/refresh → 新 jt-
- `auth_storage`: host.auth.save → `qoderwork-<uid>.json`
- PAT 存 auth.PAT 字段，jt-/jrt- 存 auth.Token/RefreshToken

**验收：** CPAMP 粘贴 PAT → 保存 → refresh 成功

### Loop 3: COSY 签名 + QoderEncoding（~1h）

- `sign.go`: rsaEncrypt/aesCBCEncrypt/buildBearer/signRequest
- `encoding.go`: qoderEncode/qoderDecode
- 单元测试对照 Python 实现

**验收：** Go 签名结果与 Python 一致（同输入同输出）

### Loop 4: 对话执行（~1.5h）

- `executor.execute_stream`: 构造 body → 签名 → POST SSE → 解析 data:{"body":"..."} → 转 OpenAI delta
- `executor.execute`: 聚合完整响应
- 模型路由：CPA model name → `x-model-key`
- 错误处理：401 → 触发 refresh → 重试一次

**验收：** `curl CPA/v1/chat/completions -d '{"model":"qwen3.8-max","messages":[...]}'` 返回流式

### Loop 5: 签到 + 积分（~30min）

- `GET /sash/api/v1/me/daily-check-in/status` (jt- Bearer)
- `POST /sash/api/v1/me/daily-check-in/claim`
- `GET /api/v2/quota/usage`
- 定时 goroutine: 每日 09:00 + 21:00 UTC+8 签到
- 积分耗尽自动 disable

**验收：** 签到成功 +100，quota 正确显示

### Loop 6: 多账号 + 调度（~30min）

- scheduler.pick: 按 remaining quota 选账号
- 跳过 disabled
- CN 优先

### Loop 7: 管理界面 + 部署（~1h）

- management routes: /checkin /credits /invitation
- panel.html
- 编译 arm64+amd64 → 部署 CPA → E2E

---

## 三、模型清单（CPA 暴露）

**真实模型 key 来自 `GET /algo/api/v2/model/list?Encode=1`（已实测拿到清单）。**

| CPA model | upstream key | 真实模型 | price_factor |
|---|---|---|---|
| qoder-auto | auto | Auto（默认） | 0.5x |
| qwen3.8-max-preview | qmodel_preview | Qwen3.8-Max-Preview | 0.05x |
| qwen3.7-max | qmodel_latest | Qwen3.7-Max | 0.25x |
| qwen3.7-plus | qmodel | Qwen3.7-Plus | 0.1x |
| qwen3.6-flash | q36fmodel | Qwen3.6-Flash | 0.1x |
| deepseek-v4-pro | dmodel | DeepSeek-V4-Pro | 0.5x |
| deepseek-v4-flash | dfmodel | DeepSeek-V4-Flash | 0.1x |
| glm-5.2 | gm51model | GLM-5.2 | 0.6x |
| kimi-k2.7-code | kmodel | Kimi-K2.7-Code | 0.3x |
| minimax-m2.7 | mmodel | MiniMax-M2.7 | 0.2x |

**注意：** 响应里 `"model":"auto"` 是服务端硬编码，不代表路由失败。真实路由看响应格式（`reasoning_content`/`system_fingerprint`/id 前缀）。

---

## 四、风险矩阵

| 风险 | 概率 | 影响 | 回退 |
|---|---|---|---|
| COSY 签名 Go 实现细节差异 | 低 | 401 | 对照 Python 逐字段 diff |
| jobToken refresh 机制未验证 | 中 | 24h 后失败 | 重新 PAT 换（无感） |
| SSE body 嵌套 JSON 解析 | 低 | 解析错误 | 已实测格式固定 |
| 模型 key 名不对 | 中 | 404/路由错 | 从 CLI bundle 抄完整映射 |
| Qoder 封自动化 | 低 | 账号封 | 限速 + 随机延迟 |

---

## 五、参考实现

- **Python 验证（已通过）：** `/tmp/qw_web/qoder_chat_test.py`
- **Java 参考：** `cubk1/qoder2api` → BearerBuilder.java, BearerApiClient.java, QoderEncoding.java
- **Python 参考：** `bzym2/QoderGateway` → src/qoder2api/auth.py
- **Body 模板：** `/tmp/qw_web/baseprompt_clean.json`
- **JS 原始实现：** `/tmp/qw_extract/app/resources/asar_out/out/main/main.js:2614427-2616623`
