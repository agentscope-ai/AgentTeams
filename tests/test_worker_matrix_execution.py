"""Shared ordinary-message recovery controls; no native model execution."""
import copy
import hashlib
import os
import subprocess
from pathlib import Path
import sys
from unittest.mock import Mock, patch

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "shared/lib"))
from task_recovery import RecoveryConflict, RecoveryUnavailable
from worker_matrix_state import WorkerMatrix
from task_recovery_rpc import execute


def entry():
    return {"instance_id": "current-start", "runtime": "hermes", "etag": "current-etag", "recovery": "resume",
            "input": {"member_prefix": "agents/worker", "room_id": "!room:test", "event_id": "$input"},
            "execution": {"generation": 2, "instance_id": "current-start", "process_id": "current-process", "runtime": "hermes", "state": "active", "checkpoint": {"version": 3, "completed_steps": ["one"]}}}


def identity():
    return {"event_key": "event-key", "generation": 2, "instance_id": "current-start", "process_id": "current-process"}


@pytest.mark.parametrize('deliver_result', [False, True])
def test_claim_reconciles_a_staged_native_return_after_receipt_connection_failure(tmp_path, deliver_result):
    from task_operation import begin
    from native_tool_returns import load
    source = entry()
    op, cp = begin(source['execution']['checkpoint'], 'event-key',
                   [{'id':'call','name':'external','arguments':{'value':1}}])
    if deliver_result:
        cp['result'] = {'key':'existing-reply','sha256':'saved','size':1}
    source['execution'].update(state='needs_verification', checkpoint=cp)
    source['recovery'] = 'verify'
    def request(method, path, body=None, *args):
        if body and body.get('action') == 'checkpoint':
            source['execution'].update(checkpoint=body['checkpoint'], state='active')
            source['recovery'] = 'deliver' if deliver_result else 'resume'
        return copy.deepcopy(source)
    recovery = Mock(process='current-process',request=Mock(side_effect=request))
    matrix = WorkerMatrix('hermes',recovery=recovery)
    results = [{'id':'call','content':'actual return'}]
    with patch.dict(os.environ, TEAMHARNESS_RUNTIME_CONFIG=str(tmp_path / 'runtime.yaml')):
        with patch.object(matrix,'message_pending_tools',side_effect=RecoveryUnavailable('connection')):
            with pytest.raises(RecoveryUnavailable):
                matrix.message_finish_tools(identity(),op,results,deliver_result=deliver_result)
        assert load(identity(),op)['results'] == results
        with patch.object(matrix,'_message_artifact',return_value={'key':'receipt','sha256':'hash','size':1}):
            saved = matrix.message_claim('event-key')
    assert saved['execution']['checkpoint']['operation']['state'] == 'verified'
    assert saved['recovery'] == ('deliver' if deliver_result else 'resume')


@pytest.mark.parametrize('same_return', [True, False])
def test_message_verified_return_is_idempotent_only_when_its_evidence_matches(same_return):
    from types import SimpleNamespace
    source, uploads, writes = entry(), [], []
    def request(method, path, body=None, *args):
        if body and body.get('action') == 'checkpoint':
            writes.append(body['checkpoint'])
            source['execution']['checkpoint'] = body['checkpoint']
            pending = body['checkpoint']['operation']['state'] == 'started'
            source['execution']['state'] = 'needs_verification' if pending else 'active'
            source['recovery'] = 'verify' if pending else 'resume'
        return copy.deepcopy(source)
    matrix = WorkerMatrix('hermes', recovery=Mock(process='current-process', request=Mock(side_effect=request)))
    operation = matrix.message_begin_tools(identity(), [{'id':'call','name':'native','arguments':{}}])
    results = [{'id':'call','content':'original return'}]
    def upload(source, data):
        uploads.append(data)
        return {'key':'agents/worker/receipt','sha256':hashlib.sha256(data).hexdigest(),'size':len(data)}
    with patch.dict(os.environ, AGENTTEAMS_STORAGE_PREFIX='storage'), \
            patch.object(matrix,'_message_artifact',side_effect=upload), \
            patch('native_tool_returns.subprocess.run',side_effect=lambda *a,**kw:SimpleNamespace(stdout=uploads[0])):
        matrix.message_finish_tools(identity(), operation, results)
        if same_return:
            assert matrix.message_finish_tools(identity(),operation,results)['idempotent_hit'] is True
        else:
            with pytest.raises(RecoveryConflict):
                matrix.message_finish_tools(identity(),operation,[{'id':'call','content':'changed'}])
    assert len(uploads) == 1 and len(writes) == 2


def test_receipt_upload_timeout_retries_only_the_same_immutable_bytes():
    with patch.dict(os.environ, {"AGENTTEAMS_STORAGE_PREFIX": "storage/tenant"}), \
            patch("worker_matrix_state.subprocess.run", side_effect=[subprocess.TimeoutExpired('mc', 30), Mock()]) as upload:
        artifact = WorkerMatrix._message_artifact(entry(), b'verified native result')
    assert upload.call_count == 2
    assert upload.call_args_list[0] == upload.call_args_list[1]
    assert artifact['sha256'] == hashlib.sha256(b'verified native result').hexdigest()


def test_receipt_storage_failure_stays_a_controlled_unavailable_result():
    with patch.dict(os.environ, {"AGENTTEAMS_STORAGE_PREFIX": "storage/tenant"}), \
            patch("worker_matrix_state.subprocess.run", side_effect=subprocess.CalledProcessError(1, 'mc')) as upload, \
            pytest.raises(RecoveryUnavailable, match='keep the operation pending'):
        WorkerMatrix._message_artifact(entry(), b'verified native result')
    assert upload.call_count == 1


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


def test_media_send_stages_result_and_bytes_then_atomically_releases_only_reply_delivery():
    source = entry()
    writes, uploads = [], []

    def request(method, path, *args):
        if method == "POST":
            cp = args[0]["checkpoint"]
            writes.append((copy.deepcopy(cp), list(uploads)))
            source["execution"]["checkpoint"] = cp
            source["recovery"] = "deliver" if cp.get("delivery") == "pending" else "verify"
        return copy.deepcopy(source)

    matrix = WorkerMatrix("hermes", recovery=Mock(process="current-process", device_delivery=True, request=request))
    calls = [{"id": "plot.png", "name": "matrix.send_file", "arguments": {"sha256": hashlib.sha256(b"image").hexdigest()}}]
    with patch.dict(os.environ, {"AGENTTEAMS_STORAGE_PREFIX": "storage/tenant"}), patch(
        "worker_matrix_state.subprocess.run", side_effect=lambda *args, **kwargs: uploads.append(kwargs["input"])
    ):
        op = matrix.message_begin_tools(identity(), calls, reply="Here is the plot", output_bytes=[b"image"])
        started, persisted = writes[0]
        assert set(persisted) == {b"image", b"Here is the plot"}
        assert started["result"]["sha256"] == hashlib.sha256(b"Here is the plot").hexdigest()
        assert started["artifacts"][0]["sha256"] == calls[0]["arguments"]["sha256"]
        assert "delivery" not in started and started["operation"]["state"] == "started"
        with pytest.raises(RecoveryConflict):
            matrix.message_save_reply(identity(), "replace output")
        with pytest.raises(RecoveryConflict):
            matrix.message_finish_tools(identity(), op, [], deliver_result=True)
        assert len(writes) == 1
        saved = matrix.message_finish_tools(identity(), op, [{"id": "plot.png", "content": {"event_id": "$media"}}], deliver_result=True)
        final = saved["execution"]["checkpoint"]
        assert final["result"] == started["result"] and final["artifacts"][0] == started["artifacts"][0]
        assert final["delivery"] == "pending" and final["reply_attempt"] == {"state": "ready"}
        assert final["version"] == started["version"] + 1 and final["operation"]["state"] == "verified"
        assert final["completed_steps"] == ["one", op] and b"$media" in uploads[-1]


def test_three_confirmed_tool_failures_allow_an_alternative_without_task_escalation():
    source = entry()
    def request(method, path, body=None, *args):
        if body and body.get('action') == 'checkpoint':
            cp = body['checkpoint']
            source['execution'].update(checkpoint=cp, state='needs_verification' if cp['operation']['state'] == 'started' else 'active')
            source['recovery'] = 'verify' if cp['operation']['state'] == 'started' else 'resume'
        return copy.deepcopy(source)
    recovery = Mock(process='current-process', request=Mock(side_effect=request))
    matrix = WorkerMatrix('hermes', recovery=recovery)
    with patch.object(matrix, '_message_artifact', return_value={'key':'receipt','sha256':'hash','size':1}):
        for attempt in range(3):
            call = {'id':f'failed-{attempt}', 'name':'mcp__fixture__lookup', 'arguments':{'query':'same'}}
            op = matrix.message_begin_tools(identity(), [call])
            saved = matrix.message_finish_tools(identity(), op, [{'id':call['id'], 'status':'error', 'content':'Returned service failure'}])
            assert saved['execution']['state'] == 'active'
        matrix.message_begin_tools(identity(), [{'id':'alternative','name':'mcp__other__search','arguments':{}}])
    assert all('/submit' not in call.args[1] for call in recovery.request.call_args_list)
