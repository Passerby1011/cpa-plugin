# 分析：WorkBuddy 签到链重写

> 2026-07-27 · 用户反馈：签到链太长、单卡片签到也触发全部、签到成功却提示超时

## 现状问题诊断

### 链路（单个签到，29 账号场景）

前端 `checkin(idx)` → POST `/checkin {auth_index:idx}` → `handleManualCheckin`：

```
Phase1 classifyCheckinTargets(targets)   ← 单账号时 targets=1，不会扫全部 ✓
  hostAuthList()                          RPC#1（列全部 29 个 auth 文件！）
  hostAuthGet(authIndex)                  RPC#2
  fetchCheckinStatus(sa)                  HTTP#1（billingCall，含重试 300ms+900ms）
  [若 ci 失败] cachedCheckinToday 回退
Phase2 executeCheckinBatch(eligible)
  per-account mutex
  hostAuthGet(authIndex)  ← 又读一次      RPC#3（重复！）
  fetchCheckinStatus(sa)  ← 又查一次      HTTP#2（重复！）
  performCheckinCall(sa)                   HTTP#3（POST daily-checkin）
  fetchCheckinStatus(sa)  ← 再查一次       HTTP#4（重复！）
  reconcileOneAccount(...)                 ← lifecycle：又一轮 hostAuthGetBundle + fetchUserResource（HTTP#5, RPC#4...）
Phase3 summarize
```

**单个签到 = 4 次 RPC + 最多 5 次上游 HTTP + 每次 HTTP 最多 3 次重试（间隔 0.3s/0.9s）。**
最坏情况：5 HTTP × 3 次 × (上游 RTT + 1.2s 重试间隔) → 30s+，撞上 CPA management API 超时
（v0.1.11 QoderWork 同款病根：30s context canceled）。

### "单卡片签到触发全部"的体感来源

后端对 `auth_index` 过滤是正确的（targets=1）。但：
1. `hostAuthList()` 每次都全量列 29 个文件（RPC 重）
2. 面板 `load(false)` 在签到后整体刷新 → 看起来像"全部动了"
3. 签到成功但响应被超时截断 → 前端走 catch → toast "签到请求失败"

### 三段式过度设计

`classifyCheckinTargets → executeCheckinBatch → summarizeCheckinResults` 是为 29 账号批量设计的，
单账号也走完整框架，且 Phase1/Phase2 重复 `hostAuthGet` + `fetchCheckinStatus`（锁内再读一遍）。

## 重写方案（对齐 QoderWork v0.1.11 已验证模式）

```
handleManualCheckin:
  单账号模式（auth_index != ""）:
    直接 checkinOneAccount(authIndex):
      hostAuthGet                    RPC ×1
      isGlobalDomain? → 提示用专家包
      fetchCheckinStatus             HTTP ×1（无重试循环，失败仍继续——上游幂等）
      已签 → 返回 already
      performCheckinCall             HTTP ×1
      已签类业务消息 → already
      成功 → 更新 cache（复用已拿到的数据，不再重新 fetch）
    无 classify / 无锁内重读 / 无 lifecycle reconcile（面板不需要）
  批量模式（auth_index == ""）:
    每账号一个 goroutine（sem=4），各自跑 checkinOneAccount，收集结果
```

- 预期单账号：1 RPC + 2 HTTP ≈ 1-2s（对比现在 4 RPC + 5 HTTP × 重试 ≈ 10-30s）
- `fetchCheckinStatus` 双路径回退（checkin-activity-status → checkin-status）保留
- billingCall 重试保留在底层（5xx 瞬时错误），但签到路径总调用次数砍半以上
- cache 更新用 merge 语义（不丢 credits/plan），签到成功后不再额外 fetch
- lifecycle reconcile 从手动签到路径移除（它属于定时任务 runAutoCheckin 的职责）

## 影响面

- 改：checkin.go 的 handleManualCheckin + 新增 checkinOneAccount；删 classify/execute/summarize 三函数及 3 个 struct
- 不改：runAutoCheckin（定时任务路径，批量本来就合理）、面板 JS 契约（响应形状保持 {results, summary}）
- 风险：面板 checkinAll 依赖 summary 字段（success/already/fail/skipped_global/eligible）→ 保持同名字段
