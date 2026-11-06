#!/usr/bin/env python3
"""Smoke-load a real plugin package with the installed QwenPaw loader.

The harness drives the same code path the worker runtime uses at
startup: it copies the plugin package into a scratch working directory's
``plugins/`` tree and calls ``PluginLoader.load_all_plugins``. A healthy
package is returned in the result dict with ``enabled=True``; a broken
package (unreadable manifest, import-time failure, version mismatch) is
dropped from the result by the loader, which turns this script red.

The smoke is keyless: no credentials, services, or network access are
required. The package under test declares no dependencies and ships no
requirements.txt, so the loader performs no pip install.

Exit codes:
    0  the expected plugin loaded (or, with --expect-failure, failed)
    1  the smoke failed (or the negative control loaded a broken package)
    2  usage error (no plugin.json, unreadable manifest)
"""

from __future__ import annotations

import argparse
import asyncio
import contextlib
import os
import json
import logging
import shutil
import sys
import tempfile
from pathlib import Path
from typing import Dict, Optional, Tuple

REPO_ROOT = Path(__file__).resolve().parents[2]
DEFAULT_PLUGIN_DIR = REPO_ROOT / "plugins" / "agentteams-matrix-channel"


class SmokeError(RuntimeError):
    """Raised when the plugin package cannot even be prepared for loading."""


def prepare_plugins_root(plugin_dir: Path, working_dir: Path) -> Path:
    """Copy *plugin_dir* into ``working_dir/plugins`` and return that root."""
    plugins_root = working_dir / "plugins"
    plugins_root.mkdir(parents=True, exist_ok=True)
    target = plugins_root / plugin_dir.name
    if target.exists():
        shutil.rmtree(target)
    shutil.copytree(
        plugin_dir,
        target,
        ignore=shutil.ignore_patterns("__pycache__", "*.pyc"),
    )
    return plugins_root


@contextlib.contextmanager
def scratch_runtime(working_dir: Path):
    """Point QwenPaw's persisted-state bootstrap at the scratch tree.

    The import chain reads ``QWENPAW_WORKING_DIR`` / ``HOME`` while it
    bootstraps, so importing first would write ``.qwenpaw_restore.lock`` and
    friends into the caller's runtime — the exact leak the loader smoke must
    not have. The previous values are restored afterwards so an in-process
    caller keeps its environment.
    """
    keys = ("QWENPAW_WORKING_DIR", "HOME")
    previous = {key: os.environ.get(key) for key in keys}
    os.environ["QWENPAW_WORKING_DIR"] = str(working_dir)
    os.environ["HOME"] = str(working_dir)
    try:
        yield
    finally:
        for key, value in previous.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value


def load_plugins(plugins_root: Path, expected_id: str, working_dir: Path) -> Dict:
    """Run the real loader against *plugins_root* and return its records."""
    with scratch_runtime(working_dir):
        from qwenpaw.plugins.loader import PluginLoader

        loader = PluginLoader([plugins_root])
        # Pass explicit configs to bypass load_config(): the smoke must not
        # read or create any user configuration.
        return asyncio.run(loader.load_all_plugins(configs={expected_id: {}}))


def smoke(
    plugin_dir: Path,
    working_dir: Path,
    plugin_id: Optional[str] = None,
) -> Tuple[str, object]:
    """Copy the package, run the loader, return (plugin_id, record).

    The record may be None: the loader drops plugins it cannot load, and
    the caller decides whether absence is a failure or an expected
    negative-control hit.
    """
    manifest_path = plugin_dir / "plugin.json"
    if not manifest_path.is_file():
        raise SmokeError(f"no plugin.json in {plugin_dir}")
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    expected_id = plugin_id or manifest.get("id")
    if not expected_id:
        raise SmokeError(f"plugin.json in {plugin_dir} has no 'id'")
    plugins_root = prepare_plugins_root(plugin_dir, working_dir)
    records = load_plugins(plugins_root, expected_id, working_dir)
    return expected_id, records.get(expected_id)


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(
        description="Smoke-load a plugin package with the QwenPaw loader.",
    )
    parser.add_argument(
        "--plugin-dir",
        type=Path,
        default=DEFAULT_PLUGIN_DIR,
        help="plugin package directory (default: the shipped matrix-channel)",
    )
    parser.add_argument(
        "--plugin-id",
        default=None,
        help="expected plugin id (default: read from plugin.json)",
    )
    parser.add_argument(
        "--working-dir",
        type=Path,
        default=None,
        help="scratch working directory (default: a temp dir, removed after)",
    )
    parser.add_argument(
        "--expect-failure",
        action="store_true",
        help="negative control: succeed only if the package does NOT load",
    )
    args = parser.parse_args(argv)

    logging.basicConfig(
        level=logging.INFO,
        format="%(levelname)s %(name)s: %(message)s",
    )

    plugin_dir = args.plugin_dir.resolve()
    if args.working_dir is not None:
        working_dir = args.working_dir.resolve()
        working_dir.mkdir(parents=True, exist_ok=True)
        try:
            plugin_id, record = smoke(plugin_dir, working_dir, args.plugin_id)
        except SmokeError as exc:
            print(f"FAIL: {exc}", file=sys.stderr)
            return 2
    else:
        with tempfile.TemporaryDirectory(prefix="qwenpaw-smoke-") as tmp:
            try:
                plugin_id, record = smoke(
                    plugin_dir, Path(tmp), args.plugin_id
                )
            except SmokeError as exc:
                print(f"FAIL: {exc}", file=sys.stderr)
                return 2

    loaded = record is not None and record.enabled
    if args.expect_failure:
        if loaded:
            print(
                f"FAIL: negative control broken — '{plugin_id}' loaded "
                "cleanly; the smoke must reject this package",
                file=sys.stderr,
            )
            return 1
        print(f"OK: negative control — '{plugin_id}' correctly failed to load")
        return 0

    if record is None:
        print(
            f"FAIL: plugin '{plugin_id}' missing from loader result "
            "(broken package?)",
            file=sys.stderr,
        )
        return 1
    if not record.enabled:
        print(
            f"FAIL: plugin '{plugin_id}' loaded disabled: "
            f"{record.diagnostics}",
            file=sys.stderr,
        )
        return 1
    print(
        f"OK: plugin '{plugin_id}' v{record.manifest.version} "
        "loaded and enabled"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
