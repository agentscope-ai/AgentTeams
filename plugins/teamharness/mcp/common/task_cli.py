"""Shared Task tool entry. Business rules stay in the public agt CLI/API."""
from __future__ import annotations

import json
import copy
import os
from pathlib import Path
import re
import shutil
import subprocess


# Required fields, optional fields, and repeated flags. No caller-supplied
# executable, shell, environment, storage path, recipient or Project routing.
_ACTIONS = {
    "create": (("leader", "title"), ("spec",), {}),
    "plan": (("nodes",), ("team",), {}),
    "get": ((), ("team",), {}),
    "begin": ((), ("team",), {}),
    "submit": (("summary",), ("report", "status", "team"), {"files": "file"}),
    "accept": (("submissionId", "note"), ("team",), {}),
    "reject": (("submissionId", "note"), ("team", "replacementSpec"), {}),
    "complete": (("report",), ("team",), {"artifacts": "artifact"}),
    "files_list": ((), ("team",), {}),
    "files_read": ((), ("team", "name", "artifactId", "version", "offset", "limit"), {}),
    "files_get": ((), ("team", "name", "artifactId", "version"), {}),
    "files_put": (("source", "name"), ("kind", "team"), {}),
}
_FLAGS = {"submissionId": "submission", "artifactId": "artifact", "replacementSpec": "replacement-spec"}
_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")

TASK_SCHEMA = {
    "description": (
        "Operate on tasks through the common task-system CLI/API. Manager: create an original request; "
        "Leader: plan, inspect and accept/reject exact submissions, then complete the main task; "
        "Worker: begin and submit outputs. Use task IDs and artifact IDs, not Project or storage paths. "
        "The task system prepares rooms, dispatches dependencies and returns accepted reports/artifacts "
        "to the original conversation automatically. Do not send duplicate notifications. "
        "files_read returns bounded text; files_get returns a verified local path. "
        "This tool has no arbitrary shell execution."
    ),
    "inputSchema": {
        "type": "object", "required": ["action"], "additionalProperties": False,
        "properties": {
            "action": {"type": "string", "enum": list(_ACTIONS)},
            "taskId": {"type": "string", "description": "Required except create. Use the task-system ID, not a local step alias."},
            "leader": {"type": "string", "description": "For create: registered Leader name."},
            "title": {"type": "string", "description": "For create: concise original request title."},
            "spec": {"type": "string", "description": "For create: optional supplementary context. The task system captures the original Human message automatically; do not copy or summarize it here."},
            "nodes": {"type": "array", "description": "For plan: complete execution DAG; no source routes or execution Task IDs.",
                      "items": {"type": "object", "additionalProperties": False, "required": ["id", "assignedTo", "spec"],
                                "properties": {"id": {"type": "string"}, "title": {"type": "string"},
                                               "assignedTo": {"type": "string"}, "spec": {"type": "string"},
                                               "dependsOn": {"type": "array", "items": {"type": "string"}}}}},
            "summary": {"type": "string", "description": "For submit: actual result and validation summary."},
            "report": {"type": "string", "description": "For submit: optional detailed report; for complete: verified final human report."},
            "status": {"type": "string", "enum": ["SUCCESS", "SUCCESS_WITH_NOTES", "REVISION_NEEDED", "BLOCKED", "INTERRUPTED"]},
            "files": {"type": "array", "items": {"type": "string"}, "description": "For submit: files relative to this task's prepared output directory."},
            "artifacts": {"type": "array", "items": {"type": "string"}, "description": "For complete: optional exact accepted artifact IDs; default all accepted outputs."},
            "submissionId": {"type": "string", "description": "For accept/reject: exact candidate ID from the task system."},
            "replacementSpec": {"type": "string", "description": "For reject only: optional explicit corrections to dispatch a new execution of the same step, preserving the rejected attempt."},
            "note": {"type": "string", "description": "For accept/reject: your independent business validation evidence or rejection reason."},
            "team": {"type": "string", "description": "Only when the task system reports an ambiguous task ID."},
            "name": {"type": "string", "description": "Declared task-local file name. With artifactId, this is an optional consistency check and must match that artifact."},
            "artifactId": {"type": "string"}, "version": {"type": "string"},
            "offset": {"type": "integer", "minimum": 0}, "limit": {"type": "integer", "minimum": 4, "maximum": 32768},
            "source": {"type": "string", "description": "For files_put: file relative to your prepared task output directory, including scratch intermediates."},
            "kind": {"type": "string", "enum": ["output", "intermediate"]},
        },
    },
}


def schema(role: str) -> dict:
    """Present each runtime role's operations; API authorization remains final."""
    allowed = {
        "manager": {"create", "get", "files_list", "files_read", "files_get"},
        "leader": {"plan", "get", "accept", "reject", "complete", "files_list", "files_read", "files_get"},
        "worker": {"begin", "get", "submit", "files_list", "files_read", "files_get", "files_put"},
        "remote-member": {"begin", "get", "submit", "files_list", "files_read", "files_get", "files_put"},
    }.get(role, set(_ACTIONS))
    result = copy.deepcopy(TASK_SCHEMA)
    properties = result["inputSchema"]["properties"]
    fields = {"action", "taskId"}
    for action in allowed:
        required, optional, repeated = _ACTIONS[action]
        fields.update((*required, *optional, *repeated))
    result["inputSchema"]["properties"] = {key: value for key, value in properties.items() if key in fields}
    result["inputSchema"]["properties"]["action"]["enum"] = [action for action in _ACTIONS if action in allowed]
    if "create" not in allowed:
        result["inputSchema"]["required"] = ["action", "taskId"]
    return result


def command(arguments: dict) -> tuple[list[str], dict[str, str]]:
    if not isinstance(arguments, dict):
        raise ValueError("task arguments must be an object")
    action = arguments.get("action")
    if not isinstance(action, str) or action not in _ACTIONS:
        raise ValueError("unknown task action")
    required, optional, repeated = _ACTIONS[action]
    allowed = {"action", *required, *optional, *repeated}
    if action != "create":
        allowed.add("taskId")
    else:
        allowed.add("_nativeSource")
    if set(arguments) - allowed:
        raise ValueError("unsupported arguments for this task action")
    binary = shutil.which("agt")
    if not binary:
        raise ValueError("task CLI unavailable in this runtime")
    argv = [binary, "task"]
    argv += ["files", action[6:]] if action.startswith("files_") else [action]
    if action != "create":
        task_id = arguments.get("taskId")
        if not isinstance(task_id, str) or not _ID.fullmatch(task_id):
            raise ValueError("valid taskId required")
        argv.append(task_id)
    for field in required:
        if field not in arguments or (field != "nodes" and (not isinstance(arguments[field], str) or not arguments[field].strip())):
            raise ValueError(field + " required")
    for field in (*required, *optional):
        if field not in arguments:
            continue
        value = arguments[field]
        if field == "nodes":
            if not isinstance(value, list):
                raise ValueError("nodes must be an array")
            argv += ["--json", json.dumps({"nodes": value}, ensure_ascii=False)]
        elif field in {"offset", "limit"}:
            if type(value) is not int:
                raise ValueError(field + " must be an integer")
            argv += ["--" + field, str(value)]
        else:
            if not isinstance(value, str):
                raise ValueError(field + " must be a string")
            if field == "source":
                shared = os.getenv("TEAMHARNESS_SHARED_DIR") or os.getenv("AGENTTEAMS_SHARED_DIR")
                if not shared or Path(value).is_absolute() or ".." in Path(value).parts:
                    raise ValueError("source must be relative to your prepared task output directory")
                root = Path(shared).resolve() / "tasks" / arguments["taskId"] / "workspace"
                source = (root / value).resolve()
                if root.resolve() != root or not source.is_relative_to(root):
                    raise ValueError("source escapes the prepared task output directory")
                value = str(source)
            argv += ["--" + _FLAGS.get(field, field), value]
    for field, flag in repeated.items():
        values = arguments.get(field, [])
        if not isinstance(values, list) or any(not isinstance(value, str) or not value for value in values):
            raise ValueError(field + " must be an array of nonempty strings")
        for value in values:
            argv += ["--" + flag, value]
    if action in {"files_read", "files_get"} and not (arguments.get("name") or arguments.get("artifactId")):
        raise ValueError("name or artifactId required")
    env = os.environ.copy()
    if action == "create":
        source = arguments.get("_nativeSource")
        if (not isinstance(source, dict) or set(source) != {"eventId", "userId", "roomId"}
                or not all(isinstance(value, str) and value for value in source.values())
                or not source["userId"].startswith("@") or not source["roomId"].startswith("!")):
            raise ValueError("native original-conversation context unavailable")
        env.update({"AGENTTEAMS_ORIGIN_EVENT_ID": source["eventId"], "AGENTTEAMS_REQUESTER_USER_ID": source["userId"],
                    "AGENTTEAMS_REQUESTER_ROOM_ID": source["roomId"]})
    return argv, env


def run(arguments: dict) -> dict:
    try:
        argv, env = command(arguments)
        if os.getenv("AGENTTEAMS_OTEL_ENABLED", "").lower() in {"true", "1"}:
            try:
                from opentelemetry.propagate import inject
            except ImportError:
                pass
            else:
                carrier = {}
                inject(carrier)
                for name in ("traceparent", "tracestate", "baggage"):
                    if carrier.get(name):
                        env["AT_OTEL_" + name.upper()] = carrier[name]
        process = subprocess.run(argv, env=env, capture_output=True, text=True, timeout=120)
        if process.returncode:
            return {"ok": False, "error": "task_operation_failed", "exitCode": process.returncode,
                    "message": process.stderr.strip()[:2048]}
        result = json.loads(process.stdout)
        if not isinstance(result, dict):
            raise ValueError("task CLI returned an invalid receipt")
        return result
    except (OSError, ValueError, subprocess.TimeoutExpired) as error:
        return {"ok": False, "error": "task_operation_unavailable", "message": str(error)[:2048]}
