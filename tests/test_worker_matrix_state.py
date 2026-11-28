"""Common Matrix persistence client/facade contracts, not Harness E2E."""
import io
import json
import os
from pathlib import Path
import sys
from unittest.mock import Mock, call, patch

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "shared/lib"))
from task_recovery import Recovery, RecoveryConflict
from task_recovery_rpc import execute
from worker_matrix_state import WorkerMatrix


def snapshot(etag="version-one"):
    return {"instance_id": "actual-instance", "runtime": "hermes", "etag": etag,
            "state": {"active_start": {"instance_id": "actual-instance", "process_id": "actual-process", "runtime": "hermes"},
                      "pending": ["event-key"]}}


def test_start_uses_actual_instance_and_create_if_absent():
    original = snapshot("")
    request = Mock(side_effect=[original, {"etag": "created", "state": original["state"]}])
    recovery = Mock(process="actual-process", request=request)
    result = WorkerMatrix("hermes", recovery=recovery).start()
    assert result["etag"] == "created"
    assert result["instance_id"] == "actual-instance"
    assert request.call_args_list[1].kwargs == {"create": True}
    assert request.call_args_list[1].args[2]["process_id"] == "actual-process"


def test_resume_keeps_conditional_version_and_reads_pending_without_project_list():
    current = snapshot()
    current["state"]["active_start"]["instance_id"] = "previous-instance"
    request = Mock(side_effect=[current, {"etag": "started", "state": current["state"]},
                                {"etag": "ingested", "state": current["state"]}, {"event_id": "$one"}])
    recovery = Mock(process="actual-process", request=request)
    state = WorkerMatrix("hermes", recovery=recovery)
    started = state.start()
    updated = state.ingest(started, {"runtime": "hermes", "token": "native-token"}, [{"event_id": "$one"}])
    assert request.call_args_list[1].args[3] == "version-one"
    assert request.call_args_list[2].args[3] == "started"
    assert state.pending(updated) == [{"event_id": "$one"}]
    assert request.call_args.args == ("GET", "runtime/matrix/events/event-key")


def test_repeated_start_does_not_race_with_matrix_or_tool_checkpoint_writes():
    current = snapshot()
    request = Mock(return_value=current)
    recovery = Mock(process="actual-process", request=request)
    matrix = WorkerMatrix("hermes", recovery=recovery)
    assert matrix.start() is current
    assert matrix.start() is current
    assert request.call_args_list == [call("GET", "runtime/matrix"), call("GET", "runtime/matrix")]


@pytest.mark.parametrize("change", ["actual-runtime", "process", "consumer-runtime"])
def test_foreign_native_state_is_rejected_before_mutation(change):
    current = snapshot()
    request = Mock(return_value=current)
    recovery = Mock(process="actual-process", request=request)
    matrix = WorkerMatrix("hermes", recovery=recovery)
    if change == "actual-runtime":
        current["runtime"] = "qwenpaw"
        with pytest.raises(RecoveryConflict):
            matrix.start()
        assert request.call_count == 1
    else:
        consumer = {"runtime": "hermes"}
        if change == "process":
            current["state"]["active_start"]["process_id"] = "old-process"
        else:
            consumer["runtime"] = "qwenpaw"
        with pytest.raises(RecoveryConflict):
            matrix.ingest(current, consumer, [])
        request.assert_not_called()


def test_native_rpc_exposes_the_same_common_matrix_entry():
    recovery = Mock(process="actual-process")
    recovery.request.return_value = snapshot()
    with patch("task_recovery_rpc.enabled", return_value=True), patch("task_recovery_rpc.Recovery", return_value=recovery):
        assert execute({"action": "matrix.load", "runtime": "hermes"}) == snapshot()
    recovery.request.assert_called_once_with("GET", "runtime/matrix")


def test_native_callback_does_not_need_to_fabricate_a_batch_cursor():
    current = snapshot()
    recovery = Mock(process="actual-process")
    recovery.request.return_value = {"etag": "persisted", "state": current["state"]}
    result = WorkerMatrix("hermes", recovery=recovery).ingest(current, None, [{"event_id": "$one"}])
    assert result["etag"] == "persisted"
    assert recovery.request.call_args.args[2]["consumer"] is None


@pytest.mark.parametrize("accepted", [True, False])
def test_attention_decision_does_not_create_independent_message_work(accepted):
    source = {"sender": "@human:test", "content": {"com.agentteams.answer": {
        "confirmation_id": "attention:T:one", "responses": []}}}
    matrix = WorkerMatrix("hermes", recovery=Mock())
    with patch("runtime_hitl.consume_attention", return_value=accepted), patch.object(matrix, "input", return_value=None):
        item = matrix.prepare_input("!room:test", "$answer", source, "message")
    assert item == {"room_id": "!room:test", "event_id": "$answer", "input": source, "intent": "context"}


def test_common_transport_sends_create_precondition_without_task_lease():
    with patch.dict(os.environ, {"AGENTTEAMS_PROCESS_ID": "actual-process", "AGENTTEAMS_CONTROLLER_URL": "http://controller.invalid"}), \
            patch("task_recovery.Path.read_text", return_value="worker-token"), \
            patch("task_recovery.urllib.request.urlopen", return_value=io.BytesIO(b'{}')) as send:
        Recovery()._request("POST", "runtime/matrix", {"action": "start"}, create=True)
    request = send.call_args.args[0]
    headers = {key.lower(): value for key, value in request.header_items()}
    assert headers["if-none-match"] == "*"
    assert "if-match" not in headers
    assert json.loads(request.data)["action"] == "start"


@pytest.mark.parametrize("changed_content", [False, True])
def test_replay_of_legacy_control_with_missing_sdk_timestamp(changed_content):
    matrix = WorkerMatrix("openhuman", recovery=Mock())
    stored = {"sender": "@leader:test", "type": "org.agentteams.coordination",
              "origin_server_ts": 0, "content": {"body": "plan"}}
    source = {**stored, "origin_server_ts": 1234}
    if changed_content:
        source["content"] = {"body": "different"}
    with patch("runtime_hitl.consume_attention", return_value=None), patch.object(
        matrix, "input", return_value={"intent": "control", "input": stored}
    ):
        item = matrix.prepare_input("!room:test", "$wake", source, "control")
    assert item["input"] == (source if changed_content else stored)
