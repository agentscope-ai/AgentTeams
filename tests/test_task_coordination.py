import copy
import importlib.util
from pathlib import Path

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
