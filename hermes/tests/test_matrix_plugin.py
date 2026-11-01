"""Tests for the AgentTeams Matrix user-plugin bridge."""

from __future__ import annotations

import importlib.util
import sys
import types
from pathlib import Path


def _load_plugin_module():
    plugin_path = Path(__file__).resolve().parents[1] / "plugins" / "platforms" / "matrix" / "adapter.py"
    spec = importlib.util.spec_from_file_location("agentteams_matrix_plugin", plugin_path)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_register_swaps_bundled_adapter_and_delegates(monkeypatch) -> None:
    native_adapter_class = object()
    overlay_adapter_class = object()
    calls: list[tuple[str, object]] = []

    native = types.ModuleType("plugins.platforms.matrix.adapter")
    native.MatrixAdapter = native_adapter_class

    def native_register(ctx):
        calls.append(("register", native.MatrixAdapter))

    native.register = native_register

    overlay = types.ModuleType("hermes_matrix.overlay_adapter")
    overlay.MatrixAdapter = overlay_adapter_class

    monkeypatch.setitem(sys.modules, "plugins", types.ModuleType("plugins"))
    monkeypatch.setitem(sys.modules, "plugins.platforms", types.ModuleType("plugins.platforms"))
    monkeypatch.setitem(sys.modules, "plugins.platforms.matrix", types.ModuleType("plugins.platforms.matrix"))
    monkeypatch.setitem(sys.modules, "plugins.platforms.matrix.adapter", native)
    monkeypatch.setitem(sys.modules, "hermes_matrix", types.ModuleType("hermes_matrix"))
    monkeypatch.setitem(sys.modules, "hermes_matrix.overlay_adapter", overlay)

    plugin = _load_plugin_module()
    plugin.register(object())

    assert calls == [("register", overlay_adapter_class)]
    assert native.MatrixAdapter is overlay_adapter_class
