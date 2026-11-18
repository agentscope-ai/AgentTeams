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
if direct_command and (team != "recovery-team" or not re.fullmatch(r"live-[a-z0-9-]+-\d{2}", task_id)
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
if ((done == ["one"] and generation == 1) or
        (task["recovery_fixture"].get("return_to_source") and done == ["one", "two"] and generation == 2)):
    print("PAUSED_AT_STEP_BOUNDARY: end this turn. Keep Task in_progress; a replacement will continue without a new message.")
    sys.exit(0)
step = steps[len(done)]
key = f"teams/{team}/shared/tasks/{task_id}/output/step-{step}.json"
prefix = os.environ["AGENTTEAMS_STORAGE_PREFIX"].rstrip("/") + "/"


def write(key, data):
    subprocess.run(["mc", "pipe", prefix + key], input=data, check=True, capture_output=True)


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
print(json.dumps({"step": step, "checkpoint_version": cp["version"], "remaining_steps": cp["remaining_steps"], "sha256": digest}))
