"""Loop budget ledger tests for TeamHarness (idempotency / revision
semantics, #1406).

Covers project["loop"]["budget"] = {tier, iteration_budget,
iterations_used, tokens_used, over_budget} plus the per-iteration ledger
project["loop"]["iterations"]:

- No budget by default: absent/unrecognized tier and no explicit
  iteration_budget -> iteration_budget stays None and over_budget is
  never set, while iterations_used still accumulates
- Explicit tier -> mapping simple=3 / standard=5 / full=10
- Explicit iteration_budget (project meta) wins over the tier mapping
- Over budget is flagged (over_budget + provenance), never truncated
- Idempotent retry of an already-recorded iteration (identical
  decision/summary/nextAction): no state change, no history/provenance
  entry, no signal, no disk write
- Revision (same id, different content): revision bumps, the
  distinct-iteration count stays len(iterations)
- Out-of-order unrecorded iteration below current_iteration ->
  ValueError("iteration_below_current")
- Legacy loops (history only, duplicate ids) rebuild the ledger from
  history on the first record (self-heals the historical double count)
"""

from __future__ import annotations

import importlib.util
import json
import sys
from pathlib import Path
from typing import Any


REPO_ROOT = Path(__file__).resolve().parents[3]
MCP_DIR = REPO_ROOT / "plugins" / "teamharness" / "mcp"


def _load_server() -> Any:
    spec = importlib.util.spec_from_file_location(
        "teamharness_mcp_server_loop_budget_test", MCP_DIR / "server.py"
    )
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.path.insert(0, str(MCP_DIR))
    try:
        spec.loader.exec_module(module)
    finally:
        sys.path.remove(str(MCP_DIR))
    return module


class FakeFilesync:
    """Records _filesync calls instead of touching the network."""

    def __init__(self) -> None:
        self.calls: list[dict] = []

    def __call__(self, arguments: dict) -> dict:
        self.calls.append(dict(arguments))
        return {"ok": True, "tool": "filesync", "action": arguments.get("action")}


def _arguments(workspace: Path, action: str, **payload: Any) -> dict[str, Any]:
    return {"workspaceDir": str(workspace), "action": action, "payload": payload}


def _write_project(workspace: Path, project_id: str, meta: dict[str, Any]) -> None:
    path = workspace / "shared" / "projects" / project_id / "meta.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(meta, ensure_ascii=False), encoding="utf-8")


def _loop_of(server: Any, workspace: Path, project_id: str) -> dict[str, Any]:
    project = server._read_json(server._project_state_path(_arguments(workspace, "resolve_project"), project_id))
    return project.get("loop", {})


def _plan_loop(server: Any, workspace: Path, project_id: str, **extra: Any) -> dict[str, Any]:
    return server._projectflow(_arguments(
        workspace, "plan_loop", projectId=project_id,
        maxIterations=10, goal="g", stopCondition="done", iterationTemplate="t", **extra,
    ))


def _record(
    server: Any, workspace: Path, project_id: str, iteration: int,
    decision: str, summary: str, next_action: str = "",
) -> dict[str, Any]:
    return server._projectflow(_arguments(
        workspace, "record_loop_iteration", projectId=project_id,
        iteration=iteration, decision=decision, summary=summary, nextAction=next_action,
    ))


def _fresh_server(monkeypatch: Any) -> Any:
    server = _load_server()
    monkeypatch.setattr(server, "_filesync", FakeFilesync())
    monkeypatch.setattr(server, "_sync_project", lambda *a, **k: True)
    return server


def test_plan_loop_no_budget_by_default(tmp_path: Path, monkeypatch: Any) -> None:
    """No tier and no explicit iteration_budget in the project meta -> no
    budget: iteration_budget stays None, over_budget is never set, and the
    distinct-iteration counter still accumulates on record_loop_iteration."""
    server = _fresh_server(monkeypatch)
    workspace = tmp_path / "agent"
    _write_project(workspace, "p1", {"project_id": "p1", "title": "P1", "status": "active", "tasks": []})

    result = _plan_loop(server, workspace, "p1")
    assert result.get("ok") is True
    loop = result["loop"]
    assert loop["budget"] == {
        "tier": None,
        "iteration_budget": None,
        "iterations_used": 0,
        "tokens_used": 0,
        "over_budget": False,
    }
    assert loop["iterations"] == {}
    assert loop["provenance"] == []

    for iteration in (1, 2):
        result = _record(server, workspace, "p1", iteration, "continue", f"s{iteration}")
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
        "iteration": 2, "revision": 0, "iterations_used": 2,
        "iteration_budget": None, "over_budget": False,
    }
    # The ledger keys by distinct iteration id.
    assert set(loop["iterations"]) == {"1", "2"}


def test_plan_loop_explicit_tier_mapping(tmp_path: Path, monkeypatch: Any) -> None:
    """An explicitly declared tier maps to its iteration budget
    (simple=3 / standard=5 / full=10); an unrecognized tier gives no
    budget instead of falling back to a default tier."""
    server = _fresh_server(monkeypatch)
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
        result = _plan_loop(server, workspace, project_id)
        assert result.get("ok") is True
        budget = result["loop"]["budget"]
        assert budget["iteration_budget"] == expected
        assert budget["over_budget"] is False
        assert budget["iterations_used"] == 0
        assert budget["tier"] == (tier if expected is not None else None)


def test_plan_loop_explicit_iteration_budget_wins(tmp_path: Path, monkeypatch: Any) -> None:
    """An explicit iteration_budget in the project meta wins over the tier
    mapping it sits next to, and on its own (without a tier) it enables a
    budget."""
    server = _fresh_server(monkeypatch)
    workspace = tmp_path / "agent"
    _write_project(workspace, "p1", {
        "project_id": "p1", "title": "P1", "status": "active",
        "tasks": [], "tier": "simple", "iteration_budget": 7,
    })
    result = _plan_loop(server, workspace, "p1")
    assert result.get("ok") is True
    budget = result["loop"]["budget"]
    assert budget["tier"] == "simple"
    assert budget["iteration_budget"] == 7

    _write_project(workspace, "p2", {
        "project_id": "p2", "title": "P2", "status": "active",
        "tasks": [], "iteration_budget": 4,
    })
    result = _plan_loop(server, workspace, "p2")
    assert result.get("ok") is True
    budget = result["loop"]["budget"]
    assert budget["tier"] is None
    assert budget["iteration_budget"] == 4
    assert budget["over_budget"] is False


def test_loop_over_budget_flagged_not_truncated(tmp_path: Path, monkeypatch: Any) -> None:
    """A pre-seeded tier (simple -> budget 3) is honored: the 4th
    iteration exceeds it. The loop keeps running (flag, don't truncate)
    and the provenance marks the over-budget state."""
    server = _fresh_server(monkeypatch)
    workspace = tmp_path / "agent"
    _write_project(workspace, "p1", {
        "project_id": "p1", "title": "P1", "status": "active", "tasks": [], "tier": "simple",
    })
    _plan_loop(server, workspace, "p1")

    for iteration in (1, 2, 3):
        _record(server, workspace, "p1", iteration, "continue", f"s{iteration}")
        loop = _loop_of(server, workspace, "p1")
        assert loop["budget"]["iterations_used"] == iteration
        assert loop["budget"]["over_budget"] is False

    result = _record(server, workspace, "p1", 4, "continue", "s4")
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
        "iteration": 4, "revision": 0, "iterations_used": 4,
        "iteration_budget": 3, "over_budget": True,
    }
    assert len(loop["provenance"]) <= 5


def test_idempotent_retry_same_iteration_is_noop(tmp_path: Path, monkeypatch: Any) -> None:
    """budget=1, record iteration 1 twice with identical
    decision/summary/nextAction: the retry is a full no-op - one distinct
    iteration, no over-budget flag, one history entry, and
    idempotent:true on the second response (maintainer repro)."""
    server = _fresh_server(monkeypatch)
    workspace = tmp_path / "agent"
    _write_project(workspace, "p1", {
        "project_id": "p1", "title": "P1", "status": "active",
        "tasks": [], "iteration_budget": 1,
    })
    _plan_loop(server, workspace, "p1")

    first = _record(server, workspace, "p1", 1, "continue", "s1")
    assert first["ok"] is True
    assert first.get("idempotent") is not True

    second = _record(server, workspace, "p1", 1, "continue", "s1")
    assert second["ok"] is True
    assert second["idempotent"] is True
    assert second["revision"] == 0

    loop = second["loop"]
    assert loop["budget"]["iterations_used"] == 1
    assert loop["budget"]["iteration_budget"] == 1
    assert loop["budget"]["over_budget"] is False
    assert len(loop["history"]) == 1
    assert len(loop["provenance"]) == 1
    # Nothing was written on the retry.
    on_disk = _loop_of(server, workspace, "p1")
    assert len(on_disk["history"]) == 1
    assert on_disk["budget"]["iterations_used"] == 1


def test_revision_same_iteration_bumps_revision_not_count(tmp_path: Path, monkeypatch: Any) -> None:
    """Same iteration id with different content is a revision: the ledger
    entry's revision bumps to 1, the distinct-iteration count stays 1,
    and history/provenance record the revision."""
    server = _fresh_server(monkeypatch)
    workspace = tmp_path / "agent"
    _write_project(workspace, "p1", {
        "project_id": "p1", "title": "P1", "status": "active",
        "tasks": [], "iteration_budget": 1,
    })
    _plan_loop(server, workspace, "p1")

    first = _record(server, workspace, "p1", 1, "continue", "s1")
    assert first["ok"] is True
    assert first["loop"]["budget"]["iterations_used"] == 1

    second = _record(server, workspace, "p1", 1, "stop_success", "s1 done")
    assert second["ok"] is True
    assert second.get("idempotent") is not True
    loop = second["loop"]
    assert loop["iterations"]["1"]["revision"] == 1
    assert loop["iterations"]["1"]["decision"] == "stop_success"
    assert loop["budget"]["iterations_used"] == 1
    assert loop["budget"]["over_budget"] is False
    assert len(loop["history"]) == 2
    assert loop["history"][0]["revision"] == 0
    assert loop["history"][1]["revision"] == 1
    assert loop["provenance"][-1]["revision"] == 1
    assert loop["provenance"][-1]["iterations_used"] == 1


def test_distinct_iterations_count_toward_budget(tmp_path: Path, monkeypatch: Any) -> None:
    """budget=1: recording iteration 1 then 2 gives two distinct
    iterations -> over budget (flag, don't truncate)."""
    server = _fresh_server(monkeypatch)
    workspace = tmp_path / "agent"
    _write_project(workspace, "p1", {
        "project_id": "p1", "title": "P1", "status": "active",
        "tasks": [], "iteration_budget": 1,
    })
    _plan_loop(server, workspace, "p1")
    _record(server, workspace, "p1", 1, "continue", "s1")
    result = _record(server, workspace, "p1", 2, "continue", "s2")
    loop = result["loop"]
    assert loop["budget"]["iterations_used"] == 2
    assert loop["budget"]["over_budget"] is True
    assert loop["current_iteration"] == 2


def test_out_of_order_unrecorded_iteration_rejected(tmp_path: Path, monkeypatch: Any) -> None:
    """Recording iteration 2 first advances current_iteration; a later
    unrecorded iteration 1 is rejected (a *recorded* id below current
    would be a revision and is allowed)."""
    server = _fresh_server(monkeypatch)
    workspace = tmp_path / "agent"
    _write_project(workspace, "p1", {"project_id": "p1", "title": "P1", "status": "active", "tasks": []})
    _plan_loop(server, workspace, "p1")
    assert _record(server, workspace, "p1", 2, "continue", "s2")["ok"] is True
    result = _record(server, workspace, "p1", 1, "continue", "s1")
    assert result["ok"] is False
    assert result["error"] == "iteration_below_current"


def test_legacy_history_rebuilds_ledger(tmp_path: Path, monkeypatch: Any) -> None:
    """A pre-ledger loop (history with a duplicate id, no
    iterations/budget keys) rebuilds the ledger on the first record: the
    same id collapses to its last entry with revision = occurrences - 1,
    and iterations_used counts distinct iterations (self-healing the
    historical double count)."""
    server = _fresh_server(monkeypatch)
    workspace = tmp_path / "agent"
    loop = {
        "goal": "g", "stop_condition": "s", "max_iterations": 10,
        "current_iteration": 2, "status": "running", "tasks": [],
        "history": [
            {"iteration": 1, "decision": "continue", "summary": "a", "next_action": ""},
            {"iteration": 1, "decision": "replan", "summary": "b", "next_action": ""},
            {"iteration": 2, "decision": "replan", "summary": "c", "next_action": ""},
        ],
    }
    _write_project(workspace, "p1", {"project_id": "p1", "title": "P1", "status": "active",
                                      "plan_type": "loop", "loop": loop})
    result = _record(server, workspace, "p1", 3, "continue", "d")
    assert result["ok"] is True
    loop = result["loop"]
    assert set(loop["iterations"]) == {"1", "2", "3"}
    assert loop["iterations"]["1"]["decision"] == "replan"  # last entry wins
    assert loop["iterations"]["1"]["revision"] == 1  # two occurrences
    assert loop["iterations"]["2"]["revision"] == 0
    assert loop["iterations"]["3"]["revision"] == 0
    assert loop["budget"]["iterations_used"] == 3  # distinct, not 4 (3 legacy + 1 record)
    assert loop["budget"]["iteration_budget"] is None
    assert loop["budget"]["over_budget"] is False
    assert len(loop["history"]) == 4


def test_idempotent_retry_emits_no_second_signal(tmp_path: Path, monkeypatch: Any) -> None:
    """Monkeypatched _emit_signal_event on the flow module: the first
    record emits once, the idempotent retry emits nothing further, and a
    later revision emits again."""
    server = _fresh_server(monkeypatch)
    workspace = tmp_path / "agent"
    _write_project(workspace, "p1", {"project_id": "p1", "title": "P1", "status": "active", "tasks": []})
    _plan_loop(server, workspace, "p1")

    emissions: list[dict] = []

    def fake_emit(arguments: Any, source_event: Any, **kwargs: Any) -> None:
        emissions.append(kwargs)

    flow_module = server.flow
    monkeypatch.setattr(flow_module, "_emit_signal_event", fake_emit)

    assert _record(server, workspace, "p1", 1, "continue", "s1")["ok"] is True
    assert len(emissions) == 1

    second = _record(server, workspace, "p1", 1, "continue", "s1")
    assert second["idempotent"] is True
    assert len(emissions) == 1

    third = _record(server, workspace, "p1", 1, "replan", "s1 changed")
    assert third.get("idempotent") is not True
    assert len(emissions) == 2
