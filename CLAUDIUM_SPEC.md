# QoderWork CPA Plugin — Claudium Implementation Spec

> 任务：基于 `/root/qoderwork/workbuddy` 插件骨架，实现 `qoderwork` 插件。
> 已验证事实全部在此文档，**不要重新逆向**。

## 0. 目录与产出

- **工作目录：** `/root/qoderwork/qoderwork/`（新建）
- **编译产物：** `qoderwork.so`（`go build -buildmode=c-shared`）
- **go.mod：** `module github.com/sliverkiss/qoderwork-plus`，go 1.26.0，依赖 `github.com/router-for-me/CLIProxyAPI/v7 v7.2.30`
- **参考插件：** `/root/qoderwork/workbuddy/`（完整可编译，照抄结构）
- **Logo：** `https://github.com/DGZSbot/ai-icon/raw/main/QoderWork.png`

## 1. 已验证的认证链路（不要改）

### 1.1 PAT → jobToken

```
POST https://openapi.qoder.com.cn/api/v1/jobToken/exchange
Content-Type: application/json
{"personal_token": "pt-xxx"}
→ 200 {"token":"jt-xxx","refresh_token":"jrt-xxx","expires_at":"...","expires_in":86400000,
       "refresh_token_expires_at":"...","refresh_token_expires_in":172800000}
```

- PAT (pt-) 是用户唯一的长期凭证（创建时 expires_at 自填，可 100 年）
- jt- 24h 有效；jrt- 48h 内可 refresh：`POST /api/v1/jobToken/refresh {"refresh_token":"jrt-xxx"}`（响应同 exchange）
- 超过 48h 用 PAT 重新 exchange（无感）

### 1.2 COSY 签名（纯 Go 可实现，Go 标准库够）

每次进程启动生成一次 session：
```go
machineId   = uuid (带连字符)
machineToken = urlsafe_b64_no_pad((uuid+uuid)[:50])
machineType = uuid.hex[:18]
tempKey     = []byte(uuid.hex[:16])   // 16 字节 ASCII

cosyKey = base64_std(RSA_PKCS1v15_encrypt(SERVER_PUBKEY, tempKey))
identityJSON = json_sorted_compact({
    "name": name, "aid": uid, "uid": uid,
    "yx_uid": "", "organization_id": "", "organization_name": "",
    "user_type": "personal_professional_trial",
    "security_oauth_token": jt, "refresh_token": jrt,
})
info = base64_std(AES_128_CBC_encrypt(identityJSON, key=tempKey, iv=tempKey))
```

每次请求：
```go
payload = {"cosyVersion":"0.1.43","ideVersion":"","info":info,"requestId":uuid,"version":"v1"}
payloadB64 = base64_std(json_sorted_compact(payload))
pathSig = strings.TrimPrefix(url.Path, "/algo")
date = strconv.FormatInt(time.Now().Unix(), 10)
sig = md5_hex(payloadB64 + "\n" + cosyKey + "\n" + date + "\n" + body + "\n" + pathSig)
Authorization = "Bearer COSY." + payloadB64 + "." + sig
```

**RSA 公钥（硬编码）：**
```
-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----
```

### 1.3 请求 Headers（15 个，全部必带）

```
cosy-data-policy: AGREE
content-type: application/json
cosy-machinetype: {machineType}
cosy-clienttype: 5
cosy-date: {unix_ts}
cosy-user: {uid}
cosy-key: {cosyKey}
accept: text/event-stream          (SSE) 或 application/json
cosy-clientip: 169.254.198.161
authorization: Bearer COSY.{payloadB64}.{sig}
accept-encoding: identity
cosy-version: 0.1.43
cosy-machineid: {machineId}
cosy-machinetoken: {machineToken}
login-version: v2
user-agent: Go-http-client/2.0
x-model-key: {model_key}           (推理时)
x-model-source: system             (推理时)
cache-control: no-cache            (仅 SSE)
```

### 1.4 QoderEncoding（body 编码）

```go
const customAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
const stdAlphabet   = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
const customPad     = '$'

func qoderEncode(plain []byte) string {
    std := base64.StdEncoding.EncodeToString(plain)
    n := len(std); a := n / 3
    rearranged := std[n-a:] + std[a:n-a] + std[:a]
    var sb strings.Builder
    for _, c := range []byte(rearranged) {
        if c == '=' { sb.WriteByte(customPad) } else {
            idx := strings.IndexByte(stdAlphabet, c)
            sb.WriteByte(customAlphabet[idx])
        }
    }
    return sb.String()
}
```

### 1.5 推理端点

```
POST https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation
     ?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1
```

**响应：** SSE 流。每行 `data:{"headers":{...},"body":"<json-string>","statusCodeValue":200}`。
**body 字段是 JSON 字符串**，需二次解析得到标准 OpenAI chunk：
```json
{"choices":[{"delta":{"content":"Hi"},"index":0}],"created":...,"id":"chatcmpl-...","model":"auto","object":"chat.completion.chunk"}
```
结束标志：`data:{"body":"[DONE]"}` + `event:finish`。

### 1.6 请求 Body 模板

完整模板：`/root/qoderwork/qoderwork/baseprompt.json`（从 cubk1/qoder2api 抄，占位符已清）。

每次请求必须覆盖：
- `request_id` = `chat_record_id` = uuid
- `request_set_id` = `session_id` = `business.id` = uuid
- `business.begin_at` = unix_ms
- `business.name` = prompt[:30]
- `model_config.key` = model_key (e.g. "qwen3.8-max")
- `chat_context.text.text` = `chat_context.extra.originalContent.text` = 最新 user prompt
- `messages` = OpenAI 格式 [{role, content}]（保留 system，但模板里有 10657 tokens 的内置 system prompt，**用模板的 messages 字段**，把用户消息 append 进去）
- `aliyun_user_type` = "personal_professional_trial"
- `session_type` = "qodercli"
- `stream` = true

### 1.7 业务 API（jt- Bearer，无需 COSY）

```
GET  https://openapi.qoder.com.cn/api/v1/userinfo
GET  https://openapi.qoder.com.cn/api/v2/quota/usage
GET  https://openapi.qoder.com.cn/sash/api/v1/me/daily-check-in/status
POST https://openapi.qoder.com.cn/sash/api/v1/me/daily-check-in/claim
POST https://openapi.qoder.com.cn/sash/api/v1/me/pro-upgrade/claim
GET  https://openapi.qoder.com.cn/sash/api/v1/me/invitationCode
Header: Authorization: Bearer {jt}
```

## 2. 插件功能范围

### 2.1 必须实现（Loop 1-5）

| 模块 | 文件 | 功能 |
|---|---|---|
| main.go | C ABI + handleMethod dispatch | 照抄 WorkBuddy |
| auth.go | PAT parse + jobToken exchange/refresh + storage | 新方法 |
| sign.go | COSY 签名 | 新 |
| encoding.go | QoderEncoding | 新 |
| body.go | baseprompt 模板构造 | 新 |
| executor.go | execute + execute_stream (SSE 解析 + OpenAI 转换) | 参考 WorkBuddy |
| credits.go | quota/usage 查询 | 新 |
| checkin.go | 签到 | 参考 WorkBuddy CN 签到 |
| scheduler.go | 按 remaining 选账号 | 照抄 WorkBuddy |
| lifecycle.go | 耗尽 disable | 照抄 WorkBuddy |
| management.go | /checkin /credits /invitation 路由 | 参考 WorkBuddy |
| panel.html | 管理面板 | 参考 WorkBuddy |

### 2.2 Auth 流程（与 WorkBuddy 不同）

WorkBuddy 是 OAuth device flow；QoderWork 是 **PAT 手动粘贴**：

- `auth.login_start` → 返回 `manual_instructions`：
  ```
  1. 浏览器打开 https://qoder.com.cn → 登录（手机验证码）
  2. F12 Console 执行:
     fetch('/api/v1/me/personal-access-tokens', {method:'POST',credentials:'include',
       headers:{'Content-Type':'application/json'},body:JSON.stringify({name:'cpa',expires_at:Date.now()+3153600000000})})
       .then(r=>r.json()).then(d=>console.log(d.token))
  3. 复制 pt-xxx 粘贴到下方
  ```
- `auth.parse` → 接受 `{"pat":"pt-xxx"}` 或纯字符串 `pt-xxx`
- `auth.refresh` → 用 jrt- 调 `/api/v1/jobToken/refresh`；若 401/过期则用 PAT 重新 exchange
- storage JSON 格式：
  ```json
  {
    "pat": "pt-xxx",
    "jt": "jt-xxx", "jrt": "jrt-xxx",
    "jt_expires_at": 1785028404, "jrt_expires_at": 1785114804,
    "uid": "019f8417-...", "name": "aliyun6109533651",
    "machine_id": "uuid", "machine_token": "...", "machine_type": "...",
    "temp_key": "16chars", "cosy_key": "b64", "info": "b64"
  }
  ```
  （cosy_key/info 在进程启动时重算，不依赖存储；但 machine_id 持久化同一账号不变）

### 2.3 模型清单

静态注册（model.static）：

| CPA model id | name | x-model-key |
|---|---|---|
| qoder-auto | Qoder Auto | auto |
| qoder-ultimate | Qoder Ultimate | ultimate |
| qoder-performance | Qoder Performance | performance |
| qoder-efficient | Qoder Efficient | efficient |
| qoder-lite | Qoder Lite | lite |
| qwen3.8-max | Qwen 3.8 Max | qwen3.8-max |
| qwen3.7-max | Qwen 3.7 Max | qmodel |
| deepseek-v4-pro | DeepSeek V4 Pro | dmodel |
| deepseek-v4-flash | DeepSeek V4 Flash | dfmodel |
| glm-5.2 | GLM 5.2 | gm51model |
| kimi-k3 | Kimi K3 | kmodel |
| minimax-m3 | MiniMax M3 | mmodel |

### 2.4 SSE 解析细节

上游 SSE 每行：`data:{"headers":{...},"body":"<escaped-json-string>","statusCodeValue":200,"statusCode":"OK"}`

处理：
1. 去 `data:` 前缀
2. 解析外层 JSON → 取 `body` 字段（字符串）
3. 若 body == "[DONE]" → 结束
4. 否则再解析 body 字符串 → 标准 OpenAI chunk → 直接透传给 CPA

## 3. 验证标准

```bash
cd /root/qoderwork/qoderwork
make build   # 生成 qoderwork.so
go test ./... # 单元测试全过
```

单元测试必须覆盖：
- QoderEncoding encode/decode 与 Python 实现对拍（`/tmp/qw_web/qoder_chat_test.py`）
- COSY 签名结构（不能对拍因含随机，但字段齐全性）
- body 构造字段完整性
- SSE 解析（mock 上游响应）

## 4. 参考代码

- **WorkBuddy 骨架：** `/root/qoderwork/workbuddy/`（抄 main.go 的 C ABI + handleMethod + envelope + hostCall + streamEmit + publishUsage + scheduler.go + lifecycle.go + management.go 结构）
- **Python 签名参考（已验证）：** `/tmp/qw_web/qoder_chat_test.py`
- **Java 签名参考：** 可 `curl https://raw.githubusercontent.com/cubk1/qoder2api/master/src/main/java/us/cubk/BearerBuilder.java`
- **body 模板：** 从 `https://raw.githubusercontent.com/cubk1/qoder2api/master/baseprompt.json` 下载后清理占位符，存为 `qoderwork/baseprompt.json`（embed 进 Go）

## 5. 明确不做

- ❌ WASM（已废弃）
- ❌ qodercli 子进程（已废弃）
- ❌ device_token OAuth flow（用 PAT）
- ❌ count_tokens（返回 0 即可）
- ❌ tool calls 转换（先透传，后续再加）

## 6. 关键常量

```go
providerName = "qoderwork"
version      = "0.1.0"
logoURL      = "https://github.com/DGZSbot/ai-icon/raw/main/QoderWork.png"
githubRepo   = "https://github.com/Sliverkiss/cpa-plugin"

openAPIBase  = "https://openapi.qoder.com.cn"
gatewayBase  = "https://gateway.qoder.com.cn"
inferPath    = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
```
