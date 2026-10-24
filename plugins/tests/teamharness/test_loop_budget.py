"""Loop budget ledger tests for TeamHarness server.py (INT-9 dual ledger,
Python side).

Covers project["loop"]["budget"] = {tier, iteration_budget, iterations_used,
tokens_used, over_budget}:

- No budget by default: absent/unrecognized tier and no explicit
  iteration_budget → iteration_budget stays None and over_budget is never
  set, while iterations_used / tokens_used still accumulate
- Explicit tier → mapping simple=3 / standard=5 / full=10
- Explicit iteration_budget (project meta) wins over the tier mapping
- Over budget is flagged (over_budget + provenance), never truncated
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
        "teamharness_mcp_server_budget_test",
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
    """Fake for server._filesync: pulls that match a pre-seeded remote file
    write the remote content locally; everything else is recorded and
    returns ok=True."""

    def __init__(self, remote_files: dict[str, dict] | None = None) -> None:
        self.remote_files = remote_files or {}
        self.calls: list[dict] = []

    def __call__(self, arguments: dict):
        self.calls.append(dict(arguments))
        action = arguments.get("action")
        path = str(arguments.get("path") or "").rstrip("/")
        if action == "pull" and path in self.remote_files:
            workspace = Path(arguments.get("workspaceDir") or "")
            local = workspace / path
            local.parent.mkdir(parents=True, exist_ok=True)
            local.write_text(json.dumps(self.remote_files[path], ensure_ascii=False), encoding="utf-8")
        return {"ok": True, "tool": "filesync", "action": action, "path": path}


def _loop_of(server: object, workspace: Path, project_id: str) -> dict:
    project = server._read_json(server._project_state_path(_arguments(workspace, "resolve_project"), project_id))
    return project.get("loop", {})


def test_plan_loop_no_budget_by_default(tmp_path: Path, monkeypatch) -> None:
    """No tier and no explicit iteration_budget in the project meta → no
    budget: iteration_budget stays None, over_budget is never set, and the
    usage counters still accumulate on record_loop_iteration."""
    server = _load_server()
    monkeypatch.setattr(server, "_filesync", FakeFilesync())

    workspace = tmp_path / "agent"
    _write_project(workspace, "p1", {"project_id": "p1", "title": "P1", "status": "active", "tasks": []})

    result = server._projectflow(_arguments(
        workspace, "plan_loop", projectId="p1", maxIterations=10,
        goal="g", stopCondition="done", iterationTemplate="t",
    ))
    assert result.get("ok") is True
    loop = result["loop"]
    assert loop["budget"] == {
        "tier": None,
        "iteration_budget": None,
        "iterations_used": 0,
        "tokens_used": 0,
        "over_budget": False,
    }
    assert loop["provenance"] == []

    for iteration in (1, 2):
        result = server._projectflow(_arguments(
            workspace, "record_loop_iteration", projectId="p1",
            iteration=iteration, decision="continue", summary=f"s{iteration}",
        ))
        assert result.get("ok") is True

    loop = result["loop"]
    budget = loop["budget"]
    assert budget["tier"] is None
    assert budget["iteration_budget"] is None
    assert budget["iterations_used"] == 2
    assert budget["tokens_used"] == 0
    assert budget["over_budget"] is False
    assert len(loop["history"]) == 2
    assert len(loop["provenance"]) == 2
    assert loop["provenance"][-1] == {
        "iteration": 2, "iterations_used": 2, "iteration_budget": None, "over_budget": False,
    }


def test_plan_loop_explicit_tier_mapping(tmp_path: Path, monkeypatch) -> None:
    """An explicitly declared tier maps to its iteration budget
    (simple=3 / standard=5 / full=10); an unrecognized tier gives no
    budget instead of falling back to a default tier."""
    server = _load_server()
    monkeypatch.setattr(server, "_filesync", FakeFilesync())

    workspace = tmp_path / "agent"
    for i, (tier, expected) in enumerate(
        (("simple", 3), ("standard", 5), ("full", 10), ("weird", None)),
        start=1,
    ):
        project_id = f"p{i}"
        _write_project(workspace, project_id, {
            "project_id": project_id, "title": f"P{i}", "status": "active",
            "tasks": [], "tier": tier,
        })
        result = server._projectflow(_arguments(
            workspace, "plan_loop", projectId=project_id, maxIterations=10,
            goal="g", stopCondition="done", iterationTemplate="t",
        ))
        assert result.get("ok") is True
        budget = result["loop"]["budget"]
        assert budget["iteration_budget"] == expected
        assert budget["over_budget"] is False
        assert budget["iterations_used"] == 0
        assert budget["tier"] == tier if expected is not None else budget["tier"] is None


def test_plan_loop_explicit_iteration_budget_wins(tmp_path: Path, monkeypatch) -> None:
    """An explicit iteration_budget in the project meta wins over the tier
    mapping it sits next to, and on its own (without a tier) it enables a
    budget."""
    server = _load_server()
    monkeypatch.setattr(server, "_filesync", FakeFilesync())

    workspace = tmp_path / "agent"
    _write_project(workspace, "p1", {
        "project_id": "p1", "title": "P1", "status": "active",
        "tasks": [], "tier": "simple", "iteration_budget": 7,
    })
    result = server._projectflow(_arguments(
        workspace, "plan_loop", projectId="p1", maxIterations=10,
        goal="g", stopCondition="done", iterationTemplate="t",
    ))
    assert result.get("ok") is True
    budget = result["loop"]["budget"]
    assert budget["tier"] == "simple"
    assert budget["iteration_budget"] == 7

    _write_project(workspace, "p2", {
        "project_id": "p2", "title": "P2", "status": "active",
        "tasks": [], "iteration_budget": 4,
    })
    result = server._projectflow(_arguments(
        workspace, "plan_loop", projectId="p2", maxIterations=10,
        goal="g", stopCondition="done", iterationTemplate="t",
    ))
    assert result.get("ok") is True
    budget = result["loop"]["budget"]
    assert budget["tier"] is None
    assert budget["iteration_budget"] == 4
    assert budget["over_budget"] is False


def test_loop_over_budget_flagged_not_truncated(tmp_path: Path, monkeypatch) -> None:
    """A pre-seeded tier (simple → budget 3) is honored: the 4th iteration
    exceeds it. The loop keeps running (flag, don't truncate) and the
    provenance marks the over-budget state."""
    server = _load_server()
    monkeypatch.setattr(server, "_filesync", FakeFilesync())

    workspace = tmp_path / "agent"
    _write_project(workspace, "p1", {
        "project_id": "p1", "title": "P1", "status": "active", "tasks": [], "tier": "simple",
    })
    server._projectflow(_arguments(
        workspace, "plan_loop", projectId="p1", maxIterations=10,
        goal="g", stopCondition="done", iterationTemplate="t",
    ))

    for iteration in (1, 2, 3):
        server._projectflow(_arguments(
            workspace, "record_loop_iteration", projectId="p1",
            iteration=iteration, decision="continue", summary=f"s{iteration}",
        ))
        loop = _loop_of(server, workspace, "p1")
        assert loop["budget"]["iterations_used"] == iteration
        assert loop["budget"]["over_budget"] is False

    result = server._projectflow(_arguments(
        workspace, "record_loop_iteration", projectId="p1",
        iteration=4, decision="continue", summary="s4",
    ))
    assert result.get("ok") is True
    loop = result["loop"]
    assert loop["status"] == "running"
    assert len(loop["history"]) == 4
    budget = loop["budget"]
    assert budget["tier"] == "simple"
    assert budget["iteration_budget"] == 3
    assert budget["iterations_used"] == 4
    assert budget["over_budget"] is True
    assert loop["provenance"][-1] == {
        "iteration": 4, "iterations_used": 4, "iteration_budget": 3, "over_budget": True,
    }
    assert len(loop["provenance"]) <= 5
