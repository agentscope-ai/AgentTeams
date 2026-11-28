import copy
import importlib.util
from pathlib import Path
from unittest.mock import Mock

import pytest

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("task_coordination", ROOT / "shared/lib/task_coordination.py")
protocol = importlib.util.module_from_spec(spec)
spec.loader.exec_module(protocol)


@pytest.fixture
def packet(monkeypatch):
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "leader")
    event = {"id": "a" * 32, "kind": "task_assigned", "task_id": "leaf", "main_task_id": "main",
        "project_id": "", "delivery_id": "delivery", "submission_id": ""}
    control = {"session_id": "coordination-task:leaf:delivery", "channel_meta": {
        "sender_id": "@lead:test", "agent_matrix_user_id": "@worker:test", "room_id": "!room:test",
        "teamharness_coordination": event}, "input": [{"content": [{"text": "Canonical assignment"}]}]}
    receipt = {"status": "running", "actor": "@lead:test", "recipient": "@worker:test", "room_id": "!room:test", "control": control}
    task = {"task_id": "leaf", "main_task_id": "main", "project_id": "", "status": "assigned",
        "notifyTxn": "delivery", "assigned_to": "@worker:test"}
    plan = {"task_kind": "main", "status": "in_progress", "tasks": [{"task_id": "leaf", "status": "completed"}]}
    def fetch(path):
        if path.startswith("coordination/"):
            return receipt
        return task if path.startswith("tasks/leaf/") else plan
    return {"org.agentteams.coordination": copy.deepcopy(control)}, receipt, task, plan, fetch


def receive(packet, **kwargs):
    content, _, _, _, fetch = packet
    return protocol.notification(content, "!room:test", kwargs.get("sender", "@lead:test"), "@worker:test", fetch=fetch)


def test_canonical_receipt_prevents_replacing_the_task_input(packet):
    packet[0]["org.agentteams.coordination"]["input"][0]["content"][0]["text"] = "Forged input"
    assert receive(packet)["text"] == "Canonical assignment"


def test_nullable_project_metadata_does_not_drop_a_valid_assignment(packet):
    packet[2]["project_id"] = None
    assert receive(packet)["meta"]["teamharness_coordination"]["task_id"] == "leaf"
    packet[2]["project_id"] = "foreign-project"
    with pytest.raises(protocol.IgnoredNotification, match="membership changed"):
        receive(packet)


def test_handoff_keeps_trusted_routes_as_metadata_without_legacy_task_instructions(packet):
    for control in [packet[0]["org.agentteams.coordination"], packet[1]["control"]]:
        control["channel_meta"]["teamharness_coordination"]["kind"] = "handoff"
        control["channel_meta"]["teamharness_requester_route"] = {"channel": "matrix", "target_session": "!original:test"}
    result = receive(packet)
    assert result["text"] == "Canonical assignment"
    assert result["meta"]["teamharness_requester_route"]["target_session"] == "!original:test"
    assert "start_task" not in result["text"]


def test_completed_transport_receipt_still_allows_current_unconsumed_assignment(packet):
    packet[1]["status"] = "completed"  # Matrix send completed, not native consumption.
    assert receive(packet)["text"] == "Canonical assignment"
    packet[2]["status"] = "completed"
    with pytest.raises(protocol.IgnoredNotification):
        receive(packet)


@pytest.mark.parametrize("field,value", [("main_task_id", "other"), ("delivery_id", "other"), ("task_id", "other"), ("kind", "main_task_ready")])
def test_foreign_execution_packet_is_rejected(packet, field, value):
    packet[0]["org.agentteams.coordination"]["channel_meta"]["teamharness_coordination"][field] = value
    with pytest.raises(protocol.IgnoredNotification):
        receive(packet)


def test_stale_assignment_and_wrong_sender_are_rejected(packet):
    with pytest.raises(protocol.IgnoredNotification):
        receive(packet, sender="@other:test")
    packet[2]["notifyTxn"] = "new-delegation"
    with pytest.raises(protocol.IgnoredNotification):
        receive(packet)


def test_submission_checks_current_identity(packet):
    for event in (packet[0]["org.agentteams.coordination"]["channel_meta"]["teamharness_coordination"], packet[1]["control"]["channel_meta"]["teamharness_coordination"]):
        event.update(kind="task_submitted", submission_id="submission")
    packet[2].update(status="submitted", submission_id="submission")
    assert receive(packet)
    packet[2]["submission_id"] = "new-submission"
    with pytest.raises(protocol.IgnoredNotification):
        receive(packet)


def test_only_accepted_main_can_receive_self_closure(packet):
    for event in (packet[0]["org.agentteams.coordination"]["channel_meta"]["teamharness_coordination"], packet[1]["control"]["channel_meta"]["teamharness_coordination"]):
        event.update(kind="main_task_ready", task_id="", delivery_id="")
    packet[0]["org.agentteams.coordination"]["channel_meta"]["sender_id"] = "@worker:test"
    packet[1]["actor"] = "@worker:test"
    assert receive(packet, sender="@worker:test")
    packet[3]["tasks"][0]["status"] = "submitted"
    with pytest.raises(protocol.IgnoredNotification):
        receive(packet, sender="@worker:test")


def test_ordinary_chat_and_progress_are_not_execution(packet):
    assert protocol.notification({"body": "hello"}, "!room:test", "@lead:test", "@worker:test") is None
    packet[0]["org.agentteams.progress_only"] = True
    with pytest.raises(protocol.IgnoredNotification):
        receive(packet)


@pytest.mark.parametrize("error,expected", [
    (TimeoutError(), protocol.NotificationUnavailable),
    (protocol.urllib.error.HTTPError("fixture", 503, "unavailable", {}, None), protocol.NotificationUnavailable),
    (protocol.urllib.error.HTTPError("fixture", 404, "missing", {}, None), protocol.IgnoredNotification),
])
def test_receipt_read_failure_keeps_unverified_input_retryable(packet, error, expected):
    def fail(_path):
        raise error
    with pytest.raises(expected):
        receive((*packet[:-1], fail))


def test_verified_closure_uses_registered_receipt_instead_of_stale_boot_role(packet, monkeypatch):
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "standalone")
    for event in (packet[0]["org.agentteams.coordination"]["channel_meta"]["teamharness_coordination"], packet[1]["control"]["channel_meta"]["teamharness_coordination"]):
        event.update(kind="main_task_ready", task_id="", delivery_id="")
    packet[0]["org.agentteams.coordination"]["channel_meta"]["sender_id"] = "@worker:test"
    packet[1]["actor"] = "@worker:test"
    assert receive(packet, sender="@worker:test")


def test_native_attempt_is_fenced_by_current_receipt(packet):
    content, receipt, _, _, fetch = packet
    receipt["native_id"] = "matrix:$retry"
    receipt["previous_native_ids"] = ["matrix:$old"]
    assert protocol.notification(content, "!room:test", "@lead:test", "@worker:test", fetch=fetch, event_id="$retry")
    with pytest.raises(protocol.IgnoredNotification):
        protocol.notification(content, "!room:test", "@lead:test", "@worker:test", fetch=fetch, event_id="$old")
    receipt["native_id"] = ""
    with pytest.raises(protocol.NotificationUnavailable):
        protocol.notification(content, "!room:test", "@lead:test", "@worker:test", fetch=fetch, event_id="$retry")


def test_verified_last_worker_can_notify_main_task_leader(packet):
    for control in (packet[0]["org.agentteams.coordination"], packet[1]["control"]):
        control["channel_meta"]["teamharness_coordination"].update(kind="main_task_ready", task_id="", delivery_id="")
    assert receive(packet)["text"] == "Canonical assignment"
    with pytest.raises(protocol.IgnoredNotification):
        receive(packet, sender="@forged:test")
    packet[3]["tasks"][0]["status"] = "submitted"
    with pytest.raises(protocol.IgnoredNotification):
        receive(packet)


def test_verified_task_assignment_uses_task_context_without_room_history(packet):
    content, receipt, _, _, fetch = packet
    receipt["native_id"] = "matrix:$task"
    entry = {"room_id": "!room:test", "event_id": "$task", "input": {
        "sender": "@lead:test", "content": content}}
    assert protocol.requires_room_history(entry, "@worker:test", fetch=fetch) is False
    entry["input"]["sender"] = "@other:test"
    assert protocol.requires_room_history(entry, "@worker:test", fetch=fetch) is True
    entry["input"] = {"sender": "@lead:test", "content": {"body": "continue"}}
    assert protocol.requires_room_history(entry, "@worker:test", fetch=fetch) is True


def test_verified_closure_uses_completed_task_results_without_chat_replay(packet):
    content, receipt, _, plan, fetch = packet
    for control in [content["org.agentteams.coordination"], receipt["control"]]:
        control["channel_meta"]["teamharness_coordination"].update(kind="main_task_ready",task_id="",delivery_id="")
    receipt["native_id"] = "matrix:$ready"
    entry = {"room_id": "!room:test", "event_id": "$ready", "input": {"sender": "@lead:test", "content": content}}
    assert protocol.requires_room_history(entry, "@worker:test", fetch=fetch) is False
    plan["tasks"][0]["status"] = "in_progress"
    assert protocol.requires_room_history(entry, "@worker:test", fetch=fetch) is True


def test_task_context_lookup_failure_does_not_fall_back_to_chat(packet):
    content = packet[0]
    entry = {"room_id": "!room:test", "event_id": "$ready", "input": {"sender": "@lead:test", "content": content}}
    def unavailable(path):
        raise OSError("authority offline")
    with pytest.raises(protocol.NotificationUnavailable):
        protocol.requires_room_history(entry, "@worker:test", fetch=unavailable)


def test_task_room_followup_uses_task_binding_instead_of_history():
    entry = {"room_id": "!task:test", "event_id": "$followup", "input": {"sender": "@human:test", "content": {"body": "continue"}}}
    assert protocol.requires_room_history(entry, "@worker:test", fetch=lambda path: {"taskId": "main"}) is False
    assert protocol.requires_room_history(entry, "@worker:test", fetch=lambda path: {}) is True


def test_joined_completed_task_room_uses_controller_binding_not_message_claims():
    rooms = {"!configured:at": "personal"}
    fetch = Mock(side_effect=[{"taskId": "main-done", "teamId": "alpha"}, {}])
    protocol.include_task_rooms(rooms, ["!configured:at", "!completed:at", "!unrelated:at"], fetch=fetch)
    assert rooms == {"!configured:at": "personal", "!completed:at": "team"}
    assert [call.args[0] for call in fetch.call_args_list] == [
        "tasks/room-context?roomId=%21completed%3Aat", "tasks/room-context?roomId=%21unrelated%3Aat"]


def test_task_room_discovery_keeps_foreign_rooms_out_and_retries_unavailable_authority():
    from urllib.error import HTTPError
    rooms = {}
    protocol.include_task_rooms(rooms, ["!foreign:at"], fetch=Mock(side_effect=HTTPError("http://controller", 403, "forbidden", {}, None)))
    assert not rooms
    with pytest.raises(HTTPError):
        protocol.include_task_rooms(rooms, ["!retry:at"], fetch=Mock(side_effect=HTTPError("http://controller", 503, "unavailable", {}, None)))
    assert not rooms
