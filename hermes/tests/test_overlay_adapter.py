"""Tests for hermes_matrix.overlay_adapter's outbound quiet-rooms wrapper.

hermes_matrix.overlay_adapter imports ``gateway.config.PlatformConfig`` and
``gateway.platforms._matrix_native.MatrixAdapter`` — both only exist inside
the built hermes-agent image (this repo's lean pytest image does not vendor
hermes-agent). We stub minimal fakes into ``sys.modules`` before import so
this module can be unit-tested with a stubbed client, per the plan's ⚠️
S3 note: "unit tests stub the client."
"""
from __future__ import annotations

import sys
import types
from typing import Any

import pytest


def _install_gateway_stubs() -> None:
    """Install the minimal ``gateway.config`` / ``gateway.platforms._matrix_native``
    surface that hermes_matrix.overlay_adapter imports at module scope.
    """
    if "gateway" in sys.modules and getattr(
        sys.modules["gateway"], "_hiclaw_test_stub", False
    ):
        return  # already installed by an earlier test in this session

    gateway_mod = types.ModuleType("gateway")
    gateway_mod._hiclaw_test_stub = True  # type: ignore[attr-defined]

    config_mod = types.ModuleType("gateway.config")

    class PlatformConfig:  # noqa: D401 - minimal stand-in
        pass

    config_mod.PlatformConfig = PlatformConfig

    platforms_mod = types.ModuleType("gateway.platforms")
    native_mod = types.ModuleType("gateway.platforms._matrix_native")

    class _FakeNativeMatrixAdapter:
        """Stand-in for hermes-agent's real mautrix-based adapter.

        Only implements the surface overlay_adapter.MatrixAdapter touches:
        __init__ stash of config, an async connect() returning True, and a
        _client attribute the test wires up directly.
        """

        def __init__(self, config: Any) -> None:
            self._config = config
            self._client: Any = None
            self._user_id = "@worker:matrix.local"

        async def connect(self) -> bool:
            return True

        async def _is_dm_room(self, room_id: str) -> bool:
            return False

        async def _get_display_name(self, room_id: str, sender: str) -> str:
            return sender

        async def _resolve_message_context(self, *args: Any, **kwargs: Any):
            return None

        async def _handle_media_message(self, *args: Any, **kwargs: Any) -> None:
            return None

    native_mod.MatrixAdapter = _FakeNativeMatrixAdapter

    sys.modules["gateway"] = gateway_mod
    sys.modules["gateway.config"] = config_mod
    sys.modules["gateway.platforms"] = platforms_mod
    sys.modules["gateway.platforms._matrix_native"] = native_mod


_install_gateway_stubs()

from hermes_matrix.overlay_adapter import MatrixAdapter  # noqa: E402


class _FakeSentEvent:
    def __init__(self, room_id: Any, event_type: Any, content: Any):
        self.room_id = room_id
        self.event_type = event_type
        self.content = content


class _FakeClient:
    def __init__(self) -> None:
        self.sent: list[_FakeSentEvent] = []

    async def send_message_event(
        self, room_id: Any, event_type: Any, content: Any, *args: Any, **kwargs: Any
    ) -> _FakeSentEvent:
        event = _FakeSentEvent(room_id, event_type, content)
        self.sent.append(event)
        return event


async def _connect_and_send_return(adapter: MatrixAdapter, content: dict) -> Any:
    """Like ``_connect_and_send`` but returns the wrapped call's own result,
    so suppression tests can assert on what the caller actually sees.
    """
    await adapter.connect()
    client = adapter._client
    return await client.send_message_event(
        "!room:matrix.local", "m.room.message", content
    )


@pytest.fixture()
def adapter(monkeypatch: pytest.MonkeyPatch) -> MatrixAdapter:
    for name in ("MATRIX_FILTER_TOOL_MESSAGES", "MATRIX_FILTER_THINKING"):
        monkeypatch.delenv(name, raising=False)
    a = MatrixAdapter(config=object())
    a._client = _FakeClient()
    return a


async def _connect_and_send(adapter: MatrixAdapter, content: dict) -> list:
    await adapter.connect()
    client = adapter._client
    await client.send_message_event("!room:matrix.local", "m.room.message", content)
    return client.sent


@pytest.mark.asyncio
async def test_filters_off_by_default_everything_passes(
    adapter: MatrixAdapter,
) -> None:
    tool_content = {"body": "ran a tool", "hermes.event_kind": "tool"}
    sent = await _connect_and_send(adapter, tool_content)
    assert len(sent) == 1
    assert sent[0].content["body"] == "ran a tool"


@pytest.mark.asyncio
async def test_tool_and_thinking_suppressed_when_env_enabled(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("MATRIX_FILTER_TOOL_MESSAGES", "true")
    monkeypatch.setenv("MATRIX_FILTER_THINKING", "true")
    a = MatrixAdapter(config=object())
    a._client = _FakeClient()

    tool_result = await _connect_and_send_return(
        a, {"body": "ran a tool", "hermes.event_kind": "tool"}
    )
    assert a._client.sent == []
    assert tool_result is not None
    assert isinstance(tool_result, str) and tool_result
    assert "suppress" in tool_result.lower()

    thinking_result = await _connect_and_send_return(
        a, {"body": "pondering", "hermes.event_kind": "thinking"}
    )
    assert a._client.sent == []
    assert thinking_result is not None
    assert isinstance(thinking_result, str) and thinking_result
    assert "suppress" in thinking_result.lower()


@pytest.mark.asyncio
async def test_plain_final_and_lifecycle_events_always_pass(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("MATRIX_FILTER_TOOL_MESSAGES", "true")
    monkeypatch.setenv("MATRIX_FILTER_THINKING", "true")
    a = MatrixAdapter(config=object())
    a._client = _FakeClient()
    await a.connect()
    client = a._client

    await client.send_message_event(
        "!room:matrix.local", "m.room.message", {"body": "final answer"}
    )
    await client.send_message_event(
        "!room:matrix.local",
        "m.room.message",
        {"body": "[start]", "hermes.event_kind": "start"},
    )
    await client.send_message_event(
        "!room:matrix.local",
        "m.room.message",
        {"body": "[finish]", "hermes.event_kind": "finish"},
    )
    await client.send_message_event(
        "!room:matrix.local",
        "m.room.message",
        {"body": "[heartbeat]", "hermes.event_kind": "heartbeat"},
    )

    assert len(client.sent) == 4


@pytest.mark.asyncio
async def test_replace_edit_always_passes_even_when_tagged_as_tool(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("MATRIX_FILTER_TOOL_MESSAGES", "true")
    monkeypatch.setenv("MATRIX_FILTER_THINKING", "true")
    a = MatrixAdapter(config=object())
    a._client = _FakeClient()

    edit_content = {
        "body": "* edited final text",
        "hermes.event_kind": "tool",
        "m.relates_to": {"rel_type": "m.replace", "event_id": "$abc"},
    }
    sent = await _connect_and_send(a, edit_content)
    assert len(sent) == 1
