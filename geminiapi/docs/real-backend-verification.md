# Real-backend verification record

This file records what was actually executed, on which host, and with what
observable result. Anything not listed here is not claimed as verified.

Two backends are pinned and published, and **each one has its own record** below:

| Namespace | Backend | Record |
|---|---|---|
| `gemini-web` | gemini-web2api-go `4.28.0-magpie.1` | C1 run (2026-11-13) + re-run after the split |
| `ai-studio` | AIStudio2API `0.3.5-magpie.1` | first run in the C2 card (2026-11-13), including a failed start and its fix |

Run with:

```bash
# both backends (the default; a record cannot accidentally cover only one)
GEMINIAPI_REAL_BACKEND=1 go test -count=1 -run TestRealBackend -v ./internal/backend/

# one backend, with a data directory that survives the run so an 80MB download
# is reused instead of fetched again (the child's log also outlives the failure)
GEMINIAPI_REAL_BACKEND=1 GEMINIAPI_REAL_FLAVOR=ai-studio \
GEMINIAPI_REAL_DATA_DIR=/tmp/geminiapi-real \
  go test -count=1 -run 'TestRealBackend' -v ./internal/backend/
```

The opt-in env var exists so the default `make test` gate stays
offline-deterministic: a test that downloads 18 MB (gemini-web) or 37 MB
(AI Studio's archive, 81 MB unpacked) must never be part of the every-commit
gate. `GEMINIAPI_REAL_FLAVOR` selects `gemini-web` or `ai-studio`; leaving it
unset runs both.

## Platform caveat — read this first

Every recorded run happened inside a **Linux/amd64** container (`golang:1.26`)
because that is where the repository's build/test toolchain lives. The
Windows-only code paths (`internal/backend/process_windows.go`: Job Object
creation, `QueryFullProcessImageName`, `CREATE_NEW_PROCESS_GROUP`) **compiled and
vet-passed** in that run but were **not** exercised by a real spawn. They are
covered only by the platform-neutral unit tests that inject the identity lookup.

| Path | Verified by |
|---|---|
| gemini-web: download + sha256 + spawn + health + stop (Unix process group) | real execution, record below |
| AI Studio: archive download + archive sha256 + executable sha256 + spawn + health + stop | real execution, record below |
| AI Studio: a bad pin is refused on genuine bytes (fresh download, 52s) | real execution, record below |
| AI Studio: its own 503 `service_stopped` reads as *up, no account* | real execution, record below |
| Two backends, separate data directories and ports, in one process | real execution, record below |
| Job Object orphan kill (Windows) | **not verified** — compile/vet only |
| `QueryFullProcessImageName` identity check (Windows) | **not verified** — the identity logic itself is unit-tested via an injected lookup |

## Record (C1): first attempt failed, and that is part of the record

The first real run downloaded 3,668,786 bytes and then failed:

```
install failed: read download stream: unexpected EOF
```

Measured throughput on that link was ~46 KB/s for an 18,473,120-byte asset, so a
reset mid-transfer was expected rather than exceptional. The downloader was
changed to resume with `Range` headers (12 attempts, per-read stall budget).
Resume is safe precisely because the digest is computed over the whole
concatenated file — a corrupted middle byte still fails the gate.

## Record (C1): install → start → health → stop (passing, gemini-web)

```
platform pin: asset=gemini-web2api-go_v4.28.0-magpie.1_linux_amd64
              sha256=ec3ee723dbd6f99b6e874eaf2aab23b7c8afb3ab5c2a3498b51fea27b1f42ded
INSTALL ok: path=/tmp/.../geminiapi/bin/gemini-web2api-go
            size=18473120 bytes  elapsed=1m21.939s
SHA256 verified: ec3ee723dbd6f99b6e874eaf2aab23b7c8afb3ab5c2a3498b51fea27b1f42ded
START ok: origin=http://127.0.0.1:24051  base_url=http://127.0.0.1:24051/v1  elapsed=3.336s
STATUS: state=running  port=24051  pid=19091  pid_verified=true
        health_state=up  health_reason=""  version="4.28.0-magpie.1"
models fetch ok
STOP ok: elapsed=93ms
ORPHAN CHECK ok: process gone, pid file removed, port released
--- PASS: TestRealBackendInstallStartStop (85.40s)
```

What each line proves:

| Line | Proves |
|---|---|
| `size=18473120` | the real asset downloaded completely |
| `SHA256 verified: ec3ee723…` | the bytes are exactly the pinned release |
| `origin=http://127.0.0.1:24051` | the backend bound loopback only, on the recorded port |
| `pid=19091 pid_verified=true` | the recorded PID really is this data directory's binary |
| `health_state=up` + `version="4.28.0-magpie.1"` | both health checks passed, and `/v1/models` accepted the setup's key |
| `models fetch ok` | the OpenAI surface answered a real catalog read |
| `STOP ok: elapsed=93ms` | the process group was torn down promptly |
| `ORPHAN CHECK ok` | process gone, pid file removed, port released — no orphan |

## Record (C1): a bad pin is refused on real bytes (passing, gemini-web)

Same genuine asset, deliberately wrong digest:

```
BAD-PIN refused as expected: sha256 mismatch: gemini-web2api-go 4.28.0-magpie.1
  expected 000000…0000, downloaded bytes hash to ec3ee723…f42ded
  (release https://github.com/BUKOWSKIREAL/magpie-gemini-web/releases/tag/backend-v4.28.0-magpie.1)
--- PASS: TestRealBackendRefusesABadPin (60.75s)
```

This is the important negative control: the download **succeeded**, the bytes
were **genuine**, and the install was still **refused** and nothing reached the
launch path.

## Record (C2): both backends in ONE run (passing)

`GEMINIAPI_REAL_BACKEND=1 go test -run TestRealBackend -v ./internal/backend`
with no `GEMINIAPI_REAL_FLAVOR`, so both flavors ran in the same process, each
with its own data directory (`/realdata/gemini-web`, `/realdata/ai-studio`):

```text
=== RUN   TestRealBackendInstallStartStop/gemini-web
    INSTALL ok: path=/realdata/gemini-web/bin/gemini-web2api-go size=18473120 bytes elapsed=38.407s
    SHA256 verified (asset pin ec3ee723dbd6…, executable ec3ee723dbd6…)
    START ok: origin=http://127.0.0.1:25486 base_url=http://127.0.0.1:25486/v1 elapsed=4.187s
    STATUS: state=running port=25486 pid=3999 pid_verified=true
            health_state=up health_reason="" version="4.28.0-magpie.1"
    models fetch ok
    STOP ok: elapsed=280ms
    ORPHAN CHECK ok: process gone, pid file removed, port released
=== RUN   TestRealBackendInstallStartStop/ai-studio
    INSTALL ok: path=/realdata/ai-studio/package/aistudio2api size=81097312 bytes elapsed=555ms
    SHA256 verified (asset pin 1c848849b4eb…, executable f5e523f13f6b…)
    START ok: origin=http://127.0.0.1:24572 base_url=http://127.0.0.1:24572/v1 elapsed=1.763s
    STATUS: state=running port=24572 pid=4008 pid_verified=true
            health_state=up health_reason="no-account" version="" accounts_total=0 accounts_known=true
    models fetch (expected with no account yet): backend returned HTTP 503 for /v1/models
    STOP ok: elapsed=3.491s
    ORPHAN CHECK ok: process gone, pid file removed, port released
--- PASS: TestRealBackendInstallStartStop (53.91s)
--- PASS: TestRealBackendRefusesABadPin (124.83s)
--- PASS: TestRealBackendStatusIsSecretFree (11.59s)
ok  github.com/hex-ci/cpa-plugin/geminiapi/internal/backend  190.330s
```

`INSTALL ok … elapsed=555ms` for AI Studio is the reuse path: a verified binary
was already in the data directory (its digest is re-checked — see the `SHA256
verified` line). The download path itself is exercised by the AI Studio bad-pin
record below, which re-fetched the 37,029,867-byte archive.

What this proves:

| Observation | Proves |
|---|---|
| two different ports (`25486`, `24572`) and two data directories in one run | the two `backend.Manager` instances are independent, not one manager wearing two names |
| `gemini-web` `health_state=up` + `models fetch ok` | gemini-web2api-go really answers `/v1/models` with its key |
| `ai-studio` `health_state=up health_reason="no-account"` | AIStudio2API is genuinely running; its empty account pool is reported as its own state instead of "foreign listener" |
| `accounts_total=0 accounts_known=true` (ai-studio) vs `accounts_known=false` (gemini-web) | each namespace reads its own backend's account surface, and those surfaces differ |
| `STOP ok` + `ORPHAN CHECK ok` on both | process-group teardown, pid file removal and port release work for both backends |

## Record (C2): AI Studio's first start FAILED, and why

The first `ai-studio` run in this card produced:

```text
ai-studio start failed: http://127.0.0.1:24572 answers with another service's key;
  the backend may have bound the wrong port — see /realdata/ai-studio/backend.log
```

Two independent causes were found in the backend's own log, not guessed:

1. **The service ignored the plugin's settings.** `backend.log` said
   `Invalid setting value … key=LISTEN_ADDR value=""` /
   `AISTUDIO_AUTH_STATES value=""`, and the service listened on its defaults
   (2048/2050) while the plugin probed the port it had allocated. AIStudio2API
   reads settings from the **process environment first** and only then from
   `.env`; an inherited-but-empty variable is not "unset". Fixed by delivering
   the same `.env` values through the child's environment
   (`Flavor.ServiceEnv`), with the resolved port authoritative.
2. **The probe misread the backend's own 503.** With the port correct, the next
   run still failed, because `/v1/models` answers 503 while no account exists —
   and the message is **localised**. The body observed on the real backend:

   ```json
   {"error":{"message":"Service unavailable: AIStudio2API 生成服务已停止：在管理页面点击“启动服务”，
            或在服务配置中开启“自动启动生成服务”","type":"server_error","code":"service_stopped"}}
   ```

   The first matcher looked for the English phrase `generation service` (the
   wording in the backend's *log*, not in its *response*) and classified the
   backend as a stranger, so the manager killed a healthy process. Fixed by
   matching the machine-readable `code` (`service_stopped`) with a bilingual
   text fallback; an **unrecognised** 503 still reads as `foreign`
   (`TestProbeForeign503StaysForeign`).

Both fixes are unit-tested with the exact observed bodies
(`TestProbeStudioServiceStoppedIsUpWithNoAccount`,
`TestStudioGenerationStoppedRecognition`).

**This is the honest limit of the `ai-studio` leg:** without a Google login there
is no eligible account, so AIStudio2API's generation service refuses to start
(`Generation service startup failed | Error=No eligible AI Studio accounts`) and
`/v1/models` stays 503. The record above is therefore `up + no-account + 0
models`, not a working generation path.

## Record (C2): a bad pin is refused on real bytes (AI Studio archive)

```text
BAD-PIN refused as expected: sha256 mismatch: AIStudio2API 0.3.5-magpie.1
  expected 000000…0000, downloaded bytes hash to 1c848849b4eb…
  (release https://github.com/BUKOWSKIREAL/magpie-gemini-web/releases/tag/studio-v0.3.5-magpie.1)
--- PASS: TestRealBackendRefusesABadPin/ai-studio (52.62s)
```

A fresh 37,029,867-byte download happened inside that test (52.62s), it hashed to
the pinned archive digest, and the install was refused because the pin said
zero. Nothing reached the launch path.

## Which account surface belongs to which backend

The card that requested this work assumed the pairing `ai-studio` →
`/admin/api/cookies`. That assumption was measured against both real backends
and is **inverted**; the code follows the evidence:

```text
$ grep -a -o -e "/admin/api/[a-z0-9/_-]*" -e "/api/accounts[a-z0-9/_-]*" \
    /realdata/ai-studio/package/aistudio2api | sort -u
/api/accounts
/api/accounts/
/api/accounts/import/browsers
/api/accounts/import/chrome
/api/accounts/import/cookies/discover
/api/accounts/tasks
/api/accounts/tasks/
/api/auth/logout
/api/auth/session
```

AIStudio2API has `/api/accounts*` and a session login; it has **no**
`/admin/api/cookies`. `gemini-web2api-go` does:

```text
$ grep -rn "admin/api" D:/hermes-data/temp/magpie-gemini-web/lib/setup.mjs
lib/setup.mjs:158:    res = await fetchFn(`${setup.origin}/admin/api/${path}`, {
```

So the plugin routes:

| Namespace | Endpoint | Plugin-side behaviour |
|---|---|---|
| `gemini-web` | `POST /admin/api/cookies` (+ `/cookies/<id>/check`) | operator pastes a cookie; the backend probes the sign-in |
| `ai-studio` | `POST /api/accounts/tasks` (its own browser login task) | operator clicks "start sign-in"; the backend opens its own window |

A cookie sent to `ai-studio`, or a login task sent to `gemini-web`, is refused
with an error rather than forwarded to the wrong service.

## What is NOT verified

- Windows Job Object orphan kill under a real CPA exit (compile/vet only).
- The panel's rendering in a real CPA Management Center (the page is unit-tested
  for content and CSP, not for visual layout).
- **A full generation request through either backend with a real Google
  account.** Both need a live Google login, which is out of scope for an
  automated gate:
  - `ai-studio`: the browser login task (`/api/accounts/tasks`) was **never
    driven to completion**, because that requires a real Google sign-in in the
    backend's own window. Account import against the real backend is therefore
    **not verified**; the record above stops at "backend up, account pool empty,
    503 service_stopped".
  - `gemini-web`: the cookie import path is unit-tested end-to-end against a
    scripted host, including its "cookie never reaches a log/response"
    guarantee, but **no real Google sign-in was performed** in this run either.
- A real `/v1/chat/completions` (or streaming) round trip through either backend
  with an account. The executor and stream handling are unit-tested against
  scripted transports; no real token was ever produced.
- macOS and Windows **arm64** assets: pinned from the upstream manifest and
  unit-tested, never downloaded or executed on this host.
