# 国际版每日活跃奖励（Global Daily Activity）

> **状态：`NOT_VERIFIED_ON_LIVE`** — 协议形状来自对线上服务的实测记录与参考实现，
> 本仓库**未用真实国际版账号跑过端到端验证**。单测覆盖请求形状、凭据头、ACP 顺序、
> 去重与错误分支；真机验证待补。

## 这是什么

官方对 `www.workbuddy.ai`（国际版）个人版按天发放「每日活跃奖励」：

| 套餐 | 每日奖励 |
|---|---|
| Free | 30 积分/天 |
| Pro | 50 积分/天 |

领取条件：**当天完成过一次有效对话**。这不是签到按钮，是行为激励。

官方规则页：`https://www.workbuddy.ai/docs/zh/workbuddy/Subscription`
（"Daily activity rewards are additional credits and can be combined with the monthly
credit benefits of the applicable plan."）

## 为什么之前没人领

本插件 `checkin.go` 的设计口径是：CN 账号每日签到，Global 账号
「never check-in or auto-claim trial. Lifecycle only.」——即国际版既不算签到，
也没有任何活跃打卡，只做积分耗尽后的生命周期处理。所以这份奖励在网关侧一直是空白。

上游 `hex-ci/cpa-plugin`（v0.12.1）同样没有这个功能（`console/as/conversations` 零命中）。

## 协议（实测形状）

### 为什么不能用桌面端通道

**桌面端身分发起的对话不算数。** 上游两位报告人的实测一致：网关自动发出的桌面端身份对话
拿不到每日 30 积分，而网页版手动发一句就能拿到。

网页版 app 的「对话」**不是** `/v2/chat/completions`：

- 端点在 `/console/as/conversations/` 下，是 agent 会话，不是 chat completions；
- 只带两个凭据头：`Authorization: Bearer <token>` 与 `X-User-Id: <uid>`；
- **没有**桌面端的 `X-IDE-*` 指纹。

因此网关手里同一份账号凭据可以直接调用，不需要额外的网页登录。

### 五个步骤

```
1) POST {web}/console/as/conversations/
   headers: Authorization, X-User-Id, Content-Type, Accept, Origin, Referer, User-Agent
   body:    {"prompt":"Hi","model":"deepseek-v4.1-flash",
             "conversationOrigin":"workbuddy-app",
             "plugins":[{"name":"weixinpay","marketplace":"codebuddy-builtin"}]}
   → data.id  （这是 conversation id）

2) GET  {web}/console/as/conversations/{id}/session
   → data.link, data.token, data.sessionId, data.cwd   （沙箱地址与令牌）

3) ACP over streamable HTTP，打到 data.link：
   a) GET  link  with Accept: text/event-stream
      → 响应头 Acp-Connection-Id   （开 SSE 通道）
   b) POST link  (带 Acp-Connection-Id)，JSON-RPC：
        initialize    {"protocolVersion":1,
                       "clientCapabilities":{"fs":{"readTextFile":false,"writeTextFile":false},
                                             "terminal":false}}
        session/load  {"sessionId":..., "cwd":"/workspace", "mcpServers":[]}
        session/prompt{"sessionId":..., "prompt":[{"type":"text","text":"Hi"}]}
      （SSE 流里会推回 session/update 通知）

4) GET  {web}/console/as/conversations/{id}
   → data.status；等到 "completed" 才算这一轮真的跑完

5) 成功 → 记当天已完成（按 uid + 本地日期），不再重复
```

### 最大的坑

**只建会话不算。** `POST /console/as/conversations/` 只是**排队**一条会话。
不接上沙箱、不请求这一轮，会话会永远停在 `CREATING`、没有任何输出，
**也就不算一次有效对话**（上游 issue #90 抓到的就是这个）。
必须走完第 3 步的 ACP 三步并等到第 4 步返回 `completed`。

## 代码位置

| 文件 | 内容 |
|---|---|
| `daily_activity.go` | 全部实现：会话排队、沙箱会话、ACP 驱动、去重、调度入口 |
| `daily_activity_test.go` | 单测：资格判定、凭据头、请求形状、ACP 顺序、去重、错误分支 |
| `checkin.go` | 调度：`dailyActivityHours = []int{9}`，接入 `schedulerLoop` |
| `usage_config.go` | 配置项 `daily_activity_auto`（默认 false） |
| `management.go` | 路由 `POST /daily-activity`、`POST /daily-activity/config` |
| `panel.html` | 「每日活跃打卡(Global)」按钮 + 「自动每日活跃(Global)」开关 |

## 配置

```yaml
plugins:
  configs:
    workbuddy:
      enabled: true
      daily_activity_auto: true   # 默认 false；开启后本地时间 09:00 巡检
```

面板上的开关是**运行时**切换（CPA host 不提供插件配置写入回调），重启后以
`config_yaml` 为准——与 `checkin_auto` 同款行为。

## 范围边界

- 仅 **Global 个人账号**（`dailyActivityEligible`）。
- **CN 账号**跳过：走原有签到 / 成长任务 / 猫猫旅行链路。
- **企业账号**跳过：额度由管理员按月发放，无此奖励。

## 真机验证待办

以下三项需要真实国际版账号才能确认，`NOT_VERIFIED_ON_LIVE`：

1. `POST /console/as/conversations/` 的实际响应结构（是否 `data.id`）。
2. `GET .../session` 返回的 `link` / `token` 字段名是否与实测一致。
3. ACP 三步后会话是否稳定进入 `completed`，以及积分是否真的 +30。

验证方法：用一个国际版账号在面板点一次「每日活跃打卡(Global)»，
看返回的 `claimed` 与积分面板变化。
