# -*- coding: utf-8 -*-
"""Execution-side pause gate for task dispatch / loop progression (#1431).

PR #1402 wrote a pause flag but nothing on the execution path consumed it.
These tests exercise the REAL teamharness dispatch path: a paused
project/loop must reject new dispatch and release no ready nodes, while
in-flight work (submit/ack) is not gated, and resume restores dispatch.
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
        "teamharness_mcp_server_pause_gate_test", SERVER_PATH
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


def _write(workspace: Path, rel: str, value: Any) -> None:
    path = workspace / rel
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2), encoding="utf-8")


def _project(workspace: Path, loop: dict[str, Any] | None = None, status: str = "active") -> None:
    project = {
        "project_id": "p1",
        "title": "P1",
        "status": status,
        "tasks": [
            {"task_id": "t1", "status": "planned", "assigned_to": "@worker:mx"},
        ],
    }
    if loop is not None:
        project["plan_type"] = "loop"
        project["loop"] = loop
    _write(workspace, "shared/projects/p1/meta.json", project)
    _write(
        workspace,
        "shared/tasks/t1/meta.json",
        {"task_id": "t1", "project_id": "p1", "status": "planned"},
    )


def _setup(server: Any, tmp_path: Path, monkeypatch: Any, loop=None, status="active") -> tuple[Path, str]:
    workspace = tmp_path / "agent"
    _project(workspace, loop=loop, status=status)
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "leader")
    monkeypatch.setattr(server, "_filesync", lambda *a, **k: {"ok": True})
    monkeypatch.setattr(server, "_sync_project", lambda *a, **k: True)
    return workspace, "p1"


def _delegate(server: Any, workspace: Path) -> dict[str, Any]:
    return server._taskflow(
        _arguments(
            workspace,
            "delegate_task",
            projectId="p1",
            taskId="t1",
            assignedTo="@worker:mx",
            roomId="!room:mx",
        )
    )


def test_delegate_blocked_by_project_source(server, tmp_path, monkeypatch):
    workspace, _ = _setup(server, tmp_path, monkeypatch, loop={"pause_sources": ["project"]})
    result = _delegate(server, workspace)
    assert result["ok"] is False
    assert result["error"] == "paused"
    assert result["pause_sources"] == ["project"]
    # No mutation: the task never left "planned".
    task = json.loads((workspace / "shared/tasks/t1/meta.json").read_text())
    assert task["status"] == "planned"


def test_delegate_blocked_by_user_source(server, tmp_path, monkeypatch):
    workspace, _ = _setup(server, tmp_path, monkeypatch, loop={"pause_sources": ["user"]})
    result = _delegate(server, workspace)
    assert result["ok"] is False and result["error"] == "paused"


def test_delegate_blocked_by_legacy_project_flag(server, tmp_path, monkeypatch):
    workspace, _ = _setup(
        server, tmp_path, monkeypatch, loop={"paused_by_project": True}
    )
    result = _delegate(server, workspace)
    assert result["ok"] is False and result["error"] == "paused"


def test_delegate_blocked_by_paused_project_status(server, tmp_path, monkeypatch):
    workspace, _ = _setup(server, tmp_path, monkeypatch, status="paused")
    result = _delegate(server, workspace)
    assert result["ok"] is False and result["error"] == "paused"
    assert "project" in result["pause_sources"]


def test_ready_nodes_empty_while_paused(server, tmp_path, monkeypatch):
    workspace, _ = _setup(server, tmp_path, monkeypatch, loop={"pause_sources": ["user"]})
    result = server._projectflow(
        _arguments(workspace, "resolve_project", projectId="p1")
    )
    assert result["ok"] is True
    assert result["readyNodes"] == []


def test_submit_is_not_gated_while_paused(server, tmp_path, monkeypatch):
    """In-flight work must finish and persist while paused (#1431)."""
    workspace, _ = _setup(server, tmp_path, monkeypatch, loop={"pause_sources": ["user"]})
    _write(
        workspace,
        "shared/tasks/t1/meta.json",
        {"task_id": "t1", "project_id": "p1", "status": "in_progress"},
    )
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "worker")
    result = server._taskflow(
        _arguments(
            workspace,
            "submit_task",
            taskId="t1",
            status="SUCCESS",
            summary="done",
        )
    )
    assert result.get("error") != "paused"


def test_resume_clears_gate(server, tmp_path, monkeypatch):
    workspace, _ = _setup(server, tmp_path, monkeypatch, loop={"pause_sources": []})
    # not paused -> the gate does not fire (whatever else the fixture lacks)
    result = _delegate(server, workspace)
    assert result.get("error") != "paused"


def test_observer_preserves_other_sources(server, tmp_path, monkeypatch):
    """Project pause adds/removes only its own source, never the user's."""
    project = {"project_id": "p1", "loop": {"pause_sources": ["user"]}}
    monkeypatch.setattr(server, "_write_json", lambda *a, **k: None)
    monkeypatch.setattr(server, "_write_project_plan", lambda *a, **k: None)
    server._observe_project_pause({}, "p1", project, paused_by_project=True)
    assert project["loop"]["pause_sources"] == ["user", "project"]
    server._observe_project_pause({}, "p1", project, paused_by_project=False)
    assert project["loop"]["pause_sources"] == ["user"]
