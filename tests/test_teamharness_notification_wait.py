"""The Team E2E must check actual coordination events, not prose/tool previews."""
import json
import os
from pathlib import Path
import re
import subprocess

import pytest

SCRIPT = Path(__file__).with_name("test-26-qwenpaw-teamharness-plugin-mode.sh")


def wait(event, kind="task_assigned", sender="@leader:test", recipient="@worker:test"):
    source = SCRIPT.read_text()
    match = re.search(r"^_wait_coordination_message\(\) \{.*?^\}", source, re.M | re.S)
    assert match, "Team E2E still waits for legacy notification text"
    run = subprocess.run(["bash", "-c", match.group() + '\n'
        'matrix_read_messages() { printf "%s" "$FIXTURE"; }\n'
        'sleep() { :; }\n'
        '_wait_coordination_message "$KIND" "$SENDER" "$RECIPIENT" 1'],
        env={"PATH": os.environ["PATH"], "FIXTURE": json.dumps({"chunk": [event]}),
             "KIND": kind, "SENDER": sender, "RECIPIENT": recipient, "TASK_ID": "task-1",
             "PROJECT_ID": "project-1", "TEAM_ROOM": "!team:test", "ADMIN_TOKEN": "fixture"},
        capture_output=True, text=True)
    return run.returncode == 0


def packet(kind="task_assigned", sender="@leader:test", recipient="@worker:test"):
    return {"event_id": "$receipt", "type": "m.room.message", "sender": sender,
            "content": {"body": "任务已分配。", "org.agentteams.coordination": {"channel_meta": {
                "sender_id": sender, "agent_matrix_user_id": recipient,
                "teamharness_coordination": {"id": "a" * 32, "kind": kind,
                    "task_id": "task-1", "project_id": "project-1"}}}}}


@pytest.mark.parametrize("kind,sender,recipient", [
    ("task_assigned", "@leader:test", "@worker:test"),
    ("task_submitted", "@worker:test", "@leader:test"),
])
def test_current_notification_does_not_need_legacy_text(kind, sender, recipient):
    assert wait(packet(kind, sender, recipient), kind, sender, recipient)


@pytest.mark.parametrize("field,value", [("task_id", "old-task"), ("project_id", "other-project"),
    ("kind", "task_submitted"), ("id", "")])
def test_another_task_or_event_kind_cannot_satisfy_wait(field, value):
    event = packet()
    event["content"]["org.agentteams.coordination"]["channel_meta"]["teamharness_coordination"][field] = value
    assert not wait(event)


def test_forged_sender_and_tool_preview_do_not_prove_assignment():
    event = packet()
    event["sender"] = "@other:test"
    assert not wait(event)
    event = packet()
    event["content"] = {"body": "TASK_ASSIGNED: task-1 @worker:test"}
    assert not wait(event)


@pytest.mark.parametrize("kind,sender,recipient,body", [
    ("task_assigned", "@leader:test", "@worker:test", "@worker:test You are assigned task **task-1**: fixture"),
    ("task_submitted", "@worker:test", "@leader:test", "@leader:test TASK_COMPLETED: task-1 - Result: shared/tasks/task-1/result.md"),
])
def test_default_legacy_transport_still_proves_real_notification(kind, sender, recipient, body):
    event = packet(kind, sender, recipient)
    event["content"] = {"body": body, "m.mentions": {"user_ids": [recipient]}}
    assert wait(event, kind, sender, recipient)
    event["content"]["org.agentteams.progress_only"] = True
    assert not wait(event, kind, sender, recipient)
    event["content"].pop("org.agentteams.progress_only")
    event["content"]["m.mentions"]["user_ids"] = ["@other:test"]
    assert not wait(event, kind, sender, recipient)
