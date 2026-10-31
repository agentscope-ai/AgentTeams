"""E2EE maintenance core tests (P9, PR1 M4c).

Pins the moved behaviour: the crypto store path layout, the olm
probe, and the post-sync maintenance pass — the call order and
arguments (``keys_claim`` receiving ``get_users_for_key_claiming()``),
the unconditional to-device send, the not-ready guards, and the
warning-swallowing error handling.

M5a adds the crypto-backend detection: matrix-nio 0.26+ ships E2EE
through ``vodozemac`` (no top-level ``olm`` module), so the
availability decision must follow the first importable backend in
``CRYPTO_BACKENDS`` order, not the olm module alone.
"""

from __future__ import annotations

import asyncio
from pathlib import Path
from typing import List, Optional, Tuple

import pytest

import core.e2ee as core_e2ee
from core.e2ee import (
    CRYPTO_BACKENDS,
    E2EEMaintainer,
    crypto_available,
    detect_crypto_backend,
    e2ee_store_path,
    probe_olm,
)


def _run(coro):
    """Run an async scenario to completion (no pytest-asyncio)."""
    return asyncio.run(coro)


class FakeClient:
    """matrix-nio ``AsyncClient`` stand-in recording the maintenance calls."""

    def __init__(
        self,
        upload: bool = False,
        query: bool = False,
        claim: bool = False,
        users: Tuple[str, ...] = ("u1", "u2"),
        olm: bool = True,
        upload_exc: Optional[Exception] = None,
    ) -> None:
        self.should_upload_keys = upload
        self.should_query_keys = query
        self.should_claim_keys = claim
        self._users = list(users)
        self.olm = object() if olm else None
        self._upload_exc = upload_exc
        self.calls: List[Tuple] = []

    def get_users_for_key_claiming(self):
        return list(self._users)

    async def keys_upload(self):
        self.calls.append(("upload",))
        if self._upload_exc is not None:
            raise self._upload_exc

    async def keys_query(self):
        self.calls.append(("query",))

    async def keys_claim(self, users):
        self.calls.append(("claim", tuple(users)))

    async def send_to_device_messages(self):
        self.calls.append(("to-device",))


def test_e2ee_store_path_layout():
    assert e2ee_store_path(Path("/work")) == Path(
        "/work/matrix_crypto_store"
    )


def test_probe_olm_absent():
    if probe_olm():
        pytest.skip("olm is installed in this environment")
    assert probe_olm() is False


def _stub_backends(monkeypatch, available):
    """Point the module-level import probe at a fixed set of names."""
    monkeypatch.setattr(
        core_e2ee,
        "_importable",
        lambda module: module in available,
    )


def test_detect_backend_vodozemac_only(monkeypatch):
    _stub_backends(monkeypatch, {"vodozemac"})
    assert detect_crypto_backend() == "vodozemac"
    assert crypto_available() is True


def test_detect_backend_olm_only(monkeypatch):
    _stub_backends(monkeypatch, {"olm"})
    assert detect_crypto_backend() == "olm"
    assert crypto_available() is True


def test_detect_backend_none(monkeypatch):
    _stub_backends(monkeypatch, set())
    assert detect_crypto_backend() is None
    assert crypto_available() is False


def test_detect_backend_prefers_vodozemac(monkeypatch):
    """Both importable (legacy + 0.26+ runtime): vodozemac wins."""
    assert CRYPTO_BACKENDS == ("vodozemac", "olm")
    _stub_backends(monkeypatch, set(CRYPTO_BACKENDS))
    assert detect_crypto_backend() == "vodozemac"


def test_probe_olm_tracks_olm_module_only(monkeypatch):
    """probe_olm stays an olm-only check (vodozemac-only -> False)."""
    _stub_backends(monkeypatch, {"vodozemac"})
    assert probe_olm() is False
    _stub_backends(monkeypatch, {"olm"})
    assert probe_olm() is True


def test_run_once_all_flags_order_and_args():
    client = FakeClient(upload=True, query=True, claim=True)

    _run(E2EEMaintainer(client).run_once())

    assert client.calls == [
        ("upload",),
        ("query",),
        ("claim", ("u1", "u2")),
        ("to-device",),
    ]


def test_run_once_all_flags_false_skips_key_operations():
    """With all ``should_*`` false the three key operations are skipped;
    the to-device send is unconditional in the legacy behaviour."""
    client = FakeClient()

    _run(E2EEMaintainer(client).run_once())

    assert client.calls == [("to-device",)]


def test_run_once_without_olm_zero_calls():
    client = FakeClient(upload=True, query=True, claim=True, olm=False)

    _run(E2EEMaintainer(client).run_once())

    assert client.calls == []


def test_run_once_none_client_zero_calls():
    # Must not raise.
    _run(E2EEMaintainer(None).run_once())


def test_run_once_error_swallowed_with_warning_and_stops(
    caplog,
):
    client = FakeClient(
        upload=True,
        query=True,
        claim=True,
        upload_exc=RuntimeError("boom"),
    )

    with caplog.at_level("WARNING", logger="agentteams.matrix.core.e2ee"):
        # Must not raise.
        _run(E2EEMaintainer(client).run_once())

    # The whole block is one try (legacy behaviour): nothing after the
    # failing keys_upload runs.
    assert client.calls == [("upload",)]
    assert any(
        "MatrixChannel: E2EE maintenance error: boom" in record.getMessage()
        for record in caplog.records
    )


# ---------------------------------------------------------------------------
# Native crypto init (matrix-nio 0.26 + vodozemac): the NioTransport's
# restore_login preparation, the fail-closed E2EEInitError, the persisted
# Olm account, and the offline encrypt/decrypt round-trip.  These run the
# REAL nio client (only the HTTP boundary and the sync engine are faked).
# ---------------------------------------------------------------------------

try:
    from nio import AsyncClient, MatrixRoom, KeysUploadResponse, RoomMessageText
    from nio.responses import RoomSendResponse, SyncResponse

    from core.interfaces import OutboundMessage
    import core.transport as transport_mod
    from core.transport import E2EEInitError, NioTransport, TransportConfig, build_client

    _NIO_IMPORT_ERROR = None
except ImportError as _exc:  # pragma: no cover - nio is a hard core dep
    AsyncClient = MatrixRoom = RoomMessageText = None
    KeysUploadResponse = RoomSendResponse = SyncResponse = None
    OutboundMessage = None
    transport_mod = None
    E2EEInitError = NioTransport = TransportConfig = build_client = None
    _NIO_IMPORT_ERROR = _exc

SELF_USER = "@agent:hs.example.com"
HS = "https://hs.example.com"
DEVICE = "DEV1"
TOKEN = "tok-1"
ROOM_ID = "!enc:hs.example.com"


@pytest.fixture()
def nio_available():
    """Skip the native-crypto tests only when matrix-nio is absent."""
    if _NIO_IMPORT_ERROR is not None:
        pytest.skip(f"matrix-nio not available: {_NIO_IMPORT_ERROR}")


class _FakeStore:
    """SessionStore stand-in (the sync engine is faked in these tests)."""

    def load_sync_token(self):
        return None

    def save_sync_token(self, token):
        pass

    def load_session_key(self, room_id, sender):
        return None

    def save_session_key(self, room_id, sender, key):
        pass


class _FakeSyncEngine:
    """Recording stand-in for ``core.sync.SyncEngine`` (no sync loop)."""

    instances: list = []

    def __init__(self, client, store, sync_timeout_ms, **kwargs):
        self.client = client
        self.started = False
        _FakeSyncEngine.instances.append(self)

    def start(self):
        self.started = True

    async def stop(self):
        self.stopped = True


def _encrypted_config(tmp_path, **overrides) -> "TransportConfig":
    kwargs = dict(
        homeserver=HS,
        user_id=SELF_USER,
        access_token=TOKEN,
        device_id=DEVICE,
        store_path=tmp_path / "e2ee",
        encryption=True,
    )
    kwargs.update(overrides)
    return TransportConfig(**kwargs)


def _initialized_client(tmp_path) -> "AsyncClient":
    """A real nio client with a materialised Olm account (offline)."""
    client = build_client(_encrypted_config(tmp_path))
    client.restore_login(SELF_USER, DEVICE, TOKEN)
    assert client.olm is not None
    return client


def _prime_encrypted_room(client, room_id: str = ROOM_ID) -> None:
    """Prime the room as a synced, encrypted room and create a shared
    outbound Megolm session for it (the session is auto-imported as an
    inbound one for our own device, so our own events decrypt)."""
    room = MatrixRoom(room_id, SELF_USER, encrypted=True)
    room.members_synced = True
    client.rooms[room_id] = room
    client.olm.create_outbound_group_session(room_id)
    client.olm.outbound_group_sessions[room_id].shared = True


def _capture_send(client, room_id: str = ROOM_ID) -> dict:
    """Monkeypatch the HTTP boundary: record the wire request, answer
    with a RoomSendResponse."""
    captured: dict = {}

    async def fake_send(
        response_class, method, path, data=None, response_data=None, **kwargs
    ):
        import json as _json

        captured["method"] = method
        captured["path"] = path
        captured["body"] = _json.loads(data)
        return RoomSendResponse(room_id=room_id, event_id="$sent-1")

    client._send = fake_send
    return captured


def test_nio_transport_start_initializes_olm_via_restore_login(
    tmp_path, monkeypatch, nio_available
):
    """encryption=True + store path: start() runs the native
    ``restore_login`` preparation and the client comes up with an Olm
    account; ``transport.encryption`` stays True and the keys upload
    runs once (fresh account)."""
    _FakeSyncEngine.instances = []
    monkeypatch.setattr(transport_mod, "SyncEngine", _FakeSyncEngine)

    async def scenario():
        client = build_client(_encrypted_config(tmp_path))
        uploaded = []

        async def fake_keys_upload():
            uploaded.append(1)
            return KeysUploadResponse(
                curve25519_count=5, signed_curve25519_count=1
            )

        client.keys_upload = fake_keys_upload
        transport = NioTransport(
            _encrypted_config(tmp_path),
            store=_FakeStore(),
            state_dir=tmp_path,
            encryption=True,
            client=client,
        )
        await transport.start(lambda e: None)
        assert transport.encryption is True
        assert client.olm is not None
        assert uploaded == [1]
        assert _FakeSyncEngine.instances[-1].started
        await transport.stop()

    _run(scenario())


def test_restore_login_without_olm_aborts_start(tmp_path, monkeypatch, nio_available):
    """A client whose ``restore_login`` leaves ``olm is None`` aborts the
    start with ``E2EEInitError``; the sync engine never starts."""
    _FakeSyncEngine.instances = []
    monkeypatch.setattr(transport_mod, "SyncEngine", _FakeSyncEngine)

    class _NoOlmClient:
        """Duck-typed client whose store prep does not materialise an
        Olm account (e.g. a store path the backend cannot open)."""

        def __init__(self):
            self.user_id = SELF_USER
            self.device_id = DEVICE
            self.access_token = TOKEN
            self.olm = None
            self.restore_login_calls = []

        def restore_login(self, user_id, device_id, access_token):
            self.restore_login_calls.append((user_id, device_id, access_token))
            self.olm = None

    async def scenario():
        client = _NoOlmClient()
        transport = NioTransport(
            _encrypted_config(tmp_path),
            store=_FakeStore(),
            state_dir=tmp_path,
            encryption=True,
            client=client,
        )
        with pytest.raises(E2EEInitError, match="olm is None"):
            await transport.start(lambda e: None)
        assert client.restore_login_calls == [(SELF_USER, DEVICE, TOKEN)]
        assert _FakeSyncEngine.instances == []

    _run(scenario())


def test_persisted_account_survives_client_rebuild(tmp_path, nio_available):
    """Initialize on a store path, capture the Olm identity keys, close,
    rebuild + ``restore_login`` on the same path: the same account is
    restored (no new account on restart)."""

    async def scenario():
        store_path = tmp_path / "e2ee"
        store_path.mkdir(parents=True, exist_ok=True)
        client1 = build_client(
            _encrypted_config(tmp_path, store_path=store_path)
        )
        client1.restore_login(SELF_USER, DEVICE, TOKEN)
        keys1 = dict(client1.olm.account.identity_keys)
        assert set(keys1) == {"curve25519", "ed25519"}
        await client1.close()

        client2 = build_client(
            _encrypted_config(tmp_path, store_path=store_path)
        )
        client2.restore_login(SELF_USER, DEVICE, TOKEN)
        keys2 = dict(client2.olm.account.identity_keys)
        await client2.close()
        return keys1, keys2

    keys1, keys2 = _run(scenario())
    assert keys2 == keys1


def test_encrypted_send_emits_m_room_encrypted(tmp_path, nio_available):
    """With an initialized client, the core send helper emits an
    ``m.room.encrypted`` event whose envelope carries the Megolm
    ciphertext (the plaintext never reaches the wire)."""

    async def scenario():
        client = _initialized_client(tmp_path)
        _prime_encrypted_room(client)
        captured = _capture_send(client)
        transport = NioTransport(
            _encrypted_config(tmp_path),
            store=_FakeStore(),
            state_dir=tmp_path,
            encryption=True,
            client=client,
        )
        event_id = await transport.send(
            OutboundMessage(room_id=ROOM_ID, text="secret send")
        )
        await client.close()
        return captured, event_id

    captured, event_id = _run(scenario())
    assert event_id == "$sent-1"
    assert captured["method"] == "PUT"
    # nio percent-encodes the room id in the wire path
    # (!enc:hs.example.com -> %21enc%3Ahs.example.com).
    from urllib.parse import quote

    assert (
        f"/rooms/{quote(ROOM_ID, safe='')}/send/m.room.encrypted/"
        in captured["path"]
    )
    body = captured["body"]
    for key in ("algorithm", "ciphertext", "sender_key", "session_id"):
        assert key in body
    assert body["algorithm"] == "m.megolm.v1.aes-sha2"
    assert "secret send" not in body["ciphertext"]


def test_encrypted_receive_decrypts_megolm_event(tmp_path, nio_available):
    """A Megolm event fed through a raw sync response is decrypted into
    the plaintext event (``decrypted=True``) and delivered to the
    registered room-event callback."""

    async def scenario():
        client = _initialized_client(tmp_path)
        _prime_encrypted_room(client)
        enc = client.olm.group_encrypt(
            ROOM_ID,
            {
                "content": {"msgtype": "m.text", "body": "secret receive"},
                "type": "m.room.message",
            },
        )
        raw = {
            "next_batch": "tok-1",
            "to_device": {"events": []},
            "rooms": {
                "join": {
                    ROOM_ID: {
                        "timeline": {
                            "events": [
                                {
                                    "type": "m.room.encrypted",
                                    "event_id": "$enc-1",
                                    "room_id": ROOM_ID,
                                    "sender": SELF_USER,
                                    "origin_server_ts": 1_700_000_000_000,
                                    "content": enc,
                                }
                            ],
                            "limited": False,
                            "prev_batch": "tok-0",
                        },
                        "state": {
                            "events": [
                                {
                                    "type": "m.room.encryption",
                                    "event_id": "$enc-state",
                                    "state_key": "",
                                    "room_id": ROOM_ID,
                                    "sender": SELF_USER,
                                    "origin_server_ts": 1_700_000_000_001,
                                    "content": {
                                        "algorithm": "m.megolm.v1.aes-sha2"
                                    },
                                }
                            ]
                        },
                    }
                },
            },
        }
        got = []

        async def on_event(room, event):
            got.append(event)

        client.add_event_callback(on_event, (RoomMessageText,))
        await client.receive_response(SyncResponse.from_dict(raw))
        await client.close()
        return got

    got = _run(scenario())
    assert len(got) == 1
    event = got[0]
    assert isinstance(event, RoomMessageText)
    assert event.body == "secret receive"
    assert event.decrypted is True
    assert event.verified is True


def test_non_crypto_client_keeps_keys_upload_contract(
    tmp_path, monkeypatch, nio_available
):
    """A duck-typed client without a native crypto surface (no
    ``restore_login``) keeps the pre-existing ``should_upload_keys``
    behaviour: no ``E2EEInitError``, keys upload runs when requested,
    the sync engine starts."""
    _FakeSyncEngine.instances = []
    monkeypatch.setattr(transport_mod, "SyncEngine", _FakeSyncEngine)

    class _PlainClient:
        """Duck-typed client without the native crypto surface."""

        def __init__(self, should_upload: bool):
            self.user_id = SELF_USER
            self.device_id = DEVICE
            self.access_token = TOKEN
            self.olm = None
            self.should_upload_keys = should_upload
            self.keys_upload_calls = 0
            self.event_callbacks: list = []
            self.sync_timeout_ms = 30000
            self.closed = False

        def add_event_callback(self, callback, filter=None):
            self.event_callbacks.append((callback, filter))

        async def keys_upload(self):
            self.keys_upload_calls += 1
            return KeysUploadResponse(
                curve25519_count=5, signed_curve25519_count=1
            )

        async def close(self):
            self.closed = True

    async def scenario():
        client = _PlainClient(should_upload=True)
        transport = NioTransport(
            _encrypted_config(tmp_path),
            store=_FakeStore(),
            state_dir=tmp_path,
            encryption=True,
            client=client,
        )
        # Must not raise E2EEInitError: no restore_login -> native init
        # does not apply, the legacy contract below is kept as-is.
        await transport.start(lambda e: None)
        assert client.keys_upload_calls == 1
        assert _FakeSyncEngine.instances[-1].started
        await transport.stop()
        assert client.closed is True

    _run(scenario())
