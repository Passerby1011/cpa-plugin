# QoderWork CPA Plugin — Dead Code & Remnants Analysis

> **Scope:** `/root/qoderwork/qoderwork/*.go` (25 files), cross-referenced with `KNOWLEDGE.md`'s documented API surface.
> **Mode:** Analysis only — no files were modified.
> **Generated:** 2026-07-27

---

## Summary

| Category | Count |
|---|---|
| Dead code (defined, never called) | 13 |
| WorkBuddy/CodeBuddy remnants in comments | 9 |
| Functions copied from WorkBuddy that don't apply to CN-only | 5 |
| Files partially unnecessary | 4 (no file is *entirely* dead) |
| Inefficient patterns | 8 |
| Stale `if false {}` dead blocks | 4 |

No file is **entirely** unnecessary — each has at least one live caller. The problems are isolated functions/types/comments within files.

---

## 1. Dead Code (Functions/Types/Vars Defined But Never Called)

### 1.1 `accountData` struct — **main.go:477**

```go
type accountData struct {
    UID          string `json:"uid"`
    EnterpriseID string `json:"enterpriseId"`
    Nickname     string `json:"nickname"`
}
```

**Why dead:** Defined but never instantiated, returned, or referenced anywhere. `userInfoResponse` (oauth.go:144) and `storedAccount` (main.go:460) cover the same fields. Likely a WorkBuddy leftover where a generic account type was used for OAuth state parsing. QoderWork's OAuth-like flow never returns this shape (it uses `userInfoResponse` directly).

**Action: Delete.**

---

### 1.2 `authStateData` struct — **main.go:483**

```go
type authStateData struct {
    State   string `json:"state"`
    AuthURL string `json:"authUrl"`
}
```

**Why dead:** Defined but never used. QoderWork's `handleStartLogin` (oauth.go:229) builds state with `fmt.Sprintf("qw-%d", now.UnixNano())` and constructs the URL inline — it never unmarshals an upstream `authStateData` response (QoderWork has no OAuth state endpoint; the PAT is pasted by the user).

**Action: Delete.**

---

### 1.3 `upstreamBaseFor()` — **main.go:539**

```go
func upstreamBaseFor(sa *storedAuth) string { return upstreamBaseCN }
```

**Why dead:** Never called. The constant `upstreamBaseCN` is used directly at every call site (billing.go:121, 251, 293, 314; oauth.go:109, 126). The comment on line 536-538 admits it exists "to minimise diff against the WorkBuddy skeleton" — WorkBuddy had CN+Global realm selection. QoderWork is CN-only, so this dispatcher is vestigial.

**Action: Delete.**

---

### 1.4 `backendHeaders()` — **main.go:548**

```go
func backendHeaders(req *http.Request, sa *storedAuth) {
    commonHeaders(req)
}
```

**Why dead:** Never called. The comment on lines 541-547 says it's kept "for parity with the WorkBuddy skeleton" but "the actual signing happens in `applyCosyHeaders`." Every executor path calls `applyCosyHeaders` directly (main.go:679, 787; stream.go:156). This is a no-op wrapper that does nothing but call `commonHeaders` — which is also called inside `applyCosyHeaders` indirectly.

**Action: Delete.**

---

### 1.5 `billingCall()` + `billingCallOnce()` + `isTransientBillingErr()` + `billingRetryDelays` — **billing.go:34-54, management.go:70**

```go
func billingCall(sa *storedAuth, path string, body any) (json.RawMessage, error)
func billingCallOnce(sa *storedAuth, path string, body any) (json.RawMessage, error)
func isTransientBillingErr(err error) bool
var billingRetryDelays = []time.Duration{300 * time.Millisecond, 900 * time.Millisecond}
```

**Why dead:** `billingCall` is never called. `billingCallOnce` is only called by `billingCall`. `isTransientBillingErr` is only called by `billingCall`. `billingRetryDelays` is only read by `billingCall`. All actual billing API calls (`fetchCheckinStatus`, `fetchUserResource`, `fetchPaymentType`, `performCheckinCall`) use `hostHTTPDo(req)` directly — they bypass the `billingCall` abstraction entirely.

This is a WorkBuddy inheritance: WorkBuddy had a generic billing RPC layer. QoderWork's API calls are direct `http.NewRequest` → `hostHTTPDo`, so this entire retry-envelope layer is orphaned.

**Action: Delete all four (`billingCall`, `billingCallOnce`, `isTransientBillingErr`, `billingRetryDelays`).**

---

### 1.6 `doJSON()` — **oauth.go:34**

```go
func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error)
```

**Why dead:** Never called. All QoderWork API calls use `doRawJSON` (oauth.go:80, 109, 126) for endpoints that return plain JSON, or `hostHTTPDo` directly for billing endpoints. The `doJSON` function parses the `{code,msg,data}` envelope — but the only endpoints that use that envelope (`apiEnvelope`) are accessed via `billingCallOnce` (which is itself dead, see 1.5). `keepalive.go:69` mentions `doJSON` in a comment but doesn't call it.

**Action: Delete.**

---

### 1.7 `billingBaseFor()` — **billing.go:18**

```go
func billingBaseFor(sa *storedAuth) string {
    return billingBase
}
```

**Why dead:** Only called by `billingCallOnce` (billing.go:64), which is itself dead (see 1.5). All live call sites use `upstreamBaseCN` constant directly. This is a WorkBuddy remnant where the base URL varied by realm.

**Action: Delete (along with `billingCall` cluster).**

---

### 1.8 `deleteAuthFileAt()` — **authfile.go:265**

```go
func deleteAuthFileAt(path string) error
```

**Why dead:** Never called. The comment says "Deprecated: use `deleteAuthFileInDir` instead." All delete paths (`deleteAuth` in lifecycle.go:189, `hostAuthPersistMigrate` in authfile.go:137) call `deleteAuthFileInDir`. The deprecated function exists only for "test coverage" per its comment, but no test file was found.

**Action: Delete.**

---

### 1.9 `aggregateSSEWithCollector()` — **stream.go:228**

```go
func aggregateSSEWithCollector(r io.Reader, sseFramed bool, collector *sseUsageCollector) ([]pluginapi.ExecutorStreamChunk, error)
```

**Why dead:** Never called. The executor uses `collectUpstreamStreamQoder` (stream.go:151) for synchronous collection and `pumpUpstreamStream` (stream.go:70) for async — both have their own inline SSE parsing loops that don't delegate to this function. `aggregateQoderSSE` (stream.go:447) handles the non-streaming path. This function is a workbuddy-shaped generic SSE aggregator that was superseded by QoderWork-specific nested-SSE parsers.

**Action: Delete.**

---

### 1.10 `cachedCheckinToday()` — **cache.go:175**

```go
func cachedCheckinToday(authID string) *bool
```

**Why dead:** Never called. The scheduler and checkin paths check `ci.TodayCheckedIn` directly on the `checkinSummary` returned by `fetchCheckinStatus` (checkin.go:137, 288). This helper was likely from WorkBuddy's three-phase classify flow where cached state was consulted before hitting the API.

**Action: Delete.**

---

### 1.11 `nextKeepaliveTime()` — **keepalive.go:264**

```go
func nextKeepaliveTime(now time.Time) time.Time
```

**Why dead:** Never called. The scheduler uses `nextCheckinTime` (checkin.go:36) which combines both `checkinHours` and `keepaliveHours` into one schedule. `shouldRunKeepaliveNow` (keepalive.go:281) handles the "fire keepalive on the current tick" logic. This standalone function duplicates logic already merged into `nextCheckinTime`.

**Action: Delete.**

---

### 1.12 `hostAuthGetFull` comment — **authfile.go:298**

```go
// hostAuthGetFull returns physical JSON, path, and name for an auth index.
```

**Why dead:** This is a dangling comment at the end of authfile.go with no function body after it. It was likely the doc comment for a function that was deleted but the comment wasn't cleaned up.

**Action: Delete the orphan comment.**

---

### 1.13 `qoderDecode()` — **encoding.go:56**

```go
func qoderDecode(encoded string) ([]byte, error)
```

**Why dead:** Never called. The plugin only **encodes** request bodies (main.go:674, 756). Decoding is never needed — QoderWork's responses are plain JSON/SSE, not QoderEncoding-encoded (KNOWLEDGE §6.1: "200 明文 JSON（不是 QoderEncoding）"). This was likely included for completeness during the clean-room port from the Python reference.

**Action: Delete (or keep if future use is planned, but mark as unused).**

---

### 1.14 `staticModels` var — **body.go:36**

```go
var staticModels = []qwModel{ ... }
```

**Why dead:** Never read. The model list is served by `wbModels()` in models.go:22 which returns `[]pluginapi.ModelInfo` directly. `staticModels` is a `[]qwModel` (body.go's own type) that is never referenced. This appears to be an earlier attempt at a model list that was superseded by `wbModels()` + dynamic `callModelsAPI()`.

**Action: Delete.**

---

### 1.15 `resourcePackage` struct + `packageRemainUsed()` — **management.go:79, billing.go:165**

```go
type resourcePackage struct { ... }  // management.go:79
func packageRemainUsed(a resourcePackage) (remain, used, size int64)  // billing.go:165
```

**Why dead:** `packageRemainUsed` is never called. `resourcePackage` is only used as the parameter type for `packageRemainUsed`. These are WorkBuddy remnants — WorkBuddy had a `/v2/billing/meter/get-user-resource` endpoint that returned a list of `resourcePackage` objects. QoderWork uses `/api/v2/quota/usage` which returns `quotaUsageResponse` (billing.go:227) with `userQuota`/`addOnQuota` — a different shape that doesn't use `resourcePackage` at all.

**Action: Delete both.**

---

## 2. WorkBuddy/CodeBuddy Remnants in Comments & Strings

### 2.1 C ABI wrapper function names — **main.go:47, 50, 223, 229**

```c
static int wb_call_host(...)        // line 47 — "wb" = WorkBuddy
static void wb_free_host_buffer(...) // line 50
```

**What:** The C wrapper functions are named `wb_call_host` / `wb_free_host_buffer` (wb = WorkBuddy). Called at main.go:223, 229.

**Action: Keep** (these are C ABI symbols that must match the C struct; renaming is cosmetic but the `wb` prefix is a naming remnant. Safe to rename to `qw_call_host` for clarity but not functionally required.)

### 2.2 `wbRegistration()` — **main.go:335**

```go
func wbRegistration() registration
```

**What:** Function name uses `wb` (WorkBuddy) prefix. Called at main.go:243.

**Action: Rename to `qwRegistration()` or `pluginRegistration()`** for consistency with `providerName = "qoderwork"`.

### 2.3 `wbModels()` — **models.go:22**

```go
func wbModels() []pluginapi.ModelInfo
```

**What:** Function name uses `wb` prefix. Called at models.go:57.

**Action: Rename to `qwStaticModels()` or `staticModels()`.**

### 2.4 `wbAccount` struct — **panel.go:15**

```go
type wbAccount struct { ... }
```

**What:** Type name uses `wb` prefix. Used at panel.go:65, 74, 196; active_auth.go:111.

**Action: Rename to `qwAccount` or `dashboardAccount`.**

### 2.5 Comment: "WorkBuddy skeleton" — **main.go:538, 545**

```go
// the WorkBuddy skeleton (clean-room reference) (callers pass sa but it's ignored).
// We keep the function signature (req, sa) for parity with the WorkBuddy
```

**What:** These comments are on `upstreamBaseFor` and `backendHeaders`, both of which are dead code (see 1.3, 1.4). Once those functions are deleted, these comments go too.

**Action: Delete (with the dead functions).**

### 2.6 Comment: "WorkBuddy helper" — **oauth.go:371**

```go
// toAuthDataForRefresh mirrors the WorkBuddy helper: blank out FileName and
```

**What:** Comment references WorkBuddy. The function itself is live (called at oauth.go:358).

**Action: Simplify comment** — remove "WorkBuddy" reference, just explain what it does.

### 2.7 Comment: "workbuddy-*.json auths" — **host_auth.go:49**

```go
// workbuddy- prefix but no type field. Filename prefix is the only
```

**What:** Comment mentions WorkBuddy auth files. This is in the context of explaining why filename-prefix filtering is used (to avoid accidentally matching WorkBuddy auth files). The concern is valid but the reference is stale.

**Action: Simplify** — reword to "other plugins' auth files" instead of naming WorkBuddy.

### 2.8 Comment: "WorkBuddy three-phase" — **checkin.go:186**

```go
// accounts. Unlike the WorkBuddy three-phase classify/execute/summarize flow,
```

**What:** Comment contrasts QoderWork's approach with WorkBuddy's. The comparison is informative but the named competitor is stale.

**Action: Simplify** — remove the WorkBuddy reference, just describe QoderWork's approach.

### 2.9 Comment: "Buddy-gas-station" — **management.go:18**

```go
// billingBase hosts the Buddy-gas-station check-in and resource-package APIs.
```

**What:** "Buddy-gas-station" is a workbuddy/CodeBuddy internal name for their billing API. QoderWork uses `openapi.qoder.com.cn`. The comment is misleading.

**Action: Rewrite** to "billingBase hosts the QoderWork OpenAPI check-in and quota endpoints."

### 2.10 Comment: "codebuddy.cn" — **management.go:67**

```go
// codebuddy.cn
// intermittently returns 500s; without a retry a single hiccup surfaces as a
```

**What:** Direct reference to codebuddy.cn domain. This is on the `billingRetryDelays` var which is dead code (see 1.5).

**Action: Delete (with the dead `billingRetryDelays`).**

### 2.11 Panel HTML remnants — **panel.html:63, 663, 674**

```css
.badge.global{background:var(--warn-soft);color:var(--warn);...}
```
```js
if(res.reason=="global"||(/国际版|不支持/.test(String(res.message||"")))){
```

**What:** The panel HTML has CSS classes and JS logic for "global" (international) accounts and "国际版" (international version) error handling. QoderWork is CN-only — there is no global/intl realm. The `summarizeCredits` function in panel.go (line 218) also has `if a.Region == "global"` branches that can never trigger since Region is always hardcoded to "cn".

**Action: Simplify panel.go** — remove `global_remain/global_used/global_size` from `summarizeCredits`. Clean up panel.html global/intl handling.

---

## 3. Functions Copied from WorkBuddy That Don't Apply to CN-Only

### 3.1 Region branching in `summarizeCredits()` — **panel.go:196-249**

```go
func summarizeCredits(accounts []wbAccount) map[string]any {
    var remain, used, size, cnRemain, cnUsed, cnSize, glRemain, glUsed, glSize int64
    ...
    if a.Region == "global" {
        glRemain += cr.TotalRemain
        ...
    } else {
        cnRemain += cr.TotalRemain
        ...
    }
    ...
    "global_remain":   glRemain,
    "global_used":     glUsed,
    "global_size":     glSize,
}
```

**What:** All accounts are hardcoded `Region: "cn"` (panel.go:97). The `global` branch is dead. The `glRemain/glUsed/glSize` vars are always 0. The `global_remain/used/size` keys in the response are always 0.

**Action: Simplify** — remove global branches and keys.

### 3.2 `displayNote()` region logic — **policy.go:143-149**

```go
func displayNote(sa *storedAuth, cr *creditsSummary, disabled bool) string {
    region := strings.ToUpper("cn")
    if region == "CN" {
        region = "CN"
    } else {
        region = "CN"
    }
```

**What:** Both branches set `region = "CN"`. This is a no-op if/else that was probably a realm switch in WorkBuddy (CN vs Global). The `strings.ToUpper("cn")` is also pointless.

**Action: Simplify** to `region := "CN"`.

### 3.3 `labelForAuth()` region logic — **policy.go:176-186**

```go
func labelForAuth(sa *storedAuth) string {
    ...
    tag := "CN"
    if "cn" == "global" {
        tag = "CN"
    }
    return base + " [" + tag + "]"
}
```

**What:** `if "cn" == "global"` is always false — this is dead code that was a realm check in WorkBuddy.

**Action: Simplify** — remove the dead if, just `tag := "CN"`.

### 3.4 `lifecycleActionFor()` region branching — **policy.go:117-125**

```go
func lifecycleActionFor(region string, cr *creditsSummary) lifecycleAction {
    if !shouldActOnCredits(cr) {
        return lifecycleNone
    }
    if region == "cn" {
        return lifecycleDelete
    }
    return lifecycleDisable
}
```

**What:** Since QoderWork is CN-only, `region` is always `"cn"` (hardcoded at lifecycle.go:319). The `return lifecycleDisable` branch is dead. Furthermore, the `deleteAuth` path for CN means exhausted CN accounts get their auth file **deleted** — but per KNOWLEDGE.md, QoderWork accounts are PAT-based and long-lived. Deleting an auth file means the user must re-import the PAT. This seems aggressive for a CN-only architecture where `disableAuth` would be safer.

**Action: Review** — consider whether CN should also use `lifecycleDisable` instead of `lifecycleDelete`. At minimum, remove the dead `lifecycleDisable` return since region is always "cn". Actually, the CN-delete vs CN-disable distinction may have been inherited from WorkBuddy where CN had trial packs (one-shot, delete on exhaust) vs Global (monthly, disable). QoderWork has no trial packs per KNOWLEDGE.md, so **delete-on-exhaust may be wrong** — disable would let the user re-checkin and restore credits without re-importing PAT.

### 3.5 `checkin.go` line 4 — stale CN exclusion comment

```go
// tabs. CN accounts are excluded — they use one-shot trial claims instead.
```

**What:** This comment is at the top of checkin.go. It says "CN accounts are excluded" from check-in, but the actual code (checkin.go:136) **does** perform CN daily check-in. The comment is a WorkBuddy remnant where CN (trial) and Global (check-in) had different flows. QoderWork CN uses daily check-in.

**Action: Delete or rewrite** the comment.

### 3.6 `checkin.go` lines 77, 119-134 — dead CN/trial block

```go
// CN: no auto trial (one-shot claim is manual only); reconcile may delete exhausted auths.
...
if false {
    // CN: never check-in or auto-claim trial. Lifecycle only.
    ...
}
```

**What:** The `if false {}` block (lines 119-134) is dead code that will never execute. It's a WorkBuddy remnant where CN accounts skipped check-in and only did lifecycle. In QoderWork, CN accounts **do** check in (the code after the `if false` block, lines 136+). The `if false` block should be removed entirely.

**Action: Delete** the `if false { ... }` block (lines 119-134).

---

## 4. Files That Might Be Entirely Unnecessary

### 4.1 `active_auth.go` — **KEEP**

Contains `getActiveAuthID`, `setActiveAuthID`, `clearActiveAuthIfMatch`, `pickActiveAuth`, `ensureDefaultActiveAuth`. All are called from scheduler.go, panel.go, lifecycle.go, credits_handler.go. **Not unnecessary.**

### 4.2 `policy.go` — **KEEP (but simplify)**

Contains `lifecycleAction`, `isHardCreditError`, `isSoftRateLimit`, `shouldReenableCN`, `displayNote`, `labelForAuth`. All are called from lifecycle.go, keepalive.go, checkin.go. `shouldActOnCredits` is trivially a wrapper for `isCreditsExhausted` but is called. **Keep, but simplify dead branches (3.2, 3.3, 3.4).**

### 4.3 `lifecycle.go` — **KEEP**

All functions called: `reconcileOneAccount`, `reconcileAllAccounts`, `reconcileAfterExecutorError`, `resolveAuthIndexAndID`, `reconcileByUID`, `invalidateAccountCredits`, `listEntryMatchesUID`, `enrichAuthMetadata`, `pruneLifecycleState`, `disableAuth`, `reenableAuth`, `deleteAuth`, `applyExhaustedPolicy`, `syncAuthNote`, `peerAuthDir`. **Not unnecessary.**

### 4.4 `scheduler.go` — **KEEP**

All functions called: `handleSchedulerPick`, `candidateDisabled`, `cachedCreditsScore`, `loadedSchedulerMode`, `setSchedulerMode` (test helper). **Not unnecessary.**

### 4.5 `usage.go` — **KEEP**

`handleUsage`, `publishUsage`, `forwardUsageToCPAMP`, `normalizeUsageDetail`, `usageDetailFromMap`, `usageDetailFromCompletion`, `sseUsageCollector` — all called. **Not unnecessary.**

### 4.6 `usage_config.go` — **KEEP**

`configure`, `resolveUsageReport`, `probeUsageReportURL`, `probeURL`, `readSecretFile` — all called. **Not unnecessary.**

### 4.7 `cache.go` — **KEEP (but delete `cachedCheckinToday`)**

`cachedAccountDetails`, `pruneAccountCacheSoftCap` — called. `cachedCheckinToday` — dead (1.10). **Mostly necessary, one dead function.**

### 4.8 `redact.go` — **KEEP**

`redactSecrets`, `truncateRedacted`, `truncate` — all called extensively. **Not unnecessary.**

---

## 5. Inefficient Patterns

### 5.1 `loginCtx` with nil client — **main.go:108-112, oauth.go:232**

```go
type loginCtx struct {
    client    *http.Client  // always nil in QoderWork
    expires   time.Time
    startedAt int64
}
...
loginStates.Store(state, &loginCtx{client: nil, expires: now.Add(loginTTL), startedAt: now.UnixNano()})
```

**What:** The `loginCtx` struct has a `client *http.Client` field that is always set to `nil` in QoderWork (oauth.go:232). In WorkBuddy, this held a cookie-jar-affined HTTP client for the OAuth flow. QoderWork's "OAuth" is just "paste a PAT" — no cookie jar needed.

**Action: Simplify** — remove the `client` field from `loginCtx`. The struct becomes just `{expires, startedAt}`.

### 5.2 Duplicate model list definitions — **models.go:22 + body.go:36**

**What:** The model list is defined **twice**:
- `wbModels()` in models.go:22 returns `[]pluginapi.ModelInfo` (10 models, used for registration)
- `staticModels` in body.go:36 is `[]qwModel` (10 models, never read — see 1.14)

These are the same 10 models with the same keys/names. Having two copies means model updates must be made in two places. The `qwModel` type (body.go:21) has extra fields (`Format`, `Source`, `Enable`, `IsDefault`, `IsVL`, `IsReasoning`, `PriceFactor`, `MaxInputTokens`) that are never used since `staticModels` is never read.

**Action: Delete `staticModels` and `qwModel` type** (body.go:21-47). If `qwModel` fields are needed in the future, they can be added to `pluginapi.ModelInfo` metadata.

### 5.3 `loginStates` janitor goroutine + `loginStatesPruneInterval` — **main.go:126-139**

**What:** A background goroutine runs forever (every 1 minute) pruning expired login states. The login states map is only populated by `handleStartLogin` (oauth.go:232) and cleaned up by `handlePollLogin` (oauth.go:291, 306). Since QoderWork's "login" is just generating a state token for the panel redirect, the states are very short-lived (5-min TTL) and few in number. A permanent background goroutine for this is over-engineered.

**Action: Keep** (the goroutine is cheap and prevents leaks if users abandon logins). But consider lazy pruning (prune on access instead of a ticker) to eliminate the background goroutine.

### 5.4 Redundant `hostAuthList()` calls in `reconcileAfterExecutorError` + `resolveAuthIndexAndID` — **lifecycle.go:379-443**

**What:** `reconcileAfterExecutorError` calls `resolveAuthIndexAndID`, which in the worst case calls `hostAuthList()` up to 3 times:
1. Line 408: `hostAuthList()` for fast-path ID match
2. Line 417: `hostAuthList()` again if fast-path fails
3. Line 434: `hostAuthGet(f.AuthIndex)` per file in slow path

Plus `invalidateAccountCredits` (lifecycle.go:483) calls `hostAuthList()` again. In the executor error path, this can result in 4+ `host.auth.list` RPCs in rapid succession.

**Action: Simplify** — `resolveAuthIndexAndID` should call `hostAuthList()` once and reuse the result. The fast-path `hostAuthGet(authID)` at line 406 is redundant — if `authID` is already an auth_index, the list at line 408 would find it anyway.

### 5.5 `processAutoCheckinAccount` double-fetches `sa` — **checkin.go:111-183**

**What:** When `doCheckin` is true:
1. Line 115: `sa, err := hostAuthGet(f.AuthIndex)` — fetch #1
2. Line 136: `fetchCheckinStatus(sa)` — uses #1
3. Line 149: `fetchUserResource(sa)` — uses #1
4. Line 175: `reconcileOneAccount(f.AuthIndex, f.ID, true)` — calls `hostAuthGetBundle` which calls `hostAuthGetPhysical` → fetch #2

The reconcile path re-reads the auth that was already fetched at line 115. This is because `reconcileOneAccount` is designed to be standalone (called from both checkin and lifecycle-only paths).

**Action: Keep** (the design is intentional — `reconcileOneAccount` must work standalone). But consider passing `sa` into `reconcileOneAccount` to avoid the re-fetch when the caller already has it.

### 5.6 `callModelsAPI` will always fail upstream — **models.go:130-137**

```go
func callModelsAPI(accessToken string) ([]pluginapi.ModelInfo, error) {
    ...
    // TODO(Loop 8): route this through cosySignedRequest() so the gateway
    // accepts the call. For now the call will fail upstream — models list
    // degrades to wbModels() static fallback.
    req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointModels, nil)
    ...
    req.Header.Set("Authorization", "Bearer "+accessToken)  // ← wrong auth
```

**What:** The models API endpoint (`gateway.qoder.com.cn/algo/api/v2/model/list?Encode=1`) requires COSY signing per KNOWLEDGE §6.1. But `callModelsAPI` sends a plain `Bearer` token — which will 401. The code comment admits this. This means `fetchDynamicModels()` always falls back to `wbModels()` static list, making the entire dynamic-fetch path dead code in practice.

**Action: Fix** — route `callModelsAPI` through `applyCosyHeaders` (the COSY signing path). Until then, the dynamic model list is never used and the entire `fetchDynamicModels` → `callModelsAPI` chain is effectively dead.

### 5.7 `refreshCall` uses `sharedHTTPClient()` instead of `hostHTTPDo` — **keepalive.go:80, 87**

```go
data, status, err := doRawJSON(sharedHTTPClient(), http.MethodPost, endpointJobTokenRefresh, ...)
```

**What:** `refreshCall` (keepalive.go:77) uses `doRawJSON` with `sharedHTTPClient()` — the direct HTTP client — instead of routing through `hostHTTPDo`. This bypasses the host's request-log and transport policy. The comment at keepalive.go:72 says "v0.8.0: routed via host.http.do" but the implementation still uses `doRawJSON` → `sharedHTTPClient()`. Similarly, `exchangePATForJobToken` and `refreshJobToken` in oauth.go also use `doRawJSON` → `sharedHTTPClient()`.

**Action: Fix** — route these through `hostHTTPDo` for compliance. The auth token exchange/refresh endpoints are upstream API calls that should be logged.

### 5.8 `keepalive.go` header comment references wrong endpoint — **keepalive.go:11-13**

```go
//   - Iterates all QoderWork auths via host.auth.list/get, calls
//     {realm-base}/v2/plugin/auth/token/refresh with X-Refresh-Token via
//     the host HTTP bridge (host.http.do).
```

**What:** The comment says `/v2/plugin/auth/token/refresh` with `X-Refresh-Token` header — this is WorkBuddy's refresh endpoint. QoderWork uses `/api/v1/jobToken/refresh` with a JSON body `{refresh_token: "jrt-..."}` (as the code at line 79 correctly implements). The comment is wrong.

**Action: Rewrite** the header comment to match the actual implementation.

---

## 6. `if false {}` Dead Blocks

Four `if false {}` blocks exist — all are WorkBuddy remnants where the condition was originally a realm check (e.g., `if region == "cn" && trialClaimed`):

| File | Line | Content |
|---|---|---|
| checkin.go | 119 | `if false { // CN: never check-in or auto-claim trial. Lifecycle only.` |
| panel.go | 104 | `if false {` (inside fetchCredits branch) |
| panel.go | 117 | `if false {` (inside cache-read branch) |
| credits_handler.go | 170 | `if false {` (inside single-account credits query) |

**Action: Delete all four.** They contain dead code that was meant to be removed when the CN/trial logic was stripped. The `if false` pattern was used to preserve the code "just in case" but it's confusing and will never execute.

---

## Consolidated Action List

### High Priority (dead code — delete)
1. `accountData` struct (main.go:477)
2. `authStateData` struct (main.go:483)
3. `upstreamBaseFor()` (main.go:539)
4. `backendHeaders()` (main.go:548)
5. `billingCall()` + `billingCallOnce()` + `isTransientBillingErr()` + `billingRetryDelays` (billing.go:34-54, management.go:70)
6. `doJSON()` (oauth.go:34)
7. `billingBaseFor()` (billing.go:18)
8. `deleteAuthFileAt()` (authfile.go:265)
9. `aggregateSSEWithCollector()` (stream.go:228)
10. `cachedCheckinToday()` (cache.go:175)
11. `nextKeepaliveTime()` (keepalive.go:264)
12. `qoderDecode()` (encoding.go:56)
13. `staticModels` + `qwModel` type (body.go:21-47)
14. `resourcePackage` + `packageRemainUsed()` (management.go:79, billing.go:165)
15. All four `if false {}` blocks (checkin.go:119, panel.go:104, panel.go:117, credits_handler.go:170)
16. Orphan comment `hostAuthGetFull` (authfile.go:298)

### Medium Priority (WorkBuddy remnants — rename/rewrite)
17. Rename `wb_call_host` → `qw_call_host` (main.go:47, 50, 223, 229)
18. Rename `wbRegistration()` → `pluginRegistration()` (main.go:335)
19. Rename `wbModels()` → `staticModels()` or `qwStaticModels()` (models.go:22)
20. Rename `wbAccount` → `dashboardAccount` (panel.go:15)
21. Rewrite "WorkBuddy skeleton" comments (main.go:538, 545)
22. Rewrite "WorkBuddy helper" comment (oauth.go:371)
23. Rewrite "workbuddy-*.json" comment (host_auth.go:49)
24. Rewrite "WorkBuddy three-phase" comment (checkin.go:186)
25. Rewrite "Buddy-gas-station" comment (management.go:18)
26. Rewrite "codebuddy.cn" comment (management.go:67)
27. Remove `loginCtx.client` field (main.go:108-112)
28. Fix keepalive.go header comment (keepalive.go:11-13)
29. Fix checkin.go line 4 comment ("CN accounts are excluded")

### Medium Priority (CN-only simplification)
30. Remove global/intl branches in `summarizeCredits()` (panel.go:196-249)
31. Simplify `displayNote()` region logic (policy.go:143-149)
32. Simplify `labelForAuth()` region logic (policy.go:176-186)
33. Remove dead `lifecycleDisable` return in `lifecycleActionFor()` (policy.go:124)
34. Clean panel.html global/国际版 handling (panel.html:63, 663, 674)

### Needs Review (architectural)
35. **`lifecycleActionFor` CN=delete vs disable** (policy.go:121-123) — QoderWork has no trial packs; deleting auth on exhaust forces PAT re-import. Consider using `lifecycleDisable` for CN too, so check-in can restore credits.
36. **`callModelsAPI` missing COSY signing** (models.go:130) — dynamic model list never works; always falls back to static.
37. **`refreshCall`/`exchangePATForJobToken`/`refreshJobToken` bypass hostHTTPDo** (keepalive.go:80, oauth.go:109, 126) — compliance gap.
38. **`resolveAuthIndexAndID` redundant hostAuthList calls** (lifecycle.go:406-443) — efficiency.

---

*End of report.*
