"""All adapters retain the same immutable accepted-input identity."""
from pathlib import Path
import sys
from unittest.mock import Mock, patch

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "shared/lib"))
from task_recovery import RecoveryConflict
from worker_matrix_state import WorkerMatrix, matrix_event_key
from task_recovery_rpc import execute


@pytest.mark.parametrize("runtime", ["hermes", "qwenpaw"])
def test_accept_uses_self_task_discovery_and_never_fabricates_consumer(runtime):
    recovery = Mock()
    recovery.discover.return_value = {"tasks": [{"team_id": "team", "task": {
        "task_id": "T", "room_id": "!room:test", "session_state": "waiting_user"}}]}
    matrix = WorkerMatrix(runtime, recovery=recovery)
    matrix.start = Mock(return_value={"etag": "start"})
    matrix.input = Mock(return_value=None)
    matrix.ingest = Mock()
    source = {"sender": "@leader:test", "type": "m.room.message", "origin_server_ts": 42,
              "content": {"msgtype": "m.text", "body": "accepted input"}}
    key = matrix.accept("!room:test", "$event", source, "message", task_id="T")
    assert key == matrix_event_key("!room:test", "$event")
    assert matrix.ingest.call_args.args == ({"etag": "start"}, None, [{
        "room_id": "!room:test", "event_id": "$event", "intent": "message", "input": source,
        "work": {"kind": "task", "team_id": "team", "task_id": "T"}}])
    recovery.discover.assert_called_once_with(include_waiting=True)
    recovery.request.assert_not_called()


def test_replayed_input_retains_original_classification_after_task_reassignment():
    recovery = Mock()
    matrix = WorkerMatrix("hermes", recovery=recovery)
    matrix.start = Mock(return_value={})
    matrix.input = Mock(return_value={"intent": "message", "work": {
        "kind": "task", "team_id": "original-team", "task_id": "T"}})
    matrix.ingest = Mock()
    matrix.accept("!room:test", "$event", {"content": {"body": "original bytes"}}, "context", task_id="T")
    accepted = matrix.ingest.call_args.args[2][0]
    assert accepted["intent"] == "message"
    assert accepted["work"] == {"kind": "task", "team_id": "original-team", "task_id": "T"}
    recovery.discover.assert_not_called()


@pytest.mark.parametrize("matches", [[], [{"team_id": team, "task": {
    "task_id": "T", "room_id": "!room:test"}} for team in ["one", "two"]]])
def test_new_task_input_requires_unambiguous_owned_team_and_room(matches):
    matrix = WorkerMatrix("hermes", recovery=Mock())
    matrix.start = Mock(return_value={})
    matrix.input = Mock(return_value=None)
    matrix.ingest = Mock()
    matrix.recovery.discover.return_value = {"tasks": matches}
    with pytest.raises(RecoveryConflict):
        matrix.accept("!room:test", "$event", {}, "message", task_id="T")
    matrix.ingest.assert_not_called()


def test_non_python_adapters_use_the_same_acceptance_contract():
    with patch("task_recovery_rpc.enabled", return_value=True), patch("task_recovery_rpc.Recovery"), \
            patch.object(WorkerMatrix, "accept", return_value="key") as accept:
        source = {"content": {"body": "input"}}
        assert execute({"action": "matrix.accept", "runtime": "codex", "room_id": "!room:test",
            "event_id": "$event", "source": source, "intent": "message", "task_id": "T"}) == "key"
        accept.assert_called_once_with("!room:test", "$event", source, "message", task_id="T")
