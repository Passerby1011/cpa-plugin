# packaging provenance — commandcode

This directory is a **repackaging** of the upstream plugin
[`mczhoucn/commandcode`](https://github.com/mczhoucn/commandcode)
(MIT), contributed into the [`Passerby1011/cpa-plugin`](https://github.com/Passerby1011/cpa-plugin)
collection so it can be installed and updated through the repo's plugin-store
(`registry.json`, `direct` install, schema_version 2).

Original work © mczhoucn. The MIT license text is unchanged
([LICENSE](LICENSE)).

## What this repackaging changed

The upstream source is vendored **verbatim** except for these mechanical
adaptations required by this monorepo's conventions:

| Area | Upstream | Here |
| --- | --- | --- |
| Go module path | `commandcode` | `github.com/hex-ci/cpa-plugin/commandcode` |
| Internal import prefix | `commandcode/internal/...` | `github.com/hex-ci/cpa-plugin/commandcode/internal/...` |
| Registration metadata `Name` | `commandcode` | unchanged (bare plugin id; the host keys plugins off the library filename, so `Name` is display-only) |
| Registration metadata `GitHubRepository` | `https://github.com/mczhoucn/commandcode` | `https://github.com/Passerby1011/cpa-plugin` (the repo this build is published from) |
| Packaging files | only `go.mod` / `go.sum` / `Makefile`-less tree | added `VERSION`, `Makefile`, `.gitignore`, `CHANGELOG.md`, this file |

No protocol, routing, reasoning, scheduling, or quota logic was modified.

## Version

Vendored at upstream **v0.1.1** (commit `ea84cdd`). `VERSION` here starts at
`0.1.1` to stay in lockstep with upstream; this repo re-tags it as
`commandcode-v<version>`.

To pull a future upstream release:

```bash
# from a clean checkout of this repo
git clone --depth 1 https://github.com/mczhoucn/commandcode /tmp/cc
rsync -a --delete --exclude .git --exclude .github /tmp/cc/ commandcode/
# re-apply the mechanical adaptations above (module path, imports, metadata
# GitHubRepository), bump VERSION, add a CHANGELOG entry, then commit.
```

## Compatibility verification

Verified in an isolated **CLIProxyAPI v8.0.13** host built from source
(`router-for-me/CLIProxyAPI` @ `v8.0.13`):

- the `linux/amd64` `.so` loads (`pluginhost: plugin loaded`),
- registers (`pluginhost: plugin registered … version=0.1.1`),
- publishes its `commandcode/…` models, and
- registers the `CommandCode Quota` management menu + routes.

**`api-keys` is mandatory**: with an empty key list the host logs
`plugin.register failed: api-keys: at least one key is required` and the plugin
is reported as *not registered* (未注册 / 未生效) in the Management Center,
even though the library itself loaded fine. That is a configuration error, not
a version/ABI error.

## 0.6.0 increment — sources

The 0.6.0 feature set closes the gap against the reference implementation
[`magpie-community/plugins`](https://github.com/magpie-community/plugins)
`packages/commandcode` at commit
**`0135b7b0ea979c94033a7712a0c6ca1be8dafb70`** (1 682 lines of `index.mjs` plus
its `*.test.mjs` behaviour contracts). That
implementation is **not vendored** here: this plugin keeps its own Go structure
(host bridge, pool scheduler, plan gate, quota page) and only the missing
capabilities were added, each against the reference's contract:

| Capability | Reference contract | How it was verified here |
|---|---|---|
| Zero Data Retention (`x-cmd-zdr: 1`) | `zdr.test.mjs`, `go-refused.test.mjs` | Header on both surfaces (Provider API and `/alpha/generate`), env `CMD_ZDR=1` re-read per request, refusals passed through untranslated and never retried without the header. Unit-tested; **not** verified against the live upstream. |
| `supported_endpoints` routing | `models.test.mjs` | Catalogue entry selects chat-completions / messages / responses; no claim keeps the chat-completions default. Unit-tested only. |
| `reasoning_effort: "off"` | `think-off.test.mjs` | Client `none`/`"off"` becomes upstream `"off"` for the five models observed to accept it, is omitted for models observed to refuse it, and is never sent on the go-cli leg. The accept/refuse lists come from probes, see [`docs/model-capabilities.md`](docs/model-capabilities.md). Unit-tested; the real upstream was not re-probed here. |
| Signed-out mark confirmed by `whoami` | `signed-out.test.mjs` | A `401` is only a candidate: `GET /alpha/whoami` must refuse the key too (401/403). Network errors, 429 and 5xx never mark, and a clean read clears the mark. Unit-tested, including the panel entry. |
| Jev (`typesafe/jev`) | `jev.test.mjs` | **NOT exposed.** Feasibility verdict below. |

### Jev / decision models: not exposed (host has no `decides` capability)

The reference implementation lists TypeSafe's Jev as a *decision model*
(`decides: true`): it answers typed questions (`{model, state, questions}`) with
probabilities instead of holding a conversation, is offered to magpie as a
routing group's classifier, and is served on magpie's own `/v1/systemone`.
magpie can do this because it owns a `decide`/`decides` concept in its provider
config.

CLIProxyAPI **v7.2.138** (the version this plugin builds against) has no
equivalent. Verified by reading the host source:

1. `sdk/pluginapi/types.go` → `Capabilities` has no decision-model slot. The
   complete field list is `ModelRegistrar`, `ModelProvider`, `AuthProvider`,
   `FrontendAuthProvider`, `FrontendAuthProviderExclusive`, `Scheduler`,
   `ModelRouter`, `Executor`, `ExecutorModelScope`, `ExecutorInputFormats`,
   `ExecutorOutputFormats`, `RequestTranslator`, `RequestNormalizer`,
   `ResponseTranslator`, `ResponseBeforeTranslator`, `ResponseAfterTranslator`,
   `RequestInterceptor`, `RequestLifecyclePlugin`, `ResponseInterceptor`,
   `StreamChunkInterceptor`, `ThinkingApplier`, `UsagePlugin`,
   `CommandLinePlugin`, `ManagementAPI`. `ModelRouter` decides *where a request
   goes*; it is not a classifier surface.
2. `internal/api/server_routes.go` registers no `/systemone` and no
   `/provider/v1/*` route. The complete client-facing route set is
   `/v1/chat/completions`, `/v1/completions`, `/v1/messages`,
   `/v1/responses` (+ realtime, images, videos, Gemini `v1beta`). There is
   nowhere to serve a System One question.
3. The client-facing `/v1/models` answer is filtered to `id`, `object`,
   `created`, `owned_by` only (`sdk/api/handlers/openai/openai_handlers.go` →
   `OpenAIModels`), so a `decides` marker could not be advertised even if the
   catalogue carried one.

Consequences, implemented deliberately:

- Jev is **not** published in the chat model list. A decision model offered as
  a chat model would accept a conversation it cannot hold — strictly worse than
  a model that is absent.
- The catalogue recognises such an entry by **two independent signals** and keeps it
  out, with an explicit diagnostic
  (`"decision model: the host has no decides capability"`). A silently missing
  model is a diagnosis nobody can make.
  - the entry's own `decides` flag (how the reference's provider config marks it), and
  - the id naming TypeSafe's family (`typesafe/jev` and its `jev-*` aliases, with the
    client-facing `<prefix>/` stripped so `commandcode/typesafe/jev` matches too).

    The id rule is not redundancy: Command Code's `/models` list **does not name Jev at
    all** (the reference adds it from its own config). A deployment whose upstream, or
    `catalog.static` table, lists it plainly would carry no `decides` flag - and keying
    off the flag alone would then publish exactly the model this check exists to keep
    out. The match is bound to the `typesafe/` owner so an unrelated `jev` chat model is
    not over-excluded.
- If a future host version grows a classifier surface, this is where the
  capability would be added; until then it stays unexposed on purpose.

