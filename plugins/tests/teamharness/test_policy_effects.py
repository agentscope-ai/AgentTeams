# -*- coding: utf-8 -*-
"""TH-19 policyEffects: governance change impact-surface computation.

A governance change set (changed paths) must map to the affected surface
— runtimes, MCP tool surface, and in-flight tasks — as a human/leader-
readable list. Acceptance:
- a change set -> affected list (compute_policy_effects)
- no impact (docs/tests/CI-only) -> empty lists, not a guess
- no false positives on in-flight tasks (a task is listed only when the
  runtime role it is waiting on is actually affected)

The policy fingerprint (the policy version consumed by the TH-21
dispatch fence) is tested here too: deterministic, clock-free, and 0 for
the pre-fence baseline (a workspace with no governance files).
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


def _write_task(workspace: Path, task_id: str, meta: dict[str, Any]) -> None:
    path = workspace / "shared" / "tasks" / task_id / "meta.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(meta, ensure_ascii=False, indent=2), encoding="utf-8")


def _seed_inflight_tasks(workspace: Path) -> None:
    # One task per status the classifier must decide on:
    _write_task(workspace, "t-worker-running", {"task_id": "t-worker-running", "status": "in_progress", "project_id": "p1"})
    _write_task(workspace, "t-worker-assigned", {"task_id": "t-worker-assigned", "status": "assigned", "project_id": "p1"})
    _write_task(workspace, "t-leader-decision", {"task_id": "t-leader-decision", "status": "submitted", "project_id": "p1"})
    # Not in flight yet — never listed, no matter what changes:
    _write_task(workspace, "t-planned", {"task_id": "t-planned", "status": "planned", "project_id": "p1"})
    _write_task(workspace, "t-prepared", {"task_id": "t-prepared", "status": "prepared", "project_id": "p1"})
    # Terminal — never listed:
    _write_task(workspace, "t-done", {"task_id": "t-done", "status": "completed", "project_id": "p1"})
    _write_task(workspace, "t-cancelled", {"task_id": "t-cancelled", "status": "cancelled", "project_id": "p1"})


class TestClassifyChange:
    def test_scoped_soul_names_one_runtime(self) -> None:
        hit = server.policy_effects.classify_change("worker/SOUL.md")
        assert hit is not None
        assert hit["area"] == "governance"
        assert hit["runtimes"] == ("worker",)
        assert hit["tools"] == ()

    def test_bare_soul_is_conservative(self) -> None:
        # A workspace-root SOUL.md with no role segment: the owning
        # runtime is not identifiable from the path, so the classifier
        # over-reports (all team roles) rather than under-reporting.
        hit = server.policy_effects.classify_change("SOUL.md")
        assert hit is not None
        assert hit["runtimes"] == server.policy_effects.TEAM_RUNTIME_ROLES

    def test_allowlist_changes_the_whole_tool_surface(self) -> None:
        hit = server.policy_effects.classify_change("worker/allowlist.json")
        assert hit is not None
        assert hit["runtimes"] == ("worker",)
        assert tuple(hit["tools"]) == tuple(server.policy_effects.TOOL_NAMES)

    def test_contract_affects_all_roles_and_contract_tools(self) -> None:
        hit = server.policy_effects.classify_change(
            "plugins/teamharness/contracts/signal-mapping.json"
        )
        assert hit is not None
        assert hit["area"] == "contract"
        assert hit["runtimes"] == server.policy_effects.TEAM_RUNTIME_ROLES
        assert sorted(hit["tools"]) == ["projectflow", "taskflow"]

    def test_plugin_code_maps_to_its_tool_surface(self) -> None:
        hit = server.policy_effects.classify_change(
            "plugins/teamharness/mcp/lifecycle/taskflow.py"
        )
        assert hit is not None
        assert hit["tools"] == ("taskflow",)
        hit2 = server.policy_effects.classify_change("plugins/teamharness/mcp/server.py")
        assert hit2 is not None
        assert tuple(hit2["tools"]) == tuple(server.policy_effects.TOOL_NAMES)

    def test_role_prompt_scopes_to_its_role(self) -> None:
        hit = server.policy_effects.classify_change("plugins/teamharness/prompts/leader/leader.md")
        assert hit is not None
        assert hit["runtimes"] == ("leader",)

    def test_docs_and_ci_carry_no_impact(self) -> None:
        for path in (
            "docs/design/teamharness/boundary-and-contracts.md",
            "README.md",
            ".github/ISSUE_TEMPLATE/bug_report.yml",
            "plugins/tests/teamharness/test_policy_effects.py",
        ):
            assert server.policy_effects.classify_change(path) is None, path


class TestPolicyFingerprint:
    def test_absent_baseline_is_zero(self, tmp_path: Path) -> None:
        assert server.policy_effects.policy_fingerprint(tmp_path) == 0

    def test_stable_while_unchanged(self, tmp_path: Path) -> None:
        (tmp_path / "SOUL.md").write_text("soul v1", encoding="utf-8")
        first = server.policy_effects.policy_fingerprint(tmp_path)
        assert first > 0
        assert server.policy_effects.policy_fingerprint(tmp_path) == first

    def test_modify_moves_the_version(self, tmp_path: Path) -> None:
        soul = tmp_path / "SOUL.md"
        soul.write_text("soul v1", encoding="utf-8")
        before = server.policy_effects.policy_fingerprint(tmp_path)
        soul.write_text("soul v2", encoding="utf-8")
        after = server.policy_effects.policy_fingerprint(tmp_path)
        assert before != after

    def test_add_and_remove_move_the_version(self, tmp_path: Path) -> None:
        (tmp_path / "SOUL.md").write_text("soul", encoding="utf-8")
        base = server.policy_effects.policy_fingerprint(tmp_path)
        (tmp_path / "allowlist.json").write_text("{}", encoding="utf-8")
        with_extra = server.policy_effects.policy_fingerprint(tmp_path)
        (tmp_path / "allowlist.json").unlink()
        restored = server.policy_effects.policy_fingerprint(tmp_path)
        assert base != with_extra
        assert restored == base


class TestComputeEffects:
    def test_empty_change_set_has_no_impact(self) -> None:
        effects = server.policy_effects.compute_policy_effects([])
        assert effects["affected_runtimes"] == []
        assert effects["tool_surface"] == []
        assert effects["in_flight_tasks"] == []
        assert "no impact" in effects["summary"]

    def test_docs_only_change_lists_nothing(self, tmp_path: Path) -> None:
        _seed_inflight_tasks(tmp_path)
        effects = server.policy_effects.compute_policy_effects(
            ["docs/design/teamharness/boundary-and-contracts.md"], workspace=tmp_path
        )
        assert effects["affected_runtimes"] == []
        assert effects["tool_surface"] == []
        # No false positives: in-flight tasks exist in the workspace but a
        # docs change must not list a single one of them.
        assert effects["in_flight_tasks"] == []

    def test_worker_soul_lists_only_worker_inflight_tasks(self, tmp_path: Path) -> None:
        _seed_inflight_tasks(tmp_path)
        effects = server.policy_effects.compute_policy_effects(
            ["worker/SOUL.md"], workspace=tmp_path
        )
        assert effects["affected_runtimes"] == ["worker"]
        assert sorted(t["task_id"] for t in effects["in_flight_tasks"]) == [
            "t-worker-assigned",
            "t-worker-running",
        ]
        for item in effects["in_flight_tasks"]:
            assert item["waiting_on"] == "worker"

    def test_leader_change_lists_only_leader_decision_tasks(self, tmp_path: Path) -> None:
        _seed_inflight_tasks(tmp_path)
        effects = server.policy_effects.compute_policy_effects(
            ["leader/SOUL.md"], workspace=tmp_path
        )
        assert effects["affected_runtimes"] == ["leader"]
        assert [t["task_id"] for t in effects["in_flight_tasks"]] == ["t-leader-decision"]
        assert effects["in_flight_tasks"][0]["status"] == "submitted"

    def test_contract_change_lists_every_inflight_task(self, tmp_path: Path) -> None:
        _seed_inflight_tasks(tmp_path)
        effects = server.policy_effects.compute_policy_effects(
            ["plugins/teamharness/contracts/task-transitions.json"], workspace=tmp_path
        )
        assert set(effects["affected_runtimes"]) == set(
            server.policy_effects.TEAM_RUNTIME_ROLES
        )
        assert sorted(t["task_id"] for t in effects["in_flight_tasks"]) == [
            "t-leader-decision",
            "t-worker-assigned",
            "t-worker-running",
        ]

    def test_no_workspace_means_change_set_only(self) -> None:
        effects = server.policy_effects.compute_policy_effects(["worker/SOUL.md"])
        assert effects["affected_runtimes"] == ["worker"]
        assert effects["in_flight_tasks"] == []

    def test_summary_is_a_readable_list(self, tmp_path: Path) -> None:
        _seed_inflight_tasks(tmp_path)
        effects = server.policy_effects.compute_policy_effects(
            ["worker/allowlist.json"], workspace=tmp_path
        )
        assert "runtimes: worker" in effects["summary"]
        assert "tool surface" in effects["summary"]
        assert "t-worker-running" in effects["summary"]
        assert "waiting on worker" in effects["summary"]

    def test_dict_shaped_changes_accepted(self) -> None:
        # git-style {"path": ...} entries (diff --name-only shape either way)
        effects = server.policy_effects.compute_policy_effects(
            [{"path": "worker/SOUL.md"}, {"path": "docs/x.md"}]
        )
        assert effects["affected_runtimes"] == ["worker"]
