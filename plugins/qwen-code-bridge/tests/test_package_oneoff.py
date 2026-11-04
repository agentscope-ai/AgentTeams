# -*- coding: utf-8 -*-
"""Package layout regression test (v0.2.0 B6).

Rebuilds the distributable zip from the working tree with exactly the
logic of ``scripts/package.sh`` (same exclusions, same arcname layout,
ZIP_DEFLATED, version read from plugin.json) and asserts the layout.
Output goes to /tmp; nothing in the repo is modified.
"""
from __future__ import annotations

import json
import os
import re
import sys
import zipfile
from pathlib import Path

PLUGIN_ROOT = Path(__file__).resolve().parent.parent
NAME = "qwen-code-bridge"

EXCLUDE_DIRS = {"__pycache__", "tests", "state"}
EXCLUDE_FILE_SUFFIXES = (".pyc", ".log")


def _plugin_version() -> str:
    return (
        json.loads((PLUGIN_ROOT / "plugin.json").read_text(encoding="utf-8"))
        .get("version")
        or ""
    )


def _build_zip(out: Path) -> int:
    count = 0
    with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as zf:
        for root, dirs, files in os.walk(PLUGIN_ROOT):
            dirs[:] = [d for d in dirs if d not in EXCLUDE_DIRS]
            if os.path.relpath(root, PLUGIN_ROOT) == os.path.join(
                "web-shell", "dist"
            ):
                dirs[:] = []
            for fname in sorted(files):
                if fname.endswith(EXCLUDE_FILE_SUFFIXES):
                    continue
                full = os.path.join(root, fname)
                arcname = os.path.join(NAME, os.path.relpath(full, PLUGIN_ROOT))
                zf.write(full, arcname)
                count += 1
    return count


def test_package_layout() -> None:
    version = _plugin_version()
    assert re.fullmatch(r"\d+\.\d+\.\d+", version), (
        f"unexpected plugin version: {version!r}"
    )
    out = Path("/tmp") / f"{NAME}-{version}.zip"
    count = _build_zip(out)
    assert out.exists()
    with zipfile.ZipFile(out) as zf:
        names = zf.namelist()
        assert all(n.startswith(f"{NAME}/") for n in names)
        assert f"{NAME}/plugin.json" in names
        assert f"{NAME}/plugin.yaml" in names
        assert f"{NAME}/plugin.py" in names
        assert f"{NAME}/qwen_code_bridge/dispatch.py" in names
        assert f"{NAME}/scripts/package.sh" in names
        # exclusions hold
        assert not any("/tests/" in n or n.endswith("/tests") for n in names)
        assert not any("__pycache__" in n for n in names)
        assert not any(n.endswith(".pyc") or n.endswith(".log") for n in names)
    size = out.stat().st_size
    # Emit an `unzip -l`-style manifest to /tmp for the closeout report.
    with zipfile.ZipFile(out) as zf:
        infos = zf.infolist()
    lines = [
        f"Archive:  {out}",
        f"{'Length':>8}  {'Date':<10}  Name",
        "------  ----------  ------------------------------------",
    ]
    for info in infos[:13]:
        year, month, day, hour, minute, _sec = info.date_time
        stamp = f"{year:04d}-{month:02d}-{day:02d} {hour:02d}:{minute:02d}"
        lines.append(
            f"{info.file_size:>8}  {stamp}  {info.filename}"
        )
    lines.append(f"{len(infos)} files total, {size} bytes on disk")
    (out.with_suffix(".zip.manifest.txt")).write_text(
        "\n".join(lines) + "\n", encoding="utf-8"
    )
    print(f"\npackaged {count} files -> {out} ({size} bytes)")
    sys.stdout.flush()
