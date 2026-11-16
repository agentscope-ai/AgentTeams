"""Deterministic side-effect fixture invoked by the real Harness's model tool.

This never starts a model or resumes a Task by itself. A separately owned
HTTP ledger executes/counts effects; Worker-written objects are only artifacts.
"""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import urllib.parse
import urllib.request

sys.path.insert(0, "/opt/agentteams/scripts/lib")
from task_recovery import Recovery

task_id, team = sys.argv[1:]
recovery = Recovery()
task = recovery.current(task_id, team)
if not task or task["status"] != "in_progress":
    raise RuntimeError("ACK the assigned Task durably before running a step")
cp = dict(task["execution"].get("checkpoint") or {})
steps = ["one", "two", "three"]
done = cp.get("completed_steps", [])
if done == steps:
    print("ALL_STEPS_DONE: submit the same task ID with output/step-*.json")
    sys.exit(0)
if done == ["one"] and task["execution"]["generation"] == 1:
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
else:
    effect("REGISTER")
    for field in ("result", "delivery", "reply_txn", "reply_event"):
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
