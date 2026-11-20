import copy
import importlib.util
from pathlib import Path
import pytest

ROOT = Path(__file__).resolve().parents[3]
spec = importlib.util.spec_from_file_location("task_context", ROOT / "plugins/teamharness/mcp/common/task_context.py")
context = importlib.util.module_from_spec(spec)
spec.loader.exec_module(context)


def assignment():
    event = {"task_id": "t1", "main_task_id": "main", "delivery_id": "d1"}
    result = {"task": {"id": "t1", "mainTaskId": "main", "status": "in_progress", "assignmentId": "d1", "assignedTo": "canonical-worker-cr"},
              "taskDirectory": "/shared/tasks/t1", "outputDirectory": "/shared/tasks/t1/workspace", "scratchDirectory": "/shared/tasks/t1/workspace/scratch"}
    return event, result


def test_prepared_assignment_uses_backend_canonical_identity():
    event, result = assignment()
    prepared = context.prepare("task_assigned", event, result)
    assert prepared["execution"] == "t1"
    assert prepared["ack"][2:] == ("canonical-worker-cr", "d1")


@pytest.mark.parametrize("field,value", [("id", "other"), ("assignmentId", "stale"), ("status", "submitted"), ("mainTaskId", "other")])
def test_wrong_assignment_fails_closed(field, value):
    event, result = assignment()
    result["task"][field] = value
    with pytest.raises(ValueError):
        context.prepare("task_assigned", event, result)


@pytest.mark.parametrize("field,value", [("outputDirectory", "/other"), ("scratchDirectory", "relative"), ("taskDirectory", "/shared/tasks/other")])
def test_invalid_prepared_directory_fails_closed(field, value):
    event, result = assignment()
    result[field] = value
    with pytest.raises(ValueError):
        context.prepare("task_assigned", event, result)


def test_review_binds_exact_public_candidate_and_does_not_claim_business_validation():
    event = {"task_id": "t1", "main_task_id": "main", "submission_id": "s1"}
    result = {"task": {"id": "t1", "mainTaskId": "main", "status": "submitted", "submissionId": "s1",
                       "candidate": {"id": "s1", "artifacts": [], "reportFile": {"artifactId": "report-s1"}}}}
    prepared = context.prepare("task_submitted", event, result)
    assert prepared["review"] == ("t1", "s1")
    assert "not a completed business review" in prepared["fact"]
    for change in ("submission", "candidate", "missing"):
        bad = copy.deepcopy(result)
        if change == "submission": bad["task"]["submissionId"] = "other"
        elif change == "candidate": bad["task"]["candidate"]["id"] = "other"
        else: del bad["task"]["candidate"]
        with pytest.raises(ValueError): context.prepare("task_submitted", event, bad)


def test_closure_requires_all_public_nodes_accepted():
    event = {"main_task_id": "main"}
    result = {"task": {"id": "main", "kind": "main", "status": "in_progress", "nodes": [{"status": "completed"}]}}
    assert context.prepare("main_task_ready", event, result)["completion"] == "main"
    result["task"]["nodes"][0]["status"] = "submitted"
    with pytest.raises(ValueError): context.prepare("main_task_ready", event, result)
