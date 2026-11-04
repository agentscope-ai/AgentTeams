"""Drop-ledger tests (core.outcomes) and per-adapter drop-point coverage.

Covers the silent-drop trace contract:

* ``record_drop`` / ``read_drops`` round-trip on
  ``<workspace>/shared/matrix/drops.jsonl`` (append-only, detail cap,
  ``since``/``limit`` filters, malformed-line tolerance);
* write failures are warn-only and never raise;
* credential redaction — the ledger never retains an ``access_token``
  (``redact_detail`` / ``describe_exception`` unit cases, the
  end-to-end maintainer repro on a real query-token client against a
  redirecting local server, and the red control proving the
  assertions catch the leak);
* the frozen ``DROP_REASONS`` vocabulary (including the shared subset
  that must stay verbatim identical to the TeamHarness side's
  ``NOTIFICATION_GAP_REASONS``);
* the adapter drop points — QwenPaw channel and Hermes adapter driven
  with the same logical inputs produce the same reason codes
  (parity); the OpenClaw runtime is TypeScript and tracked as a TODO
  in its README.
"""

from __future__ import annotations

import asyncio
import importlib.util
import json
import logging
import sys
import threading
import time
from datetime import datetime, timedelta
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from types import SimpleNamespace

import aiohttp
import httpx
import pytest
from nio.responses import RoomSendError

REPO_ROOT = Path(__file__).resolve().parents[3]
BUNDLE_DIR = Path(__file__).resolve().parents[2] / "agentteams-matrix"
for _p in (
    str(REPO_ROOT),
    str(BUNDLE_DIR),
    str(BUNDLE_DIR / "adapters" / "qwenpaw"),
):
    if _p not in sys.path:
        sys.path.insert(0, _p)

from core import outcomes as _outcomes  # noqa: E402
from core.history import RoomHistory  # noqa: E402
from core.outcomes import (  # noqa: E402
    DETAIL_LIMIT,
    DROP_REASONS,
    DROPS_RELATIVE_PATH,
    describe_exception,
    drops_path,
    read_drops,
    record_drop,
    redact_detail,
)
from agentteams_matrix.channel import AgentTeamsMatrixChannel  # noqa: E402
from test_adapter_hermes import (  # noqa: E402
    _FAKE_MODULE_NAMES,
    _build_fake_modules,
    _group_fixtures,
    _inbound,
    _wire_adapter,
    DM_ROOM,
    FakeClient,
    GROUP_ROOM,
    SELF,
    USER,
)

HERMES_ADAPTER_PATH = BUNDLE_DIR / "adapters" / "hermes" / "adapter.py"

ALL_MATRIX_ENV_VARS = (
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
)


# ── QwenPaw channel driver (no client, no sync loop) ──────────────────


def _qp_event(
    *,
    body: str,
    event_id: str = "$e1",
    sender: str = USER,
    thread: bool = False,
    mentions=(),
    room_id: str = GROUP_ROOM,
):
    """Duck-typed nio ``RoomMessageText`` (source + attribute surface)."""
    content: dict = {"msgtype": "m.text", "body": body}
    if mentions:
        content["m.mentions"] = {"user_ids": list(mentions)}
    if thread:
        content["m.relates_to"] = {
            "rel_type": "m.thread",
            "event_id": "$root-1",
            "is_falling_back": True,
        }
    source = {
        "event_id": event_id,
        "sender": sender,
        "room_id": room_id,
        "type": "m.room.message",
        "content": content,
    }
    return SimpleNamespace(source=source, event_id=event_id, sender=sender, body=body)


def _qp_room(room_id: str):
    room = SimpleNamespace(room_id=room_id, users={})
    room.user_name = lambda uid: (
        "Alice" if uid == USER else "AgentBot" if uid == SELF else ""
    )
    return room


@pytest.fixture()
def qp_channel(tmp_path):
    """Channel instance without ``BaseChannel.__init__`` (no network).

    ``_is_dm`` flips the stubbed DM detection per test; ``enqueued``
    captures what would go to the agent.
    """
    channel = object.__new__(AgentTeamsMatrixChannel)
    channel._user_id = SELF
    channel._workspace_dir = tmp_path
    channel.homeserver = ""
    channel.encryption = False
    channel.dm_disabled = False
    channel.group_disabled = False
    channel.groups = {}
    channel._room_history = RoomHistory(limit=50)
    channel._client = None
    channel._is_dm = True
    channel.enqueued = []
    channel._enqueue = channel.enqueued.append
    channel.typing_calls = []

    async def _is_dm_room(*args, **kwargs):
        return channel._is_dm

    async def _read_receipt(*args, **kwargs):
        return None

    async def _typing(room_id, state, *args, **kwargs):
        channel.typing_calls.append((room_id, state))

    channel._is_dm_room = _is_dm_room
    channel._send_read_receipt = _read_receipt
    channel._send_typing = _typing
    return channel


class _FailClient:
    """Duck-typed nio client whose ``room_send`` always errors."""

    def __init__(self, resp):
        self.resp = resp
        self.calls = 0

    async def room_send(
        self,
        room_id,
        message_type,
        content,
        tx_id=None,
        ignore_unverified_devices=False,
        **kwargs,
    ):
        self.calls += 1
        return self.resp


# ── Hermes adapter driver (fake gateway, like test_adapter_hermes) ────


@pytest.fixture(scope="module")
def hermes_mod():
    """Load ``adapters/hermes/adapter.py`` against the fake gateway."""
    saved = {name: sys.modules.get(name) for name in _FAKE_MODULE_NAMES}
    sys.modules.update(_build_fake_modules())
    try:
        spec = importlib.util.spec_from_file_location(
            "agentteams_hermes_adapter_drop_ledger", HERMES_ADAPTER_PATH
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


def _hermes(hermes_mod, monkeypatch, tmp_path, *, client=None, env=True):
    """Wired adapter with the ledger rooted at ``tmp_path / "state"``."""
    for var in ALL_MATRIX_ENV_VARS:
        monkeypatch.delenv(var, raising=False)
    if env:
        monkeypatch.setenv("AGENTTEAMS_MATRIX_SERVER", "https://matrix.example.org/")
        monkeypatch.setenv("AGENTTEAMS_MATRIX_USER_ID", SELF)
        monkeypatch.setenv("AGENTTEAMS_MATRIX_TOKEN", "s3cr3t-token")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_STATE_DIR", str(tmp_path / "state"))
    adapter, client, transport, events = _wire_adapter(
        hermes_mod, client=client
    )
    return adapter, client, transport, events


# ── 1. core ledger round-trip ─────────────────────────────────────────


def test_record_read_roundtrip(tmp_path):
    record_drop(
        tmp_path,
        reason="not_mentioned",
        room_id="!r:hs",
        event_id="$e1",
        detail="d1",
    )
    record_drop(tmp_path, reason="no_reply", room_id="!r:hs")
    path = drops_path(tmp_path)
    assert path == tmp_path / DROPS_RELATIVE_PATH
    assert len(path.read_text(encoding="utf-8").splitlines()) == 2

    rows = read_drops(tmp_path)
    assert [r["reason"] for r in rows] == ["not_mentioned", "no_reply"]
    assert rows[0]["event_id"] == "$e1"
    assert rows[0]["detail"] == "d1"
    assert rows[0]["room_id"] == "!r:hs"
    assert isinstance(rows[0]["ts"], float)


def test_detail_is_capped(tmp_path):
    record_drop(
        tmp_path, reason="room_send_failed", room_id="!r:hs", detail="x" * 500
    )
    row = read_drops(tmp_path)[0]
    assert len(row["detail"]) == DETAIL_LIMIT == 200


def test_read_drops_missing_file_and_malformed_lines(tmp_path):
    assert read_drops(tmp_path) == []
    ledger = tmp_path / DROPS_RELATIVE_PATH
    ledger.parent.mkdir(parents=True)
    ledger.write_text(
        "{not json}\n"
        + json.dumps(
            {"ts": 1.0, "reason": "ping_pong", "room_id": "!r:hs",
             "event_id": "", "detail": ""}
        )
        + "\n",
        encoding="utf-8",
    )
    rows = read_drops(tmp_path)
    assert len(rows) == 1
    assert rows[0]["reason"] == "ping_pong"


def test_read_drops_since_and_limit(tmp_path):
    before = time.time()
    record_drop(tmp_path, reason="ping_pong", room_id="!r:hs")
    for i in range(5):
        record_drop(tmp_path, reason="no_reply", room_id=f"!r{i}:hs")

    assert len(read_drops(tmp_path, limit=3)) == 3
    # All six records are at/after ``before`` (numeric and ISO forms).
    assert len(read_drops(tmp_path, since=before)) == 6
    assert len(
        read_drops(
            tmp_path,
            since=datetime.fromtimestamp(before).isoformat(),
        )
    ) == 6
    future = (
        datetime.fromtimestamp(time.time()) + timedelta(hours=1)
    ).isoformat()
    assert read_drops(tmp_path, since=future) == []
    # A future numeric timestamp filters everything out too.
    assert read_drops(tmp_path, since=time.time() + 3600) == []


def test_unknown_reason_warns_but_is_still_recorded(tmp_path, caplog):
    with caplog.at_level(logging.WARNING, logger="agentteams.matrix.outcomes"):
        record_drop(tmp_path, reason="made_up", room_id="!r:hs")
    assert any("made_up" in m for m in caplog.messages)
    assert read_drops(tmp_path)[0]["reason"] == "made_up"


# ── 2. frozen vocabulary ──────────────────────────────────────────────


def test_drop_reasons_frozen_vocabulary():
    assert DROP_REASONS == (
        "not_mentioned",
        "no_reply",
        "ping_pong",
        "room_send_failed",
        "matrix_env_missing",
        "allowlist",
    )
    assert len(set(DROP_REASONS)) == len(DROP_REASONS)
    for code in DROP_REASONS:
        assert code == code.strip().lower()


def test_shared_subset_matches_teamharness_contract():
    # The TeamHarness side (``NOTIFICATION_GAP_REASONS``) lives in a
    # separate branch; the codes both sides produce must stay verbatim
    # identical, so both suites assert this frozen shared subset.
    shared = {"matrix_env_missing", "room_send_failed"}
    assert shared <= set(DROP_REASONS)
    for code in shared:
        assert code in DROP_REASONS


# ── 3. qwenpaw drop points ────────────────────────────────────────────


def test_thread_event_without_mention_records_not_mentioned(qp_channel):
    qp_channel._is_dm = False
    event = _qp_event(body="thread chit-chat", event_id="$thread-1", thread=True)
    asyncio.run(qp_channel._on_room_event(_qp_room(GROUP_ROOM), event))

    rows = read_drops(qp_channel._workspace_dir)
    assert len(rows) == 1
    assert rows[0]["reason"] == "not_mentioned"
    assert rows[0]["room_id"] == GROUP_ROOM
    assert rows[0]["event_id"] == "$thread-1"
    assert qp_channel.enqueued == []


def test_group_event_without_mention_records_not_mentioned(qp_channel):
    qp_channel._is_dm = False
    event = _qp_event(body="chit-chat", event_id="$g1")
    asyncio.run(qp_channel._on_room_event(_qp_room(GROUP_ROOM), event))

    rows = read_drops(qp_channel._workspace_dir)
    assert [r["reason"] for r in rows] == ["not_mentioned"]
    assert rows[0]["event_id"] == "$g1"
    # Message still lands in the history buffer (semantics unchanged).
    assert qp_channel._room_history.record_count(GROUP_ROOM) == 1
    assert qp_channel.enqueued == []


def test_no_reply_records(qp_channel):
    event = _qp_event(body="NO_REPLY", room_id=DM_ROOM)
    asyncio.run(qp_channel._on_room_event(_qp_room(DM_ROOM), event))

    rows = read_drops(qp_channel._workspace_dir)
    assert [r["reason"] for r in rows] == ["no_reply"]
    assert qp_channel.enqueued == []
    # Flow continued: typing was stopped after the silent drop.
    assert qp_channel.typing_calls[-1] == (DM_ROOM, False)


def test_self_trigger_records_ping_pong(qp_channel):
    qp_channel._is_dm = True
    event = _qp_event(body="echo", sender=SELF, room_id=DM_ROOM)
    asyncio.run(qp_channel._on_room_event(_qp_room(DM_ROOM), event))

    rows = read_drops(qp_channel._workspace_dir)
    assert [r["reason"] for r in rows] == ["ping_pong"]
    assert rows[0]["event_id"] == "$e1"
    assert qp_channel.enqueued == []


def test_room_send_http_403_records_room_send_failed(qp_channel):
    resp = RoomSendError(
        message="Forbidden", status_code="M_FORBIDDEN", room_id=GROUP_ROOM
    )
    resp.transport_response = httpx.Response(403)
    client = _FailClient(resp)
    qp_channel._client = client

    result = asyncio.run(
        qp_channel._room_send_with_retry(
            GROUP_ROOM, "m.room.message", {"msgtype": "m.text", "body": "x"}
        )
    )
    assert result is resp
    assert client.calls == 1  # 403 is not retryable: one attempt, one record

    rows = read_drops(qp_channel._workspace_dir)
    assert len(rows) == 1
    row = rows[0]
    assert row["reason"] == "room_send_failed"
    assert row["room_id"] == GROUP_ROOM
    assert "M_FORBIDDEN" in row["detail"]
    assert "403" in row["detail"]


def test_room_send_exception_records_room_send_failed(qp_channel):
    class _RaiseClient:
        async def room_send(self, *a, **k):
            raise ConnectionError("homeserver unreachable")

    qp_channel._client = _RaiseClient()
    with pytest.raises(ConnectionError):
        asyncio.run(
            qp_channel._room_send_with_retry(
                GROUP_ROOM,
                "m.room.message",
                {"msgtype": "m.text", "body": "x"},
                max_retries=1,
                base_delay=0.01,
            )
        )
    rows = read_drops(qp_channel._workspace_dir)
    assert [r["reason"] for r in rows] == ["room_send_failed"]
    assert "ConnectionError" in rows[0]["detail"]


def test_start_without_homeserver_records_env_missing(qp_channel):
    asyncio.run(qp_channel.start())
    rows = read_drops(qp_channel._workspace_dir)
    assert [r["reason"] for r in rows] == ["matrix_env_missing"]
    assert rows[0]["room_id"] == ""


# ── 4. hermes drop points ─────────────────────────────────────────────


def test_hermes_thread_without_mention_records_not_mentioned(
    hermes_mod, monkeypatch, tmp_path
):
    members, room = _group_fixtures()
    client = FakeClient(
        member_map={GROUP_ROOM: members}, rooms={GROUP_ROOM: room}
    )
    adapter, _, _, events = _hermes(
        hermes_mod, monkeypatch, tmp_path, client=client
    )
    asyncio.run(
        adapter._on_inbound(
            _inbound(
                room_id=GROUP_ROOM,
                text="thread chit-chat",
                event_id="$thread-1",
                thread_root="$root-1",
            )
        )
    )
    assert events == []
    rows = read_drops(adapter.state_dir)
    assert [r["reason"] for r in rows] == ["not_mentioned"]
    assert rows[0]["event_id"] == "$thread-1"


def test_hermes_no_reply_records(hermes_mod, monkeypatch, tmp_path):
    client = FakeClient(
        member_map={DM_ROOM: [SELF, USER]},
    )
    adapter, _, _, events = _hermes(
        hermes_mod, monkeypatch, tmp_path, client=client
    )
    asyncio.run(adapter._on_inbound(_inbound(room_id=DM_ROOM, text="NO_REPLY")))
    assert events == []
    rows = read_drops(adapter.state_dir)
    assert [r["reason"] for r in rows] == ["no_reply"]


def test_hermes_self_trigger_records_ping_pong(
    hermes_mod, monkeypatch, tmp_path
):
    adapter, _, _, events = _hermes(hermes_mod, monkeypatch, tmp_path)
    asyncio.run(
        adapter._on_inbound(
            _inbound(room_id=DM_ROOM, sender=SELF, text="echo")
        )
    )
    assert events == []
    rows = read_drops(adapter.state_dir)
    assert [r["reason"] for r in rows] == ["ping_pong"]
    assert rows[0]["event_id"] == "$e1"


def test_hermes_send_failure_records_room_send_failed(
    hermes_mod, monkeypatch, tmp_path
):
    adapter, _, transport, _ = _hermes(hermes_mod, monkeypatch, tmp_path)
    transport.send_exc = RuntimeError("homeserver unreachable")
    result = asyncio.run(adapter.send(GROUP_ROOM, "hello"))
    assert result.success is False
    rows = read_drops(adapter.state_dir)
    assert [r["reason"] for r in rows] == ["room_send_failed"]
    assert "RuntimeError" in rows[0]["detail"]


def test_hermes_connect_without_config_records_env_missing(
    hermes_mod, monkeypatch, tmp_path
):
    adapter, _, _, _ = _hermes(hermes_mod, monkeypatch, tmp_path, env=False)
    assert asyncio.run(adapter.connect()) is False
    assert adapter.has_fatal_error
    assert adapter._fatal_error_code == "config_missing"
    rows = read_drops(adapter.state_dir)
    assert [r["reason"] for r in rows] == ["matrix_env_missing"]
    assert rows[0]["room_id"] == ""


# ── 5. write-failure safety ───────────────────────────────────────────


def test_write_failure_never_raises_and_flow_continues(tmp_path):
    # A plain file where ``shared/`` must be a directory: mkdir/append
    # fails, and record_drop must swallow it.
    (tmp_path / "shared").write_text("blocker", encoding="utf-8")
    record_drop(tmp_path, reason="not_mentioned", room_id="!r:hs")  # no raise
    healthy = tmp_path / "ok"
    record_drop(healthy, reason="no_reply", room_id="!r:hs")
    assert [r["reason"] for r in read_drops(healthy)] == ["no_reply"]


def test_channel_flow_continues_when_ledger_unwritable(qp_channel):
    (qp_channel._workspace_dir / "shared").write_text(
        "blocker", encoding="utf-8"
    )
    event = _qp_event(body="NO_REPLY", room_id=DM_ROOM)
    # Must complete without raising: recording never disturbs the flow.
    asyncio.run(qp_channel._on_room_event(_qp_room(DM_ROOM), event))
    assert qp_channel.enqueued == []
    assert qp_channel.typing_calls[-1] == (DM_ROOM, False)


# ── 6. qwenpaw / hermes parity (same input → same reason) ────────────


def _drive_qwenpaw(qp_channel, scenario):
    if scenario == "thread_unmentioned":
        qp_channel._is_dm = False
        event = _qp_event(
            body="thread chit-chat", event_id="$thread-1", thread=True
        )
        room = _qp_room(GROUP_ROOM)
    elif scenario == "no_reply":
        qp_channel._is_dm = True
        event = _qp_event(body="NO_REPLY", room_id=DM_ROOM)
        room = _qp_room(DM_ROOM)
    else:  # ping_pong
        qp_channel._is_dm = True
        event = _qp_event(body="echo", sender=SELF, room_id=DM_ROOM)
        room = _qp_room(DM_ROOM)
    asyncio.run(qp_channel._on_room_event(room, event))
    return [r["reason"] for r in read_drops(qp_channel._workspace_dir)]


def _drive_hermes(hermes_mod, monkeypatch, tmp_path, scenario):
    if scenario == "thread_unmentioned":
        members, room = _group_fixtures()
        client = FakeClient(
            member_map={GROUP_ROOM: members}, rooms={GROUP_ROOM: room}
        )
        adapter, _, _, _ = _hermes(hermes_mod, monkeypatch, tmp_path, client=client)
        inbound = _inbound(
            room_id=GROUP_ROOM,
            text="thread chit-chat",
            event_id="$thread-1",
            thread_root="$root-1",
        )
    elif scenario == "no_reply":
        client = FakeClient(
            member_map={DM_ROOM: [SELF, USER]},
        )
        adapter, _, _, _ = _hermes(hermes_mod, monkeypatch, tmp_path, client=client)
        inbound = _inbound(room_id=DM_ROOM, text="NO_REPLY")
    else:  # ping_pong
        adapter, _, _, _ = _hermes(hermes_mod, monkeypatch, tmp_path)
        inbound = _inbound(room_id=DM_ROOM, sender=SELF, text="echo")
    asyncio.run(adapter._on_inbound(inbound))
    return [r["reason"] for r in read_drops(adapter.state_dir)]


@pytest.mark.parametrize(
    "scenario", ["thread_unmentioned", "no_reply", "ping_pong"]
)
def test_same_input_same_reason_across_adapters(
    scenario, qp_channel, hermes_mod, monkeypatch, tmp_path
):
    qp_reasons = _drive_qwenpaw(qp_channel, scenario)
    hermes_reasons = _drive_hermes(hermes_mod, monkeypatch, tmp_path, scenario)
    assert qp_reasons == hermes_reasons
    assert len(qp_reasons) == 1
    # The OpenClaw runtime is TypeScript (separate repo); parity there is
    # a documented TODO in adapters/openclaw/README.md.
    assert qp_reasons[0] in DROP_REASONS


# ── 7. credential redaction (the ledger must never hold a token) ─────
#
# #1497 merge blocker: ``QwenPawMatrixClient.send`` (``query_token=True``)
# puts the ``access_token`` in the request query, and transport
# exceptions (aiohttp ``TooManyRedirects``) embed the full request URL
# in ``str(exc)``.  The maintainer's repro showed the complete message
# (182 chars) fits inside ``DETAIL_LIMIT`` — the 200-char cap alone
# cannot protect the credential, so redaction must run BEFORE it.


# Synthetic credential: long enough to be a real token, sized so the
# whole URL-bearing exception message stays under ``DETAIL_LIMIT``
# (the repro precondition asserted in the integration test below).
SYNTHETIC_SECRET = "syt_synthetic_16"


class _AlwaysRedirectHandler(BaseHTTPRequestHandler):
    """Homeserver stand-in that answers 307 forever, echoing the request
    path (query included) back in ``Location`` so the ``access_token``
    query parameter survives every redirect hop."""

    port = 0

    def _redirect(self):
        length = int(self.headers.get("Content-Length") or 0)
        if length:
            self.rfile.read(length)
        self.send_response(307)
        self.send_header(
            "Location",
            "http://127.0.0.1:{0}{1}".format(self.__class__.port, self.path),
        )
        self.send_header("Content-Length", "0")
        self.end_headers()

    do_GET = do_PUT = do_POST = _redirect

    def log_message(self, *args):
        pass  # keep pytest output clean


def _run_qwenpaw_redirect_repro(channel):
    """Maintainer repro: a REAL ``QwenPawMatrixClient``
    (``build_client(TransportConfig(query_token=True, ...))``) sends
    through the channel's real ``_room_send_with_retry`` against a local
    ``ThreadingHTTPServer`` that redirects forever.

    Returns ``(raised, rows)`` — the transport exception (expected
    ``aiohttp.TooManyRedirects``) and the ledger records it produced.
    The server and the client are closed here (same event loop).
    """
    from core.transport import TransportConfig, build_client

    server = ThreadingHTTPServer(("127.0.0.1", 0), _AlwaysRedirectHandler)
    port = server.server_address[1]
    _AlwaysRedirectHandler.port = port
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    client = build_client(
        TransportConfig(
            homeserver="http://127.0.0.1:{0}".format(port),
            access_token=SYNTHETIC_SECRET,
            query_token=True,
        )
    )
    channel._client = client

    async def _scenario():
        raised = None
        try:
            await channel._room_send_with_retry(
                GROUP_ROOM,
                "m.room.message",
                {"msgtype": "m.text", "body": "x"},
                max_retries=1,
                base_delay=0.01,
            )
        except BaseException as exc:  # captured, not swallowed
            raised = exc
        await client.close()
        return raised

    try:
        raised = asyncio.run(_scenario())
    finally:
        server.shutdown()
        thread.join(timeout=5)
    return raised, read_drops(channel._workspace_dir)


def _url_bearing_exception():
    """A send-failure exception whose message embeds the request URL
    carrying the query token (the shape transport exceptions have)."""
    return RuntimeError(
        "TooManyRedirects: GET https://hs/_matrix/client/v3/rooms"
        "?access_token={0}&x=1".format(SYNTHETIC_SECRET)
    )


# ── unit: redact_detail / describe_exception ──────────────────────────


def test_redact_detail_query_token_redacted_before_persist(tmp_path):
    record_drop(
        tmp_path,
        reason="room_send_failed",
        room_id="!r:hs",
        detail="GET https://hs/_matrix/x?access_token=SECRET123&x=1",
    )
    row = read_drops(tmp_path)[0]
    assert "SECRET123" not in row["detail"]
    assert "access_token=***" in row["detail"]
    # Non-credential parameters keep their diagnostic value.
    assert "x=1" in row["detail"]


def test_redact_detail_safe_text_untouched(tmp_path):
    safe = "ConnectionError: homeserver unreachable"
    assert redact_detail(safe) == safe
    record_drop(
        tmp_path, reason="room_send_failed", room_id="!r:hs", detail=safe
    )
    # Safe diagnostic text survives the ledger round-trip verbatim.
    assert read_drops(tmp_path)[0]["detail"] == safe


def test_redact_detail_url_userinfo_redacted():
    assert (
        redact_detail("GET https://alice:pass123@hs/_matrix/client/v3/rooms")
        == "GET https://alice:***@hs/_matrix/client/v3/rooms"
    )


def test_redact_detail_bearer_redacted():
    assert (
        redact_detail("request failed: Authorization: Bearer abc.def.ghi123")
        == "request failed: Authorization: Bearer ***"
    )


def test_describe_exception_keeps_type_name_and_redacts():
    exc = RuntimeError("GET https://hs/_matrix/x?access_token=SECRET123&x=1")
    out = describe_exception(exc)
    assert out.startswith("RuntimeError: ")
    assert "SECRET123" not in out
    assert "access_token=***" in out


# ── integration: the maintainer repro (query-token send path) ────────


def test_qwenpaw_query_token_send_failure_never_leaks_token(qp_channel):
    """End-to-end regression for the #1497 merge blocker.

    Real client + real ``_room_send_with_retry`` + a local server that
    redirects forever: the ``TooManyRedirects`` message embeds the
    request URL carrying ``access_token=<SYNTHETIC_SECRET>``, and the
    repro precondition below asserts the whole raw message fits inside
    ``DETAIL_LIMIT`` with the token intact — truncation alone cannot
    keep it out.  The persisted line must be redacted.
    """
    raised, rows = _run_qwenpaw_redirect_repro(qp_channel)
    assert isinstance(raised, aiohttp.TooManyRedirects)
    assert len(rows) == 1  # not retryable: one attempt, one record
    row = rows[0]
    assert row["reason"] == "room_send_failed"
    assert row["room_id"] == GROUP_ROOM
    # The exception type name is retained for diagnosis...
    assert "TooManyRedirects" in row["detail"]
    # ...the credential never lands on disk...
    assert SYNTHETIC_SECRET not in row["detail"]
    ledger_text = drops_path(qp_channel._workspace_dir).read_text(
        encoding="utf-8"
    )
    assert SYNTHETIC_SECRET not in ledger_text
    # ...and the marker shows the URL really did carry the token.
    assert "access_token=***" in row["detail"]
    # Repro precondition: the unredacted detail fits under DETAIL_LIMIT
    # with the token intact — the cap is not what keeps it out.
    raw = "{0}: {1}".format(type(raised).__name__, raised)
    assert SYNTHETIC_SECRET in raw
    assert len(raw) <= DETAIL_LIMIT


def test_red_repro_identity_redaction_leaks_token(qp_channel, monkeypatch):
    """Red control for the maintainer repro.

    Swaps ``redact_detail`` back to the identity function — disabling
    BOTH layers at once, since ``record_drop``'s central backstop and
    ``describe_exception`` (used by the call sites) resolve it from the
    same module globals — and runs the identical scenario: the raw
    token must then land in the ledger.  Proves the scenario truly
    reproduces the leak, i.e. the assertions in
    ``test_qwenpaw_query_token_send_failure_never_leaks_token`` (the
    exact opposite) go red without the fix.
    """
    monkeypatch.setattr(_outcomes, "redact_detail", lambda text: text)
    raised, rows = _run_qwenpaw_redirect_repro(qp_channel)
    assert isinstance(raised, aiohttp.TooManyRedirects)
    assert len(rows) == 1
    detail = rows[0]["detail"]
    assert SYNTHETIC_SECRET in detail  # the leak is present without redaction
    assert "access_token=***" not in detail


# ── integration: the Hermes send-failure sites ───────────────────────


def test_hermes_send_url_bearing_exception_never_leaks_token(
    hermes_mod, monkeypatch, tmp_path
):
    adapter, _, transport, _ = _hermes(hermes_mod, monkeypatch, tmp_path)
    transport.send_exc = _url_bearing_exception()
    result = asyncio.run(adapter.send(GROUP_ROOM, "hello"))
    assert result.success is False
    rows = read_drops(adapter.state_dir)
    assert [r["reason"] for r in rows] == ["room_send_failed"]
    assert "RuntimeError" in rows[0]["detail"]
    assert SYNTHETIC_SECRET not in rows[0]["detail"]
    assert "access_token=***" in rows[0]["detail"]


def test_hermes_media_send_url_bearing_exception_never_leaks_token(
    hermes_mod, monkeypatch, tmp_path
):
    exc = _url_bearing_exception()

    class _RaisingSendClient(FakeClient):
        async def room_send(self, *args, **kwargs):
            raise exc

    client = _RaisingSendClient(member_map={GROUP_ROOM: [SELF, USER]})
    adapter, _, _, _ = _hermes(
        hermes_mod, monkeypatch, tmp_path, client=client
    )
    image = tmp_path / "shot.png"
    image.write_bytes(b"\x89PNG fake")
    result = asyncio.run(
        adapter._send_media_file(GROUP_ROOM, str(image), "m.image", "cap")
    )
    assert result.success is False
    rows = read_drops(adapter.state_dir)
    assert [r["reason"] for r in rows] == ["room_send_failed"]
    assert SYNTHETIC_SECRET not in rows[0]["detail"]
    assert "access_token=***" in rows[0]["detail"]
