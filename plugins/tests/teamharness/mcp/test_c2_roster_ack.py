"""C-2 (TeamHarness half): delegate_task roster gate + register_delegated_task.

Two breakpoints from the maintainer baseline report:

1. ``delegate_task`` did not validate the recipient — a short name or an
   unknown mxid was silently sent, and the leader believed the task was
   dispatched.
2. Worker-side task registration relied on a *model* action — a missed
   step made ``submit_task`` fail with ``task not found``, and the model
   then claimed completion in the group chat.

This file covers the TeamHarness side only: the pre-write roster gate in
``delegate_task`` (recipient must resolve against the team roster before
any write or notification) and the runtime-facing
``register_delegated_task`` action (records the delegation link and the
ack receipt in the local task meta; no status change, no signal event,
no sync). The MX adapter wiring is a later slice.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Any

import pytest


MCP_DIR = Path(__file__).resolve().parents[3] / "teamharness" / "mcp"
if str(MCP_DIR) not in sys.path:
    sys.path.insert(0, str(MCP_DIR))

import server  # noqa: E402


WORKER_A = "@worker-a:example.test"
WORKER_B = "@worker-b:example.test"
ADMIN = "@admin:example.test"
ROOM = "!taskroom:example.test"
PROJECT_ID = "c2-project"
TASK_ID = "c2-project-01"


# --- workspace / roster fixtures -----------------------------------------


def _write_project(workspace: Path, task_status: str = "planned") -> None:
    project = {
        "project_id": PROJECT_ID,
        "title": "C2 roster ack project",
        "status": "active",
        "tasks": [
            {
                "task_id": TASK_ID,
                "title": "C2 task",
                "status": task_status,
            }
        ],
    }
    server._write_json(workspace / "shared" / "projects" / PROJECT_ID / "meta.json", project)


def _write_task_meta(workspace: Path, task: dict[str, Any]) -> None:
    server._write_json(workspace / "shared" / "tasks" / task["task_id"] / "meta.json", task)


def _assigned_task_meta() -> dict[str, Any]:
    return {
        "task_id": TASK_ID,
        "project_id": PROJECT_ID,
        "room_id": ROOM,
        "status": "assigned",
        "assigned_to": WORKER_A,
        "assigned_at": "2026-11-02T00:00:00Z",
    }


def _write_roster(
    workspace: Path,
    members: list[Any],
    admin: str | None = ADMIN,
) -> Path:
    team: dict[str, Any] = {"members": members}
    if admin:
        team["admin"] = {"matrixUserId": admin}
    config_path = workspace / "runtime-config.json"
    config_path.write_text(json.dumps({"team": team}), encoding="utf-8")
    return config_path


def _task_meta_path(workspace: Path, task_id: str = TASK_ID) -> Path:
    return workspace / "shared" / "tasks" / task_id / "meta.json"


def _event_count(workspace: Path) -> int:
    events_dir = workspace / "shared" / "events"
    if not events_dir.is_dir():
        return 0
    return len(list(events_dir.glob("*/*.json")))


@pytest.fixture
def side_effects(monkeypatch: pytest.MonkeyPatch) -> dict[str, list[Any]]:
    calls: dict[str, list[Any]] = {"notify": []}

    def notify(*args: Any, **kwargs: Any) -> dict[str, Any]:
        calls["notify"].append(kwargs)
        return {
            "sent": True,
            "eventId": f"$c2-event-{len(calls['notify'])}",
            "roomId": kwargs.get("room_id", ""),
            "assignee": kwargs.get("assignee", ""),
        }

    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "http://matrix.example.test")
    monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "test-token")
    # Standalone by default; roster tests opt in per-test via setenv.
    monkeypatch.delenv("TEAMHARNESS_RUNTIME_CONFIG", raising=False)
    monkeypatch.setattr(server, "_pull_project", lambda *_a, **_k: True)
    monkeypatch.setattr(server, "_pull_task", lambda *_a, **_k: True)
    monkeypatch.setattr(server, "_sync_task", lambda *_a, **_k: True)
    monkeypatch.setattr(server, "_sync_project", lambda *_a, **_k: True)
    monkeypatch.setattr(server, "_send_delegate_notification", notify)
    monkeypatch.setattr(
        server,
        "_validate_assignee_membership",
        lambda *_a, **_k: {"ok": True},
    )

    def no_matrix_membership(*_a: Any, **_k: Any) -> Any:
        raise ValueError("matrix membership unavailable in tests")

    # Deterministic default: roster-gate tests that fall through to the
    # invited-recipient check must not hit a live Matrix homeserver.
    monkeypatch.setattr(server, "_matrix_room_member_user_ids", no_matrix_membership)
    return calls


# --- action drivers --------------------------------------------------------


def _delegate(workspace: Path, assigned_to: str) -> dict[str, Any]:
    arguments = {
        "role": "leader",
        "action": "delegate_task",
        "workspaceDir": str(workspace),
        "payload": {
            "projectId": PROJECT_ID,
            "taskId": TASK_ID,
            "roomId": ROOM,
            "assignedTo": assigned_to,
            "title": "C2 task",
            "spec": "Do the thing.",
        },
    }
    return json.loads(server.call_tool("taskflow", arguments)["content"][0]["text"])


def _register(
    workspace: Path,
    *,
    task_id: str = TASK_ID,
    delegation_id: str | None = None,
    project_id: str = PROJECT_ID,
) -> dict[str, Any]:
    payload: dict[str, Any] = {"taskId": task_id}
    if project_id:
        payload["projectId"] = project_id
    if delegation_id:
        payload["delegationId"] = delegation_id
    arguments = {
        "role": "worker",
        "action": "register_delegated_task",
        "workspaceDir": str(workspace),
        "payload": payload,
    }
    return json.loads(server.call_tool("taskflow", arguments)["content"][0]["text"])


# --- 1-4: delegate_task roster gate ----------------------------------------


def test_delegate_to_roster_mxid_succeeds(
    tmp_path: Path,
    side_effects: dict[str, list[Any]],
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    _write_project(tmp_path)
    config = _write_roster(
        tmp_path,
        [
            {"role": "worker", "name": "alice", "matrixUserId": WORKER_A},
            {"role": "worker", "name": "bob", "matrixUserId": WORKER_B},
        ],
    )
    monkeypatch.setenv("TEAMHARNESS_RUNTIME_CONFIG", str(config))

    result = _delegate(tmp_path, WORKER_A)

    assert result["ok"] is True
    assert result["task"]["status"] == "assigned"
    assert result["task"]["assigned_to"] == WORKER_A
    assert len(side_effects["notify"]) == 1
    assert side_effects["notify"][0]["assignee"] == WORKER_A
    meta = server._read_json(_task_meta_path(tmp_path))
    assert meta["assigned_to"] == WORKER_A


def test_delegate_unknown_short_name_fails_closed(
    tmp_path: Path,
    side_effects: dict[str, list[Any]],
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    _write_project(tmp_path)
    config = _write_roster(
        tmp_path,
        [{"role": "worker", "name": "alice", "matrixUserId": WORKER_A}],
    )
    monkeypatch.setenv("TEAMHARNESS_RUNTIME_CONFIG", str(config))

    result = _delegate(tmp_path, "worker-a")

    assert result["ok"] is False
    assert "unknown recipient: worker-a" in result["error"]
    assert "available:" in result["error"]
    assert WORKER_A in result["error"]
    # Fail closed before any write: no task meta, no notification, and the
    # project node was not advanced either.
    assert not _task_meta_path(tmp_path).exists()
    assert side_effects["notify"] == []
    project = server._read_json(tmp_path / "shared" / "projects" / PROJECT_ID / "meta.json")
    entry = next(t for t in project["tasks"] if t.get("task_id") == TASK_ID)
    assert entry["status"] == "planned"
    assert "assigned_to" not in entry


def test_delegate_invited_but_not_joined_is_retryable(
    tmp_path: Path,
    side_effects: dict[str, list[Any]],
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """A recipient the room has invited (membership=invite) gets the same
    retryable "not a joined member" failure as the membership gate: no
    notification, no assignment, project node not advanced."""
    _write_project(tmp_path)
    config = _write_roster(
        tmp_path,
        [{"role": "worker", "name": "alice", "matrixUserId": WORKER_A}],
    )
    monkeypatch.setenv("TEAMHARNESS_RUNTIME_CONFIG", str(config))
    invited = "@worker-invited:example.test"
    monkeypatch.setattr(
        server,
        "_matrix_room_member_user_ids",
        lambda room_id, memberships=None: (
            [invited] if memberships == {"invite"} else [WORKER_A, ADMIN]
        ),
    )

    result = _delegate(tmp_path, invited)

    assert result["ok"] is False
    assert result["retryable"] is True
    assert (
        f"assignee {invited} is not a joined member of room {ROOM}" in result["error"]
    )
    # Fail closed before any write: no notification, no task meta, and the
    # project node was not advanced either.
    assert side_effects["notify"] == []
    assert not _task_meta_path(tmp_path).exists()
    project = server._read_json(tmp_path / "shared" / "projects" / PROJECT_ID / "meta.json")
    entry = next(t for t in project["tasks"] if t.get("task_id") == TASK_ID)
    assert entry["status"] == "planned"
    assert "assigned_to" not in entry


def test_delegate_unknown_recipient_not_in_room_stays_non_retryable(
    tmp_path: Path,
    side_effects: dict[str, list[Any]],
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """A recipient nobody knows (not in the roster and not invited to the
    room) keeps the fail-closed unknown-recipient error: not retryable."""
    _write_project(tmp_path)
    config = _write_roster(
        tmp_path,
        [{"role": "worker", "name": "alice", "matrixUserId": WORKER_A}],
    )
    monkeypatch.setenv("TEAMHARNESS_RUNTIME_CONFIG", str(config))
    monkeypatch.setattr(
        server,
        "_matrix_room_member_user_ids",
        lambda room_id, memberships=None: [WORKER_A, ADMIN],
    )

    result = _delegate(tmp_path, "@stranger:example.test")

    assert result["ok"] is False
    assert result.get("retryable") is not True
    assert "unknown recipient: @stranger:example.test" in result["error"]
    assert "available:" in result["error"]
    assert side_effects["notify"] == []
    assert not _task_meta_path(tmp_path).exists()


def test_delegate_without_roster_keeps_legacy_passthrough(
    tmp_path: Path,
    side_effects: dict[str, list[Any]],
) -> None:
    """Standalone runs (no roster in the runtime config) keep the legacy
    pass-through behavior: the recipient is recorded as-is."""
    _write_project(tmp_path)

    result = _delegate(tmp_path, "worker-a")

    assert result["ok"] is True
    assert result["task"]["assigned_to"] == "worker-a"
    assert len(side_effects["notify"]) == 1
    assert side_effects["notify"][0]["assignee"] == "worker-a"


def test_delegate_short_name_matching_member_name_resolves_to_mxid(
    tmp_path: Path,
    side_effects: dict[str, list[Any]],
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    _write_project(tmp_path)
    config = _write_roster(
        tmp_path,
        [{"role": "worker", "name": "alice", "matrixUserId": WORKER_A}],
    )
    monkeypatch.setenv("TEAMHARNESS_RUNTIME_CONFIG", str(config))

    result = _delegate(tmp_path, "alice")

    assert result["ok"] is True
    assert result["task"]["status"] == "assigned"
    assert result["task"]["assigned_to"] == WORKER_A
    assert len(side_effects["notify"]) == 1
    assert side_effects["notify"][0]["assignee"] == WORKER_A
    meta = server._read_json(_task_meta_path(tmp_path))
    assert meta["assigned_to"] == WORKER_A


# --- 5-8: register_delegated_task ------------------------------------------


def test_register_delegated_task_first_call(
    tmp_path: Path,
    side_effects: dict[str, list[Any]],
) -> None:
    _write_project(tmp_path, task_status="assigned")
    _write_task_meta(tmp_path, _assigned_task_meta())

    result = _register(tmp_path)

    assert result["ok"] is True
    assert result["taskId"] == TASK_ID
    assert result["registered"] is True
    assert result["alreadyRegistered"] is False
    assert result["ackReceivedAt"]
    meta = server._read_json(_task_meta_path(tmp_path))
    assert meta["ack_received_at"] == result["ackReceivedAt"]
    assert meta["delegation_id"] == f"delegate-{TASK_ID}"


def test_register_delegated_task_is_idempotent(
    tmp_path: Path,
    side_effects: dict[str, list[Any]],
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    _write_project(tmp_path, task_status="assigned")
    _write_task_meta(tmp_path, _assigned_task_meta())
    clock = {"ts": "2026-11-02T01:00:00Z"}
    monkeypatch.setattr(server.taskflow, "_utc_timestamp", lambda: clock["ts"])

    first = _register(tmp_path)
    clock["ts"] = "2026-11-02T01:00:05Z"
    second = _register(tmp_path, delegation_id="delegate-custom")

    assert first["alreadyRegistered"] is False
    assert first["ackReceivedAt"] == "2026-11-02T01:00:00Z"
    assert second["alreadyRegistered"] is True
    assert second["registered"] is True
    # The repeat only refreshes the timestamp; the first delegation link
    # stands.
    assert second["ackReceivedAt"] == "2026-11-02T01:00:05Z"
    meta = server._read_json(_task_meta_path(tmp_path))
    assert meta["ack_received_at"] == "2026-11-02T01:00:05Z"
    assert meta["delegation_id"] == f"delegate-{TASK_ID}"
    # Still one task record — no duplicate registration is appended.
    assert isinstance(meta, dict) and meta["task_id"] == TASK_ID


def test_register_delegated_task_unknown_task(
    tmp_path: Path,
    side_effects: dict[str, list[Any]],
) -> None:
    _write_project(tmp_path)

    result = _register(tmp_path)

    assert result["ok"] is False
    assert result["reason"] == "unknown_task"
    assert not _task_meta_path(tmp_path).exists()


def test_register_delegated_task_preserves_status_and_emits_no_events(
    tmp_path: Path,
    side_effects: dict[str, list[Any]],
) -> None:
    _write_project(tmp_path, task_status="assigned")
    _write_task_meta(tmp_path, _assigned_task_meta())
    events_before = _event_count(tmp_path)

    result = _register(tmp_path)

    assert result["ok"] is True
    meta = server._read_json(_task_meta_path(tmp_path))
    assert meta["status"] == "assigned"
    assert _event_count(tmp_path) == events_before == 0
