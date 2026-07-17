#!/usr/bin/env python3
"""Patch CoPaw 1.0.2 Matrix channel _sync_loop indentation bug.

Upstream nests ``async def _sync_loop`` inside ``_refresh_matrix_token``;
fail the build loudly if the marker moves so the patch cannot silently no-op.
"""

from __future__ import annotations

import pathlib
import sys


def patch(site_packages: pathlib.Path) -> None:
    target = site_packages / "copaw" / "app" / "channels" / "matrix" / "channel.py"
    if not target.exists():
        raise FileNotFoundError(f"CoPaw Matrix channel not found: {target}")

    lines = target.read_text(encoding="utf-8").splitlines(keepends=True)
    marker = "        async def _sync_loop"
    replacement = "    async def _sync_loop"

    patched = False
    for idx, line in enumerate(lines):
        if line.startswith(marker):
            lines[idx] = line.replace(marker, replacement, 1)
            patched = True
            break

    if not patched:
        if any("    async def _sync_loop" in line for line in lines):
            print("Matrix channel _sync_loop already at module level; skipping")
            return
        raise RuntimeError(
            "could not find mis-indented _sync_loop marker in copaw Matrix channel"
        )

    target.write_text("".join(lines), encoding="utf-8")

    pycache = target.parent / "__pycache__"
    if pycache.is_dir():
        for child in pycache.glob("channel*"):
            child.unlink(missing_ok=True)


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        print(f"usage: {argv[0]} <site-packages>", file=sys.stderr)
        return 2
    patch(pathlib.Path(argv[1]))
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
