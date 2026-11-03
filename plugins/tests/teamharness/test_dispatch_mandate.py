# -*- coding: utf-8 -*-
"""TH-21 Mandate quadruple: dispatch anchored to the policy version.

delegate_task records ``dispatch = {policy_version, basis, actor, scope}``
on the task meta (the taskflow dispatch path). The assigned -> in_progress
resume (ack_task) revalidates the recorded policy version against the
current workspace governance state:

- policy moved since dispatch -> reject with a re-dispatch hint (the
  recorded mandate no longer covers this run; never a silent resume)
- unchanged policy -> proceed
- no dispatch mandate / version-0 baseline (pre-fence) -> legacy
  behavior, never rejected
"""

from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Any

import pytest


MCP_DIR = Path(__file__).resolve().parents[2] / "teamharness" / "mcp"
if str(MCP_DIR) not in sys.path:
    sys.path.insert(0, str(MCP_DIR))

import server  # noqa: E402


ROOM = "!taskroom:example.test"
WORKER = "@alice:example.test"


def _write_project(workspace: Path, project_id: str, task_id: str) -> None:
    project = {
        "project_id": project_id,
        "title": "Mandate project",
        "status": "active",
        "tasks": [
            {
                "task_id": task_id,
                "title": "Do the thing",
                "assigned_to": WORKER,
                "depends_on": [],
                "status": "planned",
            }
        ],
    }
    server._write_json(workspace / "shared" / "projects" / project_id / "meta.json", project)


def _write_task(workspace: Path, task_id: str, meta: dict[str, Any]) -> None:
    server._write_json(workspace / "shared" / "tasks" / task_id / "meta.json", meta)


def _read_task(workspace: Path, task_id: str) -> dict[str, Any]:
    return server._read_json(workspace / "shared" / "tasks" / task_id / "meta.json")


@pytest.fixture
def side_effects(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "http://matrix.example.test")
    monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "test-token")
    monkeypatch.setattr(server, "_pull_project", lambda *_a, **_k: True)
    monkeypatch.setattr(server, "_pull_task", lambda *_a, **_k: True)
    monkeypatch.setattr(server, "_sync_task", lambda *_a, **_k: True)
    monkeypatch.setattr(server, "_update_project_task", lambda *_a, **_k: True)
    monkeypatch.setattr(
        server, "_validate_assignee_membership", lambda *_a, **_k: {"ok": True}
    )
    monkeypatch.setattr(
        server,
        "_send_delegate_notification",
        lambda *_a, **_k: {
            "sent": True,
            "eventId": "$evt-mandate",
            "roomId": ROOM,
            "assignee": WORKER,
        },
    )


def _delegate(workspace: Path, project_id: str, task_id: str) -> dict[str, Any]:
    arguments = {
        "role": "leader",
        "action": "delegate_task",
        "workspaceDir": str(workspace),
        "payload": {
            "projectId": project_id,
            "taskId": task_id,
            "roomId": ROOM,
            "assignedTo": WORKER,
            "title": "Do the thing",
            "spec": "Do the thing.",
        },
    }
    response = server.call_tool("taskflow", arguments)
    return json.loads(response["content"][0]["text"])


def _ack(workspace: Path, task_id: str) -> dict[str, Any]:
    arguments = {
        "role": "worker",
        "action": "ack_task",
        "workspaceDir": str(workspace),
        "payload": {"taskId": task_id},
    }
    response = server.call_tool("taskflow", arguments)
    return json.loads(response["content"][0]["text"])


def test_delegate_persists_mandate_quadruple(tmp_path, side_effects):
    (tmp_path / "SOUL.md").write_text("governance v1", encoding="utf-8")
    _write_project(tmp_path, "mandate-p", "mandate-t")
    result = _delegate(tmp_path, "mandate-p", "mandate-t")
    assert result["ok"] is True, result
    dispatch = result["task"].get("dispatch")
    assert dispatch is not None, sorted(result["task"].keys())
    # The recorded version is the workspace governance state at dispatch.
    assert dispatch["policy_version"] == server.policy_effects.policy_fingerprint(tmp_path)
    assert dispatch["basis"]
    assert dispatch["actor"]
    assert dispatch["scope"]["task_id"] == "mandate-t"
    assert dispatch["scope"]["project_id"] == "mandate-p"
    assert dispatch["scope"]["assigned_to"] == WORKER
    # Persisted on the task meta, not just returned in the response:
    assert _read_task(tmp_path, "mandate-t")["dispatch"] == dispatch


def test_policy_change_blocks_resume_with_re_dispatch_hint(tmp_path, side_effects):
    (tmp_path / "SOUL.md").write_text("governance v1", encoding="utf-8")
    _write_project(tmp_path, "mandate-p", "mandate-t")
    delegated = _delegate(tmp_path, "mandate-p", "mandate-t")
    assert delegated["ok"] is True, delegated
    # Governance moves after dispatch:
    (tmp_path / "SOUL.md").write_text("governance v2", encoding="utf-8")
    acked = _ack(tmp_path, "mandate-t")
    assert acked["ok"] is False, acked
    assert "policy" in acked["error"]
    # The hint says how to recover: re-dispatch under the current policy.
    assert "delegate_task" in acked["error"]
    # The task was not resumed:
    assert _read_task(tmp_path, "mandate-t")["status"] == "assigned"


def test_unchanged_policy_allows_resume(tmp_path, side_effects):
    (tmp_path / "SOUL.md").write_text("governance v1", encoding="utf-8")
    _write_project(tmp_path, "mandate-p", "mandate-t")
    delegated = _delegate(tmp_path, "mandate-p", "mandate-t")
    assert delegated["ok"] is True, delegated
    acked = _ack(tmp_path, "mandate-t")
    assert acked["ok"] is True, acked
    assert _read_task(tmp_path, "mandate-t")["status"] == "in_progress"


def test_legacy_record_without_dispatch_is_compatible(tmp_path, side_effects):
    # A pre-TH-21 record (no dispatch mandate) never revalidates, whatever
    # the governance state is.
    (tmp_path / "SOUL.md").write_text("governance v1", encoding="utf-8")
    _write_project(tmp_path, "mandate-p", "mandate-t")
    _write_task(
        tmp_path,
        "mandate-t",
        {
            "task_id": "mandate-t",
            "project_id": "mandate-p",
            "room_id": ROOM,
            "status": "assigned",
            "assigned_to": WORKER,
            "eventId": "$evt-legacy",
        },
    )
    (tmp_path / "SOUL.md").write_text("governance v2", encoding="utf-8")
    acked = _ack(tmp_path, "mandate-t")
    assert acked["ok"] is True, acked
    assert _read_task(tmp_path, "mandate-t")["status"] == "in_progress"


def test_version_zero_baseline_is_never_rejected(tmp_path, side_effects):
    # The pre-fence baseline (no governance files at dispatch) records
    # policy_version 0 and is never rejected afterwards.
    _write_project(tmp_path, "mandate-p", "mandate-t")
    _write_task(
        tmp_path,
        "mandate-t",
        {
            "task_id": "mandate-t",
            "project_id": "mandate-p",
            "room_id": ROOM,
            "status": "assigned",
            "assigned_to": WORKER,
            "eventId": "$evt-zero",
            "dispatch": {
                "policy_version": 0,
                "basis": "taskflow.delegate_task",
                "actor": "leader:default",
                "scope": {
                    "task_id": "mandate-t",
                    "project_id": "mandate-p",
                    "assigned_to": WORKER,
                },
            },
        },
    )
    (tmp_path / "SOUL.md").write_text("governance appeared", encoding="utf-8")
    acked = _ack(tmp_path, "mandate-t")
    assert acked["ok"] is True, acked
    assert _read_task(tmp_path, "mandate-t")["status"] == "in_progress"
