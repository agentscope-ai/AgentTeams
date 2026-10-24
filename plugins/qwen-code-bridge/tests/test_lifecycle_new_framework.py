# -*- coding: utf-8 -*-
"""D3 dual-framework acceptance: new-framework lifecycle (reload / clean unload).

Old framework (pre-#7565) has no ``PluginLifecycle`` — this module skips there
(importorskip), so the same pytest run stays green on both trees.

New-framework claims covered here:
  1. ``load`` registers the plugin's contributions (router/hooks/middleware).
  2. ``reload`` (same source) succeeds without duplicating registrations.
  3. File-backed dispatch state (design D5: no module globals) survives reload:
     a fresh ``DispatchService`` reads the same state dir.
  4. ``unload(UNLOAD)`` leaves zero registrations for the plugin (clean disable)
     and does not delete the plugin source directory.
"""

# pylint: disable=protected-access,redefined-outer-name
from __future__ import annotations

import json
import shutil
from pathlib import Path

import pytest

_PLUGIN_DIR = Path(__file__).resolve().parent.parent
_STATE_DIR_ENV = "QWENPAW_CODING_CLI_STATE_DIR"

_lifecycle = pytest.importorskip("qwenpaw.plugins.lifecycle")


@pytest.fixture
def workspace(tmp_path, monkeypatch):
    installed = tmp_path / "plugins" / "qwen-code-bridge"
    shutil.copytree(
        _PLUGIN_DIR,
        installed,
        ignore=shutil.ignore_patterns("__pycache__", "tests", ".pytest_cache"),
    )
    state = tmp_path / "coding-cli-state"
    monkeypatch.setenv(_STATE_DIR_ENV, str(state))
    return installed, state


def _manifest(installed: Path):
    from qwenpaw.plugins.architecture import PluginManifest

    return PluginManifest.from_dict(
        json.loads((installed / "plugin.json").read_text(encoding="utf-8")),
    )


def _regs(registry, plugin_id: str):
    http = [
        r
        for r in registry.get_http_router_registrations()
        if r.plugin_id == plugin_id
    ]
    startup = [
        h for h in registry.get_startup_hooks() if h.plugin_id == plugin_id
    ]
    shutdown = [
        h for h in registry.get_shutdown_hooks() if h.plugin_id == plugin_id
    ]
    middlewares = [
        r
        for r in getattr(registry, "_middleware_registrations", [])
        if r.plugin_id == plugin_id
    ]
    return http, startup, shutdown, middlewares


@pytest.mark.asyncio
async def test_load_reload_unload_is_clean(workspace) -> None:
    from qwenpaw.plugins.loader import PluginLoader
    from qwenpaw.plugins.registry import PluginRegistry

    installed, state = workspace
    from fastapi import FastAPI

    registry = PluginRegistry()
    registry.set_plugin_http_app(FastAPI())
    loader = PluginLoader(plugin_dirs=[installed.parent])
    loader.registry = registry
    lifecycle = _lifecycle.PluginLifecycle(loader)
    manifest = _manifest(installed)
    plugin_id = manifest.id

    await lifecycle.load(manifest, installed)
    try:
        http, startup, shutdown, middlewares = _regs(registry, plugin_id)
        assert http, "http router not registered"
        assert startup, "startup hook not registered"
        assert shutdown, "shutdown hook not registered"
        assert middlewares, "middleware factory not registered"

        # D5: dispatch state is file-backed -> visible to any fresh instance.
        dispatch_dir = state / "dispatch"
        dispatch_dir.mkdir(parents=True, exist_ok=True)
        (dispatch_dir / "0123456789abcdef0123456789abcdef.json").write_text(
            json.dumps({"task_id": "0123456789abcdef0123456789abcdef", "status": "running"}),
            encoding="utf-8",
        )
        from qwen_code_bridge.dispatch import DispatchService

        assert DispatchService().get_task("0123456789abcdef0123456789abcdef")["status"] == (
            "running"
        )

        # reload: same source, must succeed and not duplicate registrations
        startup_names = sorted(h.hook_name for h in startup)
        report = await lifecycle.reload(plugin_id)
        assert getattr(report, "ok", True), report
        http2, startup2, shutdown2, middlewares2 = _regs(registry, plugin_id)
        assert len(http2) == 1
        assert len(middlewares2) == 1
        # reload must not duplicate or drop startup hooks (slash hooks included)
        assert sorted(h.hook_name for h in startup2) == startup_names

        # state survives reload (fresh service instance, same dir)
        assert DispatchService().get_task("0123456789abcdef0123456789abcdef")["status"] == (
            "running"
        )
    finally:
        unload = await lifecycle.unload(
            plugin_id,
            _lifecycle.UnloadMode.UNLOAD,
        )
        assert getattr(unload, "ok", True), unload

    http3, startup3, shutdown3, middlewares3 = _regs(registry, plugin_id)
    assert not http3, "http router left behind after unload"
    assert not startup3, "startup hook left behind after unload"
    assert not shutdown3, "shutdown hook left behind after unload"
    assert not middlewares3, "middleware left behind after unload"
    assert installed.is_dir(), "UNLOAD must not delete the plugin source"
