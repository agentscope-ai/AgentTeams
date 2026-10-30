"""Regression: non-vision image downgrade must not resolve the Matrix context twice."""

from __future__ import annotations

import asyncio
import importlib.util
import sys
import types
from pathlib import Path

from hermes_matrix.policies import HistoryBuffer


def _load_overlay_with_fake_gateway(monkeypatch):
    class FakePlatformConfig:
        pass

    class FakeNative:
        def __init__(self):
            self.resolve_calls = 0
            self.text_calls = 0
            self.build_ctx = None
            self.build_content = None
            self.handled = []

        async def _is_dm_room(self, _room_id):
            return False

        async def _get_display_name(self, _room_id, _sender):
            return "alice"

        async def _resolve_message_context(
            self, room_id, sender, event_id, body, source_content, relates_to,
            mention_claimed=False, voice_gate=None,
        ):
            self.resolve_calls += 1
            return body, False, "group", None, "alice", {"room": room_id}

        async def _build_inbound_event(
            self, room_id, sender, event_id, body, source_content, relates_to,
            ctx=None, **extra,
        ):
            self.build_ctx = ctx
            self.build_content = source_content
            return types.SimpleNamespace(message_type="text")

        async def _handle_text_message(self, *args, **kwargs):
            self.text_calls += 1

        async def handle_message(self, event):
            self.handled.append(event)

    gateway = types.ModuleType("gateway")
    gateway_config = types.ModuleType("gateway.config")
    gateway_config.PlatformConfig = FakePlatformConfig
    gateway_platforms = types.ModuleType("gateway.platforms")
    gateway_event = types.ModuleType("gateway.platforms.event")
    gateway_event.MessageType = types.SimpleNamespace(TEXT="text", COMMAND="command")
    gateway_platforms.event = gateway_event
    gateway_platforms_adapter = types.ModuleType("gateway.platforms.matrix")
    gateway_platforms_matrix_adapter = types.ModuleType("gateway.platforms.matrix.adapter")
    gateway_platforms_matrix_adapter.MatrixAdapter = FakeNative

    plugin = types.ModuleType("plugins")
    plugin_platforms = types.ModuleType("plugins.platforms")
    plugin_matrix = types.ModuleType("plugins.platforms.matrix")
    plugin_matrix_adapter = types.ModuleType("plugins.platforms.matrix.adapter")
    plugin_matrix_adapter.MatrixAdapter = FakeNative

    monkeypatch.setitem(sys.modules, "gateway", gateway)
    monkeypatch.setitem(sys.modules, "gateway.config", gateway_config)
    monkeypatch.setitem(sys.modules, "gateway.platforms", gateway_platforms)
    monkeypatch.setitem(sys.modules, "gateway.platforms.event", gateway_event)
    monkeypatch.setitem(sys.modules, "gateway.platforms.matrix", gateway_platforms_adapter)
    monkeypatch.setitem(sys.modules, "gateway.platforms.matrix.adapter", gateway_platforms_matrix_adapter)
    monkeypatch.setitem(sys.modules, "plugins", plugin)
    monkeypatch.setitem(sys.modules, "plugins.platforms", plugin_platforms)
    monkeypatch.setitem(sys.modules, "plugins.platforms.matrix", plugin_matrix)
    monkeypatch.setitem(sys.modules, "plugins.platforms.matrix.adapter", plugin_matrix_adapter)

    overlay_path = Path(__file__).resolve().parents[1] / "src" / "hermes_matrix" / "overlay_adapter.py"
    spec = importlib.util.spec_from_file_location("test_overlay_target", overlay_path)
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module, FakeNative


def test_nonvision_image_downgrade_resolves_context_once(monkeypatch):
    module, fake_native = _load_overlay_with_fake_gateway(monkeypatch)
    adapter = object.__new__(module.MatrixAdapter)

    class Allow:
        def permits(self, _sender, is_dm=False):
            return True

    adapter._dual_allow = Allow()
    adapter._history = HistoryBuffer(limit=10)
    adapter._history.record("!room", "bob", "prior group context")
    adapter._vision_enabled = False
    adapter._text_batch_delay_seconds = 0
    fake_native.__init__(adapter)

    asyncio.run(adapter._handle_media_message(
        "!room", "@alice:server", "$event", 0.0,
        {"msgtype": "m.image", "body": "photo.png"},
        {}, "m.image",
    ))

    assert adapter.resolve_calls == 1
    assert adapter.text_calls == 0
    assert adapter.build_ctx is not None
    resolved_body = adapter.build_ctx[0]
    assert "prior group context" in resolved_body
    assert "[User sent an image" in resolved_body
    assert adapter.build_content["msgtype"] == "m.text"
    assert len(adapter.handled) == 1
