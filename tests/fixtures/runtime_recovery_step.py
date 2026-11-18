#!/usr/bin/env python3
"""Deterministic side-effect fixture invoked by the real Harness's model tool.

This never starts a model or resumes a Task by itself. A separately owned
HTTP ledger executes/counts effects; Worker-written objects are only artifacts.
"""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time
import urllib.parse
import urllib.request

sys.path.insert(0, "/opt/agentteams/scripts/lib")
from task_recovery import Recovery

task_id, team = sys.argv[1:]
direct_command = Path(sys.argv[0]).name == "agentteams-recovery-step"
controller_host = urllib.parse.urlparse(os.getenv("AGENTTEAMS_CONTROLLER_URL", "")).hostname or ""
fixture_prefix = controller_host.removesuffix("-controller")
if direct_command and (team != "recovery-team" or not re.fullmatch(r"live-[a-z0-9-]+-[0-9]{2,3}", task_id)
        or not re.fullmatch(r"recovery-1631-live-\d{8}-v\d+", fixture_prefix)
        or controller_host != fixture_prefix + "-controller"):
    raise ValueError("This command is only for the isolated recovery fixture")
recovery = Recovery()
task = recovery.current(task_id, team)
if not task or task["status"] != "in_progress":
    raise RuntimeError("ACK the assigned Task durably before running a step")
if direct_command and task.get("recovery_fixture", {}).get("oracle_url") != "http://" + fixture_prefix + "-oracle:8091":
    raise ValueError("Task does not reference the fixture's independent oracle")
cp = dict(task["execution"].get("checkpoint") or {})
steps = ["one", "two", "three"]
done = cp.get("completed_steps", [])
if done == steps:
    print("ALL_STEPS_DONE: submit the same task ID with output/step-*.json")
    sys.exit(0)
generation = task["execution"]["generation"]
if ((done == ["one"] and generation == 1 and not task["recovery_fixture"].get("pause_after_submit")) or
        (task["recovery_fixture"].get("return_to_source") and done == ["one", "two"] and generation == 2)):
    print("PAUSED_AT_STEP_BOUNDARY: end this turn. Keep Task in_progress; a replacement will continue without a new message.")
    sys.exit(0)
step = steps[len(done)]
key = f"teams/{team}/shared/tasks/{task_id}/output/step-{step}.json"
prefix = os.environ["AGENTTEAMS_STORAGE_PREFIX"].rstrip("/") + "/"


def write(key, data):
    subprocess.run(["mc", "pipe", prefix + key], input=data, check=True, capture_output=True)


def submit_and_wait_for_stop():
    """Save the canonical report through TeamHarness while the native tool is running."""
    if team != "recovery-team" or not re.fullmatch(r"recovery-1631-live-\d{8}-v\d+", fixture_prefix):
        raise ValueError("The saved-reply barrier requires the isolated fixture")
    home = Path("/root/agentteams-fs/agents/agent-a")
    os.environ.update(TEAMHARNESS_RUNTIME_CONFIG=str(home / "runtime/runtime.yaml"),
                      TEAMHARNESS_SHARED_DIR=str(home / "shared"))
    from task_recovery import RecoveryConflict

    server = Path("/opt/agentteams/plugins/teamharness/mcp/server.py")
    if not server.is_file():
        server = Path("/opt/agentteams/qwenpaw-builtin/plugins/teamharness/teamharness/mcp/server.py")
    report = f"# Task {task_id}\n\nAll three fixture effects were verified.\n"
    payload = {"taskId": task_id, "mainTaskId": task["main_task_id"], "status": "SUCCESS",
               "summary": "All three fixture effects verified", "resultMarkdown": report,
               "conclusions": ["Step one verified", "Step two verified", "Step three verified"]}
    request = {"jsonrpc": "2.0", "id": 1, "method": "tools/call",
               "params": {"name": "taskflow", "arguments": {"action": "submit_task", "payload": payload}}}
    response = subprocess.run([sys.executable, str(server)], input=json.dumps(request) + "\n",
                              check=True, capture_output=True, text=True, timeout=35)
    outcome = json.loads(json.loads(response.stdout)["result"]["content"][0]["text"])
    if not outcome.get("ok") or not outcome.get("synced"):
        raise RuntimeError("TeamHarness did not persist the report: " + str(outcome.get("error")))
    delivery = Recovery(device_delivery=True)
    for _ in range(3):
        snapshot = delivery.discover()
        entry = next(row for row in snapshot["tasks"] if row["team_id"] == team and row["task"]["task_id"] == task_id)
        try:
            saved = delivery.claim(snapshot, entry)
        except RecoveryConflict:
            continue  # Leader acceptance may have updated the metadata ETag.
        break
    else:
        raise RuntimeError("The fixture could not register the saved reply")
    if saved["saved_result"] != report or saved["execution"]["checkpoint"]["delivery"] != "pending":
        raise RuntimeError("The canonical report must be pending delivery")
    print("FAULT_AFTER_SUBMIT: waiting for process termination before the Matrix reply", flush=True)
    time.sleep(120)
    raise RuntimeError("Fault-injection barrier expired without process termination")


def source_was_read():
    from agentteams_receipts import RECEIPT_TYPE, credentials, matrix_request

    request = matrix_request(*credentials(Path("/root/agentteams-fs/agents/agent-a")))
    room = urllib.parse.quote(task["room_id"], safe="")
    rows = request("GET", f"/_matrix/client/v3/rooms/{room}/messages?dir=b&limit=100")["chunk"]
    assignments, reads = set(), set()
    for row in rows:
        content = row.get("content", {})
        if (row.get("type") == RECEIPT_TYPE and row.get("sender") == task["assigned_to"]
                and content.get("status") == "read"):
            reads.add(content.get("m.relates_to", {}).get("event_id"))
        notice = (content.get("org.agentteams.coordination") or {}).get("channel_meta", {}).get("teamharness_coordination", {})
        if (row.get("type") == "m.room.message" and notice.get("kind") == "task_assigned"
                and notice.get("task_id") == task_id and notice.get("delivery_id") == task["notifyTxn"]):
            assignments.add(row["event_id"])
    return bool(assignments & reads)


if generation == 1 and step == "one" and task["recovery_fixture"].get("wait_for_source_read"):
    # Return control to the native turn so Matrix input can be persisted.
    if not source_was_read():
        print("SOURCE_READ_PENDING: no effect started. Run the same helper again in this turn.")
        sys.exit(0)


operation = {"id": f"fixture:{task_id}:{step}", "state": "started"}


def effect(method):
    token = Path(os.getenv("AGENTTEAMS_AUTH_TOKEN_FILE") or "/var/run/secrets/agentteams/token").read_text().strip()
    query = urllib.parse.urlencode({"team": team, "task": task_id, "step": step})
    endpoint = "/intent?" if method == "REGISTER" else "/effect?"
    http_method = "POST" if method == "REGISTER" else method
    request = urllib.request.Request(task["recovery_fixture"]["oracle_url"] + endpoint + query,
        method=http_method, data=b"" if http_method == "POST" else None,
        headers={"Authorization": "Bearer " + token, "X-AgentTeams-Process-ID": recovery.process})
    with urllib.request.urlopen(request, timeout=20) as response:
        return response.read()


if cp.get("operation", {}).get("state") in {"started", "unknown"}:
    if cp["operation"]["id"] != operation["id"]:
        raise RuntimeError("Needs verification: operation outcome cannot be proved; no rerun")
    data = effect("GET")
    proof = json.loads(data)
    if task["recovery_fixture"].get("oracle_version") == 4 and proof.get("verification") == "never_authorized":
        # The oracle requires the same current owner, checkpoint and operation
        # to register against its durable negative evidence. HTTP errors or an
        # absent response grant no authority to execute.
        effect("REGISTER")
        data = effect("POST")
else:
    effect("REGISTER")
    for field in ("result", "delivery", "reply_txn", "reply_event", "reply_attempt"):
        cp.pop(field, None)
    cp.update(version=cp.get("version", 0) + 1, completed_steps=done, remaining_steps=steps[len(done):], operation=operation)
    recovery.checkpoint(task_id, team, cp)
    data = effect("POST")
    if task["recovery_fixture"].get("pause_after_effect") == step and task["execution"]["generation"] == 1:
        print("FAULT_AFTER_EFFECT: waiting for process termination before publishing the checkpoint", flush=True)
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            time.sleep(1)
        raise RuntimeError("Fault-injection barrier expired without process termination")
write(key, data)
digest = hashlib.sha256(data).hexdigest()
operation.update(state="verified", evidence="Independent effect ledger; persisted output SHA256=" + digest + "; step=" + step + "; count=1")
cp.update(version=cp["version"] + 1, completed_steps=done + [step], remaining_steps=steps[len(done) + 1:], operation=operation,
    artifacts=(cp.get("artifacts") or []) + [{"key": key, "sha256": digest, "size": len(data)}])
recovery.checkpoint(task_id, team, cp)
if cp["completed_steps"] == steps and task["recovery_fixture"].get("pause_after_submit"):
    submit_and_wait_for_stop()
print(json.dumps({"step": step, "checkpoint_version": cp["version"], "remaining_steps": cp["remaining_steps"], "sha256": digest}))
