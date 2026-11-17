"""Shared ordinary-message recovery controls; no native model execution."""
import copy
import hashlib
import os
from pathlib import Path
import sys
from unittest.mock import Mock, patch

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "shared/lib"))
from task_recovery import RecoveryConflict
from worker_matrix_state import WorkerMatrix
from task_recovery_rpc import execute


def entry():
    return {"instance_id": "current-start", "runtime": "hermes", "etag": "current-etag", "recovery": "resume",
            "input": {"member_prefix": "agents/worker", "room_id": "!room:test", "event_id": "$input"},
            "execution": {"generation": 2, "instance_id": "current-start", "process_id": "current-process", "runtime": "hermes", "state": "active", "checkpoint": {"version": 3, "completed_steps": ["one"]}}}


def identity():
    return {"event_key": "event-key", "generation": 2, "instance_id": "current-start", "process_id": "current-process"}


def test_message_reply_bytes_are_persisted_before_the_checkpoint():
    source = entry()
    order = []
    def request(method, path, *args):
        order.append(method)
        return copy.deepcopy(source)
    recovery = Mock(process="current-process", request=Mock(side_effect=request))
    with patch.dict(os.environ, {"AGENTTEAMS_STORAGE_PREFIX": "storage/tenant"}), patch("worker_matrix_state.subprocess.run", side_effect=lambda *a, **kw: order.append("upload")) as upload:
        WorkerMatrix("hermes", recovery=recovery).message_save_reply(identity(), "saved answer")
    assert order == ["GET", "upload", "POST"]
    assert upload.call_args.kwargs["input"] == b"saved answer"
    cp = recovery.request.call_args.args[2]["checkpoint"]
    assert cp["version"] == 4 and cp["completed_steps"] == ["one"]
    assert cp["result"]["sha256"] == hashlib.sha256(b"saved answer").hexdigest()
    assert cp["result"]["key"].startswith("agents/worker/runtime/matrix/results/")
    assert cp["delivery"] == "pending" and cp["reply_txn"].startswith("matrix-reply:")


def test_late_native_text_cannot_replace_an_already_saved_result():
    source = entry()
    source["recovery"] = "deliver"
    source["execution"]["checkpoint"].update(delivery="pending", reply_txn="original-txn", result={"key": "saved"})
    claimed = {**source, "saved_result": "authoritative answer"}
    recovery = Mock(process="current-process", request=Mock(side_effect=[source, source, claimed]))
    with patch("worker_matrix_state.subprocess.run") as upload:
        saved = WorkerMatrix("hermes", recovery=recovery).message_save_reply(identity(), "late differing text")
    upload.assert_not_called()
    assert saved["saved_result"] == "authoritative answer"
    assert recovery.request.call_args.args[2]["action"] == "claim"


@pytest.mark.parametrize("state,mode", [("needs_verification", "verify"), ("active", "deliver"), ("cancelled", "done"), ("released", "resume"), ("active", "backfill")])
def test_non_execution_modes_cannot_admit_native_inference(state, mode):
    source = entry()
    source["execution"]["state"], source["recovery"] = state, mode
    recovery = Mock(process="current-process", request=Mock(return_value=source))
    with pytest.raises(RecoveryConflict):
        WorkerMatrix("hermes", recovery=recovery).message_current(identity())
    assert recovery.request.call_count == 1


def test_replaced_native_callback_cannot_upload_a_reply():
    source = entry()
    source["execution"]["generation"] = 3
    recovery = Mock(process="current-process", request=Mock(return_value=source))
    with patch("worker_matrix_state.subprocess.run") as upload, pytest.raises(RecoveryConflict):
        WorkerMatrix("hermes", recovery=recovery).message_save_reply(identity(), "stale")
    upload.assert_not_called()
    assert recovery.request.call_count == 1


def test_reply_delivery_keeps_the_existing_transaction_and_progress():
    source = entry()
    source["recovery"] = "deliver"
    source["execution"]["checkpoint"].update(delivery="pending", result={"key": "saved"}, reply_txn="original-txn")
    recovery = Mock(process="current-process", request=Mock(side_effect=[source, source]))
    WorkerMatrix("hermes", recovery=recovery).message_delivered(identity(), "$canonical-reply")
    cp = recovery.request.call_args.args[2]["checkpoint"]
    assert cp["reply_txn"] == "original-txn" and cp["reply_event"] == "$canonical-reply"
    assert cp["completed_steps"] == ["one"] and cp["delivery"] == "delivered"


def test_fresh_message_session_context_and_digest_use_the_shared_checkpoint():
    source = entry()
    source["input"]["input"] = {"sender": "@human:test", "content": {"body": "the original request"}}
    recovery = Mock(process="current-process", request=Mock(side_effect=[source, source]))
    claimed = WorkerMatrix("hermes", recovery=recovery).message_claim("event-key")
    assert claimed["identity"] == identity()
    assert "the original request" in claimed["resume_text"]
    assert '"completed_steps": ["one"]' in claimed["resume_text"]
    assert claimed["checkpoint_digest"]


@pytest.mark.parametrize("action", [None, 3, []])
def test_non_string_rpc_actions_are_controlled_invalid_requests(action):
    with patch("task_recovery_rpc.enabled", return_value=True), patch("task_recovery_rpc.Recovery"), pytest.raises(ValueError):
        execute({"action": action})


def test_native_tool_batch_is_fenced_before_dispatch_and_verified_with_matching_receipts():
    source = entry()
    requests = []
    def request(method, path, *args):
        if method == "POST":
            cp = args[0]["checkpoint"]
            requests.append(copy.deepcopy(cp))
            source["execution"]["checkpoint"] = cp
            pending = cp["operation"]["state"] == "started"
            source["recovery"] = "verify" if pending else "resume"
            source["execution"]["state"] = "needs_verification" if pending else "active"
        return copy.deepcopy(source)
    matrix = WorkerMatrix("hermes", recovery=Mock(process="current-process", request=request))
    calls = [{"id": "call-a", "name": "write", "arguments": {"file": "report.txt"}}]
    op = matrix.message_begin_tools(identity(), calls)
    assert requests[-1]["operation"]["state"] == "started"
    with pytest.raises(RecoveryConflict):
        matrix.message_begin_tools(identity(), calls)
    with pytest.raises(RecoveryConflict):
        matrix.message_finish_tools(identity(), op, [{"id": "other", "content": "ok"}])
    assert len(requests) == 1
    with patch.dict(os.environ, {"AGENTTEAMS_STORAGE_PREFIX": "storage/tenant"}), patch("worker_matrix_state.subprocess.run") as upload:
        matrix.message_finish_tools(identity(), op, [{"id": "call-a", "content": "verified file digest"}])
    cp = requests[-1]
    assert cp["version"] == 5 and cp["completed_steps"] == ["one", op]
    assert cp["operation"]["id"] == op and cp["operation"]["state"] == "verified"
    assert hashlib.sha256(upload.call_args.kwargs["input"]).hexdigest() == cp["artifacts"][-1]["sha256"]
    assert "verified file digest" in upload.call_args.kwargs["input"].decode()
    with pytest.raises(RecoveryConflict):
        matrix.message_begin_tools(identity(), calls)
    assert len(requests) == 2


def test_missing_or_ambiguous_tool_results_leave_original_operation_pending():
    source = entry()
    matrix = WorkerMatrix("hermes", recovery=Mock(process="current-process", request=Mock(return_value=source)))
    calls = [{"id": "a", "name": "command", "arguments": "foo"}, {"id": "b", "name": "command", "arguments": "bar"}]
    op = matrix.message_begin_tools(identity(), calls)
    cp = matrix.recovery.request.call_args.args[2]["checkpoint"]
    source["execution"].update(checkpoint=cp, state="needs_verification")
    source["recovery"] = "verify"
    with patch("worker_matrix_state.subprocess.run") as upload:
        for results in ([], [{"id": "a", "content": "ok"}], [{"id": "a", "content": "ok"}, {"id": "a", "content": "ok"}]):
            with pytest.raises(RecoveryConflict):
                matrix.message_finish_tools(identity(), op, results)
        upload.assert_not_called()
