# -*- coding: utf-8 -*-
"""Attention notification delivery receipt: recipients[] + gaps[].

request_attention's send result now carries per-recipient visibility:
``recipients[]`` (the mxids written into this send's ``m.mentions`` with
a ``delivered`` flag — the mention was written AND the room send
succeeded, which is not a read or an answer) and ``gaps[]`` (NOTIFICATION_
GAP_REASONS codes for the recipients that could not be reached). Both
are persisted on the ``notification_log`` entry (a same-attempt retry
folds the previous receipt into ``retries[]``) and echoed verbatim on
the response's ``attention.notification``. A non-empty ``gaps`` list is
not a failure — only ``sent=false`` is — and ``channel_accepted`` stays
null: the room adapter reports no per-recipient receipt, and it is
never inferred from room acceptance.

The tests enter through the assembled server (refs proxies keep
server-attribute monkeypatches visible) and fake only the membership
check and the Matrix HTTP send; the real ``_send_attention_notification``
runs in between.
"""

from __future__ import annotations

import io
import json
import sys
import urllib.error
from pathlib import Path
from typing import Any

import pytest


MCP_DIR = Path(__file__).resolve().parents[2] / "teamharness" / "mcp"
if str(MCP_DIR) not in sys.path:
    sys.path.insert(0, str(MCP_DIR))

import server  # noqa: E402


ROOM = "!gaproom:matrix.local"
LEADER = "@lead:matrix.local"
HUMAN = "@luo:matrix.local"
TASK_ID = "attn-gap-task-1"


class _FakeResponse:
    def __init__(self, payload: dict[str, Any]) -> None:
        self._payload = json.dumps(payload).encode("utf-8")

    def read(self) -> bytes:
        return self._payload

    def __enter__(self) -> "_FakeResponse":
        return self

    def __exit__(self, *exc: Any) -> bool:
        return False


class _SendCapture:
    """Stands in for urlopen on the Matrix send path.

    ``fail_next`` selects the failure mode for the next request:
    ``"http"`` raises HTTP 403, ``"network"`` raises a URLError.
    """

    def __init__(self) -> None:
        self.requests: list[tuple[str, dict[str, Any]]] = []
        self.fail_next: str | None = None

    def urlopen(self, request: Any, timeout: Any = None) -> _FakeResponse:
        url = request.full_url if hasattr(request, "full_url") else str(request)
        body = json.loads(request.data.decode("utf-8") or "{}")
        self.requests.append((url, body))
        if self.fail_next == "http":
            self.fail_next = None
            raise urllib.error.HTTPError(url, 403, "Forbidden", {}, io.BytesIO(b"Forbidden"))
        if self.fail_next == "network":
            self.fail_next = None
            raise urllib.error.URLError("connection refused")
        return _FakeResponse({"event_id": f"$evt-{len(self.requests)}"})


def _arguments(workspace: Path, **payload: Any) -> dict[str, Any]:
    return {
        "role": "worker",
        "action": "request_attention",
        "workspaceDir": str(workspace),
        "payload": payload,
    }


def _write_task(workspace: Path, task_id: str, meta: dict[str, Any]) -> None:
    path = workspace / "shared" / "tasks" / task_id / "meta.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(meta, ensure_ascii=False, indent=2), encoding="utf-8")


def _read_task(workspace: Path, task_id: str) -> dict[str, Any]:
    path = workspace / "shared" / "tasks" / task_id / "meta.json"
    return json.loads(path.read_text(encoding="utf-8"))


def _prepare(
    workspace: Path,
    monkeypatch: pytest.MonkeyPatch,
    *,
    room_id: str | None = ROOM,
    leader: str = LEADER,
    humans: tuple[str, ...] = (HUMAN,),
) -> tuple[Path, str, dict[str, set[str]], _SendCapture]:
    """Assembled-server stand-ins for the attention send path.

    Returns the mutable ``membership`` fact table (``in_room`` mxid set);
    a test may mutate ``membership["in_room"]`` between calls.
    """
    meta: dict[str, Any] = {
        "task_id": TASK_ID,
        "status": "in_progress",
        "project_id": "",
    }
    if room_id is not None:
        meta["room_id"] = room_id
    _write_task(workspace, TASK_ID, meta)

    monkeypatch.setattr(server, "_pull_project", lambda *a, **k: True)
    monkeypatch.setattr(server, "_pull_task", lambda *a, **k: True)
    monkeypatch.setattr(server, "_sync_task", lambda *a, **k: True)
    membership: dict[str, set[str]] = {"in_room": {leader, *humans}}

    def fake_membership(room: str, assignee: str) -> dict[str, Any]:
        if assignee in membership["in_room"]:
            return {"ok": True, "member": True}
        return {
            "ok": False,
            "error": f"assignee {assignee} is not a joined member of room {room}",
        }

    monkeypatch.setattr(server, "_validate_assignee_membership", fake_membership)
    monkeypatch.setattr(server.taskflow, "_team_leader_matrix_id", lambda: leader)
    monkeypatch.setattr(server.taskflow, "_team_human_matrix_ids", lambda: list(humans))
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "http://matrix.local")
    monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "test-token")
    capture = _SendCapture()
    monkeypatch.setattr(server.urllib.request, "urlopen", capture.urlopen)
    return workspace, TASK_ID, membership, capture


def _request(workspace: Path, **payload: Any) -> dict[str, Any]:
    return server._taskflow(_arguments(workspace, **payload))


def test_gap_vocabulary_is_frozen() -> None:
    import mcp.room.notify as notify_module

    assert notify_module.NOTIFICATION_GAP_REASONS == (
        "leader_not_in_room",
        "human_not_in_room",
        "no_room_id",
        "no_leader_mxid",
        "matrix_env_missing",
        "room_send_failed",
    )


def test_all_recipients_in_room_delivered(tmp_path, monkeypatch):
    workspace, _, _, capture = _prepare(tmp_path, monkeypatch)
    result = _request(workspace, taskId=TASK_ID, kind="approval", question="Approve?")
    assert result["ok"] is True, result
    notification = result["attention"]["notification"]
    assert notification["sent"] is True
    assert notification["recipients"] == [
        {"mxid": LEADER, "role": "leader", "delivered": True},
        {"mxid": HUMAN, "role": "human", "delivered": True},
    ]
    assert notification["gaps"] == []
    # Delivered means the mxids were written into the m.mentions of the
    # room event that actually went out.
    url, content = capture.requests[0]
    assert "/send/m.room.message/" in url
    assert content["m.mentions"]["user_ids"] == [LEADER, HUMAN]
    entry = result["task"]["attention"][-1]["notification_log"][-1]
    assert entry["room_accepted"] is True
    assert len(entry["recipients"]) == 2
    assert entry["gaps"] == []
    # The per-recipient receipt is not reported by the room adapter: it
    # must stay null, never inferred from room acceptance.
    assert entry["channel_accepted"] is None


def test_human_not_in_room_gaps(tmp_path, monkeypatch):
    workspace, _, membership, _ = _prepare(tmp_path, monkeypatch)
    membership["in_room"] = {LEADER}  # the human was left behind
    result = _request(workspace, taskId=TASK_ID, kind="approval", question="Approve?")
    assert result["ok"] is True
    notification = result["attention"]["notification"]
    # The leader still receives: a human gap is not a send failure.
    assert notification["sent"] is True
    assert notification["recipients"] == [
        {"mxid": LEADER, "role": "leader", "delivered": True},
    ]
    assert notification["gaps"] == [
        {
            "recipient": HUMAN,
            "role": "human",
            "reason": "human_not_in_room",
            "error": f"assignee {HUMAN} is not a joined member of room {ROOM}",
        }
    ]
    entry = result["task"]["attention"][-1]["notification_log"][-1]
    assert entry["room_accepted"] is True
    assert entry["recipients"] == [
        {"mxid": LEADER, "role": "leader", "delivered": True},
    ]
    assert entry["gaps"][0]["reason"] == "human_not_in_room"
    assert entry["gaps"][0]["recipient"] == HUMAN


def test_leader_not_in_room_no_send(tmp_path, monkeypatch):
    workspace, _, membership, capture = _prepare(tmp_path, monkeypatch)
    membership["in_room"] = {HUMAN}  # the leader is not in the task room
    result = _request(workspace, taskId=TASK_ID, kind="approval", question="Approve?")
    # Ok semantics unchanged: a skipped notification is not a tool failure.
    assert result["ok"] is True
    notification = result["attention"]["notification"]
    assert notification["sent"] is False
    assert notification["skipped"] is True
    assert notification["recipients"] == []
    assert notification["gaps"][0]["reason"] == "leader_not_in_room"
    assert notification["gaps"][0]["recipient"] == LEADER
    assert notification["gaps"][0]["role"] == "leader"
    # The existing no-send behavior is preserved: no Matrix request at all.
    assert capture.requests == []
    record = result["task"]["attention"][-1]
    assert "eventId" not in record
    entry = record["notification_log"][-1]
    assert entry["room_accepted"] is False
    assert entry["channel_accepted"] is None
    assert entry["gaps"][0]["reason"] == "leader_not_in_room"


def test_no_room_id_gap(tmp_path, monkeypatch):
    workspace, _, _, capture = _prepare(tmp_path, monkeypatch, room_id=None)
    result = _request(workspace, taskId=TASK_ID, kind="approval", question="Approve?")
    assert result["ok"] is True
    notification = result["attention"]["notification"]
    assert notification["sent"] is False
    assert notification["recipients"] == []
    assert notification["gaps"] == [{"reason": "no_room_id"}]
    assert capture.requests == []
    entry = result["task"]["attention"][-1]["notification_log"][-1]
    assert entry["room_accepted"] is False
    assert entry["gaps"] == [{"reason": "no_room_id"}]


def test_no_leader_mxid_gap(tmp_path, monkeypatch):
    workspace, _, _, capture = _prepare(tmp_path, monkeypatch, leader="")
    result = _request(workspace, taskId=TASK_ID, kind="approval", question="Approve?")
    assert result["ok"] is True
    notification = result["attention"]["notification"]
    assert notification["sent"] is False
    assert notification["recipients"] == []
    assert notification["gaps"] == [{"reason": "no_leader_mxid"}]
    assert capture.requests == []
    entry = result["task"]["attention"][-1]["notification_log"][-1]
    assert entry["room_accepted"] is False
    assert entry["gaps"] == [{"reason": "no_leader_mxid"}]


def test_matrix_env_missing_gap(monkeypatch):
    import mcp.room.notify as notify_module

    monkeypatch.delenv("AGENTTEAMS_MATRIX_URL", raising=False)
    monkeypatch.delenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", raising=False)
    result = notify_module._send_attention_notification(
        {},
        room_id=ROOM,
        task_id=TASK_ID,
        kind="approval",
        question="Approve?",
        leader=LEADER,
        humans=[HUMAN],
    )
    assert result["sent"] is False
    assert result["recipients"] == []
    assert result["gaps"] == [{"reason": "matrix_env_missing"}]


def test_retry_folds_receipt_into_retries(tmp_path, monkeypatch):
    workspace, _, _, capture = _prepare(tmp_path, monkeypatch)
    capture.fail_next = "http"
    first = _request(workspace, taskId=TASK_ID, kind="approval", question="Approve?")
    assert first["ok"] is True
    # The failed send leaves the record pending (no eventId), so the
    # retry re-sends the same attempt.
    assert "eventId" not in first["task"]["attention"][-1]
    failed = first["task"]["attention"][-1]["notification_log"][-1]
    assert failed["room_accepted"] is False
    assert failed["channel_accepted"] is None
    assert [r["delivered"] for r in failed["recipients"]] == [False, False]
    assert failed["gaps"][0]["reason"] == "room_send_failed"
    assert "HTTP 403" in failed["gaps"][0]["error"]

    second = _request(workspace, taskId=TASK_ID, kind="approval", question="Approve?")
    assert second["ok"] is True
    record = second["task"]["attention"][-1]
    log = record["notification_log"]
    # One entry per logical attempt: the retry did not append a second
    # attempt=1 entry.
    assert len(log) == 1, log
    entry = log[0]
    assert entry["attempt"] == 1
    assert entry["room_accepted"] is True
    assert entry["gaps"] == []
    assert [r["delivered"] for r in entry["recipients"]] == [True, True]
    # The failed physical attempt was folded into retries[] with its
    # receipt.
    assert len(entry["retries"]) == 1, entry
    folded = entry["retries"][0]
    assert folded["room_accepted"] is False
    assert folded["channel_accepted"] is None
    assert "HTTP 403" in folded["error"]
    assert [r["delivered"] for r in folded["recipients"]] == [False, False]
    assert folded["gaps"][0]["reason"] == "room_send_failed"
    assert record["eventId"]
    on_disk = _read_task(tmp_path, TASK_ID)
    assert len(on_disk["attention"][-1]["notification_log"]) == 1


def test_new_attempt_appends_entry(tmp_path, monkeypatch):
    workspace, _, _, _ = _prepare(tmp_path, monkeypatch)
    first = _request(workspace, taskId=TASK_ID, kind="approval", question="Approve?")
    assert first["ok"] is True
    # Close the first record; a re-ask for the same kind mints a second
    # attempt, which must get its own log entry (no folding across
    # attempts).
    resolved = _request(
        workspace, taskId=TASK_ID, kind="approval", question="Approve?", resolved=True
    )
    assert resolved["ok"] is True
    third = _request(workspace, taskId=TASK_ID, kind="approval", question="Approve again?")
    assert third["ok"] is True
    records = [r for r in third["task"]["attention"] if r.get("kind") == "approval"]
    assert [r["attempt"] for r in records] == [1, 2]
    assert records[0]["notification_log"][0]["attempt"] == 1
    assert records[1]["notification_log"][0]["attempt"] == 2
    assert records[1]["notification_log"][0]["room_accepted"] is True
