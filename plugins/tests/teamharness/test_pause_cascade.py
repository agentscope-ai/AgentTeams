"""INT-7b: the project pause/resume cascade into the plan's loop dict.

The Go control plane owns the Loop CRD; the MCP side only observes.
pause_project / resume_project mark ``loop.paused_by_project`` on the
project's loop record so the worker's next loop tick can see the parent
project state. Observation is best-effort and idempotent:

- no loop dict on the project -> nothing is created
- value unchanged -> no state/plan rewrite (idempotent)
- value changed   -> in-memory update + best-effort state/plan rewrite
"""

from __future__ import annotations

import importlib.util
import json
from pathlib import Path
import sys

REPO_ROOT = Path(__file__).resolve().parents[3]
MCP_DIR = REPO_ROOT / "plugins" / "teamharness" / "mcp"


def _load_server() -> object:
    spec = importlib.util.spec_from_file_location(
        "teamharness_mcp_server_pause_cascade_test",
        MCP_DIR / "server.py",
    )
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.path.insert(0, str(MCP_DIR))
    try:
        spec.loader.exec_module(module)
    finally:
        sys.path.remove(str(MCP_DIR))
    return module


def _arguments(workspace: Path, action: str, **payload) -> dict:
    return {
        "workspaceDir": str(workspace),
        "action": action,
        "payload": payload,
    }


def _write_project(workspace: Path, project_id: str, meta: dict) -> None:
    path = workspace / "shared" / "projects" / project_id / "meta.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(meta, ensure_ascii=False), encoding="utf-8")


class FakeFilesync:
    """Records filesync calls; every action succeeds."""

    def __init__(self) -> None:
        self.calls: list[dict] = []

    def __call__(self, arguments: dict):
        self.calls.append(dict(arguments))
        return {
            "ok": True,
            "tool": "filesync",
            "action": arguments.get("action"),
            "path": arguments.get("path"),
        }


def _plan_write_spy(server: object, monkeypatch) -> list:
    """Count _write_project_plan invocations while keeping real behavior."""
    writes: list = []
    real = server._write_project_plan

    def spy(project_dir, project):
        writes.append(1)
        real(project_dir, project)

    monkeypatch.setattr(server, "_write_project_plan", spy)
    return writes


def _loop_meta(status: str = "active", loop: dict | None = None) -> dict:
    meta: dict = {
        "project_id": "p1",
        "title": "P1",
        "status": status,
        "plan_type": "loop" if loop is not None else "dag",
        "tasks": [],
    }
    if loop is not None:
        meta["loop"] = loop
    return meta


def test_pause_project_cascades_flag_to_loop_dict(tmp_path: Path, monkeypatch) -> None:
    server = _load_server()
    fake = FakeFilesync()
    monkeypatch.setattr(server, "_filesync", fake)
    writes = _plan_write_spy(server, monkeypatch)

    workspace = tmp_path / "agent"
    _write_project(workspace, "p1", _loop_meta("active", {
        "goal": "g", "stop_condition": "done", "max_iterations": 5,
        "current_iteration": 0, "status": "running",
    }))

    resp = server._projectflow(_arguments(workspace, "pause_project", projectId="p1"))
    assert resp.get("ok") is True
    assert resp.get("project", {}).get("status") == "paused"

    local = server._read_json(server._project_state_path(_arguments(workspace, "resolve_project"), "p1"))
    assert local["loop"]["paused_by_project"] is True

    # plan.md written once by the main flow + once by the observer
    # rewrite (the flag was set after the first write).
    assert len(writes) == 2


def test_resume_project_clears_flag(tmp_path: Path, monkeypatch) -> None:
    server = _load_server()
    fake = FakeFilesync()
    monkeypatch.setattr(server, "_filesync", fake)
    writes = _plan_write_spy(server, monkeypatch)

    workspace = tmp_path / "agent"
    _write_project(workspace, "p1", _loop_meta("paused", {
        "goal": "g", "stop_condition": "done", "max_iterations": 5,
        "current_iteration": 2, "status": "running", "paused_by_project": True,
    }))

    resp = server._projectflow(_arguments(workspace, "resume_project", projectId="p1"))
    assert resp.get("ok") is True
    assert resp.get("project", {}).get("status") == "active"

    local = server._read_json(server._project_state_path(_arguments(workspace, "resolve_project"), "p1"))
    assert local["loop"]["paused_by_project"] is False
    assert len(writes) == 2


def test_pause_without_loop_dict_creates_nothing(tmp_path: Path, monkeypatch) -> None:
    server = _load_server()
    fake = FakeFilesync()
    monkeypatch.setattr(server, "_filesync", fake)
    writes = _plan_write_spy(server, monkeypatch)

    workspace = tmp_path / "agent"
    _write_project(workspace, "p1", _loop_meta("active", None))

    resp = server._projectflow(_arguments(workspace, "pause_project", projectId="p1"))
    assert resp.get("ok") is True

    local = server._read_json(server._project_state_path(_arguments(workspace, "resolve_project"), "p1"))
    assert "loop" not in local or not isinstance(local.get("loop"), dict)
    # No observer rewrite: the main flow wrote the plan exactly once.
    assert len(writes) == 1


def test_pause_is_idempotent_no_double_rewrite(tmp_path: Path, monkeypatch) -> None:
    server = _load_server()
    fake = FakeFilesync()
    monkeypatch.setattr(server, "_filesync", fake)
    writes = _plan_write_spy(server, monkeypatch)

    workspace = tmp_path / "agent"
    _write_project(workspace, "p1", _loop_meta("active", {
        "goal": "g", "stop_condition": "done", "max_iterations": 5,
        "current_iteration": 0, "status": "running",
    }))

    first = server._projectflow(_arguments(workspace, "pause_project", projectId="p1"))
    assert first.get("ok") is True
    assert len(writes) == 2  # main flow + observer (value changed)

    # Second pause: the flag is already True -> the observer is a no-op
    # and the plan is written only by the main flow.
    second = server._projectflow(_arguments(workspace, "pause_project", projectId="p1"))
    assert second.get("ok") is True
    assert len(writes) == 3

    local = server._read_json(server._project_state_path(_arguments(workspace, "resolve_project"), "p1"))
    assert local["loop"]["paused_by_project"] is True
