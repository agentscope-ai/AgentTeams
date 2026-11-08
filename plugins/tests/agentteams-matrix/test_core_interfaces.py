"""Unit tests for the runtime-neutral core interfaces (P9, PR1 M1)."""

from __future__ import annotations

import dataclasses
from pathlib import Path

import pytest

from core.interfaces import (
    INBOUND_KINDS,
    OUTBOUND_KINDS,
    TYPING_STATES,
    CoreConfig,
    InboundEvent,
    MatrixTransport,
    MediaRef,
    OutboundMessage,
    RuntimeBridge,
    SessionStore,
)


# ---------------------------------------------------------------------------
# Dataclasses
# ---------------------------------------------------------------------------


def test_media_ref_defaults_and_roundtrip():
    ref = MediaRef(mxc_uri="mxc://local/img1")
    assert ref.mime_type is None
    assert ref.file_name is None
    assert ref.size is None
    full = MediaRef(
        mxc_uri="mxc://local/img1",
        mime_type="image/png",
        file_name="shot.png",
        size=42,
    )
    data = full.to_dict()
    assert data == {
        "mxc_uri": "mxc://local/img1",
        "mime_type": "image/png",
        "file_name": "shot.png",
        "size": 42,
    }
    assert MediaRef.from_dict(data) == full
    # Unknown keys are ignored (forward compatibility).
    assert MediaRef.from_dict({**data, "future_field": 1}) == full


def test_inbound_event_construction_and_defaults():
    event = InboundEvent(event_id="$e1", room_id="!r", sender="@a", ts=1, text="hi")
    assert event.media == []
    assert event.mentions == []
    assert event.thread_root is None
    assert event.kind == "text"
    for kind in INBOUND_KINDS:
        InboundEvent(event_id="$e", room_id="!r", sender="@a", ts=0, text="", kind=kind)


@pytest.mark.parametrize("kind", ["image", "TEXT", "", None])
def test_inbound_event_rejects_unknown_kind(kind):
    with pytest.raises(ValueError):
        InboundEvent(event_id="$e", room_id="!r", sender="@a", ts=0, text="t", kind=kind)


@pytest.mark.parametrize("field", ["event_id", "room_id", "sender"])
def test_inbound_event_requires_identity_fields(field):
    kwargs = dict(event_id="$e", room_id="!r", sender="@a", ts=0, text="t")
    kwargs[field] = ""
    with pytest.raises(ValueError):
        InboundEvent(**kwargs)


def test_inbound_event_is_frozen():
    event = InboundEvent(event_id="$e", room_id="!r", sender="@a", ts=0, text="t")
    with pytest.raises(dataclasses.FrozenInstanceError):
        event.text = "mutated"


def test_outbound_message_construction_and_defaults():
    msg = OutboundMessage(room_id="!r", text="hello")
    assert msg.reply_to is None
    assert msg.kind == "final"
    assert msg.media == ()
    for kind in OUTBOUND_KINDS:
        OutboundMessage(room_id="!r", text="t", kind=kind)


@pytest.mark.parametrize("kind", ["reply", "FINAL", ""])
def test_outbound_message_rejects_unknown_kind(kind):
    with pytest.raises(ValueError):
        OutboundMessage(room_id="!r", text="t", kind=kind)


def test_outbound_message_requires_room_id():
    with pytest.raises(ValueError):
        OutboundMessage(room_id="", text="t")


def test_outbound_message_carries_media_and_reply():
    ref = MediaRef(mxc_uri="mxc://local/x")
    msg = OutboundMessage(room_id="!r", text="t", reply_to="$root", kind="tool", media=[ref])
    assert msg.reply_to == "$root"
    assert msg.media == [ref]


# ---------------------------------------------------------------------------
# Protocol shape (runtime checks with fake implementations)
# ---------------------------------------------------------------------------


class _FullTransport:
    async def start(self, on_event):
        pass

    async def stop(self):
        pass

    async def send(self, msg):
        return "$id"

    async def edit(self, room_id, event_id, text):
        pass

    async def react(self, room_id, event_id, key):
        pass

    async def typing(self, room_id, state):
        pass

    async def download(self, media):
        return b""


class _NoDownloadTransport(_FullTransport):
    download = None  # type: ignore[assignment]


def test_transport_protocol_runtime_check():
    assert isinstance(_FullTransport(), MatrixTransport)
    # Missing a single method fails the shape check.
    assert not isinstance(_NoDownloadTransport(), MatrixTransport)


class _FullStore:
    def load_sync_token(self):
        return None

    def save_sync_token(self, token):
        pass

    def load_session_key(self, room_id, sender):
        return None

    def save_session_key(self, room_id, sender, key):
        pass


class _NoSaveTokenStore(_FullStore):
    save_sync_token = None  # type: ignore[assignment]


def test_session_store_protocol_runtime_check():
    assert isinstance(_FullStore(), SessionStore)
    assert not isinstance(_NoSaveTokenStore(), SessionStore)


class _FullBridge:
    async def on_inbound(self, event):
        pass

    async def on_runtime_output(self, msg):
        pass


class _NoOutputBridge(_FullBridge):
    on_runtime_output = None  # type: ignore[assignment]


def test_runtime_bridge_protocol_runtime_check():
    assert isinstance(_FullBridge(), RuntimeBridge)
    assert not isinstance(_NoOutputBridge(), RuntimeBridge)


def test_typing_states_constant():
    assert TYPING_STATES == ("start", "stop")


# ---------------------------------------------------------------------------
# CoreConfig
# ---------------------------------------------------------------------------


def test_core_config_defaults_and_socket_resolution(tmp_path):
    config = CoreConfig(state_dir=tmp_path / "state", agent_id="worker-a")
    assert config.state_dir == tmp_path / "state"
    assert config.socket_path is None
    assert config.sidecar_socket_path == tmp_path / "state" / "sidecar.sock"
    assert config.reconnect_initial_delay == 1.0
    assert config.reconnect_max_delay == 60.0
    assert config.reconnect_backoff_factor == 2.0


def test_core_config_explicit_socket_path(tmp_path):
    config = CoreConfig(
        state_dir=tmp_path / "state",
        agent_id="worker-a",
        socket_path=tmp_path / "custom" / "sidecar.sock",
    )
    assert config.sidecar_socket_path == tmp_path / "custom" / "sidecar.sock"


def test_core_config_accepts_str_paths(tmp_path):
    config = CoreConfig(state_dir=str(tmp_path / "state"), agent_id="worker-a")
    assert isinstance(config.state_dir, Path)


@pytest.mark.parametrize(
    "kwargs",
    [
        dict(agent_id=""),
        dict(reconnect_initial_delay=0),
        dict(reconnect_initial_delay=0.5, reconnect_max_delay=0.25),
        dict(reconnect_backoff_factor=0.5),
    ],
)
def test_core_config_validation(tmp_path, kwargs):
    base = dict(state_dir=tmp_path / "state", agent_id="worker-a")
    base.update(kwargs)
    with pytest.raises(ValueError):
        CoreConfig(**base)
