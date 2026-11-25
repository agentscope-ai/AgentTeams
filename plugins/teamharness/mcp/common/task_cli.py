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
    "create": (("leader", "title"), ("spec", "inputId"), {"relatedInputIds": "related-input-id"}),
    "plan": (("nodes",), ("team",), {}),
    "get": ((), ("team",), {}),
    "begin": ((), ("team",), {}),
    "submit": (("summary",), ("report", "status", "team"), {"files": "file"}),
    "accept": (("submissionId", "note"), ("team",), {}),
    "reject": (("submissionId", "note"), ("team", "replacementSpec", "replacementNodes"), {}),
    "complete": (("report",), ("team",), {"artifacts": "artifact"}),
    "cancel": (("reason",), ("team",), {}),
    "files_list": ((), ("team",), {}),
    "files_read": ((), ("team", "name", "artifactId", "version", "offset", "limit"), {}),
    "files_get": ((), ("team", "name", "artifactId", "version"), {}),
    "files_put": (("source", "name"), ("kind", "team"), {}),
}
_FLAGS = {"submissionId": "submission", "artifactId": "artifact", "replacementSpec": "replacement-spec", "inputId": "input-id"}
_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")
_READ_ONLY_ACTIONS = {"get", "files_list", "files_read"}

TASK_SCHEMA = {
    "description": (
        "Operate on tasks through the common task-system CLI/API. Interpret Human intent before choosing an operation. "
        "Manager: create a task when needed; Manager/Leader: cancel the intended existing main task; "
        "Leader: plan only initial required work, never hypothetical correction nodes; handle failures with replacementNodes local IDs (entry dependsOn=[]), then verify and complete the main task; "
        "Worker: begin and submit truthful outputs; normal planned SUCCESS hands off directly to ready downstream Workers. Use REVISION_NEEDED for failed checks, not SUCCESS. Use task IDs and artifact IDs, not Project or storage paths. "
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
            "inputId": {"type": "string", "description": "For create: source input reference supplied by the framework; required when more than one message could be the original request. This is not a room, Project or storage path."},
            "relatedInputIds": {"type": "array", "items": {"type": "string"}, "description": "For create: later corrections or supplements to this same request, in supplied input order; same Human conversation only. Never use this field to merge distinct work requests merely because their text matches. The task system captures their exact bodies automatically."},
            "spec": {"type": "string", "description": "For create: only necessary earlier context outside the supplied inputs. Bind supplied corrections with relatedInputIds; never put only their paraphrase here. The task system captures original Human bodies automatically."},
            "nodes": {"type": "array", "description": "For plan: complete execution DAG; no source routes or execution Task IDs.",
                      "items": {"type": "object", "additionalProperties": False, "required": ["id", "assignedTo", "spec", "dependsOn"],
                                "properties": {"id": {"type": "string"}, "title": {"type": "string"},
                                               "assignedTo": {"type": "string"}, "spec": {"type": "string", "description": "Only this node responsibility, inputs, outputs and acceptance checks. The original Human request is attached automatically; do not copy it or other nodes into each spec."},
                                               "dependsOn": {"type": "array", "items": {"type": "string"}, "description": "Required on every node. List all upstream local node IDs whose completed outputs this node needs; use [] only for independent work."}}}},
            "summary": {"type": "string", "description": "For submit: actual result and validation summary."},
            "report": {"type": "string", "description": "For submit: optional detailed report; for complete: verified final human report."},
            "status": {"type": "string", "enum": ["SUCCESS", "SUCCESS_WITH_NOTES", "REVISION_NEEDED", "BLOCKED", "INTERRUPTED"]},
            "files": {"type": "array", "items": {"type": "string"}, "description": "For submit: JSON array of filenames relative to this task's prepared output directory, even for one file."},
            "artifacts": {"type": "array", "items": {"type": "string"}, "description": "For complete: final deliverable set, using accepted artifactId values. Exclude superseded intermediate versions and repeated filenames for the same project. Omit only when every accepted output belongs in the final set."},
            "submissionId": {"type": "string", "description": "For accept/reject: exact candidate ID from the task system."},
            "replacementSpec": {"type": "string", "description": "For reject only: optional explicit corrections to dispatch a new execution of the same step, preserving the rejected attempt."},
            "note": {"type": "string", "description": "For accept/reject: your independent business validation evidence or rejection reason."},
            "reason": {"type": "string", "description": "For cancel: explicit reason for cancelling this exact main Task. Saved results are retained; cancellation does not confirm a running tool has stopped."},
            "team": {"type": "string", "description": "Only when the task system reports an ambiguous task ID."},
            "name": {"type": "string", "description": "Declared task-local file name. With artifactId, this is an optional consistency check and must match that artifact."},
            "artifactId": {"type": "string"}, "version": {"type": "string"},
            "offset": {"type": "integer", "description": "For files_read: nonnegative byte offset."},
            "limit": {"type": "integer", "description": "For files_read: maximum number of bytes, from 4 to 32768."},
            "source": {"type": "string", "description": "For files_put: file relative to your prepared task output directory, including scratch intermediates."},
            "kind": {"type": "string", "enum": ["output", "intermediate"]},
        },
    },
}


TASK_SCHEMA["inputSchema"]["properties"]["replacementNodes"] = {
    **copy.deepcopy(TASK_SCHEMA["inputSchema"]["properties"]["nodes"]),
    "description": "For reject only: replace this failed step with a correction DAG, such as Developer fix then independent Reviewer. Requires replacementSpec. Entry nodes inherit this step's upstream inputs; downstream receives all corrected outputs and waits for the complete correction DAG. Use local IDs only. The main Task and prior attempts are retained.",
}


def schema(role: str) -> dict:
    """Present each runtime role's operations; API authorization remains final."""
    allowed = {
        "manager": {"create", "get", "cancel", "files_list", "files_read", "files_get"},
        "leader": {"create", "plan", "get", "accept", "reject", "complete", "cancel", "files_list", "files_read", "files_get"},
        "worker": {"begin", "get", "submit", "files_list", "files_read", "files_get", "files_put"},
        "remote-member": {"begin", "get", "submit", "files_list", "files_read", "files_get", "files_put"},
    }.get(role, set(_ACTIONS))
    result = copy.deepcopy(TASK_SCHEMA)
    if role == "manager":
        result["description"] = (
            "Delegate new business work with action=create whenever a registered Team can handle it, "
            "including quick work that produces a file or other deliverable. The Task system delivers "
            "the accepted result to the original conversation. Use get to inspect an existing Task, "
            "cancel for its explicit withdrawal, and the file actions for its verified artifacts."
        )
    elif role == "leader":
        result["description"] = (
            "Use create only for a direct registered Team Admin work request in your private conversation; "
            "set leader to your own registered identity and use the supplied Human input reference. "
            "Preserve its receipt and wait for the admitted handoff before plan. "
            "Use plan/review/completion only for the framework-prepared Task phase."
        )
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
    unsupported = set(arguments) - allowed
    if unsupported:
        public_fields = sorted(allowed - {"_nativeSource"})
        raise ValueError(f"unsupported arguments for task {action}: {', '.join(sorted(unsupported))}; "
                         f"allowed fields: {', '.join(public_fields)}")
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
        if field in {"nodes", "replacementNodes"}:
            if not isinstance(value, list):
                raise ValueError("nodes must be an array")
            if field == "replacementNodes":
                if not arguments.get("replacementSpec", "").strip() or not 0 < len(value) <= 50:
                    raise ValueError("replacementNodes requires replacementSpec and 1 to 50 nodes")
                ids = [node.get("id") for node in value if isinstance(node, dict)]
                if len(ids) != len(value) or any(not isinstance(identity, str) or not _ID.fullmatch(identity) or len(identity) > 64 for identity in ids) or len(set(ids)) != len(ids):
                    raise ValueError("replacementNodes requires distinct local node IDs")
                pending = {}
                for node in value:
                    if (set(node) - {"id", "title", "assignedTo", "spec", "dependsOn"}
                            or any(not isinstance(node.get(key), str) or not node[key].strip() for key in ("assignedTo", "spec"))
                            or ("title" in node and not isinstance(node["title"], str))):
                        raise ValueError("replacementNodes requires assignedTo and spec, with only the public node fields")
                    dependencies = node.get("dependsOn")
                    if (not isinstance(dependencies, list) or any(not isinstance(dep, str) or dep not in ids or dep == node["id"] for dep in dependencies)
                            or len(set(dependencies)) != len(dependencies)):
                        raise ValueError("replacementNodes dependsOn uses only local correction IDs; use [] for entry nodes, which inherit prior inputs automatically")
                    pending[node["id"]] = set(dependencies)
                while pending:
                    ready = {identity for identity, dependencies in pending.items() if not dependencies}
                    if not ready:
                        raise ValueError("replacementNodes must be acyclic")
                    pending = {identity: dependencies - ready for identity, dependencies in pending.items() if identity not in ready}
            argv += (["--json", json.dumps({"nodes": value}, ensure_ascii=False)] if field == "nodes"
                     else ["--replacement-nodes", json.dumps(value, ensure_ascii=False)])
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
        if (not isinstance(source, dict) or set(source) - {"eventId", "userId", "roomId", "relatedSources", "frontierEventId"} or not {"eventId", "userId", "roomId"}.issubset(source)
                or not all(isinstance(source.get(key), str) and source[key] for key in ("eventId", "userId", "roomId"))
                or not source["userId"].startswith("@") or not source["roomId"].startswith("!")):
            raise ValueError("native original-conversation context unavailable")
        env.update({"AGENTTEAMS_ORIGIN_EVENT_ID": source["eventId"], "AGENTTEAMS_REQUESTER_USER_ID": source["userId"],
                    "AGENTTEAMS_REQUESTER_ROOM_ID": source["roomId"]})
        if source.get("frontierEventId"):
            env["AGENTTEAMS_INPUT_FRONTIER"] = source["frontierEventId"]
        related = source.get("relatedSources", [])
        refs = arguments.get("relatedInputIds", [])
        if (not isinstance(related, list) or len(related) != len(refs)
                or len(set(refs)) != len(refs) or any(not isinstance(event, str) or not event.startswith("$") for event in related)):
            raise ValueError("related inputs do not match their native source binding")
        if refs:
            env["AGENTTEAMS_RELATED_INPUTS"] = json.dumps(dict(zip(refs, related)))
        if "inputId" in arguments:
            env["AGENTTEAMS_INPUT_ID"] = arguments["inputId"]
    return argv, env


def cli_http_status(message: str) -> int | None:
    first = message.strip().splitlines()[0] if message.strip() else ""
    status = re.match(r"^(?:Error: )?HTTP ([0-9]{3})(?::|\s|$)", first)
    return int(status.group(1)) if status else None


def retryable_cli_error(message: str) -> bool:
    """Classify the CLI transport status, never numbers inside a server message."""
    code = cli_http_status(message)
    if code is not None:
        return code == 429 or 500 <= code <= 599
    first = message.strip().splitlines()[0] if message.strip() else ""
    return bool(re.match(r'^(?:Error: )?(?:Get|Post|Put|Patch|Delete|Head) "https?://[^"\n]+": ', first))


def _run(arguments: dict) -> dict:
    failure_effect = "none"
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
        if arguments["action"] not in _READ_ONLY_ACTIONS:
            failure_effect = "unknown"
        process = subprocess.run(argv, env=env, capture_output=True, text=True, timeout=120)
        if process.returncode:
            failure = {"ok": False, "error": "task_operation_failed", "exitCode": process.returncode,
                       "message": process.stderr.strip()[:2048], "retryable": retryable_cli_error(process.stderr),
                       "effect": failure_effect}
            if (status := cli_http_status(process.stderr)) is not None:
                failure["httpStatus"] = status
            return failure
        result = json.loads(process.stdout)
        if not isinstance(result, dict):
            raise ValueError("task CLI returned an invalid receipt")
        return result
    except (OSError, ValueError, subprocess.TimeoutExpired) as error:
        return {"ok": False, "error": "task_operation_unavailable", "message": str(error)[:2048],
                "retryable": isinstance(error, subprocess.TimeoutExpired), "effect": failure_effect}


def _validate_mutation(arguments: dict) -> None:
    command(arguments)
    selectors = arguments.get("artifacts")
    if arguments["action"] != "complete" or not selectors:
        return
    lookup = {"action": "get", "taskId": arguments["taskId"]}
    if "team" in arguments:
        lookup["team"] = arguments["team"]
    task = _run(lookup).get("task")
    if not isinstance(task, dict) or task.get("id") != arguments["taskId"]:
        raise ValueError("Could not read the accepted Task results. Retry before completing.")
    if "acceptedResults" not in task and not task.get("planId"):
        return  # Older main tasks do not expose the saved accepted-result projection.
    accepted = {artifact["artifactId"] for result in task.get("acceptedResults", [])
                for artifact in result["candidate"]["artifacts"]}
    if len(selectors) != len(set(selectors)) or any(ref not in accepted for ref in selectors):
        raise ValueError("Use distinct accepted artifactId values, or omit artifacts to deliver all accepted outputs.")


def run(arguments: dict) -> dict:
    if not os.getenv("AGENTTEAMS_CONTROLLER_URL"):
        return _run(arguments)
    if __package__:
        from . import task_canonical
    else:
        import task_canonical
    return task_canonical.run(arguments, _run, validate=_validate_mutation)
