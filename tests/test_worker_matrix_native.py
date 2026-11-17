"""Native read/persistence facts never imply tool or Task completion."""
import copy
from pathlib import Path
import sys
from unittest.mock import Mock, patch

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "shared/lib"))
from task_recovery import RecoveryConflict
from task_recovery_rpc import execute
from worker_matrix_state import WorkerMatrix, matrix_event_key


def view():
    return {"runtime": "hermes", "instance_id": "actual/start", "etag": "input-etag", "current": True, "input": {
        "room_id": "!room:test", "event_id": "$event", "input_state": "native_committed", "native_commit": {
            "runtime": "hermes", "start": {"runtime": "hermes", "instance_id": "actual/start", "process_id": "actual-process"},
            "native_ref": "session/row1", "native_digest": "a" * 64}}}


def test_native_input_requires_persisted_row_content_and_current_start():
    matrix = WorkerMatrix("hermes", recovery=Mock(process="actual-process"))
    snapshot = view()
    assert matrix.native_current(snapshot, "session/row1", "a" * 64)
    for ref, digest in [(None, "a" * 64), ("session/row1", None), ("session/row2", "a" * 64), ("session/row1", "b" * 64)]:
        assert not matrix.native_current(snapshot, ref, digest)
    for field in ["process_id", "instance_id", "runtime"]:
        old = copy.deepcopy(snapshot)
        old["input"]["native_commit"]["start"][field] = "old"
        assert not matrix.native_current(old, "session/row1", "a" * 64)
    snapshot["current"] = False
    assert not matrix.native_current(snapshot, "session/row1", "a" * 64)


def test_native_commit_preserves_the_input_etag_and_does_not_complete_work():
    recovery = Mock(process="actual-process")
    matrix = WorkerMatrix("hermes", recovery=recovery)
    key = matrix_event_key("!room:test", "$event")
    matrix.native_committed(key, view(), "session/row1", "a" * 64)
    assert recovery.request.call_args.args == ("POST", "runtime/matrix/events/" + key + "/native", {
        "instance_id": "actual/start", "process_id": "actual-process", "native_ref": "session/row1", "native_digest": "a" * 64}, "input-etag")
    assert recovery.request.call_count == 1
    with pytest.raises(RecoveryConflict):
        matrix.native_committed(matrix_event_key("!room:test", "$other"), view(), "session/row1", "a" * 64)
    assert recovery.request.call_count == 1


def test_backfill_rpc_keeps_original_page_and_pagination_snapshot():
    with patch("task_recovery_rpc.enabled", return_value=True), patch("task_recovery_rpc.Recovery") as factory:
        recovery = factory.return_value
        recovery.process = "actual-process"
        snapshot = {"state": {"active_start": {"instance_id": "actual/start", "process_id": "actual-process", "runtime": "hermes"}}, "etag": "root-etag"}
        recovery.request.return_value = {"state": snapshot["state"], "etag": "next-etag"}
        page = {"room_id": "!room:test", "gap_id": "gap", "from_token": "current-page", "next_token": "next-page", "inputs": []}
        result = execute({"action": "matrix.backfill", "runtime": "hermes", "snapshot": snapshot, "page": page})
        assert recovery.request.call_args.args == ("POST", "runtime/matrix", {
            "action": "backfill", "instance_id": "actual/start", "process_id": "actual-process", "backfill": page}, "root-etag")
        assert result["etag"] == "next-etag"
