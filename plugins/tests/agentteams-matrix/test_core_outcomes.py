"""Drop-ledger tests (core.outcomes) and per-adapter drop-point coverage.

Covers the silent-drop trace contract:

* ``record_drop`` / ``read_drops`` round-trip on
  ``<workspace>/shared/matrix/drops.jsonl`` (append-only, detail cap,
  ``since``/``limit`` filters, malformed-line tolerance);
* write failures are warn-only and never raise;
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
import time
from datetime import datetime, timedelta
from pathlib import Path
from types import SimpleNamespace

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

from core.history import RoomHistory  # noqa: E402
from core.outcomes import (  # noqa: E402
    DETAIL_LIMIT,
    DROP_REASONS,
    DROPS_RELATIVE_PATH,
    drops_path,
    read_drops,
    record_drop,
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
