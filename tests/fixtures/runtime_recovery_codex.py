"""Read the standard Codex Task descriptor and measure the real App Server rollout."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import uuid


def rollout_facts(home, thread, turn, marker, room):
    if not thread or str(uuid.UUID(thread)) != thread or not turn:
        raise ValueError("Missing native Codex thread or turn identity")
    paths = list((home / "sessions").rglob("rollout-*-" + thread + ".jsonl"))
    if len(paths) != 1:
        raise ValueError("Codex thread does not identify one native rollout")
    data = paths[0].read_bytes()
    if not data.endswith(b"\n"):
        raise ValueError("Incomplete native rollout row")
    rows = [json.loads(line) for line in data.splitlines()]
    meta = rows[0]
    if meta["type"] != "session_meta" or meta["payload"].get("id") != thread:
        raise ValueError("Native Codex session metadata does not match the selected thread")
    current = None
    user_inputs = tools = outputs = 0
    for row in rows:
        item = row["payload"]
        if (row["type"] == "turn_context" or row["type"] == "event_msg" and item.get("type") == "task_started") and item.get("turn_id"):
            current = item["turn_id"]
        if current != turn or row["type"] != "response_item":
            continue
        if item.get("type") == "message" and item.get("role") == "user":
            user_inputs += sum(part.get("type") == "input_text" and (
                "[AgentTeams event " + marker + "]\nCurrent Matrix session: matrix:" + room + "\n") in part.get("text", "")
                for part in item["content"])
        tools += item.get("type") in ("function_call", "custom_tool_call")
        outputs += item.get("type") in ("function_call_output", "custom_tool_call_output")
    return {"thread_id": thread, "metadata_id": meta["payload"]["id"], "turn_id": turn,
            "rollout": str(paths[0].relative_to(home)), "native_digest": hashlib.sha256(data).hexdigest(),
            "native_bytes": len(data), "source_inputs": user_inputs, "tool_calls": tools, "tool_outputs": outputs}


def collect(task_id, team, room, generation):
    import yaml
    # docker exec does not inherit variables exported by the entrypoint.
    worker_home = Path(os.getenv("AGENTTEAMS_WORKER_HOME") or "/root/agentteams-fs/agents/agent-a")
    runtime = Path(os.getenv("TEAMHARNESS_RUNTIME_CONFIG") or worker_home / "runtime/runtime.yaml")
    config = yaml.safe_load(runtime.read_text())
    marker = f"recovery:{team}/{task_id}:{generation}"
    journal = runtime.parent / "codex-bridge-state.json"
    event = json.loads(journal.read_text())["events"].get(marker, {}) if journal.is_file() else {}
    work = ["agents/agent-a", "task", team, task_id]
    key = hashlib.sha256(json.dumps(work, ensure_ascii=False, separators=(",", ":")).encode()).hexdigest() + ".json"
    base = os.environ["AGENTTEAMS_STORAGE_PREFIX"].rstrip("/") + "/agents/agent-a/runtime/sessions/codex/"
    listing = subprocess.run(["mc", "ls", "--json", base], capture_output=True, check=True, text=True)
    descriptor = None
    if any(json.loads(row).get("key") == key for row in listing.stdout.splitlines()):
        descriptor = json.loads(subprocess.check_output(["mc", "cat", base + key]))
    identity = event.get("event", {}).get("recovery", {})
    native = None
    if event.get("thread_id") and event.get("turn_id"):
        home = Path(os.environ.get("CODEX_HOME") or runtime.parent.parent / ".codex")
        native = rollout_facts(home, event["thread_id"], event["turn_id"], marker, room)
    return {"cache": descriptor, "config_generation": config["metadata"].get("generation", 0), "route": {
        k: identity.get(k) for k in ("task_id", "team_id", "generation", "instance_id", "process_id")},
        "native": native}


if __name__ == "__main__":
    print(json.dumps(collect(sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4]))))
