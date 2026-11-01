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
import sys
import zipfile
from pathlib import Path

PLUGIN_ROOT = Path(__file__).resolve().parent.parent
NAME = "qwen-code-bridge"
OUT = Path("/tmp") / f"{NAME}-0.2.0.zip"

EXCLUDE_DIRS = {"__pycache__", "tests", "state"}
EXCLUDE_FILE_SUFFIXES = (".pyc", ".log")


def _build_zip(out: Path) -> int:
    version = (
        json.loads((PLUGIN_ROOT / "plugin.json").read_text(encoding="utf-8"))
        .get("version")
        or ""
    )
    assert version == "0.2.0", f"unexpected plugin version: {version!r}"
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
    count = _build_zip(OUT)
    assert OUT.exists()
    with zipfile.ZipFile(OUT) as zf:
        names = zf.namelist()
        assert all(n.startswith(f"{NAME}/") for n in names)
        assert f"{NAME}/plugin.json" in names
        assert f"{NAME}/plugin.py" in names
        assert f"{NAME}/qwen_code_bridge/dispatch.py" in names
        assert f"{NAME}/scripts/package.sh" in names
        # exclusions hold
        assert not any("/tests/" in n or n.endswith("/tests") for n in names)
        assert not any("__pycache__" in n for n in names)
        assert not any(n.endswith(".pyc") or n.endswith(".log") for n in names)
    size = OUT.stat().st_size
    # Emit an `unzip -l`-style manifest to /tmp for the closeout report.
    with zipfile.ZipFile(OUT) as zf:
        infos = zf.infolist()
    lines = [
        f"Archive:  {OUT}",
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
    (OUT.with_suffix(".zip.manifest.txt")).write_text(
        "\n".join(lines) + "\n", encoding="utf-8"
    )
    print(f"\npackaged {count} files -> {OUT} ({size} bytes)")
    sys.stdout.flush()
