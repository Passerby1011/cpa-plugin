# GeminiAPI — a CPA plugin for Gemini Web + AI Studio

A single CLIProxyAPI plugin (provider id `geminiapi`) that **hosts, verifies and
supervises a local OpenAI-compatible backend** and publishes its models under a
Gemini namespace.

Unlike a config-only provider, this plugin owns a real process:

```
CPA  ──►  geminiapi plugin (.so)
              │
              ├── supervises: <data-dir>/bin/gemini-web2api-go  (pinned, sha256-verified)
              │        └── listens on 127.0.0.1:<port>
              │
              └── publishes models as  ai-studio/<model>
```

## Why one provider with a namespace prefix

A native plugin declares exactly one `executor.identifier`, so it cannot declare
two providers the way a JS extension can. This plugin therefore serves **one
provider** and expresses its backends as **model-id namespaces**:

| Namespace | Model id | Backend |
|---|---|---|
| `ai-studio` | `ai-studio/gemini-2.5-pro` | this card |
| `gemini-web` | `gemini-web/<model>` | its own upstream card |

Multi-segment ids are supported (`commandcode/deepseek/...` already proves the
host accepts them), and routing is decided by the id alone — no hidden state.

> This card ships `ai-studio` only. A `gemini-web/<model>` request is refused
> with a clear error rather than silently answered by the wrong backend.

## The managed backend

| Property | Value |
|---|---|
| Version | `4.28.0-magpie.1` (pinned in `internal/backend/pin.go`) |
| Source | `BUKOWSKIREAL/magpie-gemini-web`, tag `backend-v4.28.0-magpie.1` |
| Assets | linux/darwin/windows × amd64/arm64, each pinned by sha256 |
| Default port | `2048`, resolved to the next free port on a conflict |
| Listen address | `127.0.0.1` **only** (enforced) |

Sequence on first use:

1. **Download** the pinned asset (resumable, stall-timed, optional proxy).
2. **Verify sha256** against the compile-time pin. *A mismatch never reaches the
   launch path and never starts.*
3. **Create** `config.json` (loopback-only) and `secrets.json` (generated key +
   admin token, mode 0600).
4. **Spawn** the process into a **Job Object** (Windows, kill-on-close) or a
   **process group** (Unix) so the whole tree dies with CPA.
5. **Health probe**: `GET /` for the version, `GET /v1/models` with the setup's
   key to prove the listener is *our* backend and not a stranger on the port.
6. **Supervise**: crash restarts (up to 3), and a PID file plus an identity
   check on every later reclaim.

### Orphan safety (the main risk this plugin guards against)

The backend is a long-lived child of the CPA host. If CPA exits and the child
survives, it keeps a port busy and a Google session alive.

Three layers prevent that:

- **OS-level**: `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` on Windows / `setpgid` +
  `kill(-pgid)` on Unix. The OS tears the tree down even on a hard kill.
- **Plugin-level**: `plugin.shutdown` stops the child explicitly.
- **Next-run**: the PID file is reclaimed on startup — but **only** after a
  PID **and** executable-path identity check. A live PID belonging to another
  program is reported and *never* killed.

## Security boundaries

- The launch path is the only executable this plugin ever runs, and it must
  hash-match the pin at launch time.
- The child's argv is a closed shape: `--config <path> --db <path>`. No
  configuration value can inject a flag.
- Credentials travel in the **environment**, not argv (argv is world-readable).
- `HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY` are **stripped** from the child's
  environment, so the host's proxy can never route the plugin's own localhost
  calls.
- Management action `backend/stop` refuses to act when the recorded port is
  owned by a foreign service.

## Accounts, and the caveat you must read

AI Studio needs a **Google sign-in**, which is a Cookie, not an API key. CPA has
no browser, so this plugin takes the manual path:

**F12 → Network → any `gemini.google.com` request → Request Headers →
copy the whole `cookie` line → paste it into the plugin page.**

The plugin validates it (naming the cookies still missing), forwards it once to
the **local backend's own admin API over loopback**, and asks the backend to
probe the sign-in. A cookie that fails the probe is deleted again.

- The Cookie is stored **only** in the backend's own database inside `data-dir`.
  It is never written to a CPA config, never logged, never returned in a
  response.
- Use a **private window**. The backend rotates the session, which signs the
  browser it came from out of Google.

> ### ⚠️ Multiple accounts ≠ multiple selectable accounts
>
> Several CPA auth records are several **scheduling slots**; which Google
> account actually serves a request is decided by the **backend's own account
> pool**, not by the auth record CPA picked. The plugin cannot target a specific
> Google account and cannot read account emails or cookies.
>
> The panel and the status payload both say this, because an operator who
> assumes otherwise will report "the wrong account answered" as a bug.

## Models

- Catalog comes from the backend's `/v1/models` and is refreshed every 10
  minutes, plus immediately after a start or an account import.
- Reasoning levels `minimal` / `low` / `medium` / `high` are published exactly
  as the catalog declares them.
- Image **input** is on by default (a multimodal chat model reads images).
- Models named for a non-chat modality (image / tts / audio / live / video /
  music / canvas / research) are excluded, as are `-build` models and
  `assistant_route` models.
- **Image generation models are NOT published by default.** CPA has no image
  generation route, so a chat request naming one could only fail. `ai-studio.
  allow-image-models: true` publishes them anyway, marked as image-output.

## Timeouts

| Knob | Default | Meaning |
|---|---|---|
| `request-timeout` | `30m` | the WHOLE request |
| `stream-idle-timeout` | `5m` | the gap BETWEEN chunks, independently configurable |
| `model-timeout` | `15s` | catalog / health / account reads |

A productive long reasoning stream is never killed by the idle budget; a
silently stalled one is stopped long before the total deadline. A client
disconnect cancels the request context, which propagates to the backend through
the host transport — the backend stops working on a request nobody will read.

## Configuration

```yaml
plugins:
  configs:
    geminiapi:
      data-dir: ""                  # default: <user config dir>/cpa-plugin/geminiapi
      proxy: ""                     # download + admin API only; never localhost
      request-timeout: 30m
      stream-idle-timeout: 5m
      model-timeout: 15s
      models:
        allow: []                   # empty = publish everything
        deny: []
      ai-studio:
        base-url: ""                # set = EXTERNAL backend, plugin does not manage
        api-key: ""                 # required with base-url, refused without it
        proxy: ""                   # what the BACKEND uses to reach Google
        allow-image-models: false
        port: 2048
        context-limit: 0            # 0 = use the backend's value
        output-limit: 0
```

`ai-studio.base-url` points at an already-running OpenAI-compatible backend.
When it is set the plugin proxies and never installs, starts or stops anything —
and account import is refused, because it has no admin credential for somebody
else's service.

## Install

```bash
cd geminiapi
make build      # CGO_ENABLED=1 go build -buildmode=c-shared -o geminiapi.so .
make test       # go test -race -count=1 ./...
make lint       # gofmt + go vet
```

Copy `geminiapi.so` into the CPA plugins directory, add a `plugins.configs.
geminiapi` node, restart CPA, then open:

```
/v0/resource/plugins/geminiapi/panel
```

## Verification status

| Area | Status |
|---|---|
| Pin table matches the upstream manifest | unit-tested |
| sha256 mismatch refuses to install/start | unit-tested **and** proven by negative control |
| A previously verified binary survives a failed replacement | unit-tested |
| PID identity check refuses a foreign process | unit-tested **and** proven by negative control |
| Port conflict picks and persists the next free port | unit-tested |
| Models fetch failure keeps the previous snapshot | implemented, unit-tested |
| `ai-studio/` prefix routing | unit-tested **and** proven by negative control |
| Cookie never reaches a log, response or config | unit-tested |
| Stream idle timeout is independent of the total timeout | unit-tested |
| Panel embedded and served with a self-contained CSP | unit-tested |
| Real download → start → health → stop on this host | see `docs/real-backend-verification.md` |

Anything not listed as verified above is **not** claimed as verified.

## Licence

MIT. The hosted backend is a separate work with its own licence — see
`LICENSES/` and the upstream release.
