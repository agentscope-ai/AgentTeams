"""Behaviour tests for the Hermes platform adapter
(``adapters/hermes/adapter.py``) — **fake SDK, behaviour only**; SDK
compatibility (symbols, ``PlatformEntry`` kwargs, real registration) is
covered by ``test_adapter_hermes_sdk.py`` against a real Hermes source
tree.

The Hermes gateway package (``gateway.*``) is not installed in this
repo's test environment, so this module injects minimal fakes into
``sys.modules`` and loads the adapter from its file path.  ``matrix-nio``
is installed, so the response classes the adapter type-checks
(``JoinedMembersResponse``, ``RoomSendResponse``, ``UploadResponse``)
are the real ones.

Coverage:
* ``register(ctx)`` — platform registration kwargs
* ``env_enablement_fn`` — env → ``extra`` seeding (incl. the
  ``home_channel`` dict shape)
* ``connect()`` — env → ``TransportConfig`` mapping, client injection,
  E2EE degrade, fatal-error paths, disconnect
* inbound ``InboundEvent`` → ``handle_message`` dispatch (DM, mention
  gating, history prefix, ``NO_REPLY``, self-skip, edits, threads)
* outbound ``send`` / ``send_typing`` / media → transport + client calls
* ``_standalone_send`` — cron delivery envelope
* ``install.sh`` / ``uninstall.sh`` — staging against a scratch
  ``HERMES_HOME``
"""

import asyncio
import datetime
import importlib.util
import inspect
import json
import os
import subprocess
import sys
import types
from dataclasses import dataclass
from pathlib import Path
from types import SimpleNamespace

import pytest
from nio import (
    JoinedMembersResponse,
    KeysUploadResponse,
    RoomSendResponse,
    UploadResponse,
)

from core.interfaces import InboundEvent

BUNDLE_DIR = Path(__file__).resolve().parents[2] / "agentteams-matrix"
HERMES_ADAPTER_DIR = BUNDLE_DIR / "adapters" / "hermes"
ADAPTER_PATH = HERMES_ADAPTER_DIR / "adapter.py"

SELF = "@agent:example.org"
USER = "@alice:example.org"
THIRD = "@bob:example.org"
DM_ROOM = "!dm:example.org"
GROUP_ROOM = "!group:example.org"


# ── Fake gateway package (the Hermes SDK is not installed here) ────────


class _FakePlatform:
    """Stand-in for ``gateway.config.Platform``.

    The real enum accepts runtime-registered plugin names via
    ``_missing_`` (``platform_registry`` lookup) and caches pseudo
    members; this stand-in keeps the same value semantics.
    """

    def __init__(self, value):
        self.value = value
        self.name = str(value).upper().replace("-", "_")

    def __eq__(self, other):
        return isinstance(other, _FakePlatform) and other.value == self.value

    def __hash__(self):
        return hash(self.value)

    def __repr__(self):
        return f"Platform.{self.name}"


@dataclass
class _FakeSendResult:
    success: bool
    message_id: str | None = None
    error: str | None = None


class _FakeBasePlatformAdapter:
    """The slice of ``BasePlatformAdapter`` the adapter under test uses."""

    def __init__(self, config, platform):
        self.config = config
        self.platform = platform
        self._message_handler = None
        self._fatal_error_code = None
        self._fatal_error_message = None
        self._fatal_error_retryable = True
        self._connected = False
        self._locks_held = 0
        self._plugin_handlers_wired = 0

    @property
    def name(self) -> str:
        return "Fake Platform"

    @property
    def has_fatal_error(self) -> bool:
        return self._fatal_error_message is not None

    def _set_fatal_error(self, code, message, *, retryable):
        self._fatal_error_code = code
        self._fatal_error_message = message
        self._fatal_error_retryable = retryable

    def _acquire_platform_lock(self, scope, identity, desc) -> bool:
        self._locks_held += 1
        return True

    def _release_platform_lock(self):
        self._locks_held = max(0, self._locks_held - 1)

    def _mark_connected(self):
        self._connected = True
        self._fatal_error_code = self._fatal_error_message = None
        self._fatal_error_retryable = True

    def _mark_disconnected(self):
        self._connected = False

    def _wire_plugin_handlers(self, native=None):
        self._plugin_handlers_wired += 1

    def set_message_handler(self, handler):
        self._message_handler = handler

    def build_source(
        self,
        chat_id,
        chat_name=None,
        chat_type="dm",
        user_id=None,
        user_name=None,
        **kwargs,
    ):
        return SimpleNamespace(
            platform=self.platform,
            chat_id=str(chat_id),
            chat_name=chat_name,
            chat_type=chat_type,
            user_id=user_id,
            user_name=user_name,
        )

    async def handle_message(self, event):
        if self._message_handler is None:
            return
        result = self._message_handler(event)
        if inspect.isawaitable(result):
            await result


class _FakeMessageType:
    TEXT = "text"


@dataclass
class _FakeMessageEvent:
    text: str
    message_type: object
    source: object
    message_id: str = ""
    timestamp: object = None


def _fake_get_scoped_secret(name, default=None, *, external_fallback=False):
    value = os.environ.get(name)
    return value if value is not None else default


def _fake_seed_extra_from_env(spec, *, home_env=None, home_default=""):
    """Mirror the gateway's env → ``extra`` seeding semantics."""
    seed = {}
    for env, key, conv in spec:
        raw = str(_fake_get_scoped_secret(env, "") or "").strip()
        if not raw:
            continue
        try:
            seed[key] = conv(raw) if conv else raw
        except ValueError:
            pass
    if home_env:
        home = (
            str(_fake_get_scoped_secret(home_env, "") or "").strip()
            or home_default
        )
        if home:
            seed["home_channel"] = {
                "chat_id": home,
                "name": _fake_get_scoped_secret(f"{home_env}_NAME", "Home"),
            }
    return seed


def _fake_send_error(message):
    return {"error": str(message)}


_FAKE_MODULE_NAMES = (
    "gateway",
    "gateway.config",
    "gateway.platforms",
    "gateway.platforms.base",
    "gateway.platforms.event",
    "gateway.platforms._shared",
)


def _build_fake_modules():
    gateway = types.ModuleType("gateway")
    config = types.ModuleType("gateway.config")
    config.Platform = _FakePlatform
    platforms = types.ModuleType("gateway.platforms")
    base = types.ModuleType("gateway.platforms.base")
    base.BasePlatformAdapter = _FakeBasePlatformAdapter
    base.SendResult = _FakeSendResult
    event = types.ModuleType("gateway.platforms.event")
    event.MessageEvent = _FakeMessageEvent
    event.MessageType = _FakeMessageType
    shared = types.ModuleType("gateway.platforms._shared")
    shared.get_scoped_secret = _fake_get_scoped_secret
    shared.seed_extra_from_env = _fake_seed_extra_from_env
    shared.send_error = _fake_send_error
    gateway.config = config
    gateway.platforms = platforms
    platforms.base = base
    platforms.event = event
    platforms._shared = shared
    return {
        "gateway": gateway,
        "gateway.config": config,
        "gateway.platforms": platforms,
        "gateway.platforms.base": base,
        "gateway.platforms.event": event,
        "gateway.platforms._shared": shared,
    }


@pytest.fixture(scope="module")
def adapter_mod():
    """Load ``adapters/hermes/adapter.py`` against the fake gateway."""
    saved = {name: sys.modules.get(name) for name in _FAKE_MODULE_NAMES}
    sys.modules.update(_build_fake_modules())
    try:
        spec = importlib.util.spec_from_file_location(
            "agentteams_hermes_adapter_under_test", ADAPTER_PATH
        )
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        yield module
    finally:
        for name, mod in saved.items():
            if mod is None:
                sys.modules.pop(name, None)
            else:
                sys.modules[name] = mod


# ── env / fakes for the adapter under test ─────────────────────────────


@pytest.fixture()
def matrix_env(monkeypatch):
    """Required env surface set; all optional vars cleared."""
    monkeypatch.setenv("AGENTTEAMS_MATRIX_SERVER", "https://matrix.example.org/")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_USER_ID", SELF)
    monkeypatch.setenv("AGENTTEAMS_MATRIX_TOKEN", "s3cr3t-token")
    for var in (
        "AGENTTEAMS_MATRIX_DEVICE_ID",
        "AGENTTEAMS_MATRIX_STATE_DIR",
        "AGENTTEAMS_MATRIX_ENCRYPTION",
        "AGENTTEAMS_MATRIX_HISTORY_LIMIT",
        "AGENTTEAMS_MATRIX_SYNC_TIMEOUT_MS",
        "AGENTTEAMS_MATRIX_HOME_ROOM",
        "AGENTTEAMS_MATRIX_ALLOWED_USERS",
        "AGENTTEAMS_MATRIX_ALLOW_ALL_USERS",
    ):
        monkeypatch.delenv(var, raising=False)
    return monkeypatch


def _clear_matrix_env(monkeypatch):
    for var in (
        "AGENTTEAMS_MATRIX_SERVER",
        "AGENTTEAMS_MATRIX_USER_ID",
        "AGENTTEAMS_MATRIX_TOKEN",
        "AGENTTEAMS_MATRIX_DEVICE_ID",
        "AGENTTEAMS_MATRIX_STATE_DIR",
        "AGENTTEAMS_MATRIX_ENCRYPTION",
        "AGENTTEAMS_MATRIX_HISTORY_LIMIT",
        "AGENTTEAMS_MATRIX_SYNC_TIMEOUT_MS",
        "AGENTTEAMS_MATRIX_HOME_ROOM",
        "AGENTTEAMS_MATRIX_ALLOWED_USERS",
        "AGENTTEAMS_MATRIX_ALLOW_ALL_USERS",
    ):
        monkeypatch.delenv(var, raising=False)


class FakeRoom:
    """Duck-typed nio room: name/topic, display names, member keys."""

    def __init__(self, name="", topic="", members=(), display_names=None):
        self.name = name
        self.topic = topic
        self._members = list(members)
        self._display_names = dict(display_names or {})
        self.users = {
            mxid: SimpleNamespace(display_name=self._display_names.get(mxid, ""))
            for mxid in self._members
        }

    def user_name(self, user_id):
        return self._display_names.get(user_id, "")


class FakeClient:
    """Duck-typed nio client for the adapter's direct calls."""

    def __init__(self, member_map=None, rooms=None):
        self._member_map = dict(member_map or {})
        self.rooms = dict(rooms or {})
        self.read_marker_calls = []
        self.room_send_calls = []
        self.upload_calls = []
        self.upload_response = (
            UploadResponse(content_uri="mxc://example.org/abc"),
            None,
        )
        #: Encrypt the uploaded bytes for real when ``encrypt=True`` (the
        #: decryption descriptor is what the event's ``file`` object carries).
        self.upload_encryption_info = True
        self.uploaded_ciphertext = b""
        self.closed = False

    async def joined_members(self, room_id):
        members = [
            SimpleNamespace(user_id=mxid)
            for mxid in self._member_map.get(room_id, [])
        ]
        # Real nio shape: ``members`` is a list of member objects carrying
        # ``.user_id`` (the SDK type is ``list[RoomMember]``).
        return JoinedMembersResponse(room_id=room_id, members=members)

    async def room_read_markers(
        self, room_id, fully_read_event=None, read_event=None, **kwargs
    ):
        self.read_marker_calls.append(
            (room_id, fully_read_event, read_event)
        )

    async def room_send(
        self,
        room_id,
        message_type,
        content,
        tx_id=None,
        ignore_unverified_devices=False,
        **kwargs,
    ):
        self.room_send_calls.append(
            {
                "room_id": room_id,
                "message_type": message_type,
                "content": content,
            }
        )
        return RoomSendResponse(room_id=room_id, event_id="$room-send")

    async def upload(
        self,
        data,
        content_type=None,
        filename=None,
        filesize=None,
        encrypt=False,
        **kwargs,
    ):
        payload = data.read() if hasattr(data, "read") else bytes(data or b"")
        self.upload_calls.append(
            {
                "content_type": content_type,
                "filename": filename,
                "filesize": filesize,
                "encrypt": encrypt,
                "data": payload,
            }
        )
        if encrypt:
            if self.upload_encryption_info is None:
                return self.upload_response[0], None
            from nio.crypto import encrypt_attachment

            ciphertext, info = encrypt_attachment(payload)
            self.uploaded_ciphertext = ciphertext
            return self.upload_response[0], info
        return self.upload_response

    async def close(self):
        self.closed = True


class FakeTransport:
    """Duck-typed ``core.transport.NioTransport``."""

    def __init__(self):
        self.start_callbacks = []
        self.stopped = False
        self.sent = []
        self.typing_calls = []
        self.send_exc = None
        self.start_exc = None

    async def start(self, on_event):
        if self.start_exc is not None:
            raise self.start_exc
        self.start_callbacks.append(on_event)

    async def stop(self):
        self.stopped = True

    async def send(self, outbound):
        if self.send_exc is not None:
            raise self.send_exc
        self.sent.append(outbound)
        return f"$out-{len(self.sent)}"

    async def typing(self, room_id, state):
        self.typing_calls.append((room_id, state))


def _inbound(
    room_id=DM_ROOM,
    sender=USER,
    text="hello",
    *,
    event_id="$e1",
    ts=1_700_000_000_000,
    media=None,
    mentions=(),
    thread_root=None,
    kind="text",
):
    return InboundEvent(
        event_id=event_id,
        room_id=room_id,
        sender=sender,
        ts=ts,
        text=text,
        media=list(media or []),
        mentions=list(mentions),
        thread_root=thread_root,
        kind=kind,
    )


def _wire_adapter(adapter_mod, *, member_map=None, rooms=None, client=None):
    """Adapter with a faked client + transport and a capturing handler."""
    if client is None:
        client = FakeClient(member_map=member_map, rooms=rooms)
    transport = FakeTransport()
    adapter = adapter_mod.AgentTeamsMatrixAdapter(SimpleNamespace(extra={}))
    adapter._client = client
    adapter._transport = transport
    events = []
    adapter.set_message_handler(lambda event: events.append(event))
    return adapter, client, transport, events


def _group_fixtures():
    members = [SELF, USER, THIRD]
    room = FakeRoom(
        name="Team", members=members, display_names={USER: "Alice", SELF: "AgentBot"}
    )
    return members, room


# ── register(ctx) ──────────────────────────────────────────────────────


class _FakeCtx:
    def __init__(self):
        self.registered = []

    def register_platform(self, **kwargs):
        self.registered.append(kwargs)


def test_register_platform_kwargs(adapter_mod):
    ctx = _FakeCtx()
    adapter_mod.register(ctx)
    assert len(ctx.registered) == 1
    kw = ctx.registered[0]
    assert kw["name"] == "agentteams_matrix"
    assert kw["label"] == "AgentTeams Matrix"
    assert kw["adapter_factory"] is adapter_mod.AgentTeamsMatrixAdapter
    assert kw["required_env"] == [
        "AGENTTEAMS_MATRIX_SERVER",
        "AGENTTEAMS_MATRIX_USER_ID",
        "AGENTTEAMS_MATRIX_TOKEN",
    ]
    assert kw["check_fn"] is adapter_mod.check_requirements
    assert kw["validate_config"] is adapter_mod.validate_config
    assert kw["is_connected"] is adapter_mod.is_connected
    assert kw["env_enablement_fn"] is adapter_mod._env_enablement
    assert kw["cron_deliver_env_var"] == "AGENTTEAMS_MATRIX_HOME_ROOM"
    assert kw["standalone_sender_fn"] is adapter_mod._standalone_send
    assert kw["allowed_users_env"] == "AGENTTEAMS_MATRIX_ALLOWED_USERS"
    assert kw["allow_all_env"] == "AGENTTEAMS_MATRIX_ALLOW_ALL_USERS"
    assert kw["max_message_length"] == (64 * 1024 * 3) // 4 - 1024
    assert kw["pii_safe"] is True
    assert kw["allow_update_command"] is True
    assert "matrix-nio[e2e]" in kw["install_hint"]
    assert kw["emoji"]
    assert kw["platform_hint"]


# ── env enablement / validation ────────────────────────────────────────


def test_env_enablement_seeds_extra(adapter_mod, matrix_env, monkeypatch):
    monkeypatch.setenv("AGENTTEAMS_MATRIX_DEVICE_ID", "DEV1")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_ENCRYPTION", "true")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_HISTORY_LIMIT", "25")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_SYNC_TIMEOUT_MS", "5000")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_HOME_ROOM", "!home:example.org")
    seed = adapter_mod._env_enablement()
    assert seed["homeserver"] == "https://matrix.example.org/"
    assert seed["user_id"] == SELF
    assert seed["access_token"] == "s3cr3t-token"
    assert seed["device_id"] == "DEV1"
    assert seed["encryption"] is True
    assert seed["history_limit"] == 25
    assert seed["sync_timeout_ms"] == 5000
    assert "store_path" not in seed
    assert seed["home_channel"] == {"chat_id": "!home:example.org", "name": "Home"}


def test_env_enablement_none_when_not_configured(adapter_mod, monkeypatch):
    _clear_matrix_env(monkeypatch)
    monkeypatch.setenv("AGENTTEAMS_MATRIX_SERVER", "https://matrix.example.org")
    assert adapter_mod._env_enablement() is None


def test_check_requirements_and_validate_config(adapter_mod, matrix_env, monkeypatch):
    assert adapter_mod.check_requirements() is True

    class _Cfg:
        def __init__(self, extra):
            self.extra = extra

    assert (
        adapter_mod.validate_config(
            _Cfg({"homeserver": "https://x", "user_id": "@a:x", "access_token": "t"})
        )
        is True
    )
    _clear_matrix_env(monkeypatch)
    assert adapter_mod.validate_config(_Cfg({})) is False
    assert (
        adapter_mod.is_connected(
            _Cfg({"homeserver": "https://x", "user_id": "@a:x", "access_token": "t"})
        )
        is True
    )


# ── connect() ──────────────────────────────────────────────────────────


def _stub_connect(adapter_mod, monkeypatch, *, crypto=None):
    """Replace build_client/NioTransport with capturing fakes."""
    captured = {}
    client = FakeClient()
    transport = FakeTransport()

    def fake_build_client(config):
        captured["config"] = config
        return client

    def fake_transport(config, **kwargs):
        captured["config"] = config
        captured["kwargs"] = kwargs
        return transport

    monkeypatch.setattr(adapter_mod, "build_client", fake_build_client)
    monkeypatch.setattr(adapter_mod, "NioTransport", fake_transport)
    if crypto is not None:
        monkeypatch.setattr(adapter_mod, "crypto_available", lambda: crypto)
    return captured, client, transport


def test_connect_builds_transport_config(adapter_mod, matrix_env, monkeypatch, tmp_path):
    monkeypatch.setenv("AGENTTEAMS_MATRIX_STATE_DIR", str(tmp_path / "state"))
    monkeypatch.setenv("AGENTTEAMS_MATRIX_SYNC_TIMEOUT_MS", "8000")
    captured, client, transport = _stub_connect(adapter_mod, monkeypatch)

    adapter = adapter_mod.AgentTeamsMatrixAdapter(SimpleNamespace(extra={}))
    assert asyncio.run(adapter.connect()) is True

    tc = captured["config"]
    assert tc.homeserver == "https://matrix.example.org"  # trailing slash rstripped
    assert tc.user_id == SELF
    assert tc.access_token == "s3cr3t-token"
    assert tc.device_id == ""
    assert tc.encryption is False
    assert tc.store_path is None
    assert tc.sync_timeout_ms == 8000
    # client-injection path: the built client is passed to the transport.
    assert captured["kwargs"]["client"] is client
    assert captured["kwargs"]["state_dir"] == adapter.state_dir
    assert captured["kwargs"]["encryption"] is False
    assert adapter._client is client
    assert adapter._transport is transport
    assert adapter._connected is True
    assert adapter._locks_held == 1
    assert adapter._plugin_handlers_wired == 1
    assert transport.start_callbacks == [adapter._on_inbound]


def test_connect_e2ee_with_backend_sets_store_path(
    adapter_mod, matrix_env, monkeypatch, tmp_path
):
    from core.state import StateLayout

    monkeypatch.setenv("AGENTTEAMS_MATRIX_STATE_DIR", str(tmp_path / "state"))
    monkeypatch.setenv("AGENTTEAMS_MATRIX_ENCRYPTION", "true")
    captured, client, transport = _stub_connect(adapter_mod, monkeypatch, crypto=True)

    adapter = adapter_mod.AgentTeamsMatrixAdapter(SimpleNamespace(extra={}))
    assert asyncio.run(adapter.connect()) is True

    tc = captured["config"]
    assert tc.encryption is True
    assert tc.store_path is not None
    assert Path(tc.store_path) == StateLayout(adapter.state_dir).e2ee
    assert captured["kwargs"]["encryption"] is True


def test_connect_refuses_when_e2ee_backend_is_missing(
    adapter_mod, matrix_env, monkeypatch, tmp_path, caplog
):
    """#1469 round 13: requested E2EE with no backend must not connect.

    The old behaviour demoted the config to plaintext and connected anyway,
    so an encrypted room received m.room.message. The connect now fails
    with a non-retryable reason and never builds a transport.
    """
    monkeypatch.setenv("AGENTTEAMS_MATRIX_STATE_DIR", str(tmp_path / "state"))
    monkeypatch.setenv("AGENTTEAMS_MATRIX_ENCRYPTION", "true")
    captured, client, transport = _stub_connect(
        adapter_mod, monkeypatch, crypto=False
    )

    adapter = adapter_mod.AgentTeamsMatrixAdapter(SimpleNamespace(extra={}))
    assert asyncio.run(adapter.connect()) is False

    # No transport was built and no plaintext client was injected.
    assert captured == {}
    assert "no Matrix E2EE backend" in caplog.text
    assert "refusing to connect in plaintext" in caplog.text


def test_connect_config_missing_sets_fatal(adapter_mod, monkeypatch):
    _clear_matrix_env(monkeypatch)
    adapter = adapter_mod.AgentTeamsMatrixAdapter(SimpleNamespace(extra={}))
    assert asyncio.run(adapter.connect()) is False
    assert adapter.has_fatal_error
    assert adapter._fatal_error_code == "config_missing"
    assert adapter._fatal_error_retryable is False
    assert adapter._locks_held == 0


def test_connect_failure_is_retryable_fatal(adapter_mod, matrix_env, monkeypatch, tmp_path):
    monkeypatch.setenv("AGENTTEAMS_MATRIX_STATE_DIR", str(tmp_path / "state"))

    def boom(config):
        raise RuntimeError("homeserver unreachable")

    monkeypatch.setattr(adapter_mod, "build_client", boom)
    monkeypatch.setattr(adapter_mod, "NioTransport", FakeTransport)

    adapter = adapter_mod.AgentTeamsMatrixAdapter(SimpleNamespace(extra={}))
    assert asyncio.run(adapter.connect()) is False
    assert adapter._fatal_error_code == "connect_failed"
    assert adapter._fatal_error_retryable is True
    assert adapter._client is None
    assert adapter._connected is False


def test_disconnect_stops_transport_releases_lock(
    adapter_mod, matrix_env, monkeypatch, tmp_path
):
    monkeypatch.setenv("AGENTTEAMS_MATRIX_STATE_DIR", str(tmp_path / "state"))
    captured, client, transport = _stub_connect(adapter_mod, monkeypatch)

    adapter = adapter_mod.AgentTeamsMatrixAdapter(SimpleNamespace(extra={}))
    assert asyncio.run(adapter.connect()) is True
    asyncio.run(adapter.disconnect())

    assert transport.stopped is True
    assert client.closed is True
    assert adapter._locks_held == 0
    assert adapter._connected is False
    assert adapter._transport is None
    assert adapter._client is None


# ── terminal auth (token invalid) & E2EE init fatal ─────────────────────


def test_token_invalid_hook_marks_fatal_and_derived_state(
    adapter_mod, matrix_env, monkeypatch, tmp_path
):
    """Successful connect, then the sync engine's terminal-auth path
    (token cleared + ``on_token_invalid`` hook) must read as a
    non-retryable fatal error with a derived ``is_connected`` of False."""
    monkeypatch.setenv("AGENTTEAMS_MATRIX_STATE_DIR", str(tmp_path / "state"))
    captured, client, transport = _stub_connect(adapter_mod, monkeypatch)

    adapter = adapter_mod.AgentTeamsMatrixAdapter(SimpleNamespace(extra={}))
    assert asyncio.run(adapter.connect()) is True
    assert adapter.is_connected is True
    # The sync engine's terminal-auth hook is wired through the transport
    # (bound methods compare by function + instance, not identity).
    assert captured["kwargs"]["on_token_invalid"] == adapter._on_token_invalid

    # Simulate core/sync.py's terminal-auth branch: the engine clears the
    # client token, stops its loop, then invokes the hook.
    client.access_token = ""
    adapter._on_token_invalid()

    assert client.access_token == ""
    assert adapter.is_connected is False
    assert adapter.has_fatal_error
    assert adapter._fatal_error_code == "token_invalid"
    assert adapter._fatal_error_message == (
        "M_UNKNOWN_TOKEN: matrix re-login required"
    )
    assert adapter._fatal_error_retryable is False


def test_invalid_token_at_startup_fails_connect(
    adapter_mod, matrix_env, monkeypatch, tmp_path
):
    """A client whose whoami answers M_UNKNOWN_TOKEN must fail the
    connect (fatal, re-login) before the transport ever starts."""
    from nio import WhoamiError

    monkeypatch.setenv("AGENTTEAMS_MATRIX_STATE_DIR", str(tmp_path / "state"))
    captured, client, transport = _stub_connect(adapter_mod, monkeypatch)

    async def rejected_whoami():
        return WhoamiError(status_code="M_UNKNOWN_TOKEN", message="Unknown token")

    client.whoami = rejected_whoami

    adapter = adapter_mod.AgentTeamsMatrixAdapter(SimpleNamespace(extra={}))
    assert asyncio.run(adapter.connect()) is False
    assert adapter.is_connected is False
    assert adapter.has_fatal_error
    assert adapter._fatal_error_code == "token_invalid"
    assert adapter._fatal_error_retryable is False
    # The transport never started and the rejected client was closed.
    assert transport.start_callbacks == []
    assert client.closed is True
    assert adapter._client is None
    assert adapter._transport is None


def test_reconnect_clears_fatal_error_and_reads_connected(
    adapter_mod, matrix_env, monkeypatch, tmp_path
):
    """After a terminal-auth fatal, ``connect(is_reconnect=True)`` builds
    a fresh client/transport, clears the fatal error, and the derived
    ``is_connected`` reads True again."""
    monkeypatch.setenv("AGENTTEAMS_MATRIX_STATE_DIR", str(tmp_path / "state"))
    clients: list = []
    transports: list = []

    def fake_build_client(config):
        client = FakeClient()
        clients.append(client)
        return client

    def fake_transport(config, **kwargs):
        transport = FakeTransport()
        transport.kwargs = kwargs
        transports.append(transport)
        return transport

    monkeypatch.setattr(adapter_mod, "build_client", fake_build_client)
    monkeypatch.setattr(adapter_mod, "NioTransport", fake_transport)

    adapter = adapter_mod.AgentTeamsMatrixAdapter(SimpleNamespace(extra={}))
    assert asyncio.run(adapter.connect()) is True

    # Kill the first session the way the sync engine would.
    clients[0].access_token = ""
    transports[0].kwargs["on_token_invalid"]()
    assert adapter.has_fatal_error
    assert adapter.is_connected is False

    assert asyncio.run(adapter.connect(is_reconnect=True)) is True
    assert adapter.has_fatal_error is False
    assert adapter.is_connected is True
    assert len(clients) == 2
    assert len(transports) == 2
    # The new transport carries the same terminal-auth hook (bound
    # methods compare by function + instance, not identity).
    assert transports[1].kwargs["on_token_invalid"] == adapter._on_token_invalid
    assert transports[1].start_callbacks == [adapter._on_inbound]


def test_e2ee_init_error_maps_to_retryable_fatal(
    adapter_mod, matrix_env, monkeypatch, tmp_path
):
    """``E2EEInitError`` from the transport start must map to the
    retryable ``e2ee_init_failed`` fatal and read as no encryption."""
    from core.transport import E2EEInitError

    monkeypatch.setenv("AGENTTEAMS_MATRIX_STATE_DIR", str(tmp_path / "state"))
    monkeypatch.setenv("AGENTTEAMS_MATRIX_ENCRYPTION", "true")
    captured, client, transport = _stub_connect(
        adapter_mod, monkeypatch, crypto=True
    )
    transport.start_exc = E2EEInitError(
        "E2EE store was not initialized: olm is None after restore_login"
    )

    adapter = adapter_mod.AgentTeamsMatrixAdapter(SimpleNamespace(extra={}))
    assert asyncio.run(adapter.connect()) is False
    assert adapter.is_connected is False
    assert adapter._fatal_error_code == "e2ee_init_failed"
    assert adapter._fatal_error_retryable is True
    assert "olm is None" in adapter._fatal_error_message
    # The configured intent is not demoted (no silent plaintext state);
    # the failed connect and the closed client are the fail-closed signal.
    assert adapter.encryption is True
    assert client.closed is True
    assert adapter._client is None
    assert adapter._transport is None


# ── inbound ────────────────────────────────────────────────────────────


def test_inbound_dm_dispatched(adapter_mod, matrix_env):
    client = FakeClient(
        member_map={DM_ROOM: [SELF, USER]},
        rooms={
            DM_ROOM: FakeRoom(
                name="Alice DM",
                members=[SELF, USER],
                display_names={USER: "Alice", SELF: "AgentBot"},
            )
        },
    )
    adapter, client, transport, events = _wire_adapter(adapter_mod, client=client)
    asyncio.run(adapter._on_inbound(_inbound(room_id=DM_ROOM, text="hello")))

    assert len(events) == 1
    ev = events[0]
    assert ev.text == "hello"  # DM: no sender prefix, no history block
    assert ev.message_id == "$e1"
    assert ev.source.chat_type == "dm"
    assert ev.source.chat_id == DM_ROOM
    assert ev.source.chat_name == "Alice DM"
    assert ev.source.user_id == USER
    assert ev.source.user_name == "Alice"
    assert ev.timestamp == datetime.datetime(
        2023, 11, 14, 22, 13, 20, tzinfo=datetime.timezone.utc
    )
    assert client.read_marker_calls == [(DM_ROOM, "$e1", "$e1")]
    assert transport.typing_calls == [(DM_ROOM, "start")]


def test_inbound_own_message_skipped(adapter_mod, matrix_env):
    client = FakeClient(
        member_map={DM_ROOM: [SELF, USER]},
        rooms={DM_ROOM: FakeRoom(members=[SELF, USER])},
    )
    adapter, client, transport, events = _wire_adapter(adapter_mod, client=client)
    asyncio.run(adapter._on_inbound(_inbound(sender=SELF, text="echo")))
    assert events == []
    assert client.read_marker_calls == []
    assert transport.typing_calls == []


def test_inbound_edit_ignored(adapter_mod, matrix_env):
    client = FakeClient(
        member_map={DM_ROOM: [SELF, USER]},
        rooms={DM_ROOM: FakeRoom(members=[SELF, USER])},
    )
    adapter, client, transport, events = _wire_adapter(adapter_mod, client=client)
    asyncio.run(adapter._on_inbound(_inbound(text="edited", kind="edit")))
    asyncio.run(adapter._on_inbound(_inbound(text="", kind="redaction")))
    assert events == []
    assert client.read_marker_calls == []
    assert transport.typing_calls == []


def test_inbound_group_unmentioned_goes_to_history(adapter_mod, matrix_env):
    members, room = _group_fixtures()
    client = FakeClient(member_map={GROUP_ROOM: members}, rooms={GROUP_ROOM: room})
    adapter, client, transport, events = _wire_adapter(adapter_mod, client=client)
    asyncio.run(
        adapter._on_inbound(_inbound(room_id=GROUP_ROOM, text="chit-chat", event_id="$g1"))
    )
    assert events == []
    assert adapter._room_history.record_count(GROUP_ROOM) == 1
    assert "chit-chat" in adapter._room_history.build_history_prefix(GROUP_ROOM)


def test_inbound_group_mentioned_dispatched_with_history_prefix(
    adapter_mod, matrix_env
):
    members, room = _group_fixtures()
    client = FakeClient(member_map={GROUP_ROOM: members}, rooms={GROUP_ROOM: room})
    adapter, client, transport, events = _wire_adapter(adapter_mod, client=client)

    # 1) Unmentioned group chatter is buffered.
    asyncio.run(
        adapter._on_inbound(_inbound(room_id=GROUP_ROOM, text="chit-chat", event_id="$g1"))
    )
    assert events == []

    # 2) The mentioning message is dispatched with the sender prefix and
    #    the buffered history block merged in by core.
    asyncio.run(
        adapter._on_inbound(
            _inbound(
                room_id=GROUP_ROOM,
                event_id="$g2",
                text=f"{SELF} do the thing",
                mentions=[SELF],
            )
        )
    )
    assert len(events) == 1
    ev = events[0]
    assert ev.text == (
        "[Chat messages since your last reply - for context]\n"
        "Alice: chit-chat [id:$g1]\n"
        "\n"
        "[Current message - respond to this]\n"
        "Alice: do the thing"
    )
    assert ev.source.chat_type == "group"
    assert ev.source.user_name == "Alice"
    # History is consumed after the dispatch.
    assert adapter._room_history.build_history_prefix(GROUP_ROOM) == ""


def test_inbound_dm_no_reply_dropped(adapter_mod, matrix_env):
    client = FakeClient(
        member_map={DM_ROOM: [SELF, USER]},
        rooms={DM_ROOM: FakeRoom(members=[SELF, USER])},
    )
    adapter, client, transport, events = _wire_adapter(adapter_mod, client=client)
    asyncio.run(adapter._on_inbound(_inbound(room_id=DM_ROOM, text="NO_REPLY")))
    assert events == []
    assert client.read_marker_calls == [(DM_ROOM, "$e1", "$e1")]
    assert transport.typing_calls == [(DM_ROOM, "start"), (DM_ROOM, "stop")]


def test_inbound_group_thread_unmentioned_silent(adapter_mod, matrix_env):
    members, room = _group_fixtures()
    client = FakeClient(member_map={GROUP_ROOM: members}, rooms={GROUP_ROOM: room})
    adapter, client, transport, events = _wire_adapter(adapter_mod, client=client)
    asyncio.run(
        adapter._on_inbound(
            _inbound(
                room_id=GROUP_ROOM, text="thread chit-chat", thread_root="$root1"
            )
        )
    )
    assert events == []
    # Thread events are not buffered (they carry their own context).
    assert adapter._room_history.record_count(GROUP_ROOM) == 0
    assert client.read_marker_calls == []


# ── outbound ───────────────────────────────────────────────────────────


def test_send_dispatches_outbound_message(adapter_mod, matrix_env):
    adapter, client, transport, events = _wire_adapter(adapter_mod)
    result = asyncio.run(
        adapter.send(GROUP_ROOM, "hello there", reply_to="$parent")
    )
    assert result.success is True
    assert result.message_id == "$out-1"
    assert len(transport.sent) == 1
    out = transport.sent[0]
    assert out.room_id == GROUP_ROOM
    assert out.text == "hello there"
    assert out.reply_to == "$parent"


def test_send_not_connected_fails(adapter_mod, matrix_env):
    adapter, client, transport, events = _wire_adapter(adapter_mod)
    adapter._transport = None
    result = asyncio.run(adapter.send(GROUP_ROOM, "x"))
    assert result.success is False
    assert result.error == "Not connected"


def test_send_typing(adapter_mod, matrix_env):
    adapter, client, transport, events = _wire_adapter(adapter_mod)
    asyncio.run(adapter.send_typing(GROUP_ROOM))
    assert transport.typing_calls == [(GROUP_ROOM, "start")]


def test_send_document_uploads_and_room_sends(adapter_mod, matrix_env, tmp_path):
    client = FakeClient()
    adapter, client, transport, events = _wire_adapter(adapter_mod, client=client)
    doc = tmp_path / "report.pdf"
    doc.write_bytes(b"%PDF-1.4 fake")

    result = asyncio.run(adapter.send_document(GROUP_ROOM, str(doc), caption="report"))
    assert result.success is True
    assert result.message_id == "$room-send"
    assert client.upload_calls == [
        {
            "content_type": "application/pdf",
            "filename": "report.pdf",
            "filesize": len(doc.read_bytes()),
            "encrypt": False,
            "data": doc.read_bytes(),
        }
    ]
    call = client.room_send_calls[0]
    assert call["room_id"] == GROUP_ROOM
    assert call["message_type"] == "m.room.message"
    assert call["content"]["msgtype"] == "m.file"
    assert call["content"]["body"] == "report"
    assert call["content"]["url"] == "mxc://example.org/abc"
    assert call["content"]["info"]["size"] == len(doc.read_bytes())
    assert call["content"]["info"]["mimetype"] == "application/pdf"


def test_get_chat_info_dm_and_group(adapter_mod, matrix_env):
    client = FakeClient(
        member_map={
            DM_ROOM: [SELF, USER],
            GROUP_ROOM: [SELF, USER, THIRD],
        },
        rooms={
            DM_ROOM: FakeRoom(
                name="Alice DM", members=[SELF, USER], display_names={USER: "Alice"}
            ),
            GROUP_ROOM: FakeRoom(name="Team", members=[SELF, USER, THIRD]),
        },
    )
    adapter, client, transport, events = _wire_adapter(adapter_mod, client=client)
    # Prime the DM cache through the detection path.
    asyncio.run(adapter._on_inbound(_inbound(room_id=DM_ROOM, text="hi")))
    info = asyncio.run(adapter.get_chat_info(DM_ROOM))
    assert info == {"name": "Alice DM", "type": "dm", "chat_id": DM_ROOM}
    group_info = asyncio.run(adapter.get_chat_info(GROUP_ROOM))
    assert group_info["type"] == "group"
    assert group_info["name"] == "Team"


# ── standalone (cron) send ─────────────────────────────────────────────


def test_standalone_send_delivers(adapter_mod, matrix_env, monkeypatch, tmp_path):
    monkeypatch.setenv("AGENTTEAMS_MATRIX_STATE_DIR", str(tmp_path / "sa-state"))
    sent = {}

    class _SAClient(FakeClient):
        async def room_send(
            self,
            room_id,
            message_type,
            content,
            tx_id=None,
            ignore_unverified_devices=False,
            **kwargs,
        ):
            sent["room_id"] = room_id
            sent["content"] = content
            return RoomSendResponse(room_id=room_id, event_id="$sa1")

    monkeypatch.setattr(adapter_mod, "build_client", lambda config: _SAClient())
    pconfig = SimpleNamespace(extra={})
    result = asyncio.run(
        adapter_mod._standalone_send(pconfig, "!cron:example.org", "ping")
    )
    assert result == {"success": True, "message_id": "$sa1"}
    assert sent["room_id"] == "!cron:example.org"
    assert sent["content"] == {"msgtype": "m.text", "body": "ping"}

    # Control characters in the target are rejected.
    bad = asyncio.run(
        adapter_mod._standalone_send(pconfig, "!bad\nroom:example.org", "ping")
    )
    assert "error" in bad


def test_standalone_send_requires_config(adapter_mod, monkeypatch, tmp_path):
    _clear_matrix_env(monkeypatch)
    result = asyncio.run(
        adapter_mod._standalone_send(SimpleNamespace(extra={}), "!r:x", "hi")
    )
    assert "error" in result


# ── install.sh / uninstall.sh staging ──────────────────────────────────


def test_install_uninstall_staging(tmp_path):
    hermes_home = tmp_path / "hermes"
    env = {**os.environ, "HERMES_HOME": str(hermes_home)}
    target = hermes_home / "plugins" / "agentteams-matrix"
    marker = hermes_home / ".agentteams" / "agentteams-matrix" / "installed.json"
    install = HERMES_ADAPTER_DIR / "install.sh"
    uninstall = HERMES_ADAPTER_DIR / "uninstall.sh"

    r1 = subprocess.run(
        ["bash", str(install)], capture_output=True, text=True, env=env
    )
    assert r1.returncode == 0, r1.stderr
    # Payload staged; installer artifacts excluded.
    assert (target / "adapter.py").is_file()
    assert (target / "__init__.py").is_file()
    assert (target / "plugin.yaml").is_file()
    # The runtime-neutral core ships next to the installed plugin.
    assert (target / "core" / "__init__.py").is_file()
    assert not (target / "install.sh").exists()
    assert not (target / "uninstall.sh").exists()
    assert not (target / "README.md").exists()
    # Marker describes the install.
    assert marker.is_file()
    data = json.loads(marker.read_text(encoding="utf-8"))
    assert data["schemaVersion"] == 1
    assert data["bundle"] == "agentteams-matrix"
    assert data["runtime"] == "hermes"
    assert data["pluginId"] == "agentteams-matrix"
    assert data["files"]["adapter.py"]

    # Second run with unchanged payload is a no-op.
    r2 = subprocess.run(
        ["bash", str(install)], capture_output=True, text=True, env=env
    )
    assert r2.returncode == 0, r2.stderr
    assert "up to date" in r2.stdout

    # Uninstall removes exactly what the marker owns.
    ru = subprocess.run(
        ["bash", str(uninstall)], capture_output=True, text=True, env=env
    )
    assert ru.returncode == 0, ru.stderr
    assert not target.exists()
    assert not marker.exists()

    # Without a marker uninstall is a no-op.
    r3 = subprocess.run(
        ["bash", str(uninstall)], capture_output=True, text=True, env=env
    )
    assert r3.returncode == 0


def _encrypted_group_room(client):
    room = FakeRoom(name="Team", members=[SELF, USER])
    room.encrypted = True
    client.rooms[GROUP_ROOM] = room
    return room


def test_send_document_in_encrypted_room_uses_file_descriptor(
    adapter_mod, matrix_env, tmp_path
):
    """P1469: an encrypted room's attachment is uploaded as ciphertext and
    the event carries the file descriptor (url + key/iv/hashes/v), never a
    bare url — the receiving client decrypts the event and requires it."""
    client = FakeClient()
    adapter, client, transport, events = _wire_adapter(adapter_mod, client=client)
    adapter.encryption = True
    _encrypted_group_room(client)
    doc = tmp_path / "report.pdf"
    plaintext = b"%PDF-1.4 encrypted attachment"
    doc.write_bytes(plaintext)

    result = asyncio.run(adapter.send_document(GROUP_ROOM, str(doc), caption="report"))

    assert result.success is True
    assert client.upload_calls[0]["encrypt"] is True
    content = client.room_send_calls[0]["content"]
    assert "url" not in content
    file_info = content["file"]
    for key in ("url", "key", "iv", "hashes", "v"):
        assert key in file_info, f"missing {key} in the file descriptor"
    assert file_info["url"] == "mxc://example.org/abc"
    assert file_info["mimetype"] == "application/pdf"
    assert file_info["size"] == len(plaintext)
    # The "media repository" stored ciphertext, and a receiving client can
    # decrypt it back to the plaintext with exactly the descriptor fields.
    assert client.uploaded_ciphertext != plaintext
    from nio.crypto import decrypt_attachment

    decrypted = decrypt_attachment(
        client.uploaded_ciphertext,
        file_info["key"]["k"],
        file_info["hashes"]["sha256"],
        file_info["iv"],
    )
    assert decrypted == plaintext


def test_send_document_in_plaintext_room_keeps_url(adapter_mod, matrix_env, tmp_path):
    """P1469: the ordinary plaintext-room path is unchanged — no encryption
    is requested and the event keeps the url media content."""
    client = FakeClient()
    adapter, client, transport, events = _wire_adapter(adapter_mod, client=client)
    adapter.encryption = True  # E2EE is configured, but the room is not encrypted
    doc = tmp_path / "report.pdf"
    doc.write_bytes(b"%PDF-1.4 fake")

    result = asyncio.run(adapter.send_document(GROUP_ROOM, str(doc), caption="report"))

    assert result.success is True
    assert client.upload_calls[0]["encrypt"] is False
    content = client.room_send_calls[0]["content"]
    assert content["url"] == "mxc://example.org/abc"
    assert "file" not in content


def test_send_document_in_encrypted_room_fails_closed_without_metadata(
    adapter_mod, matrix_env, tmp_path
):
    """P1469: when the encrypted upload returns no decryption metadata the
    adapter refuses to send a plaintext attachment into the encrypted room."""
    client = FakeClient()
    client.upload_encryption_info = None
    adapter, client, transport, events = _wire_adapter(adapter_mod, client=client)
    adapter.encryption = True
    _encrypted_group_room(client)
    doc = tmp_path / "report.pdf"
    doc.write_bytes(b"%PDF-1.4 fake")

    result = asyncio.run(adapter.send_document(GROUP_ROOM, str(doc), caption="report"))

    assert result.success is False
    assert "decryption metadata" in (result.error or "")
    assert client.room_send_calls == []


# ── standalone (cron) E2EE: round 12 ───────────────────────────────────
#
# The maintainer's round-12 repro: with encryption enabled and a real
# encrypted room, the installed plugin's `_standalone_send` returned
# success while the server stored a plaintext `m.room.message` — the
# one-shot client never initialized its crypto store/room state, so nio
# did not know the room was encrypted. These tests pin the init sequence
# and the fail-closed branches (a live-homeserver e2e covers the wire).


class _SyncStub:
    def __init__(self, **kwargs):
        self.kwargs = kwargs


class _E2EEStubClient:
    """Duck-typed nio client with the crypto surface the init path uses."""

    def __init__(self, *, room_id, encrypted_room=True, olm_missing=False,
                 room_known=True):
        self.room_id = room_id
        # A real AsyncClient carries the identity from its build config.
        self.user_id = "@bot:example.org"
        self.device_id = "DEVICE123"
        self.access_token = "token"
        self.encrypted_room = encrypted_room
        self.olm_missing = olm_missing
        self.room_known = room_known
        self.calls = []
        self.rooms = {}
        self.olm = None
        self.should_upload_keys = True
        self.should_query_keys = False
        self.should_claim_keys = False
        self.sent = []
        self.closed = False

    def restore_login(self, user_id, device_id, access_token):
        self.calls.append(("restore_login", user_id, device_id, access_token))
        self.user_id = user_id
        self.device_id = device_id
        if not self.olm_missing:
            self.olm = object()

    async def keys_upload(self):
        self.calls.append(("keys_upload",))
        return KeysUploadResponse(curve25519_count=1, signed_curve25519_count=1)

    async def sync(self, timeout=0):
        self.calls.append(("sync", timeout))
        if self.room_known:
            self.rooms[self.room_id] = SimpleNamespace(
                room_id=self.room_id, encrypted=self.encrypted_room
            )
        return _SyncStub(next_batch="s1")

    async def room_send(self, room_id, message_type, content, tx_id=None,
                        ignore_unverified_devices=False, **kwargs):
        room = self.rooms.get(room_id)
        self.sent.append(
            {
                "room_id": room_id,
                "content": content,
                "encrypted": bool(getattr(room, "encrypted", False)),
                "known": room is not None,
            }
        )
        return RoomSendResponse(room_id=room_id, event_id="$e2ee-sent")

    async def close(self):
        self.closed = True


def _e2ee_env(monkeypatch, tmp_path):
    monkeypatch.setenv("AGENTTEAMS_MATRIX_ENCRYPTION", "1")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_DEVICE_ID", "DEVICE123")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_STATE_DIR", str(tmp_path / "e2ee-state"))


def test_standalone_send_initializes_e2ee_before_an_encrypted_send(
    adapter_mod, matrix_env, monkeypatch, tmp_path
):
    """The one-shot sender must establish the same state the long-running
    transport has: crypto store, uploaded keys, synced room state. Only
    then does nio know the room is encrypted."""
    _e2ee_env(monkeypatch, tmp_path)
    room = "!e2ee:example.org"
    client = _E2EEStubClient(room_id=room, encrypted_room=True)
    built = {}

    def fake_build(config):
        built["config"] = config
        return client

    maintainer_runs = []

    class _Maintainer:
        def __init__(self, c):
            self.client = c

        async def run_once(self):
            maintainer_runs.append(True)

    monkeypatch.setattr(adapter_mod, "build_client", fake_build)
    monkeypatch.setattr(adapter_mod, "crypto_available", lambda: True)
    monkeypatch.setattr(adapter_mod, "E2EEMaintainer", _Maintainer)
    monkeypatch.setattr(adapter_mod, "SyncResponse", _SyncStub)

    result = asyncio.run(
        adapter_mod._standalone_send(SimpleNamespace(extra={}), room, "ping")
    )

    assert result == {"success": True, "message_id": "$e2ee-sent"}
    # The E2EE store was requested on the client.
    assert built["config"].encryption is True
    assert built["config"].store_path is not None
    # The init sequence ran before the send, in order.
    ops = [c[0] for c in client.calls]
    assert ops[:3] == ["restore_login", "keys_upload", "sync"]
    assert maintainer_runs == [True]
    # The send happened with the room known-encrypted.
    assert client.sent == [
        {
            "room_id": room,
            "content": {"msgtype": "m.text", "body": "ping"},
            "encrypted": True,
            "known": True,
        }
    ]
    assert client.closed is True


def test_standalone_send_fails_closed_without_a_crypto_backend(
    adapter_mod, matrix_env, monkeypatch, tmp_path
):
    """Encryption enabled but no crypto backend: refuse — never downgrade a
    cron message to plaintext."""
    _e2ee_env(monkeypatch, tmp_path)
    monkeypatch.setattr(adapter_mod, "crypto_available", lambda: False)
    monkeypatch.setattr(
        adapter_mod,
        "build_client",
        lambda config: (_ for _ in ()).throw(AssertionError("must not build")),
    )

    result = asyncio.run(
        adapter_mod._standalone_send(SimpleNamespace(extra={}), "!r:x", "ping")
    )

    assert "error" in result
    assert "crypto backend" in result["error"]


def test_standalone_send_fails_closed_when_the_room_state_is_unknown(
    adapter_mod, matrix_env, monkeypatch, tmp_path
):
    """An unreadable/unjoined room means the encryption status is unknown:
    refuse instead of sending a message that may be plaintext."""
    _e2ee_env(monkeypatch, tmp_path)
    room = "!unknown:example.org"
    client = _E2EEStubClient(room_id=room, room_known=False)
    monkeypatch.setattr(adapter_mod, "build_client", lambda config: client)
    monkeypatch.setattr(adapter_mod, "crypto_available", lambda: True)
    monkeypatch.setattr(adapter_mod, "SyncResponse", _SyncStub)

    result = asyncio.run(
        adapter_mod._standalone_send(SimpleNamespace(extra={}), room, "ping")
    )

    assert "error" in result
    assert "room state" in result["error"]
    assert client.sent == []


def test_standalone_send_fails_closed_when_olm_is_not_initialized(
    adapter_mod, matrix_env, monkeypatch, tmp_path
):
    """restore_login without an Olm account (no store materialised) must
    abort the send, not fall through to plaintext."""
    _e2ee_env(monkeypatch, tmp_path)
    room = "!e2ee:example.org"
    client = _E2EEStubClient(room_id=room, olm_missing=True)
    monkeypatch.setattr(adapter_mod, "build_client", lambda config: client)
    monkeypatch.setattr(adapter_mod, "crypto_available", lambda: True)
    monkeypatch.setattr(adapter_mod, "SyncResponse", _SyncStub)

    result = asyncio.run(
        adapter_mod._standalone_send(SimpleNamespace(extra={}), room, "ping")
    )

    assert "error" in result
    assert "E2EE" in result["error"]
    assert client.sent == []


def test_standalone_send_plaintext_control_skips_crypto(
    adapter_mod, matrix_env, monkeypatch, tmp_path
):
    """Control: with encryption off the sender stays the plain one-shot
    client (no crypto init, no sync)."""
    monkeypatch.setenv("AGENTTEAMS_MATRIX_STATE_DIR", str(tmp_path / "plain-state"))
    sent = {}

    class _Plain(FakeClient):
        async def room_send(self, room_id, message_type, content, tx_id=None,
                            ignore_unverified_devices=False, **kwargs):
            sent["content"] = content
            return RoomSendResponse(room_id=room_id, event_id="$plain1")

    monkeypatch.setattr(adapter_mod, "build_client", lambda config: _Plain())

    result = asyncio.run(
        adapter_mod._standalone_send(SimpleNamespace(extra={}), "!r:x", "ping")
    )

    assert result == {"success": True, "message_id": "$plain1"}
    assert sent["content"] == {"msgtype": "m.text", "body": "ping"}
