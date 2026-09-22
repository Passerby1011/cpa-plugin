# WorkBuddy Global Plugin for CLIProxyAPI

A [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) plugin that
provides **Tencent WorkBuddy international** (`www.workbuddy.ai`, the Global
realm) as a native OAuth provider: per-account dynamic model discovery,
streaming execution, credit-aware scheduling, one-shot trial claim, and a
built-in management dashboard.

This plugin targets the international service only. Credentials that turn out
to belong to the CN realm are still detected and routed to the right gateway,
so importing one will not break anything — but the plugin's defaults, panel and
activities are all shaped for `workbuddy.ai`.

[中文文档 → README_CN.md](README_CN.md)

## Features

- **OAuth login** — multi-account `workbuddy-global-<uid>.json` auth files via the
  host's auth store. `oauth_client_mode` picks the login channel and defaults to
  `workbuddy-ai` (international desktop: `platform=workbuddy-ai` on
  `www.workbuddy.ai`); `workbuddy` (CN desktop: `platform=workbuddy` on
  `copilot.tencent.com`) and `cli` remain available for CN accounts. The mode
  only shapes the login flow — the issued token's domain decides routing
  afterwards, so a CN credential still reaches the CN gateway without extra
  config.
- **Model catalog**: by default the plugin discovers and caches each
  authenticated account's model entitlements. An optional authoritative YAML
  list can replace WorkBuddy discovery. Every model field comes from the
  WorkBuddy catalogue (`/v3/config`), and the plugin never talks to a
  third-party site. Host-side `oauth-model-alias` / `oauth-excluded-models`
  config still applies.
- **Executor** — OpenAI-compatible chat completions, both streaming (real SSE
  via `host.stream.emit`) and non-streaming (SSE folded into a single
  completion). `tool_choice` normalization, Claude Code template sanitization,
  and per-realm system-message injection are built in.
- **Credit lifecycle** — Global accounts are deleted when their credits are
  known to be exhausted (one-shot trial quota). A CN account that ends up in
  the store is `disabled` instead and re-enabled once its credits recover.
  Hard credit errors from the executor trigger an immediate reconcile.
- **Trial claim** — Global accounts can claim the one-time 250-credit expert
  trial pack from the panel.
- **Dashboard** — embedded panel at `/v0/resource/plugins/workbuddy-global/panel`
  with credits progress bars, plan badges, exhausted/disabled flags, region
  filter, and credential import.
- **Scheduler** (optional) — `scheduler_mode: credits` makes the plugin pick
  the panel-selected account; `off` (default) defers to CPA's built-in
  scheduler entirely.
- **Usage forwarding** — implements `UsagePlugin`; every request's usage
  record is forwarded to a configurable CPAMP endpoint. No record is sent
  unless a URL+key are configured.

## Quickstart

### 1. Install the plugin

Drop the compiled `workbuddy-global.so` into CPA's plugin directory:

```bash
cp workbuddy-global.so /path/to/cliproxyapi/plugins/
```

For multi-arch deployments use the platform subdirectory convention:

```
plugins/
  linux/amd64/workbuddy-global.so
  linux/arm64/workbuddy-global.so
  darwin/arm64/workbuddy-global.so
```

### 2. Enable in `config.yaml`

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    workbuddy-global:
      enabled: true
```

### 3. Sign in

Open the WorkBuddy panel from CPA's sidebar (or hit
`/v0/resource/plugins/workbuddy-global/panel` directly) and click **登录** to start
the OAuth flow. Repeat for each account you want to add — the plugin writes
one `workbuddy-global-<uid>.json` per account to the auth store.

### 4. Use it

Call the OpenAI-compatible endpoint with any alias that maps to a model in the
account's discovered catalog:

```bash
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer $CPA_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "client-alias",
    "messages": [{"role": "user", "content": "hi"}],
    "stream": true
  }'
```

## Configuration

All fields are optional and live under `plugins.configs.workbuddy-global`.

```yaml
plugins:
  configs:
    workbuddy-global:
      enabled: true

      # Optional authoritative model ID list. Entries must be single-line YAML strings.
      # A non-empty list is the complete catalog: it is served as-is with no
      # network request and no cache read or write.
      # Missing, null, or [] keeps dynamic WorkBuddy discovery.
      models: []

      # Optional plugin-level proxy for every HTTP request initiated by WorkBuddy.
      # Supported schemes: http, https, socks5, socks5h.
      # Empty/unset inherits existing CPA routing. Invalid settings and runtime
      # proxy failures fail closed and never fall back to CPA or a direct route.
      proxy-url: ""

      # OAuth login channel (default "workbuddy-ai"):
      #   workbuddy-ai → international desktop profile: platform=workbuddy-ai on
      #                  www.workbuddy.ai. The default, and what an
      #                  international account needs; the issued token's domain
      #                  then routes the account to the Global gateway.
      #   workbuddy   → CN desktop profile (platform=workbuddy)
      #   cli         → CLI client profile on copilot.tencent.com (platform=CLI)
      oauth_client_mode: "workbuddy-ai"

      # Daily check-in automation. Check-in is a CN-realm mechanic and is
      # skipped entirely for Global accounts, so this has no effect on a
      # Global-only store; it is kept for imported CN credentials.
      checkin_auto: true

      # Credit lifecycle: disable CN on exhaust, delete Global on exhaust,
      # re-enable CN after check-in restores credits (default true).
      lifecycle_auto: true

      # Scheduler behavior (default "off"):
      #   off     → defer to CPA's built-in scheduler entirely
      #   credits → plugin picks the panel-selected account (with fallback
      #             when that account is exhausted / disabled)
      scheduler_mode: "off"

      # CPAMP usage forwarding. Both must be set for any record to be sent.
      # Falls back to USAGE_REPORT_URL / USAGE_REPORT_KEY /
      # CPAMP_ADMIN_KEY env vars or docker secret files when unset here.
      usage_report_url: "http://cpa-manager-plus:18317/v0/management/usage/import"
      usage_report_key: ""

      # Plugin-layer management auth. When set, all mutating endpoints under
      # /v0/management/plugins/workbuddy-global/* require this Bearer token.
      # When empty (default) the host's management middleware is the only
      # guard. Also readable from WB_MANAGEMENT_KEY env var.
      management_key: ""
```

Model aliases and exclusions are handled natively by CPA's
`oauth-model-alias` and `oauth-excluded-models` config — no plugin-side
duplication needed.

When `proxy-url` is set, chat, billing/check-in/trial, token refresh,
`executor.http_request`, usage forwarding, OAuth state/token/account calls,
and usage endpoint probes all use that proxy. Because CLIProxyAPI v7.2.30's
native-plugin host HTTP API has no per-request proxy override, explicit plugin
proxy traffic is sent by the plugin and does not appear in CPA's request-log.
The OAuth URL opened by the browser is not fetched by the plugin; the browser
needs its own network route.

## Model catalog

`model.static` is an offline fallback contract. It returns only `auto` with the
generic default metadata template and never reads account caches or performs a
network request.

When `models` is a non-empty YAML sequence of single-line strings, it is the complete model
list in the configured order. `model.for_auth` validates the account as usual,
but makes no network request at all and does not read or write any catalog
cache, or delete an existing cache. A configured catalog is published `ready`
with `model_source: config` and an empty `models_fetched_at`; only an unusable
cache root (e.g. `os.UserConfigDir()` failing) still fails closed. Missing
`models`, `models: null`, and `models: []` restore dynamic discovery and can
reuse the existing WorkBuddy catalog cache.

In dynamic mode, the first `model.for_auth` call for an account is the
authenticated bootstrap boundary:

1. WorkBuddy `GET /v3/config` supplies the account's entitled model IDs and
   every serving field: limits, credit rate, and the `supportsImages` flag that
   becomes the model's input modality. Only an HTTP 404 or 405 falls back to
   the legacy `GET /console/enterprises/personal/models` endpoint; other
   failures do not.
2. The response is validated before it replaces the persistent cache, and an
   immutable per-account catalog is published.

The cache root comes from `os.UserConfigDir()` rather than a hard-coded
platform path:

```plaintext
<user-config-dir>/CLIProxyAPI/workbuddy-global/model-catalog/
  models/
    <identity-sha256>.json
    <identity-sha256>.json.bak
```

For example, the default path for Linux root is
`/root/.config/CLIProxyAPI/workbuddy-global/model-catalog/`.

A first bootstrap with no valid cache is fail-closed: the WorkBuddy catalogue
must fetch, validate, and persist successfully before the account becomes
`ready`. `model.for_auth` returns an empty successful model response if
bootstrap fails. On later process starts, the first call still attempts the
refresh. If the refresh fails but a valid last-good cache exists, the account
starts `stale` and uses that cache. A refresh with neither a fresh result nor a
valid last-good cache leaves the account `failed`.

Only `ready` and `stale` are executable states. `not_started`, `loading`, and
`failed` are blocked at all executor entry points with a fixed, redacted
`not_ready` response and HTTP 503; the scheduler also excludes them. The panel
keeps loading and exposes `model_status` with per-account state, source, and
timestamp fields. Its fixed error categories are `auth_invalid`,
`workbuddy_transport`, `workbuddy_http`, `workbuddy_schema`, `cache_read`,
and `cache_write`; raw upstream errors, response bodies, credentials, and cache
paths are not exposed.

There is no background or request-time refresh, panel retry endpoint, or
Enterprise custom-model source. A new process start or an auth, token, or
plugin-config generation change supplies the next bootstrap opportunity.

## Lifecycle

| State | Global account (this plugin's target) | CN account (imported credential) |
|---|---|---|
| Credits > 0 | active | active |
| Credits = 0 | auth file **deleted** | `disabled: true` (auth file kept) |
| Check-in restores credits | n/a (already deleted) | re-enabled |
| Trial available | claimable once per account | n/a |
| Unknown credits | untouched (never mis-kill) | untouched |

Hard credit errors from the executor (status 402, "insufficient credits",
"积分不足", etc.) trigger an immediate reconcile of the failing account.

## Development

Requires Go 1.26+ (matches CPA).

```bash
# Build the plugin
go build -buildmode=c-shared -o workbuddy-global.so .

# Run tests
go test -race ./...

# Lint
gofmt -l .
go vet ./...
```

> **Cross-compiling**: `-buildmode=c-shared` needs CGO, so each target needs its
> own C toolchain (windows-arm64 and freebsd-amd64 in particular). A plain
> `GOOS/GOARCH` env pair on a Linux host only produces `linux/amd64`. Use the
> repository's CI (`build` + `build-cross` jobs) or the `Makefile`'s `release`
> target with the matching toolchains; see
> [docs/development.md](docs/development.md).

With `proxy-url` empty, the shared request helpers preserve their existing
routing: host-bridged requests use CPA's request-log and transport policy,
while the established OAuth, usage-probe, Windows, and old-host direct paths
remain unchanged. With `proxy-url` set, every plugin-initiated HTTP request
uses the plugin proxy instead and never falls back.

See [docs/development.md](docs/development.md) for the full workflow,
[docs/architecture.md](docs/architecture.md) for the module map, and
[docs/PROVENANCE.md](docs/PROVENANCE.md) for what is verified against the
international service versus what still needs a live account.

## License

MIT — see [LICENSE](LICENSE).
