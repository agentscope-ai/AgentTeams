"""NioTransport tests: the matrix-nio MatrixTransport implementation (M5b).

Covers the lifecycle (start/stop with the SyncEngine, keys upload,
crypto degradation), the inbound normalisation (text/media/edit/
redaction, undecryptable Megolm suppression, handler resilience) and
the outbound methods (send/edit/react/typing/download) using a
duck-typed fake client — no network.
"""

from __future__ import annotations

import asyncio
import inspect
from types import SimpleNamespace
from typing import Any, List, Optional, Tuple

import pytest

pytest.importorskip("nio")

from nio import (  # noqa: E402
    AsyncClient,
    MegolmEvent,
    RoomMessageImage,
    RoomMessageNotice,
    RoomMessageText,
    RedactionEvent,
    SyncResponse as _NioSyncResponse,
)
from nio.responses import (  # noqa: E402
    DeviceList,
    DeviceOneTimeKeyCount,
    KeysUploadResponse,
    MemoryDownloadResponse,
    RoomSendError,
    RoomSendResponse,
    RoomTypingResponse,
    Rooms,
)

from core.interfaces import (  # noqa: E402
    InboundEvent,
    MediaRef,
    OutboundMessage,
)
from core.transport import (  # noqa: E402
    MIN_SYNC_TIMEOUT_MS,
    ROOM_EVENT_FILTER,
    TYPING_TIMEOUT_MS,
    NioTransport,
    TransportConfig,
    build_client,
    normalize_room_event,
)
import core.transport as transport_mod  # noqa: E402

ROOM = SimpleNamespace(room_id="!r:hs.example.com")
SENDER = "@u:hs.example.com"


def _sync_response(next_batch: str) -> _NioSyncResponse:
    return _NioSyncResponse(
        next_batch=next_batch,
        rooms=Rooms(invite={}, join={}, leave={}),
        device_key_count=DeviceOneTimeKeyCount(
            curve25519=0, signed_curve25519=0
        ),
        device_list=DeviceList(changed=[], left=[]),
        to_device_events=[],
        presence_events=[],
    )


class FakeClient:
    """Duck-typed nio ``AsyncClient``: scripted syncs, recorded outbound."""

    def __init__(self, script: Optional[List[tuple]] = None):
        self.script = list(script or [])
        self.event_callbacks: List[tuple] = []
        self.sync_calls: List[dict] = []
        self.joined: List[str] = []
        self.access_token = "tok-1"
        self.sync_timeout_ms = 30000
        # E2EE surface (E2EEMaintainer no-ops while olm is None).
        self.olm = None
        self.should_upload_keys = False
        self.keys_upload_calls = 0
        self.keys_upload_response: Any = KeysUploadResponse(
            curve25519_count=5, signed_curve25519_count=1
        )
        # Outbound recording.
        self.room_send_calls: List[dict] = []
        self.room_send_response: Any = "response"
        self.typing_calls: List[tuple] = []
        self.typing_response: Any = "response"
        self.download_calls: List[str] = []
        self.download_response: Any = MemoryDownloadResponse(
            body=b"media-bytes", content_type="image/jpeg", filename=None
        )
        self.closed = False

    def add_event_callback(self, callback, filter=None):
        self.event_callbacks.append((callback, filter))

    async def sync(self, timeout=30000, full_state=False, since=None, **_kw):
        self.sync_calls.append(
            {"timeout": timeout, "since": since, "full_state": full_state}
        )
        if self.script:
            resp, events = self.script.pop(0)
        else:
            await asyncio.Event().wait()
        for room, event in events:
            for callback, event_types in self.event_callbacks:
                if event_types and not isinstance(event, event_types):
                    continue
                result = callback(room, event)
                if inspect.isawaitable(result):
                    await result
        await asyncio.sleep(0)
        return resp

    async def join(self, room_id: str):
        self.joined.append(room_id)

    async def keys_upload(self):
        self.keys_upload_calls += 1
        return self.keys_upload_response

    async def room_send(
        self,
        room_id,
        message_type,
        content,
        tx_id=None,
        ignore_unverified_devices=False,
    ):
        self.room_send_calls.append(
            {
                "room_id": room_id,
                "message_type": message_type,
                "content": content,
                "tx_id": tx_id,
                "ignore_unverified_devices": ignore_unverified_devices,
            }
        )
        if self.room_send_response == "response":
            return RoomSendResponse(room_id=room_id, event_id="$sent")
        return self.room_send_response

    async def room_typing(self, room_id, typing_state=True, timeout=30000):
        self.typing_calls.append((room_id, typing_state, timeout))
        if self.typing_response == "response":
            return RoomTypingResponse(room_id=room_id)
        return self.typing_response

    async def download(self, mxc=None, filename=None, **_kw):
        self.download_calls.append(mxc)
        return self.download_response

    async def close(self):
        self.closed = True


class FakeStore:
    """SessionStore stand-in recording the sync-token traffic."""

    def __init__(self, token: Optional[str] = None):
        self.token = token
        self.saved: List[str] = []

    def load_sync_token(self):
        return self.token

    def save_sync_token(self, token):
        self.saved.append(token)
        self.token = token

    def load_session_key(self, room_id, sender):
        return None

    def save_session_key(self, room_id, sender, key):
        pass


class FakeSyncEngine:
    """Recording stand-in for ``core.sync.SyncEngine`` (build-path tests)."""

    instances: List["FakeSyncEngine"] = []

    def __init__(self, client, store, sync_timeout_ms, **kwargs):
        self.client = client
        self.store = store
        self.sync_timeout_ms = sync_timeout_ms
        self.on_maintenance = kwargs.get("on_maintenance")
        self.started = False
        self.stopped = False
        FakeSyncEngine.instances.append(self)

    def start(self):
        self.started = True
        return None

    async def stop(self):
        self.stopped = True


def _config(**overrides: Any) -> TransportConfig:
    kwargs: dict = {
        "homeserver": "https://hs.example.com/",
        "user_id": "",
        "access_token": "tok-1",
        "device_id": "",
        "store_path": None,
        "encryption": False,
        "sync_timeout_ms": None,
    }
    kwargs.update(overrides)
    return TransportConfig(**kwargs)


def _run(scenario_factory):
    """Run an async scenario to completion (no pytest-asyncio)."""
    return asyncio.run(scenario_factory())


async def _wait_until(predicate, timeout=5.0):
    loop = asyncio.get_running_loop()
    deadline = loop.time() + timeout
    while not predicate():
        if loop.time() > deadline:
            raise TimeoutError("condition not met in time")
        await asyncio.sleep(0.01)


def _text_event(
    event_id="$t1", body="hello", ts=1700000000000, content=None
) -> RoomMessageText:
    payload = {
        "type": "m.room.message",
        "room_id": "!r:hs.example.com",
        "sender": SENDER,
        "event_id": event_id,
        "origin_server_ts": ts,
        "content": {"msgtype": "m.text", "body": body, **(content or {})},
    }
    return RoomMessageText.from_dict(payload)


def _media_event() -> RoomMessageImage:
    payload = {
        "type": "m.room.message",
        "room_id": "!r:hs.example.com",
        "sender": SENDER,
        "event_id": "$m1",
        "origin_server_ts": 1700000000001,
        "content": {
            "msgtype": "m.image",
            "body": "photo.jpg",
            "url": "mxc://hs.example.com/img1",
            "info": {"mimetype": "image/jpeg", "size": 5},
        },
    }
    return RoomMessageImage.from_dict(payload)


def _edit_event() -> RoomMessageText:
    payload = {
        "type": "m.room.message",
        "room_id": "!r:hs.example.com",
        "sender": SENDER,
        "event_id": "$e1",
        "origin_server_ts": 1700000000002,
        "content": {
            "msgtype": "m.text",
            "body": "* corrected",
            "m.new_content": {"msgtype": "m.text", "body": "corrected"},
            "m.relates_to": {"rel_type": "m.replace", "event_id": "$orig"},
        },
    }
    return RoomMessageText.from_dict(payload)


def _redaction_event() -> RedactionEvent:
    payload = {
        "type": "m.room.redaction",
        "room_id": "!r:hs.example.com",
        "sender": SENDER,
        "event_id": "$r1",
        "origin_server_ts": 1700000000003,
        "content": {"redacts": "$target"},
    }
    return RedactionEvent(payload, redacts="$target")


def _megolm_event() -> MegolmEvent:
    payload = {
        "type": "m.room.encrypted",
        "room_id": "!r:hs.example.com",
        "sender": SENDER,
        "event_id": "$x1",
        "origin_server_ts": 1700000000004,
        "content": {
            "alg": "m.megolm.v1.aes-sha2",
            "ciphertext": "AAAA",
            "sender_key": "KEY",
            "session_id": "SESS",
        },
    }
    return MegolmEvent(
        payload,
        device_id="DEVICE",
        ciphertext="AAAA",
        algorithm="m.megolm.v1.aes-sha2",
    )


# -- lifecycle ---------------------------------------------------------------


def test_start_build_path_starts_sync_engine(tmp_path, monkeypatch):
    FakeSyncEngine.instances = []
    monkeypatch.setattr(transport_mod, "SyncEngine", FakeSyncEngine)

    async def scenario():
        transport = NioTransport(
            _config(encryption=False),
            store=FakeStore(),
            state_dir=tmp_path,
        )
        await transport.start(lambda e: _noop())
        # Default query_token=False → plain nio AsyncClient (the
        # QwenPaw compat subclass is opt-in via TransportConfig).
        assert type(transport._client) is AsyncClient
        engine = FakeSyncEngine.instances[-1]
        assert engine.started
        assert engine.client is transport._client
        assert engine.sync_timeout_ms == MIN_SYNC_TIMEOUT_MS
        # The room-event callback is registered on the built client
        # (real nio stores ClientCallback entries, not raw tuples).
        def _cb_of(entry):
            if isinstance(entry, tuple):
                return entry[0]
            return getattr(entry, "callback", None) or getattr(entry, "func", None)

        assert any(
            _cb_of(entry) == transport._on_room_event
            for entry in transport._client.event_callbacks
        )
        closed: List[int] = []
        real_close = transport._client.close

        async def _recording_close():
            closed.append(1)
            await real_close()

        transport._client.close = _recording_close
        await transport.stop()
        assert engine.stopped
        assert closed == [1]

    _run(scenario)


def test_start_injected_client_keys_upload_once(tmp_path, monkeypatch):
    FakeSyncEngine.instances = []
    monkeypatch.setattr(transport_mod, "SyncEngine", FakeSyncEngine)

    async def scenario():
        client = FakeClient()
        client.should_upload_keys = True
        transport = NioTransport(
            _config(), store=FakeStore(), state_dir=tmp_path, client=client
        )
        await transport.start(lambda e: _noop())
        assert client.keys_upload_calls == 1
        assert FakeSyncEngine.instances[-1].started
        await transport.stop()
        assert client.closed

    _run(scenario)


def test_start_injected_client_no_keys_upload_when_not_needed(tmp_path, monkeypatch):
    FakeSyncEngine.instances = []
    monkeypatch.setattr(transport_mod, "SyncEngine", FakeSyncEngine)

    async def scenario():
        client = FakeClient()
        client.should_upload_keys = False
        transport = NioTransport(
            _config(), store=FakeStore(), state_dir=tmp_path, client=client
        )
        await transport.start(lambda e: _noop())
        assert client.keys_upload_calls == 0
        await transport.stop()

    _run(scenario)


def test_start_build_path_crypto_degrades_without_backend(tmp_path, monkeypatch):
    """No crypto backend → degrade to non-encrypted, log, do not raise."""
    FakeSyncEngine.instances = []
    monkeypatch.setattr(transport_mod, "SyncEngine", FakeSyncEngine)
    monkeypatch.setattr(transport_mod, "crypto_available", lambda: False)

    async def scenario():
        transport = NioTransport(
            _config(encryption=True),
            store=FakeStore(),
            state_dir=tmp_path,
        )
        await transport.start(lambda e: _noop())
        assert transport.encryption is False
        assert type(transport._client) is AsyncClient
        assert transport._client.config.encryption_enabled is False
        await transport.stop()

    _run(scenario)


def test_start_twice_raises(tmp_path, monkeypatch):
    FakeSyncEngine.instances = []
    monkeypatch.setattr(transport_mod, "SyncEngine", FakeSyncEngine)

    async def scenario():
        transport = NioTransport(
            _config(), store=FakeStore(), state_dir=tmp_path,
            client=FakeClient(),
        )
        await transport.start(lambda e: _noop())
        with pytest.raises(RuntimeError):
            await transport.start(lambda e: _noop())
        await transport.stop()

    _run(scenario)


def test_stop_idempotent(tmp_path, monkeypatch):
    FakeSyncEngine.instances = []
    monkeypatch.setattr(transport_mod, "SyncEngine", FakeSyncEngine)

    async def scenario():
        client = FakeClient()
        transport = NioTransport(
            _config(), store=FakeStore(), state_dir=tmp_path, client=client
        )
        await transport.start(lambda e: _noop())
        await transport.stop()
        await transport.stop()  # idempotent: no raise, still closed once
        assert client.closed

    _run(scenario)


async def _noop(event: InboundEvent) -> None:
    return None


# -- normalisation -----------------------------------------------------------


def test_normalize_text_event():
    inbound = normalize_room_event(ROOM, _text_event())
    assert inbound is not None
    assert inbound.kind == "text"
    assert inbound.event_id == "$t1"
    assert inbound.room_id == "!r:hs.example.com"
    assert inbound.sender == SENDER
    assert inbound.ts == 1700000000000
    assert inbound.text == "hello"
    assert inbound.media == []
    assert inbound.thread_root is None


def test_normalize_text_event_with_mention_and_thread():
    inbound = normalize_room_event(
        ROOM,
        _text_event(
            body="ping @worker:hs.example.com please",
            content={
                "m.relates_to": {
                    "rel_type": "m.thread",
                    "event_id": "$root1",
                    "is_falling_back": True,
                }
            },
        ),
    )
    assert inbound is not None
    assert inbound.kind == "text"
    assert inbound.mentions == ["@worker:hs.example.com"]
    assert inbound.thread_root == "$root1"


def test_normalize_notice_event_is_text():
    payload = {
        "type": "m.room.message",
        "room_id": "!r:hs.example.com",
        "sender": SENDER,
        "event_id": "$n1",
        "origin_server_ts": 1700000000005,
        "content": {"msgtype": "m.notice", "body": "a notice"},
    }
    inbound = normalize_room_event(ROOM, RoomMessageNotice.from_dict(payload))
    assert inbound is not None
    assert inbound.kind == "text"
    assert inbound.text == "a notice"


def test_normalize_media_event():
    inbound = normalize_room_event(ROOM, _media_event())
    assert inbound is not None
    assert inbound.kind == "media"
    assert inbound.text == "photo.jpg"
    assert inbound.media == [
        MediaRef(
            mxc_uri="mxc://hs.example.com/img1",
            mime_type="image/jpeg",
            file_name="photo.jpg",
        )
    ]


def test_normalize_edit_event():
    inbound = normalize_room_event(ROOM, _edit_event())
    assert inbound is not None
    assert inbound.kind == "edit"
    assert inbound.text == "corrected"
    assert inbound.event_id == "$e1"


def test_normalize_redaction_event():
    inbound = normalize_room_event(ROOM, _redaction_event())
    assert inbound is not None
    assert inbound.kind == "redaction"
    assert inbound.text == ""
    assert inbound.media == []


def test_normalize_undecryptable_megolm_not_delivered():
    assert normalize_room_event(ROOM, _megolm_event()) is None


# -- inbound delivery through the sync engine --------------------------------


def test_start_delivers_normalized_events(tmp_path):
    script = [
        (
            _sync_response("t1"),
            [
                (ROOM, _text_event()),
                (ROOM, _media_event()),
                (ROOM, _edit_event()),
                (ROOM, _redaction_event()),
                (ROOM, _megolm_event()),  # undecryptable: suppressed
            ],
        )
    ]

    async def scenario():
        received: List[InboundEvent] = []

        async def on_event(event: InboundEvent):
            received.append(event)

        client = FakeClient(script)
        store = FakeStore(token="t0")
        transport = NioTransport(
            _config(), store=store, state_dir=tmp_path, client=client
        )
        await transport.start(on_event)
        await _wait_until(lambda: len(received) == 4)
        assert [e.kind for e in received] == [
            "text",
            "media",
            "edit",
            "redaction",
        ]
        await transport.stop()

    _run(scenario)


def test_start_handler_exception_does_not_break_sync(tmp_path):
    script = [
        (_sync_response("t1"), [(ROOM, _text_event(event_id="$t1"))]),
        (_sync_response("t2"), [(ROOM, _text_event(event_id="$t2"))]),
    ]

    async def scenario():
        received: List[str] = []

        async def on_event(event: InboundEvent):
            received.append(event.event_id)
            if event.event_id == "$t1":
                raise RuntimeError("handler boom")

        client = FakeClient(script)
        transport = NioTransport(
            _config(), store=FakeStore(token="t0"), state_dir=tmp_path,
            client=client,
        )
        await transport.start(on_event)
        await _wait_until(lambda: len(received) == 2)
        assert received == ["$t1", "$t2"]
        await transport.stop()

    _run(scenario)


# -- outbound ----------------------------------------------------------------


def test_send_final_with_reply(tmp_path):
    async def scenario():
        client = FakeClient()
        transport = NioTransport(
            _config(), store=FakeStore(), state_dir=tmp_path, client=client
        )
        event_id = await transport.send(
            OutboundMessage(
                room_id="!r:hs.example.com",
                text="hello",
                reply_to="$orig",
                kind="final",
            )
        )
        assert event_id == "$sent"
        call = client.room_send_calls[0]
        assert call["message_type"] == "m.room.message"
        assert call["content"]["msgtype"] == "m.text"
        assert call["content"]["body"] == "hello"
        assert call["content"]["m.in_reply_to"] == {"event_id": "$orig"}
        assert call["tx_id"]

    _run(scenario)


def test_send_tool_and_notice_msgtypes(tmp_path):
    async def scenario():
        client = FakeClient()
        transport = NioTransport(
            _config(), store=FakeStore(), state_dir=tmp_path, client=client
        )
        await transport.send(
            OutboundMessage(
                room_id="!r:hs.example.com", text="tool out", kind="tool"
            )
        )
        await transport.send(
            OutboundMessage(
                room_id="!r:hs.example.com", text="a notice", kind="notice"
            )
        )
        assert client.room_send_calls[0]["content"]["msgtype"] == "m.notice"
        assert client.room_send_calls[1]["content"]["msgtype"] == "m.text"
        assert "m.in_reply_to" not in client.room_send_calls[0]["content"]

    _run(scenario)


def test_send_failure_raises(tmp_path):
    async def scenario():
        client = FakeClient()
        client.room_send_response = RoomSendError(
            status_code=500, message="boom"
        )
        transport = NioTransport(
            _config(), store=FakeStore(), state_dir=tmp_path, client=client
        )
        with pytest.raises(RuntimeError):
            await transport.send(
                OutboundMessage(room_id="!r:hs.example.com", text="hi")
            )

    _run(scenario)


def test_edit_sends_replace(tmp_path):
    async def scenario():
        client = FakeClient()
        transport = NioTransport(
            _config(), store=FakeStore(), state_dir=tmp_path, client=client
        )
        await transport.edit("!r:hs.example.com", "$orig", "corrected")
        call = client.room_send_calls[0]
        content = call["content"]
        assert content["m.relates_to"] == {
            "rel_type": "m.replace",
            "event_id": "$orig",
        }
        assert content["m.new_content"]["body"] == "corrected"
        assert content["body"].startswith("* ")

    _run(scenario)


def test_react_sends_annotation(tmp_path):
    async def scenario():
        client = FakeClient()
        transport = NioTransport(
            _config(), store=FakeStore(), state_dir=tmp_path, client=client
        )
        await transport.react("!r:hs.example.com", "$orig", "+1")
        call = client.room_send_calls[0]
        assert call["message_type"] == "m.reaction"
        assert call["content"]["m.annotation"] == {"key": "+1"}

    _run(scenario)


def test_typing_start_and_stop(tmp_path):
    async def scenario():
        client = FakeClient()
        transport = NioTransport(
            _config(), store=FakeStore(), state_dir=tmp_path, client=client
        )
        await transport.typing("!r:hs.example.com", "start")
        await transport.typing("!r:hs.example.com", "stop")
        assert client.typing_calls == [
            ("!r:hs.example.com", True, TYPING_TIMEOUT_MS),
            ("!r:hs.example.com", False, TYPING_TIMEOUT_MS),
        ]

    _run(scenario)


def test_typing_bad_state_raises(tmp_path):
    async def scenario():
        client = FakeClient()
        transport = NioTransport(
            _config(), store=FakeStore(), state_dir=tmp_path, client=client
        )
        with pytest.raises(ValueError):
            await transport.typing("!r:hs.example.com", "pause")
        assert client.typing_calls == []

    _run(scenario)


# -- media download -----------------------------------------------------------


def test_download_returns_bytes(tmp_path):
    async def scenario():
        client = FakeClient()
        client.download_response = MemoryDownloadResponse(
            body=b"media-bytes", content_type="image/jpeg", filename=None
        )
        transport = NioTransport(
            _config(), store=FakeStore(), state_dir=tmp_path, client=client
        )
        data = await transport.download(
            MediaRef(
                mxc_uri="mxc://hs.example.com/img1",
                mime_type="image/jpeg",
                file_name="photo.jpg",
            )
        )
        assert data == b"media-bytes"
        assert client.download_calls == ["mxc://hs.example.com/img1"]
        # Cached under the state dir's media_cache.
        cached = (tmp_path / "media_cache" / "photo.jpg").read_bytes()
        assert cached == b"media-bytes"

    _run(scenario)


def test_download_failure_raises(tmp_path):
    async def scenario():
        client = FakeClient()

        async def _boom(mxc=None, filename=None, **_kw):
            raise RuntimeError("fetch failed")

        client.download = _boom
        transport = NioTransport(
            _config(), store=FakeStore(), state_dir=tmp_path, client=client
        )
        with pytest.raises(RuntimeError):
            await transport.download(
                MediaRef(mxc_uri="mxc://hs.example.com/img1")
            )

    _run(scenario)
