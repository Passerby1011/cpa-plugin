# GeminiAPI — a CPA plugin for Gemini Web + AI Studio

A single CLIProxyAPI plugin (provider id `geminiapi`) that **hosts, verifies and
supervises a local OpenAI-compatible backend** and publishes its models under a
Gemini namespace.

Unlike a config-only provider, this plugin owns a real process:

```
CPA  ──►  geminiapi plugin (.so)
              │
              ├── supervises  <data-dir>/ai-studio/package/aistudio2api      (AIStudio2API 0.3.5-magpie.1, archive pinned + executable pinned)
              │      └── listens on 127.0.0.1:<port>  ──►  publishes  ai-studio/<model>
              │
              └── supervises  <data-dir>/gemini-web/bin/gemini-web2api-go    (gemini-web2api-go 4.28.0-magpie.1)
                     └── listens on 127.0.0.1:<port>  ──►  publishes  gemini-web/<model>
```

Two backends, two pins, two processes. Nothing is shared between them except
this plugin's provider id.

## Why one provider with a namespace prefix

A native plugin declares exactly one `executor.identifier`, so it cannot declare
two providers the way a JS extension can. This plugin therefore serves **one
provider** and expresses its backends as **model-id namespaces**:

| Namespace | Model id | Backend (pinned independently) |
|---|---|---|
| `ai-studio` | `ai-studio/gemini-2.5-pro` | **AIStudio2API** `0.3.5-magpie.1` |
| `gemini-web` | `gemini-web/gemini-2.5-pro` | **gemini-web2api-go** `4.28.0-magpie.1` |

Multi-segment ids are supported (`commandcode/deepseek/...` already proves the
host accepts them), and routing is decided by the id alone — no hidden state.

Both namespaces are published by this build. Each one owns its own pin table,
its own data directory (`<data-dir>/ai-studio`, `<data-dir>/gemini-web`), its
own port, its own PID file and its own process group, so a backend that crashes
or is stopped cannot affect the other. A `gemini-web/<model>` request never
travels through the AI Studio backend, and vice versa: an id whose prefix is
not one of the two is refused with a clear error.

## The managed backends

| Property | `ai-studio` | `gemini-web` |
|---|---|---|
| Backend | AIStudio2API `0.3.5-magpie.1` | gemini-web2api-go `4.28.0-magpie.1` |
| Source | `BUKOWSKIREAL/magpie-gemini-web`, tag `studio-v0.3.5-magpie.1` | same repo, tag `backend-v4.28.0-magpie.1` |
| Assets | darwin/linux/windows × amd64/arm64 **archives**, each pinned by sha256 **and** by the sha256 of the executable inside | linux/darwin/windows × amd64/arm64, each pinned by sha256 |
| Default port | `2049` (its own default is 2048; see below) | `2048` |
| Listen address | `127.0.0.1` **only** (enforced) | `127.0.0.1` **only** (enforced) |
| Settings source | `.env` in its data directory **and** the process environment (upstream reads the environment first) | environment only, for its two credentials |

Both pin tables live in `internal/backend/pin.go` / `internal/backend/flavors.go`
and are copied from the upstream release manifests; the digests are not restated
anywhere else.

AI Studio's assets are archives, so install verifies twice: the archive digest
before unpacking, and the executable digest after (the executable digest is
checked again before every launch). gemini-web2api-go ships a bare executable,
so one digest covers it.

`2049` for AI Studio exists because both backends default to 2048: a fresh
install of both on one machine must not race for the same port. A port already
recorded in a data directory always wins afterwards.

Sequence on first use:

1. **Download** the pinned asset (resumable, stall-timed, optional proxy).
2. **Verify sha256** against the compile-time pin. *A mismatch never reaches the
   launch path and never starts.* For AI Studio the executable inside the
   archive is verified too, before and after unpacking.
3. **Create** the backend's own state in its data directory: `config.json`
   (loopback-only) plus `secrets.json` (generated key + admin token, mode 0600)
   for gemini-web; `config.json` plus the `.env` AI Studio reads for AI Studio.
4. **Spawn** the process into a **Job Object** (Windows, kill-on-close) or a
   **process group** (Unix) so the whole tree dies with CPA.
5. **Health probe** per backend: gemini-web is asked for `GET /` (version +
   models) and then `GET /v1/models` with the setup's key; AI Studio is asked
   for `GET /api/status` (401 proves the admin surface is there) and then
   `GET /v1/models` with the key. A 503 carrying AI Studio's own
   `service_stopped` code is reported as *up, no account* — not as a stranger
   on the port, and not as "still starting".
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

The two backends take accounts in **different ways**, because they are different
products:

| Namespace | How an account is added | Where it lands |
|---|---|---|
| `gemini-web` | paste a Google `cookie` header into the plugin page; the plugin forwards it over loopback to the backend's own `POST /admin/api/cookies` and asks that backend to probe the sign-in | gemini-web2api-go's own database inside its data directory |
| `ai-studio` | click "start sign-in"; the plugin asks AIStudio2API (over loopback) to begin its own browser login task (`/api/accounts/tasks`), and the operator finishes the Google sign-in in the backend's own window | AIStudio2API's own auth state inside its data directory |

AIStudio2API has **no cookie endpoint** and gemini-web2api-go has no browser
login: pasting a cookie into `ai-studio`, or expecting `gemini-web` to open a
login window, is refused with an error rather than sent to the wrong service.

**F12 → Network → any `gemini.google.com` request → Request Headers →
copy the whole `cookie` line → paste it into the plugin page** (gemini-web only).

The plugin validates the cookie first (naming the cookies still missing), and a
cookie that fails the backend's own probe is deleted again.

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

The two backends describe their catalogs differently, so each namespace is built
with its own rules — one ruleset for both is what shipped an empty namespace:

- **`ai-studio`** (AIStudio2API): the catalog carries a **capability map** per
  model. A model is published only when it declares `chat_model`, is not an
  `assistant_route`, advertises `generateContent`, carries no non-text
  capability (speech / live / transcription) and is covered by an enabled
  account when the backend reports channel coverage. Reasoning levels come from
  the model's own `thinking_levels`. **Image generation models are NOT published
  by default** (CPA has no image route, so a chat request naming one could only
  fail); `ai-studio.allow-image-models: true` publishes them, marked as
  image-output.
- **`gemini-web`** (gemini-web2api-go): the catalog is **OpenAI-shaped**
  (`id` / `object` / `owned_by` and nothing else) — no capability map. Requiring
  `chat_model` here would publish zero models, so an entry with an id is
  published; only ids *named* for a non-chat modality (image / tts / audio /
  live / video / music / canvas / research) are dropped. The `-thinking` and
  `-thinking-medium` twins the backend lists are folded into their base model as
  the `high` and `medium` reasoning levels (`low` is the plain id).
- Either namespace: the catalog comes from the backend's `/v1/models`, refreshes
  every 10 minutes, and refreshes immediately after a start or an account
  import. **A namespace that ends up publishing zero models is reported as an
  error naming the cause** (empty backend list, or which filter removed what) —
  the panel and the `/models` response show it instead of a silent empty table.

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
        port: 2049
        context-limit: 0            # 0 = use the backend's value
        output-limit: 0
      gemini-web:
        base-url: ""                # same shape, independent of ai-studio
        api-key: ""
        proxy: ""
        allow-image-models: false   # gemini-web publishes none either
        port: 2048
        context-limit: 1000000      # used only when the backend reports no limit
        output-limit: 8192
```

Each namespace is configured independently: an external `base-url` on one
namespace leaves the other managed, and the plugin keeps one data directory per
namespace (`<data-dir>/ai-studio`, `<data-dir>/gemini-web`).

`base-url` points at an already-running OpenAI-compatible backend. When it is
set the plugin proxies and never installs, starts or stops anything — and
account import is refused, because it has no admin credential for somebody
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
| Two pin tables, one per backend, copied from the upstream manifests | unit-tested |
| sha256 mismatch refuses to install/start (gemini-web and AI Studio) | unit-tested **and** proven by negative control on real bytes |
| AI Studio: the archive **and** the executable inside it are verified | unit-tested **and** verified on real bytes (linux/amd64) |
| A previously verified binary survives a failed replacement | unit-tested |
| PID identity check refuses a foreign process | unit-tested **and** proven by negative control |
| Each namespace gets its own data directory, port and PID file | unit-tested |
| Port conflict picks and persists the next free port | unit-tested |
| Models fetch failure keeps the previous snapshot | implemented, unit-tested |
| Capability rules publish the Studio shape; id-only rules publish the OpenAI shape | unit-tested **and** proven by negative control (the OpenAI shape published 0 models before the split) |
| A namespace that publishes nothing returns a readable error naming the cause | unit-tested **and** panel-rendered |
| AI Studio's own 503 `service_stopped` reads as *up, no account* (not "foreign") | unit-tested **and** verified on a real backend |
| An unrecognised 503 on the port still reads as *foreign* | unit-tested |
| Cookie never reaches a log, response or config | unit-tested |
| Account import uses each namespace's own surface (`/admin/api/cookies` vs `/api/accounts/tasks`) | unit-tested; the endpoints themselves verified against the real backends |
| Stream idle timeout is independent of the total timeout | unit-tested |
| Panel embedded and served with a self-contained CSP; both namespaces rendered | unit-tested |
| Real download → start → health → stop, **both** backends | see `docs/real-backend-verification.md` |

Anything not listed as verified above is **not** claimed as verified.

## Licence

MIT. The hosted backend is a separate work with its own licence — see
`LICENSES/` and the upstream release.
