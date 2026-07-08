#!/usr/bin/env python3
"""Install bundled QwenPaw plugins into an image-local working directory."""

from __future__ import annotations

import argparse
import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import zipfile


MARKER = ".agentteams-builtin-plugin.sha256"


def _safe_extract(zip_path: Path, target_dir: Path) -> Path:
    with zipfile.ZipFile(zip_path) as archive:
        target_root = target_dir.resolve()
        for name in archive.namelist():
            resolved = (target_dir / name).resolve()
            try:
                resolved.relative_to(target_root)
            except ValueError:
                raise RuntimeError(f"unsafe qwenpaw plugin package path: {name}") from None
        archive.extractall(target_dir)

    packages = [
        path
        for path in target_dir.iterdir()
        if path.is_dir() and (path / "plugin.json").is_file()
    ]
    if len(packages) != 1:
        raise RuntimeError(f"expected one qwenpaw plugin package in {zip_path}")
    return packages[0]


def _directory_digest(plugin_dir: Path) -> str:
    digest = hashlib.sha256()
    for path in sorted(plugin_dir.rglob("*")):
        if not path.is_file() or path.name == MARKER:
            continue
        rel = path.relative_to(plugin_dir).as_posix()
        digest.update(rel.encode("utf-8"))
        digest.update(b"\0")
        digest.update(path.read_bytes())
        digest.update(b"\0")
    return digest.hexdigest()


def _write_markers(working_dir: Path) -> None:
    plugins_dir = working_dir / "plugins"
    for plugin_dir in sorted(path for path in plugins_dir.iterdir() if path.is_dir()):
        marker = plugin_dir / MARKER
        marker.write_text(_directory_digest(plugin_dir) + "\n", encoding="utf-8")


def _install(zip_path: Path, working_dir: Path) -> None:
    qwenpaw_bin = shutil.which("qwenpaw") or str(Path(sys.executable).with_name("qwenpaw"))
    env = dict(os.environ)
    env["QWENPAW_WORKING_DIR"] = str(working_dir)
    with tempfile.TemporaryDirectory(prefix="qwenpaw-builtin-plugin-") as tmp:
        package_dir = _safe_extract(zip_path, Path(tmp))
        subprocess.run([qwenpaw_bin, "plugin", "install", str(package_dir), "--force"], check=True, env=env)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--working-dir", required=True)
    parser.add_argument("packages", nargs="+")
    args = parser.parse_args()

    working_dir = Path(args.working_dir)
    working_dir.mkdir(parents=True, exist_ok=True)
    for package in args.packages:
        _install(Path(package), working_dir)
    _write_markers(working_dir)


if __name__ == "__main__":
    main()
