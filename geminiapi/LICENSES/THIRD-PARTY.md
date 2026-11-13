# Third-party components

This plugin does **not** bundle any third-party binary. The OpenAI-compatible
backend it supervises is downloaded at first use from its own release, and is a
separate work under its own licence.

| Component | Source | Licence | How it arrives |
|---|---|---|---|
| `gemini-web2api-go` `4.28.0-magpie.1` | `BUKOWSKIREAL/magpie-gemini-web`, tag `backend-v4.28.0-magpie.1`, built from `zexadev/gemini-web2api-go` @ `ed0f5ad` | see that repository's `LICENSE` / `LICENSE-UPSTREAM` | downloaded at runtime into `data-dir/bin/`, sha256-verified against `internal/backend/pin.go` |

Nothing in this repository contains a copy of that binary, a Google cookie, or
any account material. `git ls-files` shows no binary asset larger than the logo
SVG.
