"""Keep compact send references until the submitting native session records them."""
import hashlib
import json
import os
from pathlib import Path
from tempfile import NamedTemporaryFile


def directory():
    return Path(os.environ["AGENTTEAMS_WORKER_HOME"]) / ".agentteams/native-handoffs"


def remember(result, actor, target):
    task = {k: result["task"][k] for k in ("id", "submissionId", "mainTaskId") if k in result["task"]}
    assignments = [{k: item[k] for k in ("status", "eventId", "taskId")}
                   for item in result.get("assignments", []) if item.get("status") == "queued" and item.get("eventId")]
    notice = result.get("notification") or {}
    notification = {k: notice[k] for k in ("status", "eventId")} if notice.get("status") == "queued" and notice.get("eventId") else {}
    if not assignments and not notification:
        return
    data = {"target": target, "actor": actor, "result": {"task": task, "assignments": assignments, "notification": notification}}
    payload = json.dumps(data, sort_keys=True, ensure_ascii=False).encode()
    folder = directory()
    folder.mkdir(exist_ok=True, parents=True)
    path = folder / (hashlib.sha256(payload).hexdigest() + ".json")
    with NamedTemporaryFile(dir=folder, delete=False) as tmp:
        tmp.write(payload)
    os.replace(tmp.name, path)


def pending(get_receipts, runtime):
    for path in directory().glob("*.json"):
        raw = path.read_bytes()
        if hashlib.sha256(raw).hexdigest() != path.stem:
            raise ValueError("Pending handoff identity differs from its content")
        entry = json.loads(raw)
        if entry["target"]["runtime"] != runtime:
            continue
        result = entry["result"]
        facts = get_receipts(result, entry["actor"])
        if len(facts) == len(result["assignments"]) + bool(result["notification"]):
            yield {"key": path.stem, "target": entry["target"], "facts": [r["fact"] for r in facts],
                   "taskId": result["task"]["id"], "submissionId": result["task"]["submissionId"]}
