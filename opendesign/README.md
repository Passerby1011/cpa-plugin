# Open Design CLIProxyAPI Plugin

**English** | [简体中文](README_CN.md)

A native dynamic Go plugin for [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) that exposes an **Open Design** plan as a single provider (`opendesign`; published model ids carry the `opendesign/` prefix).

The plugin owns the device sign-in, model discovery, the three protocol executors and key scheduling, so one account (or one pasted `sk-amr-…` key) serves OpenAI, Anthropic and Responses clients through CLIProxyAPI.

> **Attribution / packaging** — This is a Go port of magpie's
> [`@magpie-community/opencode-opendesign-auth`](https://www.npmjs.com/package/@magpie-community/opencode-opendesign-auth)
> (MIT). Every behaviour below was transcribed from that package's source and
> its tests, which is why the comments cite the issues the behaviour was learned
> from. The differences are the packaging (a c-shared Go plugin instead of an
> OpenCode hook) and the addition of CPA's provider surface.

## What Open Design actually serves

Two planes, and they take different credentials:

| Plane | Default base | Credential | Purpose |
|---|---|---|---|
| Model plane (`amr-link`) | `https://amr-link.open-design.ai/v1` | `sk-amr-…` runtime key | every model request, and `GET /models` |
| Control plane (`amr-api`) | `https://amr-api.open-design.ai` | `vela_ctrl_…` control key | the device sign-in, and the plan allowance |

The control plane **does not accept an API key**. That single fact is the reason
this plugin distinguishes a signed-in account from a pasted key:

- **Device sign-in** yields both keys, so the account's plan allowance is readable.
- **A pasted `sk-amr-…` key** yields only the runtime key, so its model list is
  discoverable but its allowance cannot be read. The auth row says so rather than
  leaving the operator to wonder.

Every model is served on all three wires (`/chat/completions`, `/messages`,
`/responses`), but a model only *thinks* on the wire it is declared on.

## Installation

### From a release

```bash
# pick your platform (example: linux/arm64)
unzip opendesign_0.1.0_linux_arm64.zip
cp opendesign.so /path/to/cliproxyapi/plugins/opendesign.so
```

### From source

```bash
cd opendesign
make build           # CGO_ENABLED=1 go build -buildmode=c-shared -o opendesign.so .
cp opendesign.so /path/to/cliproxyapi/plugins/
```

CGO is required: the plugin is a `c-shared` library loaded into the host process
and cannot run standalone.

### config.yaml

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    opendesign:
      enabled: true

      # A pasted key works too, but cannot read the plan allowance.
      api-keys:
        - value: "${OPENDESIGN_RUNTIME_KEY}"      # sk-amr-…
          # control: "vela_ctrl_…"                # optional; only a sign-in yields one
          label: work

      # Optional knobs, shown with their defaults.
      base-url: "https://amr-link.open-design.ai/v1"
      control-url: "https://amr-api.open-design.ai"
      request-timeout: "5m"
      models:
        allow: []                                # empty = publish everything
        deny: ["claude-*"]                       # always wins over allow
      pool:
        strategy: sticky                         # or round-robin
        max-concurrency-per-account: 0           # 0 = unlimited
```

`${ENV_VAR}` is expanded inside the CPA process for credentials only. A URL
carrying userinfo, a query or a fragment is refused: a credential must never ride
in an upstream URL.

## Usage

### Sign in with an Open Design account (recommended)

1. CPA Management UI → Auth files → the Open Design sign-in card.
2. Approve the code on the page that opens — the plugin polls until it is
   approved, and finishes by itself.
3. Both keys are stored in `opendesign-<uid>.json`, and the account's models
   appear without a manual reconfigure.

A transport hiccup during polling does **not** end the sign-in: the loop keeps
asking until the code's own expiry, because a network blip must not cost the
operator a fresh code. Only a 404, a terminal status (`expired` / `canceled` /
`denied` / `rejected`) or a 401 ends it.

### Paste an API key

Add a `sk-amr-…` key under `api-keys` (or paste it into the sign-in modal). The
model catalog is still read at that key; the plan allowance is not, and the auth
row states why.

### Panel

`/v0/resource/plugins/opendesign/panel` — the accounts, the served models, the
wire each model is declared on, and its measured reasoning levels. The page reads
the management key the CPA web UI already holds, so there is no second login.

## Models

The catalog is read at the account's runtime key from `{base-url}/models`. On a
401/403 the sign-in is marked expired rather than silently replaced by the seed;
any other failure falls back to the vendored seed so the operator still gets a
usable list.

The seed (twelve models, transcribed from magpie's `models.mjs`) fills in what a
live entry leaves out:

- a live entry's limits win where it states them, and the seed fills the rest —
  falling back to a generic 200K/32K would silently halve a model whose live
  entry omitted its limits;
- **a kind the seed declares stays** when the live catalog omits it, because a
  kind dropped is silent: the caller's image is sent as text and the model
  answers about nothing;
- a live catalog's output limit is capped where the API takes less than it
  publishes (`glm-5.3-flash` → 131072, `gemini-3.8-flash` → 65536).

Per-model wire:

| Model | Wire |
|---|---|
| `claude-haiku-5.5`, `deepseek-v4-pro` | `/responses` |
| `glm-5.3-flash` | `/messages` (Anthropic schema) |
| everything else | `/chat/completions` |

Reasoning levels are `low`/`high`/`max` where measured; `gpt-6-luna` declares
none, and a model with no levels is sent **no** reasoning control at all — that
is what keeps it from being asked to think at a level it ignores.

## Protocol behaviour

- **Signature follows the PATH, not the model's declared API.** `/messages` takes
  `x-api-key` plus `anthropic-version: 2023-06-01`; every other path takes a
  `Bearer` token. A request can reach `/messages` for a model declared on chat
  completions, and the key rides as the path it was asked on takes it.
- **Thinking survives all three crossings.** Chat's `reasoning_content`,
  Anthropic's `thinking` block (signature preserved) and Responses' `reasoning`
  item are moved between schemas rather than dropped. Nine pairwise translators
  would be nine places for a thought to disappear; this plugin uses one internal
  representation with a decoder and an encoder per schema.
- Chat/Responses/Anthropic clients can all reach any model, whatever wire it is
  declared on.

## Filtering

`models.allow` / `models.deny` match either the upstream id
(`glm-5.3-flash`) or the prefixed public id (`opendesign/glm-5.3-flash`).
`*` matches any run of characters, including none, so `deny: ["claude-*"]` drops
a family. An empty `allow` publishes everything; `deny` always wins.

## Development

```bash
cd opendesign
make build   # c-shared plugin
make test    # go test -race -count=1 ./...
make lint    # gofmt must be empty (fail-fast), then go vet
make release # cross-compile linux/amd64 + linux/arm64
```

`panel.html` is embedded with `go:embed`, so a change to it requires
`make build` — `gofmt` and `go vet` do not look at it.

## License

MIT. Ported from magpie's `opencode-opendesign-auth` (MIT); see [LICENSE](LICENSE).
