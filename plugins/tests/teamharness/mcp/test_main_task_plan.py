"""Ordinary collaboration is scoped to a main Task, never an implicit Project."""

import json
from pathlib import Path
import sys

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[3] / "teamharness/mcp"))
import server


@pytest.fixture
def plan(tmp_path, monkeypatch):
    for key in (
        "TEAMHARNESS_COORDINATION_URL",
        "AGENTTEAMS_AGENT_ROLE",
        "AGENTTEAMS_WORKER_ROLE",
        "TEAMHARNESS_RUNTIME_CONFIG",
        "AGENTTEAMS_CONTROLLER_URL",
    ):
        monkeypatch.delenv(key, raising=False)
    def sync_task(arguments, task_id, **kwargs):
        # The live Controller advances the parent after the child's durable ACK.
        path = Path(arguments["workspaceDir"]) / "shared/tasks" / task_id / "meta.json"
        task = json.loads(path.read_text()) if path.exists() else {}
        if task.get("main_task_id") and task.get("status") == "in_progress":
            parent = path.parent.parent / task["main_task_id"] / "meta.json"
            main = json.loads(parent.read_text())
            if main["status"] == "assigned":
                main["status"] = "in_progress"
                parent.write_text(json.dumps(main))
        return True

    monkeypatch.setattr(server, "_sync_task", sync_task)
    monkeypatch.setattr(server, "_pull_task", lambda *a, **k: True)
    monkeypatch.setattr(server, "_sync_project", lambda *a, **k: True)
    monkeypatch.setattr(server, "_pull_project", lambda *a, **k: True)
    calls = []
    monkeypatch.setattr(
        server,
        "_roomflow",
        lambda a: calls.append(a)
        or {"ok": True, "roomId": "!" + a["payload"]["taskId"] + ":test"},
    )
    monkeypatch.setattr(
        server,
        "_send_delegate_notification",
        lambda *a, **k: {"sent": True, "eventId": "$" + k["task_id"]},
    )
    monkeypatch.setattr(
        server, "_validate_assignee_membership", lambda *a: {"ok": True}
    )
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "http://matrix")
    monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "test")
    args = {
        "role": "leader",
        "workspaceDir": str(tmp_path),
        "action": "start_task",
        "payload": {
            "taskId": "main-a",
            "teamId": "alpha",
            "title": "Deliver a small feature",
            "spec": "Preserve the original contract.",
            "sourceRoomId": "!source:test",
            "requester": "@human:test",
            "replyRoute": {
                "channel": "matrix",
                "target_session": "!source:test",
                "target_user": "@human:test",
            },
            "tasks": [
                {
                    "taskId": "analysis-a",
                    "assignedTo": "@analyst:test",
                    "spec": "Analyze.",
                },
                {
                    "taskId": "dev-a",
                    "assignedTo": "@dev:test",
                    "spec": "Implement.",
                    "dependsOn": ["analysis-a"],
                },
            ],
        },
    }
    return args, calls


def invoke(args):
    return json.loads(server.call_tool("taskflow", args)["content"][0]["text"])


def test_main_task_starts_without_any_project(plan):
    args, rooms = plan
    result = invoke(args)
    assert result["ok"], json.dumps(result, ensure_ascii=False)
    root = Path(args["workspaceDir"]) / "shared"
    assert not (root / "projects").exists()
    main = json.loads((root / "tasks/main-a/meta.json").read_text())
    child = json.loads((root / "tasks/analysis-a/meta.json").read_text())
    assert main["task_id"] == "main-a" and main["status"] == "assigned"
    assert not main.get("project_id") and not child.get("project_id")
    assert child["main_task_id"] == child["parent_task_id"] == "main-a"
    assert child["task_kind"] == "execution" and child["team_id"] == "alpha"
    assert child["status"] == "assigned" and main["source_room_id"] == "!source:test"
    assert (
        "Original contract" not in (root / "tasks/analysis-a/spec.md").read_text()
    )  # No invented contract.
    assert (
        "Preserve the original contract."
        in (root / "tasks/analysis-a/spec.md").read_text()
    )
    assert not (root / "tasks/dev-a/meta.json").exists()
    assert rooms[0]["payload"]["taskId"] == "main-a"


def test_leader_plans_the_existing_pending_main_task(plan):
    args, _ = plan
    root = Path(args["workspaceDir"]) / "shared/tasks/main-a"
    root.mkdir(parents=True)
    original = {"task_id": "main-a", "task_kind": "main", "status": "planned",
                "spec": args["payload"]["spec"], "tasks": [], "created_at": "2026-11-17T00:00:00Z",
                "source_room_id": "!source:test", "requester": "@human:test", "reply_route": args["payload"]["replyRoute"]}
    (root / "meta.json").write_text(json.dumps(original))
    result = invoke(args)
    assert result["ok"], result
    main = json.loads((root / "meta.json").read_text())
    assert main["created_at"] == original["created_at"]
    assert main["status"] == "assigned"
    assert len(main["tasks"]) == 2


def test_main_task_retry_preserves_assignments_and_uses_same_room(plan):
    args, rooms = plan
    assert invoke(args)["ok"]
    root = Path(args["workspaceDir"]) / "shared/tasks"
    before = (root / "analysis-a/meta.json").read_bytes()
    result = invoke(args)
    assert result["ok"] and result["reused"], result
    assert len(rooms) == 1 and (root / "analysis-a/meta.json").read_bytes() == before
    args["payload"]["spec"] = "A different request"
    assert not invoke(args)["ok"]


def test_main_task_cannot_close_with_unaccepted_children(plan):
    args, _ = plan
    assert invoke(args)["ok"]
    mainpath = Path(args["workspaceDir"]) / "shared/tasks/main-a/meta.json"
    before = mainpath.read_bytes()
    result = invoke(
        {
            **args,
            "action": "complete_task",
            "payload": {"taskId": "main-a", "report": "Not finished"},
        }
    )
    assert not result["ok"] and mainpath.read_bytes() == before


def test_two_main_tasks_in_one_project_have_separate_rooms_and_lifecycles(plan):
    args, rooms = plan
    project = Path(args["workspaceDir"]) / "shared/projects/p/meta.json"
    project.parent.mkdir(parents=True)
    project.write_text(
        json.dumps(
            {
                "project_id": "p",
                "title": "Shared project",
                "status": "active",
                "tasks": [],
            }
        )
    )
    args["payload"]["projectId"] = "p"
    assert invoke(args)["ok"]
    second = {
        **args,
        "payload": {
            **args["payload"],
            "taskId": "main-b",
            "tasks": [
                {
                    "taskId": "dev-b",
                    "assignedTo": "@dev:test",
                    "spec": "Independent task.",
                }
            ],
        },
    }
    assert invoke(second)["ok"]
    root = Path(args["workspaceDir"]) / "shared/tasks"
    a, b = [
        json.loads((root / t / "meta.json").read_text()) for t in ("main-a", "main-b")
    ]
    assert a["project_id"] == b["project_id"] == "p"
    assert a["room_id"] != b["room_id"]
    assert {n["task_id"] for n in json.loads(project.read_text())["tasks"]} == {
        "main-a",
        "main-b",
    }
    assert len(rooms) == 2


@pytest.mark.parametrize("mutation", ["cycle", "cross-main", "worker"])
def test_invalid_task_plan_has_no_effects(plan, mutation):
    args, rooms = plan
    if mutation == "cycle":
        args["payload"]["tasks"][0]["dependsOn"] = ["dev-a"]
    if mutation == "cross-main":
        args["payload"]["tasks"][0]["parentTaskId"] = "another-main"
    if mutation == "worker":
        args["role"] = "worker"
    assert not invoke(args)["ok"] and not rooms


def test_full_main_task_acceptance_and_closure_without_project(plan, monkeypatch):
    args, _ = plan
    monkeypatch.setattr(
        server, "_message", lambda a: {"ok": True, "messageId": "$final"}
    )
    assert invoke(args)["ok"]
    root = Path(args["workspaceDir"]) / "shared/tasks"
    for task_id in ("analysis-a", "dev-a"):
        directory = root / task_id
        if task_id == "dev-a":
            assert directory.exists()  # Dispatched only after acceptance.
        ack = invoke(
            {
                **args,
                "role": "worker",
                "action": "ack_task",
                "payload": {"taskId": task_id},
            }
        )
        assert ack["ok"], ack
        (directory / "result.md").write_text("Actual checked result")
        submitted = invoke(
            {
                **args,
                "role": "worker",
                "action": "submit_task",
                "payload": {
                    "taskId": task_id,
                    "status": "SUCCESS",
                    "summary": "Contract verified.",
                    "deliverables": [f"shared/tasks/{task_id}/result.md"],
                },
            }
        )
        assert submitted["ok"], submitted
        accepted = invoke(
            {
                **args,
                "action": "accept_task_result",
                "payload": {
                    "mainTaskId": "main-a",
                    "taskId": task_id,
                    "submissionId": submitted["task"]["submission_id"],
                    "resultStatus": "SUCCESS",
                    "accepted": True,
                },
            }
        )
        assert accepted["ok"] and accepted["accepted"], accepted
    assert accepted["mainTaskReady"] and accepted["nextAction"]["taskId"] == "main-a"
    final = invoke(
        {
            **args,
            "action": "complete_task",
            "payload": {
                "taskId": "main-a",
                "report": "Both execution subtasks accepted.",
            },
        }
    )
    assert final["ok"] and final["task"]["status"] == "completed", final
    assert not (Path(args["workspaceDir"]) / "shared/projects").exists()
    assert invoke(
        {
            **args,
            "action": "complete_task",
            "payload": {
                "taskId": "main-a",
                "report": "Both execution subtasks accepted.",
            },
        }
    )["ok"]
    assert not invoke(
        {
            **args,
            "action": "complete_task",
            "payload": {"taskId": "main-a", "report": "Changed after completion"},
        }
    )["ok"]


def test_main_plan_commit_merges_only_one_node_and_preserves_other_decisions(
    plan, monkeypatch
):
    args, _ = plan
    assert invoke(args)["ok"]
    from mcp.lifecycle.plans import _sync_main_plan
    from mcp.artifact import sync

    path = Path(args["workspaceDir"]) / "shared/tasks/main-a/meta.json"
    local = json.loads(path.read_text())
    remote = json.loads(path.read_text())
    remote["status"] = "in_progress"
    local["tasks"][0]["status"] = "completed"
    remote["tasks"][1]["status"] = "completed"
    path.write_text(json.dumps(local))
    monkeypatch.setenv("AGENTTEAMS_CONTROLLER_URL", "http://controller")
    monkeypatch.setattr(
        sync, "_remote_meta_consistent_read", lambda *a: (remote, "v2", None)
    )
    from unittest.mock import create_autospec

    push = create_autospec(sync._push_task_files, return_value=True)
    monkeypatch.setattr(sync, "_push_task_files", push)
    committed = []
    monkeypatch.setattr(
        sync, "_commit_task_meta", lambda *a, **k: committed.append(a) or {"ok": True}
    )
    assert _sync_main_plan(
        args, "main-a", task_update=("analysis-a", {"status": "completed"})
    )
    assert all(n["status"] == "completed" for n in committed[0][1]["tasks"])
    assert committed[0][2] == "v2"
    remote["status"] = "cancelled"
    assert not _sync_main_plan(
        args, "main-a", task_update=("analysis-a", {"status": "completed"})
    )


def test_assignment_retry_repairs_only_missing_parent_plan_projection(plan):
    args, _ = plan
    assert invoke(args)["ok"]
    path = Path(args["workspaceDir"]) / "shared/tasks/main-a/meta.json"
    main = json.loads(path.read_text())
    main["tasks"][0]["status"] = "planned"
    path.write_text(json.dumps(main))
    result = invoke(args)
    assert result["ok"], result
    assert json.loads(path.read_text())["tasks"][0]["status"] == "assigned"


def test_dispatch_uses_task_kind_not_parent_presence(plan):
    args, _ = plan
    assert invoke(args)["ok"]
    path = Path(args["workspaceDir"]) / "shared/tasks/main-a/meta.json"
    main = json.loads(path.read_text())
    main["tasks"][0].update(task_kind="main", status="planned")
    path.write_text(json.dumps(main))
    result = invoke(
        {**args, "action": "advance_task", "payload": {"mainTaskId": "main-a"}}
    )
    assert not result["ok"] and "coordinating main" in result["error"]


def test_main_completion_uses_shared_submit_then_accept_lifecycle(plan, monkeypatch):
    args, _ = plan
    assert invoke(args)["ok"]
    from mcp.lifecycle.plans import _sync_main_plan
    from mcp.artifact import sync
    from unittest.mock import create_autospec

    path = Path(args["workspaceDir"]) / "shared/tasks/main-a/meta.json"
    remote = json.loads(path.read_text())
    for node in remote["tasks"]:
        node["status"] = "completed"
    remote["status"] = "in_progress"  # Workers have ACKed before acceptance.
    local = {
        **remote,
        "status": "completed",
        "summary": "Final report",
        "result_path": "shared/tasks/main-a/result.md",
        "result_status": "SUCCESS",
        "deliverables": ["shared/tasks/main-a/result.md"],
        "completed_at": "2026-11-16T00:00:00Z",
    }
    path.write_text(json.dumps(local))
    monkeypatch.setenv("AGENTTEAMS_CONTROLLER_URL", "http://controller")
    versions = ["v1"]
    monkeypatch.setattr(
        sync,
        "_remote_meta_consistent_read",
        lambda *a: (json.loads(json.dumps(remote)), versions[-1], None),
    )
    monkeypatch.setattr(
        sync,
        "_push_task_files",
        create_autospec(sync._push_task_files, return_value=True),
    )
    states = []

    def commit(task_id, body, etag, team):
        assert etag == versions[-1]
        assert (remote["status"], body["status"]) in {
            ("in_progress", "submitted"),
            ("submitted", "completed"),
        }
        states.append(body["status"])
        remote.update(body)
        versions.append("v" + str(len(versions) + 1))
        return {"ok": True}

    monkeypatch.setattr(sync, "_commit_task_meta", commit)
    assert _sync_main_plan(args, "main-a", completing=True)
    assert states == ["submitted", "completed"]
