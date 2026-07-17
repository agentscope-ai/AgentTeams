#!/usr/bin/env python3
"""Patch CoPaw Matrix channel _sync_loop indentation bug when present.

Older CoPaw 1.0.2 builds nested ``async def _sync_loop`` inside
``_refresh_matrix_token`` (IndentationError). Current PyPI 1.0.2 uses a
valid nested local ``async def sync_loop()`` inside ``start()`` instead.
Treat that (and a module-level ``_sync_loop``) as already healthy and
no-op so the image build stays green across both wheel shapes.
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
        # Module-level method (already patched or fixed upstream).
        if any(line.startswith("    async def _sync_loop") for line in lines):
            print("Matrix channel _sync_loop already at class level; skipping")
            return
        # Current PyPI shape: nested local sync_loop() inside start().
        if any("async def sync_loop(" in line for line in lines):
            print("Matrix channel uses nested sync_loop(); skipping legacy patch")
            return
        raise RuntimeError(
            "could not find mis-indented _sync_loop marker or known-good "
            "sync_loop shape in copaw Matrix channel"
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
