import json

import pytest

from copaw_worker.hooks.tools import taskflow as taskflow_tool
from copaw_worker.hooks.tools.projectflow import projectflow
from copaw_worker.hooks.tools.taskflow import taskflow


def _response_json(response):
    item = response.content[0]
    text = item.get("text") if isinstance(item, dict) else item.text
    return json.loads(text)


def _set_actor(monkeypatch, actor: str) -> None:
    monkeypatch.setenv("AGENTTEAMS_MATRIX_USER_ID", actor)


def _mock_sync(monkeypatch):
    from unittest.mock import MagicMock

    mock = MagicMock()
    monkeypatch.setattr(taskflow_tool, "create_sync", lambda: mock)
    return mock


def _write_runtime_config(worker_root, content: str) -> None:
    runtime_dir = worker_root / "runtime"
    runtime_dir.mkdir(parents=True)
    (runtime_dir / "runtime.yaml").write_text(content, encoding="utf-8")


def _write_agents_roster(worker_root, worker_name: str, matrix_id: str) -> None:
    (worker_root / "AGENTS.md").write_text(
        "- **Team Workers**:\n"
        f"  - {worker_name} ({matrix_id})\n",
        encoding="utf-8",
    )


def test_resolve_worker_matrix_id_from_runtime_name(tmp_path, monkeypatch):
    worker_root = tmp_path / "leader"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(worker_root / ".copaw"))
    _write_runtime_config(
        worker_root,
        "team:\n"
        "  members:\n"
        "    - name: Developer\n"
        "      runtimeName: team-dev\n"
        "      matrixUserId: '@team-dev:matrix.local'\n",
    )

    assert taskflow_tool._resolve_worker_matrix_id("team-dev") == (
        "@team-dev:matrix.local"
    )
    assert taskflow_tool._resolve_worker_matrix_id("Developer") == (
        "@team-dev:matrix.local"
    )


def test_runtime_roster_takes_precedence_over_stale_agents(tmp_path, monkeypatch):
    worker_root = tmp_path / "leader"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(worker_root / ".copaw"))
    _write_runtime_config(
        worker_root,
        "team:\n"
        "  members:\n"
        "    - runtimeName: team-dev\n"
        "      matrixUserId: '@team-dev:new.local'\n",
    )
    _write_agents_roster(worker_root, "team-dev", "@team-dev:stale.local")

    assert taskflow_tool._resolve_worker_matrix_id("team-dev") == (
        "@team-dev:new.local"
    )


def test_runtime_roster_does_not_fallback_for_unknown_member(tmp_path, monkeypatch):
    worker_root = tmp_path / "leader"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(worker_root / ".copaw"))
    _write_runtime_config(
        worker_root,
        "team:\n"
        "  members:\n"
        "    - runtimeName: team-qa\n"
        "      matrixUserId: '@team-qa:matrix.local'\n",
    )
    _write_agents_roster(worker_root, "team-dev", "@team-dev:stale.local")

    assert taskflow_tool._resolve_worker_matrix_id("team-dev") is None


def test_resolve_worker_matrix_id_falls_back_to_agents(tmp_path, monkeypatch):
    worker_root = tmp_path / "leader"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(worker_root / ".copaw"))
    _write_runtime_config(
        worker_root,
        "member:\n"
        "  role: team_leader\n"
        "team:\n"
        "  teamRoomId: '!team:matrix.local'\n",
    )
    _write_agents_roster(worker_root, "team-dev", "@team-dev:legacy.local")

    assert taskflow_tool._resolve_worker_matrix_id("team-dev") == (
        "@team-dev:legacy.local"
    )


@pytest.mark.asyncio
async def test_notify_task_assignment_normalizes_room_id_at_matrix_boundary(
    tmp_path, monkeypatch,
):
    """_notify_task_assignment normalizes room: targets before nio calls.

    The documented target format is ``room:!team-room:domain``, but
    matrix-nio's joined_members/room_send require the raw
    ``!team-room:domain``. This test exercises the real function (not a
    mock) and asserts the nio boundary receives the normalized room ID
    and the stable txn_id.
    """
    import nio

    from copaw_worker.hooks.tools import taskflow as taskflow_tool
    from copaw_worker.hooks.tools.taskflow import _notify_task_assignment
    from copaw_worker.task import DagTask

    calls: list[dict] = []

    class _FakeMembers:
        members = [
            type("M", (), {"user_id": "@worker-a:matrix.local"})(),
        ]

    class _FakeClient:
        def __init__(self, *a, **kw):
            pass

        def set_access_token(self, token):
            pass

        async def joined_members(self, room_id):
            calls.append({"op": "joined_members", "room_id": room_id})
            return _FakeMembers()

        async def room_send(self, room_id, message_type, content, **kw):
            calls.append(
                {
                    "op": "room_send",
                    "room_id": room_id,
                    "txn_id": kw.get("tx_id"),
                    "content": content,
                },
            )
            return type("R", (), {"event_id": "$evt1"})()

        async def close(self):
            pass

    monkeypatch.setattr(nio, "AsyncClient", _FakeClient)
    monkeypatch.setattr(
        taskflow_tool,
        "_resolve_worker_matrix_id",
        lambda worker: "@worker-a:matrix.local",
    )
    monkeypatch.setattr(
        "copaw_worker.hooks.tools.message._matrix_config_for_agent",
        lambda account_id: (
            "https://matrix.local",
            "token",
            "@leader:matrix.local",
        ),
    )

    task = DagTask(
        task_id="st-01",
        title="Do stuff",
        assigned_to="@worker-a:matrix.local",
    )
    result = await _notify_task_assignment(
        task=task,
        room_id="room:!team-room:matrix.local",
        spec="Do the work.",
        txn_id="delegate-st-01",
    )

    assert result["sent"] is True
    assert result["eventId"] == "$evt1"
    assert result["roomId"] == "!team-room:matrix.local"
    ops = [c["op"] for c in calls]
    assert ops == ["joined_members", "room_send"]
    # Both nio APIs received the RAW room id, not the room: prefix form.
    for call in calls:
        assert call["room_id"] == "!team-room:matrix.local"
    # Stable txn_id reached the send boundary.
    assert calls[1]["txn_id"] == "delegate-st-01"
    # m.mentions preserved in the sent content.
    assert calls[1]["content"]["m.mentions"] == {
        "user_ids": ["@worker-a:matrix.local"],
    }


@pytest.mark.asyncio
async def test_delegate_task_retry_reuses_event_id_and_skips_resend(
    tmp_path, monkeypatch,
):
    """A fully-assigned task (event_id present) is idempotent on retry.

    After a successful send+commit, re-running delegate_task returns the
    existing meta with reused=True and does not call the notification
    boundary again.
    """
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@lead:domain")
    mock = _mock_sync(monkeypatch)

    notify_calls = {"count": 0}

    async def counting_notify(**kwargs):
        notify_calls["count"] += 1
        return {
            "sent": True,
            "eventId": "$evt1",
            "roomId": kwargs.get("room_id", ""),
            "assignee": "@worker-a:domain",
        }

    monkeypatch.setattr(taskflow_tool, "_notify_task_assignment", counting_notify)

    await projectflow(
        action="create_project",
        payload={"projectId": "tp-01", "title": "Test"},
    )
    await projectflow(
        action="plan_dag",
        payload={
            "projectId": "tp-01",
            "tasks": [
                {
                    "taskId": "st-01",
                    "title": "Do stuff",
                    "assignedTo": "@worker-a:domain",
                    "dependsOn": [],
                },
            ],
        },
    )

    payload_dict = {
        "projectId": "tp-01",
        "taskId": "st-01",
        "roomId": "room:!team-room:domain",
        "spec": "Do the stuff.",
    }

    first = _response_json(
        await taskflow(action="delegate_task", payload=payload_dict),
    )
    assert first["ok"] is True
    assert first["task"]["status"] == "assigned"
    assert first["task"]["event_id"] == "$evt1"
    assert notify_calls["count"] == 1

    retry = _response_json(
        await taskflow(action="delegate_task", payload=payload_dict),
    )
    assert retry["ok"] is True
    assert retry["notification"]["reused"] is True
    assert retry["notification"]["eventId"] == "$evt1"
    assert retry["task"]["status"] == "assigned"
    # No second notification on idempotent retry.
    assert notify_calls["count"] == 1
    # Still pushes so remote state converges.
    assert mock.push_shared_path.call_count >= 1
