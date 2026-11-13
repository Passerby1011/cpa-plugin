# Real-backend verification record

This file records what was actually executed, on which host, and with what
observable result. Anything not listed here is not claimed as verified.

Run with:

```bash
GEMINIAPI_REAL_BACKEND=1 go test -count=1 -run TestRealBackend -v ./internal/backend/
```

The opt-in env var exists so the default `make test` gate stays
offline-deterministic: a test that downloads 18 MB must never be part of the
every-commit gate.

## Platform caveat — read this first

The recorded run happened inside a **Linux/amd64** container (`golang:1.26`)
because that is where the repository's build/test toolchain lives. The
Windows-only code paths (`internal/backend/process_windows.go`: Job Object
creation, `QueryFullProcessImageName`, `CREATE_NEW_PROCESS_GROUP`) **compiled and
vet-passed** in that run but were **not** exercised by a real spawn. They are
covered only by the platform-neutral unit tests that inject the identity lookup.

| Path | Verified by |
|---|---|
| download + sha256 + spawn + health + stop (Unix process group) | real execution, record below |
| Job Object orphan kill (Windows) | **not verified** — compile/vet only |
| `QueryFullProcessImageName` identity check (Windows) | **not verified** — the identity logic itself is unit-tested via an injected lookup |

## Record: first attempt failed, and that is part of the record

The first real run downloaded 3,668,786 bytes and then failed:

```
install failed: read download stream: unexpected EOF
```

Measured throughput on that link was ~46 KB/s for an 18,473,120-byte asset, so a
reset mid-transfer was expected rather than exceptional. The downloader was
changed to resume with `Range` headers (12 attempts, per-read stall budget).
Resume is safe precisely because the digest is computed over the whole
concatenated file — a corrupted middle byte still fails the gate.

## Record: install → start → health → stop (passing)

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

## Record: a bad pin is refused on real bytes (passing)

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

## What is NOT verified

- Windows Job Object orphan kill under a real CPA exit (compile/vet only).
- The panel's rendering in a real CPA Management Center (the page is unit-tested
  for content and CSP, not for visual layout).
- A full request through CPA with a real Google account. That needs a live
  Google cookie, which is out of scope for an automated gate: the cookie import
  path is unit-tested end-to-end against a scripted host, including its
  "cookie never reaches a log/response" guarantee, but no real Google sign-in
  was performed in this run.
- Account import against a real AI Studio backend (`aistudio2api`). This card
  targets `gemini-web2api-go`; the studio side is the downstream card.
