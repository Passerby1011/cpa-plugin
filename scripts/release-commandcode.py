#!/usr/bin/env python3
"""Build + package the commandcode plugin for a release.

This repo's CI matrix does not yet cover this plugin (adding it needs a token
with the `workflow` scope; see scripts/ci-wiring-commandcode.patch). Until then
this script reproduces the release locally: it builds the CGO c-shared library
for each platform in a throwaway golang container, zips it with the bare
library name at the archive root (what the CPA plugin store expects), and
writes checksums.txt.

Usage:
    python scripts/release-commandcode.py            # build all supported platforms
    python scripts/release-commandcode.py linux/amd64

Platforms: linux/amd64, linux/arm64, windows/amd64.
darwin needs osxcross and freebsd needs a freebsd cross-toolchain; neither is
available here, so they are not built.
"""
from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path

PLUGIN_ID = "commandcode"
IMAGE = "golang:1.26-bookworm"
ALL = ("linux/amd64", "linux/arm64", "windows/amd64")


def repo_root() -> Path:
    return Path(__file__).resolve().parent.parent


def version(plugin_dir: Path) -> str:
    return (plugin_dir / "VERSION").read_text().strip()


def build(plugin_dir: Path, targets: list[str]) -> None:
    ver = version(plugin_dir)
    # Debian cross-toolchains + zip; sysroot for arm64 is required.
    apt = ("apt-get update -qq >/dev/null 2>&1 && "
           "apt-get install -y -qq --no-install-recommends "
           "gcc-aarch64-linux-gnu libc6-dev-arm64-cross gcc-mingw-w64-x86-64 zip file >/dev/null 2>&1")
    lines = [
        "set -euo pipefail",
        apt,
        f"V={ver}",
        "mkdir -p dist",
    ]
    for t in targets:
        goos, goarch = t.split("/")
        if goos == "windows":
            ext, cc = "dll", "CC=x86_64-w64-mingw32-gcc"
        else:
            ext = "so"
            cc = "CC=aarch64-linux-gnu-gcc CGO_CFLAGS='--sysroot=/usr/aarch64-linux-gnu'" if goarch == "arm64" else ""
        lines += [
            f'echo "=== {t} ==="',
            f"CGO_ENABLED=1 GOOS={goos} GOARCH={goarch} {cc} "
            f'go build -trimpath -buildmode=c-shared -ldflags "-s -w -X main.version=$V" '
            f"-o dist/{PLUGIN_ID}.{ext} .",
            f"rm -f dist/{PLUGIN_ID}.h",
            f'(cd dist && zip -q "{PLUGIN_ID}_${{V}}_{goos}_{goarch}.zip" {PLUGIN_ID}.{ext} && rm -f {PLUGIN_ID}.{ext})',
        ]
    lines += [
        "cd dist && sha256sum *.zip > checksums.txt && ls -la && cat checksums.txt",
    ]
    cmd = ["docker", "run", "--rm",
           "-v", f"{plugin_dir}:/plugin", "-w", "/plugin",
           "-e", "PATH=/usr/local/go/bin:/usr/sbin:/usr/bin:/sbin:/bin",
           IMAGE, "bash", "-c", "\n".join(lines)]
    print("+", " ".join(cmd[:6]), "...")
    subprocess.run(cmd, check=True)


def main() -> int:
    plugin_dir = repo_root() / PLUGIN_ID
    if not plugin_dir.is_dir():
        print(f"plugin dir not found: {plugin_dir}", file=sys.stderr)
        return 1
    targets = sys.argv[1:] or list(ALL)
    for t in targets:
        if t not in ALL:
            print(f"unsupported target {t} (supported: {', '.join(ALL)})", file=sys.stderr)
            return 2
    build(plugin_dir, targets)
    print("done. upload dist/*.zip to the plugin's GitHub Release, then refresh registry.json.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
