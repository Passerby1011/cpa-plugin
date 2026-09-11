#!/usr/bin/env python3
"""Rewrite one plugin's registry entry into a direct-install plan from built artifacts.

The repository is a monorepo holding several independently versioned plugins, so
`github-release` installs cannot work: the plugin store resolves that type through
`/repos/{owner}/{repo}/releases/latest`, which is repo-wide, and the release tag must
normalize to a bare numeric version. Direct install avoids both constraints by pinning
an explicit URL + sha256 per platform in registry.json — this script regenerates that
block from the archives a release build just produced.
"""
from __future__ import annotations

import hashlib
import json
import sys
from pathlib import Path

# Every platform the release workflow builds; all must be present before publishing.
PLATFORMS = (
    ("linux", "amd64"),
    ("linux", "arm64"),
    ("darwin", "amd64"),
    ("darwin", "arm64"),
    ("windows", "amd64"),
    ("windows", "arm64"),
    ("freebsd", "amd64"),
)


def sha256_of(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def main() -> int:
    if len(sys.argv) != 5:
        print(
            "usage: sync-registry.py <registry.json> <plugin-id> <version> <dist-dir>",
            file=sys.stderr,
        )
        return 2
    registry_path = Path(sys.argv[1])
    plugin_id = sys.argv[2].strip()
    version = sys.argv[3].strip()
    dist_dir = Path(sys.argv[4])
    if not plugin_id or not version:
        print("plugin id and version are required", file=sys.stderr)
        return 2

    data = json.loads(registry_path.read_text())
    entries = data.get("plugins") or []
    target = next((e for e in entries if str(e.get("id", "")).strip() == plugin_id), None)
    if target is None:
        print(f"plugin {plugin_id!r} not found in {registry_path}", file=sys.stderr)
        return 1
    repository = str(target.get("repository") or "").strip()
    if "github.com/" not in repository:
        print(f"plugin {plugin_id!r} has no GitHub repository to derive asset URLs from", file=sys.stderr)
        return 1

    tag = f"{plugin_id}-v{version}"
    base = f"{repository.rstrip('/')}/releases/download/{tag}"
    artifacts = []
    for goos, goarch in PLATFORMS:
        archive = dist_dir / f"{plugin_id}_{version}_{goos}_{goarch}.zip"
        if not archive.is_file():
            print(f"missing artifact {archive}", file=sys.stderr)
            return 1
        artifacts.append(
            {
                "goos": goos,
                "goarch": goarch,
                "url": f"{base}/{archive.name}",
                "sha256": sha256_of(archive),
                "size": archive.stat().st_size,
            }
        )

    target["version"] = version
    target["install"] = {"type": "direct", "artifacts": artifacts}
    # direct install is only valid under schema_version 2.
    data["schema_version"] = 2
    registry_path.write_text(json.dumps(data, indent=2, ensure_ascii=False) + "\n")
    print(f"updated {plugin_id} v{version}: {len(artifacts)} artifact(s) from {dist_dir}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
