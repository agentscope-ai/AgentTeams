"""Deterministic side-effect fixture invoked by the real Harness's model tool.

This never starts a model or resumes a Task by itself. Separate per-invocation
effect receipts let the driver count steps even after a stale Task-dir sync.
"""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import uuid

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
effects_prefix = f"teams/{team}/shared/recovery-fixtures/{task_id}/effects/"


def read(key):
    return subprocess.run(["mc", "cat", prefix + key], check=True, capture_output=True).stdout


def write(key, data):
    subprocess.run(["mc", "pipe", prefix + key], input=data, check=True, capture_output=True)


operation = {"id": f"fixture:{task_id}:{step}", "state": "started"}
listing = subprocess.run(["mc", "ls", "--json", prefix + effects_prefix], check=True, capture_output=True).stdout
receipts = []
for line in listing.splitlines():
    row = json.loads(line)
    if row.get("type") == "file" and row.get("key", "").endswith(".json"):
        receipt_key = effects_prefix + row["key"].rsplit("/", 1)[-1]
        data = read(receipt_key)
        receipts.append((receipt_key, json.loads(data), data))
existing = [receipt for receipt in receipts if receipt[1]["step"] == step]
if cp.get("operation", {}).get("state") in {"started", "unknown"}:
    if cp["operation"]["id"] != operation["id"] or len(existing) != 1:
        raise RuntimeError("Needs verification: operation outcome cannot be proved; no rerun")
    receipt_key, entry, data = existing[0]
else:
    if existing:
        raise RuntimeError("Unexpected side effect already exists; no rerun")
    for field in ("result", "delivery", "reply_txn", "reply_event"):
        cp.pop(field, None)
    cp.update(version=cp.get("version", 0) + 1, completed_steps=done, remaining_steps=steps[len(done):], operation=operation)
    recovery.checkpoint(task_id, team, cp)
    entry = {"step": step, "generation": task["execution"]["generation"], "process_id": recovery.process,
        "task_id": task_id, "checkpoint_version": cp["version"]}
    data = json.dumps(entry, sort_keys=True).encode()
    receipt_key = effects_prefix + step + "-" + uuid.uuid4().hex + ".json"
    write(receipt_key, data)
write(key, data)
digest = hashlib.sha256(data).hexdigest()
operation.update(state="verified", evidence="Persisted step output SHA256=" + digest + "; effect receipt=" + receipt_key + "; count=1")
cp.update(version=cp["version"] + 1, completed_steps=done + [step], remaining_steps=steps[len(done) + 1:], operation=operation,
    artifacts=cp.get("artifacts", []) + [{"key": key, "sha256": digest, "size": len(data)}])
recovery.checkpoint(task_id, team, cp)
print(json.dumps({"step": step, "checkpoint_version": cp["version"], "remaining_steps": cp["remaining_steps"], "sha256": digest}))
