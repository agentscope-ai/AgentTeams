# -*- coding: utf-8 -*-
"""S0 + S4a of the request_attention UX line (#1394).

S0 — record fields (all optional, backward compatible): ``timeoutAt``,
``options[]``, ``resolvedVia`` / ``resolvedBy`` / ``resolveEvidence``.
S4a — notification outcome persistence: one ``notification_log`` entry per
logical attempt with ``room_accepted`` (Matrix room acceptance) and a
``channel_accepted`` that is never inferred from room acceptance.
"""

from __future__ import annotations

import importlib.util
import json
from pathlib import Path
import sys
from typing import Any

import pytest


REPO_ROOT = Path(__file__).resolve().parents[3]
MCP_DIR = REPO_ROOT / "plugins" / "teamharness" / "mcp"
SERVER_PATH = MCP_DIR / "server.py"


def _load_server() -> Any:
    spec = importlib.util.spec_from_file_location(
        "teamharness_mcp_server_attention_test", SERVER_PATH
    )
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.path.insert(0, str(MCP_DIR))
    try:
        spec.loader.exec_module(module)
    finally:
        sys.path.remove(str(MCP_DIR))
    return module


@pytest.fixture()
def server() -> Any:
    return _load_server()


def _arguments(workspace: Path, action: str, **payload: Any) -> dict[str, Any]:
    return {"workspaceDir": str(workspace), "action": action, "payload": payload}


def _write_task(workspace: Path, task_id: str, meta: dict[str, Any]) -> None:
    path = workspace / "shared" / "tasks" / task_id / "meta.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(meta, ensure_ascii=False, indent=2), encoding="utf-8")


def _prepare(server: Any, tmp_path: Path, monkeypatch: Any, *, notify=None) -> tuple[Path, str]:
    workspace = tmp_path / "agent"
    task_id = "attn-task-1"
    _write_task(
        workspace,
        task_id,
        {
            "task_id": task_id,
            "status": "in_progress",
            "project_id": "",
            "room_id": "!room:matrix.local",
        },
    )
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "worker")
    monkeypatch.setattr(server, "_filesync", lambda *a, **k: {"ok": True})
    monkeypatch.setattr(server, "_team_leader_matrix_id", lambda: "@lead:matrix.local")
    monkeypatch.setattr(
        server, "_validate_assignee_membership", lambda room, leader: {"ok": True}
    )
    monkeypatch.setattr(server, "_team_human_matrix_ids", lambda: ["@luo:matrix.local"])
    if notify is not None:
        monkeypatch.setattr(server, "_send_attention_notification", notify)
    return workspace, task_id


def test_record_carries_timeout_and_options(server, tmp_path, monkeypatch):
    def notify(arguments, **kwargs):
        return {"sent": True, "eventId": "$evt1", "roomId": "!room:matrix.local"}

    workspace, task_id = _prepare(server, tmp_path, monkeypatch, notify=notify)
    result = server._taskflow(
        _arguments(
            workspace,
            "request_attention",
            taskId=task_id,
            kind="decision",
            question="Pick a database",
            timeoutAt="2026-10-24T00:00:00Z",
            options=[
                {"label": "Postgres", "id": "pg"},
                {"label": "SQLite", "description": "local only"},
            ],
        )
    )
    assert result["ok"] is True
    record = result["task"]["attention"][-1]
    assert record["timeoutAt"] == "2026-10-24T00:00:00Z"
    assert [o["label"] for o in record["options"]] == ["Postgres", "SQLite"]
    assert record["options"][1]["description"] == "local only"


def test_notification_log_records_room_acceptance_only(server, tmp_path, monkeypatch):
    def notify(arguments, **kwargs):
        return {"sent": True, "eventId": "$evt1"}

    workspace, task_id = _prepare(server, tmp_path, monkeypatch, notify=notify)
    result = server._taskflow(
        _arguments(
            workspace, "request_attention", taskId=task_id, kind="approval",
            question="Approve?",
        )
    )
    entry = result["task"]["attention"][-1]["notification_log"][-1]
    assert entry["attempt"] == 1
    assert entry["room_accepted"] is True
    # Per-recipient receipt is not reported by the room adapter: it must
    # stay null, never be inferred from room acceptance.
    assert entry["channel_accepted"] is None
    assert entry["retries"] == []


def test_notification_log_records_failure(server, tmp_path, monkeypatch):
    def notify(arguments, **kwargs):
        return {"sent": False, "error": "Matrix API error: HTTP 403"}

    workspace, task_id = _prepare(server, tmp_path, monkeypatch, notify=notify)
    result = server._taskflow(
        _arguments(
            workspace, "request_attention", taskId=task_id, kind="approval",
            question="Approve?",
        )
    )
    entry = result["task"]["attention"][-1]["notification_log"][-1]
    assert entry["room_accepted"] is False
    assert "HTTP 403" in entry["error"]


def test_resolve_records_provenance(server, tmp_path, monkeypatch):
    def notify(arguments, **kwargs):
        return {"sent": True, "eventId": "$evt1"}

    workspace, task_id = _prepare(server, tmp_path, monkeypatch, notify=notify)
    server._taskflow(
        _arguments(
            workspace, "request_attention", taskId=task_id, kind="decision",
            question="Pick one",
        )
    )
    resolved = server._taskflow(
        _arguments(
            workspace,
            "request_attention",
            taskId=task_id,
            kind="decision",
            question="Pick one",
            resolved=True,
            resolvedVia="im_reply",
            resolvedBy="@luo:matrix.local",
            resolveEvidence={"messageId": "$m1", "note": "answered in room"},
        )
    )
    assert resolved["ok"] is True
    record = resolved["task"]["attention"][-1]
    assert record["resolved"] is True
    assert record["resolvedVia"] == "im_reply"
    assert record["resolvedBy"] == "@luo:matrix.local"
    assert record["resolveEvidence"]["messageId"] == "$m1"
    assert record["resolvedAt"]


def test_invalid_timeout_and_options_rejected(server, tmp_path, monkeypatch):
    workspace, task_id = _prepare(server, tmp_path, monkeypatch)

    def run(**payload):
        return server._taskflow(
            _arguments(
                workspace, "request_attention", taskId=task_id,
                kind="decision", question="Q", **payload,
            )
        )

    # _taskflow turns ValueError into a structured error response.
    bad_timeout = run(timeoutAt="tomorrow")
    assert bad_timeout["ok"] is False
    assert "ISO-8601" in bad_timeout["error"]

    bad_options = run(options=[])
    assert bad_options["ok"] is False
    assert "options" in bad_options["error"]

    bad_via = run(resolved=True, resolvedVia="telepathy")
    assert bad_via["ok"] is False
    assert "resolvedVia" in bad_via["error"]
