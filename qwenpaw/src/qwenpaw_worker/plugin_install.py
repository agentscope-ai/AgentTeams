"""Unified QwenPaw plugin zip extract and install helpers."""

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

BUILTIN_QWENPAW_PLUGIN_MARKER = ".agentteams-builtin-plugin.sha256"


def safe_extract_zip(zip_path: Path, target_dir: Path) -> Path:
    """Extract a QwenPaw plugin zip safely and return the package directory."""
    with zipfile.ZipFile(zip_path) as archive:
        target_root = target_dir.resolve()
        for name in archive.namelist():
            resolved = (target_dir / name).resolve()
            try:
                resolved.relative_to(target_root)
            except ValueError as exc:
                raise RuntimeError(f"unsafe qwenpaw plugin package path: {name}") from exc
        archive.extractall(target_dir)

    packages = [
        path
        for path in target_dir.iterdir()
        if path.is_dir() and (path / "plugin.json").is_file()
    ]
    if len(packages) != 1:
        raise RuntimeError(f"expected one qwenpaw plugin package in {zip_path}")
    return packages[0]


def directory_digest(plugin_dir: Path, *, marker: str = BUILTIN_QWENPAW_PLUGIN_MARKER) -> str:
    digest = hashlib.sha256()
    for path in sorted(plugin_dir.rglob("*")):
        if not path.is_file() or path.name == marker:
            continue
        rel = path.relative_to(plugin_dir).as_posix()
        digest.update(rel.encode("utf-8"))
        digest.update(b"\0")
        digest.update(path.read_bytes())
        digest.update(b"\0")
    return digest.hexdigest()


def write_builtin_markers(working_dir: Path, *, marker: str = BUILTIN_QWENPAW_PLUGIN_MARKER) -> None:
    plugins_dir = working_dir / "plugins"
    if not plugins_dir.is_dir():
        return
    for plugin_dir in sorted(path for path in plugins_dir.iterdir() if path.is_dir()):
        marker_path = plugin_dir / marker
        marker_path.write_text(directory_digest(plugin_dir, marker=marker) + "\n", encoding="utf-8")


def run_qwenpaw_plugin_install(qwenpaw_bin: str, package_dir: Path, *, env: dict[str, str] | None = None) -> None:
    command = [qwenpaw_bin, "plugin", "install", str(package_dir), "--force"]
    subprocess.run(command, check=True, env=env)


def install_qwenpaw_plugin_package(
    qwenpaw_bin: str,
    plugin_source: Path,
    *,
    temp_prefix: str = "qwenpaw-plugin-",
    env: dict[str, str] | None = None,
) -> None:
    if not plugin_source.exists():
        raise RuntimeError(f"qwenpaw plugin package missing: {plugin_source}")

    if plugin_source.is_dir():
        run_qwenpaw_plugin_install(qwenpaw_bin, plugin_source, env=env)
        return

    if zipfile.is_zipfile(plugin_source):
        with tempfile.TemporaryDirectory(prefix=temp_prefix) as tmp:
            package_dir = safe_extract_zip(plugin_source, Path(tmp))
            run_qwenpaw_plugin_install(qwenpaw_bin, package_dir, env=env)
        return

    raise RuntimeError(f"qwenpaw plugin package must be a directory or zip: {plugin_source}")


def _cmd_extract(args: argparse.Namespace) -> int:
    package_dir = safe_extract_zip(Path(args.zip_path), Path(args.target_dir))
    print(package_dir)
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="QwenPaw plugin install helpers")
    subparsers = parser.add_subparsers(dest="command", required=True)

    extract_parser = subparsers.add_parser("extract", help="Safely extract a QwenPaw plugin zip")
    extract_parser.add_argument("zip_path")
    extract_parser.add_argument("target_dir")
    extract_parser.set_defaults(func=_cmd_extract)

    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
