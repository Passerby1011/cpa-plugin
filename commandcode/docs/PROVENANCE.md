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
