# -*- coding: utf-8 -*-
"""register() idempotency smoke (acceptance criterion: register() 幂等).

Simulates the new-framework update_config replay: the same ``PluginApi``
calls ``register()`` twice against the same registry — the second call
must not raise, and no route/hook registration may be duplicated.

Uses the real ``PluginRegistry`` singleton and a real FastAPI app
(``set_plugin_http_app`` before ``register_http_router``), mirroring how
the framework drives plugins in production.
"""

# pylint: disable=protected-access,redefined-outer-name
from __future__ import annotations

import importlib.util
import os
import sys
from pathlib import Path
from typing import Generator

import pytest

_PLUGIN_DIR = Path(__file__).resolve().parent.parent
_STATE_DIR_ENV = "QWENPAW_CODING_CLI_STATE_DIR"


@pytest.fixture
def plugin_module() -> Generator[object, None, None]:
    spec = importlib.util.spec_from_file_location(
        "at_coding_cli_plugin_entry",
        _PLUGIN_DIR / "plugin.py",
    )
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    yield module
    sys.modules.pop(spec.name, None)


@pytest.fixture
def state_env(tmp_path) -> Generator[None, None, None]:
    saved = os.environ.get(_STATE_DIR_ENV)
    os.environ[_STATE_DIR_ENV] = str(tmp_path / "state")
    yield
    if saved is None:
        os.environ.pop(_STATE_DIR_ENV, None)
    else:
        os.environ[_STATE_DIR_ENV] = saved


def test_register_is_idempotent(plugin_module, state_env, tmp_path) -> None:
    from fastapi import FastAPI
    from fastapi.testclient import TestClient

    from qwenpaw.plugins.api import PluginApi
    from qwenpaw.plugins.registry import PluginRegistry

    plugin_id = str(plugin_module.PLUGIN_ID)  # type: ignore[attr-defined]
    prefix = str(plugin_module.HTTP_PREFIX)  # type: ignore[attr-defined]
    plugin = plugin_module.plugin  # type: ignore[attr-defined]

    app = FastAPI()
    registry = PluginRegistry()  # singleton
    registry.set_plugin_http_app(app)

    api = PluginApi(plugin_id=plugin_id, config={})
    api.set_registry(registry)

    try:
        plugin.register(api)
        plugin.register(api)  # second call: must not raise

        # The management API is reachable under /api/<prefix>. (FastAPI
        # 0.143 mounts an included router as one _IncludedRouter entry in
        # app.router.routes, so assert via real requests.)
        client = TestClient(app)
        res = client.get(f"/api{prefix}")
        assert res.status_code == 200
        assert {c["id"] for c in res.json()["clis"]} == {
            "qwen-code",
            "opencode",
            "qoder",
        }
        assert client.get(f"/api{prefix}/nope").status_code == 404

        # Exactly one registration per kind (no duplicates).
        http_regs = [
            r
            for r in registry.get_http_router_registrations()
            if r.plugin_id == plugin_id
        ]
        assert len(http_regs) == 1
        assert http_regs[0].prefix == prefix

        startup = [
            h
            for h in registry.get_startup_hooks()
            if h.plugin_id == plugin_id
        ]
        # [/qcode] adds its own deferred startup/workspace hooks, so the
        # contract is "no duplicates" rather than "exactly one".
        names = [h.hook_name for h in startup]
        assert len(names) == len(set(names)), names
        assert sum(1 for n in names if n.endswith("ensure_state_dir")) == 1
        assert any(
            ("slash_cmd_" in n or "slash_command_" in n) for n in names
        ), names
        shutdown = [
            h
            for h in registry.get_shutdown_hooks()
            if h.plugin_id == plugin_id
        ]
        assert len(shutdown) == 1

        # The state-dir startup hook creates the state directory.
        next(
            h for h in startup if h.hook_name.endswith("ensure_state_dir")
        ).callback()
        assert (tmp_path / "state").is_dir()
    finally:
        registry.unregister_plugin(plugin_id)

    # Cleanup removed the mounted routes again.
    assert TestClient(app).get(f"/api{prefix}").status_code == 404
