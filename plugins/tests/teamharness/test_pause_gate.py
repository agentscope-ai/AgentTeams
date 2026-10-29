"""#1402 refactor B: the Python-side pause gate (#1431 acceptance).

Once a pause takes effect: no new tasks are delegated, no downstream is
released, no next round opens; in-flight tasks can still ack/submit and
land their result. ``resume_project`` (agent-callable) only clears the
legacy ``status`` — ``loop.pause_sources`` is cleared by the Go API admin
action alone, an agent cannot self-resolve.

Authoritative state is ``loop.pause_sources`` in the project meta (written
by the Go side); the legacy ``project.status == "paused"`` stays
compatible and contributes the ``"project"`` source.

Covers (spec 3):
1. ``pause_sources=["user"]`` -> delegate_task rejected
2. ``pause_sources=["user","project"]`` -> delegate rejected, ack/submit succeed
3. ready_loop_nodes -> readyLoopNodes=[], paused=true, pauseSources=["user"]
4. plan_loop -> rejected
5. record_loop_iteration -> recorded but paused=true, readyLoopNodes=[]
6. legacy ``status="paused"`` (no sources) -> same effect as 1/3
7. resume_project (agent) -> sources remain, delegate still rejected;
   the Go-side clear of the sources unblocks delegation
8. pause persisted during membership validation (after the initial read,
   before the send) -> send suppressed, task stays prepared
"""

from __future__ import annotations

import importlib
import importlib.util
import json
import sys
from pathlib import Path
from typing import Any

import pytest


REPO_ROOT = Path(__file__).resolve().parents[3]
MCP_DIR = REPO_ROOT / "plugins" / "teamharness" / "mcp"

WORKER = "@worker-a:example.test"
ROOM = "!pause-gate:example.test"


def _load_server() -> Any:
    spec = importlib.util.spec_from_file_location(
        "teamharness_mcp_server_pause_gate_test",
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


@pytest.fixture()
def server() -> Any:
    return _load_server()


def _arguments(workspace: Path, action: str, **payload: Any) -> dict[str, Any]:
    return {"workspaceDir": str(workspace), "action": action, "payload": payload}


def _write_project(workspace: Path, project_id: str, meta: dict[str, Any]) -> None:
    path = workspace / "shared" / "projects" / project_id / "meta.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(meta, ensure_ascii=False), encoding="utf-8")


def _read_project(workspace: Path, project_id: str) -> dict[str, Any]:
    path = workspace / "shared" / "projects" / project_id / "meta.json"
    return json.loads(path.read_text(encoding="utf-8"))


def _write_task(workspace: Path, task_id: str, meta: dict[str, Any]) -> None:
    path = workspace / "shared" / "tasks" / task_id / "meta.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(meta, ensure_ascii=False), encoding="utf-8")


def _read_task(workspace: Path, task_id: str) -> dict[str, Any]:
    path = workspace / "shared" / "tasks" / task_id / "meta.json"
    return json.loads(path.read_text(encoding="utf-8"))


class FakeFilesync:
    """Records _filesync calls instead of touching shared storage."""

    def __init__(self) -> None:
        self.calls: list[dict[str, Any]] = []

    def __call__(self, arguments: dict[str, Any]) -> dict[str, Any]:
        self.calls.append(dict(arguments))
        return {
            "ok": True,
            "tool": "filesync",
            "action": arguments.get("action"),
            "path": arguments.get("path"),
        }


def _loop(
    pause_sources: list[str] | None = None,
    *,
    tasks: list[dict[str, Any]] | None = None,
    current_iteration: int = 0,
) -> dict[str, Any]:
    if tasks is None:
        tasks = [
            {
                "task_id": "pg-task-01",
                "title": "In-flight pass",
                "assigned_to": WORKER,
                "depends_on": [],
                "status": "in_progress",
            },
        ]
    loop: dict[str, Any] = {
        "goal": "iterate until done",
        "stop_condition": "all checks green",
        "iteration_template": "run the checks",
        "max_iterations": 3,
        "current_iteration": current_iteration,
        "status": "running",
        "tasks": tasks,
    }
    if pause_sources is not None:
        loop["pause_sources"] = pause_sources
    return loop


def _project(
    project_id: str, *, status: str, loop: dict[str, Any] | None = None
) -> dict[str, Any]:
    meta: dict[str, Any] = {
        "project_id": project_id,
        "title": f"Pause gate {project_id}",
        "status": status,
        "plan_type": "loop" if loop is not None else "dag",
        "tasks": [],
    }
    if loop is not None:
        meta["loop"] = loop
    return meta


def _delegate_args(workspace: Path, project_id: str, task_id: str) -> dict[str, Any]:
    return _arguments(
        workspace,
        "delegate_task",
        projectId=project_id,
        taskId=task_id,
        roomId=ROOM,
        assignedTo=WORKER,
    )


def _taskflow_module() -> Any:
    return importlib.import_module("mcp.lifecycle.taskflow")


def test_delegate_rejected_when_paused_via_sources(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 1: loop.pause_sources=["user"] blocks a new delegation."""
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "leader")
    workspace = tmp_path / "agent"
    _write_project(
        workspace, "pg-1", _project("pg-1", status="active", loop=_loop(pause_sources=["user"]))
    )

    result = server._taskflow(_delegate_args(workspace, "pg-1", "pg-1-next"))

    assert result["ok"] is False
    assert result["action"] == "delegate_task"
    assert result["error"] == "project is paused (sources: user)"


def test_inflight_ack_submit_succeed_while_delegate_blocked(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 2: ["user","project"] blocks delegation; the in-flight task
    still acks and submits and its result lands in the project node."""
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    monkeypatch.setattr(
        _taskflow_module(),
        "_task_completion_notification",
        lambda *args, **kwargs: {"sent": True, "eventId": "$pg-completion"},
    )
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "leader")
    workspace = tmp_path / "agent"
    task_id = "pg-2-inflight"
    loop_tasks = [
        {
            "task_id": task_id,
            "title": "In-flight pass",
            "assigned_to": WORKER,
            "depends_on": [],
            "status": "assigned",
        },
    ]
    _write_project(
        workspace,
        "pg-2",
        _project(
            "pg-2",
            status="active",
            loop=_loop(pause_sources=["user", "project"], tasks=loop_tasks),
        ),
    )
    _write_task(
        workspace,
        task_id,
        {
            "task_id": task_id,
            "project_id": "pg-2",
            "room_id": ROOM,
            "status": "assigned",
            "assigned_to": WORKER,
        },
    )

    # New dispatch is blocked; both sources are reported (sorted).
    delegate = server._taskflow(_delegate_args(workspace, "pg-2", "pg-2-next"))
    assert delegate["ok"] is False
    assert delegate["error"] == "project is paused (sources: project, user)"

    # The in-flight task still acks (assigned -> in_progress).
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "worker")
    ack = server._taskflow(_arguments(workspace, "ack_task", taskId=task_id))
    assert ack["ok"] is True
    assert ack["task"]["status"] == "in_progress"

    # ...and submits (in_progress -> submitted); the result lands.
    submit = server._taskflow(
        _arguments(workspace, "submit_task", taskId=task_id, status="SUCCESS", summary="done")
    )
    assert submit["ok"] is True
    assert submit["task"]["status"] == "submitted"
    meta = _read_project(workspace, "pg-2")
    assert meta["loop"]["tasks"][0]["status"] == "submitted"
    assert meta["loop"]["pause_sources"] == ["user", "project"]


def test_ready_loop_nodes_reports_paused(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 3: paused -> readyLoopNodes=[] with paused/pauseSources exposed.
    The second task is dependency-ready, so the empty list proves the gate."""
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    workspace = tmp_path / "agent"
    loop_tasks = [
        {
            "task_id": "pg-3-a",
            "title": "Done pass",
            "assigned_to": WORKER,
            "depends_on": [],
            "status": "completed",
        },
        {
            "task_id": "pg-3-b",
            "title": "Next pass",
            "assigned_to": WORKER,
            "depends_on": ["pg-3-a"],
            "status": "planned",
        },
    ]
    _write_project(
        workspace,
        "pg-3",
        _project("pg-3", status="active", loop=_loop(pause_sources=["user"], tasks=loop_tasks)),
    )

    result = server._projectflow(_arguments(workspace, "ready_loop_nodes", projectId="pg-3"))

    assert result["ok"] is True
    assert result["readyLoopNodes"] == []
    assert result["paused"] is True
    assert result["pauseSources"] == ["user"]


def test_plan_loop_rejected_when_paused(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 4: plan_loop is refused while paused and writes nothing."""
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    workspace = tmp_path / "agent"
    _write_project(
        workspace, "pg-4", _project("pg-4", status="active", loop=_loop(pause_sources=["user"]))
    )
    meta_before = _read_project(workspace, "pg-4")

    result = server._projectflow(
        _arguments(
            workspace,
            "plan_loop",
            projectId="pg-4",
            goal="iterate",
            stopCondition="done",
            iterationTemplate="run",
            maxIterations=3,
            tasks=[
                {
                    "taskId": "pg-4-x",
                    "title": "x",
                    "assignedTo": WORKER,
                    "dependsOn": [],
                    "status": "planned",
                },
            ],
        )
    )

    assert result["ok"] is False
    assert result["error"] == "project is paused (sources: user)"
    assert _read_project(workspace, "pg-4") == meta_before


def test_record_iteration_lands_but_releases_nothing_when_paused(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 5: the in-flight iteration is recorded, but nothing is released
    downstream and no next round opens (readyLoopNodes=[])."""
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    workspace = tmp_path / "agent"
    _write_project(
        workspace,
        "pg-5",
        _project("pg-5", status="active", loop=_loop(pause_sources=["user"], current_iteration=0)),
    )

    result = server._projectflow(
        _arguments(
            workspace,
            "record_loop_iteration",
            projectId="pg-5",
            iteration=1,
            decision="continue",
            summary="iteration one",
        )
    )

    assert result["ok"] is True
    assert result["paused"] is True
    assert result["pauseSources"] == ["user"]
    assert result["readyLoopNodes"] == []
    # The in-flight iteration still landed in the project state.
    meta = _read_project(workspace, "pg-5")
    assert meta["loop"]["current_iteration"] == 1
    # The recorded entry may carry newer additive fields (e.g. revision);
    # assert the fields this contract owns instead of exact-dict equality.
    entry = meta["loop"]["history"][0]
    for key, value in {
        "iteration": 1,
        "decision": "continue",
        "summary": "iteration one",
        "next_action": "",
    }.items():
        assert entry[key] == value
    assert meta["loop"]["pause_sources"] == ["user"]


def test_legacy_paused_status_gates_like_sources(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 6: legacy status="paused" (no pause_sources) gates delegation and
    ready nodes the same way, reporting the "project" source."""
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "leader")
    workspace = tmp_path / "agent"
    _write_project(workspace, "pg-6", _project("pg-6", status="paused", loop=_loop()))

    delegate = server._taskflow(_delegate_args(workspace, "pg-6", "pg-6-next"))
    assert delegate["ok"] is False
    assert delegate["error"] == "project is paused (sources: project)"

    ready = server._projectflow(_arguments(workspace, "ready_loop_nodes", projectId="pg-6"))
    assert ready["ok"] is True
    assert ready["readyLoopNodes"] == []
    assert ready["paused"] is True
    assert ready["pauseSources"] == ["project"]


def test_resume_keeps_sources_and_admin_clear_unblocks(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 7: the agent's resume_project clears only the legacy status;
    pause_sources remain and delegation stays blocked until the Go API admin
    action clears them."""
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "worker")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "http://matrix.example.test")
    monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "test-token")
    monkeypatch.setattr(
        server,
        "_send_delegate_notification",
        lambda *args, **kwargs: {
            "sent": True,
            "eventId": "$pg-fresh",
            "roomId": kwargs.get("room_id", ""),
            "assignee": kwargs.get("assignee", ""),
        },
    )
    monkeypatch.setattr(server, "_validate_assignee_membership", lambda *args, **kwargs: {"ok": True})
    workspace = tmp_path / "agent"
    _write_project(
        workspace,
        "pg-7",
        _project("pg-7", status="paused", loop=_loop(pause_sources=["user"])),
    )

    # The agent resume clears the legacy status only.
    resumed = server._projectflow(_arguments(workspace, "resume_project", projectId="pg-7"))
    assert resumed["ok"] is True
    meta = _read_project(workspace, "pg-7")
    assert meta["status"] == "active"
    assert meta["loop"]["pause_sources"] == ["user"]

    # ...so the project is still paused: delegation stays blocked.
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "leader")
    still = server._taskflow(_delegate_args(workspace, "pg-7", "pg-7-next"))
    assert still["ok"] is False
    assert still["error"] == "project is paused (sources: user)"

    # The Go API admin action clears the source -> dispatch unblocks.
    meta["loop"]["pause_sources"] = []
    _write_project(workspace, "pg-7", meta)
    freed = server._taskflow(
        _arguments(
            workspace,
            "delegate_task",
            projectId="pg-7",
            taskId="pg-7-next",
            roomId=ROOM,
            assignedTo=WORKER,
            title="Next round",
            spec="do it",
        )
    )
    assert freed["ok"] is True
    assert freed["task"]["status"] == "assigned"


def test_pause_during_membership_validation_suppresses_send(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """Spec 8: a pause persisted during membership validation - after
    delegate_task's initial pause read, before the send - is already
    effective at the pre-send re-check: the assignment is suppressed,
    the task stays prepared, and nothing is committed to assigned."""
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "leader")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "http://matrix.example.test")
    monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "test-token")
    workspace = tmp_path / "agent"
    _write_project(workspace, "pg-8", _project("pg-8", status="active", loop=_loop()))
    sent: list[dict[str, Any]] = []

    def _membership_with_pause(*args: Any, **kwargs: Any) -> dict[str, Any]:
        # Storage interleaving: the user pause lands while room membership
        # is being validated - after the initial pause read.
        meta = _read_project(workspace, "pg-8")
        meta["loop"]["pause_sources"] = ["user"]
        _write_project(workspace, "pg-8", meta)
        return {"ok": True}

    monkeypatch.setattr(server, "_validate_assignee_membership", _membership_with_pause)
    monkeypatch.setattr(
        server,
        "_send_delegate_notification",
        lambda *args, **kwargs: sent.append(dict(kwargs))
        or {
            "sent": True,
            "eventId": "$pg-never-sent",
            "roomId": kwargs.get("room_id", ""),
            "assignee": kwargs.get("assignee", ""),
        },
    )

    result = server._taskflow(_delegate_args(workspace, "pg-8", "pg-8-next"))

    # The send was suppressed and the failure matches the pre-check shape.
    assert sent == []
    assert result["ok"] is False
    assert result["action"] == "delegate_task"
    assert "paused" in result["error"]
    # The task stays prepared - it was never committed to assigned.
    task = _read_task(workspace, "pg-8-next")
    assert task["status"] == "prepared"
    # The project task state was not marked assigned either.
    meta = _read_project(workspace, "pg-8")
    nodes = meta["tasks"] + meta["loop"]["tasks"]
    assert not any(
        isinstance(item, dict) and item.get("task_id") == "pg-8-next" for item in nodes
    )
    assert all(item.get("status") != "assigned" for item in nodes)
