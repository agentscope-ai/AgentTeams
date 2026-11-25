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


def test_assistant_is_a_verified_control_turn_without_business_task_operations():
    def no_task(_arguments):
        raise AssertionError("assistant reminder must not prepare a business Task")
    assert context.prepare_event({"kind": "assistant_digest"}, no_task) == {}
    with pytest.raises(ValueError):
        context.prepare_event({"kind": "assistant_digest", "task_id": "foreign"}, no_task)


def test_prepared_assignment_uses_backend_canonical_identity():
    event, result = assignment()
    prepared = context.prepare("task_assigned", event, result)
    assert prepared["execution"] == "t1"
    assert prepared["ack"][2:] == ("canonical-worker-cr", "d1")
    assert "exact absolute outputDirectory" in prepared["fact"]
    assert "submit before ending your turn" in prepared["fact"]
    assert "native Task tool" in prepared["fact"]
    assert "JSON array" in prepared["fact"]
    assert "only the deliverables assigned to your node" in prepared["fact"]
    assert "Prefer native file tools for simple reads and writes" in prepared["fact"]


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


def test_managed_file_writes_use_prepared_directories(tmp_path):
    output = tmp_path / "tasks" / "t1" / "workspace"
    scratch = tmp_path / "tasks" / "t1" / "scratch"
    output.mkdir(parents=True)
    neighbor = tmp_path / "tasks" / "t11"
    (output / "outside").symlink_to(neighbor)

    for path in ("report.json", str(output / "nested" / "report.json"), str(scratch / "test.log")):
        assert context.task_write_path_allowed(path, output, scratch)
    for path in ("", "../inputs/orders.json", str(neighbor / "report.json"), "outside/report.json"):
        assert not context.task_write_path_allowed(path, output, scratch)


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


def closure():
    event = {"main_task_id": "main"}
    accepted = {"id": "t1", "kind": "execution", "mainTaskId": "main", "status": "completed", "submissionId": "s1",
                "candidate": {"id": "s1", "summary": "Verified result", "artifacts": [], "reportFile": {"artifactId": "report-s1"}},
                "review": {"id": "r1", "submissionId": "s1", "accepted": True, "status": "completed"}}
    result = {"task": {"id": "main", "kind": "main", "status": "in_progress", "nodes": [{"taskId": "t1", "status": "completed"}], "acceptedResults": [accepted]}}
    return event, result


def test_closure_requires_all_public_nodes_accepted():
    event, result = closure()
    prepared = context.prepare("main_task_ready", event, result)
    assert prepared["completion"] == "main"
    assert prepared["context"]["acceptedResults"][0]["review"]["id"] == "r1"
    assert "final deliverable set" in prepared["fact"]
    assert "superseded intermediate" in prepared["fact"]
    result["task"]["nodes"][0]["status"] = "submitted"
    with pytest.raises(ValueError): context.prepare("main_task_ready", event, result)


def test_final_acceptance_consumes_exact_handoff_without_inventing_leader_review():
    event, result = closure()
    child = result['task']['acceptedResults'][0]
    del child['review']
    child.update(resultStatus='SUCCESS', handoff={'submissionId': 's1', 'actorName': 'dev', 'actorRole': 'worker'})
    prepared = context.prepare('main_task_ready', event, result)
    assert prepared['completion'] == 'main'
    assert 'review' not in prepared['context']['acceptedResults'][0]
    child['handoff']['submissionId'] = 'other'
    with pytest.raises(ValueError): context.prepare('main_task_ready', event, result)


@pytest.mark.parametrize("change", ["missing", "wrong-task", "wrong-main", "unreviewed", "wrong-candidate", "wrong-review", "duplicate"])
def test_closure_rejects_missing_or_mismatched_accepted_evidence(change):
    event, result = closure()
    task = result["task"]; accepted = task["acceptedResults"][0]
    if change == "missing": del task["acceptedResults"]
    elif change == "wrong-task": accepted["id"] = "other"
    elif change == "wrong-main": accepted["mainTaskId"] = "other"
    elif change == "unreviewed": accepted["review"]["accepted"] = False
    elif change == "wrong-candidate": accepted["candidate"]["id"] = "other"
    elif change == "wrong-review": accepted["review"]["submissionId"] = "other"
    else: task["acceptedResults"].append(copy.deepcopy(accepted))
    with pytest.raises(ValueError): context.prepare("main_task_ready", event, result)


def test_model_context_deduplicates_only_exact_original_and_node_spec_without_mutation():
    original = "Keep all original requirements, including keyboard access."
    node = {"taskId": "t1", "spec": "Independently verify the published HTML."}
    value = {"id": "main", "spec": original, "nodes": [node], "acceptedResults": [
        {"id": "t1", "spec": "# Original request\n" + original + "\n\n# Your assignment\n" + node["spec"],
         "candidate": {"report": "Measured evidence", "artifacts": [{"artifactId": "f1", "sha256": "abc"}]}},
        {"id": "t2", "spec": "Unique correction requirements"}]}
    before = copy.deepcopy(value)
    result = context.model_context(value)
    assert result["spec"] == original
    assert "spec" not in result["acceptedResults"][0]
    assert result["acceptedResults"][0]["candidate"] == value["acceptedResults"][0]["candidate"]
    assert result["acceptedResults"][1]["spec"] == "Unique correction requirements"
    assert value == before


def test_prepared_inputs_carry_manifest_not_whole_file_previews():
    value = {"task": {"id":"t1","spec":"Your task requirements"}, "inputs":[{"taskId":"upstream","summary":"PASS, head abc", "files":[
        {"name":"page.html","localPath":"/inputs/page.html","sha256":"sha","version":"sha","preview":{"text":"large source"}},
        {"name":"test.md","localPath":"/inputs/test.md","artifactId":"report","preview":{"text":"large report"}}]}]}
    result=context.model_context(value)
    assert result['task']==value['task']
    assert result['inputs'][0]['summary']=='PASS, head abc'
    assert result['inputs'][0]['files'][0]=={'name':'page.html','localPath':'/inputs/page.html','sha256':'sha','version':'sha'}
    assert 'preview' in value['inputs'][0]['files'][0]
