# Model capabilities (reasoning effort)

CommandCode **does not expose a capability API**. This file records the only
authoritative per-model capability data that exists today, what was probed to
establish that, and how the plugin behaves because of it.

## There is no capability endpoint

Evidence gathered 2026-09-17 against `api.commandcode.ai`:

| Probe | Result |
|---|---|
| `GET https://api.commandcode.ai/provider/v1/models` (no auth needed) | 69 models, each with exactly `id`, `object`, `created`, `owned_by`, `name`, `context_length` — no capability, thinking or effort field |
| `/alpha/*` route inventory from the vendor's own CLI bundle and web bundle | `billing/credits`, `billing/subscriptions`, `usage/summary`, `whoami`, `agent/generate`, `generate`, `learn`, `namespaces`, `sandbox/*`, `share/*`, `taste/*`, `web-fetch`, `web-search`, `fingerprint/record`, `lifecycle-events`, `devrel-thread/*`, `consent/set`, `conversions/track` — **no models/capabilities route** |
| Vendor CLI (`command-code` v1.54.2, `dist/cli.mjs`) | `getSupportedEfforts(model)` reads a static `new Map([...])` of 40 models; a model outside it yields `null` → the CLI reports "Reasoning effort not supported for X" and sends no effort field |
| Vendor web app (`commandcode.ai`, `assets/constants-*.js`) | ships a static model registry with `reasoning: true` and `reasoningEfforts: [...]` per model — the effort selector in the UI reads that bundled table, not an API |

So the vendor's own clients hardcode this data; nothing in this repository can
fetch it, and a plugin has to embed it (or forward the client's request
verbatim) if it wants per-model behaviour.

## What the upstream actually enforces

Live probes against `/provider/v1/chat/completions` with a real provider key
(2026-09-17) show the upstream validates only the **union**
`low | medium | high | xhigh | max` and ignores the per-model table:

| Model | Vendor-declared list | Probed efforts | Upstream answer |
|---|---|---|---|
| `deepseek/deepseek-v4.1-flash` | low, high, max | low, high, max, **medium**, **xhigh**, *omitted* | 200 for all |
| `z-ai/glm-5.3-flash` | low, high, max | low, high, max, **medium** | 200 for all |
| `Qwen/Qwen3.8-Max` | low, medium, xhigh | high, **max** | 200 for both |

Rejected values are the ones outside the union: `reasoning_effort: "none"`
(observed in real client traffic) fails with
`Invalid option: expected one of "low"|"medium"|"high"|"xhigh"|"max"`.

**Acceptance is not the same as effect.** Whether the upstream *honours* an
effort outside a model's declared list, or silently falls back to a default, is
not verified here; that would need comparing
`usage.completion_tokens_details.reasoning_tokens` across efforts.

## Vendor-declared table (for reference / possible future embedding)

Source: the web app's model registry (63 models, of which those below carry an
explicit effort list). Extracted 2026-09-17; `command-code` CLI 1.54.2 agrees on
every model the two share.

| Model id | Reasoning efforts |
|---|---|
| `claude-sonnet-5`, `claude-sonnet-4-6`, `claude-fable-5-1`, `claude-fable-5`, `claude-opus-5`, `claude-opus-4-8`, `claude-opus-4-7` | low, medium, high, xhigh, max |
| `gpt-6-astra`, `gpt-5.6-sol`, `gpt-5.6-terra`, `gpt-5.6-luna` | low, medium, high, xhigh, max |
| `gpt-5.5`, `gpt-5.4`, `gpt-5.3-codex` | low, medium, high, xhigh |
| `gpt-5.4-mini`, `MiniMaxAI/MiniMax-M3`, `minimax/minimax-m3-free`, `tencent/hy4-preview`, `google/gemini-3.8-flash`, `google/gemini-3.7-flash`, `google/gemini-3.6-flash`, `google/gemini-3.5-flash`, `google/gemini-3.5-flash-lite`, `google/gemini-3.1-flash-lite`, `xai/grok-4.5` | low, medium, high |
| `deepseek/deepseek-v4.1-flash`, `deepseek/deepseek-v4-flash-fast`, `z-ai/glm-5.3-flash`, `zai-org/GLM-5.3`, `moonshotai/Kimi-K3` | low, high, max |
| `deepseek/deepseek-v4-pro`, `deepseek/deepseek-v4-flash`, `deepseek/deepseek-v4-flash-vision-exp`, `zai-org/GLM-5.2`, `sakana/fugu-ultra` | high, max |
| `Qwen/Qwen3.8-Max`, `Qwen/Qwen3.8-Max-0902`, `Qwen/Qwen3.8-27B`, `Qwen/Qwen3.8-Flash` | low, medium, xhigh |
| `meta/muse-spark-1.1`, `meta/muse-spark-1.2`, `meta/muse-spark-1.2-contributor`, `meta/muse-spark-1.3-contributor`, `xai/grok-4.6` | low, medium, high, xhigh |
| `meta/muse-spark-1.3` | low, medium, high, xhigh, max |
| `moonshotai/Kimi-K2.7-Code`, `moonshotai/Kimi-K2.7-Code-Highspeed`, `Qwen/Qwen3.7-Max`, `Qwen/Qwen3.7-Plus`, `Qwen/Qwen3.7-Flash`, `Qwen/Qwen3.6-Max-Preview`, `Qwen/Qwen3.6-Plus`, `meituan/LongCat-2.0:free`, `stepfun/Step-3.7-Flash`, `stepfun/Step-3.5-Flash`, `tencent/Hy3`, `tencent/hy3-paid`, `nvidia/nemotron-3-ultra-550b-a55b`, `thinkingmachines/inkling`, `thinkingmachines/inkling-small`, `poolside/laguna-s-2.1-free`, `inclusionai/ling-3.0-flash-free`, `inclusionai/ling-3.0-flash-sante:free` | *(none declared — the vendor offers no effort selector)* |

## How the plugin behaves

`internal/thinking/thinking.go` treats a model's catalog `thinking` object
(`levels`, `min`, `max`, `zero_allowed`, `dynamic_allowed` — decoded by
`internal/catalog/catalog.go`'s `rawThinking`) as the capability declaration and
uses it when present. CommandCode's catalog declares nothing, so:

- the client's `reasoning_effort` is forwarded to the upstream **verbatim**
  (`auto` and `none` omit the field), with the upstream as the authority;
- a model whose catalog entry *does* declare levels or bounds opts back into
  eager validation and budget clamping.

Embedding the vendor table above as per-model declarations is possible (it would
restore local validation and let a numeric-budget client — the Anthropic
Messages target — map effort to a budget) but is deliberately not done: the
upstream accepts the whole union, so local validation would only create false
rejections, and the table is undocumented vendor client data that can change
without notice.
