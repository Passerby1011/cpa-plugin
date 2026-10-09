# CommandCode CLIProxyAPI Plugin

**English** | [简体中文](README_CN.md)

A native dynamic Go plugin for [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) that exposes a **CommandCode** plan as a single provider (`commandcode`; published model ids carry the `commandcode/` prefix).

The plugin owns model discovery, protocol translation, execution, key scheduling, and a quota page for the CommandCode account surface, so one API-key pool serves OpenAI, Anthropic, and Responses clients through CLIProxyAPI.

> **Attribution / packaging** — This is a repackaging of the upstream plugin
> [`mczhoucn/commandcode`](https://github.com/mczhoucn/commandcode)
> (MIT) as a module of this plugin collection. Upstream credit and its MIT
> license are preserved; the differences from upstream are only the module path,
> the metadata repository URL, and the packaging files (`Makefile`, `VERSION`,
> this README). See [docs/PROVENANCE.md](docs/PROVENANCE.md).

## What the upstream actually serves

CommandCode is reached through its OpenAI-compatible provider surface:

| Upstream route | Status |
|---|---|
| `POST {base-url}/chat/completions` | Serves the OSS models (`deepseek/*`, `z-ai/*`, `Qwen/*`, `MiniMaxAI/*`, `moonshotai/*`, …) |
| `GET {base-url}/models` | Model catalog |
| `POST {base-url}/messages` | Claude models only — OSS ids are rejected with *"Model … is not supported on this endpoint"* |
| `POST {base-url}/responses` | Not a registered route |

The default upstream base URL is `https://api.commandcode.ai/provider/v1`.

Because there is exactly one usable route for the catalog, **every model discovered from `/models` is routed to chat-completions by default**, whatever its family or vendor prefix. `route-overrides` exist for upstreams that really do serve another protocol on another endpoint.

## The Problem

Without this plugin, using a CommandCode plan in CLIProxyAPI requires hand-written provider blocks per protocol family, and the pieces that CLI clients depend on are missing or wrong:

- **Duplicated configuration & keys**: the same keys must be configured in several provider blocks.
- **Fragmented scheduling**: rotation, rate limits, and cooldowns cannot be shared, and the Management Center cannot show per-key usage.
- **Client protocol burden**: clients must know which upstream endpoint a model requires.
- **Lost reasoning**: CommandCode returns thinking text under `reasoning` / `reasoning_details[].text` (never the standard `reasoning_content`), so a plain OpenAI-compatible provider silently drops every chain-of-thought for Anthropic and Responses clients.
- **Rejected reasoning effort**: CommandCode's `/models` carries no thinking metadata, so a provider that validates `reasoning_effort` locally rejects `xhigh`/`max` even though the upstream accepts them.

## The Solution

- **Unified auth pool**: the plugin registers each configured key as a CLIProxyAPI auth record, so the host's scheduler handles rotation, retries, error cooldowns, session affinity, and per-key statistics.
- **Transparent translation & routing**: clients ask for `commandcode/<upstream-id>` and never need to know the upstream protocol.
- **Reasoning fidelity**: upstream thinking text is preserved for all three client formats, and `reasoning_effort` is forwarded verbatim when the model declares no capability.
- **One model catalog**: everything published by `{base-url}/models` is discoverable under the `commandcode/` prefix in `/v1/models`.

## Features

- **Single Provider Namespace**: models appear as `commandcode/deepseek/deepseek-v4.1-flash`, `commandcode/z-ai/glm-5.3-flash`, … (prefix configurable, can be disabled for bare upstream ids).
- **Multi-Protocol Client Translation**: OpenAI Chat Completions, Anthropic Messages, and OpenAI Responses requests all become upstream chat-completions calls, and responses are converted back — including streaming.
- **Reasoning Preservation**: upstream `reasoning`, `reasoning_details[].text`, and `reasoning_content` are normalized across targets:
  - Claude clients get a leading `thinking` block with `thinking_delta` events;
  - Responses clients get a leading `reasoning` output item with `response.reasoning_summary_*` events;
  - OpenAI clients get `reasoning_content` backfilled onto each chunk while the vendor fields stay intact.
- **Capability-aware Reasoning Controls**: an explicit `route-overrides` declaration (or catalog thinking metadata) re-enables eager validation and clamping; with nothing declared, the client's `reasoning_effort` is forwarded verbatim (`auto`/`none` omit the field) and the upstream is the authority.
- **Dynamic Catalog Discovery**: remote catalog with local fallback, deduplication, and diagnostics for what was excluded.
- **CommandCode Quota Page**: a Management Center page listing every configured credential with its account email, plan, remaining plan credits, and the rolling 5-hour/weekly windows (used, cap, reset time), refreshed per card on demand.
- **Multi-Key Auth Scheduling**: one key pool shared across all protocols through CLIProxyAPI's native scheduler.

## Requirements

- **CLIProxyAPI**: `v7.2.138+` (verified on **v8.0.13**; no ABI change between the v7 module line and the v8 host)
- **Go Toolchain**: Go 1.26+ (CGO enabled for `-buildmode=c-shared`)

## Configuration essentials

> **`api-keys` is REQUIRED to serve models**, but it is no longer required to
> *register*. With no keys the plugin registers in a **pending** state: the
> Management Center shows it as registered and renders the full config form, so
> the key can be entered right there in the UI. Until a key is saved the plugin
> advertises no models and refuses execution with
> `commandcode plugin is registered but not configured: set api-keys in the plugin configuration`.
>
> This replaces the earlier dead end where registration itself failed
> (`api-keys: at least one key is required`), the panel showed **未注册 / 未生效**,
> and — because the plugin declared no config fields — there was no place in the
> UI to enter a key at all.

## Configuring from the Management Center

Every supported option is declared as a plugin config field, so it can be set
in the plugin management page without hand-editing `config.yaml`:

| Field | Type | Notes |
| --- | --- | --- |
| `api-keys` | array (JSON) | `[{"value":"cc_..."}]`; supports `${ENV_VAR}` |
| `base-url` | string | default `https://api.commandcode.ai/provider/v1` |
| `catalog-url` | string | default `{base-url}/models` |
| `model-prefix` | object (JSON) | `{"enabled":true,"value":"commandcode"}` |
| `catalog` | object (JSON) | `{"refresh-interval":"15m","stale-while-unavailable":true}` |
| `protocols` | object (JSON) | `{"chat-completions":true,"messages":true,"responses":true}` |
| `route-overrides` | object (JSON) | `{"<model>":{"protocol":"...","endpoint":"..."}}` |
| `request-timeout` | string | e.g. `5m` |
| `max-response-bytes` | integer | default `67108864` |
| `allow-http` | boolean | http:// upstreams, testing only |

Nested options are declared as **object** fields holding JSON, not as dotted
names like `model-prefix.enabled`: the host shallow-merges the submitted object
into the plugin config node, so a dotted key would be written as a literal
`model-prefix.enabled` key and the plugin would never read it.

> **Model list.** Models are discovered from `{base-url}/models` automatically —
> there is nothing to add by hand. Per-model control is expressed through
> `model-prefix`, `protocols`, and `route-overrides`.

> **Key visibility.** CPA's config form has no secret field type, so the API key
> is displayed in plain text and stored in the CPA config file. Avoid
> screenshotting or sharing the config page. The plugin itself never logs key
> material — only key counts and truncated hashes.

## Build

```bash
# Linux (AMD64)
go build -buildmode=c-shared -o plugins/linux/amd64/commandcode.so .

# macOS (ARM64)
go build -buildmode=c-shared -o plugins/darwin/arm64/commandcode.dylib .

# Windows (AMD64)
go build -buildmode=c-shared -o plugins/windows/amd64/commandcode.dll .
```

Place the artifact into the host's plugin directory (e.g. `<cliproxyapi_root>/plugins/<os>/<arch>/`).

## Configuration

```yaml
plugins:
  enabled: true
  dir: /data/plugins
  configs:
    commandcode:
      # Upstream base URL (default: "https://api.commandcode.ai/provider/v1")
      base-url: "https://api.commandcode.ai/provider/v1"

      # Optional catalog override (default: "{base-url}/models")
      # catalog-url: "https://api.commandcode.ai/provider/v1/models"

      # Client-facing model id prefix
      model-prefix:
        enabled: true              # true -> "commandcode/<model>" (default: true)
        value: "commandcode"

      # CommandCode API keys (at least one required); supports ${ENV_VAR}
      api-keys:
        - value: "user_xxx"
        - value: "${COMMANDCODE_API_KEY}"

      # Catalog discovery
      catalog:
        refresh-interval: "15m"           # minimum "1m"
        stale-while-unavailable: true

      # Protocol switches: disabling one removes every model routed to it
      protocols:
        chat-completions: true
        messages: true
        responses: true

      # Per-model route pins; only needed when the upstream serves another
      # endpoint (CommandCode itself serves OSS models on chat-completions)
      route-overrides:
        "claude-sonnet-5":
          protocol: "messages"            # chat-completions | messages | responses
          endpoint: "/v1/messages"        # required, must start with "/"

      request-timeout: "5m"
      max-response-bytes: 67108864        # 64 MiB
      allow-http: false                   # http:// base-url for local testing
```

### Configuration Options

| Option | Type | Default | Description |
|---|---|---|---|
| `api-keys` | `[]object` | *(legacy)* | List of API keys (`- value: "..."`). Supports `${ENV_VAR}` expansion. Duplicates and empty values are rejected. Folded in as `provider` accounts; kept for backward compatibility. |
| `accounts` | `[]object` | `[]` | Account pool. Each entry: `label` (display), `mode` (`provider` default / `go-cli`), `credential` (supports `${ENV_VAR}`), `disabled`. **A Go-plan key MUST use `mode: go-cli`**, otherwise it is sent to the Provider API that refuses it. |
| `pool.strategy` | `string` | `sticky` | `sticky` (prefer the credential used last) or `round-robin`. Stickiness is process-scoped, not session-scoped. |
| `pool.max-concurrency-per-account` | `int` | `0` | In-flight request cap per credential; `0` means unlimited. The reference implementations default to 1, which also keeps one credential from looking like a burst. |
| `base-url` | `string` | `https://api.commandcode.ai/provider/v1` | Upstream provider base URL. Valid HTTPS (or HTTP with `allow-http: true`), no query, fragment, or userinfo. |
| `catalog-url` | `string` | `{base-url}/models` | Catalog discovery URL. |
| `model-prefix.enabled` | `bool` | `true` | Client-facing ids use `<prefix>/<model>`; `false` publishes bare upstream ids. |
| `model-prefix.value` | `string` | `commandcode` | Provider prefix. |
| `catalog.refresh-interval` | `duration` | `15m` | Catalog polling cadence (minimum `1m`). |
| `catalog.stale-while-unavailable` | `bool` | `true` | Keep serving the last good snapshot when a refresh fails. |
| `catalog.static` | `[]string` | `[]` | Static model-id list used when the live `{base-url}/models` cannot be fetched — most often a pool with **no provider-mode account**, since `/models` belongs to the Provider API, which refuses Go-plan keys. On a failed refresh it is used only after `stale-while-unavailable` has had its chance, so a coarser static table never overwrites a good snapshot. |
| `plan-filter` | *(automatic)* | on | Since 0.4.2 the live catalog is plan-filtered: only models the pool's account(s) can actually call are published (union across accounts, fail-open on unknown plans/models — the server stays the final gate). Driven by each go-cli account's subscription; nothing to configure. Filtered models appear in the unsupported diagnostics as "not included in the account's plan". |
| `retry` | `object` | `{"max":2,"base-backoff":"400ms"}` | Transparent retry of transport-level upstream flips. Only fires **before any byte is written downstream**; only for transport faults (connection reset / terminated / socket hang up / connection refused / i/o timeout). **429/503 are never retried** — they are deliberate upstream signals, and retrying swallows the backoff and multiplies load. `max=0` disables. |
| `watchdog` | `object` | `{"enabled":true,"stream":"30s","non-stream":"90s"}` | Bounds the gap **between upstream reads** (reset on every chunk), not total request time: vendor CLIs have no upstream idle timeout and legitimate long thinking pauses would be killed by a total-duration cap. |
| `max-inflight` | `int` | `0` (unlimited) | Global in-flight request cap; excess requests get a retryable `503`. Concurrency control normally belongs to the reverse proxy in front, which is the only layer that knows the box's capacity. |
| `protocols.*` | `bool` | `true` | Route kill switches. A disabled protocol excludes its models with a diagnostic. |
| `route-overrides` | `map` | `{}` | `{ model: { protocol, endpoint } }` pins a model onto another upstream route. `endpoint` is required. |
| `device.enabled` | `bool` | `true` | Fabricated device identity for go-cli requests. On, each credential presents a stable fake machine and announces it (fingerprint + lifecycle) before its first generate. Off drops the project slug AND stops both announcements. |
| `device.project-dir` | `string` | `C:\Users\dev\projects\app` | Fabricated working directory. Feeds BOTH the envelope's `config.workingDir` and the `x-project-slug` header, so they cannot disagree. |
| `device.identity-salt` | `string` | `""` | Shifts WHICH fake machine a credential maps to. The escape hatch for a flagged credential; it never reaches the hash stage and is not sent upstream. |
| `request-timeout` | `duration` | `5m` | Upstream HTTP timeout (also bounds account/quota calls to 30s). |
| `max-response-bytes` | `int64` | `67108864` | Maximum non-streaming response body size. |
| `allow-http` | `bool` | `false` | Permit `http://` upstreams for local testing. |

### Quota page
The `CommandCode Quota` page (Management Center → plugins) reads the account surface on the same authority as `base-url`:

| Endpoint | Purpose |
|---|---|
| `GET {authority}/alpha/billing/credits` | Remaining plan credits and the 5-hour/weekly windows |
| `GET {authority}/alpha/billing/subscriptions` | Plan id/status |
| `GET {authority}/alpha/whoami?limits=1` | Account email for the card label |

`{authority}` is derived from `base-url` by trimming its provider path (`/provider/v1`). Each card is refreshed manually and independently; the page never polls, and quota values never influence routing.

Each card's header states the **plan type** (`套餐 Go`, `套餐 GOAT`, …) with the plan's
monthly allowance, and shows `套餐未知` rather than nothing when the subscription could
not be read. The plan comes from the same `planId` the table above already fetches.

The page follows the **same design system as the WorkBuddy panel** (identical theme tokens
from the CPA management panel's `key-policy / themes.scss`, the same component classes, and
the same `data-theme` bridge that mirrors the parent shell). It is laid out like that panel:
a toolbar (`刷新数据` refreshes every credential at once), filter pills with live counts
(`全部 / 正常 / 已超限 / 套餐未知`), a search box, a remaining-credit sort, a summary card
(剩余/已用/额度池/消耗占比 plus a consumption meter), and one card per credential whose
5-hour, weekly and monthly allowances are all metered, colour-graded at 50/75/90%.

Filters, search and sort operate only on the snapshot already fetched — they never issue a
quota request, so a credential refreshes only when you ask it to.

Each card also states **where its credential comes from**: `来源 配置` (the plugin's own
`accounts`) or `来源 认证文件` (a CPA auth record, where a key added on this page lands). The
two stores are independent - deleting an auth record does nothing for a config-declared
credential, and one materialization pass would even recreate it - so the badge tells the
operator which place to edit. A credential present in both is reported as `配置`, the store
that must be changed for the removal to stick. The page deliberately offers **no removal
control**: the host API has no auth delete, and its `auth.save` discards a `disabled` flag, so
a button here could only pretend to work.

The page can also **add a key** ("＋ 添加 Key"): credential, optional label, and transport
mode (`go-cli` for Go-plan keys — the default — or `provider`). The host gives a plugin no
way to write its own config, so the key is stored as a **CPA auth record** and the plugin
folds auth-record credentials into its account pool; entries in `accounts` keep working
unchanged, and the two sources are unioned (config wins on a duplicate). The credential is
never echoed back in a response, a log line, or an error.

> One caveat when starting from an **empty** configuration: the host reads a plugin's model
> list at startup / config load only, so after adding the *first* key the model list appears
> after a CPA restart (or one config re-save). The page says so in that case. With a config
> account already present, an added key takes effect immediately.

### Reasoning effort

CommandCode publishes **no capability API**: `{base-url}/models` returns only `id`, `object`, `created`, `owned_by`, `name` and `context_length`, and its `/alpha/*` account surface has no models or capabilities route. The per-model effort lists that exist live inside the vendor's own clients (the `command-code` CLI and the web app both ship a static table), and the upstream gateway itself accepts every value in the union `low | medium | high | xhigh | max` regardless of the per-model list.

Consequences for this plugin:

- with nothing declared in the catalog, `reasoning_effort` is forwarded verbatim (`auto`/`none` omit the field) so `xhigh` and `max` work;
- if a catalog entry ever carries a `thinking` object (`levels`, `min`, `max`, `zero_allowed`, `dynamic_allowed`), that declaration takes over and eager validation plus budget clamping are re-enabled;
- the vendor's per-model table, the probes behind these statements and the evidence for the missing capability API are recorded in [`docs/model-capabilities.md`](docs/model-capabilities.md).

## Testing

```bash
go test ./...        # unit + mocked end-to-end tests
go test ./... -cover # with coverage
go vet ./...         # vetting
```

## Provenance

This plugin was built with reference to two prior CommandCode plugins:

- [opencode-go-cliproxyapi](https://github.com/massiveits/opencode-go-cliproxyapi) (v0.1.7) is the direct ancestor: the adapter kernel, catalog, config, auth, and quota scaffolding come from there, and the CommandCode-specific behaviour — reasoning preservation, unattributed-capability effort passthrough, the chat-completions-only route default, and the account API used by the quota page — was adapted on top.
- [cpa-plugin-commandcode](https://github.com/ahoo/cpa-plugin-commandcode) (ahoo) is an independent earlier CommandCode plugin whose write-up established the vendor behaviours this plugin had to reproduce: thinking text arriving under `reasoning` / `reasoning_details[].text` and never `reasoning_content`, the resulting need to backfill `reasoning_content` before CLIProxyAPI's openai→claude translator sees it, the SSE `data: ` prefix that streaming `/v1/messages` requires, and the fact that only fully-qualified vendor names (`deepseek/deepseek-v4.1-flash`) are accepted upstream.

Upstream repository: <https://github.com/mczhoucn/commandcode>. See
[docs/PROVENANCE.md](docs/PROVENANCE.md) for exactly what this repackaging changed.

## License

[MIT](LICENSE)
