"""A stage start reuses the existing room, plan and delegation boundaries."""
import json
from pathlib import Path
import sys

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[3] / "teamharness/mcp"))
import server


@pytest.fixture
def stage(tmp_path, monkeypatch):
    monkeypatch.delenv("TEAMHARNESS_COORDINATION_URL", raising=False)
    monkeypatch.delenv("AGENTTEAMS_AGENT_ROLE", raising=False)
    monkeypatch.delenv("AGENTTEAMS_WORKER_ROLE", raising=False)
    monkeypatch.delenv("TEAMHARNESS_RUNTIME_CONFIG", raising=False)
    monkeypatch.setattr(server, "_pull_project", lambda *a: True)
    monkeypatch.setattr(server, "_sync_project", lambda *a: True)
    calls = []
    def room(args):
        calls.append(("room", args))
        return {"ok": True, "roomId": "!task:test"}
    def delegate(args):
        calls.append(("delegate", args))
        return {"ok": True, "task": {"task_id": args["payload"]["taskId"]}, "notification": {"sent": True}}
    monkeypatch.setattr(server, "_roomflow", room)
    monkeypatch.setattr(server, "_taskflow", delegate)
    args = {"role": "leader", "workspaceDir": str(tmp_path), "action": "start_project", "payload": {
        "projectId": "stage", "source": "matrix", "sourceRoomId": "!source:test", "requester": "@requester:test",
        "replyRoute": {"channel": "matrix", "target_session": "!source:test", "target_user": "@requester:test"},
        "tasks": [
            {"taskId": "stage-1", "assignedTo": "@analyst:test", "spec": "Produce bounded requirements."},
            {"taskId": "stage-2", "assignedTo": "@dev:test", "dependsOn": ["stage-1"]},
        ]}}
    return args, calls


def invoke(args):
    return json.loads(server.call_tool("projectflow", args)["content"][0]["text"])


def test_start_creates_one_room_one_plan_and_only_ready_assignments(stage):
    args, calls = stage
    result = invoke(args)
    assert result["ok"] and result["roomId"] == "!task:test"
    assert [name for name, _ in calls] == ["room", "delegate"]
    delegated = calls[-1][1]["payload"]
    assert delegated["taskId"] == "stage-1" and delegated["roomId"] == "!task:test"
    assert result["project"]["tasks"][1]["depends_on"] == ["stage-1"]


@pytest.mark.parametrize("invalid", ["cycle", "missing-spec", "worker"])
def test_invalid_start_has_no_external_side_effects(stage, invalid):
    args, calls = stage
    if invalid == "cycle": args["payload"]["tasks"][0]["dependsOn"] = ["stage-2"]
    if invalid == "missing-spec": args["payload"]["tasks"][0].pop("spec")
    if invalid == "worker": args["role"] = "worker"
    assert not invoke(args)["ok"]
    assert not calls


def test_start_retry_reuses_plan_and_does_not_reset_accepted_work(stage):
    args, calls = stage
    assert invoke(args)["ok"]
    path = Path(args["workspaceDir"]) / "shared/projects/stage/meta.json"
    project = json.loads(path.read_text())
    project["tasks"][0]["status"] = "completed"
    project["tasks"][1]["status"] = "assigned"
    path.write_text(json.dumps(project))
    calls.clear()
    result = invoke(args)
    assert result["ok"] and result["reused"]
    assert result["project"]["tasks"][0]["status"] == "completed"
    assert calls == []


def test_automatic_start_requires_complete_leaf_specs_before_creating_room(stage):
    args, calls = stage
    args["payload"]["dispatchMode"] = "automatic"
    assert not invoke(args)["ok"]
    assert not calls


def test_native_plan_rejects_unsupported_future_assignees_before_side_effects(stage, monkeypatch):
    from mcp.transport import coordination
    args, calls = stage
    args["payload"]["dispatchMode"] = "automatic"
    args["payload"]["tasks"][1]["spec"] = "Implement."
    monkeypatch.setenv("TEAMHARNESS_COORDINATION_URL", "http://controller/api/v1/coordination")
    monkeypatch.setattr(coordination, "native_team_members", lambda team: [
        {"name": "analyst", "matrixUserID": "@analyst:test", "nativeCoordinationSupported": True},
        {"name": "dev", "matrixUserID": "@dev:test", "nativeCoordinationSupported": False}])
    result = invoke(args)
    assert not result["ok"] and "@dev:test" in result["error"]
    assert not calls
    assert not (Path(args["workspaceDir"]) / "shared/projects/stage/meta.json").exists()


def test_automatic_plan_persists_specs_and_advances_only_accepted_dependencies(stage):
    args, calls = stage
    args["payload"]["dispatchMode"] = "automatic"
    args["payload"]["tasks"][1]["spec"] = "Implement the accepted requirements."
    assert invoke(args)["ok"]
    path = Path(args["workspaceDir"]) / "shared/projects/stage/meta.json"
    project = json.loads(path.read_text())
    assert project["tasks"][1]["spec"] == "Implement the accepted requirements."
    calls.clear()
    args.update(action="advance_project", payload={"projectId": "stage"})
    project["tasks"][0]["status"] = "submitted"
    path.write_text(json.dumps(project))
    assert invoke(args)["ok"] and not calls
    project["tasks"][0]["status"] = "completed"
    path.write_text(json.dumps(project))
    result = invoke(args)
    assert result["ok"] and len(calls) == 1
    assert calls[0][1]["payload"]["taskId"] == "stage-2"
    assert calls[0][1]["payload"]["spec"] == "Implement the accepted requirements."


def test_automatic_dispatch_respects_pause(stage):
    args, calls = stage
    args["payload"]["dispatchMode"] = "automatic"
    args["payload"]["tasks"][1]["spec"] = "Implement."
    assert invoke(args)["ok"]
    path = Path(args["workspaceDir"]) / "shared/projects/stage/meta.json"
    project = json.loads(path.read_text())
    project["tasks"][0]["status"] = "completed"
    project["status"] = "paused"
    path.write_text(json.dumps(project))
    calls.clear()
    args.update(action="advance_project", payload={"projectId": "stage"})
    assert not invoke(args)["ok"] and not calls


def test_start_receipt_does_not_echo_plan_specs_but_keeps_durable_inputs(stage):
    args, calls = stage
    spec = "Required behavior and exact acceptance cases. " * 500
    args["payload"]["tasks"][0]["spec"] = spec
    result = invoke(args)
    assert result["ok"]
    assert spec.strip() not in json.dumps(result)
    assert calls[-1][1]["payload"]["spec"] == spec.strip()
    saved = json.loads((Path(args["workspaceDir"]) / "shared/projects/stage/meta.json").read_text())
    assert saved["tasks"][0]["spec"] == spec.strip()
    assert result["project"]["tasks"][1]["depends_on"] == ["stage-1"]
