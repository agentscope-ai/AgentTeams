"""Common batch/cursor preparation, independent of a native Matrix SDK."""
import copy
from pathlib import Path
import sys
from unittest.mock import Mock, patch

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "shared/lib"))
from task_recovery import RecoveryConflict
from task_recovery_rpc import execute
from worker_matrix_state import WorkerMatrix


def fixture(runtime="hermes"):
    recovery = Mock(process="actual-process")
    matrix = WorkerMatrix(runtime, recovery=recovery)
    snapshot = {"runtime": runtime, "instance_id": "actual-instance", "etag": "read-version",
        "state": {"active_start": {"runtime": runtime, "instance_id": "actual-instance",
            "process_id": "actual-process"}, "consumers": {}, "rooms": {}, "pending": []}}
    recovery.request.return_value = {"etag": "saved", "state": snapshot["state"]}
    return matrix, snapshot, recovery


def event(identifier, room="!room:test"):
    return {"room_id": room, "event_id": identifier, "intent": "message",
        "input": {"sender": "@human:test", "type": "m.room.message",
            "origin_server_ts": 12, "content": {"msgtype": "m.text", "body": identifier}}}


@pytest.mark.parametrize("runtime", ["codex", "hermes", "qwenpaw", "deepseek-harness", "openclaw", "openhuman"])
def test_only_same_native_consumer_can_restore_a_cursor(runtime):
    matrix, snapshot, recovery = fixture(runtime)
    identity = {"account": "@worker:test", "device": "device", "format": "native/v1"}
    snapshot["state"]["consumers"] = {
        "other-runtime": {**identity, "runtime": "other", "token": "foreign-runtime"},
        "other-device": {**identity, "runtime": runtime, "device": "old", "token": "foreign-device"},
        "matching": {**identity, "runtime": runtime, "token": "same-native"}}
    assert matrix.consumer(snapshot, **identity)["token"] == "same-native"
    assert matrix.consumer(snapshot, **{**identity, "format": "native/v2"})["token"] == ""
    recovery.request.assert_not_called()


def test_batch_keeps_server_order_and_existing_gaps_before_cursor_commit():
    matrix, snapshot, recovery = fixture()
    snapshot["state"]["rooms"]["!room:test"] = {"through": "$old", "gaps": ["older-gap"]}
    consumer = {"runtime": "hermes", "account": "@worker:test", "format": "native/v1", "token": "previous"}
    snapshot["state"]["consumers"]["native"] = consumer
    inputs = [event("$z"), event("$a")]
    original = copy.deepcopy(snapshot)
    matrix.sync(snapshot, consumer, "next", [{"room_id": "!room:test", "inputs": inputs,
        "limited": True, "prev_batch": "new-gap"}])
    args = recovery.request.call_args.args
    assert args[:2] == ("POST", "runtime/matrix")
    assert args[2]["inputs"] == inputs
    assert args[2]["rooms"] == {"!room:test": {"through": "$a", "gaps": ["older-gap", "new-gap"]}}
    assert args[2]["consumer"] == {**consumer, "device": "", "token": "next", "from_token": "previous"}
    assert args[3] == "read-version"
    assert snapshot == original


def test_new_native_consumer_must_reconnect_existing_room_coverage():
    matrix, snapshot, recovery = fixture("qwenpaw")
    snapshot["state"]["rooms"]["!room:test"] = {"through": "$old"}
    consumer = matrix.consumer(snapshot, account="@worker:test", device="new", format="nio/v1")
    matrix.sync(snapshot, consumer, "next", [{"room_id": "!room:test", "inputs": [event("$new")],
        "limited": False, "prev_batch": "history"}])
    assert recovery.request.call_args.args[2]["rooms"]["!room:test"]["gaps"] == ["history"]


def test_timeline_overlap_does_not_invent_a_new_gap():
    matrix, snapshot, recovery = fixture()
    snapshot["state"]["rooms"]["!room:test"] = {"through": "$old"}
    consumer = matrix.consumer(snapshot, account="@worker:test", format="native/v1")
    matrix.sync(snapshot, consumer, "next", [{"room_id": "!room:test", "inputs": [event("$old"), event("$new")],
        "limited": True, "prev_batch": "history"}])
    assert recovery.request.call_args.args[2]["rooms"]["!room:test"]["gaps"] == []


@pytest.mark.parametrize("timeline", [
    {"room_id": "!room:test", "inputs": [], "limited": True},
    {"room_id": "!room:test", "inputs": [event("$new")], "limited": True},
    {"room_id": "!room:test", "inputs": [event("$new", "!foreign:test")]},
])
def test_incomplete_or_mis_scoped_timeline_cannot_advance_native_cursor(timeline):
    matrix, snapshot, recovery = fixture()
    consumer = matrix.consumer(snapshot, account="@worker:test", format="native/v1")
    with pytest.raises((ValueError, RecoveryConflict)):
        matrix.sync(snapshot, consumer, "next", [timeline])
    recovery.request.assert_not_called()


@pytest.mark.parametrize("previous,limited", [("", True), ("$old", True), ("$old", False)])
def test_filtered_window_keeps_pagination_without_inventing_an_input(previous, limited):
    matrix, snapshot, recovery = fixture()
    room = "!room:test"
    snapshot["state"]["rooms"][room] = {"through": previous}
    consumer = matrix.consumer(snapshot, account="@worker:test", format="native/v1")
    matrix.sync(snapshot, consumer, "next", [{"room_id": room, "inputs": [],
        "limited": limited, "prev_batch": "older"}])
    body = recovery.request.call_args.args[2]
    assert body["inputs"] == []
    assert body["rooms"] == {room: {"through": previous, "gaps": ["older"]}}
    assert body["consumer"]["token"] == "next"


def test_old_native_cursor_cannot_be_relabelled_with_a_new_etag():
    matrix, snapshot, recovery = fixture()
    consumer = {"runtime": "hermes", "account": "@worker:test", "format": "native/v1", "token": "old"}
    snapshot["state"]["consumers"]["current"] = {**consumer, "token": "newer"}
    with pytest.raises(RecoveryConflict):
        matrix.sync(snapshot, consumer, "next", [])
    recovery.request.assert_not_called()


@pytest.mark.parametrize("field,value", [("runtime", "qwenpaw"), ("process_id", "old-process"),
                                          ("instance_id", "old-instance")])
def test_native_cursor_restore_cannot_bypass_actual_start(field, value):
    matrix, snapshot, recovery = fixture()
    snapshot["state"]["active_start"][field] = value
    with pytest.raises(RecoveryConflict):
        matrix.consumer(snapshot, account="@worker:test", format="native/v1")
    recovery.request.assert_not_called()


def test_native_rpc_uses_same_common_batch_preparation():
    matrix, snapshot, recovery = fixture()
    consumer = {"runtime": "hermes", "account": "@worker:test", "format": "native/v1", "token": ""}
    with patch("task_recovery_rpc.enabled", return_value=True), patch("task_recovery_rpc.Recovery", return_value=recovery):
        execute({"action": "matrix.sync", "runtime": "hermes", "snapshot": snapshot,
            "consumer": consumer, "next_batch": "next", "timelines": []})
    assert recovery.request.call_args.args[2]["consumer"]["token"] == "next"
