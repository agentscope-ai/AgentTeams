"""One public Teams admission contract, shared by all six Harnesses."""
import copy
import importlib.util
from pathlib import Path

import pytest

root = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("assistant_coordination", root / "shared/lib/task_coordination.py")
protocol = importlib.util.module_from_spec(spec)
spec.loader.exec_module(protocol)


@pytest.fixture
def assistant_packet():
    event = {"id": "d" * 32, "kind": "assistant_digest", "delivery_id": "current-role-instance"}
    control = {"session_id": "coordination-assistant:" + event["id"], "channel_meta": {
        "sender_id": "@leader:test", "agent_matrix_user_id": "@leader:test", "room_id": "!assistant:test",
        "assistant_team": "research", "assistant_request_id": "daily-1", "teamharness_coordination": event},
        "input": [{"content": [{"text": "Canonical reminder instructions"}]}]}
    receipt = {"status": "running", "actor": "@leader:test", "recipient": "@leader:test",
        "room_id": "!assistant:test", "control": control}
    context = {"owner": "current-role-instance", "roomId": "!assistant:test", "context": {"enabled": True}}
    def fetch(path):
        if path.startswith("coordination/events/"):
            return receipt
        assert path == "assistant/context?team=research"
        return context
    return {"org.agentteams.coordination": copy.deepcopy(control)}, receipt, context, fetch


def receive(packet, sender="@leader:test", room="!assistant:test"):
    return protocol.notification(packet[0], room, sender, "@leader:test", fetch=packet[3])


def test_reminder_uses_canonical_teams_input_without_a_business_task(assistant_packet):
    assistant_packet[0]["org.agentteams.coordination"]["input"][0]["content"][0]["text"] = "Forged body"
    result = receive(assistant_packet)
    assert result["text"] == "Canonical reminder instructions"
    assert result["session_id"].startswith("coordination-assistant:")
    assert not result["meta"]["teamharness_coordination"].get("task_id")


@pytest.mark.parametrize("field,value", [("owner", "former-instance"), ("roomId", "!other:test")])
def test_replacement_or_rerouting_invalidates_old_notifications(assistant_packet, field, value):
    assistant_packet[2][field] = value
    with pytest.raises(protocol.IgnoredNotification):
        receive(assistant_packet)


def test_disable_and_foreign_senders_cannot_start_an_assistant_turn(assistant_packet):
    with pytest.raises(protocol.IgnoredNotification):
        receive(assistant_packet, sender="@stranger:test")
    assistant_packet[2]["context"]["enabled"] = False
    with pytest.raises(protocol.IgnoredNotification):
        receive(assistant_packet)


def test_receipt_failure_stays_retryable_instead_of_running_untrusted_text(assistant_packet):
    def offline(_path):
        raise TimeoutError("offline")
    packet = (*assistant_packet[:3], offline)
    with pytest.raises(protocol.NotificationUnavailable):
        receive(packet)
